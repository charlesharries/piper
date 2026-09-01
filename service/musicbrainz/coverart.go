package musicbrainz

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// The Cover Art Archive is keyed on releases, so it's important to get the
// releaseMbId right when publishing a new play. Not just that the release
// matches what the user's client has reported, but that the release has some
// cover art to show on your sick AppView.

// browseReleasesLimit is the page size for the release browse.
const browseReleasesLimit = 100

// coverArtTimeout bounds the artwork request.
const coverArtTimeout = 3 * time.Second

// buildCoverArtEndpoint addresses a release's front cover.
func buildCoverArtEndpoint(releaseMBID string) string {
	return "https://coverartarchive.org/release/" + url.PathEscape(releaseMBID) + "/front"
}

// hasCoverArt reports whether the Cover Art Archive holds a front cover for a
// release. We don't actually care about retrieving the cover art here -- just
// checking whether it exists.
func (s *Service) hasCoverArt(ctx context.Context, releaseMBID string) bool {
	if releaseMBID == "" {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, coverArtTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, buildCoverArtEndpoint(releaseMBID), nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", userAgent())

	// Don't bother following redirects -- if we're being redirected, it's
	// because cover art exists, which is what we want to know!
	client := *s.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := client.Do(req)
	if err != nil {
		eventFrom(ctx).noteErr(err)
		return false
	}
	resp.Body.Close()

	return resp.StatusCode >= 300 && resp.StatusCode < 400
}

// buildBrowseReleasesEndpoint lists a release group's pressings. Unlike the
// recording lookup, this endpoint reports each release's cover-art-archive
// flags, so one request answers both which pressings exist and which have art.
func buildBrowseReleasesEndpoint(releaseGroupID string) string {
	q := url.Values{}
	q.Set("release-group", releaseGroupID)
	q.Set("fmt", "json")
	q.Set("limit", strconv.Itoa(browseReleasesLimit))
	// The browse response omits both by default, and scoreRelease reads the
	// group's title and type and the media's format. Neither costs a request.
	q.Set("inc", "release-groups+media")
	return "https://musicbrainz.org/ws/2/release?" + q.Encode()
}

// browseReleasesResponse is the subset of the browse reply that piper uses.
type browseReleasesResponse struct {
	Releases []struct {
		Release
		CoverArtArchive struct {
			Front bool `json:"front"`
		} `json:"cover-art-archive"`
	} `json:"releases"`
}

// pressings is a release group's releases together with the subset of them the
// Cover Art Archive holds a front cover for.
type pressings struct {
	releases  []Release
	artOwners map[string]bool
}

// releaseGroupPressings fetches a release group's pressings and their artwork
// availability. A failure is reported as an empty result rather than an error.
func (s *Service) releaseGroupPressings(ctx context.Context, releaseGroupID string) pressings {
	if releaseGroupID == "" {
		return pressings{}
	}

	if cached, found := s.pressingsCache.get(releaseGroupID); found {
		return cached
	}

	var result browseReleasesResponse
	if err := s.doRequest(ctx, buildBrowseReleasesEndpoint(releaseGroupID), &result); err != nil {
		eventFrom(ctx).noteErr(err)
		return pressings{}
	}

	found := pressings{
		releases:  make([]Release, 0, len(result.Releases)),
		artOwners: make(map[string]bool, len(result.Releases)),
	}
	for _, r := range result.Releases {
		found.releases = append(found.releases, r.Release)
		if r.CoverArtArchive.Front {
			found.artOwners[r.ID] = true
		}
	}

	s.pressingsCache.put(releaseGroupID, found, s.cacheTTL)
	return found
}

// preferReleaseWithArt re-picks a play's release across its whole release group
// with artwork availability added as a scoring signal.
//
// The pressing chosen on metadata grounds usually already has art, so ask the
// Cover Art Archive about that one first and stop there when it does; only when
// it has none is the release group browsed, at the cost of a rate-limited
// MusicBrainz request. The incumbent is re-scored under the new signal rather
// than swapped blindly, so a pressing only loses to one that is at least as
// good an answer for the album.
func (s *Service) preferReleaseWithArt(ctx context.Context, in matchInput, rec Recording, release *Release) *Release {
	if release == nil || release.ReleaseGroup == nil {
		return release
	}

	ev := eventFrom(ctx)

	// Fast path: the release we already picked resolves to a cover, so a
	// consumer of the play record can reach artwork and there is nothing to fix.
	if s.hasCoverArt(ctx, release.ID) {
		ev.artOutcome = artHadArt
		return release
	}

	found := s.releaseGroupPressings(ctx, release.ReleaseGroup.ID)
	if len(found.releases) == 0 {
		return release
	}
	if found.artOwners[release.ID] {
		ev.artOutcome = artHadArt
		return release
	}
	if len(found.artOwners) == 0 {
		ev.artOutcome = artNoneInGroup
		return release
	}

	better, _ := bestRelease(in, found.releases, rec.Title, found.artOwners)
	if better == nil || !found.artOwners[better.ID] {
		ev.artOutcome = artKept
		return release
	}

	ev.artOutcome, ev.artFrom = artSwapped, release.ID
	return better
}
