package listenbrainz

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// stubTransport serves a canned response and records the request it saw.
type stubTransport struct {
	status int
	body   string
	last   *http.Request
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.last = req
	return &http.Response{
		StatusCode: s.status,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     http.Header{},
	}, nil
}

func newTestClient(t *testing.T, status int, body string) (*Client, *stubTransport) {
	t.Helper()
	transport := &stubTransport{status: status, body: body}
	client := NewClient("test-token", WithHTTPClient(&http.Client{Transport: transport}))
	if client == nil {
		t.Fatal("NewClient returned nil for a non-empty token")
	}
	return client, transport
}

// Without a token the endpoint returns 401, so the client stage has to be
// skippable rather than a hard requirement.
func TestNewClientWithoutToken(t *testing.T) {
	for _, token := range []string{"", "   "} {
		if got := NewClient(token); got != nil {
			t.Errorf("NewClient(%q) = %v, want nil", token, got)
		}
	}
}

func TestLookupSuccess(t *testing.T) {
	body := `{
		"recording_mbid": "b1a9c0e9-d987-4042-ae91-78d6a3267d69",
		"release_mbid": "6b47c9a0-b9e1-3df9-a5e8-50a6ce0dbdbd",
		"artist_mbids": ["0383dadf-2a4e-4d10-a46a-e9e041da8eb3"],
		"metadata": {
			"release": {
				"caa_id": 1234567,
				"caa_release_mbid": "caa-release-mbid"
			}
		}
	}`

	client, transport := newTestClient(t, http.StatusOK, body)

	got, err := client.Lookup(context.Background(), "Queen", "Bohemian Rhapsody", "A Night at the Opera")
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if got == nil {
		t.Fatal("Lookup() = nil, want a result")
	}

	// The body carries release_mbid and artist_mbids as ListenBrainz really
	// sends them; piper takes neither, re-deriving both from the recording.
	if got.RecordingMBID != "b1a9c0e9-d987-4042-ae91-78d6a3267d69" {
		t.Errorf("RecordingMBID = %q", got.RecordingMBID)
	}
	if got.CAAReleaseMBID != "caa-release-mbid" {
		t.Errorf("CAAReleaseMBID = %q, want the cover art holder", got.CAAReleaseMBID)
	}

	if auth := transport.last.Header.Get("Authorization"); auth != "Token test-token" {
		t.Errorf("Authorization = %q, want %q", auth, "Token test-token")
	}

	query := transport.last.URL.Query()
	wantParams := map[string]string{
		"artist_name":    "Queen",
		"recording_name": "Bohemian Rhapsody",
		"release_name":   "A Night at the Opera",
		"metadata":       "true",
		"inc":            "release",
	}
	for key, want := range wantParams {
		if got := query.Get(key); got != want {
			t.Errorf("query %s = %q, want %q", key, got, want)
		}
	}
}

// A miss comes back as an empty object, not an error, and must not be treated
// as a failure.
func TestLookupNoMatch(t *testing.T) {
	client, _ := newTestClient(t, http.StatusOK, `{}`)

	got, err := client.Lookup(context.Background(), "Nobody", "Nothing", "")
	if err != nil {
		t.Fatalf("Lookup() error = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("Lookup() = %v, want nil", got)
	}
}

// Cover art ids are optional; their absence must not lose the rest of the match.
func TestLookupWithoutCoverArt(t *testing.T) {
	client, _ := newTestClient(t, http.StatusOK,
		`{"recording_mbid": "rec-mbid", "release_mbid": "rel-mbid"}`)

	got, err := client.Lookup(context.Background(), "Queen", "Bohemian Rhapsody", "")
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if got == nil || got.RecordingMBID != "rec-mbid" {
		t.Fatalf("Lookup() = %v, want the recording mbid", got)
	}
	if got.CAAReleaseMBID != "" {
		t.Errorf("CAAReleaseMBID = %q, want empty", got.CAAReleaseMBID)
	}
}

func TestLookupUnauthorized(t *testing.T) {
	client, _ := newTestClient(t, http.StatusUnauthorized,
		`{"code": 401, "error": "You need to provide an Authorization header."}`)

	if _, err := client.Lookup(context.Background(), "Queen", "Bohemian Rhapsody", ""); err == nil {
		t.Error("Lookup() error = nil, want an error for 401")
	}
}

// Nothing to match on means no request should be made at all.
func TestLookupSkipsIncompleteInput(t *testing.T) {
	tests := []struct {
		name              string
		artist, recording string
	}{
		{name: "no artist", artist: "", recording: "Bohemian Rhapsody"},
		{name: "no recording", artist: "Queen", recording: ""},
		{name: "blank artist", artist: "   ", recording: "Bohemian Rhapsody"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, transport := newTestClient(t, http.StatusOK, `{}`)

			got, err := client.Lookup(context.Background(), tt.artist, tt.recording, "")
			if err != nil || got != nil {
				t.Errorf("Lookup() = (%v, %v), want (nil, nil)", got, err)
			}
			if transport.last != nil {
				t.Error("expected no request to be made")
			}
		})
	}
}

// The release name is optional and must be omitted rather than sent empty.
func TestLookupOmitsEmptyRelease(t *testing.T) {
	client, transport := newTestClient(t, http.StatusOK, `{}`)

	if _, err := client.Lookup(context.Background(), "Queen", "Bohemian Rhapsody", ""); err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}

	if _, ok := transport.last.URL.Query()["release_name"]; ok {
		t.Errorf("release_name should be omitted, got %q", transport.last.URL.RawQuery)
	}
}

// Names with reserved characters have to survive the round trip intact.
func TestLookupEscapesQueryValues(t *testing.T) {
	client, transport := newTestClient(t, http.StatusOK, `{}`)

	const recording = `Say "Yes" & No`
	if _, err := client.Lookup(context.Background(), "AC/DC", recording, ""); err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}

	query, err := url.ParseQuery(transport.last.URL.RawQuery)
	if err != nil {
		t.Fatalf("could not parse query %q: %v", transport.last.URL.RawQuery, err)
	}
	if got := query.Get("artist_name"); got != "AC/DC" {
		t.Errorf("artist_name = %q, want %q", got, "AC/DC")
	}
	if got := query.Get("recording_name"); got != recording {
		t.Errorf("recording_name = %q, want %q", got, recording)
	}
}
