package musicbrainz

import (
	"fmt"
	"strings"
)

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
// and remix albums. It is waived when the incoming album name matches the
// release, so that a user genuinely playing a soundtrack still gets one.
const releaseSecondaryPenalty = 0.5

// releaseIsTrackTitlePenalty discourages picking a single named after the track
// when the play came from an album.
const releaseIsTrackTitlePenalty = 0.3

// releaseVariantPenalty is applied to a pressing whose title carries an edition
// qualifier the play never mentioned, e.g. attributing a plain
// "How the West Was Won" to "How the West Was Won (JRK remix)".
const releaseVariantPenalty = 0.4

// preferredCountries are the broad-market release territories. A release scoped
// to a single small market is usually a regional pressing rather than the
// edition a streaming service is serving.
var preferredCountries = map[string]bool{"XW": true, "US": true, "GB": true, "XE": true}

// scoreRelease rates one release as the album to attribute a play to.
// artOwners, when non-empty, is the set of release MBIDs already known to have
// cover art (currently supplied by ListenBrainz).
func scoreRelease(in matchInput, r Release, trackTitle string, artOwners map[string]bool) (float64, []string) {
	var weighted, total float64
	reasons := make([]string, 0, 5)

	add := func(name string, score, weight float64) {
		weighted += score * weight
		total += weight
		reasons = append(reasons, fmt.Sprintf("%s=%.2f", name, score))
	}

	titleScore := in.compareAlbum(r.Title)
	if r.ReleaseGroup != nil {
		// Release groups carry the name people know an album by; individual
		// releases often differ ("Power Corruption and Lies" vs the group's
		// "Power, Corruption & Lies").
		if rg := in.compareAlbum(r.ReleaseGroup.Title); rg > titleScore {
			titleScore = rg
		}
	}
	if in.album != "" {
		add("title", titleScore, releaseWeightTitle)
	}

	official := 0.0
	if r.Status == "" || r.Status == "Official" {
		official = 1
	}
	add("official", official, releaseWeightOfficial)

	if r.ReleaseGroup != nil && r.ReleaseGroup.PrimaryType != "" {
		albumType := 0.0
		switch r.ReleaseGroup.PrimaryType {
		case "Album":
			albumType = 1
		case "EP":
			albumType = 0.6
		}
		add("type", albumType, releaseWeightAlbumType)
	}

	if r.Country != "" {
		country := 0.0
		if preferredCountries[r.Country] {
			country = 1
		}
		add("country", country, releaseWeightCountry)
	}

	if len(artOwners) > 0 {
		art := 0.0
		if artOwners[r.ID] {
			art = 1
		}
		add("art", art, releaseWeightArt)
	}

	if total == 0 {
		return 0, reasons
	}
	score := weighted / total

	// A strong title match means the user asked for this album; don't then
	// punish it for being a soundtrack or a live record.
	titleMatched := in.album != "" && titleScore >= 0.9
	if r.ReleaseGroup != nil && len(r.ReleaseGroup.SecondaryTypes) > 0 && !titleMatched {
		score -= releaseSecondaryPenalty
		reasons = append(reasons, fmt.Sprintf("secondary=-%.2f", releaseSecondaryPenalty))
	}
	if !titleMatched && trackTitle != "" && normalize(r.Title) == normalize(trackTitle) {
		score -= releaseIsTrackTitlePenalty
		reasons = append(reasons, fmt.Sprintf("single=-%.2f", releaseIsTrackTitlePenalty))
	}

	// Only penalise when the play named no edition at all. Someone playing
	// "Rumours (Super Deluxe)" may legitimately land on either the deluxe
	// pressing or the plain one, but someone playing "Rumours" should never be
	// attributed to a remix or anniversary edition.
	if in.albumQualifier == "" {
		if _, qualifier := splitQualifier(r.Title); isEditionQualifier(qualifier) {
			score -= releaseVariantPenalty
			reasons = append(reasons, fmt.Sprintf("edition=-%.2f", releaseVariantPenalty))
		}
	}

	return score, reasons
}

// bestRelease picks the release to attribute a play to by scoring an input
// against a set of releases.
func bestRelease(in matchInput, releases []Release, trackTitle string, artOwners map[string]bool) (*Release, float64, []string) {
	if len(releases) == 0 {
		return nil, 0, nil
	}

	bestIdx := -1
	var bestScore float64
	var bestReasons []string

	for i := range releases {
		score, reasons := scoreRelease(in, releases[i], trackTitle, artOwners)
		if bestIdx == -1 || score > bestScore || (score == bestScore && earlier(releases[i], releases[bestIdx])) {
			bestIdx, bestScore, bestReasons = i, score, reasons
		}
	}

	// Copy: the slice belongs to a cached Recording and must not be handed out
	// for callers to mutate.
	chosen := releases[bestIdx]
	return &chosen, bestScore, bestReasons
}

// releaseConfidence is the score above which a release is good enough to stop
// looking. Below it, the recording's full release list is worth fetching: a
// search result carries only a slice of the releases a recording appears on,
// and the good pressing is often not in it.
const releaseConfidence = 0.8

// earlier breaks scoring ties towards the original issue of an album, falling
// back to MBID so the choice is stable across runs.
func earlier(a, b Release) bool {
	aDated, bDated := len(a.Date) >= 4, len(b.Date) >= 4
	if aDated != bDated {
		return aDated
	}
	if a.Date != b.Date {
		return a.Date < b.Date
	}
	return a.ID < b.ID
}

// editionWords mark a variant pressing of an album. They are deliberately
// separate from the recording-variant vocabulary in clean.go: "Deluxe"
// describes an edition of a release, not a different performance of a song.
var editionWords = []string{
	"anniversary", "bonus", "collector", "collectors", "complete", "deluxe",
	"edition", "expanded", "limited", "platinum", "reissue", "special",
}

func isEditionQualifier(qualifier string) bool {
	if qualifier == "" {
		return false
	}
	for _, word := range editionWords {
		if strings.Contains(qualifier, word) {
			return true
		}
	}
	return isVariantQualifier(qualifier)
}

// releaseDiscriminant reports the edition qualifier the service supplied that
// MusicBrainz does not carry, e.g. "super deluxe" from "Rumours (Super Deluxe)".
// It populates the lexicon's releaseDiscriminant field so the distinction
// survives even though we resolve to the base release.
func releaseDiscriminant(album string) string {
	_, qualifier := splitQualifier(album)
	if !isEditionQualifier(qualifier) {
		return ""
	}
	return strings.TrimSpace(qualifier)
}
