package musicbrainz

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"golang.org/x/time/rate"

	"github.com/teal-fm/piper/db"
	"github.com/teal-fm/piper/models"
)

// Talking to MusicBrainz: addressing it, staying inside its rate limit, and
// remembering what it said. Nothing here decides whether an answer is right.

// ArtistCredit API Types
type ArtistCredit struct {
	Artist     Artist `json:"artist"`
	Joinphrase string `json:"joinphrase,omitempty"`
	Name       string `json:"name"`
}

type Artist struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	SortName string `json:"sort-name,omitempty"`
	// Aliases are where the Latin spelling of a name MusicBrainz holds in
	// another script lives. Search carries them; recording lookup does not.
	Aliases []Alias `json:"aliases,omitempty"`
}

type Alias struct {
	Name string `json:"name"`
}

type ArtistSearchResponse struct {
	Artists []Artist `json:"artists"`
}

type ReleaseGroup struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	PrimaryType    string   `json:"primary-type,omitempty"`
	SecondaryTypes []string `json:"secondary-types,omitempty"`
}

// Track is one entry of a release's listing: the title that release gives the
// recording, often not the recording's own.
type Track struct {
	Title string `json:"title"`
}

// Medium is one disc of a release. Only search results carry the listing, and
// they name it `track` where a release lookup would say `tracks`.
type Medium struct {
	Tracks []Track `json:"track,omitempty"`
}

type Release struct {
	ID             string        `json:"id"`
	Title          string        `json:"title"`
	Status         string        `json:"status,omitempty"`
	Date           string        `json:"date,omitempty"` // YYYY-MM-DD, YYYY-MM, or YYYY
	Country        string        `json:"country,omitempty"`
	Disambiguation string        `json:"disambiguation,omitempty"`
	TrackCount     int           `json:"track-count,omitempty"`
	ReleaseGroup   *ReleaseGroup `json:"release-group,omitempty"`
	Media          []Medium      `json:"media,omitempty"`
}

type Recording struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Score is MusicBrainz's own query match score (0-100): how well a record
	// matched the query -- not whether it is right!
	Score        int            `json:"score,omitempty"`
	Length       int            `json:"length,omitempty"` // milliseconds
	Video        bool           `json:"video,omitempty"`
	ISRCs        []string       `json:"isrcs,omitempty"`
	ArtistCredit []ArtistCredit `json:"artist-credit,omitempty"`
	Releases     []Release      `json:"releases,omitempty"`
}

type SearchResponse struct {
	Created    time.Time   `json:"created"`
	Count      int         `json:"count"`
	Offset     int         `json:"offset"`
	Recordings []Recording `json:"recordings"`
}

type SearchParams struct {
	Track   string
	Artist  string
	Release string
	ISRC    string
}

// maxSearchCacheEntries caps the size of each cache.
const maxSearchCacheEntries = 1000

// negativeCacheTTL is how long an empty result is remembered for. A search that
// found nothing is often transient, or a gap MusicBrainz has since filled, and
// holding it for the full TTL leaves every replay of that track unmatched.
const negativeCacheTTL = 5 * time.Minute

type Service struct {
	db           *db.DB
	httpClient   *http.Client
	limiter      *rate.Limiter
	listenbrainz ListenBrainzClient
	cacheTTL     time.Duration
	cleaner      MetadataCleaner
	logger       *slog.Logger

	// searchCache holds search and recording lookup results, keyed by endpoint.
	searchCache *ttlCache[[]Recording]
	// pressingsCache holds a release group's pressings and which of them have
	// cover art, keyed by release group MBID.
	pressingsCache *ttlCache[pressings]
	// artistCache holds resolved artist MBIDs, keyed by endpoint.
	artistCache *ttlCache[string]
}

// Option configures a Service after construction.
type Option func(*Service)

// WithListenBrainz enables ListenBrainz as the first-choice backend.
func WithListenBrainz(lb ListenBrainzClient) Option {
	return func(s *Service) { s.listenbrainz = lb }
}

// WithHTTPClient replaces the HTTP client, so tests can serve canned responses
// instead of reaching musicbrainz.org.
func WithHTTPClient(c *http.Client) Option {
	return func(s *Service) { s.httpClient = c }
}

// WithLogger replaces the logger, for callers that route logs somewhere other
// than stderr or want them silenced.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) { s.logger = l }
}

// NewMusicBrainzService creates a new service instance with rate limiting and caching.
func NewMusicBrainzService(db *db.DB, opts ...Option) *Service {
	s := &Service{
		db: db,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		// MusicBrainz allows one request per second, across the whole process.
		limiter:        rate.NewLimiter(rate.Every(time.Second), 1),
		searchCache:    newTTLCache[[]Recording](maxSearchCacheEntries),
		pressingsCache: newTTLCache[pressings](maxSearchCacheEntries),
		artistCache:    newTTLCache[string](maxSearchCacheEntries),
		cacheTTL:       1 * time.Hour,
		cleaner:        *NewMetadataCleaner("Latin"),
		logger:         newLogger(),
	}
	for _, opt := range opts {
		opt(s)
	}
	// Name the source on every line, whoever supplied the logger: matching
	// lines have to be separable from the trackers that feed plays in.
	s.logger = s.logger.With("service", "musicbrainz")
	return s
}

// newLogger builds the service's logger; musicbrainz uses wide events
// for help debugging matching problems.
func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
}

// logLevel reads the verbosity from LOG_LEVEL, defaulting to info. The
// hydration event is info; request retries are debug.
func logLevel() slog.Level {
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return level
}

// searchRequest is one call to the recording search endpoint.
type searchRequest struct {
	query string
	limit int
	// dismax swaps the Lucene parser for MusicBrainz's fuzzy one, which copes
	// better with unfielded, messy input.
	dismax bool
}

// searchLimit is how deep into MusicBrainz's own ranking each tier looks.
// Candidates are re-scored locally, so this is not how many results are wanted
// but how far down the right one is still allowed to sit: on the tiers that
// drop a filter, the recording being looked for routinely lands behind a run of
// compilations and live versions.
const searchLimit = 50

// artistSearchLimit is how many artists to weigh when resolving a name.
const artistSearchLimit = 5

func buildSearchEndpoint(req searchRequest) string {
	q := url.Values{}
	q.Set("query", req.query)
	q.Set("fmt", "json")
	if req.limit > 0 {
		q.Set("limit", strconv.Itoa(req.limit))
	}
	if req.dismax {
		q.Set("dismax", "true")
	}
	// The search endpoint ignores `inc`: releases, release groups and artist
	// credits come back anyway, ISRCs do not -- which is why the ISRC tier
	// verifies through the query rather than the response.
	return "https://musicbrainz.org/ws/2/recording?" + q.Encode()
}

// buildArtistEndpoint builds a search for an artist by name. Neither artist
// field on the *recording* index consults aliases, so an artist MusicBrainz
// holds in a non-Latin script cannot be found there by their Latin name. The
// artist index does index aliases, which makes this the way back to their MBID.
func buildArtistEndpoint(name string) string {
	q := url.Values{}
	q.Set("query", fmt.Sprintf("%s OR %s", phrase("artist", name), phrase("alias", name)))
	q.Set("fmt", "json")
	q.Set("limit", strconv.Itoa(artistSearchLimit))
	return "https://musicbrainz.org/ws/2/artist?" + q.Encode()
}

// buildRecordingEndpoint builds a direct lookup for a known recording. Unlike
// search it honours `inc`, so it is the way to a full release list.
func buildRecordingEndpoint(mbid string) string {
	return fmt.Sprintf(
		"https://musicbrainz.org/ws/2/recording/%s?fmt=json&inc=releases+release-groups+artist-credits+isrcs",
		url.PathEscape(mbid),
	)
}

func (s *Service) SearchMusicBrainz(ctx context.Context, params SearchParams) ([]Recording, error) {
	if params.Track == "" && params.Artist == "" && params.Release == "" && params.ISRC == "" {
		return nil, fmt.Errorf("at least one search parameter (Track, Artist, Release, ISRC) must be provided")
	}

	params.Track, _ = s.cleaner.CleanRecording(params.Track)
	params.Artist, _ = s.cleaner.CleanArtist(params.Artist)

	return s.search(ctx, searchRequest{query: buildSearchQuery(params), limit: searchLimit})
}

// search runs one search request, going through the cache.
func (s *Service) search(ctx context.Context, req searchRequest) ([]Recording, error) {
	endpoint := buildSearchEndpoint(req)

	if recordings, found := s.searchCache.get(endpoint); found {
		return recordings, nil
	}

	var result SearchResponse
	if err := s.doRequest(ctx, endpoint, &result); err != nil {
		return nil, err
	}

	s.cacheRecordings(endpoint, result.Recordings)
	return result.Recordings, nil
}

// LookupRecording fetches a single recording with its full release list, which
// search results carry only a subset of.
func (s *Service) LookupRecording(ctx context.Context, mbid string) (*Recording, error) {
	endpoint := buildRecordingEndpoint(mbid)

	if recordings, found := s.searchCache.get(endpoint); found && len(recordings) == 1 {
		return &recordings[0], nil
	}

	var rec Recording
	if err := s.doRequest(ctx, endpoint, &rec); err != nil {
		return nil, err
	}

	s.cacheRecordings(endpoint, []Recording{rec})
	return &rec, nil
}

// artistMBID resolves an artist name to its MusicBrainz id, returning empty
// when nobody convincingly goes by that name: scoping a search to somebody
// else's catalogue is worse than not scoping it, so a near miss is rejected.
func (s *Service) artistMBID(ctx context.Context, name string) (string, error) {
	endpoint := buildArtistEndpoint(name)

	if mbid, found := s.artistCache.get(endpoint); found {
		return mbid, nil
	}

	var result ArtistSearchResponse
	if err := s.doRequest(ctx, endpoint, &result); err != nil {
		return "", err
	}

	var mbid string
	var best float64
	for _, artist := range result.Artists {
		if agreement := artist.goesBy(name); agreement > best {
			mbid, best = artist.ID, agreement
		}
	}

	ttl := s.cacheTTL
	if best < artistNameAgreement {
		mbid, ttl = "", negativeCacheTTL
	}
	s.artistCache.put(endpoint, mbid, ttl)
	return mbid, nil
}

// cacheRecordings stores a search or lookup result against its endpoint.
func (s *Service) cacheRecordings(endpoint string, recordings []Recording) {
	ttl := s.cacheTTL
	if len(recordings) == 0 {
		ttl = negativeCacheTTL
	}
	s.searchCache.put(endpoint, recordings, ttl)
}

// userAgent identifies piper to MusicBrainz, which requires a contactable
// application string. Resolved per request because the configured agent is not
// loaded until after package initialisation.
func userAgent() string {
	return models.SubmissionAgent() + " ( https://github.com/teal-fm/piper )"
}

// maxAttempts bounds retries of a single request.
const maxAttempts = 3

// doRequest performs a rate-limited GET, retrying on the throttling statuses
// MusicBrainz uses, and decodes the body into out.
func (s *Service) doRequest(ctx context.Context, endpoint string, out any) error {
	var lastErr error
	ev := eventFrom(ctx)

	for attempt := range maxAttempts {
		if err := s.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("rate limiter error: %w", err)
		}
		ev.requests++

		resp, err := executeRequest(ctx, s.httpClient, endpoint)
		if err != nil {
			// A dropped connection is as transient as a 503.
			if ctx.Err() != nil || attempt == maxAttempts-1 {
				return err
			}
			lastErr = err
			s.logger.Debug("retrying request",
				"endpoint", endpoint, "attempt", attempt+1, "err", err)
			if !sleep(ctx, backoff(attempt)) {
				return ctx.Err()
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			delay, retryable := retryDelay(resp, attempt)
			resp.Body.Close()
			lastErr = fmt.Errorf("MusicBrainz API request to %s returned status %d", endpoint, resp.StatusCode)
			if !retryable || attempt == maxAttempts-1 {
				return lastErr
			}
			s.logger.Debug("retrying request",
				"endpoint", endpoint, "attempt", attempt+1, "delay", delay, "status", resp.StatusCode)
			if !sleep(ctx, delay) {
				return ctx.Err()
			}
			continue
		}

		err = json.NewDecoder(resp.Body).Decode(out)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("failed to decode response from %s: %w", endpoint, err)
		}
		return nil
	}

	return lastErr
}

func executeRequest(ctx context.Context, client *http.Client, endpoint string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent())

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("context error during request execution: %w", ctx.Err())
		}
		return nil, fmt.Errorf("failed to execute request to %s: %w", endpoint, err)
	}
	return resp, nil
}

// retryDelay reports how long to wait before retrying, and whether the status
// is worth retrying at all.
func retryDelay(resp *http.Response, attempt int) (time.Duration, bool) {
	if resp.StatusCode != http.StatusServiceUnavailable && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second, true
	}
	return backoff(attempt), true
}

func backoff(attempt int) time.Duration {
	return time.Duration(1<<attempt) * time.Second
}

// sleep waits for d, reporting false if the context was cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
