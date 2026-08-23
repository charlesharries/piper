package musicbrainz

import (
	"context"
	"log/slog"
	"time"

	"github.com/teal-fm/piper/models"
)

// Hydrating a play is one unit of work made of a dozen decisions. Logging them
// as they happen scatters the interesting fields across lines that nothing
// correlates, and concurrent trackers interleave. So each step records what it
// learned on an event carried on the context, and Resolve emits the lot as one
// wide line on the way out. One play, one event.

const eventName = "track_hydrated"

const (
	outcomeMatched   = "matched"
	outcomeUnmatched = "unmatched"
	outcomeError     = "error"
)

// What became of ListenBrainz's answer.
const (
	lbAccepted = "accepted"
	lbDoubted  = "doubted" // kept as incumbent, but search got a chance to better it
	lbRejected = "rejected"
	lbDropped  = "dropped" // doubted, and no search ran to second it
	lbMiss     = "miss"
	lbError    = "error"
)

// What became of the artwork pass over the release group.
const (
	artHadArt       = "had_art"
	artSwapped      = "swapped"
	artNoneInGroup  = "no_art_in_group"
	artKept         = "kept_without_art"
	artNotAttempted = "not_attempted"
)

// Whether an artist name resolved to an MBID for a scoped search tier.
const (
	artistResolved   = "resolved"
	artistUnresolved = "unresolved"
	artistFailed     = "failed"
)

type eventKey struct{}
type callerKey struct{}

// WithEventContext attaches dimensions this package cannot know -- which user,
// which music service reported the play -- to the hydration event.
func WithEventContext(ctx context.Context, attrs ...slog.Attr) context.Context {
	prior, _ := ctx.Value(callerKey{}).([]slog.Attr)
	return context.WithValue(ctx, callerKey{}, append(append([]slog.Attr{}, prior...), attrs...))
}

// event rides on the context so nothing in the resolve chain needs a new
// parameter. One event per Resolve, never shared, so no locking.
type event struct {
	logger  *slog.Logger
	started time.Time
	caller  []slog.Attr
	in      models.Track

	outcome string
	match   *Match
	reasons signals
	err     error

	candidates    int
	runnerUpScore float64
	hasRunnerUp   bool

	// The lb group is emitted only once a lookup is attempted, so its absence
	// means ListenBrainz has no token rather than nothing to say.
	lbAttempted bool
	lbOutcome   string
	lbMBID      string
	lbScore     float64
	lbDoubt     string

	tiersRun    int
	tiersFailed int
	wonAtTier   int // 1-based; 0 when search did not produce the winner
	artistScope string

	artOutcome string
	artFrom    string

	requests int
}

func startEvent(ctx context.Context, logger *slog.Logger, track models.Track) (context.Context, *event) {
	caller, _ := ctx.Value(callerKey{}).([]slog.Attr)
	e := &event{
		logger:     logger,
		started:    time.Now(),
		caller:     caller,
		in:         track,
		outcome:    outcomeUnmatched,
		artOutcome: artNotAttempted,
	}
	return context.WithValue(ctx, eventKey{}, e), e
}

// eventFrom never returns nil, so recording a field is always a plain
// assignment. Paths reachable outside a hydration -- SearchMusicBrainz from the
// search handler -- get a throwaway that is never emitted.
func eventFrom(ctx context.Context) *event {
	if e, ok := ctx.Value(eventKey{}).(*event); ok {
		return e
	}
	return &event{}
}

func (e *event) matched(match *Match) {
	e.outcome, e.match = outcomeMatched, match
	if len(match.Candidates) > 0 {
		e.reasons = match.Candidates[0].reasons
	}
	e.rank(match.Candidates)
}

// unmatched keeps the candidate that came closest, so a threshold set too high
// reads as a near miss rather than as MusicBrainz holding nothing.
func (e *event) unmatched(candidates []candidate) {
	e.rank(candidates)
	if len(candidates) == 0 {
		return
	}
	best := candidates[0]
	e.match = &Match{Recording: best.recording, Release: best.release, Score: best.score}
	e.reasons = best.reasons
}

// rank records the pool the answer came from. The gap to the runner-up is how
// close the call was; a match that won by a hair is the kind that turns out wrong.
func (e *event) rank(candidates []candidate) {
	e.candidates = len(candidates)
	if len(candidates) > 1 {
		e.runnerUpScore, e.hasRunnerUp = candidates[1].score, true
	}
}

func (e *event) noteErr(err error) {
	if err != nil {
		e.err = err
	}
}

// emit deliberately does not pass the resolve's context: a hydration cancelled
// halfway is the one most worth having a record of.
func (e *event) emit() {
	e.logger.LogAttrs(context.Background(), slog.LevelInfo, eventName, e.attrs()...)
}

func (e *event) attrs() []slog.Attr {
	attrs := []slog.Attr{
		slog.String("outcome", e.outcome),
		slog.Int64("duration_ms", time.Since(e.started).Milliseconds()),
	}
	attrs = append(attrs, e.caller...)
	if e.match != nil && e.match.Source != "" {
		attrs = append(attrs, slog.String("source", e.match.Source))
	}

	attrs = append(attrs, slog.Group("in", e.inputAttrs()...))
	if e.match != nil && e.match.Recording.ID != "" {
		attrs = append(attrs, slog.Group("out", recordingAttrs(e.match.Recording, e.match.Release)...))
	}
	attrs = append(attrs, e.scoreAttrs()...)
	if len(e.reasons) > 0 {
		attrs = append(attrs, slog.Group("sig", e.reasons.attrs()...))
	}
	if e.lbAttempted {
		attrs = append(attrs, slog.Group("lb", e.listenbrainzAttrs()...))
	}
	attrs = append(attrs,
		slog.Group("search", e.searchAttrs()...),
		slog.Group("art", e.artAttrs()...),
		slog.Group("cost", slog.Int("mb_requests", e.requests)))
	if e.err != nil {
		attrs = append(attrs, slog.String("err", e.err.Error()))
	}
	return attrs
}

func (e *event) inputAttrs() []any {
	attrs := []any{
		slog.String("track", e.in.Name),
		slog.String("artist", primaryArtist(e.in)),
		slog.String("album", e.in.Album),
	}
	if e.in.ISRC != "" {
		attrs = append(attrs, slog.String("isrc", e.in.ISRC))
	}
	if e.in.DurationMs > 0 {
		attrs = append(attrs, slog.Int64("duration_ms", e.in.DurationMs))
	}
	return attrs
}

func (e *event) scoreAttrs() []slog.Attr {
	if e.match == nil {
		return nil
	}
	attrs := []slog.Attr{
		slog.Float64("score", logRound(e.match.Score)),
		slog.Float64("threshold", minConfidence),
		slog.Int("candidates", e.candidates),
	}
	if e.hasRunnerUp {
		attrs = append(attrs, slog.Float64("runner_up_score", logRound(e.runnerUpScore)))
	}
	return attrs
}

func (e *event) listenbrainzAttrs() []any {
	attrs := []any{slog.String("outcome", e.lbOutcome)}
	if e.lbMBID != "" {
		attrs = append(attrs,
			slog.String("recording_mbid", e.lbMBID),
			slog.Float64("score", logRound(e.lbScore)))
	}
	if e.lbDoubt != "" {
		attrs = append(attrs, slog.String("disagrees_on", e.lbDoubt))
	}
	return attrs
}

func (e *event) searchAttrs() []any {
	attrs := []any{
		slog.Int("tiers_run", e.tiersRun),
		slog.Int("tiers_failed", e.tiersFailed),
	}
	if e.wonAtTier > 0 {
		attrs = append(attrs, slog.Int("won_at_tier", e.wonAtTier))
	}
	if e.artistScope != "" {
		attrs = append(attrs, slog.String("artist_scope", e.artistScope))
	}
	return attrs
}

func (e *event) artAttrs() []any {
	attrs := []any{slog.String("outcome", e.artOutcome)}
	if e.artFrom != "" {
		attrs = append(attrs, slog.String("from_release_mbid", e.artFrom))
	}
	return attrs
}
