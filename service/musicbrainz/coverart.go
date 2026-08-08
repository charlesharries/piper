package musicbrainz

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The Cover Art Archive is keyed on releases, never on recordings, and
// fm.teal.alpha.feed.play carries no release-group field. That leaves
// releaseMbId as the only identifier in a play record a consumer can turn into
// artwork, which makes choosing a pressing a publishing decision rather than
// only a metadata one.
//
// Art is uploaded per pressing and plenty of pressings have none: of the 82
// releases of Rumours, 19 carry no front cover, and picking on metadata grounds
// alone lands on one of those often enough to matter. Preferring a pressing the
// archive actually holds art for is what makes the stored MBID resolve to an
// image for whoever reads the record.

// browseReleasesLimit is the page size for the release browse. Release groups
// with more pressings than this exist, but the first page is a large enough
// pool to find one with artwork.
const browseReleasesLimit = 100

// coverArtTimeout bounds the artwork probe. It is deliberately short: knowing
// whether a pressing has art is a nicety, and a play must not wait on the
// archive to be published.
const coverArtTimeout = 3 * time.Second

// buildCoverArtEndpoint addresses a release's front cover. The unsized variant
// is asked for on purpose -- a specific thumbnail size can be missing for art
// that does exist, which would read as "no cover".
func buildCoverArtEndpoint(releaseMBID string) string {
	return "https://coverartarchive.org/release/" + url.PathEscape(releaseMBID) + "/front"
}

// hasCoverArt reports whether the Cover Art Archive holds a front cover for a
// release.
//
// This exists to keep the release browse off the hot path. The browse is a
// MusicBrainz call, and MusicBrainz allows one request per second across the
// whole process, so paying it for every play is what makes hydration slow. The
// archive is a different host on its own CDN, and answers the only question
// that matters most of the time: does the pressing we already chose have art?
//
// A redirect means yes, 404 means no, and anything else is unknown -- reported
// as false so the caller falls back to the browse rather than trusting a
// network blip.
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

	// The archive answers with a redirect to the image on archive.org. Following
	// it would cost two more round trips to learn a fact the 307 already states,
	// so stop at the first response.
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
	if len(found.artOwners) == 0 || found.artOwners[release.ID] {
		return release
	}

	better, _, reasons := bestRelease(in, found.releases, rec.Title, found.artOwners)
	if better == nil || !found.artOwners[better.ID] {
		return release
	}

	s.logger.Printf("swapped release %s (%s) for %s, which has cover art [%s]",
		release.ID, release.Title, better.ID, strings.Join(reasons, " "))
	return better
}
