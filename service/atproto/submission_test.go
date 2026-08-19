package atproto

import (
	"testing"
	"time"

	"github.com/teal-fm/piper/models"
)

func mbid(s string) *string { return &s }

func TestTrackToPlayRecord(t *testing.T) {
	recording := "b1a9c0e9-d987-4042-ae91-78d6a3267d69"
	release := "6b47c9a0-b9e1-3df9-a5e8-50a6ce0dbdbd"
	artistMBID := "0383dadf-2a4e-4d10-a46a-e9e041da8eb3"

	track := &models.Track{
		Name:                "Bohemian Rhapsody",
		Artist:              []models.Artist{{Name: "Queen", ID: "spotify-id", MBID: &artistMBID}},
		Album:               "A Night at the Opera",
		ReleaseDiscriminant: "2011 remaster",
		RecordingMBID:       mbid(recording),
		ReleaseMBID:         mbid(release),
		ISRC:                "GBUM71029604",
		URL:                 "https://open.spotify.com/track/abc",
		ServiceBaseUrl:      "open.spotify.com",
		DurationMs:          355106,
		Timestamp:           time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
	}

	got, err := TrackToPlayRecord(track)
	if err != nil {
		t.Fatalf("TrackToPlayRecord() error = %v", err)
	}

	if got.TrackName != "Bohemian Rhapsody" {
		t.Errorf("TrackName = %q", got.TrackName)
	}
	if got.RecordingMbId == nil || *got.RecordingMbId != "mbid:"+recording {
		t.Errorf("RecordingMbId = %v, want the mbid: URI", got.RecordingMbId)
	}
	if got.ReleaseMbId == nil || *got.ReleaseMbId != "mbid:"+release {
		t.Errorf("ReleaseMbId = %v, want the mbid: URI", got.ReleaseMbId)
	}
	// Resolving to the base release must not lose which edition was played.
	if got.ReleaseDiscriminant == nil || *got.ReleaseDiscriminant != "2011 remaster" {
		t.Errorf("ReleaseDiscriminant = %v, want %q", got.ReleaseDiscriminant, "2011 remaster")
	}
	if got.Duration == nil || *got.Duration != 355 {
		t.Errorf("Duration = %v, want 355 seconds", got.Duration)
	}
	if len(got.Artists) != 1 || got.Artists[0].ArtistName != "Queen" {
		t.Fatalf("Artists = %v", got.Artists)
	}
	if got.Artists[0].ArtistMbId == nil || *got.Artists[0].ArtistMbId != "mbid:"+artistMBID {
		t.Errorf("ArtistMbId = %v", got.Artists[0].ArtistMbId)
	}
}

// Optional fields must be omitted rather than sent empty.
func TestTrackToPlayRecordOmitsEmptyFields(t *testing.T) {
	got, err := TrackToPlayRecord(&models.Track{
		Name:   "Dreams",
		Artist: []models.Artist{{Name: "Fleetwood Mac"}},
	})
	if err != nil {
		t.Fatalf("TrackToPlayRecord() error = %v", err)
	}

	if got.RecordingMbId != nil {
		t.Errorf("RecordingMbId = %v, want nil", got.RecordingMbId)
	}
	if got.ReleaseMbId != nil {
		t.Errorf("ReleaseMbId = %v, want nil", got.ReleaseMbId)
	}
	if got.ReleaseDiscriminant != nil {
		t.Errorf("ReleaseDiscriminant = %v, want nil", got.ReleaseDiscriminant)
	}
	if got.Isrc != nil {
		t.Errorf("Isrc = %v, want nil", got.Isrc)
	}
	if got.Duration != nil {
		t.Errorf("Duration = %v, want nil", got.Duration)
	}
}

func TestTrackToPlayRecordRequiresName(t *testing.T) {
	if _, err := TrackToPlayRecord(&models.Track{Artist: []models.Artist{{Name: "Queen"}}}); err == nil {
		t.Error("TrackToPlayRecord() error = nil, want an error for an empty track name")
	}
}
