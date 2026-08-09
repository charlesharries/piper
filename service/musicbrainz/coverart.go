package musicbrainz

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The Cover Art Archive is keyed on releases, so it's important to get the
// releaseMbId right when publishing a new play. Not just that the release
// matches what the user's client has reported, but that the release has some
// cover art to show on your sick AppView.

// browseReleasesLimit is the page size for the release browse. 100 items
// should be more than enough to find a release with artwork.
const browseReleasesLimit = 100

// coverArtTimeout bounds the artwork request.
const coverArtTimeout = 3 * time.Second

// buildCoverArtEndpoint addresses a release's front cover. We query for an
// unsized version deliberately: possible that some sizes just don't exist!
func buildCoverArtEndpoint(releaseMBID string) string {
	return "https://coverartarchive.org/release/" + url.PathEscape(releaseMBID) + "/front"
}

// hasCoverArt reports whether the Cover Art Archive holds a front cover for a
// release.
//
// Most of the time, the releaseMbId that we've chosen has cover art. To save
// ourselves a query to the MusicBrainz API for all releases with cover art, we
// can first check whether the Cover Art Archive API (i.e. not the MusicBrainz
// API, and therefore not bound by the 1 req/s rate limit!) has cover art for
// the current release. Just do a HEAD request so it's quick!
//
// If no cover art is found, well, then we have to go to MusicBrainz.
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
		s.logger.Printf("cover art probe for release %s failed: %v", releaseMBID, err)
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
	// The browse response omits release groups by default, and scoreRelease
	// reads the group's title and type.
	q.Set("inc", "release-groups")
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
// availability. A failure is reported as an empty result rather than an error:
// artwork is a preference, and a play should still be published with the
// release chosen on metadata alone when MusicBrainz is unreachable.
func (s *Service) releaseGroupPressings(ctx context.Context, releaseGroupID string) pressings {
	if releaseGroupID == "" {
		return pressings{}
	}

	if cached, found := s.pressingsCache.get(releaseGroupID); found {
		return cached
	}

	var result browseReleasesResponse
	if err := s.doRequest(ctx, buildBrowseReleasesEndpoint(releaseGroupID), &result); err != nil {
		s.logger.Printf("pressing lookup for release group %s failed: %v", releaseGroupID, err)
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
// Cover Art Archive about that one first and stop there when it does. Only when
// it has none is the release group browsed, which is the expensive half: it is
// a MusicBrainz call, and MusicBrainz permits one request per second across the
// whole process. The browse result is cached per release group.
//
// The incumbent is re-scored under the same signal rather than swapped blindly,
// so a pressing only loses to one that is at least as good an answer for the
// album.
func (s *Service) preferReleaseWithArt(ctx context.Context, in matchInput, rec Recording, release *Release) *Release {
	if release == nil || release.ReleaseGroup == nil {
		return release
	}

	// Fast path: the release we already picked resolves to a cover, so a
	// consumer of the play record can reach artwork and there is nothing to fix.
	if s.hasCoverArt(ctx, release.ID) {
		return release
	}

	found := s.releaseGroupPressings(ctx, release.ReleaseGroup.ID)
	if len(found.releases) == 0 {
		return release
	}
	if found.artOwners[release.ID] {
		return release
	}
	if len(found.artOwners) == 0 {
		s.logger.Printf("no cover art for release %s (%s) or any of the %s in its release group",
			release.ID, release.Title, pressingCount(len(found.releases)))
		return release
	}

	better, _, reasons := bestRelease(in, found.releases, rec.Title, found.artOwners)
	if better == nil || !found.artOwners[better.ID] {
		s.logger.Printf("keeping release %s (%s) without cover art: none of the %s that have some scored better",
			release.ID, release.Title, pressingCount(len(found.artOwners)))
		return release
	}

	s.logger.Printf("swapped release %s (%s) for %s, which has cover art [%s]",
		release.ID, release.Title, better.ID, strings.Join(reasons, " "))
	return better
}

// pressingCount renders a count of pressings for a log line.
func pressingCount(n int) string {
	if n == 1 {
		return "1 pressing"
	}
	return fmt.Sprintf("%d pressings", n)
}
