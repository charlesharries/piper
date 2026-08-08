// Package listenbrainz talks to ListenBrainz's MBID mapper, which resolves
// loose artist/track/release names to MusicBrainz identifiers. It is a
// purpose-built matcher and copes with the decorated metadata streaming
// services emit far better than raw MusicBrainz search does.
package listenbrainz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/teal-fm/piper/models"
	"github.com/teal-fm/piper/service/musicbrainz"
)

const lookupEndpoint = "https://api.listenbrainz.org/1/metadata/lookup/"

// userAgent identifies piper to ListenBrainz. Resolved per request rather than
// once at init, because the configured agent is not loaded until after package
// initialisation.
func userAgent() string {
	return models.SubmissionAgent() + " ( https://github.com/teal-fm/piper )"
}

// Mapper is a client for the ListenBrainz metadata lookup endpoint.
type Mapper struct {
	token      string
	httpClient *http.Client
	limiter    *rate.Limiter
}

// Option configures a Mapper.
type Option func(*Mapper)

// WithHTTPClient replaces the HTTP client, so tests can serve canned responses.
func WithHTTPClient(c *http.Client) Option {
	return func(m *Mapper) { m.httpClient = c }
}

// NewMapper builds a mapper for the given user token. The endpoint requires
// authentication, so an empty token yields a nil mapper and callers fall back
// to searching MusicBrainz directly.
func NewMapper(token string, opts ...Option) *Mapper {
	if strings.TrimSpace(token) == "" {
		return nil
	}

	m := &Mapper{
		token:      strings.TrimSpace(token),
		httpClient: &http.Client{Timeout: 10 * time.Second},
		// ListenBrainz is more permissive than MusicBrainz's 1/sec, but there
		// is no reason to lean on it.
		limiter: rate.NewLimiter(rate.Every(200*time.Millisecond), 4),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// lookupResponse is the subset of the mapper's reply that piper uses. The reply
// also carries release and artist ids, which are ignored: piper re-derives both
// from the recording it looks up.
type lookupResponse struct {
	RecordingMBID string `json:"recording_mbid"`
	Metadata      *struct {
		Release *struct {
			CAAID          *int64 `json:"caa_id"`
			CAAReleaseMBID string `json:"caa_release_mbid"`
		} `json:"release"`
	} `json:"metadata"`
}

// Lookup resolves a play to MusicBrainz identifiers. A miss is reported as a
// nil result with a nil error, since not matching is an ordinary outcome.
func (m *Mapper) Lookup(ctx context.Context, artist, recording, release string) (*musicbrainz.MapperResult, error) {
	if strings.TrimSpace(artist) == "" || strings.TrimSpace(recording) == "" {
		return nil, nil
	}

	query := url.Values{}
	query.Set("artist_name", artist)
	query.Set("recording_name", recording)
	if release != "" {
		query.Set("release_name", release)
	}
	// metadata+inc=release returns the Cover Art Archive ids alongside the
	// match, which tells us which release actually has artwork.
	query.Set("metadata", "true")
	query.Set("inc", "release")

	if err := m.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limiter error: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lookupEndpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+m.token)
	req.Header.Set("User-Agent", userAgent())

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute lookup: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ListenBrainz lookup returned status %d", resp.StatusCode)
	}

	var parsed lookupResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("failed to decode lookup response: %w", err)
	}

	// An unmatched lookup comes back as an empty object rather than an error.
	if parsed.RecordingMBID == "" {
		return nil, nil
	}

	result := &musicbrainz.MapperResult{RecordingMBID: parsed.RecordingMBID}
	if md := parsed.Metadata; md != nil && md.Release != nil && md.Release.CAAID != nil {
		result.CAAReleaseMBID = md.Release.CAAReleaseMBID
	}
	return result, nil
}
