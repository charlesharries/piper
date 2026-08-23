package musicbrainz

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	Artist     Artist `json:"artist"`
	Joinphrase string `json:"joinphrase,omitempty"`
	Name       string `json:"name"`
}

type Artist struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	SortName string `json:"sort-name,omitempty"`
	// Aliases are the other names an artist is known by, which is where the
	// Latin spelling of a name MusicBrainz holds in another script lives. Search
	// responses carry them; the recording lookup endpoint does not.
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

// ListenBrainzResult is ListenBrainz's answer for a play.
//
// Only the recording is taken on ListenBrainz's word. Its release and artist
// ids are deliberately not carried: the recording is looked up against
// MusicBrainz anyway, and the release is re-derived from that, so accepting
// ListenBrainz's would mean two sources of truth for the same field.
//
// That once included the release the Cover Art Archive holds art for. It was
// dropped because ListenBrainz picks a different one for nearly every track of
// the same album -- seven across one ten-track soundtrack -- and it entered
// release scoring at the same weight as whether a pressing is official at all,
// so it scattered an album across pressings. preferReleaseWithArt finds the
// pressings that have art from MusicBrainz directly, which is both independent
// and consistent.
type ListenBrainzResult struct {
	RecordingMBID string
}

// ListenBrainzClient resolves a play to MusicBrainz identifiers via
// ListenBrainz. It is optional: when absent, resolution falls back to search.
type ListenBrainzClient interface {
	Lookup(ctx context.Context, artist, recording, release string) (*ListenBrainzResult, error)
}

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

// newLogger builds the service's logger. Matching fails quietly -- a play just
// ends up carrying the wrong MBID -- so the logs are the only record of what was
// decided, and they are written as JSON for a log pipeline to query rather than
// as prose.
//
// Logs go to stderr, where the rest of piper's logging already goes, and out of
// the way of the cli, which writes its result as JSON on stdout.
func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
}

// logLevel reads the verbosity from LOG_LEVEL, defaulting to info. The
// hydration event is info; request retries are debug, being useful when chasing
// a specific bad match and noise the rest of the time.
func logLevel() slog.Level {
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return level
}

// NewMusicBrainzService creates a new service instance with rate limiting and caching.
func NewMusicBrainzService(db *db.DB, opts ...Option) *Service {
	// MusicBrainz allows 1 request per second
	limiter := rate.NewLimiter(rate.Every(time.Second), 1)

	// Set a default cache TTL (e.g., 1 hour)
	defaultCacheTTL := 1 * time.Hour

	s := &Service{
		db: db,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		limiter:        limiter,
		searchCache:    newTTLCache[[]Recording](maxSearchCacheEntries),
		pressingsCache: newTTLCache[pressings](maxSearchCacheEntries),
		artistCache:    newTTLCache[string](maxSearchCacheEntries),
		cacheTTL:       defaultCacheTTL,              // Set the cache TTL
		cleaner:        *NewMetadataCleaner("Latin"), // Initialize the cleaner
		logger:         newLogger(),
	}
	for _, opt := range opts {
		opt(s)
	}
	// Name the source on every line, whoever supplied the logger: once logs are
	// shipped somewhere central, matching lines have to be separable from the
	// trackers that feed plays into them.
	s.logger = s.logger.With("service", "musicbrainz")
	return s
}

// resolveViaListenBrainz asks ListenBrainz for an answer and verifies it against
// MusicBrainz before accepting. ListenBrainz can be confidently wrong, so the
// same scoring that guards search results guards its answers too.
func (s *Service) resolveViaListenBrainz(ctx context.Context, in matchInput, track models.Track) (*Match, error) {
	res, err := s.listenbrainz.Lookup(ctx, primaryArtist(track), track.Name, searchAlbum(track.Album))
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
		ev := eventFrom(ctx)
		ev.lbOutcome, ev.lbMBID, ev.lbScore = lbRejected, rec.ID, score
		return nil, nil
	}

	release := s.resolveRelease(ctx, in, *rec, nil)

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
		// artistname holds each artist's canonical name; artist holds the
		// rendered credit line. Foreign artists often have non-Latin canonical
		// names, so searching by both is more reliable.
		queryParts = append(queryParts, fmt.Sprintf("(%s OR %s)",
			phrase("artistname", params.Artist), phrase("artist", params.Artist)))
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
	// scopeArtist names an artist whose MBID is to be resolved and appended to
	// the query as an arid filter. The lookup costs a request of its own, so it
	// is left until the tier actually runs.
	scopeArtist string
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

// artistSearchLimit is how many artists to weigh when resolving a name. The
// artist wanted is routinely not MusicBrainz's top hit -- a search for "Eiko
// Ishibashi" ranks her trio above her -- so the answer is chosen on name
// agreement rather than on MusicBrainz's ranking.
const artistSearchLimit = 5

// buildArtistEndpoint builds a search for an artist by name.
//
// Both artist fields on the recording index hold names as catalogued: `artist`
// the credit line, `artistname` the artist's own name. Neither consults
// aliases, so an artist MusicBrainz holds in a non-Latin script cannot be found
// by their Latin name however it is spelled. The artist index does index
// aliases, which makes this the way back to their MBID.
func buildArtistEndpoint(name string) string {
	q := url.Values{}
	q.Set("query", fmt.Sprintf("%s OR %s", phrase("artist", name), phrase("alias", name)))
	q.Set("fmt", "json")
	q.Set("limit", strconv.Itoa(artistSearchLimit))
	return "https://musicbrainz.org/ws/2/artist?" + q.Encode()
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
// application string. Resolved per request rather than once at init, because the
// configured agent is not loaded until after package initialisation.
func userAgent() string {
	return models.SubmissionAgent() + " ( https://github.com/teal-fm/piper )"
}

// maxAttempts bounds retries of a single request. MusicBrainz sheds load with
// 503s routinely; without a retry the play loses its MBIDs permanently, because
// nothing ever revisits it.
const maxAttempts = 3

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
			// A dropped connection or a client timeout is as transient as a
			// 503, and the caller has no way to come back for this play later.
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
		return recordings, nil
	}

	var result SearchResponse
	if err := s.doRequest(ctx, endpoint, &result); err != nil {
		return nil, err
	}

	s.cacheRecordings(endpoint, result.Recordings)
	return result.Recordings, nil
}

// artistMBID resolves an artist name to its MusicBrainz id, returning an empty
// id when nobody convincingly goes by that name. A wrong id would scope a
// search to somebody else's catalogue, which is worse than not scoping it at
// all, so a near miss is rejected rather than merely ranked lower.
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
		// Not knowing who an artist is is as likely to be a gap MusicBrainz has
		// since filled as a settled answer, so it is forgotten quickly.
		mbid, ttl = "", negativeCacheTTL
	}
	s.artistCache.put(endpoint, mbid, ttl)
	return mbid, nil
}

// scopeTier resolves the artist a tier filters on, reporting false when the
// tier cannot run because nobody was found to scope it to.
func (s *Service) scopeTier(ctx context.Context, req searchRequest) (searchRequest, bool) {
	if req.scopeArtist == "" {
		return req, true
	}

	ev := eventFrom(ctx)
	mbid, err := s.artistMBID(ctx, req.scopeArtist)
	if err != nil {
		ev.artistScope = artistFailed
		ev.noteErr(err)
		return req, false
	}
	if mbid == "" {
		ev.artistScope = artistUnresolved
		return req, false
	}
	ev.artistScope = artistResolved

	req.query = fmt.Sprintf("%s AND arid:%s", req.query, mbid)
	return req, true
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
	album := searchAlbum(track.Album)
	cleanTrack, _ := s.cleaner.CleanRecording(track.Name)
	cleanArtist, _ := s.cleaner.CleanArtist(artist)
	cleanAlbum, _ := s.cleaner.CleanRecording(album)

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

	// Cleaned metadata, narrowing from all three fields down to two. Past that
	// the album is a scoring signal instead of a filter.
	addTier(SearchParams{Track: cleanTrack, Artist: cleanArtist, Release: cleanAlbum}, defaultSearchLimit)
	addTier(SearchParams{Track: cleanTrack, Artist: cleanArtist}, 50)

	// The cleaner is lossy: it truncates artist credits at the first comma and
	// strips non-Latin script entirely. Retry with what the service actually
	// sent us in case cleaning was what lost the match.
	addTier(SearchParams{Track: track.Name, Artist: artist, Release: album}, defaultSearchLimit)
	addTier(SearchParams{Track: track.Name, Artist: artist}, 50)

	// Every tier above finds an artist by the name they are catalogued under, so
	// an artist MusicBrainz holds in a non-Latin script is unreachable by the
	// Latin name a music service credits them with, and all of those tiers come
	// back empty. Resolving the name to an MBID goes through the artist index,
	// which does consult aliases, and filtering on that id sidesteps names
	// altogether. See buildArtistEndpoint.
	if scope := cmp.Or(cleanArtist, artist); scope != "" && cleanTrack != "" {
		tiers = append(tiers, searchRequest{
			query:       phrase("recording", cleanTrack),
			limit:       defaultSearchLimit,
			scopeArtist: scope,
		})
	}

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

// recordingAttrs describes what a play was resolved to, for the event's `out`
// group. The MBIDs are the point of it: a title says a match looks plausible, an
// MBID says which of the many recordings and pressings sharing that title was
// actually attached to the play, which is what a wrong match has to be traced
// back to.
func recordingAttrs(rec Recording, release *Release) []any {
	attrs := []any{
		slog.String("recording_mbid", rec.ID),
		slog.String("recording", rec.Title),
	}
	if len(rec.ArtistCredit) > 0 {
		attrs = append(attrs, slog.String("artist_mbid", rec.ArtistCredit[0].Artist.ID))
	}
	if release != nil {
		attrs = append(attrs,
			slog.String("release_mbid", release.ID),
			slog.String("release", release.Title))
		if release.ReleaseGroup != nil {
			attrs = append(attrs, slog.String("release_group_mbid", release.ReleaseGroup.ID))
		}
	}
	return attrs
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
// first tier that produces a confident match. It logs nothing as it goes; see
// event.go.
func (s *Service) Resolve(ctx context.Context, track models.Track) (*Match, error) {
	ctx, ev := startEvent(ctx, s.logger, track)
	defer ev.emit()

	in := newMatchInput(track)

	// ListenBrainz is purpose-built for this and beats raw search on messy
	// input, but it needs a token, so it is optional.
	var lbMatch *Match
	if s.listenbrainz != nil {
		ev.lbAttempted, ev.lbOutcome = true, lbMiss
		match, err := s.resolveViaListenBrainz(ctx, in, track)
		if err != nil {
			ev.lbOutcome = lbError
			ev.noteErr(err)
		}
		lbMatch = match
	}
	// A ListenBrainz answer beat no alternatives, so clearing minConfidence says less
	// about it than the same score would for a ranked search result. Accept it
	// outright only when nothing about it looks doubtful; otherwise keep it as
	// the incumbent and let search offer something better.
	if lbMatch != nil {
		reason, doubted := in.contradicts(lbMatch.Recording)
		ev.lbMBID, ev.lbScore = lbMatch.Recording.ID, lbMatch.Score
		if !doubted {
			ev.lbOutcome = lbAccepted
			ev.matched(lbMatch)
			return lbMatch, nil
		}
		ev.lbOutcome, ev.lbDoubt = lbDoubted, reason
	}

	var allCandidates []candidate
	var lastErr error
	// Whether any tier actually reached MusicBrainz. A doubted answer that
	// survives a search is a different thing from one that was never checked.
	var searched bool

	for _, tier := range s.searchTiers(track) {
		tier, ok := s.scopeTier(ctx, tier)
		if !ok {
			continue
		}

		ev.tiersRun++
		recordings, err := s.search(ctx, tier)
		if err != nil {
			lastErr = err
			ev.tiersFailed++
			ev.noteErr(err)
			continue
		}
		searched = true

		ranked := rankCandidates(in, recordings)
		if len(ranked) > len(allCandidates) {
			allCandidates = ranked
		}
		if len(ranked) == 0 || ranked[0].score < minConfidence {
			continue
		}

		best := ranked[0]
		// ListenBrainz's answer was doubted, not discarded; it still wins if
		// search cannot do better.
		if lbMatch != nil && lbMatch.Score >= best.score {
			ev.matched(lbMatch)
			return lbMatch, nil
		}

		release := s.resolveRelease(ctx, in, best.recording, nil)
		// Hand the winner its release so -explain can show what the play was
		// actually attributed to. Only the winner: the losers would each need
		// their own lookup, and the release is exactly what is being diagnosed
		// when the recording was right and the pressing was not.
		ranked[0].release = release
		match := &Match{
			Recording:  best.recording,
			Release:    release,
			Score:      best.score,
			Source:     "musicbrainz",
			Candidates: ranked,
		}
		ev.wonAtTier = ev.tiersRun
		ev.matched(match)
		return match, nil
	}

	// Doubted is not discarded: search ran, could not better it, and ListenBrainz's
	// answer still stands. But when no tier could be reached, the second opinion
	// we asked for never arrived, and publishing the answer anyway would put a
	// recording we distrusted into the user's repo on the strength of a
	// MusicBrainz outage.
	if lbMatch != nil && searched {
		ev.matched(lbMatch)
		return lbMatch, nil
	}
	if lbMatch != nil {
		ev.lbOutcome = lbDropped
	}
	if lastErr != nil && len(allCandidates) == 0 {
		ev.outcome = outcomeError
		return nil, lastErr
	}
	ev.unmatched(allCandidates)
	return &Match{Candidates: allCandidates}, ErrNoConfidentMatch
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
		// The search result's own release list still stands.
		eventFrom(ctx).noteErr(err)
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
	return HydrateTrackContext(context.Background(), mb, track)
}

// HydrateTrackContext is HydrateTrack on the caller's own context, so the
// hydration is cancelled with whatever started it and anything the caller
// attached with WithEventContext reaches the event.
func HydrateTrackContext(ctx context.Context, mb *Service, track models.Track) (*models.Track, error) {
	match, err := mb.Resolve(ctx, track)
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
