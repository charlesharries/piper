package musicbrainz

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Matching is largely a string problem: the same record is punctuated,
// transliterated and decorated differently by everyone who catalogues it.
// Everything here works on strings alone.

// normalize prepares a string for comparison: it strips diacritics, lowercases,
// spells out '&', and reduces everything else to single-space-separated
// alphanumerics. This is what lets "Power, Corruption & Lies" compare equal to
// "Power Corruption and Lies", and "Beyoncé" to "Beyonce".
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "&", " and ")

	var b strings.Builder
	b.Grow(len(s))
	lastWasSpace := true
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
			// Combining mark left over from decomposition.
			continue
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastWasSpace = false
		case !lastWasSpace:
			b.WriteRune(' ')
			lastWasSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// levenshtein returns the edit distance between two rune slices.
func levenshtein(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}

	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(min(curr[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

// similarity scores two already-normalized strings in [0,1]. An empty string
// agrees with nothing, itself included: absent metadata is not a match.
func similarity(a, b string) float64 {
	if a == b {
		if a == "" {
			return 0
		}
		return 1
	}
	if a == "" || b == "" {
		return 0
	}

	ra, rb := []rune(a), []rune(b)
	longest := max(len(ra), len(rb))
	return 1 - float64(levenshtein(ra, rb))/float64(longest)
}

// bestSimilarity returns the closest agreement between any two of the names
// offered: a play and a candidate each go by several, and agreeing on any one
// of them is agreement.
func bestSimilarity(a, b []string) float64 {
	var best float64
	for _, x := range a {
		for _, y := range b {
			best = max(best, similarity(x, y))
		}
	}
	return best
}

// reading is a title in the forms worth comparing: as written, with any
// trailing qualifier removed, and the qualifier itself. Either side may carry a
// qualifier the other lacks -- a service reports "Rumours (Super Deluxe)" where
// MusicBrainz has "Rumours" -- so every pairing of the forms is tried.
type reading struct {
	full      string
	base      string
	qualifier string
}

func readingOf(title string) reading {
	base, qualifier := splitQualifier(title)
	return reading{full: normalize(title), base: base, qualifier: qualifier}
}

// agrees rates how closely two titles match.
func (r reading) agrees(other reading) float64 {
	return bestSimilarity([]string{r.full, r.base}, []string{other.full, other.base})
}

// splitQualifier separates a title from a trailing qualifier, e.g.
// "Dreams (outtake)" or "Dreams - 2004 Remaster". Both halves are returned
// normalized; an empty qualifier means the title carries none.
func splitQualifier(title string) (base, qualifier string) {
	rawBase, rawQualifier := splitQualifierRaw(title)
	return normalize(rawBase), normalize(rawQualifier)
}

// splitQualifierRaw is splitQualifier without the normalizing, for callers that
// need the title as written -- a search query, which MusicBrainz matches against
// titles as they are catalogued.
func splitQualifierRaw(title string) (base, qualifier string) {
	trimmed := strings.TrimSpace(title)

	// Bracketed suffix: (...), [...], {...}
	for _, pair := range []struct{ open, close string }{{"(", ")"}, {"[", "]"}, {"{", "}"}} {
		if !strings.HasSuffix(trimmed, pair.close) {
			continue
		}
		if idx := strings.LastIndex(trimmed, pair.open); idx > 0 {
			inner := trimmed[idx+len(pair.open) : len(trimmed)-len(pair.close)]
			return strings.TrimSpace(trimmed[:idx]), strings.TrimSpace(inner)
		}
	}

	// Dash suffix: "Title - 2004 Remaster". The dash must be surrounded by
	// spaces, so hyphenated titles survive intact.
	for _, sep := range []string{" - ", " – ", " — "} {
		if idx := strings.LastIndex(trimmed, sep); idx > 0 {
			return strings.TrimSpace(trimmed[:idx]), strings.TrimSpace(trimmed[idx+len(sep):])
		}
	}

	return trimmed, ""
}

// A qualifier means one of two different things, and the difference decides
// what to do about it. A *variant* -- live, outtake, karaoke -- marks a
// different performance, so a play that did not ask for one should not match
// it; its vocabulary is guffParenWords, which MetadataCleaner already relies on
// (clean.go). An *edition* -- deluxe, anniversary, remaster -- marks a
// different issue of the same record, and is dropped before searching,
// penalised when unasked for, and preserved as the releaseDiscriminant.
var editionWords = []string{
	"anniversary", "bonus", "collector", "collectors", "complete", "deluxe",
	"edition", "expanded", "limited", "platinum", "reissue", "remaster",
	"remastered", "special",
}

// isVariantQualifier reports whether a qualifier marks a different performance.
func isVariantQualifier(qualifier string) bool {
	if qualifier == "" {
		return false
	}
	for _, word := range guffParenWords {
		if strings.Contains(qualifier, normalize(word)) {
			return true
		}
	}
	return false
}

// hasEditionWord reports whether a qualifier names an edition of a release.
func hasEditionWord(qualifier string) bool {
	for _, word := range editionWords {
		if strings.Contains(qualifier, word) {
			return true
		}
	}
	return false
}

// isEditionQualifier reports whether a qualifier marks any variant pressing,
// whether by edition or by performance.
func isEditionQualifier(qualifier string) bool {
	if qualifier == "" {
		return false
	}
	return hasEditionWord(qualifier) || isVariantQualifier(qualifier)
}

// truncate shortens a string to n runes for column-aligned CLI output.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
