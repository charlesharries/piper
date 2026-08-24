package musicbrainz

import "testing"

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
