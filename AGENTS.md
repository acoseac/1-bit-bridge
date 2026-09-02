# AGENTS.md — 1-bit-bridge

Guidance for AI coding agents working in this repository. Assumes no prior
knowledge of the project. [`CLAUDE.md`](CLAUDE.md) is the deeper
institutional-knowledge companion — its "things that have bitten before"
section carries the project's invariants grouped by subsystem. Consult it
before touching concurrency, the wire protocol, or any subsystem flagged
below. The evidence behind each invariant (the measurement, the rejected
alternative, the test that pins it, the PR) lives in
[`ops/engineering-log.md`](ops/engineering-log.md); grep it by symbol or PR
number when a rule looks arbitrary.

**Agent-doc maintenance.** `CLAUDE.md` is the authority, and it is a living
document: new invariants — the "things that have bitten before" entries a
fix produces — go THERE, committed direct to `main` as that file's own
convention prescribes, with the supporting record appended to
`ops/engineering-log.md`. Only `CLAUDE.md` is auto-loaded into an agent
session, so a finding written only into the log is inert. This file is the
onboarding layer: project shape, build commands, module map. Keep it current
when those change, and don't duplicate invariant history into it.

(An earlier revision of this line said to treat `CLAUDE.md` as read-only and
never edit it. That contradicted `CLAUDE.md`'s own "CLAUDE.md updates direct
to main" rule and would have stranded every future invariant in the wrong
file; the two docs would have drifted apart within weeks.)

## Project overview

**1-bit-bridge** is a cross-platform Go companion server for the
[1-bit](https://apps.apple.com/us/app/1-bit/id6762529497) iOS music player
(bit-exact playback, incl. DSD). It replaces SMB as the transport over
high-latency links (Tailscale / DERP relay) with an HTTPS API: HTTP/2 +
HTTP/3 (QUIC), bearer-token auth, TLS fingerprint pinning, and a pre-built
SQLite-backed library manifest so the iOS app syncs its whole library in one
request instead of walking an SMB tree.

- Module: `github.com/acoseac/1-bit-bridge`, binary: `bridge` (from `./cmd/bridge`).
- Pure-Go, CGO-free stack (`modernc.org/sqlite`, `mewkiz/flac`, `dhowden/tag`,
  `hashicorp/mdns`, `quic-go`, `tailscale.com/tsnet`) — one static binary, no
  runtime deps.
- Target platforms: macOS / Linux / Windows × amd64 / arm64. Tested primarily
  on macOS. Current release line: v0.1.9; wire protocol: **v1** (additive only).
- **Bit-exact by mission: no server-side transcoding, ever.** `/v1/download`
  serves the file as-is via `http.ServeContent`.

## Build and test commands

Requires the Go toolchain matching `go.mod` — trust `go.mod` (README tracks
it).

| Command | What it does |
|---|---|
| `make build` | Builds `./bin/bridge` for the host OS (injects `VERSION` from `git describe` into `internal/version.ServerVersion`). |
| `make build-all` | Cross-compiles all 6 targets into `dist/bridge-<os>-<arch>{.exe}`. |
| `make test` | Full suite: `go test -p $(P) -race -timeout 30m ./...`. |
| `make test-fast` | Same without `-race` — the inner-loop option. |
| `make check` | Per-change gate: `fmt` + `vet` + `test` (race), sequentially; skips `build-all`. |
| `make fmt` / `make vet` | `go fmt ./...` / `go vet ./...`. |
| `make run` | `go run ./cmd/bridge serve --config config/bridge.yaml.example`. |
| `make check-go-version` | Guards that `Dockerfile`'s `ARG GO_VERSION` matches `go.mod`'s major.minor (alpine sets `GOTOOLCHAIN=local`, so a stale ARG fails the image build). |
| `make docker` | Builds the container image (runs `check-go-version` first). |
| `make clean` | Removes `bin/` and `dist/`. |

- **`P` caps Go's `-p` parallelism** (default 4). The `-race` build plus the
  6-target cross-compile can OOM a RAM-constrained machine; lower with
  `make test P=2`, raise with `P=$(sysctl -n hw.ncpu)` on a roomy box.
- **Pre-push gate: `make fmt vet test build-all`.** `make check` in the inner
  loop; `make build-all` once before pushing. CI mirrors this
  command-for-command — keep CI and Makefile in lockstep.
- The 30m test timeout (not the 10m default) exists because
  `internal/adminauth`'s suite pays real bcrypt cost-12 per simulated login
  under `-race`.
- There is **no `make fixtures` target yet** — `CONTRIBUTING.md` mentions it
  as "to be added".

## Repository layout

```text
cmd/bridge/    CLI entry point (package main) + wiring
internal/      38 packages, one concern each (map below; incl. dlna/discovery)
config/        bridge.yaml.example — annotated sample config
deploy/        operator deploy scripts (canonical copies; sync FROM here)
docs/          PUBLIC website (GitHub Pages) — see Security section
ops/           internal operator docs, deliberately NOT published
testdata/      test fixtures
bin/, dist/    build outputs (git-ignored)
```

Key root files: `PROTOCOL.md` (wire-contract source of truth), `Makefile`,
`Dockerfile`, `.goreleaser.yaml`, `compose.yaml`, `CONTRIBUTING.md`,
`SECURITY.md`, `docs/release-process.md`, `ops/deployment-runbook.md`.

## Architecture / module map

`cmd/bridge` implements the CLI: `init` / `serve` / `pair` / `scan` /
`version` / `doctor` / `analyze` / `tsnet auth` / `admin reset-password` and
more. Bare `bridge` on a real TTY drops into a context-aware launcher menu
(`menu.go`); pipes / non-TTY callers fall through to usage + exit 2 so
automation is unaffected.

| Package | Role |
|---|---|
| `internal/config` | YAML loader with defaults, path-relative resolution, `Save()` for admin edits |
| `internal/tls` | Self-signed ECDSA P-256 cert minter; SHA-256 fingerprint for iOS pinning (`LoadOrGenerate` is sticky — don't re-mint per run) |
| `internal/tlsacme` | `autocert` wrapper — Let's Encrypt certs for public/Tailscale modes |
| `internal/auth` | Bearer-token store: generation, hashed storage, atomic persist, cross-process pickup |
| `internal/adminauth` | Admin console single-user auth (bcrypt credentials, session tokens, login rate limiting) — public mode only |
| `internal/fs` | Path-safe resolver: lexical traversal rejection, multi-root routing, hot-reload via `SetRoots` |
| `internal/fsutil` | Cross-cutting FS helpers (durability barrier: file + parent-dir fsync for sidecars) |
| `internal/dsn` | SQLite URI DSN builder safe for paths with URL-reserved chars, POSIX + Windows |
| `internal/manifest` | Library index: SQLite store, tag extractors (FLAC / DSF / ALAC / WAV / MP3 / M4A), scanner, JSON serializer. Wire types live in `types.go` |
| `internal/enrich` | MusicBrainz + Cover Art Archive + Deezer clients, rate-limited (1.1s / 500ms / 120ms) |
| `internal/atlasharvest` | Bridge-driven bulk metadata harvest from Atlas (iOS provisions the credential) |
| `internal/api` | HTTP/2 + HTTP/3 handlers: `/v1/{health,list,stat,read,download,manifest,artwork,...}`; per-route write deadlines; SSE |
| `internal/admin` | Local web console on `127.0.0.1:7789` — library/devices/stats/settings pages + JSON API + `bridge://` QR pairing |
| `internal/advertise` | Enumerates every reachable endpoint (LAN v4/v6, mDNS, Tailscale) so `/v1/health` self-reports |
| `internal/mdns` | Bonjour `_onebit-bridge._tcp` advertisement |
| `internal/packaging` | launchd plist / systemd unit rendering + install/uninstall, used by `bridge init` |
| `internal/doctor` | Environment preflight checks for `bridge doctor` / `bridge init` |
| `internal/supervision` | Answers "if this process exits, will something relaunch us?" |
| `internal/pairing` | In-memory state machine for the admin-approval pairing flow |
| `internal/analyze` | Offline audio analysis: sox(1) PCM decode → peak waveform sidecars; worker pool (opt-in via `analysis.enabled`) |
| `internal/transcode` | Offline PCM upscaling via sox(1) (variants; never alters the served original) |
| `internal/smartplaylist` | Pure engine for server-side Smart Mixes (Heavy Rotation, Daily Mix, Auto Mix, …) |
| `internal/smartplaylistgen` | Orchestrates smart-playlist regeneration from `manifest.Store` aggregations |
| `internal/backup` | State-dir snapshots + restore |
| `internal/integrity` | Proactive consistency watchers reconciling durable state vs. runtime beliefs |
| `internal/updater` | GitHub Releases polling + self-update (downloads ride a timeout-free client) |
| `internal/metrics` | Prometheus exposition surface |
| `internal/logging` | Structured-logging entry point for all `internal/` packages |
| `internal/lrucache` | Small bounded concurrent-safe LRU (enricher memoization) |
| `internal/atomicwrite` | tmp-file-then-rename commits (no torn cached JPEGs) |
| `internal/tailscale` | Wrapper over the host's Tailscale CLI (status detection, HTTPS auto-pilot) |
| `internal/tsnet` | Embedded tailnet node via `tailscale.com/tsnet` — no external daemon |
| `internal/dlna` | DLNA MediaServer + SSDP discovery so network renderers stream the library bit-exact |
| `internal/upnp` | UPnP/DLNA control-point client toward upstream MediaServers |
| `internal/upnpingest` | Turns upstream UPnP servers into rows in the manifest store |
| `internal/upnpproxy` | HTTP byte proxy fronting upstream UPnP media |
| `internal/version` | `ServerVersion`, `MinClientVersion`, `ProtocolVersion` constants — source of truth for `PROTOCOL.md` |

Runtime shape: two listeners — the **client API** on `:7788` (HTTPS, H2+H3,
bearer auth) and the **admin console** on `127.0.0.1:7789` (loopback-only, no
auth in default `loopback` deployment mode; bcrypt session login in `public`
mode). iOS never talks to the admin listener.

## Wire protocol & the mirror-PR rule

- [`PROTOCOL.md`](PROTOCOL.md) is the **source of truth** for the wire
  contract; a verbatim mirror lives in the iOS repo at
  `com.acoseac.dsdplayer/docs/BridgeProtocol.md`. Protocol version is `1`;
  every response carries `X-Bridge-Protocol: 1`.
- **Breaking wire changes bump `internal/version.ProtocolVersion`** (and the
  iOS `supportedProtocolVersion` together). Additive changes (new optional
  fields with `omitempty`, new endpoints) stay at the same version — add
  optional properties, don't rename existing ones.
- **Mirror-PR rule** (enforced by review convention): any PR touching
  `PROTOCOL.md`, the manifest JSON shape, or any request/response schema MUST
  either link a companion PR on the iOS repo
  ([`github.com/acoseac/1-bit`](https://github.com/acoseac/1-bit); updates
  `docs/BridgeProtocol.md`, the `Tests/…/Fixtures/Bridge/` golden fixtures,
  and any `BridgeSourceClient.swift` decoder changes) or explicitly justify
  `Backward-compatible protocol change, no iOS change required` in the PR body.
- **Wire-type discipline**: `internal/manifest/types.go` (`Track`, `Folder`,
  `Manifest`, `Variant`, `EnrichmentProgress`) IS the wire contract — the
  bridge serializes these directly. Conversely, domain types like
  `auth.Token` / `pairing.Request` must NEVER be passed to a
  `json.NewEncoder(w).Encode(...)` from an `internal/api/` handler — always
  wrap in a DTO defined under `internal/api/`. SQLite row-scan structs in
  `internal/manifest/store.go` must NOT gain `json:` tags for wire
  serialization — narrow exceptions exist for structs that decode SQL
  `json_object(...)` output and admin-side DTOs (e.g. `VariantKindStat`),
  which never cross the client wire. Columns
  selected inside SQL `json_object(...)` aggregations are wire fields and
  follow the same versioning rule.

## Code style guidelines

- Prefer stdlib. Dependencies are admitted one at a time, each justified in
  the PR that adds it.
- Every package carries a one-paragraph `// Package x …` doc comment (some via
  a dedicated `doc.go`); every new exported function/type gets a godoc comment.
- gofmt-clean is CI-enforced (`gofmt.yml`); run `make fmt` before committing.
- GitHub Actions references are **commit-pinned** (with version comments), not
  tag-pinned. Note: `.github/dependabot.yml` does not exist yet — adding it
  (github-actions + gomod + docker ecosystems) so the pin comments are actually
  consumed is a pending quick win (2026-07-21 review).
- `docs/*.html` are redirect stubs to `1-bit.app/bridge/*` (user-facing docs
  live in the separate `acoseac/1bitapp` repo) — **don't edit them**.
- Public-facing prose names the entity **ars.md**, not `acoseac` (README
  prose, release notes, web pages). `acoseac` stays verbatim in genuine
  identifiers: GitHub URLs, the Pages domain, badge URLs, `com.acoseac.*`
  bundle/launchd identifiers, and the author's name in footers.

## Testing instructions

- `make test` (race, 30m timeout) before pushing; `make test-fast` or
  `go test ./internal/<pkg>/` while iterating. The suite is ~2,400 test
  functions across `internal/…` and `cmd/bridge`.
- Tests are pure Go (no cgo); table-driven tests pin most documented
  invariants — when a section below says "pinned by TestX", changing the
  behavior requires changing the test deliberately, not silently.
- **Local live fixture** (no service install):

  ```sh
  make build >/dev/null
  ./bin/bridge init --yes --no-service \
    --dir /tmp/bridge-live --library ~/Music/test-library --name "Test Library"
  ./bin/bridge serve --config /tmp/bridge-live/bridge.yaml &
  # Admin console: http://127.0.0.1:7789/
  ```

  Any folder with a few dozen tagged FLAC / DSF / MP3 / M4A files works.
- **`UPDATE tracks SET enriched_at = 0` only re-triggers the enricher**, not
  tag re-extraction (the scanner's skip gate compares file mtime against
  `Track.ModTime` inside the `tags_json` BLOB — `mtime_ns = 0` does nothing).
  To force re-extraction, delete the affected `tracks` rows and rescan.
- **Since migration v4, an external `sqlite3` CLI cannot DELETE/UPDATE/INSERT
  on `tracks` / `track_variants`** — the expression index uses the
  Go-registered `unicode_lower()` function (`unknown function` error). Either
  delete the DB file and let the startup scan rebuild (loses pairings), or use
  a throwaway Go helper that blank-imports `internal/manifest`.
- **WAL read trap**: an external `sqlite3` SELECT against a live bridge DB can
  read stale state for minutes; verify backfills after a checkpoint/restart.

## Cross-cutting invariants — do not regress

- **No server-side transcoding, ever.** The bridge delivers bytes; iOS
  pre-caches full files before playback (DSD/DoP lock).
- **Rate limits respect the services**: MusicBrainz anon 1 req/s (paced at
  1.1s), Cover Art Archive 500ms, Deezer ~50 req/5s (paced at 120ms).
  User-Agent identifies the app per MB's TOS. Negative-cache MB errors so one
  failing album doesn't spin-loop sibling tracks.
- **TLS fingerprint is captured once at pairing**; rotating the cert requires
  re-pairing. `LoadOrGenerate` is sticky by design.
- **`enriched_at` monotonicity**: upsert resets it to 0 on track change; the
  enricher sets it on completion. The only other sanctioned writers are the
  operator-triggered "retry missing" resets. `WHERE enriched_at = 0` drives
  the worker.
- **Admin console is loopback-only with no auth in the default mode** —
  anyone on the host already owns the token store; auth on top would be
  theatre. Don't expose admin behind Tailscale/reverse-proxy in loopback
  mode; `public` mode (VPS) is the supported non-loopback posture and gates
  everything behind `adminauth`.
- **Graceful shutdown triggers full cleanup**: `POST /api/restart` must invoke
  the same cancellation closure as SIGINT/SIGTERM (honors the `bgScans`
  WaitGroup, cleans up transcode jobs, flushes the auth last-used-at buffer) —
  never a bare `os.Exit(0)`.
- **Dual-stack HTTP/2 + HTTP/3**: QUIC on by default (`disableHttp3: true` or
  `BRIDGE_DISABLE_HTTP3=true` to turn off); graceful `.Shutdown(ctx)` with a
  5s window protects active streams.
- **Single ↔ multi-root storage flip**: adding a second root (or dropping to
  one) changes stored track paths; the admin handler calls
  `store.WipeAllTracks()` before rescanning. Don't migrate in place.
- **Use `safeQuery(r)` for any handler reading a library path from the query
  string** — `r.URL.Query()` form-decodes literal `+` to space.
- Per-route write-deadline overrides are a deliberate registry
  (`internal/api/route_classification.go`, pinned by
  `TestRouteRegistry_writeDeadlineOverrides`); long ops get 15 min.

## Security considerations

- Report vulnerabilities privately via GitHub Security Advisories or
  `support@1-bit.app` — see [`SECURITY.md`](SECURITY.md) for scope/SLA. Never
  open public issues for vulnerabilities.
- **`docs/` is a PUBLIC website** (GitHub Pages serves `main:/docs`; the repo
  is public). Internal operator docs live in [`ops/`](ops/README.md) —
  anything naming a live host, IP, port-forward, key path, or enumerating
  unfixed weaknesses belongs in `ops/`, never `docs/`. Moving a file out of
  `docs/` does not purge git history or caches — a published secret still
  needs rotating.
- `deploy/` scripts are public too: host coordinates go in env vars with
  placeholder defaults, never hardcoded.
- Client API: HTTPS only (self-signed ECDSA P-256 + SHA-256 fingerprint
  pinning, or public-CA/autocert cert), bearer tokens stored hashed.
- The `bridge://pair?…` URL parser in the iOS app is in security scope when
  reachable via a malformed bridge-issued URL.

## CI, release, and deployment

CI workflows (`.github/workflows/`):

- `gofmt.yml` — read-only formatting check on PRs/pushes to main.
- `gate.yml` — `vet` + `test-race` + `build-all` + `go-version` as four
  parallel jobs with a `gate` fan-in; mirrors the local pre-push gate.
- `codeql.yml` — security static analysis (PRs, main, weekly cron).
- `release.yml` — goreleaser on `v*` tag push: signed + notarized macOS
  archives (Developer ID; Windows Authenticode signing pending via SignPath),
  unsigned linux/windows archives, draft GitHub Release. A local dry-run:
  `goreleaser release --snapshot --clean`.
- `docker.yml` — multi-arch image `ghcr.io/acoseac/1-bit-bridge` (tags
  `X.Y.Z` / `X.Y` / `latest`) on the same tag push.

Release hygiene: keep `Dockerfile`'s `ARG GO_VERSION` in step with `go.mod`
(`make check-go-version` guards it); per-release doc steps live in
`docs/release-process.md` (README status bump + cross-repo logging/privacy
audit; user-facing copy changes go to the `1bitapp` repo).

Deployment: two production bridges exist — **home-pc** (Windows) and
**bridge.ars.md** (Linux VPS, public mode). Full runbook:
[`ops/deployment-runbook.md`](ops/deployment-runbook.md) (3-step flow: local
`/tmp/bridge-live/` fixture → home-pc → VPS). Deploy scripts in `deploy/` are
the canonical copies — sync hosts FROM the repo, never edit in place.

## Development workflow

- Work on `feat/<topic>` branches; open a PR against `main`. **Never push
  code directly to `main`** (docs-only changes to agent-doc files are the
  sole exception). One logical theme per PR; avoid micro-PRs — ~5 cohesive
  ~200-line PRs beat 15 tiny ones.
- Pre-push: `make fmt vet test build-all` clean; paste the output into the PR
  body. CI reproduces the same gate.
- Bot reviews (CodeRabbit, Gemini, Qodo/Greptile) post within ~3–6 min —
  expect **at least 2 rounds** (fixes, then a confirmation pass). Batch each
  round's comments into one fix commit. Reject suggestions that contradict
  documented in-code rationale, citing the rationale.
- For jobs spanning 3+ PRs use the stack-and-batch pattern (each branch based
  on the prior tip, PRs opened together, one review wait, merge bottom-up)
  unless the PRs are genuinely independent — then open them in parallel.
  Details in `CLAUDE.md` → "Multi-PR batch workflow".
- Post-merge, deploy to the two reachable bridges (see above) so paired iOS
  clients actually get the change.

## Documentation map

| File | Content |
|---|---|
| `README.md` | User-facing intro, install, Tailscale modes |
| `PROTOCOL.md` | Wire protocol v1 — source of truth |
| `CONTRIBUTING.md` | Branch/PR conventions, mirror-PR rule, pre-push checklist |
| `CLAUDE.md` | Deep agent knowledge: invariants by subsystem, workflows, external-review processes |
| `ops/engineering-log.md` | The record behind each invariant — measurement, rejected alternatives, tests, PRs (not auto-loaded) |
| `SECURITY.md` | Vulnerability reporting, scope |
| `docs/release-process.md` | Per-release doc/publish checklist |
| `docs/docker.md`, `docs/deployment/public-vps.md` | Public operator guides |
| `ops/deployment-runbook.md` | Live-bridge deploy runbook (NOT published) |
| `config/bridge.yaml.example` | Annotated config reference (loopback vs public mode) |
