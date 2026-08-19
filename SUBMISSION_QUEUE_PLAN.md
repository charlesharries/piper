# Queue play submission behind the trackers

> Design note, not yet implemented. Written 2026-08-08 against branch `mb-lookup-fidelity`.
> Line numbers refer to that branch and will drift.

## Context

Every producer today does the same thing inline in its polling loop: build a track, hydrate it against MusicBrainz, save it, submit it to the PDS. Hydration is the slow part — MusicBrainz allows one request per second across the whole process — so a poll cycle is held open by network work that has nothing to do with polling. That is what put seconds onto PDS upload latency, and it lengthens the Apple Music poll window in which plays get dropped.

Splitting the two halves means producers do only what they are good at (notice a play, write it down) and a single consumer does the slow work (hydrate, submit, retry). The `tracks` table becomes the queue.

Decisions taken: goroutines in one binary, not separate processes; a play whose hydration fails is still submitted with the service's own metadata; playing-now status stays inline and unqueued.

**Why the tracks table rather than a channel or a broker:** rows already carry every field the worker needs, `SaveTrack` already returns the row id, `models.Track.PlayID` is already populated from it, and `UpdateTrack` already exists. A DB-backed queue also survives restarts — an in-memory channel would drop the backlog on every deploy. And if you later want a separate process, it is a deployment change against the same table rather than a rewrite.

**Why one worker:** MusicBrainz is rate-limited to 1 req/sec globally, so concurrency buys nothing on the expensive path. A single consumer means no claiming, no locking, no visibility timeout — the whole class of queue problems disappears.

---

## 1. Schema — `db/db.go`

Add beside the existing `ALTER TABLE tracks` block (~line 147), following its `duplicate column name` idiom:

```sql
ALTER TABLE tracks ADD COLUMN submitted_at TIMESTAMP;      -- NULL = queued
ALTER TABLE tracks ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tracks ADD COLUMN next_attempt_at TIMESTAMP;   -- NULL = ready now
CREATE INDEX IF NOT EXISTS idx_tracks_pending ON tracks(id) WHERE submitted_at IS NULL;
```

**Backfill, and do not skip this.** Every existing row would otherwise read as queued and the worker would re-submit the user's entire listening history to their PDS. Guard on the ALTER having actually succeeded, so it runs exactly once:

```go
_, err = db.Exec(`ALTER TABLE tracks ADD COLUMN submitted_at TIMESTAMP`)
switch {
case err == nil:
    // Column is new, so every existing row predates the queue and has already
    // been submitted by the old inline path. Claiming otherwise would replay a
    // user's whole history into their repo.
    if _, err := db.Exec(`UPDATE tracks SET submitted_at = CURRENT_TIMESTAMP`); err != nil {
        return err
    }
case err.Error() != "duplicate column name: submitted_at":
    return err
}
```

The truly minimal version is `submitted_at` alone. `attempts` and `next_attempt_at` are worth the two extra columns: without them a play that fails permanently is retried forever, and one that fails transiently (a PDS outage, the DPoP nonce path) is retried immediately and pointlessly.

## 2. Queue accessors — `db/db.go`

Three functions, modelled on the existing `GetRecentTracks` scan:

- `PendingTracks(limit int) ([]PendingTrack, error)` — joins `users` for the DID and session id the worker needs:
  ```sql
  SELECT t.id, t.name, t.recording_mbid, t.artist, t.album, t.release_mbid, t.url,
         t.timestamp, t.duration_ms, t.progress_ms, t.service_base_url, t.isrc,
         t.has_stamped, t.user_id, u.atproto_did, u.most_recent_at_session_id
  FROM tracks t JOIN users u ON u.id = t.user_id
  WHERE t.submitted_at IS NULL
    AND t.attempts < ?
    AND (t.next_attempt_at IS NULL OR t.next_attempt_at <= CURRENT_TIMESTAMP)
    AND u.atproto_did IS NOT NULL AND u.atproto_did != ''
  ORDER BY t.id
  LIMIT ?
  ```
  `ORDER BY t.id` gives insertion order, so plays reach the PDS in the order they happened.
- `MarkTrackSubmitted(trackID int64) error`
- `MarkTrackFailed(trackID int64, retryAt time.Time) error` — increments `attempts`, sets `next_attempt_at`.

`PendingTrack` is a small struct wrapping `models.Track` plus `UserID`, `DID`, `SessionID`.

## 3. The worker — new `service/submitter/submitter.go`

Follows the shape of the other services (struct + `New` + `StartX` ticker loop, logger with a package prefix).

```go
type Service struct {
    db      *db.DB
    mb      *musicbrainz.Service
    atproto *atprotoauth.AuthService
    logger  *log.Logger
}

func New(database *db.DB, mb *musicbrainz.Service, atproto *atprotoauth.AuthService) *Service
func (s *Service) StartSubmitter(ctx context.Context, interval time.Duration)  // ticker -> drain
func (s *Service) drain(ctx context.Context)
```

`drain` fetches a batch and, per row:

1. `musicbrainz.HydrateTrack(s.mb, track)` — on error, log and keep the unhydrated track (the agreed behaviour: a play reaches the PDS with the service's own metadata rather than not at all).
2. `db.UpdateTrack(pending.Track.PlayID, hydrated)` on success, so the MBIDs persist and a retry does not re-do the lookup.
3. `atprotoservice.SubmitPlayToPDS(ctx, did, sessionID, track, s.atproto)` — the canonical submitter at `service/atproto/submission.go:17`; the per-service `SubmitTrackToPDS` methods are thin wrappers around it.
4. `MarkTrackSubmitted` on success; `MarkTrackFailed` with an exponential delay on failure.

Constants: `maxAttempts = 5`, `batchSize = 20`, backoff `1 << attempts` minutes. Drain is synchronous inside the one goroutine, so ticks cannot overlap and no re-entrancy guard is needed.

## 4. Producers become save-only

Strip hydration and PDS submission from each; leave the dedupe and `SaveTrack` calls alone.

- `service/spotify/spotify.go:786` `stampTrack` — drop the `HydrateTrack` block and everything after `SaveTrack` (the user fetch, the DID checks, the submit).
- `service/applemusic/applemusic.go:384-389` — drop the inline `HydrateTrack` from `toTrack`, so `toTrack` becomes pure conversion. This is what shortens the Apple Music poll cycle.
- `service/lastfm/lastfm.go:428` — drop the hydrate and the `SubmitTrackToPDS` call.
- `cmd/handlers.go:572` `hydrateAndSubmitListens` — delete it and its goroutine launch. The import handler saves rows; the worker picks them up. This is the function the new worker generalises.

Then delete what is left dead: `spotify.SubmitTrackToPDS` and `lastfm.SubmitTrackToPDS` (check for remaining callers first — the spotify one carries an empty-name guard worth moving into the worker).

`service/playingnow` is deliberately untouched.

## 5. Wire it up — `cmd/main.go`

Beside the existing tracker goroutines (~line 224):

```go
submitterService := submitter.New(database, mbService, atprotoService)
go submitterService.StartSubmitter(ctx, submitInterval)
```

`submitInterval` from `viper` with a default around 5s, added to `config/config.go` and `.env.template` following the `listenbrainz.token` pattern.

---

## Verification

1. `gofmt -l .`, `go build ./...`, `go vet ./...`, `go test ./...`.
2. **Migration safety, the one that matters** — copy a DB that has existing rows, run the binary once, and confirm `SELECT count(*) FROM tracks WHERE submitted_at IS NULL` is `0` immediately after startup. A non-zero count means history is about to be replayed into a user's repo. Also run twice to confirm the backfill is not re-applied.
3. Unit tests in `db` for `PendingTracks` (ordering, the attempts cap, the `next_attempt_at` gate, and that users with no DID are excluded) against an in-memory SQLite DB.
4. Unit tests for `drain` using the existing fake patterns: a stub that fails hydration must still submit; a failing submit must increment `attempts` and set `next_attempt_at`; a successful one must set `submitted_at` and persist the MBIDs via `UpdateTrack`.
5. End to end: start piper, play something, and watch a row appear with `submitted_at IS NULL`, then flip to set within a tick. `musicbrainz:` log lines should now come from the worker, not from a poll cycle.
6. Confirm the poll loops got faster — Apple Music's `ProcessUser` should no longer block on MusicBrainz at all, which is the point.

## Out of scope

- Separate processes. The table-as-queue makes that a later deployment change; doing it now means WAL mode, busy timeouts and two systemd units.
- The Apple Music `limit=1` dropped-plays bug (`applemusic.go:400`). Independent, lives on `main`, and this change only reduces how often it fires by shortening the poll cycle.
