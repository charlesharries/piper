package musicbrainz

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/teal-fm/piper/models"
)

// This file answers one question: is this the right *performance*? Picking the
// release it lives on is release.go's separate scoring system.

// minConfidence is the score a candidate must reach before we attach its MBIDs
// to a play.
const minConfidence = 0.62

// Signal weights.
const (
	weightTitle    = 3.0
	weightArtist   = 3.0
	weightDuration = 2.0
	weightAlbum    = 1.0
	weightMBScore  = 0.25
)

// qualifierPenalty is subtracted when a candidate carries a variant qualifier
// the play did not ask for, e.g. matching "Dreams" against "Dreams (outtake)".
const qualifierPenalty = 0.25

// durationConflictPenalty is the most that is subtracted when the play and the
// candidate both carry a length and disagree by more than durationScore
// tolerates.
const durationConflictPenalty = 0.25

// durationTolerance is the disagreement durationScore still scores above zero.
// Songs within 20s of each other may be a valid match
const durationTolerance = 20 * 1000

// durationConflictRamp is how far past durationTolerance the lengths must
// disagree before the conflict penalty is felt in full.
const durationConflictRamp = 40 * 1000

// minTitleAgreement is the title agreement below which a candidate is not
// trusted to be the same song at all, whatever else lines up. Title is the
// track's identity, not one vote among several: without this, a candidate
// sharing only an artist and a runtime -- which any two tracks on an album may
// -- outscores the recording actually played. Correct matches sit at 1.00 with
// the exception of classical, where MusicBrainz numbers movements the services
// do not; the lowest in the golden set is 0.51, so this leaves room beneath it.
const minTitleAgreement = 0.45

// uncorroboratedPenalty is subtracted when title and artist are the only
// signals available -- this could be basically anything as far as we know!
const uncorroboratedPenalty = 0.4

// qualifierAgreement is how closely two qualifiers must match before a
// candidate's counts as the one the play asked for.
const qualifierAgreement = 0.8

// matchInput is the normalized view of an incoming play, computed once and
// reused across every candidate.
type matchInput struct {
	title      reading
	artists    []string
	album      reading
	durationMs int64
	isrc       string
}

func newMatchInput(track models.Track) matchInput {
	artists := make([]string, 0, len(track.Artist))
	for _, a := range track.Artist {
		if n := normalize(a.Name); n != "" {
			artists = append(artists, n)
		}
	}

	return matchInput{
		title:      readingOf(track.Name),
		artists:    artists,
		album:      readingOf(track.Album),
		durationMs: track.DurationMs,
		isrc:       strings.ToUpper(strings.TrimSpace(track.ISRC)),
	}
}

// scoreRecording rates a candidate in [0,1] against the incoming play and
// returns the breakdown behind it, for the -explain flag and the log event.
func scoreRecording(in matchInput, rec Recording) (float64, signals) {
	// An ISRC identifies a recording outright: if the candidate carries the one
	// the service gave us, nothing else needs checking.
	if in.isrc != "" {
		for _, isrc := range rec.ISRCs {
			if strings.EqualFold(strings.TrimSpace(isrc), in.isrc) {
				return 1, signals{{"isrc", 1}}
			}
		}
	}

	var card scorecard

	title, unmatchedVariant := in.titleScore(rec)
	card.add("title", title, weightTitle)
	card.add("artist", in.artistScore(rec.ArtistCredit), weightArtist)

	// corroborated records whether anything beyond title and artist had a view.
	var corroborated, durationConflict bool
	var durationDelta int64

	if in.durationMs > 0 && rec.Length > 0 {
		duration := durationScore(in.durationMs, int64(rec.Length))
		card.add("duration", duration, weightDuration)
		durationConflict = duration == 0
		durationDelta = difference(in.durationMs, int64(rec.Length))
		corroborated = true
	}
	if in.album.full != "" && len(rec.Releases) > 0 {
		card.add("album", in.albumScore(rec.Releases, rec.Title), weightAlbum)
		corroborated = true
	}
	if rec.Score > 0 {
		// MusicBrainz's own query score says how well a record matched the
		// query, not whether it is the right recording, so it breaks ties but
		// doesn't count as corroboration.
		card.add("mb", float64(rec.Score)/100, weightMBScore)
	}

	if unmatchedVariant {
		card.penalise("variant", qualifierPenalty)
	}
	if rec.Video {
		card.penalise("video", qualifierPenalty)
	}
	if durationConflict {
		if conflict := durationConflictAmount(durationDelta); conflict > 0 {
			card.penalise("conflict", conflict)
		}
	}
	if !corroborated {
		card.penalise("uncorroborated", uncorroboratedPenalty)
	}

	// confidence is a final multiplier based on the title -- if everything but
	// the title matches, but the title is totally wrong, we could still clear
	// the 0.62 confidence threshold. Multiplying the whole score by how well
	// the title matches avoids these scenarios. Scenarii?
	confidence := titleConfidence(title)
	if confidence < 1 {
		card.signals = append(card.signals, signal{"title_floor", confidence})
	}

	return math.Max(0, card.score()*confidence), card.signals
}

// durationConflictAmount grades the penalty for two lengths that disagree by
// more than durationScore tolerates, reporting zero for a gap still inside it.
func durationConflictAmount(deltaMs int64) float64 {
	over := deltaMs - durationTolerance
	if over <= 0 {
		return 0
	}
	return durationConflictPenalty * math.Min(1, float64(over)/durationConflictRamp)
}

// titleConfidence is the factor a score is scaled by for how far its title fell
// short of minTitleAgreement: 1 at or above it, tapering to 0 for a title that
// agrees with nothing the play named.
func titleConfidence(title float64) float64 {
	if title >= minTitleAgreement {
		return 1
	}
	return title / minTitleAgreement
}

// difference returns the absolute gap between two lengths in milliseconds.
func difference(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}

// trackTitles returns the names the candidate's releases list it under: where a
// recording is titled after every song on a combined track, these name the one
// song a service reports.
func (rec Recording) trackTitles() []string {
	var titles []string
	seen := map[string]bool{rec.Title: true}
	for _, release := range rec.Releases {
		for _, medium := range release.Media {
			for _, track := range medium.Tracks {
				if track.Title == "" || seen[track.Title] {
					continue
				}
				seen[track.Title] = true
				titles = append(titles, track.Title)
			}
		}
	}
	return titles
}

// titleScore compares the incoming title against every name the candidate goes
// by, reporting whether the name that matched carries an unasked-for qualifier.
func (in matchInput) titleScore(rec Recording) (score float64, unmatchedVariant bool) {
	score, unmatchedVariant = in.compareTitle(rec.Title)

	for _, title := range rec.trackTitles() {
		if trackScore, trackVariant := in.compareTitle(title); trackScore > score {
			score, unmatchedVariant = trackScore, trackVariant
		}
	}
	return score, unmatchedVariant
}

// compareTitle rates one title against the incoming play, reporting whether it
// carries a variant qualifier the play never asked for.
func (in matchInput) compareTitle(title string) (float64, bool) {
	candidate := readingOf(title)
	unmatched := isVariantQualifier(candidate.qualifier) &&
		similarity(in.title.qualifier, candidate.qualifier) < qualifierAgreement
	return in.title.agrees(candidate), unmatched
}

// compareAlbum matches a release title against the incoming album name.
func (in matchInput) compareAlbum(title string) float64 {
	return in.album.agrees(readingOf(title))
}

// artistNameAgreement is how closely an artist has to answer to the name a play
// credited before their MBID is trusted to scope a search.
const artistNameAgreement = 0.9

// sortNameReadings renders a sort name the ways a service might credit it.
// Personal names sort inverted ("Yonezu, Kenshi"), so the comma-swapped reading
// is offered alongside the literal one.
func sortNameReadings(sortName string) []string {
	if strings.TrimSpace(sortName) == "" {
		return nil
	}
	readings := []string{normalize(sortName)}
	if family, given, ok := strings.Cut(sortName, ","); ok {
		readings = append(readings, normalize(given+" "+family))
	}
	return readings
}

// goesBy rates how strongly an artist answers to a name, across every reading
// MusicBrainz files them under.
func (a Artist) goesBy(name string) float64 {
	names := append([]string{normalize(a.Name)}, sortNameReadings(a.SortName)...)
	for _, alias := range a.Aliases {
		names = append(names, normalize(alias.Name))
	}
	return bestSimilarity([]string{normalize(name)}, names)
}

// artistScore returns the best agreement between any incoming artist name and
// any name on the candidate's artist credit.
func (in matchInput) artistScore(credits []ArtistCredit) float64 {
	if len(in.artists) == 0 || len(credits) == 0 {
		return 0
	}

	names := make([]string, 0, len(credits)*4+1)
	var joined strings.Builder
	for _, c := range credits {
		names = append(names, normalize(c.Name), normalize(c.Artist.Name))

		// For an artist held in a non-Latin script, the sort name or an alias
		// is where the Latin spelling a service credits them by lives.
		names = append(names, sortNameReadings(c.Artist.SortName)...)
		for _, alias := range c.Artist.Aliases {
			names = append(names, normalize(alias.Name))
		}

		joined.WriteString(c.Name)
		joined.WriteString(c.Joinphrase)
	}
	// The full credit line, so "Calvin Harris & Dua Lipa" can match an input
	// that kept both names.
	names = append(names, normalize(joined.String()))

	return bestSimilarity(in.artists, names)
}

// durationScore grades how closely two durations agree. Recordings of the same
// song differ by seconds and an outtake or live take by far more, which makes
// this the strongest signal for separating candidates MusicBrainz returns at
// identical query scores.
func durationScore(a, b int64) float64 {
	delta := difference(a, b)
	switch {
	case delta <= 2000:
		return 1.0
	case delta <= 5000:
		return 0.8
	case delta <= 10000:
		return 0.4
	case delta <= durationTolerance:
		return 0.1
	default:
		return 0
	}
}

// albumAgreement is the album score below which a recording is treated as not
// belonging to the album the play named. Correct attributions land near 1;
// around a half means a different record that merely shares an artist.
const albumAgreement = 0.8

// albumScore rates the best album the candidate could be attributed to, by
// running the release scorer over its releases. Grading on release quality
// rather than title similarity is what couples the two scoring systems: a
// recording whose only release is a bootleg should lose to one on the official
// issue, even though both releases carry the album's name.
func (in matchInput) albumScore(releases []Release, trackTitle string) float64 {
	if in.album.full == "" || len(releases) == 0 {
		return 0
	}
	_, score := bestRelease(in, releases, trackTitle, nil)
	return math.Min(1, math.Max(0, score))
}

// scorecard accumulates a weighted mean of signals, less any flat penalties.
// Both scoring systems are built on it: a signal is added only when the data
// for it exists, so the denominator shrinks to the evidence actually available,
// and penalties then answer for what that leaves unsaid.
type scorecard struct {
	weighted float64
	weight   float64
	penalty  float64
	signals  signals
}

func (c *scorecard) add(name string, score, weight float64) {
	c.weighted += score * weight
	c.weight += weight
	c.signals = append(c.signals, signal{name, score})
}

func (c *scorecard) penalise(name string, amount float64) {
	c.penalty += amount
	c.signals = append(c.signals, signal{name, -amount})
}

// score is the weighted mean less the penalties, unclamped: bestRelease orders
// on the raw value, so callers that publish a score clamp it themselves.
func (c *scorecard) score() float64 {
	if c.weight == 0 {
		return 0
	}
	return c.weighted/c.weight - c.penalty
}

// signal is one component of a score: what was compared, and how well it
// agreed. Penalties are negative.
type signal struct {
	name  string
	value float64
}

// signals is a score's full breakdown, in the order the components were
// applied. It renders for the CLI and expands into the event's `sig` group, so
// a bad match can be queried by the signal that let it through.
type signals []signal

func (s signals) String() string {
	parts := make([]string, len(s))
	for i, sig := range s {
		parts[i] = fmt.Sprintf("%s=%.2f", sig.name, sig.value)
	}
	return strings.Join(parts, " ")
}

// value returns what a named signal scored, reporting false when it never
// applied -- the difference between disagreement and no evidence either way.
func (s signals) value(name string) (float64, bool) {
	for _, sig := range s {
		if sig.name == name {
			return sig.value, true
		}
	}
	return 0, false
}

// candidate is a scored recording along with the release chosen for it.
type candidate struct {
	recording Recording
	release   *Release
	score     float64
	reasons   signals
}

func (c candidate) explain() string {
	title := c.recording.Title
	release := "<none>"
	if c.release != nil {
		release = c.release.Title
	}
	return fmt.Sprintf("%.3f  %-45s  %-35s  [%s]",
		c.score, truncate(title, 45), truncate(release, 35), c.reasons)
}

// rankCandidates scores every recording, best first.
func rankCandidates(in matchInput, recordings []Recording) []candidate {
	ranked := make([]candidate, 0, len(recordings))
	for _, rec := range recordings {
		score, reasons := scoreRecording(in, rec)
		ranked = append(ranked, candidate{recording: rec, score: score, reasons: reasons})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})
	return ranked
}
