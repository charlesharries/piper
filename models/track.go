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
	ReleaseMBID *string `json:"releaseMBID,omitempty"`
	// ReleaseDiscriminant preserves the edition the music service reported
	// ("Super Deluxe", "2011 Remaster") when we resolve the play to the base
	// release, which MusicBrainz holds under its plain title.
	ReleaseDiscriminant string    `json:"releaseDiscriminant,omitempty"`
	URL                 string    `json:"url"`
	Timestamp           time.Time `json:"timestamp"`
	DurationMs          int64     `json:"durationMs"`
	ProgressMs          int64     `json:"progressMs"`
	ServiceBaseUrl      string    `json:"serviceBaseUrl"`
	ISRC                string    `json:"isrc"`
	HasStamped          bool      `json:"hasStamped"`
}

type Artist struct {
	Name string  `json:"name"`
	ID   string  `json:"id"`
	MBID *string `json:"mbid,omitempty"`
}
