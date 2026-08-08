package musicbrainz

import (
	"testing"

	"github.com/teal-fm/piper/models"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "lowercases", in: "Blue Monday", want: "blue monday"},
		{name: "strips punctuation", in: "Power, Corruption & Lies", want: "power corruption and lies"},
		{name: "spells out ampersand", in: "Power Corruption and Lies", want: "power corruption and lies"},
		{name: "strips diacritics", in: "Beyoncé", want: "beyonce"},
		{name: "collapses whitespace", in: "  Dreams   ", want: "dreams"},
		{name: "keeps digits", in: "99 Problems", want: "99 problems"},
		{name: "punctuation becomes a break", in: "Mr.Brightside", want: "mr brightside"},
		{name: "empty", in: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalize(tt.in); got != tt.want {
				t.Errorf("normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// The release "Power Corruption and Lies" and the release group
// "Power, Corruption & Lies" are the same album; normalization is what lets a
// title from Spotify match either.
func TestNormalizeMatchesReleaseAndGroupTitles(t *testing.T) {
	if normalize("Power, Corruption & Lies") != normalize("Power Corruption and Lies") {
		t.Errorf("expected release and release group titles to normalize alike")
	}
}

func TestSimilarity(t *testing.T) {
	tests := []struct {
		name    string
		a, b    string
		wantMin float64
		wantMax float64
	}{
		{name: "identical", a: "dreams", b: "dreams", wantMin: 1, wantMax: 1},
		{name: "empty pair", a: "", b: "", wantMin: 0, wantMax: 0},
		{name: "one empty", a: "dreams", b: "", wantMin: 0, wantMax: 0},
		{name: "close", a: "rumours", b: "rumors", wantMin: 0.8, wantMax: 0.99},
		{name: "unrelated", a: "dreams", b: "bohemian rhapsody", wantMin: 0, wantMax: 0.3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := similarity(tt.a, tt.b)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("similarity(%q, %q) = %v, want within [%v, %v]", tt.a, tt.b, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestSplitQualifier(t *testing.T) {
	tests := []struct {
		name          string
		in            string
		wantBase      string
		wantQualifier string
	}{
		{name: "parenthesised", in: "Dreams (outtake)", wantBase: "dreams", wantQualifier: "outtake"},
		{name: "bracketed", in: "Dreams [Live]", wantBase: "dreams", wantQualifier: "live"},
		{name: "dash suffix", in: "Dreams - 2004 Remaster", wantBase: "dreams", wantQualifier: "2004 remaster"},
		{name: "no qualifier", in: "Dreams", wantBase: "dreams", wantQualifier: ""},
		{name: "hyphenated title survives", in: "Jack-in-the-Box", wantBase: "jack in the box", wantQualifier: ""},
		{name: "edition suffix", in: "Rumours (Super Deluxe)", wantBase: "rumours", wantQualifier: "super deluxe"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, qualifier := splitQualifier(tt.in)
			if base != tt.wantBase || qualifier != tt.wantQualifier {
				t.Errorf("splitQualifier(%q) = (%q, %q), want (%q, %q)",
					tt.in, base, qualifier, tt.wantBase, tt.wantQualifier)
			}
		})
	}
}

func TestDurationScore(t *testing.T) {
	tests := []struct {
		name string
		a, b int64
		want float64
	}{
		{name: "exact", a: 257000, b: 257000, want: 1},
		{name: "within two seconds", a: 257000, b: 258500, want: 1},
		{name: "within five seconds", a: 257000, b: 261000, want: 0.8},
		{name: "ten seconds apart", a: 257000, b: 267000, want: 0.4},
		{name: "twenty seconds apart", a: 257000, b: 277000, want: 0.1},
		{name: "a minute apart", a: 257000, b: 317000, want: 0},
		{name: "order does not matter", a: 267000, b: 257000, want: 0.4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := durationScore(tt.a, tt.b); got != tt.want {
				t.Errorf("durationScore(%d, %d) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// An exact ISRC hit identifies the recording outright and must win regardless
// of how badly the other signals read.
func TestScoreRecordingISRCWins(t *testing.T) {
	in := newMatchInput(models.Track{
		Name:   "Something Entirely Different",
		ISRC:   "gbum71029604",
		Artist: []models.Artist{{Name: "Nobody"}},
	})
	rec := recording("Bohemian Rhapsody", "Queen", 355106)
	rec.ISRCs = []string{"GBUM71029604"}

	score, _ := scoreRecording(in, rec)
	if score != 1 {
		t.Errorf("score = %v, want 1 for an exact ISRC match", score)
	}
}

// This is the case that motivated the rewrite. MusicBrainz returns
// "Dreams (outtake)" ahead of the real recording at a higher query score;
// scoring has to reverse that.
func TestScoreRecordingRejectsOuttakeOverRealRecording(t *testing.T) {
	in := newMatchInput(models.Track{
		Name:       "Dreams",
		Artist:     []models.Artist{{Name: "Fleetwood Mac"}},
		Album:      "Rumours (Super Deluxe)",
		DurationMs: 257800,
	})

	rumours := release("Rumours", "Rumours", "1977-02-04", "US")

	outtake := recording("Dreams (outtake)", "Fleetwood Mac", 261546, rumours)
	outtake.Score = 100
	real := recording("Dreams", "Fleetwood Mac", 257800, rumours)
	real.Score = 95

	best, ok := bestCandidate(in, []Recording{outtake, real})
	if !ok {
		t.Fatal("expected a confident match")
	}
	if best.recording.Title != "Dreams" {
		t.Errorf("chose %q, want %q", best.recording.Title, "Dreams")
	}
}

// Five recordings tie at MusicBrainz score 100 with lengths minutes apart;
// duration is the only thing that separates them.
func TestScoreRecordingUsesDurationToBreakTies(t *testing.T) {
	in := newMatchInput(models.Track{
		Name:       "Bohemian Rhapsody",
		Artist:     []models.Artist{{Name: "Queen"}},
		DurationMs: 355106,
	})

	lengths := []int{363106, 323000, 320000, 343000, 355106, 333133}
	recordings := make([]Recording, 0, len(lengths))
	for _, length := range lengths {
		rec := recording("Bohemian Rhapsody", "Queen", length)
		rec.ID = "rec-" + string(rune('a'+len(recordings)))
		recordings = append(recordings, rec)
	}

	best, ok := bestCandidate(in, recordings)
	if !ok {
		t.Fatal("expected a confident match")
	}
	if best.recording.Length != 355106 {
		t.Errorf("chose length %d, want 355106", best.recording.Length)
	}
}

// Recording and release choice are coupled. Several recordings of a song score
// identically on title, artist and duration; the one that matters is the one
// that lives on a good pressing of the requested album, not on a bootleg that
// merely shares the album's name.
func TestScoreRecordingPrefersRecordingOnAGoodRelease(t *testing.T) {
	in := newMatchInput(models.Track{
		Name:       "Whole Lotta Love",
		Artist:     []models.Artist{{Name: "Led Zeppelin"}},
		Album:      "How the West Was Won",
		DurationMs: 1400000,
	})

	bootleg := release("How the West Was Won (JRK remix)", "How the West Was Won (JRK remix)", "2021", "JP", "Compilation", "Live")
	bootleg.Status = "Bootleg"
	onBootleg := recording("Whole Lotta Love", "Led Zeppelin", 1380333, bootleg)
	onBootleg.ID = "on-bootleg"

	official := release("How the West Was Won", "How the West Was Won", "2003-05-27", "US", "Live")
	onOfficial := recording("Whole Lotta Love", "Led Zeppelin", 1387160, official)
	onOfficial.ID = "on-official"

	best, ok := bestCandidate(in, []Recording{onBootleg, onOfficial})
	if !ok {
		t.Fatal("expected a confident match")
	}
	if best.recording.ID != "on-official" {
		t.Errorf("chose %q (%v), want the recording on the official release",
			best.recording.ID, best.reasons)
	}
}

// Without durations (Last.fm supplies none) the remaining signals still have to
// carry a clean match.
func TestScoreRecordingWithoutDuration(t *testing.T) {
	in := newMatchInput(models.Track{
		Name:   "Blue Monday",
		Artist: []models.Artist{{Name: "New Order"}},
		Album:  "Power, Corruption & Lies",
	})
	rec := recording("Blue Monday", "New Order", 0,
		release("Power Corruption and Lies", "Power, Corruption & Lies", "1983-05-02", "GB"))

	score, reasons := scoreRecording(in, rec)
	if score < minConfidence {
		t.Errorf("score = %v (%v), want at least %v", score, reasons, minConfidence)
	}
}

// A remaster suffix from a streaming service must not stop the base recording
// from matching.
func TestScoreRecordingIgnoresRemasterSuffix(t *testing.T) {
	in := newMatchInput(models.Track{
		Name:       "Dreams - 2004 Remaster",
		Artist:     []models.Artist{{Name: "Fleetwood Mac"}},
		Album:      "Rumours",
		DurationMs: 257800,
	})
	rec := recording("Dreams", "Fleetwood Mac", 257800,
		release("Rumours", "Rumours", "1977-02-04", "US"))

	score, reasons := scoreRecording(in, rec)
	if score < minConfidence {
		t.Errorf("score = %v (%v), want at least %v", score, reasons, minConfidence)
	}
}

func TestArtistScoreMatchesCollaborations(t *testing.T) {
	in := newMatchInput(models.Track{
		Name:   "One Kiss",
		Artist: []models.Artist{{Name: "Calvin Harris"}, {Name: "Dua Lipa"}},
	})

	credits := []ArtistCredit{
		{Name: "Calvin Harris", Joinphrase: " & "},
		{Name: "Dua Lipa"},
	}

	if got := in.artistScore(credits); got != 1 {
		t.Errorf("artistScore() = %v, want 1", got)
	}
}

func TestIsVariantQualifier(t *testing.T) {
	tests := []struct {
		qualifier string
		want      bool
	}{
		{qualifier: "outtake", want: true},
		{qualifier: "live", want: true},
		{qualifier: "2004 remaster", want: true},
		{qualifier: "radio edit", want: true},
		{qualifier: "feat kanye west", want: false},
		{qualifier: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.qualifier, func(t *testing.T) {
			if got := isVariantQualifier(tt.qualifier); got != tt.want {
				t.Errorf("isVariantQualifier(%q) = %v, want %v", tt.qualifier, got, tt.want)
			}
		})
	}
}

// MusicBrainz catalogues a two-song track as "A / B", while services report only
// the song the listener knows. Scoring the whole string sent Death Cab's
// "Stability" to The Photo Album instead of the EP it was played from.
func TestTitleScoreMatchesOneSongOfAMedley(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Stability", Album: "The Stability EP"})

	score, variant := in.titleScore("Stability / Coney Island (alternate version)")
	if score < 0.99 {
		t.Errorf("title score = %.2f, want a full match on the first song", score)
	}
	// "(alternate version)" qualifies Coney Island, not Stability.
	if variant {
		t.Error("penalised for a qualifier belonging to the other song in the medley")
	}
}

// A qualifier on the song that actually matched must still count.
func TestTitleScoreKeepsVariantOnTheMatchedSong(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Stability"})
	if _, variant := in.titleScore("Stability (live) / Coney Island"); !variant {
		t.Error("expected the live qualifier on the matched song to be penalised")
	}
}

// A slash that is part of the name must not be read as a medley separator.
func TestTitleScoreLeavesNonMedleySlashesAlone(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Sgt. Pepper's Lonely Hearts Club Band / With a Little Help From My Friends"})
	score, _ := in.titleScore("Sgt. Pepper's Lonely Hearts Club Band / With a Little Help From My Friends")
	if score < 0.99 {
		t.Errorf("whole-title match = %.2f, want the full string to still win", score)
	}
}
