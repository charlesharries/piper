// Package listenbrainz talks to ListenBrainz's metadata lookup endpoint, which
// does a better job matching loose inputs to MusicBrainz IDs than plain ol'
// search does.
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

// userAgent identifies piper to ListenBrainz.
func userAgent() string {
	return models.SubmissionAgent() + " ( https://github.com/teal-fm/piper )"
}

// Client calls the ListenBrainz metadata lookup endpoint.
type Client struct {
	token      string
	httpClient *http.Client
	limiter    *rate.Limiter
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client, so tests can serve canned responses.
func WithHTTPClient(c *http.Client) Option {
	return func(lb *Client) { lb.httpClient = c }
}

// NewClient builds a client for the given user token. Since we need a token, if
// one isn't set then just fall back to nil.
func NewClient(token string, opts ...Option) *Client {
	if strings.TrimSpace(token) == "" {
		return nil
	}

	lb := &Client{
		token:      strings.TrimSpace(token),
		httpClient: &http.Client{Timeout: 10 * time.Second},
		limiter:    rate.NewLimiter(rate.Every(200*time.Millisecond), 4),
	}
	for _, opt := range opts {
		opt(lb)
	}
	return lb
}

// lookupResponse is the subset of ListenBrainz's reply that piper uses.
type lookupResponse struct {
	RecordingMBID string `json:"recording_mbid"`
	Metadata      *struct {
		Release *struct {
			CAAID          *int64 `json:"caa_id"`
			CAAReleaseMBID string `json:"caa_release_mbid"`
		} `json:"release"`
	} `json:"metadata"`
}

// Lookup resolves a play to MusicBrainz identifiers
func (lb *Client) Lookup(ctx context.Context, artist, recording, release string) (*musicbrainz.ListenBrainzResult, error) {
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

	if err := lb.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limiter error: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lookupEndpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+lb.token)
	req.Header.Set("User-Agent", userAgent())

	resp, err := lb.httpClient.Do(req)
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

	// An unmatched lookup is a valid result, so no error here
	if parsed.RecordingMBID == "" {
		return nil, nil
	}

	result := &musicbrainz.ListenBrainzResult{RecordingMBID: parsed.RecordingMBID}
	if md := parsed.Metadata; md != nil && md.Release != nil && md.Release.CAAID != nil {
		result.CAAReleaseMBID = md.Release.CAAReleaseMBID
	}
	return result, nil
}
