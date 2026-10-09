// Command musicbrainz-cli resolves plays against MusicBrainz from the command
// line. Its batch mode scores the matcher against a golden set, which is how
// changes to the matching heuristics are shown to actually help.
//
//	musicbrainz-cli -track Dreams -artist "Fleetwood Mac" -release Rumours -explain
//	musicbrainz-cli -batch cmd/musicbrainz-cli/testdata/golden.jsonl
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/spf13/viper"

	"github.com/teal-fm/piper/config"
	"github.com/teal-fm/piper/models"
	"github.com/teal-fm/piper/service/listenbrainz"
	"github.com/teal-fm/piper/service/musicbrainz"
)

// goldenCase is one row of the evaluation set: some input from the connected
// service, and expected output from our matcher.
type goldenCase struct {
	Name       string `json:"name"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	ISRC       string `json:"isrc,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	Note       string `json:"note,omitempty"`

	ExpectRecordingMBID  string `json:"expect_recording_mbid,omitempty"`
	ExpectRecordingTitle string `json:"expect_recording_title,omitempty"`
	ExpectReleaseTitle   string `json:"expect_release_title,omitempty"`
	ExpectNoMatch        bool   `json:"expect_no_match,omitempty"` // i.e nonsense input
}

func (c goldenCase) track() models.Track {
	track := models.Track{
		Name:       c.Name,
		Album:      c.Album,
		ISRC:       c.ISRC,
		DurationMs: c.DurationMs,
	}
	for _, name := range strings.Split(c.Artist, ";") {
		if name = strings.TrimSpace(name); name != "" {
			track.Artist = append(track.Artist, models.Artist{Name: name})
		}
	}
	return track
}

func main() {
	var (
		trackName = flag.String("track", "", "Track name")
		artist    = flag.String("artist", "", "Artist name (separate collaborators with ';')")
		release   = flag.String("release", "", "Release/Album name")
		isrc      = flag.String("isrc", "", "ISRC code")
		duration  = flag.Int64("duration", 0, "Track duration in milliseconds")
		batch     = flag.String("batch", "", "Path to a JSONL golden set to evaluate")
		explain   = flag.String("explain", "", "Show scored candidates: 'top' (best 5), 'all', or 'miss' (only where the expectation failed)")
	)
	flag.Parse()
	config.Load()

	service := musicbrainz.NewMusicBrainzService(nil, listenBrainzOption()...)

	if *batch != "" {
		if err := runBatch(service, *batch, *explain); err != nil {
			log.Fatalf("Error running batch: %v", err)
		}
		return
	}

	if *trackName == "" && *isrc == "" {
		log.Fatal("Provide -track or -isrc, or -batch to evaluate a golden set")
	}

	one := goldenCase{Name: *trackName, Artist: *artist, Album: *release, ISRC: *isrc, DurationMs: *duration}
	match, err := service.Resolve(context.Background(), one.track())
	if *explain != "" && match != nil {
		printExplanation(match, *explain)
	}
	if err != nil {
		log.Fatalf("Error enriching track: %v", err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "\t")
	if err := enc.Encode(musicbrainz.ApplyMatch(one.track(), match)); err != nil {
		log.Fatalf("Error encoding result: %v", err)
	}
}

// listenBrainzOption enables ListenBrainz when a token is available, so the CLI
// evaluates the same pipeline the service runs.
func listenBrainzOption() []musicbrainz.Option {
	lb := listenbrainz.NewClient(viper.GetString("listenbrainz.token"))
	if lb == nil {
		return nil
	}
	fmt.Fprintln(os.Stderr, "using ListenBrainz")
	return []musicbrainz.Option{musicbrainz.WithListenBrainz(lb)}
}

type tally struct {
	total             int
	recordingCorrect  int
	releaseCorrect    int
	recordingExpected int
	releaseExpected   int
	noMatch           int
	falsePositives    int
	correctRejections int
}

func runBatch(service *musicbrainz.Service, path, explain string) error {
	cases, err := loadGoldenSet(path)
	if err != nil {
		return err
	}

	fmt.Printf("Evaluating %d cases (MusicBrainz allows 1 request/sec, so give it a sec)\n\n", len(cases))

	var t tally
	for _, c := range cases {
		t.total++
		match, err := service.Resolve(context.Background(), c.track())

		switch {
		case errors.Is(err, musicbrainz.ErrNoConfidentMatch):
			if c.ExpectNoMatch {
				t.correctRejections++
				fmt.Printf("PASS  %-55s correctly rejected\n", label(c))
			} else {
				t.noMatch++
				fmt.Printf("MISS  %-55s no confident match\n", label(c))
			}
		case err != nil:
			t.noMatch++
			fmt.Printf("ERR   %-55s %v\n", label(c), err)
		case c.ExpectNoMatch:
			t.falsePositives++
			fmt.Printf("FALSE %-55s matched %q, expected rejection\n", label(c), match.Recording.Title)
		default:
			reportMatch(&t, c, match)
		}

		if shouldExplain(explain, c, match, err) {
			printExplanation(match, explain)
		}
	}

	printSummary(t)
	return nil
}

func reportMatch(t *tally, c goldenCase, match *musicbrainz.Match) {
	var releaseTitle string
	if match.Release != nil {
		releaseTitle = match.Release.Title
	}

	status := "PASS"
	var problems []string

	switch {
	case c.ExpectRecordingMBID != "":
		t.recordingExpected++
		if match.Recording.ID == c.ExpectRecordingMBID {
			t.recordingCorrect++
		} else {
			status = "FAIL"
			problems = append(problems, fmt.Sprintf("recording=%s (%s)", match.Recording.ID, match.Recording.Title))
		}
	case c.ExpectRecordingTitle != "":
		t.recordingExpected++
		if sameTitle(match.Recording.Title, c.ExpectRecordingTitle) {
			t.recordingCorrect++
		} else {
			status = "FAIL"
			problems = append(problems, fmt.Sprintf("recording=%q want %q", match.Recording.Title, c.ExpectRecordingTitle))
		}
	}
	if c.ExpectReleaseTitle != "" {
		t.releaseExpected++
		if sameTitle(releaseTitle, c.ExpectReleaseTitle) {
			t.releaseCorrect++
		} else {
			status = "FAIL"
			problems = append(problems, fmt.Sprintf("release=%q want %q", releaseTitle, c.ExpectReleaseTitle))
		}
	}

	detail := fmt.Sprintf("%.2f via %s", match.Score, match.Source)
	if len(problems) > 0 {
		detail = strings.Join(problems, "  ")
	}
	fmt.Printf("%s  %-55s %s\n", status, label(c), detail)
}

func shouldExplain(explain string, c goldenCase, match *musicbrainz.Match, err error) bool {
	if match == nil {
		return false
	}
	switch explain {
	case "all", "top":
		return true
	case "miss":
		return err != nil ||
			(c.ExpectRecordingMBID != "" && match.Recording.ID != c.ExpectRecordingMBID) ||
			(c.ExpectRecordingTitle != "" && !sameTitle(match.Recording.Title, c.ExpectRecordingTitle))
	default:
		return false
	}
}

func printExplanation(match *musicbrainz.Match, mode string) {
	lines := match.Explain()
	if mode != "all" && len(lines) > 5 {
		lines = lines[:5]
	}
	for _, line := range lines {
		fmt.Println("       " + line)
	}
	fmt.Println()
}

func printSummary(t tally) {
	fmt.Printf("\n%d cases\n", t.total)
	if t.recordingExpected > 0 {
		fmt.Printf("  recording accuracy  %d/%d  (%.0f%%)\n",
			t.recordingCorrect, t.recordingExpected, percent(t.recordingCorrect, t.recordingExpected))
	}
	if t.releaseExpected > 0 {
		fmt.Printf("  release accuracy    %d/%d  (%.0f%%)\n",
			t.releaseCorrect, t.releaseExpected, percent(t.releaseCorrect, t.releaseExpected))
	}
	fmt.Printf("  unmatched           %d\n", t.noMatch)
	fmt.Printf("  correct rejections  %d\n", t.correctRejections)
	fmt.Printf("  false positives     %d\n", t.falsePositives)
}

// sameTitle compares album titles loosely, since a correct answer may be any of
// an album's many pressings and their titles differ in punctuation and casing.
func sameTitle(got, want string) bool {
	clean := func(s string) string {
		s = strings.ReplaceAll(strings.ToLower(s), "&", "and")
		var b strings.Builder
		for _, r := range s {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	return clean(got) == clean(want)
}

func percent(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

func label(c goldenCase) string {
	text := c.Artist + " - " + c.Name
	if c.Note != "" {
		text += " [" + c.Note + "]"
	}
	if len([]rune(text)) > 55 {
		text = string([]rune(text)[:54]) + "…"
	}
	return text
}

func loadGoldenSet(path string) ([]goldenCase, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening golden set: %w", err)
	}
	defer file.Close()

	var cases []goldenCase
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		// Blank lines and # comments keep the file readable.
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var c goldenCase
		if err := json.Unmarshal([]byte(text), &c); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		cases = append(cases, c)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading golden set: %w", err)
	}
	return cases, nil
}
