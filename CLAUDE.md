# 1-bit-bridge

Cross-platform Go companion server for the [1-bit](https://apps.apple.com/us/app/1-bit/id6762529497) iOS music player. Replaces SMB as the transport over high-latency links (Tailscale / DERP relay) — HTTP/2 + bearer-token auth, TLS fingerprint pinning, pre-built library manifest that skips the iOS scanner's two-phase walk. Target platforms: macOS / Linux / Windows × amd64 / arm64. Tested primarily on macOS.

## Build

- `make build` — builds `./bin/bridge` for the host OS.
- `make build-all` — cross-compiles to `dist/bridge-<os>-<arch>{.exe}`.
- `make test` — pure-Go race-enabled suite; ~150 tests across 10 packages.
- **Fuzz targets are fuzzed NIGHTLY in CI, and are NOT fuzzed by `make test`.**
  `.github/workflows/fuzz.yml` fans every target out as a parallel matrix job at
  `-fuzztime 5m -fuzzminimizetime 1s`; the target list is DISCOVERED from the tree via
  `go test -list 'Fuzz.*' ./...`, so adding a target needs no workflow edit. Fan-out, not
  rotation: one-target-per-night by day-of-year would give each target five minutes a
  MONTH. A crasher fails that matrix leg and uploads `testdata/fuzz/**` as an artifact —
  deliberately not auto-committed, since a corpus commit from CI is noise while a crasher
  deserves a human-reviewed PR. Locally, `make test` still runs seed corpora only. **39** targets across **ten** packages —
  `internal/{manifest,fs,dlna,dlna/discovery,upnp,enrich,dupes,lyrics,upload,atlasharvest}` —
  and `atlasharvest`'s `FuzzMatchRelease` lives in
  `lyrics_test.go` rather than a `fuzz_*_test.go` file, so a census that
  greps only the latter undercounts. (This entry said 37 across nine until
  2026-09-10: the sixth stale claim of the kind, and in the paragraph that
  warns about them; 38 until 2026-09-12, when `internal/lyrics` gained
  `FuzzTextCandidateClassification`.) They cover
  the five untrusted-input surfaces: the audio extractors (whole-file + the pure
  chunk-body parsers + the SACD ISO reader), the LAN-facing UNAUTHENTICATED parsers (SSDP /
  SOAP / DIDL / device description), `fs.Resolver`, the web-upload path validation
  (`internal/upload`, which this list omitted until 2026-09-09), and the Atlas
  release matcher (`internal/atlasharvest`).
  **Count them by file:name pair** to get 39 targets. A function-name-only
  census (`grep -h '^func Fuzz' | sort -u`) reports 38, because
  `FuzzNormalize` exists in both `internal/dupes` and `internal/lyrics`. Without `-fuzz` they run their seed
  corpora as ordinary tests, so the normal suite absorbs them for free. To actually fuzz:
  `go test ./internal/fs/ -run XXX -fuzz FuzzResolveContainment -fuzztime 60s -fuzzminimizetime 1s`
  (one target at a time — Go permits only one `-fuzz` per invocation).
  **`-fuzzminimizetime` is not optional on a target that keeps finding new coverage.** It
  defaults to 60s, minimization burns CPU *without incrementing the reported `execs`*, and the
  run still says `PASS` — so the failure mode is a target that looks like it ran and did not.
  Measured on `FuzzFoldForMatch`: `-fuzztime 60s` alone executes **19,003** inputs and then
  sits at 0/sec for 43 seconds; adding `-fuzzminimizetime 1s` executes **1,302,362** in half
  the wall clock. Four carry PROPERTY assertions worth keeping green rather than merely
  not-crashing: `FuzzResolveContainment` (a successful `Resolve` must land inside a root —
  asymmetric, so only a real escape fails it), `FuzzFoldForMatch` (the documented
  `foldNameNoArticle == stripLeadingArticle∘foldName` identity `pickBestArtist` depends on),
  `FuzzParseRetryAfter` (the `maxRetryAfter` cap), and `FuzzSACDVirtualPathRoundTrip` (a
  rendered virtual path must parse back to the same index and container — the renderer and the
  parser disagreeing is a row-reaping bug, since the deletion pass keys on
  `IsSACDVirtualPath`). **A crash found by the extractor
  targets is a REAL defect, not a nicety** — `runScanWorker`'s per-iteration `recover()` means
  a panicking file is skipped, so it silently never reaches the manifest. Baseline at
  introduction: ~41M executions total, zero panics, zero escapes.
- `make fmt vet test build-all` is the pre-push gate, now mirrored by CI (`.github/workflows/gofmt.yml` = the fmt check, `gate.yml` = vet + test + build-all). Run `make check` (fmt + vet + race test, skips build-all) in the inner loop; `make build-all` once before pushing. On a RAM-constrained box the `-race` + 6-target cross-compile peak can OOM — the Makefile caps Go's `-p` parallelism via `P` (default 4; `make test P=2` to go lower, `P=$(sysctl -n hw.ncpu)` for a roomy box). See `CONTRIBUTING.md`.
- Pure-Go stack: `modernc.org/sqlite` (no cgo), `github.com/mewkiz/flac`, `github.com/dhowden/tag`, `github.com/hashicorp/mdns`. One static binary, no runtime deps.

## Architecture at a glance

| Package | Role |
|---|---|
| `cmd/bridge` | CLI: **30 subcommands** dispatched in `run()` — `init` / `serve` / `pair` / `scan` / `upscale` / `analyze` / `optimize` / `render` / `variants` / `artwork` / `enrichment` / `duplicates` / `fingerprint` / `doctor` / `update` / `backup` / `restore` / `token` / `cert` / `status` / `health` / `logs` / `library` / `admin` / `manifest` / `tsnet` / `start` / `stop` / `restart` / `version`. Bare `bridge` on a real TTY drops into a context-aware launcher menu (`menu.go`); pipes / non-TTY callers fall through to `usage + exit 2` so automation is unchanged. Box / frame / shell-aware handoff helpers in `styles.go`. |
| `internal/config` | YAML loader with defaults + path-relative resolution + `Save()` for admin edits |
| `internal/tls` | Self-signed ECDSA P-256 cert minter, SHA-256 fingerprint for iOS pinning |
| `internal/auth` | Bearer-token store (hashed, atomic persist, cross-process pickup) |
| `internal/fs` | Path-safe resolver (traversal-rejection, multi-root routing, hot-reload via SetRoots) |
| `internal/manifest` | SQLite library index + tag extractors (FLAC / DSF / MP3 / M4A) + JSON serializer; Scanner roots are hot-reloadable |
| `internal/enrich` | MusicBrainz + Cover Art Archive + Deezer clients, rate-limited (1.1s / 500ms / 120ms) |
| `internal/api` | HTTP/2 handlers: `/v1/{health,list,stat,read,download,manifest,artwork/{mbid},artist-image/{mbid}}` |
| `internal/admin` | Local web console on 127.0.0.1:7789 — library/devices/stats/settings pages + JSON API + bridge:// QR pair URL. Loopback-only, no auth. |
| `internal/packaging` | launchd plist + systemd unit templates + install/uninstall helpers, used by `bridge init` |
| `internal/mdns` | Bonjour `_onebit-bridge._tcp` advertisement |
| `internal/version` | `ServerVersion` + `ProtocolVersion` constants (source of truth for `PROTOCOL.md`) |

## iOS companion (coupled repo)

The iOS app **1-bit** lives at `github.com/acoseac/1-bit` with a local clone at `~/dev/com.acoseac.dsdplayer/`. It consumes this server via `BridgeSourceClient.swift` + `LibraryScanner.runBridgeSync` + the artwork prefetch. The wire protocol spec is co-committed as `PROTOCOL.md` here and `docs/BridgeProtocol.md` in the iOS repo — they must stay byte-identical (see `CONTRIBUTING.md` → **Mirror-PR rule**).

**When editing this repo, also check the iOS app:**

- **Wire-protocol change** (`/v1/*` response shape, `BridgeTrack` / `BridgeManifest` JSON, `X-Bridge-Protocol` header semantics, error-envelope codes): the iOS `BridgeSourceClient.swift` DTOs + `Tests/…/Fixtures/Bridge/manifest_basic.json` golden fixture MUST be updated in the same PR pair. Bump `internal/version.ProtocolVersion` + iOS's `BridgeSourceClient.supportedProtocolVersion` together for breaking changes; leave both unchanged for purely additive fields (add optional properties, don't rename existing ones).
- **Behavioural coupling**: auth flow, rate-limit assumptions, buffer-size tradeoffs, error semantics. A bug on one side often has a sibling on the other — pause before committing and ask whether the iOS decoder/scanner could hit the same class of issue.
- **Bug-fix mirroring examples**: the MB `release-group` decode bug (this repo) had no iOS twin — the iOS side just consumes the server's output. The iOS `resolveLibraryTrackID` path-normalization bug had no server twin — paths are normalized once, on the iOS side, from the server's raw output. Each case was 30 seconds of "does the other side need the same fix?" — worth asking every time.

**Don't regress these cross-cutting invariants:**

- **No server-side transcoding, ever.** 1-bit is bit-exact by mission. `/v1/download` serves the file as-is via `http.ServeContent`; never introduce a transcoding path. Renditions — the `upscaled-` / `optimized-` PCM families and, since PR #863, the DSD `optimized-dsd-` / `pcm-` tiers — are OFFLINE sidecar files built by the job pool and served through `serveVariant`, i.e. the same `http.ServeContent` over a file that already exists; the rule is about the serving path, and a rendition never substitutes for the bit-exact source when the client asked for the source.
- **Rate limits respect the services.** MB anon is 1 req/s (we pace at 1.1s); CAA is IA-infrastructure and polite at 500ms; Deezer is ~50 req/5s (we pace at 120ms). User-Agent identifies the app + GitHub URL per MB's TOS.
- **TLS fingerprint is captured once.** The iOS pin is set during pairing via first-contact; rotating the server cert requires re-pairing. Don't mint a new cert on every `serve` run — `LoadOrGenerate` is sticky by design.
- **`enriched_at` monotonicity.** Upsert resets to 0 on track change so the enricher re-runs; the enricher marks it to `time.Now().UnixNano()` on completion (success or skipped). The other sanctioned writers are a CLOSED SET of four — `ResetEnrichedMisses`, `ResetEnrichedByArtistMBIDs`, `ResetEnrichedMissesUnderPrefix` and `ResetEnrichedByPaths` (the first two behind POST /api/enrichment/retry since PR #495, scoped to enriched-but-incomplete rows so a full MB/CAA re-crawl is never triggered; the last is the fingerprint sweeper's explicit-path form). All four are live callers — this bullet listed only two until 2026-09-06, so an audit against it would have flagged two sanctioned writers as violations. Never touch it anywhere else — the query `WHERE enriched_at = 0` drives the worker.
- **Admin console is loopback-only, no auth — IN LOOPBACK MODE.** `config.validateLoopbackAddress` + `admin.loopbackOnly` middleware both enforce this. **Public mode is the separate, credentialed posture** (`internal/admin/middleware_auth.go`: session auth, persisted since PR #800), which is what `bridge.ars.md` actually runs; this bullet omitted that until 2026-09-06. Don't add an auth layer that bypasses the loopback constraint; don't expose admin behind Tailscale / reverse-proxy. Anyone on the host already owns the token store and the SQLite DB — auth on top would be theatre. For remote admin, SSH-tunnel the port.
- **Graceful shutdown triggers full cleanup.** The `POST /api/restart` admin handler MUST NOT call `os.Exit(0)` directly. It must invoke the same cancellation closure that handles `SIGINT/SIGTERM`. This ensures the `bgScans` WaitGroup is honored (preventing SQLite corruption), in-flight transcode jobs are cleaned up, and the `auth.Store` flushes its last-used-at debounce buffer. Wired in `cmd/bridge/main.go` via `admin.Deps.Restart`.
- **Dual-stack HTTP/2 and HTTP/3 API.** The bridge serves the v1 API over both TCP (HTTP/2) and UDP (HTTP/3). QUIC is enabled by default but can be disabled via `disableHttp3: true` or `BRIDGE_DISABLE_HTTP3=true`. LAN HTTP/3 uses on-disk certs with forced "h3" ALPN; Tailscale HTTP/3 uses `tsnet.LocalClient` to fetch Let's Encrypt certs dynamically. Graceful shutdown uses `.Shutdown(ctx)` with a 5s window to protect active media streams.
- **A recorded sidecar path is a claim, never proof the file is gone.** `sidecar_path` / `waveform_path` are absolute; after a host move every row reads ENOENT while the files sit at their canonical places. The three reapers ask `integrity.LocateSidecar` and ADOPT a relocated row; the forward sweeps' known sets carry the canonical spelling; a mass deletion while the tree still holds sidecars is refused. Full rule under **Job pools** below (2026-09-20).
- **Single ↔ multi-root storage form flips.** When the admin adds a second root or removes back down to one, track paths change from `Artist/Album/…` to `<basename>/Artist/Album/…`. The admin handler calls **`store.WipeFilesystemTracks()`** before the new scan so no stale rows survive — **never `WipeAllTracks`**, which CASCADE-deletes `upnp_track_routing` and destroys an entire upstream library on a mere root-count toggle. (This bullet said `WipeAllTracks` until 2026-09-06, contradicting the rule under **Scanner** below; no production path has ever called it.) Don't try to migrate in place — the rescan is cheap, enrichment is cached by MBID.

**Working the bridge**: `feat/<topic>` branches, PR to `main`, pre-push `make fmt vet test build-all`. **Working the iOS side**: same convention at `~/dev/com.acoseac.dsdplayer/`. Never push direct to `main` on either repo.

## Wire-type discipline

Types in [`internal/manifest/types.go`](internal/manifest/types.go) — `Track`, `Folder`, `Manifest`, `Variant`, `EnrichmentProgress` — ARE the wire contract. Their `json:` tags are versioned by `internal/version.ProtocolVersion`. Adding or renaming a tagged field requires a `ProtocolVersion` bump (or an `omitempty`-gated additive that pre-version-N iOS will ignore). This is intentional: the bridge serializes `manifest.Track` directly into the `/v1/manifest` stream and into the `Track.Variants` aggregation built by SQL `json_object` — those rows ARE the JSON payload iOS reads.

**The constraint that's load-bearing is "must not be encoded directly from an HTTP handler", not "must not have `json:` tags".** Some internal domain types (`auth.Token`, `pairing.Request`) carry tags for their own persistence reasons — the `tokens.json` flat-file store, future on-disk pairing journals. That's fine and intentional. The rule is one level higher: NEVER pass any of those types to `json.NewEncoder(w).Encode(x)` / `json.Marshal(x)` from an `internal/api/` handler. Always wrap in a DTO under `internal/api/` (e.g. `tokenRow` in `apiTokensList`, `UpscaleStats`, `HealthResponse`, `ScanState`, `Entry`, `StatResponse`, `ErrorResponse`) so a future schema change to the domain type — renaming a SQLite column, adding an internal-only field — can't silently leak onto the wire.

For the SQLite row structs in [`internal/manifest/store.go`](internal/manifest/store.go) (the row scan targets — distinct from the public `manifest.Track`), the stricter rule applies: those MUST NOT gain `json:` tags at all, because they have no persistence justification and the only reason to add tags would be handler convenience — exactly the leak vector this section guards against.

**Hidden-leak vectors to check during review:**

- **`json.RawMessage` in any handler return path** would pass bytes-shaped data through and bypass the type-tag discipline. None today. If a new use lands, name the wire field it serves and document the schema-stability contract — most realistic motivations (preserving a SQLite blob column verbatim) are better served by a wire DTO whose body field is `[]byte` or an explicit struct so the schema is committed to up front. The `marshalForStorage` shim in [`internal/manifest/store.go`](internal/manifest/store.go) is NOT this pattern — it's a producer of the persisted blob via `json.Marshal` of a `*Track` clone, not a `RawMessage` carrier.
- **`any` / `interface{}` in handler-side helper signatures** defeats compile-time wire-shape checking. `writeJSON(w http.ResponseWriter, status int, v any)` in `internal/api/api.go` is the canonical example — convenient, but the caller's responsibility to pass a wire DTO is enforced by code review, not the compiler. When writing a new handler that hands a fresh type to `writeJSON`, double-check that type's `json:` tags + field list intentionally form the wire contract.
- **SQL `json_object` / `json_group_array` aggregations** (used in `Track.Variants` via `variantsAggSQL` in [store.go](internal/manifest/store.go)) build wire JSON inside SQLite. The columns selected in the aggregation are wire fields and follow the same versioning rule as struct `json:` tags — adding a new column inside the `json_object(...)` call is an additive wire change.

Audited PR-by-PR; verified clean at the time of this section's introduction. Re-audit when introducing a new handler or a new SQLite column on a wire-aggregated table.

## Local test fixture

Point `--library` at any folder with a handful of tagged audio files and you've got a working test setup — FLAC / DSF / MP3 / M4A all work. A few dozen tracks across 4–5 artists covers the tag-extraction, enrichment, and playback paths without needing a NAS.

Short path (no service install, doesn't touch launchd):

```sh
make build >/dev/null
./bin/bridge init --yes --no-service \
  --dir /tmp/bridge-live \
  --library ~/Music/test-library \
  --name "Test Library"
./bin/bridge serve --config /tmp/bridge-live/bridge.yaml &
# Admin console: http://127.0.0.1:7789/ — pair from there, or keep using `bridge pair` for scripts.
```

The old hand-authored `bridge.yaml` recipe still works; keeping the init form because it also mints the TLS cert up-front and catches config typos before `serve` is up.

**Public-mode variant** — renders Sign out, the session gate and every public-only piece of chrome (used for the 2026-09-18 phone top-bar report, #934):

```sh
./bin/bridge init --yes --no-service --dir /tmp/bridge-pub --library ~/Music/test-library --name "Test Library" \
  --public --domain localhost --admin-tls-proxy --admin-address 127.0.0.1:7791 --listen-address 127.0.0.1:7790
printf 'upload:\n    enabled: true\ndisableHttp3: true\n' >> /tmp/bridge-pub/bridge.yaml   # uploads on = the space meter renders
echo "throwaway" | ./bin/bridge admin reset-password --config /tmp/bridge-pub/bridge.yaml --from-stdin
./bin/bridge serve --config /tmp/bridge-pub/bridge.yaml &
./bin/bridge admin login-link --config /tmp/bridge-pub/bridge.yaml --ttl 5m   # open http://localhost:7791<path>, press Continue — no password typed
```

Four things that each cost a round: `--admin-tls-proxy` is what lets the console serve plain HTTP (public mode otherwise demands `autocert.enabled`); the browser must use the HOST that `autocert.domain` names — `originMatchesPublicMode` compares every POST's Origin host to it, so `127.0.0.1` against `domain: localhost` fails the ticket redeem with a cross-origin refusal (loopback is a secure context, so the `Secure` session cookie itself works over plain http on either name); `mdns.enabled` must stay `false` — a blanket `sed` on `enabled:` flips it and public mode refuses to start; and the static files are `//go:embed`ded, so every CSS / JS / template change is a `make build` + restart, never a reload. A second bridge on other ports for a before/after comparison needs a different host (`127.0.0.1`, with `autocert.domain: 127.0.0.1`): cookies ignore ports, so two bridges on `localhost` overwrite each other's session.

Force re-enrichment if the DB is already populated from a prior run:

```sh
sqlite3 /tmp/bridge-live/data/bridge.db "UPDATE tracks SET enriched_at = 0;"
```

**`enriched_at = 0` is NOT a tag-re-extraction reset.** It only triggers the MusicBrainz / CoverArt / Deezer enricher (the `WHERE enriched_at = 0` worker query at `internal/manifest/store.go:477`). It does NOT cause the scanner to re-read file tags — the scanner's skip gate at `internal/manifest/scanner.go:511` compares the file's on-disk mtime against `Track.ModTime`, which is stored INSIDE the `tags_json` BLOB column (read back via `GetTrack` from `tags_json` alone, NOT the standalone `mtime_ns` column). A `UPDATE tracks SET mtime_ns = 0` looks like it should work but doesn't, because `GetTrack` never reads that column. To force tag re-extraction after an `internal/manifest/extractors.go` change (e.g. the PR #208 multi-value Vorbis fix) without touching real file mtimes you must wipe the affected `tracks` rows so the next scan re-inserts them from scratch.

**The old `sqlite3 … "DELETE FROM tracks;"` form here is BROKEN since migration v4** (corrected 2026-06-23). v4 added the expression index `tracks(unicode_lower(path))` (+ a `track_variants` twin), and `unicode_lower` is a Go-registered scalar (`internal/manifest/sqlfunc.go` `init()`), so an external `sqlite3` CLI `DELETE`/`UPDATE`/`INSERT` on those tables fails at prepare with `unknown function: unicode_lower()`. Dropping the index doesn't help — it's created only by the version-gated migration, so a restart won't recreate it. Two working paths:

- **Disposable local fixture** — simplest is to delete the DB file and let the startup scan rebuild (loses pairings/tokens; re-pair after):
  ```sh
  kill -TERM $(pgrep -f "bin/bridge serve --config /tmp/bridge-live")   # graceful
  rm -f /tmp/bridge-live/data/bridge.db*
  ./bin/bridge serve --config /tmp/bridge-live/bridge.yaml &            # RunPeriodic's startup scan re-extracts every file
  ```
- **Selective / pairing-preserving / production** — use a throwaway Go helper that blank-imports `internal/manifest` (its `init` registers `unicode_lower`) + `modernc.org/sqlite`, opens the DB with `file:<path>?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)`, and runs `DELETE FROM tracks WHERE <predicate>` (e.g. `json_extract(tags_json,'$.sampleRate') IS NULL` — only the rows a new extractor now fills, so only those re-enrich; a full `DELETE FROM tracks` resets `enriched_at` on every row → the MB/CAA/Deezer treadmill). Put it in a `_`-prefixed dir (ignored by `go ./...`/`build-all`), run with the bridge stopped, then restart to trigger the scan. On a loopback bridge `curl -s -X POST http://127.0.0.1:7789/api/scan` also triggers a scan; public bridges (admin auth-walled) need a restart.

**WAL read trap**: an external `sqlite3` `SELECT COUNT(*)` against a LIVE bridge DB can read stale main-DB state for minutes — it won't see the bridge's un-checkpointed WAL writes. Verify a backfill landed AFTER a restart/checkpoint (or via the bridge), not by polling the CLI mid-scan. Full procedure + verified helper shape in the `bridge-track-reextract-gotchas` memory. The wipe still survives the TLS fingerprint + tokens (different tables); iOS pairing stays valid.

## Production deployments

**See `ops/deployment-runbook.md`** — full runbooks for the live bridges: **home-pc** (Windows, SSH `<HOMEPC-SSH>`), **bridge.ars.md** (Linux VPS, public mode) and the **public demo** `bridge.1-bit.app`, which since 2026-09-16 is a SEPARATE UNIT ON that same host (`1-bit-bridge-demo`, user `onebit-demo`, binary `/usr/local/bin/bridge-demo`) rather than its own VM — so a demo deploy MUST carry `SVC`/`REMOTE_BIN` or it restarts the operator's bridge. **One SSH ControlMaster per host, never a burst**: the flood protection is on the operator's own path, so a burst produces a multi-minute timeout that reads as an outage (see the runbook's SSH note). Atlas, which the bridges enrich against, is on its own VM (`atlas.ars.md`). **Every `<PLACEHOLDER>` here and in the runbook resolves in `ops/coordinates.local.md`, which is gitignored** — this file is tracked in a PUBLIC repo, so a real SSH target or IP must not be written into it (`.gitignore`: "never commit real WAN/LAN IPs, SSH targets, or key paths to this public repo"); canonical fleet coordinates live in the private conductor repo's `ops/hosts.md`. Read it before any deploy; covers scheduled-task names, update procedure, TLS/Tailscale gotchas, and the macOS sandboxed-CLI cert gotcha.

**`docs/` is a PUBLIC WEBSITE — internal operator docs go in `ops/`.** GitHub Pages serves `main:/docs` at `acoseac.github.io/1-bit-bridge/` and the repo is public, so anything under `docs/` is on the open web (indexable, scrapeable, no login). The deployment runbook and the codebase audits were published there until 2026-07-20 — the runbook exposing live SSH coordinates, the router port-forward endpoint, and the ufw posture; the audits amounting to an exploit index for unreleased fixes. They now live in `ops/` (see `ops/README.md`). **Don't move them back, and don't add a new doc naming a live host / IP / port-forward / key path — or enumerating unfixed weaknesses — under `docs/`.** Deploy scripts in `deploy/` are not Pages-served but ARE public: keep host coordinates in env vars with placeholder defaults (`BRIDGE_WAN_URL`, `HOMEPC`), never hardcoded. Moving a file out of `docs/` stops future serving but does NOT purge git history or caches — a published secret still needs rotating.

## Agent memory — write it to the repo, not (only) to private memory

The user works across **multiple Claude accounts and machines**. Per-account private memory (`~/.claude/projects/<project>/memory/`) does **not** travel between them, so anything durable that lives only there is invisible to the next agent — including a session launched directly from `~/dev/1-bit-bridge` rather than from the iOS repo. **The repo is the shared brain; private memory is the personal notebook.** The same policy is mirrored in the iOS repo's `CLAUDE.md`.

**The repo is the default destination.** Before saving a memory, ask: *would a fresh agent on another account need this?* If yes:

| What | Where |
|---|---|
| Invariants, "don't undo this", rejected-and-why decisions | `CLAUDE.md` — the matching subsystem group under "## Things that have bitten before" below |
| The record behind an invariant — the measurement, the rejected alternative, the test, the PR | **`ops/engineering-log.md`** — appended in date order. **Not auto-loaded**, so the *rule* must still land in `CLAUDE.md` |
| Wire-protocol / cross-repo contracts | `PROTOCOL.md` (repo root — **not** `docs/`) + the iOS repo's mirror entry, bumped in lockstep |
| Deploy procedures, journal diagnostics, host-specific ops | **`ops/deployment-runbook.md`** — `## Production deployments` above already tells every agent to read it before a deploy, so it reaches sessions that never open `CLAUDE.md` |
| Build/test commands and their gotchas | `## Build` above |

**`ops/`, never `docs/`, for anything operational** — see the warning in `## Production deployments`: `docs/` is served as a public website from a public repo, so live hosts, key paths and unfixed-weakness enumerations must not go there. This makes the repo-vs-private line *three*-way here: public `docs/` → internal `ops/` → private memory.

**Stays in private memory — and only this:** secrets-adjacent coordinates (SSH key paths, private hosts — these can't go in `ops/` either if the repo is public); the user's personal working preferences; and perishable state ("deployed vX on `<date>`, rollback at …") that would rot in git — anything *actionable* belongs in `~/Desktop/to-do/` instead.

**Three rules that make this hold:**
1. **Don't commit the memory *files*** into a `.claude/memory/` folder — the harness doesn't auto-load them from there, so they'd be inert paper. Fold the **content** into files that do auto-load.
2. **Never cite a private memory from a repo file** — it's a dangling reference for every other account. If a repo file needs the fact, put the **fact** in the repo.
3. **Repo first, then the pointer.** When something is both shareable and worth a personal reminder, write the repo entry first and leave the private memory as a one-line pointer.
4. **Only `CLAUDE.md` is auto-loaded.** `ops/engineering-log.md` exists so a rule can stay short without losing its evidence — but a finding written *only* there is the same inert paper as rule 1. Every batch writes the **rule** here and the **record** there.

## Things that have bitten before

Every rule here was paid for by a real defect, most of them silent. They are
grouped by subsystem rather than by date, because the question you arrive with is
"what must I not break in the scanner", never "what happened in June".

**The full record behind each rule lives in
[`ops/engineering-log.md`](ops/engineering-log.md)** — the measurement that
settled it, the alternative that was tried and rejected, the test that pins it,
and the PR. That file is not auto-loaded and does not need to be: act on the rule
here, and **grep the log by symbol, file or PR number before changing anything a
rule names.** A rule that looks arbitrary almost always has a measurement behind
it.

**When you fix something here, put the rule in this file and the record in the
log** — never only in the log, because nothing there reaches a session that has
not gone looking for it.

**Five claims in this list have gone stale and been corrected** — the WAV/AIFF
extractor gap, the `deletedIds` field name, "the bridge has no DLNA Search",
`manualDescriptionURL` being unimplemented, and (2026-09-22) "`waveform_path`
has the same shape and NO adoption yet", which #954 had falsified two days
earlier by wiring `integrity.LocateWaveform` into `analysisStoreAdapter`. Each
cost a later session real time; the fourth was written **after** the PR that
falsified it, by a session that had this very warning in front of it, and the
fifth sent `bridge doctor` on telling operators to run `bridge analyze --force`
— hours of decoding to recover curves the next request rebinds for free.
(Sections further down keep their own running tally of the same class, which
reaches higher; this count is of THIS list.) **Check the code before believing
any doc about it, including this one** — and when you find a stale claim,
correct it here rather than working around it.

**The same rot reaches CODE COMMENTS, and there it is more dangerous**, because a
comment is read as the reason for the code beside it. The retention LOUPE found
`apiDiagnostics` claiming "It touches no database" with its own "Three PRAGMAs"
and "Two COUNTs and a MIN" comments directly beneath, and `app.js` carrying a
second copy of that claim as the stated JUSTIFICATION for a 5-second poll. Both
were true when written. A false comment that explains a design choice is how the
next change gets made on the same reasoning.

### Scanner, deletion passes, and manifest writes

This is the highest-stakes area in the codebase: every bug here deletes rows
that still have files on disk, and the symptom reaches the user as "the bridge
lost my library."

- **"We could not see this path" dominates every "…but it looks like X"
  classification.** `isUnderErroredSubtree` must be evaluated BEFORE every other
  branch in the deletion loop that can delete. #549 added a case-fold rename
  reap ahead of it and reaped live rows of a case-twin directory during a
  permission flap; each new classification branch is a fresh chance to delete
  rows the walk never observed.
- **The five post-scan reconciliation passes all exclude UPnP-routed rows, from
  ONE routed set computed at the reconciliation head**, fail-closed (a fetch
  error skips all five) — never a per-pass `routedExclusionSet` call. Four of them didn't, and since `walkFieldsEqual` diffs
  exactly AlbumArtist/Album/Year, a disagreeing routed album flip-flopped every
  fs-scan ↔ UPnP-walk cycle — a perpetual re-enrich treadmill plus iOS delta
  churn.
- **Reconciliation is directory-scoped and never crosses directories**, picks
  the dominant EXISTING value (never MusicBrainz — MB's classical credits favour
  performers over composers and would shatter composer-sorted libraries), and
  **leaves `enriched_at` untouched**. Year reconciliation is FILL-MISSING ONLY;
  a present-but-different year is left alone.
- **`Scan`'s duplicate-restamp tail runs from a `defer`, and `ScanSubtree` has
  the same tail.** Three exits reach `return count, nil` *after* the deletion
  pass has committed; reached inline, a reaped winner left its twin
  `dupe_suppressed = 1` with no served copy — an album invisible to every
  client. Best-effort is NOT symmetric here: an unstamped row is served
  (fail-open), a stale suppression hides one (fail-closed).
- **The restamp re-checks for an in-flight scan immediately before
  `ApplyDupeStamps`**, via `Scanner.activeScans`. Don't take `s.mu` instead —
  it deadlocks the in-scan callers and, for external ones, blocks the scan for
  two library walks; and **don't widen the public `IsScanning`** to cover subtree
  scans, which drives the admin badge, the SSE fast tick and the booklet-GC skip.
- **Every `indexed_at` bump goes through `indexedAtAdvanceSQL`**, which clears
  the LIBRARY-WIDE max (`MAX(?, COALESCE((SELECT MAX(indexed_at) FROM tracks),0)+1)`).
  Both terms are load-bearing: the clock term anchors to wall-clock because the
  cursor IS wall-clock, the `MAX+1` term clears same-tick siblings. Three
  deliberate exclusions — the `UpsertTrack`/`UpsertTrackBatch` conflict arms,
  migration v34's `post()`, and `StampExtractorVersionBatch` (not an
  `indexed_at` writer at all). Don't "finish the job" by converting them.
  A bump-only writer uses `bumpIndexedAtByPathSQL`. `TestIndexedAtAdvanceIsShared`
  walks the named CONSTS and is blind to an inline literal in a function body —
  which is how #840 reintroduced the dead `CASE WHEN` form — so
  `TestNoHandRolledIndexedAtBump` sweeps every non-test file in the package and
  classifies each assignment against the SQL literal that contains it.
- **Any path predicate that writes, deletes, or bounds a scope MUST be a byte
  range, never `LIKE`.** Nothing sets `case_sensitive_like`, so `path LIKE
  'p/%'` matches a case-twin sibling — a DIFFERENT directory on a case-sensitive
  filesystem. Use `path COLLATE BINARY >= ?||'/' AND < ?||'0'`, binding
  `TrimRight(prefix,"/")` twice; **always trim the trailing slash first**, or the
  range becomes `'Album//'` and matches nothing (a silently-empty rollup).
  `subtreeLikePattern` survives only for deliberately case-folding callers — if
  you can't say why folding is wanted, you want `subtreeRangeBase`.
  `DeleteTracksByPrefix` **errors on an empty base**; sidecar enumerations must
  use the SAME bounds as the DELETE.
- **`unicode_lower` does TWO jobs, and only the CASE half is unwanted on a
  destructive path — so the fold stays as the CANDIDATE GENERATOR and the
  strictness moves to the acceptance.** Both queries behind
  `DELETE /v1/upscale/variants` (`ListVariantsForPath`,
  `ListVariantsByPathPrefix`) selected with `unicode_lower` and
  `RunVariantDelete` unlinks every row it is handed, so `?prefix=Jazz` also
  reaped `jazz/` and `JAZZ/` — measured: `selected [JAZZ/c.flac Jazz/a.flac
  jazz/b.flac]`. Byte-exact SQL is the WRONG fix: `unicode_lower` also
  NFC-composes, the scanner stores the on-disk form (NFD from HFS+ or a
  Linux/NAS sync) while clients send NFC, and dropping it answers
  `deletedCount: 0` for every accented album, silently.
  `acceptCaseExactVariants` re-uses the same `nfcCompose` the scalar itself
  calls, so generator and acceptance cannot disagree about composition and
  acceptance adds case and nothing else. This is the enricher's rule —
  *relaxations in the query, strictness in the acceptance* — on a delete.
  **Every OTHER `unicode_lower` predicate is a READ and already failed closed**
  (exact first, folded fallback, `LIMIT 2`, "refusing to pick a row"); the two
  that unlinked bytes had neither, and the asymmetry was the bug.
  `subtreeLikePattern`'s sanctioned exception named a variant-GC caller **that
  does not exist** — `--gc` drives off `AllVariants` — and the log repeats it
  under a third name that does not exist either. PROTOCOL.md already said "one
  exact source track", so the spec was right and the code was not: no spec
  change, no Mirror-PR.
- **The threshold reap unlinks its sidecars**, and both DELETE arms and both
  sidecar enumerations are derived at compile time from one shared `where`
  const so the unlink set and the row set cannot diverge. The ill-formed-UTF-8
  arm needs its own enumeration. Variant enumeration is strict (abort before the
  CASCADE); waveform is best-effort.
- **Every writer holds `Store.mu`; reads stay un-mutexed** (WAL handles
  concurrent readers). The mutex also protects multi-statement transactions and
  `SELECT sidecars → DELETE rows → os.Remove` ordering — `busy_timeout` is a
  retry, not a serializer. Don't set `SetMaxOpenConns(1)`.
- **A manifest path names its own root — map it with `fs.Resolver`, never by
  joining onto one.** In multi-root mode `relPath` prefixes every stored path
  with the root's basename, so `filepath.Join(roots[0], rel)` addresses a path
  under the WRONG root with the routing segment still in it. `internal/trash`
  hand-rolled exactly that and delete was broken on every multi-root bridge —
  and where two roots' directory names overlap it trashed a real, unrelated file
  and then reaped the manifest row of the file still on disk (`retireAndRescan`
  passes threshold **1**). `Resolver.SplitRoot` returns `(root, suffix)` from the
  same computation that produces the absolute path, past the same containment
  check, under ONE lock snapshot — so `Resolve` and `SplitRoot` cannot disagree.
  A path naming a root this bridge no longer has is a REFUSAL
  (`ErrRootUnavailable`), never a fallback; a caller-supplied root CONSTRAINS
  rather than selects. Anything that joins manifest-form dirs onto a
  caller-supplied root has the same bug one layer up — `spawnBackgroundSubtreeScan`
  did, so a batch spanning roots resolves per-dir instead.
- **A single↔multi root flip calls `WipeFilesystemTracks`, never
  `WipeAllTracks`** — the latter CASCADE-deletes `upnp_track_routing`,
  destroying an entire upstream library and its cached enrichment on a mere
  root-count toggle. The folder wipe is part of it (folders flip form too).
- **`wal_checkpoint(TRUNCATE)` runs AFTER `VACUUM`, never only before.** In WAL
  mode the vacuum's own output lands in the WAL, so without the post-checkpoint
  the file does not shrink by a byte and peak disk RISES. Measured: 5,623,808 →
  5,623,808 with the WAL grown to 2.8 MB, then 2,572,288 once checkpointed. A
  review proposed the pre-vacuum checkpoint alone, which would have shipped a
  button that reports success and reclaims nothing — so assert on the FILE
  shrinking, not on `freelist_count`, which reads 0 under the broken form.
- **Migrations are append-only**; `sql` must be idempotent. A shipped migration
  is never rewritten — both live bridges already ran it, so editing it changes
  only fresh installs while diverging from what deployed DBs did.
- **Column-only columns must never gain `json:` tags or be spliced onto wire
  output**: the v25 format facts, `acoustid_match` (v28), `dupe_*` (v31),
  `audio_md5` (v32), `booklet_tag`, `artwork_version`. `tags_json` stays the Go
  readers' truth; the columns are query accelerators. `marshalForStorage` zeroes
  the spliced fields (`Enriched`, `BPMEstimated`, `Variants`) on the way in.
- **Bump `manifest.ExtractorVersion` on EVERY extraction-logic change** (const,
  must stay ≥ 1 — at 0 the `stored >= current` gate never re-extracts), and keep
  `= excluded.extractor_version` in both upserts. A bump re-extracts the library
  once; the **version-stale diff-guard** (`reExtractUnchanged`) is what keeps the
  client delta bounded to rows that actually changed. **Derive that guard's
  merge set by grepping the actual `tags_json` writers, not from what a field
  "looks like"** — `MusicBrainzTrackID` was omitted on the belief it was
  extractor-owned when the acoustic fallback writes it.
- **`plausibleDuration` has BOTH ends, and "empty" has three spellings**
  (ExtractorVersion 13, #966). The gate defended the ceiling alone until
  then: `d > 0` admits anything positive, and a forged header reaches
  absurdly SMALL as easily as large — an `mvhd` with
  `timescale = 0xFFFFFFFF, duration = 1` is 2.3e-10 s, finite, positive,
  under a week, stamped onto the wire where nothing re-derives it and the
  phone renders "0:00" instead of the absence it is. The floor is 0.1 s.
  **The docblock matched the code exactly**, which is why neither a test
  nor a review caught it — the gap was in the POLICY — and
  `TestPlausibleDuration_TruthTable` asserted `{"tiny positive", 1e-9,
  true}`, the missing half written down as intended behaviour. A test can
  encode the bug and pass forever. On the IFF side `iffPayloadFits` took a
  declared size of ZERO as fitting (`0 <= physicalSize - offset` holds for
  any bound), and an AIFF SSND body opens with an 8-byte `offset` /
  `blockSize` prefix, so **8 is as empty as 0** — a COMM claiming ten
  minutes beside either one stamped ten minutes. `ssndSoundSpan` NARROWS
  the span past the prefix rather than testing it: subtracting from `size`
  alone leaves the offset on the prefix and weakens the physical bound by
  those bytes. Three unseen-span cases — declares nothing, holds only its
  prefix, cannot hold its prefix — and a positive control, because "reject
  the empty ones" must not become "reject everything".
- **A fix placed IN FRONT of a check can destroy that check's input, and
  the AIFF sentinel is the case** (ExtractorVersion 14, #967).
  `iffPayloadFits` refuses the streaming writer's `0xFFFFFFFF` — v12's
  rule, written for BOTH IFF walkers. v13 then gave AIFF a narrowing step
  ahead of it: `ssndSoundSpan` subtracts the 8-byte SSND prefix, so the
  sentinel arrived as `0xFFFFFFF7`, an ordinary number to the arm that
  refuses `0xFFFFFFFF`. It reaches the unknown-bound case v12 was written
  for (where `iffPayloadFits` fails OPEN) and any file of 4 GiB or more.
  **A small fixture cannot see it** — below 4 GiB the narrowed value fails
  the physical bound anyway — so the pin drives the SPAN, as the
  sentinel's own test does. Refuse the sentinel at the TOP of
  `ssndSoundSpan`, before any narrowing. WAV does not narrow.
- **A range gate does not protect a value computed in a type that can wrap
  INTO the range.** `parseDurationSeconds` did `float64(h*3600+m*60)`:
  `Atoi` accepts the hours of `"1152921504606846977:00:00"` on a 64-bit
  build, `2^60*3600` is a multiple of `2^64`, and the product is **exactly
  3600** — one hour, from an attribute claiming 131 billion years, which
  `PlausibleDuration` then accepts (max int64 lands on -3600 the same way).
  Widen BEFORE multiplying. Found by a bot against the gate the same PR was
  adding.
- **`internal/upnpingest` is the one `Track.Duration` writer outside
  `internal/manifest`, and it passes the same gate** via the exported
  `manifest.PlausibleDuration` — not a second copy, which is the copy that
  drifts. A DIDL `res@duration` is an untrusted header by another name:
  `0:00:00.001` and `10000:00:00` both parse cleanly and both reached
  `tags_json`, the wire and the phone's track list. `walkFieldsEqual`
  compares `Duration`, so an affected routed row re-upserts once and then
  stabilises.
- **Every derived `Track.Duration` passes ONE gate, `plausibleDuration`, and
  the AIFF / WAV walkers add the DFF `payloadFits` rule through one
  `iffPayloadFits`** (ExtractorVersion 11, #935 — MP4 `mvhd`, MP3 Xing / Info /
  VBRI or the first-frame CBR estimate, AIFF COMM frames, WAV `data` bytes over
  `nAvgBytesPerSec`; the v11 docblock in `extractors.go` carries the per-format
  reasoning). Don't stamp a duration at a new site without both. **The fuzzer
  found the first draft's bug within 16 s: a `largesize` mvhd declaring ~2^63
  bytes converted its payload length to a NEGATIVE `int` and sliced a buffer by
  it** — compare a declared length in `uint64` BEFORE any `int` conversion, and
  treat a box declaring past its PARENT as absent (`findAtom` only checks that
  the header sits inside the bound). The crash input is committed as the
  regression seed; run the whole-file fuzz targets for a minute after any
  parser change, before the PR, not after. (And this PR re-learned the "Commit BEFORE the negative control" rule
  recorded further down: a control's `git checkout --` took an uncommitted
  refactor with it. Commit before EVERY control, including the small
  confirmatory one.)
- **`enriched_at`'s sanctioned writers are a closed set**: the enricher, the
  operator "Retry missing" resets (`ResetEnrichedMisses`,
  `ResetEnrichedByArtistMBIDs`, `ResetEnrichedMissesUnderPrefix`), and
  `ResetEnrichedByPaths` (the fingerprint sweeper's narrow, explicit-path form).
  The upsert reset to 0 is load-bearing. Never touch it anywhere else — the
  `WHERE enriched_at = 0` query drives the worker, and a broad reset pushes a
  whole-library delta to every paired device.
- **`enriched_at = 0` is NOT a tag-re-extraction reset**, and `mtime_ns = 0`
  looks like it should work but doesn't (`GetTrack` reads mtime from inside
  `tags_json`). See `## Local test fixture` for the two working paths.
- **The scanner excludes its own variant sidecars** via an ANCHORED filename
  match (`^(upscaled|optimized)-v\d+-\d+-\d+$` on the final dot-segment, with
  the part before it a supported audio ext). Don't loosen to a substring — it
  false-positives on a real `Song.optimized-Mix.flac`; don't skip a whole
  `variants/` directory by name.
- **`fillFromPath` is multiRoot-aware** — with a root-basename prefix the
  album/artist heuristics must strip `parts[0]` first, or an untagged file under
  a root named like an artist inherits the root basename as its Artist.
- **Per-iteration panic recovery lives INSIDE the per-file closure** in
  `runScanWorker`, not around the loop — a panicking file must skip and let the
  worker continue. A crash found by the extractor fuzz targets is a REAL defect:
  the recover means a panicking file silently never reaches the manifest.
- **Shutdown joins every background writer**, and the wait must be written
  INLINE in the defer, never routed through a variable assigned later in
  `runServe` — the first tracked goroutine starts ~1200 lines before the end, so
  an early return leaves it nil and lets a live writer race `Store.Close()`.
  Both this wait and the watcher's are grace-BOUNDED; a wedged writer degrades
  to a log line, never a hung exit. A writer includes a goroutine whose CHILD
  PROCESS writes files: the Tailscale auto-pilot's `tailscale cert` was the one
  unjoined writer in `runServe` until #997 (the `cmd/bridge` section below).
- **Anything walking FLAC metadata blocks SEEKS past a validated PICTURE payload,
  never drains it.** The single-open FLAC path exists because a 5–25 MiB embedded
  cover crossing the wire twice per track halved scanner throughput on NAS-mounted
  libraries; a verdict needs ~30 bytes of header. This is safe ONLY because
  `meta.New` reads the 4-byte header directly from the reader and wraps it in a
  plain `io.LimitReader` — check that still holds before adding a seek anywhere
  else in that walk. A misaligned walk bails fail-open and silently disables the
  allocation guard, so alignment needs its own pin. **Verified against
  mewkiz/flac v1.0.14** (2026-09-23): `meta.New` is byte-identical to 1.0.13,
  still reads exactly 4 header bytes through a non-buffering reader, still wraps
  the body in a plain `io.LimitReader` — and upstream's new `readString` now
  type-asserts `*io.LimitedReader` itself, so it depends on the same property.
  **"Anything" was aspirational until then**: there are THREE walks over the
  same handle, and `applyFLACMultiValueArtists` drained. `block.Skip()` ALWAYS
  drains — it checks for an `io.Seeker` and `*io.LimitedReader` never is — so
  "skipped via Skip()" is a claim about ALLOCATION, not I/O. That distinction
  was already drawn and paid for in #165, whose docblock on the STREAMINFO walk
  says `Skip()` "CONSUMES the bytes from the underlying reader, NOT JUST THE
  BUFIO BUFFER"; PR #208 then reintroduced it 30 lines from the walk that does
  it right. **Latent, not live, and the reason is worth knowing**: the walk
  returns at `TypeVorbisComment`, so it only drains blocks BEFORE it, and the
  canonical `flac`/`metaflac` layout puts the comment block first (measured 0/4
  on real files). A fixture in that order proves nothing — the pin puts PICTURE
  first.
- **Extraction: presence-gate the integers, refuse bit depth on lossy codecs, and
  split TIT1→Work / TIT2→Title.** dhowden returns 0 for both "tag absent" and "an
  explicit 0", so Year/TrackNumber/DiscNumber need a raw-map presence check to
  keep `Some(0)` distinguishable from `nil`. `isLossyCodec` gates every
  `BitsPerSample` write site structurally. `Year()==0` falls back to
  `parseYearPrefix` over the raw date tags — a valid ISO `2023-06-09` otherwise
  indexes as year 0. `stringOf` iterates the REQUESTED aliases in priority order,
  never `range raw` (Go map order is randomised, so a file carrying two matching
  tags resolved differently across scans).
- **`normaliseRawTagKey` canonicalizes a LEADING `0xA9` before `ToLower`.**
  dhowden surfaces MP4 ilst atoms under a single-byte `\xa9day` key, which is
  invalid UTF-8, and `ToLower` rewrites it to U+FFFD — so source-literal `©day`
  aliases never matched and year / composer / multi-value artist were silently
  dropped for every M4A. Don't reintroduce a raw-byte alias; it would be compared
  against the mangled key and still miss. **Tests must use the real `\xa9…` byte
  keys** — UTF-8 `©ART` fixtures match the alias without reproducing the bug.
- **dhowden/tag does not read `gnre`, and keeps the data-atom locale on every
  MP4 freeform value** (ExtractorVersion 15, from a report that Rock / Pop listed
  only DSD). iTunes and Music write a STANDARD genre as `gnre` (uint16, ID3v1
  index PLUS ONE) and only a custom genre as `©gen`; `extractMP4PredefinedGenre`
  reads it when no text genre was found, from `id3v1Genres` — dhowden's table
  VERBATIM, pinned index by index against dhowden's own `"(n)"` expansion so an
  M4A `gnre` and an MP3 numeric TCON name one genre (the iOS
  `GenreNormalizer.id3GenreTable` mirrors the same table). dhowden's
  `readCustomAtom` slices a `data` body past the type indicator but not the
  locale, so every `----` value arrived `"\x00\x00\x00\x00<value>"` —
  NUL-prefixed MusicBrainz ids on the wire, unparseable ReplayGain / original
  year. `stripMP4FreeformLocales` removes it ONCE, right after `tag.ReadFrom`,
  before any reader of `m.Raw()`; only a value STARTING with the locale is
  touched (standard atoms never carry it). Two premise tests fail the day
  dhowden fixes either — delete the workaround then, not before. **The moov
  search is bounded by the FILE SIZE, never a byte span**: `findMoov` stopped at
  4 MiB until v15, costing every moov-after-mdat M4A (ffmpeg's default) its
  codec, rate, bits and duration. The M4A fixtures in `testdata/m4a` are
  byte-identical to the iOS app's `AVTagFixtures`; regenerate both or neither.
- **`Track.Enriched` allocates per row; don't reintroduce package-level singleton
  bool pointers.** `Track` is exported, so a shared pointer lets any downstream
  write clobber every subsequent read for the process lifetime. The cost is
  ~450 KB across a 50k-track library — noise against the query and marshalling.
- **Default `ScanIntervalSec` is 21600 (6h), not 3600.** An hourly cadence on a
  mechanical NAS prevents idle spindown — an operator-facing wear hazard.
- **The fsnotify watcher is OFF by default and the periodic scan is the safety
  net.** Path containment uses `filepath.Rel`, never `strings.HasPrefix` — the
  byte-exact form is case-sensitive on macOS/Windows where the filesystem isn't.
  Linux watch-limit handling is two-layer (runtime fallback to periodic-only, plus
  a `bridge doctor` pre-flight at 80% of the budget).
- **A WalkDir-resume cursor compares in TRAVERSAL order, not raw string order.**
  `WalkDir` orders by base name and visits a directory before its children, so
  `A-Bonus/…` sorts BEFORE `A/…` as a raw string while being walked after —
  pruning on a raw `<` permanently skipped a still-unwalked subtree. Use the
  segment-wise comparison, and keep it allocation-free (it runs per entry on the
  walk hot path).
- **New shared scanner fixtures go in the untagged `scanner_fixture_test.go`**,
  never a build-tagged file — untagged siblings referencing them broke the
  Windows compile of the whole `manifest` test binary, invisibly.

- **Every byte-range prefix consumer trims the trailing slash, including
  the per-FOLDER twin.** `EligibleRollupByPrefix` did; `EligibleCountsForFolders`
  beside it did not — latent only because the browse handler passes bare
  paths, which is the exact state the rollup bug was in before a second
  caller forwarded a raw one (#536). The result stays keyed by the caller's
  spelling so a lookup by what was passed still hits. (#919)

### The wire contract

- **`/v1` describes the SERVED set.** The manifest stream/page/total,
  `enrichmentProgress`, health's `tracksIndexed`, the DLNA adapter and the
  smart-playlist pools all go through the `ListServed*` / `CountServedTracks`
  family (`dupe_suppressed = 0`). The full-store readers stay unfiltered ON
  PURPOSE and must keep seeing suppressed rows: `TrackPaths`/`TrackPathsUnder`
  (deletion-pass snapshots — filtering them would REAP suppressed rows),
  `ListTracks`/`StreamTracks`, `UnenrichedTracks`, `GetTrack`, and every admin
  rollup (operator truth). `/v1/list|stat|read|download` are path-keyed and
  unfiltered so stale clients keep working.
- **Optional numeric/bool `Track` fields are pointers.** Non-pointer +
  `omitempty` silently drops zero/false, so iOS decodes `nil` instead of
  `Some(false)`. Same trap with `omitempty time.Time`: Go does NOT drop a zero
  time, so it ships `"0001-01-01T00:00:00Z"` and the client parses a real,
  very-old date — use `*time.Time`.
- **An optional time on a wire DTO is a `*time.Time`, and a guard enforces it.**
  This rule was stated here, fixed correctly in three structs, and violated in
  TEN fields across five files — including the `/v1/health` DTO iOS decodes.
  Documenting it three times did not stop the eleventh, so
  `TestNoOmitemptyValueTimeOnTheWire` (an AST walk over `internal/api` +
  `internal/admin`) now fails the build on a value `time.Time` tagged
  `omitempty`. **Check the client's optionality before converting one** — omitting
  a field a shipped app declares non-optional breaks its decode, which is why
  `/v1/health`'s withholding scope was set by reading the iOS Codable rather than
  by principle. **And move the TEMPLATES in the same commit**: `timeAgo` takes a
  value, text/template auto-indirects a pointer, and a NIL pointer is an
  execution error that fails the whole page rather than rendering "never". That
  half no Go type check can see.
- **Delta manifests omit the `folders` block** (full-sync only), and `since`
  filters on `indexed_at`, never mtime.
- **A served→suppressed transition writes a `manifest_deletions` tombstone in
  the same transaction as the stamp**, and the since-leg emits it as
  `deleted: [paths]`. `indexed_at` is still never bumped on suppression; the
  tombstone is the delta signal.
- **A playlist tombstone is DATA, and `updated_at` on a `deleted = 1` row IS
  the delete time.** `DELETE /v1/playlists/{id}` sets a flag and keeps every
  `playlist_items` row, so the console can lift it (`RestorePlaylist`, behind
  `GET /api/playlists/deleted` + `POST /api/playlists/{id}/restore`) — which is
  what the 2026-09-20 incident needed and did not have: a freshly-paired phone
  replayed a queued delete, 17 DELETEs in 26 s, and recovery was an `UPDATE`
  by hand against a live database. A restore **must not move
  `last_modified_at`** — that is the client's wall clock and the LWW guard key,
  and an operator undoing a delete has not authored a new version; leaving it
  makes LWW resolve exactly as if the delete had never happened. There is
  deliberately **no `deleted_at` column**: `TombstonePlaylist` is the only
  writer of `deleted = 1` and stamps `updated_at` in the same statement, so a
  second column carrying the same instant could only disagree by being wrong
  (`TestUpdatedAtOnATombstonedRowIsTheDeleteTime`). What WAS missing is the
  DELETER — `device_token` is the last WRITER, a different device for all 17
  rows — hence `playlists.deleted_by` (v45), cleared on restore so a stale
  value cannot attribute the next delete. The custom cover does NOT come back:
  the DELETE unlinks the JPEG (`api.pruneCover`) and nothing keeps a copy.
- **The playlist mass-delete WARN counts from the TABLE, never an in-process
  ring**, and fires ONE LINE PER TOMBSTONE past the threshold
  (`manifest.PlaylistDeleteBurstThreshold` = 5 within
  `PlaylistDeleteBurstWindow` = 60 s). The console's panel gets the SAME
  predicate, not the same two numbers: `LargestPlaylistDeleteRun` groups the
  tombstones server-side over the whole `deleted_by` token, because the browser
  cannot reproduce it even in principle — `deletedByPrefix` is eight redacted
  characters, and the first draft's chain-of-pairwise-gaps ended a run at one
  interleaved delete from another device, so the panel said 3 where the journal
  said 6. **A predicate the client cannot evaluate exactly belongs on the
  server, whatever the numbers behind it.** Counting from the rows is
  what makes the warning and the restore panel unable to tell different
  stories, and what makes a burst spanning a restart still one burst. The
  repetition is deliberate and is **not** the M-SEARCH case: that ticker's
  failure is persistent and every line identical, while a delete run is bounded
  by how many playlists exist, each line carries a different count, and the last
  one is where the run stopped. Never a gate — the tombstone has committed by
  then, so a failed count logs and the delete stands.
- **Verify a wire field name against the Go tag before "fixing" code to match a
  doc.** `deletedIds` was written up as `deletedPlaylistIDs` here for months
  while the tag, both PROTOCOL.mds and the iOS DTO always agreed on `deletedIds`.
- **`last_modified_at` is the only precision-critical wire field** — client
  UnixNano `Int64`, never round-tripped through a string or `Date`, or
  truncation falsely trips the LWW 409.
- **Every `/v1` route declares a `rateClass`, and the zero value is INVALID.**
  Unlike `routeKind`, the permissive answer is the dangerous one here. **Size the
  BURST, not the rate**: iOS surfaces 429 as a transport error and does NOT
  retry, so a tight bucket breaks a sync rather than slowing it.
- **`safeQuery` on every path-bearing query consumer.** `url.Values` decodes `+`
  as a space, so a path containing `+` silently resolves to the wrong file — a
  `200 {deletedCount: 0}` no-op. The client half must use `encodeURIComponent`,
  never `URLSearchParams` (which form-encodes a space back to `+`).
- **`/v1/health` withholds `scanState` and the update triple from an
  unauthenticated caller**; the scope was set by reading which iOS Codable
  fields are optional, not by principle — `libraryName`, `libraryRoots`,
  `certFingerprint`, `serverVersion` and `startedAt` are non-optional there, so
  withholding them fails decoding on every shipped app. An invalid token is
  unauthenticated, never 401 — a client holding a revoked token still needs the
  endpoint list to reconnect.
- **Long-lived and streaming routes need their own deadlines**:
  `/v1/booklet/{mbid}` and `/v1/download` are `streamingRoute`; the two
  synchronous upscale operations carry a 15-minute `writeDeadline` because
  `boundedHandler` starts the clock BEFORE the handler runs.
- **`/v1/artwork` and `/v1/artist-image` cache-miss is a THREE-way split** — 202
  `pending` only while enrichment is genuinely pending, terminal 404 `no_image`
  once it has run. Fail OPEN to pending on a DB fault; no `Retry-After` on
  `no_image`; don't collapse it back to two states.
- **Don't assume byte-wise client consumption.** The iOS client once used
  `URLSession.bytes(for:)`, which yields one `UInt8` per async step — 20M yields
  for a 20 MB file stalled the pipeline and surfaced as "Network connection lost"
  even over localhost. Don't add a server-side chunked mode that assumes it.
- **Never advertise an endpoint synthesised from this process's own listen
  port.** In public mode the bridge emits `https://<autocert.domain>:<listen
  port>` beside `customEndpoints`. That is right only when nothing remaps the
  port — behind a proxy that does (each hosted tenant listens on a loopback high
  port and is published on one shared external port) it publishes an address no
  client can reach, and iOS puts every advertised URL into its failover
  rotation. The entry is skipped when `customEndpoints` already names that HOST;
  the operator declaring it there is them saying what the reachable address is.
  Host comparison, not URL comparison — the two differ in exactly the part that
  is wrong, so the existing string dedupe cannot see it. `autocert.domain` cannot
  simply be omitted: public mode refuses to start without it.
- **mDNS TXT records carry `host` + `port`.** Without them iOS must
  NWConnection-resolve the Bonjour service to a hostport, which is unreliable;
  the bare-hostname-plus-`.local` form matches the SRV target the cert SANs
  already cover. Never emit `host=.local` — fall back to `localhost.local`.
- **A shape documented in PROTOCOL.md is a Mirror-PR obligation.** The two specs
  are byte-identical by rule, and both directions are guarded
  (`TestEveryDocumentedEndpointIsRouted` / `TestEveryRoutedEndpointIsDocumented`).
  A mention in running prose is NOT a contract — the guard accepts only a `### `
  heading or a bold `METHOD /path` lead-in, because prose-mention is the state
  six live endpoints were already in.
- **Don't label a spec section with a version you cannot verify.** The `since
  v1.x` labels are iOS app versions, which a bridge-side session cannot derive.
  Name the **feature flag** instead — it is checkable here and is what a client
  keys on, since no client can ask a bridge its protocol era.

### Lyrics — the one document per track

The surface landed whole in ONE PR (#840) and had no section here until a LOUPE
run swept it; every rule below is from that sweep (PRs #849 / #850 / #851).
**Three of the four defects were permanent and silent** — no error, no log line,
no failing test — which is the shape to expect in this area.

- **A bump-only writer uses `bumpIndexedAtByPathSQL`, never a hand-rolled
  `CASE`.** `writeLyricsRowTx` shipped the exact
  `CASE WHEN indexed_at >= ? THEN indexed_at + 1 ELSE ? END` form that
  `indexedAtAdvanceSQL`'s docblock quotes verbatim as dead, so lyrics-changed
  rows landed on a cursor clients already held and the track never reached the
  phone. `StampExtractorVersionBatch` takes ONE `now` for a whole batch, which
  is precisely the caller shape that collides. `TestIndexedAtAdvanceIsShared`
  could not see it — it walks named CONSTS and this was an inline literal — so
  `TestNoHandRolledIndexedAtBump` now sweeps every non-test file in the package
  and classifies each `indexed_at =` against the SQL literal that contains it.
- **The skip gate and the extractor must resolve a sidecar the SAME way, and
  the gate's answer must be able to change.** Two non-convergent shapes both
  re-extracted the audio file on every scan forever, invisibly (they land on
  `reExtractUnchanged` → `versionStampOnly`, so there is not even `indexed_at`
  churn to notice): a stale `sidecar-rejected` row that the nil branch declined
  to DELETE because it gated on `oldTag == ""` rather than `!hadRow`; and
  `sidecarLyricsDrifted` comparing the sidecar's EXTENSION rank when an empty,
  oversized, legacy-encoded or TAGLESS `.lrc` resolves to something that loses.
  `sidecarLyricsFile` answers WHICH file; `readSidecarCandidate` answers WHAT
  IT IS WORTH — both shared, neither duplicated.
- **`Pick`'s comparator is a strict TOTAL order.** Candidates arrive partly from
  `range m.Raw()`, a Go map, so any pair the comparator leaves equal flips the
  winner between scans → `lyricsTag` re-keys → `indexed_at` bumps → the track
  re-enters every device's delta on every scan. Same rule as the dupe elector,
  same reason. `mergeDuplicate` picks its surviving base with that same order,
  not by arrival — the dedup key is only `(Source, Body)`, so a second sighting
  can still differ in the derived fields.
- **`m.Lyrics()`'s `Priority: 0` is FABRICATED.** dhowden's
  `metadataID3v2.Lyrics()` returns `m.frames["USLT"].(*Comm).Text` — literally
  the frame the raw walk then re-reports with its real `DescriptorPriority` —
  so keeping the first sighting let an "Amazon" / "Song ID" descriptor launder
  itself back to the best rank and `junkExact` / `junkSubstring` silently did
  nothing. The merge keeps the LARGER priority: the real classification always
  beats the fabricated one.
- **Everything `lrcTime` emits must match `lineTag` or `hoursTag`.** `ParseSYLT`
  reads a raw uint32 of milliseconds from an untrusted frame, so past 999
  minutes it rendered `[1000:00.000]`, which neither regex accepts, inside a
  document `syltCandidate` stamps `synced: true` regardless — the phone drops
  the line. It is CLAMPED to 999:59.999, not promoted to `[hh:mm:ss.xxx]`:
  that form would rewrite every legitimately >1 h track (a delta wave for
  content that works today) and `renderLine` uses the same helper for the
  enhanced `<mm:ss.xxx>` WORD tags, whose iOS grammar is not mirrored here.
- **`Normalize` must be IDEMPOTENT, and `stripBOMs` must stay LINEAR.**
  `resolveLyrics` normalises an already-normalised body, so the two passes
  disagreeing silently drops a document that was accepted as a candidate.
  Deleting a U+FEFF splices its neighbours and can form a NEW one
  (`"\xef\xbb" + BOM + "\xbf"`), and Go's `unicode.IsSpace` does NOT count
  U+FEFF, so `TrimSpace` keeps a lone BOM. **Don't "simplify" the byte-wise
  reducer back to `ReplaceAll` in a loop** — that is quadratic on a nested
  input, `MaxBodyBytes` is only checked AFTER it, and the USLT / ©lyr / Vorbis
  path reaches `Normalize` with no size gate at all. Measured on a
  180,003-byte nest: 3.967 s against 640 µs.
- **The lyrics parsers are fuzzed, and the targets carry PROPERTIES.** The
  package had zero targets against a policy that names the audio extractors as
  one of the three untrusted-input surfaces. `FuzzParseSYLTToLRC` asserts every
  emitted line is LRC-parseable (which is what catches the clamp class),
  `FuzzNormalize` asserts idempotence + the cap + no CR,
  `FuzzPickIsShuffleInvariant` asserts the winner survives shuffling — that one
  found a SECOND order-dependence, in `mergeDuplicate`, that no
  extractor-driven test could reach — and `FuzzTextCandidateClassification`
  pins the synced verdict to `LooksLikeLRC && !sparse` on both halves, with
  format / synced / source agreeing.
- **A sparse-timed body is PLAIN, and the rule lives in the CLASSIFICATION,
  not in `LooksLikeLRC`.** `LooksLikeLRC` is any-line on both sides and must
  stay so; the app's `LRCParser.parse` returns the plain document when
  `timed.isEmpty || timedCoverageIsTooSparse(...)` (fewer than 2 timed TEXT
  lines beside any untimed text, or under 25 % of the non-blank lines), and
  `IsSyncedLRCBody` mirrors BOTH arms — `TimedCoverage` for the counts,
  `TimedCoverageIsTooSparse` line for line with the app's function, guard
  included, constants and truth table lifted from the Swift tests verbatim
  (iOS #1759). **Keep the `timed > 0` arm outside the sparse function**: the
  app's guard answers "not sparse" for zero timed lines because the app never
  asks it that, so a body whose only tags are clear events (`Prose\n[00:12.00]`)
  is caught by `timed.isEmpty` there and must be caught by `timed > 0` here —
  Gemini saw the outcome on #904 and proposed dropping the guard, which would
  have broken the verbatim truth table. Before it, a Genius transcript with
  one `[4:20]` cue was a rank-4 `text-lrc` stub outranking the complete plain
  document in the same file. The phone was never at risk — it re-parses the
  body and reads `synced` as advisory — so a divergence here shows up only
  as the bridge's election and stored verdict, silently. `ExtractorVersion`
  went to 9 for it, per the every-extraction-change rule and v8's precedent.
- **The SYLT marker set is `\r`, `\n`, `\r\n` — the phone's — and a "VERBATIM
  mirror" has to be re-read when the original moves.** iOS #1564 (2026-09-03)
  made a bare `\r` count in `entriesLookLikeWholeLines`, `carriesMarker` and
  the prefix/suffix strip because CR-only taggers exist; `hasNewlineMarker` /
  `splitMarkers` stayed on `\n` / `\r\n` for nine days while
  `strings.Trim(text, "\r\n")` kept stripping the CR from the body — so
  `leading` / `trailing` read false and ToLRC merged the next entry onto the
  open line. `TestToLRCTreatsABareCarriageReturnAsAMarker` pins it; the
  two-entry cases were rescued by the whole-line gap heuristic by accident,
  which is why the mixed-marker fixture is the one that fails.
  `ExtractorVersion` went to 10 for it. When an iOS lyrics PR touches
  `ID3v2Parser`'s SYLT arm, diff `sylt.go` against it in the same week.
- **`sidecarLyricsExts` is `.ttml` > `.lrc` > `.txt`** — `Source.Rank()` order,
  PROTOCOL.md's order, the app's order. `sidecarLyricsFile`'s own doc comment
  said the opposite for three days.
- **The scanner compares mtime EXACTLY; the API's 410 check uses a 2 s
  tolerance.** Both are correct and they are not the same question. An external
  review proposed importing the tolerance into `sidecarLyricsDrifted` — declined:
  the primary skip gate compares the AUDIO file byte-exactly
  (`scanner.go`, `existing.MTimeNS == pi.info.ModTime().UnixNano()`), so
  loosening only the sidecar half would be the inconsistency, not the fix.
- **⚠️ "Atlas cannot supply lyrics" is STALE and was corrected 2026-09-09.**
  It was true when measured on 2026-09-06 — `atlas_qobuz_track` held 679,296
  rows with 0 non-empty `lyrics` — but that measured the QOBUZ mirror, and
  Atlas has since grown an LRCLIB-backed surface at
  `/v1/atlas/recording/{mbid}` that is nothing to do with it. The fifth stale
  claim this file has shipped. **A negative result is about the thing you
  measured, at the time you measured it** — scope such a bullet to the table
  it looked in, or it reads as a standing verdict on the whole upstream.

#### The network lyrics tier (`atlas-lrc` / `atlas`)

- **A network row is not the scanner's to reap.** Every branch of
  `writeLyricsRowTx` reasons from a LOCAL extraction, and its "nothing found"
  arm DELETEs — right for a removed sidecar, and about a document Atlas
  supplied it is evidence about nothing. Unguarded it reaps the row on the
  very next scan and every scan after. Arbitration is `Source.Rank()`'s, not
  a second copy of it: `atlas-lrc` at 5 sits above every UNTIMED local
  document and below every timed one (the app's DD3 rule expressed in the
  ladder), plain `atlas` at 8 beats nothing. `UpsertAtlasLyrics` applies the
  same order in reverse and **refuses to demote**, so two sweeps cannot
  alternate a track between documents — every write strict-advances
  `indexed_at`, and a flap is a delta to every paired device per cycle.
- **A release is not the RUN.** Any release-fetch error used to abort the whole
  sweep, and the candidate query is `ORDER BY albumMBID, t.path` — deterministic
  — so one unanswerable release aborted at the same point on every 60-second
  tick while healthy albums drained out of the candidate set and the failing one
  migrated toward the front. The tier went silent for the whole library with one
  warn line. Skip the ALBUM and continue; abort only on auth or a cancelled
  context, which are facts about the run. **The skip needs a cooldown or it
  trades a stalled tier for a hammered upstream**, and the cooldown must not
  RE-cool on a candidate it is already suppressing: pushing `until` forward every
  tick means it never expires, which is the negative cache it exists not to be.
  Two sentinels, not one.
- **`isUpstreamAnswered` governs BOTH legs.** The release leg had it and the
  recording leg did not, so a durable 404 wrote no verdict — and the candidate
  query gates on the ABSENCE of a row, so the track returned every sweep forever.
  At `LIMIT 400` over a deterministic order, a few hundred stale
  `musicBrainzTrackID`s hold every slot and nothing else is ever considered.
- **An UNRECOGNISED upstream status is transient, and never written verbatim.**
  Falling through to `documentFrom` stamped `unavailable` with a thirty-day
  backoff — a durable verdict about the TRACK from a sentence about the UPSTREAM
  this build could not read. Stamping nothing is the other half of the trap (the
  candidate returns every tick), so it takes `pending`'s short backoff. Never the
  upstream's own string: `status` is a column the candidate and stats queries
  switch on.
- **The scan guard is per CANDIDATE, not per sweep.** A pass is
  `lyricsCandidateBatch` × `lyricsPacing` of pure pacing — longer than the poll
  interval at the defaults — so a scan starting a second in ran entirely inside
  the sweep, which is the double `indexed_at` bump the guard exists to prevent.
- **`Addressable` must carry the candidate query's `instrumental` arm.**
  `instrumental` is a success that correctly leaves no `track_lyrics` row, so a
  count that only asks "no lyrics, has an MBID" counts it forever: a quarter of
  Atlas's measured mix, and a finished tier renders as a stalled job. It is a
  named const beside the candidate query, with the same retag invalidation, so
  the two cannot drift.
- **`atlas.lyricsEnabled` is LIVE, through `AtlasConfig.LyricsTierActive`.** Its
  two halves disagreed: `/api/jobs` read the flag live while the sweeper took it
  at boot inside the `if` that decided whether to wire the sink. Invisible while
  a restart was the only way to change it — and adding a `settingsPatch` field is
  exactly what makes it bite. One predicate, both readers.
- **`/v1/lyrics` exempts network rows from the drift check**, and that is what
  makes the tier work at all rather than a nicety. `lyricsSourceInfo` returns
  the AUDIO file's stat for any non-sidecar source, so a row with no local
  provenance compares zero against a real mtime and answers **410 forever**.
  Binding the row to the audio file only moves the bug — a tagger writing a
  genre changes that mtime and stales a document that never came from there.
  Server-side only; `lyricsDocument` never carried the stat fields.
- **The sweeper's gate is the ABSENCE of a `track_lyrics` row, never the
  verdict.** That is what lets a track whose Atlas document was displaced by a
  local one — or whose local one its owner deleted — come back and be
  re-fetched from its cached recording MBID in one call. `instrumental` is the
  ONE verdict that must exclude by status, because it is a success that
  correctly leaves no row. And the attempt row records the identity it was
  made AGAINST, not just the answer, so a retag invalidates a stale verdict by
  construction — no scanner hook, nothing to remember to call.
- **Corroboration, not duration, is the matcher's discriminator.** Atlas's
  release listing carries no ISRC and no per-track artist, so the keys are
  position, number, title and length. Measured over 752 tracks on 30 releases:
  position+title agreeing gives a 134 ms median duration delta and **100%
  within 30 s**; title-only and position-only give ~1.5 s medians and 82/87%.
  A hard 4 s duration gate was proposed and is WRONG — only 77.9% of 447
  position-matched pairs agree within 4 s (p90 **21.2 s**), so it discards a
  fifth of correct matches. The veto applies ONLY to the uncorroborated tiers.
  A file with no disc number must never be assumed onto medium 1 (3,752 tracks
  here have none), and a title is used alone only when UNIQUE on the release
  (1,275 albums here carry a duplicate title across 2,987 tracks).
- **`pending` is a fact about the upstream, never about the track.** Atlas
  warms on demand: over 32 unseen recordings every one resolved, median
  **6.5 s**, p90 15.2 s, max 18.8 s. So a fixed 2-second re-probe — the
  obvious design — sits below the median and misses most. The sweep warms its
  batch on pass one and collects on pass two, and the 150 ms politeness
  interval IS the delay. Never negative-cache it.
- **Only an upstream that ANSWERED may write a verdict.** `doCapped` returns a
  `*httpStatusError` and `isUpstreamAnswered` reads the CODE: 4xx durable,
  **except 429 and 408**, which are 4xx by number and transient by meaning.
  A 502 on the release fetch otherwise reaches `MatchNone` and parks a real
  album for a fortnight. Same reason `isHTTPNotFound` no longer substring
  matches `": http 404:"` — the error's last field is 512 bytes of response
  BODY, so a failing upstream quoting a 404 read as a clean miss.
- **The sweep stands down while a scan is in flight** (`ScanInProgress`, the
  booklet GC's hook): both write `track_lyrics`, and a sweep landing seconds
  before the scanner extracts a local document bumps `indexed_at` twice for
  one track. The write budget is not a queue guard either — 9,454 addressable
  tracks uncapped is 9,454 delta rows to every device at once.
- **`/api/jobs` snapshots use TTL + singleflight, NOT the diagnostics cache's
  mutex-across-the-query.** They look interchangeable and are not: the db
  context is DETACHED (`context.WithoutCancel`), because the result is shared
  by every queued caller and one client hanging up must not synthesize a
  failure for the rest; a failure serves LAST-GOOD rather than blanking a
  card; and the clock is stamped on failure TOO, or the TTL never trips and
  every poll re-runs a scan that is already failing. `AtlasLyricsStats` is
  25.8 ms at 21,000 tracks — the JSON predicate cannot use the functional
  index on `$.musicBrainzAlbumID` because it is wrapped in COALESCE.

- **Every pass that WRITES checks the scan latch, not just the first.**
  `tickLyrics` re-checked `ScanInProgress` per candidate in pass one and
  broke — leaving everything it had already warmed in `pending`, which
  pass two then drained into `track_lyrics` with the scan under way: the
  double `indexed_at` bump the guard exists to prevent, one loop later.
  The stand-down test seeded only `available` recordings, so its pass two
  was always empty and could not see it. Nothing is stamped for what a
  stood-down pass leaves behind; the candidate query offers it again next
  tick, warm. (#916)

### Enrichment — MusicBrainz, Atlas, artwork, fingerprinting

- **Relaxations belong in the QUERY; strictness belongs in the ACCEPTANCE.**
  Every dangerous idea in this area is dangerous because it sits in the wrong
  bucket. Stripping a leading "The" as a *fold rule* silently accepts any
  `The X`/`X` pair anywhere; as a *query rung* it issues a fresh request and
  still demands the result fold-equal.
- **`internal/enrich/matchfold.go` is COMPARISON-TIME ONLY** and must never be
  persisted, hashed into a filename, used as a cache key, or put in SQL. **Do
  not unify it with its three siblings** — `unicodeLowerScalar` backs functional
  indexes (changing it needs a migration), `ArtistImagePathByName` hashes into a
  FILENAME (unifying orphans every cached portrait), and `normTitle` is
  deliberately weak because its pass REWRITES TAGS. `internal/dupes` and
  `librarycat`'s genre fold are the fourth and fifth members of that family.
- **Never split a head credit on `&`, a bare comma, ` with ` or ` vs `** — all
  appear inside real artist names. `pickBestArtist` validates against the query
  that was SENT, not the original tag, and `foldName` erases commas, so
  `Peter, Paul` matches an unrelated `Peter Paul` at 100 and 186 tracks get the
  wrong MBID. The acceptance layer cannot catch that even in principle; **never
  generating the query is the only defence.**
- **Classify errors before caching or stamping.** Transient (5xx, 429, timeouts,
  ECONNRESET/ECONNREFUSED/ENETUNREACH/EHOSTUNREACH) must NOT `markSkipped` and
  must NOT negative-cache — a 30-second outage would otherwise poison every
  in-flight track permanently. The HTTP-code parser must be structural
  (`HasPrefix` + `Atoi`), never a substring match, or a 4xx whose *body* mentions
  "HTTP 503" retries forever. A clean artist no-match is still NOT cached.
- **Pacing derives from the client's base URL** (`minIntervalForBase`,
  fail-safe to the public interval, dot-anchored suffix match) — public MB is
  1.1s and self-hosted is 150ms — **not zero**, because Atlas's own per-IP tier
  gate is shared with the harvest client. The invariant is enforced by
  construction rather than by a code path remembering; don't put a fixed interval
  back in `NewEnricher`. `Enricher.Run` needs its
  own inter-batch pause too: the pacer only fires when a network call is made.
- **A release-search miss must not cost the track its artist resolution** — the
  two halves are independent and the artist search is the cheap reliable one.
- **`ResetEnrichedMisses` tests THREE arms — artwork, artist AND release MBID.**
  `artworkMBID` is not a proxy: it also carries the scanner's `local-<sha256>`
  sentinel, so 6,801 of 8,945 affected rows were invisible to the two-arm form
  while the handler reported success.
- **A fingerprint identifies AUDIO, so `acoustid.Decision` has nowhere to put a
  release or artwork MBID** — one recording sits under many release groups
  precisely because they contain the same audio. Reach an album MBID only by
  running the existing text ladder with the recovered artist NAME. This is what
  bounds a wrong answer to a wrong portrait rather than a wrong album identity,
  cover, booklet and grouping.
- **Fingerprint suppression is keyed on BOTH inputs and TTL'd**, and the two
  marker kinds are cleared by ONE statement while being READ separately — a
  retry that cleared one and not the other makes the button silently do nothing
  for half the population. Only `ErrNoMatch` and the tag-contradiction veto
  persist; a lookup error is a fact about the upstream and must not sideline a
  file. Retry must clear the in-process cache AND the SQLite rows, or everything
  answered this session stays suppressed until a restart.
- **The local-artist veto lives in `internal/enrich`, not `internal/acoustid`**
  (import cycle), only ever SUBTRACTS, and its junk-tag list is closed and tiny.
  An all-digits ALBUM title is NOT junk though an all-digits artist is:
  misclassifying an artist removes a witness, misclassifying an album
  substitutes the fingerprint's title for the operator's own.
- **MusicBrainz `release-group` is an OBJECT, not a string** — `{id, title,
  primary-type}`. Mock fixtures must match the live shape.
- **Negative-cache PERSISTENT search failures** (store an empty MBID under the
  `(artist, album)` key) or sibling tracks on the same album re-query the same
  inputs and turn a 1-track failure into an N-track spin loop. Transient failures
  must NOT be cached — see the classification rule above.
- **`Retry-After` is honoured on MB and iTunes 429/503, capped at 1h.** The cap
  applies in the SECONDS domain before multiplying (overflow guard) and on
  `strconv.ErrRange`; without it a hostile or misconfigured upstream parks the
  enricher indefinitely. Pacing sleeps are ctx-aware so shutdown isn't blocked.
- **The enricher caches are bounded LRUs, pre-allocated at full capacity**, not
  `sync.Map` — the unbounded form leaked for the process lifetime on a
  multi-decade library. The Deezer negative cache is presence-only and must not
  promote to MRU, or a stale entry outlives a positive re-fetch.
- **An MBID that reaches a path is validated at the site that BUILDS the path,
  and the artwork write is bounded to its own directory.** `ArtworkCachePath`
  makes the value the LEADING component of a `filepath.Join` (which Cleans) and
  `writeArtworkAtomicStream` then `MkdirAll`s the parent — so a traversing value
  CREATES its way out rather than failing. Every caller validated at entry
  except the pair fed by the ATLAS HARVEST RESULTS PAGE, where the UPSTREAM
  chooses the MBID and the bridge never checks it against what it submitted:
  `atlasCoverRefetcher.RefetchPremium` wrote up to `MaxCoverArtBytes` there and
  the stale-tier loop beside it `os.Remove`d two more. Persisted, too —
  `AddPendingCovers` skipped only `""`, so a hostile value survived restarts and
  came back every tick. Three layers now, each negative-controlled separately:
  shape at ingest (`pollResults`) and at the sink, containment via
  `fsutil.IsUnderAny` inside the write primitive, and an image-signature check on
  the body. **`config.go`'s unpinned-`harvestBaseUrl` note bounds the accepted
  risk at CONTENT INJECTION** ("the bios it returns land in `artist_atlas`") —
  that was written against an incomplete model, and an arbitrary file write is
  outside it. Pin `atlas.harvestBaseUrl` on any bridge with harvest enabled.
- **Artwork is JPEG-only and two-layer verified (MIME *and* the `FF D8 FF` magic
  bytes) ON THE SCANNER'S LOCAL FOLDER-ART PATH — that rule never covered the
  NETWORK path, and this bullet read as though it did.** `internal/manifest` has
  `looksLikeJPEG` + the `folderArtCandidates` sniff; `internal/enrich` had no
  content check of any kind, on ANY of its five write sites (CAA release, CAA
  release-group, iTunes, and both premium paths), so whatever an upstream
  returned was stored behind a `*-N.jpg` name that `/v1/artwork` serves as
  `image/jpeg`. The write primitive now refuses bytes that are not a recognised
  image. Deliberately NOT JPEG-only, though that is what this path's contract
  says: CAA can serve PNG, those covers render today because clients sniff, and
  dropping them inside a security fix buys nothing the arbitrary-payload refusal
  does not already buy. `artwork_scale.go`'s rule — **"a verbatim PNG write would
  put PNG bytes behind an image/jpeg label"**, which is why the SCANNER
  transcodes — still applies here and is still unimplemented on the network
  side; the warn line is what stops that staying invisible. Folder-art
  lookup is single-flighted per directory (a `sync.Once` promise stored with
  `LoadOrStore` — **never compute-then-`LoadOrStore`**, which runs N concurrent
  ReadDir+hash per album under contention) and reset per scan. The disc-subfolder
  parent fallback climbs EXACTLY ONE level, gated on an anchored disc-folder
  name and bounded by the absolute library roots.
- **iTunes is a fallback, not a primary source**; artwork cache keys stay
  MB-derived so the wire shape is unchanged.

### Duplicates and the served set

- **`internal/dupes` is a VERBATIM MIRROR of the iOS normalizer** — its output
  must equal the client's partition, so a fix that makes it "better" than the
  client makes it WRONG. Test literals are lifted verbatim from the Swift tests.
- **DSD and PCM are never cross-suppressed**, and `different-audio` (proven
  remasters) never suppresses under any mode. Winner election is a strict total
  order so winners never flap (flapping = `indexed_at` churn).
- **`outranks` has NO availability term** — so enabling cross-source
  suppression would suppress a local rip in favour of a copy on an upstream that
  is powered off half the time. Deciding where availability sits in that ranking
  IS the product decision; don't flip `includeRouted` without making it.
- Tier names are evidence claims: only `identical-audio` may say "reclaimable";
  `inconclusive` is never suppressed.

### Job pools — upscale, optimize, analysis

- **THREE CLI commands feed the same classifier and worker pool, split by
  tier, and their DSD arms are gated per RUN.** `bridge upscale` (PCM →
  higher-rate PCM), `bridge optimize` (PCM → 16/44.1|48, and DSD →
  `optimized-dsd-v1-*` under the flag) and `bridge render` (DSD →
  `pcm-v1-*-24`). `runUpscaleParams.dsdCaps` is probed once per run and its
  ZERO VALUE refuses every DSD source, which is what keeps `upscale` and a
  flag-less `optimize` byte-for-byte unchanged. **The admission itself
  delegates to `transcode.OptimizeEligibleFor` / `PCMRenderEligible`** — the
  same predicates the coordinator's walks and the auto-optimize sweeper use.
  Don't hand-roll a local reading: a source the sweep renders and the CLI
  refuses (or the reverse) is precisely the drift that indirection prevents.
- **"We could not read the decoder listing" is not "your ffmpeg lacks the
  decoders", and `HasDSD == false` means both.** `ProbeFFmpeg` sets
  `DecodersKnown=false` on a timeout, an unparseable listing or a failed exec,
  and `HasDSD` is false in every one of those — so a ladder without its own
  `!DecodersKnown` case reports a confident fact about the operator's BUILD
  that the bridge never established. Three surfaces phrase this verdict;
  `bridge doctor` and `bridge render`'s precheck both call `ProbeFFmpeg`
  directly and keep the error, and both said it correctly. The CONSOLE read
  `FFmpegSnapshot()`, which **discards** the error — hence `FFmpegInfo.ProbeErr`,
  so the cached path can say why. Field-reported against the v0.2.0 Docker
  image, whose Dockerfile asserts all four `dsd_*` decoders plus `dst` at BUILD
  time and fails without them — making the build the one explanation it could
  not have been. The control reproduces the reported message verbatim from a
  probe that merely timed out.
- **A DSD source SKIPS the `soxInfo.CanDecode` check in
  `classifyUpscaleTrack`.** sox cannot open DSF/DFF at all — ffmpeg decodes
  and sox takes the raw pipe — so asking sox would refuse every DSD
  candidate. The route is decided, and fails CLOSED, inside `transcode.Run`
  (`ErrDSDDecodeUnavailable`); this check is layering for the PCM path, never
  the verdict. `ffmpegDSDCLIReady` refuses BEFORE the library walk, which is
  the difference between one honest message and one failed job per DSD track;
  `dst` is a NOTE there, never a refusal, because a build with the `dsd_*`
  decoders and no `dst` renders plain DSF/DFF and skips only DST-compressed
  DSDIFF.

- **The DSD decimation was MEASURED and it does not alias — but the test that
  says so is a DIFFERENTIAL, and an absolute in-band bar cannot replace it**
  (PR B4; numbers + method in `ops/engineering-log.md`). The 5th-order fixtures
  carry **about -104 dBFS of their OWN in-band content** near 20 kHz (the NTF
  rising at the band edge, plus odd harmonics of the test tone — ordinary for a
  1-bit quantizer), so an absolute floor measures the MODULATOR: the first draft
  asserted -110 dBFS and duly "failed" at -91.8 with the pipeline blameless.
  `TestDSDRender_AliasRejection` instead renders the 50 kHz probe through Stage A
  twice — the shipping effect order (taken from `dsdStageAArgs`, not retyped) and
  a reference that low-passes at the 352.8 kHz intermediate BEFORE decimating —
  and asserts the difference. Measured **+0.00 dB** (faithful) and **+0.38 dB
  peak / +0.46 dB energy** (compact), against a 1.0 dB bar. **So the design's
  recorded "`sinc` BEFORE `rate`" fallback is NOT needed; don't take it without
  re-running this.** The coarse `peak <= -80 dBFS` pin beside it exists only to
  catch an anti-alias filter that vanished entirely.
- **`sinc -a 110 -t 10000 -35000`: `-t` is the FULL transition width CENTRED on
  the cutoff, and the stopband is -116.8 dB.** Measured — -6.02 dB at exactly
  35 kHz, flat to 31 kHz, -131.4 dB by 60 kHz — so the 30 kHz passband edge /
  40 kHz stop edge the docblock claims is what ships.
  `TestDSDLowpassSincConvention` is gated on **sox alone** — it synthesises and
  filters with sox and never decodes DSD, so gating it on ffmpeg too would skip
  the convention pin on a host that can run it (CodeRabbit on #866).
- **⚠️ `sox … stats` over a SHORT file after a steep FIR reports the edge
  TRANSIENT, not the stopband — a 61 dB error, in the direction that makes a good
  filter look broken.** The filtered 40 kHz tone reads **-64.56 dBFS**
  whole-file against **-125.85 dBFS** steady-state, i.e. a 117 dB filter measures
  as 55 dB. `soxSteadyStateRMSdB` takes the middle half for exactly this reason.
  Sibling trap in the same sweep: **`-r` must PRECEDE `-n`** — sox binds options
  to the file that FOLLOWS them, so `sox -n -r 352800 … synth … sine 80000` sets
  the OUTPUT rate, generates at sox's 48 kHz default, and aliases the request to
  16 kHz; every stopband frequency then silently lands in the passband and the
  whole sweep reads -0.00 dB.
- **Assert the clip guard on the TRUE PEAK; only log the spectrum.** The
  rendition lands at **-0.97 dBTP** against its -1.0 target (applied gain
  +5.00 dB on the -6 dBFS fixture), which is the end-to-end pin that Stage C's
  `6.0206 + G` agrees with `G = clamp(0, 6, -TP_unity - 1)` — the arithmetic an
  earlier draft got wrong as `6.0206 + G - 6`, which would have shipped every
  rendition 6 dB quiet. The spectrum reads the tone 0.76 dB lower because 1 kHz
  does not land on a bin and Blackman-Harris costs up to 0.83 dB of scalloping,
  so a tone-based bar would have to be loose enough to hide a real error.
- **The fixture generator's modulator is a low-distortion CIFF and it was
  verified BEFORE anything was generated.** A first attempt used CIFB with
  feedback at all five integrators: stable, linear, and **DC gain 0.257**, so
  every fixture decoded ~12 dB low and all three tests failed on real clipping.
  The shipped form is checked by DC probe (`mean(y) == u` exactly,
  `mean(e) = 0.000000`). ⚠️ Between the two, a throwaway harness reported
  "gain 2.0" across three different coefficient sets — **a constant factor across
  every variant of a parameter is evidence about the MEASUREMENT, not the
  subject**; it was a print bug (`want {dc*0.5}` while passing `u = dc`).

- **`Enqueue` fires `fireStateChange()` UNDER the lock, before the unlock**, in
  both pools. Workers are bounded by `Stop`'s `wg.Wait()`; `Enqueue` is not, so
  firing after the unlock lets a preempted enqueuer resume after `Stop` closed
  the channel and panic on send-to-closed — which fires even inside a `select`
  with a `default` (that guard covers a FULL channel, not a CLOSED one). Don't
  move it back to "shrink the lock window".
- **The publisher is one long-lived goroutine, never `go fire()` per
  transition** (which fanned thousands of goroutines under burst). State changes
  coalesce on a cap-1 channel; job-completions do NOT coalesce (each carries a
  unique path iOS keys on) and block for fidelity. `Stop`'s five-step ordering
  is load-bearing, and the publisher must NOT exit on `stopCtx.Done()` or a
  blocking worker send deadlocks.
- **`finishJob` clears the worker slot and releases dedup BEFORE each terminal
  `fireStateChange`** — a `defer Store(nil)` registered ahead of `processJob`'s
  tail runs LIFO *after* that tail's fire and publishes a stale "active"
  frame. `ActiveJob` is immutable after `Store()` and carries a start
  timestamp, not a ticking `elapsedSec`, so the SSE frame stays diff-stable;
  the browser ticks elapsed.
- **In the analysis pool, a job's count and its dedup release are ONE step,
  taken after its bookkeeping** (#987). `processJob` counted a failure, THEN
  wrote the strike and its WARN, and released the path LAST, and `Enqueue`
  then answered a held path with a silent nil. So a caller that acted on the count,
  which is what a retry is, queued nothing:
  `TestASuccessfulAnalysisClearsTheStrikes` timed out on a CI runner after 900
  clean runs on a laptop, and under 3× CPU oversubscription the old pool
  failed 9 runs in 720, six of them on the strike not yet written when
  `Failed` said 1. `analyze.Pool.finishJob` releases and counts in one `p.mu`
  critical section, and `Stats` reads under the same lock. Split the two and
  a window opens in EITHER order: count-first drops the retry, release-first
  shows the job nowhere. Every exit reaches `finishJob` through ONE deferred
  call, a panic included, so no new path picks its own order. The counters
  are plain integers under `p.mu`, so an increment outside the lock is a race
  the detector reports (measured: 15 reports across 11 tests). **Decide the
  outcome where `p.closed` is read, never after the release**: `bridge
  analyze` answers an idle pool with `Stop`, so a later read un-counts the
  run's last job. #947 met this window first (its helper's tests failed 1 run
  in 5) and fixed it in the test HELPER, leaving the pool and the one sibling
  test that did not use the helper. The transcode pool had the same order,
  plus events sent before the release, until #988 (next bullet).
- **In the transcode pool, the count, the release AND the job's event are one
  ordered tail** (#988). Every `processJob` exit counted (atomics) before
  `finishJob`, and the fsync and store exits also sent their blocking
  `jobFailed` before it, under a `fireJobFailed` docblock that said "after
  releaseDedup". Each exit now records one `jobEnd` and returns. The deferred
  tail runs `finishJob` (the ownership-checked `releaseDedupLocked` plus the
  count, one `p.mu` section), THEN `announce` (the event and the state
  change, outside the lock, since the sends block). So a count or an event
  means the path is free, and three consumers act on exactly that. The batch
  Coordinator answers `ErrDuplicateInflight` by DROPPING the path from the
  batch it is building. `POST /v1/upscale` treats it as accepted. And the
  console's `notifyUpscaleProgress` bypasses its refresh throttle only when
  the frame that advanced Done+Failed also shows nothing in flight, so a count
  that arrived first could throttle away a batch's final refresh. **A counted
  job is always announced, an uncounted one never**: `p.closed` is read where
  each exit decides, never after the release (the old runner-error tail
  re-read it there and could count a failure and drop its event). The success
  exit reads it not at all: its row has committed, so it counts and announces
  during shutdown and `Stop` drains the event. The strike and the orphan
  sidecar's removal stay INSIDE the claim, or a retry's fresh output is what
  the old attempt deletes. Measured on the old pool: a busy poll saw the job
  counted while in flight 400 times in 400, and every immediate retry was
  refused, while a 1 ms poll saw it 0 times in 100.
- **Both pools answer a held path with `ErrDuplicateInflight`, and a caller
  that counts gives it a bucket of its own** (#992). The analysis pool
  answered with nil, and `analysisSweeper.enqueueAll` counts every nil as
  enqueued. A track has no analysis row until its job finishes, so every
  sweep during a long first analysis re-offers the whole backlog: the
  `auto-analysis sweep enqueued tracks count=N` line and the Jobs card's
  `enqueued` reported a full queue of old work as new, every sweep, in a line
  whose parts must add up to the track total. They are `AlreadyQueued` now.
  **A new `Enqueue` answer reaches every caller in the same change**: this
  pool has two, and `bridge analyze`'s dispatch stops on any error it does
  not know, so the sentinel alone would have ended a run at the first
  duplicate (unreachable today: each candidate is a distinct track, offered
  once to a pool the run created). `AnalysisSweepCounts` states the contract
  the line relies on (every int field but `Total` is a bucket, and the
  buckets partition `Total`), and `TestDescribeAnalysisSweepAccountsForEveryTrack`
  runs the shipped `describeAnalysisSweep` under node with each bucket
  holding a distinct power of two. A count then identifies its bucket, so
  the test names a bucket the line leaves out or shows under another's
  label, and a part that is no bucket at all, such as one read from a field
  the server does not send.
- **Every job gets its own `context.WithTimeout`, cancelled per job**, or one
  pathological file consumes a worker slot until restart. Shutdown gating reads
  the monotonic `p.closed` flag, NOT `stopCtx.Err()` — `Stop` flips the flag
  before cancelling the context, and in that gap a graceful shutdown would be
  misclassified as a real failure.
- **`JobSpec.Background` is load-bearing and `Kind` cannot express it.** A swept
  auto-optimize job is the same KIND of work as an on-demand CarPlay request
  with the opposite urgency; enqueuing a library-wide sweep on the foreground
  lane re-opens exactly the head-of-line blocking the two-channel queue exists
  to prevent, with no bridge-side symptom.
- **ALAC reaches the pipeline through an ffmpeg pipe, and the completeness
  guard on it is TWO-SIDED.** No stock sox build has an MP4 demuxer, and ALAC
  is the one LOSSLESS format that clears every upstream gate and then cannot be
  decoded; `ffmpeg … -f f32le -` into `sox -t raw` fixes it with the effects
  chain shared byte-for-byte (`soxArgsFrom` takes the input argv, so both routes
  have ONE definition of gain-guard/rate/dither). Three things are measured, not
  assumed: **ffmpeg exits 0 on a truncated source** — a half-truncated faststart
  `.m4a` still reports its full 8.000s from an intact moov and produced 3.901s of
  audio, silently, and the sidecar is keyed on source mtime+size so it would
  never regenerate; **a complete decode is EXACTLY 1.000000** across 44.1/48/96/
  192 kHz, mono and stereo, so a 2% tolerance is generous; and **an input rate
  declared too LOW makes the output LONGER** (a 44.1 kHz source described as
  22050 produced exactly 2.0x), which a lower-bound-only guard accepts while
  committing a half-speed variant — `internal/analyze`'s one-sided form is right
  for its own purpose, this one needs both bounds. **Raw, never `-f wav`**:
  ffmpeg cannot seek back to patch a header on a pipe so it writes RIFF size
  `0xFFFFFFFF`, and sox then prints `WARN wav: Premature EOF` on EVERY successful
  job — noise, and indistinguishable from the real truncation this guards. The
  fallback is an **allowlist of the MP4 family, not "anything sox refused"**:
  lossy is excluded upstream and DSD takes its OWN route (`routeFFmpegDSDPipe`,
  granted only by the fail-closed decoder probe — PR #863; this sentence read
  "lossy and DSD are already excluded upstream" until then), so anything else
  reaching a refusal is a shape neither decoder was chosen for, and routing it
  would turn an honest refusal into a mystery failure. **The probe is gated on the extension**
  — `ProbeSox` is a fork+exec (7.9 ms; `FFmpegAvailable` is 17 µs) and `RunSox`
  documents that it does not probe per iteration, so only a source whose decoder
  is genuinely undecided pays one. `RunSox` returns the settings it actually
  used — the persist site cannot know the route, and a forensic record that
  names the wrong decoder is worse than one that names none. **The doctor
  warning names WHICH binary is absent**: `ffmpeg` and `ffprobe` are both
  required (ffprobe supplies the pipe's geometry and the guard's duration) and
  some distros package them apart, so blaming ffmpeg sends that operator to a
  binary they already have.
- **`redactSoxErr` must know EVERY absolute-path family a job can name, and the
  journal gets the redacted message too.** sox's stderr quotes its argv, so a
  failing job names the absolute source path and — for a DSD render — the
  Stage A scratch under `upscale.tempDir` (or the OS temp dir): a THIRD family
  the two original passes (source, `OutputDir`) could not see, added with the
  renditions and found by the v0.2.0 logging audit. Pass 2b strips the scratch
  directory and the configured tempDir; and `pool: sox failed` logs
  `redactSoxErr(err.Error(), spec)`, not the raw `err` — the raw form put the
  absolute path in the journal beside a `path` attribute that already named the
  file the way the privacy page promises (library-relative). A new absolute
  path in any job's argv needs a pass here in the same PR.
- **Analysis commits only on a length-complete decode**, gated by the probed
  duration — NOT exit code, `-xerror`, or stderr matching. Both decoders exit 0
  on a truncated-but-openable source, and a partial commit is keyed to
  mtime+size so the skip gate never re-analyzes it. Two alternatives are
  empirically disproven: `-xerror` also fails a glitchy-but-COMPLETE file
  (permanent treadmill), and sox stderr markers cannot distinguish truncation
  from a resynced complete file.
- **…and a source that will NEVER decode stops being offered, on the decoder's
  own verdict.** A failed analysis writes no `track_analysis` row and every
  candidate query selects tracks that LACK a fresh waveform, so a truncated
  file was re-selected on every sweep forever: 30 of them produced **1,385 WARN
  lines in 7 days** on one host and re-failed at every start on the next, with
  nothing naming which files to replace. `internal/analyze` classifies the
  failure at the site where the fact is KNOWN (never by matching the message
  later — the enricher's "a 4xx whose body mentions HTTP 503" trap):
  `ErrSourceUnreadable` covers a clean-exit decode materially short of the
  probed duration, and a decoder that RAN and EXITED non-zero. A decoder killed
  by a SIGNAL is excluded — `ProcessState.Exited()` is the split, because the
  per-job timeout and the OOM killer reach no verdict, and the OOM killer picks
  the biggest decode. **Everything unclassified is transient**, so a missing
  sox, a faulted read and a full output volume record nothing; the permissive
  answer is the safe one here, since a transient verdict costs one decode and a
  permanent one costs the operator a file that silently stops being analysed.
  `markUnreadable` deliberately does NOT prefix the message — these strings are
  logged, persisted as `analysis_fail_reason` and rendered in the console.
- **The strike is stamped FROM the manifest row; the walk's LIVE stat overrules
  it.** `RecordAnalysisFailure` takes no size/mtime argument: it writes
  `analysis_fail_size = size, analysis_fail_mtime_ns = mtime_ns` in the
  statement that reads them, so the version a strike records IS the version the
  predicates compare against — binding the live stat would stamp one world and
  compare in another, and a strike written while the scanner is behind could
  never match. (The auto-optimize rule below, one subsystem over.) The
  candidate walk then re-checks against `info`, because it knows what is on
  disk RIGHT NOW: between a repair and the next scan the row still describes
  the broken file, and any disagreement resolves toward ANALYSING. Three
  consecutive verdicts suppress, a 7-day TTL retries (the toolchain is what can
  change the answer for a file nobody touched), and replacing the file re-opens
  it with no flag — that last one is the remedy the console list exists to
  prompt, so a debounce outliving it would be worse than the bug. `--force`
  does NOT bypass it, like the zero-byte gate; `--retry-failed` does, honouring
  `--filter` through the explicit-path form because `--filter` is a SUBSTRING
  match no byte range expresses, and COUNTING under `--dry-run`.
- **The WARN is kept and deduplicated to one line per (path, size, mtime)** —
  the strike count is the log gate (`count == 1`). Identical lines forever is
  the M-SEARCH shape, not the playlist mass-delete shape: every line here is
  the same by construction, so it is suppressed rather than repeated. A
  TRANSIENT failure still warns every time, deliberately — it has no marker to
  dedup against, and silencing a missing sox behind the fix for truncated files
  is how one alarm hides another. Measured end to end with a real decoder: 4
  WARN lines over 5 runs of 4 broken files, and zero work at all from run 4.
- **A NEGATED condition over a LEFT JOIN needs COALESCE, and the sibling terms
  that do not are why it is easy to miss.** `AnalysisCoverage`'s four existing
  terms test `ta.waveform_tag != ''` POSITIVELY, so a join miss yields NULL,
  the CASE takes ELSE and the row scores 0 — right for "is it analysed". The
  unreadable term NEGATES that, and `NOT NULL` is still NULL, so it scored 0 on
  the very population it exists to count: no error, a plausible number, and the
  coverage bar silently unchanged. `TestCoverageSubtractsTheGivenUpOnSet` found
  it. Suppressed tracks are SUBTRACTED from eligible (a remainder that can
  never drain reads as a stuck job) while tracks still being retried stay in
  it.
- **The auto-optimize candidate query is its own thing, not the Inspector's** —
  it must exclude UPnP-routed rows and suppressed rows, and select on "NO FRESH
  variant exists" via a correlated subquery, never a JOIN and never "some row is
  stale". One track can hold several `optimized-%` rows, so the stale form
  re-selects forever and pushes a delta to every device on every sweep.
  Staleness compares against the TRACK ROW, and the sweeper stamps from that
  same row — a live stat makes every variant read stale whenever the scanner
  hasn't caught up.
- **`maxPerSweep` is not just a queue guard**: `UpsertVariant` strict-advances
  `indexed_at`, so an uncapped first sweep pushes one delta row per variant to
  every paired device at once. The disk floor is a RUNNING budget and the probe
  fails CLOSED.
- **Disk pre-flight grades the VARIANTS volume, resolved per call** from the
  live config holder — not `DataDir`, and never snapshotted at adapter
  construction. `AvailableDiskSpaceNearest` walks to the nearest existing
  ancestor, advancing only on `os.IsNotExist` so a real EACCES still surfaces.
- **The eligibility SQL and the Go gates are LOCKSTEP MIRRORS** — change one and
  the other in the same commit; a test in `internal/admin` (the only package
  importing both) fails on divergence.
- **`SetPostScanHook` REPLACES.** Append to `postScanNudges` and register one
  fan-out hook; a second registration silently unhooks the previous sweeper.
- **Every reaper fails closed on an EMPTY referenced set, and the CLI ones offer
  a way past it.** A query that succeeds and returns no rows is not a fault, and
  the routes to it are ordinary: `rm -f bridge.db*` + restart (the reset this
  file documents, and `run` takes a boot tick), or the window between a root
  flip's `WipeFilesystemTracks` — which CASCADEs `track_variants` — and the
  rescan. `runArtworkGC` and `VariantWatcher.tick` had the guard;
  `OrphanSidecarSweeper.tick`, `upscale --gc`'s FORWARD sweep and `analyze --gc`
  did not, and `upscale --gc`'s reverse guard fires only after the forward sweep
  has already unlinked. An empty set over an EMPTY directory stays a silent
  no-op. A BACKGROUND sweeper gets no override (nobody is in the loop to express
  intent); the CLI ones take `--allow-empty`, and a sweep test pins that every
  `--gc` command offers it — which is how `artwork --gc` was found to have
  carried an un-escapable refusal since it was written.
- **A sidecar walk prunes dot-directories at the WALK.** With `variantsDir` on
  its own volume, `.Trashes/<uid>/` and `.Trash-1000/` sit under the walk root,
  so any `.flac` inside one is missing from the catalog and older than the grace
  — files an operator put in the Trash to get back. Gate on `d.IsDir()`:
  `SkipDir` returned for a FILE skips the rest of its parent directory and ends
  the sweep early. `filepath.WalkDir` does not follow symlinks, so a symlinked
  `.Trashes` needs nothing extra.
- **No server-side transcoding, ever.** Conversion is offline; `/v1/download`
  serves bit-exact via `http.ServeContent`. The DSD → PCM renditions (PR #863)
  are conversion in exactly that sense — a sidecar the job pool built earlier,
  served as a file — never a decode in the request path.

- **Every consumer of `variantsDir` resolves it LIVE — the integrity
  watchers included.** `POST /api/upscale/variants-dir` is hot, and the
  enqueuer, coordinator and auto-optimize sweeper read `liveVariantsDir`;
  `VariantWatcher` and `OrphanSidecarSweeper` took the path at
  construction, so after a move the orphan GC walked the tree the operator
  had left while new sidecars landed where it never looked, and the
  mount-loss guard probed the wrong volume. Both constructors take a
  `func() string`. A root change drops the sweeper's chunk-resume cursor —
  a position in ONE tree; against another root `dirEntirelyBehindCursor`
  can prune that whole tree as "already swept" — and an empty answer is a
  refusal, never `WalkDir("")`. The Jobs chips gate on the INTERVAL, as the
  wiring does, not on `UpscaleStats()`, which is nil with upscale off while
  the watchers tick regardless. (#917)

- **A recorded sidecar path is a CLAIM about where the file was, never
  proof that it is gone.** `track_variants.sidecar_path` and
  `track_analysis.waveform_path` are ABSOLUTE, so a `bridge.db` copied to
  a host where the variants dir (or dataDir) has a different path reads
  ENOENT on every row while every file sits, byte-identical, at its
  source-mirrored place under the current directory. Field report
  2026-09-20: the boot sweep reaped all 10,248 rows (259.7 GiB of
  renditions) with NO log line — the mount-loss guard saw a healthy,
  full directory — and the auto-optimize sweeper re-rendered over 200
  good files before it was caught. **Every consumer that turns a missing
  recorded path into a deletion asks `integrity.LocateSidecar` first**
  (FIVE of them, and the enumeration has already been wrong twice: the
  three reapers `VariantWatcher.tick`, `upscale --gc`'s reverse sweep and
  the reactive reap in `serveVariant` via the cmd/bridge
  `variantStoreAdapter` — plus, since #959, `DELETE /v1/upscale/variants`,
  which unlinked the recorded path alone and read its ENOENT as
  already-gone, so a relocated catalog answered `deletedCount: 10248,
  freedBytes: 0` and stranded the tree where `--gc` then refuses it; and
  the serve reap's own MOUNT check, which the other two had and it did
  not, so an unmounted volume cost one row per PLAY), and **every forward sweep's known
  set carries the CANONICAL path beside the recorded one**
  (`integrity.KnownSidecarSet` — the orphan sweeper and `--gc`'s file
  walk; `analyze --gc` for waveforms), or a moved tree is 10k orphans to
  the file walk and gets UNLINKED, which is worse. `transcode.
  VariantSidecarPath` is the ONE layout (the pool, `variants move`, the
  probe); don't hand-copy the `Join(dir, Dir(rel), basename)` body.
  Relocated = canonical exists with the recorded `size_bytes` → adopt
  (`UpdateVariantSidecarPath`, no `indexed_at` bump, nothing on the wire);
  a size MISMATCH is a copy in flight → keep the row, adopt nothing,
  delete nothing (the serve lookup answers `api.ErrVariantSidecarUnavailable`
  → 410 without the reap); delete only when NEITHER location has the file.
- **A sidecar walk that feeds a deletion RESOLVES ITS ROOT, and reports
  under the configured one.** `filepath.WalkDir` `Lstat`s its root and
  follows no link, so a variants directory that is itself a symlink
  (`/srv/variants -> /mnt/vol/…`, the ordinary mountpoint alias) arrives
  as ONE non-directory entry and the walk ends — measured, not reasoned
  about. `upscale --gc` passes `SidecarInventoryOptions{}`, so `Consider`
  is nil and that entry is a candidate: the variants directory itself was
  counted, missed by the known set, and handed to the forward sweep to
  `os.Remove`. ONE orphan is below the mass-orphan floor, so no guard
  could see it, and `bridge doctor`'s `variants-index` probe reported it
  as reclaimable and named `--gc` in the hint. `TreeHoldsVariantSidecars`
  has resolved its root since #937; #940's shared walker — **the one that
  DELETES** — did not, so `resolveSidecarRoot` is now one body for both.
  **Both halves are load-bearing**: walk the RESOLVED root so it descends,
  REPORT under the configured one, because `KnownSidecarSet` keys on the
  recorded `sidecar_path` and on `CanonicalSidecarPath(variantsDir, …)`
  and neither is symlink-resolved — emitting resolved paths would miss
  every key and classify a healthy tree as orphans, which is worse than
  the bug. A dangling root is ENOENT and takes the missing-root reading;
  neither error branch may fall back to the unresolved path, which IS the
  defect. A symlinked SUBDIRECTORY is skipped rather than classified
  (unlinking it takes a subtree's only reference), and a symlink that
  cannot be STATTED is counted `Unreadable` rather than classified — "not
  there" and "could not find out" are different questions and only the
  first is junk. `analyze --gc` was never exposed: its `Consider` requires
  `.1bwf`. (#959)
- **A sweep that would reap more than `integrity.variantSweepMaxDeletePercent`
  (default 20, floor 10 rows) of the catalog while the tree still holds
  sidecar-shaped files is REFUSED, and every tick that saw rows logs one
  summary line** — `integrity.MassDeleteRefusal`, the one decision both
  reapers make; `--allow-mass-delete` is the CLI's way past it. **In `runGC`
  the guard is a PRE-FLIGHT over the whole gc, never inside the reverse
  half**: the forward sweep runs first, and for a tree copied into a
  layout the probe doesn't know it unlinks the very files that are the
  guard's evidence, then hands the reverse sweep a tree that "holds no
  sidecars" — the first draft's test went green while the files were
  already gone (`TestRunGCRefusesAMassDeleteUntilAllowed` now asserts on
  the FILES). The summary is Warn when it deleted or refused, Info
  otherwise; per-row lines are sampled at 10 per message per tick, the
  M-SEARCH lesson applied before the flood. `waveform_path` has the same
  shape and adopts too, by a narrower route: #954 wired
  `integrity.LocateWaveform` into `analysisStoreAdapter`, so a relocated
  curve rebinds on the first analysis lookup — served from canonical, row
  rewritten, no `indexed_at` bump, nothing re-decoded. There is no
  proactive waveform sweep, so it heals when the track is next asked for
  and not before, and the analysis skip gate still reads the row rather
  than the file — which is why a curve at NEITHER location stays silent
  and needs `bridge analyze --force`. `bridge doctor`'s `sidecar-paths`
  check reports both tables; the schema-relative follow-up is still #938.
  (#937, #954)
- **`sidecar-paths` counts RECORDED PATHS and stats nothing, so it must not
  be described as a list of files that are gone** (#972).
  `CountVariantsNotUnderPrefix` / `CountWaveformsNotUnderPrefix` are pure
  SQL, so a row whose file is still AT the path it records is counted too —
  served from there, never relocated, in the count forever, which is the
  ordinary shape of a dataDir change with the old tree still mounted. The
  waveform hint said "Rows still listed afterwards point at curves which are
  NOT there" and named `bridge analyze --force`: hours of decoding to
  rebuild curves that already play, for an operator who had just done what
  the same hint told them to. Say what the number MEASURES before saying
  what to do about it, and scope `--force` to a curve at NEITHER location.
  The variants half carried the same false claim beside a remedy that was
  already right — which is how the next change gets made on the same
  reasoning.
- **The delete handler LOCATES before it judges the volume, and only the
  already-gone path asks** (#968). `SidecarStoreState` answers "is a MISSING
  sidecar evidence about the file or about the volume" — a question about a
  file that is missing. Asked per row BEFORE the locate it also answered for
  rows whose file had just been found, and an EMPTY variants directory is
  one of its unavailable reasons: a directory repointed at a fresh folder
  with every rendition still at its recorded path made "delete all
  renditions" unlink nothing and leave the bytes, which is #959's own
  stranding one commit later. An empty recorded path is still asked — 
  `locateRecordedFile` stats `""` (ENOENT everywhere) and then the canonical
  path, which is exactly where a file with no recorded path would be found.
- **EMPTY is explicable only by an unlink from the SAME directory
  INSTANCE.** Two things had to be narrowed, both found by review. A
  recorded path can name the OLD tree, so unlinking there explains nothing
  about the current directory (`VariantSidecarLocation.WithinStore`,
  `filepath.Rel` containment so `/srv/variants-old` is not a child of
  `/srv/variants`). And a clean unmount reverts a mountpoint to an empty
  LOCAL directory, so an unlink at row k proves the volume was mounted at
  row k and says NOTHING about row k+1 — which is why the probe is per row
  in the first place. `SidecarStoreIdentifier` is opaque because identity is
  device+inode on POSIX and volume+file index on Windows, which
  `os.SameFile` answers portably and no exported type carries as a value;
  nil or foreign is NOT the same, because the compare exists to refuse an
  unmount. Captured on the FIRST in-store unlink only — a re-probe per row
  puts a stat on the happy path of a whole-library delete and a differing
  instance is refused by the comparison anyway. Missing, unreadable and
  not-a-directory still refuse however much was unlinked: the loop removes
  files and never directories, so it cannot be what took the root.
- **A sidecar walk tests "not a REGULAR file", never "is a symlink"** (#969).
  Since Go 1.23's `winsymlink` change a Windows directory JUNCTION
  (`mklink /J`, `IO_REPARSE_TAG_MOUNT_POINT`) gets `ModeIrregular` and is
  denied `ModeDir`, so it reports `IsDir()` false with NO symlink bit, fell
  through every arm in `TakeSidecarInventory`, and `upscale --gc`'s nil
  `Consider` unlinked it as an orphan file — taking the only reference to an
  album parked on another volume, which is the ORDINARY way to do that on
  Windows (a real symlink needs a privilege a service account lacks). One
  junction is below the mass-orphan floor, so no guard could see it.
  **`os.Lstat` is the wrong probe here**: it does not follow the link, so
  `IsDir()` is false for a link TO a directory and the skip never fires —
  proposed in review, and it turns two tests red. The decision is taken as
  `(mode, stat)` so the Windows shape is driveable on any platform; a plain
  file answers without a stat, and the closure does not escape (measured).
- **`SidecarInventory.Unreadable` is a count of ENTRIES, and BOTH sweeps say
  so.** It covers a directory the walk could not descend into AND a
  non-regular entry it could not stat. `upscale --gc` and `analyze --gc`
  print that one count from two places and both called it directories; a
  wording fix reaching one of them is the enumeration failure this file
  keeps recording.
- **A forward sweep's denominator is the TREE, never the catalog, and the
  term that knows a lost index is `orphans > rows`.** Adoption and the
  canonical known set both need the ROWS; the 2026-09-20 aftermath had
  none — the boot sweep had dropped all 10,248 and the auto-optimize
  sweeper had written 200 fresh ones over the stranded tree, so
  `gcRefuseEmptyKnownSetOverPopulatedDir` ("is the catalog EMPTY?") said
  no, `gcRefuseRelocationInProgress` ("how many ROWS lost their file?")
  said none, and `--gc` unlinked 10,048 files at exit 0.
  `integrity.MassOrphanRefusal` is the one decision both file-deleting
  sweeps make: the same floor of ten, more than
  `variantSweepMaxDeletePercent` of the FILES (one knob, same meaning at
  both ends — leaving the unrecoverable half on a second number is a
  trap), AND more unreferenced files than the catalog has rows in total.
  That last term is what passes an ordinary crop — a naming-scheme change
  leaves one old file per CURRENT row, an interrupted bulk delete one per
  DELETED row — and it is load-bearing for `--allow-mass-delete` too,
  where `orphans == rows` must proceed. Unlike the reverse twin it is NOT
  gated on `TreeHoldsVariantSidecars`: there the probe tells a relocation
  from a deletion, here the files are the evidence. **The walk is split
  from the unlink** (`integrity.TakeSidecarInventory` classifies, the
  sweep removes the list it is handed) for #937's own reason one layer
  up — a guard inside the walk measures a ratio from the tree it has
  already destroyed, so the test asserts on the FILES.
  `--allow-mass-orphans` is the CLI's way past it on upscale / optimize /
  render / analyze; `artwork --gc` is exempt BY NAME in the sweep test
  (content/MBID-keyed, no absolute path a relocation can strand), and the
  background `OrphanSidecarSweeper` is deliberately left for its own
  change — chunked at 5,000 entries it cannot see a whole-tree ratio, a
  per-chunk one is a different statistic, and a background sweeper gets no
  override, so it has to be right first time. `analyze --gc` shares both
  halves and gained the dot-directory prune and a fail-closed walk error
  with them; its `.tmp` scratch is removed unconditionally and kept OUT of
  the ratio, or a crashed run trips the guard on the next one. (#940)
- **`bridge doctor`'s `variants-index` is the other side of
  `sidecar-paths`, and its walk is BOUNDED.** `sidecar-paths` counts rows
  recorded outside the current directory (a relocation the sweeps heal);
  this one counts FILES no row mentions (a relocation nothing can heal —
  the evidence that would connect them is what went missing). It reuses
  `KnownSidecarSet` + `TakeSidecarInventory`, which is what keeps it quiet
  on a merely relocated catalog, and takes its "the sweep would refuse
  this" wording from `MassOrphanRefusal` rather than restating the rule.
  `/api/doctor` is fetched on every settings-page render, so the walk caps
  at 20,000 TRAVERSED entries — directories and ignored files included,
  never just the candidates, or the cap bounds nothing on a tree of
  either and the guarantee holds only for callers that happen to pass a
  nil `Consider` (measured: 110 ms unbounded over 100,001 files in 111
  directories, 21 ms at the cap; ~1 s at the cap on the sweeper's
  recorded 50 µs/entry pathological tier) — and SCOPES its claim when it
  truncates: the
  all-clear is what gets scoped, never the alarm, because a lost index has
  orphans throughout. **The probe reports the budget it ran under**: a
  closure wired with 0 walks the whole tree on a page render and every
  count still looks right, which is the one mistake no number reveals.
  (#940)
- **A guard that reads the world AFTER a sweep has to be told what the sweep
  did.** `gcCheckOutputDirBeforeReverseSweep` reads a missing-or-empty variants
  directory as a lost mount — right, except that on the legacy hash-flat layout
  (`<dir>/<hash>-<variantID>.flac`, no subdirectories) the FORWARD sweep runs
  first and can remove the last file, so it refused on a state that run had
  just created. No re-run cleared it — the directory was still empty, so it
  refused again having removed nothing — and **no flag reached it either**
  (measured: `--allow-mass-delete`, `--allow-mass-orphans` and `--allow-empty`
  all still wedged; only `mkdir "$dir/.keep"` got past — a plain `touch` does
  NOT, because the `--gc` inventory passes a nil `Consider`, so a dot-FILE is
  unlinked as an orphan while only dot-DIRECTORIES are pruned at the walk). Those rows could not
  be reaped by `--gc` at all, and `manifest clear-missing` is not a route —
  `ClearMissingCounts` deletes from `tracks` + `folders` and never touches
  `track_variants`. The source-mirrored layout hid it for years: `WalkDir`
  removes no directories, so an emptied subtree still leaves dirents behind.
  It now takes the forward sweep's `removed` count and skips the refusal for
  **EMPTY alone**; missing, unreadable and not-a-directory still refuse however
  much was removed, because the forward sweep unlinks files and never
  directories (`TakeSidecarInventory` hands it none), so it cannot be what took
  the root — something else did, mid-run, which is the hazard.
  `integrity.VariantsDirSweepBlock` is the typed answer and
  `VariantsDirSweepBlockReason` delegates to it, so the two cannot drift. The
  serve-time `VariantWatcher` has the same shape one layer over —
  `OrphanSidecarSweeper` can empty a flat directory under it — and is
  deliberately left: it removes nothing itself, so it has no count to be told,
  and the CLI is the repair tool. (#941)

### DLNA, UPnP and discovery

- **The three spec-mandatory ContentDirectory introspection actions
  (`GetSearchCapabilities` / `GetSortCapabilities` / `GetSystemUpdateID`) must
  stay declared.** Strict control points poll `GetSystemUpdateID` between
  navigation steps and abandon the drill on a SOAPFault 401 — the symptom is
  "tap does nothing", with no error anywhere. Empirically validated against
  mconnect. (`Search` IS implemented now — check the code before believing any
  doc that says otherwise.)
- **ObjectIDs must stay numeric** (`FolderObjectID` hashes to a decimal string);
  a non-numeric prefix like `"f-"+hex` re-opens a silent int-parse rejection at
  every drill-down level. Root advertises exactly two
  containers and `BrowseMetadata`'s childCount must equal that count.
- **`/dlna/artwork/{key}` is `/v1/artwork/{key}`'s read path under the DLNA
  mux, and the CDS emits `albumArtURI` ONLY when that route is mounted.**
  `api.(*Server).ServeArtwork` is the one implementation (the v1 handler is
  a one-line wrapper; `TestServeArtworkAnswersExactlyLikeTheV1Route` pins the
  two byte-for-byte across hit, the three miss shapes, bad key and alias);
  the dlna package takes it as `ServerConfig.Artwork` (an interface, so it
  never imports api) and gates BOTH the mount and the `<upnp:albumArtURI>`
  emission on that one field — never on a track merely carrying a key,
  because a strict renderer that 404s the URI may decline the whole item
  (the PR #560 duration lesson). The key is `artworkVersion ?? artworkMBID`
  (`TrackInfo.ArtworkKey`), i.e. the value iOS stores as `Album.artworkHash`,
  so the app's renderer-facing URI and the bridge's own DIDL compose the
  same URL; the URL is built per REQUEST against `r.Host` like `<res>`.
  Folder containers advertise the first keyed DIRECT child in path order —
  no descent, so a multi-disc parent does not inherit disc 1 by sort order.
  `dlnaArtwork` in `/v1/health` is AND-gated (`dlnaEnabled && artworkDirs`),
  and iOS must gate its own emission on it, not on `dlnaServer`. No demo-mode
  branch is needed: the listener never starts in public mode.
- **The folder index is built LAZILY per Browse call** — pre-building it at the
  top of `handleBrowse` puts an O(N) walk on the flat-list hot path.
  `TrackInfo.RelativePath` is the load-bearing source in production; the
  LCP-derived fallback silently strips a top-level folder.
- **`upnpproxy` relays verbatim** (that IS its bit-exact contract) and sets
  `CheckRedirect: ErrUseLastResponse` — without it a rogue LAN upstream can aim
  a `<res>` fetch at the bridge's own no-auth loopback admin API, reachable
  unauthenticated. A caller needing a different Content-Type wraps the writer;
  don't change the package.
- **Both discovery clients track in-flight detail fetches in a `WaitGroup`, and
  `cache.Clear()` runs UNDER `runMu` as `Stop`'s final act.** Without the group, a
  fetch that already passed its ctx check upserts AFTER `Stop` cleared the cache —
  a ghost server that never ages out. Without `Clear()` under the lock, a
  concurrent `Start()` slips through the gap, spins fresh loops that upsert, and
  the stale `Clear()` wipes them. Every fetch spawn goes through the
  `spawnDetailFetch` helper that does the `Add(1)`; a missed site panics with a
  negative counter.
- **Both SSDP read loops share `discovery.HandleReadErr`** — timeout resets the
  streak, `net.ErrClosed`/ctx exits, anything else logs with a ctx-aware backoff
  and one escalation. A bare `return` on a transient error kills discovery for
  the process lifetime; no backoff hot-spins a core. Keep the policy in the
  shared helper.
- **The M-SEARCH SEND failure log is streak-suppressed** (first at Warn, one
  Error at ~10 min, then silence until recovery). It runs on a ticker and its
  failure mode is persistent by nature: unsuppressed it produced **199,078 of
  the last 200,000 log lines**. The cost isn't disk — it's that every other line
  becomes unfindable.
- **`upnp_track_routing.server_udn` holds the ingest's `StableServerKey`, NOT the
  device's raw UDN.** They are equal only for a device whose UDN is already
  lowercase, and never for a manually-configured server (`manual:<sha256(url)>`).
  The SSDP cache is keyed on the raw UDN, so handing a routing key to a raw-UDN
  lookup reports "offline" about upstreams that are up. Anything wanting both
  membership and liveness must carry BOTH spellings — one lookup cannot serve
  both. `librarycat.SourceID` prefixes `"source:"` before hashing so a routing
  key can never collide with an album or artist id.
- **`ServerCache.Upsert`'s merge must carry EVERY descriptive field, and the
  test that pins it is reflective.** The SSDP handler refreshes a known UDN
  with `{UDN, LastSeenAt}` on every announcement and the merge preserved the
  cached fields one by one — `DeviceUDN` was added to `ServerInfo` and not to
  the list, so any partial refresh blanked it. Latent today (only the
  manual-URL poller sets it, under a key no SSDP refresh lands on), and the
  next field would have met the same list.
  `TestServerCacheUpsertPreservesEveryDescriptiveField` fills every string
  field by reflection, so a field is covered the day it is declared.
- **A path component that sanitizes to NOTHING is a folder that collapses onto
  its parent.** `sanitizePathComponent` returned "" for a control-only title
  and `joinPath` skipped the empty component. Unreachable through the DIDL
  walk — `encoding/xml` refuses every byte the sanitizer drops except the
  whitespace `TrimSpace` removes first — but it is a plain function any future
  caller can hand a raw string. Now: the floor is `"_"`, and both callers use
  `sanitizePathComponentOrEmpty` + the ObjectID fallback AFTER sanitizing
  (`containerPathComponent`, `synthesizeFilename`), which is unique where the
  floor is not. Pin the fallback through the helper, not the walk: the walk
  fixture cannot construct the input.
- **A manually-configured upstream is cached under that same `StableServerKey`**,
  which is what makes routing rows, telemetry, `LiveHost` and the online chip all
  work from one insertion point. Implementing that path took three surfaces, not
  one — the walk, `LiveHost` for playback, and status. (Until PR #824 `ingestOne`
  refused it outright; **this entry still said "CONFIGURED, VALIDATED and
  UNIMPLEMENTED" a day after that shipped** — the fourth stale claim of the kind.
  Check the code.)
- **UPnP ingest is skip-if-unchanged**, and `walkFieldsEqual`'s exclusions are
  load-bearing: `ModTime` (stamped at walkStart — including it defeats the skip)
  and the enricher-owned MBID fields (including them marks every enriched row
  changed forever). The ROUTING row is still upserted every walk. Baseline load
  failure degrades to nil, never fatal.
- **A reap grace expires for a fact about the UPSTREAM, never about our own
  READ.** `walkLooksImplausible` is true for two reasons — the upstream reported
  far fewer tracks than we last saw, and `ListUPnPTracksByServer` failed so the
  baseline is unknown — and `reapAuthorized` gave both the same 6 h window. At
  the default 6 h cadence the second or third tick authorized a reap against a
  baseline nobody had seen, which is the guard's own docblock inverted; pair it
  with a silently-partial walk (a container that Browses empty mid-tree: no
  error, no `stats.Truncated`) and the sweep deletes every unvisited row. The
  reasons are now named and only `implausibleShape` can expire. Same rule as the
  scanner's, one layer up.
- **Routed rows fill artist/album from the container path via `dupes.Resolve`,
  never manifest's `fillFromPath`** — the scanner's two-directories-up rule puts
  "CD1" in the album field, while `dupes` strips disc folders and is what the
  catalog and iOS already resolve through, so the fill writes the values already
  on screen and cannot regroup anything. **Fill only; never rewrite a field the
  upstream supplied**, and never persist the `dupes.UnknownArtist` /
  `UnknownAlbum` display sentinels. Persisted, they become the enricher's search
  terms, which resolves *some* release for "Unknown Artist" and attributes its
  cover, bio and booklet to an arbitrary track — a wrong answer that looks like
  a right one, so nobody reports it.
- **A routed track's variant-skip reason says "routed", and that case comes
  FIRST** — DIDL supplies no bit depth, so every routed FLAC otherwise reports
  "format unreadable" about a file whose format is perfectly well known.
- **Chord 2Go's file fetch identifies as generic MPD**, so the `chordFamily`
  matchers never fire on real 2Go traffic — it lands on `mpdGeneric`. Harmless
  today (identical MIME maps, no enforcement), but **any future enforcement keyed
  on the Chord profile silently won't apply to real Chord hardware**. Don't
  "fix" it with a version-anchored `Music Player Daemon 0.21` UA matcher, which
  rots on the next firmware.
- **The renderer cache's controlURL refresh is NOT a copy of the server-side
  fix** — a failed re-fetch upserts a stub whose merge advances `LastSeenAt`
  while keeping the dead URL, pinning it forever.
- **`IsLANEligibleInterface` scans all addresses then decides**
  (`hasPrivate || (hasLinkLocal && !hasPublic)`). The obvious simplification
  regresses the no-usable-address cases, and disqualifying on any public IPv4
  breaks dual-stack home LANs where SLAAC hands out a public IPv6.

- **A folder's children sort by `RelativePath`, with `AbsolutePath` only
  as the tie-break.** Every UPnP-routed track has an EMPTY `AbsolutePath`
  (`dlna_wiring.go` leaves it so the proxy fast-path takes over), so a sort
  keyed on it alone left a routed folder's children — and the "first keyed
  direct child" its `albumArtURI` comes from — in arrival order, under a
  comment that said "sorted by path". (#919)

### Config, settings and process lifecycle

- **A hardening step on a CREATION path protects nothing that already
  exists.** The `chmod 711` for the hosted product's media parent shipped
  inside `bridge-tenant create`, so it reached no existing host — including
  the one it was written for, where the directory was still world-listable
  after the deploy. Anything meant to be true of a host belongs in the
  installer, which runs on every deploy. Same shape as a migration that only
  fires on a fresh install.
- **On a bridge somebody else runs, hiding a control is not refusing it.**
  `deployment.managedSettings` covers settings FIELDS;
  `deployment.managedControls` covers ACTIONS (`restart`, `updates`, `roots`,
  `variantsDir`, `backups`) — hidden by the console AND refused by the
  handler, because each is a plain authenticated request a session holder
  can send by hand. The gate is applied at the ROUTE TABLE, which is where
  you go to ask what a session can do. **`POST /api/roots` is the one that
  was a security hole**: it takes any absolute directory that exists (no
  containment rule, deliberately, for a NAS mount), and the byte routes then
  serve what is under it — on a shared host, every world-readable file on
  the box. An unrecognised control name is a no-op PLUS a startup warning,
  never a config error: refusing would make a binary rolled BACK during an
  incident fail to start for every tenant at once. The typo guard therefore
  lives in the program that WRITES the file (the conductor's
  `managed_controls_test.go`), pinned in both directions.
- **A managed deployment must not be given advice that needs a shell on the
  host.** `bridge doctor` run as a live tenant produced eleven `ok` and two
  `warn`, both unactionable — "no user systemd session, use `bridge init
  --no-service`" and "no browser opener, install one" — so a healthy
  appliance read as two problems. Those two and `log-file-size` (which also
  prints a host path into the tenant's report) skip on `Deps.Managed`; the
  log EXPORT refuses for the same reason, ahead of the terminal / journald /
  `docker logs` branches, and for all three export routes rather than only
  status. Same for the Diagnostics `/metrics` pointer, which is
  loopback-gated and answers 403 to the reader being told to scrape it.
- **The console must SEND only what it SHOWED.** The settings Save payload is
  an explicit allowlist naming every field, `hideManagedSettings` sets `hidden`
  on the enclosing `.field` rather than removing the input, and a hidden input
  is still in `FormData` — so every managed field was supplied on every save,
  and the PATCH is refused WHOLE when any managed field is supplied. Renaming
  the library on a managed bridge failed with nineteen field names the operator
  never touched. `dropUnofferedFields` drops any key whose control is absent or
  inside something hidden; `dlnaEnabled` had done this for its own disabled-
  checkbox case since PR #342 and it was never generalised. No Go test can see
  this — the payload is built in JS — so it took driving the real form.
- **Hiding a settings field leaves its heading and its prose behind.**
  Sections are flat siblings, so `collapseEmptySettingsSections()` hides a
  heading only when its section had a `.field` and every one is now hidden,
  and hides a jump link whose pane has nothing left. Verify this class of
  change IN A BROWSER — the Go suite cannot see it, and dropping the restart
  button server-side left two unguarded `restartBtn.hidden` writes that turn
  a successful save into "Save failed".
- **Never split a config field's halves.** Either EVERY consumer reads it live
  or every consumer takes it at boot. Hot-applying a cheap struct field while
  reporting `restart` makes `/v1/health` advertise a capability in the same
  breath the settings response calls the change pending. This is why there is no
  `partial` status — the rule removes the case instead of naming it. The
  field → apply-semantics matrix is **`ops/settings-apply-semantics.md`**, and a
  test drives the real handler for every row in it.
- **When a change cannot take effect, say so** — but only when the outcome
  depended on THIS bridge's runtime state (no sweeper wired; applied-but-inert
  because a toolchain is missing). NOT for "listeners bind once", which is true
  everywhere; twenty near-identical strings is how the two that carry
  information get skipped. The verdict is computed inside the update closure,
  never from a static table, because a table cannot see this bridge's wiring.
- **A cadence provider needs a rearm, or `live` is a lie.** Every loop reads a
  `func() time.Duration` before each wait (a timer per iteration — a ticker
  cannot change period), and the rearm fires only on an actual change. A zero
  interval PARKS a loop rather than ending it; start the ticker unconditionally
  or "disabled" becomes terminal for the process.
- **Config is copy-on-write behind an atomic pointer.** Readers call `Load()`
  per request; writers clone → mutate → validate → save → hot-apply → `Store()`.
  **Never mutate `Load()`'s result in place** — even under a mutex, concurrent
  readers race. Admin mutations must re-`Load()` and clone INSIDE `Server.mu`,
  or a settings PATCH committing between the load and the lock is silently
  reverted.
- **`Validate()` is a pure shape check; accessibility is
  `CheckLibraryRootsAccessible`, called only by `serve`.** Putting `os.Stat` back
  in `Validate` breaks `bridge update` on any host where the daemon user and the
  binary owner differ (the public-mode VPS layout). Loopback installs fail fast
  on an inaccessible root; public installs warn and continue.
- **Cadence ceilings are unit-appropriate and ports admit 0.**
  `time.Duration(n)*time.Second` overflows negative past ~9.2e9 and
  `time.NewTicker` PANICS, crashing `bridge serve` at startup; a single seconds
  cap applied to an hours field still overflows. Port `0` is the documented
  OS-picks-an-ephemeral-port mode used by every `:0` test fixture.
- **Env overrides are DERIVED from the Config struct.** Only `libraryRoots` uses
  the OS path separator; everything else is comma-separated, because
  `customEndpoints` holds URLs and splitting `https://host:7788` on `:` yields
  three fragments that then vanish in validation. Two legacy enrich base-URL
  names are kept as aliases — losing them sends an Atlas-configured bridge back
  to public MusicBrainz at the self-hosted pace.
- **`Uninstall` refuses a system-level install, like `Stop`/`Start`/`Restart` —
  and the four read ONE predicate.** The condition was spelled out in the first
  three and missing from the fourth, and the two POSIX uninstallers touch only
  the fixed USER-level path (`~/Library/LaunchAgents`,
  `~/.config/systemd/user`) and treat a missing file as success. So against a
  sudo install `Uninstall` returned `(userPath, nil)`, the menu printed "service
  uninstalled." with the LaunchDaemon still registered and running, and the same
  flow then offered `os.RemoveAll(cfgDir)` — config, data, certs and TOKENS —
  out from under a live bridge. The Windows arm already reasoned about exactly
  this ("a zombie service reported as a clean uninstall"); the POSIX arms did
  not. The menu now skips the wipe after a refused uninstall and SAYS SO, since
  a silently-skipped step reads as the menu being finished.
  **`NeedsRootFor` is exported so the decision is drivable** — `installedKindForOS`
  probes absolute paths only root can create, so a behavioural test of the
  wiring passes on CI whether the call is there or not (verified: the control
  stayed green). The wiring is pinned structurally by AST across all four entry
  points; the classification is pinned by table.
- **`POST /api/restart` must invoke the same cancellation closure as
  SIGINT/SIGTERM**, never `os.Exit(0)` — that is what honours the `bgScans`
  WaitGroup (SQLite corruption), cleans up in-flight jobs, and flushes the auth
  store's debounce buffer. Same rule for the updater's auto-install restart.
- **Atomic writes: stage, then rename with retry.** `RenameWithRetry` absorbs
  the Windows AV scan-on-close window; the deferred `Close` must be registered
  AFTER the deferred `Remove` (LIFO — Windows won't unlink an open file).
  `GenerateWithOptions` stages BOTH cert and key before renaming either, so a
  failure leaves the existing pair intact; **never pre-delete**, and never run an
  orphan-cleanup `Remove` on a file already committed. Each site keeps its OWN
  `Chmod` / `Sync` / parent-dir fsync — **don't collapse them into a shared
  `WriteBytes`**, which silently drops e.g. the auth store's belt-and-braces
  `Chmod(0o600)`.
- **`auth.Store.FlushLastUsed` reloads before persisting** — the shutdown flush
  otherwise rewrites `tokens.json` from a stale in-memory slice and deletes a
  sibling `bridge pair` mint. A reload failure ABORTS the flush.
- **`logging.Component` resolves `slog.Default()` at LOG time, not construction.**
  Package-level `var logger = logging.Component(...)` runs during package init,
  before `main()` calls `logging.Init()` — a captured-handler shape would lock
  every logger to the pre-Init handler forever and Windows service log redirects
  would never reach them. No `log.Printf` anywhere in `internal/`; that is also
  what makes the CodeQL log-injection dismissal sound.
- **Use SEPARATE `http.Client`s for a probe and the mutation it gates.**
  `Client.Timeout` caps the whole request regardless of the request context, so
  reusing a 200ms probe client for a 30s mutation fails at 200ms. Same class: the
  updater's download client must have `Timeout: 0` and be bounded by a ctx
  instead, or multi-MiB archives die on slow links, permanently.
- **`bridge doctor --json --fix` writes fix progress to stderr** so stdout stays
  valid JSON. `--fix` only does mkdir-class remediations; anything destructive or
  security-relevant stays operator-in-the-loop.
- **Truncate to a UTF-8 boundary with `trimPartialTrailingRune`, not a
  validate-the-whole-string loop.** The loop shape is safe only for input
  guaranteed valid except at the cut; on localized CLI output with interior
  invalid bytes it discards everything after the first bad byte, at O(N²).
- **The Docker image runs as non-root and chowns `/data` in the same layer that
  creates the user** — otherwise `WORKDIR`/`VOLUME` create it root-owned and the
  first-run cert mint fails. Env overrides apply between defaults and path
  resolution so relative paths keep their config-dir semantics.
- **`ReapOrphans`-style directory reapers must refuse an empty root** —
  `os.ReadDir("")` reads the process working directory.
- **The retention reap fails closed on an empty live-token set**, and the two
  empty forms are NOT interchangeable: `nil` deletes zero rows while
  `[]string{}` deletes EVERY row — and the caller builds the dangerous spelling.
  The rest of the retention and compaction rules now live under **### Database
  compaction and retention** — including the CEILING those windows also need,
  which this list did not have and which is the more dangerous half.
- **The binary swap keeps `dst` present throughout** (`Remove(bak)` →
  `Link(dst,bak)` → `Rename(new,dst)`), with the old two-rename path kept only
  as the EXDEV fallback: a power loss between two renames is permanently
  unbootable and rollback-on-boot cannot help, because the missing file IS the
  bridge. On Windows a stop-timeout must best-effort `Start()` again.
- **The new bytes are STAGED beside `dst` before anything is vacated, so the
  no-file window is two adjacent renames and never encloses a copy.** Both
  fallback shapes vacated `dst` first and then called `placeNewBinary`, which
  falls back to a ~30 MiB cross-volume copy plus an fsync on EXDEV /
  `ERROR_NOT_SAME_DEVICE` — so the gap both files described as "the tiny no-file
  window between the two renames" was the duration of a full transfer. **The
  POSIX hardlink path was never affected** (dst keeps resolving through its own
  dentry for the whole copy); the two that were are `swapBinaryViaRename` and
  **Windows, where the hardlink trick cannot apply at all and this is the ONLY
  path** — on the very host `placeNewBinaryWindows`' docblock names ("bridge.exe
  on D: and the data dir under %LOCALAPPDATA% on C:"), every update spent seconds
  with no `bridge.exe` on disk. The in-process restore cannot cover a power loss
  there. Staging tries the cheap same-volume move FIRST, so an ordinary install
  still pays a rename rather than a copy. `swap_test.go` exercised each fallback
  alone and never composed them, which is why nothing saw it — the pin now walks
  all four combinations and asserts the ORDER (copy before vacate), because
  "dst is never absent" is false by construction for the two-rename commit.
- **A swap PRESERVES the mode `dst` already has; it does not impose one.** The
  rename path inherited the extractor's `O_CREATE 0o755`, which IS umask-masked,
  while the copy path chmod'd an unmasked `0o755` — under a comment asserting the
  two matched. Measured under `UMask=0027`, which this repo's own deployment
  runbook prescribes: the same-volume path installed **0750** and the
  cross-volume path **0600**. The service user still execs it, so the bridge
  runs and nothing looks wrong; every other account on the host gets `EACCES` on
  a binary that worked yesterday, with no log line. The divergence is invisible
  at umask 0, so the test sets one.
- **An install of a target already staged on disk is REFUSED, and the refusal
  reads the PERSISTED marker.** `swapBinary`'s EEXIST retry does
  `os.Remove(bak)` before re-linking, so a SECOND install of the same version
  destroys the operator's rollback target and replaces it with a copy of what is
  already live — while `canRollback()` keeps reporting true, because it only
  stats for the file's existence. Measured, not argued: with the guard removed
  the test's `.bak` goes from `0.1.0` to `0.2.0`. Reachable through the ORDINARY
  console flow — `apiUpdatesInstall` does **not** restart (restart is a separate
  operator action), and `u.status.CurrentVersion` is written ONCE at
  construction (`Install` only decorates the local copy it returns), so
  `UpdateAvailable` stays true for the process lifetime and the console keeps
  inviting the click. Three terms, each load-bearing: **`Status=="installing"`
  AND `SwapStarted`** (an armed-but-unswapped marker mutated nothing — the
  Windows SCM-stop window — and `DecideBootAction` reads it as
  `BootClearNotSwapped`); **the SAME target**, because refusing a newer release
  would strand the host on a version it has not booted; and **within
  `recencyWindow`**, the same constant `DecideBootAction` uses for
  `BootClearAbandoned`, so the refusal expires exactly when boot would clear the
  marker and the two cannot disagree about whether it is live. **Marker, not the
  in-memory `pendingRestart` flag** — the flag records THAT a swap landed and
  not WHICH version, so refusing on it blocked a legitimate newer install (caught
  by the positive control), and `bridge update` is a separate PROCESS that sees
  no `atomic.Bool` anyway. `Install` now sets `pendingRestart` for EVERY path;
  it used to be set by `maybeAutoInstall` after the fact, so the admin and CLI
  installs — the two that never restart — left it false. An unreadable marker is
  NOT a refusal: failing closed there would block every install on the host.
- **Booklet GC is skipped while a scan is in flight** — mid-rescan the release
  universe is transiently partial, so GC deletes every filesystem album's
  booklets and re-fetches them next cycle. An empty universe is a deliberate
  no-op everywhere it appears.
- **`PrunePlaylistCoversExcept` is deliberately unwired** — smart-mix retirement
  is reversible and playlist deletion is a revivable tombstone, so either
  trigger silently destroys operator-uploaded content to reclaim a JPEG. An
  AST-based guard fails if a production caller appears.

### SQLite predicates and their indexes

- **A predicate that wants a PARTIAL index must contain the index's WHERE
  expression, verbatim.** SQLite admits one only when a query term MATCHES
  that expression; it does not reason that `analysis_fail_count >= 3`
  implies `!= 0`. v46 created `idx_tracks_analysis_fail … WHERE
  analysis_fail_count != 0` under a comment asserting "every predicate
  leads with `analysis_fail_count != 0` so the planner can use it" — true
  of `analysisFailureRecordedSQL`, false of the suppressed twin, which
  opened on the threshold. `SuppressedAnalysisPaths` uses that predicate
  as its ENTIRE where clause, so the miss was a full scan of `tracks` on
  the hourly sweep and on every `bridge analyze`. Measured, sqlite 3.54.0
  over 5,000 rows: `SCAN tracks USING INDEX …` for the recorded form,
  **`SCAN tracks`** for the suppressed one as shipped, and `SEARCH tracks
  USING INDEX … (analysis_fail_count>?)` once prefixed — a BETTER plan
  than the recorded predicate gets. **Assert on the PLAN, never a
  duration** (a timing test measures the host), and run the control on a
  SIBLING predicate FIRST, or a fixture with no index at all passes the
  real assertion for the wrong reason. Keep a threshold INLINED where a
  derived variant renumbers placeholders by replacing the first `?`. (#965)

### Database compaction and retention

The compact / reap / reclaim trio (#819 / #822 / #829) landed in one day and
had **no section here** — its three rules were scattered across *Scanner* and
*Config*, which is how the two most destructive `DELETE` statements in the tree
came to be filed under "Config, settings and process lifecycle". Everything
below is from the LOUPE run that swept it (#859 / #860 / #861). **Every
operator-facing number this feature reports was measuring something other than
what it claimed**, and none of it had a failing test.

- **`wal_checkpoint(TRUNCATE)` runs AFTER `VACUUM`, never only before.** In WAL
  mode the vacuum's output lands in the WAL, so without the post-checkpoint the
  file does not shrink by a byte and peak disk RISES. Assert on the FILE
  shrinking, not on `freelist_count`, which reads 0 under the broken form.
- **A retention window must have a CEILING, and `beforeNS <= 0` is the wrong
  half of the guard.** The cutoff is `now.AddDate(0,0,-days).UnixNano()`, and
  `UnixNano` is undefined outside 1678–2262; past ~127,455 days it wraps, and
  **145,092 values in [1, 400000] land POSITIVE and greater than now**, which
  makes `DELETE … WHERE started_at < ?` match every row. `999999` — the
  canonical "effectively infinite" placeholder — is one of them. The existing
  `<= 0` no-op catches the harmless negative wrap and passes the dangerous
  positive one. `config.MaxRetentionDays` refuses (never clamps, like the floor
  beside it) and `manifest.ErrCutoffNotInThePast` is the belt. **`ErrNoLiveTokens`
  does not help here** — it guards only the ORPHAN reap, so the registration
  window is the half with no second line of defence.
- **A window past ~56 years is a deliberate no-op, not a bug.** It reaches
  before 1970, so the cutoff is negative and `<= 0` returns "delete nothing" —
  the honest answer for a window longer than the bridge has existed. The
  property to assert is "no accepted value lands at or after NOW", swept over
  the whole accepted range.
- **`freelist_count` is a FLOOR on what a compaction returns, never an
  estimate.** It counts only WHOLLY free pages; VACUUM also repacks intra-page
  fragmentation, which is what SCATTERED deletion produces — and scattered
  deletion is what every reaping path here does. Measured on a 72.5 MB store
  with every second row deleted: `freelist_count = 0` while VACUUM returned
  **36,233,216 bytes, half the file**. The panel said "nothing to reclaim".
  Never render a zero there as an answer to "should I compact"; it answers a
  different, much narrower question. A fixture guard in `compact_test.go`
  (`if pre.FreelistCount == 0 { t.Fatal(…) }`) shows this was hit during
  development and worked around in the test rather than recognised.
- **The compaction's before/after figures are FOOTPRINTS — main file plus
  `-wal`.** An arbitrary share of the database lives in the WAL until a
  checkpoint folds it back, and one open reader is enough to keep it there.
  Measuring the main file alone reported `reclaimedBytes: -2330624` ("the
  compaction added 2.3 MB") and made the headroom guard demand **8,192 bytes**
  free for a vacuum needing ~4.6 MB.
- **A compaction CAN legitimately raise peak disk, so the clamp is truth, not
  tidying.** When the post-checkpoint is busy the WAL is not truncated and the
  vacuum's output sits on top of the original — measured `before=132,823,160 →
  after=133,638,920`. Nothing has been reclaimed *yet*;
  `CompactResult.ReclaimedBytes()` reports 0 and `CheckpointBusy` carries the
  rest. One definition of that subtraction, beside the measurement.
- **A context cancellation during the post-VACUUM checkpoint is NOT a failed
  compaction.** The vacuum already committed; returning an error tells the
  operator the button failed after it did the work. It maps to `CheckpointBusy`.
  The decision is the pure `checkpointOutcome` helper because the branch is
  otherwise reachable only by winning a race, and a decision that cannot be
  driven is a decision nothing pins.
- **The free-space probe takes a DIRECTORY.** `transcode.AvailableDiskSpaceNearest`
  advances only on `os.IsNotExist`, so an existing FILE goes straight through to
  the platform probe. POSIX `statfs` accepts one; Windows `GetDiskFreeSpaceExW`
  opens `lpDirectoryName` with `FILE_DIRECTORY_FILE` and returns
  `ERROR_DIRECTORY` — so `POST /api/database/compact` 500'd on **every Windows
  install**, always. Assert the CONTRACT (what gets handed to the probe is a
  directory), not the symptom, so it fails on every platform.
- **The scan guard is a check-then-act GUARD, not mutual exclusion — say
  which.** It runs one direction only: a scan starting between the caller's
  `ScanInFlight()` check and `Store.mu` still serialises behind the whole
  vacuum, and two concurrent Compacts become two vacuums. A flag the scanner had
  to honour was declined for the reason `bridge restore` narrows its window
  rather than locking — the consequence is a stall, not corruption, and the flag
  would let a wedged vacuum silence the scanner.
- **`GET /api/diagnostics` is a database reader on a 5-second poll**, and its
  docblock denied it for four days with the body's own "Three PRAGMAs" and "Two
  COUNTs and a MIN" comments directly beneath — while `app.js` carried a second
  copy of the same false claim as the JUSTIFICATION for the interval. There is
  no index on `playback_history.started_at`, so the `MIN` turns a covering-index
  count into a full SCAN: **1.0 ms at 18k rows, 9.0 ms at 90k, 39.6 ms at 500k**
  (the three PRAGMAs are 7–12 µs and genuinely free). That block sits behind
  `databaseStatsTTL`, invalidated after a compaction.
- **A TTL that takes a REQUEST context must not cache what a cancelled request
  produced.** Both diagnostics reads take `r.Context()`, so a browser navigating
  away mid-poll — or an aborted bug-report download — fails them and yields an
  "unavailable" snapshot, which the cache then answers to every healthy request
  for the next fifteen seconds. Skip the write on `ctx.Err() != nil` and ONLY
  then: a genuine failure still caches, because repeating a doomed full table
  scan every 5 s helps nobody. The distinction is whether the failure was about
  the DATABASE or about the REQUEST. This one was introduced by the very batch
  that fixed the confident-wrong-answer class, one layer down.
- **Every availability flag needs a test in the FALSE direction.**
  `RetentionCountsAvailable` was only ever asserted true, so hoisting it out of
  its `err == nil` branch left every test green while the field's entire reason
  for existing evaporated. Its twin on the page-stats block did not exist at all
  — a failed PRAGMA rendered as a confident "0 B".
- **A config field with no `settingsPatch` field is invisible to the matrix
  guard.** `settings_matrix_doc_test.go` is bidirectional, but its universe is
  `reflect.TypeOf(settingsPatch{})` — never `config.Config`. `retention.*` sat
  outside that loop while the Diagnostics panel told operators to "set a window
  in Settings", and there was no such control anywhere. **When you add a config
  field an operator is meant to choose, add the patch field in the same PR** —
  or the prose that names Settings is a promise nothing keeps.
- **`Enrich`-style scan guards must strip COMMENTS before scanning JS**, for the
  reason the CSS guards already record: the code beside a fixed string explains
  the defect BY QUOTING IT, so an unstripped scan finds the commentary and
  reports the bug as still present. `stripJSNoise` is the wrong tool when the
  string literals ARE the subject — it blanks those too.

### The CLI and the serve wiring (`cmd/bridge`)

- **`bridge init`'s preflight grades the install that is THERE — all of
  it, not just its certs.** #951/#952 taught it to read the config at the
  target path and copied the cert fields; the `doctor.Deps` literal kept
  hard-coded `APIPort: 7788, AdminPort: 7789` and never set `OwnPIDFile`.
  So a public-mode install on `:443` was graded against two ports nothing
  uses — free, therefore `port-api: ok`, a check passing because the thing
  it guards is ABSENT — and a re-init while the operator's own bridge runs
  found 7789 bound, skipped checkPort's whole "is it us?" ladder for want
  of a pid file, and ABORTED. The call site's own comment named that
  situation as a reason to pass `--skip-doctor`, i.e. the defect worked
  around in prose inside the command whose job is to grade the install.
  `withExistingInstallDeps` carries ports and pid file too, and is named
  for what it does rather than for certs — a name that says otherwise is
  how the next field gets left out. The first-install skip keeps its own
  control. (#963)
- **…and grades the ports it is about to SAVE, which is a different
  question** (#970). The preflight runs BEFORE the keep-or-overwrite
  decision, so it grades the install's current ports. For the certificate
  that is right and deliberate — init does not rewrite the pair on disk.
  For the ports it is backwards: `baseConfig` always seeds the loopback
  defaults and `--public` replaces them, so an install on `:9090`/`:9091`
  was graded on those, passed, and was then handed `:7788` /
  `127.0.0.1:7789`. A second narrow pass (`doctor.RunPortChecks`) grades
  what the config will contain, BEFORE `Save`, so a refusal leaves the
  existing config intact — and only over the ports that CHANGED, since an
  unchanged one was already graded correctly.
- **The "is it us?" fallback must NOT reach a port the run is choosing.**
  `checkPort` answers ok or warn — never fail — whenever the pid in
  `OwnPIDFile` is alive: an unattributable port warns, and one merely owned
  by this uid is reported **ok** (the capability-bound `:443` case it exists
  for). A live bridge binds what ITS config says, so it cannot legitimately
  own a port absent from it; left set, an occupied NEW port read as "our
  bridge is still running", `HasFail` stayed false, and the config saved
  anyway — the check passing because the thing it guards is absent, one
  level in from the defect the pass exists for. Clear `OwnPIDFile` for a
  changed port. `RunPortChecks` takes WHICH ports to grade, because port 0
  is a legal value with its own verdict and cannot double as "skip this one".
- **`configuredPort` asks what an address NAMES; `splitHostPort` asks what
  can be DIALED, and they differ on exactly port 0.** `config.validatePort`
  accepts 0 (the OS-picks-an-ephemeral-port mode every `:0` fixture uses),
  but `splitHostPort` folds it in with a parse failure and both Deps
  assemblies seed the DEFAULTS — so such an install was graded on 7788/7789,
  ports it does not use and which are usually free, and a re-init aborted
  when something else held 7789. `checkPort` has always had the honest
  answer (`"no port set"`, warn, non-blocking); it never received it. Every
  spelling of zero, because `validatePort` runs `Atoi` and `"00"` is as
  legal as `"0"`. `autoStartProbeTarget` is the same question for
  `spawnNowOrWarn` — extracted because the other branch starts a real
  detached process, so the behaviour otherwise has no test at all.


The largest package in the repo — 52 production files, ~19k lines, `main.go`
alone 4,698 — and until 2026-09-06 it had **no section here**. Its invariants
were distributed by subject into the sections above (the `bgWriters` join under
Scanner, `SetPostScanHook` under Job pools, the cadence rearm under Config),
which is defensible for a rule about the scanner and useless for a rule about
the wiring itself. The LOUPE run that swept it found five confirmed defects and
four stale claims; the prior-art check explains why, at 20/12/2/6 `cmd/bridge`
mentions across the four `ops/audit-*.md` files.

- **"Always construct, never stop" removes a gate wherever the enclosing `if`
  WAS the gate.** PR #781 converted `if analysisActive {` / `if upscaleActive {`
  into bare blocks so both pools are always built — which is what makes the
  flags hot — and converted every READER to a live predicate in the same commit.
  The WRITE paths had no predicate to convert, so they silently lost theirs: the
  analysis sweeper ran on the default config (`analysis.enabled` is **false**),
  forking a decode per track and pushing a whole-library `indexed_at` delta to
  every paired device 90 s after every boot, while `/v1/analysis/*` 404'd; and
  `POST /v1/upscale` + `DELETE /v1/upscale/variants` gated on an adapter that is
  now never nil, so any bearer-token holder could enqueue sox jobs on a bridge
  advertising `upscaleEnabled: false`. **When a construction guard becomes
  unconditional, enumerate what that guard was gating — the nil-ness of a
  handle is a gate, and it stops being one.**
- **A sweeper's `enabled` predicate fails CLOSED on nil**, and the gate check
  belongs in the loop's callback, not buried in the pass. `analysisSweeper.active()`
  returns false for a nil sweeper or a nil predicate; `runFingerprintSweeper`'s
  nil arm reads the other way and is its own call. A disabled pass records NO
  status, so the Jobs card keeps its last real breakdown.
- **Feature gates on a mutation handler run BEFORE path resolution and the
  folder walk.** Both checks depend only on the decoded body, and a request that
  will be refused should not first cost a recursive `WalkDir`. Asserting on the
  refusal STATUS cannot catch the ordering — 503-before and 404-after are both
  "refused"; assert on which one comes back for a nonexistent path.
- **Every subcommand tail resolves its config through `loadCLIConfig`.** The
  `--config` flag defaults to the EMPTY string, so `config.Load(*configPath)`
  resolves nothing and the command dies with `read config ""` on any host where
  the operator did not pass `--config` — while the flag's own help promises the
  `./bridge.yaml`-then-platform fallback only `loadCLIConfig` implements. Two
  shared tails missed the migration and took six commands with them, including
  `bridge token revoke`, the documented orphaned-token recovery path.
  `TestNoSubcommandTailBypassesLoadCLIConfig` now sweeps the package against a
  four-entry allowlist of sites holding an already-resolved concrete path.
- **A GC or reaper whose "in use" set comes back EMPTY must refuse, not
  proceed.** `runArtworkGC` treated an empty referenced set as "everything is an
  orphan" and would unlink the whole cache — permanently for the scanner-written
  `local-<sha256>-500.jpg` covers, which the mtime skip gate stops it from
  regenerating. Its own docblock records this shipping once via a wrong DB path;
  that fix corrected the path and left the shape. Reachable today by a `--config`
  naming another install, or a run between a root flip's `WipeFilesystemTracks`
  and the rescan. An empty set with an EMPTY cache is still a clean exit — check
  the directory, not just the set.
- **`LiveHost` must accept the routing key's spelling.** `upnp_track_routing.server_udn`
  holds `StableServerKey` (lowercased); the SSDP cache is keyed on the raw
  advertised UDN and nothing folds it. An upstream whose UDN carries any
  uppercase character walked, routed and reached the phone — then 503'd
  `upnp_server_offline` on EVERY byte fetch, across `/v1/download`,
  `/dlna/file/{trackID}` and the web player. Exact `Get` first, folded scan as
  fallback; the cache holds single digits of entries, and the fallback measures
  **411 ns / 912 B at five upstreams**, which is noise against proxying a FLAC.
  **Don't memoise it in package-level state** — `runServe` is re-entered
  in-process by the launcher menu, so the memo outlives the cache it describes.
- **An operator-facing promise is a contract with an expiry date.** The uninstall
  prompt told operators the bridge "has no code path that can delete `--library`
  files (read-only by design)" — true when written, false since delete-as-trash
  landed. Scope such a promise to the COMMAND, not the product. Guard it by
  driving the real function (`actUninstall` with a buffer), never by scanning the
  source: this package's commentary names what it discusses, so a text scan
  reports its own docblock.

- **A write gate on a second process is a GUARD, not mutual exclusion — say
  which.** `bridge restore` and `bridge manifest clear-missing` mutate the store
  from a second process, where `Store.mu` does not reach and `busy_timeout` is a
  retry rather than a serializer. Both now refuse while a bridge answers on the
  admin port, probing again immediately before the destructive call because one
  can start while a confirmation prompt waits. That NARROWS the window; closing
  it needs an interprocess lock `bridge serve` also holds, deliberately not
  added — a stale lockfile after an unclean exit blocks `restore` at exactly the
  moment an operator needs `restore`.
- **`probeBridge` cannot answer for an ephemeral admin port, and must say so.**
  It fails closed on anything but connection-refused, which is right; but
  `adminAddress: …:0` names no port to dial, so the default produced "a bridge
  is answering on 127.0.0.1:0" about an address where nothing can. Refuse with
  the true reason. **PARSE the port** — a text compare against `"0"` accepts
  `"00"`, and `validatePort` runs `Atoi`, so every spelling of zero is legal.
- **`flag.Parse` stops at the first non-flag argument, so an unguarded
  subcommand silently WIDENS its scope.** `bridge enrichment retry
  Artist/Album` parsed with `--path` empty, and an empty scope is the whole
  library: a whole-library `enriched_at` reset and a delta to every paired
  device in place of one album. `library remove` had guarded this since PR #78.
  Any command whose empty scope means "everything" needs `fs.NArg()`.
- **The positional-scope class covers `--path`, not just `--filter`.** It was
  closed twice without being swept: #856 fixed `enrichment retry`, #882 fixed the
  four `--filter` commands, and `bridge duplicates` / `bridge enrichment misses`
  still parsed with `--path` empty — where an empty path scope emits a query with
  no `WHERE` clause at all, so a full-table answer comes back presented as the
  scoped one. Read-only, which is exactly why it survived both passes. Grep for
  the PATTERN (a flag whose empty value means everything), not the symptom.
- **A `stopFn` that signals is not a join.** `internal/integrity`'s watchers
  closed a channel and returned, while `runServe` defers that stop ahead of
  `Store.Close()` — an ordering that means nothing unless the stop waits. Both
  loops now join, grace-bounded. **And the work has to be cancellable for the
  wait to mean anything**: the adapters passed `context.Background()`, so the
  wait would have delayed `Store.Close()` behind work that was never going to
  stop.
- **A cancel has to reach the process that WRITES, and `exec.CommandContext`
  reaches only its direct child.** The standalone macOS Tailscale app's CLI
  helper at `/usr/local/bin/tailscale` is a shell script that runs the app
  WITHOUT `exec`, so the kill landed on `/bin/sh` and the app, an orphan now,
  wrote `<dataDir>/tls` after `runServe` had returned. On a Mac running
  tailscaled, `TestServeWiresResolvedConfigPathIntoAdminAndBackups` failed 6
  of 116 runs in one afternoon (`TempDir RemoveAll cleanup: unlinkat
  …/data/tls: directory not empty`). CI has no tailscaled and never saw it.
  Every CLI call in `internal/tailscale` runs in its own process group now,
  and `stopTreeOnCancel` kills the group. **Neither the group kill nor the
  join is enough alone.** On macOS a child forked while the signal is
  delivered can miss it (45 of 4,830 cancels in a probe), and it holds the
  output pipes, so `Wait` returns only once it has exited (8 of 8 escapes),
  and the auto-pilot joined on `bgWriters` with it. **Never set `Cmd.WaitDelay` there**: it unblocks
  `Wait` by closing those pipes, which abandons exactly the process the join
  exists to outlast. `TestServeLeavesNoTailscaleCLIRunning` drives a
  wrapper-shaped fake through the real exec path and pins the kill;
  `TestServeWaitsForAnInFlightTailscaleMint` holds a mint that ignores its
  context and pins the join. Neither sees the other's defect. (#997)
- **A cancelled pass is not a failed one, and a join makes the difference
  visible.** `detectAndMint`'s context is cancelled by shutdown, by
  `Disable()` and by an admin client leaving mid-"Re-mint now" (RefreshNow
  runs on the request's context). `passCancelled` returns the snapshot it
  found and logs and publishes nothing, because the Detect error branch
  UNLOADS the LE cert and the MagicDNS suffix: a cancel read as a failure left
  every `*.ts.net` client on the self-signed cert until the next good pass,
  up to a day later. A DEADLINE is still a failure. Joining a writer makes its
  cancelled exit path run before shutdown completes, so check what that path
  reports and what it changes. (#997)
- **Anything reading Go source in a test must normalize CRLF first.** No
  `.gitattributes` pins `eol`, so a Windows checkout has CRLF and every
  `\n`-literal scan finds nothing. One such guard failed loudly on the Windows
  leg; its sibling would have passed VACUOUSLY, which is worse. This rule was
  already written under **Build, CI, and test discipline** and was still tripped
  by a session that had read it — the platform leg is what closes that gap.

- **`bridge init`'s preflight grades the install that is THERE, and
  `ensureDoctorClean` prints warns.** init is re-run far more often than it is
  run (service reinstall, config rewrite, data dir moved to a new host) and it
  built `doctor.Deps` from its prompts alone — so the cert checks graded
  `<cfgDir>/data/server.{crt,key}` rather than `cfg.TLSCertPath`, and
  `tls-cert-sans` skipped itself, a nil `CertSANs` being a silent ok. Both are
  wired from a config readable at the TARGET path
  (`withExistingInstallCertDeps`); a first install keeps the skip, because a
  narrower want-set than `bridge serve` builds would be a comparison presented
  as authoritative that nobody made. Grading the PRE-init state is the point,
  not a compromise: the cert on disk IS the cert, and init never prompts for
  `customEndpoints`, so the old value survives the rewrite. **The warn printing
  is what makes any of it reach an operator** — every verdict these two checks
  give about that state is warn-level by design (neither is a reason to refuse
  to initialise), and `ensureDoctorClean` printed only on a FAIL, so a check
  wired into the preflight would compute a finding and discard it. Warn-lines
  only, in `printReport`'s layout, nothing at all on a clean host.

- **`bridge doctor` finds its config through `resolveConfigPath` (explicit,
  then `./bridge.yaml`, then the platform dir) and loads only a path it
  FOUND.** A missing config is not an error there, because doctor runs before
  `bridge init`, which is why it is not `loadCLIConfig`. Until #985 it tried
  the explicit path or the platform path and nothing between, so in the image
  (WORKDIR `/data`, config `/data/bridge.yaml`) a plain `docker exec … bridge
  doctor` graded an install with NO config. With no data dir there was no
  `server.pid`, and both port checks FAILed against the bridge's own
  listeners. With no env overrides, `audio-toolchain` read "not enabled"
  beside `BRIDGE_UPSCALE_ENABLED=true`. **`config-dir` grades the absolute
  directory of the config it RESOLVED**: init, `config.Save` and a relative
  `dataDir` all write beside the config the bridge reads, and the check
  CREATES what it is handed, so grading the platform dir beside a
  `./bridge.yaml` vouches for (and leaves behind) a directory nothing uses.
  With nothing found it is the platform dir, where init writes, and it is
  EMPTY when that cannot be resolved, never the working directory. **The
  launcher's doctor row passes the platform path explicitly**: it is offered
  only before that install exists, beside the Setup wizard that writes it.
  **Images up to v0.2.0 still need `--config /data/bridge.yaml`**, which is
  why the docs keep passing it. On those images the Dockerfile and
  docs/docker.md blamed the FAIL on "`bridge serve` writes no PID file" for
  seven weeks after #639 made it write one, and a re-check without the flag
  would have CONFIRMED that, since it failed identically for another reason:
  **measure an old image's doctor with `--config`, or you are measuring its
  config lookup.** **The `config-file` line names the config the report
  graded, and one that is THERE but will not load FAILS.** Until #985 the
  load error was dropped without a word, so a typo'd key read "all clear",
  exit 0, where `bridge status` exits 2 on the same file. The local-first
  lookup made that worse: a broken `./bridge.yaml` shadows a good platform
  config (CodeRabbit's finding). The FAIL is what makes the runbook's
  "validate a config edit BEFORE restarting" step true. **A `--config` that
  names a file that is NOT THERE fails too** ("does not exist"), because an
  operator who names a file asserts it exists; that is loadCLIConfig's rule
  for every other subcommand. `config-dir` then reports "not checked" rather
  than `MkdirAll` the named file's directory, which it always did, so a
  typo'd path created that directory and called it ok. With no flag, a
  missing config is ok and the line names where doctor looked. **`resolveConfigPath` folds EVERY stat
  error into "not found"**, which is right for its fallback walk and wrong
  for a named path, so doctor re-stats a named path to learn why. One this
  user cannot reach or READ only WARNS, because that is a fact about the
  doctor run (the public-mode layout, as with the cert key). The launcher's
  pre-setup row names the platform path through `buildDoctorDepsFor(path,
  true)`, never `--config`, so its absence stays "none found", ok.
  `bridge init`'s preflight leaves the lookup nil, so the check reports
  itself skipped and a broken existing config cannot block the re-init
  that replaces it. (#984, #985)
- **The image's `lsof` package is load-bearing — don't drop it to slim the
  image.** Alpine's own `/usr/bin/lsof` is busybox's applet, which ignores
  `-iTCP:<port> -sTCP:LISTEN -t` and lists every open file, and
  `isPIDListeningOnPort` searches that output for the pidfile's PID — so doctor
  would credit ANY occupied port to a running bridge (measured: a root-held
  `:8080` read `bound by our own bridge (pid 1)`, where the package reports a
  warn). (#984)
- **The HEALTHCHECK's connect-and-close is silenced at the LISTENER, never by
  making the probe speak TLS.** net/http logs every failed handshake and
  `bridge health` closes before any ClientHello, so each probe was `http: TLS
  handshake error from 127.0.0.1:…: EOF`: 2,880 lines a day, the M-SEARCH
  class. `handshakelog.Wrap` takes the RAW listener and returns the server's
  `ErrorLog`. For a peer on this host only (loopback, or source ==
  destination: `bridge health` dials a specific bound IP as-is, so it arrives
  FROM that IP), the listener records whether the peer's first answer was
  EOF, and the logger drops a line only on that evidence. **The text cannot identify the probe**:
  a client that sends a whole ClientHello (1,483 bytes from Go's) and then
  vanishes gets the byte-identical `: EOF`, and that one is a real failure.
  **Nor can a verifying probe replace it** (`InsecureSkipVerify` was refused in
  #485): checked against the cert on disk, it reports a live bridge DEAD
  whenever the served cert is expired, not yet valid, or rotated ahead of the
  restart, and logs `remote error: tls: bad certificate` per probe in each
  case, so the flood returns exactly when someone is reading the log. Kept
  lines go to `log.Print`, where a nil `ErrorLog` sends them;
  `TestEveryOtherHandshakeFailureIsLoggedAsBefore` compares against a
  nil-`ErrorLog` server rather than restating net/http's wording. Never wrap a
  listener that yields `*tls.Conn` (tsnet's `ListenTLS` does): http.Server
  asserts that type to find the handshake, ALPN and `r.TLS`. The console's
  public-mode TLS branch takes the same pair, since the launcher's
  `probeAdminRunning` and `waitForListen` hit it the same way, and **a new TLS
  listener takes it too.** (#986)

**The four stale claims this run corrected in THIS file** — all four sat in the
"Don't regress these cross-cutting invariants" list at the top, which reads as
the most authoritative place in the document and had drifted from the hardened
sections that superseded it:

1. **`WipeAllTracks` on a root flip** — the exact CASCADE the Scanner section
   forbids and explains. No production path has ever called it.
2. **Two sanctioned `enriched_at` resets** where there are four, two of them live.
3. **"Admin console is loopback-only, no auth"** with no mention of public
   mode's credentialed posture — which is what the VPS runs.
4. **Five subcommands** in the architecture table where `run()` dispatches 28.

**When you correct a rule in a hardened section, check the top-of-file list for
its twin.** The top list is older, shorter, and read first.

### Auth, pairing, TLS and security posture

- **The admin console is loopback-only with no auth, by design.** Anyone on the
  host already owns the token store and the DB, so auth on top would be theatre.
  Don't add a layer that bypasses the loopback constraint; SSH-tunnel for remote
  admin. Public mode is the separate, credentialed posture.
- **`csrfGuard`**: body-bearing mutations must be `application/json`; body
  detection uses `ContentLength != 0 || len(TransferEncoding) > 0` because
  net/http strips the header. **A bodyless POST is deliberately allowed
  through.** The Origin allowlist is reject-if-MISMATCHED, not reject-if-missing
  — failing closed locks out real operators. The only relaxation is
  `application/octet-stream` on PUT (upload); **`multipart/form-data` stays
  refused everywhere**, because it is a CORS *simple type* and therefore
  forgeable cross-origin, while octet-stream + PUT forces a preflight the bridge
  never answers.
- **Any long-lived GET needs its own Origin gate** — `csrfGuard` lets GETs
  through, which is right for one-shot reads and wrong for a held SSE
  connection.
- **Pairing token delivery is read-many**: `Poll` returns the token on every
  authorized poll while Approved, and only a client `DELETE` or TTL+grace
  consumes it. A network blip must be recoverable — **don't add a "clear `RawToken` on first
  read" or a `StateDelivered` terminal**; the pollSecret bearer plus the cert pin
  are the safety surface. The per-request timer
  generation guard is required because `Stop()` returning false doesn't mean the
  callback won't fire; `Approve`/`Decline` check the wall clock themselves; the
  cert-rotation guard fails CLOSED including on an empty current fingerprint;
  `snapshot()` redacts `RawToken` and `PollHash`.
- **`onTimer` transitions the row to Expired UNDER the lock before revoking
  out-of-lock** — otherwise a concurrent poll hands iOS a token the revoke then
  destroys. Revoke-then-delete with bounded retry; `Delete` refuses an
  Expired-with-token row so the revoke lifecycle stays owned by `onTimer`.
- **A console login ticket is PERSISTED, because the two halves are different
  PROCESSES.** `bridge admin login-link` mints and the serving bridge redeems, so
  an in-memory map is invisible to the redeemer and the feature never works — it
  shipped that way, and every unit test passed because each minted and redeemed
  inside one process. What is stored is the hex SHA-256 in a 0600 sidecar beside
  the password hashes, so disk gains no credential it did not already hold, and
  single-use plus the 60-second window still come from deleting the record on
  presentation, before judging it. **Stage to a UNIQUE temp name**: `Store.mu`
  does not reach across processes, so one fixed `.tmp` lets two writers interleave
  and rename each other's half-written bytes into place, which loses every live
  ticket. The read-modify-write is still unserialised across processes — losing
  one of two simultaneous mints is survivable (mint again) in a way a corrupt file
  is not, and an interprocess lock is declined for `bridge restore`'s reason: a
  stale lockfile after an unclean exit blocks the login path exactly when it is
  needed. `SameSite=Strict` is NOT the usual magic-link trap here — the app opens
  the URL itself, and a navigation with no initiator is same-site (verified in a
  real browser, not reasoned about).
- **A login link is REDEEMED ON THE POST of its interstitial, never on the GET
  — a link preview is a navigation-shaped GET no header guard can tell from the
  click** (2026-09-12, the hosted uploader link: every link the phone shared
  landed on `/login?link=stale`). The HEAD-405 and the declared-prefetch-403
  only cover probers that SAY what they are; iOS's share sheet, Messages on
  both ends, Slack and mail clients draw their preview by loading the URL
  through a real browser engine, which declares `Sec-Fetch-Mode: navigate`
  exactly like the human's click that follows — so redeem-on-GET spent the
  ticket while the share sheet was still opening. `GET /login/ticket` now
  renders `login_ticket.html`, a one-button page with NO script (a previewer
  runs scripts; it does not click), and `POST /login/ticket` — an EMPTY body
  with the ticket in the action's query, the one bodiless shape `csrfGuard`
  admits without a Content-Type check — is what redeems. **The GET consults no
  store**: a ticket that never existed gets the same page as a live one, so
  it is neither the "is this link live" oracle the miss branch used to be nor
  a file read under the console's mutex; a bare `?t=` is sent to
  `/login?link=stale` because a button that can only fail is not a page. The
  URL shape is unchanged, so `bridge admin login-link` and the control
  plane's `console-link` need nothing — the operator's link just gained a
  Continue. Negative control: putting the redeem back on the GET turns exactly
  eight of the fourteen ticket tests red (`TestOpeningTheLinkDoesNotSpendIt`
  drives three navigation-shaped previews before the click).
- **The interstitial is served under `Referrer-Policy: strict-origin`, NEVER
  `no-referrer` — a form POST from a `no-referrer` page carries
  `Origin: null`, and the CSRF guard refuses the Continue button** (2026-09-12,
  the second field report on the uploader link, one build after #909: Continue
  answered with the 36-byte `admin refused: cross-origin request`, which Safari
  on iOS saved as `ticket.txt`; macOS the same; reproduced with curl against the
  live tenant — `Origin: null` gets that body, the page's real origin redeems).
  The Fetch standard's "append a request `Origin` header" serializes a non-GET,
  non-`cors` request's origin as `null` under `no-referrer` — a plain form
  navigation is exactly that shape. The console's own requests never show it
  because `fetch()` runs in `cors` mode, which the rule exempts, and the login
  form serves `same-origin`; **no test saw it because `redeemTicket` sent no
  `Origin` at all and `csrfGuard` checks the header only when present**, while
  the leak test pinned the very `no-referrer` that broke the button. Header AND
  `<meta name="referrer">` must agree (the meta is parsed later and is what the
  document keeps) — `loginTicketReferrerPolicy` is the one constant.
  `strict-origin` keeps what `no-referrer` was chosen for and more: the Referer
  carries the origin alone, never the address holding the ticket, same-origin
  subresources included, and it nulls the Origin only on an https→http
  downgrade. `TestLoginTicketRedeemsUnderTheOriginABrowserSends` reads the
  policy the GET serves, derives the Origin the spec says a browser sends
  under it, and submits that — a regression is red by construction. **The
  transferable rule: a test that exercises a form POST must send the `Origin`
  a browser would derive from the served policy, never omit it.** Negative
  control: `no-referrer` on both header and meta turns exactly the two policy
  tests red.
- **`POST /v1/pairing/requests` HAS a per-IP token bucket (burst 5, one per
  5 s, since #133) beside the bridge-wide pending cap.** This bullet said
  the opposite for over a year — "no per-IP rate cap, double-NAT puts every
  LAN device behind one address" — while PROTOCOL.md documented the 429 and
  `pairing.go` enforced it, and `TestPairingCreateQueueFull`'s docblock
  carried the same false claim. The BURST is what makes double-NAT
  survivable: a fumbling re-tap never reaches it, and a script is bounded
  below the 16-pending queue it would otherwise fill alone. Don't remove
  the limiter to match the old prose. The 6-digit code is drawn from
  `crypto/rand`.
- **The login ticket is refused by SHAPE before anything else looks at it**
  (base64url, at most 64 bytes; a minted one is 43) — on the GET so an
  unbounded query is never echoed into the page, on the POST so it never
  reaches the hash or the file read under the console's mutex. The refusal
  is the same 302 every unusable ticket gets. The interstitial's referrer
  META is injected from `loginTicketReferrerPolicy`, never typed — a
  literal is how header and meta both came to say `no-referrer`. A failed
  session mint after a successful redeem is a STALE link: the ticket is
  spent by then. **`url.QueryEscape` before templating was proposed and is
  wrong**: html/template already URL-escapes that attribute context, so it
  would double-encode; bound the SHAPE instead. (#918)
- **`AllowAndReserve` callers must NOT also call `RecordFailure`** — the
  reservation IS the failure count. Check-then-act across two lock acquisitions
  let concurrent logins all pass the ceiling.
- **The TLS cert is sticky and rotation is warn-only.** iOS pins the SHA-256 at
  pairing, so auto-rotation silently breaks every paired device; `certDuration`
  stays ≤397 days (Apple ATS rejects at handshake, before pinning runs).
- **A warning that only fires at SERVE time arrives after the damage.** The
  SAN-staleness check ran once inside `LoadOrGenerateWithOptions` and nowhere
  else, so a data directory carried to another host reported `[ok] tls-cert
  present` in `bridge doctor` and only said `cert SANs are stale` once the
  bridge was up — by which point devices have pinned a cert that fails TLS for
  every Tailscale and custom-endpoint URL, and the fix costs a re-pair of each.
  `bridge doctor`'s `tls-cert-sans` runs the SAME comparison
  (`servertls.InspectSANCoverage`) against the SAME want-set BEFORE the first
  start. **That want-set is `cmd/bridge`'s `certSANOptions`, the one gather all
  four cert paths use** — serve, `init`, `cert rotate`, doctor; they were four
  copies of three lines, and the copy that drifts is the one that calls a cert
  fine when a rotation would mint something different
  (`TestCertSANOptionsIsWhatEveryCertPathMints` pins it structurally). The
  doctor's cert checks read `cfg.TLSCertPath` too, via `doctor.certPaths` —
  resolved from `DataDir` alone they graded a cert nobody serves. Expiry is on
  the `tls-cert` line against the exported `servertls.ExpiryWarningWindow`, the
  startup warning's own threshold — **compared as an exact duration, never
  `DaysUntilExpiry`**, which truncates toward zero, so a day count would warn at
  30d23h while `logIfExpiringSoon` stays quiet. The fail/warn split is **"can
  `bridge serve` start"**: a pair it cannot LOAD fails (partial, unparseable, or
  mismatched — `VerifyKeyPair` is `LoadX509KeyPair`, serve's own call, and an
  interrupted two-rename rotate leaves a new cert with the old key, which
  `Inspect` grades as a clean 396 days); an expiring, expired or NOT-YET-VALID
  cert loads, so it warns about the clients. **The validity window has a near
  end** — `LoadX509KeyPair` ignores dates, and the mint allows one hour of skew
  (`NotBefore: now-1h`), so a host whose clock was further ahead at mint time (a
  NUC or Pi with no RTC, pre-NTP) leaves a future `NotBefore` that reads as a
  comfortable year of life left; `logIfExpiringSoon` carries the same arm, and
  the hint says CHECK THE CLOCK FIRST because rotating against a wrong clock
  mints another bad cert. **Every surface that grades the window grades BOTH
  ends** — the console tile, `bridge cert info` (human AND `--json`, where the
  verdict is `notYetValid` beside `expired`/`expiringSoon`) and `bridge cert
  rotate`'s preamble all stayed on `DaysUntilExpiry` and read a cert starting
  in 30 days as `(426 days)`, unbadged. The prose is
  ONE const, `servertls.NotYetValidRemediation`, pinned by a test on all three;
  it is deliberately NOT `RotationRemediation` with a lead-in, because this is
  the one band where rotating FIRST mints a second wrong cert — which is why
  `bridge cert rotate`'s preamble carries it and carries NO other band (an
  expiring cert is why the operator is there). The startup log keeps its own
  shorter wording on purpose (structured, sized like its expiry sibling) and
  the console keeps a badge plus a date, because rotating from a browser is
  exactly what this state must not do. **A permission failure reading the 0600 key is NOT
  a finding** — on the public-mode layout the operator is not the service user,
  and that is a fact about the doctor run, the same reason `Validate()` does not
  stat the roots. **Every cert READER skips to the first CERTIFICATE block**
  (`decodeCertificatePEM`, shared by `Inspect`, `fingerprintFromPEM` and
  `InspectSANCoverage`) because `LoadX509KeyPair` does: a key-first or
  `Bag Attributes` PEM loads at serve time and read as "unreadable" on both
  doctor lines. `tls-cert-sans` skips on a managed
  bridge (the control plane owns rotation and the tenant reaches it over the
  autocert domain); **expiry does not** — `TestManagedReportsExpiryButNotStaleSANs`
  pins the split, which was prose in a docblock and nothing else. `--fix` does
  NOT rotate: that invalidates every pin and is an operator decision with a
  device in hand.
- **The pairing QR advertises the SERVED cert**, resolved by SNI —
  `FingerprintForServerName` mirrors `Get`'s routing rather than delegating, so
  **every freshness and validity gate in `Get` must be restated there**; losing
  one made the QR advertise a fingerprint the listener never presents.
- **The pairing QR's `urls=` and the console's "Reachable endpoints" panel
  read `admin.Deps.Endpoints` — `api.(*Server).ReachableEndpoints`, the ONE
  enumeration `/v1/health` serves — never a second `advertise.Endpoints`
  walk.** PR #269 moved the Tailscale append out of the advertise package
  into the api layer and left both admin consumers on the old walk, so from
  May to September the QR baked no Tailscale fallback (while `buildPairURL`'s
  docblock promised one) and dropped `customEndpoints`, and the panel could
  never show the row its own prose said to look for — a loopback cli-mode
  bridge whose health advertised `.ts.net` + `100.x` + `fd7a:…` paired the
  phone with `.local` and the LAN IP alone. **Nil is NO list, not the old
  walk**: on a host without Tailscale that walk is byte-identical to health,
  which is how the first boot control stayed green with the wiring line
  deleted; the QR falls back to the primary (which always pairs) and the
  panel to its empty state. `TestServeBakesHealthEndpointsIntoThePairingQR`
  boots the real `serve` and requires `POST /api/tokens` to bake exactly
  `[primary] + /v1/health.endpoints`. Public mode keeps its own explicit-
  `:443` synthesis in `pairAlternates` on purpose (the iOS 7788-default bug)
  and never consults the provider. (#936)
- **`atlas.harvestBaseUrl` pins the host a credential POST may set** — the body
  carries the base URL, so whoever sets it chooses where bios come from, and
  those render as an attacker-chosen "Read more on …" link. A pin binds in every
  mode; unpinned is refused in demo and still allowed off-demo. Both sides of
  the comparison must go through `CanonicalHTTPSBase`, or a one-sided reduction
  turns a correct pin into a mismatch that fails closed.
- **`fs.Resolve`'s final containment check is the PRIMARY defense on Windows**,
  not belt-and-braces: both guards above it are slash-based, so a backslash
  traversal passes them untouched and only `filepath.Join` + the prefix check
  catches it.
- **HSTS is public-mode + TLS only** — pinning it for `localhost` poisons that
  hostname in the operator's browser for every other local service.
- **CodeQL's `go/log-injection` is a false positive BY CONSTRUCTION and will
  regenerate.** Both slog handlers quote the value and escape `\n`/`\r`, every
  flagged site passes a structured attribute, and `internal/` contains no
  `log.Printf` at all. Verify empirically in ten lines rather than from memory;
  dismiss on those grounds rather than re-deriving them. Read the dismiss
  comment before re-opening any alert in this repo.

### Admin console and the web player

- **The cert tiles grade and PHRASE from one number.** #952 gave all three
  one ladder (`certExpiryBadge`, which returns `expired` for `left <= 0`)
  and left each tile's wording alone — wording written against the
  PREVIOUS ladder, which had no expired arm — so two of them clamped with
  `Math.max(0, …)` and an expired certificate rendered `expired · expires
  in 0 days`. The self-signed tile reached the same place from the other
  side, displaying the `daysUntilExpiry` sentinel `Inspect` forces
  NEGATIVE past NotAfter as `(-40 days)`. `certValidityText` is the
  sibling of that one ladder and derives from the same signed `left`.
  **"Today" is a CALENDAR question**, not a 24-hour one: a duration test
  called an expiry at 00:30 tomorrow "expires today" beside a date saying
  otherwise. `now` comes from the caller's own `left` rather than a second
  clock read, so the badge and the phrase cannot drift and a test can
  drive any instant. The Go suite cannot run it, so the guard is
  structural — it requires the calendar comparison to be present. (#962)
- **…and so is the COUNT after it** (#971). #962 gave "today" the calendar
  and left the days on `Math.round(Math.abs(left)/86_400_000)`. All three
  tiles print that beside `toLocaleDateString()`, and a rounded duration
  disagrees with the date in BOTH directions: 47 h from 00:30 read "in 2
  days" beside TOMORROW, 25.5 h from 23:30 read "in 1 day" beside the day
  after. `Date.UTC` over the local Y/M/D is DST-safe for the reason the
  same-day test is, and the `Math.max(1, …)` clamp the duration form needed
  becomes unreachable — keeping it would only suggest the arithmetic can
  produce a zero. `left` carries the TENSE, never the count, and the
  structural guard now says so: it rejects any expression dividing `left` by
  a day, because requiring the Y/M/D getters to appear SOMEWHERE was
  satisfied by `Math.round` and passed throughout.


- **The catalog is computed, not stored.** Album identity is
  `dupes.AlbumIDOf(dupes.Resolve(row))` — the same value the iOS client computes
  — so the browser's partition equals the phone's by construction. **Don't add
  album/artist columns "for speed"**: genres and composers are multi-value axes
  with fold rules SQL can't express, so the Go pass is required either way.
  Invalidation is LAZY (an epoch bump; the next reader rebuilds) — that IS the
  debounce, because an eager rebuild re-folds the library on every watcher event
  during a bulk import.
- **An album is a SET of tracks, never a path prefix.** `FolderPath` is the
  common directory, and on a real library ~8% of albums share one — so a prefix
  submit enqueues every neighbour and a prefix delete reclaims their sidecars. A
  single track is the mirror image: the subtree pattern matches strict
  DESCENDANTS, so a file path projects zero rows. Identity scopes travel as IDs
  and are expanded server-side.
- **A present-but-EMPTY scope must never read as an ABSENT one** — absence means
  the folder form, and an empty folder path means everything. `{"albumIds": []}`
  would upscale the whole library.
- **The browser MIME table is NOT the DLNA one** (`audio/x-flac` is right for
  hardware renderers and unplayable in browsers). Playability reports FACTS
  (`universal` / `engine-dependent` / `none`), never a verdict — `canPlayType`
  answers `""` for codecs an engine can actually decode.
- **`//go:embed static/*` skips `.`/`_`-prefixed names inside matched
  subdirectories** — a `_util.js` compiles, embeds nothing, and 404s only in a
  release build. Don't "fix" it with `all:static` (that ships `.DS_Store`).
  `/static/` must force Content-Type + `nosniff` (module scripts are MIME-checked
  and hard-fail; Windows serves `.js` as `text/plain` from the registry) and
  `no-cache` (a `?v=` busts only the entry module — relative import specifiers
  don't inherit the query).
- **Partial-boost works only because the `<audio>` element and now-playing bar
  are parented to `<body>`, outside `<main>`.** Every page's init registers
  document/window listeners scoped to an `AbortController` aborted before the
  next page's init; `boostSwap` claims a generation up front and discards a
  superseded response; the post-render scroll restore is generation-guarded
  (the stale offset lands LAST, measured, not first). `PLAYER_HEADS`,
  `playerRoutes` and boot.js's route table are a three-way parity contract, each
  pair pinned.
- **`boot();` must be the last statement in `boot.js`** — it reaches most of the
  module, so a call near the top puts every later `const`/`let` in the temporal
  dead zone. This emptied the whole player twice with no failing test and no
  symptom but a console `ReferenceError`.
- **A settings control that renders but isn't in the PATCH allowlist saves
  nothing while the page still says "Saved."** — worse than not offering it. The
  same class covers apply-semantics stated in three places (badge, hint prose,
  server report); each surface is walked from its own side, because a test that
  walks one proves nothing about the others.
- **`.small` had no rule in either stylesheet for months**, and `.hint.warn` /
  `.error` were inert class combinations — everything asking to recede rendered
  at body size. A test connects emitted classes to rules and records which are
  BORROWED from app.css. **Strip CSS comments before scanning**: this repo's
  commentary names the classes it discusses.
- **player.css must not style operator classes**, and a bare class rule there
  hijacks anything app.css qualifies with an element (a bare `.rows` collapsed
  every operator table).
- **Deleting scattered CSS or JS needs a selector-set / declaration-aware diff**,
  not a brace or docblock scan. A comment containing a literal `{` swallowed the
  global `[hidden]` rule; a cut ending at "the next `function`" swallowed five
  unrelated functions; and `/*` inside a `//` JS comment opened a fake block
  comment that hid 46 KB of app.js from a guard test.
- **Dynamically-composed class names (`class="status-${x}"`) are not dead
  because no literal exists.** Check composition before deleting.
- **`/api/jobs` is guarded in BOTH directions, down to the LEAF.**
  `TestSettingsPrereqsOnlyReadRealJobsFields` walks JS→Go (every `jobs.<field>`
  names a real field) and cannot see a field nobody reads;
  `TestEveryJobsFieldIsRenderedSomewhere` walks Go→JS. #891's stated purpose was
  "a Jobs card" and it touched no template and no JS — the endpoint ran a
  full-table scan every thirty seconds for numbers no pixel consumed, and the DTO
  test passed. **The first version of that guard walked the twelve CONTAINERS
  and was satisfied by `j.lyrics` alone** — so "`lyrics` was the only field with
  zero reads" was true at the only level it looked, and the nine numbers inside
  were as unguarded as `lyrics` had been. Walked recursively it found six more
  leaves marshalled for nobody (`scanner.isScanning`, `scanner.lastFullScan`,
  `enrichment.source` — all read off `/api/stats` instead — plus
  `analysis.intervalSec`, `coverage.totalLocal`, `smartMixes.analysisAssisted`).
  Two mechanisms make the recursive form work, and both are negative-controlled:
  it resolves the JS's local and parameter aliases (`const lyr = j.lyrics`;
  `renderAnalysisCoverage(an.coverage)` → `cov.eligible`), and it strips
  COMMENTS ONLY — most nested reads sit inside `${…}` template interpolations,
  which `stripJSNoise` blanks. Recursion stops at the exported shared types
  (`*JobRunState` and kin), which have other consumers. **A guard that checks
  containers proves nothing about their contents.** One of those stops is
  `*AnalysisSweepState`, and a field added to its `last` breakdown and never
  rendered leaves this guard green (measured on #992), so that breakdown is
  pinned the other way: `TestDescribeAnalysisSweepAccountsForEveryTrack`
  executes the line that renders it.
- **`/api/stats` is guarded in both directions too, and there "read" means the
  console OR `bridge status`.** Unlike `/api/jobs` this payload has a SECOND
  consumer — `cmd/bridge/status.go` decodes it into a `map[string]any` and
  prints ten fields — so an app.js-only sweep reports seven false positives, and
  a missing KEY there is nil, formats to `""`, and silently drops a line from
  `bridge status` (`TestBridgeStatusOnlyReadsRealStatsFields`, by AST: the
  subject is a string literal, which this repo's comment strippers blank). **A
  `bridge status --json` dump does NOT count as a read** — it passes the whole
  decoded map through verbatim, so it would exonerate every field, which is the
  same as having no guard. Four had no reader at all: `tracksUnreadable` (the
  one query in `readStatsDBPart` run for a single field — a SELECT per snapshot
  for nobody, #947's own addition, now the dashboard's alarm row, hidden at zero
  and pointing at the Jobs list built from the same predicate), plus three
  duplicates now GONE rather than exempted — `startedAt` (`uptimeSec` on the
  same payload), `dbBytes` (an `os.Stat` per snapshot for what
  `/api/diagnostics` serves) and `upnpRoutedTracks` (the COUNT is earned,
  `trackSourceCounts` hands it to `/api/sources`; the scalar was
  `sources.routedTotal` one payload over). **No exemption list, on purpose** —
  it is the frictionless way to put a fifth one back, which is the failure the
  jobs guard exists for. `statsResponse` is FLAT, so the recursion question does
  not arise, and that is ASSERTED rather than assumed so a future NON-SCALAR
  field forces the decision — an allowlist of scalar kinds plus `time.Time`,
  not a list of the shapes to reject, because a map or a slice marshals nested
  leaves exactly as a struct does and `reads` counts every PREFIX of a path.
  The JS root is scoped to ONE function: `applyStats`'s
  parameter is `s` and app.js has seven one-argument functions whose parameter
  is `s`, so an unscoped root is a false PASS. (#948)
- **An SSE list handler needs an explicit empty-list teardown branch.** A restart
  wipes the in-memory pairing store, so the next snapshot is `[]`, and
  `applyPairing([])` must clear the optimistic-action latch and hide the panel —
  otherwise a stale latch entry is inherited by whatever new request lands in the
  same id slot. The snapshot must marshal as `[]`, not `null`, for the array
  check to reach that branch.
- **The SSE stream byte-diff-suppresses per CONNECTION**, so a freshly-injected
  tile gets nothing until a value changes — recycle the EventSource after a swap
  to force a full snapshot. Anything monotonic (uptime) must be zeroed in the
  SSE DTO (`statsResponse` has a dedicated SSE wrapper for exactly this) or it
  poisons the diff every tick — and never add a server-computed `elapsedSec`. Expensive snapshots go behind a TTL
  + singleflight and never on a fast tick; a busy/idle gate needs a latch to fire
  one final frame.
- **Refresh on the pool's DONE+FAILED counters, not a busy→idle edge** — a short
  batch starts and finishes between two frames, so the edge never fires.
- **A back link is navigation and does not belong in an action row**; a heading
  takes slack with an auto margin, not a `justify-content` flip (a page-scoped
  `.panel-head` selector silently outranks a two-class `:has()`).
- **`.sr-only` is `position: absolute`** — inside a horizontally-scrolling
  container it escapes the clip and extends the document's scroll width.
- **Icon presentation lives on the `<use>` host, not the sprite's source `<g>`**
  (a CSS rule matching the original element does not cross the shadow boundary;
  inheritance does), and `viewBox` is an HTML attribute, not a CSS property.
- **A badge inside a link must carry no `aria-label`** — accname replaces the
  subtree with it, dropping the live count the badge exists to say.
- **Whole-library destructive actions live behind a typed exact phrase** (never
  prefix or case-fold), re-checked in the handler rather than trusting
  `disabled`.
- **`WriteTimeout` stays UNSET on the admin server** (long synchronous endpoints
  and SSE); the upload path rolls the READ deadline forward on elapsed time, not
  bytes — a byte threshold starves the slow client it exists to protect.
- **Staging and trash live INSIDE the target root as dot-directories**, skipped
  before the walker upserts anything, which is what makes commit and delete
  same-filesystem renames. Staging under `dataDir` is a cross-device copy
  wherever the library is a separate mount — the normal case.
- **The durable upload offset is the meta record, never the staged file's
  size**; ordering is bytes → fsync → offset → fsync, and every open truncates
  back to the recorded offset. Locks are refcounted per `(session, file)`; a
  session-wide lock passes every locking test while quietly serialising a folder
  upload.
- **Committed files are 0644** (the staged mode survives the rename), and trash
  age comes from the `<stamp>` DIRECTORY NAME — `os.Rename` preserves mtime, so
  an mtime-driven sweeper purges oldest-content-first the instant it lands.
- **Deleting takes an explicit path list, never a prefix** — that sidesteps the
  case-fold class entirely rather than getting it right.

- **A CSS grid with no `grid-template-columns` sizes its track to the WIDEST
  item's max-content, and no Go guard can see the result.** `.deleted-list`
  copied `.unresolved-list`'s `display: grid; gap: 2px` and one long playlist
  name resolved the implicit `auto` column to **925 px inside a 317 px panel**
  at 375 px: rows overflowed, every action button left the viewport, and the
  list grew a horizontal scrollbar. `min-width: 0` on the child does NOT fix
  it — the TRACK is what grew; `minmax(0, 1fr)` on the list plus `min-width: 0`
  on the grid ITEM is what lets an ellipsis apply.
  `TestPlayerEmittedClassesAreStyled` was green throughout, because the class
  HAD a rule: the markup was correct and the page rendered wrong, which is the
  exact failure that test describes and cannot detect. **Drive the real console
  at 375 px with a deliberately long fixture string**, and check
  `scrollWidth == clientWidth` rather than eyeballing it. (2026-09-20)

- **A gate on a query parameter reads the PARSED predicate, never the
  parameter's presence.** The player sends `needs=all` on every default
  grid load (its default is the literal `all`, and `qs()` drops only the
  empty string), and `parseVariantFilter` maps `all` to no filter — so
  #913's variant-free coverage skip, gated on `q.Get("needs") != ""`,
  never ran for the live grid on exactly the libraries it was built for,
  and its test passed because it sent no query at all. Parse first, gate on
  `pred != nil`, and **test with the query the client actually sends**. An
  active filter with no snapshot is REFUSED (503 `coverage_unavailable`),
  never dropped: `filterAlbums` served the unfiltered library under a
  plausible total beneath a comment calling that the safe direction. (#915)
- **An `<a class="btn">` inside running text needs `display: inline-block`.**
  An inline box's vertical padding does not grow its line box, so the
  pill's padding spills over the line above — the post-upload "Browse your
  library" link overlapped its sentence by 4.4 px with the UA underline
  showing through. Every other `.btn` is a `<button>` or sits in a flex
  row, which is why only this one did it. Measured in a browser. (#920)
- **Below 1024px, a rail child that is not inside `.sidebar-drawer` is in
  the TOP BAR.** `header.sidebar` is a flex ROW there, and the space meter
  was added as a plain rail child: on a public bridge with uploads enabled
  the bar at 375px held "42 GB free", the live dot, the theme pill and
  Sign out beside the 44px hamburger, so the head shrank to 57px, the mark
  sat under the button and the library name rendered at 0px of its 70.
  The meter's gate is `upload.enabled || upload.minFreeBytes > 0`, NOT the
  deployment mode — which is why the demo tenant, same binary, looked
  fine. The nav, the meter and the foot block now share one wrapper that
  the breakpoint hides and reveals; it is `display: contents` on the
  desktop rail, so that column is unchanged (measured identical against a
  main build), and `TestEverythingBelowTheBrandIsInTheDrawer` pins the
  header to exactly two element children — put a new rail element inside
  the wrapper unless it has a case for the bar. The live dot stays in the
  bar as a SECOND `.conn-badge` inside the brand — dot-only there, hidden
  on the desktop rail — because the foot's `#conn-status` is inside the
  hidden drawer and its `aria-live` region would announce nothing on a
  phone; `applyConnState` updates every `.conn-badge`, so each viewport
  shows exactly one live region. (The first form was a `:has()`-coloured
  pseudo-element; CodeRabbit caught the silent live region.) Verified in a
  browser at 375 / 430 / 1280, dark and light, with and without the
  meter; the Go suite sees only the containment. (#934)

### Build, CI, and test discipline

- **A `needs` entry only makes a job WAIT; something has to READ its
  result.** `gate`'s `needs` listed six jobs and its verification step
  checked five, so with `if: always()` a failing `dsd-measure` produced a
  GREEN gate — in the one job that exists BECAUSE those tests had silently
  never run in CI. The echo was honest (it named five), which is how
  nobody noticed. The verdict now iterates `toJSON(needs)`, because adding
  the sixth name would leave the seventh to be forgotten the same way;
  `skipped` and `cancelled` count as not-passing, and an EMPTY `needs` is
  refused — the `checked == 0` floor, applied to a gate. Verify by
  EXECUTING the extracted `run:` block against each case, not by reading
  it. **This does not cover a job absent from `needs` altogether**, which
  is not waited on and cannot be seen from inside. (#961)
- **A doc comment must be attached to the declaration it names.** Glued —
  no blank line — onto a different one, `go doc Y` prints X's prose and X
  has none. It happens when something is INSERTED between a comment and
  its subject: #840 and #953 each did it to a docblock carrying a live
  invariant, and `advertisedEndpoints`' orphaned paragraph had two
  cross-references still pointing at it. Sixteen across the tree, all
  pre-existing. `TestNoDocblockNamesAnotherDeclaration` flags only where
  the named identifier is DECLARED in the same package and has NO doc of
  its own — both conditions load-bearing, taking 467 raw candidates to 21
  to 16. It reads `ast.CommentGroup.Text()`, so `/* */` and a leading bare
  `//` are seen. **Re-run the control after refactoring a detector**: the
  `doc.Text()` change was verified to find the same sixteen, since that is
  exactly where a guard quietly stops guarding. (#964)
- **…and it reads consts and vars, against a verb list DERIVED from the
  tree** (#989). #964 inspected func and type docs only, against 31
  hand-picked verbs, and a 2026-09-24 census found 36 more: 18 glued onto a
  const or var (`handleBrowse`'s on `maxSOAPBodyBytes`, `VariantWatcher`'s
  43 lines on `stopGrace`), and 28 opening with a verb
  the list lacked ("GetTrack fetches", "Extract reads"). **#964's 5-of-5
  spot check measured PRECISION; nothing measured RECALL**, and the list
  recognised 61% of the openers correctly-attached docs use, which is the
  share of displaced blocks it could see. A closed list in a detector is an
  enumeration, and its reach has to be measured: a verb is now listed when
  five correctly-attached docs open with it or when it opened a real
  misattachment, and `docVerbCoverageFloor` fails below 85% (it reads 90%),
  naming the words to add. Don't trim the list for tidiness. A grouped
  `const ( … )` doc's subject is every member, but a member counts as
  documented only by its OWN spec doc or line comment, which is what lets a
  spec doc displaced inside a documented group be caught. Still unseen: a
  subject that has since grown a second doc of its own. (`_test.go` files
  headed this list until #990, and a doc opening with a name nothing
  declares until #994.)
- **…and test files, against their own census and a floor of their own**
  (#990). The 716 `_test.go` files held eleven more, and #989's list would
  have seen four of them. A test's doc opens "… pins …": 1,033 of their
  2,843 subject-first openers, against 2 of the 4,522 elsewhere, so
  `docVerbs` recognised only 48% of them. `testDocVerbs` is derived by
  #989's rule over the test files ALONE and read only there, beside
  `docVerbs`, so non-test files read exactly as #989 measured them. **A
  coverage floor over two populations is measured per population.** One
  85% floor over both passed with "pins" alone added (88.1% overall) while
  the test files sat at 84.5%: the larger population carried the smaller.
  Names resolve the way the compiler scopes them, keyed by directory,
  package NAME and test-ness. An external `foo_test` file sees only its own
  package, and an internal test file also sees its package's non-test names.
  Both directions were negative-controlled on synthetic shapes, since
  nothing collides across the tree's one external test package. A name an
  internal test file sees in both counts as documented if EITHER declaration
  is: answering from the test scope first let a test fake's undocumented
  method, recorded by bare name like every method, strip the production doc
  from under a test's prose about it (a Gemini consult found it; NC7
  reproduces it). Seeing the package's names has a measured price: 14 test
  docs open by naming the production declaration they test, with a listed
  verb ("loadCLIConfig is …"), and only the no-doc condition keeps them
  quiet. Delete one of those production docs and its test's prose is
  reported; restore the production doc rather than moving the test's. This
  guard's walk skips `.`- and `_`-prefixed files, as the go tool does: an
  emacs `.#name.go` lock is a dangling symlink, and one failed the guard
  with a parse error over a file no build reads. Still unseen: a title-line
  doc (`// Name.`, 29 in test files, none misattached).
- **…and a doc that opens with a name NOTHING declares** (#994). "Declared
  in the same package" left that half unread by design, and #989's census
  had seen three (`expectedTeamID`, `ensurePathExists`, `errITunesNoMatch`).
  Measured before building anything, the class was 25 wide: one rename the
  doc did not follow (`routesToForegroundLane`'s said
  `routesToOptimizeChannel`, which #863 retired) and 24 docs written under a
  name no commit on main ever declared, six of them import keepers
  documented as helpers that never existed. **What makes it precise is that
  an English sentence opens with a capital.** An opener that starts
  lowercase or has a camelCase hump (`identifierShaped`) can only be a name.
  A capitalised one-hump word or an all-caps one can be either, and every
  such opener a listed verb followed was prose, 10 of 10 ("It is",
  "Removal is", "DST is"). Followed by anything but a listed verb, 26 of 31
  identifier-shaped openers were prose (JS and CSS names, config keys,
  brands, "silence …" notes), so the arm reads the verb shape only. **The
  lookup is the DIRECTORY, not the file's scope and not the module**: an
  external `foo_test` file's prose names `foo`'s declarations
  (`LooksLikeSnapshotDir`), while a name only another package declares is
  still reported, because a doc opens with its own subject. Predeclared
  names (`nil`, `error`) and a documented function's own parameters count as
  declared (`namesNothingDeclared`). **A test function's doc is read only
  for a test-shaped name** (`testFuncName`, go test's rule). It states a
  premise, and a premise's subject is often a library or a writer: the first
  test file merged after the census (#991's M4A tests) opened four docs
  "dhowden does …", "iTunes writes …" and "QuickTime writes …", which
  failed #994's CI. Brand, tool and unit names ("iOS", "sox", "dBFS") are
  still identifier-shaped wherever else they open a doc; reword the
  sentence rather than exempt the word. **On a clean tree an arm counts
  zero, so the tree cannot show it still works**: with either arm's report
  deleted, the tree scan PASSED (CodeRabbit, round 2), which is "a helper
  nothing calls" and had been true of the misattachment arm since #964. The
  scan is `scanDocblockSubjects(r, root, wholeTree)`, and
  `TestDocblockScanReportsBothArmsOnAFixture` runs it over a synthetic tree
  with exact findings for both arms. Table tests pin `identifierShaped` and
  `namesNothingDeclared` beside it. (The import-keeper rule this bullet
  carried until #996 has its own bullet now, after the next one.)
- **Measure a new detector over sampled HISTORY, not only the tree it was
  written against** (#994). The undeclared-name arm read 20 of 20 on its
  census tree and then met four false positives in the first test file
  merged after it: one tree is one snapshot of the vocabulary. The detector,
  copied into a standalone program, ran over 18 trees sampled along main's
  first-parent history (`git archive <sha> | tar -x`, April to September,
  minutes). It found 29 distinct hits, and the history had already judged
  most of them: a hit a later commit fixed was real by that fix. That run
  showed which condition each false positive needed and what each would
  cost, one real finding. A census of today's tree says what a detector
  finds now; the history says what it would have found, and how often a
  shape it has never seen turns up. And merge main before pushing a
  tree-wide guard: its verdict depends on code the branch did not write,
  and here that code merged while the census ran.
- **An import keeper is dead code, never documentation, and
  `TestNoBlankKeepers` fails on one** (#996). A keeper is a blank reference
  that only names something: a top-level `var _ = pkg.X`, or `_ = pkg.X` /
  `var _ = pkg.X` in a function. Imports are per FILE, so it does nothing
  when the file uses `pkg` elsewhere and keeps an import nothing needs when
  it does not (one kept `io` in a test file because a helper in another
  file uses `io.EOF`). A top-level `var _ = logger` keeps a package-level
  name Go never reports unused anyway. Delete it, and the import too when
  nothing else in the file uses it. **Three hand sweeps each fixed only
  their own scope** (c062ac95 one, #855 three in cmd/bridge, #994 five);
  #825 added one between two of them, and #996 found fourteen more. Five of
  those were in the statement form no census had counted, and one,
  `var _ = (*manifest.Store)(nil)`, sat under "Statically assert the
  Manifest store has the helpers we need. A missing method here will fail
  the build". A nil conversion checks only that a type exists, and every
  real use makes that check. A keeper's premise decays unseen, too: two
  began as their import's only use and turned redundant as their files
  grew. **The sweep reads the four shapes keepers took, and nothing else**
  (`keeperName`): `N`, `N{}`, `&N{}` and `(*N)(nil)`, where N is an
  unshadowed import selector or, at the top level only, a bare identifier.
  All 24 keepers in 37 sampled history trees took one of them. A general
  "has no effect" classifier came first, and six review rounds each found
  a construct it misread: an operator that can panic, a compile-time
  assertion hidden in an array length, a call through a function pointer
  spelled like a conversion. Syntax cannot tell those apart without the
  type checker, so a keeper spelled any other way goes unseen, which is the
  safe direction; the rule above still applies to it. `(*N)(nil)` keeps one
  ambiguity (a call through a pointer-to-function variable with a nil
  argument), and no such exported variable exists in this module, the
  standard library or any dependency. The statement form reads imports
  only (`_ = cfg` marks a local used; the tree has 23), and a selector
  names an import only where no local of that name is in scope, scoped as
  Go scopes it, from the end of the declaring statement (a whole-function
  approximation missed `_ = path.Join` above `path := …`; CodeRabbit). The
  local form is not read in a file with a dot import, and cgo's `C` is
  never read as an import (its import carries the preamble). Two keepers of
  one package are not each other's use (update.go's pair).
  **The one allowance stands on its stated CONDITION, not its path**:
  `allowedKeepers` holds internal/tsnet's `var _ = errors.Is` while its doc
  says "Don't remove until typed errors land.", `typedErrorIn` finds no
  sentinel, `Error() string` method or error interface there, and the file
  uses `errors` nowhere else. Any of those failing is reported, and so is
  an entry whose keeper is gone. On a clean tree the sweep reports nothing, so
  `TestBlankKeeperScanOnFixtures` pins every shape, refused shape, scope
  rule, skip rule and allowance state: 49 of 51 mutations turn it red, and
  five turn the tree red. Of the two that stay green, one drops the CRLF
  normalisation (the scan is CRLF-safe without it) and one widens a check
  that `keep` makes again.
- **A test that sweeps this repo's own files decides from the NAME what it
  opens, before it opens anything** (#993). Emacs locks a file it is editing
  with `.#<name>` beside it. Where it can, the lock is a DANGLING symlink.
  Where it cannot, which is always on Windows (its `filelock.c`), it is a
  REGULAR file holding `user@host.pid:boot`. The symlink fails
  `os.ReadFile`, and the regular file fails `parser.ParseFile` or `node`, so
  a file-type check or tolerating ENOENT each misses one shape. A census
  planted both shapes for eleven extensions in every tracked directory and
  ran the suite. The symlink shape failed **19 tests in four packages** and
  the regular-file shape 6. All were sweeps that picked files by suffix
  alone, among them `TestEveryCitedTestNameExists` and eight tests of the
  console's static JS. `TestNoProductionCodeLowersTheHashCost` reported a
  lock as a production caller of `SetTestHashCost`. Each population takes
  its own rule. A Go sweep skips a name the go tool ignores, `.` or `_`
  (`goToolIgnores`): that file is never compiled, so a test defined in it
  never runs and must not satisfy a citation. `internal/admin`'s static
  sweeps skip `isEditorDetritus` (`.` or `~`) but NOT `_`, because
  `static/*` embeds a top-level `_x.js` and it ships. The citation guard
  asks git whether a doc is tracked BEFORE opening it; it used to discard an
  untracked doc only after reading it, so an unreadable gitignored doc
  failed the run. It also never opens a doc whose name begins with ".",
  which is what covers a tree with no `.git` (a fixture, or a source
  archive), where there is no tracked set. **No directory rule was
  widened**: the guard reads a tracked doc and a tracked Go file under
  `.github/`, and the go tool's `.`-directory rule would drop both. **Plant
  the census, not one probe.** A grep for `filepath.WalkDir` reached 3 of
  the 14 files; the rest used `os.ReadDir`, `filepath.Walk` or
  `filepath.Glob`, whose `*` matches a leading dot. A probe lock named
  `…_test.go` passed the hash-cost guard for the wrong reason, since that
  guard reads only non-test files. Two failures are out of reach of test
  code. A lock at the top of an embedded directory (`static/*`,
  `templates/*.html`, `*.tmpl`) breaks the BUILD with `cannot embed
  irregular file`. And Go's fuzz seed-corpus reader fails on one inside
  `testdata/fuzz/<Name>/`.
- **A timeout is not a failure, and the difference is one flag.** A local
  `go test -race` without `-timeout` uses Go's 10-minute default, while
  the Makefile passes `30m` — `internal/admin` reported `FAIL … 600.758s`
  under CPU contention and passes in **826s** with the flag. The signature
  is a duration landing on exactly 600s with a goroutine dump and no
  assertion. Don't report one as a code failure.
- **A package-local test run cannot see a tree-wide guard.** Renaming a
  test in `internal/manifest` left a docblock citing the old name;
  `TestEveryCitedTestNameExists` lives in `cmd/bridge` and caught it on
  CI's macOS leg after a local `go test ./internal/manifest/` had passed.
  After a rename, run the package that holds the sweeps.
- **Time an event where it HAPPENS, and match interleaved runs by an id.**
  Both errors were made measuring #997. A "serve has returned" marker printed
  from a `t.Cleanup` registered after `drainServeOnCleanup` runs BEFORE the
  drain (LIFO), so it marked the start of teardown. It put a figure into a
  commit message and a docblock that the right marker, printed where `run()`
  returns, then contradicted. And under `-count=N` a late line from one run
  prints during the next, so reading lines by position mixed runs up. Tag
  each line with something unique to its run (its data dir) and match on
  that.
- **A window between two statements is reproduced by PARKING a goroutine in
  it, not by stress.** The analysis pool's count-before-release window passed
  900 idle runs on the dev Mac, failed 9 in 720 under 3× CPU
  oversubscription, and failed on every run once the worker was held inside
  it. When a log line sits in the window, holding it there needs no
  production hook: `logging.Component` resolves `slog.Default` at log time,
  so a test handler that blocks on one message stops the worker right there
  (`loggingtest.ParkOn`, a test-only package both pools share; it was
  `parkOnLog` in `internal/analyze`). **When the window ENDS in a lock
  acquisition, hold that lock instead**: the worker stops exactly there
  whatever it did on the way, and something it stores lock-free just before
  (the transcode pool's worker slot) says when it has arrived. That reaches
  an exit with no log line in it, the transcode pool's success path (#988).
  With neither, oversubscription is the fallback: build with `go test -c
  -race`, then run about three processes per core. Idle stress passing is not
  evidence the window is absent. (#987)

- **A test that never touches the wiring proves nothing.** Three shapes, all of
  which shipped a dead feature with a green suite: a helper nothing calls, a
  type nothing constructs (a stub implementing the interface directly, while
  production passes a different concrete type), and a handler nothing dispatches
  to. Drive the real entry point — `Handler()`, the real Provider, the real
  endpoint — at least once per feature.
- **A docblock that names a test is a claim, and six of them were false.** Two
  stood in for an invariant nothing pinned at all; one was a false safety claim
  on a security boundary (`managed_controls.go` said a test "walks the registered
  routes so a new one cannot be added without a decision" — no test of that name
  had ever existed). `TestEveryCitedTestNameExists` sweeps the tree for cited
  `Test…` names with no definition. Write the guard or name the test that
  actually covers the invariant; do not leave prose asserting a check that is
  not there.
- **A guard that scans SOURCE must parse, or strip comments, when the thing it
  forbids is named in the commentary beside it.** `player_audio.go`'s docblock
  says "Deliberately NOT `dlna.defaultMIMEForExtension`" — a text scan finds that
  and reports the rule as broken, which it did on the first run. Walk the AST, or
  strip; and anchor on an IDENTIFIER, never a string literal, because
  `stripGoComments` blanks literals too.
- **A fixture that omits a live gate describes a different bridge than
  production.** The gate belongs in the BASE fixture with a test overriding it
  both ways, and a fixture that wires a dependency the production path does not
  cannot see that gap at all — `upload.WithReclaimable` had two tests passing the
  option and zero production callers, so every 507 answered
  `reclaimableBytes: 0`. When only the wiring can be wrong, check the wiring.
- **Commit BEFORE the negative control.** `git checkout --` and
  `git restore --source=HEAD` revert the whole file, so a control run against an
  uncommitted round silently takes the round with it. This file already recorded
  the lesson; I hit it twice in one session anyway. And a control that fails to
  BUILD reads as "control invalid", never as a pass — revert the test fixture
  alongside the production line so the control compiles.
- **...and once you HAVE committed, `git stash` is a no-op, which is the same
  trap inverted.** `git stash && go test && git stash pop` against a clean tree
  stashes nothing and runs the test against the FIXED build, so the control
  passes for the wrong reason and reports the opposite of the truth — the only
  tell is `pop`'s "No stash entries found", and it comes at the END. Check out
  the pre-fix commit instead (a throwaway `git worktree add <dir> <sha>`, removed
  after), and **assert you are on the old code** — grep the tree for the
  identifier the fix introduced and require zero — rather than assuming the
  stash took. Found on #941, where a "plain file works too" measurement was
  entirely an artifact of this. (Also: `grep -c … && next` exits 1 on zero
  matches and silently ends the chain.)
- **Negative-control every load-bearing assertion**, and check what the mutation
  actually did. A control that fails to BUILD reads as "control invalid", never
  as a pass — and most "just disable this branch" edits delete a variable's only
  use. A control that mutates the *wrong occurrence* is worse, because it
  passes: identical blocks in two tests in one file, or a first-occurrence
  replace, will silently prove nothing.
- **A fixture must be a value the transformation would actually change**, or the
  test pins nothing (an "upstream metadata always wins" test used a name that
  cleans to itself and passed against code that overwrote unconditionally).
- **Pin cross-repo and cross-cycle contracts with CAPTURED bytes**, never a
  second hand-written copy of the offsets — two copies can be wrong together,
  and were.
- **Assert omitempty absence by unmarshalling and checking key-absence**, never
  a substring probe on the raw body.
- **`hidden === false` says nothing about an ancestor.** UI assertions from JS
  can and did lie; seed a throwaway bridge and drive the console in a browser
  before believing a green suite about UI. `document.visibilityState` is
  `"hidden"` in an automated tab, so `loading="lazy"` images never load and any
  perceived-performance claim measured that way is suspect.
- **A field deliberately left unsynchronised binds TESTS too.**
  `sendErrStreak` is only ever touched from its own run loop, so a test calling
  `noteSendResult` directly must do so with no loop live — before `Start`, or
  after `Stop` (which joins it). One that did neither raced under `-race` on CI
  and was not reproducible locally in 26 runs. Adding a mutex would pay
  production for a test's convenience.
- **A test that boots a server on a goroutine drains it in a `t.Cleanup`, never
  a `defer cancel()` plus a cancel-and-assert tail.** The tail runs only when
  the body completes: a `t.Fatalf` above it Goexits, the deferred cancel fires,
  and the test returns without waiting. Measured on the failing path — the
  serve goroutine had NOT finished when the test binary exited, while
  `t.TempDir`'s cleanup (registered by the fixture earlier, so running later)
  removes the data dir from under a store still checkpointing, and that removal
  is then what gets reported instead of the assertion that failed. macOS hides
  it, because `RemoveAll` over an open file succeeds silently; it surfaces on
  the Windows leg, or as a leaked server racing the next test.
  `drainServeOnCleanup` is the ONE definition: wait on a channel the goroutine
  CLOSES, never `done` (a failure path may already have consumed the exit code
  — `waitForAdminReady` does), and report with `t.Errorf`, since `FailNow` from
  a cleanup skips the very removals the drain sequences itself against. Three
  such tests, written months apart, and the surviving shape had reached only
  the newest — so `TestEveryBackgroundGoroutineDrainsOnCleanup` pins the
  POPULATION, by AST: a text scan for the marker is satisfied by the comments
  that merely NAME a helper, and one for the old `defer cancel()` shape misses
  the sites that never had one. (#944; extended to the in-process loops, and
  `drainLoopOnCleanup` added beside it, in #945 — where a hand-written list of
  five files missed a sixth site that the shape match found.)
- **Two things the drain cannot fix by itself, both found converting the loop
  tests (#945).** A **`defer` beats EVERY `t.Cleanup`**, so a fixture that tears
  down with `defer store.Close()` can have no drain ordered behind it — the
  Close runs first, by construction, on the failing path. Measured on the
  smart-playlist test: pre-fix the regenerator was still running against a
  CLOSED SQLite handle (`store already closed when the loop returned = true`);
  as a `t.Cleanup` registered before the drain, false. And **a fixture that
  takes `*testing.T` must be built on the TEST goroutine** — `disabledIngester(t)`
  was called inside the `go func`, so `t.TempDir`, `t.Fatal` and `t.Cleanup` all
  ran off it; `FailNow` from a non-test goroutine is documented misuse (it
  Goexits that goroutine, not the test), and the `store.Close` registration
  landed at whatever moment the goroutine was scheduled, which can be AFTER the
  drain and so invert the very ordering the drain establishes. Neither is
  visible to an AST shape check, so the guard does not claim them.
- **Windows CI catches wall-clock assumptions** — ~15.6 ms granularity means two
  stamps milliseconds apart are not reliably ordered. Assert on counted events,
  and detect "was this rewritten?" by planted CONTENT, never by comparing mtimes
  (two writes in one tick leave them equal, so the check silently passes on the
  platform most likely to break). Normalize CRLF before any `\n`-literal scan of
  a static file — there is no `.gitattributes` pinning `eol`.
- **`filepath.ToSlash` is a no-op on POSIX**, so a Windows-shaped path handed to
  it on a Mac keeps its backslashes.
- **A test asserting that a message NAMES A PATH must not substring-match the
  raw path.** `%q` escapes backslashes, so a Windows `C:\Users\…` renders as
  `C:\\Users\\…` and `strings.Contains(out, dir)` fails on that platform
  alone. Accept either rendering — the property is that the path is named, not
  the verb it is named with; pinning `%q` instead would go red on a benign
  change to `%s`. **And a quick follow-up push CANCELS the in-flight platform
  legs**, so a green tick on an older SHA is not evidence Windows ran: #941's
  code commit had its Windows and race legs cancelled by a docs-only push
  twenty minutes later, and the defect surfaced on the leg's first real run. (#941)
- **CI's cost is SQLite under the race detector, not the tests' shape.**
  `modernc.org/sqlite` is pure Go, so every page operation is Go code `-race`
  instruments, and the multiplier is ~48x, not the usual eight: deleting 2,000
  rows through `DeleteTracksBatch` measured **1.43s normally against 69s under
  `-race`**. `internal/manifest` was 1392s of a 25m18s race job; nothing else was
  close. **Before optimising a test, measure which PACKAGE the job is spending
  its time in** — a batch that fixed `internal/adminauth` (297s → 40s, real) was
  first written up as fixing the job, which it does not.
- **…and its WALL CLOCK is one package's SEQUENTIAL runtime, which no number
  of cores touches.** `-p $(nproc)` parallelises across PACKAGES; inside one,
  tests run in a single binary unless they call `t.Parallel()`, and that appears
  in **0 of `internal/manifest`'s 138 test files and 0 of `internal/admin`'s
  131**. Measured 2026-09-21: 895s wall over ~1,970 CPU-seconds — the scheduler
  was packing four cores well and the floor was `manifest` alone at 760s
  (`admin` 668s, `cmd/bridge` 218s, everything else under 140s). So the race job
  is SHARDED BY TEST NAME (`.github/scripts/test-shard.sh`, four legs each for
  those two plus one `rest` leg): **895s → 287s**, near-linear, with
  `cmd/bridge` at 218s the new floor — splitting `rest` further buys nothing.
  The gate's critical path is now `test (windows-latest)` at 353s, so measure
  again before optimising the race job any further. **A self-hosted runner is
  the WORSE answer here**: measured at ~1.85x (the dev Mac runs `manifest` in
  410s against the runner's 760s), it is less than sharding, GitHub's own
  guidance is that self-hosted runners do not belong on a PUBLIC repo (a fork PR
  runs arbitrary code on the machine), and standard hosted runners are free for
  public repos so the extra legs cost nothing billed.
- **A sharded suite's whole risk is a test that runs in NO shard** — every leg
  reports PASS, the gate is green, and nothing in the tree notices. Four guards,
  each of which caught something: the partition is verified as **SET EQUALITY**
  against the full list, never a count (a matching sum is also what you get when
  one name lands in two shards and another in none); `-run` is **ANCHORED**
  `^(A|B)$`, because it matches each `/`-part UNANCHORED and `internal/manifest`
  really has **23 name pairs where one is a prefix of the other** (unanchored
  `TestAnalysisCoverage` selects 2, anchored selects 1); discovery runs under
  **the same `-race`**, because `-race` defines the `race` build tag, so a
  listing taken WITHOUT it describes a different build — and a `//go:build race`
  test would then be in neither the listing nor any shard. Not hypothetical:
  this repo already carries `racefixture_race_test.go`. (It is also strictly
  less work — the non-race listing builds a second binary nothing uses.) And the
  `rest` leg asserts the WORKFLOW MATRIX schedules the exact index set
  `0..n-1` of every group it skips, because checking that the group is merely
  MENTIONED passes when one leg is deleted and 208 tests then run nowhere.
  **Fuzz targets belong in the partition** — without `-fuzz` they run their seed
  corpora as ordinary tests, so dropping `Fuzz*` retires every corpus from CI
  with no red X. The shard COUNT lives in the script
  (`SHARDED=(manifest:4 admin:4)`), never in the matrix: two places that must
  agree about a partition can disagree silently, since a leg passing a different
  count partitions the same names differently.
- **`set -e` does not fire inside an `if` condition, so `[ "$x" -lt N ]` on
  caller input is not a guard.** A non-numeric value makes both halves error,
  the condition evaluate FALSE, and execution carry on — `test-shard.sh manifest
  abc` printed two lines of raw `[: abc: integer expression expected` and then
  failed with "is empty (more shards than tests?)", a confident wrong diagnosis.
  Compare against the admitted STRING set instead, which also disposes of `007`
  (bash `test` reads a leading zero as OCTAL while awk reads decimal, so the
  bounds check and the partition disagree about which shard it is) and of a
  21-digit value (bash rejects it as not an integer at all, so a numeric guard
  could never bound it). Same family as the grep below.
- **A guard that fails closed but prints NOTHING is half a guard.** `grep` exits
  1 on no match, and under `pipefail` inside a command substitution that killed
  `test-shard.sh` before the caller could report anything — on the single case
  it most needed to report, a sharded group with no matrix legs left. It exited
  1, correctly, with an empty log. `|| true` on that pipeline.
- **A test fixture that bulk-loads or bulk-deletes rows uses the BATCH APIs.**
  `UpsertTrackBatch` / `DeleteTracksBatch` exist because the one-at-a-time calls
  each take `Store.mu` and run their own BEGIN/COMMIT/fsync; a fixture calling
  the singular form pays that per row, under the detector. Seeding 4,000 rows:
  19.0s against 6.8s.
- **Size a SQLite-heavy fixture by build tag when the test has no concurrency.**
  The compaction tests seed, delete and vacuum on one goroutine, so `-race` can
  find nothing in them — but the code paths must still RUN there, so shrink the
  fixture rather than skipping the test. The property that needs the full size is
  asserted in the `!race` build, which is exactly what the macOS and Windows legs
  run (`go test ./...`, whole suite, no `-race`, ~3 min). **Gate the
  "does this fixture still reproduce the hazard" assertion on the full size**, or
  it passes vacuously at the small one.
- **Fuzz targets need `-fuzzminimizetime 1s`** — the 60s default burns CPU
  without incrementing `execs` while the run still says PASS, so the failure
  mode is a target that looks like it ran and did not.
- **Stacked PRs get NO gate CI here** (the workflows are `pull_request:
  branches: [main]`), and retargeting alone doesn't fire them — amend for a
  fresh SHA after retargeting. Capture each child's fork point BEFORE amending
  its parent. `git add -A` with another branch's untracked files on disk sweeps
  them into your commit; use explicit paths.
- **A rate-limited bot's silence is not approval — and CodeRabbit's notice is
  both invisible and TRANSIENT.** Both bots post a quota notice and then don't
  review; "no comments" after one means unreviewed. CodeRabbit's is an HTML
  COMMENT inside its walkthrough body
  (`<!-- … rate limited by coderabbit.ai -->`), NOT a comment of its own, so
  every way of listing the thread reads as normally-reviewed. The allowance is
  derived from RECENT USE — one review per *hour* off ~88 attempts in 7 days —
  so it binds hardest on a multi-PR day, which is already the day this file says
  review gets skipped.
  **While you are blocked**, the marker is greppable and that is when it
  matters: `gh api --paginate repos/acoseac/1-bit-bridge/issues/<pr>/comments
  --jq '.[].body' | grep -c "rate limited by coderabbit"`. **Afterwards it is
  not**: CodeRabbit EDITS the marker out of the walkthrough when the review finally
  runs, so a later 0 is not evidence a review happened. Auditing after the fact
  asks a different question — whether the bot left any review or inline comment
  at all. Ask for the pass rather than waiting it out — but **`@coderabbitai
  review` does NOT clear a plan-limit pause**: on 2026-09-18 it answered
  "Review rate limited" on all five paused PRs, and the notice's own small
  print says the command applies only when AUTOMATIC reviews are paused. What
  works is the **"Run this review for free" checkbox inside the walkthrough
  comment** — tick it by PATCHing the comment body (`- [ ]` → `- [x]` on the
  `checkboxId` line; the repo owner may edit the bot's comment), and the
  review runs within minutes. `/gemini review` still works as written.
  CodeRabbit's real pass says "No actionable comments were generated" or
  "Actionable comments posted: N"; Gemini's says it has no comments to
  address. Anything less than one of those is not a round.
  **#900 is the live instance**: merged nineteen minutes after opening with the
  notice standing, and it carries zero CodeRabbit reviews and zero inline
  comments to this day. #901 was limited from its FIRST review too, which left
  both fix rounds unread while all twelve checks stayed green — it was only
  caught by looking for the notice on purpose.
  **And the verdict is in the WALKTHROUGH, not in `pulls/N/reviews`.**
  Querying the reviews API showed CodeRabbit's last review sitting on an
  older commit for five of six PRs while every one of them had in fact
  passed on its current head — the pass is an EDIT to the walkthrough issue
  comment, which carries the `headCommitId` it covers. The check that works
  is grepping that comment for the verdict string and comparing its
  `headCommitId` to the PR head. Absence of new findings is not a pass, and
  saying so out loud without checking is how this was learned twice. (#967–#972)
- **Read a PR's reviews and comments with `gh api --paginate`: it returns 30
  a page, and a long PR's newest round is on the page it drops.** On #996
  (40 reviews, 38 review comments) every check read page one only. Two
  CodeRabbit findings (12:17, 12:58) and two clean Gemini passes were never
  fetched. The report that went out said "CodeRabbit clean, Gemini silent,
  probably quota", and that story reached the engineering log, the PR body
  and a PR comment before an unresolved-thread query (GraphQL
  `reviewThreads`) showed otherwise. This is the
  complement of the walkthrough rule above, not a correction of it: the
  walkthrough's coverage and merge risk say a review RAN on a head, while a
  review that FOUND something posts a review ("Actionable comments posted:
  N") and inline comments. "Covered, and nothing seen" is clean only when
  every read was paginated. Before a merge, list the unresolved threads, and
  page that query too: `reviewThreads` is a connection, complete only once
  `pageInfo.hasNextPage` is false (CodeRabbit, on the first draft of this
  very rule, which said the query "returned them all").
- **A fix round needs a FRESH pass from every bot, not just the one that
  found something.** Gemini does not re-review each push: after four rounds
  on one PR its last review still predated every fix commit on five of six
  branches, and `/gemini review` on each head then produced two more real
  findings. Ask both, per head.
- The `test (windows-latest)` leg was non-blocking until 2026-09-01 — a
  permanently-red non-blocking leg hides every genuine regression behind it.
- **Merging with review comments outstanding is a process failure**, not a
  shortcut — expect two rounds minimum. Verify a bot's severity label before
  acting: recurring false positives here include `windows.Errno` vs
  `syscall.Errno` (an alias — and the "fix" doesn't compile), guards proposed
  after `url.Parse` for a backslash host (Go refuses it outright), and claims
  that `omitempty` keeps a non-nil empty map. Reply on the thread with the
  evidence when declining.

- **`TestEveryCitedTestNameExists` scans `_test.go` COMMENTS too.** It read
  only non-test source, so a docblock in a test file naming a renamed
  sibling was invisible — fifteen were, including a first sentence on the
  fsync contract claiming a property the test beneath it does not pin.
  Test files are parsed with go/parser and only their comment groups are
  scanned (a string literal can spell a test name and is not a citation).
  A historical note naming a removed test is REWORDED rather than exempted,
  and the guard scans its own file, so its examples cannot name one either.
  (#921)
- **…and the TRACKED `.md` docs, with three exemptions and one hard
  constraint.** It read Go only, which is how #945's rename left three stale
  citations in `CLAUDE.md` and the log — the auto-loaded file being the most
  expensive place for a stale guard name. Exempt: `ops/plan-*.md` (a plan
  names coverage it INTENDS — thirty of the forty stale names were in one,
  all correct as written), three metasyntactic `-run` placeholders, and one
  test owned by the private conductor repo. **TRACKED is load-bearing**:
  `ops/*audit*.md` and friends are gitignored and differ per machine, so the
  first draft failed on this one and would have passed on CI and a fresh
  clone — a verdict that depends on untracked local files is not a guard. The
  discriminator is `.git`: absent means a fixture tree with no ignore rules,
  present means git must answer and a failing git is reported, never quietly
  widened back. **Elide the `Test` prefix** (`…ServeDrainsOnCleanup`, or
  `…_FileHandler_…` for an underscore name) when a note must name a test that
  no longer exists: the pattern needs the prefix itself, so the name stays
  readable without claiming to exist. Four stale
  citations were corrected, not exempted — three plain renames and the guard
  name #945 retired — plus one FALSE POSITIVE, a UA string literal that was
  being discussed rather than cited. Keep the two apart: a stale citation is
  repointed or elided, a false positive means the prose should stop spelling a
  token it is only talking about. (#946)
- **…and every name `go test` runs, which is more than `Test` and an
  uppercase letter.** The pattern took only that shape, so no citation of the
  223 tests named `Test_…` (internal/dlna's convention) was ever checked, and
  five names of tests that did not exist were cited, one of them the
  engineering log's record of what guards a DLNA invariant. It is `go help
  testfunc`'s rule now: `Test`, then a letter of any script that is not
  lowercase, a digit, or underscores and a letter or digit. **Measure the
  population a pattern must cover, not the example that prompted it**: 32 of
  the 223 continue in lowercase after the underscore, so "`Test_` then
  uppercase" would have left them unguarded. **A `Test_` citation passes as a prefix only if it stops on
  a word boundary**, ending on an underscore or just before one
  (`…_CDS_Search_` names that family). The prefix rule had been "verifying" a
  fixture FOLDER name quoted in a comment against eighteen unrelated DLNA
  tests. camelCase has no delimiter to check and keeps its leniency. **The
  walk skips a directory below the root that has its own `go.mod`**: another
  module, which `go test ./...` never runs. `.claude/worktrees/` is the case
  that exists. Three leftover worktrees still held the stale citations, so
  the extended guard went red in the main checkout alone, and an old copy's
  tests can satisfy a citation this tree no longer backs, which passes. That
  is the one directory rule added; #993's reason for not borrowing the go
  tool's `.`-directory rule (`.github/`) still holds. (#995)
- **The Dockerfile REQUIRES BuildKit, and its builder `FROM` says so through
  an invalid fallback:**
  `--platform=${BUILDPLATFORM:-this-Dockerfile-requires-BuildKit--build-with-docker-buildx}`.
  BuildKit always sets `BUILDPLATFORM`, so the fallback is inert there
  (measured: amd64 and cross-compiled arm64 binaries byte-identical to the
  bare form's, `--check` clean). The legacy builder — what `docker build`
  falls back to when the buildx plugin is missing, which is the DEFAULT with
  Ubuntu's and Debian's `docker.io` — sets nothing, and the bare form died
  with a platform-regex dump naming neither BuildKit nor buildx. **Never
  replace it with a real default.** Under BuildKit a declared `ARG` default
  does not fill a gap, it REPLACES the automatic value, globally and in a
  stage (`ARG BUILDPLATFORM=linux/s390x` echoes `linux/s390x`; `ARG
  TARGETARCH=bogus` echoes `bogus`), so `ARG BUILDPLATFORM=linux/amd64` puts
  every arm64 host's Go compile under QEMU and a `TARGETARCH` default ships
  one arch's binary in every leg. `:-linux` did build a working image on the
  legacy builder (measured on amd64) and was refused anyway: a second path no
  CI runs, for a builder Docker has deprecated. #451 had already shipped one
  such path — `ARG TARGETOS=linux` "so a non-BuildKit `docker build` still
  builds", dead from the day it landed because the `FROM` failed first. `:?`
  would read better; `:-` is the form every Dockerfile lexer parses. `make
  docker` is `docker buildx build --load` behind a `check-buildx` guard.
  **`docker.yml` runs only on tags and dispatch**, so a Dockerfile PR gets no
  CI build at all: verify on a real daemon under BOTH builders
  (`DOCKER_BUILDKIT=0` forces the legacy one). (#983)

### <a name="review-2026-09-22-fixes"></a>2026-09-22 — review of the #959–#966 fix window

The eight PRs that closed the window below had merged the same day with
no review as a window. A pass over `9f1289f`..`9b1a5a8` found eight
defects plus one adjacent; shipped as #967–#972. The record is in
`ops/engineering-log.md`.

**Every defect was a behaviour the existing tests still accept**, and
THREE were introduced by the fixes they sit in — one release or one
commit from the rule they broke. That is the thing to expect when
reviewing a hardening batch: the new code is where the new defects are,
and its own tests were written by the same reasoning that missed them.

- **A fix placed in FRONT of a check can destroy that check's input.**
  v13's SSND narrowing ran ahead of v12's sentinel refusal and handed it
  `0xFFFFFFF7`. Neither PR was wrong on its own; the ORDER was.
- **A range gate does not protect a value that can wrap into the range.**
  A bot found an `int` overflow landing on exactly 3600 — inside the
  plausibility band the same PR was adding.
- **An escape hatch re-opens the failure its own PR fixes.** The
  empty-store exception on the variant delete took two more rounds to
  narrow: an unlink outside the store explained the store, then an
  unlink before a mid-request unmount did. Both were bot findings on my
  own fix, and the second is the sharper: "we emptied it" and "it
  unmounted" are the same stat.
- **Four of eleven accepted findings were on my own fixes.** Two rounds
  is the documented floor; #968 took five.
- **Three findings were declined by MEASUREMENT, not argument** — escape
  analysis for a claimed allocation, two red tests for a proposed
  `os.Lstat`, and the top-level `ReadDir(1)` for a claimed symlink
  escape. One more was declined with its premise CONCEDED: the config
  really is validated, but the invariant is split across two validators
  chosen by deployment mode, and the failure mode of dropping the guard
  is the vacuous pass the PR existed to fix.
- **"No new findings" is not "reviewed", and I reported it as such
  before checking.** The verdict lives in the walkthrough comment, not
  `pulls/N/reviews`. Rule and the check that works under **### Build, CI,
  and test discipline**.
- **Disjointness decays across review rounds.** Six branches were
  file-disjoint at open and I said so; by round 3 two of them shared a
  test file. Re-derive the overlap matrix before merging.
- **Commit BEFORE the negative control — hit again**, one commit after
  reading the rule, and a tree that did not build was pushed.

### <a name="review-2026-09-22"></a>2026-09-22 — full review of the post-v0.2.0 window

Twenty-four PRs (#934–#958, ~19.5k lines) had merged since the `v0.2.0`
tag with no review as a window and nothing deployed. Three parallel
sweeps plus direct verification of every finding; shipped as #959–#966.
The record, with every measurement, is in `ops/engineering-log.md`.

**The build was healthy and the wire contract intact** — `gofmt`, `vet`,
the whole suite, the race suite on every changed package, `PROTOCOL.md`
byte-identical with the iOS mirror, `ExtractorVersion` correctly bumped,
nightly fuzz green. The adversarial pass over #935/#956's new
MP3/MP4/AIFF/WAV parsers found no overflow, no negative-length slice and
no division by zero. **Every defect was one the suite cannot see**, which
is the shape to expect in a window whose own tests all pass.

- **The enumeration failure, again, and this time on the destructive
  site.** #937 named three reapers; there were five, and the two it
  missed were `DELETE /v1/upscale/variants` and the serve reap's mount
  check. #940's shared walker did not get the root resolution #937 had
  given its read-only twin, so `--gc` could unlink the variants directory
  itself. **Grep for the PATTERN, not the symptom** — and when a rule
  names its sites, the count is the thing most likely to be wrong.
- **A fix's own blast radius is a thing to check.** Resolving the walk
  root would have made every known sidecar look like an orphan, because
  the known set keys on the configured spelling. The safe fix needed a
  second half the finding did not mention.
- **Three tests asserted the defect.** `TestPlausibleDuration_TruthTable`
  required `{"tiny positive", 1e-9, true}`; `TestCheckSidecarPaths`
  required the stale `analyze --force` advice; `TestRejectedCertGrading…`
  listed the clamping line among its ACCEPTED forms. A want-list encodes
  a policy as readily as a fix does.
- **A docblock that matches its code exactly can still be the bug.**
  `plausibleDuration` defended the ceiling and said so precisely. The gap
  was in the policy, so neither the comment nor a test could show it.
- **Bot findings earned their rounds, and one extended a fix of mine.**
  CodeRabbit found that the round-1 SSND fix missed the 8-byte-prefix
  spelling of "empty" — reproduced before it was believed — and that
  `certValidityText` tested a duration where the question is a calendar.
  Two proposals were DECLINED on evidence: defaulting a bare
  customEndpoint to the active listen port would re-open #953/#936, and
  `bridge doctor --rate-limit` does not exist.
- **Merge order from file overlap, not instinct.** Seven of the eight
  branches were mutually disjoint; the docblock PR shared files with four
  of them, so merging it LAST cost one merge-from-main instead of four
  rebases. `main` merged INTO the branch, not a force-push, so bot-review
  history stayed addressable.

### <a name="review-2026-09-18"></a>2026-09-18 — findings review on the post-#899 window

An external full-tree pass (the 2026-09-10 invariants re-checked
mechanically, plus targeted reads of everything after #899) came back with
four bugs and eight quick wins; every one was verified against the code
before acting, and one proposed fix was declined on evidence. Shipped as
PRs #915–#921 plus a field report folded in (#920). The named classes from the
2026-09-10 sweep — `indexed_at`, `enriched_at`, `LIKE` on path predicates,
`WipeFilesystemTracks`, empty-set GC, `loadCLIConfig`, feature gates,
`SetPostScanHook` — are still closed. The record is in
`ops/engineering-log.md`.

- **A same-week fix that does not match the wire the UI actually sends.**
  #913's gate read the parameter, the player always sends it, and the test
  used an empty query. Gate on the PARSED value, and test with the client's
  real request — a fixture must be the value the transformation would
  actually change, and here that value was the default one.
- **A sibling loop that missed the guard.** The lyrics sweep's pass two had
  no scan latch, and the test that pinned pass one could not reach it. The
  same enumeration failure the 2026-09-09 batch names, one function wide.
- **One consumer that never went live when its writers did.** The integrity
  watchers kept a boot-time `variantsDir` while every other consumer had
  been made live, and the Jobs chips then reported them off while they
  ticked. "Never split a field's halves", with the display half following.
- **Verify a proposed fix's MECHANISM before taking it.** `QueryEscape`
  before an html/template URL attribute double-encodes; bounding the
  ticket's shape was the right thing. Same rule as the 2026-09-10 note on
  the lyrics-cooldown patch.
- **Stale claims corrected:** AGENTS.md still said `WipeAllTracks` on a
  root flip — a THIRD copy of the claim CLAUDE.md's top list corrected on
  2026-09-06; the pairing bullet denied a limiter PROTOCOL.md documents;
  the `csrfGuard` docblock said `Origin: null` is allowed while
  `originMatchesAdmin` refuses it; two artwork comments described a test
  that ACCEPTS PNG as one that rejects it. When you correct a claim, grep
  for its twins in every file that restates the rule — AGENTS.md is one.

### <a name="review-2026-09-10"></a>2026-09-10 — full-codebase review

Three parallel sweeps (the unreviewed Atlas lyrics tier; the least-audited
packages; a mechanical invariant sweep across all 46) plus a pass on the exposed
API/admin surface. Shipped as PRs #892–#899. **The worst defect was not in the
new code**: `internal/trash` had never worked on a multi-root bridge, and in the
overlapping-names case it trashed a real file and reaped the manifest row of the
one still on disk. The unswept window supplied the rest.

- **The mechanical sweeps came back CLEAN, and that is worth as much as the
  findings.** `indexed_at` (17 sites), `LIKE` on path predicates (104 + 24),
  cutoff arithmetic (20), ticker validation (52), goroutine lifecycle (55),
  unbounded caches, error-string status parsing (8), `http.Client` probe/mutation
  separation (24), defer ordering (11). Don't re-derive these; the record is in
  `ops/engineering-log.md`.
- **Where the defects actually were: code nobody had reviewed, and the seam
  between two subsystems.** Six of the seven PRs fixed something in the
  2026-09-09 lyrics window or at a boundary — trash↔fs, upload↔trash,
  console↔sweeper, one GC's guard versus its sibling's. A rule held inside each
  package and did not cross.
- **The same enumeration failure, three more times.** A fix that lists the sites
  it covers misses one: the empty-set guard existed in two of five reapers, the
  positional-scope guard in five of seven commands, the `omitempty` time rule in
  three of thirteen structs. Every one was fixed by grepping for the PATTERN and
  writing the sweep test in the same PR — and each sweep found a site the
  enumeration had missed (`artwork --gc`, `internal/upload`).
- **Two tests asserted the defect in their own comments.** `TestAtlasLyricsStats`
  said "a/3 is addressable and still has no row" about an instrumental track that
  the candidate query will never offer again;
  `TestATransientReleaseFetchFailureWritesNoVerdict` asserted `wantErr: true` on
  a sweep-ending bug, with one candidate seeded so it could not tell "skipped"
  from "over". A test can encode the bug and pass forever.
- **Three findings came from bots and were real**, and one suggested patch was
  wrong in both halves — Gemini saw a redundant lock acquisition in the lyrics
  cooldown and the defect underneath it was that the cooldown re-armed itself
  every tick and never expired; the proposed guard compared a WRAPPED error with
  `==` and inverted the condition. Take the observation, verify the mechanism,
  write your own fix.

### <a name="loupe-2026-09-09"></a>2026-09-09 — LOUPE on the 2026-09-04..09 window

The window's own three hardening batches (#849-851 lyrics, #852-858 CLI,
and #859-862 compaction) refuted almost everything, as expected. **Every
confirmed finding was in the unswept half** — the DSD renditions, the login
tickets, the export, managed controls — and the two worst were sites where
a fix that landed IN THIS WINDOW did not reach a sibling.

- **When a fix enumerates the sites it covers, the enumeration is the
  defect.** #852 restored the live feature gate on `POST /v1/upscale` and
  `DELETE /v1/upscale/variants` and its own test file says "**Both**
  mutation handlers" — there are three, and the third,
  `POST /v1/upscale/batch`, walks the WHOLE LIBRARY. Any bearer-token holder
  could start a library-wide sox run on a bridge reporting
  `upscaleEnabled: false`. #856 added the `fs.NArg()` positional guard to
  `enrichment retry` and left the four `--filter` commands unswept, so
  `bridge render "Kind of Blue"` rendered the library. **Grep for the
  pattern, not the symptom, and write the sweep test in the same PR.**
- **The suite proved both holes rather than catching them.**
  `batchFixtureWith` never called `WithUpscale`, so every batch test ran
  against a bridge with the feature OFF and asserted 202. **A fixture that
  omits a live gate describes a different bridge than production** — the
  flags belong in the BASE fixture, and a decorate hook can still override.
- **A behaviour-preserving fix needs a test that fails without it.**
  Reverting the `CanDecodeVia` extension gate left the whole package green.
  Count what must not happen (`LookPath` calls) rather than timing anything,
  and check the equivalence the fix rests on instead of asserting it in a
  comment.
- **`ExtractorVersion` was not bumped for the lyrics batch**, so #849/#850/
  #851's three changes to what extraction PRODUCES were inert on every
  already-scanned track, forever, with no error and no log line. Their own
  tests pass because they call the extractor directly. **A test that calls
  the extractor cannot see the skip gate.**
- **A managed CONTROL must own the settings FIELD that performs it.** #876
  gated the actions at the route table; `PATCH /api/settings` has no control
  gate and `managedFieldsIn` consulted only `managedSettings`, so one
  authenticated PATCH of `updateAutoInstall` reached the binary swap and the
  process restart that `updates` and `restart` both exist to refuse. Derive
  the implied field set FROM the control — telling the control plane to also
  list them makes correctness depend on a second program getting a list
  right, which is what route-table placement was chosen to avoid. **And
  report the widened set to the console**, or every save on a managed bridge
  supplies a refused field and the PATCH fails whole.
- **The one unauthenticated endpoint deserves a second look at every write.**
  `GET /login/ticket`'s miss branch rewrote the ticket file unconditionally
  while its comment said "when pruning removed something" — `prunedTickets`
  gave no way to ask. Measured 3.93 ms/req against 159 µs idle, and eight
  flooding clients took an authenticated `GET /api/stats` from 278 µs to
  33.1 ms, under the mutex `ValidateSession` takes. It was also an oracle for
  "a login link is live now".
- **`mux.HandleFunc("GET …")` also matches HEAD.** A HEAD burned the
  one-time ticket, because redemption deletes before judging. Anything that
  probes a link before the human clicks — a mail scanner, an unfurler, a
  proxy — spends it.
- **`truncated` must be a fact about the DATA, not about the loop.** The
  export's cap check ran BEFORE each fetch with a page size that divided the
  cap, so `len` could never exceed it: the trim was unreachable, its comment
  described an impossible overshoot, and a history of exactly 100,000 rows
  shipped `truncated: true` about an export that omitted nothing. The page
  size now deliberately does NOT divide the cap, and a test pins that — if
  they become commensurate the distinction collapses silently.
- **A test seam is per-SERVER, never a package var.** Shrinking the export
  cap through package globals is a write the race detector can pair with a
  live handler's read, and `buildExport` read the cap twice, so a change
  between them could panic the slice. Read once into locals; put the seam on
  the struct.
- **The scratch pre-flight is sized for every LANE**, not the largest single
  job — the pool runs `EffectiveWorkers()` concurrently on distinct dedup
  keys and holds that many Stage A intermediates at once. The docblock
  asserted the opposite. The auto-optimize sweeper had it right all along.
- **`FFmpegInfo`'s zero value granted the MP4 fallback** against its own "the
  zero value grants nothing" docstring — a nil `MissingBinaries` has
  `len() == 0`. Not live, but a trap for the extension gate, which passes
  exactly that value.
- **`pickPlayableVariant` had no `pcm-` arm**, so a DSD track whose only
  rendition is the faithful tier read as unplayable in the web player — with
  the FLAC sitting right there. `optimized-` covers `optimized-dsd-` by
  prefix, which is why eight other prefix sites were swept and this one was
  not.
- **`BRIDGE_DSD_FIXTURE_TESTS` appeared nowhere outside the test file that
  reads it**, so the alias-rejection differential, the level-parity clip
  pin and the CCIF check had never run since they were written — while this
  file cites their numbers as settling the decimation design. They take 2.8
  seconds. Run on Debian trixie with apt sox 14.4.2 + ffmpeg they reproduce
  every number here to the decimal (stopband −116.84 dB, alias delta +0.00 /
  +0.38 dB, rendition −0.97 dBTP at +5.00 dB applied gain) — a VERIFICATION,
  not a correction. `make measure-dsd` and a `dsd-measure` CI job now run
  them. **A gated test that skips looks exactly like one that passed**, which
  is why the job PRINTS the decoder list.
- **The fuzz-target package list here omitted `internal/upload`** (2 targets),
  and with it the web-upload path validation from the "three untrusted-input
  surfaces" enumeration. Counting targets needs the file:name pair —
  `grep '^func Fuzz' | sort -u` says 36, because `FuzzNormalize` exists in
  both `internal/dupes` and `internal/lyrics`.
- **`stripGoComments` blanks string LITERALS as well as comments.** A sweep
  test anchored on a flag NAME (`fs.String("filter"`) matches nothing and
  passes vacuously; anchor on the identifier. Only the `checked == 0` floor
  caught it — every scan-based guard needs one.
- **Two process failures worth more than the fixes.** I ran a negative
  control BEFORE committing that round, and the control's `git checkout --`
  restore took the uncommitted helper with it; the `git add -A` that followed
  pushed a tree that did not build. *Commit before the control, always.* And
  three DSD tests failed on a machine with 6.8 GB free and a 20 GB Go build
  cache — `df -h /` before reading the failures, as this file already says.
  With the lane multiplier those fixtures wanted 11.4 GB, so they were also
  scaled down: **a unit test must not depend on the host's free disk.**

## Licensing — FSL-1.1-MIT (relicensed 2026-08-20; was MIT)

The bridge is licensed under the Functional Source License 1.1 with the MIT future
grant (SPDX: `FSL-1.1-MIT`). Rationale: block competing commercial use — another app
shipping the bridge as its companion server, or a third party selling it as a hosted
service — ahead of the planned first-party cloud offering, while keeping self-hosting
free and the source public. What matters when touching license-adjacent surfaces:

- **Prior releases (≤ v0.1.9) were published under MIT and remain MIT forever** — a
  relicense is forward-only. Never claim otherwise in docs or release notes.
- **Each FSL release auto-converts to MIT two years after it ships** (the Future
  License grant inside `LICENSE`). That grant is the goodwill half of the design —
  don't remove or weaken it.
- The license name lives in FIVE places that must stay in sync: `LICENSE`, the README
  badge + `## License` section, CONTRIBUTING.md's inbound=outbound note, the
  Dockerfile's exec-boundary comment, and `org.opencontainers.image.licenses` in
  `.github/workflows/docker.yml`.
- **No per-file license headers** — the repo-level `LICENSE` governs; don't start
  adding them (CONTRIBUTING.md says the same to contributors, inbound = outbound,
  no CLA).
- The licensor string is `acoseac`, kept for continuity with the original MIT notice;
  revisit alongside the pending ars.md entity-name review if a legal-name change is
  ever wanted.
- GPL/LGPL tools (sox / ffmpeg / fpcalc) remain exec-boundary-separated processes —
  the relicense changes nothing about that analysis; the Dockerfile comment documents
  it.

## Repo clean-up

Pre-push:

```sh
make fmt vet test build-all
```

CI now runs the gate on every PR (`.github/workflows/gofmt.yml` = the fmt check, `gate.yml` = vet + test + build-all on a runner that doesn't OOM under the `-race` peak). Still run `make fmt vet test build-all` (or `make check` in the inner loop) locally first and paste the clean output into the PR body — local-green keeps reviewers off the runner's critical path, and the CI check backstops it. The CI workflows deliberately mirror the local gate command-for-command; keep them in lockstep if either changes.

Releases *are* wired up: `.github/workflows/release.yml` runs goreleaser on tag push (`git tag v0.1.0 && git push --tags`), producing signed+notarized darwin archives and unsigned linux / windows archives as a draft GitHub Release. Windows Authenticode signing is pending — tracked against the next release once SignPath Foundation approval lands. Edit the auto-generated release notes and publish; the `README.md` install recipe works for end users from that point. **Alongside it, `.github/workflows/docker.yml` publishes the multi-arch image `ghcr.io/acoseac/1-bit-bridge` (linux/amd64 + arm64; tags `X.Y.Z` / `X.Y` / `latest`) to GHCR on the same tag push** — the package is public (set once with v0.1.7), so future releases need no manual step. Keep `Dockerfile`'s `ARG GO_VERSION` in step with go.mod's `go` directive or the image build fails (the alpine golang image runs `GOTOOLCHAIN=local`; this broke the first v0.1.7 image build). To publish an image for a frozen tag whose own `Dockerfile` predates a fix, dispatch `docker.yml` with `tag=<ver>` + `ref=main`. Full procedure + per-release hygiene: `docs/release-process.md` → **Container image (GHCR)**.

## Documentation refresh on each release

**See `docs/release-process.md`** — which docs to update per release, which NOT to touch, the process, gotchas, after-the-tag steps.

**User-facing docs moved to 1-bit.app (2026-06-09).** The overview / setup / features / troubleshooting / privacy pages now live in the **`acoseac/1bitapp`** repo (local `~/dev/1bitapp`), published at **`1-bit.app/bridge/*`**. This repo's `docs/*.html` are **redirect stubs** to those URLs — don't edit them. Per-release doc work *here* is just the `README.md` status bump + the cross-repo logging/privacy audit (`docs/release-process.md`); user-facing copy is updated in the `1bitapp` repo. `docs/docker.md`, `docs/deployment/*`, `PROTOCOL.md`, and `.nojekyll` stay.

**Public-facing prose names the project entity as `ars.md`, not `acoseac`** (set 2026-06-02, PR #344). In any reader-facing text — the `1-bit.app/bridge/*` pages (in the `1bitapp` repo), README prose, GitHub release notes, the App Store description — refer to the entity that runs no backend / receives no data as **ars.md**. (This was named to match the former `support@ars.md` contact. The contact is now `support@1-bit.app` and the site is `coseac.swiss`, so the rationale is obsolete. The rule is unchanged pending a deliberate review of the entity name.) `acoseac` is reserved for things that are genuine identifiers and MUST stay verbatim: GitHub repo URLs (`github.com/acoseac/…`), the `acoseac.github.io` Pages domain, shields.io badge URLs, and the launchd / log / bundle identifiers (`com.acoseac.*`). The author's personal name "Arsenie Coseac" in footers also stays. Don't sweep-rename `acoseac` blindly — distinguish prose from identifiers.
## External consultation (Gemini)

When you hit a non-obvious decision — Go concurrency subtleties, tsnet integration nuances, framework version-specific behaviors, algorithm tradeoffs that aren't covered in this CLAUDE.md or visible in the code — **consult rather than guess**. Two routes, and prefer the first: **`python3 ~/dev/gemini-review/consult.py --question-file q.md --context <file>`** sends one focused question directly (key at `~/dev/gemini.api`, header-only, never logged) and comes back in seconds, so a question no longer has to interrupt the work; **or** formulate the question and share it with the user, who routes it through Gemini and relays the response back. The direct route is what makes "don't stop, resolve it" achievable — see [docs/LoupeReviewCycle.md](docs/LoupeReviewCycle.md) § "Consulting mid-run". The cost of one extra round-trip is small; the cost of shipping wrong is bot reviews, follow-up PRs, regressions visible to operators running the bridge.

The pattern that works:
1. Diagnose the problem in your own words first — don't outsource the thinking.
2. Write a question that includes context: what you tried, what you observed, what your current hypothesis is, what alternatives you're considering.
3. Share the question with the user, wait for the response.
4. Apply the fix grounded in the response. Cite the consultation in the commit message so future sessions can trace the rationale.

Lean toward consulting whenever you'd otherwise ship code with a "I think this is right but I'm not 100% sure" caveat. Worth consulting on: subtle Go concurrency questions (lock ordering, goroutine lifecycle), tsnet / Tailscale internals not covered in their docs, cross-platform behavior that differs between darwin / linux / windows, framework upgrade gotchas. Pattern proven on the iOS side (1-bit) — see CLAUDE.md `## External consultation (Gemini)` entry there for examples that prevented multiple SwiftUI / iOS-internal regressions.

**Skip when:** the answer is verifiable by reading the code, running a test (the project's `make test` is fast), or checking the upstream library's source. Don't consult on things you could resolve in a minute by reading the source.

## Bot-review discipline (PR-time)

The same "verify before acting" rule the DeepSeek triage runs on applies to the PR bots — **their severity label is a claim, not a finding**. Recurring false-positive shapes, all observed on real PRs here:

- **`windows.Errno` vs `syscall.Errno`.** Gemini has flagged `errors.Is(err, windows.WSAEADDRINUSE)` as HIGH — "cross-package type mismatch, can never match" — and suggested `syscall.WSAEADDRINUSE`. Both halves are wrong: `x/sys/windows/aliases.go` declares `type Errno = syscall.Errno` (an **alias**, no distinct type) and `zerrors_windows.go` declares the constant AS `syscall.Errno`, so `errors.Is` compares the stdlib type with itself; and stdlib `syscall` on Windows has no `WSAEADDRINUSE`, so the suggested fix **would not compile**. The rationale now lives in `isAddrInUse`'s comment ([doctor_windows.go](internal/doctor/doctor_windows.go)) — read it before re-raising.
- **"URL parser differential" via a backslash in the authority.** Go's `net/url` REFUSES `\` in a host (`invalid character "\\" in host name`), so guards proposed "after `url.Parse`" against a backslash host are unreachable dead code. A regression TEST asserting refusal is still worth taking (it survives a future Go relaxing the parse); the guard is not.
- **A "compilation failure" claim that is really a test failure.** `os.Geteuid` IS defined on Windows (returns -1). Fixing the *stated* cause (a `//go:build !windows` tag) would have hidden a test from the platform where its other branch still works; the real fix was a runtime skip.

Take the accurate half of a wrong finding when there is one — the backslash and Windows-fixture cases both yielded a useful test even though the proposed code change was rejected. And **reply on the thread with the evidence when declining**, so the same claim doesn't cost a fresh investigation next quarter.

**Merging with review comments outstanding is a process failure, not a shortcut.** PRs #562 / #563 / #564 (2026-07-22) each merged with one commit and no fix round — #563 nine minutes after its review landed, #564 while a comment was still in flight. That deferred one Major (the FLAC preflight double-read, #568) and one High (unvalidated updater asset URLs, #569) into a separate remediation batch, and both were real. The documented loop — *"don't merge after round 1 — expect 2 rounds minimum"* — is what catches this; a same-day 9-PR batch is exactly when it gets skipped and exactly when it matters.

**And check the bot actually reviewed before counting the round.** A rate-limited CodeRabbit hides its notice in an HTML comment inside the walkthrough, so a PR with no new comments after a fix round looks identical to a clean pass — full rule, the grep that detects it, and the two commands that ask for the pass, under **### Build, CI, and test discipline**.

## LOUPE — the recent-work review cycle (user-invoked by name)

Full procedure: **[docs/LoupeReviewCycle.md](docs/LoupeReviewCycle.md)** — the
bridge-side twin of the iOS repo's `docs/LoupeReviewCycle.md`. When the user says
*"run LOUPE on last week"* / *"LOUPE the last N commits"* / *"LOUPE the enrich
package"* / *"LOUPE since v0.1.9"*, that is a request for the whole loop: scope
the window (or the package) → review it from two directions (PRISM batches +
targeted read-only agents) → triage every finding against the code and the
invariants in `## Things that have bitten before` → one written plan with its
rejections → plan review → ship as file-disjoint PRs, **one per script run**,
each negative-controlled red-first (`-count=1`, never a cached PASS) → bot sweep
across all FOUR bots (Gemini, CodeRabbit, SonarCloud, **CodeQL**) with
evidence-backed declines → merge → the `make fmt vet test build-all` gate → a
dated CLAUDE.md entry → a deploy-and-journal field loop, where
`journalctl -u 1-bit-bridge` outranks anything this file claims.

**It runs end to end without stopping for approval**, and a *question* is not a
stopping condition — measure it (module source under `$(go env GOMODCACHE)`, a
three-line probe, `-gcflags=-m`, `go test -race`, a fuzz target), then consult
Gemini over the API, then decide and record the decision. Escalate only for
product direction, a production deploy, a wire change that commits the iOS side
(the Mirror-PR contract above), or a refusal condition.

It is the third named procedure beside PRISM (bug review) and VISTA (design
briefs), and it uses PRISM as one phase. **The Gemini API key lives at
`~/dev/gemini.api`** (override `GEMINI_API_KEY_FILE`), is sent as an
`x-goog-api-key` header, and must never reach a URL, a log, or a commit; the
three harnesses (`consult.py`, `relay.py`, `prism.py`) live at
`~/dev/gemini-review/`, outside both repos, and work from here unchanged.

## External code review (DeepSeek sweep)

The repeatable external-LLM (DeepSeek v4-pro) review process spans BOTH repos — full procedure is documented on the iOS side at `1-bit/docs/DeepSeekReviewProcess.md`. Harness at `~/dev/deepseek-review/` (`plan.py` collects last-week's changed files incl. `**/*.go`; user runs `run_all.py`; agent triages `responses/*.md`). **Value is in the triage** — the 2026-06 baseline ran ~70% false-positive, so verify EVERY finding against the actual code + the invariants in `## Things that have bitten before` before acting (zero false fixes shipped that way). Go-specific FP classes are pre-encoded in `review.py` + `~/dev/deepseek-review/known_fp.md` (append confirmed FPs there so re-runs stay quiet): `&local` in an atomic ≠ use-after-free (escape analysis heap-promotes); defers run during panic unwind (no "permanent deadlock"); builder/`With*` setters are construction-time not racy; partial-file chunk views can't see paired Close/shutdown elsewhere; check multi-return signatures before "swallowed error". Real fixes ship via the Multi-PR batch workflow below (themed, parallel-off-main when disjoint, `make fmt vet test build-all` before push, fast-forward-never-force on bot-review commits). `run_all.py bridge` limits a run to this repo.

## Development workflow

Standard single-PR loop for any non-trivial change:

1. **Branch off main.** `git checkout -b feat/<topic>`. Never push code directly to main (CLAUDE.md-only docs changes are the sole exception).
2. **Pre-push gate.** `make fmt vet test build-all` — clean before pushing. Paste the output into the PR body.
3. **Open PR to main.** One PR per logical theme. Push the branch and open the PR; don't wait to batch unrelated changes into it.
4. **Wait ~6 min for bot reviews.** CodeRabbit, Gemini, and Qodo/Greptile each post within ~3 min; 6 min covers the slow tail. Don't poll.
5. **Address all comments in one fix commit per round.** Don't merge after round 1 — bots run a second pass on the fixes. Batch all round-N comments into a single commit.
6. **Reject suggestions that contradict deliberate rationale.** When a bot flags something that was a conscious design decision, cite the rationale in a reply and move on. Don't blindly apply suggestions that would undo a documented invariant (e.g. the `apiScan → spawnBackgroundScan` WG contract, the `pairing.Poll` read-many contract).
7. **Expect 2 rounds minimum.** One round of fixes, one confirmation pass. High-surface PRs (new endpoints, config changes, concurrency) routinely take 3–4 rounds.
8. **Merge once reviews are quiet** and `make fmt vet test build-all` is clean.
9. **Post-merge deploy.** Once main carries the fix, deploy it to the two reachable bridges so the change is actually live — see [Post-merge deployment](#post-merge-deployment) below. Skipping this step leaves the codebase ahead of every actual bridge a paired iOS client can reach.

For jobs spanning 3+ PRs, use the stacking pattern below instead.

## Post-merge deployment

**See `ops/deployment-runbook.md`** — 3-step flow (local `/tmp/bridge-live/` fixture → home-pc Windows → bridge.ars.md VPS). Read it before deploying.
## Multi-PR batch workflow

For any larger job spanning **3+ PRs**, use the **stack-and-batch** pattern instead of the default serial merge-after-each. Time-validated against the v1.2 improvements batch (PRs #76 / #81 / #82 / #83 / #84 / #85 — security + slog + CLI + fsnotify + Docker + post-merge follow-ups). The serial pattern would have spent ~30 min just on bot-review wait windows; the stacked pattern collapsed that to one ~6 min wait.

1. **Plan first.** Write the plan to the plan file before any code. Cross-PR invariants caught at plan time save hours of post-merge debugging — the dropped `db.SetMaxOpenConns(1)` from PR #76's first draft is the canonical example. The plan file is the highest-leverage artifact in the batch; every "Things that have bitten before" entry traces back to a deliberate plan-time decision.
2. **Stack PRs end-to-end.** Each branch bases off the prior one's tip (`git checkout -b feat/X feat/W`); each PR's `base` is the prior branch, NOT main. Open all PRs in one pass without waiting between them. Build PR-N+1 while bots review PR-N — overlap the work.
3. **One 6-minute wait** after the last PR opens. Bots (CodeRabbit / Gemini / Qodo) post within ~3 min; 6 min covers the slow-arrival tail. Don't poll; use ScheduleWakeup once.
4. **Address all comments in one combined pass per branch.** Don't merge anything yet. Bots see cross-PR context on a stack — their PR-N+1 comments may reference PR-N's invariants, and folding both into a single fix is cheaper than amending after merge. Reject bot suggestions that contradict deliberate in-code rationale (the PR-76 review's "cache transient MB errors" suggestion vs the PR #74 invariant is the canonical example — verify, don't blindly apply).
5. **Merge bottom-up in dependency order at the end.** GitHub auto-closes a stacked PR when its base branch is deleted, so as each ancestor merges, retarget the next via `git rebase --onto main <ancestor-tip>` and open a fresh PR (the previous one auto-closed). Plan ~2 min of rebase per child PR; `--reapply-cherry-picks` is unnecessary because already-applied commits are detected and skipped automatically.
6. **One combined follow-up PR** for any post-merge bot comments. Bots run a second review pass after the first round of fixes lands — that's the realistic floor (two rounds, not one). Batch the late-arriving items into a single follow-up PR rather than amending merged branches.
7. **CLAUDE.md updates direct to main.** Per the existing memory-entry convention — docs-only changes bypass the feature-branch path.
8. **End-of-session quality gate.** `make fmt vet test build-all` on bridge, `xcodebuild build` on iOS, resolve any warnings before reporting done. The stacked workflow's main risk is cross-PR drift; the build matrix catches it cheaply at the end.
9. **Post-merge deploy** after the whole stack lands — see [Post-merge deployment](#post-merge-deployment). For a stack, deploy ONCE at the end carrying every merged fix, not per-merge.

**Avoid small PRs.** 5 cohesive ~200-line PRs ship faster and review better than 15 micro-PRs — bots calibrate review priority to PR scope, and a coherent theme per PR makes the eventual squash-commit message useful as ship-history.

**When NOT to stack:** if PRs are genuinely independent (disjoint files, no semantic dependencies, no shared invariants), open them in parallel against main rather than stacking — review converges faster and there's no rebase cost. Stack only when PR-N+1 logically depends on PR-N's API surface or invariants.
