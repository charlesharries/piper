package musicbrainz

import (
	"net/http"
	"strings"

	"github.com/teal-fm/piper/models"
)

// track builds an incoming play the way a music service would hand it over.
func track(name, artist, album, isrc string) models.Track {
	t := models.Track{Name: name, Album: album, ISRC: isrc}
	if artist != "" {
		t.Artist = []models.Artist{{Name: artist}}
	}
	return t
}

// bestCandidate returns the top-scoring recording and whether it cleared
// minConfidence. Resolve works from rankCandidates directly; this wraps the same
// two steps so scoring tests can assert on a single winner.
func bestCandidate(in matchInput, recordings []Recording) (candidate, bool) {
	ranked := rankCandidates(in, recordings)
	if len(ranked) == 0 {
		return candidate{}, false
	}
	return ranked[0], ranked[0].score >= minConfidence
}

// queries renders tier queries for failure messages.
func queries(tiers []tier) []string {
	out := make([]string, len(tiers))
	for i, t := range tiers {
		out[i] = t.query
	}
	return out
}

// response builds a bare HTTP response for header-driven logic.
func response(status int, retryAfter string) *http.Response {
	resp := &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       http.NoBody,
	}
	if retryAfter != "" {
		resp.Header.Set("Retry-After", retryAfter)
	}
	return resp
}

// recording builds a MusicBrainz search result.
func recording(title, artist string, lengthMs int, releases ...Release) Recording {
	rec := Recording{
		ID:       "rec-" + strings.ToLower(strings.ReplaceAll(title, " ", "-")),
		Title:    title,
		Length:   lengthMs,
		Score:    100,
		Releases: releases,
	}
	if artist != "" {
		credit := ArtistCredit{Name: artist}
		credit.Artist.ID = "artist-" + strings.ToLower(strings.ReplaceAll(artist, " ", "-"))
		credit.Artist.Name = artist
		rec.ArtistCredit = []ArtistCredit{credit}
	}
	return rec
}

// listedAs gives a release its track listing.
func listedAs(rel Release, trackTitles ...string) Release {
	tracks := make([]Track, 0, len(trackTitles))
	for _, title := range trackTitles {
		tracks = append(tracks, Track{Title: title})
	}
	rel.Media = []Medium{{Tracks: tracks}}
	return rel
}

// release builds an official album release in the given release group.
func release(title, groupTitle, date, country string, secondaryTypes ...string) Release {
	return Release{
		ID:      "rel-" + strings.ToLower(strings.ReplaceAll(title, " ", "-")) + "-" + country + date,
		Title:   title,
		Status:  "Official",
		Date:    date,
		Country: country,
		ReleaseGroup: &ReleaseGroup{
			ID:             "rg-" + strings.ToLower(strings.ReplaceAll(groupTitle, " ", "-")),
			Title:          groupTitle,
			PrimaryType:    "Album",
			SecondaryTypes: secondaryTypes,
		},
	}
}
