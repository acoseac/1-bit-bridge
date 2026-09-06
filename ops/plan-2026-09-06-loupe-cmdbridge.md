# LOUPE — `cmd/bridge` surface (2026-09-06)

## Phase 0 — scope

**Shape: SURFACE**, not a window. One Go package: `cmd/bridge`.

| | |
|---|---|
| Production files | 52 (`19,375` LOC) — `main.go` alone is 4,698 |
| Test files | 42 (`9,781` LOC) |
| Commits touching it, last 90 days | 126 (261 all-time) |
| `go vet ./cmd/bridge/` | clean at HEAD `b60ee9b` |

### The structural check — and its answer

Per the LOUPE doc's bridge-side structural question: **does the surface have a
`### ` section under `## Things that have bitten before`?**

**It does not.** Eleven subsystem sections exist; none is `cmd/bridge`. The
package's invariants are distributed by *subject* into other sections — the
`bgWriters` join lives under "Scanner", `SetPostScanHook` under "Job pools", the
cadence-rearm rule under "Config, settings and process lifecycle" — which is
defensible, but it means **the largest single package in the repo has no home
for a rule that is about the wiring itself**, and no reviewer arriving at
`cmd/bridge` is pointed at anything.

The prior-art check agrees: the four `ops/audit-*.md` files mention `cmd/bridge`
20/12/2/6 times against a 19k-line surface, and `ops/engineering-log.md` cites
only 10 distinct files in it.

**Prediction from that**: an unswept surface should refute *fewer* findings than
the ~70% baseline for hardened ground. **Borne out** — see the triage table.

---

## Phase 1 — two directions

1. **PRISM over the API.** 20 batches, 19,667 review lines, slabbed by concern
   (`~/dev/gemini-review-bridge/batches-cmdbridge-loupe`), relayed through the
   **Go** primer + blueprint at `~/dev/gemini-review-bridge/`, `gemini-3.8-flash`.
2. **Five targeted read-only agents**, one per concern group, each with
   CLAUDE.md in hand.

**Harness change made during the run**: `relay.py` gained `--primer-dir`. It
hardcoded `~/dev/gemini-review` for the preamble+blueprint, which is the
**Swift/iOS** primer — every prior bridge-side relay would have reviewed Go
against iOS false-positive rules. Default unchanged; the bridge run passes
`--primer-dir ~/dev/gemini-review-bridge`.

---

## Phase 2 — triage

Every finding below was verified against the source before adoption. Where a
claim was settled by **measurement**, the measurement is named.

### SHIPPING

#### F1 [HIGH] The auto-analysis sweeper has no `analysis.enabled` gate
`cmd/bridge/analyze.go:352` (`runAnalysisSweeper`), wired `main.go:3033`.

**Mechanism.** `runAnalysisSweeper`'s signature carries no `enabled func() bool`
and its `sweep` closure runs `collectAnalysisCandidates` → `pool.Enqueue` with no
gate. Its three siblings all have one (`fingerprint_sweeper.go:146`,
`auto_optimize.go`, `smartplaylists.go:20`).

**Measured.** `git show a25c8ba` (PR #781, "always construct, never stop")
converts `if analysisActive {` → `{`. That `if` **was** the sweeper's gate. The
same commit converted every *read* consumer to the live `analysisActiveFn`
(`WithAnalysis`, `analysisStatsAdapter.enabled`, `Deps.AnalysisActive`) — the
*write* path got nothing. `analyze.Pool.Enqueue` (`internal/analyze/pool.go:160`)
gates only on `p.closed`. And `Store.UpsertAnalysis` ends in
`bumpIndexedAtByPathSQL` (`internal/manifest/store.go:8348`) whenever a visible
field changed.

**Trigger.** Any `bridge serve` on the **default** config (`analysis.enabled`
defaults false), 90 s after boot: walk every local track, fork a decode per
track, write waveform sidecars, and bump `indexed_at` per track — **a
whole-library delta to every paired device**, repeated every `scanIntervalSec`
and on every post-scan nudge. `/v1/analysis/*` 404s and `bridge analyze` refuses,
so nothing surfaces it.

**Fix.** Add `enabled func() bool`; return early from `sweep` when false, in
`runFingerprintSweeper`'s exact shape (including its "a disabled pass records NO
status" rule). Pass `analysisActiveFn`.

#### F2 [HIGH] `POST /v1/upscale` and `DELETE /v1/upscale/variants` lost their feature gate
`internal/api/upscale.go:162`, `internal/api/upscale_delete.go:183`. **Root cause
is the `cmd/bridge` wiring**: `main.go:3126` `{` + `main.go:3183`
`WithUpscaleEnqueuer`.

**Mechanism.** Both handlers gate on a nil adapter, whose comment still says nil
means "Feature off (config flag false) OR sox precheck failed at startup". The
same #781 conversion made the wiring unconditional, so the adapter is **never
nil** and the gate is unreachable. Neither handler calls `s.upscaleActive()` —
which exists (`internal/api/api.go:677`) and *is* consulted by `/v1/health` and
the manifest variant gate.

**Trigger.** `upscale.enabled` defaults **false**. On a stock bridge any holder
of a valid bearer token can `POST /v1/upscale` and run real sox jobs writing
`track_variants` rows and FLAC sidecars, while `/v1/health` reports
`upscaleEnabled: false`. The per-track POST path has no disk floor — that guard
lives only in the sweeper and `Coordinator.Submit`.

**Wire check.** No shape change: `errCodeUpscaleDisabled` already exists and is
already the documented answer. **No `ProtocolVersion` bump, no Mirror-PR.**

#### F3 [HIGH] Six operator commands cannot find the platform config
`cmd/bridge/token.go:71` (`openTokenStoreFromCfg`), `cmd/bridge/upscale.go:86`
(`bootstrapTranscodeCmd`).

**Measured**, with a built binary from an empty directory:

```
$ bridge token list
config load failed: read config "": open : no such file or directory   (exit 2)
$ bridge status                      # the loadCLIConfig control
status: probe 127.0.0.1:7789: ...    # resolved the platform config, works
```

16 files in the package use `loadCLIConfig`; these two are the holdouts still
calling `config.Load(configPath)` with the flag's empty default. That covers
`bridge token {list,rotate,expire,revoke}`, `bridge upscale`, `bridge optimize`.
`bridge token revoke` is the documented orphaned-token recovery path.

Five flag-help strings say `(default: ./bridge.yaml, else the platform config
dir)`. **No such fallback exists on these paths** — the help text is false.

#### F4 [HIGH] `runArtworkGC` has no empty-referenced-set guard
`cmd/bridge/artwork.go:141`.

`known` is built from `ArtworkMBIDsInUse`; every file whose stem misses it is
unlinked at :190. An empty set means "everything is an orphan" — including every
`local-<sha256>-500.jpg` the scanner extracted from embedded art, which the mtime
skip gate stops it from regenerating, so the loss is permanent short of an
`ExtractorVersion` bump or a row wipe.

**The file's own docblock (:121-127) records this exact bug shipping once** via a
wrong DB path. The fix corrected the path and never added the structural guard.
CLAUDE.md:878 claims "An empty universe is a deliberate no-op everywhere it
appears" — `DeleteBookletsNotIn` honours it; this does not.

#### F5 [HIGH] `LiveHost` hands a routing key to a raw-UDN cache lookup
`cmd/bridge/upnp_upstream_wiring.go:344`.

**Measured by reading both key writers.** `upnp_track_routing.server_udn` is
stamped with `StableServerKey` = `strings.ToLower(UDN)`
(`internal/upnpingest/ingest.go:354,869`). `ServerCache` is keyed on `info.UDN`
**verbatim** — `Upsert` (`internal/upnp/discovery.go:84`) and `Get` (:120) are
exact map operations and nothing folds case anywhere on that path. `LiveHost`
does one exact `Get`.

So for any upstream whose advertised UDN contains an uppercase character, the
walk succeeds and routing rows land — and then **every byte fetch 503s
`upnp_server_offline`**, across all three proxy surfaces (`/v1/download`,
`/dlna/file/{trackID}`, the web player).

This is CLAUDE.md's own documented class ("one lookup cannot serve both"),
written as though settled. `internal/admin/handlers_player.go:229` fixed the
*badge*; the byte path was never fixed. No test touches `LiveHost`.

#### F6 [DOC/HIGH] CLAUDE.md's cross-cutting list instructs a destructive action its own hardened section forbids

`CLAUDE.md:80` — *"The admin handler calls `store.WipeAllTracks()` before the new
scan"*. `CLAUDE.md:259` — *"A single↔multi root flip calls
`WipeFilesystemTracks`, **never** `WipeAllTracks` — the latter CASCADE-deletes
`upnp_track_routing`, destroying an entire upstream library."*

**Measured.** Every production caller is `WipeFilesystemTracks`
(`internal/admin/handlers_api.go:1583,1711`, `cmd/bridge/library.go:165,306`).
`WipeAllTracks` has **zero** production callers — only two tests. A session
following line 80 would compile, pass the suite, and destroy every routed
upstream library on a root-count toggle.

Two siblings in the same list: **:76** names 2 of the 5 sanctioned `enriched_at`
resets (all five exist and two of the omitted are live callers); **:77** states
"loopback-only, no auth" flatly, omitting the public-mode credentialed posture
that `bridge.ars.md` actually runs (`internal/admin/middleware_auth.go`). And the
architecture table (:48) lists 5 subcommands; `run()` dispatches **28**.

#### F7 [MEDIUM] The uninstall prompt tells operators the bridge cannot delete their music
`cmd/bridge/menu.go:544` and its docblock at :507.

> `• your music library — bridge has no code path that`
> `  can delete --library files (read-only by design)`

False since the web-upload/trash batch (#788–#794). `main.go:3725` says, of the
trash manager, "the only thing in the bridge that removes library content", and
`trashMgr.RunSweeper` unlinks under a library root. The docblock notes this
reassurance is printed "because at least one user reached out asking exactly
this" — so it is load-bearing operator-facing text, and it is now wrong.

#### F8 [LOW] Dead import-keepalives — **measured**
Removing `var _ = json.Marshal` + the `encoding/json` import + `var _ = os.Stdin`
from `doctor.go`, and `var _ = time.Time{}` from `upnp_upstream_admin.go`, builds
clean except that `"time"` then reports *imported and not used* — proving that
import exists only for the keepalive. Three dead vars, two dead imports.

### ACCEPTED, DEFERRED to a follow-up batch (recorded so the next run does not re-derive them)

- **F9 [MEDIUM] `runSmartPlaylistRegenerator` freezes `analysisEnabled` at boot.**
  `main.go:3101` passes `analysisActiveFn()` — the *value*. The `enabled`
  parameter beside it is a closure. Found independently by two agents.
  `handlers_smartplaylists.go:61` claims it "matches the scheduled ticker"; it
  does not. Defer reason: touches `main.go`, which F1 also touches.
- **F10 [MEDIUM] The UPnP ingest loop is a third `scanIntervalSec` consumer**
  with a fixed `time.NewTicker` on a boot snapshot and no `cadenceRearms` entry
  (`upnp_upstream_wiring.go:178,246`) — both halves of the cadence rule, while
  the settings matrix classes the field `live`.
- **F11 [MEDIUM] Four un-joined background writers.**
  `integrity.VariantWatcher` (`main.go:3306`) writes the store via
  `DeleteVariant` and passes `context.Background()`; `OrphanSidecarSweeper`
  (:3328), `runArtworkCacheSweeper` (:2408) and `upd.Run` (:2595) are also
  un-joined. Falsifies CLAUDE.md's "Shutdown joins every background writer".
- **F12 [MEDIUM] `bridge restore` and `bridge manifest clear-missing` have no
  liveness gate**, justified by a docblock (`backup.go:89`) claiming "no PID file
  today" — false since `pidfile.go` landed and `doctor.go:249` already reads it.
- **F13 [MEDIUM] Offline `bridge enrichment retry` never clears AcoustID
  suppression** (`enrichment.go:383`), unlike both admin paths it claims parity
  with — so it silently excludes the population fingerprinting exists to rescue.
- **F14 [MEDIUM] `cert rotate` / `restore` prompts go to stdout** while
  `update.go:177` asserts, as a repo convention, that both send them to stderr.
- **F15 [LOW] `writeJSONIndent` writes its encode error into the JSON stream**
  (`status.go:172`), used by `bridge doctor --json` and `bridge cert info --json`
  — undercutting the stdout-purity invariant `doctor.go:56` protects.
- **F16 [LOW] No dispatcher ⇄ `usage()` parity test.** Currently consistent (28/28
  verified), but nothing pins it.

### REJECTED / REFUTED — do not re-raise

- **`artwork.cacheMaxBytes`'s conditional spawn is a hot-apply violation.**
  Refuted: the field is **not** in the admin PATCH allowlist (only read at
  `handlers_jobs.go:270`), so both halves are boot-read and CLAUDE.md's
  "never split a config field's halves" is satisfied. The reported
  `ArtworkCacheLRU` flag mirrors the spawn condition exactly.
- **`menu.go`'s `os.RemoveAll(filepath.Dir(s.cfgPath))` can wipe `$HOME`.**
  Refuted: the menu's `cfgPath` comes from `packaging.IsInitialized()`, which
  always returns the platform config dir — never `resolveConfigPath`'s
  cwd-relative `./bridge.yaml`.
- **`colorState` is unsynchronised package state across `runServe` re-entry.**
  Refuted: `sync.Once`, and every input is process-lifetime constant.
- **`retention_sweeper`'s fixed `time.NewTicker` violates the cadence rule.**
  Refuted: the 24 h cadence is a const and deliberately not settings-driven; the
  *policy* days are read live per pass (`retention_sweeper.go:56`).
- **`splitHostPort` rejecting port 0 is a defect.** Accepted as real but NOT
  shipped: reachable only via `listenAddress: ":0"`, which is a test-fixture
  mode no production bridge uses (it publishes no stable port for iOS), and the
  proposed contract change touches several callers for no operator-visible gain.
- **The `WipeAllTracks` AST guard.** Considered — `WipeAllTracks` is
  production-unwired and the repo has the exact precedent
  (`TestPrunePlaylistCoversExceptStaysUnwired`). **Deferred, not rejected**: the
  doc fix (F6) removes the instruction that would cause the call; the guard is
  worth adding in the follow-up batch.

---

## Phase 3 — PR breakdown

`main.go` is touched by several items, so these are **sequential off `main`**,
not a stack. One PR per script run.

| PR | Theme | Files | Test |
|---|---|---|---|
| 1 | F1 + F2 — the two gates #781 removed | `cmd/bridge/analyze.go`, `cmd/bridge/main.go`, `internal/api/upscale.go`, `internal/api/upscale_delete.go` | disabled sweeper enqueues nothing; disabled `POST`/`DELETE` answer 503 |
| 2 | F3 — CLI config resolution + the false help text | `cmd/bridge/token.go`, `cmd/bridge/upscale.go` | both tails resolve the platform config with no `--config` |
| 3 | F4 + F5 — fail closed | `cmd/bridge/artwork.go`, `cmd/bridge/upnp_upstream_wiring.go` | empty referenced set removes nothing; `LiveHost` resolves an uppercase UDN |
| 4 | F6 + F7 + F8 — docs and dead code | `CLAUDE.md`, `cmd/bridge/menu.go`, `cmd/bridge/doctor.go`, `cmd/bridge/upnp_upstream_admin.go` | n/a (F8 is proven by the build) |

**Wire/Mirror-PR check**: F2 touches `internal/api` but changes no shape — the
error code and message already exist and are already documented. No
`ProtocolVersion` movement. **No iOS Mirror-PR needed in this batch.**

---

## Phase 4 — plan review, and the dispositions

Reviewed the plan adversarially and consulted Gemini 3.8 over the API on the one
decision with real blast radius: **F2 turns an endpoint that currently succeeds
into one that answers 503.**

### Accepted as written
F1, F3, F4, F5, F6, F7, F8.

### Corrected before adoption — F2

The consult raised two things worth acting on and one worth checking:

1. **"Clients respect the advertised capability" is an assumption, not a fact —
   verify it.** So I did, in the coupled iOS repo:
   `DownloadCoordinator.swift:1607` states the dispatch precondition as
   `share.bridgeUpscaleEnabled == true`, and `BridgePairingPersistence.swift:158`
   sets that field from `health.upscaleEnabled`. **The shipped client does gate
   its `POST /v1/upscale` on the flag**, so restoring the gate cannot break it;
   the only callers that newly see a 503 are ones already ignoring the
   advertised capability, which is the population the gate exists for.
   Assumption verified rather than relied on.
2. **Log the refusal at Warn.** Adopted. An operator whose deployment was
   silently relying on the ungated path gets a reason in the journal instead of
   a bare 503. Preferred over the consult's "option 2" (allow-but-warn for one
   release), which would keep an unbounded-CPU-and-disk mutation path open on a
   default-off feature — the thing being fixed.
3. **"Gating the whole handler on `upscaleActive()` also shuts down CarPlay
   `kind: optimize`"** — checked and **does not apply**: `carPlayOptimizeEnabled`
   is already AND-gated on upscale by the wiring
   (`main.go:2706`, and `internal/api/api.go:134` documents it as such), so
   optimize is a strict subset of upscale-active and cannot be over-refused. The
   optimize arm still gets its own `carPlayOptimizeEnabled()` check, which it
   also lacked.

### Rejected
The consult's **option 3** (make `/v1/health` report the feature ON to match the
wiring, rather than gating the wiring to match health). Rejected for the reason
it gave and one more: `upscale.enabled` defaults false, the per-request path has
no disk floor, and flipping the read side would turn a default-off feature on for
every deployment in the field.

---

## Phase 5–7 — what shipped

| PR | Theme | State |
|---|---|---|
| [#852](https://github.com/acoseac/1-bit-bridge/pull/852) | F1 + F2 — the two gates #781 removed | 3 review rounds |
| [#853](https://github.com/acoseac/1-bit-bridge/pull/853) | F3 — CLI config resolution + a package-wide guard | 1 round |
| [#854](https://github.com/acoseac/1-bit-bridge/pull/854) | F4 + F5 — fail closed in the artwork GC and `LiveHost` | 1 round |
| [#855](https://github.com/acoseac/1-bit-bridge/pull/855) | F7 + F8 — the stale operator promise, dead keepalives | 1 round |

Docs (F6 + the new `### The CLI and the serve wiring` section + the engineering
log record) went direct to `main` per the repo's docs-only convention.

### Bot coverage — stated rather than implied

**CodeRabbit reviewed #852 only.** #853 / #854 / #855 received Gemini and
SonarCloud but no CodeRabbit pass, which is the rolling-budget shape
`docs/LoupeReviewCycle.md` warns about for a multi-PR run. Those three are
Gemini + SonarCloud + CodeQL reviewed, not four-bot reviewed. Re-request the day
after if a second opinion on them is wanted.

### Findings taken from review

- **CodeRabbit, #852** — the disabled-gate test used fixed sleeps, so a loaded
  runner could reach the assertion before either trigger fired and pass against
  code that never consulted the gate. Now synchronises on the predicate actually
  being called. The sharpest comment of the run.
- **CodeRabbit, #852** — the optimize gate sat after `ResolveChecked` and the
  recursive walk, so a refused request still paid for a full `WalkDir`. Moved
  ahead of both; `TestUpscaleGateRunsBeforePathResolution` pins the ORDER via a
  nonexistent path (gate-first answers 503, resolve-first 404 — both "refused",
  which is why the other tests could not have caught it).
- **SonarCloud, #852** — 10 params / cognitive complexity 20, then 19. Fixed
  properly rather than appeased: dependencies became an `analysisSweeper` struct
  and the pass became `(*analysisSweeper).sweep`, which is the shape
  `fingerprintSweeper` already had. Then `new_duplicated_lines_density` at 10%
  → one parameterised `gateFixture`.
- **Gemini, #852** — the nil predicate contradicted its own docblock. Taken as
  **fail closed**, not as dropping the check.
- **Gemini, #853** — the `config.Load` guard stripped comments but kept string
  contents, so an error message containing the call spelling would cry wolf.
- **Gemini, #855** — drive `actUninstall` instead of scanning `menu.go`. Strictly
  better, and it buys an assertion a text scan cannot make: the config file is
  still on disk afterwards.

### Declined, with evidence

- **Gemini, #852 — move the test goroutine's join into `t.Cleanup`** to avoid a
  `t.TempDir()` race. It cannot race: the body already cancels and joins with a
  5 s bound *before returning*, and `t.Cleanup` runs after the body.
- **Gemini, #854 — memoise the folded UDN lookup in a package-level `sync.Map`.**
  Benchmarked: 297.9 ns / 304 B at one upstream, 410.6 ns / 912 B at five,
  994.6 ns / 3216 B at twenty — sub-microsecond once per request on a path that
  proxies multi-megabyte FLACs. Against that, a package-level memo outlives
  `runServe`'s in-process re-entry from the launcher menu and has no
  invalidation path when a cache entry ages out.

### Two process failures worth recording

1. **A negative control ran before the commit**, and its `git checkout --`
   restore took the fix with the mutation. `docs/LoupeReviewCycle.md` says commit
   first, in bold, for exactly this. It is easy to skip when the edit feels small.
2. **A source-scanning guard fired on its own docblock — twice.** Both the
   `config.Load` sweep and the uninstall-prompt guard. The eventual answers
   differ and both are worth knowing: strip comments *and* string-literal
   contents when a scan is genuinely the right tool, and prefer driving the real
   function when it isn't.

## Still open — a recorded follow-up batch, not a silent gap

F9–F16 in the triage above are confirmed and unshipped, deferred because they
overlap `main.go` with F1 or are below the bar for this batch's blast radius.
The highest-value three:

- **F9** `runSmartPlaylistRegenerator` freezes `analysisEnabled` at boot while
  the admin handler reads it live, so the same cache flips content depending on
  which trigger last ran. Found independently by two reviewers.
- **F10** the UPnP ingest loop is a third `scanIntervalSec` consumer with a fixed
  ticker and no rearm — both halves of the cadence rule, while the settings
  matrix classes the field `live`.
- **F11** four un-joined background writers (`integrity.VariantWatcher` passes
  `context.Background()` and writes the store), which falsifies CLAUDE.md's
  "Shutdown joins every background writer".

Plus, from the PRISM pass and not yet triaged in depth: `bridge enrichment retry`
accepting a positional path that `fs.Parse` silently drops, turning an intended
subtree reset into a whole-library one; `variants move --dry-run` calling
`os.MkdirAll`; and `duplicatesCmd` ignoring `--tier` / `--nested-only` under
`--json`.
