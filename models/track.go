package models

import "time"

// Track represents a Spotify track
type Track struct {
	PlayID int64  `json:"playId"`
	Name   string `json:"name"`
	// analogous to "track"
	RecordingMBID *string  `json:"trackMBID,omitempty"`
	Artist        []Artist `json:"artist"`
	Album         string   `json:"album"`
	// analogous to "album"
	ReleaseMBID    *string   `json:"releaseMBID,omitempty"`
	URL            string    `json:"url"`
	Timestamp      time.Time `json:"timestamp"`
	DurationMs     int64     `json:"durationMs"`
	ProgressMs     int64     `json:"progressMs"`
	ServiceBaseUrl string    `json:"serviceBaseUrl"`
	ISRC           string    `json:"isrc"`
	HasStamped     bool      `json:"hasStamped"`
	// SourceID is the upstream service's own identity for this play, used to
	// match a track against a provider's history. Internal to syncing, so it is
	// not part of the JSON API.
	SourceID string `json:"-"`
}

type Artist struct {
	Name string  `json:"name"`
	ID   string  `json:"id"`
	MBID *string `json:"mbid,omitempty"`
}
