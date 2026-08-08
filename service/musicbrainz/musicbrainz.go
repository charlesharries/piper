package musicbrainz

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/teal-fm/piper/db"
	"github.com/teal-fm/piper/models"
)

// ArtistCredit API Types
type ArtistCredit struct {
	Artist struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		SortName string `json:"sort-name,omitempty"`
	} `json:"artist"`
	Joinphrase string `json:"joinphrase,omitempty"`
	Name       string `json:"name"`
}

type ReleaseGroup struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	PrimaryType    string   `json:"primary-type,omitempty"`
	SecondaryTypes []string `json:"secondary-types,omitempty"`
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
}

type Recording struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Score is MusicBrainz's own query match score (0-100). It says how well a
	// record matched the query, not whether it is the right recording, so it is
	// only ever used as a tiebreaker.
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

// maxSearchCacheEntries caps the search cache size.
const maxSearchCacheEntries = 1000

// negativeCacheTTL is how long an empty result is remembered for.
const negativeCacheTTL = 5 * time.Minute

// MapperResult is an external matching service's answer for a play.
//
// Only the recording is taken on the mapper's word. Its release and artist ids
// are deliberately not carried: the recording is looked up against MusicBrainz
// anyway, and the release is re-derived from that, so accepting the mapper's
// would mean two sources of truth for the same field.
type MapperResult struct {
	RecordingMBID string
	// CAAReleaseMBID is a release the Cover Art Archive is known to hold art
	// for, which makes it a good release to attribute the play to.
	CAAReleaseMBID string
}

// Mapper resolves a play to MusicBrainz identifiers using a dedicated matching
// service. It is optional: when absent, resolution falls back to search.
type Mapper interface {
	Lookup(ctx context.Context, artist, recording, release string) (*MapperResult, error)
}

type Service struct {
	db         *db.DB
	httpClient *http.Client
	limiter    *rate.Limiter
	mapper     Mapper
	cacheTTL   time.Duration   // Time-to-live for cache entries
	cleaner    MetadataCleaner // Cleaner for cleaning up expired cache entries
	logger     *log.Logger     // Logger for logging

	// searchCache holds search and recording lookup results, keyed by endpoint.
	searchCache *ttlCache[[]Recording]
	// pressingsCache holds a release group's pressings and which of them have
	// cover art, keyed by release group MBID.
	pressingsCache *ttlCache[pressings]
}

// Option configures a Service after construction.
type Option func(*Service)

// WithMapper enables an external matching service as the first-choice backend.
func WithMapper(m Mapper) Option {
	return func(s *Service) { s.mapper = m }
}

// WithHTTPClient replaces the HTTP client, so tests can serve canned responses
// instead of reaching musicbrainz.org.
func WithHTTPClient(c *http.Client) Option {
	return func(s *Service) { s.httpClient = c }
}

// NewMusicBrainzService creates a new service instance with rate limiting and caching.
func NewMusicBrainzService(db *db.DB, opts ...Option) *Service {
	// MusicBrainz allows 1 request per second
	limiter := rate.NewLimiter(rate.Every(time.Second), 1)
	// Set a default cache TTL (e.g., 1 hour)
	defaultCacheTTL := 1 * time.Hour

	// the main piper service writes all output to stdout, but since the cli
	// actually outputs JSON, logs should be written to stderr so they don't conflict.
	logger := log.New(os.Stderr, "musicbrainz: ", log.LstdFlags|log.Lmsgprefix)

	s := &Service{
		db: db,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		limiter:        limiter,
		searchCache:    newTTLCache[[]Recording](maxSearchCacheEntries),
		pressingsCache: newTTLCache[pressings](maxSearchCacheEntries),
		cacheTTL:       defaultCacheTTL,              // Set the cache TTL
		cleaner:        *NewMetadataCleaner("Latin"), // Initialize the cleaner
		logger:         logger,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// resolveViaMapper asks the external matcher (here, listenbrainz) for an answer
// and verifies it against MusicBrainz before accepting. Mappers can be confidently
// wrong, so the same scoring that guards search results guards these too.
func (s *Service) resolveViaMapper(ctx context.Context, in matchInput, track models.Track) (*Match, error) {
	res, err := s.mapper.Lookup(ctx, primaryArtist(track), track.Name, track.Album)
	if err != nil {
		return nil, err
	}
	if res == nil || res.RecordingMBID == "" {
		return nil, nil
	}

	rec, err := s.LookupRecording(ctx, res.RecordingMBID)
	if err != nil {
		return nil, err
	}

	score, reasons := scoreRecording(in, *rec)
	if score < minConfidence {
		s.logger.Printf("rejected listenbrainz match %q for %q: %.2f < %.2f [%s]",
			rec.Title, track.Name, score, minConfidence, strings.Join(reasons, " "))
		return nil, nil
	}

	artOwners := map[string]bool{}
	if res.CAAReleaseMBID != "" {
		artOwners[res.CAAReleaseMBID] = true
	}
	release := s.resolveRelease(ctx, in, *rec, artOwners)

	return &Match{
		Recording:  *rec,
		Release:    release,
		Score:      score,
		Source:     "listenbrainz",
		Candidates: []candidate{{recording: *rec, release: release, score: score, reasons: reasons}},
	}, nil
}

// escapeLucene escapes the characters that would otherwise terminate or corrupt
// a quoted Lucene phrase. Track titles containing quotes are common enough
// (Say "Yes", 'Til I Collapse) that leaving them raw produces malformed queries.
var escapeLucene = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace

func phrase(field, value string) string {
	return fmt.Sprintf(`%s:"%s"`, field, escapeLucene(value))
}

// luceneSpecial are the characters that carry meaning to the query parser.
const luceneSpecial = `+-&|!(){}[]^"~*?:\/`

// freeText strips query syntax rather than escaping it, for the dismax tier
// where the input is meant to read as plain words. Escaping there would leave
// backslashes in the text being matched against.
func freeText(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(luceneSpecial, r) {
			return ' '
		}
		return r
	}, s)
}

func buildSearchQuery(params SearchParams) string {
	var queryParts []string
	if params.ISRC != "" {
		queryParts = append(queryParts, phrase("isrc", params.ISRC))
	}
	if params.Track != "" {
		queryParts = append(queryParts, phrase("recording", params.Track))
	}
	if params.Artist != "" {
		// artistname matches the individual artists on a credit rather than the
		// rendered credit string, which is more forgiving for collaborations.
		queryParts = append(queryParts, phrase("artistname", params.Artist))
	}
	if params.Release != "" {
		queryParts = append(queryParts, phrase("release", params.Release))
	}
	return strings.Join(queryParts, " AND ")
}

// searchRequest is one attempt at the recording search endpoint.
type searchRequest struct {
	query string
	limit int
	// dismax swaps the Lucene parser for MusicBrainz's fuzzy one, which copes
	// better with unfielded, messy input.
	dismax bool
}

// defaultSearchLimit is the number of candidates to consider. MusicBrainz
// defaults to 25; scoring benefits from a wider pool because the right
// recording is regularly not in the first handful.
const defaultSearchLimit = 25

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
	// Note: the search endpoint ignores `inc`, so releases, release groups and
	// artist credits come back as part of its fixed response shape. ISRCs do
	// not, which is why the ISRC tier verifies via the query rather than the
	// response.
	return "https://musicbrainz.org/ws/2/recording?" + q.Encode()
}

// buildRecordingEndpoint builds a direct lookup for a known recording. Unlike
// search this honours `inc`, so it is the way to get a recording's full release
// list with release group types attached.
func buildRecordingEndpoint(mbid string) string {
	return fmt.Sprintf(
		"https://musicbrainz.org/ws/2/recording/%s?fmt=json&inc=releases+release-groups+artist-credits+isrcs",
		url.PathEscape(mbid),
	)
}

// cacheRecordings stores a search or lookup result against its endpoint.
func (s *Service) cacheRecordings(endpoint string, recordings []Recording) {
	// Misses expire quickly. A search that found nothing is often a transient
	// failure or a gap MusicBrainz has since filled, and holding it for the
	// full TTL means every replay of that track stays unmatched for an hour.
	ttl := s.cacheTTL
	if len(recordings) == 0 {
		ttl = negativeCacheTTL
	}
	s.searchCache.put(endpoint, recordings, ttl)
}

// userAgent identifies piper to MusicBrainz, which requires a contactable
// application string.
var userAgent = models.SubmissionAgent + " ( https://github.com/teal-fm/piper )"

// maxAttempts bounds retries of a single request. MusicBrainz sheds load with
// 503s routinely; without a retry the play loses its MBIDs permanently, because
// nothing ever revisits it.
const maxAttempts = 3

func executeRequest(ctx context.Context, client *http.Client, endpoint string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

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

// doRequest performs a rate-limited GET, retrying on the throttling statuses
// MusicBrainz uses, and decodes the body into out.
func (s *Service) doRequest(ctx context.Context, endpoint string, out any) error {
	var lastErr error

	for attempt := range maxAttempts {
		if err := s.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("rate limiter error: %w", err)
		}

		resp, err := executeRequest(ctx, s.httpClient, endpoint)
		if err != nil {
			// A dropped connection or a client timeout is as transient as a
			// 503, and the caller has no way to come back for this play later.
			if ctx.Err() != nil || attempt == maxAttempts-1 {
				return err
			}
			lastErr = err
			s.logger.Printf("retrying %s after transport error: %v", endpoint, err)
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
			s.logger.Printf("retrying %s in %s after status %d", endpoint, delay, resp.StatusCode)
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

func (s *Service) SearchMusicBrainz(ctx context.Context, params SearchParams) ([]Recording, error) {
	if params.Track == "" && params.Artist == "" && params.Release == "" && params.ISRC == "" {
		return nil, fmt.Errorf("at least one search parameter (Track, Artist, Release, ISRC) must be provided")
	}

	params.Track, _ = s.cleaner.CleanRecording(params.Track)
	params.Artist, _ = s.cleaner.CleanArtist(params.Artist)

	return s.search(ctx, searchRequest{query: buildSearchQuery(params), limit: defaultSearchLimit})
}

// search runs one search request, going through the cache.
func (s *Service) search(ctx context.Context, req searchRequest) ([]Recording, error) {
	endpoint := buildSearchEndpoint(req)

	if recordings, found := s.searchCache.get(endpoint); found {
		s.logger.Printf("Cache hit for MusicBrainz search: %s", req.query)
		return recordings, nil
	}
	s.logger.Printf("Cache miss for MusicBrainz search: %s", req.query)

	var result SearchResponse
	if err := s.doRequest(ctx, endpoint, &result); err != nil {
		return nil, err
	}

	s.cacheRecordings(endpoint, result.Recordings)
	return result.Recordings, nil
}

// LookupRecording fetches a single recording with its full release list. Search
// results carry only a subset of a recording's releases, so this is used to get
// a better release pool once the recording itself is settled.
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

// searchTiers returns the search attempts to make for a play, most constrained
// first. Each successive tier trades precision for recall; scoring is what
// keeps the looser tiers safe, and the first tier to produce a confident
// candidate wins.
func (s *Service) searchTiers(track models.Track) []searchRequest {
	artist := primaryArtist(track)
	cleanTrack, _ := s.cleaner.CleanRecording(track.Name)
	cleanArtist, _ := s.cleaner.CleanArtist(artist)
	cleanAlbum, _ := s.cleaner.CleanRecording(track.Album)

	var tiers []searchRequest
	seen := map[string]bool{}
	addTier := func(params SearchParams, limit int) {
		if params.Track == "" && params.ISRC == "" {
			return
		}
		query := buildSearchQuery(params)
		if query == "" || seen[query] {
			return
		}
		seen[query] = true
		tiers = append(tiers, searchRequest{query: query, limit: limit})
	}

	// An ISRC identifies a recording outright, so it needs no other filter --
	// and adding one causes false negatives when MusicBrainz holds the title in
	// a different script or language.
	if track.ISRC != "" {
		addTier(SearchParams{ISRC: track.ISRC}, defaultSearchLimit)
	}

	// Cleaned metadata, narrowing from all three fields down to two. The album
	// is dropped rather than trusted because services decorate it
	// ("Rumours (Super Deluxe)") in ways MusicBrainz does not; from here on it
	// is a scoring signal instead of a filter.
	addTier(SearchParams{Track: cleanTrack, Artist: cleanArtist, Release: cleanAlbum}, defaultSearchLimit)
	addTier(SearchParams{Track: cleanTrack, Artist: cleanArtist}, 50)

	// The cleaner is lossy: it truncates artist credits at the first comma and
	// strips non-Latin script entirely. Retry with what the service actually
	// sent us in case cleaning was what lost the match.
	addTier(SearchParams{Track: track.Name, Artist: artist, Release: track.Album}, defaultSearchLimit)
	addTier(SearchParams{Track: track.Name, Artist: artist}, 50)

	// Last resort: hand the bare words to MusicBrainz's fuzzy parser.
	if query := strings.Join(strings.Fields(freeText(track.Name+" "+artist)), " "); query != "" && !seen[query] {
		tiers = append(tiers, searchRequest{query: query, limit: 50, dismax: true})
	}

	return tiers
}

// primaryArtist renders the incoming artist credit as a single string.
func primaryArtist(track models.Track) string {
	names := make([]string, 0, len(track.Artist))
	for _, a := range track.Artist {
		if name := strings.TrimSpace(a.Name); name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}

// Match is the outcome of resolving a play against MusicBrainz.
type Match struct {
	Recording Recording
	Release   *Release
	Score     float64
	// Source names the backend that produced the match, for logging.
	Source string
	// Candidates holds every scored alternative, best first, for -explain.
	Candidates []candidate
}

// Explain renders the scored candidates, best first, for diagnosing why a
// lookup landed where it did.
func (m *Match) Explain() []string {
	lines := make([]string, 0, len(m.Candidates))
	for _, c := range m.Candidates {
		lines = append(lines, c.explain())
	}
	return lines
}

// ErrNoConfidentMatch is returned when nothing scored well enough to publish.
// Attaching a wrong MBID to a play is worse than attaching none: the record
// goes to the user's repo and misattributes their listening.
var ErrNoConfidentMatch = errors.New("no confident MusicBrainz match")

// Resolve finds the best MusicBrainz recording and release for a play.
//
// It works through progressively looser searches, scoring every candidate
// locally rather than trusting MusicBrainz's own ordering, and stops at the
// first tier that produces a confident match.
func (s *Service) Resolve(ctx context.Context, track models.Track) (*Match, error) {
	in := newMatchInput(track)

	// The ListenBrainz mapper is purpose-built for this and beats raw search on
	// messy input, but it needs a token, so it is optional.
	if s.mapper != nil {
		if match, err := s.resolveViaMapper(ctx, in, track); err != nil {
			s.logger.Printf("ListenBrainz mapper lookup failed for %q, falling back: %v", track.Name, err)
		} else if match != nil {
			s.logMatch(track, match)
			return match, nil
		}
	}

	var allCandidates []candidate
	var lastErr error

	for _, tier := range s.searchTiers(track) {
		recordings, err := s.search(ctx, tier)
		if err != nil {
			lastErr = err
			s.logger.Printf("search tier %q failed: %v", tier.query, err)
			continue
		}

		ranked := rankCandidates(in, recordings)
		if len(ranked) > len(allCandidates) {
			allCandidates = ranked
		}
		if len(ranked) == 0 || ranked[0].score < minConfidence {
			continue
		}

		best := ranked[0]
		release := s.resolveRelease(ctx, in, best.recording, nil)
		match := &Match{
			Recording:  best.recording,
			Release:    release,
			Score:      best.score,
			Source:     "musicbrainz",
			Candidates: ranked,
		}
		s.logMatch(track, match)
		return match, nil
	}

	if lastErr != nil && len(allCandidates) == 0 {
		return nil, lastErr
	}
	s.logNoMatch(track, allCandidates)
	return &Match{Candidates: allCandidates}, ErrNoConfidentMatch
}

// logMatch records which recording a play was attached to and what the score
// was built from. Matching fails quietly -- a play just ends up carrying the
// wrong MBID -- so every accepted match leaves a trail that can be read back.
func (s *Service) logMatch(track models.Track, match *Match) {
	release := "<none>"
	if match.Release != nil {
		release = match.Release.Title
	}
	var reasons []string
	if len(match.Candidates) > 0 {
		reasons = match.Candidates[0].reasons
	}
	s.logger.Printf("matched %q by %q -> %q / %q (%.2f via %s) [%s]",
		track.Name, primaryArtist(track), match.Recording.Title, release,
		match.Score, match.Source, strings.Join(reasons, " "))
}

// logNoMatch records a rejection along with the candidate that came closest, so
// a threshold set too high is visible rather than looking like MusicBrainz
// simply holding nothing.
func (s *Service) logNoMatch(track models.Track, candidates []candidate) {
	if len(candidates) == 0 {
		s.logger.Printf("no match for %q by %q: no candidates returned", track.Name, primaryArtist(track))
		return
	}
	best := candidates[0]
	s.logger.Printf("no confident match for %q by %q: best was %q at %.2f < %.2f [%s]",
		track.Name, primaryArtist(track), best.recording.Title, best.score, minConfidence,
		strings.Join(best.reasons, " "))
}

// resolveRelease picks the album to attribute a play to. When the release list
// that came back with the recording yields nothing convincing, it pays for one
// more request to fetch the recording's full release list, which search results
// only ever carry a slice of.
func (s *Service) resolveRelease(ctx context.Context, in matchInput, rec Recording, artOwners map[string]bool) *Release {
	release := s.pickRelease(ctx, in, rec, artOwners)

	// The release MBID is the only identifier in a play record that resolves to
	// cover art, so make a last pass for a pressing that actually has some.
	return s.preferReleaseWithArt(ctx, in, rec, release)
}

// pickRelease chooses the release on metadata grounds alone.
func (s *Service) pickRelease(ctx context.Context, in matchInput, rec Recording, artOwners map[string]bool) *Release {
	release, score, _ := bestRelease(in, rec.Releases, rec.Title, artOwners)
	if release != nil && score >= releaseConfidence {
		return release
	}

	full, err := s.LookupRecording(ctx, rec.ID)
	if err != nil {
		s.logger.Printf("release lookup for %s failed, keeping search result: %v", rec.ID, err)
		return release
	}

	better, betterScore, _ := bestRelease(in, full.Releases, rec.Title, artOwners)
	if better != nil && (release == nil || betterScore > score) {
		return better
	}
	return release
}

// HydrateTrack enriches a play with MusicBrainz identifiers.
//
// When nothing matches confidently the play is returned unchanged rather than
// carrying a guess, so callers can keep publishing it with the metadata the
// music service gave them.
func HydrateTrack(mb *Service, track models.Track) (*models.Track, error) {
	match, err := mb.Resolve(context.Background(), track)
	if err != nil {
		return nil, err
	}
	return ApplyMatch(track, match), nil
}

// ApplyMatch merges a resolved match into a play. Fields the music service
// supplied are preserved wherever MusicBrainz has nothing better: its data is
// more complete, but the service's data is what the user actually played.
func ApplyMatch(track models.Track, match *Match) *models.Track {
	result := track

	result.RecordingMBID = &match.Recording.ID
	// MusicBrainz omits lengths on plenty of recordings; letting a zero through
	// would erase the real duration and, for Spotify, break the play-time
	// threshold that decides when a track is stamped.
	result.DurationMs = cmp.Or(int64(match.Recording.Length), track.DurationMs)

	if len(match.Recording.ISRCs) > 0 {
		result.ISRC = cmp.Or(track.ISRC, match.Recording.ISRCs[0])
	}

	if len(match.Recording.ArtistCredit) > 0 {
		result.Artist = mergeArtists(track.Artist, match.Recording.ArtistCredit)
	}

	if match.Release != nil {
		result.ReleaseMBID = &match.Release.ID
		// Resolving to the base release drops the edition the service reported,
		// so keep it rather than silently losing that the user played the
		// deluxe or remastered issue.
		result.ReleaseDiscriminant = cmp.Or(track.ReleaseDiscriminant, releaseDiscriminant(track.Album))
		result.Album = match.Release.Title
	}

	return &result
}

// mergeArtists attaches MusicBrainz IDs to the artist credit without discarding
// the music service's own artist IDs, which are the only link back to the
// source catalogue.
func mergeArtists(existing []models.Artist, credits []ArtistCredit) []models.Artist {
	byName := make(map[string]models.Artist, len(existing))
	for _, a := range existing {
		byName[normalize(a.Name)] = a
	}

	artists := make([]models.Artist, len(credits))
	for i, c := range credits {
		mbid := c.Artist.ID
		artist := models.Artist{Name: c.Name, MBID: &mbid}
		if prior, ok := byName[normalize(c.Name)]; ok {
			artist.ID = prior.ID
		}
		artists[i] = artist
	}
	return artists
}
