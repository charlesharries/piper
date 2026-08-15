package musicbrainz

import (
	"strings"
	"testing"
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
				Artist:  "Test Artist",
				Release: "Test Album",
				ISRC:    "USUM71801197",
			},
			want: `isrc:"USUM71801197" AND recording:"Test Song" AND (artistname:"Test Artist" OR artist:"Test Artist") AND release:"Test Album"`,
		},
		{
			name: "without ISRC",
			params: SearchParams{
				Track:   "Test Song",
				Artist:  "Test Artist",
				Release: "Test Album",
			},
			want: `recording:"Test Song" AND (artistname:"Test Artist" OR artist:"Test Artist") AND release:"Test Album"`,
		},
		{
			name:   "only ISRC",
			params: SearchParams{ISRC: "USUM71801197"},
			want:   `isrc:"USUM71801197"`,
		},
		{
			name: "only track and artist",
			params: SearchParams{
				Track:  "Test Song",
				Artist: "Test Artist",
			},
			want: `recording:"Test Song" AND (artistname:"Test Artist" OR artist:"Test Artist")`,
		},
		{
			name: "quotes in title are escaped",
			params: SearchParams{
				Track:  `Say "Yes"`,
				Artist: "Test Artist",
			},
			want: `recording:"Say \"Yes\"" AND (artistname:"Test Artist" OR artist:"Test Artist")`,
		},
		{
			name:   "backslash in title is escaped",
			params: SearchParams{Track: `AC\DC`},
			want:   `recording:"AC\\DC"`,
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

func TestBuildSearchEndpoint(t *testing.T) {
	tests := []struct {
		name string
		req  searchRequest
		want string
	}{
		{
			name: "basic query",
			req:  searchRequest{query: `recording:"Test Song" AND artistname:"Test Artist"`, limit: 25},
			want: "https://musicbrainz.org/ws/2/recording?fmt=json&limit=25&query=recording%3A%22Test+Song%22+AND+artistname%3A%22Test+Artist%22",
		},
		{
			name: "ISRC query",
			req:  searchRequest{query: `isrc:"USUM71801197"`, limit: 25},
			want: "https://musicbrainz.org/ws/2/recording?fmt=json&limit=25&query=isrc%3A%22USUM71801197%22",
		},
		{
			name: "dismax free text",
			req:  searchRequest{query: "Test Song Test Artist", limit: 50, dismax: true},
			want: "https://musicbrainz.org/ws/2/recording?dismax=true&fmt=json&limit=50&query=Test+Song+Test+Artist",
		},
		{
			name: "no limit",
			req:  searchRequest{query: "x"},
			want: "https://musicbrainz.org/ws/2/recording?fmt=json&query=x",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildSearchEndpoint(tt.req); got != tt.want {
				t.Errorf("buildSearchEndpoint() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildRecordingEndpoint(t *testing.T) {
	got := buildRecordingEndpoint("b1a9c0e9-d987-4042-ae91-78d6a3267d69")
	want := "https://musicbrainz.org/ws/2/recording/b1a9c0e9-d987-4042-ae91-78d6a3267d69?fmt=json&inc=releases+release-groups+artist-credits+isrcs"
	if got != want {
		t.Errorf("buildRecordingEndpoint() = %v, want %v", got, want)
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
			case `recording:"Dreams" AND (artistname:"Fleetwood Mac" OR artist:"Fleetwood Mac") AND release:"Rumours"`:
				withAlbum = i
			case `recording:"Dreams" AND (artistname:"Fleetwood Mac" OR artist:"Fleetwood Mac")`:
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
		if want := `recording:"Dreams"`; tiers[scoped].query != want {
			t.Errorf("scoped tier query = %q, want %q", tiers[scoped].query, want)
		}
		if scoped != len(tiers)-2 {
			t.Errorf("artist-scoped tier at %d, want second to last of %d", scoped, len(tiers))
		}
	})

	t.Run("uncleaned retry is included", func(t *testing.T) {
		// The cleaner truncates artists at the first comma, so the raw credit
		// has to get its own attempt.
		tiers := s.searchTiers(track("One Kiss", "Calvin Harris, Dua Lipa", "", ""))
		var found bool
		for _, tier := range tiers {
			if tier.query == `recording:"One Kiss" AND (artistname:"Calvin Harris, Dua Lipa" OR artist:"Calvin Harris, Dua Lipa")` {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a tier with the full artist credit, got %v", queries(tiers))
		}
	})
}

func TestRetryDelay(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		retryAfter  string
		attempt     int
		wantRetry   bool
		wantSeconds float64
	}{
		{name: "503 backs off", status: 503, attempt: 0, wantRetry: true, wantSeconds: 1},
		{name: "503 backs off further", status: 503, attempt: 2, wantRetry: true, wantSeconds: 4},
		{name: "429 backs off", status: 429, attempt: 1, wantRetry: true, wantSeconds: 2},
		{name: "Retry-After wins", status: 503, retryAfter: "7", attempt: 0, wantRetry: true, wantSeconds: 7},
		{name: "404 is not retried", status: 404, attempt: 0, wantRetry: false},
		{name: "400 is not retried", status: 400, attempt: 0, wantRetry: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := response(tt.status, tt.retryAfter)
			delay, retry := retryDelay(resp, tt.attempt)
			if retry != tt.wantRetry {
				t.Fatalf("retryDelay() retry = %v, want %v", retry, tt.wantRetry)
			}
			if retry && delay.Seconds() != tt.wantSeconds {
				t.Errorf("retryDelay() = %v, want %vs", delay, tt.wantSeconds)
			}
		})
	}
}
