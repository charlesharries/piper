package musicbrainz

import (
	"testing"

	"github.com/teal-fm/piper/models"
)

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

// The Blade Runner case. MusicBrainz holds bootleg pressings of the soundtrack
// whose tracks carry exactly the official titles but run minutes longer, so
// title, artist and album all read perfectly and only the length dissents. A
// weighted mean cannot reject on one signal out of four, which is how plays of
// the 8:54 "Blade Runner Blues" were published as the 10:19 bootleg cut.
func TestScoreRecordingRejectsContradictoryDuration(t *testing.T) {
	in := newMatchInput(models.Track{
		Name:       "Blade Runner Blues",
		Artist:     []models.Artist{{Name: "Vangelis"}},
		Album:      "Blade Runner (Music From The Original Soundtrack)",
		DurationMs: 534400,
	})

	bootleg := release("Blade Runner", "Blade Runner", "1993-12", "GB", "Soundtrack")
	bootleg.Status = "Bootleg"
	rec := recording("Blade Runner Blues", "Vangelis", 619133, bootleg)

	score, reasons := scoreRecording(in, rec)
	if score >= minConfidence {
		t.Errorf("score = %.3f (%v), want below %v for a recording 85s adrift", score, reasons, minConfidence)
	}
}

// The penalty is for contradiction, not for imprecision. Services and
// MusicBrainz routinely disagree by a few seconds on the same recording, and
// durationScore already grades that.
func TestScoreRecordingAcceptsNearbyDuration(t *testing.T) {
	in := newMatchInput(models.Track{
		Name:       "Blade Runner Blues",
		Artist:     []models.Artist{{Name: "Vangelis"}},
		Album:      "Blade Runner",
		DurationMs: 534400,
	})
	rec := recording("Blade Runner Blues", "Vangelis", 549000, // ~15s out
		release("Blade Runner", "Blade Runner", "1994-06-21", "XE", "Soundtrack"))

	score, reasons := scoreRecording(in, rec)
	if score < minConfidence {
		t.Errorf("score = %.3f (%v), want at least %v", score, reasons, minConfidence)
	}
}

// A length is only evidence when both sides have one. MusicBrainz omits lengths
// on plenty of recordings, and penalising their absence would reject them all.
func TestScoreRecordingIgnoresMissingLength(t *testing.T) {
	in := newMatchInput(models.Track{
		Name:       "Dreams",
		Artist:     []models.Artist{{Name: "Fleetwood Mac"}},
		Album:      "Rumours",
		DurationMs: 257800,
	})
	rec := recording("Dreams", "Fleetwood Mac", 0,
		release("Rumours", "Rumours", "1977-02-04", "US"))

	score, reasons := scoreRecording(in, rec)
	if score < minConfidence {
		t.Errorf("score = %.3f (%v), want a missing length to cost nothing", score, reasons)
	}
}

// Title and artist agreeing is not a match. Every recording of a song shares
// both, and because the score is a weighted mean over the signals it has, the
// pair alone would otherwise score a perfect 1.00.
func TestScoreRecordingRejectsTitleAndArtistAlone(t *testing.T) {
	in := newMatchInput(models.Track{
		Name:   "Whole Lotta Love",
		Artist: []models.Artist{{Name: "Led Zeppelin"}},
	})
	rec := recording("Whole Lotta Love", "Led Zeppelin", 0)
	rec.Score = 0

	score, reasons := scoreRecording(in, rec)
	if score >= minConfidence {
		t.Errorf("score = %.3f (%v), want title and artist alone to fall short of %v",
			score, reasons, minConfidence)
	}
}

// One corroborating signal is enough. Last.fm supplies no duration, so the album
// has to be able to carry a match on its own.
func TestScoreRecordingAcceptsAlbumAsCorroboration(t *testing.T) {
	in := newMatchInput(models.Track{
		Name:   "Go Your Own Way",
		Artist: []models.Artist{{Name: "Fleetwood Mac"}},
		Album:  "Rumours",
	})
	rec := recording("Go Your Own Way", "Fleetwood Mac", 0,
		release("Rumours", "Rumours", "1977-02-04", "US"))

	score, reasons := scoreRecording(in, rec)
	if score < minConfidence {
		t.Errorf("score = %.3f (%v), want the album to corroborate on its own", score, reasons)
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

// Some artists have non-Latin name for both artist & artistname,
// and sort name (i.e. "Harris, Calvin") is the only Latin name
// on their record.
func TestArtistScoreUsesSortNameForNonLatinCredits(t *testing.T) {
	tests := []struct {
		name       string
		artist     string
		creditName string
		artistName string
		sortName   string
	}{
		{"mononym sorts as itself", "MEITEI", "Meitei / 冥丁", "冥丁", "Meitei"},
		{"personal name sorts inverted", "Kenshi Yonezu", "米津玄師", "米津玄師", "Yonezu, Kenshi"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := newMatchInput(models.Track{
				Name:   "Kintsugi",
				Artist: []models.Artist{{Name: tt.artist}},
			})

			credit := ArtistCredit{Name: tt.creditName}
			credit.Artist.Name = tt.artistName
			credit.Artist.SortName = tt.sortName

			if got := in.artistScore([]ArtistCredit{credit}); got != 1 {
				t.Errorf("artistScore() = %v, want 1", got)
			}
		})
	}
}

// Nor is the sort name always Latin: 亜蘭知子 sorts as 亜蘭知子, and only an alias
// records the name a music service credits her by.
func TestArtistScoreUsesAliasesForNonLatinCredits(t *testing.T) {
	in := newMatchInput(models.Track{
		Name:   "I'm in Love",
		Artist: []models.Artist{{Name: "Tomoko Aran"}},
	})

	credit := ArtistCredit{Name: "亜蘭知子"}
	credit.Artist.Name = "亜蘭知子"
	credit.Artist.SortName = "亜蘭知子"
	credit.Artist.Aliases = []Alias{{Name: "あらんともこ"}, {Name: "Tomoko Aran"}}

	if got := in.artistScore([]ArtistCredit{credit}); got != 1 {
		t.Errorf("artistScore() = %v, want 1", got)
	}
}

// An artist MBID scopes a whole search to one catalogue, so the name it was
// resolved from has to be the artist's, not merely close to it.
func TestArtistGoesBy(t *testing.T) {
	ishibashi := Artist{
		Name:     "石橋英子",
		SortName: "Ishibashi, Eiko",
		Aliases:  []Alias{{Name: "Eiko Ishibashi"}, {Name: "Ishibashi Eiko"}},
	}
	trio := Artist{Name: "Eiko Ishibashi Trio", SortName: "Ishibashi, Eiko, Trio"}

	if got := ishibashi.goesBy("Eiko Ishibashi"); got != 1 {
		t.Errorf("goesBy() = %v for the artist herself, want 1", got)
	}
	if got := trio.goesBy("Eiko Ishibashi"); got >= artistNameAgreement {
		t.Errorf("goesBy() = %v for her trio, want below %v", got, artistNameAgreement)
	}
}

// Scoring the recording title alone sent Death Cab's "Stability" to The Photo
// Album instead of the EP it was played from, which lists it under that name.
func TestTitleScoreMatchesTheNameItsReleaseListsItUnder(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Stability", Album: "The Stability EP"})

	rec := recording("Stability / Coney Island (alternate version)", "Death Cab for Cutie", 741600,
		listedAs(release("The Stability EP", "The Stability E.P.", "2002-02-19", "XW"), "Stability"))

	score, variant := in.titleScore(rec)
	if score < 0.99 {
		t.Errorf("title score = %.2f, want a full match on the name the EP lists it under", score)
	}
	// "(alternate version)" qualifies Coney Island, not Stability.
	if variant {
		t.Error("penalised for a qualifier belonging to the other song on the track")
	}
}

// A qualifier on the name that matched must still count.
func TestTitleScoreKeepsVariantOnTheMatchedName(t *testing.T) {
	in := newMatchInput(models.Track{Name: "Stability"})

	rec := recording("Stability / Coney Island", "Death Cab for Cutie", 741600,
		listedAs(release("A Live One", "A Live One", "2003-01-01", "US"), "Stability (live)"))

	if _, variant := in.titleScore(rec); !variant {
		t.Error("expected the live qualifier on the matched name to be penalised")
	}
}

// A DJ mix is one track listed under its whole tracklist, so there is no
// per-song name to match -- and a mix credits every artist it plays.
func TestTitleScoreDoesNotMatchASongInsideAMix(t *testing.T) {
	in := newMatchInput(models.Track{Name: "I'm in Love", Album: "浮遊空間"})

	tracklist := "Side A (intro) / Sun Trails / Tasogare / Seven / Slowyazi / You / " +
		"Plastic Love / Fools / Hit n Run / Dreams / Cruise / I’m in Love / I’m Not in Love"
	rec := recording(tracklist, "Various Artists", 0,
		listedAs(release("Modern Yacht Rock", "Modern Yacht Rock", "2019-01-01", "US"), tracklist))

	score, _ := in.titleScore(rec)
	if score > 0.5 {
		t.Errorf("title score = %.2f, want a mix to be scored whole rather than per song", score)
	}
}

// A slash that is part of the name must still match in full.
func TestTitleScoreLeavesSlashesInRealNamesAlone(t *testing.T) {
	const title = "Sgt. Pepper's Lonely Hearts Club Band / With a Little Help From My Friends"
	in := newMatchInput(models.Track{Name: title})

	rec := recording(title, "The Beatles", 0,
		listedAs(release("Sgt. Pepper's", "Sgt. Pepper's", "1967-06-01", "GB"), title))

	score, _ := in.titleScore(rec)
	if score < 0.99 {
		t.Errorf("whole-title match = %.2f, want the full string to still win", score)
	}
}

// The same length disagreement still has to do its job where the album doesn't match.
func TestScoreRecordingStillSeparatesLiveTake(t *testing.T) {
	in := newMatchInput(models.Track{
		Name:       "Whole Lotta Love",
		Album:      "Led Zeppelin II",
		DurationMs: 333000,
		Artist:     []models.Artist{{Name: "Led Zeppelin"}},
	})
	studio, _ := scoreRecording(in, recording("Whole Lotta Love", "Led Zeppelin", 334000,
		release("Led Zeppelin II", "Led Zeppelin II", "1969", "US")))
	live, _ := scoreRecording(in, recording("Whole Lotta Love", "Led Zeppelin", 1400000,
		release("How the West Was Won", "How the West Was Won", "2003", "US")))

	if live >= studio {
		t.Errorf("live take scored %.3f, must lose to the studio recording at %.3f", live, studio)
	}
	if live >= minConfidence {
		t.Errorf("live take scored %.3f, must not be publishable for a studio play", live)
	}
}

// The conflict penalty grades with the size of the disagreement rather than
// switching on at the tolerance.
func TestDurationConflictAmountGrades(t *testing.T) {
	justOver := durationConflictAmount(durationTolerance + 1)
	wayOver := durationConflictAmount(durationTolerance + durationConflictRamp)

	if justOver <= 0 || justOver >= 0.01 {
		t.Errorf("a millisecond past tolerance = %v, want a hair above zero", justOver)
	}
	if wayOver != durationConflictPenalty {
		t.Errorf("a full ramp past tolerance = %v, want %v", wayOver, durationConflictPenalty)
	}
	if amount := durationConflictAmount(durationTolerance); amount != 0 {
		t.Errorf("a gap inside tolerance = %v, want no penalty at all", amount)
	}
}

// Title is the track's identity, so use it as a final multiplier for confidence.
func TestScoreRecordingRequiresTitleAgreement(t *testing.T) {
	in := newMatchInput(models.Track{
		Name:       "Longhope",
		Album:      "Longhope",
		DurationMs: 211000,
		Artist:     []models.Artist{{Name: "Hinako Omori"}},
	})
	rel := release("Auraelia", "Auraelia", "2025", "GB")

	right, _ := scoreRecording(in, recording("Longhope", "Hinako Omori", 211000, rel))
	if right < minConfidence {
		t.Errorf("the right title scored %.3f, want at least %v", right, minConfidence)
	}

	for _, title := range []string{"Memory Grooves", "Heartplant"} {
		got, reasons := scoreRecording(in, recording(title, "Hinako Omori", 211000, rel))
		if got >= minConfidence {
			t.Errorf("%q scored %.3f on artist and length alone, want below %v [%s]",
				title, got, minConfidence, reasons)
		}
	}
}

// Some titles legitimately disagree, e.g. in classical, so the
// title confidence floor has to sit below them.
func TestTitleConfidenceSparesClassicalTitles(t *testing.T) {
	if got := titleConfidence(0.51); got != 1 {
		t.Errorf("titleConfidence(0.51) = %v, want 1: the golden set's lowest correct title", got)
	}
	if got := titleConfidence(0); got != 0 {
		t.Errorf("titleConfidence(0) = %v, want 0", got)
	}
}
