# v0.2.0 — release notes draft (operator pastes into the drafted GitHub Release)

> Working copy for the operator. The release workflow drafts a Release with
> goreleaser's raw changelog; replace that body with the text below the rule,
> after checking that `git log --oneline v0.1.9..v0.2.0 | grep -cE '\(#[0-9]+\)$'` on the
> tagged tip says **161** (the figure README.md:22 carries: 156 merged PRs in the window at
> the time of writing, plus #904, #905, #906, #907 and the release-prep PR). Everything here
> was derived from `git log v0.1.9..main` and the dated CLAUDE.md /
> `ops/engineering-log.md` sections; PR numbers are kept so a reader can
> follow any line back to its diff. Grouped by who notices it.

---

**The first release under FSL-1.1-MIT** (#730). v0.1.9 and every earlier release stay MIT; each FSL release converts to MIT two years after it ships. Self-hosting is free and the source is public as before — the license blocks another app shipping the bridge as its companion server, or someone selling it as a hosted service, ahead of the planned first-party cloud offering.

Wire protocol stays at v1 — everything here is additive. A paired iOS app on any v1 bridge keeps working; new capabilities light up through `/v1/health` `features[]`.

## For listeners

**The admin console is now a library player.** Albums, artists (round portraits, an A–Z rail), genres and composers, playlists and smart mixes as real player surfaces, favorites as a queue, a now-playing bar that survives navigation to the operator pages, and a download indicator when the browser is fetching a hi-res file rather than streaming it (#739, #740–#747, #754–#758, #763, #901). One sidebar replaces two levels of navigation (#748); breadcrumbs follow the route you took (#785–#787); a grey/yellow/black/white theme with chromeless transport controls (#765, #766); mobile top bar and section strip fixed (#808, #874).

**Add music from the browser.** Chunked, resumable uploads verified by content hash, a settings toggle and a sidebar space meter; unacceptable files are skipped rather than failing the session; a read-only library is named rather than answered with a 500 (#788–#791, #794–#796, #793). **Delete-as-trash** — a separate switch from uploads — moves files into a dated trash folder inside the library root instead of unlinking them (#792).

**DSD → PCM renditions.** For a wired DAC that cannot take a DSD rate, the bridge can render a DSD source offline into a faithful `pcm-` tier (176.4 or 192 kHz, 24-bit) and a compact `optimized-dsd-` tier, through a clip-guarded two-stage ffmpeg + sox chain, and serve them as variants beside the untouched original (#863, #864, #865). Measured, not assumed: the decimation stopband is −116.8 dB, the alias differential +0.00 dB (faithful) / +0.38 dB (compact), and the rendition lands at −0.97 dBTP against its −1.0 target — and those measurements now run in CI on every push (#866, #884). `bridge render` drives it from the CLI; the web player treats a DSD track whose only rendition is the faithful tier as playable (#885).

**Lyrics.** The scanner extracts, stores and serves **one lyrics document per track** — `GET /v1/lyrics` — from ID3 SYLT (rendered to LRC) and USLT, Vorbis `SYNCEDLYRICS` / `UNSYNCEDLYRICS` / `LYRICS`, MP4 `©lyr`, and `.lrc` / `.ttml` / `.txt` sidecars, in the precedence order the app already uses (#840, #849, #850, #851). An opt-in **network tier** (`atlas.lyricsEnabled`) fills the gaps from a self-hosted Atlas relaying LRCLIB, with a Jobs card showing what it is doing (#887, #890, #891, #893, #899, #900). A transcript with a single `[4:20]` cue marker is plain text, not a one-line synced document — the same rule the app applies when it parses (#904).

**Covers for DLNA renderers.** `/dlna/artwork/{key}` serves the same bytes as `/v1/artwork/{key}` on the LAN-only DLNA listener, and the bridge's ContentDirectory finally sets `albumArtURI` on items and album folders. Advertised as `dlnaArtwork`; the iOS app gates its own renderer-facing cover URLs on it (#906).

**Formats.** SACD ISO images expand into virtual DST track rows (#779); DST-compressed DSDIFF is typed correctly and all-DFF duration/channels are read (#767); ALAC can be upscaled through an ffmpeg pipe (#838).

**Search.** `GET /v1/search` puts the FTS5 index on the wire (#826, #830, #833). Artist portraits are content-keyed and cache immutable (#799).

**Multiple sources.** The Library page gains a source facet with live upstream status, the section rail keeps the scope, mixed albums are shown as such, and a UPnP upstream's description URL is advertised so a phone whose own discovery failed can still add it (#807, #809–#815, #824).

## Fixes a listener could notice

- Delete targeted the wrong library root on every multi-root bridge — and where two roots' directory names overlapped, trashed a real file and reaped the manifest row of the one still on disk (#892).
- A bad release fetch stopped the whole network lyrics tier; a network lyrics document was reaped by the next scan; an unread upstream status was written as a durable verdict (#887, #893).
- Ten optional timestamps shipped as `0001-01-01T00:00:00Z` instead of being omitted (#896).
- `/v1/health` withholds `scanState` and the update fields from unauthenticated callers, with the scope set by which fields the shipped app declares optional — so nothing a client requires is ever missing (#801; the spec's wording on what absence means corrected in #804).
- A bridge behind a port-remapping proxy advertised an endpoint no client could reach (#871); a wedged scan was advertised to clients as in progress (#784).
- Routed (UPnP-upstream) tracks: untagged ones get a searchable artist and album; the variant panel says "on an upstream" instead of "format unreadable" (#813, #814).
- An interrupted upload resumes instead of starting over (#795); a 507 reported zero reclaimable bytes (#898).
- In-flight streams are drained before an admin-initiated restart (#780).
- SYLT lyrics written by CR-only taggers (older Mac tools) rendered with their lines merged — a bare `\r` now counts as a line marker, as it has on the phone since app #1564 (#907).

## For operators

**Console login links.** `bridge admin login-link` mints a one-time, 60-second link that opens the console signed in — and it now survives a phone-to-laptop hand-off and the minting process exiting, because the ticket is persisted rather than held in memory; the endpoint is hardened as the one anonymous caller on the box (#868, #872, #880, #886). The link now lands on a one-button page and is spent by the Continue click, not by opening it, so a link preview — a share sheet, Messages, a mail client — can no longer use it up before you do.

**Hosted-tenant posture.** `deployment.managedSettings` hides and refuses settings fields the tenant does not own; `deployment.managedControls` does the same for actions — restart, updates, roots, the variants directory, backups — and a managed control owns the setting that performs it (#782, #876, #877, #879). `bridge doctor` skips advice that needs a shell on a host the tenant does not have (#876).

**Settings hot-apply.** Cadences (scan, backup, update), the feature gates (smart mixes, CarPlay optimize, fingerprinting), the enrichment endpoints and pacing, upscale and analysis all take effect on save; the PATCH response says per field what happened, and the apply-semantics matrix is self-verifying (#768–#774, #781). Two values that were frozen at boot while the console called them live are now live (#857); two feature gates the always-construct conversion removed are restored (#852).

**Export.** Everything a person made — playlists, favorites, history, device registrations — in one file, with no credentials in it; and the file tells the truth about its own truncation (#875, #881).

**Database.** Compaction with honest before/after footprints, a retention window for playback history with a ceiling, and a Diagnostics panel showing how big the two unbounded tables actually are (#819, #822, #829, #859, #860, #861, #862).

**Public / hosted deployments.** Per-route rate-limit classes; every mutating route is bounded (#821). Persisted admin sessions, `/healthz` and `/readyz` (#800). Env overrides derived from the config struct; the admin credential can be seeded from the environment (#802). JSON logs off-terminal, HSTS in public mode, scrapeable metrics (#803). A build that is a descendant of a tag is no longer offered its own tag as an update (#797).

**CLI.** Seven ways the CLI surprised or lied — including `bridge token revoke` dying on a host without `--config` — fixed; positional arguments that silently widened a scope to the whole library now refuse (#853, #855, #856, #882). The artwork GC and the UPnP live-host lookup fail closed (#854); every reaper fails closed on an empty referenced set (#895); the store is joined at shutdown (#858).

**DLNA / UPnP.** GENA callbacks that differ from the SUBSCRIBE source are observed (#818); `upnpUpstream.servers[].manualDescriptionURL` is implemented rather than refused (#824); an unknown upstream baseline no longer expires into a reap (#898).

## Under the hood

- Three review sweeps in one release: a LOUPE on `cmd/bridge` (#852–#858), a LOUPE on the 2026-09-04..09 window (#878–#886) and a full-codebase review (#892–#899). Their rules are in `CLAUDE.md`; the measurements behind them in `ops/engineering-log.md`, split out of the always-loaded file (#836, #837).
- **39 fuzz targets across ten packages**, fanned out nightly in CI (#823, #805, #851, #904); the SACD reader and the upload path validation joined the covered surfaces.
- The Windows and macOS test legs are promoted into the merge gate (#817); `internal/adminauth`'s race-mode time cut from ~300 s to 40 s (#869); the gate's real cost measured — SQLite under the race detector (#873).
- Dependencies: tailscale.com 1.102.3 (#775), golang.org/x/crypto 0.55.0 (#733), golang.org/x/mod 0.40.0 (#732), golang.org/x/image 0.45.0 (#820), golang.org/x/sync 0.23.0 (#903), prometheus/client_model 0.6.3 (#845). GitHub Actions: docker/setup-buildx-action and setup-qemu-action 4.3.0 (#731, #841), codeql-action 4.37.8 → 4.37.9 (#816, #905).

## Upgrading

- **One-time full re-extraction.** `ExtractorVersion` moved 7 → 10 across this release (lyrics correctness #878; the sparse-timed rule #904; SYLT CR markers #907), so the first scan after upgrading re-reads every audio file's tags once. On a NAS- or cloud-mounted library that is a full read of the library; budget for it. The client delta stays bounded to rows whose metadata actually changed.
- **New config keys** — `upscale.dsdRender`, `upscale.tempDir` (#863, at `v0.1.9-150`), `atlas.lyricsEnabled` (#887) and `deployment.managedControls` (#876). The loader is strict, so a config that names any of them will not start an OLDER binary: roll the config back together with the binary (`ops/deployment-runbook.md`).
- **`upscale.tempDir`** — set it explicitly on a systemd host with `PrivateTmp=yes`, on local disk, never on a network mount; a faithful 176.4 kHz render of an hour-long track stages ~5 GB there.
- **New optional dependency.** DSD renditions need an `ffmpeg` with the `dsd_*` decoders beside `sox`; `bridge doctor`'s `dsd-render-toolchain` check reports which half is missing. Without them the feature is written but inert and `/v1/health` stops advertising `dsdRender`.
- Windows binaries remain unsigned (Authenticode via SignPath Foundation pending); macOS binaries are Developer-ID-signed and notarized.
