package musicbrainz

import (
	"strings"
	"testing"

	"github.com/teal-fm/piper/models"
)

// The album the service reported wins over an earlier release, because "oldest"
// systematically favoured obscure pressings with no cover art.
func TestBestReleasePrefersTheReportedAlbum(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Dreams", Album: "Rumours"})

	releases := []Release{
		release("Fleetwood Mac Live", "Fleetwood Mac Live", "1975-01-01", "US"),
		release("Rumours", "Rumours", "1977-02-04", "US"),
		release("25 Years: The Chain", "25 Years: The Chain", "1992-11-24", "GB", "Compilation"),
	}

	got, _, reasons := bestRelease(in, releases, "Dreams", nil)
	if got == nil {
		t.Fatal("expected a release")
	}
	if got.Title != "Rumours" {
		t.Errorf("chose %q (%v), want %q", got.Title, reasons, "Rumours")
	}
}

// Services report "Rumours (Super Deluxe)"; MusicBrainz holds "Rumours".
func TestBestReleaseMatchesThroughEditionSuffix(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Dreams", Album: "Rumours (Super Deluxe)"})

	releases := []Release{
		release("Greatest Hits", "Greatest Hits", "1988-11-22", "US", "Compilation"),
		release("Rumours", "Rumours", "1977-02-04", "US"),
	}

	got, _, reasons := bestRelease(in, releases, "Dreams", nil)
	if got == nil || got.Title != "Rumours" {
		t.Fatalf("chose %v (%v), want Rumours", got, reasons)
	}
}

// The release is titled "Power Corruption and Lies" but the group carries the
// name the service reports.
func TestBestReleaseMatchesOnReleaseGroupTitle(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Blue Monday", Album: "Power, Corruption & Lies"})

	releases := []Release{
		release("Substance", "Substance", "1987-08-17", "GB", "Compilation"),
		release("Power Corruption and Lies", "Power, Corruption & Lies", "1983-05-02", "GB"),
	}

	got, _, reasons := bestRelease(in, releases, "Blue Monday", nil)
	if got == nil || got.Title != "Power Corruption and Lies" {
		t.Fatalf("chose %v (%v), want the release group match", got, reasons)
	}
}

// Compilations are penalised, not excluded: the old code filtered them out
// entirely, which meant a play of a genuine compilation fell through to an
// arbitrary pick.
func TestBestReleaseAllowsCompilationWhenItIsTheAlbum(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Bohemian Rhapsody", Album: "Greatest Hits"})

	releases := []Release{
		release("A Night at the Opera", "A Night at the Opera", "1975-11-21", "GB"),
		release("Greatest Hits", "Greatest Hits", "1981-10-26", "GB", "Compilation"),
	}

	got, _, reasons := bestRelease(in, releases, "Bohemian Rhapsody", nil)
	if got == nil || got.Title != "Greatest Hits" {
		t.Fatalf("chose %v (%v), want Greatest Hits", got, reasons)
	}
}

// The same soundtrack case: a secondary type must not disqualify the album the
// user is actually playing.
func TestBestReleaseAllowsSoundtrackWhenItIsTheAlbum(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Speak to Me", Album: "Guardians of the Galaxy"})

	releases := []Release{
		release("Guardians of the Galaxy", "Guardians of the Galaxy", "2014-07-29", "US", "Soundtrack"),
		release("Some Other Record", "Some Other Record", "1970-01-01", "US"),
	}

	got, _, reasons := bestRelease(in, releases, "Speak to Me", nil)
	if got == nil || got.Title != "Guardians of the Galaxy" {
		t.Fatalf("chose %v (%v), want the soundtrack", got, reasons)
	}
}

// With no album to go on, prefer an official broad-market album over a
// compilation or a single named after the track.
func TestBestReleaseWithoutAlbumHint(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Bohemian Rhapsody"})

	releases := []Release{
		release("Bohemian Rhapsody", "Bohemian Rhapsody", "1975-10-31", "GB"),
		release("Classic Queen", "Classic Queen", "1992-03-03", "US", "Compilation"),
		release("A Night at the Opera", "A Night at the Opera", "1975-11-21", "GB"),
	}

	got, _, reasons := bestRelease(in, releases, "Bohemian Rhapsody", nil)
	if got == nil || got.Title != "A Night at the Opera" {
		t.Fatalf("chose %v (%v), want A Night at the Opera", got, reasons)
	}
}

// Bootlegs and promos lose to official releases.
func TestBestReleasePrefersOfficial(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Dreams", Album: "Rumours"})

	bootleg := release("Rumours", "Rumours", "1977-01-01", "US")
	bootleg.Status = "Bootleg"
	official := release("Rumours", "Rumours", "1977-02-04", "US")

	got, _, reasons := bestRelease(in, []Release{bootleg, official}, "Dreams", nil)
	if got == nil || got.Status != "Official" {
		t.Fatalf("chose %v (%v), want the official release", got, reasons)
	}
}

// When two releases are otherwise equal, a known cover art holder wins.
func TestBestReleasePrefersKnownCoverArt(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Dreams", Album: "Rumours"})

	plain := release("Rumours", "Rumours", "1977-02-04", "US")
	withArt := release("Rumours", "Rumours", "1977-02-04", "GB")

	got, _, reasons := bestRelease(in, []Release{plain, withArt}, "Dreams", map[string]bool{withArt.ID: true})
	if got == nil || got.ID != withArt.ID {
		t.Fatalf("chose %v (%v), want the release with known art", got, reasons)
	}
}

// A play that named no edition must not be attributed to a remix or
// anniversary pressing that happens to share the album's name.
func TestBestReleaseRejectsUnaskedForEdition(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Whole Lotta Love", Album: "How the West Was Won"})

	releases := []Release{
		release("How the West Was Won (JRK remix)", "How the West Was Won", "2003-05-27", "US"),
		release("How the West Was Won", "How the West Was Won", "2003-05-27", "US"),
	}

	got, _, reasons := bestRelease(in, releases, "Whole Lotta Love", nil)
	if got == nil || got.Title != "How the West Was Won" {
		t.Fatalf("chose %v (%v), want the plain release", got, reasons)
	}
}

// But when the play does name an edition, the deluxe pressing is a legitimate
// answer and must not be penalised out of contention.
func TestBestReleaseAllowsRequestedEdition(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Dreams", Album: "Rumours (Super Deluxe)"})

	releases := []Release{
		release("Rumours (super deluxe)", "Rumours", "2013-01-25", "US"),
		release("Rumours", "Rumours", "1977-02-04", "US"),
	}

	got, _, reasons := bestRelease(in, releases, "Dreams", nil)
	if got == nil {
		t.Fatalf("expected a release (%v)", reasons)
	}
	if !strings.HasPrefix(got.Title, "Rumours") {
		t.Errorf("chose %q, want a Rumours pressing", got.Title)
	}
}

// Ties fall back to the original issue, and must be stable across runs.
func TestBestReleaseTieBreaksOnDate(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Dreams", Album: "Rumours"})

	reissue := release("Rumours", "Rumours", "2011-01-24", "US")
	original := release("Rumours", "Rumours", "1977-02-04", "US")

	for range 5 {
		got, _, _ := bestRelease(in, []Release{reissue, original}, "Dreams", nil)
		if got == nil || got.Date != "1977-02-04" {
			t.Fatalf("chose %v, want the original issue", got)
		}
	}
}

func TestBestReleaseEmpty(t *testing.T) {
	got, _, _ := bestRelease(newMatchInput(models.Track{}), nil, "Dreams", nil)
	if got != nil {
		t.Errorf("bestRelease() = %v, want nil", got)
	}
}

// The caller's slice belongs to a cached Recording and must survive intact.
func TestBestReleaseDoesNotMutateInput(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Dreams", Album: "Rumours"})

	releases := []Release{
		release("Greatest Hits", "Greatest Hits", "1988-11-22", "US", "Compilation"),
		release("Rumours", "Rumours", "1977-02-04", "US"),
	}
	before := []string{releases[0].Title, releases[1].Title}

	if _, _, _ = bestRelease(in, releases, "Dreams", nil); releases[0].Title != before[0] || releases[1].Title != before[1] {
		t.Errorf("bestRelease reordered its input: %v", releases)
	}
}

func TestReleaseDiscriminant(t *testing.T) {
	tests := []struct {
		album string
		want  string
	}{
		{album: "Rumours (Super Deluxe)", want: "super deluxe"},
		{album: "Abbey Road - 2019 Remaster", want: "2019 remaster"},
		{album: "Rumours", want: ""},
		{album: "Guardians of the Galaxy", want: ""},
		{album: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.album, func(t *testing.T) {
			if got := releaseDiscriminant(tt.album); got != tt.want {
				t.Errorf("releaseDiscriminant(%q) = %q, want %q", tt.album, got, tt.want)
			}
		})
	}
}

func TestSearchAlbum(t *testing.T) {
	tests := []struct {
		album string
		want  string
	}{
		{album: "Yankee Hotel Foxtrot (Expanded Edition)", want: "Yankee Hotel Foxtrot"},
		{album: "Rumours (Super Deluxe)", want: "Rumours"},
		{album: "Abbey Road - 2019 Remaster", want: "Abbey Road"},
		{album: "Yankee Hotel Foxtrot", want: "Yankee Hotel Foxtrot"},
		// Not an edition, so the parenthetical is part of the album's name.
		{album: "Blade Runner (Music From The Original Soundtrack)", want: "Blade Runner (Music From The Original Soundtrack)"},
		{album: "(What's the Story) Morning Glory?", want: "(What's the Story) Morning Glory?"},
		{album: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.album, func(t *testing.T) {
			if got := searchAlbum(tt.album); got != tt.want {
				t.Errorf("searchAlbum(%q) = %q, want %q", tt.album, got, tt.want)
			}
		})
	}
}
