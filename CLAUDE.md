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
  deserves a human-reviewed PR. Locally, `make test` still runs seed corpora only. **44** targets across **ten** packages —
  `internal/{manifest,fs,dlna,dlna/discovery,upnp,enrich,dupes,lyrics,upload,atlasharvest}` —
  and `atlasharvest`'s `FuzzMatchRelease` lives in
  `lyrics_test.go` rather than a `fuzz_*_test.go` file, so a census that
  greps only the latter undercounts. (This entry said 37 across nine until
  2026-09-10: the sixth stale claim of the kind, and in the paragraph that
  warns about them; 38 until 2026-09-12, when `internal/lyrics` gained
  `FuzzTextCandidateClassification`; 39 until 2026-09-28, when
  `internal/manifest` gained `FuzzSACDExpandUnderAReadFault`; 40 until
  2026-09-29, when it gained `FuzzExtractOGG`, 41 until later that day,
  when it gained `FuzzID3v2WalkAgreesWithDhowden`, 42 until later still, when
  it gained `FuzzOggFLACReadsBackTheMetadataItCarries`, and 43 until backlog B117 added
  `FuzzDhowdenReadBufferReadsAsItsStreamDoes`.) They cover
  the five untrusted-input surfaces: the audio extractors (whole-file + the pure
  chunk-body parsers + the SACD ISO reader), the LAN-facing UNAUTHENTICATED parsers (SSDP /
  SOAP / DIDL / device description), `fs.Resolver`, the web-upload path validation
  (`internal/upload`, which this list omitted until 2026-09-09), and the Atlas
  release matcher (`internal/atlasharvest`).
  **Count them by file:name pair** to get 44 targets. A function-name-only
  census (`grep -h '^func Fuzz' | sort -u`) reports 43, because
  `FuzzNormalize` exists in both `internal/dupes` and `internal/lyrics`. Without `-fuzz` they run their seed
  corpora as ordinary tests, so the normal suite absorbs them for free. To actually fuzz:
  `go test ./internal/fs/ -run XXX -fuzz FuzzResolveContainment -fuzztime 60s -fuzzminimizetime 1s`
  (one target at a time — Go permits only one `-fuzz` per invocation).
  **`-fuzzminimizetime` is not optional on a target that keeps finding new coverage.** It
  defaults to 60s, minimization burns CPU *without incrementing the reported `execs`*, and the
  run still says `PASS` — so the failure mode is a target that looks like it ran and did not.
  Measured on `FuzzFoldForMatch`: `-fuzztime 60s` alone executes **19,003** inputs and then
  sits at 0/sec for 43 seconds; adding `-fuzzminimizetime 1s` executes **1,302,362** in half
  the wall clock. Twenty-five carry PROPERTY assertions worth keeping green rather than merely
  not-crashing: `FuzzResolveContainment` (a successful `Resolve` must land inside a root —
  asymmetric, so only a real escape fails it), `FuzzFoldForMatch` (the documented
  `foldNameNoArticle == stripLeadingArticle∘foldName` identity `pickBestArtist` depends on),
  `FuzzParseRetryAfter` (the `maxRetryAfter` cap), `FuzzSACDVirtualPathRoundTrip` (a
  rendered virtual path must parse back to the same index and container — the renderer and the
  parser disagreeing is a row-reaping bug, since the deletion pass keys on
  `IsSACDVirtualPath`), `FuzzSACDExpandUnderAReadFault` (an SACD image expanded through
  one failing read answers what it answers fault-free, or an error: never "not an SACD"
  where the fault-free read found an album, the answer that retires its rows; the rule
  is under **Scanner**), the four lyrics targets (`FuzzParseSYLTToLRC`, `FuzzNormalize`,
  `FuzzPickIsShuffleInvariant`, `FuzzTextCandidateClassification`; their properties are
  under **Lyrics**), `FuzzMatchRelease` (a claimed match is an entry the listing holds),
  `FuzzKeyFor` (the dupe key is deterministic and every field reaches `Key.ID`),
  `FuzzValidateRelPath` (an accepted upload path meets every invariant the commit relies
  on), `FuzzAcceptedExt` (an audio extension is always accepted) and
  `FuzzParseDeviceDescription` (every service URL the parser keeps, re-parsed, is http(s)
  with a host, stays on the description's host when discovered, names this machine or a
  link-local address only from a description URL that does too, and never names a cloud
  metadata address; it fuzzes the base URL as well as the XML), the eight whole-file
  extractor targets (`FuzzExtract{AIFF,WAV,DFF,DSF,FLAC,OGG,M4A,MP3}`, through
  `fuzzExtractOnce`: one extraction allocates no more than `extractionAllocLimit`, 64 MiB
  plus 64 bytes per input byte; the rule is under **Scanner**),
  `FuzzID3v2WalkAgreesWithDhowden` (wherever dhowden reads an ID3v2 tag, the ID3v2 guard's
  walk counts what dhowden stores, per frame id; the rule is under **Scanner** too),
  `FuzzOggFLACReadsBackTheMetadataItCarries` (a FLAC stream's metadata laid out as Ogg FLAC
  pages of any size reads back byte for byte as a .flac file's; also under **Scanner**) and
  `FuzzDhowdenReadBufferReadsAsItsStreamDoes` (the page buffer dhowden reads through holds
  the bytes its stream holds, where it holds them, over any tape of reads and seeks; under
  **Scanner**). This said "Four" until 2026-09-28, while eight more were added beside
  them, then "Twelve" and "Thirteen" that same day, as two more joined, "Fourteen" until
  2026-09-29, "Twenty-two" until later that day, "Twenty-three" until later still, and
  "Twenty-four" until backlog B117: **count them
  by the assertions in each `f.Fuzz` body**, not from this list. **A crash found by the extractor
  targets is a REAL defect, not a nicety** — `runScanWorker`'s per-iteration `recover()` means
  a panicking file is skipped, so it silently never reaches the manifest, and a throw (out of
  memory) is not recovered at all. Baseline at
  introduction: ~41M executions total, zero panics, zero escapes.
  **The nightly job runs every fuzz process under a 5 GiB address-space limit**
  (`prlimit --as` through `go test -exec`, so the compiler and linker are not held to it;
  2026-09-29, backlog B99). Without it an input that made a worker allocate gigabytes did
  not fail the run: the four workers re-zeroed the address space on every execution until
  GitHub shut the runner down ("The runner has received a shutdown signal", exit 143) before
  the upload step ran, so the leg read as an infrastructure failure and saved nothing.
  `FuzzExtractFLAC` died that way on 8 of the 28 nights from 2026-09-02, from 09-07 on, not
  (as the backlog entry first read) since #991. Under the limit the allocation fails, the
  runtime exits with status 2, and Go records the input: on dido (a 4.5 GiB limit), 23 s in,
  a 40-byte FLAC whose PICTURE block declared 4,281,344,048 bytes of data. **Not a cgroup cap**: the kernel kills a
  worker with SIGKILL, and Go records a worker a signal killed only when it dies MINIMIZING
  an input (`internal/fuzz/worker.go`); killed while fuzzing, the run fails with "terminated
  by unexpected signal; no crash will be recorded". **Size the limit from the measured
  reservation, not from RAM**: RLIMIT_AS counts reserved address space, and a
  `FuzzExtractFLAC` process reserves most of its size before it allocates (go1.26.6 on
  Linux, in decimal units: 2.2 to 2.6 GB for a worker, up to 4.3 GB for the coordinator,
  which maps each worker's shared memory), so 5 GiB (5.37 GB) leaves a worker at least
  2.8 GB of heap and the coordinator about 1.1 GB; a coordinator that reaches the limit fails its leg with its own out-of-memory,
  which is visible, and raising the limit gives each of the four workers that much more of
  the runner's 16 GB. The
  limit is the backstop; the allocation property is what names a bomb the limit would let
  through. **The extractor targets carry no per-execution time budget, on measurement**
  (2026-09-29, backlog B101): a tag at the ID3v2 guard's bounds runs about 0.25 s on a
  loaded dido (the slowest of 3.3 M FuzzExtractMP3 executions, 253 ms, none past 1 s), so a
  budget under a second would flake, and one above it catches only superlinear work that does
  not allocate, while work that allocates (B101's renaming) already fails the allocation
  property. Go's engine bounds no execution itself; whether a budget belongs to B103's answer
  is B103's question.
- `make fmt vet test build-all` is the pre-push gate, now mirrored by CI (`.github/workflows/gofmt.yml` = the fmt check, `gate.yml` = vet + test + build-all). Run `make check` (fmt + vet + race test, skips build-all) in the inner loop; `make build-all` once before pushing. On a RAM-constrained box the `-race` + 6-target cross-compile peak can OOM — the Makefile caps Go's `-p` parallelism via `P` (default 4; `make test P=2` to go lower, `P=$(sysctl -n hw.ncpu)` for a roomy box). See `CONTRIBUTING.md`.
- **On a host whose Go is newer than `go.mod`'s, `make fmt` rewrites files CI calls clean.** CI's gofmt check runs `go.mod`'s toolchain (`go-version-file: go.mod`, 1.26.6 as of 2026-09-25), and gofmt 1.27 indents a composite literal in a multi-value `return` differently: on a 1.27.1 host `make fmt` re-indents `internal/manifest/favorites_test.go` and `internal/atlasharvest/lyrics_test.go`, and 1.26.6's `gofmt -l` reports both rewrites. Never commit such a rewrite. Restore those files, and check your own with the pinned gofmt: `$(GOTOOLCHAIN=go1.26.6 go env GOROOT)/bin/gofmt -l <files>`.
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
- **TLS fingerprint is captured once.** The iOS pin is set during pairing via first-contact; rotating the server cert requires re-pairing. Don't mint a new cert on every `serve` run — `LoadOrGenerate` is sticky by design. Nor on a `bridge init` rewrite: it keeps the pair the config names (`tlsCertPath`, or the data dir's) and the data dir, which `--force` dropped until 2026-09-27 (the `cmd/bridge` bullet on what a rewrite keeps).
- **`enriched_at` monotonicity.** Upsert resets to 0 on track change so the enricher re-runs; the enricher marks it to `time.Now().UnixNano()` on completion (success or skipped). The other sanctioned writers are a CLOSED SET of four — `ResetEnrichedMisses`, `ResetEnrichedByArtistMBIDs`, `ResetEnrichedMissesUnderPrefix` and `ResetEnrichedByPaths` (the first two behind POST /api/enrichment/retry since PR #495, scoped to enriched-but-incomplete rows so a full MB/CAA re-crawl is never triggered; the last is the fingerprint sweeper's explicit-path form). All four are live callers — this bullet listed only two until 2026-09-06, so an audit against it would have flagged two sanctioned writers as violations. Never touch it anywhere else — the query `WHERE enriched_at = 0` drives the worker.
- **Admin console is loopback-only, no auth — IN LOOPBACK MODE.** `config.validateLoopbackAddress` + `admin.loopbackOnly` middleware both enforce this, and `admin.loopbackHostOnly` holds a request's Host to loopback as well (421 otherwise, backlog B170: the source alone admits a browser a page has rebound to 127.0.0.1). **Public mode is the separate, credentialed posture** (`internal/admin/middleware_auth.go`: session auth, persisted since PR #800), which is what the public demo and the hosted tenants run, and what `bridge.ars.md` ran as the operator bridge until it moved to a home NUC on 2026-09-22; this bullet omitted public mode until 2026-09-06. Don't add an auth layer that bypasses the loopback constraint; don't expose admin behind Tailscale / reverse-proxy. Anyone on the host already owns the token store and the SQLite DB — auth on top would be theatre. For remote admin, SSH-tunnel the port.
- **Graceful shutdown triggers full cleanup.** The `POST /api/restart` admin handler MUST NOT call `os.Exit(0)` directly. It must invoke the same cancellation closure that handles `SIGINT/SIGTERM`. This ensures the `bgScans` WaitGroup is honored (preventing SQLite corruption), in-flight transcode jobs are cleaned up, and the `auth.Store` flushes its last-used-at debounce buffer. Wired in `cmd/bridge/main.go` via `admin.Deps.Restart`, as `restart.request`, which also makes serve exit with `supervision.RestartExitCode` (75) where a stop exits 0: launchd and the Windows SCM relaunch only the former (B201, under **Config, settings and process lifecycle**).
- **Dual-stack HTTP/2 and HTTP/3 API.** The bridge serves the v1 API over both TCP (HTTP/2) and UDP (HTTP/3). QUIC is enabled by default but can be disabled via `disableHttp3: true` or `BRIDGE_DISABLE_HTTP3=true`. LAN HTTP/3 uses on-disk certs with forced "h3" ALPN; Tailscale HTTP/3 uses `tsnet.LocalClient` to fetch Let's Encrypt certs dynamically. Graceful shutdown uses `.Shutdown(ctx)` with ONE 5s window, which the LAN and tailnet servers drain under together, to protect active media streams, and never waits on a handler past it: an HTTP/3 drain gets the window plus a 1 s allowance for quic-go's force-close, and a handler still running then costs a line (the serve-wiring section's HTTP/3 drain bullets).
- **A recorded sidecar path is a claim, never proof the file is gone.** `sidecar_path` / `waveform_path` are absolute; after a host move every row reads ENOENT while the files sit at their canonical places. The three reapers ask `integrity.LocateSidecar` and ADOPT a relocated row; the forward sweeps' known sets carry the canonical spelling; a mass deletion while the tree still holds sidecars is refused. Full rule under **Job pools** below (2026-09-20).
- **Single ↔ multi-root storage form flips.** When the admin adds a second root or removes back down to one, track paths change from `Artist/Album/…` to `<basename>/Artist/Album/…`. The admin handler calls **`store.WipeFilesystemTracks()`** before the new scan so no stale rows survive — **never `WipeAllTracks`**, which CASCADE-deletes `upnp_track_routing` and destroys an entire upstream library on a mere root-count toggle. (This bullet said `WipeAllTracks` until 2026-09-06, contradicting the rule under **Scanner** below; no production path has ever called it.) Don't try to migrate in place — the rescan is cheap, enrichment is cached by MBID.

**Working the bridge**: `feat/<topic>` branches, PR to `main`, pre-push `make fmt vet test build-all`. **Working the iOS side**: same convention at `~/dev/com.acoseac.dsdplayer/`. Never push direct to `main` on either repo; the one exception, on this repo only, is a change to `CLAUDE.md` alone (**Development workflow**, step 1), and a rule that describes code lands in the PR that changes the code.

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

**`enriched_at = 0` is NOT a tag-re-extraction reset.** It only triggers the MusicBrainz / CoverArt / Deezer enricher (the `WHERE enriched_at = 0` worker query at `internal/manifest/store.go:477`). It does NOT cause the scanner to re-read file tags: the scanner's skip gate (`runScanWorker`) compares the walk's size and mtime with the row's `size` and `mtime_ns` COLUMNS, read through `GetTrackStat`. **This paragraph said until 2026-09-28 that the gate compared the mtime inside `tags_json` (through `GetTrack`), so that `UPDATE tracks SET mtime_ns = 0` does not work. It does**: the gate has read the columns since it moved to `GetTrackStat` (#574), and measured through the Go store, a row whose `mtime_ns` was zeroed is re-extracted on the next scan, on the full upsert leg (its `enriched_at` resets, as for a changed file). So to force tag re-extraction after an `internal/manifest/extractors.go` change (e.g. the PR #208 multi-value Vorbis fix) without touching real file mtimes, zero `mtime_ns` on the affected rows or wipe them, either way through the Go helper below. In code, a change to what extraction produces bumps `ExtractorVersion` instead, whose diff-guard bounds the delta to rows that changed.

**The old `sqlite3 … "DELETE FROM tracks;"` form here is BROKEN since migration v4** (corrected 2026-06-23). v4 added the expression index `tracks(unicode_lower(path))` (+ a `track_variants` twin), and `unicode_lower` is a Go-registered scalar (`internal/manifest/sqlfunc.go` `init()`), so an external `sqlite3` CLI `DELETE`/`UPDATE`/`INSERT` on those tables fails at prepare with `unknown function: unicode_lower()`. Dropping the index doesn't help — it's created only by the version-gated migration, so a restart won't recreate it. Two working paths:

- **Disposable local fixture** — simplest is to delete the DB file and let the startup scan rebuild (loses pairings/tokens; re-pair after):
  ```sh
  kill -TERM $(pgrep -f "bin/bridge serve --config /tmp/bridge-live")   # graceful
  rm -f /tmp/bridge-live/data/bridge.db*
  ./bin/bridge serve --config /tmp/bridge-live/bridge.yaml &            # RunPeriodic's startup scan re-extracts every file
  ```
- **Selective / pairing-preserving / production** — use a throwaway Go helper that blank-imports `internal/manifest` (its `init` registers `unicode_lower`) + `modernc.org/sqlite`, opens the DB with `file:<path>?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)`, and runs `DELETE FROM tracks WHERE <predicate>` (e.g. `json_extract(tags_json,'$.sampleRate') IS NULL` — only the rows a new extractor now fills, so only those re-enrich; a full `DELETE FROM tracks` resets `enriched_at` on every row → the MB/CAA/Deezer treadmill). Put it in a `_`-prefixed dir (ignored by `go ./...`/`build-all`, and by the tests that sweep the tree from its root), run with the bridge stopped, then restart to trigger the scan. On a loopback bridge `curl -s -X POST http://127.0.0.1:7789/api/scan` also triggers a scan; public bridges (admin auth-walled) need a restart.

**WAL read trap**: an external `sqlite3` `SELECT COUNT(*)` against a LIVE bridge DB can read stale main-DB state for minutes — it won't see the bridge's un-checkpointed WAL writes. Verify a backfill landed AFTER a restart/checkpoint (or via the bridge), not by polling the CLI mid-scan. Full procedure + verified helper shape in the `bridge-track-reextract-gotchas` memory. The wipe still survives the TLS fingerprint + tokens (different tables); iOS pairing stays valid.

## Production deployments

**See `ops/deployment-runbook.md`** — full runbooks for the live bridges: **the operator bridge**, which since 2026-09-22 runs on a home NUC reached over Tailscale as `<OPERATOR-SSH>` (a self-signed cert, so every health probe there needs `curl -k`, and `deploy/linux/deploy-bridge-vps.sh` cannot verify that host: drive the runbook's manual form), **home-pc** (Windows, SSH `<HOMEPC-SSH>`), and the **public demo** `bridge.1-bit.app`, a unit of its own (`1-bit-bridge-demo`, user `onebit-demo`, binary `/usr/local/bin/bridge-demo`) on the Azure VPS **bridge.ars.md**. That VPS ran the operator bridge, in public mode, until 2026-09-22 and hosts only the demo now, so `https://bridge.ars.md` no longer answers as a bridge. A demo deploy MUST still carry `SVC`/`REMOTE_BIN`: the script's defaults name the operator bridge's old binary and unit on that host. **One SSH ControlMaster per host, never a burst**: the flood protection is on the operator's own path, so a burst produces a multi-minute timeout that reads as an outage (see the runbook's SSH note). Atlas, which the bridges enrich against, is on its own VM (`atlas.ars.md`). **Every `<PLACEHOLDER>` here and in the runbook resolves in `ops/coordinates.local.md`, which is gitignored** — this file is tracked in a PUBLIC repo, so a real SSH target or IP must not be written into it (`.gitignore`: "never commit real WAN/LAN IPs, SSH targets, or key paths to this public repo"); canonical fleet coordinates live in the private conductor repo's `ops/hosts.md`. **Output pasted from a live host carries its coordinates too**: a `bridge doctor` report, a health body or a journal line names the host's tailnet and addresses, so a copy in README, a test fixture or `ops/engineering-log.md` takes examples (`nuc.tailnet.ts.net`, `100.64.0.24`, `192.168.50.24`) or a redaction token (`<NUC-LAN-IP>`). The operator host's were in all three, and the demo section's SSH target in the runbook itself, until backlog B110 (2026-09-29). Read the runbook before any deploy; covers scheduled-task names, update procedure, TLS/Tailscale gotchas, and the macOS sandboxed-CLI cert gotcha.

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
| A follow-up found and not built (a defect beside this change, a finding agreed to be out of scope, a question to measure later) | **`~/Desktop/to-do/bridge-backlog.md`**, never a task chip: see `## Follow-ups go in the backlog, not in task chips` below |

**`ops/`, never `docs/`, for anything operational** — see the warning in `## Production deployments`: `docs/` is served as a public website from a public repo, so live hosts, key paths and unfixed-weakness enumerations must not go there. This makes the repo-vs-private line *three*-way here: public `docs/` → internal `ops/` → private memory.

**Stays in private memory — and only this:** secrets-adjacent coordinates (SSH key paths, private hosts — these can't go in `ops/` either if the repo is public); the user's personal working preferences; and perishable state ("deployed vX on `<date>`, rollback at …") that would rot in git — anything *actionable* belongs in `~/Desktop/to-do/` instead.

**Three rules that make this hold:**
1. **Don't commit the memory *files*** into a `.claude/memory/` folder — the harness doesn't auto-load them from there, so they'd be inert paper. Fold the **content** into files that do auto-load.
2. **Never cite a private memory from a repo file** — it's a dangling reference for every other account. If a repo file needs the fact, put the **fact** in the repo.
3. **Repo first, then the pointer.** When something is both shareable and worth a personal reminder, write the repo entry first and leave the private memory as a one-line pointer.
4. **Only `CLAUDE.md` is auto-loaded.** `ops/engineering-log.md` exists so a rule can stay short without losing its evidence — but a finding written *only* there is the same inert paper as rule 1. Every batch writes the **rule** here and the **record** there.

## Follow-ups go in the backlog, not in task chips

**Don't call `spawn_task`** (the desktop app's background-task chip) in this repo;
the user asked for this on 2026-09-27. A session that finds something worth doing
outside its own change adds an entry to **`~/Desktop/to-do/bridge-backlog.md`**
instead, and names the entry in its final report. Chips piled up on finished
sessions, each visible only from the session that raised it, with nothing to collect
them before a release; the ones still showing on four finished sessions that day
were moved into the backlog and withdrawn.

- **Add** under `## Open` with the next free id, in the shape the file's header
  gives: Status, Priority, Source (session, PR, finding), What, Why it matters,
  Evidence (how it was measured and where the record is), Constraints, and a
  **Prompt** a fresh session can run as it stands, the text a chip would have
  carried.
- **Take** an entry by putting the branch and PR on its Status line. **Ship** it by
  moving it under `## Done` with the PR number and merge sha.
- **Before a release**, read `## Open` and decide what goes in.

It lives outside the repo on purpose. The repo is public, and entries describe
weaknesses that are not fixed yet and name private hosts, which is the content
`ops/README.md` keeps untracked. `~/Desktop` is iCloud-synced, so the file reaches
every Mac, and every account on a Mac reads the same file. A backlog entry holds
WORK, not rules: a rule a follow-up teaches still goes in this file, and its record
in `ops/engineering-log.md`.

**When the follow-up is an unfixed weakness, the public record names the entry
and nothing more**: not this file, the log, a PR body, a PR comment or a commit
message describes it or how to reproduce it before its fix ships (SECURITY.md).
The B97 session wrote one up in this file, the log and its PR body, and a review
caught it (#1110's log entry).

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

**Fifteen claims in this list have been wrong and been corrected** — the
WAV/AIFF extractor gap, the `deletedIds` field name, "the bridge has no DLNA
Search", `manualDescriptionURL` being unimplemented, (2026-09-22)
"`waveform_path` has the same shape and NO adoption yet", which #954 had
falsified two days earlier by wiring `integrity.LocateWaveform` into
`analysisStoreAdapter`, (2026-09-25) "a broken existing config cannot block
the re-init that replaces it", which held for config-file and not for the port
checks, (2026-09-26) "`os.ReadDir("")` reads the process working
directory", (2026-09-27) "init never prompts for `customEndpoints`, so
the old value survives the rewrite", (2026-09-27) "`Load` serves a config
giving a blank name `DefaultLibraryName`", (2026-09-28) "`analyze --gc`'s
`Consider` requires `.1bwf`", (2026-09-28) "`mtime_ns = 0` does not
force a re-extraction", (2026-09-29) "the app's SSDP path has no
LOCATION-versus-source check", (2026-09-29) "Go binds a multicast
listener to the group address", (2026-09-29) "`isLossyCodec` gates every
`BitsPerSample` write site", and (2026-09-29) "reconciliation never
crosses directories". The first five cost a later session real
time; the fourth was written **after** the PR that falsified it, by a session
that had this very warning in front of it, and the fifth sent `bridge doctor`
on telling operators to run `bridge analyze --force` — hours of decoding to
recover curves the next request rebinds for free. The sixth was found by measuring
the re-init rather than reading the bullet. The seventh was never true: a
bot's finding on #531, accepted without a probe, it spread to six code
comments and three test docblocks, two of whose tests passed with the
refusal deleted, and a bot on #1030 quoted it back as a rule. The eighth
was the premise a preflight judgement rested on, and no rewrite had ever
kept the value: measured, the same run also minted a new TLS pair over an
install that named its own. The ninth sat in a test docblock whose own table
asserted the opposite (`served: "  "`, as written), and `Load` gave the
default only to an exactly-empty name until #1042. The tenth named an
extension the bridge has never written (waveforms have been `.waveform.bin`
since #395), and survived because its conclusion, that a directory symlink
is no candidate there, holds for either spelling. The eleventh outlived the
code it described: #574 moved the skip gate to `GetTrackStat`, which reads
the `mtime_ns` column, and the bullet went on describing `GetTrack`, which
reads the mtime inside `tags_json`. The twelfth was about the OTHER repo, and
true for a day: the iOS app merged the check (acoseac/1-bit#1998), which
nothing in this repo sees, so it was corrected only because that session filed
the bridge's doc change as a backlog entry (B78). The thirteenth was the
premise of a backlog entry (B71) and, in another form, of a code comment
that chose a socket by it (the renderer discovery client's); measuring the
consequence the entry predicted is what found the premise false, from the
toolchain's own source. The fourteenth named a function #225's review had
already replaced (a denylist that failed open on an empty codec, for the
allowlist `canSetBitsPerSample`), and the comment beside the wire field said
the same; both were found while naming the compressed AIFF-C and WAV encodings
(B124).
The fifteenth sat in the Scanner bullet on the reconciliation passes, true of
four passes and not of the fifth, the year fill by release id, which crosses
folders by design; it was found while replaying the passes in memory (B188).
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
- **…and a read that did not complete is the same case: only a COMPLETED read
  may answer "not an SACD"** (2026-09-28). `processSACDISO` retires every
  virtual row under an `.iso` container at threshold 1, journaled (a tombstone
  to every paired device), whenever the expansion answers `(nil, nil)`, and
  three reads in `sacd.go` answered exactly that when they FAILED: the
  master-signature probe and the DST probe dropped their error (`n, _ :=`),
  and `parseSACDArea` folded a failed area-TOC read into `ok=false`. An EIO,
  ETIMEDOUT or ESTALE from a NAS deleted the album while the file sat on
  disk, on any scan that missed the skip gate (a size or mtime change, every
  `ExtractorVersion` bump). `sacdReadOutcome` is the rule: a read is full when
  it returned the bytes the parse needs, judged BEFORE its error
  (`io.ReaderAt` permits `(len(p), io.EOF)`); a short read ending in `io.EOF`
  or `io.ErrUnexpectedEOF` is the end of the file, structural, so a truncated
  image still answers `(nil, nil)`; any other short read, a nil error
  included, is an error, and an error retires nothing. Each phase keeps its
  FIRST failure and returns it only if it ends with nothing found, so one bad
  copy still expands from the next (the doubled-TOC design); `parseSACDArea`
  returns `(area, ok, err)`, err only for a read. The iOS reader makes the
  same split (transport errors throw, past-EOF reads come back short). **A
  container written in place reads as one that ends early, which IS a
  completed read**, so the scanner also skips, retiring and writing nothing,
  a container that changed during the scan (`expandSACDContainer`): the
  handle's stat before the first read against a stat of the path after it
  (`os.SameFile`, size, mtime), and the walk's stat against that same STAT
  (size, mtime), since the walk's stat of a linked container is its TARGET's
  (the next bullet). **Never `os.SameFile` against the walk's stat**: on
  Windows a directory entry carries no file index on FAT or exFAT, so every
  container there would skip, forever. **Never compare the walk's stat with an
  LSTAT**: for a link the walk's stat is the target's, so every symlinked
  container would read as moved (`TestScanner_SACDSymlinkedContainer_Expands`).
  This bullet said the opposite, lstat, until 2026-09-28, when the walk's stat
  of a link was the link's own; that pairing could not see a write to a
  symlinked container's target before the open, and the album was retired
  (`TestScanner_ALinkedSACDContainerWrittenAfterTheWalkKeepsItsRows`).
  Residual: an in-place overwrite that keeps size and inode inside one coarse
  mtime tick (FAT's 2 s), and a link repointed after the walk at a file with
  the first one's size and mtime. **No `ExtractorVersion` bump for this**:
  readable files expand
  byte-identically, a wrongly retired container has no representative row so
  the gate re-expands it anyway, and a bump re-upserts every virtual row (that
  leg has no diff-guard). `TestScanner_SACDReadFailure_RetiresNothing` (a
  case per site) and `TestScanner_SACDContainerChangingDuringTheScan_KeepsItsRows`
  (a case per arm) drive it through a per-scanner opener seam
  (`Scanner.openSACD`), and
  `TestScanner_SACDReadWholeAsJunk_StillRetiresWithTombstones` is the positive
  control: a container read whole as junk still retires, with tombstones.
  **`FuzzSACDExpandUnderAReadFault` puts the rule under the fuzzer** (the other
  SACD targets' reader never fails): an image expanded through one failing
  read must answer what it answers fault-free, or an error. It holds only for
  an image carrying ONE answer, so the harness builds it with identical TOC
  copies, one stereo area and one geometry, and truncates it only BETWEEN
  structures (`sacdFaultCuts`): an image whose copies differ can legitimately
  expand from the second, and a later copy cut short is one that differs
  (measured: cut inside the second area copy's TTxt sector, it expands to
  "Track 1" where the first says "T1"). A reader that reports the end of the
  file early is a truncation nobody can tell apart, so only failures that say
  so are injected; `TestSACDFaultPropertySeesWhatTheHarnessKeepsOut` shows
  the property reporting both. Its seeds fault each of the three reads above,
  and they are what gives the fuzzer its reach: Go's mutator walks an integer
  by at most 100, one argument per step, so with the DST probe's failure
  dropped again it found the violation within half a second from the other
  seeds (on the first harness and on the final one), and not in 90 s
  (468,005 inputs) from one seed whose fault touched no read.
- **A file the walk reaches through a link is indexed under its TARGET's
  stat** (2026-09-28). `filepath.WalkDir` hands an entry its lstat, so a
  symlinked audio file was indexed under the LINK's size (the length of the
  path it stores: in one test run 123 bytes against a 116-byte FLAC, and 134
  against a 1,228,800-byte `.iso`) and the link's mtime, while its tags came
  from the target and every endpoint serves the target's bytes. The skip
  gate compared the same
  link stat, so a retagged target was never re-extracted (and a re-authored
  linked `.iso` never re-expanded); `/v1/lyrics` answered 410 `lyrics_stale`
  forever for its embedded lyrics (the row's stat against the resolver's
  `os.Stat`); and the phone, which stores the size as `Track.fileSize`, fails
  every offline download of the file (`validateDownloadedSize` wants an exact
  match), re-downloads it forever through the auto-cache, and cannot read a
  linked SACD container at all (its reads clamp to the container size).
  `walkedFileInfo` is the one decision, in `walkRoot` and `ScanSubtree` alike:
  a REGULAR file keeps its own stat and pays no second syscall; anything else
  is stat'ed through (the listing's "not a regular file" test,
  `resolveEntryInfo`, because a Windows junction is `ModeIrregular` with no
  `ModeDir`; a cloud placeholder stats to itself). **A link whose target
  cannot be stat'ed spares its own row, keyed on the entry itself, and mints
  none**: "could not see" dominates, so a mount that went away keeps its rows
  as they were, and the old walk's row minted from the path alone (which the
  skip gate then kept once the target came back) is gone. Keyed on the entry,
  not its directory, so a permanently dangling link spares nothing beside it.
  One Warn per scan names them (`links whose target could not be read`),
  never one per link: a mount takes every link into it at once. **A link to a
  DIRECTORY is not a track**, whatever its name, and the walk still follows no
  directory link (loops; what it says about one is the B74 bullet below).
  **Nor is a named pipe, a socket or a device, or a
  link to one** (`fsutil.NotAFile`, the list every byte route refuses by too:
  the next bullet): whatever stat the row would carry must describe
  something that opens as a file, and a regular file whose own stat says
  otherwise is judged through. A worker opens what the walk hands it, and
  opening a FIFO waits for a writer with nothing to cancel the wait, so a FIFO
  named `01.flac` held a worker, and the scan with it, forever (measured still
  running at 10 s, and at 15 s with its context expired at 5 s; an `.iso`
  FIFO blocked the next scan too, since no row ever let the skip gate pass
  it). By reading: `Scan` holds the scanner's mutex for its whole run, so
  every later scan waits on it, and the stuck `IsScanning` stands down the
  Atlas lyrics sweep, the booklet GC and the duplicate restamp and makes a
  compaction answer 409. When a writer did come, the row was replaced by one
  minted from the path (title "01", 0 bytes), and a link to a device or a
  socket was indexed that way outright. **The refusal is a list of kinds,
  never "is a regular file"**: a Windows cloud placeholder is
  `ModeIrregular` after `os.Stat` and opens, and hydrates, as a file, so a
  OneDrive library with files on demand would vanish. **A row at such a path
  is reaped like a deleted file's**, after the usual missing-count grace, a
  directory link's included: the walk stat'ed the entry and knows what is
  there. That is the dangling link's answer reversed on purpose: a stat that
  FAILED is "could not see", a stat that answered is a fact. One Warn per
  scan names them with the example's kind (`audio-named entries that are not
  files`), and a row the old walk minted for one is reaped the same way.
  **No `ExtractorVersion` bump**: the stored stat of
  each linked row no longer matches, so the first scan after the change
  re-extracts exactly those rows, once, on the full upsert leg (one delta row
  to every paired device, and a re-enrichment), and nothing else moves
  (`TestScanner_TheFirstScanAfterTheFixRewritesOnlyTheLinkedRows`). PROTOCOL.md
  needed no change: it already said a listing row describes the target, and
  a virtual row carries the container's size. A library ROOT that is itself
  a link is not this rule's shape: that is the `fsutil.WalkableRoot` bullet
  below (this one said "still open" until the same day).
- **…and a route that serves a file's bytes opens it with
  `fsutil.OpenAsFile`, never `os.Open`** (2026-09-28, B46). Keeping such
  entries out of the manifest stopped nothing a client names:
  `/v1/download`, `/v1/read`, the player's audio and download routes and the
  DLNA file route opened the path with `os.Open`, and opening a named pipe
  waits for a writer with nothing that can cancel the wait (a blocked
  open(2) cannot be interrupted from Go). Measured over the real
  `api.Server`: the client gave up at 2 s, and both handlers, and the two
  updater sessions they had begun (a pinned one keeps auto-install
  deferring on every poll), stayed until a writer came 7 s in; `/v1/stat`
  and `/v1/list` answered at once. A link to `/dev/null` was served as an
  empty 200, a socket as a 500. The DLNA route serves the MANIFEST's path,
  and a row outlives its file until the scan that reaps it. **One list**:
  `fsutil.NotAFile` is #1070's, moved out of the scanner, so the manifest and
  the routes cannot disagree about what a track can be; never a second copy.
  `OpenAsFile` opens `O_NONBLOCK|O_NOCTTY` on unix, refuses by the OPENED
  file's own stat (so a path replaced after a caller's stat is judged as
  what it is now), and clears `O_NONBLOCK` again for a file, because a FUSE
  daemon is handed the flags with every read. **Its EWOULDBLOCK fallback to
  a plain open is load-bearing**: a nonblocking open of a file under another
  process's write lease (Samba's kernel oplocks, an NFS delegation) fails
  where a plain open waits for the break (up to lease-break-time, 45 s),
  and a FIFO's nonblocking open never answers EWOULDBLOCK. The
  resolver-backed routes ALSO refuse on the resolver's stat before any open:
  that is what refuses a socket (the kernel refuses its open itself,
  EOPNOTSUPP on macOS, ENXIO on Linux, so `OpenAsFile` cannot name it) and
  keeps a device from being opened at all. The answers reuse existing codes,
  so there is no wire change and no Mirror-PR: 400 `bad_request` "path is a
  <kind>, not a file" (what a directory already got), 400 `bad_path` on the
  player, 404 on DLNA, 410 `variant_missing_on_disk` for a rendition whose
  sidecar is not a file. **`TestEveryServedFileIsOpenedAsAFile` fails on any
  production declaration that passes `http.ServeContent` a file it opened
  with `os.Open`/`os.OpenFile`**, so the cache routes (artwork, booklets,
  playlist covers, waveforms) open through it too and the rule has no
  exceptions; it reads one declaration at a time, so an open in one function
  served from another goes unseen. The scanner's reads and the jobs' open
  the same way since 2026-09-29 (the next bullet); this said they were
  still `os.Open` until then.
- **…and so does everything else that reads a library file in this
  process, and the listing opens its directory with `fsutil.OpenDir`**
  (2026-09-29, backlog B62). The walk judges what it hands the workers, and
  a worker opens the path later (on a large library, minutes later), so a
  file renamed over by a named pipe in between reached the extractors'
  `os.Open` and held the worker, the scan and the scanner's mutex until
  something wrote to the pipe. Measured on main: every extension the
  scanner reads (14) waited, as did an `.iso`, and a worker handed a path
  swapped for a pipe waited and then wrote a row minted from the path
  (title "01", the walk's size of a file that was gone); one swapped for a
  socket or a directory wrote that row at once. **The folder-art lookup
  needed no swap**: it read `cover.jpg` with `os.ReadFile` after a stat
  that judged only the size, and the walk judges only audio-named entries,
  so a named pipe called `cover.jpg` held every scan that extracted a track
  of its album without embedded art, and a link to `/dev/zero` called
  `cover.jpg` (0 bytes by its stat) grew the heap past 3 GiB in 0.85 s and
  was still reading, toward the out-of-memory throw no `recover` catches
  (B99's bullet). The lyrics sidecar read,
  the analysis job's STREAMINFO read and the fingerprint prefix read had the
  swap window too. Each now opens through `fsutil.OpenAsFile`, or
  `fsutil.ReadAsFile` for a whole read (`os.ReadFile`, opened as a file),
  and the folder art refuses on its stat first, so a device is never opened
  and a socket is named. **A worker whose path is no longer a file writes
  no row** (`notAFileNow`: the refusal's kind, or for the socket no open
  reaches, a stat after the failed open) and logs one Warn naming it
  library-relative; the next walk sees what is there and the row goes as a
  deleted file's, after the grace. **`/v1/list` opens with `OpenDir`**
  (`O_DIRECTORY` on unix, which the kernel checks before it opens anything;
  on Windows, whose pipe opens do not wait, `os.Open` and an `IsDir`
  check): a directory replaced by a named pipe between the resolver's stat
  and the open held the request, and now answers 500 "couldn't open this
  directory" at once. `os.ReadDir` and `filepath.WalkDir` open with
  `O_DIRECTORY` already (go1.26.6's `os.openDir`, measured), so only a
  hand-rolled directory open needs it. **The listing still lists a named
  pipe, socket or device, as an entry that is not a directory, on
  purpose**: the app's bridge share syncs from the manifest, which holds
  none (#1070), and reads the listing only to browse, where following such
  an entry gets #1082's 400 naming the kind; leaving it out would change
  what the app shows, and PROTOCOL.md's rule for what a listing holds (a
  Mirror-PR), for a row that harms nothing
  (`TestListingListsWhatIsNotAFileAsAnEntry`). A job that hands the path
  to a CHILD process (sox, ffmpeg, fpcalc's `Compute`) needs nothing: SIGKILL
  ends a child waiting to open a pipe, so the job's own timeout bounds it
  (measured on macOS and Linux). **`TestEveryLibraryReadOpensAsAFile` fails
  on a read-open (`os.Open`, `os.ReadFile`, a read-only `os.OpenFile`) in a
  package that reads library files** (`libraryReaders`: api, dlna,
  manifest, analyze, acoustid; a new such package joins the list). The
  cost is 1.5 µs an open (13.1 against 11.6 µs, APFS), and nothing dhowden,
  B117's buffer or the B99 and B101 guards see changes: they read the same
  `*os.File`, blocking again, from offset 0 (through `faultNotingSource`
  since B134, the next bullet, which forwards every call). No
  `ExtractorVersion` bump: what extraction produces for a file is unchanged.
- **…and a worker writes a file's row only from a read that COMPLETED: a
  file it could not read whole keeps the row it had, and a new one gets
  none** (2026-09-29, backlog B134). The full path wrote the track whatever
  `ExtractWithContext` returned, over `fillFromPath`'s guess (the file name
  as the title, the folders as the album and the artist), under the walk's
  stat, so a changed file whose open failed (EIO or ESTALE from a NAS,
  EACCES, a file gone since the walk) was written as that guess, a new one
  got a row by its name, and the skip gate, finding the stored size and
  mtime equal to the file's, kept the guess on every later scan until the
  file changed again (measured with chmod 0: titled "01" before and after
  the file was readable again). **A READ that failed did the same**, through
  the parsers that drop their errors (dhowden answers any failed read as a
  tag it cannot parse; the MP4 walks and the FLAC format read log theirs
  and read on), and on the version-stale leg that partial extract reached
  the diff as a changed row, replaced the stored one and was STAMPED
  CURRENT: an `ExtractorVersion` bump over a flaky mount made the guesses
  permanent. **Every extractor opens its audio file through
  `openAudioFile`**, which under `ExtractWithContext` hands back a
  `faultNotingSource` that notes the first open, read, seek or stat of the
  file that did not complete, and `ExtractWithContext` then answers
  `readIncompleteError` (`readFault`) whatever the parser made of it.
  `TestEveryAudioFileReadGoesThroughOpenAudioFile` fails on a function of
  the package that opens or reads a file through fsutil and is not one of
  the readers of OTHER files it names (the `.iso` containers, a folder's
  cover, a lyrics sidecar, the artwork cache): a new extractor opens
  through `openAudioFile`. **Two error answers complete a read**,
  being about what the file holds: the end of the file (a truncated file)
  and an offset the OS refuses before reading (`seekOffsetRefused`: EINVAL,
  on Windows ERROR_NEGATIVE_SEEK; a ReadAt at a negative offset), which a
  malformed file's own bytes ask for (dhowden's ID3v1 look 128 bytes back
  from the end of a shorter file, a DSF metadata pointer with its top bit
  set). **Don't count those as failed reads**: such files would never be
  indexed (`TestScanner_AFileReadWholeIsWrittenAsItAlwaysWas`, which also
  keeps a file a walk refuses as not its format written by name, as it
  always was). A refusal because the path is not a file is B62's answer
  (`notAFileNow`, asked first), never a fault. `keepUnread` writes nothing,
  keeps the row as it was (its old stat, or a version-stale row's old
  version, is what makes the next scan read it again), resets its missing
  count as the skip gate does (the walk saw the file), and one Warn per scan
  counts the files (`msgUnreadAudio`, with an example and the failed
  operation), never one per file: a mount that drops mid-scan fails every
  open after it. **A new file that never opens appears nowhere** (one the
  service user may not read, say) until a scan reads it: its download fails
  the same way, and a row by name is a guess the enricher would search
  MusicBrainz with; the dangling link's rule (keep, mint none) and the SACD
  one's. Driven through `Scanner.openAudio` (the `openSACD` seam's twin) on
  every platform, eight extractor families by four faults
  (`TestScanner_AChangedFileWhoseReadDidNotCompleteKeepsItsRow`, its
  new-file and version-stale twins), and with chmod 0 on unix. **No
  `ExtractorVersion` bump**: a file read whole extracts byte-identically,
  and the first scan after the change rewrites only the files it reads, as
  it always did (`TestScanner_AScanOverUnreadFilesRewritesOnlyWhatItRead`).
  The cost, accepted: a file that never reads whole (a permission, a bad
  sector under its tags) never gets a row, or keeps the one it had, with
  a line every scan; and a kept row of a changed file keeps its old size,
  so the app's size check fails a download of it until a scan reads it,
  the state the library was in before that scan. The guesses
  the defect already wrote keep a stat that matches, so they stay until
  their file changes or a later bump re-reads every row (its diff then sees
  the file's tags); a bump now re-reads the library and re-upserts every
  SACD virtual row to heal rows nothing can tell from a tagless file. To
  heal one by hand, zero its `mtime_ns` (`## Local test fixture`).
- **…and a folder's cover reaches the tracks beside it when IT changes,
  not only when they do** (2026-09-29, backlog B141). A cover is read only
  when a track is extracted, and the skip gate extracts an unchanged audio
  file only for a version bump, a missing `local-` cache file or lyrics
  drift, so a `cover.jpg` added after its tracks were indexed never reached
  them (measured: a full scan and a subtree scan both left the rows
  without it), nor did a replaced one, and a removed one's art stayed
  forever. **Every row records the identity of the folder art it was
  extracted against** (`tracks.folder_art_key`, v49, column-only: each
  candidate's name, size and mtime, a disc folder's parent's after a '|';
  `folderArtKey`; a candidate that is not a file is left out of the key by
  `fsutil.NotAFile`'s list of kinds, **never by "is a regular file"**, which
  would drop a OneDrive placeholder cover, and is still handed to the lookup,
  which refuses and names it as B62 made it), and `folderArtDrifted` re-extracts a row whose folder's
  identity changed, through `reExtractUnchanged`'s diff-guard, so only rows
  whose art changes reach the delta: a cover beside an embedded picture
  takes the stamp leg, which records the new key (**the stamp must write
  the key**, or the gate goes back to the row every scan). **The gate and
  the extraction read ONE per-scan state** (`folderArtDirStateOf`: the
  directory listing the lyrics check reads, now one per directory per scan
  shared by every worker, `dirListings`, plus a stat of each candidate),
  **taken BEFORE the lookup reads the cover** (`folderArtFor`), so no row
  records an identity newer than its cover: taken after, a cover replaced
  as it was read was recorded under the new identity beside the old bytes,
  and never read again (`TestScanner_ACoverReplacedWhileItIsReadIsReadAgain`,
  red with the order swapped). **A removed cover takes its art away**:
  `mergePostScanFields` no longer copies an old `local-` value onto a fresh
  extraction whose pipeline ran and completed (`localArtSettled`) and found
  none, since `local-` is the scanner's own value (the enricher writes
  MusicBrainz ids, never over one); the upsert's `enriched_at` reset lets
  the enricher give the row a network cover, as for a changed file. A
  folder cover still outranks the enricher's (fresh non-zero wins).
  **A cover the scan could not see, read or store is B134's rule for
  covers**: a stat or read that did not complete (`folderArtReadIncomplete`;
  gone, or not a file, is an answer), or a cover whose cache file could not
  be written (a full or read-only data directory), leaves the extraction
  `localArtUnsettled`, the row keeps the art it had
  (`keepArtOfUnsettledRead`, on the full path too, and over a partial answer
  from another candidate: taking it would change the row twice), records
  `folderArtUnsettledKey` ("?"), and the gate then retries only the COVER
  (`folderArtUnreadable`) and re-extracts the rows once it is stored, so a
  cover that stays unreadable costs one failed attempt per folder a scan,
  never a tag read of its album, and one Warn per scan counts its tracks
  (`msgUnreadFolderArt`). A folder whose listing or a candidate's stat fails
  keeps its rows untouched (an ELOOP'd cover read as "no cover" dropped the
  album's art). A cache write that failed was first read as settled, and a
  cover replaced while the data directory was full kept its rows on the old
  art under the new cover's key, for good (CodeRabbit on #1117,
  `TestScanner_AReplacedCoverWhoseCacheCannotBeWrittenIsStoredLater`). An
  EMBEDDED picture whose cache file could not be written is no verdict
  either (`localArtWriteFailed`), and the gate does not retry it, since that
  retry is a tag read: the merge keeps the row's old `local-` value for
  `needsLocalArtworkRecovery` to retry (read as "no cover", a wiped cache
  whose rewrite failed lost its art for good,
  `TestScanner_AWipedCacheThatCannotBeRewrittenKeepsTheArt`). **A
  file its extractor refuses** (read whole, not its format, written by
  name) records `folderArtNotLookedKey` ("-"), which the gate never
  re-checks, and one the gate re-reads records it through the stamp of
  its refusal (the next bullet), without which it was re-read every
  scan (`SetFolderArtKey`, which recorded it alone, went with B145). **The
  upgrade**: existing rows' key is '', which is also the key of a folder
  with no cover, so the first scan re-reads, once, the tracks in folders
  that hold one. **No `ExtractorVersion` bump**: what extraction produces
  for a file and its folder is unchanged. **Cost, measured** on dido (3,000
  FLACs in 300 albums with covers): an unchanged scan takes the same time
  (522 ± 26 against main's 528 ± 14 ms) and makes 1,262 directory reads
  (getdents64) where main made 6,360, since the listing is no longer read
  once per worker, plus one stat per cover. Residuals: a cover rewritten in
  place at the same size inside one mtime tick (the audio gate's own), and
  a stale `local-` value from a cover removed BEFORE the upgrade (its folder
  keys to '' like its row; the next `ExtractorVersion` bump drops it).
- **…and a file its extractor REFUSES records the refusal, and the skip
  gate asks its row nothing more until the file or the extractor changes**
  (2026-09-29, backlog B145). A refusal is a read that completed and was
  refused as not its format (the DSF, DFF, AIFF and WAV walks, for a header
  that is not one; every other extractor reads what it can and refuses
  nothing), indexed by its name as it always was. Such a file was re-read,
  with an ERROR line, on every scan, forever: after any `ExtractorVersion`
  bump, since `reExtractUnchanged` answered a refusal with nil and stamped
  nothing; and with no bump at all beside a lyrics sidecar (`bad.dsf` +
  `bad.lrc`) or when its row names a `local-` cache file that is gone,
  since those gate questions ask about what the extraction of a refused file
  never reaches (measured with the real scanner, all red on main). **Every
  row records whether the extraction it was written or stamped from refused
  the file** (`tracks.extract_refused`, v50, column-only,
  `Track.extractRefused`), and `rowIsCurrent` answers yes for a refused row
  at the current version without asking the lyrics, art-recovery or
  folder-art question. **A refusal on the version-stale leg is stamped,
  never written**: `reExtractUnchanged` hands the writer a
  `versionStampOnly` Track with `extractRefused`, and
  `StampExtractorVersionBatch` records the version, the mark, the
  missing-count reset and the refusal's folder-art key ("-"), and SKIPS
  the lyrics write, which reads a refusal as a file with no lyrics and
  deletes a row an older extractor wrote. Tags, `indexed_at` and
  `enriched_at` are untouched, so nothing reaches a paired device. **Don't
  rewrite the row**: it may hold tags an older extractor read before a
  stricter one refused the file
  (`TestScanner_ARefusalKeepsTheRowAnOlderExtractorWrote`). **Every other
  write clears the mark** (both upserts from the Track, the stamp from a
  Track that read), so a file that reads again, changed or under an
  extractor that learned to read it, goes back to the gate every file takes
  (`TestScanner_AFileTheExtractorNowReadsLosesItsRefusal`). **B134's line
  holds**: only a completed read can refuse, `readFault` is asked FIRST, and
  a read fault keeps the row at its stale version for the next scan
  (`TestScanner_ARefusedFileWhoseReadDidNotCompleteIsReadAgain`). **A
  refusal is logged once per path, size and mtime**, the analysis strike's
  dedup: the full path logs each refusal it writes (it reads only a file no
  row describes), and the version-stale leg only when the row had not
  recorded one, so a later bump re-reads a refused file once, silently.
  **No `ExtractorVersion` bump**: extraction is unchanged. v50 leaves
  existing rows unmarked, so a refused file the gate goes back to (after a
  bump, or for one of those questions) is read, and logged, once more, and
  marked then. Residual: a refused row holding a `local-` value an older
  extractor gave it, whose cache file is gone, keeps it (a refusal cannot
  recover it, and the row is not rewritten); it no longer costs a read per
  scan.
- **A library ROOT that is itself a link to a directory is walked THROUGH,
  and every walk of a root starts from `fsutil.WalkableRoot`** (2026-09-28,
  backlog B41). `filepath.WalkDir` Lstats its root and follows no link, so a
  configured `/music -> /mnt/nas/music`, and on Windows a junction or a
  volume mounted in a folder (`ModeIrregular` without `ModeDir` since Go
  1.23), was one entry that is not a directory. Measured on main: Scan and a
  subtree scan of the root indexed 0 rows (2 with the same root spelled with
  a trailing slash), a multi-root scan indexed nothing under the linked root,
  the watcher registered 0 watches and returned nil, the doctor's inotify
  count saw 0 directories, and `POST /v1/upscale` of the root enqueued the
  folder itself.
  **The half that deleted a library**: an install whose root BECAME a link
  after it was indexed logged `suspected clean-empty mount failure` every
  scan, with the hint to place `.bridge-allow-empty`; the sentinel, created
  through the link, lands beside the files, and at the production threshold
  the third scan deleted every row and sent the tombstones, every file on
  disk. A subtree scan of the root (the watcher's, an upload's, the trash
  tidy-scan) reaped every row with no line at all, and so it did when the
  link DANGLED, where Scan spared them. `WalkableRoot` answers the root with
  a separator appended when it is a link to a directory: POSIX resolves a
  trailing slash through a symlink, chains included, and Go's `os.Lstat` on
  Windows follows a name surrogate when the path ends in a separator. So
  every path below keeps the CONFIGURED spelling: `relPath` stores what it
  always stored, a multi-root prefix is the configured root's basename and
  never the target's, and `fs.Resolver`, which joins lexically, serves each
  row from the file the scanner read. **Compare the root entry against the
  WALKED string, never the root**: WalkDir hands its callback the string it
  was given, separator included, so `abs != root` counts the root as an
  entry and can prune it. **Not `filepath.EvalSymlinks`**: since Go 1.23 it
  resolves no Windows junction or mounted folder, which are not
  `ModeSymlink`, so the "resolved" root is the junction again. Nor the root
  resolved through them (`fsutil.ResolveLinks`, which the sidecar walks use
  because they unlink in the tree they walked): that respells every path,
  which each caller would have to map back. A root that cannot be stat'ed
  through (missing, a dangling link, a link into a mount that went away)
  is an error and "",
  never the unresolved root: Scan logs `root unreachable` and spares it,
  ScanSubtree of the root returns before its deletion pass, the watcher and
  the doctor report it. **Only the root is followed**: a link to a directory
  BELOW a root is still not walked by any of them (and the scanner says so:
  the B74 bullet below), and the upscale folder
  walk follows a folder only when it IS a root, so it enqueues nothing the
  manifest does not hold. `TestEveryWalkOfALibraryRootStartsFromWalkableRoot`
  requires every production function that calls `filepath.WalkDir`,
  `filepath.Walk` or `fs.WalkDir` to be classified: the five root walks
  (`walkRoot`, `ScanSubtree`, the watcher's `addTree`, the doctor's
  `countDirs`, the upscale folder walk) must reach `WalkableRoot`, and every
  other names what it walks. It sees the call, not that its answer is what
  gets walked; the tests of each walk drive a linked root through it. No
  `ExtractorVersion` bump and no PROTOCOL.md change: an install whose root
  became a link rewrites nothing, and one whose root was always a link
  indexes its library for the first time (a whole-library delta and
  enrichment, once).
- **A subtree scan OF the root runs the clean-empty guard Scan runs, and a
  line about a linked root names what it links to** (2026-09-28). The
  owning-root audit answers a SUBTREE that is not there; nothing covered the
  root itself, so a subtree scan of an emptied mount point reaped every row
  at the threshold while Scan spared them (measured on a plain root: three
  subtree scans, every row deleted, no line). `emptyRootMustBeSpared` runs
  there now, after a walk of the root that saw nothing and did not fail. For
  a linked root the directory found empty, and the one the sentinel is
  looked for in, is the link's target, so `suspected clean-empty mount
  failure` and `root unreachable` carry `links_to` (`rootLinkTarget`:
  `EvalSymlinks` for a live symlink, `os.Readlink` for a dangling one or a
  junction) and the hint says to check that volume is mounted. What the
  guard counts is the next bullet's.
- **…and the guard counts only LIBRARY CONTENT, by the walk's own rule**
  (2026-09-28, CodeRabbit on #1076). The walks counted every entry they were
  handed, dot-files included, so a `.DS_Store` Finder wrote into an emptied
  mount point, or a Synology `@eaDir`, made the root non-empty: no line, and
  the third scan deleted every row, in Scan and in a subtree scan of the
  root, single- and multi-root (measured, 8 cases of 8). The owning-root
  audit a subtree scan runs when its subtree is missing counted
  `len(entries)` the same way, and the bounded pass reaped the subtree.
  `isLibraryEntry` is the one predicate: a directory the walk descends into
  (`ShouldSkipDir` says no) or a file it indexes (not a dot-file, an audio
  file `enqueueableAudioFile` takes). Both walks skip by it and count by
  it, and the audit asks it of the root's listing (`holdsLibraryContent`).
  **Every file the walk does not index counts as nothing**, not a list of
  named ones (`Thumbs.db`, `desktop.ini`, a `NOT_MOUNTED` marker, a cover
  image): a second list is a second rule, and a file the walk ignores is no
  evidence the volume is there. The cost: a mounted root whose last audio
  file went, with only such files left, reads as emptied too and keeps its
  rows, with a line per scan, until the sentinel is placed, as an emptied
  root always did (`TestScannerThreshold1PreservesImmediateDelete` kept its
  database inside the root and passed only because that file counted; it
  keeps a second track now). **The sentinel is asked for by name, in the
  audit too**: it used to work by being an entry like any other, which is
  why a review on #289 called the audit's explicit check redundant, and it
  is now the only thing that says a root is empty on purpose. **`ShouldSkipDir`
  also names the directories operating systems and NAS firmware leave in a
  volume**, exactly, case included: `$RECYCLE.BIN`, `$Recycle.Bin`,
  `System Volume Information`, `lost+found`, Synology's `@eaDir`,
  `#recycle` and `#snapshot`, QNAP's `@Recycle` and `@Recently-Snapshot`,
  NetApp's `~snapshot`. The walk descended them, so a recycle bin's deleted
  files and a snapshot's copies were indexed as tracks of their own (the
  old walk indexed six of the test's fixtures on macOS), and their rows now
  go after the usual missing-count grace. It is exported so the doctor's
  inotify count skips by it: that count kept a copy of the old list, which
  would have gone on counting what the watcher now skips. So does the
  upscale folder walk (`POST /v1/upscale` of a folder, 2026-09-29), which
  descended them and offered every file in a snapshot to the enqueuer as a
  candidate; it walks the folder the request names whatever its name, as
  the watcher walks a dot-named root
  (`TestUpscaleFolderRequestSkipsWhatTheScannerSkips`,
  `TestUpscaleFolderRequestWalksADotNamedRoot`).
  `TestScanner_AnEmptiedRootHoldingOnlyNoiseSparesItsRows`,
  `TestScanner_ASubtreeScanBelowARootHoldingOnlyNoiseIsRefused`,
  `TestScanner_OSAndNASDetritusIsNotLibraryContent`.
- **…and the rows it counts are the ROOT'S OWN: never a UPnP-routed row**
  (2026-09-29, backlog B51). `CountTracksUnderRoot` answered the whole
  table in single-root mode, so a bridge whose root is empty and which
  relays an upstream logged `suspected clean-empty mount failure` on every
  scan (measured: three lines in three scans, `rows_in_db=3`, all of them
  routed), and the owning-root audit refused every subtree scan below the
  root ("no library content on disk but 3 tracks in DB"). A routed row is
  no evidence a root held anything: no walk sees one and neither deletion
  pass reaps one (`routedPathSet`). In multi-root mode a routed path prefix
  spelled like a root's basename inflated that root's count the same way.
  Both statements anti-join `upnp_track_routing`; the byte range and the
  empty-base fail-safe are `CountTracksByPrefix`'s, which the admin's
  per-root rollups still read unfiltered
  (`TestScanner_AnEmptyRootBesideRoutedRowsIsNotAMountDrop`,
  `TestScanner_TheCleanEmptyGuardCountsOnlyTheRootsOwnRows`).
- **A link to a directory BELOW a root is still not followed, and the
  scanner SAYS so** (2026-09-29, backlog B74). Moving an album folder to
  another volume and leaving a link (on Windows a junction, `mklink /J`)
  took it off every paired device, measured on main 6dfba62c (macOS
  symlinks, Windows 11 junctions; full scans, subtree scans of the root and
  of the folder holding the link, multi-root). (a) A root whose only
  content was such links read as a suspected clean-empty mount: an ERROR
  every scan, rows kept, and the hint to place `.bridge-allow-empty`, which
  then deleted both rows, a tombstone each, while the files stat'ed through
  the link. (b) A root holding one beside real content lost the rows under
  it at the threshold, a tombstone each, with only an Info count naming
  nothing. Both walks now record every entry `dirLinkEntry` calls a link to
  a directory: listing type neither regular nor a directory, and a stat
  THROUGH it a directory. **Never the `ModeSymlink` bit**: a junction or a
  volume mounted in a folder is `ModeIrregular` since Go 1.23, and a
  symlink-only test turned every behavioural test red on Windows with real
  junctions while macOS stayed green. (a) The guard's line for a root
  holding only such links is its own, `library root holds no content but
  links to directories…; its rows are kept`, not the mount-failure one (a
  link stat'ed through to a directory says the volume is there), with the
  count, an example, `rows_under_links` (`CountOwnTracksUnderPrefix`, the
  one statement `CountTracksUnderRoot` now runs too), and a hint that the
  sentinel deletes those rows and that the way to index a linked directory
  is to make it a library root. The owning-root audit's refusal says the
  same. (b) The deletion pass Warns `rows under links to directories are
  counted missing…` **once per ROW's streak**: `TracksNotYetCountedMissing`
  asks, BEFORE the increment, which of the rows under a link are still at
  `missing_count` 0, so a restart mid-streak says nothing more and a row
  that came back and went again says it again; a pass whose increment
  failed says nothing. **A link to a directory is never library content,
  whatever its name**: counted by its name (`Live.flac -> dir`), a root
  holding nothing else passed the guard and its rows went with no line, (a)
  turned into (b). `isLibraryEntry` stays the NAME rule and is never asked
  about one: both walks and `holdsLibraryContent` ask `dirLinkEntry` first
  (a track-named one is still in the "entries that are not files" line). A
  line names a link library-relative (#1055), never the absolute path it
  leads to, and a scan's guard line names only ITS root's links. Whether the
  scanner should follow such links is a product decision this rule does
  not make: backlog B80 (loops, #1070)
  (`TestScanner_ARootHoldingOnlyLinksToDirectoriesNamesThem`,
  `TestScanner_RowsUnderALinkBesideContentAreAnnouncedOncePerStreak`,
  `TestScanner_ALinkStreakIsSaidOnceAcrossARestartAndAgainWhenItRestarts`,
  `TestScanner_ALinkToADirectoryNamedLikeATrackIsNotContent`,
  `TestScanner_ASubtreeMissBesideOnlyLinksNamesThem`,
  `TestDirLinkEntryTellsALinkToADirectoryByTheStatThroughIt`).
- **A configured root that is a link is WATCHED at the directory it
  resolves to, and every event under it is named back under the configured
  root** (2026-09-29, backlog B51). fsnotify's kqueue backend (macOS, the
  BSDs) follows ONE level of a link it is asked to watch (it `Readlink`s
  and `Lstat`s once), so a root that is a link to a link had its own watch
  registered as a watch on a FILE: no Create event for anything added to
  it, and each change named after the root itself, which the watcher took
  for a change in the root's PARENT. Measured on macOS: a file dropped into
  the root, and one dropped into a folder made in it after the watcher
  started, never reached the manifest through the watcher, and each change
  logged `ERROR subtree scan … is not under any configured library root`.
  `watchWalkStart` answers the directory `filepath.EvalSymlinks` resolves a
  linked root to when that is a plain directory, the walk registers every
  watch in that spelling, and `configuredName` renames an event's path
  (longest resolved prefix, separator-bounded) before the scan is asked
  for. **The Create branch watches a new directory as fsnotify named it**,
  the resolved spelling, because kqueue already holds an entry watch under
  that name and a configured spelling would be a second descriptor. This is
  on every platform, not only kqueue: inotify and ReadDirectoryChangesW
  followed the chain already, and renaming their events back changes
  nothing they did. A Windows junction, which `EvalSymlinks` leaves as it
  is, is walked through `WalkableRoot` and watched as the configured path,
  as before (ReadDirectoryChangesW follows it). A plain directory root with
  a linked ANCESTOR (`/var` on macOS) is not a linked root and is
  untouched. Cost: `WatchList()` and the watch lines name a linked root's
  tree by its resolved spelling
  (`TestWatcherWatchesARootThatIsALinkToALink`,
  `TestConfiguredNameRenamesOnlyWhatIsUnderAResolvedRoot`).
- **Every containment check and sidecar walk resolves a Windows junction
  as it resolves a symlink** (`fsutil.ResolveLinks`, 2026-09-29, backlog
  B51). Since Go 1.23 `filepath.EvalSymlinks` leaves a junction (and a
  mounted folder, both `ModeIrregular`) as it is at the end of a path and
  fails with ENOTDIR through one, so `fsutil.EvalSymlinksOrClean` and
  `IsUnderAny` compared a junction'd path by its own spelling: measured on
  Windows 11, a variants directory spelled by the target's path under a
  junction'd library root (`D:\real\variants` against
  `C:\lib -> D:\real`) read as NOT nested, which `validateVariantsDir`,
  the admin variants-dir handler
  and `bridge variants move` exist to refuse, and so did every other nested
  case through a junction or a chain of them. And `resolveSidecarRoot`
  walked a junction'd variants directory AT the junction, as one entry:
  the inventory found 0 files of 2, and `TreeHoldsVariantSidecars` read a
  tree full of sidecars as holding none, which is the reading that lets a
  mass reap through. On Windows `ResolveLinks` opens an absolute path
  following every reparse point and names it by the handle
  (`GetFinalPathNameByHandle`, `\\?\` taken off); a path that is not
  there is `fs.ErrNotExist`; one the call cannot name by a drive letter (a
  volume mounted only in a folder can be one) is named by its volume GUID
  path, `\\?\Volume{…}\…`, which Go's os and filepath functions take as it
  is; and one that is there but cannot be named at all (two junctions
  pointing at each other, measured) gets `EvalSymlinks`'s answer, as
  before, **unless that answer still ends at a link to a directory, which is
  an error**: answered as resolved it is one entry to a walk and its own
  spelling to a comparison (CodeRabbit on #1090). Elsewhere it IS
  `EvalSymlinks`. **Not
  `os.Readlink` component by component**: that is a fork of the stdlib's
  link walk, and it answers `\\?\Volume{…}\` for every mounted folder, one
  with a drive letter included. The
  sidecar walk now starts at the junction's TARGET, so the paths a sweep
  unlinks are in the tree it walked (#1063's rule, for junctions). A SUBST
  drive resolves to what it stands for (measured), a mapped drive by the
  same call; both sides of every comparison go through the same function,
  so they agree
  (`TestIsUnderAnySeesThroughAJunction`,
  `TestSidecarInventoryResolvesAJunctionedRoot`,
  `TestTreeHoldsVariantSidecarsThroughAJunction`, Windows only, with real
  `mklink /J` junctions, and `TestResolveLinksFallsBackOnAJunctionLoop`;
  `TestResolveLinksAgreesWithEvalSymlinksWhereThereIsNoLink` on every
  platform).
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
  a present-but-different year is left alone. The one pass that crosses
  folders is the year fill by release id (`reconcileYearsByMBID`), bounded to
  strays (at most three year-0 tracks under one release); this bullet said
  none did until 2026-09-29.
- **A version-stale re-read that a reconciliation pass would rewrite is
  judged by the passes, at the scan's tail, with every such re-read in
  place** (2026-09-29, backlog B188). The passes REWRITE values a file
  sets (an album that is its folder's name, a minority album artist, a
  year of 0), and `mergePostScanFields` keeps a stored value only where
  the fresh one is empty, so the diff guard called every reconciled row
  changed on every ExtractorVersion bump: the upsert served the file's
  value and reset `enriched_at`, and the tail reconciled it back (a second
  `indexed_at` bump). Measured on main 99f3353e: a bump over 11 rows moved
  and re-enriched exactly the 3 a pass had rewritten, and after a
  `cover.jpg` touch (B141's re-read) a subtree scan, which runs no pass,
  served "Some Folder" for "Real Album" until the next full scan. The
  v0.2.1 upgrade scan (ExtractorVersion 21) would have done it to every
  reconciled row of a library. `reExtractUnchanged` now HOLDS a re-read
  that still differs from its row in one of the four fields after the
  merge (`reconciledFieldsDiffer`, `Track.awaitsReconcile`, unmerged; at
  most `maxHeldReconciles`, 10,000, past which it is decided as before);
  the scan's writer collects it, and `settleHeldReconciles`, at the
  reconciliation head (after the deletion pass, before the passes, with the
  one routed set), merges each with its row as stored now, runs the passes
  IN MEMORY over the library with every held re-read's values in place of
  its row's (`runReconcileStepsInMemory`), and stamps a re-read that then
  marshals as its row, or writes it whole with the passes' values, so the
  passes after it find nothing. A subtree scan settles its held re-reads
  the same way (`settleSubtreeHeldReconciles`), and one that returns before
  either writes them as they stand (`settleHeldUnreconciled`, a shutdown
  nothing). **The tail and the settle run one table**, `reconcileSteps`, in
  one order: the five `run*Reconciliation` functions went into it.
  **Judge with every held re-read in place, never against the stored
  siblings**: a bump that reads a whole album's tag differently leaves no
  outlier, while a re-read judged against its siblings' stored rows is
  voted back to the old reading and the change never applied (with the
  overlay dropped, `TestScanner_ABumpStillAppliesAnExtractorChangeAcrossAWholeAlbum`
  keeps "Old Reading" on all three rows and moves none). **Not a record of
  the value each pass replaced** (the review's first suggestion): rows
  reconciled before the upgrade carry none, so the upgrade scan would
  churn as before. **Not the passes in ScanSubtree's tail** (its second):
  measured, a subtree scan followed by the passes wrote the file's value
  (a bump and an `enriched_at` reset) and then the reconciled one (a second
  bump), and a bump's full scan runs the passes already. **The enricher's
  two replacements of a file's id meet the same guard**: a file's release
  or recording id that is no MBID reads as no id in the merge
  (`manifest.IsValidMBID`, the one shape the enricher's `isValidMBID` now
  answers through), and the acoustic fallback fills a recording id only
  where the file carries no valid one, as the merge's docblock said it
  did. `TestScanner_ABumpOverReconciledRowsOnlyStampsThem`,
  `TestScanner_ASubtreeScanAfterACoverTouchKeepsAReconciledAlbumTitle`,
  `TestScanner_ABumpOverAnIDTheEnricherReplacedOnlyStampsIt`,
  `TestScanner_ReReadsPastTheHoldLimitAreWrittenAsBefore`,
  `TestApplyAcousticFallbackKeepsARecordingIDTheFileCarries`.
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
- **A writer that writes back a row it READ earlier writes it only while the
  row is still the one it read: `MarkEnriched` and `applyReconciledTracks`
  compare-and-set on `indexed_at`** (2026-09-29, backlog B187). Both wrote
  back the WHOLE `tags_json` of an earlier read with no check, so whatever
  another writer did in between was overwritten for good. Measured through
  the real Store and Scanner on main 1f784879: a file retagged between the
  enricher's read (`UnenrichedTracks`) and its stamp kept its old title and
  a `size` of 124 inside `tags_json` against 161 on disk, `enriched_at` set,
  through three more scans; a `cover.jpg` added while the enricher worked
  lost the scanner's `local-` art (`ArtworkMBID` "") while `folder_art_key`
  recorded the cover as seen; and a reconcile pass that read a row before a
  stamp and wrote it after took every MBID away, `enriched_at` still set.
  Nothing healed any of it: the skip gate compares the `size` and
  `mtime_ns` COLUMNS, which neither writer touches, the enricher never
  revisits a stamped row, and the phone's exact-size check
  (`validateDownloadedSize`) fails every offline download of such a file.
  **`indexed_at` is the row's version**: every `tags_json` writer moves it
  (the two upserts' conflict arms, the two write-backs through
  `indexedAtAdvanceSQL`), so a write-back carries the version it read
  (`Track.rowVersion`, unexported, never marshaled) and writes `WHERE path
  = ? AND indexed_at = ?`. On a miss it writes nothing. `MarkEnriched`
  answers `ErrTrackChanged`, and the enricher counts nothing, logs one
  Debug line (`msgChangedWhileEnriched`) and leaves the row unenriched for
  its next batch, which reads it again and answers mostly from its caches;
  the pass skips the row, uncounted, and the next scan reconciles it from
  what it holds then. A row deleted in between is a miss too. **Every store
  reader that returns a Track records the version** (`GetTrack`,
  `LookupTrack`'s folded fallback, `UnenrichedTracks`, the list, stream and
  page readers, the UPnP baseline), and `UpsertTrack` and `MarkEnriched`
  the version they wrote. **A Track the store did not hand out is refused**
  (`errTrackNotRead`), never written unchecked: failing open is how a
  reader that forgets the version would reopen the race without a word, and
  a reconcile batch refuses before it writes anything. A writer that bumps
  `indexed_at` without changing `tags_json` (a rendition, lyrics, a booklet
  tag) costs at most a spurious miss, one more enrichment from the caches,
  never a livelock: each bump is a one-off, and no enricher path bumps the
  row it is enriching. **Don't compare the `tags_json` bytes** (older rows
  hold TEXT or BLOB, and it buys only fewer spurious misses), **and don't
  `json_set` only the fields a writer owns**: that stamps an enrichment of
  the old tags onto new ones and marks them done. `TestNoHandRolledIndexedAtBump`
  reads a WHERE clause's `indexed_at = ?` as a comparison, not an
  assignment (`TestTheIndexedAtSweepTellsAnAssignmentFromAComparison`).
  `TestAStampOverARowTheScannerRewroteWritesNothing`,
  `TestAStampOverARowWhoseCoverArrivedWritesNothing`,
  `TestAReconcileWriteOverAStampedRowWritesNothing`,
  `TestAWriteBackOfATrackTheStoreDidNotHandOutIsRefused`,
  `TestAStampRecordsTheVersionItWrote`, and the enricher's
  `TestAStampOverARowThatChangedMidEnrichmentIsNotCounted` and
  `TestASkipOverARowThatChangedMidEnrichmentIsNotCounted`.
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
  client delta bounded to rows that actually changed, and it stamps a file its
  extractor refuses too, or that file is re-read every scan after the bump
  (the B145 bullet above), and a re-read a reconciliation pass would rewrite
  when the passes leave it as it is stored (the B188 bullet above). **Derive that guard's
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
- **`enriched_at = 0` is NOT a tag-re-extraction reset**, and a zeroed
  `mtime_ns` IS: the skip gate reads the `size` and `mtime_ns` columns through
  `GetTrackStat` (measured 2026-09-28). This bullet said until then that
  `mtime_ns = 0` does not work because `GetTrack` reads the mtime from inside
  `tags_json`, which described the gate before #574 moved it to `GetTrackStat`. See
  `## Local test fixture` for the working paths.
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
  **Nothing recovers the WALK's callback** (`walkRoot`, `ScanSubtree`), so a
  panic there ends the scan and the process with it: what the walk logs about
  an error goes through `walkErrReason`, which cannot panic. An `*fs.PathError`
  without a cause panics in its own `Error()`, so a guard on the cause does not
  help once the text is taken first, which is where the suggested fix put it
  (Gemini on #1070; measured, the suggestion still panicked).
- **Shutdown joins every background writer**, and the wait must be written
  INLINE in the defer, never routed through a variable assigned later in
  `runServe` — the first tracked goroutine starts ~1200 lines before the end, so
  an early return leaves it nil and lets a live writer race `Store.Close()`.
  Both this wait and the watcher's are grace-BOUNDED; a wedged writer degrades
  to a log line, never a hung exit. A writer includes a goroutine whose CHILD
  PROCESS writes files: the Tailscale auto-pilot's `tailscale cert` was the one
  unjoined writer in `runServe` until #997 (the `cmd/bridge` section below).
- **A scan a shutdown stops reports no write the stop cancelled as a
  failure, and the final flush DROPS its batch on purpose**: the skip gate
  redoes exactly what was dropped, and flushing on a context detached from
  the cancel would hold shutdown for rows the next scan writes anyway
  (`TestAScanStoppedInItsFinalWriteLosesNothingTheNextScanCannotRedo`). A
  write that fails for any other reason is still reported. The rule is
  under **The CLI and the serve wiring**. (#1000)
- **Anything walking FLAC metadata blocks SEEKS past a validated PICTURE payload,
  never drains it.** The single-open FLAC path exists because a 5–25 MiB embedded
  cover crossing the wire twice per track halved scanner throughput on NAS-mounted
  libraries; a verdict needs ~30 bytes of header. This is safe ONLY because
  `meta.New` reads the 4-byte header directly from the reader and wraps it in a
  plain `io.LimitReader` — check that still holds before adding a seek anywhere
  else in that walk. A misaligned walk bails fail-open and silently disables the
  allocation guard, so alignment needs its own pin (that guard walked mewkiz's
  framing until 2026-09-29; it walks dhowden's now, by offset: the next bullet). **Verified against
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
- **A picture dhowden reads is guarded by walking the file AS DHOWDEN DOES, at the
  one `tag.ReadFrom` call, for every extension** (2026-09-29, backlog B99).
  dhowden's `readPictureBlock` does `make([]byte, dataLen)` from a 32-bit field
  BEFORE the read that would fail, and the runtime THROWS on the out-of-memory:
  `recover()` cannot catch a throw, so one file takes `bridge serve` down on every
  scan that reaches it. The old guard, `flacPictureBlocksSane`, sat in the `.flac`
  branch alone and walked the blocks by their DECLARED lengths, which dhowden does
  not: it reads a VORBIS_COMMENT or PICTURE block field by field and takes the next
  header from where the fields end. So it passed a PICTURE block declaring 0 or 6
  bytes (its own read of the fields failed, and it failed open), a PICTURE header in
  the unused tail of a VORBIS_COMMENT or PICTURE block, every METADATA_BLOCK_PICTURE
  comment (base64 of the same structure, decoded at the end of every VORBIS_COMMENT
  block and in an Ogg comment packet), and a FLAC- or Ogg-shaped file under any other
  extension (dhowden picks its parser by the first bytes). Measured on main, one
  extraction each: 13 shapes of 50 to 868 bytes allocated about 1 GiB apiece, the
  fuzzer's 40-byte input 4.28 GB. `dhowdenPicturesWithinBudget` (dhowden_picture_guard.go) mirrors
  `ReadFrom`'s dispatch, `ReadFLACTags` and `ReadOGGTags`, sums what dhowden would
  allocate for pictures (a PICTURE block's MIME type, description and data; for each
  METADATA_BLOCK_PICTURE decode, the decoded bytes, the MIME type, the description
  and the data), and refuses the file when the sum passes `pictureBudget`: twice the
  file's size plus dhowden's own 10 MB up-front allowance (a well-formed file needs
  at most 1.5 times its size; the allowance lets a picture truncated within it be
  read and dropped, as it always was). **Count the strings, not only the picture**:
  a `readString` whose length is within that 10 MB allocates all of it up front,
  there or not (`readStringCost`), and the fuzzer broke the first version, which
  counted the picture alone, in under six minutes: a 787-byte file whose picture
  declared a 2.9 MB MIME type allocated 68 MB. **A sum, not a per-picture bound**:
  dhowden's comment map outlives the block, so one METADATA_BLOCK_PICTURE is decoded
  again at every later VORBIS_COMMENT block (41 decodes of a 9 MB declared picture,
  or of a 9 MB MIME length, 387 MB from under 800 bytes). A refused file is one
  whose tags dhowden could not read: a Warn naming the picture kind, the folder-art
  fallback, no tags. **Stop only where dhowden stops**: where it would fail with an
  error the walk may read on (a comment with no '=' past the 67 bytes scanned for the
  key, invalid base64 past the decoded header, an Ogg page with a bad CRC, which the
  walk does not check), which only adds to the sum, and a file dhowden fails on has
  no tags either way. It fails
  OPEN on a read it cannot complete (dhowden's fails there too), I/O errors included.
  **It reads no picture payload**: fields by offset, a METADATA_BLOCK_PICTURE's header
  through base64's streaming decoder, Ogg page headers and segment tables and no
  segment data it does not need (`TestThePictureGuardDoesNotReadAPicturePayload`).
  **Lowercase as dhowden does**: U+212A KELVIN SIGN lowers to 'k', so a key spelled
  with it is decoded (`TestNoRuneLongerThanThreeBytesLowersIntoThePictureKey` pins
  the 67-byte scan). `TestDhowdenStillAllocatesAPictureBeforeReadingIt` fails the day
  dhowden bounds the allocation itself; retire the guard then, and keep the seeds.
  **No `ExtractorVersion` bump**: every picture of a well-formed file fits the
  budget, so no such file's output moves, and a refused file either made dhowden fail
  anyway (a PICTURE block whose data is not there: no tags before or after) or
  declared a METADATA_BLOCK_PICTURE more than 10 MB past its bytes, whose stored row
  keeps the tags it has until the file changes, which a bump would take away. **Every
  whole-file extractor fuzz target carries the allocation property**
  (`fuzzExtractOnce`), so a length that sizes a buffer the file cannot back is a
  crasher, on any platform, with the size in its message.
- **…and an ID3v2 tag dhowden reads is walked the same way, against two bounds**
  (2026-09-29, backlog B101). dhowden gives every repeat of a frame id a key of its
  own by counting up from `id_0` (`readID3v2Frames`), so n copies of one id cost
  n(n-1)/2 map lookups, each building a string: through `tag.ReadFrom`, 4,000
  copies of a 12-byte TIT2 took 337 ms, and 16,000 (192 KB) 6.0 s and 1.9 GB of
  allocation. A 5 MB tag of one repeated frame is an hour of one scan worker, and
  the size field (whose synchsafe bytes dhowden does not mask) admits about
  512 MiB, while `Scan` holds the scanner's mutex.
  `dhowdenID3v2WithinBudget` (at the one `tag.ReadFrom` call, through
  `dhowdenReaderFor`, ReadFrom's dispatch in its order, the "DSD " pointer
  included) and `id3v2TagWithinBudget` (the two `tag.ReadID3v2Tags` calls: the
  DSF extractor and `applyEmbeddedID3`) walk the tag as `ReadID3v2Tags` reads it
  and refuse one past either bound. **The renaming lookups, SUMMED over every id**
  (`maxID3v2RenameLookups`, 2^21: one id 2,048 times): a bound per id lets n ids
  each repeat up to it. **And the frames stored** (`maxID3v2Frames`, 65,536):
  linear, but 1,000,000 distinct 12-byte frames took 3.7 s to extract and held
  160 MB, most of it `populateFromTagMetadata`'s lookups walking the raw map.
  **The renaming bound is held to the allocation property**: the renaming builds
  about 15 bytes a lookup, so at the bound 32 MB, half the 64 MiB
  `extractionAllocLimit` allows any extraction, and a tag the guard passes meets the
  property the fuzz targets assert (the first draft's 2^23 allocated 127 MB against
  a limit of 70 MB; `TestATagTheID3v2GuardPassesMeetsTheAllocationProperty`, which
  skips its allocation check under -race: the race runtime turns the tiny allocator
  off and doubles it). **Walk the reads, never the declared sizes**: dhowden reads
  the next header from where the payload it read ends, and version 3 compression
  takes 4 from the size (a frame declaring under 4 wraps, and `readBytes` then reads
  nothing: `io.CopyN` of a negative count), a version 4 data length indicator
  replaces it, encryption takes 1 (a zero indicator wraps), a version 4 extended
  header counts its own 4 length bytes (a length under 4 wraps), and under
  unsynchronisation every FF 00 is a byte the sizes do not count (the filter's state
  runs across frames). Each is a shape in `id3v2Shapes`, and
  `FuzzID3v2WalkAgreesWithDhowden` fuzzes the walk against dhowden itself: wherever
  dhowden reads a tag, the walk counts what it stores, per id. The loop ends on
  dhowden's `offset` (the header's 10 bytes, the extended header, every DECLARED
  size) against the size field, never on the stream; the one place the walk reads
  on where dhowden may not is the last frame, dropped by dhowden for an id it does
  not know (`validID3Frame`), one frame at most. **It reads no payload without
  unsynchronisation**; with it, where a frame ends depends on every FF 00 before
  it, so it reads through the filter (dhowden reads such a tag one byte per Read
  call). A refused tag is one dhowden could not read: a Warn naming the bound, the
  frame id and the counts, the folder-art fallback, no tags. **No
  `ExtractorVersion` bump**, for B99's reason: no real tag comes near either bound
  (taggers repeat an id a handful of times, chapters a few hundred), and a stored
  row keeps its tags until its file changes. `TestEveryDhowdenReadIsGuarded`
  requires every call of a dhowden reader in the package to sit in a function that
  asks the guards for what it reads, and refuses any other reader;
  `TestDhowdenStillRenamesRepeatedID3v2FramesOneLookupAtATime` fails the day dhowden
  stops counting up, and the renaming bound can go then.
- **An Ogg FLAC stream's tags are read from its own header packets, as a .flac
  file's are** (2026-09-29, backlog B102). dhowden's `ReadOGGTags` looks only for a
  `\x03vorbis` or `OpusTags` packet, and a FLAC stream in Ogg carries its comments as
  a FLAC VORBIS_COMMENT block in a header packet, so an `.oga` (or such a stream under
  any name: dhowden picks its reader by the first bytes) had no tags, and dhowden read
  every page to the end of the file, CRC-checking each, on every extraction (measured
  on main: an ffmpeg `.oga` and a `flac --ogg` one, both tagless, both read to their
  last byte). `oggFLACMetadata` (ogg_flac.go), at the top of
  `extractViaDhowdenFromReader`, finds a FLAC stream's first packet among the leading
  BOS pages (another stream's first page may come ahead of it) and joins that mapping
  packet, from its `fLaC` on, with the header packets after it: the metadata of a
  .flac file, read in place from the file. `readDhowdenTags` (both guards and
  `tag.ReadFrom`) and then `applyFLACMultiValueArtists` read the join, so an Ogg FLAC
  file's tags, pictures, multi-value artists and lyrics are a .flac file's. **The
  header packets end at the first of**: the count the mapping declares, the block
  flagged last, an empty packet or one opening `0xFF` (a frame's sync code: no block
  type is 127), the stream's EOS page, and `maxOggFLACHeaderPackets` (65,535, the most
  the count can declare: with no count and no last flag a stream would hold a
  packetSource per packet). Removing one of these leaves the join, and so the tags,
  unchanged, so each is pinned by how far the extraction READS
  (`TestAnOggFLACStreamIsReadNoFurtherThanItsHeaderPackets`). **An empty PADDING block
  flagged last ends the join** (`oggFLACTerminator`): in a .flac file the first audio
  frame's `0xFF` reads to dhowden as a header flagged last, and the join has no frame,
  so a stream whose last block is not flagged read no tags without it. **It declines a
  stream beside a Vorbis or Opus one**: dhowden reads those (it returns the first
  comment packet it meets), so no file dhowden could read changes. It reads no page
  after the last header packet and no segment data but the first bytes of a packet
  (the fixtures: 360 of 2,117 bytes, 610 of 1,441), and it wants mapping major version
  1, which libFLAC and ffmpeg both require (measured: both refuse 2). **ExtractorVersion
  18**; the codec stays "OGG", and an Ogg row still has no sample rate, bit depth or
  duration (backlog B118). The iOS app lists no `.ogg` or `.oga` from SMB or the device
  and takes a bridge row's tags as strings: no twin, no Mirror-PR.
  `FuzzOggFLACReadsBackTheMetadataItCarries` lays blocks out in pages of any size and
  requires them back byte for byte, and `FuzzExtractOGG` carries Ogg FLAC seeds, both
  picture bombs among them: the guards meet them in the join.
- **An ID3v2 tag's MusicBrainz ids and ReplayGain are read BY NAME, from its TXXX
  frames and the MusicBrainz UFID** (ExtractorVersion 19, 2026-09-29, backlog B116).
  dhowden files a TXXX frame as a `*tag.Comm` under `TXXX`, `TXXX_0`, … (the renaming
  B101 bounds) and a UFID as a `*tag.UFID` under `UFID`, and `stringOf` matches the raw
  map's KEYS, so no MP3, DSF, AIFF or WAV ever gave up TXXX "MusicBrainz Album Id",
  the recording id (the UFID owned by http://musicbrainz.org) or TXXX
  "REPLAYGAIN_TRACK_GAIN" / "…_ALBUM_GAIN": the enricher searched by text for releases
  the file names exactly. The call site's comment and `stringOf`'s docblock said
  otherwise, and the lookup's test (`…StringOfMatchesVorbisAndID3v2Spellings`, now
  `TestStringOfMatchesVorbisAndMP4Spellings`) passed because it keyed a synthetic map
  by the description, which is MP4's freeform shape. `id3v2NamedValues` and `namedValueOf`
  (id3v2_named_values.go) read them under the aliases a Vorbis comment and an MP4
  freeform atom answer, normalised the same way, with the same precedence: per alias
  in order, the raw map, then the named values. **Their order is never the map's**:
  the MusicBrainz UFIDs, then the TXXX frames, each in the tag's order from dhowden's
  suffix, the key breaking a tie. **The UFID answers `musicbrainz_trackid`, ahead of a
  TXXX of that name**, and Picard's TXXX "MusicBrainz Release Track Id" names the
  release's TRACK and answers nothing: `MusicBrainzTrackID` is the RECORDING id (the
  Atlas lyrics tier asks `/v1/atlas/recording/{it}`). **Every writer measured
  (mutagen, so Picard; ffmpeg) ends a TXXX value with a NUL, which dhowden keeps**, a
  2.4 field holds its values NUL-separated, and a later UTF-16 value keeps its byte
  order mark: `id3v2TextValue` takes the first value not empty after trimming space
  and U+FEFF. Kept, the NUL makes the release id no UUID (the enricher drops it with a
  Warn) and a gain no number. **Only these fields, never one ID3v2 gives a frame of
  its own**: the compilation flag (TCMP; the app pins that neither side reads
  TXXX:COMPILATION, `test_TXXXCompilationAndV22TCP_areNotRead`), the composer, the
  conductor, the work, the original year and the tempo stay frame-only on both sides,
  and the app's ID3v2Parser reads its MusicBrainz ids and ReplayGain from TXXX and
  UFID too (`TestAFieldID3v2GivesAFrameOfItsOwnIsNotReadFromATXXX`). **Don't widen it
  without the app**, and don't let `hasAnyRawKey`'s presence gates see a TXXX: they
  gate dhowden's accessors, which read frames (a TXXX "TRACKNUMBER" would make a nil
  track number Some(0)). The two readers still differ where the app is wrong or
  narrower (backlog B126), and neither reads Picard's TXXX:WORK (B127). **v19 changes
  only files whose tag carries such a frame**: they take the full-upsert leg (the tag's
  release id replaces one the enricher found by searching, since the re-extract wins
  `mergePostScanFields`; the enricher's cover stays until it runs again), their
  enrichment is re-queued once, and with a release
  id in hand the enricher does not search MusicBrainz for the release but fetches its
  cover (unless the file has local art) and resolves the artist. A tag ReplayGain now
  outranks the analysis loudness spliced in for a file with none, which PROTOCOL.md
  has always promised. No wire change and no Mirror-PR: the four fields exist and the
  app decodes them. The fixtures are real files (`testdata/gen/id3_txxx_fixtures.py`:
  mutagen writing as Picard's `_save` does, in its three encodings and four
  containers, and ffmpeg); `TestPicardsID3v2IdsAndReplayGainReachTheirFields`,
  `TestAnID3v2NameAnswersAsAVorbisOrMP4NameDoes` (each case twenty times: a map-order
  winner flaps) and `TestScanner_V19_ATagNamingItsIDsJoinsTheDelta_APlainID3RowOnlyStamps`.
- **Every dhowden read goes through `dhowdenReadBuffer`, a page at a time**
  (2026-09-29, backlog B117). dhowden's `unsynchroniser.Read` reads its stream ONE BYTE
  per Read call, and the extractors handed dhowden the `*os.File`, so every byte of an
  ID3v2 tag carrying the unsynchronisation flag was a read(2): measured on the dev Mac, a
  10 MB such tag took 3.8 s to extract (6.5 s in a Linux container), and the 512 MiB
  the size field admits would hold a scan worker for minutes while `Scan` holds the
  scanner's mutex. Now 62 ms (72 ms there). Each call (`tag.ReadFrom` in `readDhowdenTags`,
  `tag.ReadID3v2Tags` in the DSF extractor and in `applyEmbeddedID3`) reads the stream
  `newDhowdenReadBuffer` returns and calls its release after. **The buffer is the stream,
  read ahead**: it holds the stream's bytes from where the stream was, answers a Seek
  landing within them without touching the stream (ReadFrom's `Seek(-11,
  io.SeekCurrent)`, dhowden's short skips), hands any other Seek to the stream with its
  result and error, and hands a Read of a page or more, once drained, straight to the
  stream (a cover is not copied twice), an empty Read too. **release is load-bearing**:
  it puts the stream where dhowden's reading got to, so what reads the file after (the
  FLAC multi-value pass, the MP4 genre walk) finds it as before. **The guards still walk
  the bare stream**, before the buffer exists, and `TestDhowdenReadsTheSameThroughTheBuffer`
  is what keeps them seeing what dhowden reads: over every kind of stream dhowden reads
  (the committed Picard, ffmpeg, iTunes and Ogg FLAC fixtures, the B101 shapes, a FLAC,
  an ID3v1-only file, a DSF, unsynchronised tags) it answers through the buffer exactly as
  bare: error, raw map, picture and final offset. **The common path got faster, not
  slower**: dhowden reads a frame's id, size and flags a read each, and those now come
  from memory (through dhowden alone, 50 reads to 1 on a Picard MP3, 104 to 3 on an M4A;
  through `ExtractWithContext`, 95 to 80 µs and 144 to 108 µs), for 4 KB more allocated and
  up to a page more read after a far seek. `TestEveryDhowdenReadIsGuarded` requires each
  dhowden read to take the stream a `newDhowdenReadBuffer` in its function returned, the
  in-memory AIFF/WAV chunk included (one rule, and a buffer costs it nothing), and
  `FuzzDhowdenReadBufferReadsAsItsStreamDoes` holds the buffer to a bare `bytes.Reader`
  over a tape of reads and seeks (a seek window one byte too wide, a control, fell to it
  in 4 s; that input is a seed). No `ExtractorVersion` bump: dhowden reads the same bytes.
- **A tag a container keeps in chunks of its own outranks the path's guess, and a
  DFF's are read where its writers put them** (ExtractorVersion 20, 2026-09-29,
  backlog B140). The scanner fills a track's title, album and artist from its path
  (`fillFromPath`) BEFORE it extracts, and two walkers wrote a field only while it
  was EMPTY, which in a scan is never: the DSDIFF DIIN walk and the WAV LIST/INFO
  walk. Their extractor tests start from an empty Track and passed; B134's
  read-fault test told its DFF versions apart by sample rate because the title
  never moved. **The DIIN walk also read a layout no writer makes**: PR #223 read
  DITI / DIAR as a 1-byte length where DSDIFF 1.5 has a 4-byte big-endian count,
  and took DIAL and DIGN, chunks no specification defines, for an album and a
  genre, so it read nothing from a real DIIN even into an empty Track. Measured
  on a Philips reference DFF given a spec-layout DIIN: ffmpeg 9.0.2 and TagLib
  2.0.2 read its title and artist, MediaInfo 25.04 its title, the bridge nothing;
  on the 1-byte layout ffmpeg read "ring Title" and the others nothing. **And the
  DFF walk skipped the "ID3 " chunk**, which is where mutagen, so Picard, tags a
  DSDIFF file. Now `parseDIINChunks` reads DITI and DIAR as TagLib writes them
  (ISO-8859-1 when the bytes are not UTF-8: TagLib writes 0xE9 for é), the ID3
  chunk goes through `applyEmbeddedID3` (its picture is the cover), and
  `containerText.applyUnder`, once the walk is over, writes the container's own
  text beneath the ID3 tag: the tag answers each field it has a value for and the
  DIIN or INFO the rest, whichever chunk comes first, as TagLib reads a DSDIFF
  file (measured on `testdata/dff/diin_then_id3.dff` and `id3_then_diin.dff`).
  **An ID3 chunk nested in PROP counts too**, where the file holds no root one:
  TagLib 2.0.2 reads that placement (and rewrites a tag it found there in
  place), and a root tag wins whole wherever the chunks sit (`prop_id3.dff`,
  `prop_and_root_id3.dff`; CodeRabbit on #1118). So PROP's cap is 1 MiB plus
  the ID3 chunk cap, behind a fit check: at 1 MiB a nested tag holding a cover
  refused the whole file. **A DIIN holds a title and an artist and no album**:
  TagLib drops an album set on its DIIN tag when it saves, EMID is an opaque id
  and MARK a position in the audio. A DIIN or ID3 chunk the file ends inside is
  never allocated
  (`readDFFTagChunk`, the payloadFits rule): the walk ends there with the format
  stamped, where a truncated DIIN failed the file (indexed by name alone, no
  sample rate). **Don't write a path-guessed field only while it is empty**:
  `TestNoExtractorFillsAPathGuessedFieldOnlyWhenEmpty` fails on a comparison of
  Title, Artist or Album with "" anywhere in the package but `fillFromPath` and
  `mergePostScanFields`, and `TestScanner_EveryFormatsOwnTitleOutranksThePathsGuess`
  scans one file of every extractor family, since a test on an empty Track cannot
  see this. The fixtures are real writers' output (`testdata/gen/dff_tag_fixtures.sh`:
  TagLib 2 and mutagen in a Debian 13 container). The iOS app reads no DFF tags
  (`MetadataExtractionRoute` gives `.dff` no extractor and `DFFHeadScan` types the
  file only), so there is no Mirror-PR; an app reader added later takes this
  precedence (backlog B149). v20 changes only rows of files carrying such text.
- **A compressed AIFF-C or WAV is named by its encoding, and counted lossy**
  (ExtractorVersion 21, 2026-09-29, backlog B124, B154). The AIFF walker named
  every AIFF-C "AIFF" and the RIFF walker every WAV "WAV", names on every lossless
  list (librarycat's quality sets, the CarPlay optimize gate, the app's), so a
  µ-law AIFF-C at 44.1 kHz with no depth was filed CD Quality and an ADPCM WAV or
  IMA4 AIFF-C at 96 kHz Hi-Res and a CarPlay render candidate (measured on
  afconvert's, ffmpeg's and sox's files). `aifcEncodingOf` names an AIFF-C by its
  COMM compression type and `wavEncodingOf` a WAV by its fmt format code (or an
  extensible header's subformat: ffmpeg writes one for every ADPCM WAV above
  48 kHz), as the app names them since its #2014 and #2028: the linear types keep
  "AIFF" or "WAV" and their depth; the compressed ones are "ULAW", "ALAW" and
  "IMA4" in an AIFF-C and "ADPCM", "GSM", "ALAW", "ULAW", "MP3" and "MP2" in a
  WAV, with no depth. **An AIFF-C the bridge cannot read is "AIFC"** (an unknown
  compression, or no COMM), the app's name for it, on neither list, with no depth
  and no duration; **a WAV of a format code it does not name keeps "WAV" and loses
  its rate** (default-deny, as for a DFF of an unknown compression: with the rate,
  the lossless name claims a tier). **An IMA4 COMM counts 64-frame packets**: the
  duration was 64 times too short (a 30 s file read 0.4688 s). **The first COMM
  and fmt chunk the walk can read names the file**, a later one skipped whole, as
  TagLib (and ffmpeg, for a WAV) reads it and as the walkers read their first SSND
  and data chunk: parsed one on top of the other, a second chunk's codec landed
  beside the first one's depth (CodeRabbit on #1122). A WAV of code 0x0050 is
  "MP3" where its MPEG1WAVEFORMAT `fwHeadLayer` names layer III, read under that
  tag alone (an extensible header's subformat has other fields there). **The six names are
  one vocabulary in four places**, `manifest.IsLossyCodec`, its SQL mirror in
  `upscaleEligibleSQL` (the admin lockstep matrix pins the pair), librarycat's
  `lossyCodecs` and the dupes ranking set; a new compressed name joins all four.
  The dupes lockstep test checked one direction until then, so a name reaching
  `IsLossyCodec` alone passed it. The app's lossy list lacks "MP2" (backlog B158).
  `.aifc` is in the DLNA MIME table, the UPnP walker's audio set, the ingest's
  codec table ("AIFC": a DIDL cannot say what one holds) and the console player's
  MIME and playability tables (a compressed WAV plays engine-dependent there).
  **The optimize gate's codec-empty extension fallback leaves `.aifc` out on
  purpose**, in Go and SQL alike: the extension cannot say whether one is
  compressed. v21 changes those rows, and a linear AIFF-C of a type the old depth
  list lacked (42ni, FL32, FL64), which gains its depth.
  `TestACompressedAIFCOrWAVIsNamedByItsEncoding` (twenty real files,
  `testdata/gen/compressed_pcm_fixtures.sh`), `TestAIFCEncodingOf`,
  `TestScanner_V21_ACompressedAIFCOrWAVJoinsTheDelta_ALinearOneOnlyStamps`.
- **Extraction: presence-gate the integers, refuse bit depth on lossy codecs, and
  split TIT1→Work / TIT2→Title.** dhowden returns 0 for both "tag absent" and "an
  explicit 0", so Year/TrackNumber/DiscNumber need a raw-map presence check to
  keep `Some(0)` distinguishable from `nil`. `canSetBitsPerSample`, an allowlist
  of the lossless names, gates every extractor's `BitsPerSample` write
  structurally (this said `isLossyCodec`, a denylist #225's review replaced
  because it failed open on an empty codec, until 2026-09-29). `Year()==0` falls back to
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
- **`skipID3v2` walks a STACK of prepended ID3v2 tags, at most
  `maxStackedID3v2Tags` (8), not only the first** (ExtractorVersion 16, from the
  iOS app's FLAC follow-up to #1935). A tagger that prepends a new tag without
  removing the old leaves two, and Core Audio plays such a FLAC (measured
  2026-09-25); stopping after one left the cursor on the second, so the fLaC
  check failed ("bad magic ID3") and the track lost its rate, depth, duration
  and multi-value artists, and an MP3's frame search began inside a tag. Its
  iOS twin, `FLACStreamInfo.magicOffset(reading:)` (the engine's STREAMINFO
  read and the scanner's `streamStart` share it), has the same cap and the same
  v2.4-only footer, and also refuses a size byte that isn't synchsafe (Core
  Audio refuses that file) where this masks it, as it always has. **Don't stop
  after one tag, don't drop the cap, and don't change one side's walk without
  the other.**
- **`Track.Compilation` puts the file's TCMP / `cpil` / COMPILATION flag on the
  wire** (ExtractorVersion 17, for the iOS app's "hide artists who appear only on
  compilations"). It is set by the SAME `stringOf(raw, "tcmp", "cpil",
  "compilation") == "1"` read that fills AlbumArtist "Various Artists" for a
  flagged file with no album artist, so the flag and that fill can never
  disagree about which files are compilations; the iOS enrichers apply the same
  "only 1 counts" rule to local files. The fill's condition did not change — a
  flagged file with a TAGGED album artist (a DJ mix, a label sampler) keeps it,
  and was indistinguishable from an ordinary album on iOS until v17. **Don't make
  it a `*bool` or emit `false`**: `omitempty` keeps every unflagged row
  byte-identical to its pre-v17 form, which is what limits the v17 re-extract's
  delta to the flagged files (plus the SACD ISO rows every bump re-expands).
  Those take the full-upsert leg, so their enrichment is re-queued once
  (`mergePostScanFields` keeps their MBIDs and art meanwhile). An app older than
  the field consumes that delta and drops it, so the iOS app's first sync after
  it gains the field reaches back to 2026-09-27, once per bridge.
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
  walk hot path). The one such cursor, the orphan sweeper's, went on
  2026-09-28 with `pathWalkCompare` and `dirEntirelyBehindCursor`: every tick
  walks the whole tree now (under **Job pools**). The rule is for any cursor
  that comes back.
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
  never `URLSearchParams` (which form-encodes a space back to `+`). **The
  console's variant delete was the last admin handler reading a library path
  through `r.URL.Query()`** (2026-09-28): `curl -X DELETE
  '…/api/upscale/variants?prefix=AC+DC'` answered `deletedCount: 0` on main and
  left the rendition on disk. Its client, `deleteVariants` in
  static/player/api.js, was on `URLSearchParams` and worked only because the
  server form-decoded too, so the handler moved to `safeQuery` and the client to
  `encodeURIComponent` in one commit. **The two halves move together**: either
  alone breaks every path with a space.
  `TestDeleteVariantsClientRoundTripsThroughTheServer` runs the shipped client
  under node and sends the URL it builds to the handler.
- **…and a query the bridge WRITES for such a reader goes through
  `internal/urlquery`, never `url.QueryEscape` or `url.Values.Encode`**
  (#1046). Both write a space as `+`, and two readers keep `+` as a plus:
  Foundation's `URLComponents.queryItems`, which the app reads the pairing QR
  with, and `safeQuery`. So from April to September the QR named a library
  `My Library` as `My+Library` (the default as `1-bit+Bridge`), which the app
  pre-fills as the name it saves the bridge under, and `bridge enrichment
  misses --path "Meridian Glass"` asked for `Meridian+Glass` and reported 0
  misses of 5. `urlquery.Escape` / `Encode` write `%20`, which loses nothing
  (a literal `+` is always `%2B`) and decodes alike under both kinds of
  reader. **Read a pairing URL in a test as the app does, never with
  `u.Query()`**: that is a form decoder, it reads the `+` back as a space,
  and it is how every pairing test passed (`appQueryItems` in
  internal/admin; with the old encoder and a form-decoding reader, the
  mint/rotate test goes green). The bridge's fix is the whole fix: the app
  parses RFC 3986 correctly, and reading `+` as a space there would refuse
  codes that pair today (a pre-#1042 name with a trailing space is written
  `name=X+`, which the app takes as "X+" and would then refuse as "extra
  spaces").
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
- **An endpoint the bridge advertises carries no user name, password, path,
  query or fragment** (backlog B54; the path since B66). A `customEndpoints` entry was kept as
  written, so `/v1/health`, which answers without a token, published
  `https://user:password@host:7788` (a token as the user name, a `?token=`,
  a `#…` alike) to any caller, and the pairing QR's `urls=` carried it
  (measured on main with the real binary: all four shapes, in both); in
  public mode a declared endpoint is also the QR's PRIMARY `url=`. The phone
  relies on none of those parts: it sets its own `Authorization` header (the
  bearer token) on every request, its request delegates cancel every
  challenge but the server's certificate, and `buildRequest` replaces the
  path, and the query of every request that carries one (the file routes),
  so no proxy that needed one of those parts could have served the app's
  file routes (read in `BridgeSourceClient`, `BridgePairingURL` and
  `SMBStore`, 2026-09-28). **A path is not the root's to keep either**
  (B66): `buildRequest` sets every data request's path over the endpoint's,
  and the requests that APPEND to it instead (`appendingPathComponent`: the
  pairing join and the redeem, the first-contact `/v1/health` probe, the
  `/v1/events` stream) reach a path the bridge does not serve, which answers
  404, and a redeem that meets one fails pairing outright ("doesn't accept
  pairing codes", no fallback). So no deployment used one, every request
  reaches the bridge without it, and a token in it reached every caller of
  health for nothing (read in `BridgePairingClient`, `BridgeEventStream`
  and `BridgePairingPersistence`, 2026-09-30). A bare `/` is the root and
  is kept; a path counts only after an authority, so `not a url` is still
  the prune's to drop, not a refusal naming a path.
  **Repair what is stored, refuse what is typed**
  (the library name's rule, under Config): `ValidateCustomEndpoints`, which
  `Normalize` runs for `Load` and every writer, keeps such an entry WITHOUT
  those parts (`config.HasCredentialParts`), dedupes on the published form,
  and warns once per entry under its own message, never "dropped"; the
  settings PATCH refuses a typed one whole (`config.CheckCustomEndpoints`,
  400 `validate`, nothing written), and `bridge init --public --domain`
  refuses any domain `config.AutocertHost` would change (exit 2, nothing
  written; the next bullet), since the domain is also the autocert host
  that health and the QR build a URL from, as a string. It asked
  `HasCredentialParts` until B66, which let a path, a scheme and a port
  through; and it wrote the untrimmed flag, so a padded domain's endpoint
  (`https://` joined to the spaces) did not parse and the install was saved
  with none.
  **Don't drop such an entry**: its host and port still reach the bridge,
  and a loaded config must not lose a route over a part nothing reads.
  **Don't strip at the publish sites**: every enumeration
  (`ReachableEndpoints`, `pairAlternates`' public branch,
  `defaultBridgeURL`, the console's panel) reads the normalized list, and a
  second strip is the copy that drifts. No wire change and no Mirror-PR: an
  endpoint is still a URL the client dials. The file keeps the value until
  the next save writes the normalized list, which the warning says.
  `TestServePublishesNoCustomEndpointCredential` boots serve and checks
  health, a minted link and every log line;
  `TestHealthPublishesNoCustomEndpointCredential` and
  `TestPublicPairingCarriesNoCustomEndpointCredential` cover public mode.
  **Nor does `/v1/health` publish a manual upstream's URL**:
  `upnpUpstreamServers[].descriptionURL` read it back for a server
  configured with a UDN AND a manual URL, whose poller caches its fetch
  under the UDN, while the DTO and PROTOCOL.md keep a manual URL off the
  wire. The public adapter leaves out a value equal to the configured
  manual URL, and never strips one: the device's own LOCATION is published
  as it gave it, query included (a Windows device host's is
  `…/udhisapi.dll?content=uuid:…`), so a rule on its parts would cost those
  devices the hint (`TestHealthDoesNotPublishTheOperatorsManualURL`).
- **…and `autocert.domain` is a host name alone, served as the host it
  names** (backlog B66). Public mode builds a URL from it as a string,
  `https://<domain>:<listen port>`, in `/v1/health` (`publicModeEndpoints`),
  in the QR's alternates (`pairAlternates`) and as the QR's PRIMARY `url=`
  (`defaultBridgeURL`), and the banner prints it ("Public mode — domain:",
  "Admin console:"). A hand edit `user:password@host` was published and
  printed whole (measured with the real binary: health, both QR fields and
  both banner lines). Every other consumer wants a host too and matched
  nothing: the ACME whitelist (`autocert.HostWhitelist` runs IDNA, which
  refuses the `:` and the `/` such values carry, measured), the SNI route,
  and the console's Origin allowlist, which compares a browser's Origin
  hostname with the domain. So `Normalize` serves it as
  `config.AutocertHost`'s reading (no scheme, user information, port, path,
  query or fragment), warning once under its own message and naming the
  field by scheme and host (`urlFieldForLog`), never the value. **Repaired,
  never refused**: the value loaded before. **Refuse what is typed**:
  `bridge init --public --domain` refuses any value `AutocertHost` would
  change (exit 2, nothing written), and trims once and writes what it
  checked.
  **A value that names no host that can be read is served as
  `config.InvalidAutocertDomain`** (`autocert-domain.invalid`, RFC 6761),
  never blanked: `Validate` refuses an empty public domain, and the phones of
  such a bridge may still reach it through `customEndpoints`. None of those
  values ever worked (no certificate, no Origin match, a URL the app cannot
  parse), so the placeholder breaks nothing. **Three rules keep the
  reading clean.** An IP address, once a scheme, user information, a path,
  a query and a fragment are removed, is returned as written:
  `url.Parse("//2001:db8::1")` reads the host `2001:db8:` and the port 1,
  after `user@` too. **Never bracketed**: the Origin allowlist compares the
  domain with an Origin hostname, which has no brackets, so bracketing
  would lock the console out of a bridge whose unbracketed IPv6 domain
  passes it today; the URL built from that domain is invalid, which is
  backlog B152 (both spellings measured, a review's proposal). **Nothing that
  precedes an `@` is ever returned**: an `@` after the first `/`, `?` or `#`
  could as well end user information a hand edit left unescaped
  (`user:12/34@host` parses with the host `user`), so it names no host.
  And a value `url.Parse` cannot read (a password with a space) names none.
  **What it returns is a fixed point** (`Normalize` is idempotent, and init
  takes back what it stores; a bracketed IPv6 keeps its brackets and a zone
  its `%25`). Read in public mode and wherever `autocert.enabled` (serve
  starts the ACME manager in either posture and prints the domain); a
  loopback config with autocert off keeps its value, unwarned.
  **Don't strip at the publish sites** (the bullet above: they read the
  normalized value). The Gemini consult on the placeholder was refused by
  the API's spending cap; decided here.
  `TestAutocertHostReadsTheHostAValueNames` (a table, every shape, the
  fixed point),
  `TestNormalizeServesAutocertDomainAsItsHost`,
  `TestHealthPublishesNoAutocertDomainCredential`,
  `TestPublicPairingCarriesNoAutocertDomainCredential` and
  `TestServePublishesNoAutocertDomainCredential` (the real public serve:
  health, a minted link, the banner and every log line).
- **mDNS TXT records carry `host` + `port`.** Without them iOS must
  NWConnection-resolve the Bonjour service to a hostport, which is unreliable;
  the bare-hostname-plus-`.local` form matches the SRV target the cert SANs
  already cover. Never emit `host=.local` — fall back to `localhost.local`.
- **The Bonjour instance name is ONE DNS label, 63 bytes, and hashicorp/mdns
  does not check it** (#1046). The instance is the library name:
  `NewMDNSService` takes a longer one, `Advertise` succeeds, serve prints
  "mDNS: advertising as", and every answer to a browse then fails to pack
  (`dns: bad rdata`), so the bridge is never discovered; the one trace is an
  INFO line per query ("[ERR] mdns: Failed to handle query"). 64 ASCII
  characters, or 22 CJK ones, were enough (measured on a Linux LAN).
  `sanitizeInstance` cuts it to `maxInstanceLen` bytes on a rune boundary;
  TXT `library=` carries up to 240 bytes of the name. **The instance and
  every TXT string are DNS presentation format to miekg/dns**, where a
  backslash escapes what follows it (`\X` is X, `\DDD` a byte): each is
  written `\\` AFTER the cut or cap, which count the bytes on the wire.
  Unescaped, `AC\DC Live` was browsed as instance and `library=` "ACDC
  Live", and a trailing backslash (a name's own, or one a cut left) merged
  the instance into the service label or vanished from the TXT (CodeRabbit
  reported the cut case; measuring it found the rest). **A browse that finds
  nothing is a finding only once a short-named control is found**: on the
  dev Mac the responder bound a link-local `utun0` tunnel ahead of `en0`, so
  even "Probe Short Name" was invisible there, until the picker learned to
  prefer a real LAN (the selection bullet under DLNA, UPnP and discovery,
  2026-09-27).
- **A shape documented in PROTOCOL.md is a Mirror-PR obligation.** The two specs
  are byte-identical by rule, and both directions are guarded
  (`TestEveryDocumentedEndpointIsRouted` / `TestEveryRoutedEndpointIsDocumented`).
  A mention in running prose is NOT a contract — the guard accepts only a `### `
  heading or a bold `METHOD /path` lead-in, because prose-mention is the state
  six live endpoints were already in.
- **`DELETE /v1/atlas-harvest/credential` forgets the harvest credential,
  and a demo bridge refuses it** (#1049). Switching the app's library
  harvest off stopped only the app's renewals: the `bulk_harvest`
  credential the bridge held stayed usable until it expired (the audit's
  H3). The route calls the store's `Clear()`, which drops the token and its
  expiry and KEEPS the sync position (a re-provision resumes), answers 204
  whether or not one was held, and draws from the write bucket. **The demo
  answers 403 `demo_read_only`**: its one credential is shared by every
  demo user, so one of them switching off must not stop the harvest for
  all, and the POST's accepted residual (a public bearer can overwrite the
  token) is not widened into a public off switch, whatever the harvest
  setting. **A bridge with the harvest OFF clears the file too, and answers
  204**: no store is open there, so `serve` wires
  `atlasharvest.ClearStoredCredential` instead of the sink. This bullet said
  "a credential file it may still hold is left alone, since nothing there
  reads it" until CodeRabbit (on the app's #1981) caught the premise:
  re-enabling the harvest reads that file again, so a 404 told the app
  nothing was held while a credential waited to come back into use. **`204`
  is the only answer that means revoked**; the app reports anything else,
  405 from an older bridge included, as not revoked.
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
- **A stamp a shutdown stopped records nothing**: no `mark skipped` count, no
  `enrichment skipped` line, and the row stays `enriched_at = 0`. The stop is
  at `stampEnriched` / `markSkipped`, never at the fetches that absorb their
  own errors. The rule is under **The CLI and the serve wiring**. (#1001)
  **Nor does a stamp over a row that changed since the batch read it**
  (`manifest.ErrTrackChanged`: it writes nothing, and the next batch reads
  the row again; the compare-and-set bullet under **Scanner**, B187).
- **Pacing derives from the client's base URL** (`minIntervalForBase`,
  fail-safe to the public interval, dot-anchored suffix match) — public MB is
  1.1s and self-hosted is 150ms — **not zero**, because Atlas's own per-IP tier
  gate is shared with the harvest client. The invariant is enforced by
  construction rather than by a code path remembering; don't put a fixed interval
  back in `NewEnricher`. `Enricher.Run` needs its
  own inter-batch pause too: the pacer only fires when a network call is made.
- **A configured base URL's user information travels in a header, never in a
  request URL** (backlog B69). `enrich.musicbrainzBaseURL` and
  `coverArtBaseURL` may carry a mirror's credential (`https://user:password@mirror/ws/2`,
  or a token written as the user name), and net/http both sends it as Basic
  auth and names the request URL in every error it returns, through a
  `stripPassword` that masks a PASSWORD and nothing else. So a token written
  as the user name reached the journal whole on every transport failure:
  the `MB search`, `MB artist search`, `release-group lookup` and `artwork`
  Error lines, the `enrichment skipped` line's `detail`, and the premium
  fetch's Warn (measured over a mirror whose certificate this process does
  not trust, a persistent failure, and over a refused connect). The skip
  REASON was never affected: `markSkipped` counts one of the fixed
  `skipReason*` keys and the error rides only that log line, so "no stored
  skip reason" is about the line, not a column. No RoundTripper can stop it,
  because the client builds the error from the request's own URL; so
  `baseEndpoint` (baseauth.go) cuts a base in two, the ROOT every request URL
  is built from and the user information, which `newRequest` sends with
  `SetBasicAuth` (the user alone as `user:`), byte for byte the header
  net/http built from the URL: `TestABaseURLsCredentialReachesTheMirrorAsBasicAuth`
  takes its reference from net/http on every run. Five rules keep it whole.
  **Every request built from a configured base goes through `newRequest`**,
  and `TestEveryRequestThisPackageBuildsComesFromAListedBuilder` lists every
  function in the package that calls `http.NewRequest*` (or the package-level
  `Get`, `Post`…), so the next client cannot skip it: a builder is listed
  with the reason its URL is a public constant, or is not built. **A request
  resolves its base ONCE and carries the credential with the root**
  (`resolveBase` returns the `baseEndpoint`; never keep the root alone): a
  live base can change between two reads, and a credential read apart from the
  URL sends one mirror's to another's host
  (`TestALiveBaseThatChangesBetweenReadsNeverPairsACredentialWithAnotherHost`,
  whose provider answers a different mirror on every call). **A base that is
  not an absolute http(s) URL with a host is an error naming none of it**,
  never a fallback and never net/url's own `parse "…"` error, which quotes the
  value with no mask at all; config refuses such a value
  (`normalizeBaseURL`), so this is a backstop. **The root ends in no
  slash**: `newRequest` joins a path that begins with one, so the parser trims
  a base's trailing slashes (config, a live value and the premium fetch's
  stored base were trimmed already; a base handed straight to a constructor
  was not, and requested `/ws/2//release/…`,
  `TestABaseWithTrailingSlashesRequestsNoDoubleSlashPath`). **The header
  follows a redirect by net/http's rule for an explicit Authorization
  header, and only while the scheme stays https**: to the same host and its
  subdomains, never to another domain
  (`TestABaseURLsCredentialFollowsARedirectOnlyWhereNetHTTPSendsAnAuthorizationHeader`),
  where the URL form followed a relative Location only. That rule compares
  host names and not the scheme, so it also carried the header from an https
  request onto a plain-http hop on the same host, in cleartext, where the URL
  form's absolute Location carried none. `guardRedirects` (baseauth.go, a
  delegate to `authredirect.Guard` since backlog B133) is the
  `CheckRedirect` of every client that sends a credential: it strips the
  header from a hop that is not https when the request began on https, judged
  per hop, because net/http copies the header from the FIRST request onto
  every hop and a cleartext hop's own redirect would have it back. **Strip,
  never refuse**: a request with no credential follows a downgrade as it
  always has, and a mirror that insists on the credential at the plain hop
  answers with the 401 it always did, where a refusal is an error
  `IsTransient` does not know, stamped persistent all the same. The guard
  keeps a caller's own policy and net/http's limit of ten
  (a client that sets a policy loses that default), and copies the caller's
  client (`TestNoCredentialFollowsARedirectFromHTTPSToACleartextHop`,
  `TestARequestWithNoCredentialStillFollowsARedirectToPlainHTTP` and
  `TestTheCredentialGuardKeepsACallersRedirectPolicy`, each over all three
  clients). A constructor of a client that calls `newRequest` builds it over
  the guard, and
  `TestEveryClientThatSendsACredentialIsBuiltWithTheRedirectGuard` lists them.
  The Atlas premium
  cover fetch builds its request the same way and sends the bearer token
  alone, which the guard withholds from a hop that leaves https as well: its
  stored base carries no user information (`baseurl.CredentialBase` refuses
  it when it is provisioned, and the harvest state store drops one a hand
  edit left in its file: the next bullet), and the fetch goes through the
  parser all the same, for any other credential source. iTunes and Deezer
  take no operator URL.
  `TestNoRequestErrorNamesABaseURLsCredential` drives every request either
  client makes against three ways for a mirror to fail and five ways to write a
  credential, and `TestNoLogLineOrSkipDetailCarriesABaseURLsCredential` runs
  the real enricher and searches every line it logs, at every level, without
  regard to case.
- **…and the harvest credential's stored base is `scheme://host` or
  nothing, decided at the STORE** (backlog B97, and the stored half of B49).
  `atlasharvest.StateStore` kept whatever `atlas-harvest.json` held, and the
  harvest client's submit and poll, the booklet check and fetch, and the
  lyrics tier build every request URL from it. Measured on main with the
  real `serve` over a hand-edited file: `WARN atlasharvest.tick_error
  phase=poll error="Get \"https://s3cret-Pw@127.0.0.1:1/v1/atlas/harvest/results?…\":
  … connection refused"`, a token written as the user name, whole, on every
  tick. A path, a query or a fragment reached the same errors; a base
  written without a scheme (`user:pw@host`) failed `unsupported protocol
  scheme` with the URL quoted whole and lowercased (search without regard to
  case); plain http sent `Authorization: Bearer <token>` in the clear; and a
  port with no host, stored before #1074's check, was dialled on this
  machine (a TCP connect; the TLS handshake then fails for want of a server
  name, so the token never left). **Fix it at the store, never per
  consumer**: #1091's `parseBaseEndpoint` covered the premium fetch alone,
  and two readers of one stored value disagreed about what they tolerate.
  `OpenStateStore` reduces a loaded base with `baseurl.CredentialBase`, the
  reduction `POST /v1/atlas-harvest/credential` stores; a base with no such
  form is DROPPED with the credential held against it (token and expiry;
  the sync position stays, as `Clear` keeps it), the drop is WRITTEN BACK so
  neither stays on disk (a harvest-off revoke must not answer 204 over a
  credential still in the file), and one `atlasharvest.state.base_refused`
  Warn names the file and no part of the value. A write-back that fails
  fails the open, and `serve` runs without the harvest, as for any state
  file it cannot open. `SetCredential` refuses such a base with an error
  naming none of it, BEFORE it touches anything (a changed base resets the
  cursor). A base that reduces (a trailing slash, `:443` or an empty port,
  an uppercase scheme, space) is held reduced and keeps its credential.
  **One reduction,
  in `internal/baseurl`**: `CanonicalHTTPS` (the pin's), `NamesHost` and
  `CredentialBase` (the canonical form, when it names a host and any port it
  names is 1-65535: `url.Parse` checks a port's digits, not its range, so
  `https://atlas.example:99999` was stored and failed every dial), imported
  by config, the handler and the store, since `internal/atlasharvest`
  imports neither config nor enrich; `config.CanonicalHTTPSBase` and
  `BaseURLNamesHost` are gone, so there is no second copy to drift. **Keep
  the host and port tests in `CredentialBase`, out of `CanonicalHTTPS`**:
  the pin goes through the latter, `Validate` refuses a pin that reduces to
  "", and a config that loaded must keep loading (B36's reasoning); such a
  pin keeps its canonical form and matches no credential. **The
  reduction is a fixed point** (`TestTheReductions` reduces every answer
  again): the pin is reduced twice, by config and again by
  `WithAtlasHarvest`, and the store reduces at every open. `https://:443`
  reduced to `https://` and then to "", so a pin written that way left a
  non-demo bridge UNPINNED (measured with main's binary: a paired device's
  credential for `https://attacker.example` answered 200 and was stored);
  it stays itself now and matches nothing, as a port and no host should.
  **The harvest tests' fake Atlases are TLS servers** (`httptest.NewTLSServer`,
  the client given `srv.Client()`), since the store holds an https base or
  none: one that seeds an http base fails at `SetCredential`, and one that
  ignored that error would pass having shown nothing
  (`TestClientTokenRejectedClearsCredential` did, `_ = state.SetCredential`).
  `TestAStoredBaseThatIsNotSchemeAndHostIsNeverUsed` (eleven shapes, each
  searched for the secret without regard to case in every line at every
  level, and a positive control over a base in the stored form that must
  connect and log),
  `TestSetCredentialRefusesABaseThatIsNotSchemeAndHost`,
  `TestTheStoreHoldsABaseInItsCanonicalForm`,
  `TestARevokeLeavesNoStoredBaseBehind`,
  `TestOpeningAStoreThatCannotDropItsBaseFails` and
  `TestAPinOfAPortAndNoHostStaysAPinAsServeWiresIt`.
- **…and the harvest client is built over the enrich clients' redirect guard,
  `authredirect.Guard`** (backlog B133, found by the B97 session). Every
  harvest request (submit, poll, the booklet check and PDF, the lyrics tier)
  sets its bearer token as an explicit Authorization header, and
  `defaultHarvestHTTPClient` had no `CheckRedirect`, so an https Atlas
  answering with a redirect to http on its own host had the token sent in
  the clear: #1091's round-2 finding, fixed then for the enrich clients
  alone, because their guard lived in `internal/enrich`, which
  `internal/atlasharvest` must not import (measured: a TLS fake Atlas 307ing
  to a plain server on 127.0.0.1, and the plain hop saw `Bearer <token>` on
  all three legs tested). The guard moved to `internal/authredirect`;
  enrich's `guardRedirects` is a delegate, so its population test
  (`TestEveryClientThatSendsACredentialIsBuiltWithTheRedirectGuard`) still
  finds each constructor's call by name, and `Client.httpClient` builds the
  default client and any client a caller hands in over the guard (a copy,
  never the caller's own). **A client that sends a credential is built over
  this guard, whatever package it lives in**: the population test sees
  enrich alone. `TestNoHarvestRequestCarriesItsTokenOntoACleartextHop`,
  `TestTheDefaultHarvestClientDropsTheTokenLeavingHTTPS`, and
  `internal/authredirect`'s own two tests. B97's log entry named B133 and no
  more while it was unfixed (the rule in the backlog section above).
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
  ReadDir+hash per album under contention) and reset per scan; a reset alone
  never brought a new cover to tracks already indexed, which the folder-art
  key does (the B141 bullet under Scanner). The disc-subfolder
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
  `optimized-dsd-v2-*` under the flag) and `bridge render` (DSD →
  `pcm-v2-*-24`). `runUpscaleParams.dsdCaps` is probed once per run and its
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
- **The album-level gain's parts (2026-09-27, #1053; switched on as the DSD
  `v2` schema by the PR after it; `ops/plan-2026-09-27-dsd-album-gain.md`).**
  Stage B's guard decides each track's boost alone, so an album's tracks shift
  against each other (Phase 0, on the operator's library: 57 % of DSD albums
  by more than 1 dB). The album gain gives every track the boost its album's
  hottest track allows.
  - **`dsd_peaks` (v47)** holds one true peak at unity per (track,
    `DSDPeakProfile` = recipe|tier|rate|rateFlag). NULL means silent and a
    missing row means never measured. A peak is fresh only while its mtime and
    size match the track row, so **never read one without that join**
    (`FreshDSDPeaks`).
  - Three writers fill it: every render, through `UpsertVariant`
    (`VariantRow.PeakProfile`, in the same transaction); the survey's
    `UpsertDSDPeak`; and v47's one-time seed from `track_variants.true_peak_dbtp`.
    The seed spells the profile in SQL, so **don't change `DSDPeakProfileFor`
    without the seed**. Both pin `"a1|compact|44100|-v"`.
  - **`DSDPeakRecipe` is not `DSDRenditionSchemaVersion`.** Bump the recipe
    only when Stage A changes what it produces, which makes every stored peak
    stale. A gain-policy change keeps them all.
  - **`MeasureDSDPeak` is the render's own Stages A and B** (the shared
    `decodeAndMeasure`), so a measured peak equals the render's exactly
    (`TestMeasureDSDPeakMatchesTheRender`). **Don't fork a second decode path
    for measuring.**
  - `JobSpec.AlbumGain` is injected by the POOL when the job runs, never at an
    enqueue site. The pool also widens the job's deadline by `SurveyBudget`.
  - Stage C applies the album figure **never above the track's own guard**
    (`albumBoundedGain`), so a stale figure cannot make a file clip.
  - **Claims cannot deadlock, by construction.** A render claims its own track
    before Stage A and resolves the claim on EVERY exit (a deferred error
    covers early failures). A survey claims an album-mate while it measures
    it, and releases it on every exit too, a panic included: `measureClaimed`
    defers the release as its first statement (2026-09-28). Released only by
    plain calls after the measurement, a panic in the decode left the claim
    registered, and because `transcode.Pool.processJob` recovers a runner
    panic and keeps the worker, every later render of that album waited on it
    until its own deadline, until a restart
    (`TestAMateWhoseMeasurementPanicsReleasesItsClaim`: 2 s of waiting, then
    `context deadline exceeded`, before the defer). So a claim is only ever
    held by work that is decoding, and nothing waits while holding one.
    **Don't make a render wait before resolving its own claim, don't hold a
    survey claim across a wait, and don't release a claim anywhere but on
    every exit.**
  - A waiter whose claim resolved without a peak measures that mate itself,
    because the holder's failure may have been transient. Only the waiter's OWN
    failed measurement leaves the mate out of the album.
  - **A survey reads the store again after it takes a claim, and before it
    measures.** Its pass's read can be minutes old by then, because it
    measures mates one after another, and another survey may have measured,
    recorded and released that mate in between. A measurement records before
    it releases, so the second read sees it. **Don't drop the re-read:**
    without it the mate is decoded twice (`TestASurveyRereadsThePeakUnderItsClaim`).
  - **Membership is the admin catalog's album identity** over served, local
    DSD rows. `StreamDSDCatalogRefs` shares `StreamCatalogRefs`'s scan, and
    `TestIndexGroupsLikeTheAdminCatalog` pins the grouping against
    `librarycat`. **Don't group by folder.** Routed rows and SACD virtual
    tracks never count: neither is rendered here.
  - **The switch-on is the DSD schema `v2`, never a re-render under `v1`.** A
    phone applies a rendition's `appliedGainDB` to the bytes of its
    downloaded copy, looked up by id, so re-rendering an id in place gives old
    bytes the new gain. v2 mints new ids, and **a `v1` row stays while its file
    exists** (nothing reaps it). **Don't add a reaper until the app records the
    gain with its downloaded copy, plus a grace period.** `variantsAggSQL`
    lists a track's renditions newest first (`created_at DESC, variant_id
    DESC`), because iOS takes the first prefix match
    (`TestVariantsListTheNewestRenditionFirst`).
  - **A DSD source's coverage needs a fresh row of the CURRENT DSD schema**
    (`manifest.DSDRenditionSchemaVersion`, the mirror
    `TestManifestMirrorsTheDSDRenditionSchema` pins); a PCM source's stays
    version-agnostic. A phone never requests a family it holds, so the bridge
    drives the move. The sweeper's compact pass moves by that coverage rule.
    `drainSupersededPCMRenditions` moves the faithful tier after it, under the
    same cap and disk budgets, and **only for tracks that already hold a
    `pcm-` row** (never a new 5 GB-an-hour tier). The card's "remaining"
    counts both. Without auto-optimize, `bridge optimize` / `bridge render`
    move them: their skip check is the exact current id.
  - **Wiring.** Serve: `wireAlbumGain` (`albumgain.New` with
    `upscaleEnqueuerAdapter.albumMateSpec`, then `Pool.SetAlbumGainer`), and
    `Invalidate()` in the post-scan hook. CLI: `newCLIAlbumGainer` /
    `cliAlbumMateSpec`, the run's own classifier with `--filter` and the resume
    check lifted, because a mate outside the filter still bounds the album.
    Both build a mate's spec the way a render of it would be built, with the
    source facts from the TRACK ROW, which a peak's freshness is judged
    against. `TestAlbumGainEndToEnd_RealToolchain` (serve) and
    `TestRenderCLIAlbumGainEndToEnd_RealToolchain` (CLI) render one album of two
    real tones and check the published files keep the source's 9 dB between them.

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
  path in any job's argv needs a pass here in the same PR. **And every exit's
  message goes through it, not only sox's** (#1055): the fsync exit put the
  sidecar's absolute path on the batch row `GET /v1/upscale/batches` serves,
  and the store exit, a recovered panic and the timeout warning (ffmpeg's
  stderr) could do the same. A new exit that builds a message from an error
  redacts it here. **And a bare directory is a path too**: a root-level
  source's sidecar sits directly in the variants directory, so a failed
  parent-directory fsync names that directory with no separator after it,
  which the prefix strip cannot see (CodeRabbit on #1055). Pass 2c turns
  each bare directory (the render scratch, tempDir, the variants
  directory) into a placeholder, longest first, because the variants
  directory and tempDir can nest either way or share a string prefix.
  **And a job's error logged outside the pool takes the same redaction**:
  the album survey logs an album-mate it could not measure, and
  `MeasureDSDPeak`'s error names the mate's absolute path and the scratch
  under the tempDir, so `albumgain`'s `measure` returns it through
  `JobSpec.RedactError`, by the mate's own spec (2026-09-28).
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
- **…and the TRANSCODE pool strikes a file only for what a tool said about
  it: a tool the host lacks strikes nothing** (2026-09-28). `processJob`
  struck the source for every runner error but a timeout and a shutdown, a
  missing sox included, and three strikes on one (size, mtime) suppress the
  file from every candidate query for 30 days, keyed on what installing sox
  does not change. The live upscale gate refuses NEW work within its 30 s
  probe, and that is all it can do: a job already queued (up to 5,000 a lane)
  or queued inside the probe window still reached the runner. Measured with the
  real `serve`: sox removed after the gate probed it, three `POST /v1/upscale`
  of a 3-track album failed 9 jobs, logged 9 identical WARNs, suppressed all 3
  files, and with sox back the batch over the album found `totalFiles: 0`.
  `unavailableTool` (internal/transcode/tool_unavailable.go) classifies at the
  site where the fact is known, by TYPE, never by message: an `*exec.Error`
  (exec's PATH lookup), a fork/exec `*fs.PathError` (the OS could not start
  the tool: gone since the lookup, a missing interpreter, the wrong
  architecture), and `Run`'s own route mark (`markToolUnavailable`) where the
  decoder probe found no route on this host's toolchain: a `.dsf` / `.dff`
  whose ffmpeg is missing, lacks the `dsd_*` decoders or has an unreadable
  listing, and an MP4 source sox has no reader for with ffmpeg or ffprobe
  missing. **A DSD-flagged row under another extension keeps its strike**: the
  same `ErrDSDDecodeUnavailable`, but a fact about the row. Such a job is still
  counted and announced (its batch row and `jobFailed` event say what
  happened), and #988's ordered tail is unchanged; the exit is a row in both
  terminal-order tables. **The default is the opposite of the analysis
  pool's**, which strikes only on a classified verdict: this debounce predates
  classification, so everything not classified as the host's (or, since
  B53, as a source newer than its row, `ErrSourceChanged`: the B53 bullet
  below) still strikes, a tool that ran and refused the file included,
  which is what it is for. **The
  outage is reported per TOOL, once**: one Warn when it starts, the jobs after
  it at Debug under the same message, one Info when a job whose chain ran the
  tool succeeds (read from the settings' `decoder`, the route the run took),
  and a re-Warn after 24 h of silence. Per tool, because a FLAC success ran
  sox alone: ending every outage on any success reported ffmpeg back, then
  missing again, once per DSD job. This is not the analysis pool's
  per-FILE dedup the bullet above warns against: the key is the tool, so the
  alarm stays up until the tool is proven back. **v48 expired every
  suppression once**, as the TTL would (`variant_fail_at = 0`, the count
  kept): the strike records carry no reason, so a missing tool's cannot be
  told from the rest; a still-broken file costs one more attempt and is
  suppressed again. Measured on main's struck v47 database: the branch
  binary's first batch converted all 3. It leans on #1067 for the sweeper:
  with no strike, only the gate keeps the sweeper from re-offering the
  backlog to a host without sox (each such job fails in microseconds at exec).
  `TestAJobThatCannotRunItsToolStrikesNoSource` (the real pool and runner on an
  empty PATH), `TestAToolThatRanAndRefusedTheFileStillStrikesIt` (the positive
  control), `TestAToolOutageIsReportedWhenItStartsAndWhenAJobProvesItBack`,
  `TestMigration48ExpiresEverySuppressionOnceAndKeepsTheCount`.
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
- **…and EVERY rendition writer stamps the row, and the on-demand path, the
  sweeper and the CLI queue a render only while the file still matches it**
  (#1077, backlog B24). A rendition records ONE
  version (`source_mtime_ns` / `source_size`, and a DSD one's peak in
  `dsd_peaks`) and two clocks judge it: the sweep's candidate queries and
  `FreshDSDPeaks` against the row, the serve path against the file on disk (a
  sidecar of other bytes must never be served). Between a change to a file and
  the scan that reads it no stamp satisfies both. The on-demand path (POST
  /v1/upscale) and the CLI stamped a live stat while the sweeper, the batch
  coordinator and the album survey stamped the row, so each re-rendered what
  the other wrote: measured on a real bridge (`autoOptimize.intervalSec: 20`),
  three rounds of a request for three renditions and a sweep made 18 renders,
  a delta for both tracks on every step, and the downloads answered 200 after
  each request and 410 after each sweep; with the fix, 3 renders, all 200.
  With the defaults the sweep's periodic tick fires milliseconds after each
  periodic scan STARTS, on the previous scan's rows, so the flip came once per
  scan interval; the faithful tier took part through
  `drainSupersededPCMRenditions` (a live-stamped `pcm-v2` row is not "fresh"),
  and a live-stamped peak was re-decoded by every album-mate render. The phone
  asks for a family only while it lists none of it (PlayerService tier 0,
  `shouldAutoGenerateVariant`, `BridgeRenditionRequestGate`; what it does
  after a 410 is in the B82 bullet two down), so one of its requests cost 3
  renders and up to a scan interval of 410; the CLI, a folder POST or a
  script repeat it. `transcode.SourceIsAtRow` is the one check
  (`sourceIsAtRow` in cmd/bridge until B53), the scanner's
  EXACT skip-gate comparison, never serve's 2 s tolerance: the on-demand path
  refuses (`errSourceAheadOfRow`, counted `rejected`, no wire change) and
  queues a rescan of the file's directory (`sourceRescanner`, bgWriters-joined),
  so the next request renders; the sweeper counts `changedSinceScan` and passes
  over; the CLI lists the file as needing no run, `--force` included. **The
  rescanner's queue is its pending set**: a directory is queued once at a time
  (and again once its scan has started), drained oldest first, up to 1,024.
  Until review round 1 it was a 64-slot channel that dropped the 65th, and a
  dropped directory was rescanned only if a later request named a file in it,
  so a client that asked once (a folder POST over more album directories, or
  plays piling up while a full scan holds the scanner's lock) waited for the
  periodic scan. The bound is also the work one burst queues: a rescan that
  re-reads a changed file runs the whole-library duplicate restamp (1.1 s over
  50,012 rows on the dev Mac, 7 ms for an album with nothing changed). **Don't stamp a
  live stat anywhere** (`FreshnessFromFile` is gone), **and don't render a file
  its row no longer describes**: a row-stamped render of new bytes is the one
  the serve path refuses, and with auto-optimize off (the default) nothing
  rendered it again until B82 (two bullets down). The album survey still MEASURES a
  changed mate (a peak describes the bytes on disk; `cliAlbumMateSpec` ignores
  needsRun). Until B53 (the next bullet) this bullet also said the batch
  coordinator does not check and that nothing checks when the pool starts a
  job; both were true then and are not now. `TestAChangedFileIsNotRenderedUntilItsRowIsReRead`
  drives the loop through the real handler, sweeper and download path, with
  `committingQueue` committing each job as `Pool.processJob` does (the adapter
  takes its pool through `renditionQueue` for that).
- **…and the batch walks check too, `transcode.Run` checks again when it
  starts and before it publishes, and a stale download asks for the rescan**
  (backlog B53). Measured on main at 6dfba62c: a batch over a changed file
  rendered the new bytes under the row's stamp (410 `variant_stale`), and a
  batch after the scan counted the track covered (a projection's `HasVariant`
  is ANY rendition of the family) and rendered nothing, so it stayed 410; a
  job whose file changed while it waited in the queue, or while it rendered,
  succeeded with a rendition the serve path refused; and five downloads of a
  pre-generated rendition after a retag answered 410 while nothing rescanned
  and the sweep passed the file over (`changedSinceScan`) until the periodic
  scan. Now the walks compare the resolver's stat (`ResolverFunc` returns
  `ResolveChecked`'s triple, the stat production already took) with each
  projection, pass a changed file over into the row's skipped count and ask
  for its rescan. `Run` answers `ErrSourceChanged` FIRST (before a decoder
  probe, a scratch file or an album-gain claim) and again in
  `JobSpec.publishSidecar`, the one publish helper both chains call, so bytes
  the stamp does not describe are never renamed into place. A source that
  is no longer there, or whose stat fails at all (a stale NFS handle, a FUSE
  mount's ENOTCONN, EIO), answers it too (review rounds 5 and 6): rendered
  on, the tool failed on the input and the pool struck the file, which a
  mount dropping under a queued batch did to every file behind it. Every
  enqueuer stats before it queues, so a file that stays unreadable is refused
  at its next enqueue rather than re-offered to `Run`. The pool
  classifies it BY TYPE: counted and announced like any failure (a batch must
  hear it to drain; #988's tail is unchanged, and the exit is a row in both
  terminal-order tables), it **strikes nothing** (a newer version is not a bad
  file; three strikes would suppress a good one for 30 days), and it asks for
  the rescan inside the claim. The CLI worker renders through `Run`, so a file
  retagged during a long `bridge render` fails with the change named. A 410
  `variant_stale` tells `api.StaleRenditionFunc`, and its hook
  (`staleRenditionRescan` until B82, `staleRenditionHeal` since) asks for a
  rescan only while the ROW is behind the file (once a scan read the change a
  rescan changes nothing) and once per directory per minute, bounded at 1,024
  directories: every play, device and range request makes that GET. The
  minute is spent only on a rescan the rescanner queued or already had
  waiting (`sourceRescanner.queue`); one its full queue dropped leaves the
  next GET free to ask (review round 2). EVERY
  rescan the shutdown did not interrupt drops the album-gain index, queues
  the renders stale downloads waited for (B82, the next bullet), and THEN
  nudges the auto-optimize sweep, whose tick otherwise follows the periodic
  scan (`afterRescan`): only a full scan dropped that index, so a DSD render
  the nudge starts could take a retagged track's old album-mates for its gain
  (review round 3), and not only a rescan that committed rows, since one whose
  file was deleted before it ran deletes a row and counts none (round 4).
  **A rescan request names the file by its ROW's path, and
  the rescanner resolves the directory itself**: the scanner makes each row's
  path from the spelling of the directory it is handed, and on main a request
  naming a changed file in lower case, on a filesystem that opens it, left the
  rows `Fixture/DSD/01.dsf` and `fixture/dsd/01.dsf`. **Still open**: a
  watcher-driven subtree scan nudges neither the sweep nor the player's
  catalog, nor drops the album-gain index (B83; `player_wiring.go` said it
  nudged the catalog until B53). This said a stale rendition whose row has
  caught up is rendered again by nothing (B82) until the next bullet. Tests:
  `TestABatchPassesOverAFileThatChangedSinceItsScan`,
  `TestARescanDropsTheAlbumIndexBeforeItNudgesTheSweep`,
  `TestEveryRescanRunsItsAfterStepHoweverFewRowsItWrote`,
  `TestRunRendersNothingFromASourceThatChangedSinceItsStamp`,
  `TestPublishingRefusesASourceThatChangedWhileItRendered`,
  `TestAJobWhoseSourceChangedIsNotRenderedAndStrikesNothing` (the real pool and
  `Run` over a stand-in sox), `TestTheCLIRendersNothingFromAFileThatChangedDuringItsRun`,
  `TestAStaleDownloadRescansItsSourceAndRendersItAgain` (renamed by B82),
  `TestAStaleDownloadAsksForARescanOnlyWhileItsRowIsBehindAndOncePerMinute`,
  `TestARescanIndexesNoSecondSpellingOfTheDirectory`.
- **…and a download that finds a rendition stale has it rendered again; a
  batch and the coverage bars still count a stale rendition as covered**
  (backlog B82). Measured on main at 6bc4605a: with auto-optimize off (the
  default), renditions made on request before a retag answered 410
  `variant_stale` once a scan read the change and nothing rendered them
  again: the stale downloads rendered nothing, and a batch counts a track
  with ANY rendition of the family covered (`TrackProjection.HasVariant`).
  On a real `bridge serve` with sox, after a retag and a scan the batch
  answered `enqueuedCount: 0, alreadyCovered: 1` and 15 downloads over 30 s
  all answered 410; with the fix the second download was served (the render
  took under 2 s), and so it was after a second retag with no scan.
  What the app does after a 410 (read in the iOS source, 2026-09-29): on a
  PLAYBACK 410 it drops the id from its local row, in memory and persisted,
  and retries; the retry asks for the family again only under CarPlay or
  cellular routing of a PCM source (the Tier 0 lazy POST) or where a DSD
  source's route wants a rendition (`BridgeRenditionRequestGate`); an offline
  download keeps the id and fails, the upscaled toggle and a picked rendition
  never ask, and every manifest delta for the track lists the stale id again.
  So "the phone never asks again for a family the manifest lists", B82's
  premise and this file's until now, holds only while the app still lists
  it. Now `staleRenditionHeal` (B53's hook,
  renamed) asks for the render at once when the row is current, and when the
  file is ahead of its row it keeps the render until the rescan it asks for:
  the rescanner's `after` step takes the directory it read, and `afterRescan`
  drops the album-gain index, queues the waiting renders whose rows the
  rescan brought level (`rescanned`; one still behind keeps waiting for a
  later rescan, at most an hour from the first ask, and one whose row is
  gone is dropped), and THEN nudges the sweep, so a DSD render never takes a
  stale album index. `staleRerender.rerender`
  reads the kind off the id's prefix (`renditionKindOf`; `optimized-` covers
  the DSD compact tier), and refuses unless the kind's LIVE gate is open
  (`renditionGates`: the very closures `/v1` reads, `dsdRenderActiveFn` among
  them, pinned by `TestAStaleDownloadRendersUnderTheV1KindGates`), never on a
  demo bridge (its POST `/v1/upscale` answers 403: every bearer there is
  public), never for a suppressed file (`Store.VariantFailureSuppressed`, the
  one-row form of `variantFailureSuppressedSQL`), and only then calls the
  adapter's entry point for the kind (`enqueueKind`), which renders what a
  request for the family would: the family's CURRENT id, stamped from the row
  (a DSD `v1` rendition is rendered as the `v2` one, which the app, taking the
  newest of a family, plays; the `v1` row stays, and its later downloads
  render nothing). **On the BACKGROUND lane** (`JobSpec.Background`), the
  sweep's: nobody waits on it, since the download that asked has already
  played the source, and on the foreground lane a library retagged at once
  would queue its renditions ahead of a request a client does wait on (a
  CarPlay plug-in). That is also what bounds a token holder's GETs, which
  pass no write bucket: they can queue at most one render per stale
  rendition and file version, all behind the waiting work (CodeRabbit's
  review of #1097). **At most once per rendition AND VERSION of the file per
  minute**: the minute bounds the tries of a render that fails (a failure
  writes no row), and a file retagged again is a render not yet tried.
  Keyed without the version, on a real bridge a second retag 30 s after the
  first answered 410 on 15 downloads over 30 s: the render its rescan queued
  was refused as asked for within the minute. The minute is not spent when
  nothing was tried (the pool's queue full, the kind off), and a file that
  changed again before the enqueue (`errSourceAheadOfRow`) waits for a
  rescan: the wait is recorded, THEN the heal asks for the rescan (folded
  into the one the enqueue asked for), since the enqueue's request comes
  first and a quick rescan could otherwise go by before the wait exists.
  **A rescan that brought every waiting file
  level frees its directory's rescan minute** (B53's), so the next change
  asks at once; one that left a file behind keeps it, which is the minute's
  job (a file still being written, a directory the scan cannot read). At
  most 1,024 renders wait, those older than an hour forgotten first.
  **No loop**: the render is stamped with the row, fresh to the serve path,
  the sweep and the album gain alike (#1077's one clock), and a served
  rendition calls no hook. **A batch and the Inspector's coverage bars were
  left counting a stale rendition as covered, on purpose**: making them
  freshness-aware is a product decision, backlog B100 (a whole-library
  projection 421 → 476 ms, `AllEligibleKinds` 122 → 133 ms, the root folder
  rollup 94 → 128 ms over 50,000 tracks; about six SQL sites plus
  `catalog_refs`). **Don't render from a download whose row is behind** (the
  render would record a version the serve path refuses), **and don't give
  the re-render gates of its own**: a copy is how a kind's sox half has
  drifted before. Residual: the render replaces a same-id rendition in place,
  so an offline copy of the old bytes on a phone takes the new
  `appliedGainDB` by id, as the auto-optimize sweep's in-place re-render
  already did. Tests: `TestAStaleRenditionIsRenderedAgainWhenADownloadFindsItsRowCurrent`,
  `TestAStaleDownloadWhoseRowIsBehindRendersAgainAfterItsRescan`,
  `TestAStaleDownloadRescansItsSourceAndRendersItAgain`,
  `TestAStaleDownloadRendersEveryNewVersionOfItsFileAgain`,
  `TestAStaleDownloadRendersNothingForAKindThatIsSwitchedOff`,
  `TestAStaleDownloadRendersNothingForAFileWhoseRendersKeepFailing`,
  `TestAStaleRenditionOfAnOlderSchemaIsRenderedAsTheCurrentOne`,
  `TestAStaleRerenderGoesThroughTheKindsGateAndTheSuppression`,
  `TestAStaleDownloadAsksForARenderOncePerMinuteAndWaitsForItsRescan`,
  `TestAtMostACapOfRendersWaitForARescan`,
  `TestARescanDropsTheAlbumIndexBeforeItNudgesTheSweep`,
  `TestAStaleDownloadRendersUnderTheV1KindGates`.
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
  no-op, and "empty" means nothing the reaper would remove, never "no entry at
  all" (2026-09-28): the background sweep asks its inventory
  (`emptyCatalogRefusal`), so a directory left holding empty folders, a
  `.DS_Store`, a Trash or the filesystem's `lost+found` is quiet, and
  `artwork --gc`'s empty-store guard reads the cache as its walk does. **The
  CLI `--gc` sweeps decide it the same way since 2026-09-29** (backlog B65),
  after the walk and ahead of the mass-orphan check (`gcRefuseEmptyCatalog`,
  which replaced `gcRefuseEmptyKnownSetOverPopulatedDir`, whose "any entry at
  all" read made empty folders or a `lost+found` need `--allow-empty`).
  **`integrity.EmptyCatalogOrphans` is the one rule of all three**: a file
  the sweep would remove BY ITS OWN Consider, or an entry the walk could not
  stat, weighed as one; never a directory it could not list (the partial
  walk's refusal) nor a scratch `.tmp`, which is removed whatever the catalog
  says. So `upscale --gc`, whose nil Consider removes every file, still
  refuses over a lone `.DS_Store` (the refusal names it), while `analyze --gc`
  passes over one. **Don't count only rendition-shaped files there while the
  sweep goes on removing every file**: the refusal would wave through a run
  that removes files it never weighed. The refusal names
  `--allow-mass-orphans` beside `--allow-empty` when the mass-orphan check
  would refuse the same run (ten files or more with no rows). A BACKGROUND sweeper gets no override (nobody is in the loop to
  express intent); the CLI ones take `--allow-empty`, and a sweep test pins that every
  `--gc` command offers it — which is how `artwork --gc` was found to have
  carried an un-escapable refusal since it was written.
- **Every walk of the artwork cache starts where the cache directory
  resolves, the size cap goes on over what it cannot list, and the GC keeps
  the thumbnails of an artist a track row names** (2026-09-29, backlog
  B64). The three walks (`artwork --gc`, its empty-store guard, the serve
  cap `sweepArtworkCache`) share `artworkWalkRoot`: `fsutil.ResolveLinks`,
  as `resolveSidecarRoot` resolves, removing by the WALKED path and
  reporting under the configured one (`artworkReportPath`). A cache
  directory that is a symlink or a Windows junction was one entry to all
  three: the GC removed nothing ("1 skipped") and the cap never evicted.
  **The guard and the GC resolve together, or not at all**: a guard that
  reads the link as one entry beside a GC that walks its target waves an
  empty store through over the whole cache
  (`TestArtworkGCEmptyStoreGuardResolvesALinkedCache`). A root that
  resolves to a file is refused (resolved, the GC would judge the target by
  its own name, and removed it in the control), and "" before anything
  resolves it. **The cap steps over a directory it cannot list, and a cache
  file it cannot stat**: it returned the first such error before evicting
  anything, so on a cache that is an ext4 volume's mount root it was never
  enforced and WARNed every 15 minutes. The filesystem's `lost+found` goes
  without a word (`IsFilesystemLostFound`); anything else is logged once
  when a streak begins, at most daily while it lasts, and once at Info when
  it ends. **Going on is sound only because the cap evicts oldest-first**:
  a pass that cannot see some files evicts a subset of what a pass over the
  whole cache would, never a file that pass would keep, and never evicts
  what it sees to make up for what it cannot
  (`TestSweepArtworkCacheEvictsOnlyWhatAWholeCachePassWould`), so its one
  error is staying over the cap, which it reports. That is the cap's
  argument, not a sweep's: a sidecar sweep decides from a ratio over the
  tree, which an unseen part can flip (`PartialWalkRefusal`). **The GC's
  keep set holds `manifest.ArtistThumbKey` of every `$.artistMBID`**
  (`artworkKeysInUse`), the key the enricher fetched the portrait under and
  the console files its thumbnails under; it held artwork keys alone and
  removed every artist thumbnail in `thumbs/`. From the store, never from
  the file name: keeping whatever is named like an artist's thumbnail keeps
  one whose artist no row names for ever. The empty-set refusal stays keyed
  on the ARTWORK keys (the covers are what it protects) and asks whether
  the walk would remove a file. A thumbnail under a 16-hex artworkVersion
  alias stays an orphan: the console resolves an alias before it derives
  anything (`TestAnArtworkAliasFilesItsThumbUnderTheResolvedKey`), so no
  build files or reads one. **A cover the cap evicts does not come back**:
  its tracks are enriched, so `/v1/artwork` answers the terminal 404
  `no_image` and nothing fetches it again, a `local-` cover included (the
  cap's docblock said a 202 and a re-enrichment until 2026-09-29; backlog
  B84).
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
  `func() string`. A root change also had to drop the sweeper's
  chunk-resume cursor, a position in ONE tree, until 2026-09-28, when the
  cursor went: every tick walks the whole tree (the "…and the background
  `OrphanSidecarSweeper`" bullet below). An empty answer is a
  refusal, taken before anything can resolve `""` to the working directory
  (the `ReapOrphans` bullet under Config; `WalkDir("")` itself only
  errors), and `TakeSidecarInventory`, the walker the sweep uses now,
  refuses it again. The Jobs chips gate on the INTERVAL, as the
  wiring does, not on `UpscaleStats()`, which is nil with upscale off while
  the watchers tick regardless. (#917) The orphan GC's chip also says when
  that sweep is refusing (2026-09-28, the Jobs-card bullet below), and the
  variant integrity chip when the watcher is (2026-09-29, the B131 bullet
  below).

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
  It resolves a Windows junction too since 2026-09-29
  (`fsutil.ResolveLinks`; the junction bullet under **Scanner**).
  **Both halves are load-bearing**: walk the RESOLVED root so it descends,
  REPORT under the configured one, because `KnownSidecarSet` keys on the
  recorded `sidecar_path` and on `CanonicalSidecarPath(variantsDir, …)`
  and neither is symlink-resolved — emitting resolved paths would miss
  every key and classify a healthy tree as orphans, which is worse than
  the bug. **And the UNLINK goes by the WALKED path** (2026-09-28): each
  listed file keeps the path the walk visited beside it
  (`OrphanWalkedPaths`, `ScratchWalkedPaths`), and all three deleting
  sweeps (the background one, `upscale --gc`, `analyze --gc`) remove
  that. Unlinked through the configured spelling, a link repointed
  between the walk and the unlinks sent them into a tree the walk never
  counted, past the mass-orphan refusal, whose verdict was taken over the
  first tree (CodeRabbit on #1063, which named the background sweep;
  `TestAnOrphanSweepUnlinksInTheTreeItWalked` repoints the link in that
  window). An inventory whose two lists do not pair up is REFUSED before
  anything is removed (`SidecarInventory.CheckPaired`, the one check all
  three sweeps make; Gemini on #1063): falling back to the listed
  spelling is the defect, and indexing past the shorter list is a panic,
  which in the background sweep takes `bridge serve` down with it. This
  sentence said the panic was deliberate until the same review.
  `TakeSidecarInventory` cannot return such an inventory; the check is
  for a later change that builds or trims one. A dangling root is ENOENT
  and takes the missing-root reading; neither error branch may fall back
  to the unresolved path, which IS the
  defect. A symlinked SUBDIRECTORY is skipped rather than classified
  (unlinking it takes a subtree's only reference), and a symlink that
  cannot be STATTED is counted `Unreadable` rather than classified — "not
  there" and "could not find out" are different questions and only the
  first is junk. `analyze --gc` was never exposed: its `Consider` requires
  `analyze.WaveformExt`, `.waveform.bin` (this bullet said `.1bwf`, an
  extension nothing writes, until 2026-09-28). (#959, #1063)
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
  the FILES). The summary is Warn when it deleted, Info otherwise, a
  refused tick's included since 2026-09-29: its WARN is the refusal's,
  latched (the next bullet). Per-row lines are sampled at 10 per message
  per tick, the M-SEARCH lesson applied before the flood. `waveform_path` has the same
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
- **…and the watcher's refusal goes through the orphan sweep's latch**
  (2026-09-29, backlog B65). `VariantWatcher` logged the relocation
  refusal at WARN on every tick, beside a WARN summary: measured on a real
  serve at a 2 s interval over twelve rows whose sidecars were gone while
  the tree held one, fourteen WARN lines in 13 s (48 a day at the default
  hour) for a state that lasts until someone acts. **`refusalLatch`
  (internal/integrity/latch.go) is the ONE latch both background sweeps
  keep**, the M-SEARCH rule: one WARN when a streak starts or changes
  kind, at most one per `sweepRefusalRepeat` (a day) while it lasts, and
  one Info line (`no longer refusing`) from the first tick whose check
  proceeds. A refused tick still summarises, at Info (`refused=N`); a
  tick that DELETED still summarises at Warn. A tick that asked neither
  guard's question leaves the latch alone (a failed listing, a tick the
  shutdown stopped in its first pass; the mount-loss skip was one until
  B131, the next bullet); an EMPTY catalog ends a streak, since the rows
  it withheld are gone and a refusal after it must WARN at once, not a
  day later. **A third sweep that refuses takes a `refusalLatch` of its
  own kind type, never a copy of the fields.**
- **…and the mount-loss skip is the latch's second kind, and both of the
  watcher's refusals reach the Jobs card** (2026-09-29, backlog B131).
  The skip (`skipping sweep, variants dir unhealthy with rows in
  catalog`) WARNed on every tick and logged no summary: measured on a
  real serve at a 2 s interval over four rows and an empty variants
  directory, nine WARN lines in 16 s and 114 in under four minutes (24 a
  day at the default hour), for as long as the volume stays unmounted,
  while the "Variant integrity" chip read "on", as it did through a
  relocation streak. The skip is `VariantRefusalVariantsDir`
  (`variantsDirUnavailable`) now, beside `VariantRefusalRelocation`, in
  the one latch: one WARN per streak, with a hint (mount the volume, or
  point the variants directory at where the renditions are), at most one
  a day, an Info summary per tick (`skipped=true`), and one Info line
  when a tick passes both guards again, naming the kind that ended
  (`ended=`, which the orphan sweep's lifted line carries too). A streak
  that turns into the other kind WARNs at once. **Latched, not an hourly
  WARN, though an unmounted volume is the more urgent state**: every
  rendition download it breaks WARNs on its own (`variant sidecar
  missing, but the variants directory is unavailable; keeping the row`,
  a 410), and the card says it until it ends. **The latch publishes
  itself** (`RefusalStatus[K]`, `refusalLatch.status`) whenever a streak
  starts or ends, so a sweep cannot move it and forget the card; the
  orphan sweep published by hand. `VariantWatcher.Status` →
  `admin.Deps.VariantSweepStatus` (runServe) → `/api/jobs`
  `maintenance.variantIntegrityRefusal` (a key) and
  `variantIntegrityRefusingSince`, omitted while it is not refusing or its
  interval is off (`refusalOf`, both chips) → the line's "refusing" badge
  and a `hint warn` worded by `describeVariantIntegrityRefusal`, through
  the renderer the orphan GC's line shares (`renderMaintenanceLine`).
  `TestVariantWatcherLatchesItsVariantsDirRefusal`,
  `TestVariantWatcherSaysEachKindOfRefusalWhenItStarts`,
  `TestVariantWatcherStatusFollowsTheRefusalLatch`,
  `TestTheMaintenanceLinesSayWhenASweepRefuses` (the shipped renderJobCards
  under node, over the served payloads) and
  `TestServeReportsTheVariantWatcherRefusalOnTheJobsCard` (the wiring
  line; red alone with it nil).
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
- **`SidecarInventory.Unreadable` is a count of ENTRIES, and every place
  that prints it says so.** It covers a directory the walk could not
  descend into AND a non-regular entry it could not stat. `upscale --gc`
  and `analyze --gc` print that one count from two places and both called
  it directories; a wording fix reaching one of them is the enumeration
  failure this file keeps recording, and it happened: `bridge doctor`'s
  variants-index summary said "director(y/ies)" until 2026-09-28. The two
  kinds are counted apart now (`UnlistedDirs` is the directories), because
  they bound different things: a directory the walk could not list may
  hold any number of files, a link it could not stat at most one (the
  partial-walk bullet below).
- **A forward sweep's denominator is the TREE, never the catalog, and the
  term that knows a lost index is `orphans > rows`.** Adoption and the
  canonical known set both need the ROWS; the 2026-09-20 aftermath had
  none — the boot sweep had dropped all 10,248 and the auto-optimize
  sweeper had written 200 fresh ones over the stranded tree, so
  the empty-catalog refusal (`gcRefuseEmptyCatalog` now; "is the catalog
  EMPTY?") said no, `gcRefuseRelocationInProgress` ("how many ROWS lost their file?")
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
  render / analyze (and `--allow-partial-walk` past the refusal of a walk
  that could not list part of the tree, below); `artwork --gc` is exempt
  BY NAME in the sweep test (content/MBID-keyed, no absolute path a
  relocation can strand). **So `artwork --gc` steps over a directory it
  cannot list** (2026-09-28): its verdict about a file is that file's name
  against the keys, with no count over the tree for an unseen part to
  flip, so it names the directory on stderr, counts it on the summary,
  exits 0 unless a removal failed, and steps over the filesystem's
  `lost+found` (`IsFilesystemLostFound`) without a word. It stopped at the
  first such directory with exit 1 and no summary, after removing the
  orphans that sort ahead of it; a cache root it cannot read still fails.
  #940 left
  the background `OrphanSidecarSweeper` for its own change, which is the
  next bullet. `analyze --gc` shares both
  halves and gained the dot-directory prune and a fail-closed walk error
  with them; its `.tmp` scratch is removed unconditionally and kept OUT of
  the ratio, or a crashed run trips the guard on the next one. (#940)
- **…and the background `OrphanSidecarSweeper` makes the same decision
  every tick, with no override** (2026-09-28). It unlinked INSIDE a walk
  chunked at 5,000 entries, with a cursor across ticks, and its only guard
  was the empty-known-set one, so #940's own shape (200 rows over 10,048
  stranded files) went in three ticks on the old code: 4,800, 5,000, 248.
  Each tick now lists the catalog, takes the whole tree's inventory
  (`TakeSidecarInventory`, read-only, a `.flac` Consider, NO `MaxEntries`),
  refuses on `MassOrphanRefusalFor` of the FULL `Orphans` count, then on
  `PartialWalkRefusal`, and only then unlinks up to `gcChunkSize` files,
  each path re-checked first (`reclaimOrphan`: a fresh Lstat, the
  inventory's own `classifyWalkEntry`, the grace against the tick start).
  **The chunk caps SUCCESSFUL unlinks, never the walk and never the
  attempts** (2026-09-28): a tick keeps `gcRetainedPerUnlink` (4) chunks of
  paths and tries them in walk order until it has unlinked a chunk. It
  kept one chunk until then and every attempt spent a slot, so a chunk's
  worth of files the service user cannot remove at the head of the walk (a
  root-owned directory a `sudo bridge upscale` left) stalled every orphan
  behind them, every tick, where the old cursor had moved past them
  (measured with a chunk of 5: eight in a read-only directory ahead of ten
  deletable ones, four ticks, nothing unlinked). 20,000 retained paths
  measured 10.8 MB of heap at 205-byte paths, freed with the tick; a head
  of 20,000 or more stalls again, and the summary's `retained` beside
  `failed` shows it. **Nothing about a failure crosses a tick either.** The
  refusal reads the full count, never the retained list: fed its length,
  400 retained against 500 rows proceeds where 1,000 orphans refuse (the
  full-count test, whose fixture had to grow from 150 rows when the list
  did, or it bit only through its wording). The walk costs 128 ms per
  100,001 files warm on the dev Mac, every tick. **No verdict crosses a tick**: a tally across
  ticks was rejected, because its verdict can come from a partial walk (an
  unmount, a cancel, a pruned root) or a catalog that changed mid-pass, and
  a budget carried between passes can be spent on files it never counted.
  The one cross-tick state is a LOG latch (`refusalLatch`, which
  `VariantWatcher` shares since 2026-09-29), the M-SEARCH rule: the refusal
  WARNs once when a streak starts and at most daily while it lasts; a tick
  that decided nothing (a failed or stopped listing or walk) leaves the
  latch alone; the first tick that proceeds after it logs one Info line.
  An empty catalog is a verdict, not a tick that decided nothing: the
  third kind, under the Jobs-card bullet below. Its hint never names
  `bridge variants move` (it needs the rows a lost index lacks). **A stranded tree no larger than the
  catalog is still reaped**, exactly as `--gc` reaps it: the refusal needs
  its floor, `orphans > rows` AND the ratio. The shared walker changed one
  behaviour on purpose: a SYMLINKED variants directory is walked now (the
  old WalkDir Lstat'd the link and swept nothing). The `.flac` Consider
  stays, so the ratio is over the files this sweep would remove, where
  `upscale --gc`'s nil Consider counts and removes every file. **A walk
  that could not LIST a directory refuses too** (`PartialWalkRefusal`),
  after the mass-orphan check and under a WARN of its own (CodeRabbit on
  #1063). What it could not list is missing from every count and may hold
  any number of orphans, so a verdict that proceeds over the part it saw
  can be a refusal over the whole: 1,000 stranded files behind a locked
  directory and 15 in view, against 20 rows, pass the check, and the
  review head unlinked the 15. The inventory's docblock said an unreadable
  entry "can only make the deletion set SMALLER", which is true of the
  list and false of the verdict. The latch keys on the KIND of refusal, so
  a streak that turns into the other kind logs at once. **The cost is
  deliberate**: a variants directory holding a directory the bridge may
  never list reclaims nothing until it is readable, and its hint says so
  once a day. The ordinary such directory, the root-owned `lost+found` of
  an ext4 volume mounted AS the variants directory, is the filesystem's
  and not counted (next bullet), and a link the walk could not stat is
  weighed, not refused. The CLI sweeps make the same refusal since
  2026-09-28 (next bullet).
- **…and a partial walk is refused by every forward sweep, weighed by what
  it can hide** (2026-09-28). `upscale --gc` (optimize, render) and
  `analyze --gc` took `MassOrphanRefusal` over the part of the tree they
  could read and went on: over #1063's shape both unlinked the 15 and
  exited 0. The inventory splits its unknowns, and the three sweeps and
  the doctor read them through two shared decisions. **A directory the
  walk could not list** (`UnlistedDirs`) is unbounded, so
  `PartialWalkRefusal` refuses the verdict, after `MassOrphanRefusalFor`,
  whose lost-index advice is the more urgent. The CLI's way past it is
  **`--allow-partial-walk`, which waives that refusal and nothing else**:
  the mass-orphan check still runs over what was read. Never
  `--allow-mass-orphans` as the way past it, which gives up the whole
  protection to get past one directory; but `--allow-mass-orphans` waives
  the partial-walk refusal too, since that refusal exists only to protect
  the verdict the flag has set aside. So does a threshold of 100, where no
  count can refuse. **A link the walk could not stat** is never walked
  into, so it is nothing, one known file or one orphan, and
  `MassOrphanRefusalFor` counts it as an orphan (o+k of f+k): a known file
  only lowers the ratio and an orphan only raises the refusal (100·(o+1) >
  pct·(f+1) follows from 100·o > pct·f for pct ≤ 100), so that refuses
  exactly when some reading would, pinned against every reading by brute
  force (`TestMassOrphanRefusalForWeighsWhatTheWalkCouldNotStat`). **The
  root-owned `lost+found` of an ext4 volume mounted AS the variants
  directory is not an unknown**: a directory named exactly that, directly
  under the RESOLVED walk root, that a PERMISSION error keeps this user
  out of, is not counted (`IsFilesystemLostFound`). Since #1063 it made
  the background sweep refuse every tick and `bridge doctor` warn on every
  run. Nothing the bridge writes can be in it: a `<variantsDir>/lost+found`
  the bridge made (a single-root library's top-level folder of that name,
  or a multi-root root whose basename it is) is its own and listable, so
  it is walked as before, and so is any readable one (a CLI run as root).
  **Residuals**: a render run as ROOT for a library folder named
  `lost+found` could put sidecars in one the service user cannot list
  (unexamined, as the filesystem's); a volume mounted deeper in the tree
  has its `lost+found` counted, since nothing about the walk root vouches
  for it; an I/O error on it counts. **The reverse guard's probe reads it
  by the same rule** (2026-09-28; `IsFilesystemLostFound` is exported for
  it and for `artwork --gc`). `TreeHoldsVariantSidecars` returned that
  directory's permission error whenever no sidecar sorted ahead of it, so
  on a fresh volume mounted as the variants directory `MassDeleteRefusal`
  refused to reap rows whose sidecars really went, on every
  `VariantWatcher` tick and in `upscale --gc` (exit 1, "could not be
  read"), and it refused a tree whose sidecars sort after it for the
  error rather than for the sidecars. It is evidence neither way there
  now; any other directory the probe cannot list still fails it closed.
  A Gemini consult was attempted for the `lost+found` trade-off and
  refused by the API's spending cap; the rule is the narrow one, decided
  here.
- **The Jobs card shows the background orphan sweep's refusal**
  (2026-09-28). The "Orphan sidecar GC" line read "on" whenever the
  interval was positive, while every tick refused and only the journal
  said so, once a day. The sweep publishes its latch through an atomic
  snapshot (`OrphanSidecarSweeper.Status`: the kind,
  `integrity.OrphanRefusalKind`, and when the streak started; the latch
  publishes itself since 2026-09-29, when the variant watcher's chip
  joined, the B131 bullet above), runServe
  wires it into `admin.Deps.OrphanSweepStatus`, and `/api/jobs` carries
  `maintenance.orphanSidecarGCRefusal` (a KEY the console words) and
  `orphanSidecarGCRefusingSince`, omitted while the sweep is not refusing
  or not running. The card shows a "refusing" badge on the line and the
  reason in a `hint warn` under the list: a sentence in a list cell wrapped
  into a 163 px column 170 px tall at 375 px (measured in a browser).
  `TestEveryOrphanRefusalKindIsWorded` runs `describeOrphanGCRefusal`
  under node for every kind, because the leaf guard proves the key is READ,
  not that it is WORDED; `TestServeReportsTheOrphanSweepRefusalOnTheJobsCard`
  boots serve and is the only test that sees the wiring line (a new kind
  also joins `OrphanRefusalKinds`: the `/api/jobs` guard bullet under
  **Admin console**). **The empty catalog is the third kind**
  (`emptyCatalog`, 2026-09-28): it WARNed on every tick outside the latch
  (five lines in eight seconds at a 2 s interval, on a real serve) while
  the chip said "on". It is decided from the tick's inventory, ahead of
  the mass-orphan check (which would refuse most of the same trees, but
  none under its floor of ten): it refuses when the walk found a sidecar
  file it would remove, or an entry it could not stat, weighed as one
  (`emptyCatalogRefusal`, over `integrity.EmptyCatalogOrphans`, the rule
  the CLI `--gc` sweeps share since 2026-09-29); a tree with nothing it would remove ends a
  streak and is otherwise quiet; a directory the walk cannot list, with
  no file in view, is the partial walk's refusal. **Don't decide it from
  `dirIsEmpty`**, main's question and this change's first draft: over a
  directory left holding empty folders, a `.DS_Store`, a Trash or the
  filesystem's `lost+found`, main WARNed "holds files" on every tick, and
  the draft kept the card refusing forever (seen in a browser).
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
  (#940) **Its hint said `--gc` "measures the whole tree"**, false past a
  directory it could not list; since 2026-09-28 it says `--gc` refuses
  such a walk (`GCRefusesPartialWalk`, sound on a truncated prefix, since
  a directory this walk could not list is one the whole walk cannot list
  either), names directories and links apart, and withholds the ratio
  verdict only for an unlisted directory: a link is weighed as `--gc`
  weighs it.
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
- **The DLNA listener answers only to a Host that names THIS host, with 421
  for any other** (2026-09-29, backlog B170). It has no authentication, and
  what kept it to the LAN is that it is reached on the LAN: a page a LAN
  browser loads from a name its author controls can re-point that name at
  this host's address (DNS rebinding), and the ContentDirectory builds every
  `<res>` and albumArtURI from `r.Host`, so its answers name whatever host the
  request did (measured with the real binary; the record is in the log).
  `ownHostOnly` (host_guard.go) passes, with any port
  or none: an EMPTY Host (an HTTP/1.0 renderer may send none; no browser
  does), `localhost` and a loopback literal, the host of every advertised
  LOCATION, of ServerURL and of a pinned listen address, NAME OR LITERAL
  (`knownOwnHosts`, folded), and any other address of this host's interfaces,
  asked at the request and only for a literal the configuration does not name
  (`Server.interfaceAddrs`, a per-server seam, nil is `net.InterfaceAddrs`).
  **A name not in that list is refused, this host's own included**: resolving
  it is what the page controls. The cost is a control point pointed at the
  server by a name by hand, and the iOS app's fallback that builds a renderer
  URL on the paired host when `/v1/health.endpoints` names no RFC 1918
  address (`BridgeDLNAURLResolver`, whose own doc says mDNS names already fail
  on some renderers). The check sits inside the telemetry middleware
  (`Server.handler`, what Start serves), so a refused request is recorded
  with its 421, and a refused name is logged once (16 at most).
- **The folder index is built LAZILY per Browse call** — pre-building it at the
  top of `handleBrowse` puts an O(N) walk on the flat-list hot path.
  `TrackInfo.RelativePath` is the load-bearing source in production; the
  LCP-derived fallback silently strips a top-level folder.
- **`upnpproxy` relays verbatim** (that IS its bit-exact contract) and sets
  `CheckRedirect: ErrUseLastResponse` — without it a rogue LAN upstream can aim
  a `<res>` fetch at the bridge's own no-auth loopback admin API, reachable
  unauthenticated. A caller needing a different Content-Type wraps the writer;
  don't change the package. Its client dials through
  `discovery.NewDeviceTransport` under the server's approval (the B36 bullet
  below), with no kept-alive connections.
- **A DISCOVERED description's service URLs stay on its own host** (external
  audit 2026-09-23, M3). `resolveServiceURL`
  (`internal/dlna/discovery/url_policy.go`) is the one home: every
  `<controlURL>` and `<eventSubURL>` must be http(s) with a host, and in a
  description found through SSDP (`SourceDiscovered`, the zero value and the
  default of `ParseDeviceDescription` / `FetchDeviceDescription`) it must be on
  the description URL's host, compared case-insensitively with the port
  ignored (`url.URL.Hostname`, so an IPv6 literal compares by address).
  **Read the host with `Hostname()`, never `Host`**: `http://:7789/x` has
  `Host ":7789"` and no hostname, and Go's client dials it on the local host,
  the bridge's own console (measured). The
  manual upstream parses with `SourceUserChosen`: the operator's URL is the
  approval the host rule stands in for, so another host is kept, another
  scheme never, and never this machine or a link-local address unless the
  manual URL itself is of that kind (the next bullet). A refused AVTransport control URL drops the renderer (it reads
  as "no AVTransport"); a refused optional URL (ConnectionManager,
  RenderingControl, any eventSubURL) is dropped alone, so GetProtocolInfo is
  POSTed only to a ConnectionManager URL that passed. An SSDP LOCATION that is
  not http(s) with a host reads as absent in `ParseSSDPHeaders` and is never
  fetched. **The upstream half matters most**: `LiveHost` derives every routed
  byte fetch's host:port from the cached ContentDirectory control URL and
  `upnpproxy` rewrites each stored `<res>` onto it, so a server that
  re-announced its UDN from a new address with a control URL on the loopback
  console steered `/v1/download` and `/dlna/file/{trackID}` there, with a path
  chosen at ingest (measured: the proxy relayed the console's 200).
  `TestAMovedServerCannotSteerTheCachedControlURLToAnotherHost` pins the
  cache half, through the real SSDP handler. **The HOST, not the origin**: an
  origin compare turned six existing upnp tests red, since serving control
  endpoints on another port of the description's host is ordinary. **A
  bound, not authentication**: a spoofer can still aim a
  server's fetches at the host that served the description, its own; the rule
  removes a THIRD host. It mirrors the app's `UPnPURLPolicy` /
  `DeviceDescriptionParser.resolveServiceURL` (iOS #1911), and since iOS
  #1998 (2026-09-29) the host-kind rule of the next bullet too:
  `UPnPURLPolicy.hostKind(of:)` and `hostKindAllowed` bound the app's
  `resolveServiceURL` for every source, over `cloudMetadataAddresses`, the
  same 19 addresses as `cloudMetadataAddrs` (a change to one list is a change
  to both). The app's check on
  relayed renderers (#1977) can compare only against the control URL, since
  `/v1/renderers` carries no description URL, so refusing a device that points
  EVERY service at one other host is the bridge's job. No description fetch
  follows a redirect (each dispatcher sets `ErrUseLastResponse`), stricter
  than the app's same-host redirect rule. A real device whose description
  names another host (a hostname where its LOCATION has an IP, say) drops out
  of discovery; a manual upstream URL is the escape hatch, and a renderer has
  none on the bridge.
- **…and an SSDP LOCATION leads the bridge to THIS machine or a link-local
  address only when the packet came from that address** (backlog B14,
  2026-09-28). The same-host rule bounds a description's service URLs to the
  host that served it and nothing bounded which host that is: a LAN peer
  answering an M-SEARCH, or re-announcing a known UDN (the move detector
  re-fetches), with LOCATION `http://127.0.0.1:7789/<path>` made the bridge GET
  its own console. **A host string cannot carry this rule alone** (measured,
  Go 1.27.1): a public DNS name pointed at 127.0.0.1 reached a loopback
  listener on macOS and on Linux, and `127.1`, `2130706433`, `0x7f000001` and
  `0` reached it on macOS, whose libc resolver takes inet_aton's spellings.
  So it is enforced twice, in `internal/dlna/discovery/url_policy.go`.
  `LocationFromSource`, in both handlers, refuses what the string shows (an IP
  literal on loopback, unspecified or link-local that is not the packet's
  source; a localhost name unless the source is loopback; a host ending in a
  number without being an IP literal, which no device writes) before any
  fetch, and a refused LOCATION reads as an absent one (a known UDN
  refreshed, an unknown one skipped, the move detector never fires).
  `NewDeviceFetchClient`, both clients' default client, checks the address
  EVERY connect targets after resolution (`net.Dialer.ControlContext`), with
  the packet's source carried in the request context
  (`WithAnnouncementSource`, on the description GET and a renderer's
  GetProtocolInfo POST alike). **Don't give that client a proxy** (the check
  would judge the proxy's address, and a proxy on 127.0.0.1 would refuse
  every fetch), **kept-alive connections** (a request could reuse one another
  packet's source allowed) **or a TLS dialer of its own** (it would connect
  around the check); a test pins all three. **The same host kinds bound a
  service URL, for every source**: one on this machine or a link-local address
  is kept only from a description URL of the same kind, so a manual upstream
  elsewhere cannot make the console (or a cloud VM's metadata service at
  169.254.169.254) its `LiveHost`, while one on this machine keeps its local
  control URL. `hostPortFromURL` reads `Hostname()` too. **The exception is the
  packet's own address, never "any address like it"**: a zero-configuration
  device announces from its link-local address, and a loopback source was
  sent on this machine. **Field data, 2026-09-28**: three root devices on one
  home LAN (two hosts) sent every LOCATION on the packet's source address, as
  an IP literal; two other LANs had none. The only LOCATIONs off their source
  were this repo's own `internal/dlna` test advertisers, which multicast a
  loopback LOCATION from the host's LAN address (until B38 bound them to the
  loopback interface, the test bullet further down). Three devices do not
  support the general rule (LOCATION host == source for every address, which
  would also bound names and tailnet addresses): multi-homed hosts and some
  NAS firmware are reported to break it, and a renderer has no escape hatch,
  so **measure before tightening further**. **Not covered**: a LOCATION on a
  tailnet or public address is still fetched, by the app as by the bridge.
  **The app makes this check too since iOS #1998** (2026-09-29):
  `UPnPURLPolicy.location(_:announcedFrom:)` is `LocationPermittedBy` rule
  for rule but for B49's link rule (three bullets down), which the app does
  not have (backlog B138), with no dial check behind it (`URLSession` offers
  no hook between resolving a name and connecting). This bullet said the app had no such
  check until that day, and `resolveServiceURL`'s docblock that the app had
  no host-kind rule: a claim about the other repo goes stale the day that
  repo merges, so a session that changes one side also updates, or files,
  what the other side says about it (here, backlog B78). (This bullet also
  said the later dials of a HOSTNAME control URL were not covered; the next
  bullet covers them.)
- **…and every LATER request to a device dials under the approval its URL
  came with, because a NAME in it resolves again at each dial** (backlog B36,
  2026-09-28). The ingest's SOAP Browse (`upnpUpstreamSOAPHTTPClient`, then on
  `http.DefaultTransport`) and every `upnpproxy` byte fetch dialled the cached
  control URL's host with no dial check, so a peer that passed discovery with
  a name answering its own LAN address and then answered 127.0.0.1 took both
  to the console, and the proxy relayed its 200 (measured, macOS and Linux:
  `CONSOLE POST /ctl`, `CONSOLE GET /api/stats`). What approved a local
  connect is `discovery.DialApproval`: `AnnouncedFrom(src)` (the packet's own
  address, #1069's rule) or `OperatorChose(manualURL)` (every address of the
  kind the URL's host names; **a NAME approves no local address**, so a manual
  URL naming this host by its host name, which Debian maps to 127.0.1.1, is
  refused; write `localhost`). It is recorded as `upnp.ServerInfo.DialApproval`
  beside the control URL, and **`Upsert` keeps and replaces the two together,
  never the approval alone**: a merged-alone approval outlives the URL it
  came with, or pairs one writer's URL with another's approval.
  `ResolveControlURL` and `LiveHost` return both from ONE lookup, and the
  ingest and the proxy carry the approval in each request's context. **Every
  client that sends a device a request is built on
  `discovery.NewDeviceTransport`**: the dial check (it replaces any
  `ControlContext` the dialer template carries), no proxy, **no kept-alive
  connections**, no TLS dialer. The keep-alive rule is measured, not
  argued: with the proxy's old pool a second fetch, approved only for a LAN
  address, rode the first fetch's idle connection to 127.0.0.1 past the check
  (`TestProxy_Serve_NeverCarriesARequestOnAConnectionAnotherApprovalOpened`),
  and net/http also hands a connection dialed for one request to another
  waiting one. Cost: about 65 µs a request on loopback, one round trip on a
  LAN. **Declined, on evidence**: requiring SSDP control URLs to be IP
  literals. Three devices on one LAN (#1069) are thin evidence against names,
  UDA 1.1 says LOCATION hosts are "normally" literals, not always, the dial
  check already covers the dangerous targets, and a literal can name a
  tailnet host anyway (the previous bullet), so the rule would bound no third
  host either. Tests resolve through `discovery.UseResolverForTest` (atomic)
  and `internal/dnstest`, **never by replacing `net.DefaultResolver`**, which
  every goroutine in the process reads unsynchronised. **Residual**: an SSDP
  source is not authenticated, and a peer on the same L2 segment can send a
  packet FROM a link-local address; the same-address exception then
  approves exactly that address, for the description fetch and the later
  dials, never a cloud metadata one (the next bullet), and since B49 only
  where the packet arrived on a zero-configuration link (the bullet after
  it). A loopback source is what RFC 1122 has a host discard from any other
  interface.
- **…and no device's say-so and no approval reaches a cloud metadata
  address** (CodeRabbit on #1074, 2026-09-28). The residual above said
  169.254.169.254 was included: a packet spoofed from it approved it for the
  description fetch and every later dial, whose answers the proxy relays to
  the unauthenticated DLNA listener (a cloud VM's credentials, on IMDSv1).
  `cloudMetadataAddrs` (`url_policy.go`) is the ONE list, from each
  provider's documentation (AWS's IMDS, DNS, NTP, ECS and EKS Pod Identity
  addresses in both families; the IPv6 metadata addresses of Google Cloud,
  Oracle, Linode, OpenStack and Scaleway; Scaleway's and Tencent's IPv4
  ones; Alibaba's 100.100.100.200; Azure's 168.63.129.16). `addrKind` names
  them first (`hostMetadata`), so the string check (`LocationPermittedBy`),
  the service-URL rule (`resolveServiceURL`, for every source) and the dial
  check (`DialApproval.Permits`) refuse them whatever approved the request.
  **Exact addresses, never a range**: a direct-cable device self-assigns
  anywhere in 169.254/16 and fe80::/10, and a /24 around 169.254.169.254
  would refuse one such device in 254 (the tests keep one at 169.254.7.7).
  Ten of them are not link-local (the fd00::/8 ones, 100.100.100.200,
  168.63.129.16) and were fetched on ANY device's say-so, exception or not.
  A tailnet node may hold 100.100.100.200 (it is in 100.64/10, one address
  in four million) and would lose its routed dials. The resolver's own DNS
  connects do not pass the dial check, so Azure's DNS on 168.63.129.16 keeps
  working. `TestCloudMetadataAddrsAreTheDocumentedOnes`
  holds the list to its sources, and
  `TestAPacketFromAMetadataAddressApprovesNoLaterDialThere` drives the chain
  through the real ingest and proxy. **A manual upstream's own description
  fetch is refused one too** (backlog B54; this bullet said it was not
  checked until then): it ran on a plain client, so a manual URL on a
  metadata address was sent a GET every poll, its answer parsed and dropped,
  and nothing said why the server never appeared. **Refused, not warned
  about and fetched**: it was the one request the rule did not reach, a GET
  to IMDS is the request the rule exists to prevent, no media server serves
  on one, and such a server could never walk anyway (its later dials are
  refused). The poller refuses a literal before any request
  (`discovery.NamesCloudMetadataAddr`) and fetches on `NewDeviceTransport`
  under `discovery.ManualDescriptionFetch()`, which permits every address
  BUT a metadata one: a name resolving to one is refused at the connect,
  while a name resolving to this machine is still fetched (#1069's
  decision; only the later dials need `localhost`). Either refusal warns
  ONCE per server, naming its host: a failed fetch is a Debug line, which
  the bridge never prints. The ingest's per-server error (the console's
  last walk error) says so for a literal, where it said "has not answered
  yet". The console refuses a typed literal (`AddServer`), and a config
  holding one still loads.
  `TestManualPollerNeverFetchesACloudMetadataDescription` holds both
  refusals and both controls (a direct-cable literal, a name answering
  127.0.0.1).
- **…and a manual URL's user information travels as a header, and no line
  names the URL** (backlog B66). A ContentDirectory control URL the
  description names relative to the manual URL INHERITS its user
  information (`url.ResolveReference` copies the base's), and the SOAP
  client built its request from it: `upnp: POST
  http://user:<password>@nas:8200/ctl: status 401` reached the journal at
  WARN on every failed walk ("UPnP upstream: per-server error", measured
  with the real binary), and the console shows the same text as the
  server's last walk error. `splitControlURL` (internal/upnp/client.go)
  takes it out of the request URL and `invoke` sends it with
  `SetBasicAuth`, the header net/http built from the URL byte for byte, so
  a server behind Basic auth keeps working; the dispatcher follows no
  redirect, so the header reaches no other host. B69's rule for the enrich
  bases, one package over. The poller's two Debug lines ("description
  fetch failed", "carries no ContentDirectory service") named the URL
  whole; they name the host (`descriptionHostForLog`), as its warnings do,
  and the fetch's error goes through `fetchErrorForLog`, which replaces the
  URL in each of its renderings: discovery's own (`GET <url>: …`, as
  passed) and net/http's `*url.Error` (as re-serialized, a password masked
  `***`, a token as the user name, a query and a fragment whole), read from
  the error's own `URL` field, never guessed, and **quoted as `%q` quotes
  it**: `url.Error.Error` is `%s %q: %s`, so a URL holding a `"`, a `\` or
  an unprintable rune appears escaped, and a search for it as written
  misses (measured; found checking a review's suggestion on #1116). A
  manual URL with no host `descriptionHostForLog` can read (it does not
  parse, or `user:pw@host` was written without a scheme) logs a fixed
  reason, never its error: net/http's `unsupported protocol scheme` names
  the scheme such a value parses with, its user name. The bridge prints no
  Debug line today (`logging.Init` fixes the level at Info), so that half
  would have bitten only the day one is added. A byte fetch through
  `upnpproxy` sends no such credential at all (backlog B143).
  `TestAControlURLsUserInformationTravelsAsBasicAuth` (net/http's header
  taken on every run), `TestAWalkErrorNamesNoControlURLUserInformation`
  (the ingest's per-server error) and
  `TestManualPollerDebugLinesNameTheHostAlone` (five ways the fetch fails,
  three URL shapes, a scheme-less one, every line searched).
- **…and a packet's LINK-LOCAL source approves itself only when the packet
  arrived on a zero-configuration IPv4 link** (backlog B49, 2026-09-29). The
  metadata list closed the costly case; the rest of the residual was any
  link-local neighbour: a peer on the segment answering an M-SEARCH "from"
  169.254.x.y, with a LOCATION on it, made the bridge GET that neighbour at
  a port and path the peer chose, and (by rebinding a LOCATION name) dial
  it for the ingest's SOAP and the proxy's byte fetches, whose answers the
  unauthenticated DLNA listener relays. Measured on main: the renderer
  client sent its GET and GetProtocolInfo POST to 169.254.7.7, and through
  the real ingest and proxy the proxy dialled it (`connect: host is down`,
  the ARP on the Mac's LAN). **What a direct-cable device needs is the link
  the exception exists for**: a zero-configuration link, where THIS host
  holds an IPv4 link-local address and no other IPv4 address
  (`discovery.ZeroConfIPv4Link`). That is what macOS and Windows self-assign
  when DHCP does not answer and what a Linux link-local connection holds,
  measured with the real `net.Interface.Addrs` on each (the dev Mac's USB
  link to an iPhone, Windows' APIPA adapters, a Linux namespace interface);
  every DHCP'd interface measured reads configured. **What the rule costs is
  measured too, and it is where the forgery lands**: a device stuck on
  169.254 on a configured LAN (failed DHCP, which UPnP requires it to keep
  retrying) is not fetched, and from such a LAN a connect to 169.254.7.7 is
  refused at once on Windows (`WSAENETUNREACH`: no 169.254 route) and on
  the Linux host measured (no 169.254 route: routed to the gateway), and
  leaves through the primary interface's 169.254 route onto the LAN on
  macOS (and on any host configured with such a route). A MediaServer
  there can still be configured by a manual URL (`OperatorChose`); a
  renderer has no such hatch. **Judged from the client's OWN interface** (the one its M-SEARCH
  goes out on), read when the client is built and again before every
  M-SEARCH (`discovery.AnnouncementLink.Refresh`, in both tick loops): not
  per packet (`Addrs` is a syscall, GetAdaptersAddresses on Windows), and
  not from the interface a packet arrived on, which x/net/ipv4 cannot
  report on Windows. **Declined**: "only for the interface the packet
  arrived on" alone, which leaves the reported shape open (peer and
  neighbour share the configured LAN); and a link that merely HOLDS a
  link-local address beside a routable one (RFC 3927 says a host SHOULD NOT
  have both; such a link is a DHCP LAN). **One approval, both checks**:
  `LocationPermittedBy` (the string check, which `LocationFromSource` now
  wraps) asks `DialApproval.Permits`, as the GENA guard does, so they
  cannot disagree; `AnnouncedOn(src, zeroConfLink)` is the approval and
  `AnnouncedFrom(src)` its configured-link form. An IPv6 link-local SSDP
  source approves nothing (both clients are udp4; IPv6 SSDP announces from
  fe80 on every link, so a rule of its own would be needed). **A GENA
  subscriber keeps its link-local address on any link** (`SubscribedFrom`):
  the SUBSCRIBE came over TCP, whose handshake shows the address to be the
  peer's own. A refusal only the link decided is one Warn per source,
  bounded at 64, naming the interface and this host's IPv4 there.
  **Residual**: a forged answer on a zero-configuration link itself, and
  one sent from a configured link to the ephemeral port of the client on
  another, zero-configuration interface, from the address of a device on
  that link (neither is visible from the sender's link). No SSDP client
  runs in public mode (config refuses `upnpUpstream.enabled`, DLNA and its
  renderer discovery are gated off; measured with the real binary), so the
  cloud-VM case was never reachable there. The app's mirror has no link
  rule (backlog B138). `TestHandlePacket_NeverFetchesALinkLocalLocationOffAZeroConfLink`,
  `TestServerLinkLocalSourceIsApprovedOnlyOnAZeroConfLink`,
  `TestAPacketFromALinkLocalAddressOffAZeroConfLinkApprovesNoLaterDialThere`
  (cmd/bridge, the chain), the per-link columns of
  `TestDefaultClientDialCheck` and `TestLocationPermittedBy`,
  `TestTheRendererClientReadsItsLinkBeforeEverySearch` and
  `TestTheServerClientReadsItsLinkBeforeEverySearch`.
- **A URL that names a port and no host (`https://:8443`) is not a URL of any
  host, and Go dials it on THIS machine**, so every validator reads
  `Hostname()` (backlog B36). `customEndpoints` prunes it (it was advertised
  to every phone); the harvest credential endpoint answers 400
  (`baseurl.CredentialBase`, which asks `baseurl.NamesHost`), and the
  harvest state store drops such a base, with the credential held against
  it, whenever it opens its file (backlog B97), so one stored before the
  endpoint checked is gone at the next start instead of dialled on every
  tick, as it was until then; a configured enrich or harvest base URL of that
  shape is WARNED about in `Normalize`, never refused, because it loaded
  before and a refusal stops a bridge from starting after an update. **Don't
  move the host test into `baseurl.CanonicalHTTPS`'s reduction**: a hostless pin
  would reduce to "" (unpinned) or, through `Validate`, refuse to load; as it
  stands it pins to a value no accepted credential can carry (`https://:443`
  too, since B97: it reduced to `https://`, which the handler's second
  reduction made "", unpinned). **A warning
  about a configured URL logs its scheme and host alone**
  (`urlOriginForLog`), never the value (review round 1 on #1074): an enrich
  base accepts userinfo, so `http://user:password@:5000` reached the journal
  whole, and a dropped custom endpoint was quoted whole, a parse failure
  twice (the parse error quotes it). `url.URL.Redacted` is not enough: it
  keeps a token written as the user name, and the query. The REFUSALS take
  the same rule since backlog B54 (the bullet under Config on naming a
  configured URL), and the manual upstream poller's warnings name the host
  alone.
- **…and a GENA callback on THIS machine or a link-local address gets the
  initial NOTIFY only when the SUBSCRIBE came from that address, and the
  NOTIFY follows no redirect** (backlog B39, 2026-09-28). The DLNA listener
  binds every interface, and `callbackHostAllowed` admitted a loopback or
  link-local callback from ANY source, so a LAN peer could aim the NOTIFY at
  the bridge's own loopback services, the unauthenticated console among
  them. Measured with the real binary (two containers on dido): a peer
  container's `CALLBACK: <http://127.0.0.1:9999/…>`, or the
  `[::ffff:127.0.0.1]` spelling, made the bridge NOTIFY a listener on its own
  loopback. **The redirect was the wider hole**: the NOTIFY client followed
  the callback's 3xx, a 307/308 re-sending the NOTIFY and a 301/302/303
  turning it into a GET, which the console's `csrfGuard` passes. A peer whose
  callback is its OWN address, which every rule admits (#818's narrow one
  included), steered the bridge to any URL, by address or by name (measured:
  `GET /redirected` on the bridge's loopback). **It was the one client
  sending to a LAN peer's URL that followed redirects**: `upnpproxy`, both
  discovery dispatchers, the upstream SOAP client and the manual poller all
  relay a 3xx. Now `callbackHostAllowed` asks the NOTIFY's own dial approval
  FIRST (`discovery.SubscribedFrom(src).Permits(cb)`, #1074's
  `DialApproval` with the SUBSCRIBE as the approving peer), so the guard and
  the dial check cannot disagree: a loopback or link-local callback only
  when it IS the SUBSCRIBE's address (#1069's rule for a LOCATION, and for
  the same reason: the subscriber's own address, never "any address like
  it"; a loopback callback names the host that SENDS the NOTIFY, and a
  link-local source proves nothing, since any device on the segment can take
  one), never the unspecified address or a cloud metadata address (#1074's
  list; `fd00:ec2::254` is a ULA, which the private arm admitted from any
  source before), and still a private address from any source. The NOTIFY goes out through
  `discovery.NewDeviceFetchClient` (no redirect, no proxy, no kept-alive
  connection, the dial check) under that approval
  (`discovery.WithDialApproval`), so each layer holds without the other:
  with the predicate reverted, `TestGENANotifyNeverReachesThisHostForAnotherAddressesSubscribe`
  stays green on the dial check, and with redirects followed,
  `TestGENASubscriberCannotRedirectTheNotifyOntoThisHost` does;
  `TestGENANotifyClientChecksTheConnectAgainstTheSubscriber` and
  `TestGENAInitialNotifyFollowsNoRedirect` pin each alone. **Don't drop the
  dial check because a callback is an IP literal**: it is what still holds
  when a later change lets a name or a redirect past the string check, as
  the redirect did. **Don't widen loopback to "any address of this host"**:
  the one shape that costs is a control point ON the bridge's host that
  subscribes over the host's LAN address with a loopback callback (measured:
  refused), and admitting it trusts every NAT that rewrites a peer's source
  to a local address. A refusal is a Warn once per (callback, source) pair
  (`noteCallbackRefusal`, bounded at 64 in a set of its own, since a peer
  reaches a refusal at will and a shared bound let refusals silence the
  divergence lines step two waits for), so a control
  point that needs the shape names itself. **#818's step two is only HALF
  done**: refusing a private callback other than the source stays held,
  because the observer it was gated on (`noteCallbackDivergence`, shipped in
  v0.2.0) has watched no SUBSCRIBE: public mode never starts the DLNA
  listener, the NUC's `/v1/health` carries no `dlnaServer` (2026-09-28), and
  home-pc has not been updated since before #818. The evidence needs a LAN
  bridge with `dlna.enabled` running the observer for a release. The iOS app
  subscribes to nothing (`subscribeGENA` is a stub), so no Mirror-PR.
- **Both discovery clients track in-flight detail fetches in a `WaitGroup`, and
  `cache.Clear()` runs UNDER `runMu` as `Stop`'s final act.** Without the group, a
  fetch that already passed its ctx check upserts AFTER `Stop` cleared the cache —
  a ghost server that never ages out. Without `Clear()` under the lock, a
  concurrent `Start()` slips through the gap, spins fresh loops that upsert, and
  the stale `Clear()` wipes them. Every fetch spawn goes through the
  `spawnDetailFetch` helper that does the `Add(1)`; a missed site panics with a
  negative counter.
- **…and both CLAIM a fetch per UDN from one bounded set, because a
  semaphore acquired INSIDE the spawned goroutine bounds the fetches that
  RUN, never the goroutines waiting to** (2026-09-28, backlog B37). The
  upstream MediaServer client spawned a fetch per announcement, queued on its
  two-slot semaphore: 10,000 packets cost 10,000 goroutines and 35 to 37 MiB
  of stack, for one UDN as for many, and a burst for one new server fetched
  its description once per packet (1,000 GETs for 1,000 packets). The
  renderer client's per-UDN `claimFetch` collapsed one device's burst and
  still spawned one goroutine per DISTINCT UDN, the shape a LAN peer that
  sees the M-SEARCH's source port can send. **A dedup is not a bound**: the
  backlog entry read that claim as guarding "exactly this", and it guarded
  one shape of two. `discovery.DetailFetchClaims` is now the one set both
  clients use, under each one's `locMu`: `Claim` refuses a UDN already
  claimed and any claim past `MaxPendingDetailFetches` (64). A refused
  dispatch records nothing, so the next announcement dispatches again (a new
  device is fetched then, and a moved one still reads as moved). Measured
  after: 1 goroutine and 1 fetch for one UDN's burst, 64 goroutines for
  10,000 distinct UDNs, in either client. The claim is taken before the
  spawn and released by the fetch's last deferred call (registered after
  `wg.Done`, so it runs first): `Stop`'s join releases every claim, and a
  restarted client skips no UDN. **The bound is on goroutines, not on the
  caches**: those have their own (the next bullet). A refusal at this bound
  is silent, and stays so (B47 measured it and decided, the next bullet).
- **…and both CACHES hold at most `discovery.MaxCachedDevices` (256), and a
  full cache never makes room by dropping a device it serves** (2026-09-28,
  backlog B47). A renderer whose LOCATION answered 4xx left a stub with a
  year-2999 `LastSeenAt` that no eviction pass reached, and a location
  record beside it, so a flood of distinct UDNs grew both for the life of
  the process (5,000 of 5,000 left an hour later, about 490 bytes a UDN),
  and a REAL renderer whose description failed once, while it booted say,
  was never fetched again from that address (one GET, then nothing in two
  hours of healthy announcements). The upstream cache kept every MediaServer
  serving a valid description (100,000 held 45 MB, and LiveHost's folded
  fallback copied all of them per routed byte fetch: 7.7 ms, 18 MB).
  **A structural stub lasts `structuralStubHold` (5 minutes) whatever the
  TTL**, stamped `hold - ttl` from the failure so `EvictStale` drops it at
  the hold: BEFORE the failure under a TTL longer than the hold, which
  `dlna.discovery.rendererTTLSeconds` admits up to a year. The first form
  clamped the stamp to the failure there and held the stub for the whole
  TTL (CodeRabbit on #1086). Nothing but eviction reads the stamp, and the
  earliest stamp is still the earliest expiry. A stub still costs a broken
  device one GET per hold, not per cycle. **The renderer
  cache makes room by evicting the stub that expires first**, and refuses a
  new UDN once only renderers it serves are left (`makeRoomLocked`): a
  served renderer is what a phone may be driving, and a new UDN is what any
  LAN peer can announce. **The server cache refuses a new SSDP server past
  the bound and never one the operator configured**: `UpsertConfigured` for
  the manual poller and for a UDN `DiscoveryConfig.Configured` names (the
  wiring's `upstreamDiscoveryConfig`, case-folded like `StableServerKey`), so
  a flood of fakes cannot keep the server the ingest walks out. **Don't
  switch either to LRU**: under a flood of valid fakes the least recently
  seen entry is the real device, refreshed every 30 to 60 s while the fakes
  are refreshed every second. A refused fetch leaves nothing behind (the
  renderer client drops the record it wrote, `store`; the upstream client
  records only after a stored Upsert), and the renderer client reaps its
  location records whenever they pass `maxLocationUDNs`, because the cache
  is shared by one client per interface and an evicting write cannot clean
  another client's map. **The upstream handler looks a server up and
  refreshes it in one step, `ServerCache.Touch`**: its Get then Upsert of
  `{UDN, LastSeenAt}` resurrected an entry `EvictStale` took in between,
  with no control URL, which was never fetched again (2 of 200,000 races on
  main). **Measured and left**: a dual-homed upstream server alternating its
  two addresses re-fetches twice per M-SEARCH cycle (120 GETs an hour, the
  control URL alternating between two valid addresses); porting the
  renderer's location set into that security-relevant move detector was not
  worth two GETs a minute. And a dispatch refused at the claims bound logs
  nothing, a result refused at a full cache only at Debug: neither bound is
  reached by a real LAN, a flood's refusals are the protection working, and
  a once-per-episode Warn needs a latch with its own flap rules, the second
  streak policy #1072 declined. `TestAFloodOfBrokenRenderersStaysBounded`,
  `TestARendererWhoseDescriptionFailedOnceComesBackAfterTheHold`,
  `TestAFloodNeverDisplacesACachedRenderer`,
  `TestAFloodOfFakeServersStaysBounded` and
  `TestAConfiguredServerIsCachedPastTheBound` pin it.
- **Both SSDP read loops share `discovery.HandleReadErr`** — timeout resets the
  streak, `net.ErrClosed`/ctx exits, anything else logs with a ctx-aware backoff
  and one escalation. A bare `return` on a transient error kills discovery for
  the process lifetime; no backoff hot-spins a core. Keep the policy in the
  shared helper.
- **The M-SEARCH SEND failure log is streak-suppressed, and BOTH discovery
  clients report through the one `discovery.SendFailureLog`** (first at Warn,
  one Error ten minutes into the streak, then silence until recovery, which
  logs the streak's length). It runs on a ticker and its failure mode is
  persistent by nature: unsuppressed it produced **199,078 of the last
  200,000 log lines**. The cost isn't disk — it's that every other line
  becomes unfindable. **The upstream MediaServer client discarded every send
  error until 2026-09-28** (backlog B32): pinned to the dev Mac's routeless
  `utun0`, its socket answered `sendto: can't assign requested address` on
  every tick and the client logged nothing in 20 ticks; it now logs the Warn.
  **Ten minutes is a DURATION, which each client turns into its own ticks**
  (`sendErrEscalateAt`: 20 at the renderer's 30 s, 10 at the upstream's
  60 s, never below 2). It was the renderer's constant 20, ten minutes at one
  cadence only, and both cadences are configurable. **Every line names the
  interface**: both wirings start one client per LAN-eligible interface, and
  a route can be gone on one of them while the others send. `Start` calls
  `Reset`. Don't give a client its own copy of the policy: this is the send
  side's one definition, as `HandleReadErr` is the read side's. **The SSDP
  advertiser's NOTIFY bursts report through it too** (2026-09-28, backlog
  B47): a failed write logged at Debug alone, so an advertiser whose
  interface lost its route announced nothing and said nothing at the default
  level. It notes ONE result per burst (`announceAlive`, the first failure of
  its five writes): noted per write, the Error lands inside the first burst
  (escalation is at the second failure, ten minutes of a 14-minute cadence).
  Start builds the log, so each run starts a fresh streak; the byebye burst
  at Stop notes nothing, since the periodic goroutine may be mid-burst then.
  `NewSendFailureLog` takes what it reports on ("M-SEARCH", "NOTIFY").
- **…and a send Stop's close cut short is a STOP, not a failure**
  (2026-09-28). `sendMSearch` snapshots the socket and then writes, and Stop
  can close it in between: the write fails with `net.ErrClosed`, which logged
  "M-SEARCH send failed … use of closed network connection" and took the
  streak to 1 on 4 of 6,000 plain Start→Stop cycles on macOS (43 under
  `-race`) and 24 of 4,000 on Linux under `-race`. `SendFailureLog.Note`
  drops it before the streak sees it (0 of 12,000 after, on each host). **The ERROR
  decides, never the run's context**: the socket is the client's own and only
  Stop closes it, so `net.ErrClosed` names the stop exactly, while a write
  takes no context and a genuine failure that lands during Stop is still a
  failure (#998's second condition, under The CLI and the serve wiring).
  `TestSendMSearchReportsAFailureThatLandsDuringStop` goes red if the check is
  widened to `ctx.Err() != nil`. `HandleReadErr`'s ctx arm is no precedent: it
  decides whether the READ loop exits, not what a result means. The drop is in
  the shared log, so the upstream client has it too (its socket is likewise
  its own, and only its Stop closes it). The server-side advertiser's NOTIFY
  burst (`sendAliveAll`) ends on the same error rather than logging a Debug
  line per target left, since only its Stop closes its sender, and returns
  it, which the shared log drops; a write that fails for its own reason still
  logs one Debug line per target, and the burst's result reaches the
  default level through the log.
- **A test that starts an SSDP advertiser binds it to the loopback
  interface, and a discovery test client sends no M-SEARCH** (2026-09-28,
  backlog B38). Two `internal/dlna` tests bound the OS default, so one run of
  the package multicast about 310 NOTIFYs of a fake MediaServer with a
  loopback LOCATION to every device and bridge on the LAN (measured with a
  listener joined on en0, three runs; about 300 of them from
  `Test_SSDPAdvertiser_StartStopRaceFree`'s 1 ms advertise interval), and
  each package's `TestStopWaitsForInFlightFetch` sent a real M-SEARCH.
  `loopbackInterface(t)` pins the join, the sends and the listener to
  loopback and keeps the race and lifecycle coverage (both tests PASS, not
  skip, on macOS and in a Linux container); `newTestClient` and
  `newServerDiscoveryTestClient` install a `writeMSearch` that sends nothing.
  Measured after: 0 datagrams on en0 in three runs of each package, while a
  lo0 listener heard the advertisers' NOTIFYs; on Linux 0 on the Docker
  bridge, where main put 299. **A new test that starts an advertiser or a
  client does the same.** **The skip is decided by the PIN, not only by
  Start's error**: Start only warns when `SetMulticastInterface` fails, and
  sends on the OS default interface, so a test whose Start succeeded would
  multicast onto the LAN on such a host. `loopbackInterface` first pins a
  socket of its own with Start's own call (`pinMulticastInterface`) and
  skips where that fails (CodeRabbit on #1086). The fallback itself stays:
  a production advertiser whose pin fails still advertises. **Measure what
  leaves a Linux host from ANOTHER network namespace, or with tcpdump on
  the bridge, never with a listener
  in the same one**: Go binds a multicast listener to the WILDCARD address
  and the group's port (this said "the group address" until 2026-09-29),
  and with Linux's default `IP_MULTICAST_ALL` a socket joined on one
  interface also receives the group's datagrams arriving on any interface
  another socket joined (a listener on eth0 heard the loopback advertisers'
  NOTIFYs, and one on lo heard eth0's). A test's own listener keeps that
  default; the advertisers' listeners turn it off since backlog B71 (the
  next bullet). On the Windows runner the join and the pin both take on
  "Loopback Pseudo-Interface 1": the tests run there, and its log shows them
  starting with no pin warning. What left a Windows host was measured on
  2026-09-29 (B71, on a Windows 11 host with Tailscale, with a listener on
  the same LAN): nothing reached the LAN, because the advertiser's
  connected NOTIFY sender took the Tailscale route, which its pin did not
  move. The sender is unconnected since.
- **Each SSDP advertiser hears only the multicast M-SEARCHes that arrive on
  its own interface, and announces from that interface's address** (2026-09-29,
  backlog B71). The DLNA server runs one advertiser per LAN interface, each
  with that interface's LOCATION (#328), and by the eligibility rule a Linux
  host running Docker is such a host: `docker0` and every user bridge carry
  a private IPv4.
  **The listener**: `net.ListenMulticastUDP` binds the wildcard address
  (net's `listenDatagram` rewrites the group before the bind), and Linux's
  default `IP_MULTICAST_ALL = 1` delivers the group's datagrams to it from
  every interface where any socket on the host joined, so every advertiser
  answered every interface's M-SEARCHes with its own LOCATION. Measured with
  the real binary in three network namespaces: each M-SEARCH from either
  subnet (6 of 6) got two answers, one naming the other subnet, the wrong
  one first in 3. macOS and Windows deliver per joined interface
  (measured). `listenSSDP` (ssdp_listen_linux.go) sets `IP_MULTICAST_ALL =
  0` in the `ListenConfig`'s Control, **before the bind**: from the bind on
  a wildcard socket takes other interfaces' group datagrams, so setting it
  after `ListenMulticastUDP` returns leaves a window. A kernel that refuses
  the option keeps the old listener with one Warn, never a refusal that
  takes DLNA down. **The sender is an unconnected socket, pinned before
  anything is sent, writing each NOTIFY to the group** (`openNotifySender`).
  `net.DialUDP` fixed its route and source address at the connect, along
  the group's route, and a pin set afterwards did not move the source (Linux,
  macOS) or even the route (Windows). Measured: the second interface's
  NOTIFYs came from the first interface's address, and a renderer with no
  route back received 0 of 5 (5 of 5 now); on Windows with Tailscale
  (home-pc's shape: Windows' SSDP service holds the group on every
  interface and Tailscale's route has metric 5) the connect took the
  Tailscale address and route, and a sender pinned to Ethernet put 0 of 4
  datagrams on the LAN (2 of 2 now). **And on a host with no route to the
  group (no default route) the connect failed ("network is unreachable"),
  no advertiser could start, and DLNA did not start at all**; an
  unconnected socket needs no route. **Don't connect it, and don't pin a
  socket after connecting it.** A single-interface host sees no change:
  its advertiser's one membership is where every search arrives, and its
  source is that interface's address either way. The discovery clients
  take nothing from `IP_MULTICAST_ALL` (an ephemeral port, no membership);
  the upstream client's MediaServer search is answered by the bridge's own
  advertisers, with both LOCATIONs before and one now (a looped-back
  search, measured), and only a configured server is ever walked. Pinned
  by `TestAnSSDPListenerHearsOnlyTheInterfaceItJoined` (Linux, and macOS
  where the host sends its datagram: GitHub's macOS runner answers `no
  route to host` for en0, and the test skips there),
  `TestAStartedAdvertisersListenerHearsOnlyItsOwnInterface` and
  `TestAListenerTheKernelWillNotConfineStillListensAndSaysSo` (Linux),
  `TestAStartedAdvertiserWritesItsNotifiesFromAnUnconnectedSocket` (all),
  `TestAnAdvertiserOnLoopbackNotifiesFromALoopbackAddress` (macOS, Windows:
  Linux gives a lo-pinned multicast another interface's address either way,
  127.0.0.1 being host-scoped), and
  `TestSSDPAdvertisersKeepToTheirOwnInterfaces`, the real server over veths
  in a network namespace of the test's own, with and without a route to the
  group (root, or a user namespace: dido, not CI). **Still open**: a unicast
  M-SEARCH to port 1900 reaches ONE advertiser's socket (Linux: the last
  bound; macOS, Windows: the first) and is answered with that advertiser's
  LOCATION, whatever address it was sent to (backlog B119); on Windows the
  advertiser hears no search sent from its own host (B120); and a pinned
  `dlna.listenAddress` advertises on the picker's interface, which need
  not hold that address (B121).
- **`upnp_track_routing.server_udn` holds the ingest's `StableServerKey`, NOT the
  device's raw UDN.** They are equal only for a device whose UDN is already
  lowercase, and never for a manually-configured server (`manual:<sha256(url)>`).
  The SSDP cache is keyed on the raw UDN, so handing a routing key to a raw-UDN
  lookup reports "offline" about upstreams that are up. Anything wanting both
  membership and liveness must carry BOTH spellings — one lookup cannot serve
  both. `librarycat.SourceID` prefixes `"source:"` before hashing so a routing
  key can never collide with an album or artist id.
- **`ServerCache.Upsert`'s merge must carry EVERY descriptive field, and the
  test that pins it is reflective.** The SSDP handler refreshed a known UDN
  with `{UDN, LastSeenAt}` on every announcement (it uses `Touch` since
  2026-09-28) and the merge preserved the
  cached fields one by one — `DeviceUDN` was added to `ServerInfo` and not to
  the list, so any partial refresh blanked it. Latent today (only the
  manual-URL poller sets it, under a key no SSDP refresh lands on), and the
  next field would have met the same list. The merge is `mergeServerInfo`,
  which `Upsert` and `UpsertConfigured` share.
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
- **A Tailscale interface is eligible ONLY through the opt-in
  (`EligibilityOpts.TsnetIfaceName`), and its ULA is what had defeated it**
  (2026-09-28). A Tailscale interface carries an address in
  `fd7a:115c:a1e0::/48`, and one in 100.64/10 where the tailnet has IPv4. The
  100.64/10 address always counted as public; the ULA is
  inside fc00::/7, so `net.IP.IsPrivate` counted it as a LAN address and
  admitted the interface with no opt-in (and no production caller sets one).
  Measured: the dev Mac's `utun12` and dido's `tailscale0` were eligible with
  their addresses and not without the ULA. The real bridge on the Mac then
  ran an SSDP advertiser on utun12 (LOCATION on its 100.x address) and a
  renderer- and a UPnP-discovery client sending into it, and on dido each
  of those consumers' first step (the group join, an M-SEARCH send)
  succeeded on tailscale0. On Windows (reasoned from the ranking and pinned
  by a row, not measured) the Wintun adapter, which has no point-to-point
  flag, outranked a zero-config LAN as the mDNS responder's single pick.
  `isTailscaleULA` sorts the ULA out BEFORE `IsPrivate` and counts it as
  public, so it admits nothing AND keeps the zero-config arm from admitting
  a tailnet with IPv4 switched off (fe80 plus the ULA). **Address-based, not
  name-based**: macOS numbers its utuns, and Windows' adapter carries no
  flag that says tunnel. 100.64/10 needed no change: a LAN genuinely
  numbered in CGNAT space was refused before and still is. `tailscaleULA`
  is pinned to `tsaddr.TailscaleULARange()` by
  `TestTailscaleULAIsTailscalesRange`. Multicast written to the tailnet
  interface reached no peer in the one tailnet measured (no exit node, no
  subnet router): 0 of 32 datagrams each way between the Mac and dido,
  beside 32 of 32 unicast controls.
- **Eligibility is the allowlist; SELECTION prefers a real LAN among what it
  admits, and the two stay separate** (2026-09-27). `PickLANEligibleInterface`
  returned the FIRST eligible interface, and macOS enumerates system utuns
  ahead of `en0`: on the dev Mac that was `utun0` (`UP,POINTOPOINT,RUNNING,
  MULTICAST`, one fe80 address), which the zero-config arm admits. The mDNS
  responder pinned itself there, its IPv4 listen failed without a word
  (hashicorp/mdns drops that error, and utun0 has no IPv4), and neither
  hashicorp/mdns's client on `en0` nor `dns-sd -B` found the bridge, while
  the NUC's bridge on the same LAN was found. The picker now ranks what
  eligibility admits (`lanPreference`): not point-to-point first, then a
  private IPv4, then any other usable address (a ULA, the opted-in tsnet
  interface), then link-local only, and enumeration order among equals, so
  a host whose first eligible interface already ranks best keeps it (dido
  measured: `enp1s0f0` and the same five-member set as before; three
  members since the next paragraph's rule and the Tailscale bullet above).
  `PickAllLANEligibleInterfaces` keeps its members and order but leaves out
  a point-to-point interface whose only addresses are link-local whenever
  anything else is eligible: six such utuns sat in the dev Mac's set, and
  the renderer and UPnP-upstream discovery clients start one SSDP client per
  member, while an IPv4 multicast send pinned to such a tunnel fails
  (`sendto: can't assign requested address`, hashicorp/mdns's client on
  utun0). **Don't fold the preference into `IsLANEligibleInterface`**:
  dropping every point-to-point interface, or the zero-config arm, from
  ELIGIBILITY takes away an opted-in tunnel and a direct-cable renderer,
  and the bullet above says why each arm is there. **Don't reduce the key
  to either half**: the flag alone misses Windows' Wintun adapter
  (WireGuard's, and Tailscale's when opted in; `IF_TYPE_PROP_VIRTUAL`,
  which Go gives no point-to-point flag) and a Mac's `bridge0` holding a
  self-assigned address, and the class alone ties a WireGuard tunnel's
  private 10.x with `en0` and lets enumeration order pick the tunnel (each
  half turns rows red that the other leaves green). One case moves the
  other way: a host whose LAN is zero-config (169.254 / fe80 only) beside a
  private-IPv4 bridge (`docker0`, a VM's) now binds the bridge, where the
  first eligible used to win if it came first. The tables drive
  `pickLANInterface` and `pickAllLANInterfaces`, the seam both exported
  pickers call (`TestPickLANInterfacePrefersANonTunnelWithAPrivateIPv4`,
  `TestPickAllLANInterfacesDropsALinkLocalOnlyTunnel`), and
  `TestTheExportedPickersRunTheSelection` compares the exported pair with it
  on the host, which can fail only where the first eligible interface does
  not rank best (the dev Mac, not a typical Linux runner).
- **…and then leaves out a member with no IPv4 address, whenever one with
  an IPv4 address remains** (2026-09-28). Every consumer of the set runs
  SSDP over IPv4 (udp4, 239.255.255.250): the advertisers, which already
  skipped such a member (`gatherAdvertiseEndpoints`), and the renderer and
  UPnP-upstream discovery clients, which did not. On the dev Mac awdl0 and
  llw0 (fe80 only, not point-to-point) each got a renderer client whose every
  M-SEARCH failed with `can't assign requested address` (a WARN per client
  and an ERROR ten minutes on; since #1072 the UPnP-upstream clients on the
  same members report theirs as well); on dido each
  docker veth (fe80 only, a port of `docker0` or a user bridge, both members
  with an IPv4 address) got two clients, whose sends Linux lets out. **The
  rule is the SET's, not the SSDP call sites'**: all three consumers are IPv4
  SSDP, and it sits beside the tunnel rule #1051 justified by the same
  failed send. A 169.254 address counts (the direct-cable renderer).
  **It runs AFTER the tunnel rule, and neither rule empties the set**, so a
  host whose tunnel-ruled set holds no IPv4 member (an IPv6-only LAN) keeps
  that set exactly, and UPnP upstream, whose manual-URL poller starts only
  where an SSDP client does, still starts there. The single pick can then
  be a member the set leaves out (an IPv6-only LAN beside a tunnel holding a
  private IPv4, or awdl0 beside an opted-in tunnel, since a non-tunnel
  ranks first): `assertPickIsInTheSet` allows exactly that, a pick carrying
  no IPv4 while a member does, because the responder answers over IPv6 too.
  `TestPickAllLANInterfacesLeavesOutAMemberWithNoIPv4` and
  `TestPickersLeaveOutATailnetInterfaceWithoutTheOptIn` drive both rules.
- **The mDNS rebind loop compares the ADVERTISEMENT, never the host's
  addresses** (2026-09-28). `maybeRebind` rebuilt the responder whenever
  `ipsForAdvertise()`, every up interface's addresses, differed from the set
  cached at the last rebuild, while the records carry only the pinned
  interface's. So an address coming or going on another interface rebuilt
  it with the same records on the same interface. Sampled at the loop's
  cadence for 80 minutes: 21 such changes on dido, every one a docker veth
  (a container starting or stopping), and 1 on the dev Mac, a new utun
  coming up; the pick and its addresses changed 0 times on either.
  Each tick now builds the advertisement a rebuild would make
  (`advertisementNow`: the InterfaceSource's pick, and the addresses
  narrowed to it, or all of them when nothing is pinned) and rebuilds only
  when it is not `same` as the running one. **By name AND index**: an
  adapter re-created under a new index has none of the old sockets' group
  memberships. That asks the InterfaceSource every tick, where it was asked
  only on a rebuild, so the responder follows a better interface as soon as
  the picker names it, **and the source must not print per call**:
  cmd/bridge's `lanInterfaceSource` prints a failed pick once per streak (a
  host with no LAN-eligible interface would print a line a minute), and
  `TestMDNSInterfaceSourceIsTheOncePerStreakOne` requires the Config literal
  to take it. The rebind tests pin the responder to the loopback interface,
  which hashicorp/mdns binds on macOS and on Linux (measured).

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
  this — the payload is built in JS — so it took driving the real form. The
  feature trays sent managed fields too, until 2026-09-28 (the tray bullet
  under **Admin console and the web player**).
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
  test drives the real handler for every row in it. **That test checks the
  REPORT, and no consumer**: under `upscaleEnabled`'s `live` row the console's
  size projection was taken at boot until 2026-09-28, and four consumers read
  the flag without its sox half (the construction-time bullet under The CLI
  and the serve wiring, and the one after it).
- **…and a DEFAULT splits the halves too, when a writer can store what `Load`
  would replace** (#1042). `applyDefaults` runs in `Load` alone, and before
  the `BRIDGE_*` overrides, so its `if c.LibraryName == ""` reached neither
  the settings PATCH nor an override: `PATCH {"libraryName":""}` answered
  `live`, the running bridge served `""` in `/v1/health` and in every pairing
  QR's `name=` (which the app refuses), and a restart served
  `DefaultLibraryName`. The default now lives in `Normalize`, which every
  writer runs before it saves, so no writer can store a name `Load` would
  serve differently. **Refuse a blank that would replace a name, default one
  that loses nothing**: the PATCH answers a blank name with 400 `validate`,
  because defaulting it would replace the operator's name with one nobody
  chose, while init treats a blank `--name` as none (the kept or host name,
  as `--name ""` and Enter at the prompt). **Trim with
  `config.TrimLibraryName`, never `strings.TrimSpace`**: the app's pairing
  parser refuses a `name=` that its `.whitespacesAndNewlines` trim changes,
  and that set is `unicode.IsSpace` plus U+200B ZERO WIDTH SPACE (every
  scalar enumerated on both sides). Don't move the default back into
  `applyDefaults`, and don't turn it into a `Validate` refusal: a
  `BRIDGE_LIBRARY_NAME` of spaces would then stop a bridge from starting
  after an update, over a display name.
- **…and every stored name is one a pairing code can CARRY** (#1046). The
  app also refuses a `name=` over 256 Characters ("Pairing code's name field
  is too long.") and one that is not UTF-8 (Foundation leaves the item with
  no value: "missing the name field"). The console took any length (257
  characters answered 200 `live`, and the next QR did not pair), and `bridge
  init --name $'Caf\xe9 Tunes'`, "Café" typed in a Latin-1 terminal, was
  saved as `!!binary` and served with `name=Caf%E9`. **The cap,
  `config.MaxLibraryNameLength`, is 256 RUNES**: a Character is one or more
  scalars, so that is never more than 256 Characters on any iOS version,
  whatever Unicode tables it segments by, and Go has no grapheme
  segmentation to count the app's way. It is conservative for multi-scalar
  Characters (37 family emoji, 259 scalars, are refused; the app counts 37).
  Don't count bytes: 256 `é` must pass. **Refuse what the operator types,
  repair what is already stored** (#1042's rule, one step on):
  `config.CheckLibraryName` refuses in the PATCH (400 `validate`, nothing
  written) and in init's `--name` (exit 2, before anything is written), and
  init's prompt asks again; `Normalize` repairs a config's or an override's
  name (`config.RepairLibraryName`: U+FFFD for bytes that are not UTF-8, cut
  to 256 runes and re-trimmed, with a WARN), because a `Load` refusal would
  stop a bridge from starting over a display name. init keeps and offers an
  install's name AS LOAD SERVES IT, or its prompt would offer a name it then
  refuses. Don't cut in the PATCH instead: that stores a name nobody typed.
  **Nor in the console**: the name box has NO `maxlength`. It counts UTF-16
  units, so it stopped names the handler takes (129 emoji), and it cut a
  paste without a word; the handler's 400 is what the page shows ("Save
  failed: libraryName: must be at most 256 characters, …"). Added in #1046's
  first round, removed on review (CodeRabbit).
- **A configured URL is named in every refusal and warning by its field,
  its scheme and its host, never its value** (backlog B54). #1074 gave the
  WARNINGS that rule (`urlOriginForLog`, under DLNA) and left the two
  REFUSALS that stop the bridge from starting: `normalizeBaseURL`'s and
  `Validate`'s harvest pin quoted the value (`got
  "ftp://user:password@mirror/ws/2"`), which serve prints, `bridge doctor`
  puts on its config-file line and the console's PATCH answers, and an
  enrich base may legitimately carry a mirror's password (net/http sends
  userinfo as Basic auth). `urlFieldForLog` is the one naming
  (`enrich.musicbrainzBaseURL (ftp://mirror.example)`), and the message says
  what the value may not carry, since it is not shown. **A URL with no host
  part renders as NOTHING, scheme included**: `user:password@host`, written
  without a scheme, parses with the USER NAME as its scheme, and #1074's own
  custom-endpoint warning quoted it twice (`(s3cret-pw:)`, `got
  "s3cret-pw"`). **Search for a leaked secret without regard to case**:
  `url.Parse` LOWERCASES a scheme, which is how #1074's test, searching for
  `s3cret-Pw`, passed over that leak.
  `TestNoStartupErrorCarriesAURLsCredentials` (every shape, through Load,
  Validate and the environment) and
  `TestAStartupRefusalNamesAURLWithoutItsCredential` (serve and doctor).
  The enricher's own request errors are covered too, by keeping the
  credential out of every request URL (the bullet on a configured base URL's
  user information under **Enrichment**, backlog B69): net/http's
  `*url.Error` names a request URL with its password masked (`user:***@`)
  and a token written as the user name whole. So are `autocert.domain`'s
  two warnings (the field, and its scheme and the host it is served as), a
  manual upstream's control URL, whose user information goes in a header,
  and the manual poller's Debug lines (backlog B66: the `autocert.domain`
  bullet under **The wire contract**, and the manual-URL one under **DLNA,
  UPnP and discovery**).
- **When a change cannot take effect, say so** — but only when the outcome
  depended on THIS bridge's runtime state (a Tailscale mode's transition;
  applied-but-inert because a toolchain is missing; no sweeper wired, which
  only a harness is since #781). NOT for "listeners bind once", which is true
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
- **…and a restart request exits with `supervision.RestartExitCode` (75),
  a stop with 0: an exit 0 is what a supervisor leaves stopped** (2026-09-29,
  backlog B201). The console's Restart, "Install & restart" and the
  auto-installer's restart all exited 0, and the LaunchAgent `bridge init`
  writes relaunches an unsuccessful exit only (KeepAlive
  {SuccessfulExit: false}), while the Windows service had no recovery
  actions: measured under a real LaunchAgent on the dev Mac and a real SCM
  service on nomos, `restarting: true`, then exit 0 and nothing listening,
  for good. `serveRestart.request` marks the stop and calls runServe's own
  cancel (so the whole shutdown runs, the rule above), and runServe's
  second defer turns a clean exit after a request into 75 (EX_TEMPFAIL):
  launchd relaunched the probe with `last exit code = 75: EX_TEMPFAIL`,
  and a SIGTERM (`launchctl kill`) still exited 0 and stayed down.
  `admin.Deps.Restart` and `AutoInstallRestart` are both
  `restart.request`, never a bare `cancel`
  (`TestEveryRestartInServeGoesThroughTheRestartRequest` reads the wiring;
  `TestARestartRequestedFromTheConsoleExitsToBeRestarted` drives the
  console's). **Windows needs two more parts, both load-bearing**: the
  service's recovery actions (restart after 2 s, 2 s, then 30 s, reset
  after an hour) with `FailureActionsOnNonCrashFailures`, set by the
  installer and, for a service installed before v0.2.1, by the running
  service itself as it starts (`packaging.EnsureServiceRecovery` with its
  own name, args[0]; it leaves recovery an operator configured alone), since
  a console update replaces the binary and never re-installs; and the
  service handler returns the exit code WITHOUT reporting Stopped first,
  because the SCM takes the exit code from the first Stopped status: the
  old handler's early Stopped (exit 0) made a failure read as a clean stop
  (measured on nomos with recovery set: the early-Stopped build stayed down
  with exit code 0; the fixed one logged event 7024, "service-specific
  error 75", then 7031, "corrective action … Restart the service", and was
  back in 4 s; an SCM Stop stays stopped). systemd's Restart=always restarts
  either (its journal names this one 75/TEMPFAIL), and Docker's
  `on-failure` policy, which restarts a non-zero exit, now relaunches it
  too (docs/docker.md). **And `bridge start` kickstarts a
  loaded agent**: `launchctl bootstrap` of one answers "Bootstrap failed:
  5: Input/output error" on macOS 27, which `startForOS` did not swallow,
  so `bridge start` over the stopped agent failed; a kickstart starts a
  loaded agent and leaves a running one alone (both measured).
  **Release note**: a macOS or Windows service install that updates from
  v0.2.0 through the console runs v0.2.0's restart, which exits 0, so it
  comes back down: run `bridge restart` once after that update.
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
  rewrote `tokens.json` from a stale in-memory slice and deleted a sibling
  `bridge pair` mint. A reload failure ABORTS the flush.
- **…and a reload before the staging is not enough: every `tokens.json` write
  re-reads the file just before its rename** (#1043), adminauth's #1039 rule
  (under Auth, pairing, TLS) on the bearer-token store. All seven writers (the
  debounced Validate and RecordClientVersion writes, the shutdown flush, and
  Mint, Revoke, Rotate, SetExpiry) reloaded, then staged (a temp file, a write,
  an fsync: a median of 0.6 to 0.7 ms on ext4, 2.3 to 3.8 ms on APFS) and
  renamed with no further check. A `bridge pair` or
  `bridge token revoke|rotate|expire` committing inside the staging was renamed
  over: the paired device's token vanished (401 from then on) or the revoked one
  came back, on disk and in the running bridge. Measured with two processes and
  no test seams, against a writer every 20 ms: 24 of 200 mints lost and 30 of
  200 revokes undone on APFS (16 and 11 on ext4), and 1 and 2 after the fix (2
  and 1). `commitLocked` builds from the last read, stages, compares the file's
  BYTES with what that read or this process's last write saw (`Store.raw`), and
  on a change reloads (whose per-token merge keeps unwritten LastUsedAt and
  client-version bumps), rebuilds and restages, up to three attempts. A re-read
  or reload that fails ends the write with nothing written. **Bytes, not
  mtime+size**: a write is rare enough to afford the read, and a same-size
  sibling write in the same mtime tick passes a stat. A write adopts its list
  only once its rename lands, and a Rotate or SetExpiry of a token revoked
  meanwhile answers ErrNotFound rather than writing it back. **What a write
  records as its file is the STAGED file's stat**, after its Close and before
  the rename: RenameWithRetry fsyncs the directory after renaming (0.5 ms ext4,
  2.8 ms APFS), and a stat of the path after that took a sibling's commit in the
  fsync for this process's own, so Validate refused a device paired there until
  the next write. What remains is the rename itself (a median of 50 µs on ext4
  and 170 µs on APFS from the re-read to the rename's return; 2 of 1,600 mints
  lost against a 10 Hz writer) and, on Windows, its retries. A kernel lock would
  close it, as for adminauth.json, and is not taken.
- **…and a debounced write that FAILS starts the next window too, and a store
  the running bridge cannot read is reported once each way** (#1047).
  Validate's and RecordClientVersion's debounced write started the next 30 s
  window only when it LANDED (`writeLocked` stamped `lastUsedFlush`), so while
  it could not land every request past the window re-entered the write: 10
  requests gave 10 ERROR lines with tokens.json unreadable (a `sudo bridge
  pair` beside a service install leaves it root's 0600), damaged, or on a full
  disk, and a write failing at its commit paid its staging per request under
  `s.mu`, which every authenticated request takes. With the rename refusing
  (RenameWithRetry's 750 ms of retries), 40 concurrent requests took 30.4 s,
  the worst 6.8 s; with the fix 0.76 s and one ERROR. **`flushDueLocked(now)`
  is the one gate, and it starts the window at the ATTEMPT**, adminauth's #1039
  rule; never gate a debounced write on the interval without starting the
  window in the same step. A failed reload still ABORTS the write, the
  observations stay in memory for the attempt a window later or the shutdown
  flush, and `FlushLastUsed` asks nothing of the window. The write side still
  logs once per window: that bounds the flood rather than silencing it, and it
  is the only report of a file made unreadable IN PLACE (a chmod changes no
  mtime or size, so `reloadIfStale` has nothing to reload). **While
  `reloadIfStale` cannot read the file, devices are checked against the tokens
  last read, deliberately**: a device `sudo bridge pair` paired is refused and
  one `sudo bridge token revoke` removed is still accepted. Fail-closed was
  rejected: the content is unknown, so the only refusal left is every paired
  device, over a permissions mistake. What was missing was the SAYING: a 401 is
  not logged, and the only other line was the write side's, about a
  timestamp, from a device the bridge already knew, so a first `sudo bridge
  pair` on a fresh install 401'd in silence. `noteReadLocked` logs one Warn
  when the store first cannot be read (the path, the error, and on POSIX for a
  permission error the uid and the chown remedy) and one Info when a request
  can read it again, with no restart. Both give the token count the bridge
  answers from: `tokens=0` after the file is deleted to mend it, which unpairs
  every device, where "readable again" alone read as all clear. **A test that
  asserts only the FILE can no longer see the abort**, since #1043's re-read
  before the commit refuses the same write: the skip-persist tests passed with
  the abort removed, and count stagings now. The cause, a CLI run as root
  re-owning the file, was the runbook's "always as the service user" rule
  alone until the next bullet fixed it for this file.
- **A CLI write run as root keeps the owner of the file it replaces**
  (#1048). `sudo bridge pair`, `sudo bridge admin reset-password`,
  `sign-out-everywhere` or `login-link`, `sudo bridge cert rotate`, `sudo
  bridge update` and a `sudo bridge init --force` rewrite beside a service
  install staged their file as root, 0600, and the bridge running as the
  service user could no longer read it: #1044's 503 on every console
  request, #1047's stale token list, and a restart that could not open the
  store or load its key. Measured with the real CLI as root over an install
  uid 4242 owns: exactly six files came back root's (`bridge.yaml`,
  `tokens.json`, `adminauth.json`, `adminauth-tickets.json`, `server.crt`,
  `server.key`). `fsutil.KeepOwner` runs on each staged file while it is
  open, before the rename, as one more of the site's own steps (the Atomic
  writes rule: no shared writer), and does something only when the process
  is root: it gives the file the owner of the ENTRY it replaces (`os.Lstat`,
  so a symlink's own owner, never its target's), or of the directory for a
  new file, which every login ticket's file is since the shared tickets file
  became one file per ticket (2026-09-28, under Auth, pairing, TLS). The
  updater's `update-state.json` takes it too. **Keep, not
  refuse**: a refusal keyed on who owns the data dir turns away a setup that
  works (a container run as root over a bind mount another uid owns, whose
  files the root service created), while the replaced file's own owner is
  the evidence of who reads it. **A chown that fails abandons the write**
  (root_squash NFS gives root's files to nobody), so the service keeps a
  file it can read. The job and database CLIs (`scan`, `upscale` /
  `optimize` / `render` / `analyze`, `artwork`, `backup` / `restore`,
  `manifest`, an offline `library` change), which CREATE directories and
  sidecars rather than replace one file, were this bullet's "not covered"
  list until 2026-09-28; the next bullet covers them. Pinned per writer
  through `fsutil.SimulateRootForTest` on every host, and as root by
  `TestKeepOwnerAsRoot` and `TestCLIRunAsRootKeepsTheInstallOwner`, which CI
  skips and dido's container runs. With the six writers reverted, the
  end-to-end test names exactly those six files.
- **…and a job or database CLI run as root gives every entry it CREATES
  the install's owner** (2026-09-28, backlog B17). Measured the same way
  (`TestJobCLIsRunAsRootKeepTheInstallOwner`, as root with sox and ffmpeg
  on dido), 51 entries came back root's on main: every directory and file
  `scan` (the artwork cache, 0700), `upscale` / `optimize` / `render` (the
  variants tree and each rendition, `--force` ones included), `analyze`
  (the waveform tree, 0700, and each curve, 0600), `backup` (a snapshot the
  service's own prune cannot remove) and `variants move` create; the five
  files `restore` replaces (config, store, token file, TLS pair, root's
  0600: the service does not start); a store a root CLI creates; and the
  render scratch `1-bit-bridge-render`, root's 0700 in the shared temp dir,
  where every later DSD render of the service failed. **SQLite's `-wal` and
  `-shm` were never part of it**: its unix VFS gives both the database
  file's owner whenever it opens them as root (`robustFchown`, carried in
  modernc's transpile), measured with the store open; the one SQLite hole
  is a database root CREATES. **A new entry takes the owner of the
  directory it is created in**, #1048's rule for a file with nothing to
  replace. `fsutil.MkdirAll` / `Mkdir` make each directory through an
  `os.Root` on its parent and read the owner through that descriptor, so a
  directory only ever goes to the owner of the directory actually holding
  it, who could have made it anyway, never to the owner of a path looked
  up beforehand; one it cannot give away (root_squash) is removed again.
  Files the bridge stages take `fsutil.KeepOwner` (a cover, a curve, a
  snapshot's copies and manifest, a restored file). **A file another writer
  creates is precreated** (`fsutil.Precreate`: `O_EXCL`, the owner looked
  up BEFORE the file exists): sox fills its output with `O_TRUNC`, SQLite
  opens an existing empty database, `VACUUM INTO` writes into an empty
  file, and each keeps the owner (all measured). **Two directories take a
  reference's owner, because there the parent's says nothing about who uses
  them**: the render scratch, when created in a directory anyone may create
  entries in (other-writable and -searchable, as /tmp is), takes the
  variants directory's (`fsutil.MkdirAllShared`; only there, since the temp
  dir comes from the config the service user can write), and a `variants
  move --to` directory takes the owner of the variants directory it
  replaces, as `mv` keeps it (`fsutil.MkdirAllLike`; the command line names
  that path). A cross-device copy in the move keeps its source's owner.
  `TestJobWritersKeepTheInstallOwner` sweeps the writers
  (`internal/{atomicwrite,manifest,transcode,analyze,backup}`,
  `cmd/bridge/variants.go`) and fails on `os.MkdirAll` / `Mkdir` /
  `MkdirTemp` / `WriteFile` / `Create`, or on `os.CreateTemp` /
  `os.OpenFile(O_CREATE)` in a function with no `fsutil.KeepOwner`: **a
  new writer there makes its directories through fsutil and gives its files
  an owner**. What a child process creates the sweep cannot see; that is
  `Precreate`'s, and the root test pins it. **Not covered**: `bridge tsnet
  auth` (internal/tsnet makes `<dataDir>/tailscale` with `os.MkdirAll`,
  `assertSecureDir` then requires it to be the RUNNING uid's, and the tsnet
  library writes its state there: a root run over the service's state
  refuses, and one on a fresh install leaves a state the service's node
  refuses; read from the code, not measured). **Root's by the rule**: a
  configured `variantsDir` or `tempDir` a root CLI creates under a parent
  root owns that is not shared, where the service's own attempt would fail
  too.
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
- **`ReapOrphans`-style directory reapers must refuse an empty root, BEFORE
  anything resolves it — and not because `os.ReadDir("")` reads the working
  directory, which is false.** It fails with ENOENT on every platform, and
  `filepath.WalkDir("")` visits `""` with an lstat ENOENT (measured with
  go1.26.6 on macOS, Linux and Windows; this bullet said otherwise until
  #1031). The working directory is reached through what an empty root
  BECOMES: `filepath.Clean("")` is `"."`, `filepath.Abs("")` is the working
  directory, `filepath.EvalSymlinks("")` answers `"."`, and
  `filepath.Join("", name)` is relative to it, which let `os.RemoveAll`
  delete the working directory's `sub/` in the probe. The EvalSymlinks case
  is why `integrity.TakeSidecarInventory`'s and
  `TreeHoldsVariantSidecars`' refusals are load-bearing: both resolve the
  root before walking. Everywhere else the refusal keeps a future
  resolution from reaching the working directory, and makes an empty root
  an explicit answer rather than one that rests on ReadDir's error (in
  `backup.ReapOrphans`, an error the caller reports instead of a silent
  no-op). **A test of such a refusal must plant something in the working
  directory** (`t.Chdir` into a temp dir) and require it untouched, or it
  cannot see the change that matters, a root resolved before the listing.
  The updater's and the orphan sweeper's planted nothing and passed with the
  refusal deleted, and backup's had no test.
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
  control. (#963) The one part of that install it leaves ungraded is a
  port a rewrite moves off (2026-09-28, the "…on a certain rewrite" bullet
  below); the certificate and the data dir are graded as ever.
- **…and grades the ports it is about to SAVE, which is a different
  question** (#970). Where the install's config loads, the preflight
  grades the install's current ports (where none loads, the next
  bullet's), all of them on a run that keeps the config and those a
  rewrite keeps on one that does. For the certificate that is right and
  deliberate — init does not rewrite the pair on disk.
  For the ports it is backwards: a rewrite writes the run's own addresses,
  so an install on `:9090`/`:9091` was graded on those, passed, and was
  then handed `:7788` / `127.0.0.1:7789`. A second narrow pass
  (`doctor.RunPortChecks`) grades what the config will contain, BEFORE
  `Save`, so a refusal leaves the existing config intact — and only over
  the ports that CHANGED, since an unchanged one was already graded
  correctly.
- **…and where no install's config loads, the preflight grades the ports
  this run WRITES, from the one definition the config is built from**
  (2026-09-28). It kept init's 7788 / 7789 there, on a first install and
  over a config that does not load, whatever the run wrote. A `--public`
  run writes `:443` or `--listen-address` and 7789 or `--admin-address`, so
  another process on 7788 (a second bridge beside the operator's, say)
  refused a public first install, and a public re-init over a broken
  config, over a port neither would bind: measured with the real binary,
  exit 1 on `[FAIL] port-api :7788 in use`, and exit 0 now.
  `initAddresses` is the ONE definition of the addresses a run writes: the
  config is built from it, and the preflight is seeded with its ports,
  which `withExistingInstallDeps` still replaces with the install's own
  where the config loads (and reports whether it did). **Don't seed the
  preflight from anything else, or build the config's addresses anywhere
  else**: a divergence reaches only the second pass, which runs after init
  has made its data dir. Where no config loads that pass now grades
  nothing, and its `OwnPIDPortsUnknown` exception (the "…over a config
  that is there and does not load" bullet) stays for a port that could
  differ. **A refusal on the run's ports says so under the report**
  (`portsThisInitWrites`, printed only when a port check FAILed on the
  run's ports: where no config loaded, or on a rewrite, the next
  bullet): those lines are the run's choice, not a verdict about
  an install, and the checks' own hint names a bridge.yaml, where a run
  chooses its ports with `--listen-address` and `--admin-address`. **An
  address flag the config would refuse is refused before the preflight**
  (exit 2, `initAddressFlagsError`), which has no port to grade for it; a
  public run's was refused only at the validation before Save, after a
  preflight that graded 7788 in its place. **Both flags apply in either
  posture** since 2026-09-28: a loopback run read neither, and saved
  `:7788` / `127.0.0.1:7789` with exit 0 and no word, whatever it was
  given, `0.0.0.0` and an address with no port included. **A loopback
  run's `--admin-address` must name a loopback host**
  (`config.ValidateLoopbackAddress`, the rule `Validate` holds that
  install's adminAddress to, now one function for the file and the flag):
  its console has no login, so binding loopback is its whole trust
  boundary. Don't honour a non-loopback one there and let `Validate` refuse
  it after the preflight, and don't widen the rule for the flag.
- **…and on a certain rewrite of an install whose config loads (`--yes
  --force`, or an interactive yes), the preflight grades only the
  install's ports the rewrite KEEPS** (2026-09-28). The preflight graded
  the install's old ports all the same: an install on `:X` / `:Y`, its
  bridge stopped, another process on X, and a `--yes --force` rewrite
  moving the API off X exited 1 on `[FAIL] port-api :X in use` (measured
  with the real binary, as a public run and as a loopback one), about a
  port the saved config never binds. `portsARewriteAbandons` lists
  the install's ports the rewrite binds in NEITHER role, and
  `doctor.Deps.AbandonedPorts` answers each ok "not checked: this rewrite
  moves off :X", with no probe; the second pass grades what the rewrite
  writes in their place, as before. **Build that list over both roles,
  never per role**: a rewrite moving the console onto the old API port
  binds that port again, and a per-role list names it, so the second pass,
  which carries the same Deps while it grades that port as the new admin
  port, answers "not checked" and saves a port a stranger holds. **A kept
  port is graded as the install's, pid file and all**, and its refusal says
  it is a port this init would write (`portsThisInitWrites`). **An
  interactive run asks "Overwrite?" before the preflight** (2026-09-29,
  backlog B61), so its yes is the same certain rewrite, and a no keeps the
  config, whose ports its preflight grades, all of them, as for `--yes`
  without `--force`, and asks for no name it would discard. The decision
  is made once (`replace` / `keep` in initCmd) and read by the posture
  check, the preflight, the name prompt and the keep branch. Until then
  the question came after the preflight and the name prompt: a stranger
  on a port a yes would move off refused the run before it could ask
  (measured: exit 1, where the same flags with `--yes --force` exited
  0), and a no discarded the name just typed. #1081 had rejected the move
  as a reorder of every interactive re-init that "still leaves a
  no-answer grading the install"; a no keeps the install, so grading it
  is the right answer, and the reorder is one question moved ahead of the
  report. **It stays AFTER the library prompt**: that prompt ends a
  loopback run on a closed stdin ("input closed; aborting."), as it did
  before, where `confirm` would take its default, a no, which keeps the
  config and installs the service on nobody's say-so (a public run asks
  for no library, and reached that keep on a closed stdin before this
  change too). **And the run refuses a config that changed after it read
  it** (`configChangedSinceRead`, exit 1, the config not written): moving
  the question ahead of the preflight and the name prompt, which wait on
  the operator, put both between the decision and the write. Measured with
  the real binary: a config another init wrote while the name prompt
  waited was written over (exit 0, its name gone), where main had asked
  "Overwrite?" after that prompt, and a rewrite answered yes lost an edit
  made meanwhile (CodeRabbit's security review of #1106 named the window).
  The run keeps the bytes it read at its start (`configAsRead`, the read
  `readPriorInstall` parses) and compares them in the statement directly
  before `cfg.Save`, which `TestTheChangedConfigCheckIsTheStepBeforeSave`
  pins: its first place, before `refuseRewrite`, left the second port pass
  and the TLS load after it, and a change during them was still written
  over (CodeRabbit's next round). A refusal there can follow a first
  install's TLS mint, which the next run loads, as it loads the pair a
  failed Save leaves. What remains is Save's own staging and rename, which
  only an interprocess lock would close (#1043 declined one for
  `tokens.json`). The check also closes the change the old order let
  through: a rewrite keeps what it read, and a config written since would
  lose what its writer wrote. **Don't compare a stat**: bytes are what the
  rewrite keeps from, and a write in the same mtime tick passes a stat
  (#1043's rule for `tokens.json`).
- **The "is it us?" fallback must NOT reach a port the run is choosing.**
  `checkPort` answers ok or warn — never fail — whenever the pid in
  `OwnPIDFile` is alive and the owner probe could not rule it out (one it
  rules out FAILs: the #1029 bullet below): an unattributable port warns,
  and one merely owned by this uid is reported **ok** (the capability-bound
  `:443` case it exists for). A live bridge binds what ITS config says, so
  it cannot legitimately own a port absent from it; left set, an occupied
  NEW port read as "our bridge is still running", `HasFail` stayed false,
  and the config saved anyway — the check passing because the thing it
  guards is absent, one level in from the defect the pass exists for.
  Clear `OwnPIDFile` for a changed port, except where no config says which
  ports the running bridge binds: there it stays, confined to attribution
  (`OwnPIDPortsUnknown`, the "…over a config that is there and does not
  load" bullet below).
  `RunPortChecks` takes WHICH ports to grade, because port 0 is a legal
  value with its own verdict and cannot double as "skip this one".
- **…and clearing it did nothing on a host without lsof, because the
  verdict read which tools the host HAS** (#1021). `checkPort` ended in
  `if !portProbeAvailable() { return warn(…) }`, goreview F9's fix (#429)
  for a LIVE bridge that a host without lsof could not attribute. #640's
  liveness arm answers that case first, so all the fallback still saw was
  a port with no live pid of ours behind it: no pid file given, none
  readable, or a dead one. lsof cannot change that answer. With no pid it
  is never even asked (a logging shim counted zero calls), and a dead pid
  holds nothing for it to find. Yet its absence turned the Fail into a
  warn. In the stock `golang:1.26.6` image `bridge init` therefore
  saved an admin port another process held, on a first install, on
  #970's second pass, and on a re-init with the bridge stopped, and
  `bridge serve` then could not bind. Gating the fallback on an empty pid
  file would have fixed the reported tests and left the stopped-bridge
  re-init open. The fallback is gone, and `portProbeAvailable` with it:
  **a missing tool may explain a verdict; it never decides one.** lsof is
  Priority `standard` on Debian 13 and Ubuntu 26.04, so their minimal
  installs and container images lack it, while CI's Linux and macOS
  runners have it. `withoutLsof` sets `lsofPath = ""`, the state
  `resolveLsof` leaves on such a host, so the no-lsof leg runs on every
  runner.
- **…and grades no port at all from a config it could not load**
  (#1022). `buildDoctorDepsFor` seeds 7788 / 7789 and replaces them only
  from a config that loads, and the pid file's path comes from the same
  config. So a config that was named or found and did not load left the
  port checks grading a guess with no pid file behind it. Run by a user
  who cannot read a service-owned config (`bridge init` makes its dir
  0700), doctor warned about the config and then FAILed both ports,
  "another process owns this port", against the live bridge's own
  listeners: on every host with lsof, and since #1021 on every host. The
  runbook's validate-before-restart run on an edit with a typo got the
  same two false FAILs beside the real one, and an install off the
  defaults read "free" about ports nothing binds. For any load error,
  `ungradedConfigPortCheck` now answers ok "not checked", with the reason
  from `ConfigFile.problem` (config-file's own classifier), naming no
  port. **It runs before OwnedPorts and the bind probe**, so the answer
  cannot depend on who holds a guessed port. **ok, not warn**: config-file
  gives the one verdict about the config at #985's severity, a check that
  declines for a reason another line reports is ok elsewhere here too
  (config-dir's "not checked", tls-cert-sans, and tls-cert's since
  2026-09-29), and no consumer reads a
  port line's status (a Gemini consult argued warn for JSON consumers;
  declined on that census). **The trigger is a load error, never an absent
  config**: with nothing named or found, doctor runs before `bridge init`
  and the defaults ARE the ports init writes, so they are graded, and a
  held one still FAILs.
- **…and config-dir does not probe as a user who cannot read the config**
  (#1023). config-dir vouches that the BRIDGE can create and write the
  directory beside its config, and its two probes, a MkdirAll and a write
  of `.doctor-probe`, answer for whoever runs doctor. A user config-file
  reports as unable to read the config is not the one the bridge runs
  as, since the bridge reads it at every start. Yet beside #1022's "not
  checked" port lines, config-dir FAILed "not writable" against the 0700
  dir `bridge init` makes, so that run exited 1 and advised `bridge init
  --skip-doctor`, and `--fix` printed "created … but chmod 0700 failed"
  about a directory it had not created. Where that user may write,
  config-dir vouched ok for a directory the bridge may not. On
  `configUnreadable`, `checkConfigDir` now answers ok "not checked: the
  config in it is not readable by this user", **before the create as
  well as the write** (a config below an untraversable parent fails the
  create), and **whatever either probe would answer**. **Only that
  error**: a config that does not load was read by this user, who can be
  the bridge's, and it names its directory as well as one that loads (the
  directory comes from the PATH). A run that found nothing is by the user
  about to run `bridge init` there. Both keep the probe and its FAIL, as
  does init's own preflight (a nil lookup). **Ask which half is wrong
  before copying a decline**: the port lines decline for all three load
  errors because their INPUT is the guess, and config-dir declines for
  the one where the PROBER is. **The launcher's doctor row is the
  exception to both declines** (`ConfigFile.PreSetup`, read through
  `ungraded`; config-file keeps `problem()` and still warns). The menu
  reads any stat error as "not installed", so over another user's
  install, the root-owned dir a `sudo bridge init` leaves, it offers
  Setup, and this row exists to preview Setup's preflight. That preflight
  looks nothing up and FAILs the directory and the held default ports as
  this user. This fix's first draft declined config-dir there too, so
  beside #1022's "not checked" ports the row read "all clear." while Setup
  refused (measured on dido); a Gemini consult caught it.
  `TestMenuDoctorPreviewsSetupOverAnInstallThisUserCannotRead`
  drives both and requires the same lines. A wrong-user `bridge doctor
  --config` run now ends "all clear." (13 ok, 4 warn, 0 fail), as the
  footer does whenever nothing FAILs, below config-file's warn saying the
  install was not graded. The same consult proposed rewording the footer
  for every run with warns, which changes every report. That was not taken
  here.
- **…and over a config that is there and does not load, init recognises
  the bridge it replaces by the pid file in the data dir it WRITES, and
  only where the probe SEES that bridge on the port** (#1027).
  `withExistingInstallDeps` returned early on any load error, so the
  preflight graded 7788 / 7789 with no pid file: a bridge live on its
  defaults whose config then broke FAILed both port checks, and `bridge
  init --yes --force`, the run that replaces that config, refused (#1022
  measured it). A public re-init refused in the second pass instead,
  because "changed" was measured against init's defaults and #970 clears
  the pid for a changed port (since 2026-09-28 the preflight grades a
  public re-init's own ports there, the bullet after #970's, in this
  mode). init always writes `<dir>/data` and serve
  records `<dataDir>/server.pid`, so the bridge the re-init replaces is
  known without its config wherever the data dir did not move. That pid
  file is wired with `doctor.Deps.OwnPIDPortsUnknown`, which confines the
  "is it us?" ladder to ATTRIBUTION (`checkChosenPort`): a held port is
  excused only when the probe sees the recorded pid listening on it.
  **Liveness, the uid arm and a failed probe excuse nothing there**,
  because no config says which ports that bridge binds: an install that
  had moved off the defaults (because something else holds 7788, say)
  has a live bridge on its own ports while another process holds the one
  init writes. With the full ladder behind that pid file the re-init
  saved `:7788` without a word, and the restarted bridge died on `bind:
  address already in use` (row B on dido, with and without lsof): #970's
  defect. The second pass keeps that pid file rather than clearing it
  (though since 2026-09-28 no port of the run reaches it there). **A
  MISSING config stays a first install**, with no pid file: nothing there
  shows an install, and a leftover pid can be stale or recycled (a Gemini
  consult agreed). **Attribution does not depend on lsof on Linux**:
  where none resolves, `isPIDListeningOnPort` reads the listener's inode
  from `/proc/net/tcp{,6}` and looks for it among `/proc/<pid>/fd`'s
  `socket:[N]` links (`pidListensOnPort`), the tables lsof itself reads.
  Without that, a host with no lsof refused the bridge's own port that a
  host with lsof accepted (NC-F, one set of facts, two verdicts). Its
  limits are lsof's: another user's process, or a `cap_net_bind_service`
  binary (dumpable=0), keeps its descriptors from a non-root observer, and
  the refusal's hint names the recorded bridge and, where the probe could
  have missed it, says to stop it first (the next bullet, #1028).
  **An ok is not proof of attribution**: on Linux the uid arm answered ok
  for a test's own listener until #1030, so `TestPortCheck_OwnPIDMatches`
  asserts the "bound by our own bridge" summary; with the `/proc` path
  removed it had passed on the uid arm.
- **…and a live recorded bridge the probe did not see is explained by what
  the probe SAW, never by the one case the arm was written for** (#1028).
  #640's liveness arm said "pid attribution blocked — capability-bound
  binary" on its ok line, and blamed `cap_net_bind_service` and dumpable=0
  in its warn hint, for every live recorded pid the probe did not name,
  and `checkChosenPort`'s refusal gave the same capability as its example.
  Measured on main, that is true of one shape, a bridge granted the
  capability. It was printed for a bridge running as another user (dido,
  both images; the Mac, where lsof run without root lists only that
  user's processes), for a port whose holder lsof had just NAMED, and on
  macOS and Windows, which have no such capability. The case reported, a
  plain bridge in the no-lsof image, already read "bound by our own
  bridge" since #1027's `/proc` attribution. `isPIDListeningOnPort` now
  returns an `ownerSighting` beside found and the error, written by the
  probe that looked (`lsofSighting`, `procSighting`,
  `listenerTableSighting`): what it saw, what it cannot see as this user
  (`blindSpot`, per platform: another user or group, or dumpable=0, on
  Linux; another user, for lsof on macOS; nothing on Windows, whose
  listener table carries every listener's pid), and whether what it saw
  RULES THE PID OUT. Only a probe that saw everything there was to see
  may: Windows' listener table, or `/proc` reading every one of the pid's
  descriptors against every socket table. **Never lsof**: it lists only
  the processes this user may inspect, so naming another holder does not
  exclude a hidden bridge listening on the same port at another address,
  and a socket table that is there and did not read leaves the pid
  possible as well (both CodeRabbit on #1028, the second by a flag rather
  than the error it proposed, which would have moved a verdict). The ok
  summary gives the account. Both hints say to stop the holder when the
  pid is ruled out, and keep the hedge and the blind spot when it is not;
  the zero `ruledOut` is the hedge, the safe fallback for a probe that sets
  nothing. **#1028 left the verdicts untouched, and pinned them apart from
  the text**: `TestPortVerdictsDoNotDependOnTheAccount` walks both ladders
  over every lsof answer and passed on main before the accounts existed,
  and `TestPortVerdictsReadOnlyRuledOutFromTheSighting` (then
  `…IgnoreTheSighting`) feeds every kind of account through
  `ownerProbeFunc`, a ruled-out one included. Only Windows and Linux make
  one for real, so without the seam a verdict keyed on it passed every
  test on a Mac (NC6). Since #1029 one verdict reads the sighting, and the
  two tests pin that it reads `ruledOut` and nothing else. #1021's rule,
  that a missing tool may explain a verdict and never decides one, has a
  second half: **the explanation is the one the probe established.** Root
  gets no blind spot, because inside a Docker container it lacks
  CAP_SYS_PTRACE and "root sees everything" is false too. #1028 found one
  ok wrong and left it for its own change, which is the next bullet.
- **…and a live recorded bridge the probe RULES OUT is not ours on that
  port, so the port FAILs** (#1029). The uid arm (#640) answered ok on
  Linux for a held port whose listener runs as this user, whenever the
  recorded pid was alive and the probe had not named it. It exists for the
  capability-bound bridge, whose descriptors no unprivileged probe can
  read, and it answered the same way for a port ANOTHER process of this
  user holds. #1028's row L4 is that shape: a bridge still running on its
  old ports, with its config edited to a port something else holds. It
  read ok, `bridge doctor --config` exited 0, and the restart could not
  bind, which is #970's defect in the ordinary ladder. The sighting
  already says when what the probe saw excludes the pid (`ruledOut`):
  `/proc` read every one of its descriptors and none is a listener on the
  port, or Windows' table names every listener and the pid is not among
  them. Such a pid holds no listener on that port on any address, so
  `checkPort` FAILs it, AHEAD of the uid arm. That is the fact the
  no-live-pid branch below it already FAILs, and `checkChosenPort` already
  refused a ruled-out pid. **FAIL, not warn**: `bridge doctor` exits 1
  only on a FAIL (a warn prints "all clear." and exits 0), and `bridge
  init`'s preflight refuses only on one. A warn would have fixed the
  wording and not the defect. On main, the re-init over L4 saved the held
  `:7788`. **A missing tool must not decide it, so on Linux the probe asks
  `/proc` after lsof** (`procSecondOpinion`). lsof lists only the
  processes this user may inspect, so its miss never rules a pid out, and
  keyed on `ruledOut` alone (NC3) L4 FAILed where lsof is absent and
  passed where it is installed. After a CLEAN lsof miss the probe reads the
  recorded pid's descriptors through `/proc`, under the kernel check
  lsof's readlinks meet. A match is a match, a ruling-out is joined to
  lsof's account, and anything else leaves lsof's account as it was. A
  failed lsof asks nothing more. On dido, L4 and L5 now FAIL in both
  images where main let both through (an ok and a warn, both exit 0), the
  capability-bound L2 keeps its ok,
  and root in a container (no CAP_SYS_PTRACE) rules nothing out. **Two
  shapes stayed open.** One, L4 over a capability-bound bridge (the NUC's
  shape, row L6), is closed by the next bullet (#1030). The other, L4 on
  macOS (row ML4), is closed by the last bullet of this chain (#1034),
  and NOT by the pid's uid, which this sentence proposed until
  2026-09-26: an lsof a sandbox blinds exits 1 as a clean miss does, and
  the uid rule FAILed a sandboxed doctor's own bridge. **A test that
  records a pid of its own choosing forces what the probe says about it**
  (`procOwnerFunc`, `withUnattributedMiss`). On Linux, pid 4242 may be a
  readable process of the test's own user, and Windows' table rules it
  out, so a test left to the host was grading a different arm. **And it
  records `standInPID`, never a literal** (2026-09-29, backlog B106):
  4242, or 4243 when the test binary is 4242. Every such test binds its
  port in the test process, so a probe left to the host names that
  process as the holder, and where it IS the recorded pid the check
  answers "bound by our own bridge": a freshly booted macOS CI runner
  handed out pid 4242, and `TestPortCheck_DeadPIDStillFails` and a
  `TestChosenPortIsExcusedOnlyByTheRecordedBridgeSeenListening` row, which
  left the probe to the host, failed that way (a no-lsof row would have on
  Linux; main as pid 4242 in a pid namespace fails all three). Left to the
  host, the dead-pid test was also blind on Windows, whose table rules the
  stand-in out: a `checkPort` that ignored liveness still FAILed its port.
  `TestStandInTestsHoldWhenTheStandInIsThisProcess` finds every test that
  reaches `standInPID` in the source, runs them in a child whose stand-in
  is its own pid, the collision on every run, and refuses a
  `writePIDFile` given a pid literal. **A pid a scripted lsof lists as
  ANOTHER process dodges the stand-in too** (`otherThanStandIn`): that
  child can be pid 1305, and with the stand-in at 1305 the fixed "other"
  pid named the recorded bridge, failing the lsof-account tests
  (CodeRabbit on #1115).
- **…and a bridge no probe can read is ruled out by the port's OTHER
  holders, and the uid arm answers only for a listener no readable
  process holds** (#1030). Row L6, #1029's open shape: a bridge granted
  `cap_net_bind_service` runs with dumpable=0, so as its own user
  `/proc/<pid>/fd` is root's and EACCES, and nothing rules it out. With
  it live on its old ports and its config edited to a port ANOTHER
  process of the same user holds, the uid arm answered ok for that
  process's listener (`… lsof lists pid 579 listening on this port`),
  `bridge doctor --config` exited 0 and the restart could not bind, in
  both dido images. **The census**: where the recorded pid's descriptors
  do not read in full, `procSighting` walks every `/proc/<pid>/fd` this
  user CAN read (`socketHolders`), and every socket listening on the
  port held by one of them, other than the pid, rules the pid out
  (`heldByOthers`, now `listenersNotOf`'s first half), so #1029's arm
  FAILs it. An inode names one socket, and a listener of the pid's own is
  held by it alone, which nothing here reads, so it would have no holder. **The one shape a census cannot
  see is a socket the pid SHARES with a readable process, and the bridge
  shares none**: every listener is its own `net.Listen` (close-on-exec),
  it consumes no inherited fd (`internal/supervision` only READS
  `LISTEN_FDS`), hands none on, and `pidfd_getfd` on a dumpable=0
  process needs CAP_SYS_PTRACE. Windows' listener table rules out on the
  same terms, since a duplicated socket keeps the binder's pid. **Adding
  socket activation or a listener handoff to the bridge breaks this
  premise; revisit the census in the same change.** A listener with no
  readable holder (another user's process, root's, io_uring, another pid
  namespace) left the pid possible here, as a table that did not read
  still does; the next bullet rules it out by the uid that created it.
  Root in a container without CAP_SYS_PTRACE lists every fd directory
  and reads no other uid's link, so the holders change nothing for it
  (R6 stays a warn).
  **The uid arm** (`hiddenListenerOf`) now needs both marks of that
  bridge: the listener was created by this uid, AND no process this user
  can read holds it. One a readable process holds is that process's. The
  census could not rule out where another listener has no readable holder
  (L6m: another user's `[::1]:7788` beside the same-user holder), and
  there the old arm said ok. It was a warn after this change, exit 0, and
  FAILs since the next bullet's. **Both halves are
  needed, and the census is the one that fixes L6**: with it disabled
  (NC1) L6 reads warn, exit 0, from the refined arm alone. **Every
  pid-numbered `/proc` read first checks `/proc/self` against `getpid`**
  (`procOfAnotherPIDNamespace`, proctest's rule): under another pid
  namespace's `/proc`, `<pid>` is some other process, and the recorded
  bridge can be a "holder" under another number (a Gemini consult called
  the guard necessary). **The uid arm deliberately skips that guard**: it
  reads no pid, and a `/proc` whose socket tables read at all is this pid
  namespace's or an ancestor's (`/proc/net` is `self/net`), which omits no
  process of this one. Measured on dido: under `nsenter -m` into a
  container both tables are unreadable, and under `unshare --pid --fork`
  the ancestor `/proc` lists every host process. CodeRabbit proposed the
  guard there, and it would only turn L2 into a warn. **The walk is cheap**: 1.1 to 1.8 ms as a user
  over dido's 290 processes, 11 to 15 ms as root without
  CAP_SYS_PTRACE. **A test stands the bridge in without setcap**:
  `prctl(PR_SET_DUMPABLE, 0)` in a re-run of the test binary gives the
  same root-owned 0500 fd directory (measured), so CI runs L2 and L6 on
  the real kernel (`hidden_bridge_linux_test.go`). **A holder this user
  cannot read** (L7, another user's; root's daemon on the NUC is this
  shape) still warned, exit 0, until the next bullet.
- **…and a listener another UID created is not the bridge's, so a
  holder this user cannot read FAILs too** (#1032). Row L7, left by
  #1030: the capability-bound bridge live on its old ports, its config
  edited to a port held by a process of ANOTHER user, or by root's
  daemon (L7z, the NUC's likelier shape). No readable process holds that
  listener, so nothing ruled the bridge out: `bridge doctor --config`
  warned, exit 0, `bridge init --force` saved the port, and the restart
  could not bind (dido, both images, and as root: R7). **The census's
  second half** (`listenersNotOf`): a listener no readable process holds
  is another's when the uid that created it, the tables' uid column, is
  not the recorded pid's fsuid, the FOURTH value of `Uid:` in
  `/proc/<pid>/status` (`pidFSUID`, read behind the `/proc/self` guard,
  NC4). That file reads where the pid's descriptors do not (dumpable=0,
  to its own user and to container root). **The fsuid, not the real or
  effective uid**: a socket is stamped with its creator's fsuid
  (measured: root that called `setfsuid(1234)` and listened shows
  `Uid: 0 0 0 1234` and row uid 1234), and only `fchown` re-stamps one.
  **The premise is the bridge's, checked in the binary, not the
  source**: no setuid-family call, `AllThreadsSyscall`,
  `ParseUnixRights` or `net.FileListener` is linked into the linux build
  (`go tool nm`), nothing fchowns a socket, and every thread carries the
  leader's creds (one distinct `Uid:` line across 16 to 20 threads).
  **Adding a privilege drop, `setfsuid`, socket activation or a listener
  handoff breaks it; revisit the census in the same change.** **No
  special case for the overflow uid**: both files render a uid through
  the reader's user namespace, so DIFFERENT values always name different
  uids, 65534 included, and only EQUAL ones are ambiguous (two unmapped
  uids both render 65534), which `createdByAnother` never counts as
  another (measured in two user namespaces on dido; treating 65534 as
  unknown turned two correct rulings-out into warns, NC7). A readable
  holder is named ahead of a creator uid, since it is what an operator
  stops. **hidepid=1 or 2 hides a dumpable=0 process's status from its
  own user too**, so L7 stays a warn there (measured, with a hidepid=0
  remount as the control). A pid file naming a recycled pid of another
  user now FAILs a port whose listeners that uid did not create, the
  trust #1029 already gave a recycled pid of the same user (P7a, P7b).
  A hidden holder of the bridge's OWN uid (row L6h) was left open here;
  the next bullet closes it wherever that holder runs in another
  cgroup. The L7 kernel test takes root (it runs the holder,
  the bridge and doctor as two other uids), so it runs on dido and skips
  in CI, where the fixture tests carry the rule.
- **…and a listener created in a cgroup that does not nest with the
  bridge's is not the bridge's either, so a hidden holder of its OWN uid
  FAILs too** (#1033). Row L6h, left by #1032: the capability-bound bridge
  live on its old ports, its config edited to a port held by ANOTHER
  hidden process of the same uid (a second capability-bound binary of the
  service user in its own unit, one of its processes in another group,
  one in a container, or one hand-started from another login). No
  readable process holds that listener and it carries the bridge's uid,
  so the uid arm read ok, `bridge doctor --config` exited 0, `bridge init
  --force` saved the port, and the restart could not bind (dido's host,
  the real binary in systemd units, a container and two SSH sessions,
  with lsof and without). **The census's third accounting**
  (`cgroupsNotOf`, after the readable holders and the creator uids): the
  kernel's socket diagnostics (`NETLINK_SOCK_DIAG`, what `ss --cgroup`
  reads, `listenerCgroups`) give the cgroup each listener was created
  in, to any user and for root's sockets too, as the id of that cgroup's
  directory on the cgroup2 mount (its inode: 60 of 60 on dido, equal to
  `name_to_handle_at`'s id), and `/proc/<pid>/cgroup`, readable where a
  dumpable=0 process's descriptors are not, the one the bridge runs in.
  **A socket keeps the cgroup it was created in**: the kernel commit
  that added the attribute (6e3a401fc8af, Linux 5.8) says so, and a
  holder root moved to another unit still reported its first. So a
  listener created in a cgroup that neither is the bridge's, contains
  it, nor sits below it is another process's, **given the premise**: the
  bridge creates its listeners in the cgroup it runs in and never moves.
  Nothing in it writes `cgroup.procs` or asks systemd to move it, and
  **systemd moves no running service** (systemd 259: a `Slice=` edit plus
  daemon-reload left the process in place until a restart); root does,
  and a bridge moved to another cgroup after it listened FAILs its own
  port (row CM, the hint naming both cgroups), while one moved into a
  child of its own nests and stays ok (CMd). **Adding socket activation
  (systemd creates the socket in the `.socket` unit's cgroup), a
  listener handoff, threaded cgroups or a self-move breaks it; revisit
  the census in the same change.** **Nesting counts nothing**, in either
  direction: 5.8 to 5.14 (and 5.10.y before 5.10.226) stamp every new
  socket with the ROOT cgroup once net_cls or net_prio v1 tagging is in
  use (8520e224f547), and threads in threaded cgroups or a delegated
  subtree put a process's own sockets below its cgroup. **Nor does
  anything this process cannot see**: no attribute (before 5.8, or no
  `CONFIG_SOCK_CGROUP_DATA`), hidepid, a v1-only host, a path outside the
  mount (`/../x`, how a container renders a host cgroup; `unifiedCgroup`
  refuses it BEFORE it is joined onto the mount point, which would climb
  out of it, NC10), a deleted cgroup, one past the walk's budget of 4,096
  directories. So in a container, where every process shares its one
  cgroup, nothing changes. The kernel ends a dump it refuses with
  `NLMSG_DONE` carrying a negative errno, not with `NLMSG_ERROR`
  (captured), and the parser reads that status; its tests run on every
  platform against datagrams captured from a real kernel in a container's
  own network namespace, so no host address is in them. **Still open: a
  hidden holder in the bridge's OWN cgroup** (row C6s: a second process
  the bridge's unit starts, one started from the same login session, any
  process of the bridge's container). The runbook's answer is the moved
  port's own line: the running bridge is still on the old port, so
  anything but `free` on the new one is another process. The root-only
  kernel test makes two cgroups under its own, so it runs on dido's host
  and skips in CI and in containers (read-only cgroupfs); the kernel-fact
  tests (`listenerCgroups` against a real listener, a dumpable=0
  child's `/proc/<pid>/cgroup`) run as any user wherever a cgroup2 mount
  holds the test's cgroup (measured in both dido images, as uid 1000 and
  as root).
- **…and on macOS a live recorded bridge that lsof lists listening
  elsewhere is ruled out by that LISTING, never by its uid** (#1034). Row
  ML4, left open by #1029: nothing on macOS set `ruledOut`, so a bridge
  live on its old ports, its config edited to a port another process holds
  (1Password's, this user's; or root's Tailscale extension, row ML7),
  warned, `bridge doctor --config` exited 0, and the restart could not
  bind. After lsof's clean miss the probe now asks lsof for the recorded
  pid's own TCP listeners (`lsof -nP -a -p <pid> -iTCP -sTCP:LISTEN -F n`,
  `portowner_darwin.go`), the macOS twin of Linux's `/proc` read, through
  the same seam (`procOwnerFunc`, which takes the context now, since this
  look runs lsof) and the same merge (`procSecondOpinion`). Listeners, none
  on the port, rule the pid out (`ownListenersSighting`): XNU's check
  (`proc_security_policy`) is made per process, not per descriptor, so a
  pid lsof lists at all is one whose descriptors it read. One on the port
  is a match. Nothing listed says nothing, which is what M1 (a recorded
  pid of root's, to a user), a recorded pid with no listener (M2, a
  recycled pid), a bridge of another user and a blinded lsof all look
  like, so they stay warns. **Not the pid's uid** (`kern.proc.pid`), which
  the #1029 bullet proposed: lsof as uid 501 read all 614 processes of
  effective uid 501 and none of the other 211 (a later census: all 91
  app-sandboxed and all 107 hardened-runtime ones), but an lsof a sandbox
  denies `process-info-pidinfo` or `-pidfdinfo` (an App-Sandbox terminal's,
  an agent's) exits 1 with no output and nothing on stderr, byte for byte
  a clean miss, and the uid rule then FAILed a sandboxed doctor's own
  bridge on its own port, "stop the process that holds the port" about the
  bridge's own listener (NC3b; `TestABlindedLsofRulesNoBridgeOut` pins it).
  **Not `netstat -anv`** either: run by a Go process, from a shell or as a
  launchd job, it printed no TCP row at all, so #1028's note that it names
  every listener's pid without root holds only where a shell runs it.
  **The `-a` is load-bearing**: without it lsof ORs `-p` with `-iTCP` and
  prints every process's listeners beside all of the pid's files, which
  the reader refuses by its pid check (NC2), so a missing `-a` costs the
  ruling-out and never invents one; any name the reader cannot parse voids
  the whole listing for the same reason.
  `TestPortCheckFailsAPortTheLiveBridgeListensBeside` records a child that
  listens and FAILs on all three platforms; the tests that record the `go
  test` parent, which holds no listener, still warn on macOS
  (`parentListens` skips a run whose parent does listen).
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
- **A doctor hint is read by an OPERATOR, so no string in `internal/doctor`
  names a `Deps` field** (2026-09-28). Three hints were notes for whoever
  calls the package, and each reached operators, measured with the real
  binary: `pass Deps.port-apiPort` under "no port set" for every config or
  init flag naming `:0` (the answer the bullet above says `checkPort` "has
  always had"), `pass Deps.DataDir so doctor can inspect cert state` on
  EVERY `bridge doctor` run before `bridge init`, and `pass Deps.ConfigDir
  …` on one with no home directory. Each is a sentence about the install
  now (`portZeroHint`, `noConfigDirHint`), severities unchanged there; the
  tls-cert one, `noDataDirHint`, went with its warn on 2026-09-29, when
  the pre-init report learned to grade init's data dir (the doctor bullet
  on it, below). `TestNoStringInThisPackageNamesADepsField` walks the package's
  string LITERALS by AST, so the docblocks that discuss the fields are not
  read, and a hint built from pieces (`"pass Deps."+name+"Port"`) is caught
  by its first; a floor of files and literals keeps a sweep that read
  nothing from passing. A caller's mistake (a zero `Deps`) and an operator's
  choice (`:0`) arrive at the same branch, and only the operator reads the
  report.
- **`bridge init` decides every refusal BEFORE it writes `bridge.yaml`, and
  keeps what an install already has: its TLS pair and a public install's
  admin ACCOUNT** (#1038). A public `--force` re-init over a public install
  saved the config, kept the cert, and then exited 1: `MintInitial` refuses
  a store that holds an account, and initCmd asked the store only after
  `Save`. So every public re-init that rewrote the config reported a
  failure about an install it had already changed, and never reached the
  service install or the footer (#1027's row C). **Re-running init is not
  a request to rotate**, for the password as for the cert; `bridge admin
  reset-password` is. So
  `keepOrMintAdminCredentials` keeps the account and SAYS so, in a box that
  names it, and mints only into a store with no account (no file, or an
  empty one). **Don't mint over a store that does not load** (that destroys
  what the file still holds, and serve refuses the same file), and **don't
  keep silently**: an operator expecting the "shown ONCE" box goes looking
  for a password that was never made. The store (`openInitAdminAuth`) and
  the TLS pair (`LoadOrGenerateWithOptions`) are both READ before `Save`,
  and each refusal says the config was NOT changed. The preflight's
  tls-cert FAIL covers a broken pair only when it runs. A new check init
  can refuse on goes before `Save` too; after it, only the mint's own write
  and the service install may fail. **Which pair is kept, and what else a
  rewrite keeps, is the next bullet's**: this one said "the data dir's
  only" until 2026-09-27, an open defect #1038 measured and left.
  `TestInitPublicReinitKeepsTheAdminCredentials`
  compares every file byte for byte and verifies the first run's password,
  because an exit-code test also passes a "fix" that rotates. The other
  defect found there: `box()` cuts a line longer than its 51-column body in
  the middle, and the "shown ONCE" box printed `The plai... is not stored
  anywhere.` on every public install
  (`TestAdminCredentialBoxesAreNotTruncated`).
- **A rewrite replaces the SETTINGS, never the INSTALL.** `bridge init
  --force` (or "Overwrite? y") keeps `dataDir`, `tlsCertPath` and
  `tlsKeyPath` always, a loopback install's `customEndpoints` on a loopback
  rewrite, and `libraryRoots` when the run names no `--library` (#1040),
  and `libraryName` when it names no `--name` (#1041). It
  built `bridge.yaml` from `baseConfig` and the flags and kept nothing.
  Measured with the real binary: a config naming its pair outside the data
  dir lost it, init minted a new one there and printed it as "Stable across
  restarts", and `bridge serve` then presented `13:BE:B2:…` to devices that
  had pinned `04:AF:CB:…` (read off the socket); a config naming another
  `dataDir` was pointed back at `<dir>/data`, stranding its `tokens.json`; a
  config with a misspelt key lost its pair the same way; `customEndpoints`
  vanished, and iOS replaces its alternates with `/v1/health`'s on every
  fetch (`BridgeEndpointSelector.update`), so every device lost that route;
  a public rewrite without `--library` wrote `libraryRoots: []`. **The rule
  is who could give a value back.** The pin and the tokens only a re-pair
  restores, so they are kept always. The endpoints and roots, which init
  never asks for, are kept where the run writes nothing in their place: a
  `--public` rewrite writes the domain's endpoint (a posture change starts
  from the new posture's, since the old list names the other posture's
  addresses), and a `--library` replaces the roots. The preflight grades
  only a root the run names: a kept public root may be a mount that is not
  up, `checkLibraryRoots` FAILs a missing one, and public init must not
  need the mount (init's public-mode note). **So is the name, where the
  run names none** (#1041), in either posture, since a name names no
  address. The hostname a run without `--name` took is only init's guess
  for a first install: measured, `My Library` became `Macbook.local` in the
  file, in `/v1/health` (unauthenticated) and in every new pairing QR's
  `name=`, the name a newly paired phone takes, and the interactive prompt
  offered the hostname as its default over the install's own. The prompt
  offers the install's name now, `--name` replaces it, and the kept list
  shows it only where a first install would have taken another, so the
  rewrite of an install named for its host prints nothing. **A config
  giving no name, or a blank one, keeps none** and takes the hostname, as
  before: `Load` serves it `DefaultLibraryName` (a blank one only since
  #1042: before it, `Load` served a blank name as written; blank means blank
  to `config.TrimLibraryName`), a fallback nobody chose,
  and keeping that listed it as kept from a config that never held it (a
  Gemini consult caught the first draft doing so). Don't default the name
  in `readPriorInstall` the way it defaults `dataDir`, whose `Load` default
  is init's own. The rest (features, cadences, the ports, which #970
  GRADES rather than keeps) is the documented overwrite. **A kept endpoint
  that names the port the rewrite moves the API off is kept, and named in
  a warning** (`warnKeptEndpointsOnAMovedPort`, 2026-09-29, backlog B61):
  measured, an install on 127.0.0.1:X listing `https://nas…:X`, rewritten
  onto :Z, kept it without a word, and the restarted bridge's `/v1/health`
  advertised it beside its :Z addresses, an alternate every device tries
  and fails over past. An endpoint names a port by its own or by its
  scheme's (443, 80); `:0` names none. **Never drop or rewrite it**: the
  endpoint is the operator's word for what reaches the bridge, its port may
  be a router's or a proxy's (the endpoint-synthesis rule under The wire
  contract is the same fact), and a forward from it is right again once
  pointed at the new port. **Read what is kept from the FILE
  (`readPriorInstall`), never through `config.Load`**: Load applies
  `BRIDGE_*`, so keeping its values writes the caller's environment into the
  YAML (`writeAutoInitConfig`'s rule), and its unknown-key refusal would cost
  a misspelt-key config (#1027's row C) the pair it names. init LOADS the
  pair the saved config names through `resolveCertPaths`, which serve now
  calls too, so the fingerprint it prints is the one serve presents; over a
  config that does not load, the preflight grades that pair as well.
  **Refused before anything is written**: a config this user cannot read;
  one that does not parse, from which nothing a rewrite keeps can be read
  (**never "proceed when init's data dir holds a pair"**, #1040's first
  version: that pair being there does not make it the one the install
  serves; moving the file aside is the remedy, and init then keeps a pair it
  finds there as on a first install); one naming half a
  TLS pair (**never "keep the paths only when both are set"**, a bot's fix
  on #1040: dropping the half a config names serves the data dir's pair or
  mints one, the same pin break); and one setting
  `demo.enabled` or `deployment.managed*`, postures init never writes, whose
  rewrite dropped the demo's pinned token from every shipped app or handed a
  tenant's withheld controls to its console. A fresh start is moving
  `bridge.yaml` aside, never `--force`.
  `TestInitRewriteKeepsTheTLSPairItsConfigNames` and its siblings run the
  real initCmd twice over one `--dir` and assert the exit code, the saved
  config and the fingerprint at the paths `resolveCertPaths` finds.
- **A flag the run would not write is never dropped without a word, and
  a posture flag stops no first install** (2026-09-29, backlog B61).
  `--domain`, `--email` and `--admin-tls-proxy` describe a public install
  and were ignored without `--public`, silently: measured, a loopback first
  install given all three exited 0 and saved none, and a `--yes --force`
  rewrite of a PUBLIC install given `--domain` and `--admin-tls-proxy` but
  not `--public` exited 0 with a loopback config, the endpoint every paired
  device dials dropped. `warnIgnoredPostureFlags` decides, after the
  overwrite decision and before the preflight: **a rewrite of a PUBLIC
  install is refused, exit 2, the config untouched** (the flag says the
  operator meant public, and the rewrite would make that install loopback,
  dropping the endpoint); **a first install, and a rewrite of a loopback
  install, warn and go on** (each writes a working loopback install and
  loses nothing; refusing the loopback rewrite, the first draft, would fail
  a script that rewrites with `--yes --force` on every run, passing these
  flags, on its second run, where its first only warned); **a run that
  keeps the config says nothing more** (every flag goes unused, which
  "keeping it" says, and an idempotent `bridge init --yes` re-run must go
  on working). **Don't refuse a run over one that loses nothing by it**:
  a refusal is for the rewrite that would cost the devices their route.
  `--email` with `--public --admin-tls-proxy` is unused too (the bridge
  then runs no ACME client) and warns. Every line names the flags, never
  their values: a `--domain` can carry a password, which `--public`
  refuses without echoing (B54).


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
  handle is a gate, and it stops being one.** **Every pass stopped short of
  the console's batch** (2026-09-28): #852 restored the two /v1 handlers above,
  #878 (the 2026-09-09 LOUPE) restored `POST /v1/upscale/batch`, and
  `POST /api/upscale/batch` still checked only `BatchCoordinator == nil`, a
  coordinator runServe builds on every bridge. Measured on main with the real
  `serve` and upscale off (the default): 202, `enqueuedCount: 2`, two
  renditions written, while `/v1/health` said `upscaleEnabled: false`; any
  loopback process or public-mode session could do it. The submit now reads
  `admin.Deps.UpscaleActive` first, which runServe wires to `upscaleActiveFn`,
  the closure `WithUpscale` hands /v1 (`TestConsoleBatchGateIsTheV1UpscaleGate`
  requires the same identifier; a nil gate reads as off), and the optimize kind
  reads `OptimizeActive` too, as the projection endpoint does: /v1 refused that
  kind with the CarPlay switch off and the console accepted it. Both read it
  through `Server.optimizeActive` since 2026-09-28, the predicate the player's
  variant summary serves, so "Generate CarPlay" is disabled where this refuses
  (the Admin console section). Both come before
  the scope, and `TestEveryBatchSubmitReadsTheUpscaleGateFirst` sweeps every
  `BatchCoordinator.Submit*` caller by AST. **The console's delete stays open on
  purpose**, as do cancel, list and the failure retry: the owner's call, so an
  operator who switched upscaling off can still reclaim the disk, where
  `DELETE /v1/upscale/variants` refuses. None of them starts sox work.
  **The READS refuse on the store alone, and that is the contract** (backlog
  B108, 2026-09-29): `/v1/download?variant=`, `/v1/waveform`, `/v1/spectrum`
  and `GET /v1/upscale/batches` answer with their feature off as with it on
  (the list on a demo bridge too), and the manifest's analysis fields have no
  gate; only its `variants` are stripped. A switch stops NEW work and
  withdraws nothing made: a client drops a rendition or a curve it gets a 404
  for, and a delta sync never carries the stripped `variants` (the switch
  bumps no `indexed_at`), so a phone that listed a rendition keeps asking for
  it. PROTOCOL.md said 404 (and 503 for the list) until then, and the owner
  changed the spec to match the code. **Don't gate these reads on the live predicate**:
  `internal/api/feature_off_reads_test.go` pins each route over stubs and
  `TestServeWithItsFeaturesOffServesWhatItMadeBefore` the wiring, through a
  real serve.
- **…and a value runServe DECIDES from the config while it builds Deps is a
  boot snapshot, however live its reader is** (2026-09-28).
  `admin.Deps.ProjectedSize` and `AvailableDiskSpace` were function literals
  called in place that answered nil unless `upscale.enabled` was true at
  that moment, and the projection handler read nil as "feature off".
  Measured with the real `serve`, flipping `upscaleEnabled` through
  `PATCH /api/settings`, which answered `live` both times: `GET
  /api/library/browse-projection` answered 503 `upscale-disabled` after the
  switch went on and 200 after it went off, while `/v1/health` followed it,
  and with no sox on PATH it projected while health said off. The same nil
  made `GET /api/upscale/variants-dir` report `freeBytes: 0`, "0 B free" on
  the Library roots page, on every bridge booted with upscaling off. The two
  sat directly above `OptimizeEligible`, whose own comment records this fix
  for `optimizeEnabled`. Both helpers are now wired on every bridge, and the
  handler refuses on `s.upscaleActive()`
  (`Deps.UpscaleActive`, the batch's gate and /v1's, which
  `TestConsoleBatchGateIsTheV1UpscaleGate` pins) before the target read, the
  walk and the disk probe. **A function literal called where a Deps field or
  a `With*` option is written decides on a WIRING fact (a nil handle), never
  on the config**: `TestNoDependencyIsDecidedFromTheConfigAtConstruction`
  sweeps runServe for one that reads `cfg`, `cfgHolder`, `liveCfg()` or a
  live predicate outside the closure it returns. **A `live` row in
  `ops/settings-apply-semantics.md` is a claim about every consumer, and
  `TestMatrixDocMatchesWhatTheHandlerReports` checks only the REPORT**: it
  passed throughout. The consumers are checked by a boot test that flips the
  field through the PATCH and asks each one
  (`TestServeProjectionFollowsTheLiveUpscaleGate`, which answers serve's sox
  probe itself, `serveOpts.soxProbe`, so the switch-on leg means something
  on a host without sox, and checks health agrees before it compares; it
  put a stand-in sox first on PATH, POSIX only, until B105, under Build,
  CI, and test discipline).
- **…and a LIVE reader of the flag alone splits the gate too: every consumer
  that answers "is upscaling on" reads `upscaleActiveFn`, the flag AND a
  usable sox** (2026-09-28). Four read `upscale.enabled` live and without the
  sox half: the console's tile (`Deps.UpscaleStats` / `UpscaleBusy`, and the
  Settings chip that takes its verdict from it), `/v1/upscale/stats`'
  `enabled` (whose PROTOCOL.md row says "matching `/v1/health.upscaleEnabled`"),
  the auto-optimize sweeper and its Jobs card, and `Deps.OptimizeActive`. On
  a bridge without sox, health said off and they said on, and the sweeper
  WORKED on it: its decodability check reads a failed probe as "can decode",
  so it queued every eligible track, each job failed with a WARN and struck
  its file, and the third strike suppresses a file from pre-generation for 30
  days. Measured with the real binary, six hi-res tracks, a 20 s cadence: 18
  jobs, 18 WARNs, all six suppressed within 40 s; restarted WITH sox, every
  sweep still queued nothing (`remaining: 0`, rendered "all caught up") until
  `POST /api/upscale/failures/retry`. A toolchain fault recorded as a fact
  about the files. **The CarPlay kind reads one closure**,
  `carPlayOptimizeActiveFn` (the upscale gate AND the optimize switch):
  `WithCarPlayOptimize`, `Deps.OptimizeActive`
  (`TestConsoleCarPlayGateIsTheV1CarPlayGate`, the only pin possible, since
  both of its readers ask `UpscaleActive` first) and, with the pre-generation
  flag, the sweeper. **A card that reports a switch beside a gate says why
  they differ**: the auto-optimize card's `enabled` is the switches, `active`
  the gate, and `degradedReason: "sox_missing"` the difference, rendered as a
  "degraded" badge, a hint of its own that clears when the probe (30 s TTL)
  finds sox, and "not run" where a refused sweep's `disabled` would read
  "turned off". No restart is needed and none is advised. The settings PATCH
  gives `optimizeEnabled` and `autoOptimizeEnabled` the sox reason
  `upscaleEnabled` had. `TestServeWithoutSoxReportsUpscalingOffOnEverySurface`
  boots the real serve on a PATH with no sox and asks every surface; it and
  the report test were red on the old code, and seven controls each turn red
  only the assertions of the surface they revert. The transcode pool struck
  a file for ANY runner error, a missing sox included, which left a job
  queued inside the probe's TTL after sox disappears; this said "Still open"
  until the next change the same day made a tool the host lacks strike
  nothing and v48 expire the suppressions it had written (the TRANSCODE
  bullet under **Job pools**).
- **…and a surface that reports the gate beside its tool reads both from
  ONE probe: a cache over the shared one is a second clock** (2026-09-28,
  backlog B45). The console's two stats endpoints kept a 30 s cache of the
  sox precheck on top of `soxToolchainCache`, and the /v1 pair's two
  adapters each kept one over a `transcode.PrecheckSox` of their own, so
  `enabled` (the gate, on the shared probe) and `soxAvailable` (another
  cache) answered probes up to 30 s apart. Measured with the real binary,
  the two caches put 20 s out of phase and sox then taken off the PATH:
  `enabled: false` beside `soxAvailable: true` for 14 s on both console
  endpoints; after the fix they moved on the same poll. All three read
  `soxCache.precheck` per snapshot now (the console's `soxAvailability`, the
  adapters' `soxPrecheck`). The per-poll fork CodeRabbit flagged on #110 is
  the shared cache's to cap: a warm read is a mutex and a clock read, and
  the SSE tick already read that cache through the gate.
  `TestServeReadsSoxThroughTheSharedProbe` refuses a sox probe anywhere in
  cmd/bridge outside the shared probe and the CLI's once-per-run preflights
  (`soxProbeCallers`), and requires every precheck wiring to be
  `soxCache.precheck`. **Don't give a surface a cache of its own to save a
  probe: give the shared one to it.** Serve's boot line about a feature
  without sox said "— disabling", which reads as a demotion a restart
  undoes; it says "stays off until sox is installed (no restart needed)".
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
  context and pins the join. Neither sees the other's defect. (#997) The
  first, like `TestCancelStopsTheWholeCLIProcessTree`, asks whether the CLI
  has EXITED (`proctest.Exited`), not whether kill(pid, 0) still finds it,
  which a zombie is (#1024, under Build, CI, and test discipline).
- **A cancelled pass is not a failed one, and a join makes the difference
  visible.** `detectAndMint`'s context is cancelled by shutdown, by
  `Disable()` and by an admin client leaving mid-"Re-mint now" (RefreshNow
  runs on the request's context). `passCancelled` returns the snapshot it
  found and logs and publishes nothing, because the Detect error branch
  UNLOADS the LE cert and the MagicDNS suffix: a cancel read as a failure left
  every `*.ts.net` client on the self-signed cert until the next good pass,
  up to a day later. A DEADLINE is still a failure. **Only an ERROR the
  cancel caused is quiet**: a call that COMPLETED reports a fact (exec
  answers success only when the process finished on its own), and the pass
  applies it. Re-checking the context after a success was proposed in
  review and declined, because it would discard a completed "Re-mint now";
  `TestACompletedTailscaleCallIsAppliedAfterACancel` pins it. Joining a
  writer makes its cancelled exit path run before shutdown completes, so
  check what that path reports and what it changes. (#997)
- **…and the backup ticker applies the rule by asking the ERROR too,
  because one error can carry both** (#998). A shutdown during the startup
  snapshot (a `VACUUM INTO`, long on a big library) printed `backup
  (startup): snapshot failed: vacuum manifest db: context canceled`, and one
  during the prune printed two more lines about a pass that failed at
  nothing. `ctxerr.WithoutCancellation(ctx, err)` (`internal/ctxerr`, so
  every loop can ask it; it was `withoutCancellation` in `cmd/bridge` until
  the follow-up below) returns err with ctx's cancellation taken out, and
  the ticker reports what is left. Both of its
  conditions are load-bearing: ctx is CANCELLED (a deadline is a failure),
  and the error IS that cancellation (a snapshot's file copies do not watch
  ctx, so an I/O error that lands during a cancelled pass is a failure). An
  `errors.Join` is filtered child by child: `PruneContext` and its orphan
  sweep keep going past a directory they cannot remove or read, then join
  those failures with `ctx.Err()` when a later cancel stops them, so
  `errors.Is` on the whole error would silence the failures with the cancel.
  A join under a `%w` WRAPPER is reported whole, cancellation text
  included, since the wrapper cannot be rebuilt around what is left without
  losing its context (Gemini's suggestion returned the filtered inner error
  and compared errors with `==`, a runtime panic on an uncomparable type).
  A stopped snapshot leaves nothing to report: `Snapshot` removes its
  partial directory, destination file included
  (`TestSnapshotStoppedMidVacuumLeavesNothing` cancels INSIDE the running
  VACUUM), so the backups directory is as the pass found it. The run state
  keeps `sweepFinished(nil)`, the recorder's "no new counts" for a failed
  pass and a stopped one alike, because `running` must clear. **This did
  not close the class**: a survey the same day found about 33 more log sites
  that report a shutdown cancel as a failure (scanner batch writes,
  enricher, updater poll, harvest `tick_error`, fingerprint and smart-mix
  sweeps, integrity watchers, UPnP ingest, tsnet). Each subsystem it named
  has had its change since: the fingerprint and smart-mix sweeps (#999),
  the scanner's writes (#1000), the enricher (#1001), the updater (#1002),
  the harvest tick (#1003), the integrity watchers (#1004), tsnet's start
  and status query, UPnP ingest and the premium-cover record (#1005), and
  the tsnet goroutine's binds and HTTP/3 serves (#1009, the bullet after
  next). The rule #999–#1005 follow is the next bullet. A site is
  covered only once its own change has landed, so check the code
  before assuming one outside those is. (Until #1009 this still named
  only #999's two, through the six changes that followed it.)
- **Every loop `runServe` starts reports a shutdown as a STOP, and a log
  site in one asks `ctxerr.WithoutCancellation` with the context its PASS
  runs on** (#999, #1000–#1005). The scanner, enricher, updater, harvest
  client, integrity watchers, fingerprint and smart-mix sweeps, tsnet and
  UPnP ingest all do. The PASS's context, because a timeout derived from it
  is a failure: a harvest request that runs out of its own time while the
  loop is live is reported (`TestATickWhoseRequestTimesOutStillReportsIt`),
  and for the updater's "Check now" the pass IS the request. Sites that
  check only `ctx.Err() == nil` (the scanner's lookups, the enricher's
  searches) are the older, looser form and were left: each is a read the
  next pass redoes. That looser form is RIGHT only where the error cannot
  carry the cancellation: a tsnet node the shutdown closed under
  `ListenTLS` fails with the node's own error, so `tsnetListen` asks the
  context alone. Where the stop leaves its OWN mark on the error, ask the
  error and not the context: both discovery clients' M-SEARCH sends drop
  `net.ErrClosed`, which only each client's `Stop`'s close produces
  (`discovery.SendFailureLog`, the advertiser's NOTIFY burst likewise), and
  still report a genuine failure that lands during the stop (2026-09-28,
  under DLNA, UPnP and discovery). **A stopped pass reports no failure the stop caused, and
  records no verdict, count or status for the work the stop
  interrupted.** A `ctxerr` site still reports any other failure, even
  one that lands during the shutdown (#998's second condition). The
  enricher's fetches absorb their own errors, so a cancelled portrait
  search used to reach `markSkipped`, which counted `no_mb_match` and
  logged `enrichment skipped` for a verdict it never wrote. It now stops
  at `stampEnriched` / `markSkipped`, where every path converges, never at
  the five absorb sites (a sixth would be the one that forgets), and the
  row stays `enriched_at = 0`. The updater's `LastError` and the console's
  UPnP "Rescan now" keep the last result that ANSWERED, and a premium
  cover whose version record a shutdown stopped stays PENDING, so the next
  pass records it. What a pass DID before the stop is still summarised,
  marked `cancelled` (the integrity sweeps; an orphan walk that did not
  finish is `tick cut short`, never `tick complete`). A shutdown during
  `runServe`'s STARTUP is a stop too: the first-run upscale seed, the one
  startup step that runs on the serve context and exits 1, exits 0 when
  the shutdown lands in it. **The scanner's final flush DROPS its batch on
  a cancel, deliberately**: the skip gate compares each file with its
  STORED row, which the dropped write never touched, so the next scan
  redoes exactly what was dropped. Three traps. A cancellation error from
  an AUTOCOMMIT statement does not prove the write did not land (modernc
  answers `ctx.Err()` whenever its interrupt fired, a committed statement
  included). database/sql can answer `sql: statement is closed` in place
  of the cancellation (1 in 3,000 measured), which is reported rather than
  matched by its text. And a context cancelled BEFORE the call can pass a
  test without reaching the site at all: the seed's `GetUpscaleTarget`
  failed on it first and the seed was skipped, so the test was green on
  the unfixed code. Cancel INSIDE the call the test names
  (`internal/sqlitetest`'s collation over the column a write moves, or a
  fake upstream that cancels and holds), assert that the cancel happened
  there (`mustHaveReachedGitHub`), and give every stopped test a twin
  failing on a LIVE context: without one, the site's "never log" control
  cannot bite, which happened twice here. Joining the tsnet start
  goroutine was #997's class, not this rule's; #1009 did it (next
  bullet).
- **A goroutine serve starts is stopped by a context the TEARDOWN
  cancels, and joined before anything it uses is closed** (#1009).
  runServe ran the embedded tsnet node's start (up to five minutes,
  interactive auth included), its HTTP/3 binds and its HTTPS listen on
  one goroutine it never joined, on serve's context, and its deferred
  teardown closed the node, which upstream forbids "before or
  concurrently with Start". **serve's context is not the teardown's**:
  runServe's own cancel is its FIRST defer, so it runs LAST, and on an
  error exit (the admin console cannot bind, say) the whole teardown runs
  with serve's context live. A start waiting for auth went on waiting,
  and a listen that landed after the close was published and served on a
  node that was gone (CodeRabbit on #1005). `tsnetFront`
  (tsnet_bringup.go) runs the goroutine on a context of its own, and its
  `stop` (the shutdown branch's since #1019, deferred for every other
  exit) cancels it, refuses publication and takes what was
  published in ONE critical section, drains that, waits for the goroutine
  and every HTTP/3 Serve it started (drains and wait share one grace;
  running out costs a line), and only then closes the node. **A server is
  served only once it is published**: starting each HTTP/3 server as it
  bound let a shutdown mid-bind close the node under one nothing could
  see, which then reported `h3 serve tsnet` with quic's `transport
  closed`. The wrapper's half: a `Close` that lands during `Start` marks
  the server closed and cancels that start WITHOUT waiting (upstream's
  own start takes no context), and the start closes the node it built
  instead of publishing it; after Close, or on a context already done,
  Start builds nothing. **A drain is a wait too, and stop's is bounded**:
  quic-go runs `ServeHTTP` inside the wait `http3.Server.Shutdown` makes
  past its deadline (it calls `Close`, which waits for every connection's
  handling), so an HTTP/3 handler that ignores its context (a read from a
  hung mount) held an unbounded drain, and the exit, for as long as it
  blocked (CodeRabbit on #1009). `stop` is therefore the ONLY drainer of
  the tailnet side: the shutdown branch's early drain of the tailnet
  HTTP/3 servers was the same wait, ahead of stop's. The LAN HTTP/3 drains
  had the same shape (next bullet). **Hold the window, not its
  aftermath**: a first draft of the listener test released the listen
  after runServe had returned, and PASSED on the unfixed code, because
  runServe's final cancel had run by then and #1005's post-check caught
  it. The window is between the node's close and that cancel, and the
  test holds serve there on its `tsnet close:` print. The publication
  gate has no seam a boot test can hold, so it is driven directly.
- **An HTTP/3 drain is bounded, drained ONCE, and its socket closed a
  moment AFTER the grace, never at it** (#1010). Both LAN HTTP/3 drains,
  the shutdown branch's (beside HTTPS) and the defer every other exit
  takes, waited for `http3.Server.Shutdown` with no bound, so a LAN
  HTTP/3 handler that ignored its context held serve's exit for as long
  as it blocked: the previous bullet's defect, on the LAN. `lanHTTP3`
  (lan_http3.go) owns the server, its socket and its one drain: `stop`
  waits out the grace and `http3ForceCloseAllowance` (1 s) past it,
  prints a line if the drain is still running, and closes the socket
  under it. **Once, because a second drain is a second wait**: the
  `Close` that `Shutdown` calls past its deadline holds the server's
  mutex while it waits for the handlers, so the defer's second
  `Shutdown`, after a bounded branch, blocked on that mutex for as long
  as the first. The comment it replaced called the two calls idempotent,
  which held only for a drain that finished. **The allowance, because
  `Close` writes each connection's CONNECTION_CLOSE BEFORE it waits for
  the handlers**: a socket closed the moment the grace ran out lost the
  client's close 20 of 20 times on macOS and on Linux, whether the
  connection was idle, streaming or held, and the client learned of it
  only from its idle timeout (quic-go's default is 30 s). The writes land
  within 4 ms of the deadline. #1009's `stop` closed the tailnet node at
  exactly that moment, so it takes the same allowance (`drainedWithin`).
  **Bound a wait by what the thing you close is still doing, not by the
  deadline alone.** `stop` closes the socket itself, not the drain's
  goroutine when it returns: a held handler would keep the port bound,
  and the launcher menu's next start would fall back to HTTP/2.
  `Shutdown`'s error is printed only once it has returned, so nothing
  reaches stderr after runServe has. The boot tests hold a LAN request
  through `serveOpts.wrapAPIHandler`, and pre-pick a port free on TCP
  and UDP both (`freeLoopbackTCPAndUDPAddr`), because serve prints no
  UDP address.
- **On a shutdown the LAN and the tailnet drain TOGETHER, under one
  grace** (#1019). The shutdown branch drained the LAN servers and
  returned, and only then did the deferred `tsnetFront.stop` begin on
  the tailnet's. With an HTTP/3 request held on each side, SIGINT to
  exit measured 12.0 s. With a client on each side keeping its
  connection open past GOAWAY it measured 10.0 s, and the tailnet served
  a new request one second after SIGINT. **`docker stop` sends SIGKILL
  after 10 s**, and tsnet is how the image joins a tailnet, so both cases
  were killed mid-drain, before the node's close, the writer join and
  `Store.Close`. The branch now starts `stop` beside the LAN drains on
  the SAME context: 6.0 s and 5.0 s. **A teardown called from two places
  stops once.** The defer stays for the error exits, so `stop` runs under
  a `sync.Once`, as `lanHTTP3.stop` does. Without it the deferred call
  drained again, on the mutex of an HTTP/3 server still in Close (a
  second grace and a second line), and closed the node a second time.
  `stop` takes the drain's context, and the defer builds one when it
  RUNS, never when it is registered. quic-go's own client closes an idle
  connection on GOAWAY, so the 10 s case needs a client that does not.
  `TestServeShutdownDrainsTheTailnetBesideTheLAN` pins the ORDER, not a
  duration: it holds the LAN drain's give-up line and requires the
  tailnet's HTTPS listener to be closed already, which the old order
  cannot do on any host. The error exits still drain one after another.
- **A snapshot a shutdown cancels STOPS, and `VACUUM INTO` went on past its
  cancel in three places** (2026-09-29, backlog B63). modernc stops a
  running statement with `sqlite3_interrupt`, which SQLite reads only
  between the steps of a statement that is running. On CI's Windows leg a
  serve test's startup snapshot still held `data/backups/<stamp>/bridge.db`
  after `runServe` returned: the `bgWriters` join had given up on it, as
  it must (grace-bounded), and the TempDir cleanup met the open file.
  **The commit reads no interrupt**, and VACUUM INTO builds its output in
  a page cache the size of its source connection's (2 MB by default), so
  what is still cached at the end is written by the commit: a database
  under 2 MB was copied whole there. Of 3,818 cancels that landed while
  the statement ran (an 820 KB database, dev Mac), 2,053 finished the
  copy anyway, and on nomos under disk contention the commit spent 2.3 s
  writing a 316 KB copy after its cancel, which the driver then answered
  as `context.Canceled` and the snapshot threw away. `cache_size(-64)` on
  the snapshot's source connection (`snapshotSourceQuery`) makes the copy
  write as it goes and leaves the commit 64 KB (220 of 3,722; a 118 MB
  snapshot took 326 to 698 ms whatever the cache). **A busy handler
  sleeps through it**: `busy_timeout(5000)` answered a cancel 300 ms into
  a lock wait 5 s later, with `database is locked`. The source takes
  `busy_timeout(100)` and `retryWhileBusy` waits out a longer lock between
  attempts, on the context, for `snapshotBusyPatience` (5 s) in all:
  0.11 s now. **A cancel before the statement starts is cleared by it**
  (`sqlite3Step` resets the flag when no other statement is active, and
  the driver checks the context before it arms the interrupt): 2 of
  4,000 random cancels, each a whole copy. The INTO target is a Go SQL
  function (`vacuumTarget`), which SQLite evaluates once the statement
  runs and before `OP_Vacuum` copies anything, and which refuses a
  cancelled snapshot. What is left is a cancel in the commit's last
  64 KB. **Don't raise the source's cache, don't give the snapshot
  SQLite's busy wait back, and don't Ping before the VACUUM** (a Ping is
  a statement that waits on the lock too, and the first attempt is the
  probe). Any statement that must stop on a cancel meets the same three.
  `TestASnapshotCancelledAsItsVacuumStartsCopiesNothing`,
  `TestASnapshotWaitingOnALockedSourceStopsForItsCancel` (beside its
  control, `TestASnapshotStillWaitsOutABriefLock`) and
  `TestASnapshotWritesItsCopyWhileItCopies` were each red on the old code,
  and each turns red alone when its fix is taken out.
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
  wired from a config readable at the TARGET path (`withExistingInstallDeps`,
  #963's name for it, which this bullet gave as `withExistingInstallCertDeps`
  until 2026-09-27); a first install keeps the skip, because a
  narrower want-set than `bridge serve` builds would be a comparison presented
  as authoritative that nobody made. Grading the PRE-init state is the point,
  not a compromise: the cert on disk is the pair a rewrite keeps, and a
  loopback rewrite of a loopback install keeps `customEndpoints`, so there
  the old want-set IS the new one. **This bullet said every rewrite kept
  them until 2026-09-27, and none did** (the bullet on what a rewrite keeps):
  a `--public` rewrite writes the domain's and a posture change starts over,
  so for those two the pre-init grade is about the list being replaced, and
  `bridge doctor` after the run grades the saved one. **The warn printing
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
  doctor run (the public-mode layout, as with the cert key), and the port
  checks then say "not checked" rather than grade the defaults (#1022), as
  config-dir does rather than probe as this user (#1023).
  The launcher's pre-setup row names the platform path through
  `buildDoctorDepsFor(path, true)`, never `--config`, so its absence stays
  "none found", ok, and a config there this user cannot read still leaves
  config-dir and the ports graded, as Setup's preflight grades them
  (#1023). `bridge init`'s preflight leaves the lookup nil, so
  config-file reports itself skipped and does not block the re-init that
  replaces a broken config. Since #1027 the port checks do not block it
  either: they recognise the bridge being replaced by the pid file in the
  data dir init writes, by attribution only (the "…over a config that is
  there and does not load" bullet). This bullet said a broken existing
  config "cannot block" that re-init until 2026-09-25, when the port
  checks still could: with the config unloadable the preflight graded
  7788 / 7789 with no pid file, and a bridge still live on them FAILed
  both. (#984, #985)
- **…and where it finds no config, tls-cert grades the pair in the data
  dir `bridge init` writes, as the port checks grade the ports it writes**
  (2026-09-29, backlog B61). config-file says "none found; the checks
  below use defaults", and tls-cert graded nothing there: it warned "no
  data dir set" on EVERY pre-init run (`16 ok, 1 warn`, the warn its),
  while init, run next, keeps a pair it finds in `<config dir>/data`
  (`initDataDirFor`) and its preflight FAILs a broken one. So a cert left
  without its key there read "all clear.", exit 0, and init then refused
  on `[FAIL] tls-cert partial state` (measured with the real binary);
  now doctor FAILs it too, and a fresh host reads `absent (init will
  mint)`, `17 ok, 0 warn`. The launcher's row does the same over a config
  it finds and cannot read, as Setup's preflight grades that dir over one
  (#1023's rule for the row;
  `TestMenuDoctorPreviewsSetupOverAnInstallThisUserCannotRead` compares
  the two lines). **A config that was named or found and not graded
  leaves no data dir**: tls-cert answers ok "not checked" and why (not
  readable, not there, does not load), #1022's rule, where it warned too;
  a run over one reports one warn fewer. `bridge init`'s preflight and the
  console always hand a data dir and are unchanged.
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
- **…and the servers' error log keeps no client address** (#1055). net/http
  prints the peer's address in a failed handshake, a recovered panic and the
  HTTP/2 connection errors, and the privacy page promises no client IPs for
  the phone-facing API: a phone with a stale pin, a cancelled endpoint probe
  and a scanner each left one in the journal. `handshakelog.Wrap`'s logger
  redacts every line it keeps (`RedactPeers`, anchored on the words net/http
  prints before a PEER and on the `->` of a socket error, the next bullet,
  so a listen address in an accept error stays), and
  AFTER the silent-probe check, which recognises the probe by that address.
  The tailnet server, whose listener yields `*tls.Conn`, takes
  `handshakelog.ErrorLog()`. `TestEveryServeHTTPServerRedactsPeerAddresses`
  requires every `http.Server` in `cmd/bridge` to get its `ErrorLog` from
  handshakelog, and an assigned one before the server is first served. **A test that finds a log line by its peer address passes
  vacuously once the address is redacted**: the probe tests find the
  filtered server's line by the placeholder as well.
- **…and a kept line's REASON names the peer again, after the `->` of a
  socket error, which is redacted too** (2026-09-29, backlog B172). A
  handshake that times out or is reset, the commonest failure a phone
  produces, logs net's `*net.OpError` as its reason, and that names both
  ends of the socket: `http: TLS handshake error from <client address>:
  read tcp 127.0.0.1:7788->127.0.0.1:51786: i/o timeout`. Measured on
  macOS, Linux and Windows 11 over a stalled client, a reset before the
  ClientHello and after the ServerHello, a reset right after the
  ClientHello (the server's WRITE fails: `broken pipe` on macOS,
  `connection reset by peer` on Linux, `wsasend` on Windows) and the
  HTTP/2 preface of a reset h2 connection (the phone's own shape; logged
  on Linux and macOS, while Windows' h2 server counts `WSAECONNRESET` as a
  closed connection and logs it only verbosely): every one was
  `<local>-><peer>`, for a read and a write alike, since the OpError's
  Source is the local end and its Addr the remote, and gVisor's gonet (the
  tailnet listener's sockets) builds it the same way. So the tailnet
  server's `ErrorLog()`, which runs the same function, leaked it too.
  `RedactPeers` now also takes the address after `->`, and **every other
  place the line repeats an address a word or an arrow named as a peer**
  (`redactRepeats`, never inside a longer address): net's accept ignores a
  failed getsockname (fd_unix.go, fd_windows.go), and a socket error with
  no local end names the peer alone, `read tcp <peer>: …`, where only the
  words earlier in the line say what it is. **A lone address can also be
  the LOCAL end**: gVisor's socket has no remote address left after a
  reset, nothing names it, and it stays. **Don't redact every lone
  address after `read tcp`**, which labels the tailnet node's own address
  `<client address>`. **And an IPv6 zone runs to the `]` before the port
  whatever it holds**: Windows names a link-local zone after the adapter,
  spaces and parentheses included (`[fe80::…%Wi-Fi 4]:51195`, measured),
  and #1055's `\[[^\]\s]+\]` matched no such address even after "from ",
  so the whole line kept the peer. The part before `%` must be an IPv6
  literal, so a bracket in a panic value is not taken for one (a looser
  bracket swallowed `[x] happened at [y]:7`). The listen address before
  the arrow stays, and `isSilentProbe` still reads the raw line (its
  `: EOF` carries no arrow). **A leak check reads every address in the
  output, never only the place the fix put the redaction**:
  `TestEveryOtherHandshakeFailureIsLoggedAsBefore` drove the stalled shape
  since #986 and passed through #1055, because it looked for the peer only
  right after "from ". It now requires every address the wrapped server
  logged to be the server's own (a loopback client shares its IP, so the
  port tells them apart). `TestAnHTTP2PrefaceErrorKeepsNoClientAddress`
  (skipped on Windows, which logs nothing there) and
  `TestRedactPeersInASocketError`, whose rows are built from `net.OpError`
  and `net.TCPAddr` values so none holds a shape net never prints. The
  other `http.Server`s bypass handshakelog knowingly: DLNA's nil
  `ErrorLog` names a renderer in a panic line (the privacy page's
  disclosed DLNA exception), and the console's plain-HTTP modes see only
  loopback peers (a proxy's, in public mode).

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
- **…and "loopback" is a fact about the request's HOST as well as its source,
  so a loopback console refuses a Host that names another host with 421**
  (2026-09-29, backlog B170). `loopbackOnly` and `csrfGuard` judged a request
  by its source and its Origin, never its Host. A page served from a name its
  author controls can re-point that name at 127.0.0.1 (DNS rebinding): the
  operator's browser then sends the page's requests to the console, from
  127.0.0.1, and hands the page the answers, since to the browser they are the
  page's own origin, and a same-origin GET carries no Origin (measured with
  the real binary on main: a request naming another host was answered on
  every read route tried, and only a POST's Origin was refused; the record
  is in the log). `loopbackHostOnly` sits inside
  `loopbackOnly` in `boundaryMiddleware`'s loopback branch and answers 421 to
  any Host that `loopbackHostname` (the Origin allowlist's rule: `localhost`,
  a trailing dot, 127.0.0.0/8, `::1`) does not take, **with any port or none**:
  an `ssh -L 17789:127.0.0.1:7789` tunnel sends `Host: localhost:17789`. **An
  empty Host passes**: every browser sends one, and only an HTTP/1.0 client can
  leave it out. The configured admin host needs no case of its own, since
  `validateLoopbackAddress` admits nothing the rule refuses
  (`TestEveryAdminAddressLoopbackModeTakesIsALoopbackHost`); a config that
  learns another loopback name teaches the rule the same name. A refused name
  is logged once (`noteForeignHost`, at most 16 names), which is how an
  operator's own proxy shows up. **The cost is a reverse proxy that forwards
  the browser's Host**: Caddy's `reverse_proxy` does by default, and so does
  nginx with `proxy_set_header Host $host` (measured in Docker on dido: 421
  for those, 200 with Caddy's `header_up Host {upstream_hostport}` and with
  nginx's default `proxy_pass`), and Traefik by its documentation
  (`passHostHeader` defaults to true; not measured); `docs/docker.md` says
  how. Don't
  accept a forwarding header as the proof instead: a page can set
  `X-Forwarded-For` on a same-origin request. Public mode is untouched (a
  tenant console behind the host's proxy arrives from 127.0.0.1 under the
  tenant's name, and its session cookie is what a rebinding page cannot
  carry). The DLNA listener takes the same rule on its own addresses (under
  **DLNA, UPnP and discovery**). A console POST through a tunnel on ANOTHER
  local port is still refused by the Origin check, which compares the
  admin port (backlog B193).
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
- **`adminauth.json` is shared by PROCESSES, so the CREDENTIAL is the file's and
  the SESSIONS are the serving bridge's** (#1039). `bridge admin reset-password`
  beside a running public bridge was silently undone: the bridge kept verifying
  the password it loaded at start, and its next write of the file (a login, the
  30 s activity flush, a logout, the shutdown flush) put the old hash back,
  because `persist()` wrote this process's copy of the whole file. The restart
  the command advised was one such write, and the hosted control plane's
  `bridge-tenant passwd` restarts the tenant straight after the reset. Every
  decision about the credential (a login, a ticket's account at mint and at
  redeem, "is there an account", the reset's username check) and every write now
  re-reads the file (`readStoreFile`). A file that cannot be READ decides
  nothing (Verify refuses: this process's copy is the password rotated away
  from) and is written over by nothing, the change staying pending; a MISSING
  file is no credential, and a session write does not recreate it. **The file's,
  never "the newer passwordChangedAt"**: every credential write is synchronous
  and adopted only once its rename lands, so memory is never ahead of the file,
  and a timestamp would keep memory over a restored backup. A session write puts
  down the set in memory; a credential write that keeps the sessions puts down
  the set it finds in the file AT THE WRITE, never the one read at open, since
  reset-password waits at a prompt in between. **One re-read is not enough**:
  staging costs a write and an fsync (milliseconds, tens on a cloud disk), so
  EVERY write goes through
  `commitLocked`, which re-reads the file just before its rename and rebuilds
  from a fresh read if it changed at all, byte for byte. Comparing the
  credential alone covered the running bridge's writes and missed
  reset-password's, whose rename dropped a login or brought back a logout
  committed while it staged (CodeRabbit on #1039). The rename itself (on
  Windows, with its retries) is what remains. A kernel lock (flock / LockFileEx)
  would close that, and the "stale lockfile" reason this file gives for
  declining interprocess locks is about lockFILES: the kernel drops those locks
  with the process. **A rotation ends the sessions by default, and a restart
  does not** (they persist, #800): the next bullet. reset-password said a
  restart ended them until 2026-09-27. **`tokens.json` had the same window** (a
  `bridge pair` beside a running bridge) and closes it the same way, #1043,
  under Config, settings and process lifecycle. A new file that more than one
  process writes needs the re-read before its rename from the start, or better,
  a layout in which no process rewrites a record another wrote: the login
  tickets had the same window and no re-read, and are one file per ticket since
  2026-09-28 (two bullets down).
- **A sign-out reaches the running bridge as a MARKER in `adminauth.json`,
  never as an emptied session set** (2026-09-27). The running bridge holds the
  sessions in memory and writes them back (a login, the 30 s activity flush, a
  logout, the shutdown flush), so a file that merely lost them ends nothing.
  Until this change nothing could end a session another browser held: a
  rotation kept them (a test pinned that as operator-friendly), a restart
  reloaded them, and a console signed in with a leaked password outlived the
  rotation for up to the 7-day cap, while 1-bit.app's troubleshooting page said
  a reset invalidated sessions immediately. Now `bridge admin reset-password`
  signs every console out by default (`--keep-sessions` keeps them), `bridge
  admin sign-out-everywhere` does it with the password kept, and the Devices
  page's "Sign out all other sessions" (public mode) keeps the browser that
  asked; that one runs in the process holding the sessions and needs no
  marker, and a write of it that fails is REPORTED (`saved: false`, not a
  failure: the sessions are refused already, and a restart before the next
  write would sign them back in). A process ending sessions it does not hold moves `sessionsRevokedAt`
  in the same CAS write, and **every writer carries the file's marker over**,
  the `--keep-sessions` rotation included. **The marker is an EVENT, never a
  filter on `IssuedAt`**: a store that reads a marker it has not taken ends
  every session it holds, BEFORE it builds anything it writes
  (`persistSessionsLocked` builds its set inside the commit closure for exactly
  this), and keeps every session made after that read whatever the clocks say.
  A filter would end every login after a clock stepped back until the clock
  passed the marker; and a session held at the read predates the sign-out even
  with a later `IssuedAt`, since its login read the file before the marker
  landed. `load()` takes the file's marker as already taken, or every start
  after a sign-out signs its own sessions out at its first read. **Every
  sign-out MOVES the marker** (`nextSignOut`, one nanosecond past the last when
  the clock does not put now after it): two sign-outs inside one tick of a
  coarse clock, or across a clock stepped back, otherwise end nothing on a
  bridge that took the first. **A session check reads the file whenever a
  stat says it changed** (`refreshIfChangedLocked`: size, mtime AND
  `os.SameFile`, since every write here is a rename and so a new inode whatever
  its size; a commit records its own file's stamp so the bridge never reads
  back its own write), so a signed-out console is refused at its NEXT REQUEST,
  not at the bridge's next write. Measured: 2 µs, against a 22 µs read at one
  session and 1.75 ms at the 1,024 cap. **A check that cannot read the file
  REFUSES** (`ErrStoreUnreadable`, a 503 from the middleware), keeps the
  session, records no stamp (a chmod or chown that fixes the file changes none
  of the three), and logs at most once a `sessionFlushInterval`: the middleware
  adds no line of its own, #1039's debounce concern. **The opposite of
  `tokens.json`'s choice (#1047), deliberately**: refusing there unpairs every
  device over a permissions mistake, while here logins already refuse in that
  state (#1039) and the unread file may hold a sign-out; don't make either
  match the other. A MISSING file still ends
  nothing. **A rotation or sign-out CONFIRMS its write** (`commitAndConfirm`,
  CodeRabbit on #1044): a running bridge's write that passed its own check
  just before the command's rename lands after it, carrying back exactly the
  credential and marker the command replaced (within `RenameWithRetry`'s
  750 ms budget on Windows, where a scanner forces the retries). The command
  re-reads after that budget and a margin (about a second), and commits
  again only when the file is back to exactly what it replaced: a newer
  write carries something else, and the naive "not what I wrote" rule makes
  two concurrent rotations undo each other until they give up. Only a writer
  stalled past the settle between its check and its rename gets through; a
  kernel lock would close that, and #1039 declined one. Open: a bridge still
  running the OLD binary drops the unknown field and writes its sessions
  back, so restart first, then sign out. A `sudo bridge admin
  reset-password` left a root-owned file the service could not read, which
  refused every console request until it was chowned; since #1048 the
  write keeps the file's owner (the KeepOwner bullet under Config, settings
  and process lifecycle).
- **A console login ticket is PERSISTED, because the two halves are different
  PROCESSES, and each ticket is a FILE OF ITS OWN, because both processes write
  tickets** (2026-09-28). `bridge admin login-link` mints and the serving bridge
  redeems, so an in-memory map is invisible to the redeemer and the feature
  never works — it shipped that way, and every unit test passed because each
  minted and redeemed inside one process. Until 2026-09-28 every live ticket
  shared `adminauth-tickets.json`, which each process rewrote whole from its own
  earlier read, and `Store.mu` reaches neither from the other: a mint whose read
  predated a redemption renamed the SPENT ticket back into place (redeemable
  again for up to `MaxLoginTicketTTL`), a redemption whose read predated a mint
  dropped the new ticket, and of two overlapping mints one was lost (all three
  red against the old code with the test hooks at the same points:
  `TestAMintCannotRestoreATicketSpentDuringItsWrite`,
  `TestARedemptionCannotDropATicketMintedDuringIt`,
  `TestTwoInterleavedMintsBothLand`). **Not a re-read before the rename**, the
  cure #1039 and #1043 gave `adminauth.json` and `tokens.json`: it narrows the
  window and cannot close it, since on Windows the rename itself retries for up
  to 750 ms. Now each ticket is `adminauth-ticket-<sha256 hex>.json`, 0600,
  beside the store: a mint creates its own file (staged, `KeepOwner`, synced,
  renamed) and a redemption removes its own, so NO process rewrites a ticket it
  did not mint, and each step is atomic alone. The disk still holds only the
  hash, as the NAME, so it gains no credential it did not already hold, and
  single use plus the lifetime still come from the removal on presentation,
  BEFORE judging expiry and account, which also decides between two
  redemptions: one removal wins. **A redemption touches its own file and
  nothing else**, in the order read, credential, remove, judge: a miss opens
  one name that is not there and WRITES NOTHING (the unauthenticated branch,
  under the mutex every console request takes), and expired or damaged files
  are pruned by a MINT, an operator's act. **A ticket whose file cannot be
  read is a 500 (`ticket_store_unavailable`), never the stale-link page**, and
  stays unspent: the shared-file reader turned every read error into "no
  tickets", against the handler's own docblock. **`ticketAbsent` is the one
  "not there" rule** for a read and a removal alike: ENOENT everywhere, and on
  Windows `ERROR_ACCESS_DENIED` too, which is what every open of a file with a
  pending delete answers (a prune racing an antivirus scanner's handle, the
  window `RenameWithRetry` exists for); only a spent or expired ticket is ever
  removed, so that file was on its way out. The cost: on Windows a genuine ACL
  fault on one ticket file reads as a stale link, where POSIX answers 500. It
  takes the GOOS as a parameter so its table runs on every CI leg. A mint at
  `maxLiveTickets` (32) is REFUSED and evicts nothing (every file is a link
  somebody may hold), counts only REGULAR files (a directory named like a
  ticket failed its read, counted as possibly live, and 32 of them refused
  every mint), and the first mint removes the old shared file. Cost of
  the layout change: links minted by the old binary in the ten minutes before
  an upgrade stop working, and a rollback loses the new binary's the same way.
  No interprocess lock, and none is needed now: nothing is left that two
  processes both rewrite. `SameSite=Strict` is NOT the usual magic-link trap
  here — the app opens the URL itself, and a navigation with no initiator is
  same-site (verified in a real browser, not reasoned about).
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
- **A pairing link carries a one-time `code`, and the device keeps the token
  the code redeems for, never the link's** (#1052, the 2026-09-23 audit's
  H1). The console's QR and deep link carried the device's long-lived bearer
  token, so anything that saw the URL (a screenshot, a clipboard manager, a
  link preview) held the credential until someone revoked it. Every shipped
  app refuses a link without `token=`, so the link keeps it and gains
  `code=` (`internal/pairingcode`: 32 random bytes, single-use, 10 minutes,
  one live code per token, held as a SHA-256; `Issue` drops the token's old
  code BEFORE drawing the new one, so a failed issue after a console
  rotation still ends the old QR's code, which would otherwise rotate the
  token again for whoever holds that QR). `POST /v1/pairing/redeem`
  trades the code by ROTATING the token it names: a fresh secret for the
  same record, with the link's token dead in the same commit. So a copy of
  the link is dead once the real device has paired, and a copy redeemed
  first makes the real device's redemption fail where the user sees it.
  **One store, in the serving process**: the console issues
  (`admin.Deps.PairingCodes`) and the v1 API redeems, both in `bridge
  serve`, which is what lets the codes live in memory (a restart costs a
  fresh QR). `bridge pair`, another process, issues none, because a code
  it minted could never be redeemed. No package test can see that wiring:
  `TestServeRedeemsThePairingLinksCode` boots the real serve, and both
  halves' negative controls turn only it red. **Take before judging**, the
  login ticket's rule: the code is deleted before its age is read.
  **Every refusal is the same 410** (unknown, used, expired, token revoked
  or expired), and the route shares `POST /v1/pairing/requests`' per-IP
  limiter. `Rotate` keeps `ExpiresAt`, so an expired token is refused
  rather than handed over to 401 on its first request. **A client that
  understands `code` never falls back to the link's token on a refusal**:
  a copy redeemed first has killed it already, and a device paired with it
  would keep the secret the exchange exists to replace.
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
  the comparison must go through `baseurl.CanonicalHTTPS` (the wire side
  through `baseurl.CredentialBase`, which the harvest state store holds too),
  or a one-sided reduction turns a correct pin into a mismatch that fails
  closed. The reduction must be a fixed point, since the pin is reduced twice
  (backlog B97, under **Enrichment**).
- **`fs.Resolve`'s final containment check is the PRIMARY defense on Windows**,
  not belt-and-braces: both guards above it are slash-based, so a backslash
  traversal passes them untouched and only `filepath.Join` + the prefix check
  catches it.
- **HSTS is public-mode + TLS only** — pinning it for `localhost` poisons that
  hostname in the operator's browser for every other local service.
- **A log line names a library file library-relative, and the privacy page
  says exactly where the code does otherwise** (#1055, the v0.2.1 logging
  audit). A file-API failure on a library file logs through
  `writeFileErrorLog` (the client's library-relative path, and the error
  without the `*os.PathError`'s absolute path), and an extractor names its
  file with `trackLogPath` (the track's library-relative path, or the base
  name). The audit found the page promising more than the code did, mostly
  from before v0.2.0; the leaks were fixed and the rest (the startup banner's
  roots, the older scanner and extractor error lines, rendition and waveform
  paths when adopted or removed, the bridge's own files in fault lines, info
  lines naming a track) is now what the page describes. **A new log line
  that names a library file uses the relative path; one that cannot is a
  page change in the same release.**
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
- **`//go:embed static/[^.]*` skips `.`/`_`-prefixed names inside matched
  subdirectories** — a `_util.js` compiles, embeds nothing, and 404s only in a
  release build. Don't "fix" it with `all:static` (that ships `.DS_Store`), and
  don't take the glob back to `static/*`, whose `*` matched a top-level dot
  name (the embed bullet under **Build, CI, and test discipline**, #1006).
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
  executes the line that renders it. **A leaf the server sends as a KEY
  for the console to word needs its wording pinned too**: the guard proves
  `maintenance.orphanSidecarGCRefusal` is read, not that every value it
  can take has words, so `TestEveryOrphanRefusalKindIsWorded` runs
  `describeOrphanGCRefusal` under node for each
  `integrity.OrphanRefusalKinds()` entry (2026-09-28), and refuses one that
  comes back blank, `undefined` or `null` (what `console.log` prints for a
  case that returns nothing), as its key, as the fallback, or in another
  kind's words. **And a list that stands for every value is pinned to the
  declarations**: `TestEveryOrphanRefusalKindIsListed` reads the integrity
  package's source for every constant of type `OrphanRefusalKind` and
  requires `OrphanRefusalKinds()` to hold exactly those, since a kind the
  list omits is worded by nobody and the wording test passes over it.
  The variant watcher's key (`maintenance.variantIntegrityRefusal`,
  2026-09-29) has the same pair, `TestEveryVariantRefusalKindIsWorded` and
  `TestEveryVariantRefusalKindIsListed`, over the same helpers
  (`requireEveryRefusalKindIsWorded`, `requireEveryKindIsListed`): **a
  third refusing sweep takes both helpers, not copies of them.**
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

- **A control the server will refuse must not look live, and the panel
  reads the refusal's own predicate** (2026-09-28). With upscaling on and
  `upscale.optimizeEnabled` off, "Generate CarPlay" stayed enabled on the
  album and artist Variants panels and a click answered 503
  `optimize-disabled`: #1060 made the console's submit refuse the kind, and
  the summary the panel reads carried no switch. The summary now carries
  `optimizeActive`, which is `Server.optimizeActive`, the ONE predicate the
  batch submit, the projection endpoint and the summary read (nil reads as
  on, as `Deps.OptimizeActive` always has), so the button is disabled
  exactly where the submit refuses.
  `TestTheVariantSummaryCarriesTheSwitchTheSubmitReads` compares the served
  value with a real submit for every state of the switch, and pins that
  submit BOTH ways (a 202 that reached the coordinator once, or the
  switch's own 503 that reached it never): read as merely "not
  `optimize-disabled`", a submit failing some other way passed as an
  accepted one (CodeRabbit on #1068). And
  `TestTheVariantPanelDisablesGenerateCarPlayWhereTheSubmitRefusesIt` runs
  the SHIPPED panel under node on the summary the album detail serves,
  kind by kind against the submit, with the switches moved through the
  settings PATCH. **Don't serve the configured `optimizeEnabled` instead**:
  it agrees with the submit only while the gate is wired from the same
  config, and a control that served it went red on the summary test alone.
  The reason sits in the kind's own row with a gear for that one switch (a
  block that stops both kinds stays one note above them), and the panel
  tests `=== false`: an unknown leaves the button live and lets the
  endpoint answer, the folder view's rule, since `/api/library/browse`
  carries no feature state. The #1060 AST sweep counts a call of
  `optimizeActive` as reading the CarPlay switch. A save from either tray
  redraws the panel: the next bullet. **The panel-wide note must close
  wherever the upscale gate does**, because `Deps.OptimizeActive` includes
  that gate (the flag and a usable sox: `carPlayOptimizeActiveFn`, under The
  CLI and the serve wiring), and a block the panel-wide note does not
  show falls through to the CarPlay row's "switched off". The summary's
  `soxAvailable` is therefore `Server.soxUsable`, the gate's sox half
  (found, and FLAC when the build's formats are known): with the precheck
  alone a sox without FLAC read as available, and the panel named a switch
  that was on while the hi-res Generate stayed live over a refusal
  (`TestTheVariantSummaryReadsSoxAsTheGateDoes`, and the panel test's
  fourth state).
- **A tray redraws the page only through its spec's `onSaved`, and a page
  redraws only for a switch it draws from** (2026-09-28, CodeRabbit on
  #1068). A feature tray saves one switch and repaints nothing else, so
  after the CarPlay kind's tray said "Saved." the row beside it still said
  "switched off" over a disabled button until the next render.
  `saveTrayField` calls `onSaved(field)` once for a save the server
  answered `live` (with a reason or without), after the snapshot, the
  status line and every other tray have the new value, and outside the
  `try`, so a callback that throws cannot turn a save that landed into
  "Save failed". Never for `restart` or `unchanged`: nothing on the page
  moved, and a redraw that took the tray would take the restart
  instruction in it with it. The variant panel's trays call its own
  redraw (the bullet on the panel's in-place redraw, below): the CarPlay
  kind's tray for its one switch, and the panel-wide tray for
  `upscaleEnabled`, and for `optimizeEnabled` once generation is on:
  while it is off nothing the panel draws depends on the CarPlay switch,
  so a redraw for it would fetch and repaint the same panel for nothing,
  the reason Generate does not redraw either. **Don't make a tray redraw
  by itself, or a page redraw for every field.**
  `TestATrayCallsOnSavedOnlyAfterASaveTheServerAppliedLive` runs the
  shipped `buildFeatureTray` and save under node, and
  `TestAVariantTraySaveRedrawsThePanelWhereTheSwitchChangesIt` the shipped
  panel on the summaries the album detail serves.
- **…and a page that redraws after a save reads what it draws from the
  server, never the page seed** (2026-09-28, backlog B35). The Smart mixes
  page drew its off state from `seed.mixesEnabled`, read once per page load,
  and handed its tray no `onSaved`, so after its own gear saved the switch
  on the page said "Smart mixes are off" beside "Saved.", and the player's
  own navigation back to it said it again, until a reload (seen in a
  browser). The seed is read once per page load and neither a save nor the
  player's navigation reloads it, so **nothing a gear on a player page can
  save belongs in it**. `GET /api/player/mixes` answers `enabled` per
  request, and `managed` when the control plane owns the switch, so the off
  state points at the gear only where the gear can turn it on. The page
  redraws for `smartPlaylistsEnabled` alone, and IN PLACE: toolbar and tray
  are built once per route, the redraw repaints the view and the bar ahead
  of the gear, and is refused once the route has moved on, so the tray and
  its "Saved." survive, as the variant panel's do since 2026-09-29 (its
  bullet, below). **The player also drops the
  trays' shared settings snapshot on every route**
  (`BridgeFeatureTray.invalidate`, from boot.js's `route()`), the drop an
  operator page gets from its page init, which the player never runs: a
  player tray showed a switch as it was at page load, beside a view that
  now reads the server fresh.
  `TestTheSmartMixesPageRedrawsInPlaceAfterItsGearSavesTheSwitch` runs the
  shipped view under node; `TestThePlayerRouteDropsTheTraySnapshot` pins
  the drop structurally, since `route()` cannot run without booting the
  player. **A drop also discards the answer to a request made before it**
  (CodeRabbit on #1088): `invalidateTraySettings` nulled the promise and
  left the request running, so its answer still became the snapshot, over
  a newer answer or in place of the one the new page waited for, and its
  failure dropped the newer request. In a browser, with the older answer
  held back: the Jobs page's Smart mixes switch rendered off at the next
  tray sync while the server held on. `traySettingsSnapshot` caches an
  answer, and drops a failed request, only while its request is still
  `traySettingsPromise`
  (`TestADroppedTraySnapshotIsNotCachedWhenItsAnswerArrives`).
- **…and a tray save gives focus back to its switch** (2026-09-28). A save
  disables its switch while the PATCH is out, and a browser moves focus off
  a focused control that becomes disabled (the focus fixup rule) and does
  not give it back when the control is enabled again (measured on Chrome
  152). So every tray save left a keyboard user's focus on the body,
  whatever the answer and whether or not the page redrew: on the Smart
  mixes page the focus was gone before the in-place redraw ran, and
  equally for the Audio analysis switch beside it, whose save redraws
  nothing. `saveTrayField` notes whether the switch had focus before it
  disables it and gives focus back once the save is over, unless something
  else took focus meanwhile, and never takes focus the switch did not have.
  `TestATraySaveGivesFocusBackToItsSwitch` runs the shipped save under node
  with the fixup rule modelled in the harness. **Every other control the
  console disables while a request is out lost focus the same way** (the
  Jobs page's `wireJobButton` buttons among them, measured on "Scan now"),
  until 2026-09-29: the next bullet is the rule for all of them.
- **Every control that disables itself around a request goes through
  `setDisabled`, which gives focus back** (2026-09-29, backlog B68). The
  Jobs page's "Scan now" read `document.activeElement` as the body while its
  handler had it disabled, and still 4.5 s later once it was enabled again
  (Chrome 152), so a keyboard or screen-reader user lost their place after
  every "Scan now", "Retry missing" and the like; `saveTrayField` had given
  focus back for a tray's switch alone. `setDisabled(control, disabled)` in
  app.js is that rule for the 36 sites that disabled a control by a literal
  (app.js's 29 and the player's 7), and the enables that ended them:
  disabling notes on the control (`dataset.refocus`) whether it
  had focus, and enabling gives focus back to a control that had it, unless
  something else took focus meanwhile (`document.body` or nothing has it),
  never to a control that did not (Safari focuses no button on a click), and
  to nothing a redraw replaced. The note is on the control so the enabling
  call, a `finally` or a 4 s timer away, needs no variable carried to it,
  and `saveTrayField` no longer holds a copy of the rule. **The player
  reaches it through `window.BridgeControls`** (app.js is a classic script
  and the modules cannot import it, the `BridgeFeatureTray` handshake), with
  `ui.js`'s `setDisabled` as the forwarding wrapper whose fallback is a plain
  assignment: one implementation, and a page without app.js only loses the
  focus. **Never write `.disabled = true` or `= false`, or the attribute
  forms, outside the helper**: `TestNoConsoleControlDisablesItselfOutsideSetDisabled`
  sweeps app.js and every player module for them (a value computed from state,
  `gen.disabled = !actionable`, is not a request in flight, and the variant
  panel routes even those through the helper because an in-place repaint can
  disable a control the reader is on). `TestSetDisabledGivesFocusBackToAControlThatHadIt`
  and `TestAJobButtonGivesFocusBackAfterItsRequest` run the shipped helper and
  `wireJobButton` under node with the fixup rule modelled in the DOM.
  **Not covered: a control that VANISHES on success** (the fingerprint Enable
  button hides once the switch is on, a Delete re-renders the route), which
  has no control left to give focus to.
- **A tray offers no switch the control plane owns** (2026-09-28, backlog
  B35). Trays ignored `deployment.managedSettings`, so on a managed bridge
  they offered switches the settings PATCH refuses whole: the album page's
  Variants gear offered PCM upscaling and CarPlay, and a click answered
  "Save failed: these settings are managed by the control plane…", and
  the Jobs page's Backups and Update checks gears offered nothing else
  (seen in a browser). `applyTrayManaged` HIDES a managed row and disables
  its input, as `hideManagedSettings` hides the field and the library page
  leaves out the roots form, and hides the gear once no field row and no
  note row is left (a note is written for the reader whatever the
  switches). **Hidden, not greyed**: a greyed switch on a hosted bridge
  reads as something to earn. A change dispatched to a managed row anyway
  sends nothing (`saveTrayField`'s guard). The managed set is the
  snapshot's EFFECTIVE `managedSettings` and outlives a snapshot drop, so a
  tray built after the first snapshot leaves the row out from its first
  paint; on a fresh page load it is unknown until the tray's settings
  fetch lands (2 ms on loopback), and every row shows disabled meanwhile,
  the Settings page's own window. `TestATrayOffersNoSwitchTheControlPlaneOwns`
  runs the shipped tray under node. **Two controls outside the trays
  offered a managed field** until 2026-09-29 (the Jobs page's fingerprint
  Enable button, the Duplicates page's policy radios): the next bullet.
- **A control on the page that offers a managed field is hidden like a
  tray's row, and one that only offers a change waits to know** (2026-09-29,
  backlog B68). On a public-mode bridge whose `deployment.managedSettings`
  listed `fingerprintEnabled` and `duplicatesFilter`, the Jobs page offered
  the fingerprint Enable button and its click answered "Enable failed —
  retry", and the Duplicates page offered the policy radios and a click
  answered an alert, "these settings are managed by the control plane…":
  B35's trays on a control beside them. There are exactly four console
  callers of `PATCH /api/settings` (the Settings form, which hides managed
  fields and drops unoffered keys, the trays, and these two) and the PATCH
  is the only handler that reads the managed set. Both read the trays'
  snapshot (`trayManaged`, kept across page inits). **The Enable button
  waits for it**: `syncFingerprintEnable` shows it only for a switch that is
  off, a managed set that is known and does not name the field
  (`trayManagedKnown`; a card's button on a managed bridge must never
  flash, which a tray's rows in a closed gear can afford), and `initJobs`
  runs it again the moment the snapshot lands, not at the next 10 s poll.
  **The policy radios stay until the set says otherwise**, since they also
  say which policy is in force (`applyDupesPolicyManaged`, first from the
  set an earlier page's snapshot left, so a boosted navigation never shows
  them): the fieldset and the prose about saving one are hidden, the inputs
  disabled, and a line says which policy is set for the bridge, named as its
  radio names it (`dupesPolicyName`). Both handlers send nothing for a
  managed field (`enableFingerprint`, `saveDupesPolicy`), as `saveTrayField`
  does. **A tray blurb that counts switches is written from the rows the
  reader can see**: `spec.blurb` may be a function of the shown fields,
  which `applyTrayManaged` calls whenever the managed set may have changed,
  and the CarPlay pre-generation tray's "All three switches have to be on
  for anything to run" says how many are set for the bridge instead of
  standing beside the two rows a hosted bridge leaves it (`carPlayBlurb`;
  the DSD row is not one of the three).
  `TestTheFingerprintEnableButtonIsOnlyOfferedWhereTheOperatorOwnsTheSwitch`,
  `TestTheDuplicatesPolicyIsOnlyOfferedWhereTheOperatorOwnsIt` and
  `TestTheCarPlayTrayBlurbCountsOnlySwitchesTheReaderCanSee` run the shipped
  functions under node.
- **The variant panel redraws IN PLACE after a tray save** (2026-09-29,
  backlog B68). A save in the panel's gear called `onChanged`, the whole
  route's re-render: after turning PCM upscaling on from the Variants tab,
  `document.activeElement` was `#player-title`, the panel had no gear left
  and the tray's "Saved." was gone (Chrome 152). The panel now takes
  `refresh` (a fresh `variants` block: the album view and the artist view
  fetch their detail again) and `alive` (`gen() === at`, whether the route
  is still the one it was drawn for), and repaints around its gears.
  **Every node the panel owns is built once** and `paint` updates text,
  disabled states and attributes on them; `reconcile` adds and removes only
  the nodes a state needs and **never moves one that is already in place,
  because detaching a node takes the focus out of its subtree**, and the
  reader is in the tray's switch. The gears stay once built (a note, and the
  gear beside it, are `switchNote`; the note's text follows the state and
  the gear outlives it), so the panel's tray answers for the CarPlay switch
  too once generation is on, and a redraw for that field is skipped while
  generation is off. Concurrent redraws end on the newest one's answer
  (`seq`), and a FAILURE a newer redraw has overtaken is left to it, since
  handing over to the route there would re-render it away under the newer
  redraw (CodeRabbit on #1094); an answer after the route moved on, an
  aborted fetch, and an answer with no summary paint nothing; the newest
  redraw's failed fetch hands over to `onChanged`, whose re-render fetches
  again and shows its own error state; a panel given no `refresh` (the
  folder view has no trays) calls `onChanged` as it always did, and an
  `alive` that is not a function reads as no route check. **Not redrawn in place, still whole-route:
  Delete (the numbers are true when it answers, and the track marks beside
  the panel change with them) and the live refresh
  (`onVariantChange`, every 8 s while a batch runs).**
  `TestAVariantTraySaveRedrawsThePanelInPlace` runs the shipped panel on a
  DOM that models the focus rules (a `reconcile` that rebuilds its children
  is red on the focus lines alone), and
  `TestTheAlbumAndArtistViewsLetTheirVariantPanelRedrawInPlace` the shipped
  views: a panel that can redraw and is never handed the fetch falls back to
  the re-render without a word.
- **A job card says why a switched-on job is inactive in a note of its
  own, never over its description, and never asks for a restart a live
  gate does not need** (2026-09-28, backlog B45). The analysis and
  fingerprint cards wrote "Enabled but inactive: … Restart after fixing."
  over their description, false since #781 made both gates live, and
  nothing wrote the description back: in a browser, with sox taken off the
  running bridge's PATH and put back, the analysis card read "active" with
  "Analyze now" beside that sentence until a reload. `showJobDegraded` is
  the one note for the three cards whose gate probes a tool (analysis,
  fingerprinting, CarPlay pre-generation), hidden again once the card reads
  active. The fingerprint surfaces said "restart" three more times (the
  Enable button latched "Enabled — restart to apply" for good, its hint,
  the tray's "degrades to off at startup"), and the Settings chip beside
  the switch read `enabled` as running, so it said "active" beside a card
  that said degraded: #1067's upscale-chip defect, one switch over. **A
  chip or badge reporting a gate reads the gate (`active`), never the
  switch.** `TestAJobCardSaysWhyItIsInactiveBesideItsDescriptionAndClearsWhenActive`
  and `TestTheFingerprintChipReadsTheGateNotTheSwitch` run the shipped
  `renderJobCards` and `renderSettingsPrereqs` under node.
- **…and a card's LIVE counters follow the gate its badge reads, never
  the pool's existence** (2026-09-29, backlog B113). The analysis pool is
  built on every bridge (#781), and `GET /api/analysis/stats` (the SSE
  `analysis` event, which paints the Jobs card's Queue line) set `pool`
  whenever `Deps.AnalysisPoolStats` was wired, which serve does on every
  bridge: with analysis off the card read "off" beside "0 queued · 0 in
  flight · 4 done · 0 failed (6 workers)" (measured on a real serve after
  four analyses and a switch-off), where the upscale tile leaves its pool
  out by the live gate. `getAnalysisStatsSnapshot` sets `pool` only while
  `enabled`, from the SAME read of `AnalysisActive`, so the two cannot
  disagree within a snapshot (the closure answers whatever the gate
  says); the line reads "—", as the upscale tile's live fields do, and the
  sweeper's lines stay, being the last run's (a pass the gate refuses
  records nothing). **Gate on the live gate, never the flag**: a flag
  switched on over a missing sox is the degraded card, and a handler that
  read `cfg.Analysis.Enabled` put the counters back there
  (`TestTheAnalysisPoolLineFollowsTheGate` moves the two apart;
  `TestServeAnalysisPoolLineFollowsTheLiveGate` boots serve both ways).
  **Residual: a job queued before the gate closed still runs**, and
  neither tile shows it: a pool drains its queue whatever the gate says
  (measured: 37 analyses in 114 s after a switch-off, each an `indexed_at`
  bump; backlog B155). **A comment that explains a gate by a pool, a
  sweeper or a card being ABSENT with the feature off describes the code
  before #770 and #781**: the /v1/health gating comment said no
  `upscale.complete` event can arrive because a disabled bridge has no
  pool and no `SetOnJobComplete` (both exist on every bridge; the flag
  goes with the feature because no job can be asked for, and a job queued
  before the switch-off still publishes one), `WithUpscaleEnqueuer` said
  it was wired "IFF" the flag and sox, and some thirty more said the same
  in the stats docblocks, the auto-optimize card, its PATCH reason ("the
  upscale pool is absent"), the fingerprint cache and a Settings hint
  ("the Jobs card stays hidden"). `api.AnalysisStats` carried a `pool`
  that nothing ever set and PROTOCOL.md says is not there; it went (no
  byte changed). **Every pool, sweeper and closure beside them exists on
  every bridge, and only the live gates move**: a test that "disables" a
  feature by wiring nothing tests a bridge serve never runs.
  `TestHealthOmitsUpscaleCompleteEventsWhenUpscaleDisabled` wired no
  `WithUpscale` and passed with the flag advertised on the gate's WIRING
  rather than its answer; it wires a gate answering false now and flips it.
- **…and a card says when its next sweep is due only while its gate is
  open** (2026-09-29, backlog B156). Every sweeper loop runs on every
  bridge whatever its gate says (#781), and `runSweepLoop` arms the next
  pass from the interval alone, so with the features at their defaults the
  analysis, fingerprint and CarPlay cards read "off" beside "Next sweep:
  in 5h", and the smart mixes card, switched off, "Next run: in 23h" (seen
  in a browser on a real serve), each for a pass its gate refuses:
  `runSweepLoop`'s own dormant branch clears the time for that reason.
  `nextSweepWhileOpen` (handlers_jobs.go) leaves a gated card's
  `nextDueAt` out while the card's own gate is closed, read once for the
  snapshot (the analysis, fingerprint and CarPlay `active`, the smart
  mixes' switch), in `/api/jobs` and in the SSE `analysis` frame, which
  paints the same analysis line; the console renders the absence as "—".
  **The gate, never the switch**: a card switched on over a missing tool
  is closed too, since its next pass runs only if the tool is back by
  then, and it turns active, time and all, within a minute of that. The
  recorder keeps the time; only the payload leaves it out. **A card added
  with a next-run field is classified, or the build fails**:
  `TestEveryNextRunOnTheJobsCardsIsClassified` walks the snapshot type for
  every `next*` field and requires `nextSweepPaths` to say whether a closed
  gate withholds it (the scanner's, the backups' and the duplicates' are
  ungated, each with its reason).
  `TestAJobsCardSendsItsNextSweepOnlyWhileItsGateIsOpen` moves each switch
  apart from its gate, and `TestTheJobsCardsSayNoNextSweepWhileTheirGateIsClosed`
  runs the shipped renderJobCards and applyAnalysisStats under node over
  the payloads served.
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

- **SonarCloud's `go:S2077` follows a named const to its concatenation.** A
  query argument stays quiet only as ONE string literal, or as a function
  parameter; a const assembled with `+` is flagged even behind a name. `main`
  carries about 30 open S2077s of exactly that shape, and several comments in
  `internal/manifest` say a named const is enough: they are wrong. Measured on
  #1053 / #1054 (2026-09-27): a two-statement fix built from a shared SELECT
  was flagged, while one literal statement (`freshDSDPeaksSQL`) and helpers that
  take the statement as a parameter (`listRenditionCandidates`,
  `countRenditionCandidates`) were not. **Prefer one literal statement;** when
  a statement must share predicate constants, run it through such a helper.
  SonarCloud is not a required check, but a MEDIUM "vulnerability" on a
  constant query is noise that buries a real one.
- **A test server that redirects to the REQUEST'S OWN path is a BLOCKER for
  SonarCloud's gate.** `http.Redirect(w, r, r.URL.Path, …)` is
  `gosecurity:S5146`, an open redirect, and one such line takes a PR's
  Security Rating on New Code to E, which fails the quality gate (#1091's
  redirect-loop test server; the nine code smells beside it did not). Measured
  in the same file: a bare request path is flagged, while a path appended to a
  prefix (`target+r.URL.Path`, `"/moved"+r.URL.Path`) is not. A loop server
  redirects to a fixed path. It is test code and a false positive by
  construction, and a red gate on a PR still buries a real finding.
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
  `static/[^.]*` embeds a top-level `_x.js` and it ships. The citation guard
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
  guard reads only non-test files. One failure is out of reach of test
  code: Go's fuzz seed-corpus reader fails on a lock inside
  `testdata/fuzz/<Name>/`. (This bullet listed a second until #1006: a
  lock at the top of an embedded directory breaking the BUILD. The
  patterns were the defect, and the next bullet is the rule.)
- **Every wildcard element of a `//go:embed` glob starts with `[^.]`,
  never `*`** (#1006). A
  glob's `*` matches a leading dot (`go doc embed`: "image/*" embeds
  "image/.tempfile"), so `static/*`, `templates/*.html` and `*.tmpl`
  matched an emacs lock beside the file it locks. As a DANGLING symlink
  (macOS, Linux) the lock broke the build of the package and of
  `./cmd/bridge`: `cannot embed irregular file static/.#app.js`. As the
  REGULAR file it is on Windows it was EMBEDDED, and the console served
  its `user@host.pid:boot` at `/static/.%23app.js` with a 200. A Finder
  `.DS_Store` at the top of `static/` shipped the same way.
  `static/[^.]*`, `templates/[^.]*.html` and `[^.]*.tmpl` embed the same
  files as before and refuse the leading dot and nothing else. Two
  tempting alternatives are wrong. The bare `static` is lock-safe too, but
  it drops a top-level `_name`, which ships and which the static sweeps
  above read for that reason. `all:static` embeds the dot files below the
  top level. `TestEveryEmbedPatternRefusesALeadingDot` plants a lock
  beside every file and a `.DS_Store` in every directory of every
  embedding package, through `go list -overlay`, and requires the embedded
  sets to be unchanged. A source file planted the same way must show up in
  the package's `GoFiles`, because an overlay the go command did not read
  leaves the two listings equal and the test green over nothing. It never
  writes the tree, since a real lock would
  break the build of the test itself. An overlay file is always regular,
  and that shape answers for both: the go command asks a file's type only
  after a glob has matched its name. **It lists with `-test`**: plain `go
  list` resolves a test file's embed patterns but neither their files nor
  their errors, and without the flag the probe passed over an unsafe
  test-file embed. **A backup (`app.js~`) is the one kind of detritus
  `isEditorDetritus` names that the embed cannot refuse inside a directory
  a pattern WALKS** (`static/[^.]*` walks `static/player`), because the go
  tool's walk skips only `.` and `_` names. A glob bound to an extension,
  `[^.]*.html`, never matches one. An auto-save, `#app.js#`, is embedded
  too, but neither side of the comparison skips it, so the two agree. So
  `embedDiskProblems` tolerates a backup on the embedded side. Its disk
  side skips a dot-directory, which the embed refuses at every level, and
  walks a `_` one, whose files are the 404 the comparison exists to
  report. An embedded file that is on disk but outside what the FS holds
  is reported as a pattern too wide, never as a stale cache. Until #1006
  `TestEmbeddedStaticTreeMatchesDisk` reported one as "embedded but
  missing from disk (stale build cache?)" while it sat on disk, every time
  emacs saved an asset. `TestEmbeddedTemplatesMatchDisk` and
  `TestEmbeddedUnitTemplatesMatchDisk` pin the other two FSes the same way.
- **A test that sweeps this repo's own files skips a directory below its
  root that holds a `.git` entry: another checkout, which is not this
  tree** (#1007). Claude Code keeps its worktrees of other branches under
  `.claude/worktrees/`, each a whole checkout that git does not track and
  CI never has. With the three the main checkout held, 1,208 of the 1,617
  files the hash-cost guard opened were theirs, and 3,366 of the 4,513 the
  flac-handle guard opened, so a half-written file or a call in progress in
  any of them failed this checkout's run. `sweeptest.IsOtherCheckout` is
  the ONE definition, in a package only tests import: `os.Lstat` of
  `<dir>/.git`, whether a directory, a `gitdir:` file or a symlink,
  dangling or not, and never the root. The five sweeps from the module root
  apply it (the citation, docblock and blank-keeper guards,
  `TestNoProductionCodeLowersTheHashCost` and
  `TestNoLeakyFlacConstructors`), and **a new sweep from the root applies
  it too**. It is not the go tool's rule and does not replace it. #995's
  go.mod rule sees another MODULE, and a checkout git is still writing has
  no go.mod yet (in index order `cmd/` comes before `go.mod`): there a
  nested test satisfied a stale citation, a false pass. The docblock
  guard's `.` rule sees `.claude/` and not a checkout at a plain path,
  which doubled the files it read and failed it. **It drops no tracked
  file**: listing every file each walk opened, before and after, showed
  that, and in a clone it holds by construction, since git will not add a
  file inside another repository. The shapes where git and `os.Lstat`
  disagree (an empty `.git`, a `gitdir:` file pointing nowhere, a `git
  init` inside a tracked directory) exist only in a local checkout. **A
  skip that can drop a whole subtree needs a floor that catches the
  largest one going missing.** The hash-cost floor was `visited == 0`,
  which a walk that lost `internal/` passed, and is `>= 100` now; the flac
  walk had none, and needs 100 files of each kind plus one that imports
  the package, because only importers are judged. **Demonstrate on the
  geometry, not on the report**: of the three failures this was filed
  with, only the work-in-progress one still reproduced on main, since #995
  had closed the false pass for a checkout with a go.mod and #993 the
  locks. A scratch clone holding the same three worktrees at the same
  commits measured all of it, and the main checkout was never written.
  Two of the five still read a `_` directory until #1008, the next bullet.
- **A sweep from the root also skips a directory below the root whose name
  begins with `_`, and decides it from the NAME before listing it**
  (#1008). The go tool ignores one at any depth, so no build compiles it
  and `go test ./...` runs no test in it, and `## Local test fixture`
  tells an operator to put a throwaway helper in exactly one. The
  hash-cost and citation walks read it, and on main a `_reextract/` beat
  them five ways: a helper in mid-edit ("could not parse"), a test there
  satisfying a stale citation (a FALSE PASS), a comment there naming a
  test nothing defines, a test file in mid-edit, and a mode-000 directory
  ("permission denied", both walks). **The last is why the rule answers
  before the listing**: `filepath.Walk` lists a directory before its
  callback can skip it, so the hash-cost walk, the one root sweep still on
  it, moved to `filepath.WalkDir`; and a per-file rule (descend, open no
  Go file below a `_` directory) handles four of the five, which is what
  the unlistable fixture rows pin. **Only the `_` rule is borrowed**: the
  go tool's `.` rule would drop the doc and the Go program git tracks
  under `.github/`. The root stays exempt, so a checkout cloned into `_x`
  is read. It is spelled inline, as the other three root sweeps spell it,
  not in `sweeptest`: a one-line name test below each walk's one root
  exemption. **It drops no tracked file, and here that is a census, not a
  construction**: unlike a file inside another checkout, git would track
  one below a `_` directory, and a doc there would go unread by the
  citation guard. None is tracked, and the files each walk opened, listed
  before and after over a clean tree and over the main checkout, are the
  same 410 and 1,172.
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
- **A test that fails only where a tool is missing is a finding about the
  product on such hosts, until the code says otherwise.** #1010 saw
  `TestInitRefusesToSaveAPortItNeverGraded` and
  `TestInitDoesNotExcuseAChangedPortWithItsOwnLivePID` fail in the stock
  `golang` image, on main too, and put it down to the container, which
  has no lsof. The tests were right: `bridge init` saved a port another
  process held on every host without lsof (#1021). Before calling a
  failure environmental, read what the missing tool changes in the code.
  A failure message that names a cause is a claim too: "…excused because
  our own recorded pid is alive" was false there, since that pass had
  cleared the pid file. **And a seam forced both ways by two tests pins
  the dependence it controls.** `TestPortCheck_BusyFailsWithoutOwnPID`
  forced `portProbeAvailable` true "so the verdict doesn't depend on
  whether lsof happens to be installed", and a sibling forced it false
  and pinned the warn: one set of facts, two verdicts, both asserted.
- **kill(pid, 0) finds a ZOMBIE, so it cannot tell a test whether a
  process it did not reap has exited: who reaps that one is up to init.**
  `TestServeLeavesNoTailscaleCLIRunning` and
  `TestCancelStopsTheWholeCLIProcessTree` kill a grandchild, the fake CLI
  behind a wrapper shell, and polled kill(pid, 0) for ESRCH. The orphan
  goes to the pid namespace's init, and systemd, launchd and the tini of
  `docker run --init` each reap it within milliseconds, so every host CI
  has passed. In a container run WITHOUT `--init`, PID 1 is the
  container's command, `go test` in the stock golang image, which collects
  only its own children: both tests failed there on dido, as root and as
  uid 1000, with the fake at `State: Z (zombie)`, `PPid: 1`. The previous
  bullet's question came out the other way here: serve had killed the
  CLI, and a zombie runs nothing and holds no files, so it writes nothing,
  which is all the tests guard. Both now ask `internal/proctest.Exited`:
  kill(pid, 0)'s ESRCH, and on Linux a process it still finds reads as
  exited when EVERY task in `/proc/<pid>/task` is Z or X and a second
  listing, taken after those reads, shows no task it did not read. **Not
  `/proc/<pid>/stat` alone**: a process whose leader thread exited while
  another runs reads Z there and in `status` (measured: `Threads: 2`, the
  other task S). Whatever /proc cannot answer reads as running, including
  a /proc numbered for another pid namespace, whose `self` is not this
  process (under `unshare --pid` without `--mount-proc`, pid 1 read
  `/proc/self` as 480456), so /proc can delay an exit and never invent
  one. **A control asked once, straight after `Start`, sees the child
  still being exec'd** (state R, 8 of 8) and passed a mutation that took
  every state but R for exited, so `TestARunningProcessHasNotExited` asks
  over 500 ms, as the callers poll, and on Linux requires /proc's S. Probe
  a child you DO reap by reaping it (`cmd.Wait`, then ESRCH), as doctor's
  `pidAlive` tests do. (#1024)
- **A process a test holds on a release file must end by itself once
  nobody can release it, so hold it with `proctest.HoldUntilReleased`,
  never a hand-rolled loop.** Both tailscale shutdown fakes looped
  `while [ ! -e release ]` on a file in a `t.TempDir`. When the fake
  survived, the defect those tests catch, the test's cleanup created the
  file and t.TempDir's own cleanup, registered earlier and so run next,
  removed the directory straight after. Polling every 20 ms, the fake
  almost never saw it and looped forever, reparented to init: under a
  mutation that let the CLI survive, 23 of 25 fakes stayed on macOS and 6
  of 8 on Linux. A file that exists for microseconds is a signal a poller
  misses; a directory that is gone stays gone. So the hold's wait also
  ends, with status 3 and the fake's work not run, once the release
  file's directory is gone or the test binary that built the script is.
  The second covers a run that dies before its cleanups: a
  `-test.timeout` panic or a SIGINT left a fake on main and none with the
  hold. Neither is true while the test runs, so neither weakens the tests:
  under the mutation both stay red on every host, and with
  `proctest.Exited` made to lie both fail on their "wrote after the
  cancel" check. Three traps met on the way. **Where the hold is what is
  under test, its failure path cannot lean on it**:
  `TestAHeldShellEndsWithItsTestBinary` released its shell and failed,
  and under the control that restored the old loop it left that shell
  looping, the same defect one level down; it now releases and waits.
  **A child that holds a shell must reap it**, or a hold that ends at
  once leaves a zombie that kill(pid, 0) finds on macOS, and the check
  that it holds passed that control. **A harness that means to kill a
  test binary with SIGINT must reset the signal first**: a background job
  of a non-interactive shell starts with SIGINT ignored and Go keeps it
  that way, so `kill -INT` did nothing, and what read as a leak on main
  was a test still running. (#1025)
- **A test that starts a child with `SysProcAttr.Credential` changes
  its OWN dumpable flag on Linux** (#1032). Go forks with
  `CLONE_VFORK|CLONE_VM` (unless a user namespace is asked for), so the
  child's setuid or setgid runs `commit_creds` on memory it still shares
  with the test process, and the kernel resets that memory's dumpable
  flag to `fs.suid_dumpable` (2 on Ubuntu). Root without CAP_SYS_PTRACE (a
  container's) then cannot read the test process's descriptors, and every
  later test that attributes a port to it fails: only as root, only where
  lsof is installed, and never alone (measured on dido: two #1028 tests
  after the first form of #1032's L7 kernel test). **Have the child drop
  to its uid itself, after exec** (`dropToChildUID`), and assert the test
  process's `PR_GET_DUMPABLE` did not move. No production code uses
  `Credential`; one that did would make the BRIDGE non-dumpable the same
  way, hiding it from every same-user owner probe.
- **A test child that holds a listener keeps it reachable past its wait,
  with `runtime.KeepAlive` after the wait** (#1036). net closes a listener
  nothing references from a finalizer, and `runUndumpable` held its
  listener only in a local nothing read after the port, so a collection
  while it waited would have freed the port a test records as the
  bridge's or the holder's (`runListeningChild` had the KeepAlive since
  #1034). **It was latent for a reason the timer hides**: the runtime
  forces a collection two minutes after the last one only once one has
  run, and on Linux the child's startup runs none at the default GOGC
  (macOS's does), so it took a bigger init or a lower GOGC to arm it.
  Both children now collect before they say they are ready
  (`collectBeforeReady`), so a child that drops its listener fails every
  test that uses it at once, on every platform. The rule is not about
  listeners: a file or a conn a child or a test keeps only in a local is
  closed by a collection the same way, and only something that still
  refers to it keeps it open (a `defer` or a `t.Cleanup` that names it, a
  KeepAlive after the wait).
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
  **A window that ends in a PRINT to serve's own writer is parked by that
  writer**: runServe takes stdout and stderr as `io.Writer`s, so a test's
  writer that holds the first line containing a marker stops serve right
  there (`holdingWriter`, #1009: serve's exit reason, `tsnet close:`,
  `Shutting down`). With none of these, oversubscription is the fallback:
  build with `go test -c -race`, then run about three processes per core.
  Idle stress passing is not evidence the window is absent. (#987)
- **A running SQLite statement is parked from INSIDE it, by a Go collation**
  (#998). `VACUUM` copies an index with an append fast path that compares
  no keys, except an index with a non-BINARY collation, which it rebuilds by
  seeks (SQLite's `insert.c`, `xferOptimization`). `internal/sqlitetest`
  registers such a collation in Go (it lived in `internal/backup/backuptest`
  until the scanner needed it), so a VACUUM of a database `WriteSource`
  wrote calls back mid-copy with its destination file already created, and
  `ParkVacuum` holds it there. The same collation stops ANY statement that
  maintains an index a test created under it: an UPDATE that moves a key,
  an INSERT, a DELETE. The key is compared only against keys already in the
  index, so it needs a second row, and a partial index keeps the statements
  the test does not want to stop out of it. A cancel then reaches the statement only
  through modernc's own `interruptOnDone` goroutine, which has to be
  scheduled before the copy finishes, so `ReleaseUntil` lets comparisons go
  one at a time after a `runtime.Gosched`. Measured over 200 runs under
  `-race`: letting them all go at once let the copy finish before the
  cancel landed in 2 runs at `GOMAXPROCS=1`; one at a time without the
  yield, the cancel landed as late as the last comparison. Two traps met on
  the way. A held LOCK is not a park inside the statement: a VACUUM of a
  DELETE-mode source behind `BEGIN EXCLUSIVE` waits before the VACUUM
  starts. (That wait was `Ping`'s `select 1` under `busy_timeout(5000)`,
  and a cancel there came back as `database is locked` 5 s later, until
  B63 took the Ping out and gave the wait to `retryWhileBusy`, which
  answers it as `context.Canceled`: the snapshot bullet under The CLI and
  the serve wiring.) And modernc's `Driver.Open` reads its collation and
  hook lists without a lock, so register in `init`. **A Park's waits give
  up: `Arm`'s 10 s after each begins, for a statement the test itself
  started, and `ArmUntil`'s at the instant it is given.** A statement a
  serve runs is parked with `ArmUntil(t, serveGiveUpTime(t))`, waited for
  with `WaitUnless(t, exited)`, which reports serve's own exit rather than
  a timeout, and let go by a `Disarm` registered after the drain (B63: the
  seed test's `Arm` failed "no statement compared a key within 10s" on a
  Windows leg whose boot took longer, and its release loop's 10 s also
  bounded serve's whole teardown).
- **A test handler that holds a request until the client gives up must
  DRAIN a POST's body first** (#1003). net/http notices a client hanging up
  only once the request body is consumed, so a POST held unread sits out the
  handler's whole bound, and the test measures the timeout rather than the
  cancel. A cancel meant for a response's WRITE step is taken on the body's
  first `Read`, through a wrapping transport: one taken in the handler raced
  the client reading the headers and stopped the fetch instead in about half
  the runs, where the transport form stayed on the write in 50 of 50 under
  `-race` (#1001).

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
  A `discovery.SendFailureLog`'s streak (each discovery client's `sendErrs`,
  the renderer's `sendErrStreak` until 2026-09-28) is only ever touched from
  its own tick loop, so a test calling its `Note` or `Reset` directly must do
  so with no loop live — before `Start`, or after `Stop` (which joins it).
  One that did neither raced under `-race` on CI and was not reproducible
  locally in 26 runs. Adding a mutex would pay production for a test's
  convenience.
- **…and a test that starts the loop decides what the loop's own sends do**
  (2026-09-28). `TestSendMSearchStreakResetsOnRestart` kept that ordering and
  still failed 10 of 200 runs on the dev Mac and 17 of 1,000 on Linux under
  `-race`: `Start` spawns the tick loop, whose first send lost a race with
  `Stop`'s close and took the streak to 1 before the failure the test drove,
  so that one logged nothing. Moving the capture before `Start` is not the
  fix: where a send goes through, the loop's SUCCESS resets the streak, and
  that version passed 5 of 5 on both hosts with `Start`'s reset deleted. The
  per-client `writeMSearch` seam makes the restarted loop's own first send
  fail on every host, and the test asserts that send's Warn; with the reset
  deleted it fails 20 of 20 on both. A test whose subject a live loop also
  moves cannot leave that loop's I/O to the host. The upstream client got
  the same seam and the same restart test
  (`TestUpstreamSendStreakResetsOnRestart`) when it gained the report.
- **Putting back slog's previous default does not put back the `log`
  package, so a capture goes through `loggingtest.SetDefault`** (2026-09-28).
  `slog.SetDefault` points the log package's output at the new handler and
  zeroes its flags, and `slog.SetDefault(prev)` with slog's own default (the
  one every test binary starts with) undoes neither, while that handler
  writes THROUGH the log package. So after the first capture in a binary,
  every later default-logger line went into the finished test's buffer: of
  200 runs of one discovery test, 1 printed its lines (200 after), and a
  failing test's diagnostics are what that swallows. It is also why the
  restart flake above showed no Warn. `SetDefault` puts back the default, the
  output and the flags, **the output and flags AFTER the default**: putting
  back a default whose handler is NOT slog's own points the log package at
  that handler again and zeroes its flags, so the other order ends on that
  handler, and only a test whose prior default is one it set can see it
  (`TestInstallersRestoreTheStandardLogger`, written in a parallel session
  and adopted here; swapped, its two such cases go red and every test over
  slog's own default stays green). `Record`, `ParkOn` and both capture
  helpers in `internal/dlna` use it (`handshaketest`, which redirects the
  log package itself, already put back its output, flags and prefix), and so
  does every capture outside the logging packages' own tests (the sweep
  below). **It refuses a parallel test, and a refusal changes nothing**: it
  calls `t.Setenv` before anything else, so a parallel test (or one with a
  parallel ancestor) panics there, and a later `t.Parallel` panics too. Two
  overlapping captures put back each other's state and leave the default on
  a finished test's handler, and `-race` cannot see it, since slog's default
  is an atomic pointer and the log package locks its output. Gemini on
  #1064 asked for a docblock warning; a rule stated only in prose (the
  `omitempty` time rule) was broken in ten fields before a guard went in, so
  this one is enforced. **And outside the logging packages' own tests, a
  test file may not swap the default by hand** (#1075): twelve files still
  put back only the default (29 references), and after each one's test a
  later test's `slog.Info` and `log.Print` both went into the finished
  test's handler, 0 of 2 lines reaching the output against 2 of 2 run alone
  (14 tests, go1.26.6); 2 of 2 once they went through `SetDefault`.
  `TestNoTestSetsTheDefaultLoggerByHand`
  (cmd/bridge) fails on a TEST file naming `slog.SetDefault` outside
  loggingtest's own tests (which build a prior default by hand, since the
  restore is their subject) or `logging.Init` outside internal/logging:
  through any import name, called or not (a `defer`, a method value), and on
  a dot import of either, whose calls carry no package name to read. `Init`
  counts because it is `slog.SetDefault` behind a once, with no restore; the
  metrics test called it. A test that wants a handler of its own installs it
  with `SetDefault`, never `Init`. **internal/logging's own tests call `Init`
  unqualified, which the scan cannot read**, so there `resetOnce(t)` puts
  back the default, the log package and Init's once, and the package's
  `TestMain` fails the run when a test left any of the four changed: a test
  that calls `Init` without `resetOnce(t)` passes itself and every other
  test, and fails only there.
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
  five files missed a sixth site that the shape match found.) **It reads
  every function in the test files, helpers included** (2026-09-28): it read
  Test functions alone, so a launch factored into a helper went unaudited,
  and two such helpers already existed. It wants the drain in the function
  that launches. **A serve test boots through a helper**: `bootServe`
  (main_test.go), which runs a command line, or `startConsoleBridge`
  (served_bridge_test.go), which writes the config, calls `runServe` with
  a `serveOpts` hook no flag carries, and builds the console and phone
  clients. Both start serve through `launchServe`, the one launch and the
  one drain, and with that drain deleted the Test-only guard stayed green.
  SonarCloud's gate fails a PR past 3% duplicated new lines and counts new
  lines that repeat OLD code, and an inline boot block was 16 to 23 lines
  of exactly that.
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
- **A serve test waits for an EVENT until the test binary's deadline, never
  for a fixed time after a boot or a cancel** (2026-09-29, backlog B63).
  Serve's boot migrates and writes the store and its teardown checkpoints
  and closes it: disk writes with no bound a starved host keeps to. On
  nomos (Windows 11) with 24 writers syncing to its disk and 16 CPU hogs on
  the test's four CPUs, the store close took up to 16.4 s after the grace
  and a boot more than 30 s, and main's bounds failed 12 of 24 runs of the
  three tests CI had failed ("no statement compared a key within 10s" 4 of
  8, "serve never reached the tsnet start within 30s" and "runServe did not
  return" 6 of 8, "serve never reached the tailnet listen within 30s" 2 of
  8). With the waits below none of 24 failed, the seed test taking up to
  74 s.
  `serveGiveUp` (serve_wait_test.go) fires at the test's deadline less
  `serveWaitReserve` (30 s), and every wait on serve in a serve test uses
  it: the boot milestones (`waitForListening`, `waitForAdminReady`,
  `waitForServe`, the tailscale fakes' starts), the exits (`waitServeExit`,
  `waitBoundedServeExit`, `stopLiveServe`) and serve's work (the orphan
  sweep's refusal, the sox gate's sweep and scan). A wait that gives up
  prints serve's goroutines (`serveStacks`), and one on a serve that exits
  first reports its exit code. **The drains give up LATER**
  (`serveDrainGiveUp`, the deadline less 10 s): a drain after a wait that
  gave up still sees serve out once the cleanups after it let go, and with
  one give-up for both it reported a second failure about a serve on its
  way out. **So a hold the test takes on serve is released in a cleanup
  registered AFTER the drain**, or the drain waits on it to the deadline:
  a gate, a held print, a held mint, a sqlitetest Park (its own `Disarm`
  runs after the drain). **And nothing waits BEFORE the drain on what
  only the drain's cancel ends**: the wedge test's cleanup waited for its
  start there, on a start the drain's cancel releases, and every failed
  boot also reported "the released start did not return" (5 of 8). **A
  hang is still caught, at the deadline**: with the tsnet join unbounded,
  the wedge test failed at 60 s under `-timeout 90s`, its report naming
  `tsnetFront.stop`; debug a hang with a shorter `-timeout`. What a test
  pins about serve's pace stays on what serve does before its teardown's
  disk writes, from what it bounds: the drains-together test's order (a
  grace), a give-up's lower bound, and the tsnet join test's "no give-up
  line", asserted only when its start finished unwinding a second or more
  inside the grace (it measures when, and a 1 s start still fails the
  control). **Not converted** (backlog B107): the in-process tests' waits
  (the rescanner, the sweep passes, the album-gain render, sqlitetest's
  `Arm` in internal/backup and internal/manifest), loggingtest's 3 s
  `Park.Wait`, the tsnet front's unit tests, and clients' per-request
  timeouts.
- **…and a watcher test waits for the watcher's walk, a watch and a row
  the same way** (2026-09-29, backlog B104). The watcher tests slept 100
  or 150 ms for Run's initial walk and gave the row 3 s, and a file created
  before its directory is watched makes no event at all:
  `TestWatcherWatchesARootThatIsALinkToALink` failed that way on the macOS
  CI leg ("…never reached the manifest through the watcher"), and a drop
  with no sleep fails it 10 of 10. On a Linux host starved by a CPU hog
  (one CPU, cgroup weight 1 against 100) the walk ended up to 288 ms after
  Run started, the test failed 11 of 110 runs and the four other watcher
  tests with the same sleep 26 of 50; after, none of 100 and none of 50.
  `startWatcher` returns once `afterInitialWalkHookForTests` says every
  watch is registered (per instance, set before Run, as
  `afterDispatchHookForTests` is), and `watchWaitUntil` waits for a row or
  a folder's watch until the test binary's deadline less
  `watchWaitReserve`, or less half of what is left when that is less: a
  whole reserve under `-timeout 20s` put every give-up in the past, and
  each wait failed at once (CodeRabbit on #1115; B63's serve helpers keep
  the whole reserve). **A wait that can end on a failure event does**:
  the link-chain test stops on a subtree scan outside the root (#1090's
  defect, red in 0.09 s), and the dot-named and linked-root tests read the
  watch list once the walk is done. An absence has no event of its own, so
  `TestWatcherIgnoresDotfiles` drops a track after its dotfiles and waits
  for a scan that indexed the track to return
  (`afterDispatchHookForTests`): that scan listed the dotfiles too. Its
  dotfile was `.DS_Store`, which the scan's extension filter keeps out
  whatever the dot rule says; `._track.flac` (macOS's AppleDouble twin on
  exFAT or SMB) is kept out by the dot alone, and goes red without it.
  That test said until then that a dotfile's event triggers no scan;
  handleEvent filters by operation, not by name, so it always did.
- **A serve test that needs a tool the live gate probes answers the
  probe, never with a process on PATH** (2026-09-29, backlog B105).
  `TestServeProjectionFollowsTheLiveUpscaleGate` opened the upscale gate
  with a stand-in sox, a shell script first on PATH. `ProbeSox` gives
  `sox --help` 2 s, a timed-out probe reads as no sox, and the shared
  cache keeps that 30 s, so under sibling sessions' load the dev Mac
  failed step 1 ("/v1/health says upscaleEnabled=false with the flag at
  true…") in two sessions' gate runs. A stand-in slower than 2 s fails it
  every time, and on a Linux host starved by a CPU hog, under -race, the
  health request that ran the probe took up to 2.08 s. `serveOpts.soxProbe`
  stands in for the probe inside `soxToolchainCache`, the one sox probe
  serve makes (`withUsableSox`), which also puts the test's health check
  on Windows, where no shell script runs. With every `/bin/sh` exec
  delayed 2.2 s, main failed 10 of 10 and the branch none. **Serve's boot
  line reads that probe too**: it probed for itself (`soxFeatureReady`),
  the one consumer the stand-in did not reach, and a second fork at boot
  (`TestServeBootLineReadsTheSharedSoxProbe`). The product's 2 s is not
  the test's to raise.
- **Put a test seam on the instance it serves; where a package-level seam
  must remain, its restore runs after every goroutine that read it has
  FINISHED, and cleanups run last-registered-first** (2026-09-28). The
  first half is the per-SERVER rule under the 2026-09-09 LOUPE entries,
  met a second time. `statFunc`, the reachability probe's `os.Stat` seam,
  was a package variable, and `TestReachabilityProbe_InflightGuardIsPerRoot`
  restored it in a cleanup registered after `hangingStat`'s release, so
  the restore ran FIRST, while the probe goroutine that had read the seam
  was still parked in the stand-in; CI reported the race once and the rerun
  passed. **A released goroutine has not finished**: the release is the
  test's own close, which puts the test before the goroutine and not after
  it, so release-then-restore raced 5 runs in 5, as did release, a 50 ms
  sleep, then restore; only something the goroutine does AFTER its read,
  seen through synchronisation (its delete of its in-flight flag, under
  `c.mu`), orders the read. The first fix waited for exactly that, and
  CodeRabbit found the flaw a wait keeps: past its deadline it still had
  to restore, so it traded the race for a bound. The seam is now
  `reachabilityCache.stat`, set to `os.Stat` by `newReachabilityCache`
  and read by `probeLocked` under `c.mu` into a local the stat goroutine
  calls, so a test's stand-in reaches only its own cache and nothing is
  put back. **A race report is only as good as the detector's history**:
  it keeps four accesses per memory word, and a racing read was reported
  in 10 runs of 10 when two synchronised reads from other goroutines
  followed it, and in 0 of 10 when three did (a 30-line probe, go1.26.6).
  Here the healthy probe's read displaced the hung one's once the sibling
  test's accesses were in the word: 0 of 10 runs after the sibling and 0
  of 3 whole-package runs reported it, against 10 of 10 run alone.
  **Reproduce and negative-control such a race with the one test alone**
  (`-run '^Name$'`). The tree restores about fifty other package-level
  seams in one-line cleanups; they were not audited for this shape.
- **Windows CI catches wall-clock assumptions** — ~15.6 ms granularity means two
  stamps milliseconds apart are not reliably ordered. Assert on counted events,
  and detect "was this rewritten?" by planted CONTENT, never by comparing mtimes
  (two writes in one tick leave them equal, so the check silently passes on the
  platform most likely to break). Normalize CRLF before any `\n`-literal scan of
  a static file — there is no `.gitattributes` pinning `eol`. **A time decoded
  from JSON has no monotonic reading**, so `After` against a local
  `time.Now()` compares wall clocks, which is where the tick bites:
  `TestServeWithoutSoxReportsUpscalingOffOnEverySurface` waited for the Jobs
  card's `lastFinishedAt` to pass the instant of its nudge, a sweep the gate
  refuses finished inside the tick, and the wait ran out on the Windows leg
  (2026-09-28; 6 runs of 6 under a simulated 15.625 ms clock). A serve test
  counts through a `serveOpts` hook instead (`autoOptimizeSwept`). **An mtime
  compare is wrong in BOTH directions** (B52, 2026-09-28): "was it written?"
  FAILS correct code when both writes land in one tick, and "was it NOT
  rewritten?" passes with the guard removed. Five tests held the pattern with
  this rule in front of them (four in internal/auth, the tls reload test).
  They count writes (`inCommitWindow`), read the file back through a fresh
  store, and compare identity (`os.SameFile`): every write here stages a new
  file and renames it, so a rewritten path is another file, which bytes
  cannot tell for a rewrite of the same bytes. **A FAT disk image reproduces
  the class on any Mac**: `hdiutil create -fs MS-DOS` keeps 2 s mtimes, and
  `TMPDIR` on it puts every `t.TempDir()` there.
- **A port free on BOTH TCP and UDP cannot come from either allocator, so
  `freeLoopbackTCPAndUDPAddr` binds random numbers from 20000–32767 on both at
  once** (#1026). Windows hands ephemeral ports out IN SEQUENCE, TCP and UDP
  each from a cursor of its own (macOS does the same for TCP; Linux draws both
  at random), so asking an allocator again tests the NEXT number, not another
  one: the old helper's twenty TCP draws were twenty consecutive numbers on
  UDP. CI's Windows runners carry per-protocol WinNAT reservations: 200 UDP
  numbers the TCP cursor still hands out (starting anywhere from 49509 to
  58788) and 200 TCP numbers the UDP cursor still hands out (49698–49897), on
  six of seven VMs sampled. A draw started in front of either block fails
  twenty times with WSAEACCES, reproduced on three runners of three: the shape
  of the one CI failure, twenty refusals in 10 ms. **So "draw UDP first" moves
  the failure to the other block, more draws buy numbers the 200-long block
  still covers, and parsing `netsh … excludedportrange` misses a run of held
  sockets.** Random numbers are independent draws, and 20000–32767 lies below
  every target platform's ephemeral range (Linux 32768, Windows and macOS
  49152), so no allocator can hand the number out before serve binds it:
  30,000 calls on six runners rejected no draw. A failure names every address
  tried and its error; the old message said only "20 draws", which is why
  finding this took a probe on the runner.
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
  review runs within minutes. **Unless the notice's own wait has passed**:
  on #1008 (2026-09-25) it said "wait 1 minute for your next included
  review", nothing had resumed two minutes later, and `@coderabbitai
  review` then answered "Review triggered" and ran a full pass. Once the
  wait is up, the command is the included route; the checkbox is an
  on-demand review, which the notice prices per reviewed file.
  `/gemini review` still works as written.
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
  comment. **Compare `coveredCommitId` to the PR head, never
  `headCommitId`**: the marker is `final_review_risk_coverage`'s
  `"coveredCommitId"`, and it moves only when a review finishes. This
  bullet said `headCommitId` until #1022, and a paused walkthrough defeats
  that check: its "Run this review for free" checkbox carries the head's
  `headCommitId` while the last round's verdict string stays in the
  comment, so on #1022 grep said "No actionable comments" and head
  `4eb5ff6f` while `coveredCommitId` said `5ff22781`. Absence of new
  findings is not a pass, and saying so out loud without checking is how
  this was learned twice. (#967–#972) **Editing the PR body re-renders the
  walkthrough and drops a pause notice's checkbox**; `@coderabbitai review`
  then answers "Review rate limited" and puts the notice, checkbox and all,
  back for the current head (#1022). **Push nothing while a ticked review
  runs**: on #1026 an on-demand run still "Starting" when two fix commits
  landed never posted, and the notice came back for the new head, so the
  checkbox had to be ticked again.
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
  was the one directory rule #995 added (#1007 added the `.git` rule, for a
  checkout with no go.mod); #993's reason for not borrowing the go tool's
  `.`-directory rule (`.github/`) still holds. (#995)
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
  "a login link is live now". (`prunedTickets` is gone since 2026-09-28: each
  ticket is a file of its own, a miss writes nothing at all, and pruning is a
  mint's; the ticket bullet under Auth, pairing, TLS.)
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
  the struct. (A second case, 2026-09-28: the reachability probe's stat
  seam, whose restore raced a parked probe; under **Build, CI, and test
  discipline**.)
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
- **A "compilation failure" claim that is really a test failure.** `os.Geteuid` IS defined on Windows (returns -1). Fixing the *stated* cause (a `//go:build !windows` tag) would have hidden a test from the platform where its other branch still works; the real fix was a runtime skip. The claim recurred on #1023, twice in one Gemini review, about root skips in files with no build tag. `GOOS=windows go test -c -o /dev/null <pkg>` settles it in seconds, and the PR's own `test (windows-latest)` leg is the evidence to cite.

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

1. **Branch off main.** `git checkout -b feat/<topic>`. Never push code directly to main (CLAUDE.md-only docs changes are the sole exception; a rule that describes code is not one: it lands in the PR that changes the code, with its log record, so a review reads the two together).
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

**See `ops/deployment-runbook.md`** — 3-step flow (local `/tmp/bridge-live/` fixture → home-pc Windows → the operator bridge, on the NUC since 2026-09-22 and on the `bridge.ars.md` VPS before that). Read it before deploying.
## Multi-PR batch workflow

For any larger job spanning **3+ PRs**, use the **stack-and-batch** pattern instead of the default serial merge-after-each. Time-validated against the v1.2 improvements batch (PRs #76 / #81 / #82 / #83 / #84 / #85 — security + slog + CLI + fsnotify + Docker + post-merge follow-ups). The serial pattern would have spent ~30 min just on bot-review wait windows; the stacked pattern collapsed that to one ~6 min wait.

1. **Plan first.** Write the plan to the plan file before any code. Cross-PR invariants caught at plan time save hours of post-merge debugging — the dropped `db.SetMaxOpenConns(1)` from PR #76's first draft is the canonical example. The plan file is the highest-leverage artifact in the batch; every "Things that have bitten before" entry traces back to a deliberate plan-time decision.
2. **Stack PRs end-to-end.** Each branch bases off the prior one's tip (`git checkout -b feat/X feat/W`); each PR's `base` is the prior branch, NOT main. Open all PRs in one pass without waiting between them. Build PR-N+1 while bots review PR-N — overlap the work.
3. **One 6-minute wait** after the last PR opens. Bots (CodeRabbit / Gemini / Qodo) post within ~3 min; 6 min covers the slow-arrival tail. Don't poll; use ScheduleWakeup once.
4. **Address all comments in one combined pass per branch.** Don't merge anything yet. Bots see cross-PR context on a stack — their PR-N+1 comments may reference PR-N's invariants, and folding both into a single fix is cheaper than amending after merge. Reject bot suggestions that contradict deliberate in-code rationale (the PR-76 review's "cache transient MB errors" suggestion vs the PR #74 invariant is the canonical example — verify, don't blindly apply).
5. **Merge bottom-up in dependency order at the end.** GitHub auto-closes a stacked PR when its base branch is deleted, so as each ancestor merges, retarget the next via `git rebase --onto main <ancestor-tip>` and open a fresh PR (the previous one auto-closed). Plan ~2 min of rebase per child PR; `--reapply-cherry-picks` is unnecessary because already-applied commits are detected and skipped automatically.
6. **One combined follow-up PR** for any post-merge bot comments. Bots run a second review pass after the first round of fixes lands — that's the realistic floor (two rounds, not one). Batch the late-arriving items into a single follow-up PR rather than amending merged branches.
7. **CLAUDE.md-only updates direct to main.** A change to `CLAUDE.md` alone (the batch's closing entry, a corrected claim) is the one change that bypasses the feature-branch path, per step 1 of the Development workflow. A rule that describes a PR's code rides in that PR, and a change to any other doc, `AGENTS.md` included, goes through a PR. This step said "docs-only changes bypass the feature-branch path" until 2026-09-29, and `AGENTS.md` said new invariants went "committed direct to `main`", which review bots quoted back on PRs carrying a rule with its code (#1079, #1087).
8. **End-of-session quality gate.** `make fmt vet test build-all` on bridge, `xcodebuild build` on iOS, resolve any warnings before reporting done. The stacked workflow's main risk is cross-PR drift; the build matrix catches it cheaply at the end.
9. **Post-merge deploy** after the whole stack lands — see [Post-merge deployment](#post-merge-deployment). For a stack, deploy ONCE at the end carrying every merged fix, not per-merge.

**Avoid small PRs.** 5 cohesive ~200-line PRs ship faster and review better than 15 micro-PRs — bots calibrate review priority to PR scope, and a coherent theme per PR makes the eventual squash-commit message useful as ship-history.

**When NOT to stack:** if PRs are genuinely independent (disjoint files, no semantic dependencies, no shared invariants), open them in parallel against main rather than stacking — review converges faster and there's no rebase cost. Stack only when PR-N+1 logically depends on PR-N's API surface or invariants.
