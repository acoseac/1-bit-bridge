## 2026-11-21 — B952: the enricher tests wait for the outcomes their rows reach, never for two seconds of wall clock

Branch `fix/b952-enricher-test-waits`, from main `85c26e6f` (#1535's merge, one past the brief's
floor `d1c1515a`). Two commits:
- `3d9d865b`: the seam and the waits;
- this record, with the rule line in `.claude/rules/build-ci-tests.md`.

Every command ran from the checkout's root with `GOTOOLCHAIN=go1.27.3` and the release tags (`-tags
ts_omit_webclient,ts_omit_debugeventbus`, `Makefile:9`), each `go test` with `-race` and `-timeout
30m` (the controls' `-timeout 20m`) and `TMPDIR` on disk in a scratch folder below the checkout's
`.tmp/`, where the line says nothing else. Lines cited are `85c26e6f`'s unless a commit is named.
Each control was predicted in writing before it ran.

### The red

Two sightings on `windows-latest`, attempt 1 of each run, both green on rerun:
- run 44602688600, main `fffa7f5a`: `--- FAIL: TestEnricherFetchesAndCachesArtistImage (5.16s)`,
  `artist_test.go:173: track never enriched`;
- run 44757953432, main `d1c1515a`: `--- FAIL: TestEnricherDeduplicatesArtistLookups (4.90s)`,
  `artist_test.go:288: only enriched 0 of 3`.

Both polled `e.Done()` every 10 ms until `time.Now().Add(2 * time.Second)` (`artist_test.go:168-174`
and `:283-289`), beside a worker whose context `startEnricherForTest` ended at three seconds. In
both jobs' logs the lines before each failure are the three `MB artist search … HTTP 503` warnings
of `TestResolveArtist_TransientErrorIsNotCached` (`artist_negative_cache_test.go`), some six
seconds earlier, and no upstream warning of the failing test's own: its pass met no error. In the
second run the first test after the 503 test that runs an enricher,
`TestEnricherFetchesAndCachesArtistImage`, passed, and the next that runs one,
`TestEnricherDeduplicatesArtistLookups`, failed.

### What the wait measured (item 1)

An overlay build, nothing tracked written: the enricher marked its phases, and each wait on
`e.Done()` in `artist_test.go` and `waitEnricherDone` waited up to 60 s, the worker living past it,
and printed how long it took. From the worker's start to the last stamp, quiet, 50 runs each:
`TestEnricherFetchesAndCachesArtistImage` 50 to 53 ms, `TestEnricherDeduplicatesArtistLookups` 51
to 63 ms. The first track's pass, medians: the listing done at 1.8 ms, the release search 0.9 ms,
the cover's fetch and atomic write 22 ms, the artist search and the portrait's search, fetch, atomic
write and link 15 ms, the stamp's commit 5 ms; the dedup test's two later tracks answer from the
caches in about 4 ms each. The test's setup before the wait (the store's open and migrations under
`-race`) took 0.78 to 0.80 s and its teardown 18 ms, both outside the wait. In place, once, after
every earlier test of the package: 51 and 62 ms.

Under load, each run pinned to the CPUs named beside busy-loop processes on the same CPUs, in the
build slot:

| load | runs | wait, the one-track test (median / max) | wait, the three-track test | setup (median) | waits over 2 s |
| --- | --- | --- | --- | --- | --- |
| one CPU, GOMAXPROCS 1, alone | 20 | 50 / 56 ms | 54 / 58 ms | 0.79 s | 0 |
| one CPU, 7 hogs | 20 | 101 / 121 ms | 174 / 183 ms | 6.2 s | 0 |
| one CPU, 15 hogs | 5 | 175 / 206 ms | 299 / 354 ms | 12.4 s | 0 |
| one CPU, 31 hogs | 8 | 335 / 440 ms | 604 / 640 ms | 25.2 s | 0 |
| four CPUs, GOMAXPROCS 4, 28 hogs | 10 | 113 / 133 ms | 230 / 274 ms | 10.9 s | 0 |

The base's own tests, as committed: beside 31 hogs on one CPU, 16 of 16 runs passed; beside 47, 12
of 12. So the red did not reproduce on this host by CPU load, and item 1 is a bound, not a
reproduction: a setup 32 times slower left the wait at 0.64 s at most. The pass is latency-bound
(loopback round trips, file syncs, a commit), and starved of CPU it grew 2 to 10 times where the
CPU-bound setup grew 8 to 32. CI's failures run the other way: 5.16 s in all is about 3.2 s of setup
and teardown, some 4 times this host's quiet 0.8 s, beside a wait past 2 s, over 40 times this
host's quiet 50 ms. So the extra time sat in the pass's own work, where Windows puts its file I/O:
two atomic artwork writes, each synced and renamed through `atomicwrite.RenameWithRetry`, whose
retries on a file another process holds may take 750 ms (`renameBackoff`), and the stamp's commit.
That is an inference this host cannot test (Linux, without that platform's on-access scan); the
proof of the fix on Windows is the PR's own `test (windows-latest)` job.

### The rival: state an earlier test leaves

None, in the code or in the measurements:
- Pacing is per enricher: `MBMinInterval`, `CAAMinInterval` and `DeezerMinInterval`, read through
  `mbMinInterval` and `caaMinInterval`, which ask the client only for a live base. The upstream
  failure streak is per enricher (`NewEnricher`'s `reports`). `Retry-After` is a sleep inside the
  one request that got it (`musicbrainz.go:262-273`). No package variable holds a limiter, a
  backoff or a streak; `enrichgate` holds two error values and nothing else. The one state clients
  share is a transport's idle pool (`sharedHTTPTransport`'s, and `http.DefaultTransport`'s under the
  tests' `rewritingTransport`), keyed by host and port, and every test's fakes listen on ports of
  their own.
- `TestResolveArtist_TransientErrorIsNotCached` builds an enricher field by field (no `reports`, so
  each 503 warns: the three lines), calls `resolveArtist` three times on the test's goroutine,
  sends no `Retry-After`, and returns with nothing of its own running.
- The rate-limit fields: the failing tests leave none at its default. `startEnricherForTest` set
  the MusicBrainz, Cover Art Archive and Deezer intervals to 0 and `PollInterval` to 5 ms;
  `ITunesMinInterval` stays at 3 s with no iTunes client wired; neither client has a live base,
  so the fields are what paces, and no pacing sleep ran. They do not explain the delay.
- The minimal pair, the measure build: the 503 test first (`-run
  '^(TestResolveArtist_TransientErrorIsNotCached|TestEnricherFetchesAndCachesArtistImage|TestEnricherDeduplicatesArtistLookups)$'`,
  which runs the three in turn at each count), or the two alone. Quiet, 50 runs each: medians 51
  and 62 ms with it, 51 and 62 ms without; maxima 53 and 63 ms, 52 and 63 ms. Beside 7 hogs on one
  CPU, three rounds of four runs each, alternating: medians 95, 111 and 104 ms and 175, 176 and
  176 ms with it, 117, 116 and 108 ms and 169, 181 and 174 ms without. A limiter or a backoff would
  add a fixed delay to the first lookup; none shows.

So item 3, a reset in an earlier test's cleanup, does not apply, and control (a) parks a boundary.

### The seam and the waits (`3d9d865b`)

Production, `enricher.go`: `Enricher.SetOnOutcomeForTest`, nil in production, the package's
`…ForTest` style (`SetAllowedImageHostsForTest`). `stampEnriched` and `markSkipped` tell it each
track's `Outcome` once the stamp's write has answered: `OutcomeDone` after a committed stamp is
counted done; `OutcomeSkipped` after a committed skip is counted under its reason and logged;
`OutcomeFailed` for a stamp the store refused with an error that is no stop, a skip's included,
which `markSkipped` still counts under its reason. A pass that records nothing tells it nothing:
one the shutdown or the switch stopped, one left for the next batch, a stamp over a changed row.
Every artwork write of a pass returns before its stamp (the package starts no goroutine of its
own), so a heard outcome's writes have landed. Nothing else in production changed.

Tests, `enrich_test.go`: `startEnricherForTest(e)` returns the run. It installs the seam before Run
starts, runs Run on a context of `enricherRunLifetime` (`enricherWaitBound` and 10 s), and its
`join` cancels and waits for Run, deferred at each call site after the store's and the servers'
teardown, as before. `run.wait(t, outcome, n, why)` waits for `n` tracks of the outcome the site
names, and fails at once on another outcome (naming the track) or on Run's return before `n`, and
otherwise at `enricherWaitBound`, 30 s from the run's start, bounded from above alone. Each failure
gives the site's own words and every outcome the run reported.
`TestTheOutcomeSeamHearsEachStampOnceItsWriteHasAnswered` pins the seam: a stamp and a skip heard
only once their rows have left the unenriched set, a stamp and a skip the store refuses heard as
failed, and a stamp the shutdown stopped not heard.

The census, the 14 sites `deadline := time.Now().Add(` named in the package, every one in scope:
the 1.5 s waits are tighter than the two that failed, the 3 to 5 s ones bound the same work on the
same runners, and control (a) turns each red that asserts anything.

| site | waited for | bound / worker's life | now |
| --- | --- | --- | --- |
| `artist_test.go:168`, `TestEnricherFetchesAndCachesArtistImage` | `Done() >= 1` | 2 s / 3 s | 1 done |
| `artist_test.go:283`, `TestEnricherDeduplicatesArtistLookups` | `Done() >= 3` | 2 s / 3 s | 3 done |
| `artist_test.go:435`, `TestCAAReleaseGroupFallbackSalvagesArtwork` | `Done() >= 1` | 2 s / 3 s | 1 done |
| `mbid_validation_test.go:61`, `TestSearchResultMBIDTraversalDoesNotEscapeCacheDir` | `Done() >= 1`, which never came: its track is skipped; nothing asserted | 2 s / 3 s | 1 skipped, before it looks |
| `mbid_validation_test.go:116`, `TestBlankArtistAlbumStillScrubsHostileTagMBID` | the same | 2 s / 3 s | 1 skipped, before it reads the row |
| `enrich_test.go:84`, `waitEnricherDone` | `Done() >= want`, its callers' bound: `:706` 2 s, `:784` 2 s, `:845` 2 s, `:915` 1.5 s, `:1003` 1.5 s, `:1074` 1.5 s | as given / 3 s (`:706-845`), 2 s (`:915-1074`) | removed; its callers wait for 2, 1, 3, 1, 1 and 1 done |
| `enrich_test.go:873`, `TestEnricherSkipsUnsearchableTracks` | `skipped >= 1` | 1.5 s / 2 s | 1 skipped |
| `skip_reasons_test.go:42`, `TestSkipReasonsAttributeTheRightCause` | `skipped >= 2` | 3 s / 3 s | 2 skipped |
| `skip_reasons_test.go:104`, `TestSkipReasonsKeysStayBounded` | `skipped >= 5`; nothing asserted | 3 s / 3 s | 5 skipped |
| `artist_cache_name_test.go:116`, `TestArtistCacheHitUsesCanonicalNameForDeezer` | `Done() >= 2` | 4 s / 5 s | 2 done |
| `pacing_test.go:149`, `TestEnricherResolvesArtistWhenAlbumMisses` | `skipped >= 1` | 4 s / 5 s | 1 skipped |
| `stamp_changed_row_test.go:57`, `runUntilStamped` (two callers) | the row out of the unenriched set | 3 s / 3 s | the outcome its caller names: 1 done, 1 skipped |
| `acoustic_reachability_test.go:51`, `TestAcousticFallbackReachableWithLocalArtwork` | the row's `ArtistMBID` set | 5 s / 5 s | 1 done, then the row read |
| `cancel_test.go:425`, `waitFor` (10 tests in `cancel_test.go`, `switch_test.go`, `switch_gate_test.go`) | loop turns, log lines, rows, files | 5 s / until stopped | `enricherWaitBound`; its conditions are no track outcomes, so it keeps its poll |

### Controls

Each through `go test -overlay` over `3d9d865b`'s tree (nothing tracked written; `git status`
clean after each), each run once, its predictions written first. Boundary parked in (a) and (c):
the entry of `stampEnriched` and of `markSkipped`, a pass's last step, the first stamp attempt of
each enricher held 5.5 s or until the pass's context ended (a scratch file the overlay adds). 5.5 s
outlasts every old wait. Selection: the 19 converted tests, and the seam test where named.

| | mutation | predicted | seen |
| --- | --- | --- | --- |
| a-base | `85c26e6f`'s tests and enricher, the stamp parked | 17 red, each at its old bound in the base's words: `artist_test.go:173` and `:288` (CI's lines), `:440`, `enrich_test.go:707`, `:785`, `:846`, `:878` with `:884`, `:916`, `:1004`, `:1075`, `skip_reasons_test.go:47` ("only 0 tracks skipped"), `artist_cache_name_test.go:121`, `pacing_test.go:154`, `stamp_changed_row_test.go:166` and `:195` with their helpers' lines, `acoustic_reachability_test.go:62`, and `mbid_validation_test.go:127` ("hostile embedded MBID survived", a false red: the row read back unprocessed); 2 green, asserting nothing about the wait (`TestSearchResultMBIDTraversalDoesNotEscapeCacheDir`, `TestSkipReasonsKeysStayBounded`) | 17 red and those 2 green, as predicted, but for two details: `:47` said "only 1 tracks skipped", the base's 3 s worker deadline ending the park with its poll's, and the skip whose write then failed on that deadline counted, as `markSkipped` counts a failed stamp ("mark skipped … context deadline exceeded"), the same path giving `TestSkipReasonsKeysStayBounded` one key to check; and the stamp_changed_row helpers' assertions came at their callers' lines (171 and 175, 203 and 204), the helpers calling `t.Helper` |
| a-fix | `3d9d865b`'s tests and enricher, the stamp parked | all 20 green, each converted test 5.5 s or more, the seam test 11 s or more | as predicted: 6.34 to 6.44 s, the seam test 13.57 s |
| b | `noteOutcome` reports nothing | the 19 red at the bound, about 30 s each, "…: 0 of N tracks … when 30s had passed; the run reported []", never hanging; the seam test red in its first two subtests | as predicted: 30.87 to 30.93 s each, five lanes, the 20 m binary timeout never near |
| c | `enricherRunLifetime` 3 s, the stamp parked | the 19 red at about 3 s by Run's return: "… when the enricher's Run returned; the run reported []" | red, all 19, at 3.84 to 3.87 s, but by the other-outcome branch: the run's 3 s deadline ended the park, the stamp's write failed with `context.DeadlineExceeded`, which `ctxerr.WithoutCancellation` keeps (it drops `context.Canceled` alone), and the seam heard `failed` before Run returned |
| c2 | `enricherRunLifetime` 3 s, each enricher's first pass held at `enrichOne`'s entry 5.5 s or until its context ends, then abandoned there | the 19 red at about 3 s by Run's return | as predicted: 3.83 to 3.87 s, each "… when the enricher's Run returned; the run reported []" |
| d | `TestEnricherFetchesAndCachesArtistImage` waits for `OutcomeSkipped` | red at once: "Artist/Album/01.flac was done, where 1 skipped were awaited" | as predicted, 0.92 s |
| e | `stampEnriched` tells the seam `OutcomeDone` before its write | the seam test red in all three subtests ("done pending", an extra "done unread", "done pending" from the stopped stamp) | as predicted |
| f | `markSkipped` keeps `OutcomeSkipped` for a skip whose stamp failed | the seam test red in "a stamp and a skip the store refuses" alone ("02.flac skipped unread") | as predicted |

### Runs

At `3d9d865b`:
- The 30 tests the change reaches (the 19 converted, the seam test, and the 10 that call
  `waitFor`), `-race -count=50`, as five concurrent lanes of two CPUs each at GOMAXPROCS 2: 1,500
  of 1,500 top-level passes, no data race, the lanes 298 to 384 s.
- Under item 1's load, in the build slot: the 30 beside 7 hogs on one CPU at GOMAXPROCS 1,
  `-count=2`: 60 of 60, no data race, 440 s; the two tests CI failed beside 31 hogs, `-count=3`: 6
  of 6, 24.9 to 26.0 s each, nearly all of it setup.
- `GOOS=windows` and `GOOS=darwin` `go vet -tags ts_omit_webclient,ts_omit_debugeventbus
  ./internal/enrich/`: exit 0, nothing said.
- `internal/enrich` whole, once: ok, 96.4 s. cmd/bridge's doc guards and goroutine-drain guard
  (`TestNoDocblockNamesAnotherDeclaration`, `TestEveryCitedTestNameExists`, `TestNoBlankKeepers`,
  `TestEveryBackgroundGoroutineDrainsOnCleanup`): pass.

A record holds no run made on its own commit: `make fmt vet docs-check` and `make fmt vet test
build-all` on this record's commit are in its PR's body.

### What stays open

- Windows: the proof is the PR's `test (windows-latest)` job; this host runs Linux alone.
- `runUntilReturn`'s and `runEnricher`'s 10 s bounds on Run's return after a cancel bound a stop,
  not progress, and are left as they were.
- `waitFor` keeps its poll under the new bound: loop turns, log lines, rows and files have no
  outcome to be heard.
