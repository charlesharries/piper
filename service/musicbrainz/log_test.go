package musicbrainz

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// loggedService returns a service whose logs are captured, so a test can read
// back what a lookup recorded about itself.
func loggedService(t *testing.T) (*Service, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return NewMusicBrainzService(nil, WithLogger(slog.New(handler))), &buf
}

// matched builds a resolved play covering every identifier a log line should
// carry: recording, release, release group and artist.
func matched() (rec Recording, release *Release) {
	rec = Recording{
		ID:    "e97f805a-ab48-4c52-855e-07049142113d",
		Title: "Dreams",
		ArtistCredit: []ArtistCredit{{
			Name:   "Fleetwood Mac",
			Artist: Artist{ID: "bd13909f-1c29-4c27-a874-d4aaf27c5b1a", Name: "Fleetwood Mac"},
		}},
	}
	release = &Release{
		ID:           "0b7b1a3f-cf29-4f1a-b0a7-2e2b0a3b21f1",
		Title:        "Rumours",
		ReleaseGroup: &ReleaseGroup{ID: "0d0e9e5b-6b0e-4a44-b3d5-8b3a7d0f0f5f", Title: "Rumours"},
	}
	return rec, release
}

func TestLogMatchRecordsMBIDs(t *testing.T) {
	svc, logs := loggedService(t)
	rec, release := matched()

	svc.logMatch(track("Dreams", "Fleetwood Mac", "Rumours", ""), &Match{
		Recording: rec,
		Release:   release,
		Score:     0.94,
		Source:    "musicbrainz",
		Candidates: []candidate{{
			recording: rec,
			release:   release,
			score:     0.94,
			reasons:   signals{{"title", 1}, {"artist", 1}, {"conflict", -0.25}},
		}},
	})

	got := logs.String()
	for _, want := range []string{
		"msg=matched",
		"service=musicbrainz",
		"recording_mbid=" + rec.ID,
		"artist_mbid=" + rec.ArtistCredit[0].Artist.ID,
		"release_mbid=" + release.ID,
		"release_group_mbid=" + release.ReleaseGroup.ID,
		"score=0.94",
		"threshold=0.62",
		"source=musicbrainz",
		"sig_title=1",
		"sig_conflict=-0.25",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
}

// A rejection is the line that gets read when a play went unmatched, so it has
// to name the recording that came closest rather than only its title.
func TestLogNoMatchRecordsNearMiss(t *testing.T) {
	svc, logs := loggedService(t)
	rec, release := matched()

	svc.logNoMatch(track("Dreams", "Fleetwood Mac", "Rumours", ""), []candidate{{
		recording: rec,
		release:   release,
		score:     0.41,
		reasons:   signals{{"title", 1}, {"uncorroborated", -0.4}},
	}})

	got := logs.String()
	for _, want := range []string{
		"msg=unmatched",
		"reason=below_threshold",
		"recording_mbid=" + rec.ID,
		"release_mbid=" + release.ID,
		"score=0.41",
		"sig_uncorroborated=-0.4",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
}

func TestLogNoMatchWithoutCandidates(t *testing.T) {
	svc, logs := loggedService(t)

	svc.logNoMatch(track("Nonsense", "Nobody", "", ""), nil)

	got := logs.String()
	if !strings.Contains(got, "msg=unmatched") || !strings.Contains(got, "reason=no_candidates") {
		t.Errorf("want an unmatched line naming no_candidates, got:\n%s", got)
	}
	if strings.Contains(got, "recording_mbid") {
		t.Errorf("nothing was matched, so no recording should be named:\n%s", got)
	}
}

// A partially resolved match still has to log: a rejected ListenBrainz answer
// has no release yet, and a play with no artist credit must not panic the way
// an index would.
func TestRecordingAttrsTolerateMissingData(t *testing.T) {
	attrs := recordingAttrs(Recording{ID: "abc", Title: "Dreams"}, nil)

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("partial", attrs...)

	got := buf.String()
	if !strings.Contains(got, "recording_mbid=abc") {
		t.Errorf("want the recording MBID, got:\n%s", got)
	}
	for _, unwanted := range []string{"release_mbid", "release_group_mbid", "artist_mbid"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("nothing supplied %s, so it should be absent:\n%s", unwanted, got)
		}
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
