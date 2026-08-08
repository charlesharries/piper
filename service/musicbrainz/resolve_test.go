package musicbrainz

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/time/rate"

	"github.com/teal-fm/piper/models"
)

// route serves a canned body for requests whose URL contains match.
type route struct {
	match string
	body  string
	// status, when set, is served once before falling back to 200, so retry
	// paths can be exercised.
	status int
}

// routedTransport matches routes in order and records every URL it was asked
// for. Order matters: tier URLs overlap, so the first route wins.
type routedTransport struct {
	routes    []route
	requested []string
}

func (rt *routedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	url := req.URL.String()
	rt.requested = append(rt.requested, url)

	for i := range rt.routes {
		r := &rt.routes[i]
		if !strings.Contains(url, r.match) {
			continue
		}
		status := http.StatusOK
		if r.status != 0 {
			status = r.status
			r.status = 0
		}
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(r.body)),
			Header:     http.Header{},
		}, nil
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"count":0,"recordings":[]}`)),
		Header:     http.Header{},
	}, nil
}

// matching returns the requested URLs containing want. Cover art selection
// issues its own requests, so assertions name the traffic they care about
// rather than counting everything.
func (rt *routedTransport) matching(want string) []string {
	var out []string
	for _, url := range rt.requested {
		if strings.Contains(url, want) {
			out = append(out, url)
		}
	}
	return out
}

// searches are recording searches, browses are release-group browses, and
// artProbes are Cover Art Archive lookups.
func (rt *routedTransport) searches() []string  { return rt.matching("/ws/2/recording?") }
func (rt *routedTransport) browses() []string   { return rt.matching("/ws/2/release?") }
func (rt *routedTransport) artProbes() []string { return rt.matching("coverartarchive.org") }

func newTestService(t *testing.T, routes ...route) (*Service, *routedTransport) {
	t.Helper()
	transport := &routedTransport{routes: routes}
	svc := NewMusicBrainzService(nil, WithHTTPClient(&http.Client{Transport: transport}))
	// The real limiter serialises requests at 1/sec, which would make these
	// tests needlessly slow.
	svc.limiter = rate.NewLimiter(rate.Inf, 1)
	svc.logger.SetOutput(io.Discard)
	return svc, transport
}

func searchBody(t *testing.T, recordings ...Recording) string {
	t.Helper()
	body, err := json.Marshal(SearchResponse{Count: len(recordings), Recordings: recordings})
	if err != nil {
		t.Fatalf("marshalling search body: %v", err)
	}
	return string(body)
}

func TestResolveMatchesOnISRC(t *testing.T) {
	rec := recording("Bohemian Rhapsody", "Queen", 355106,
		release("A Night at the Opera", "A Night at the Opera", "1975-11-21", "GB"))

	svc, transport := newTestService(t, route{match: "isrc", body: searchBody(t, rec)})

	play := models.Track{
		Name:       "Bohemian Rhapsody",
		Artist:     []models.Artist{{Name: "Queen"}},
		Album:      "A Night at the Opera",
		ISRC:       "GBUM71029604",
		DurationMs: 355106,
	}

	match, err := svc.Resolve(context.Background(), play)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if match.Recording.Title != "Bohemian Rhapsody" {
		t.Errorf("matched %q", match.Recording.Title)
	}
	if match.Release == nil || match.Release.Title != "A Night at the Opera" {
		t.Errorf("release = %v, want A Night at the Opera", match.Release)
	}
	// The ISRC tier alone should settle it; nothing looser should be tried.
	if searches := transport.searches(); len(searches) != 1 {
		t.Errorf("made %d searches (%v), want 1", len(searches), searches)
	}
}

// When a constrained tier comes back empty, a looser one has to run.
func TestResolveFallsThroughToLooserTier(t *testing.T) {
	rec := recording("Dreams", "Fleetwood Mac", 257800,
		release("Rumours", "Rumours", "1977-02-04", "US"))

	svc, transport := newTestService(t,
		// Routes match in order, and the first tier's URL contains both
		// patterns. Only the tier without a release filter returns anything.
		route{match: "AND+release", body: `{"count":0,"recordings":[]}`},
		route{match: "artistname", body: searchBody(t, rec)},
	)

	play := models.Track{
		Name:       "Dreams",
		Artist:     []models.Artist{{Name: "Fleetwood Mac"}},
		Album:      "Rumours (Super Deluxe)",
		DurationMs: 257800,
	}

	match, err := svc.Resolve(context.Background(), play)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if match.Recording.Title != "Dreams" {
		t.Errorf("matched %q, want Dreams", match.Recording.Title)
	}
	if len(transport.requested) < 2 {
		t.Errorf("expected a second tier to run, got %v", transport.requested)
	}
}

// Publishing no MBID beats publishing a wrong one.
func TestResolveRejectsUnrelatedResults(t *testing.T) {
	svc, _ := newTestService(t, route{match: "recording", body: searchBody(t, recording("Bohemian Rhapsody", "Queen", 355106))})

	play := models.Track{
		Name:   "asdfgh",
		Artist: []models.Artist{{Name: "qwerty"}},
	}

	_, err := svc.Resolve(context.Background(), play)
	if !errors.Is(err, ErrNoConfidentMatch) {
		t.Errorf("Resolve() error = %v, want ErrNoConfidentMatch", err)
	}
}

func TestResolveNoResultsAtAll(t *testing.T) {
	svc, _ := newTestService(t)

	_, err := svc.Resolve(context.Background(), models.Track{
		Name:   "Dreams",
		Artist: []models.Artist{{Name: "Fleetwood Mac"}},
	})
	if !errors.Is(err, ErrNoConfidentMatch) {
		t.Errorf("Resolve() error = %v, want ErrNoConfidentMatch", err)
	}
}

// A 503 is MusicBrainz shedding load, not a permanent answer.
func TestResolveRetriesOnServiceUnavailable(t *testing.T) {
	rec := recording("Dreams", "Fleetwood Mac", 257800,
		release("Rumours", "Rumours", "1977-02-04", "US"))

	svc, _ := newTestService(t, route{
		match:  "recording",
		body:   searchBody(t, rec),
		status: http.StatusServiceUnavailable,
	})

	play := models.Track{
		Name:       "Dreams",
		Artist:     []models.Artist{{Name: "Fleetwood Mac"}},
		Album:      "Rumours",
		DurationMs: 257800,
	}

	match, err := svc.Resolve(context.Background(), play)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want a retry to succeed", err)
	}
	if match.Recording.Title != "Dreams" {
		t.Errorf("matched %q, want Dreams", match.Recording.Title)
	}
}

// The mapper is verified against MusicBrainz, not trusted outright.
func TestResolveRejectsBadMapperMatch(t *testing.T) {
	wrong := recording("Something Else Entirely", "A Different Band", 120000)
	wrong.ID = "wrong-mbid"
	right := recording("Dreams", "Fleetwood Mac", 257800,
		release("Rumours", "Rumours", "1977-02-04", "US"))

	svc, _ := newTestService(t,
		route{match: "recording/wrong-mbid", body: mustJSON(t, wrong)},
		route{match: "query=", body: searchBody(t, right)},
	)
	svc.mapper = stubMapper{result: &MapperResult{RecordingMBID: "wrong-mbid"}}

	play := models.Track{
		Name:       "Dreams",
		Artist:     []models.Artist{{Name: "Fleetwood Mac"}},
		Album:      "Rumours",
		DurationMs: 257800,
	}

	match, err := svc.Resolve(context.Background(), play)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if match.Source != "musicbrainz" {
		t.Errorf("source = %q, want the search fallback to win", match.Source)
	}
	if match.Recording.Title != "Dreams" {
		t.Errorf("matched %q, want Dreams", match.Recording.Title)
	}
}

func TestResolveUsesMapperWhenItAgrees(t *testing.T) {
	rec := recording("Dreams", "Fleetwood Mac", 257800,
		release("Rumours", "Rumours", "1977-02-04", "US"))
	rec.ID = "good-mbid"

	svc, transport := newTestService(t, route{match: "recording/good-mbid", body: mustJSON(t, rec)})
	svc.mapper = stubMapper{result: &MapperResult{RecordingMBID: "good-mbid"}}

	match, err := svc.Resolve(context.Background(), models.Track{
		Name:       "Dreams",
		Artist:     []models.Artist{{Name: "Fleetwood Mac"}},
		Album:      "Rumours",
		DurationMs: 257800,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if match.Source != "listenbrainz" {
		t.Errorf("source = %q, want listenbrainz", match.Source)
	}
	for _, url := range transport.requested {
		if strings.Contains(url, "query=") {
			t.Errorf("searched despite a good mapper match: %v", transport.requested)
		}
	}
}

// A mapper failure must not take the whole lookup down.
func TestResolveSurvivesMapperError(t *testing.T) {
	rec := recording("Dreams", "Fleetwood Mac", 257800,
		release("Rumours", "Rumours", "1977-02-04", "US"))

	svc, _ := newTestService(t, route{match: "query=", body: searchBody(t, rec)})
	svc.mapper = stubMapper{err: errors.New("listenbrainz is down")}

	match, err := svc.Resolve(context.Background(), models.Track{
		Name:       "Dreams",
		Artist:     []models.Artist{{Name: "Fleetwood Mac"}},
		Album:      "Rumours",
		DurationMs: 257800,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v, want the search fallback to run", err)
	}
	if match.Source != "musicbrainz" {
		t.Errorf("source = %q, want musicbrainz", match.Source)
	}
}

type stubMapper struct {
	result *MapperResult
	err    error
}

func (s stubMapper) Lookup(context.Context, string, string, string) (*MapperResult, error) {
	return s.result, s.err
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	return string(body)
}

// MusicBrainz omits lengths on plenty of recordings. Letting a zero through
// would erase the duration Spotify supplied, which also drives its stamping
// threshold.
func TestApplyMatchKeepsServiceDurationWhenMusicBrainzHasNone(t *testing.T) {
	play := models.Track{Name: "Dreams", DurationMs: 257800}
	match := &Match{Recording: Recording{ID: "rec", Title: "Dreams"}}

	if got := ApplyMatch(play, match); got.DurationMs != 257800 {
		t.Errorf("DurationMs = %d, want the service duration 257800", got.DurationMs)
	}
}

func TestApplyMatchPrefersMusicBrainzDuration(t *testing.T) {
	play := models.Track{Name: "Dreams", DurationMs: 257800}
	match := &Match{Recording: Recording{ID: "rec", Title: "Dreams", Length: 257213}}

	if got := ApplyMatch(play, match); got.DurationMs != 257213 {
		t.Errorf("DurationMs = %d, want 257213", got.DurationMs)
	}
}

// The service's own artist ids are the only link back to its catalogue and must
// survive hydration.
func TestApplyMatchKeepsServiceArtistIDs(t *testing.T) {
	play := models.Track{
		Name:   "Dreams",
		Artist: []models.Artist{{Name: "Fleetwood Mac", ID: "spotify-artist-id"}},
	}

	credit := ArtistCredit{Name: "Fleetwood Mac"}
	credit.Artist.ID = "mb-artist-id"
	credit.Artist.Name = "Fleetwood Mac"
	match := &Match{Recording: Recording{ID: "rec", Title: "Dreams", ArtistCredit: []ArtistCredit{credit}}}

	got := ApplyMatch(play, match)
	if len(got.Artist) != 1 {
		t.Fatalf("Artist = %v, want one entry", got.Artist)
	}
	if got.Artist[0].ID != "spotify-artist-id" {
		t.Errorf("Artist.ID = %q, want the service id to survive", got.Artist[0].ID)
	}
	if got.Artist[0].MBID == nil || *got.Artist[0].MBID != "mb-artist-id" {
		t.Errorf("Artist.MBID = %v, want mb-artist-id", got.Artist[0].MBID)
	}
}

func TestApplyMatchKeepsServiceISRC(t *testing.T) {
	play := models.Track{Name: "Dreams", ISRC: "USWB10002068"}
	match := &Match{Recording: Recording{ID: "rec", Title: "Dreams", ISRCs: []string{"GBAAA0000001"}}}

	if got := ApplyMatch(play, match); got.ISRC != "USWB10002068" {
		t.Errorf("ISRC = %q, want the service ISRC", got.ISRC)
	}
}

func TestApplyMatchFillsMissingISRC(t *testing.T) {
	play := models.Track{Name: "Dreams"}
	match := &Match{Recording: Recording{ID: "rec", Title: "Dreams", ISRCs: []string{"GBAAA0000001"}}}

	if got := ApplyMatch(play, match); got.ISRC != "GBAAA0000001" {
		t.Errorf("ISRC = %q, want GBAAA0000001", got.ISRC)
	}
}

// Fields the service supplied and MusicBrainz has no opinion on must pass
// through untouched.
func TestApplyMatchPreservesPlayMetadata(t *testing.T) {
	play := models.Track{
		PlayID:         42,
		Name:           "Dreams",
		URL:            "https://open.spotify.com/track/abc",
		ServiceBaseUrl: "open.spotify.com",
		ProgressMs:     120000,
		HasStamped:     true,
		Album:          "Rumours",
	}
	match := &Match{Recording: Recording{ID: "rec", Title: "Dreams"}}

	got := ApplyMatch(play, match)
	if got.PlayID != 42 || got.URL != play.URL || got.ServiceBaseUrl != play.ServiceBaseUrl ||
		got.ProgressMs != 120000 || !got.HasStamped || got.Name != "Dreams" {
		t.Errorf("ApplyMatch lost play metadata: %+v", got)
	}
	if got.Album != "Rumours" {
		t.Errorf("Album = %q, want the service album when no release matched", got.Album)
	}
}

// Callers keep their original track when nothing matched, so a failed lookup
// never costs them the metadata the service gave them.
func TestHydrateTrackErrorsWithoutConfidentMatch(t *testing.T) {
	svc, _ := newTestService(t)

	got, err := HydrateTrack(svc, models.Track{
		Name:   "asdfgh",
		Artist: []models.Artist{{Name: "qwerty"}},
	})
	if err == nil {
		t.Errorf("HydrateTrack() error = nil, want an error")
	}
	if got != nil {
		t.Errorf("HydrateTrack() = %v, want nil", got)
	}
}

// A mapper's answer beat no alternatives, so a recording that cannot be
// attributed to the album the play named has to be checked against search.
// Death Cab's "Stability" was resolved to The Photo Album this way: ListenBrainz
// named the reissue's recording, which scores perfectly on title and duration
// and only disagrees on the album.
func TestResolveDoubtsMapperWhenAlbumDisagrees(t *testing.T) {
	photoAlbum := release("The Photo Album", "The Photo Album", "2001-10-09", "US")
	wrong := recording("Stability", "Death Cab for Cutie", 740864, photoAlbum)
	wrong.ID = "photo-album-mbid"

	stabilityEP := release("The Stability EP", "The Stability EP", "2002-03-05", "US")
	right := recording("Stability / Coney Island (alternate version)", "Death Cab for Cutie", 741600, stabilityEP)
	right.ID = "stability-ep-mbid"

	svc, transport := newTestService(t,
		route{match: "recording/photo-album-mbid", body: mustJSON(t, wrong)},
		route{match: "/ws/2/recording?", body: searchBody(t, right)},
	)
	svc.mapper = stubMapper{result: &MapperResult{RecordingMBID: "photo-album-mbid"}}

	match, err := svc.Resolve(context.Background(), stability())
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if match.Source != "musicbrainz" {
		t.Errorf("source = %q, want search to have overruled the mapper", match.Source)
	}
	if match.Release == nil || match.Release.Title != "The Stability EP" {
		t.Errorf("release = %v, want The Stability EP", match.Release)
	}
	if len(transport.searches()) == 0 {
		t.Error("expected a search for the second opinion")
	}
}

// The doubt must not cost a search when the mapper's answer sits on the album
// the play named -- that is the whole point of having a mapper.
func TestResolveTrustsMapperWhenAlbumAgrees(t *testing.T) {
	stabilityEP := release("The Stability EP", "The Stability EP", "2002-03-05", "US")
	rec := recording("Stability", "Death Cab for Cutie", 741000, stabilityEP)
	rec.ID = "good-mbid"

	svc, transport := newTestService(t, route{match: "recording/good-mbid", body: mustJSON(t, rec)})
	svc.mapper = stubMapper{result: &MapperResult{RecordingMBID: "good-mbid"}}

	match, err := svc.Resolve(context.Background(), stability())
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if match.Source != "listenbrainz" {
		t.Errorf("source = %q, want the mapper's answer taken as-is", match.Source)
	}
	if searches := transport.searches(); len(searches) != 0 {
		t.Errorf("searched despite the mapper agreeing on the album: %v", searches)
	}
}

// Doubted is not discarded: when search offers nothing better, the mapper's
// answer still stands rather than the play going unhydrated.
func TestResolveKeepsMapperWhenSearchCannotBeatIt(t *testing.T) {
	photoAlbum := release("The Photo Album", "The Photo Album", "2001-10-09", "US")
	rec := recording("Stability", "Death Cab for Cutie", 740864, photoAlbum)
	rec.ID = "photo-album-mbid"

	svc, _ := newTestService(t,
		route{match: "recording/photo-album-mbid", body: mustJSON(t, rec)},
		route{match: "/ws/2/recording?", body: `{"count":0,"recordings":[]}`},
	)
	svc.mapper = stubMapper{result: &MapperResult{RecordingMBID: "photo-album-mbid"}}

	match, err := svc.Resolve(context.Background(), stability())
	if err != nil {
		t.Fatalf("Resolve() error = %v, want the mapper's answer to stand", err)
	}
	if match.Source != "listenbrainz" || match.Recording.ID != "photo-album-mbid" {
		t.Errorf("match = %q via %s, want the mapper's answer kept", match.Recording.Title, match.Source)
	}
}

func stability() models.Track {
	return models.Track{
		Name:       "Stability",
		Artist:     []models.Artist{{Name: "Death Cab for Cutie"}},
		Album:      "The Stability EP",
		DurationMs: 740864,
	}
}
