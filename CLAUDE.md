# 1-bit-bridge

Cross-platform Go companion server for the [1-bit](https://apps.apple.com/us/app/1-bit/id6762529497) iOS music player. Replaces SMB as the transport over high-latency links (Tailscale / DERP relay) — HTTP/2 + bearer-token auth, TLS fingerprint pinning, pre-built library manifest that skips the iOS scanner's two-phase walk. Target platforms: macOS / Linux / Windows × amd64 / arm64. Tested primarily on macOS.

**How this file is organised (since 2026-10-18).** It loads on every request, so it holds only what every session needs: build and test rules, the cross-repo contract with the iOS app, project-wide workflow rules, and where everything else lives. The invariants of particular code live in `.claude/rules/*.md`, which Claude Code loads when a file their `paths:` globs match is **Read** (a Bash `cat` or `grep` does not load them, so Read a file before changing it); procedures live in skills. A long entry is one line: its bold title plus its own bolded "Don't…/Never…" sentences, ending `→ History-X.md`, which is `ops/History-X.md` (for a condensed rules file, `ops/History-rules-<name>.md`), where the entry sits verbatim under a heading naming its section. **Those files are still binding.** When you change an entry, change its line and its long form in the same commit; new long-form detail goes to the History file, never back into this one. The record behind a rule still goes to `ops/engineering-log.md`. To resolve a citation such as "CLAUDE.md, the X bullet" or "under **Scanner**", grep this file, `.claude/rules/*.md` and `ops/History-*.md` for the title, the symbol or the PR number: the entries of "Things that have bitten before" moved to the rules files on 2026-10-18, each under its original section title.

**Procedures are skills (since 2026-10-18).** The local test fixture, production deploys, release day, Gemini consults, LOUPE and the DeepSeek sweep, and multi-PR batches are project skills in `.claude/skills/`: only their descriptions load at startup, and a skill's body loads when it is invoked. See "Procedures live in skills" at the end.

## Where things live

**Where does a new entry go?** A rule for every session: one line here. A rule about particular code: the rules file whose `paths:` globs match that code, adding a glob when none does. A procedure: a skill. Reasoning, measurements and history: the History file its line points to; the dated record: `ops/engineering-log.md`. The budgets under "Build" keep each of these in bounds, and `python3 scripts/rules-for.py <path>` prints the rules files that govern a path.

| Topic | Rules file |
|---|---|
| The scanner: the walk, the extractors and tag parsers, the deletion and reconciliation passes, the watcher, the store writes | `scanner.md` |
| The wire contract: the served set, optional fields, tombstones, events, query encoding, endpoints and mDNS, `PROTOCOL.md`; the wire-type discipline | `wire.md` |
| Lyrics: embedded, sidecar, and the Atlas network tier | `lyrics.md` |
| Enrichment (MusicBrainz, Cover Art Archive, Deezer, iTunes, the Atlas harvest), artwork, fingerprinting, duplicates | `enrichment.md` |
| The job pools (upscale, optimize, analysis), renditions and their staleness, the sidecar, variant and artwork reapers | `jobs.md` |
| Smart mixes: the families and the harmonic engine, the regenerator and its week-seeded windows, the served table and the 7-day exclusion, the regeneration loop | `smart-mixes.md` |
| DLNA, UPnP and SSDP discovery, the SSDP subsystem every device the bridge advertises registers with (B470 PR 5a1), the upstream ingest and proxy, interface eligibility, mDNS | `dlna-upnp.md` |
| Config and settings, managed controls, service install and restart, atomic writes and ownership, the self-updater | `config-lifecycle.md` |
| SQLite predicates and indexes, compaction and the WAL, retention | `sqlite.md` |
| The CLI, `bridge init` and `bridge doctor`, and the serve wiring: gates, loops, locks, shutdown | `cli-serve.md` |
| The box as an audio output: the ALSA reader (`internal/alsa`), the read-only MPD client (`internal/mpd`), `bridge output`, `GET /v1/outputs` and its two gates (MPD's elements, and since B470 PR 5b the bridge's own player's, kind `bridge`), the loopback track route for the MPD beside the bridge, the Player / DAC / Music / Phone checklist (`internal/outputcheck`), and the live reading of the DAC with the bridge's verdict (`GET /v1/outputs/{id}/reading`, `internal/signalpath`); and, for `player.md`'s budget, the sink's cut at a track boundary and its reopen where the binding resolves now (B470 PR 4b2) | `output.md` |
| The bridge's own player (B470): its PCM format and decode layer (`internal/player/format`, `internal/player/decode`), the DSD and ALAC containers, the ALSA PCM backend (`internal/alsapcm`, `scripts/alsa-abi`), the streaming sink, the player's DAC binding (`output.player`, `internal/usbid`, `internal/player/binding`, `internal/player/lan`, `bridge output dac inspect` and `bind`), its hotplug watcher (`internal/player/hotplug`), the player's state owner and queue (`internal/player/engine`), the player's backend (`internal/player/playback`), its output reports (`hotplug.Reporter`) and `bridge output play` (B470 PR 4b2), the library resolver (`internal/player/library`) and the player inside `bridge serve` (`cmd/bridge/serve_player*.go`, B470 PR 4b3), and the player PRs after them | `player.md` |
| The player's OpenHome surface (B470 PR 5a2a on): the opt-in (`output.player.openhome`), its own listener on each LAN address (`internal/player/openhome`, the served addresses from `internal/player/lan`), admission at accept and its limits, the Source's device and service descriptions, the SOAP layer and its table, the read actions (B470 PR 5a3), GENA's refusal, and its start and stop inside serve's player (`cmd/bridge/serve_player_openhome.go`) | `openhome.md` |
| Auth, pairing, sessions and login tickets, TLS and the pin, what logs and error bodies may say | `auth-tls.md` |
| The admin console, the web player, uploads and the trash | `admin-console.md` |
| Test discipline, CI, SonarCloud and Docker (loads with every test file) | `build-ci-tests.md` |

## Build

- `make build` — builds `./bin/bridge` for the host OS.
- `make build-all` — cross-compiles to `dist/bridge-<os>-<arch>{.exe}`.
- `make test` — pure-Go race-enabled suite.
- **Fuzz targets are fuzzed NIGHTLY in CI, and are NOT fuzzed by `make test`.** **`-fuzzminimizetime` is not optional on a target that keeps finding new coverage.** **Count them by file:name pair**, and their properties by the assertions in each `f.Fuzz` body. **A crash found by the extractor targets is a REAL defect, not a nicety.** **The nightly job runs every fuzz process under a 5 GiB address-space limit.** → History-Build.md
- `make fmt vet test build-all` is the pre-push gate, now mirrored by CI (`.github/workflows/gofmt.yml` = the fmt check, `gate.yml` = vet + test + build-all). Run `make check` in the inner loop and `make build-all` once before pushing; on a RAM-constrained box lower `P` (default 4), which caps Go's `-p`. → History-Build.md
- **Run the gate on `go.mod`'s toolchain: `GOTOOLCHAIN=go1.27.3` since 2026-10-24 (B452, B460; 1.27.2 from 2026-10-18).** CI runs that toolchain (`go-version-file: go.mod`), and under it `make fmt` runs that toolchain's gofmt, which is CI's. **On a host whose Go is newer than `go.mod`'s, a bare `make fmt` can rewrite files CI calls clean: never commit such a rewrite.** Restore those files, and check your own with the pinned gofmt, `$(GOTOOLCHAIN=go1.27.3 go env GOROOT)/bin/gofmt -l <files>`. → History-Build.md
- Pure-Go stack: `modernc.org/sqlite` (no cgo), `github.com/mewkiz/flac`, `github.com/dhowden/tag`, `github.com/hashicorp/mdns`, `github.com/mycophonic/saprobe-alac` (Apache-2.0, its packet decoder only, at v1.0.0-rc3; B470 PR 1c). One static binary, no runtime deps.

- **A test that removes or replaces a library fixture after a request that can start a background scan waits for the server's background scans first, and fails if they do not settle** (2026-10-14, backlog B384): wait with `srvBgScansWait(t, srv)`. → History-Build.md
- **Instruction-file budgets are enforced** by `scripts/check-doc-budgets.py`, which `make docs-check`, CI's `docs-check` job and a PostToolUse hook in `.claude/settings.json` run: CLAUDE.md ≤ 64 KB, each rules file ≤ 100 KB, CLAUDE.md plus the rules one file pulls in ≤ 480 KB. Over budget, never cut: move a topic entry from here to the rules file that loads with its code ("Where things live"; `scripts/doc-split/verify_moves.py --base <rev>` proves nothing was lost), or move an entry's long form out with `scripts/doc-split/move_entry.py FILE --largest` / `--line N`, which lands it verbatim in an `ops/History-*.md` file and leaves its one-line pointer. `scripts/doc-split/README.md` has the rest.

## Architecture at a glance

| Package | Role |
|---|---|
| `cmd/bridge` | CLI: **32 subcommands** dispatched in `run()` — `init` / `serve` / `pair` / `scan` / `upscale` / `analyze` / `optimize` / `render` / `variants` / `artwork` / `enrichment` / `duplicates` / `fingerprint` / `doctor` / `update` / `backup` / `restore` / `reset-player` / `token` / `cert` / `status` / `health` / `logs` / `library` / `admin` / `manifest` / `output` / `tsnet` / `start` / `stop` / `restart` / `version`. Bare `bridge` on a real TTY drops into a context-aware launcher menu (`menu.go`); pipes / non-TTY callers fall through to `usage + exit 2` so automation is unchanged. Box / frame / shell-aware handoff helpers in `styles.go`. |
| `internal/config` | YAML loader with defaults + path-relative resolution + `Save()`, which writes only in a turn of the lock beside the file (`LockFile`, `Edit`) that every writer of `bridge.yaml` takes (B506) |
| `internal/tls` | Self-signed ECDSA P-256 cert minter, SHA-256 fingerprint for iOS pinning |
| `internal/auth` | Bearer-token store (hashed, atomic persist, cross-process pickup) |
| `internal/fs` | Path-safe resolver (traversal-rejection, multi-root routing, hot-reload via SetRoots) |
| `internal/manifest` | SQLite library index + tag extractors (FLAC / DSF / MP3 / M4A) + JSON serializer; Scanner roots are hot-reloadable |
| `internal/enrich` | MusicBrainz + Cover Art Archive + Deezer clients, rate-limited (1.1s / 500ms / 120ms) |
| `internal/api` | HTTP/2 handlers: `/v1/{health,list,stat,read,download,manifest,artwork/{mbid},artist-image/{mbid}}` |
| `internal/admin` | Local web console on 127.0.0.1:7789 — library/devices/stats/settings pages + JSON API + bridge:// QR pair URL. Loopback-only, no auth. |
| `internal/packaging` | launchd plist + systemd unit templates + install/uninstall helpers, used by `bridge init` |
| `internal/alsa` | Reads the box's ALSA state from a `/proc` tree: the cards, a USB card's id and stream altsets, what an open playback substream was opened with, and the thread and process holding it (`bridge output dac`); the ALSA configuration under `etc` (`PCMConfig`: the card a device names, its chain, and the first stage on its way that changes samples); and from sysfs (`SysfsReader`, bounded and confined to its root), each card's USB unit and anchored port (B470 PR 4a) |
| `internal/usbid` | What names a USB unit for the player: `Serial`, the password-class guard that prints and encodes as a redaction everywhere but the config file; `ParseHex16`; `Port`, the anchored port path grammar |
| `internal/mpd` | Read-only MPD client and `mpd.conf` reader: the box's player (Volumio, moOde or MPD) and how MPD is set (`bridge output player`). Sends only an allowlist; a password is never printed |
| `internal/outputcheck` | The player checklist (Player / DAC / Music / Phone): four lines, each green or with a one-sentence fix, that `bridge doctor` and the console's dashboard print from one source |
| `internal/signalpath` | The reading's verdict on the format the DAC receives: from what one reading of the box established (the substream and its owner, MPD's status and outputs, the file's own row), `format_preserved`, `format_preserved_dop` or `withheld` with a code per condition that does not hold, `bitExactness` `not_verifiable` for an MPD output, and the settings read that would change the samples (MPD's, `mpd.conf`'s, the ALSA files') as warnings beside it; no I/O |
| `internal/player/format` | Canonical PCM / Native-DSD / DoP representations and ALSA PCM packers for the OpenHome player (B470) |
| `internal/player/decode` | Content-inspected lossless PCM decoders (FLAC / WAV / AIFF), DSF and DFF demuxers into canonical native DSD (B470 PR 1b), ALAC in M4A through a bounded demuxer and saprobe-alac's packet decoder (B470 PR 1c), and typed refusals |
| `internal/alsapcm` | The player's pure-Go ALSA PCM backend (B470 PR 2a): asound.h definitions pinned to a kernel tag and checked by the `alsa-abi` CI job (`scripts/alsa-abi`), an endpoint opened by card and device number, exact rate/channels/format, cancellable non-blocking writes, bounded drain; and its streaming `Sink` (PR 2b): a preallocated two-second lookahead, the position the kernel establishes, pause by PAUSE or zeros with a 30 s release, xruns reported and recovered only when asked, the gapless decision between tracks; and PR 4b2's `Cut` at a track boundary and the reopen at the endpoint `SinkConfig.Locate` names now. Linux amd64/arm64; `bridge output pcm-test` is its hardware harness |
| `internal/player/binding` | Resolves `output.player.device` to the card it names now (serial, uniqueModel or port; ambiguous, missing, transient and the rest as states), the check after open (`Checker`, which `SinkConfig.Admit` takes), and the binding `bridge output dac bind` writes (B470 PR 4a) |
| `internal/player/hotplug` | Watches for DACs arriving and leaving (B470 PR 4a2): the kernel's uevents on a netlink socket (group 1, the kernel alone), debounced, the whole box read with 4a's readers and the binding resolved (`binding.Settled`), published as immutable snapshots under a serve-wide inventory generation; serve runs it on Linux while the live config enables the player; its `Reporter` turns the snapshots into the engine's output reports, under a generation of its own (PR 4b2) |
| `internal/player/lan` | Whether the box has an eligible LAN interface for the player yet: runtime state (`lan_unavailable`), never a config refusal; and (B470 PR 5a2a) each served IPv4 address with its on-link prefix and its interface's index, and every address the box holds (`Look`), which the OpenHome surface binds and admits by; `lantest` provides loopback and in-memory LANs for its tests |
| `internal/player/openhome` | The player's LAN surface (B470 PR 5a2a), behind `output.player.openhome` (off by default), advertised as a strict Source through serve's shared SSDP subsystem since PR 5a2b: a listener of its own on each LAN address, followed as they come and go; admission at accept (a source in the arrival address's on-link prefix, never loopback or the box's own) and its limits; the Source's device description and the bridge's own four SCPDs (Product, Playlist, Info, Time), compared with OpenHome's files; a SOAP layer that checks one table's rows; the read actions, each answered from one snapshot of the player's engine, 501 while none runs (PR 5a3), and the writes 602 until PR 5a4; GENA's 501 |
| `internal/player/engine` | The player's state owner and queue (B470 PR 4b1), hardware-free: one owner goroutine for the transport, the OpenHome Playlist-shaped queue, generations and maintenance, immutable snapshots, IDs and tokens never handed out twice (fsynced reservations), the queue file, and the `Backend` seam whose doc is the contract PR 4b2 implements over the decoders and the sink; since PR 5a3 the occurrence heard (`Snapshot.Track`, with the backend's `Details`) and its counts |
| `internal/player/playback` | The player's backend (B470 PR 4b2): `engine.Backend` over the decoders and `alsapcm.Sink`, PCM only and bit-exact, through the one DAC the binding names: a `Resolver` maps the engine's references to content, `BindingLocator` resolves the binding to its unit now with 4a's check after open, the §1 packing table, gapless joins in one sink and honest gaps across transports, cuts after hand-off, pause, the pause's release and the reopen where the binding resolves now, faults never recovered, and a release confirmed or not; since PR 5a3 each occurrence's `Details` (the decoder's length and format) reported just before it is heard. `bridge output play` (Linux only, unlisted) drives the engine and the backend by hand |
| `internal/player/library` | The player's resolver inside `bridge serve` (B470 PR 4b3): a reference is the DLNA track id of an indexed original, `dlna.TrackID("", path)`, opened through the library index and the filesystem, never HTTP, and checked afresh at every open (the row indexed and not routed, the path in a root configured now, the loopback route's containment check on the open); each refusal typed, its detail naming no path. Serve's player (`cmd/bridge/serve_player.go`) runs the engine, this resolver and the backend from the boot snapshot, on a goroutine of its own, and joins them, bounded, before the watcher and the store close |
| `internal/mdns` | Bonjour `_onebit-bridge._tcp` advertisement |
| `internal/version` | `ServerVersion` + `ProtocolVersion` constants (source of truth for `PROTOCOL.md`) |

## iOS companion (coupled repo)

The iOS app **1-bit** lives at `github.com/acoseac/1-bit` with a local clone at `~/dev/com.acoseac.dsdplayer/`, and `PROTOCOL.md` here and `docs/BridgeProtocol.md` in the iOS repo must stay byte-identical (`CONTRIBUTING.md` → **Mirror-PR rule**). **Copy the file; never rewrite it on one side.** → History-Cross-Repo.md

**When editing this repo, also check the iOS app:**

- **Wire-protocol change** (`/v1/*` response shape, `BridgeTrack` / `BridgeManifest` JSON, `X-Bridge-Protocol` header semantics, error-envelope codes): the iOS DTOs and golden fixture change in the same PR pair; bump `internal/version.ProtocolVersion` and iOS's `BridgeSourceClient.supportedProtocolVersion` together for breaking changes, neither for purely additive fields. → History-Cross-Repo.md
- **Behavioural coupling**: auth flow, rate-limit assumptions, buffer-size tradeoffs, error semantics. A bug on one side often has a sibling on the other — pause before committing and ask whether the iOS decoder/scanner could hit the same class of issue.
- **Bug-fix mirroring examples**: the MB `release-group` decode bug (this repo) had no iOS twin — the iOS side just consumes the server's output. The iOS `resolveLibraryTrackID` path-normalization bug had no server twin — paths are normalized once, on the iOS side, from the server's raw output. Each case was 30 seconds of "does the other side need the same fix?" — worth asking every time.

**Don't regress these cross-cutting invariants:**

- **No server-side transcoding, ever.** 1-bit is bit-exact by mission: `/v1/download` serves the file as-is via `http.ServeContent`, renditions are OFFLINE sidecar files served through `serveVariant`, and a rendition never substitutes for the bit-exact source when the client asked for the source. → History-Cross-Repo.md
- **Rate limits respect the services.** MB anon is 1 req/s (we pace at 1.1s); CAA is IA-infrastructure and polite at 500ms; Deezer is ~50 req/5s (we pace at 120ms). User-Agent identifies the app + GitHub URL per MB's TOS.
- **TLS fingerprint is captured once.** The iOS pin is set during pairing via first-contact; rotating the server cert requires re-pairing. Don't mint a new cert on every `serve` run — `LoadOrGenerate` is sticky by design. Nor on a `bridge init` rewrite: it keeps the pair the config names (`tlsCertPath`, or the data dir's) and the data dir, which `--force` dropped until 2026-09-27 (the bullet on what a rewrite keeps, in `.claude/rules/cli-serve.md`).
- **`enriched_at` monotonicity.** Upsert resets it to 0 on track change and the enricher stamps it on completion; the other sanctioned writers are a CLOSED SET of four (`ResetEnrichedMisses`, `ResetEnrichedByArtistMBIDs`, `ResetEnrichedMissesUnderPrefix`, `ResetEnrichedByPaths`). Never touch it anywhere else: `WHERE enriched_at = 0` drives the worker. → History-Cross-Repo.md
- **Admin console is loopback-only, no auth — IN LOOPBACK MODE.** **Public mode is the separate, credentialed posture** (`internal/admin/middleware_auth.go`). Don't add an auth layer that bypasses the loopback constraint; don't expose admin behind Tailscale / reverse-proxy; for remote admin, SSH-tunnel the port — **local port must equal the admin port**, or the Origin check refuses with a typed `cross_origin` hint (B471). → History-Cross-Repo.md
- **Graceful shutdown triggers full cleanup.** The `POST /api/restart` admin handler MUST NOT call `os.Exit(0)` directly: it invokes the same cancellation closure that handles `SIGINT/SIGTERM`, and a restart exits with `supervision.RestartExitCode` (75) where a stop exits 0. → History-Cross-Repo.md
- **Dual-stack HTTP/2 and HTTP/3 API.** QUIC is enabled by default (`disableHttp3: true` or `BRIDGE_DISABLE_HTTP3=true` turns it off); shutdown uses `.Shutdown(ctx)` with ONE 5s window the LAN and tailnet servers drain under together, and never waits on a handler past it. → History-Cross-Repo.md
- **A request the client abandons is an Info line and status 499, and a server failure stays Error and 500** (2026-10-13, backlog B364): `writeErrorLog` and `writeFileErrorLog` are the one place, and a deadline the server set (`context.DeadlineExceeded`) stays Error and 500. → History-Cross-Repo.md
- **A recorded sidecar path is a claim, never proof the file is gone.** The three reapers ask `integrity.LocateSidecar` and ADOPT a relocated row, and a mass deletion while the tree still holds sidecars is refused. Full rule under **Job pools** in `.claude/rules/jobs.md` (2026-09-20). → History-Cross-Repo.md
- **Single ↔ multi-root storage form flips.** The admin handler calls **`store.WipeFilesystemTracks()`** before the new scan — **never `WipeAllTracks`**, which CASCADE-deletes `upnp_track_routing`; a root add or remove goes through `Scanner.CommitRootChange`. → History-Cross-Repo.md

**Working the bridge**: `feat/<topic>` branches, PR to `main`, pre-push `make fmt vet test build-all`. **Working the iOS side**: same convention at `~/dev/com.acoseac.dsdplayer/`. Never push direct to `main` on either repo; the one exception, the same in both repos (the owner, 2026-10-20), is a correction in an agent-instruction file (**Development workflow**, step 1), and a rule that describes code lands in the PR that changes the code.

## Production deployments

These hold with or without the `production-deploy` skill, which has the fleet, the demo bridge's overrides and the rest of the procedure:

- **Read `ops/deployment-runbook.md` before any deploy, and ask the operator before any production deploy**: the operator decides every one.
- **One SSH ControlMaster per host, never a burst.** The flood protection is on the operator's own network path, so a burst produces a multi-minute timeout that reads as an outage.
- **Never write a real SSH target, IP or key path into a tracked file**: the repo is public. Every `<PLACEHOLDER>` in the runbook resolves in `ops/coordinates.local.md`, which is gitignored, and output pasted from a live host is redacted first.

**`docs/` is a PUBLIC WEBSITE — internal operator docs go in `ops/`.** **Don't move them back, and don't add a new doc naming a live host / IP / port-forward / key path — or enumerating unfixed weaknesses — under `docs/`.** Deploy scripts in `deploy/` are public too: host coordinates go in env vars with placeholder defaults, never hardcoded. → History-Docs.md

## Agent memory — write it to the repo, not (only) to private memory

The user works across **multiple Claude accounts and machines**. Per-account private memory (`~/.claude/projects/<project>/memory/`) does **not** travel between them, so anything durable that lives only there is invisible to the next agent — including a session launched directly from `~/dev/1-bit-bridge` rather than from the iOS repo. **The repo is the shared brain; private memory is the personal notebook.** The same policy is mirrored in the iOS repo's `CLAUDE.md`.

**The repo is the default destination.** Before saving a memory, ask: *would a fresh agent on another account need this?* If yes:

| What | Where |
|---|---|
| Invariants, "don't undo this", rejected-and-why decisions | the rules file whose `paths:` globs match the code, `.claude/rules/<name>.md` ("Where things live" above), or `CLAUDE.md` for a rule every session needs; a long entry is one line there and its long form goes to `ops/History-*.md` |
| The record behind an invariant — the measurement, the rejected alternative, the test, the PR | **`ops/engineering-log.md`** — appended in date order. **Not auto-loaded**, so the *rule* must still land in `CLAUDE.md` or a rules file |
| Wire-protocol / cross-repo contracts | `PROTOCOL.md` (repo root — **not** `docs/`) + the iOS repo's mirror entry, bumped in lockstep |
| Deploy procedures, journal diagnostics, host-specific ops | **`ops/deployment-runbook.md`** — `## Production deployments` above already tells every agent to read it before a deploy, so it reaches sessions that never open `CLAUDE.md` |
| Build/test commands and their gotchas | `## Build` above |
| Procedures (the local fixture, deploys, release day, consults, named reviews, multi-PR batches) | a skill, `.claude/skills/<name>/SKILL.md`, whose `description` says when to use it ("Procedures live in skills" below) |
| A follow-up found and not built (a defect beside this change, a finding agreed to be out of scope, a question to measure later) | **`~/Desktop/to-do/bridge-backlog.md`**, never a task chip: see `## Follow-ups go in the backlog, not in task chips` below |

**`ops/`, never `docs/`, for anything operational** — see the warning in `## Production deployments`: `docs/` is served as a public website from a public repo, so live hosts, key paths and unfixed-weakness enumerations must not go there. This makes the repo-vs-private line *three*-way here: public `docs/` → internal `ops/` → private memory.

**Stays in private memory — and only this:** secrets-adjacent coordinates (SSH key paths, private hosts — these can't go in `ops/` either if the repo is public); the user's personal working preferences; and perishable state ("deployed vX on `<date>`, rollback at …") that would rot in git — anything *actionable* belongs in `~/Desktop/to-do/` instead.

**Four rules that make this hold:**
1. **Don't commit the memory *files*** into a `.claude/memory/` folder — the harness doesn't auto-load them from there, so they'd be inert paper. Fold the **content** into files that do auto-load.
2. **Never cite a private memory from a repo file** — it's a dangling reference for every other account. If a repo file needs the fact, put the **fact** in the repo.
3. **Repo first, then the pointer.** When something is both shareable and worth a personal reminder, write the repo entry first and leave the private memory as a one-line pointer.
4. **Only `CLAUDE.md` and the rules files are auto-loaded** (a rules file when a file its `paths:` globs match is Read). `ops/engineering-log.md` exists so a rule can stay short without losing its evidence — but a finding written *only* there is the same inert paper as rule 1. Every batch writes the **rule** here or in the rules file that loads with its code, and the **record** there.

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

**When you fix something here, put the rule in this file or the rules file that
loads with its code, and the record in the log** — never only in the log, because
nothing there reaches a session that has not gone looking for it.

**Twenty-three claims in this list have been wrong and been corrected**: **Check the code before believing any doc about it, including this one** — and when you find a stale claim, correct it here rather than working around it. → History-Workflow.md

**The same rot reaches CODE COMMENTS, and there it is more dangerous**, because a comment is read as the reason for the code beside it: a false comment that explains a design choice is how the next change gets made on the same reasoning. → History-Workflow.md

**The entries themselves moved on 2026-10-18** into the rules files listed under "Where things live", each under its original `###` title (now a `##`), so that they load with the code they bind. The dated review records stay below, one line each.

### Review and fix batches — one line each

- <a name="review-2026-09-22-fixes"></a>**2026-09-22 — review of the #959–#966 fix window** — **Every defect was a behaviour the existing tests still accept**, and three were introduced by the fixes they sit in: a fix placed in FRONT of a check can destroy that check's input, a range gate does not protect a value that can wrap into the range, an escape hatch re-opens the failure its own PR fixes; disjointness decays across review rounds → History-Batches.md
- <a name="review-2026-09-22"></a>**2026-09-22 — full review of the post-v0.2.0 window** — the enumeration failure, again, on the destructive site: **Grep for the PATTERN, not the symptom**; a fix's own blast radius is a thing to check; three tests asserted the defect; a docblock that matches its code exactly can still be the bug; merge order from file overlap, not instinct → History-Batches.md
- <a name="review-2026-09-18"></a>**2026-09-18 — findings review on the post-#899 window** — gate on the PARSED value and test with the client's real request; a sibling loop that missed the guard; one consumer that never went live when its writers did; verify a proposed fix's MECHANISM before taking it; when you correct a claim, grep for its twins in every file that restates the rule → History-Batches.md
- <a name="review-2026-09-10"></a>**2026-09-10 — full-codebase review** — **The worst defect was not in the new code**: `internal/trash` had never worked on a multi-root bridge; the mechanical sweeps came back CLEAN; the defects were in code nobody had reviewed and at the seam between two subsystems; the same enumeration failure, three more times; take the observation, verify the mechanism, write your own fix → History-Batches.md
- <a name="loupe-2026-09-09"></a>**2026-09-09 — LOUPE on the 2026-09-04..09 window** — **When a fix enumerates the sites it covers, the enumeration is the defect**; **A fixture that omits a live gate describes a different bridge than production**; `ExtractorVersion` was not bumped for the lyrics batch; a managed CONTROL must own the settings FIELD that performs it; a test seam is per-SERVER, never a package var; commit before the control, always → History-Batches.md

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

CI now runs the gate on every PR (`.github/workflows/gofmt.yml` = the fmt check, `gate.yml` = vet + test + build-all). Still run `make fmt vet test build-all` (or `make check` in the inner loop) locally first and paste the clean output into the PR body; the CI workflows mirror the local gate command-for-command, so keep them in lockstep. → History-Build.md

Releases *are* wired up: `.github/workflows/release.yml` runs goreleaser on tag push, and `.github/workflows/docker.yml` publishes the multi-arch image `ghcr.io/acoseac/1-bit-bridge` on the same push. Keep `Dockerfile`'s `ARG GO_VERSION` in step with go.mod's `go` directive or the image build fails. → History-Build.md · doc: release-process.md

## Documentation refresh on each release

**User-facing docs moved to 1-bit.app (2026-06-09).** They live in the **`acoseac/1bitapp`** repo, published at **`1-bit.app/bridge/*`**, and this repo's `docs/*.html` are **redirect stubs** to those URLs — don't edit them. → History-Docs.md · doc: release-process.md, docker.md

**Public-facing prose names the project entity as `ars.md`, not `acoseac`** (set 2026-06-02, PR #344): `acoseac` stays verbatim only in genuine identifiers (GitHub URLs, the `acoseac.github.io` Pages domain, badge URLs, `com.acoseac.*`). Don't sweep-rename `acoseac` blindly — distinguish prose from identifiers. → History-Docs.md

## Bot-review discipline (PR-time)

The same "verify before acting" rule the DeepSeek triage runs on applies to the PR bots — **their severity label is a claim, not a finding**. Recurring false-positive shapes, all observed on real PRs here:

- **`windows.Errno` vs `syscall.Errno`.** `x/sys/windows/aliases.go` declares `type Errno = syscall.Errno` (an **alias**), so `errors.Is(err, windows.WSAEADDRINUSE)` matches, and stdlib `syscall` on Windows has no `WSAEADDRINUSE`: the suggested fix **would not compile**. Read `isAddrInUse`'s comment before re-raising. → History-Workflow.md
- **"URL parser differential" via a backslash in the authority.** Go's `net/url` REFUSES `\` in a host (`invalid character "\\" in host name`), so guards proposed "after `url.Parse`" against a backslash host are unreachable dead code. A regression TEST asserting refusal is still worth taking (it survives a future Go relaxing the parse); the guard is not.
- **A "compilation failure" claim that is really a test failure.** `os.Geteuid` IS defined on Windows (returns -1); `GOOS=windows go test -c -o /dev/null <pkg>` settles it in seconds, and the PR's own `test (windows-latest)` leg is the evidence to cite. → History-Workflow.md

Take the accurate half of a wrong finding when there is one — the backslash and Windows-fixture cases both yielded a useful test even though the proposed code change was rejected. And **reply on the thread with the evidence when declining**, so the same claim doesn't cost a fresh investigation next quarter.

**Merging with review comments outstanding is a process failure, not a shortcut.** The documented loop — *"don't merge after round 1 — expect 2 rounds minimum"* — is what catches this, and a same-day multi-PR batch is exactly when it gets skipped. → History-Workflow.md

**And check the bot actually reviewed before counting the round.** A rate-limited CodeRabbit hides its notice in an HTML comment inside the walkthrough, so a PR with no new comments after a fix round looks identical to a clean pass — full rule, the grep that detects it, and the two commands that ask for the pass, under **Build, CI, and test discipline** in `.claude/rules/build-ci-tests.md`.

- **Stacked PRs get NO gate CI here** (the workflows are `pull_request:
  branches: [main]`), and retargeting alone doesn't fire them — amend for a
  fresh SHA after retargeting. Capture each child's fork point BEFORE amending
  its parent. `git add -A` with another branch's untracked files on disk sweeps
  them into your commit; use explicit paths.
- **A rate-limited bot's silence is not approval — and CodeRabbit's notice is both invisible and TRANSIENT.** **`@coderabbitai review` does NOT clear a plan-limit pause**: tick the "Run this review for free" checkbox. **And the verdict is in the WALKTHROUGH, not in `pulls/N/reviews`**: **Compare `coveredCommitId` to the PR head, never `headCommitId`**. **Push nothing while a ticked review runs.** → History-Workflow.md
- **Read a PR's reviews and comments with `gh api --paginate`: it returns 30 a page, and a long PR's newest round is on the page it drops.** → History-Workflow.md
- **A fix round needs a FRESH pass from every bot, not just the one that
  found something.** Gemini does not re-review each push: after four rounds
  on one PR its last review still predated every fix commit on five of six
  branches, and `/gemini review` on each head then produced two more real
  findings. Ask both, per head.
- **Merging with review comments outstanding is a process failure**, not a shortcut — expect two rounds minimum, verify a bot's severity label before acting, and reply on the thread with the evidence when declining. → History-Workflow.md

## Development workflow

Standard single-PR loop for any non-trivial change:

1. **Branch off main.** `git checkout -b feat/<topic>`. Never push directly to main. The sole exception (the owner, 2026-10-20; the iOS repo's CLAUDE.md says the same) is a correction, meaning a stale fact or count or a broken pointer, in an agent-instruction file (`CLAUDE.md`, `AGENTS.md`, `.claude/rules/*.md`, a skill, an `ops/History-*.md` file), with `make docs-check` clean. A new or changed rule, a move between files, `PROTOCOL.md` (the iOS app mirrors it), the other docs and runbooks, and anything touching code go through a PR. A rule that describes code lands in the PR that changes the code, with its log record, so a review reads the two together.
2. **Pre-push gate.** `make fmt vet test build-all` — clean before pushing. Paste the output into the PR body.
3. **Open PR to main.** One PR per logical theme. Push the branch and open the PR; don't wait to batch unrelated changes into it.
4. **Wait ~6 min for bot reviews.** CodeRabbit, Gemini, and Qodo/Greptile each post within ~3 min; 6 min covers the slow tail. Don't poll.
5. **Address all comments in one fix commit per round.** Don't merge after round 1 — bots run a second pass on the fixes. Batch all round-N comments into a single commit.
6. **Reject suggestions that contradict deliberate rationale.** When a bot flags something that was a conscious design decision, cite the rationale in a reply and move on. Don't blindly apply suggestions that would undo a documented invariant (e.g. the `apiScan → spawnBackgroundScan` WG contract, the `pairing.Poll` read-many contract).
7. **Expect 2 rounds minimum.** One round of fixes, one confirmation pass. High-surface PRs (new endpoints, config changes, concurrency) routinely take 3–4 rounds.
8. **Merge once reviews are quiet** and `make fmt vet test build-all` is clean.
9. **Post-merge deploy.** Once main carries the fix, deploy it to the two reachable bridges so the change is actually live — see [Post-merge deployment](#post-merge-deployment) below. Skipping this step leaves the codebase ahead of every actual bridge a paired iOS client can reach.

For jobs spanning 3+ PRs, use the stacking pattern in the `multi-pr-batch` skill instead.

## Post-merge deployment

**See `ops/deployment-runbook.md`** — 3-step flow (local `/tmp/bridge-live/` fixture → home-pc Windows → the operator bridge, on the NUC since 2026-09-22 and on the `bridge.ars.md` VPS before that). Read it before deploying.

## Procedures live in skills

A procedure, meaning steps taken when a trigger comes up, is a project skill in `.claude/skills/<name>/SKILL.md`. Claude Code loads only each skill's name and description at startup, so the description is the trigger; the body loads when the user types `/<name>` or when the description matches. Use the skill rather than improvising the procedure, and put a new procedure in a new skill, not here.

| Skill | Use it when |
|---|---|
| `local-test-fixture` | running a bridge from a folder of test audio, or touching a bridge database from outside the bridge |
| `production-deploy` | before any deploy to the operator bridge, home-pc or the demo, or any SSH to the fleet; the rules under "Production deployments" hold without it |
| `release-playbook` | release day: the scope, the sanity reviews, the candidate, the tag, the rollout, and the per-release docs |
| `gemini-consult` | a decision is non-obvious and neither the code, a test nor this file settles it: consult rather than guess |
| `review-procedures` | the user asks for LOUPE or the DeepSeek sweep by name |
| `multi-pr-batch` | a job spans three or more PRs |
