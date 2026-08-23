# How MusicBrainz matching works

A review guide for the `mb-lookup-fidelity` branch. Every heading names something
you can grep for. Line numbers are as of `1c243bc` and will drift; the
identifiers won't.

---

## 1. The shape of it

One play in, one `*Match` out, one log line written.

```
tracker (spotify/lastfm/applemusic/playingnow/import)
  └─ musicbrainz.HydrateTrackContext          musicbrainz.go:873
       └─ Service.Resolve                     musicbrainz.go:719   ← the whole algorithm
            ├─ startEvent / defer ev.emit()   event.go:97, 157
            ├─ newMatchInput                  score.go:198         normalise the play
            ├─ resolveViaListenBrainz         musicbrainz.go:218   first opinion
            ├─ searchTiers → search           musicbrainz.go:591, 494
            ├─ rankCandidates → scoreRecording score.go:578, 479   pick the recording
            └─ resolveRelease                 musicbrainz.go:832   pick the pressing
                 ├─ pickRelease → bestRelease musicbrainz.go:841, release.go:129
                 └─ preferReleaseWithArt      coverart.go:147
       └─ ApplyMatch                          musicbrainz.go:884   merge back into the play
```

`Resolve` logs nothing as it goes. Every step writes onto an `event` carried on
the context, and `Resolve` emits it all as a single wide line on the way out.
That is the "wide events" bit — see §8.

**Files:**

| File | What it owns |
|---|---|
| `musicbrainz.go` | HTTP, caching, the tier ladder, `Resolve`, `ApplyMatch` |
| `score.go` | recording scoring — normalisation, similarity, weights, penalties |
| `release.go` | release (pressing) scoring — a *separate* weight set |
| `coverart.go` | the artwork pass over the release group |
| `event.go` | the wide event: field names, groups, outcome vocabulary |
| `cache.go` | generic `ttlCache[V]` |
| `clean.go` | pre-existing `MetadataCleaner` (mostly unchanged; see §3) |

---

## 2. Two scores, not one

This is the thing that makes the code confusing on a first read: there are **two
independent scoring systems** with similar-looking names.

| | Recording scoring | Release scoring |
|---|---|---|
| Entry point | `scoreRecording` (score.go:479) | `scoreRelease` (release.go:40) |
| Answers | "is this the right *performance*?" | "is this the right *pressing*?" |
| Weights | `weightTitle`, `weightArtist`, `weightDuration`, `weightAlbum`, `weightMBScore` | `releaseWeightTitle`, `releaseWeightOfficial`, `releaseWeightAlbumType`, `releaseWeightCountry`, `releaseWeightArt` |
| Threshold | `minConfidence` = 0.62 | `releaseConfidence` = 0.8 |
| Ranker | `rankCandidates` (score.go:578) | `bestRelease` (release.go:129) |

They are coupled in one place, and it's the subtlest line in the branch:
`matchInput.albumScore` (score.go:382) computes a *recording's* album signal by
running the **release** scorer over that recording's releases and taking the
best. So "does this recording belong to the album you named" is answered by
"can this recording be attributed to a good pressing of that album" — a
recording whose only home is a bootleg loses to one on the official issue even
though both carry the album's title.

---

## 3. Normalising the input — `matchInput`

`newMatchInput` (score.go:198) is computed once per play and reused for every
candidate. Grep `matchInput` to find everything that reads from it.

- `normalize` (score.go:63) — NFD-decompose, drop combining marks, lowercase,
  `&` → ` and `, collapse everything non-alphanumeric to single spaces. This is
  what makes `Power, Corruption & Lies` == `Power Corruption and Lies` and
  `Beyoncé` == `Beyonce`.
- `similarity` (score.go:117) — `1 - levenshtein/len(longest)`, in [0,1]. Empty
  string scores 0 against anything, including another empty string.
- `splitQualifier` (score.go:136) — splits `Dreams (outtake)` and
  `Dreams - 2004 Remaster` into base + qualifier. Bracket suffixes first, then
  ` - ` / ` – ` / ` — ` (spaces required, so hyphenated titles survive).
  `splitQualifierRaw` is the un-normalised version, used to build queries.

Two separate qualifier vocabularies, deliberately kept apart:

- `guffParenWords` (clean.go:13) → `isVariantQualifier` (score.go:172) — marks a
  different *performance*: live, outtake, karaoke, remix, instrumental.
- `editionWords` (release.go:188) → `hasEditionWord`/`isEditionQualifier`
  (release.go:194, 203) — marks a different *edition of a release*: deluxe,
  anniversary, expanded, remaster.

`isEditionQualifier` is the union of both; `hasEditionWord` is edition-only.
That distinction is load-bearing in `searchAlbum` (release.go:231): only a true
*edition* suffix is stripped before searching, so
`Blade Runner (Music From The Original Soundtrack)` keeps its parenthetical
(it names the album) while `Rumours (Super Deluxe)` searches as `Rumours`.

**The one change in `clean.go`** (`DropForeignChars`, clean.go:64): the guard
went from "kept at least one letter" to "kept at least half the letters"
(`keptLetters*2 >= totalLetters`). A title that is mostly non-Latin now falls
back to the original text rather than being reduced to a stray Latin fragment.

---

## 4. ListenBrainz — the first opinion, and its four verdicts

`resolveViaListenBrainz` (musicbrainz.go:218) asks ListenBrainz's metadata
lookup for a recording MBID, then looks that recording up against MusicBrainz
and **scores it with the same `scoreRecording`** used on search results.

Only `RecordingMBID` is taken from ListenBrainz — see the doc comment on
`ListenBrainzResult` (musicbrainz.go:114) for why the release MBID was
deliberately dropped (it picked a different pressing per track and scattered
albums).

The verdict vocabulary is in event.go:26 and lands in the log as `lb.outcome`:

| `lb.outcome` | Meaning | Set at |
|---|---|---|
| `miss` | asked, ListenBrainz had nothing | musicbrainz.go:729 (default) |
| `error` | the lookup failed | musicbrainz.go:732 |
| `rejected` | answer scored below `minConfidence` — discarded outright | musicbrainz.go:235 |
| `accepted` | scored fine *and* `contradicts` had no objection — returned immediately, no search runs | musicbrainz.go:746 |
| `doubted` | scored fine but `contradicts` objected — kept as incumbent, search gets a chance to beat it | musicbrainz.go:749 |
| `dropped` | doubted, and **no search tier could be reached** — thrown away rather than published on the strength of an outage | musicbrainz.go:818 |

The `lb` group is only emitted when `lbAttempted` is true, so its **absence
means no token is configured**, not that ListenBrainz had nothing to say.

### `contradicts` — the extra bar ListenBrainz has to clear

`matchInput.contradicts` (score.go:413) is the asymmetry worth understanding. A
search winner already beat every alternative on these signals; ListenBrainz's
answer was never compared to anything, so it gets one extra test:

1. **length** — `durationScore == 0` (>20s apart). Cheap, and catches a bootleg
   pressing catalogued under the album's own title.
2. **album** — `albumScore < albumAgreement` (0.8).

Each test stays silent when the evidence is missing. The objecting signal's name
is logged as `lb.disagrees_on`.

**A `doubted` answer can still win.** In the tier loop, `lbMatch.Score >= best.score`
(musicbrainz.go:785) returns the ListenBrainz answer — **ties go to
ListenBrainz**. So a log line reading `outcome=matched source=listenbrainz
lb.outcome=doubted` means "we distrusted it, searched anyway, couldn't do
better, kept it". That combination is normal, not a bug.

---

## 5. The tier ladder — `searchTiers`

`searchTiers` (musicbrainz.go:591) builds up to seven attempts, **most
constrained first**. `Resolve` stops at the first tier whose top candidate
clears `minConfidence`; scoring is what keeps the looser tiers safe.

| # | Query | Limit | Why |
|---|---|---|---|
| 1 | `isrc:"…"` alone | 25 | An ISRC identifies a recording outright; any extra filter only causes false negatives |
| 2 | clean track + clean artist + clean album | 25 | Tightest metadata query |
| 3 | clean track + clean artist | 50 | Album drops from filter to scoring signal |
| 4 | **raw** track + artist + album | 25 | `MetadataCleaner` is lossy (truncates at commas, strips non-Latin) — retry with what the service actually sent |
| 5 | **raw** track + artist | 50 | ditto |
| 6 | `recording:"…" AND arid:<mbid>` | 25 | See below |
| 7 | dismax free text | 50 | Last resort: bare words to MusicBrainz's fuzzy parser |

Deduplicated by query string via the `seen` map inside `addTier`.

**Tier 6 is the interesting one.** Both artist fields on the recording index
(`artist` = credit line, `artistname` = artist's own name) hold names *as
catalogued*, and neither consults aliases. So an artist MusicBrainz holds in a
non-Latin script is unreachable by the Latin name Spotify credits them with, and
tiers 1–5 all come back empty. The *artist* index does index aliases, so
`buildArtistEndpoint` (musicbrainz.go:343) searches `artist:"…" OR alias:"…"`,
and `scopeTier` (musicbrainz.go:546) appends the resulting MBID as an `arid:`
filter — sidestepping names entirely.

`artistMBID` (musicbrainz.go:514) does **not** trust MusicBrainz's ranking: it
picks by `Artist.goesBy` (score.go:300) across name, both readings of the sort
name (`sortNameReadings`, score.go:281, which handles `Yonezu, Kenshi` →
`Kenshi Yonezu`) and aliases, and requires `artistNameAgreement` = 0.9. Below
that it returns empty and the tier is **skipped**, because scoping a search to
the wrong artist's catalogue is worse than not scoping it. Logged as
`search.artist_scope` = `resolved` / `unresolved` / `failed`.

Query-building safety: `phrase`/`escapeLucene` (musicbrainz.go:253) escape `\`
and `"` for the fielded tiers; `freeText` (musicbrainz.go:265) *strips* Lucene
metacharacters for the dismax tier, because escaping there would leave
backslashes in the text being matched.

---

## 6. Recording scoring — `scoreRecording`

score.go:479. A weighted mean, then flat penalties subtracted, clamped at 0.

**Short circuit:** if the play carries an ISRC and the candidate lists it, return
`1` with `sig.isrc=1` and nothing else is computed (score.go:483).

**Signals** — each contributes *only when the data exists*, so a source without
durations (Last.fm) is scored on the rest rather than penalised. That's the
`add` closure at score.go:494 accumulating both `weighted` and `total`.

| Signal | Weight | Function | Notes |
|---|---|---|---|
| `title` | 3.0 | `titleScore` (score.go:246) | medley-aware, see below |
| `artist` | 3.0 | `artistScore` (score.go:314) | best of any input name × any credit name, sort-name reading, alias, or the full joined credit line |
| `duration` | 2.0 | `durationScore` (score.go:354) | banded: ≤2s→1.0, ≤5s→0.8, ≤10s→0.4, ≤20s→0.1, else 0 |
| `album` | 1.0 | `albumScore` (score.go:382) | runs the *release* scorer — see §2 |
| `mb` | 0.25 | `rec.Score/100` | MusicBrainz's own query score, tiebreak only; explicitly **not** corroboration |

**Penalties** (flat, subtracted after the mean):

| `sig` key | Amount | Const | Triggered when |
|---|---|---|---|
| `variant` | −0.25 | `qualifierPenalty` | candidate carries a variant qualifier the play didn't ask for |
| `video` | −0.25 | `qualifierPenalty` | `rec.Video` |
| `conflict` | −0.25 | `durationConflictPenalty` | both sides have a length and `durationScore` returned 0 |
| `uncorroborated` | −0.4 | `uncorroboratedPenalty` | title and artist were the *only* signals |

The two penalties that carry the branch's headline fixes, with the arithmetic:

**`uncorroboratedPenalty`** — the score is a weighted *mean*, so its denominator
shrinks with the signals it lacks. Title 1.0 + artist 1.0 and nothing else =
`6.0/6.0` = **1.00**, a perfect score for evidence every live take and karaoke
version in the database also satisfies. Minus 0.4 → **0.60**, just under
`minConfidence`. See `TestScoreRecordingRejectsTitleAndArtistAlone`.
Note `corroborated` is set by duration *or* album (score.go:507) — either alone
is enough.

**`durationConflictPenalty`** — weighting alone can't express this. Duration is
2 parts in 9, so title + artist agreement alone is `6/9` = 0.667, already over
threshold. A soundtrack where every candidate shares a title and artist resolved
to bootleg cuts running minutes long. With the play and candidate 30s apart:
`(3+3+0+1)/9` = 0.778, minus 0.25 → **0.528**, rejected.

### Medleys and tracklists — `titleScore`

MusicBrainz titles a two-song recording `A / B`; Spotify reports only the song
the listener thinks they're playing. `titleScore` (score.go:246) scores the
whole title first (plenty of titles contain a slash), then each
`medleySeparator`-split part, keeping the best. Scoring on the best part also
decides *whose* qualifier is judged: in `A / B (alternate version)` the
qualifier belongs to B, so a play of A isn't penalised for it.

`maxMedleySongs` = 3 caps it. Past three parts the same separator is cataloguing
a **DJ mix's tracklist** — which would otherwise match any song it contains at
1.00 *and* match on artist (a mix credits everyone it plays). That's how a play
of Tomoko Aran's "I'm in Love" resolved to a 25-song yacht-rock mix. Tests:
`TestTitleScoreMatchesOneSongOfAMedley`,
`TestTitleScoreDoesNotTreatATracklistAsAMedley`.

---

## 7. Picking the pressing, and the artwork pass

`resolveRelease` (musicbrainz.go:832) = `pickRelease` then `preferReleaseWithArt`.

### `scoreRelease` (release.go:40)

Same weighted-mean-then-penalties shape, different weights:

| Signal | Weight | Notes |
|---|---|---|
| `title` | 4.0 | `compareAlbum`, also tried against the **release group** title (releases often differ from the name people know) |
| `official` | 1.0 | `Status` empty or `"Official"` |
| `type` | 0.75 | Album 1.0, EP 0.6, else 0 |
| `country` | 0.25 | `preferredCountries` = XW, US, GB, XE |
| `art` | 1.0 | only when `artOwners` is supplied — see below |

| `sig` key | Amount | Triggered when |
|---|---|---|
| `secondary` | −0.5 | compilation/live/soundtrack/remix — **waived** when `titleScore >= 0.9`, so someone genuinely playing a soundtrack still gets one |
| `single` | −0.3 | release title == track title and the play came from an album |
| `edition` | −0.4 | release carries an edition qualifier **and the play named none** — `Rumours` must never land on an anniversary remix, but `Rumours (Super Deluxe)` may legitimately land on either |

Ties break via `earlier` (release.go:166) toward the original issue, comparing
dates a *year at a time* because MusicBrainz stores YYYY / YYYY-MM / YYYY-MM-DD
and plain string comparison sorts `1994` ahead of `1994-06-21`. Within a year
the *more precise* date wins, then MBID for stability — otherwise an album's
tracks scatter across pressings.

`pickRelease` (musicbrainz.go:841): if the search result's release list yields
nothing scoring `releaseConfidence` (0.8), pay for one `LookupRecording` — search
results carry only a slice of a recording's releases, and the good pressing is
often not in it.

### The artwork pass — `preferReleaseWithArt` (coverart.go:147)

`releaseMbId` is the only identifier in a play record that resolves to a cover
(the Cover Art Archive is keyed on releases), and most pressings have no art.
So:

1. **Fast path** — `hasCoverArt` (coverart.go:39): a `HEAD` to
   `coverartarchive.org/release/<id>/front`, redirects not followed, 3s timeout.
   The CAA is *not* MusicBrainz, so this dodges the 1 req/s limit. A 3xx means
   art exists → keep the release, `art.outcome=had_art`, done.
2. Otherwise `releaseGroupPressings` (coverart.go:105) browses the whole release
   group — one MusicBrainz request that reports both which pressings exist and
   which have art (`cover-art-archive.front`). Cached per release group.
3. Re-run `bestRelease` over that group **with `artOwners` supplied**, so art
   enters as a 1.0-weighted signal. The incumbent is re-scored under the same
   signal rather than swapped blindly — a pressing only loses to one that is at
   least as good an answer for the album.

`art.outcome` vocabulary (event.go:36): `had_art`, `swapped`, `no_art_in_group`,
`kept_without_art`, `not_attempted`. `TestResolveKeepsRightAlbumOverCoverArt`
is the test that pins "right album beats having a cover".

---

## 8. The wide event

`event.go`. The rationale is at the top of the file: hydrating one play is a
dozen decisions; logging them as they happen scatters the interesting fields
across uncorrelated lines, and concurrent trackers interleave. So each step
records onto an `event` on the context and `Resolve` emits once. **One play, one
line**, `msg: "track_hydrated"`, JSON to stderr.

Mechanics worth checking in review:

- `eventFrom` (event.go:113) **never returns nil** — paths reachable outside a
  hydration (`SearchMusicBrainz` from the search handler) get a throwaway that's
  never emitted. That's why recording a field is always a plain assignment with
  no nil check. `TestNoEventOutsideAHydration` covers it.
- `emit` (event.go:157) deliberately passes `context.Background()`, not the
  resolve's context — a hydration cancelled halfway is the one most worth having
  a record of.
- `WithEventContext` (event.go:56) is how callers attach dimensions this package
  can't know. All five call sites pass `user_id` and `play_source`:
  spotify.go:800, lastfm.go:429, applemusic.go:422, playingnow.go:71,
  handlers.go:581.
- `logRound` (score.go:473) rounds to 3dp — enough to see why 0.619 missed 0.62.

### Field schema

```
outcome            matched | unmatched | error
duration_ms        wall time of the hydration
user_id            } from WithEventContext
play_source        } spotify|lastfm|applemusic|playingnow|import
source             listenbrainz | musicbrainz   (only when matched)
in.{track,artist,album,isrc?,duration_ms?}
out.{recording_mbid,recording,artist_mbid?,release_mbid?,release?,release_group_mbid?}
score, threshold, candidates, runner_up_score?
sig.{isrc | title,artist,duration?,album?,mb?, variant?,video?,conflict?,uncorroborated?}
lb.{outcome, recording_mbid?, score?, disagrees_on?}      omitted entirely when no token
search.{tiers_run, tiers_failed, won_at_tier?, artist_scope?}
art.{outcome, from_release_mbid?}
cost.mb_requests
err?
```

`sig` is a *group* specifically so a signal named after a field the event already
carries (`title`, `artist`, `album`) doesn't collide with it (score.go:462).

`score`/`threshold`/`candidates` appear on `unmatched` too: `event.unmatched`
(event.go:130) keeps the closest candidate, so a threshold set too high reads as
a near miss rather than as MusicBrainz holding nothing.

### Querying it

```sh
go run ./cmd/musicbrainz-cli -batch cmd/musicbrainz-cli/testdata/golden.jsonl 2>events.jsonl

# what's going unmatched, and which signal fell short
jq 'select(.outcome=="unmatched") | {track: .in.track, score, sig}' events.jsonl

# matches that won by a hair — the ones most likely to be wrong
jq 'select(.runner_up_score and .score - .runner_up_score < 0.05)' events.jsonl

# where ListenBrainz and MusicBrainz disagreed
jq 'select(.lb.outcome=="doubted") | {track:.in.track, on:.lb.disagrees_on, src:.source}' events.jsonl

# expensive hydrations (the case for the submission queue)
jq 'select(.cost.mb_requests > 6) | {track:.in.track, n:.cost.mb_requests, ms:.duration_ms}' events.jsonl
```

`LOG_LEVEL` (musicbrainz.go:176) gates verbosity: the hydration event is `info`,
request retries are `debug`.

---

## 9. Merging back — `ApplyMatch`

musicbrainz.go:884. The rule is *fields the music service supplied are preserved
wherever MusicBrainz has nothing better* — MusicBrainz's data is more complete,
but the service's data is what the user actually played.

- `DurationMs` — `cmp.Or(mb, service)`. MusicBrainz omits lengths on plenty of
  recordings; letting a zero through would erase the real duration and, for
  Spotify, break the play-time threshold that decides when a track is stamped.
- `ISRC` — service's wins, MusicBrainz fills a gap.
- `Artist` — `mergeArtists` (musicbrainz.go:916) attaches MBIDs **without
  discarding the service's own artist IDs**, which are the only link back to the
  source catalogue.
- `ReleaseDiscriminant` — new field (`models/track.go`, plumbed through
  `TrackToPlayRecord` in submission.go). Because we resolve `Rumours (Super
  Deluxe)` to the base release, the edition would otherwise be silently lost;
  `releaseDiscriminant` (release.go:214) preserves it on the published record.

---

## 10. Every tunable in one place

| Const | Value | File | What it decides |
|---|---|---|---|
| `minConfidence` | 0.62 | score.go:21 | publish an MBID at all |
| `weightTitle` / `weightArtist` | 3.0 / 3.0 | score.go:27 | |
| `weightDuration` | 2.0 | score.go:29 | |
| `weightAlbum` | 1.0 | score.go:30 | |
| `weightMBScore` | 0.25 | score.go:31 | |
| `qualifierPenalty` | 0.25 | score.go:37 | variant qualifier, and video |
| `durationConflictPenalty` | 0.25 | score.go:48 | |
| `uncorroboratedPenalty` | 0.4 | score.go:57 | |
| `artistNameAgreement` | 0.9 | score.go:296 | trust an artist MBID to scope a search |
| `albumAgreement` | 0.8 | score.go:402 | `contradicts` album test |
| `maxMedleySongs` | 3 | score.go:238 | medley vs tracklist |
| `releaseConfidence` | 0.8 | release.go:155 | stop looking / pay for a second fetch |
| `releaseSecondaryPenalty` | 0.5 | release.go:21 | |
| `releaseIsTrackTitlePenalty` | 0.3 | release.go:25 | |
| `releaseVariantPenalty` | 0.4 | release.go:30 | |
| `defaultSearchLimit` | 25 | musicbrainz.go:311 | candidates per tier (50 on loose tiers) |
| `artistSearchLimit` | 5 | musicbrainz.go:334 | |
| `maxAttempts` | 3 | musicbrainz.go:383 | retries per request |
| `negativeCacheTTL` | 5min | musicbrainz.go:98 | how long a miss is remembered |
| `cacheTTL` | 1h | musicbrainz.go:190 | how long a hit is remembered |
| `maxSearchCacheEntries` | 1000 | musicbrainz.go:95 | per cache, ×3 caches |
| `coverArtTimeout` | 3s | coverart.go:21 | |
| `browseReleasesLimit` | 100 | coverart.go:18 | |

---

## 11. Cost and caching

MusicBrainz allows **1 request/sec across the whole process** (`rate.NewLimiter`,
musicbrainz.go:187), so every request is a second of wall clock. Only
`doRequest` goes through the limiter; ListenBrainz has its own limiter and the
Cover Art Archive `HEAD` is unlimited.

Worst-case single hydration: 1 recording lookup (ListenBrainz's answer) +
7 search tiers + 1 artist search + 1 recording lookup (release list) + 1 release
group browse ≈ **11–12 MusicBrainz requests ≈ 12 seconds**. `cost.mb_requests`
is the field that tells you which plays are costing that. This is exactly what
`SUBMISSION_QUEUE_PLAN.md` (also on this branch, unimplemented design note) is
proposing to move off the polling loops.

Three `ttlCache[V]` instances (cache.go), all keyed on the endpoint URL:
`searchCache` (searches *and* recording lookups), `pressingsCache` (per release
group), `artistCache` (name → MBID). Misses are cached for `negativeCacheTTL`
only — a search that found nothing is often transient, or a gap MusicBrainz has
since filled, and holding it an hour keeps every replay of that track unmatched.
Eviction is "drop everything expired, then drop whatever is closest to expiring".

---

## 12. How to convince yourself it works

**The golden set** — `cmd/musicbrainz-cli/testdata/golden.jsonl`, 40 rows of real
service metadata with expected outcomes, documented in `testdata/readme.md`.
Rows can assert a recording MBID, a recording/release *title* (preferred — the
same song exists under many IDs across compilations), or `expect_no_match` for
nonsense input. Durations are load-bearing: often the only thing separating a
studio take from a live take.

```sh
go run ./cmd/musicbrainz-cli -batch cmd/musicbrainz-cli/testdata/golden.jsonl
go run ./cmd/musicbrainz-cli -batch cmd/musicbrainz-cli/testdata/golden.jsonl -explain miss
go run ./cmd/musicbrainz-cli -track Dreams -artist "Fleetwood Mac" -release Rumours -explain top
```

`-explain` prints the ranked candidates with their `signals` breakdown
(`candidate.explain`, score.go:559). The batch summary reports recording
accuracy, release accuracy, unmatched, **correct rejections** and **false
positives** separately — rejecting nonsense is scored as a win, which is the
whole point of `minConfidence`.

**Unit tests** — 82 across the package, and they read as a spec. The ones that
pin the decisions above:

| Behaviour | Test |
|---|---|
| ISRC short-circuits | `TestScoreRecordingISRCWins`, `TestResolveMatchesOnISRC` |
| outtake loses to the real take | `TestScoreRecordingRejectsOuttakeOverRealRecording` |
| duration breaks a tie / vetoes | `TestScoreRecordingUsesDurationToBreakTies`, `…RejectsContradictoryDuration` |
| bare title+artist is not enough | `TestScoreRecordingRejectsTitleAndArtistAlone` |
| medley vs DJ tracklist | `TestTitleScoreMatchesOneSongOfAMedley`, `…DoesNotTreatATracklistAsAMedley` |
| non-Latin artists | `TestArtistScoreUsesSortNameForNonLatinCredits`, `…UsesAliasesForNonLatinCredits`, `TestResolveScopesSearchToArtistMBID` |
| the LB verdict matrix | `TestResolveRejectsBadListenBrainzMatch`, `…DoubtsListenBrainzWhenAlbumDisagrees`, `…KeepsListenBrainzWhenSearchCannotBeatIt`, `…DropsDoubtedListenBrainzWhenSearchCannotRun` |
| edition handling | `TestBestReleaseRejectsUnaskedForEdition`, `…AllowsRequestedEdition`, `TestSearchAlbum`, `TestReleaseDiscriminant` |
| artwork never beats the right album | `TestResolveKeepsRightAlbumOverCoverArt` |
| the event's shape | `log_test.go` (all 6) |

`go build ./... && go test ./service/...` passes clean on `1c243bc`.

---

## 13. Things worth arguing about in review

Ordered roughly by how much they'd change behaviour. None of these are
"this is broken" — they're the judgement calls I'd want a second opinion on.

1. **`allCandidates` keeps the *longest* pool, not the best-scoring one**
   (musicbrainz.go:775). On the unmatched path the logged near-miss and
   `candidates` count come from whichever tier returned the most rows —
   typically a 50-limit loose tier — so `score` in an `unmatched` line may
   understate the best score any tier actually saw. Defensible (a bigger pool is
   more informative) but it's not what the field name suggests.
   `grep -n "allCandidates" service/musicbrainz/musicbrainz.go`

2. **`contradicts` only guards ListenBrainz, never a search winner**
   (score.go:413, called once at musicbrainz.go:742). The duration half is
   partly covered for search winners by `durationConflictPenalty`, but the
   **album half is not** — a search winner whose best attributable release scores
   0.5 against the named album still passes on title+artist+duration. Intentional
   per the doc comment; worth confirming that's the intent.

3. **Ties go to a *doubted* ListenBrainz answer** — `lbMatch.Score >= best.score`
   (musicbrainz.go:785). We objected to it, then let it win a tie against a
   candidate that survived ranking. `>` rather than `>=` is the alternative.

4. **Penalties are flat and can stack.** A video of an unasked-for variant with a
   duration conflict eats −0.75 off a weighted mean. Nothing bounds the total
   before the `math.Max(0, …)` clamp, so beyond a point the ordering between two
   bad candidates stops being meaningful.

5. **`albumScore` is the hot loop.** `scoreRecording` runs the full release
   scorer over every release of every candidate — up to 50 candidates × N
   releases per tier. There's an early break at `>= 0.99` (score.go:392) and
   it's all in-memory, so it's almost certainly fine, but it's the one place
   the two scorers multiply.

6. **The dismax tier's dedup check can't fire.** `seen` holds fielded Lucene
   queries; tier 7 checks `!seen[query]` against a bare-words string
   (musicbrainz.go:645), which will never collide. Harmless, but the check
   reads as if it does something.

7. **`HydrateTrack` is now dead in production** — every tracker moved to
   `HydrateTrackContext`, and the only remaining caller is
   `resolve_test.go:483`. Keep as a convenience wrapper or delete.

8. **Threshold provenance.** `minConfidence` = 0.62 sits ~0.02 above the exact
   0.60 that a bare title+artist match lands on. That's a tight margin, and it
   means the constant is really "just above the uncorroborated floor" rather
   than an independently chosen bar. Worth a comment saying so, or worth
   deriving it: `1 - uncorroboratedPenalty + ε`.

9. **`preferredCountries`** (release.go:35) hardcodes XW/US/GB/XE. Fine for now;
   it will mis-rank for a user whose library is mostly JP or DE pressings.

10. **`SUBMISSION_QUEUE_PLAN.md`** is a 137-line design note for unimplemented
    work, sitting at the repo root on a matching-fidelity branch. Probably wants
    to be an issue, or at least live under `docs/`.

11. **`models.SubmissionAgent` changed from a const to a function** reading
    viper (`models/constants.go`). That makes it order-dependent on
    `config.Load()` — which is why `userAgent()` (musicbrainz.go:376) resolves
    per request rather than at init. Correct as written, but it's a foot-gun for
    the next caller.

### Suggested reading order

If you want to review this in passes rather than all at once:

1. `score.go` top-to-bottom — it's self-contained and every constant is
   documented with the failure that motivated it.
2. `release.go` — same shape, second weight set.
3. `Resolve` (musicbrainz.go:719–826) — 100 lines, and it's the whole control
   flow. Read it with §4 and §5 open.
4. `event.go` + one real log line.
5. `coverart.go`, `cache.go` — independent of the rest.
6. Skim `resolve_test.go` and `score_test.go` as the spec.
