package musicbrainz

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/time/rate"
)

// loggedService is newTestService with the logs kept rather than discarded.
func loggedService(t *testing.T, routes ...route) (*Service, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	svc := NewMusicBrainzService(nil,
		WithHTTPClient(&http.Client{Transport: &routedTransport{routes: routes}}),
		WithLogger(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	svc.limiter = rate.NewLimiter(rate.Inf, 1)
	return svc, &buf
}

// decodeEvent asserts that exactly one hydration event was written and returns
// it. One play, one line, is the point of the shape, so every test checks it.
func decodeEvent(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	var events []map[string]any
	for line := range strings.Lines(strings.TrimSpace(buf.String())) {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		if parsed["msg"] == eventName {
			events = append(events, parsed)
		}
	}

	if len(events) != 1 {
		t.Fatalf("wrote %d hydration events, want exactly 1:\n%s", len(events), buf)
	}
	return events[0]
}

func group(t *testing.T, event map[string]any, name string) map[string]any {
	t.Helper()
	nested, ok := event[name].(map[string]any)
	if !ok {
		t.Fatalf("event has no %q group:\n%v", name, event)
	}
	return nested
}

// want asserts a field's value, reached by walking nested group names.
func want(t *testing.T, event map[string]any, expected any, path ...string) {
	t.Helper()
	for _, name := range path[:len(path)-1] {
		event = group(t, event, name)
	}
	field := path[len(path)-1]
	if got := event[field]; got != expected {
		t.Errorf("%s = %v, want %v", strings.Join(path, "."), got, expected)
	}
}

func TestHydrationEventRecordsAMatch(t *testing.T) {
	rec := recording("Dreams", "Fleetwood Mac", 257800,
		release("Rumours", "Rumours", "1977-02-04", "US"))

	svc, logs := loggedService(t, route{match: "/ws/2/recording?", body: searchBody(t, rec)})

	play := dreams()
	play.ISRC = "USWB10002225"
	if _, err := svc.Resolve(context.Background(), play); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	event := decodeEvent(t, logs)
	want(t, event, outcomeMatched, "outcome")
	want(t, event, "musicbrainz", "source")
	want(t, event, "musicbrainz", "service")

	want(t, event, "Dreams", "in", "track")
	want(t, event, "Fleetwood Mac", "in", "artist")
	want(t, event, "USWB10002225", "in", "isrc")
	want(t, event, float64(257800), "in", "duration_ms")

	// The MBIDs are what a wrong match has to be traced back to.
	want(t, event, rec.ID, "out", "recording_mbid")
	want(t, event, rec.ArtistCredit[0].Artist.ID, "out", "artist_mbid")
	want(t, event, rec.Releases[0].ID, "out", "release_mbid")
	want(t, event, rec.Releases[0].ReleaseGroup.ID, "out", "release_group_mbid")

	want(t, event, minConfidence, "threshold")
	want(t, event, float64(1), "search", "won_at_tier")
	if _, ok := event["score"]; !ok {
		t.Errorf("event carries no score:\n%v", event)
	}
	if sig := group(t, event, "sig"); len(sig) == 0 {
		t.Error("event carries no scoring signals")
	}
	if requests, _ := group(t, event, "cost")["mb_requests"].(float64); requests < 1 {
		t.Errorf("cost recorded %v requests, want at least 1", requests)
	}
}

// Which user, and which service reported the play, are the first things to
// slice by when a run of plays matches badly. Neither is knowable in here.
func TestHydrationEventCarriesCallerContext(t *testing.T) {
	rec := recording("Dreams", "Fleetwood Mac", 257800,
		release("Rumours", "Rumours", "1977-02-04", "US"))

	svc, logs := loggedService(t, route{match: "/ws/2/recording?", body: searchBody(t, rec)})

	ctx := WithEventContext(context.Background(),
		slog.Int64("user_id", 7), slog.String("play_source", "spotify"))
	if _, err := HydrateTrackContext(ctx, svc, dreams()); err != nil {
		t.Fatalf("HydrateTrackContext() error = %v", err)
	}

	event := decodeEvent(t, logs)
	want(t, event, float64(7), "user_id")
	want(t, event, "spotify", "play_source")
}

// A miss has to name the candidate that came closest and why it fell short, so
// a threshold set too high is visible rather than looking like MusicBrainz
// simply holding nothing.
func TestHydrationEventRecordsANearMiss(t *testing.T) {
	wrong := recording("Bohemian Rhapsody", "Queen", 355106)

	svc, logs := loggedService(t, route{match: "/ws/2/recording?", body: searchBody(t, wrong)})

	if _, err := svc.Resolve(context.Background(), dreams()); err == nil {
		t.Fatal("Resolve() error = nil, want ErrNoConfidentMatch")
	}

	event := decodeEvent(t, logs)
	want(t, event, outcomeUnmatched, "outcome")
	want(t, event, wrong.ID, "out", "recording_mbid")
	if score, _ := event["score"].(float64); score >= minConfidence {
		t.Errorf("score = %v, want something below the threshold", event["score"])
	}
	if sig := group(t, event, "sig"); len(sig) == 0 {
		t.Error("a near miss with no signal breakdown cannot be diagnosed")
	}
}

// ListenBrainz's answer can be accepted, doubted then overruled, or dropped
// unverified. Which of those happened explains a play that went one way today
// and another way yesterday, and it stays on the one line.
func TestHydrationEventRecordsListenBrainzPath(t *testing.T) {
	photoAlbum := release("The Photo Album", "The Photo Album", "2001-10-09", "US")
	doubted := recording("Stability", "Death Cab for Cutie", 740864, photoAlbum)
	doubted.ID = "photo-album-mbid"

	stabilityEP := release("The Stability EP", "The Stability EP", "2002-03-05", "US")
	better := recording("Stability / Coney Island (alternate version)", "Death Cab for Cutie", 741600, stabilityEP)
	better.ID = "stability-ep-mbid"

	agreed := recording("Stability", "Death Cab for Cutie", 741000, stabilityEP)
	agreed.ID = "good-mbid"

	tests := []struct {
		name        string
		routes      []route
		lbMBID      string
		wantOutcome string
		wantDoubt   string
	}{
		{
			name:        "accepted when nothing about it looks doubtful",
			routes:      []route{{match: "recording/good-mbid", body: mustJSON(t, agreed)}},
			lbMBID:      "good-mbid",
			wantOutcome: lbAccepted,
		},
		{
			name: "doubted on the album, then overruled by search",
			routes: []route{
				{match: "recording/photo-album-mbid", body: mustJSON(t, doubted)},
				{match: "/ws/2/recording?", body: searchBody(t, better)},
			},
			lbMBID:      "photo-album-mbid",
			wantOutcome: lbDoubted,
			wantDoubt:   "album",
		},
		{
			name: "dropped when no tier could second it",
			routes: []route{
				{match: "recording/photo-album-mbid", body: mustJSON(t, doubted)},
				{match: "/ws/2/recording?", body: `{"count":0,"recordings":[]}`,
					status: http.StatusInternalServerError, sticky: true},
			},
			lbMBID:      "photo-album-mbid",
			wantOutcome: lbDropped,
			wantDoubt:   "album",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, logs := loggedService(t, tt.routes...)
			svc.listenbrainz = stubListenBrainz{result: &ListenBrainzResult{RecordingMBID: tt.lbMBID}}

			svc.Resolve(context.Background(), stability())

			event := decodeEvent(t, logs)
			want(t, event, tt.wantOutcome, "lb", "outcome")
			want(t, event, tt.lbMBID, "lb", "recording_mbid")
			if tt.wantDoubt != "" {
				want(t, event, tt.wantDoubt, "lb", "disagrees_on")
			}
		})
	}
}

// The event belongs to a hydration. Paths reached outside one -- the search API
// handler calling SearchMusicBrainz -- record nothing.
func TestNoEventOutsideAHydration(t *testing.T) {
	rec := recording("Dreams", "Fleetwood Mac", 257800)

	svc, logs := loggedService(t,
		route{match: "/ws/2/recording?", body: searchBody(t, rec)},
		route{match: "/ws/2/recording/", body: mustJSON(t, rec)})

	if _, err := svc.SearchMusicBrainz(context.Background(), SearchParams{Track: "Dreams"}); err != nil {
		t.Fatalf("SearchMusicBrainz() error = %v", err)
	}
	if _, err := svc.LookupRecording(context.Background(), rec.ID); err != nil {
		t.Fatalf("LookupRecording() error = %v", err)
	}

	if strings.Contains(logs.String(), eventName) {
		t.Errorf("wrote a hydration event outside a hydration:\n%s", logs)
	}
}

func TestLogLevelFromEnv(t *testing.T) {
	tests := map[string]slog.Level{
		"debug":    slog.LevelDebug,
		"WARN":     slog.LevelWarn,
		"":         slog.LevelInfo,
		"nonsense": slog.LevelInfo,
	}

	for value, want := range tests {
		t.Setenv("LOG_LEVEL", value)
		if got := logLevel(); got != want {
			t.Errorf("LOG_LEVEL=%q gave %v, want %v", value, got, want)
		}
	}
}
