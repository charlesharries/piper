package musicbrainz

import (
	"strings"
)

// The other question: given the right recording, which *pressing* should the
// play be attributed to? A second scoring system, deliberately separate from
// score.go's, with its own weights and its own threshold. Recording scoring
// reaches it in one place only, through matchInput.albumScore.

// Release scoring weights. Title agreement dominates: when the service tells us
// which album the user is playing, that is far better evidence than any
// structural heuristic.
const (
	releaseWeightTitle     = 4.0
	releaseWeightOfficial  = 1.0
	releaseWeightAlbumType = 0.75
	releaseWeightCountry   = 0.25
	releaseWeightArt       = 1.0
)

// releaseSecondaryPenalty is applied to compilations, live albums, soundtracks
// and remix albums, and waived when the album name matches, so that someone
// genuinely playing a soundtrack still gets one.
const releaseSecondaryPenalty = 0.5

// releaseIsTrackTitlePenalty discourages picking a single named after the track
// when the play came from an album.
const releaseIsTrackTitlePenalty = 0.3

// releaseVariantPenalty is applied to a pressing carrying an edition qualifier
// the play never mentioned, e.g. "How the West Was Won (JRK remix)".
const releaseVariantPenalty = 0.4

// releaseConfidence is the score above which a release is good enough to stop
// looking. Below it the recording's full release list is worth fetching, since
// the good pressing is often not in the slice a search result carries.
const releaseConfidence = 0.8

// preferredCountries are the broad-market release territories. A release scoped
// to one small market is usually a regional pressing rather than what a
// streaming service is serving.
var preferredCountries = map[string]bool{"XW": true, "US": true, "GB": true, "XE": true}

// scoreRelease rates one release as the album to attribute a play to.
// artOwners, when non-empty, is the set of release MBIDs the Cover Art Archive
// holds a front cover for; see coverart.go.
func scoreRelease(in matchInput, r Release, trackTitle string, artOwners map[string]bool) (float64, signals) {
	var card scorecard

	titleScore := in.compareAlbum(r.Title)
	if r.ReleaseGroup != nil {
		// Release groups carry the name people know an album by; individual
		// releases often differ in punctuation and subtitle.
		titleScore = max(titleScore, in.compareAlbum(r.ReleaseGroup.Title))
	}
	if in.album.full != "" {
		card.add("title", titleScore, releaseWeightTitle)
	}

	official := 0.0
	if r.Status == "" || r.Status == "Official" {
		official = 1
	}
	card.add("official", official, releaseWeightOfficial)

	if r.ReleaseGroup != nil && r.ReleaseGroup.PrimaryType != "" {
		albumType := 0.0
		switch r.ReleaseGroup.PrimaryType {
		case "Album":
			albumType = 1
		case "EP":
			albumType = 0.6
		}
		card.add("type", albumType, releaseWeightAlbumType)
	}

	if r.Country != "" {
		country := 0.0
		if preferredCountries[r.Country] {
			country = 1
		}
		card.add("country", country, releaseWeightCountry)
	}

	if len(artOwners) > 0 {
		art := 0.0
		if artOwners[r.ID] {
			art = 1
		}
		card.add("art", art, releaseWeightArt)
	}

	// A strong title match means the user asked for this album; don't then
	// punish it for being a soundtrack or a live record.
	titleMatched := in.album.full != "" && titleScore >= 0.9
	if r.ReleaseGroup != nil && len(r.ReleaseGroup.SecondaryTypes) > 0 && !titleMatched {
		card.penalise("secondary", releaseSecondaryPenalty)
	}
	if !titleMatched && trackTitle != "" && normalize(r.Title) == normalize(trackTitle) {
		card.penalise("single", releaseIsTrackTitlePenalty)
	}
	// Only penalise an edition when the play named none. Someone playing
	// "Rumours (Super Deluxe)" may legitimately land on either the deluxe
	// pressing or the plain one, but someone playing "Rumours" should never be
	// attributed to a remix or anniversary edition.
	if in.album.qualifier == "" {
		if _, qualifier := splitQualifier(r.Title); isEditionQualifier(qualifier) {
			card.penalise("edition", releaseVariantPenalty)
		}
	}

	return card.score(), card.signals
}

// bestRelease picks the release to attribute a play to, and reports what it
// scored. The score is left unclamped, so two poor pressings still order.
func bestRelease(in matchInput, releases []Release, trackTitle string, artOwners map[string]bool) (*Release, float64) {
	if len(releases) == 0 {
		return nil, 0
	}

	bestIdx := -1
	var bestScore float64

	for i := range releases {
		score, _ := scoreRelease(in, releases[i], trackTitle, artOwners)
		if bestIdx == -1 || score > bestScore || (score == bestScore && earlier(releases[i], releases[bestIdx])) {
			bestIdx, bestScore = i, score
		}
	}

	// Copy: the slice belongs to a cached Recording and must not be handed out
	// for callers to mutate.
	chosen := releases[bestIdx]
	return &chosen, bestScore
}

// earlier breaks scoring ties towards the original issue of an album, falling
// back to MBID so the choice is stable across runs.
//
// Dates are compared a year at a time, because MusicBrainz records them at
// whatever precision it has -- YYYY, YYYY-MM or YYYY-MM-DD -- and comparing
// those as strings sorts "1994" ahead of "1994-06-21". Within a year the more
// precise date wins; letting the vaguer entry take the tie scatters an album's
// tracks across pressings.
func earlier(a, b Release) bool {
	aDated, bDated := len(a.Date) >= 4, len(b.Date) >= 4
	if aDated != bDated {
		return aDated
	}
	if aDated {
		if a.Date[:4] != b.Date[:4] {
			return a.Date[:4] < b.Date[:4]
		}
		if len(a.Date) != len(b.Date) {
			return len(a.Date) > len(b.Date)
		}
		if a.Date != b.Date {
			return a.Date < b.Date
		}
	}
	return a.ID < b.ID
}

// releaseDiscriminant reports the edition qualifier the service supplied that
// MusicBrainz does not carry, e.g. "super deluxe" from "Rumours (Super Deluxe)",
// so the distinction survives resolving to the base release.
func releaseDiscriminant(album string) string {
	_, qualifier := splitQualifier(album)
	if !isEditionQualifier(qualifier) {
		return ""
	}
	return strings.TrimSpace(qualifier)
}

// searchAlbum renders an album name for a lookup, dropping the edition a
// service decorated it with: "Rumours" finds the deluxe pressing too, where
// "Rumours (Super Deluxe)" matches nothing. Only an edition is dropped, not
// every qualifier isEditionQualifier would recognise -- "Blade Runner (Music
// From The Original Soundtrack)" names the album rather than decorating it.
func searchAlbum(album string) string {
	base, qualifier := splitQualifierRaw(album)
	if base == "" || !hasEditionWord(normalize(qualifier)) {
		return album
	}
	return base
}
