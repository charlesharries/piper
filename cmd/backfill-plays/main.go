// Command backfill-plays re-submits plays that piper saved to its local
// database but failed to write to the user's PDS (for example while OAuth
// token refreshes were failing).
//
// It runs as a dry run by default: it lists the tracks it would submit and
// skips any that already exist as fm.teal.feed.play records on the PDS.
// Pass -apply to actually create the records.
//
// It must run against the live piper database (same DB_PATH and ATProto
// client config as the server), because creating records refreshes the
// OAuth session and the rotated refresh token is saved back to that DB.
// Stop piper while running with -apply so the two don't race on a refresh.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/client"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/spf13/viper"

	"github.com/teal-fm/piper/config"
	"github.com/teal-fm/piper/db"
	"github.com/teal-fm/piper/models"
	atprotoauth "github.com/teal-fm/piper/oauth/atproto"
	atprotoservice "github.com/teal-fm/piper/service/atproto"
	"github.com/teal-fm/piper/session"
)

const playCollection = "fm.teal.feed.play"

type user struct {
	ID        int64
	DID       string
	SessionID string
}

type candidate struct {
	user  user
	track *models.Track
}

func main() {
	var (
		sinceStr  = flag.String("since", "", "only backfill plays at or after this time (RFC3339, required), e.g. 2026-11-11T22:40:00Z")
		untilStr  = flag.String("until", "", "only backfill plays before this time (RFC3339, default now)")
		sourceStr = flag.String("source", string(db.SourceAppleMusic), "track source to backfill: applemusic, spotify, lastfm or listenbrainz")
		didFlag   = flag.String("did", "", "only backfill this user's DID (default: every user with an ATProto session)")
		apply     = flag.Bool("apply", false, "actually create records on the PDS (default is a dry run)")
		skipCheck = flag.Bool("skip-pds-check", false, "don't check the PDS for existing records before submitting")
		delay     = flag.Duration("delay", 500*time.Millisecond, "pause between record writes")
	)
	flag.Parse()

	if *sinceStr == "" {
		flag.Usage()
		log.Fatal("-since is required")
	}
	since, err := time.Parse(time.RFC3339, *sinceStr)
	if err != nil {
		log.Fatalf("invalid -since: %v", err)
	}
	until := time.Now()
	if *untilStr != "" {
		if until, err = time.Parse(time.RFC3339, *untilStr); err != nil {
			log.Fatalf("invalid -until: %v", err)
		}
	}
	if !until.After(since) {
		log.Fatal("-until must be after -since")
	}
	source := db.TrackSource(*sourceStr)
	if !source.IsValid() {
		log.Fatalf("invalid -source %q", *sourceStr)
	}

	config.Load()

	database, err := db.New(viper.GetString("db.path"))
	if err != nil {
		log.Fatalf("opening database: %v", err)
	}
	defer database.Close()

	ctx := context.Background()

	users, err := loadUsers(database.DB, *didFlag)
	if err != nil {
		log.Fatalf("loading users: %v", err)
	}
	if len(users) == 0 {
		log.Fatal("no matching users with an ATProto DID and session")
	}

	var authService *atprotoauth.AuthService
	if *apply {
		authService, err = newAuthService(database)
		if err != nil {
			log.Fatalf("creating ATProto auth service: %v", err)
		}
	}

	dir := identity.DefaultDirectory()
	var totalCandidates, totalExisting, totalSubmitted, totalFailed int

	for _, u := range users {
		tracks, err := loadTracks(database.DB, u.ID, source, since, until)
		if err != nil {
			log.Fatalf("loading tracks for user %d: %v", u.ID, err)
		}
		fmt.Printf("\nUser %d (%s): %d %s plays between %s and %s\n",
			u.ID, u.DID, len(tracks), source, since.Format(time.RFC3339), until.Format(time.RFC3339))
		if len(tracks) == 0 {
			continue
		}

		existing := map[string]bool{}
		if !*skipCheck {
			existing, err = existingPlays(ctx, dir, u.DID, since)
			if err != nil {
				log.Fatalf("checking existing records for %s (use -skip-pds-check to bypass): %v", u.DID, err)
			}
		}

		var todo []candidate
		for _, t := range tracks {
			if existing[playKey(t.Name, t.Timestamp)] {
				totalExisting++
				fmt.Printf("  skip (already on PDS)  %s  %s - %s\n", t.Timestamp.UTC().Format(time.RFC3339), artistName(t), t.Name)
				continue
			}
			todo = append(todo, candidate{user: u, track: t})
		}
		totalCandidates += len(todo)

		for _, c := range todo {
			line := fmt.Sprintf("%s  %s - %s", c.track.Timestamp.UTC().Format(time.RFC3339), artistName(c.track), c.track.Name)
			if !*apply {
				fmt.Printf("  would submit           %s\n", line)
				continue
			}
			if err := atprotoservice.SubmitPlayToPDS(ctx, u.DID, u.SessionID, c.track, authService); err != nil {
				totalFailed++
				fmt.Printf("  FAILED                 %s: %v\n", line, err)
				if strings.Contains(err.Error(), "refresh") {
					log.Fatalf("stopping: OAuth refresh is failing, so every further write would fail too")
				}
				continue
			}
			totalSubmitted++
			fmt.Printf("  submitted              %s\n", line)
			time.Sleep(*delay)
		}
	}

	fmt.Println()
	if *apply {
		fmt.Printf("Done: %d submitted, %d failed, %d already on PDS.\n", totalSubmitted, totalFailed, totalExisting)
		if totalFailed > 0 {
			os.Exit(1)
		}
	} else {
		fmt.Printf("Dry run: %d plays would be submitted, %d already on PDS. Re-run with -apply to write them.\n", totalCandidates, totalExisting)
	}
}

func newAuthService(database *db.DB) (*atprotoauth.AuthService, error) {
	key := viper.GetString("atproto.client_secret_key")
	keyID := viper.GetString("atproto.client_secret_key_id")
	if key == "" || keyID == "" {
		return nil, fmt.Errorf("ATPROTO_CLIENT_SECRET_KEY and ATPROTO_CLIENT_SECRET_KEY_ID must be set")
	}
	return atprotoauth.NewATprotoAuthService(
		database,
		session.NewSessionManager(database),
		key,
		viper.GetString("atproto.client_id"),
		viper.GetString("atproto.callback_url"),
		keyID,
		viper.GetStringSlice("allowed_dids"),
	)
}

func loadUsers(conn *sql.DB, did string) ([]user, error) {
	query := `SELECT id, atproto_did, most_recent_at_session_id FROM users
	          WHERE atproto_did IS NOT NULL AND atproto_did != ''
	            AND most_recent_at_session_id IS NOT NULL AND most_recent_at_session_id != ''`
	args := []any{}
	if did != "" {
		query += ` AND atproto_did = ?`
		args = append(args, did)
	}
	rows, err := conn.Query(query+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []user
	for rows.Next() {
		var u user
		if err := rows.Scan(&u.ID, &u.DID, &u.SessionID); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// loadTracks returns the user's tracks for a source within [since, until),
// oldest first. The time filter runs in Go rather than SQL because stored
// timestamps may carry different zone offsets, which breaks string comparison.
func loadTracks(conn *sql.DB, userID int64, source db.TrackSource, since, until time.Time) ([]*models.Track, error) {
	rows, err := conn.Query(`
	SELECT id, name, recording_mbid, artist, album, release_mbid, url, timestamp,
	       duration_ms, progress_ms, service_base_url, isrc, has_stamped
	FROM tracks WHERE user_id = ? AND source = ?`, userID, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tracks []*models.Track
	for rows.Next() {
		var (
			t            models.Track
			artistString string
		)
		if err := rows.Scan(&t.PlayID, &t.Name, &t.RecordingMBID, &artistString, &t.Album, &t.ReleaseMBID,
			&t.URL, &t.Timestamp, &t.DurationMs, &t.ProgressMs, &t.ServiceBaseUrl, &t.ISRC, &t.HasStamped); err != nil {
			return nil, err
		}
		if t.Timestamp.Before(since) || !t.Timestamp.Before(until) {
			continue
		}
		if err := json.Unmarshal([]byte(artistString), &t.Artist); err != nil {
			t.Artist = []models.Artist{{Name: artistString}}
		}
		if len(t.Artist) == 0 || strings.TrimSpace(t.Name) == "" {
			log.Printf("skipping track %d: missing name or artist", t.PlayID)
			continue
		}
		tracks = append(tracks, &t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(tracks, func(i, j int) bool { return tracks[i].Timestamp.Before(tracks[j].Timestamp) })
	return tracks, nil
}

// existingPlays lists the user's fm.teal.feed.play records from their PDS
// (newest first, unauthenticated) back to `since`, keyed by playKey.
func existingPlays(ctx context.Context, dir identity.Directory, did string, since time.Time) (map[string]bool, error) {
	parsed, err := syntax.ParseDID(did)
	if err != nil {
		return nil, err
	}
	ident, err := dir.LookupDID(ctx, parsed)
	if err != nil {
		return nil, fmt.Errorf("resolving DID: %w", err)
	}
	pds := ident.PDSEndpoint()
	if pds == "" {
		return nil, fmt.Errorf("no PDS endpoint in DID document")
	}
	c := client.NewAPIClient(pds)

	type record struct {
		Value struct {
			TrackName  string `json:"trackName"`
			PlayedTime string `json:"playedTime"`
		} `json:"value"`
	}
	type page struct {
		Cursor  *string  `json:"cursor"`
		Records []record `json:"records"`
	}

	seen := map[string]bool{}
	cursor := ""
	// Small buffer so records with a slightly early playedTime still count.
	cutoff := since.Add(-time.Hour)
	for {
		params := map[string]any{"repo": did, "collection": playCollection, "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var out page
		if err := c.Get(ctx, syntax.NSID("com.atproto.repo.listRecords"), params, &out); err != nil {
			return nil, fmt.Errorf("listing records on %s: %w", pds, err)
		}
		reachedCutoff := false
		for _, r := range out.Records {
			played, err := time.Parse(time.RFC3339, r.Value.PlayedTime)
			if err != nil {
				continue
			}
			seen[playKey(r.Value.TrackName, played)] = true
			if played.Before(cutoff) {
				reachedCutoff = true
			}
		}
		if reachedCutoff || out.Cursor == nil || *out.Cursor == "" || len(out.Records) == 0 {
			break
		}
		cursor = *out.Cursor
	}
	return seen, nil
}

func playKey(name string, played time.Time) string {
	return strings.ToLower(strings.TrimSpace(name)) + "|" + played.UTC().Truncate(time.Second).Format(time.RFC3339)
}

func artistName(t *models.Track) string {
	if len(t.Artist) == 0 {
		return ""
	}
	return t.Artist[0].Name
}
