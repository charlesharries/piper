package musicbrainz

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/teal-fm/piper/models"
)

// minConfidence is the score a candidate must reach before we attach its MBIDs
// to a play. MusicBrainz ranks by query match, not by correctness, so its top
// hit is frequently an outtake, a live take or a karaoke version. Below this
// threshold we publish no MBID at all, which is better than publishing a wrong
// one to a user's repo.
const minConfidence = 0.62

// Signal weights. A signal only contributes when we have the data for it, so a
// source without durations (Last.fm) is scored on the remaining signals rather
// than penalised.
const (
	weightTitle    = 3.0
	weightArtist   = 3.0
	weightDuration = 2.0
	weightAlbum    = 1.0
	weightMBScore  = 0.25
)

// qualifierPenalty is subtracted when the MusicBrainz title carries a variant
// qualifier the incoming play does not, e.g. matching "Dreams" against
// "Dreams (outtake)".
const qualifierPenalty = 0.25

// normalize prepares a string for comparison: it strips diacritics, lowercases,
// spells out '&', and reduces everything else to single-space-separated
// alphanumerics. This is what lets "Power, Corruption & Lies" compare equal to
// "Power Corruption and Lies".
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "&", " and ")

	var b strings.Builder
	b.Grow(len(s))
	lastWasSpace := true
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
			// Combining mark left over from decomposition; drop it so that
			// "Beyoncé" and "Beyonce" compare equal.
			continue
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastWasSpace = false
		case !lastWasSpace:
			b.WriteRune(' ')
			lastWasSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// levenshtein returns the edit distance between two rune slices.
func levenshtein(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}

	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(min(curr[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

// similarity scores two already-normalized strings in [0,1].
func similarity(a, b string) float64 {
	if a == b {
		if a == "" {
			return 0
		}
		return 1
	}
	if a == "" || b == "" {
		return 0
	}

	ra, rb := []rune(a), []rune(b)
	longest := max(len(ra), len(rb))
	return 1 - float64(levenshtein(ra, rb))/float64(longest)
}

// splitQualifier separates a title from a trailing variant qualifier, e.g.
// "Dreams (outtake)" or "Dreams - 2004 Remaster". The qualifier is returned
// normalized; an empty qualifier means the title carries none.
func splitQualifier(title string) (base, qualifier string) {
	trimmed := strings.TrimSpace(title)

	// Bracketed suffix: (...), [...], {...}
	for _, pair := range []struct{ open, close string }{{"(", ")"}, {"[", "]"}, {"{", "}"}} {
		if !strings.HasSuffix(trimmed, pair.close) {
			continue
		}
		if idx := strings.LastIndex(trimmed, pair.open); idx > 0 {
			inner := trimmed[idx+len(pair.open) : len(trimmed)-len(pair.close)]
			return normalize(trimmed[:idx]), normalize(inner)
		}
	}

	// Dash suffix: "Title - 2004 Remaster". Only treat it as a qualifier when
	// the dash is surrounded by spaces, so hyphenated titles survive intact.
	for _, sep := range []string{" - ", " – ", " — "} {
		if idx := strings.LastIndex(trimmed, sep); idx > 0 {
			return normalize(trimmed[:idx]), normalize(trimmed[idx+len(sep):])
		}
	}

	return normalize(trimmed), ""
}

// isVariantQualifier reports whether a qualifier marks a different version of a
// recording rather than being part of its name. It reuses the vocabulary the
// MetadataCleaner already relies on.
func isVariantQualifier(qualifier string) bool {
	if qualifier == "" {
		return false
	}
	for _, word := range guffParenWords {
		if strings.Contains(qualifier, normalize(word)) {
			return true
		}
	}
	return false
}

// matchInput is the normalized view of an incoming play, computed once and
// reused across every candidate.
type matchInput struct {
	title          string
	titleBase      string
	qualifier      string
	artists        []string
	album          string
	albumBase      string
	albumQualifier string
	durationMs     int64
	isrc           string
}

func newMatchInput(track models.Track) matchInput {
	base, qualifier := splitQualifier(track.Name)
	albumBase, albumQualifier := splitQualifier(track.Album)

	artists := make([]string, 0, len(track.Artist))
	for _, a := range track.Artist {
		if n := normalize(a.Name); n != "" {
			artists = append(artists, n)
		}
	}

	return matchInput{
		title:          normalize(track.Name),
		titleBase:      base,
		qualifier:      qualifier,
		artists:        artists,
		album:          normalize(track.Album),
		albumBase:      albumBase,
		albumQualifier: albumQualifier,
		durationMs:     track.DurationMs,
		isrc:           strings.ToUpper(strings.TrimSpace(track.ISRC)),
	}
}

// medleySeparator joins the songs of a combined track. MusicBrainz titles a
// two-song recording "A / B", where a music service reports only the song the
// listener thinks they are playing -- Death Cab's "Stability" is catalogued as
// "Stability / Coney Island (alternate version)".
const medleySeparator = " / "

// titleScore compares the incoming title against a candidate's, and reports
// whether the candidate carries an unmatched variant qualifier.
//
// A medley is scored on its best-matching song rather than the whole string,
// which also decides whose qualifier is judged: in "A / B (alternate version)"
// the qualifier belongs to B, and a play of A should not be penalised for it.
func (in matchInput) titleScore(recTitle string) (score float64, unmatchedVariant bool) {
	// The title as a whole comes first, since plenty of titles contain a slash
	// without being a medley.
	score, unmatchedVariant = in.compareTitle(recTitle)

	if parts := strings.Split(recTitle, medleySeparator); len(parts) > 1 {
		for _, part := range parts {
			if partScore, partVariant := in.compareTitle(part); partScore > score {
				score, unmatchedVariant = partScore, partVariant
			}
		}
	}
	return score, unmatchedVariant
}

// compareTitle rates one title against the incoming play, reporting whether it
// carries a variant qualifier the play never asked for.
func (in matchInput) compareTitle(title string) (float64, bool) {
	base, qualifier := splitQualifier(title)

	// Compare both the full titles and the qualifier-stripped bases, and keep
	// the better reading. Either side may carry a qualifier the other lacks.
	score := math.Max(
		similarity(in.title, normalize(title)),
		similarity(in.titleBase, base),
	)

	unmatched := isVariantQualifier(qualifier) && similarity(in.qualifier, qualifier) < 0.8
	return score, unmatched
}

// artistScore returns the best similarity between any incoming artist name and
// any name on the candidate's artist credit.
func (in matchInput) artistScore(credits []ArtistCredit) float64 {
	if len(in.artists) == 0 || len(credits) == 0 {
		return 0
	}

	candidates := make([]string, 0, len(credits)*2+1)
	var joined strings.Builder
	for _, c := range credits {
		candidates = append(candidates, normalize(c.Name), normalize(c.Artist.Name))
		joined.WriteString(c.Name)
		joined.WriteString(c.Joinphrase)
	}
	// The full credit line, so "Calvin Harris & Dua Lipa" can match an input
	// that kept both names.
	candidates = append(candidates, normalize(joined.String()))

	var best float64
	for _, in := range in.artists {
		for _, c := range candidates {
			best = math.Max(best, similarity(in, c))
		}
	}
	return best
}

// durationScore grades how closely two durations agree. Recordings of the same
// song differ by seconds; an outtake, live take or extended mix differs by far
// more, which makes this the strongest signal available for separating the
// candidates MusicBrainz returns at identical query scores.
func durationScore(a, b int64) float64 {
	delta := a - b
	if delta < 0 {
		delta = -delta
	}
	switch {
	case delta <= 2000:
		return 1.0
	case delta <= 5000:
		return 0.8
	case delta <= 10000:
		return 0.4
	case delta <= 20000:
		return 0.1
	default:
		return 0
	}
}

// albumScore rates the best album the candidate could be attributed to, using
// the same scoring the release picker uses.
//
// Grading on release quality rather than title similarity alone is what couples
// the two choices: several recordings of a song routinely score identically on
// title, artist and duration, and the one that matters is the one that lives on
// a good pressing of the requested album. A recording whose only release is a
// bootleg remix should lose to one on the official issue, even though both
// releases carry the album's name.
func (in matchInput) albumScore(releases []Release, trackTitle string) float64 {
	if in.album == "" || len(releases) == 0 {
		return 0
	}

	var best float64
	for _, r := range releases {
		score, _ := scoreRelease(in, r, trackTitle, nil)
		best = math.Max(best, score)
		if best >= 0.99 {
			break
		}
	}
	return math.Min(1, math.Max(0, best))
}

// albumAgreement is the album score below which a recording is treated as not
// belonging to the album the play named. Correct attributions land near 1;
// scores around a half mean the best release the recording can be attributed to
// is a different record that merely shares an artist.
const albumAgreement = 0.8

// albumDisagrees reports whether a recording contradicts the album the music
// service named. It is the check a mapper's answer has to pass that a ranked
// search result does not need: a search winner already beat every alternative
// on this signal, where a mapper's answer was never compared to anything.
//
// Silent when the play named no album, or when the recording carries no
// releases to judge -- neither is evidence against it.
func (in matchInput) albumDisagrees(rec Recording) bool {
	if in.album == "" || len(rec.Releases) == 0 {
		return false
	}
	return in.albumScore(rec.Releases, rec.Title) < albumAgreement
}

// compareAlbum matches a release title against the incoming album name, trying
// the name as given and with any edition suffix removed, since services report
// "Rumours (Super Deluxe)" where MusicBrainz has "Rumours".
func (in matchInput) compareAlbum(title string) float64 {
	full := normalize(title)
	base, _ := splitQualifier(title)
	return math.Max(
		math.Max(similarity(in.album, full), similarity(in.albumBase, base)),
		math.Max(similarity(in.albumBase, full), similarity(in.album, base)),
	)
}

// scoreRecording rates a candidate in [0,1] against the incoming play and
// returns a human-readable breakdown for the -explain CLI flag and logs.
func scoreRecording(in matchInput, rec Recording) (float64, []string) {
	// An ISRC is a globally unique identifier for a recording. If the candidate
	// carries the one the service gave us, nothing else needs checking.
	if in.isrc != "" {
		for _, isrc := range rec.ISRCs {
			if strings.EqualFold(strings.TrimSpace(isrc), in.isrc) {
				return 1, []string{"isrc=exact"}
			}
		}
	}

	var weighted, total float64
	reasons := make([]string, 0, 6)

	add := func(name string, score, weight float64) {
		weighted += score * weight
		total += weight
		reasons = append(reasons, fmt.Sprintf("%s=%.2f", name, score))
	}

	title, unmatchedVariant := in.titleScore(rec.Title)
	add("title", title, weightTitle)
	add("artist", in.artistScore(rec.ArtistCredit), weightArtist)

	if in.durationMs > 0 && rec.Length > 0 {
		add("duration", durationScore(in.durationMs, int64(rec.Length)), weightDuration)
	}
	if in.album != "" && len(rec.Releases) > 0 {
		add("album", in.albumScore(rec.Releases, rec.Title), weightAlbum)
	}
	if rec.Score > 0 {
		add("mb", float64(rec.Score)/100, weightMBScore)
	}

	if total == 0 {
		return 0, reasons
	}

	score := weighted / total
	if unmatchedVariant {
		score -= qualifierPenalty
		reasons = append(reasons, fmt.Sprintf("variant=-%.2f", qualifierPenalty))
	}
	if rec.Video {
		score -= qualifierPenalty
		reasons = append(reasons, fmt.Sprintf("video=-%.2f", qualifierPenalty))
	}

	return math.Max(0, score), reasons
}

// candidate is a scored recording along with the release chosen for it.
type candidate struct {
	recording Recording
	release   *Release
	score     float64
	reasons   []string
}

func (c candidate) explain() string {
	title := c.recording.Title
	release := "<none>"
	if c.release != nil {
		release = c.release.Title
	}
	return fmt.Sprintf("%.3f  %-45s  %-35s  [%s]",
		c.score, truncate(title, 45), truncate(release, 35), strings.Join(c.reasons, " "))
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
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
