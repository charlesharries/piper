package applemusic

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"
	"github.com/teal-fm/piper/db"
	"github.com/teal-fm/piper/models"
	atprotoauth "github.com/teal-fm/piper/oauth/atproto"
	atprotoservice "github.com/teal-fm/piper/service/atproto"
	"github.com/teal-fm/piper/service/musicbrainz"
)

const (
	// recentWindow is how many recently-played tracks we ask Apple for each
	// poll. Apple caps this endpoint at 30. A window well above the number of
	// tracks a user can finish between ticks is what lets us recover plays
	// missed during downtime.
	recentWindow = 25

	// apiMaxAttempts bounds how many times a single Apple API read is tried.
	// Reads are idempotent, so retrying a transient failure is always safe.
	apiMaxAttempts   = 3
	apiRetryBaseWait = 500 * time.Millisecond

	// userSyncTimeout bounds the work done for one user in one tick so a hung
	// request cannot stall everyone behind it. Generous enough for a full
	// backfill window, which is paced by the MusicBrainz rate limiter.
	userSyncTimeout = 2 * time.Minute

	// Consecutive failures for one user back off exponentially from
	// syncBackoffBase up to syncBackoffMax, so a permanently broken account
	// stops consuming the tick budget every interval.
	syncBackoffBase = 1 * time.Minute
	syncBackoffMax  = 15 * time.Minute

	// catalogCacheMax bounds the library-song catalog cache in a long-lived
	// process.
	catalogCacheMax = 1024
)

// syncState tracks per-user failure backoff between ticks.
type syncState struct {
	consecutiveFailures int
	nextAttempt         time.Time
}

// nowPlayingState remembers what we last published as a user's now-playing
// track. Apple has no "currently playing" endpoint, so we treat the newest
// recently-played track as current and expire it after roughly its own
// duration rather than letting it sit there indefinitely.
type nowPlayingState struct {
	key         string
	publishedAt time.Time
	duration    time.Duration
	cleared     bool
}

type Service struct {
	teamID         string
	keyID          string
	privateKeyPath string

	mu           sync.RWMutex
	cachedToken  string
	cachedExpiry time.Time

	// optional DB-backed persistence
	getToken  func() (string, time.Time, bool, error)
	saveToken func(string, time.Time) error

	// ingestion deps
	DB                *db.DB
	atprotoService    *atprotoauth.AuthService
	mbService         *musicbrainz.Service
	playingNowService interface {
		PublishPlayingNow(ctx context.Context, userID int64, track *models.Track) error
		ClearPlayingNow(ctx context.Context, userID int64) error
	}
	httpClient *http.Client
	logger     *log.Logger

	// per-user sync bookkeeping, guarded by mu. Accessed through helpers that
	// initialise lazily, since Service is also constructed as a literal.
	syncStates   map[int64]*syncState
	nowPlaying   map[int64]*nowPlayingState
	storefronts  map[int64]string
	catalogSongs map[string]catalogSong
}

func NewService(teamID, keyID, privateKeyPath string) *Service {
	return &Service{
		teamID:         teamID,
		keyID:          keyID,
		privateKeyPath: privateKeyPath,
		httpClient:     &http.Client{Timeout: 10 * time.Second},
		logger:         log.New(os.Stdout, "applemusic: ", log.LstdFlags|log.Lmsgprefix),
		syncStates:     make(map[int64]*syncState),
		nowPlaying:     make(map[int64]*nowPlayingState),
		storefronts:    make(map[int64]string),
		catalogSongs:   make(map[string]catalogSong),
	}
}

// WithPersistence wires DB-backed getters/setters for token caching
func (s *Service) WithPersistence(
	get func() (string, time.Time, bool, error),
	save func(string, time.Time) error,
) *Service {
	s.getToken = get
	s.saveToken = save
	return s
}

// WithDeps wires services needed for ingestion
func (s *Service) WithDeps(database *db.DB, atproto *atprotoauth.AuthService, mb *musicbrainz.Service, playingNowService interface {
	PublishPlayingNow(ctx context.Context, userID int64, track *models.Track) error
	ClearPlayingNow(ctx context.Context, userID int64) error
}) *Service {
	s.DB = database
	s.atprotoService = atproto
	s.mbService = mb
	s.playingNowService = playingNowService
	return s
}

// GenerateDeveloperTokenWithForce allows bypassing caches when force is true.
func (s *Service) GenerateDeveloperTokenWithForce(force bool) (string, time.Time, error) {
	if !force {
		return s.GenerateDeveloperToken()
	}

	// Bypass caches and regenerate
	privKey, err := s.loadPrivateKey()
	if err != nil {
		return "", time.Time{}, err
	}

	if s.keyID == "" {
		return "", time.Time{}, errors.New("applemusic key_id is not configured")
	}

	now := time.Now().UTC()
	exp := now.Add(180 * 24 * time.Hour).Add(-1 * time.Hour)

	builder := jwt.NewBuilder().
		Issuer(s.teamID).
		IssuedAt(now).
		Expiration(exp)

	unsignedToken, err := builder.Build()
	if err != nil {
		return "", time.Time{}, err
	}

	headers := jws.NewHeaders()
	_ = headers.Set(jws.KeyIDKey, s.keyID)
	signed, err := jwt.Sign(unsignedToken, jwt.WithKey(jwa.ES256(), privKey, jws.WithProtectedHeaders(headers)))
	if err != nil {
		return "", time.Time{}, err
	}

	final := string(signed)

	s.mu.Lock()
	s.cachedToken = final
	s.cachedExpiry = exp
	s.mu.Unlock()

	if s.saveToken != nil {
		if err := s.saveToken(final, exp); err != nil {
			return "", time.Time{}, fmt.Errorf("persisting apple music developer token: %w", err)
		}
	}

	return final, exp, nil
}

// GenerateDeveloperToken returns a cached valid token or creates a new one.
func (s *Service) GenerateDeveloperToken() (string, time.Time, error) {
	if s.keyID == "" {
		return "", time.Time{}, errors.New("applemusic key_id is not configured")
	}
	s.mu.RLock()
	if s.cachedToken != "" && time.Until(s.cachedExpiry) > 5*time.Minute {
		token := s.cachedToken
		exp := s.cachedExpiry
		s.mu.RUnlock()
		// Validate cached token claims (iss, exp) to avoid serving bad tokens
		if s.isTokenStructurallyValid(token) {
			return token, exp, nil
		}
	} else {
		s.mu.RUnlock()
	}

	// Try DB cache if available
	if s.getToken != nil {
		if t, e, ok, err := s.getToken(); err == nil && ok {
			if time.Until(e) > 5*time.Minute && s.isTokenStructurallyValid(t) {
				s.mu.Lock()
				s.cachedToken = t
				s.cachedExpiry = e
				s.mu.Unlock()
				return t, e, nil
			}
		}
	}

	privKey, err := s.loadPrivateKey()
	if err != nil {
		return "", time.Time{}, err
	}

	now := time.Now().UTC()
	// Apple allows up to 6 months validity; choose 6 months minus a small buffer
	exp := now.Add(180 * 24 * time.Hour).Add(-1 * time.Hour)

	builder := jwt.NewBuilder().
		Issuer(s.teamID).
		IssuedAt(now).
		Expiration(exp)

	unsignedToken, err := builder.Build()
	if err != nil {
		return "", time.Time{}, err
	}

	headers := jws.NewHeaders()
	_ = headers.Set(jws.KeyIDKey, s.keyID)
	signed, err := jwt.Sign(unsignedToken, jwt.WithKey(jwa.ES256(), privKey, jws.WithProtectedHeaders(headers)))
	if err != nil {
		return "", time.Time{}, err
	}

	final := string(signed)

	s.mu.Lock()
	s.cachedToken = final
	s.cachedExpiry = exp
	s.mu.Unlock()

	if s.saveToken != nil {
		if err := s.saveToken(final, exp); err != nil {
			return "", time.Time{}, fmt.Errorf("persisting apple music developer token: %w", err)
		}
	}

	return final, exp, nil
}

// isTokenStructurallyValid parses without verification and checks the iss and
// exp claims. The signature is ours, so there is nothing to verify against.
func (s *Service) isTokenStructurallyValid(token string) bool {
	if token == "" {
		return false
	}
	parsed, err := jwt.Parse([]byte(token), jwt.WithVerify(false))
	if err != nil {
		return false
	}
	// Check issuer
	issuer, _ := parsed.Issuer()
	if issuer != s.teamID {
		return false
	}
	// Check expiration not too close
	expiration, _ := parsed.Expiration()
	if time.Until(expiration) <= 5*time.Minute {
		return false
	}
	return true
}

func (s *Service) loadPrivateKey() (*ecdsa.PrivateKey, error) {
	if s.privateKeyPath == "" {
		return nil, errors.New("applemusic private key path not configured")
	}
	pemBytes, err := os.ReadFile(s.privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("reading private key: %w", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || len(block.Bytes) == 0 {
		return nil, errors.New("invalid PEM data for private key")
	}
	pkcs8, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing PKCS#8 key: %w", err)
	}
	key, ok := pkcs8.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not ECDSA")
	}
	return key, nil
}

// ------- Recent Played Tracks ingestion -------

// AppleRecentTrack models a subset of Apple Music API track response
type AppleRecentTrack struct {
	ID         string `json:"id"`
	Attributes struct {
		Name             string  `json:"name"`
		ArtistName       string  `json:"artistName"`
		AlbumName        string  `json:"albumName"`
		DurationInMillis *int64  `json:"durationInMillis"`
		Isrc             *string `json:"isrc"`
		URL              string  `json:"url"`
		PlayParams       *struct {
			ID        string `json:"id"`
			Kind      string `json:"kind"`
			CatalogID string `json:"catalogId"`
		} `json:"playParams"`
	} `json:"attributes"`
}

// Generates a hash representing the track name, album name, and artist name,
// to be used for comparing subsequent uploaded Apple Music tracks
func generateUploadHash(track *AppleRecentTrack) string {
	input := track.Attributes.Name + track.Attributes.AlbumName + track.Attributes.ArtistName
	hash := sha256.Sum256([]byte(input))
	return fmt.Sprintf("am_uploaded_%x", hash)
}

type recentPlayedResponse struct {
	Data []AppleRecentTrack `json:"data"`
}

type appleMusicErrorResponse struct {
	Errors []struct {
		Status string `json:"status"`
		Code   string `json:"code"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	} `json:"errors"`
}

// apiError carries the Apple Music status code alongside the formatted message
// so callers can tell a dead user token apart from a blip on Apple's side.
type apiError struct {
	StatusCode int
	RetryAfter time.Duration
	message    string
}

func (e *apiError) Error() string { return e.message }

// permanent reports whether retrying could ever succeed. Apple answers 401 or
// 403 when the Music-User-Token has expired or the user revoked access; only
// re-linking fixes that.
func (e *apiError) permanent() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// retryable reports whether the same request is worth repeating within this
// tick. 429 is deliberately excluded: we honour Retry-After by deferring the
// user instead of holding the cycle open.
func (e *apiError) retryable() bool {
	return e.StatusCode >= 500
}

func newAppleMusicAPIError(statusCode int, status string, body []byte, retryAfter time.Duration) *apiError {
	err := &apiError{StatusCode: statusCode, RetryAfter: retryAfter}

	var parsed appleMusicErrorResponse
	if jsonErr := json.Unmarshal(body, &parsed); jsonErr == nil && len(parsed.Errors) > 0 {
		apiErr := parsed.Errors[0]
		message := strings.TrimSpace(apiErr.Title)
		if detail := strings.TrimSpace(apiErr.Detail); detail != "" {
			if message != "" {
				message += ": "
			}
			message += detail
		}
		if code := strings.TrimSpace(apiErr.Code); code != "" {
			if message != "" {
				message += " "
			}
			message += "[" + code + "]"
		}
		if message != "" {
			err.message = fmt.Sprintf("apple music api error: %s: %s", status, message)
			return err
		}
	}

	err.message = fmt.Sprintf("apple music api error: %s", status)
	return err
}

// parseRetryAfter reads a Retry-After header, which Apple sends as a delay in
// seconds. An absent or unparseable value yields zero.
func parseRetryAfter(header string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// doAPIRequest executes an Apple Music GET and returns the body, retrying
// transient failures with exponential backoff. newReq builds a fresh request
// per attempt so nothing is reused across retries.
func (s *Service) doAPIRequest(ctx context.Context, newReq func() (*http.Request, error)) ([]byte, error) {
	var lastErr error

	for attempt := range apiMaxAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(apiRetryBaseWait << (attempt - 1)):
			}
		}

		req, err := newReq()
		if err != nil {
			return nil, err
		}

		resp, err := s.httpClient.Do(req)
		if err != nil {
			// Network-level failure (timeout, connection reset): transient.
			lastErr = err
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil {
			s.logger.Printf("failed to close response body: %v", closeErr)
		}
		if readErr != nil {
			lastErr = fmt.Errorf("failed to read response body: %w", readErr)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			return body, nil
		}

		apiErr := newAppleMusicAPIError(resp.StatusCode, resp.Status, body, parseRetryAfter(resp.Header.Get("Retry-After")))
		if !apiErr.retryable() {
			return nil, apiErr
		}
		lastErr = apiErr
	}

	return nil, lastErr
}

// FetchRecentPlayedTracks calls Apple Music API for a user token. Results come
// back newest-first.
func (s *Service) FetchRecentPlayedTracks(ctx context.Context, userToken string, limit int) ([]AppleRecentTrack, error) {
	if limit <= 0 || limit > 30 {
		limit = 25
	}
	devToken, _, err := s.GenerateDeveloperToken()
	if err != nil {
		return nil, err
	}
	endpoint := &url.URL{Scheme: "https", Host: "api.music.apple.com", Path: "/v1/me/recent/played/tracks"}
	q := endpoint.Query()
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("types", "songs,library-songs")
	endpoint.RawQuery = q.Encode()

	bodyBytes, err := s.doAPIRequest(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+devToken)
		req.Header.Set("Music-User-Token", userToken)
		return req, nil
	})
	if err != nil {
		return nil, err
	}

	var parsed recentPlayedResponse
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return nil, err
	}
	return parsed.Data, nil
}

// trackKey returns the URL to persist for a track. Uploaded tracks carry no
// catalog URL, so they fall back to a hash of their metadata.
func trackKey(t *AppleRecentTrack) string {
	if t.Attributes.URL != "" {
		return t.Attributes.URL
	}
	return generateUploadHash(t)
}

// appleResourceID returns Apple's own identity for a play, which is what we
// match history on. The catalog id is preferred because the same song reports
// a different resource id depending on whether it was played from the catalog
// or from the user's library. Entries with no id at all (uploads) fall back to
// a metadata hash.
func appleResourceID(t *AppleRecentTrack) string {
	if t.Attributes.PlayParams != nil {
		if id := t.Attributes.PlayParams.CatalogID; id != "" {
			return id
		}
		if id := t.Attributes.PlayParams.ID; id != "" {
			return id
		}
	}
	if t.ID != "" {
		return t.ID
	}
	return generateUploadHash(t)
}

// itemIdentity carries both identities a stored row might have been written
// with, so the cursor keeps working across rows saved before and after
// source_id existed.
type itemIdentity struct {
	sourceID string
	url      string
}

func identify(t *AppleRecentTrack) itemIdentity {
	return itemIdentity{sourceID: appleResourceID(t), url: trackKey(t)}
}

// matches reports whether a stored row refers to this play. Rows written before
// source_id existed can only be compared on their URL.
func (k itemIdentity) matches(stored *models.Track) bool {
	if stored.SourceID != "" {
		return stored.SourceID == k.sourceID
	}
	return stored.URL == k.url
}

// toTrack converts AppleRecentTrack to internal models.Track. playedAt is
// synthesised by the caller: Apple's recently-played endpoint returns no
// played-at timestamp of its own.
func (s *Service) toTrack(t AppleRecentTrack, playedAt time.Time) *models.Track {
	var duration int64
	if t.Attributes.DurationInMillis != nil {
		duration = *t.Attributes.DurationInMillis
	}
	isrc := ""
	if t.Attributes.Isrc != nil {
		isrc = *t.Attributes.Isrc
	}

	track := &models.Track{
		Name:           t.Attributes.Name,
		Artist:         []models.Artist{{Name: t.Attributes.ArtistName}},
		Album:          t.Attributes.AlbumName,
		URL:            trackKey(&t),
		DurationMs:     duration,
		ProgressMs:     duration, // Assume full play since Apple Music doesn't provide partial plays
		ServiceBaseUrl: "music.apple.com",
		ISRC:           isrc,
		SourceID:       appleResourceID(&t),
		// Apple only lists a track under recently-played once it has actually
		// been played, so anything we see here is a completed listen.
		HasStamped: true,
		Timestamp:  playedAt,
	}

	if s.mbService != nil {
		hydrated, err := musicbrainz.HydrateTrack(s.mbService, *track)
		if err == nil && hydrated != nil {
			track = hydrated
		}
	}
	return track
}

// storefrontID returns the user's Apple Music storefront, caching it for the
// process lifetime. A storefront is effectively constant per account, and
// re-fetching it for every library song is a request we can do without.
func (s *Service) storefrontID(ctx context.Context, user *models.User, devToken string) (string, error) {
	s.mu.RLock()
	cached, ok := s.storefronts[user.ID]
	s.mu.RUnlock()
	if ok {
		return cached, nil
	}

	endpoint := &url.URL{Scheme: "https", Host: "api.music.apple.com", Path: "/v1/me/storefront"}
	body, err := s.doAPIRequest(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+devToken)
		req.Header.Set("Music-User-Token", *user.AppleMusicUserToken)
		return req, nil
	})
	if err != nil {
		return "", err
	}

	var storefront struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &storefront); err != nil {
		return "", fmt.Errorf("failed to decode storefront response: %w", err)
	}
	if len(storefront.Data) == 0 || storefront.Data[0].ID == "" {
		return "", errors.New("Apple Music storefront response contained no storefront")
	}

	s.mu.Lock()
	if s.storefronts == nil {
		s.storefronts = make(map[int64]string)
	}
	s.storefronts[user.ID] = storefront.Data[0].ID
	s.mu.Unlock()

	return storefront.Data[0].ID, nil
}

// catalogSong is the catalog metadata a library song arrives without. Apple
// files the same recording under a library id whose attributes carry neither a
// share URL nor an ISRC.
type catalogSong struct {
	url  string
	isrc string
}

// populateCatalogMetadata fills in the details a library song arrives without,
// from the catalog record Apple points at in its play parameters.
//
// The URL is what gives a persisted play a real origin URI. The ISRC is what
// lets MusicBrainz identify the recording outright; without it a lookup falls
// back to matching on title, artist and duration, which resolves the wrong
// recording for any song whose title another artist also used.
func (s *Service) populateCatalogMetadata(ctx context.Context, user *models.User, track *AppleRecentTrack) error {
	if track == nil || track.Attributes.PlayParams == nil {
		return nil
	}

	catalogID := track.Attributes.PlayParams.CatalogID
	needURL := track.Attributes.URL == ""
	needISRC := track.Attributes.Isrc == nil || *track.Attributes.Isrc == ""
	// A catalog song already reports both, so it costs no request at all.
	if catalogID == "" || (!needURL && !needISRC) {
		return nil
	}

	// The window largely repeats between ticks, so caching keeps the scan at
	// roughly zero extra requests in the steady state.
	s.mu.RLock()
	cached, ok := s.catalogSongs[catalogID]
	s.mu.RUnlock()
	if ok {
		applyCatalogSong(track, cached)
		return nil
	}

	devToken, _, err := s.GenerateDeveloperToken()
	if err != nil {
		return err
	}

	storefrontID, err := s.storefrontID(ctx, user, devToken)
	if err != nil {
		return err
	}

	catalogEndpoint := &url.URL{
		Scheme: "https",
		Host:   "api.music.apple.com",
		Path:   "/v1/catalog/" + url.PathEscape(storefrontID) + "/songs",
	}
	query := catalogEndpoint.Query()
	query.Set("ids", catalogID)
	catalogEndpoint.RawQuery = query.Encode()

	catalogBody, err := s.doAPIRequest(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogEndpoint.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+devToken)
		return req, nil
	})
	if err != nil {
		return err
	}

	var catalog struct {
		Data []struct {
			Attributes struct {
				URL  string `json:"url"`
				ISRC string `json:"isrc"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(catalogBody, &catalog); err != nil {
		return fmt.Errorf("failed to decode catalog song response: %w", err)
	}
	if len(catalog.Data) == 0 {
		return errors.New("Apple Music catalog response contained no song")
	}

	song := catalogSong{
		url:  catalog.Data[0].Attributes.URL,
		isrc: catalog.Data[0].Attributes.ISRC,
	}
	applyCatalogSong(track, song)

	s.mu.Lock()
	if s.catalogSongs == nil {
		s.catalogSongs = make(map[string]catalogSong)
	}
	// Crude bound: this is a lookup cache, so dropping it wholesale just costs
	// a few requests on the next poll.
	if len(s.catalogSongs) >= catalogCacheMax {
		clear(s.catalogSongs)
	}
	s.catalogSongs[catalogID] = song
	s.mu.Unlock()

	// A library song with no catalog URL cannot be given an origin URI, which
	// is what the caller logs. A missing ISRC is not worth reporting: plenty of
	// catalog entries genuinely carry none.
	if needURL && track.Attributes.URL == "" {
		return errors.New("Apple Music catalog response contained no song URL")
	}

	return nil
}

// applyCatalogSong fills in whichever details the play itself did not carry,
// leaving anything Apple already reported for the play untouched.
func applyCatalogSong(track *AppleRecentTrack, song catalogSong) {
	if track.Attributes.URL == "" && song.url != "" {
		track.Attributes.URL = song.url
	}
	if song.isrc != "" && (track.Attributes.Isrc == nil || *track.Attributes.Isrc == "") {
		isrc := song.isrc
		track.Attributes.Isrc = &isrc
	}
}

// ProcessUser ingests every Apple Music play that has appeared since the last
// track stored for this user.
//
// Apple exposes only a recently-played history: there is no "currently
// playing" endpoint and no played-at timestamp on any entry. So we poll a
// window of recent plays and line it up against the history we already hold to
// work out which entries are new.
func (s *Service) ProcessUser(ctx context.Context, user *models.User) error {
	if user.AppleMusicUserToken == nil || *user.AppleMusicUserToken == "" {
		return nil
	}

	items, err := s.FetchRecentPlayedTracks(ctx, *user.AppleMusicUserToken, recentWindow)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}

	stored, err := s.DB.GetRecentTracksForService(user.ID, db.SourceAppleMusic, recentWindow)
	if err != nil {
		// Without our own history we cannot tell new plays from old ones, and
		// guessing would duplicate the whole window.
		return fmt.Errorf("reading stored apple music tracks for user %d: %w", user.ID, err)
	}

	newItems, err := s.newSince(ctx, user, items, stored)
	if err != nil {
		return err
	}
	if len(newItems) == 0 {
		s.expireNowPlaying(ctx, user.ID)
		return nil
	}

	var lastTrack *models.Track
	if len(stored) > 0 {
		lastTrack = stored[0]
	}
	playedAt := batchTimestamps(newItems, lastTrack)

	// Save oldest-first so an interruption leaves a contiguous watermark and
	// the next tick resumes exactly where this one stopped.
	var newest *models.Track
	for i := len(newItems) - 1; i >= 0; i-- {
		track, err := s.ingest(ctx, user, newItems[i], playedAt[i])
		if err != nil {
			return err
		}
		if i == 0 {
			newest = track
		}
	}

	if newest != nil {
		s.publishNowPlaying(ctx, user.ID, newest, appleResourceID(&newItems[0]))
	}

	return nil
}

// newSince returns the window entries played since we last synced, newest-first.
//
// Matching only the single newest stored row is not enough: if the user goes
// A -> B -> A, the newest window entry is A, which equals the stored row, and
// both B and the repeat would be dropped. Matching the *last* occurrence
// instead would re-ingest the whole window whenever an older play of the same
// track is still in it.
//
// So we align sequences: find the smallest offset at which the remainder of
// Apple's window lines up with the history we already hold. That offset is
// exactly how many plays are new, and it handles repeats without duplicating.
//
// It fails rather than guess when a library song's catalog URL cannot be
// resolved, since matching that entry on anything else would misalign it.
func (s *Service) newSince(ctx context.Context, user *models.User, items []AppleRecentTrack, stored []*models.Track) ([]AppleRecentTrack, error) {
	// First sync for this user: take only the newest play to establish a
	// cursor. Backfilling the whole window here would publish a batch of
	// historical plays to their PDS with timestamps we invented.
	if len(stored) == 0 {
		return items[:1], nil
	}

	// Rows saved before source_id existed can only be matched on their URL, and
	// for a library song that means resolving its catalog URL first. Once those
	// rows age out of the window this round trip disappears entirely.
	needURLs := false
	for _, track := range stored {
		if track.SourceID == "" {
			needURLs = true
			break
		}
	}

	window := make([]itemIdentity, len(items))
	for i := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if needURLs {
			if err := s.populateCatalogMetadata(ctx, user, &items[i]); err != nil {
				return nil, fmt.Errorf("resolving catalog metadata for %q: %w", items[i].Attributes.Name, err)
			}
		}
		window[i] = identify(&items[i])
	}

	offset, ok := alignmentOffset(window, stored)
	if ok {
		return items[:offset], nil
	}

	// Nothing lines up, either because more plays happened than the window
	// holds or because our history does not mirror Apple's. We cannot tell
	// which, and treating the whole window as new would republish every play
	// in it on the second case, so fall back to the newest play alone.
	s.logger.Printf(
		"user %d: stored history does not line up with the %d-track window; earlier plays may have been missed",
		user.ID, len(items))
	if window[0].matches(stored[0]) {
		return nil, nil
	}
	return items[:1], nil
}

// alignmentOffset returns how many entries at the head of window are new, by
// finding the smallest offset at which the rest of window matches the start of
// history. It reports false when no offset matches.
func alignmentOffset(window []itemIdentity, history []*models.Track) (int, bool) {
	for offset := range window {
		if matchesHistory(window[offset:], history) {
			return offset, true
		}
	}
	return 0, false
}

// matchesHistory reports whether tail is a prefix of history, comparing over
// whichever is shorter. Both are newest-first.
//
// The comparison is only as strong as the history is deep. With a single
// stored row, a window of A B A matches at offset 0 as readily as at offset 2,
// so an immediate repeat right after a user's first ever sync is missed once.
// Every later poll has enough history to disambiguate.
func matchesHistory(tail []itemIdentity, history []*models.Track) bool {
	n := min(len(tail), len(history))
	if n == 0 {
		return false
	}
	for i := range n {
		if !tail[i].matches(history[i]) {
			return false
		}
	}
	return true
}

// batchTimestamps synthesises a played-at for each entry of a newest-first
// batch. Apple gives us none, and the history the cursor aligns against is
// read in timestamp order, so every stamp has to be strictly increasing in play
// order and later than lastTrack. A batch that reached back past lastTrack
// would interleave with the stored rows, and the next tick's alignment would
// then fail against a history that no longer mirrors Apple's.
//
// Each entry is backdated from the one after it by its own duration, starting
// from "now", which is what back-to-back playback would have looked like, and
// then pushed forward where needed to stay clear of whatever precedes it.
func batchTimestamps(items []AppleRecentTrack, lastTrack *models.Track) []time.Time {
	stamps := make([]time.Time, len(items))
	for i := range items {
		if i == 0 {
			stamps[i] = time.Now().UTC()
			continue
		}
		gap := time.Second
		if d := items[i].Attributes.DurationInMillis; d != nil && *d >= int64(time.Second/time.Millisecond) {
			gap = time.Duration(*d) * time.Millisecond
		}
		stamps[i] = stamps[i-1].Add(-gap)
	}

	var floor time.Time
	if lastTrack != nil {
		floor = lastTrack.Timestamp
	}
	for i := len(stamps) - 1; i >= 0; i-- {
		if !stamps[i].After(floor) {
			stamps[i] = floor.Add(time.Second)
		}
		floor = stamps[i]
	}
	return stamps
}

// ingest persists one play and submits it to the user's PDS. It returns nil
// when the entry carried no usable metadata.
func (s *Service) ingest(ctx context.Context, user *models.User, item AppleRecentTrack, playedAt time.Time) (*models.Track, error) {
	// Library songs arrive without a share URL or an ISRC. Resolve them here so
	// the play we persist carries a real origin URI and MusicBrainz can identify
	// the recording outright. This is a no-op when the scan already resolved it.
	if err := s.populateCatalogMetadata(ctx, user, &item); err != nil {
		s.logger.Printf("failed to resolve Apple Music catalog metadata for %q: %v", item.Attributes.Name, err)
	}

	track := s.toTrack(item, playedAt)
	if strings.TrimSpace(track.Name) == "" || len(track.Artist) == 0 {
		s.logger.Printf("skipping track with no usable metadata for user %d", user.ID)
		return nil, nil
	}

	if _, err := s.DB.SaveTrack(user.ID, db.SourceAppleMusic, track); err != nil {
		return nil, fmt.Errorf("saving apple music track for user %d: %w", user.ID, err)
	}

	s.logger.Printf("saved track for user %d: %s by %s", user.ID, track.Name, track.Artist[0].Name)

	if user.ATProtoDID != nil && user.MostRecentAtProtoSessionID != nil && s.atprotoService != nil {
		if err := atprotoservice.SubmitPlayToPDS(ctx, *user.ATProtoDID, *user.MostRecentAtProtoSessionID, track, s.atprotoService); err != nil {
			s.logger.Printf("failed submit to PDS for user %d: %v", user.ID, err)
		}
	}

	return track, nil
}

// publishNowPlaying announces the newest play as the user's current track and
// records when it was published so it can be expired later.
func (s *Service) publishNowPlaying(ctx context.Context, userID int64, track *models.Track, key string) {
	if s.playingNowService == nil {
		return
	}

	if err := s.playingNowService.PublishPlayingNow(ctx, userID, track); err != nil {
		s.logger.Printf("Error publishing playing now for user %d: %v", userID, err)
		return
	}

	duration := time.Duration(track.DurationMs) * time.Millisecond
	if duration <= 0 {
		duration = 5 * time.Minute
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nowPlaying == nil {
		s.nowPlaying = make(map[int64]*nowPlayingState)
	}
	s.nowPlaying[userID] = &nowPlayingState{key: key, publishedAt: time.Now(), duration: duration}
}

// expireNowPlaying clears a now-playing status once the track we published has
// had time to finish. Apple cannot tell us playback stopped, so elapsed time is
// the only signal available.
func (s *Service) expireNowPlaying(ctx context.Context, userID int64) {
	if s.playingNowService == nil {
		return
	}

	s.mu.Lock()
	state := s.nowPlaying[userID]
	expired := state != nil && !state.cleared && time.Since(state.publishedAt) > state.duration
	if expired {
		state.cleared = true
	}
	s.mu.Unlock()

	if !expired {
		return
	}

	if err := s.playingNowService.ClearPlayingNow(ctx, userID); err != nil {
		s.logger.Printf("Error clearing playing now for user %d: %v", userID, err)
	}
}

// StartListeningTracker periodically fetches recent plays for Apple Music linked users
func (s *Service) StartListeningTracker(interval time.Duration) {
	if s.DB == nil {
		if s.logger != nil {
			s.logger.Printf("DB not configured; Apple Music tracker disabled")
		}
		return
	}
	ticker := time.NewTicker(interval)
	go func() {
		s.runOnce(context.Background())
		for range ticker.C {
			s.runOnce(context.Background())
		}
	}()
}

func (s *Service) runOnce(ctx context.Context) {
	users, err := s.DB.GetAllAppleMusicLinkedUsers()
	if err != nil {
		s.logger.Printf("error loading Apple Music users: %v", err)
		return
	}
	for _, u := range users {
		if ctx.Err() != nil {
			return
		}
		if !s.readyToSync(u.ID) {
			continue
		}
		s.syncUser(ctx, u)
	}
}

// syncUser runs one user's sync under its own deadline, converting a panic into
// a logged error. The tracker shares a process with the HTTP server, so one bad
// row must not take the whole of piper down with it.
func (s *Service) syncUser(ctx context.Context, user *models.User) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Printf("panic processing user %d: %v\n%s", user.ID, r, debug.Stack())
			s.recordFailure(user.ID, 0)
		}
	}()

	userCtx, cancel := context.WithTimeout(ctx, userSyncTimeout)
	defer cancel()

	err := s.ProcessUser(userCtx, user)
	if err == nil {
		s.recordSuccess(user.ID)
		return
	}

	s.logger.Printf("error processing user %d: %v", user.ID, err)

	var apiErr *apiError
	if errors.As(err, &apiErr) {
		if apiErr.permanent() {
			// The Music-User-Token is expired or revoked. Drop it so the user
			// falls out of the poll roster and the UI prompts a re-link
			// instead of us retrying a dead token forever.
			s.logger.Printf("clearing dead Apple Music token for user %d: %v", user.ID, err)
			if clearErr := s.DB.ClearAppleMusicUserToken(user.ID); clearErr != nil {
				s.logger.Printf("failed clearing Apple Music token for user %d: %v", user.ID, clearErr)
			}
			s.recordSuccess(user.ID)
			return
		}
		s.recordFailure(user.ID, apiErr.RetryAfter)
		return
	}

	s.recordFailure(user.ID, 0)
}

// readyToSync reports whether a user's backoff window has elapsed.
func (s *Service) readyToSync(userID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := s.syncStates[userID]
	return state == nil || !time.Now().Before(state.nextAttempt)
}

func (s *Service) recordSuccess(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.syncStates, userID)
}

// recordFailure backs a user off exponentially. retryAfter, when Apple supplied
// one, wins over the computed delay.
func (s *Service) recordFailure(userID int64, retryAfter time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.syncStates == nil {
		s.syncStates = make(map[int64]*syncState)
	}
	state := s.syncStates[userID]
	if state == nil {
		state = &syncState{}
		s.syncStates[userID] = state
	}
	state.consecutiveFailures++

	delay := syncBackoffBase << min(state.consecutiveFailures-1, 8)
	if delay > syncBackoffMax {
		delay = syncBackoffMax
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	state.nextAttempt = time.Now().Add(delay)
}
