package musicbrainz

import (
	"strings"
	"testing"

	"github.com/teal-fm/piper/models"
)

func TestBuildSearchQuery(t *testing.T) {
	tests := []struct {
		name   string
		params SearchParams
		want   string
	}{
		{
			name: "with ISRC",
			params: SearchParams{
				Track:   "Test Song",
				Artists: []string{"Test Artist"},
				Release: "Test Album",
				ISRC:    "USUM71801197",
			},
			want: `isrc:"USUM71801197" AND recording:"Test Song"~3 AND (artistname:"Test Artist" OR artist:"Test Artist") AND release:"Test Album"`,
		},
		{
			name: "without ISRC",
			params: SearchParams{
				Track:   "Test Song",
				Artists: []string{"Test Artist"},
				Release: "Test Album",
			},
			want: `recording:"Test Song"~3 AND (artistname:"Test Artist" OR artist:"Test Artist") AND release:"Test Album"`,
		},
		{
			name:   "only ISRC",
			params: SearchParams{ISRC: "USUM71801197"},
			want:   `isrc:"USUM71801197"`,
		},
		{
			name: "each credited artist is asked after separately",
			params: SearchParams{
				Track:   "Different Trains: America—Before the war",
				Artists: []string{"Steve Reich", "Kronos Quartet"},
			},
			want: `recording:"Different Trains: America—Before the war"~3 AND ` +
				`(artistname:"Steve Reich" OR artist:"Steve Reich" OR ` +
				`artistname:"Kronos Quartet" OR artist:"Kronos Quartet" OR ` +
				`artistname:"Steve Reich, Kronos Quartet" OR artist:"Steve Reich, Kronos Quartet")`,
		},
		{
			name: "a lone name is not repeated as its own credit line",
			params: SearchParams{
				Track:   "One Kiss",
				Artists: []string{"Calvin Harris, Dua Lipa"},
			},
			want: `recording:"One Kiss"~3 AND (artistname:"Calvin Harris, Dua Lipa" OR artist:"Calvin Harris, Dua Lipa")`,
		},
		{
			name: "only track and artist",
			params: SearchParams{
				Track:   "Test Song",
				Artists: []string{"Test Artist"},
			},
			want: `recording:"Test Song"~3 AND (artistname:"Test Artist" OR artist:"Test Artist")`,
		},
		{
			name: "quotes in title are escaped",
			params: SearchParams{
				Track:   `Say "Yes"`,
				Artists: []string{"Test Artist"},
			},
			want: `recording:"Say \"Yes\""~3 AND (artistname:"Test Artist" OR artist:"Test Artist")`,
		},
		{
			name:   "backslash in title is escaped",
			params: SearchParams{Track: `AC\DC`},
			want:   `recording:"AC\\DC"~3`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildSearchQuery(tt.params); got != tt.want {
				t.Errorf("buildSearchQuery() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The tiers must run from most constrained to loosest, and must not repeat a
// query when cleaning leaves the metadata unchanged.
func TestSearchTiers(t *testing.T) {
	s := NewMusicBrainzService(nil)

	t.Run("isrc first", func(t *testing.T) {
		tiers := s.searchTiers(track("Dreams", "Fleetwood Mac", "Rumours", "USWB10002068"))
		if len(tiers) == 0 {
			t.Fatal("expected tiers")
		}
		if want := `isrc:"USWB10002068"`; tiers[0].query != want {
			t.Errorf("first tier = %q, want %q", tiers[0].query, want)
		}
	})

	t.Run("album is dropped before artist", func(t *testing.T) {
		tiers := s.searchTiers(track("Dreams", "Fleetwood Mac", "Rumours", ""))
		var withAlbum, withoutAlbum = -1, -1
		for i, tier := range tiers {
			switch tier.query {
			case `recording:"Dreams"~3 AND (artistname:"Fleetwood Mac" OR artist:"Fleetwood Mac") AND release:"Rumours"`:
				withAlbum = i
			case `recording:"Dreams"~3 AND (artistname:"Fleetwood Mac" OR artist:"Fleetwood Mac")`:
				withoutAlbum = i
			}
		}
		if withAlbum == -1 || withoutAlbum == -1 {
			t.Fatalf("missing expected tiers in %v", queries(tiers))
		}
		if withAlbum > withoutAlbum {
			t.Errorf("album-filtered tier must come first, got %v", queries(tiers))
		}
	})

	t.Run("no duplicate queries", func(t *testing.T) {
		tiers := s.searchTiers(track("Dreams", "Fleetwood Mac", "Rumours", ""))
		seen := map[string]bool{}
		for _, tier := range tiers {
			if seen[tier.query] {
				t.Errorf("duplicate tier query %q in %v", tier.query, queries(tiers))
			}
			seen[tier.query] = true
		}
	})

	// Escaping would leave backslashes in text that dismax matches literally.
	t.Run("dismax tier strips query syntax", func(t *testing.T) {
		tiers := s.searchTiers(track(`Say "Yes": Part (1)`, "Floetry", "Floetic", ""))
		last := tiers[len(tiers)-1]
		if !last.dismax {
			t.Fatalf("last tier is not dismax: %v", queries(tiers))
		}
		if strings.ContainsAny(last.query, `"\:()`) {
			t.Errorf("dismax query %q still contains query syntax", last.query)
		}
		if want := "Say Yes Part 1 Floetry"; last.query != want {
			t.Errorf("dismax query = %q, want %q", last.query, want)
		}
	})

	t.Run("dismax is last", func(t *testing.T) {
		tiers := s.searchTiers(track("Dreams", "Fleetwood Mac", "Rumours", ""))
		for i, tier := range tiers {
			if tier.dismax && i != len(tiers)-1 {
				t.Errorf("dismax tier at %d, want last of %d", i, len(tiers))
			}
		}
	})

	// The artist-scoped tier costs a lookup of its own, so it goes after every
	// tier that searches by name -- but still ahead of the fuzzy parser, which
	// has no artist filter at all.
	t.Run("artist scope sits between the named tiers and dismax", func(t *testing.T) {
		tiers := s.searchTiers(track("Dreams", "Fleetwood Mac", "Rumours", ""))
		scoped := -1
		for i, tier := range tiers {
			if tier.scopeArtist != "" {
				scoped = i
			}
		}
		if scoped == -1 {
			t.Fatalf("missing artist-scoped tier in %v", queries(tiers))
		}
		if want := `recording:"Dreams"~3`; tiers[scoped].query != want {
			t.Errorf("scoped tier query = %q, want %q", tiers[scoped].query, want)
		}
		if scoped != len(tiers)-2 {
			t.Errorf("artist-scoped tier at %d, want second to last of %d", scoped, len(tiers))
		}
	})

	t.Run("title searching includes a bit of slop in case we've got a movement in a larger piece", func(t *testing.T) {
		in := newMatchInput(track("Different Trains: America—Before the war", "Kronos Quartet", "", ""))
		rec := Recording{Title: "Different Trains: I. America - Before the War"}
		if score, _ := in.titleScore(rec); score < 0.9 {
			t.Fatalf("scoring rates the numbered title %0.2f, so retrieval is what must reach it", score)
		}
		for _, tier := range s.searchTiers(track("Different Trains: America—Before the war", "Kronos Quartet", "", "")) {
			if strings.Contains(tier.query, `recording:"Different Trains: America—Before the war"`) &&
				!strings.Contains(tier.query, `"~`) {
				t.Errorf("tier %q phrases the title exactly: %s", tier.name, tier.query)
			}
		}
	})

	t.Run("individual artists are queried individually", func(t *testing.T) {
		play := models.Track{
			Name:   "Different Trains: America—Before the war",
			Album:  "Different Trains / Electric Counterpoint",
			Artist: []models.Artist{{Name: "Steve Reich"}, {Name: "Kronos Quartet"}},
		}
		var found bool
		for _, tier := range s.searchTiers(play) {
			if strings.Contains(tier.query, `artistname:"Kronos Quartet"`) {
				found = true
			}
		}
		if !found {
			t.Errorf("no tier asks after the performer, got %v", queries(s.searchTiers(play)))
		}
	})

	t.Run("uncleaned retry is included", func(t *testing.T) {
		// The cleaner truncates artists at the first comma, so the raw credit
		// has to get its own attempt.
		tiers := s.searchTiers(track("One Kiss", "Calvin Harris, Dua Lipa", "", ""))
		var found bool
		for _, tier := range tiers {
			if tier.query == `recording:"One Kiss"~3 AND (artistname:"Calvin Harris, Dua Lipa" OR artist:"Calvin Harris, Dua Lipa")` {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a tier with the full artist credit, got %v", queries(tiers))
		}
	})
}
