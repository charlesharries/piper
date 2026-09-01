package musicbrainz

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/teal-fm/piper/models"
)

// tier is one rung of the search ladder: a query to try, and a name for the
// log. Resolve works down the ladder and stops at the first tier whose top
// candidate scores well enough, so the tiers run most constrained first and
// scoring is what keeps the looser ones safe.
type tier struct {
	name string
	searchRequest
	// scopeArtist names an artist whose MBID is to be resolved and appended to
	// the query as an arid filter. The lookup costs a request of its own, so it
	// is left until the tier actually runs.
	scopeArtist string
}

// searchTiers returns the searches to try for a play, in order.
//
//	isrc           an ISRC identifies a recording outright
//	clean+album    cleaned metadata, all three fields
//	clean          album drops from filter to scoring signal
//	raw+album      the cleaner is lossy, so retry with what the service sent
//	raw
//	artist-scoped  find the artist by alias, then filter on their MBID
//	dismax         bare words to MusicBrainz's fuzzy parser
//
// When cleaning changed nothing -- the usual case for Latin metadata -- the raw
// tiers repeat the clean ones and are dropped as duplicates, leaving five.
func (s *Service) searchTiers(track models.Track) []tier {
	artist := primaryArtist(track)
	album := searchAlbum(track.Album)
	cleanTrack, _ := s.cleaner.CleanRecording(track.Name)
	cleanArtist, _ := s.cleaner.CleanArtist(artist)
	cleanAlbum, _ := s.cleaner.CleanRecording(album)
	cleanArtists := s.cleanNames(artistNames(track))

	var tiers []tier
	seen := map[string]bool{}
	add := func(name string, req searchRequest, scopeArtist string) {
		key := req.query + "\x00" + scopeArtist
		if req.query == "" || seen[key] {
			return
		}
		seen[key] = true
		tiers = append(tiers, tier{name: name, searchRequest: req, scopeArtist: scopeArtist})
	}
	metadata := func(name string, params SearchParams) {
		if params.Track == "" {
			return
		}
		add(name, searchRequest{query: buildSearchQuery(params), limit: searchLimit}, "")
	}

	// An ISRC needs no other filter, and adding one causes false negatives when
	// MusicBrainz holds the title in a different script or language.
	if track.ISRC != "" {
		add("isrc", searchRequest{query: buildSearchQuery(SearchParams{ISRC: track.ISRC}), limit: searchLimit}, "")
	}

	metadata("clean+album", SearchParams{Track: cleanTrack, Artists: cleanArtists, Release: cleanAlbum})
	metadata("clean", SearchParams{Track: cleanTrack, Artists: cleanArtists})

	// Maybe the cleaner messed up the input -- this covers the situation where
	// the music service's artist is non-Latin but MusicBrainz holds the artist
	// name in Latin characters.
	metadata("raw+album", SearchParams{Track: track.Name, Artists: artistNames(track), Release: album})
	metadata("raw", SearchParams{Track: track.Name, Artists: artistNames(track)})

	// Now the opposite -- the music service gave us Latin characters but the
	// artist is non-Latin in MusicBrainz; doing an artist search allows us to
	// search by artist aliases, including Latin-character aliases
	if scope := cmp.Or(cleanArtist, artist); scope != "" && cleanTrack != "" {
		add("artist-scoped", searchRequest{query: nearPhrase("recording", cleanTrack, titleSlop), limit: searchLimit}, scope)
	}

	// Last resort: hand the bare words to MusicBrainz's fuzzy parser.
	if query := strings.Join(strings.Fields(freeText(track.Name+" "+artist)), " "); query != "" {
		add("dismax", searchRequest{query: query, limit: searchLimit, dismax: true}, "")
	}

	return tiers
}

// scopeTier resolves the artist a tier filters on, reporting false when the
// tier cannot run because nobody was found to scope it to.
func (s *Service) scopeTier(ctx context.Context, t tier) (tier, bool) {
	if t.scopeArtist == "" {
		return t, true
	}

	ev := eventFrom(ctx)
	mbid, err := s.artistMBID(ctx, t.scopeArtist)
	if err != nil {
		ev.artistScope = artistFailed
		ev.noteErr(err)
		return t, false
	}
	if mbid == "" {
		ev.artistScope = artistUnresolved
		return t, false
	}
	ev.artistScope = artistResolved

	t.query = fmt.Sprintf("%s AND arid:%s", t.query, mbid)
	return t, true
}

func buildSearchQuery(params SearchParams) string {
	var queryParts []string
	if params.ISRC != "" {
		queryParts = append(queryParts, phrase("isrc", params.ISRC))
	}
	if params.Track != "" {
		queryParts = append(queryParts, nearPhrase("recording", params.Track, titleSlop))
	}
	if clause := artistClause(params.Artists); clause != "" {
		queryParts = append(queryParts, clause)
	}
	if params.Release != "" {
		queryParts = append(queryParts, phrase("release", params.Release))
	}
	return strings.Join(queryParts, " AND ")
}

// escapeLucene escapes the characters that would otherwise terminate or corrupt
// a quoted Lucene phrase. Track titles containing quotes are common enough
// (Say "Yes", 'Til I Collapse) that leaving them raw produces malformed queries.
var escapeLucene = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace

func phrase(field, value string) string {
	return fmt.Sprintf(`%s:"%s"`, field, escapeLucene(value))
}

// titleSlop is how many positions a title may shift and still match; MusicBrainz
// lets us query with a bitta slop.
const titleSlop = 3

// nearPhrase is phrase with room for the words to move.
func nearPhrase(field, value string, slop int) string {
	return fmt.Sprintf(`%s~%d`, phrase(field, value), slop)
}

// artistClause adds a search clause for each individual artist, plus also all of
// the artists together, comma-separated.
func artistClause(artists []string) string {
	var terms []string
	seen := map[string]bool{}
	for _, name := range append(slices.Clone(artists), strings.Join(artists, ", ")) {
		if name = strings.TrimSpace(name); name == "" || seen[name] {
			continue
		}
		seen[name] = true
		terms = append(terms, phrase("artistname", name), phrase("artist", name))
	}
	if len(terms) == 0 {
		return ""
	}
	return "(" + strings.Join(terms, " OR ") + ")"
}

// luceneSpecial are the characters that carry meaning to the query parser.
const luceneSpecial = `+-&|!(){}[]^"~*?:\/`

// freeText strips query syntax rather than escaping it, for the dismax tier
// where the input is meant to read as plain words.
func freeText(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(luceneSpecial, r) {
			return ' '
		}
		return r
	}, s)
}

// artistNames returns the credited names separately, for the tiers that ask
// after each of them rather than the line they were rendered into.
func artistNames(track models.Track) []string {
	names := make([]string, 0, len(track.Artist))
	for _, a := range track.Artist {
		if name := strings.TrimSpace(a.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// cleanNames puts artist names through the cleaner individually.
func (s *Service) cleanNames(names []string) []string {
	cleaned := make([]string, 0, len(names))
	for _, name := range names {
		if clean, _ := s.cleaner.CleanArtist(name); clean != "" {
			cleaned = append(cleaned, clean)
		}
	}
	return cleaned
}

// primaryArtist renders the incoming artist credit as a single string.
func primaryArtist(track models.Track) string {
	return strings.Join(artistNames(track), ", ")
}
