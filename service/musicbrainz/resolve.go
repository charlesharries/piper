package musicbrainz

import (
	"cmp"
	"context"
	"errors"

	"github.com/teal-fm/piper/models"
)

// Match is the outcome of resolving a play against MusicBrainz.
type Match struct {
	Recording Recording
	Release   *Release
	Score     float64
	// Source names the backend that produced the match, for logging.
	Source string
	// Candidates holds every scored alternative, best first, for -explain.
	Candidates []candidate
}

// signals returns the breakdown behind the winning score.
func (m *Match) signals() signals {
	if len(m.Candidates) == 0 {
		return nil
	}
	return m.Candidates[0].reasons
}

// Explain renders the scored candidates, best first, for diagnosing why a
// lookup landed where it did.
func (m *Match) Explain() []string {
	lines := make([]string, 0, len(m.Candidates))
	for _, c := range m.Candidates {
		lines = append(lines, c.explain())
	}
	return lines
}

// ErrNoConfidentMatch is returned when nothing scored well enough to publish.
// A wrong MBID is worse than none: the record goes to the user's repo and
// misattributes their listening.
var ErrNoConfidentMatch = errors.New("no confident MusicBrainz match")

// Resolve finds the best MusicBrainz recording and release for a play. It asks
// ListenBrainz first where one is configured, then works down the search
// ladder, scoring every candidate locally rather than trusting MusicBrainz's
// own ordering, and stops at the first tier that produces a confident match.
// It logs nothing as it goes; see event.go.
func (s *Service) Resolve(ctx context.Context, track models.Track) (*Match, error) {
	ctx, ev := startEvent(ctx, s.logger, track)
	defer ev.emit()

	in := newMatchInput(track)

	// ListenBrainz beats raw search on messy input, but it needs a token.
	var incumbent *Match
	if s.listenbrainz != nil {
		match, believed := s.listenBrainzOpinion(ctx, in, track)
		if believed {
			ev.matched(match)
			return match, nil
		}
		incumbent = match
	}

	// closest is the best-scoring ranking any tier produced, kept so that a play
	// nothing matched is logged as the near miss it was.
	var closest []candidate
	var lastErr error
	// Whether any tier actually reached MusicBrainz. A doubted answer that
	// survives a search is a different thing from one that was never checked.
	var searched bool

	for _, t := range s.searchTiers(track) {
		t, ok := s.scopeTier(ctx, t)
		if !ok {
			continue
		}

		ev.tiersRun++
		recordings, err := s.search(ctx, t.searchRequest)
		if err != nil {
			lastErr = err
			ev.tiersFailed++
			ev.noteErr(err)
			continue
		}
		searched = true

		ranked := rankCandidates(in, recordings)
		if len(ranked) > 0 && (len(closest) == 0 || ranked[0].score > closest[0].score) {
			closest = ranked
		}
		if len(ranked) == 0 || ranked[0].score < minConfidence {
			continue
		}

		best := ranked[0]
		// A doubted ListenBrainz answer was not discarded; it still wins if
		// search cannot do better.
		if incumbent != nil && incumbent.Score >= best.score {
			ev.matched(incumbent)
			return incumbent, nil
		}

		// Only the winner gets a release: the losers would each need their own
		// lookup. -explain still shows it, which is what is being diagnosed
		// when the recording was right and the pressing was not.
		release := s.resolveRelease(ctx, in, best.recording)
		ranked[0].release = release
		match := &Match{
			Recording:  best.recording,
			Release:    release,
			Score:      best.score,
			Source:     "musicbrainz",
			Candidates: ranked,
		}
		ev.wonAtTier = t.name
		ev.matched(match)
		return match, nil
	}

	// Search ran, could not better the doubted answer, so it still stands. When
	// no tier could be reached, though, the second opinion never arrived, and
	// publishing anyway would put a recording we distrusted into the user's
	// repo on the strength of a MusicBrainz outage.
	if incumbent != nil && searched {
		ev.matched(incumbent)
		return incumbent, nil
	}
	if incumbent != nil {
		ev.lbOutcome = lbDropped
	}
	if lastErr != nil && len(closest) == 0 {
		ev.outcome = outcomeError
		return nil, lastErr
	}
	ev.unmatched(closest)
	return &Match{Candidates: closest}, ErrNoConfidentMatch
}

// ListenBrainzResult is ListenBrainz's answer for a play. Only the recording is
// taken on its word: its release id was dropped because ListenBrainz picks a
// different pressing for nearly every track of an album, which scattered them.
type ListenBrainzResult struct {
	RecordingMBID string
}

// ListenBrainzClient resolves a play to MusicBrainz identifiers via
// ListenBrainz. It is optional: when absent, resolution falls back to search.
type ListenBrainzClient interface {
	Lookup(ctx context.Context, artist, recording, release string) (*ListenBrainzResult, error)
}

// listenBrainzOpinion asks ListenBrainz for an answer and decides how far to
// believe it: nil when there is nothing worth keeping, believed when it can be
// published without searching, otherwise kept as the incumbent for search to
// try to beat.
func (s *Service) listenBrainzOpinion(ctx context.Context, in matchInput, track models.Track) (match *Match, believed bool) {
	ev := eventFrom(ctx)
	ev.lbAttempted, ev.lbOutcome = true, lbMiss

	match, err := s.resolveViaListenBrainz(ctx, in, track)
	if err != nil {
		ev.lbOutcome = lbError
		ev.noteErr(err)
	}
	if match == nil {
		return nil, false
	}
	ev.lbMBID, ev.lbScore = match.Recording.ID, match.Score

	// A ListenBrainz answer beat no alternatives, so clearing minConfidence says
	// less about it than the same score does for a ranked search result.
	if reason, doubted := match.signals().disagreement(); doubted {
		ev.lbOutcome, ev.lbDoubt = lbDoubted, reason
		return match, false
	}
	ev.lbOutcome = lbAccepted
	return match, true
}

// disagreement reports whether what the music service told us argues against a
// candidate, naming the signal that objected. It reads the breakdown
// scoreRecording already produced, so a signal missing from it -- no album
// named, no length either side, an ISRC that settled it -- stays silent.
func (s signals) disagreement() (string, bool) {
	// Length first: it catches a bootleg pressing catalogued under the album's
	// own title, which the album test cannot see.
	if duration, ok := s.value("duration"); ok && duration == 0 {
		return "length", true
	}
	if album, ok := s.value("album"); ok && album < albumAgreement {
		return "album", true
	}
	return "", false
}

// resolveViaListenBrainz fetches and scores ListenBrainz's answer. It can be
// confidently wrong, so the same scoring that guards search results guards it.
func (s *Service) resolveViaListenBrainz(ctx context.Context, in matchInput, track models.Track) (*Match, error) {
	res, err := s.listenbrainz.Lookup(ctx, primaryArtist(track), track.Name, searchAlbum(track.Album))
	if err != nil {
		return nil, err
	}
	if res == nil || res.RecordingMBID == "" {
		return nil, nil
	}

	rec, err := s.LookupRecording(ctx, res.RecordingMBID)
	if err != nil {
		return nil, err
	}

	score, reasons := scoreRecording(in, *rec)
	if score < minConfidence {
		ev := eventFrom(ctx)
		ev.lbOutcome, ev.lbMBID, ev.lbScore = lbRejected, rec.ID, score
		return nil, nil
	}

	release := s.resolveRelease(ctx, in, *rec)

	return &Match{
		Recording:  *rec,
		Release:    release,
		Score:      score,
		Source:     "listenbrainz",
		Candidates: []candidate{{recording: *rec, release: release, score: score, reasons: reasons}},
	}, nil
}

// resolveRelease picks the album to attribute a play to, then makes a pass for
// a pressing that has cover art -- the release MBID is the only identifier in a
// play record that resolves to one.
func (s *Service) resolveRelease(ctx context.Context, in matchInput, rec Recording) *Release {
	return s.preferReleaseWithArt(ctx, in, rec, s.pickRelease(ctx, in, rec))
}

// pickRelease chooses the release on metadata grounds alone. When the list that
// came back with the recording yields nothing convincing, it pays for one more
// request to fetch the full one, which search results carry only a slice of.
func (s *Service) pickRelease(ctx context.Context, in matchInput, rec Recording) *Release {
	release, score := bestRelease(in, rec.Releases, rec.Title, nil)
	if release != nil && score >= releaseConfidence {
		return release
	}

	full, err := s.LookupRecording(ctx, rec.ID)
	if err != nil {
		// The search result's own release list still stands.
		eventFrom(ctx).noteErr(err)
		return release
	}

	better, betterScore := bestRelease(in, full.Releases, rec.Title, nil)
	if better != nil && (release == nil || betterScore > score) {
		return better
	}
	return release
}

// HydrateTrack enriches a play with MusicBrainz identifiers, returning
// ErrNoConfidentMatch rather than a guess when nothing matches, so callers can
// keep publishing what the music service gave them. The context is the
// caller's own, so the hydration is cancelled with whatever started it and
// anything attached with WithEventContext reaches the event.
func (s *Service) HydrateTrack(ctx context.Context, track models.Track) (*models.Track, error) {
	match, err := s.Resolve(ctx, track)
	if err != nil {
		return nil, err
	}
	return ApplyMatch(track, match), nil
}

// ApplyMatch merges a resolved match into a play. Fields the music service
// supplied are preserved wherever MusicBrainz has nothing better: its data is
// more complete, but the service's is what the user actually played.
func ApplyMatch(track models.Track, match *Match) *models.Track {
	result := track

	result.RecordingMBID = &match.Recording.ID
	// MusicBrainz omits lengths on plenty of recordings, and a zero would erase
	// the real duration -- which for Spotify is what decides when to stamp.
	result.DurationMs = cmp.Or(int64(match.Recording.Length), track.DurationMs)

	if len(match.Recording.ISRCs) > 0 {
		result.ISRC = cmp.Or(track.ISRC, match.Recording.ISRCs[0])
	}

	if len(match.Recording.ArtistCredit) > 0 {
		result.Artist = mergeArtists(track.Artist, match.Recording.ArtistCredit)
	}

	if match.Release != nil {
		result.ReleaseMBID = &match.Release.ID
		// Resolving to the base release drops the edition the service reported,
		// so keep it rather than lose that a remaster was played.
		result.ReleaseDiscriminant = cmp.Or(track.ReleaseDiscriminant, releaseDiscriminant(track.Album))
		result.Album = match.Release.Title
	}

	return &result
}

// mergeArtists attaches MusicBrainz IDs to the artist credit without discarding
// the service's own artist IDs, the only link back to the source catalogue.
func mergeArtists(existing []models.Artist, credits []ArtistCredit) []models.Artist {
	byName := make(map[string]models.Artist, len(existing))
	for _, a := range existing {
		byName[normalize(a.Name)] = a
	}

	artists := make([]models.Artist, len(credits))
	for i, c := range credits {
		mbid := c.Artist.ID
		artist := models.Artist{Name: c.Name, MBID: &mbid}
		if prior, ok := byName[normalize(c.Name)]; ok {
			artist.ID = prior.ID
		}
		artists[i] = artist
	}
	return artists
}
