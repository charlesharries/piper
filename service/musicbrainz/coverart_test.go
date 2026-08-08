package musicbrainz

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/teal-fm/piper/models"
)

// browseBody renders a release browse response, marking the releases whose
// MBIDs appear in withArt as having a front cover.
func browseBody(t *testing.T, withArt map[string]bool, releases ...Release) string {
	t.Helper()

	type coverArt struct {
		Front bool `json:"front"`
	}
	type browsed struct {
		Release
		CoverArtArchive coverArt `json:"cover-art-archive"`
	}

	body := struct {
		Releases []browsed `json:"releases"`
	}{}
	for _, r := range releases {
		body.Releases = append(body.Releases, browsed{
			Release:         r,
			CoverArtArchive: coverArt{Front: withArt[r.ID]},
		})
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshalling browse body: %v", err)
	}
	return string(encoded)
}

func dreams() models.Track {
	return models.Track{
		Name:       "Dreams",
		Artist:     []models.Artist{{Name: "Fleetwood Mac"}},
		Album:      "Rumours",
		DurationMs: 257800,
	}
}

// The release MBID is the only identifier in a play record that resolves to
// cover art, so a pressing without art should lose to an equally good one that
// has it.
func TestResolvePrefersPressingWithCoverArt(t *testing.T) {
	bare := release("Rumours", "Rumours", "1977-02-04", "US")
	withArt := release("Rumours", "Rumours", "1977-06-01", "GB")

	rec := recording("Dreams", "Fleetwood Mac", 257800, bare)

	svc, _ := newTestService(t,
		route{match: "/ws/2/release?", body: browseBody(t, map[string]bool{withArt.ID: true}, bare, withArt)},
		route{match: "/ws/2/recording?", body: searchBody(t, rec)},
	)

	match, err := svc.Resolve(context.Background(), dreams())
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if match.Release == nil {
		t.Fatal("no release chosen")
	}
	if match.Release.ID != withArt.ID {
		t.Errorf("release = %s, want %s (the pressing with cover art)", match.Release.ID, withArt.ID)
	}
}

// Artwork is a tiebreaker, not an override: a pressing of the wrong album must
// not win just because it has a cover.
func TestResolveKeepsRightAlbumOverCoverArt(t *testing.T) {
	correct := release("Rumours", "Rumours", "1977-02-04", "US")
	wrong := release("Greatest Hits", "Greatest Hits", "1988-11-22", "US", "Compilation")

	rec := recording("Dreams", "Fleetwood Mac", 257800, correct)

	svc, _ := newTestService(t,
		route{match: "/ws/2/release?", body: browseBody(t, map[string]bool{wrong.ID: true}, correct, wrong)},
		route{match: "/ws/2/recording?", body: searchBody(t, rec)},
	)

	match, err := svc.Resolve(context.Background(), dreams())
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if match.Release == nil || match.Release.ID != correct.ID {
		t.Errorf("release = %v, want %s", match.Release, correct.ID)
	}
}

// A browse that fails or returns nothing must leave the metadata choice intact
// rather than dropping the release.
func TestResolveKeepsReleaseWhenBrowseFails(t *testing.T) {
	only := release("Rumours", "Rumours", "1977-02-04", "US")
	rec := recording("Dreams", "Fleetwood Mac", 257800, only)

	svc, _ := newTestService(t,
		route{match: "/ws/2/release?", status: 503, body: `{"releases":[]}`},
		route{match: "/ws/2/recording?", body: searchBody(t, rec)},
	)

	match, err := svc.Resolve(context.Background(), dreams())
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if match.Release == nil || match.Release.ID != only.ID {
		t.Errorf("release = %v, want %s", match.Release, only.ID)
	}
}

// The browse is per release group and cached, so replaying an album must not
// pay for it again.
func TestPressingsAreCachedPerReleaseGroup(t *testing.T) {
	bare := release("Rumours", "Rumours", "1977-02-04", "US")
	withArt := release("Rumours", "Rumours", "1977-06-01", "GB")
	rec := recording("Dreams", "Fleetwood Mac", 257800, bare)

	svc, transport := newTestService(t,
		route{match: "/ws/2/release?", body: browseBody(t, map[string]bool{withArt.ID: true}, bare, withArt)},
		route{match: "/ws/2/recording?", body: searchBody(t, rec)},
	)

	for range 3 {
		if _, err := svc.Resolve(context.Background(), dreams()); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}

	browses := len(transport.requested) - len(transport.searches())
	if browses != 1 {
		t.Errorf("made %d browses, want 1", browses)
	}
}
