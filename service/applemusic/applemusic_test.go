package applemusic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/teal-fm/piper/db"
	"github.com/teal-fm/piper/models"
)

// createTestJWT creates a minimal JWT for testing that will pass structural validation
func createTestJWT(teamID string, expiry time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","typ":"JWT"}`))

	claims := map[string]any{
		"iss": teamID,
		"iat": time.Now().Unix(),
		"exp": expiry.Unix(),
	}
	claimsJSON, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(claimsJSON)

	// Signature doesn't need to be valid for structural validation
	signature := base64.RawURLEncoding.EncodeToString([]byte("fake-signature"))

	return header + "." + payload + "." + signature
}

// Helper to create AppleRecentTrack for testing
func makeTestTrack(name, album, artist string) *AppleRecentTrack {
	track := &AppleRecentTrack{}
	track.Attributes.Name = name
	track.Attributes.AlbumName = album
	track.Attributes.ArtistName = artist
	return track
}

// trackResponseTransport returns a fixed JSON response simulating the Apple Music API.
type trackResponseTransport struct {
	response string
}

func (t *trackResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(t.response)),
		Header:     make(http.Header),
	}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// newTestDB creates an in-memory SQLite database for testing.
func newTestDB(t *testing.T) *db.DB {
	t.Helper()
	testDB, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	if err := testDB.Initialize(); err != nil {
		t.Fatalf("failed to initialize test db: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })
	return testDB
}

// createTestUser creates a user in the DB with an Apple Music token set.
func createTestUser(t *testing.T, testDB *db.DB) *models.User {
	t.Helper()
	userID, err := testDB.CreateUser(&models.User{})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	if err := testDB.UpdateAppleMusicUserToken(userID, "fake-token"); err != nil {
		t.Fatalf("failed to set apple music token: %v", err)
	}
	user, err := testDB.GetUserByID(userID)
	if err != nil {
		t.Fatalf("failed to get user: %v", err)
	}
	return user
}

// newTestService creates a Service backed by an in-memory DB and the given transport.
func newTestService(t *testing.T, testDB *db.DB, transport http.RoundTripper) *Service {
	t.Helper()
	tokenExpiry := time.Now().Add(1 * time.Hour)
	return &Service{
		DB:           testDB,
		httpClient:   &http.Client{Transport: transport},
		logger:       log.New(io.Discard, "", 0),
		teamID:       "test-team",
		keyID:        "test-key",
		cachedToken:  createTestJWT("test-team", tokenExpiry),
		cachedExpiry: tokenExpiry,
	}
}

// uploadedTrackJSON builds an Apple Music API response for an uploaded track (no URL).
func uploadedTrackJSON(name, artist, album string) string {
	// No id: uploaded tracks fall back to a metadata hash, as they do live.
	track := map[string]any{
		"attributes": map[string]string{
			"name":       name,
			"artistName": artist,
			"albumName":  album,
		},
	}
	data, _ := json.Marshal(map[string]any{"data": []any{track}})
	return string(data)
}

// resourceID is the Apple resource id recentTracksJSON gives a named track.
func resourceID(name string) string {
	return "ams." + name
}

// catalogURL is the share URL recentTracksJSON gives a named track.
func catalogURL(name string) string {
	return "https://music.apple.com/song/" + name
}

// recentTracksJSON builds an Apple Music recently-played response for catalog
// tracks. Names are given newest-first, matching Apple's ordering.
func recentTracksJSON(names ...string) string {
	tracks := make([]any, 0, len(names))
	for _, name := range names {
		tracks = append(tracks, map[string]any{
			"id": resourceID(name),
			"attributes": map[string]any{
				"name":             name,
				"artistName":       name + " Artist",
				"albumName":        name + " Album",
				"url":              catalogURL(name),
				"durationInMillis": 180000,
			},
		})
	}
	data, _ := json.Marshal(map[string]any{"data": tracks})
	return string(data)
}

// processUserTestEnv sets up a DB, user, and service wired to the given API response.
type processUserTestEnv struct {
	testDB *db.DB
	user   *models.User
	svc    *Service
}

func newProcessUserTestEnv(t *testing.T, apiResponse string) *processUserTestEnv {
	t.Helper()
	testDB := newTestDB(t)
	user := createTestUser(t, testDB)
	transport := &trackResponseTransport{response: apiResponse}
	svc := newTestService(t, testDB, transport)
	return &processUserTestEnv{testDB: testDB, user: user, svc: svc}
}

// seedUploadedTrack saves an uploaded track to the DB, using its upload hash as the URL.
func (env *processUserTestEnv) seedUploadedTrack(t *testing.T, name, artist, album string) {
	t.Helper()
	hash := generateUploadHash(makeTestTrack(name, album, artist))
	_, err := env.testDB.SaveTrack(env.user.ID, db.SourceAppleMusic, &models.Track{
		Name:           name,
		Artist:         []models.Artist{{Name: artist}},
		Album:          album,
		URL:            hash,
		SourceID:       hash,
		ServiceBaseUrl: "music.apple.com",
	})
	if err != nil {
		t.Fatalf("failed to seed track: %v", err)
	}
}

// trackCount returns the number of tracks stored for the test user.
func (env *processUserTestEnv) trackCount(t *testing.T) int {
	t.Helper()
	tracks, err := env.testDB.GetRecentTracks(env.user.ID, 100)
	if err != nil {
		t.Fatalf("failed to get recent tracks: %v", err)
	}
	return len(tracks)
}

// seedCatalogTracks stores catalog tracks as existing history. Names are given
// newest-first; timestamps are assigned so the stored order matches.
func (env *processUserTestEnv) seedCatalogTracks(t *testing.T, newestFirst ...string) {
	t.Helper()
	base := time.Now().UTC().Add(-time.Duration(len(newestFirst)) * time.Hour)
	for i := len(newestFirst) - 1; i >= 0; i-- {
		name := newestFirst[i]
		_, err := env.testDB.SaveTrack(env.user.ID, db.SourceAppleMusic, &models.Track{
			Name:           name,
			Artist:         []models.Artist{{Name: name + " Artist"}},
			Album:          name + " Album",
			URL:            catalogURL(name),
			SourceID:       resourceID(name),
			ServiceBaseUrl: "music.apple.com",
			Timestamp:      base.Add(time.Duration(len(newestFirst)-i) * time.Minute),
		})
		if err != nil {
			t.Fatalf("failed to seed track %q: %v", name, err)
		}
	}
}

// storedNames returns the stored track names for the test user, newest-first.
func (env *processUserTestEnv) storedNames(t *testing.T) []string {
	t.Helper()
	tracks, err := env.testDB.GetRecentTracksForService(env.user.ID, db.SourceAppleMusic, 100)
	if err != nil {
		t.Fatalf("failed to read stored tracks: %v", err)
	}
	names := make([]string, len(tracks))
	for i, track := range tracks {
		names[i] = track.Name
	}
	return names
}

func TestProcessUserSkipsDuplicateUploadedTrack(t *testing.T) {
	env := newProcessUserTestEnv(t, uploadedTrackJSON("My Upload", "Local Artist", "Local Album"))
	env.seedUploadedTrack(t, "My Upload", "Local Artist", "Local Album")

	if err := env.svc.ProcessUser(context.Background(), env.user); err != nil {
		t.Fatalf("ProcessUser returned error: %v", err)
	}

	if got := env.trackCount(t); got != 1 {
		t.Errorf("expected 1 track (no duplicate save), got %d", got)
	}
}

func TestProcessUserSavesDifferentUploadedTrack(t *testing.T) {
	env := newProcessUserTestEnv(t, uploadedTrackJSON("New Upload", "New Artist", "New Album"))
	env.seedUploadedTrack(t, "Old Upload", "Old Artist", "Old Album")

	if err := env.svc.ProcessUser(context.Background(), env.user); err != nil {
		t.Fatalf("ProcessUser returned error: %v", err)
	}

	if got := env.trackCount(t); got != 2 {
		t.Errorf("expected 2 tracks (new upload saved), got %d", got)
	}
}

func TestProcessUserResolvesCatalogURL(t *testing.T) {
	testDB := newTestDB(t)
	var paths []string
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		switch req.URL.Path {
		case "/v1/me/recent/played/tracks":
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"i.library-song","attributes":{"name":"Catalog Song","artistName":"Catalog Artist","albumName":"Catalog Album","playParams":{"id":"i.library-song","kind":"song","catalogId":"123456789"}}}]}`)),
				Header:     make(http.Header),
			}, nil
		case "/v1/me/storefront":
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"us"}]}`)),
				Header:     make(http.Header),
			}, nil
		case "/v1/catalog/us/songs":
			if got := req.URL.Query().Get("ids"); got != "123456789" {
				t.Errorf("catalog ids = %q, want %q", got, "123456789")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader(`{"data":[{"attributes":{"url":"https://music.apple.com/us/song/catalog-song/123456789"}}]}`)),
				Header:     make(http.Header),
			}, nil
		default:
			return nil, fmt.Errorf("unexpected request path %q", req.URL.Path)
		}
	})
	svc := newTestService(t, testDB, transport)
	user := createTestUser(t, testDB)

	if err := svc.ProcessUser(context.Background(), user); err != nil {
		t.Fatalf("ProcessUser returned error: %v", err)
	}

	stored, err := testDB.GetLatestTrackForService(user.ID, db.SourceAppleMusic)
	if err != nil {
		t.Fatalf("failed to read stored track: %v", err)
	}
	if stored == nil {
		t.Fatal("no track was stored")
	}
	if stored.URL != "https://music.apple.com/us/song/catalog-song/123456789" {
		t.Errorf("stored URL = %q, want the resolved catalog URL", stored.URL)
	}
	if got, want := strings.Join(paths, ","), "/v1/me/recent/played/tracks,/v1/me/storefront,/v1/catalog/us/songs"; got != want {
		t.Fatalf("request paths = %q, want %q", got, want)
	}
}

func TestProcessUserBackfillsWindowOldestFirst(t *testing.T) {
	env := newProcessUserTestEnv(t, recentTracksJSON("D", "C", "B", "A", "Z"))
	env.seedCatalogTracks(t, "A", "Z")

	if err := env.svc.ProcessUser(context.Background(), env.user); err != nil {
		t.Fatalf("ProcessUser returned error: %v", err)
	}

	got := env.storedNames(t)
	want := []string{"D", "C", "B", "A", "Z"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stored tracks (newest-first) = %v, want %v", got, want)
	}
}

func TestProcessUserRecordsRepeatAfterAnotherTrack(t *testing.T) {
	// A -> B -> A within one tick: the newest entry equals the newest stored
	// row, so a naive newest-row comparison would drop both B and the repeat.
	env := newProcessUserTestEnv(t, recentTracksJSON("A", "B", "A", "Z"))
	env.seedCatalogTracks(t, "A", "Z")

	if err := env.svc.ProcessUser(context.Background(), env.user); err != nil {
		t.Fatalf("ProcessUser returned error: %v", err)
	}

	got := env.storedNames(t)
	want := []string{"A", "B", "A", "Z"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stored tracks (newest-first) = %v, want %v", got, want)
	}
}

func TestProcessUserIgnoresUnchangedWindow(t *testing.T) {
	env := newProcessUserTestEnv(t, recentTracksJSON("A", "B", "C"))
	env.seedCatalogTracks(t, "A", "B", "C")

	if err := env.svc.ProcessUser(context.Background(), env.user); err != nil {
		t.Fatalf("ProcessUser returned error: %v", err)
	}

	if got := env.trackCount(t); got != 3 {
		t.Errorf("expected 3 tracks (nothing new), got %d", got)
	}
}

func TestProcessUserFirstSyncTakesNewestOnly(t *testing.T) {
	// With no history we only establish a cursor: backfilling the window would
	// publish invented timestamps to the user's PDS.
	env := newProcessUserTestEnv(t, recentTracksJSON("D", "C", "B", "A"))

	if err := env.svc.ProcessUser(context.Background(), env.user); err != nil {
		t.Fatalf("ProcessUser returned error: %v", err)
	}

	got := env.storedNames(t)
	if len(got) != 1 || got[0] != "D" {
		t.Errorf("stored tracks = %v, want just [D]", got)
	}
}

func TestProcessUserTakesWholeWindowWhenHistoryDiverges(t *testing.T) {
	env := newProcessUserTestEnv(t, recentTracksJSON("C", "B", "A"))
	env.seedCatalogTracks(t, "Y", "Z")

	if err := env.svc.ProcessUser(context.Background(), env.user); err != nil {
		t.Fatalf("ProcessUser returned error: %v", err)
	}

	got := env.storedNames(t)
	want := []string{"C", "B", "A", "Y", "Z"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stored tracks (newest-first) = %v, want %v", got, want)
	}
}

func TestProcessUserSavesStrictlyIncreasingTimestamps(t *testing.T) {
	env := newProcessUserTestEnv(t, recentTracksJSON("D", "C", "B", "A"))
	env.seedCatalogTracks(t, "A")

	if err := env.svc.ProcessUser(context.Background(), env.user); err != nil {
		t.Fatalf("ProcessUser returned error: %v", err)
	}

	tracks, err := env.testDB.GetRecentTracksForService(env.user.ID, db.SourceAppleMusic, 100)
	if err != nil {
		t.Fatalf("failed to read stored tracks: %v", err)
	}
	// Newest-first, so each timestamp must be strictly after the next one.
	for i := 0; i+1 < len(tracks); i++ {
		if !tracks[i].Timestamp.After(tracks[i+1].Timestamp) {
			t.Errorf("timestamp for %q (%s) is not after %q (%s)",
				tracks[i].Name, tracks[i].Timestamp, tracks[i+1].Name, tracks[i+1].Timestamp)
		}
	}
}

func TestProcessUserWithoutATProtoSession(t *testing.T) {
	// A DID with no session id must not panic the tracker.
	env := newProcessUserTestEnv(t, recentTracksJSON("A"))
	did := "did:plc:example"
	env.user.ATProtoDID = &did
	env.user.MostRecentAtProtoSessionID = nil

	if err := env.svc.ProcessUser(context.Background(), env.user); err != nil {
		t.Fatalf("ProcessUser returned error: %v", err)
	}
	if got := env.trackCount(t); got != 1 {
		t.Errorf("expected the track to still be saved, got %d", got)
	}
}

// statusTransport answers every request with a fixed status and body.
type statusTransport struct {
	statusCode int
	status     string
	body       string
	header     http.Header
}

func (t *statusTransport) RoundTrip(*http.Request) (*http.Response, error) {
	header := t.header
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{
		StatusCode: t.statusCode,
		Status:     t.status,
		Body:       io.NopCloser(strings.NewReader(t.body)),
		Header:     header,
	}, nil
}

func appleMusicToken(t *testing.T, testDB *db.DB, userID int64) string {
	t.Helper()
	user, err := testDB.GetUserByID(userID)
	if err != nil {
		t.Fatalf("failed to read user: %v", err)
	}
	if user.AppleMusicUserToken == nil {
		return ""
	}
	return *user.AppleMusicUserToken
}

func TestSyncUserClearsTokenOnUnauthorized(t *testing.T) {
	testDB := newTestDB(t)
	user := createTestUser(t, testDB)
	svc := newTestService(t, testDB, &statusTransport{
		statusCode: http.StatusUnauthorized,
		status:     "401 Unauthorized",
		body:       `{"errors":[{"status":"401","code":"UNAUTHORIZED","title":"Unauthorized"}]}`,
	})

	svc.syncUser(context.Background(), user)

	if got := appleMusicToken(t, testDB, user.ID); got != "" {
		t.Errorf("apple music token = %q, want it cleared", got)
	}
}

func TestSyncUserKeepsTokenOnServerError(t *testing.T) {
	testDB := newTestDB(t)
	user := createTestUser(t, testDB)
	svc := newTestService(t, testDB, &statusTransport{
		statusCode: http.StatusInternalServerError,
		status:     "500 Internal Server Error",
		body:       `{"errors":[{"status":"500","title":"Server Error"}]}`,
	})

	svc.syncUser(context.Background(), user)

	if got := appleMusicToken(t, testDB, user.ID); got != "fake-token" {
		t.Errorf("apple music token = %q, want it kept", got)
	}
	if svc.readyToSync(user.ID) {
		t.Error("user should be backed off after a transient failure")
	}
}

func TestSyncUserHonoursRetryAfter(t *testing.T) {
	testDB := newTestDB(t)
	user := createTestUser(t, testDB)
	header := make(http.Header)
	header.Set("Retry-After", "1800")
	svc := newTestService(t, testDB, &statusTransport{
		statusCode: http.StatusTooManyRequests,
		status:     "429 Too Many Requests",
		body:       `{"errors":[{"status":"429","title":"Rate Limited"}]}`,
		header:     header,
	})

	svc.syncUser(context.Background(), user)

	if got := appleMusicToken(t, testDB, user.ID); got != "fake-token" {
		t.Errorf("apple music token = %q, want it kept", got)
	}
	if svc.readyToSync(user.ID) {
		t.Error("user should be deferred after a 429")
	}
	state := svc.syncStates[user.ID]
	if state == nil {
		t.Fatal("no sync state recorded")
	}
	if wait := time.Until(state.nextAttempt); wait < 25*time.Minute {
		t.Errorf("next attempt in %s, want Retry-After of 30m to be honoured", wait)
	}
}

// librarySongsJSON builds a response of library songs, which arrive with no
// share URL and must have their catalog URL resolved before it can be used.
func librarySongsJSON(names ...string) string {
	tracks := make([]any, 0, len(names))
	for _, name := range names {
		tracks = append(tracks, map[string]any{
			"id": "i." + name,
			"attributes": map[string]any{
				"name":             name,
				"artistName":       name + " Artist",
				"albumName":        name + " Album",
				"durationInMillis": 180000,
				"playParams": map[string]any{
					"id":        "i." + name,
					"kind":      "song",
					"catalogId": "cat." + name,
				},
			},
		})
	}
	data, _ := json.Marshal(map[string]any{"data": tracks})
	return string(data)
}

// countingTransport serves a fixed recently-played response and records which
// endpoints were hit.
type countingTransport struct {
	recent string
	paths  []string
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.paths = append(t.paths, req.URL.Path)
	body := `{"data":[]}`
	switch req.URL.Path {
	case "/v1/me/recent/played/tracks":
		body = t.recent
	case "/v1/me/storefront":
		body = `{"data":[{"id":"us"}]}`
	default:
		body = `{"data":[{"attributes":{"url":"https://music.apple.com/us/song/x/1"}}]}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

func TestProcessUserSkipsCatalogLookupWhenHistoryHasResourceIDs(t *testing.T) {
	testDB := newTestDB(t)
	user := createTestUser(t, testDB)
	transport := &countingTransport{recent: librarySongsJSON("Song")}
	svc := newTestService(t, testDB, transport)

	// A row carrying the resource id, as ingest now writes it.
	if _, err := testDB.SaveTrack(user.ID, db.SourceAppleMusic, &models.Track{
		Name:           "Song",
		Artist:         []models.Artist{{Name: "Song Artist"}},
		Album:          "Song Album",
		URL:            "https://music.apple.com/us/song/song/1",
		SourceID:       "cat.Song",
		ServiceBaseUrl: "music.apple.com",
		Timestamp:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("failed to seed track: %v", err)
	}

	if err := svc.ProcessUser(context.Background(), user); err != nil {
		t.Fatalf("ProcessUser returned error: %v", err)
	}

	if got := strings.Join(transport.paths, ","); got != "/v1/me/recent/played/tracks" {
		t.Errorf("request paths = %q, want only the recently-played call", got)
	}
}

func TestProcessUserMatchesLegacyRowsByURL(t *testing.T) {
	// A row written before source_id existed: matching it still requires
	// resolving the library song's catalog URL.
	testDB := newTestDB(t)
	user := createTestUser(t, testDB)
	transport := &countingTransport{recent: librarySongsJSON("Song")}
	svc := newTestService(t, testDB, transport)

	if _, err := testDB.SaveTrack(user.ID, db.SourceAppleMusic, &models.Track{
		Name:           "Song",
		Artist:         []models.Artist{{Name: "Song Artist"}},
		Album:          "Song Album",
		URL:            "https://music.apple.com/us/song/x/1",
		ServiceBaseUrl: "music.apple.com",
		Timestamp:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("failed to seed track: %v", err)
	}

	if err := svc.ProcessUser(context.Background(), user); err != nil {
		t.Fatalf("ProcessUser returned error: %v", err)
	}

	tracks, err := testDB.GetRecentTracksForService(user.ID, db.SourceAppleMusic, 100)
	if err != nil {
		t.Fatalf("failed to read tracks: %v", err)
	}
	if len(tracks) != 1 {
		t.Errorf("expected the legacy row to match (1 track), got %d", len(tracks))
	}
	if !slices.Contains(transport.paths, "/v1/me/storefront") {
		t.Errorf("expected a storefront lookup to resolve the legacy URL, got %v", transport.paths)
	}
}

func identitiesOf(ids ...string) []itemIdentity {
	out := make([]itemIdentity, len(ids))
	for i, id := range ids {
		out[i] = itemIdentity{sourceID: id, url: "url:" + id}
	}
	return out
}

func historyOf(ids ...string) []*models.Track {
	out := make([]*models.Track, len(ids))
	for i, id := range ids {
		out[i] = &models.Track{SourceID: id, URL: "url:" + id}
	}
	return out
}

func TestAlignmentOffset(t *testing.T) {
	tests := []struct {
		name    string
		window  []itemIdentity
		history []*models.Track
		want    int
	}{
		{"nothing new", identitiesOf("a", "b"), historyOf("a", "b"), 0},
		{"one new", identitiesOf("c", "a", "b"), historyOf("a", "b"), 1},
		{"repeat after another track", identitiesOf("a", "b", "a", "z"), historyOf("a", "z"), 2},
		{"no overlap", identitiesOf("c", "b"), historyOf("y", "z"), 2},
		{"history shorter than window", identitiesOf("c", "b", "a"), historyOf("a"), 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := alignmentOffset(tt.window, tt.history); got != tt.want {
				t.Errorf("alignmentOffset() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestAlignmentOffsetFallsBackToURLForLegacyRows(t *testing.T) {
	// Rows written before source_id existed carry only a URL.
	window := identitiesOf("a", "b")
	history := []*models.Track{{URL: "url:a"}, {URL: "url:b"}}

	if got := alignmentOffset(window, history); got != 0 {
		t.Errorf("alignmentOffset() = %d, want 0 (legacy rows matched on URL)", got)
	}
}

func TestGenerateUploadHash(t *testing.T) {
	tests := []struct {
		name     string
		track    *AppleRecentTrack
		wantHash string
	}{
		{
			name:     "basic track",
			track:    makeTestTrack("Test Song", "Test Album", "Test Artist"),
			wantHash: "am_uploaded_ec50bb20ebeddc6f04cb65bcff156fed3c59d7e311e3d3efbbea506c3f8ad5ae",
		},
		{
			name:     "track with different data",
			track:    makeTestTrack("Collaboration", "Best Hits", "Artist One"),
			wantHash: "am_uploaded_c387e87af520fd4dae9b9cb872bd9cfbc8f7a88ca1b49d874dc1045183ba43c3",
		},
		{
			name:     "track with empty album",
			track:    makeTestTrack("Single Track", "", "Solo Artist"),
			wantHash: "am_uploaded_f73402f47a46c5ff050b64a86d5a1fcbaa20a734a8963f458dca81be290731f7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := generateUploadHash(tt.track)

			// The hash should be prefixed with "am_uploaded_" so that it's clear it's an uploaded Apple Music track
			if !strings.HasPrefix(got, "am_uploaded_") {
				t.Errorf("generateUploadHash() = %v, want prefix 'am_uploaded_'", got)
			}

			if got != tt.wantHash {
				t.Errorf("generateUploadHash() = %v, want %v", got, tt.wantHash)
			}

			// Hash is deterministic -- same track will return same hash
			got2 := generateUploadHash(tt.track)
			if got != got2 {
				t.Errorf("generateUploadHash() is not deterministic: first=%v, second=%v", got, got2)
			}
		})
	}
}
