# LOUPE — the 2026-09-04..09 window

**Scope shape: WINDOW.** Base `4721a51` (2026-09-03, PR #840) → `de6d13e`
(2026-09-09). 200 files, +22,761 / −1,114. Production Go: ~5,200 added lines
across 13 packages; test Go: ~10,800.

## The batches in the window

| PRs | What | Prior sweep? |
|---|---|---|
| #849 #850 #851 | lyrics surface hardening | **yes** — a LOUPE run's own output |
| #852 … #858 | `cmd/bridge` hardening | **yes** — a LOUPE run's own output |
| #859 … #862 | database compaction + retention | **yes** — a LOUPE run's own output |
| #863 #864 #865 #866 | **DSD → PCM renditions** (~1,600 prod lines) | no |
| #868 #872 | **one-time console login tickets** | no |
| #869 #873 | CI race-time reductions | no |
| #871 | don't advertise a proxy-remapped listen port | no |
| #874 | console mobile top bar | no |
| #875 | **export everything a person made** | no |
| #876 | **managed controls** | no |
| #877 | settings Save payload + parity guard | no |

The three hardened batches were expected to refute most findings, and did. Every
confirmed finding below is in the unswept half.

## Structural check (the bridge's substitute for `.claude/rules/` globs)

Every surface in the window has a `### ` section under `## Things that have
bitten before` **except the export handler** (#875): `handlers_export.go` is 286
new lines with zero CLAUDE.md mentions and — see M3 — no console affordance and
no doc anywhere. That absence is itself a finding.

Citation sweep of CLAUDE.md, clean except one item:
- 119 identifiers cited by the four sections this window touches: all resolve.
  (`StateDelivered` and `elapsedSec` are absent **on purpose** — both are
  prohibitions.)
- 14 cited `Test*`/`Fuzz*` names: all exist.
- All cited in-repo paths exist. `PROTOCOL.md` is byte-identical with the iOS
  mirror.
- **D1 — the fuzz-target package list is short one.** CLAUDE.md says 37 targets
  "across `internal/{manifest,fs,dlna,dlna/discovery,upnp,enrich,dupes,lyrics}`".
  37 is right; the list omits **`internal/upload`** (`FuzzValidateRelPath`,
  `FuzzAcceptedExt`). The sentence continues "covering the three untrusted-input
  surfaces" and enumerates them — so the web-upload path validation, a genuinely
  untrusted surface on a public-mode bridge, is missing from both the package
  list and the enumeration.

  *(A trap worth recording: `grep '^func Fuzz' | sort -u` counts **36**, because
  `FuzzNormalize` exists in both `internal/dupes` and `internal/lyrics`. Counting
  targets requires the file:name pair, not the name.)*


## Review directions

1. **PRISM**, 13 batches (19,323 lines) through `~/dev/gemini-review-bridge`'s
   Go primer, `gemini-3.8-flash`, `--workers 3`.
2. **Five targeted read-only agents**, one per surface, each with CLAUDE.md in
   hand.
3. **One dedicated quick-wins consult** (`consult.py`), which the user asked for
   explicitly.

Three sources converged independently on the export defects, which is the
strongest signal in the batch.

### PRISM batching artifact worth recording

`w5-08` reported a **HIGH**: "DSD tracks projected as optimize-eligible when
`dsdRender` is disabled" (`handlers_library_browse.go:727`). **REFUTED** —
`transcode.DSDRenderEligible`'s first line is `if !caps.Active() { return false }`
and `Active()` is `Enabled && DecodeDSD`. The reviewer could not see it because
`dsd_render.go` was in a **different batch**. This is exactly the failure the
LOUPE doc warns about ("slab Go by package, not by file"): the batch carried the
caller and not the predicate. `w5-06` (auth) came back
`[NO CANDIDATES] blockReason=OTHER` — Gemini declined the security batch
outright; that surface's coverage is the targeted agent's alone.

---

# Findings

Severity is **mine**, assigned after verifying against source. Every item below
was confirmed by reading the code; measurements are stated where taken.

## P0a — HIGH — `POST /v1/upscale/batch` never received the gate #852 restored on its two siblings

**Files** `internal/api/upscale_batch.go:201` · `upscale.go:191` ·
`upscale_delete.go:187`

PR #781's "always construct, never stop" made the adapters unconditional, so
`== nil` stopped being a gate. **#852, in this window**, enumerated the affected
mutation handlers and added `|| !s.upscaleActive()` to `POST /v1/upscale` and
`DELETE /v1/upscale/variants`. `POST /v1/upscale/batch` — the third handler of
the same class, and the one with the **largest** scope — was not enumerated:

```go
upscale.go:191         if s.upscaleEnqueuer == nil || !s.upscaleActive() {
upscale_delete.go:187  if s.variantDeleter   == nil || !s.upscaleActive() {
upscale_batch.go:201   if s.batchCoordinator == nil {                        // ← no live gate
```

#852's own test file says it: *"**Both** mutation handlers used to gate on their
adapter being nil"* (`upscale_gate_test.go:60`) — there are three.

**Consequence** Any bearer-token holder can `POST /v1/upscale/batch
{"path":"","kind":"upscale"}` on a bridge with `upscale.enabled: false` (the
default). The coordinator walks the whole library and enqueues into a pool that
is also constructed unconditionally with live workers — on a bridge whose
`/v1/health` reports `upscaleEnabled: false`. Every completed job's
`UpsertVariant` strict-advances `indexed_at`, so it is also a whole-library delta
to every paired device.

The suite proves the hole rather than catching it:
`upscale_batch_test.go:87` builds its fixture without `WithUpscale`, so
`upscaleActive()` is false, and `TestUpscaleBatchRejectsInvalidTargetWith400`
asserts **202 Accepted** on it — and passes.

**Second half, MEDIUM, same file** The `case "optimize"` arm
(`upscale_batch.go:262`) has no `carPlayOptimizeEnabled` check where its twin
(`upscale.go:246`) does — so with `upscale.enabled: true` and
`optimize.enabled: false`, the per-path route answers 503 and the batch route
answers 202. The `pcm` arm added directly beside it **in this window** does carry
its gate, which is what makes the omission conspicuous.

**Fix** `|| !s.upscaleActive()` on submit and cancel; move the optimize-kind
check into its arm, mirroring `upscale.go:246`; extend `upscale_gate_test.go`'s
existing `gateFixture` to the batch route rather than adding a parallel fixture.

## P0b — HIGH — the lyrics batch is inert on every existing library: `ExtractorVersion` was not bumped

**Files** `internal/manifest/extractors.go:247` · gate at `scanner.go:1241`

`const ExtractorVersion = 7`, last moved by **`4721a51` — the base commit of this
window**. Three PRs since changed what extraction *produces*:

- `lyrics/sylt.go` — `lrcTime` now clamps to `999:59.999`, so a SYLT entry past
  999 minutes renders a line the phone can parse instead of `[1000:00.000]`.
- `lyrics/lyrics.go` — `mergeDuplicate` now keeps the **larger** priority, which
  is the whole fix that stops an "Amazon" / "Song ID" descriptor laundering back
  to rank 0. Different winner ⇒ different `lyricsTag`.
- `lessCandidate` is now a strict total order, so a previously-tied pair resolves
  differently.
- `manifest/lyrics_extract.go` — `syltCandidate` rejects oversized frames.

`scanner.go:1241` skips re-extraction when
`existing.ExtractorVersion >= ExtractorVersion` **and** the audio file's size
and mtime are unchanged. Both hold for every row a v7 scan already stamped and
nobody has touched since — which, on a library scanned any time after #840
shipped, is essentially all of them. For those rows the junk-descriptor lyric
and the unparseable timestamp persist **forever**. (A row still stamped below 7,
or one whose audio changed, re-extracts anyway and was never affected.) Silent: no error, no log
line, and every fix's own test passes because it calls the extractor directly.
There is no lyrics backfill trigger anywhere in `cmd/bridge` or `internal/admin`.

CLAUDE.md carries the rule verbatim: *"**Bump `manifest.ExtractorVersion` on EVERY
extraction-logic change**."*

**Fix** `const ExtractorVersion = 8`. The delivery machinery is already correct
and was built for exactly this: `StampExtractorVersionBatch` calls
`writeLyricsRowTx` on the version-stamp leg, and #849's fix made that bump
`indexed_at` **only when the tag actually moved** — so the client delta stays
bounded to rows whose lyrics really changed. The bump is the only missing piece.

*(#850's two fixes do reach production without it, because they change the gate
itself and those rows were already re-extracting every scan.)*

## P0c — MEDIUM — the batch scratch pre-flight ignores the worker-lane multiplier

**File** `internal/transcode/batch.go:923-929` (docblock), `:1043-1047` (check)

`submitRenditionProjections` grades the scratch volume against
`picked.maxRenderScratch` — the largest **single** Stage A intermediate — while
the pool runs `EffectiveWorkers()` = `min(NumCPU-1, 4)` jobs concurrently on
distinct dedup keys, so up to `workers` scratches are held at once. The docblock
asserts the opposite: *"The LARGEST, not the sum: jobs run one per worker lane, so
the sum … is never held at once."* One job per lane × N lanes is N at once.

The auto-optimize sweeper answers the same question correctly and says so:
`spec.RenderScratchBytes() * int64(sw.laneCount())` (`auto_optimize.go:346-353`).
`TestBuildPCMRenderCandidates_ScratchIsTheLargestSingleJob` pins the
un-multiplied form, so nothing catches drift in either direction.

Not live on the 2-vCPU VPS (`min(NumCPU-1,4)` = 1 lane); live on home-pc and any
4-core host, where the failure is a temp-volume ENOSPC rather than one failed job.

**Related, same budget** `RenderScratchBytes` falls back to `ch = 2`, and
**`Track.Channels` is written by the DFF extractor only** — the DSF extractor
parses rate, bits, sample count and duration but never `channelNum`, which is
already in the bytes it reads. For every DSF the two fallbacks stop cancelling:
duration is known and real, channels are assumed stereo, so a 5.1 DSF
under-reserves by 3× (measured shape: budget 5.08 GB against 15.24 GB actual for
one hour). The `add` docblock claims a projection "carries neither duration nor
channels" and that multichannel "over-estimates, which is the conservative
direction" — it carries both (three lines below the comment) and it
under-estimates. Budget-only: the render itself takes its channel count from
ffprobe.

## P0d — MEDIUM — the clip guard is one-sided, and two docblocks say it is not

**File** `internal/transcode/dsd_render_chain.go:40-46, 150-157`

`ClipGuardedGainDB` clamps to `[0, 6]`, so it can never attenuate. Final peak is
`TP_unity + G`: at or below −1 dBTP for `TP_unity ≤ −1`, but **equal to
`TP_unity`** above it. For an over-modulated master (`TP_unity > 0`) Stage C's
`gain 6.0206` restores unity, sox clips, `soxReportedClipping` fires, and the job
returns `ErrDSDClipped` — no sidecar, ever, until the failure threshold suppresses
the source.

Measured with the shipping Stage C argv on a scratch whose unity peak is 1.2:

```text
sox WARN gain: gain clipped 262400 samples; decrease volume?
sox WARN dither: dither clipped 229546 samples; decrease volume?
exit=0
```

The chain docblock says the peak "lands at or below −1 dBTP **by construction**"
and the helper's says the arithmetic makes it so. Both are false above −1.
`TestRunDSD_RealToolchain`'s hottest fixture is −6 dBFS, so the `G == 0` arm is
never exercised.

Fail-closed, so no wrong audio ships — but a legitimately hot master can never
get a rendition and the error reads as a bridge bug. **This is a product
decision**: letting the clamp go negative would attenuate to −1 dBTP like every
other source (changing what a rendition sounds like), while keeping it means
correcting both docblocks to say the guard is one-sided and a `> 0 dBTP` source
is a deliberate refusal. **Escalated, not taken.**

## P0e — MEDIUM — two `pcm-` family arms were missed

**Files** `internal/admin/player_audio.go:134-148` ·
`internal/manifest/catalog_refs.go:388-397`

The DSD PRs added `pcm-` arms to eight sites but not to `pickPlayableVariant`
(switches on `optimized-` / `upscaled-` only) or `variantPresenceSelect` (has
`upscaled-%` / `optimized-%` presence+fresh arms, no `pcm-%`, while its
`SUM(size_bytes)` *does* count pcm bytes — so the bytes and the flags disagree).

A DSD track holding only `pcm-v1-176400-24` renders a "pcm" chip in the console
and the web player reports it unplayable — with the playable FLAC sidecar sitting
right there, which is the one case the faithful tier exists for.


## P1 — HIGH — `PATCH /api/settings` is the back door around three managed controls

**Files** `internal/admin/admin.go:1561` · `internal/admin/settings_apply.go:225`
· `internal/admin/handlers_api.go:2340` · `cmd/bridge/main.go:2717, 2765-2782`

#876 gates host-owned ACTIONS at the route table — `POST /api/updates/{check,
install,rollback}` behind `ManagedControlUpdates`, `POST /api/restart` behind
`ManagedControlRestart`, `POST/DELETE /api/roots` behind `ManagedControlRoots`.
`PATCH /api/settings` carries **no** `s.managed(...)`, and `managedFieldsIn`
consults **only** `deployment.managedSettings`. Nothing couples a managed
*control* to the settings *field* that performs the same action.

So on a bridge declaring `managedControls: [updates, restart]` with
`updateAutoInstall` absent from `managedSettings`, one authenticated request —

```text
PATCH /api/settings   {"updateAutoInstall": true}
```

— flips a field that `main.go:2717` reads **live** (`LiveAutoInstall`), with
`AutoInstallOpts` and `AutoInstallRestart` wired **unconditionally** (2765-2782).
The updater's next poll downloads the release, swaps the binary and calls
`cancel()`. That is the binary move `ManagedControlUpdates` exists to refuse plus
the process restart `ManagedControlRestart` exists to refuse, from a plain
authenticated PATCH with no console affordance involved.

The wiring block's own comment says the gate "belongs in ONE place, read live,
not duplicated as a wiring decision taken once at boot" — correct, and that one
place is a config field `managedControls` does not reach.

Same shape, lower stakes: `backupIntervalHours` (hot, with a rearm) and
`backupKeep` (**no ceiling** — `config.go:2959` bounds only the interval) reach
the quota `ManagedControlBackups` refuses, on a timer, without touching the
button.

**Fix** Derive an implied managed-settings set from each managed control and
union it into `managedFieldsIn`'s test, so the refusal comes from the same
declaration the route gate reads and the two cannot drift. Documenting "also
list these in `managedSettings`" is the weaker fix — it makes correctness depend
on a second program getting a list right, which is what route-table placement
was chosen to avoid.

**Test** A guard that walks the control→fields map and asserts each named field
is refused on a bridge declaring only the control; plus a negative control on a
bridge declaring neither.

## P2 — MEDIUM — an unauthenticated failed redemption rewrites the ticket file

**File** `internal/adminauth/ticket.go:101-107`

```go
t, ok := live[key]
if !ok {
    // Still rewrite when pruning removed something, so expired records do
    // not accumulate on a store nobody successfully logs into.
    _ = s.writeTicketsLocked(live)     // ← no `changed` gate exists
    return "", ErrTicketInvalid
}
```

`prunedTickets` returns a fresh map and tells the caller nothing about whether it
dropped anything, so the comment's condition is unimplementable as written. Every
failed redemption therefore performs `CreateTemp` → `Write` → `Chmod` → `Close` →
`RenameWithRetry` (which fsyncs the parent directory), under `Store.mu` — the
mutex `ValidateSession` takes on **every** authenticated console request.

`GET /login/ticket?t=x` is unauthenticated (bypass list,
`middleware_auth.go:174`), unthrottled (touches neither `AllowAndReserve` nor
`RecordFailure`), and past `csrfGuard` (GETs pass by design).

Measured by the review agent on darwin/arm64+APFS: **3.93 ms/req with a ticket
outstanding vs 159 µs idle (24.8×)**; eight flooding clients took an
authenticated `GET /api/stats` from **278 µs to 33.1 ms (118.9×)**; control with
no ticket outstanding **1.1×**, isolating the cause. It is also a one-request
oracle for "a login link is live right now", and it hands the documented
cross-process clobber to an anonymous caller.

**Fix** Have `prunedTickets` report whether it removed anything and gate the
write on it — i.e. implement the comment. A miss with nothing pruned then does
no syscalls beyond the read.

**Second reviewer** The quick-wins consult reached the same finding
independently.

## P3 — MEDIUM — a `HEAD` request burns the one-time ticket

**Files** `internal/admin/admin.go:1685` · `internal/admin/handlers_login.go:209`

`mux.HandleFunc("GET /login/ticket", …)`. Go's ServeMux maps a `GET` pattern onto
**HEAD** as well; `pageLoginTicket` never checks `r.Method`, and
`RedeemLoginTicket` deletes before judging. Verified with a 20-line probe against
the real `net/http`: GET 200, **HEAD 200 (handler reached)**, POST 405 — two
handler invocations for two requests. `$GOROOT/src/net/http/server.go:2485` says
so in as many words.

Anything that HEADs the link before the human clicks — a mail scanner, a chat
unfurler, a corporate proxy, a prefetcher — spends the credential, and the human
then gets a bare redirect to `/login` with (by deliberate design) no explanation.

**Fix** Refuse anything but GET in the handler.

## P4 — MEDIUM — the export buffers what its docblock says it streams

**File** `internal/admin/handlers_export.go:121, 130-143`

`// apiExport streams the bundle as a download.` It does not, twice over:
`buildExport` fully materialises the bundle before a byte is written, and
`SetIndent` makes `Encode` marshal into an internal buffer and indent into a
second one. Measured by the review agent on 100k history rows: **24,488,947 bytes
in a single `Write`**, heap delta **51.4 MB**, totalAlloc **200.4 MB**.

`exportHistoryCap` is 100,000, the admin server runs with **`WriteTimeout` unset**
(a documented invariant), and `/api/*` GETs are unrated — so one authenticated
GET is a ~100 MB allocation with no deadline to cut it off, repeatable and
concurrent. On a shared hosted host that is one tenant's lever on the box.

Same file, same loop:
- **The `truncated` flag lies at exactly the cap.** The loop condition
  `len(out.History) < exportHistoryCap` is checked *before* each fetch and pages
  are 1,000, and `100000 % 1000 == 0` — so `len` can never exceed the cap, the
  `[:exportHistoryCap]` trim is unreachable, and the comment above it ("can
  overshoot by up to 999") describes a mechanism the code cannot reach. With
  *exactly* 100,000 rows the loop exits on the condition, `len >= cap` is true,
  and the bundle ships `"truncated": {playbackHistory: true}` about an export
  that omitted nothing — in the one field whose whole stated job is honesty.
- **A redundant query on every export.** The loop breaks only on `len(page) == 0`,
  so any history that is not an exact multiple of 1,000 costs one extra SQLite
  round-trip to fetch an empty page.
- **`s.deps.CfgHolder.Load().LibraryName` is dereferenced unguarded** (line 163)
  while `managed_controls.go:48-51` — same package — explicitly guards
  `cfg == nil`, and `handlers_pages.go` uses the safe `s.config()`.
- **A client disconnect logs at `Error`.** `buildExport` takes `r.Context()`; a
  cancelled download surfaces as `logger.Error("admin export", …)`.

**Confirmed by all three sources independently** (PRISM w5-07, the admin agent,
and the quick-wins consult).

## P5 — MEDIUM — `bridge render` (and three siblings) silently widen a positional scope to the whole library

**Files** `cmd/bridge/render.go:45` · `upscale.go:161` · `optimize.go:40` ·
`analyze.go:35`

`flag.Parse` stops at the first non-flag argument. `--filter` defaults to `""`,
and empty means *every track*. None of the four has an `fs.NArg()` guard, so
`bridge render "Kind of Blue"` — the form the command's own docblock all but
invites, showing `--filter "Kind of Blue"` two lines above — parses with an empty
filter and renders the whole library: hours of ffmpeg+2×sox per track, GBs of
sidecars, and one `indexed_at` delta row per track to every paired device.

Driven against a scratch install:

```text
$ bridge render --dry-run "Kind of Blue"
Found 0 candidate track(s); 0 need conversion.        exit=0   ← accepted
$ bridge enrichment retry "Kind of Blue"
enrichment retry: unexpected argument "Kind of Blue"  exit=2   ← the guarded sibling
```

CLAUDE.md already carries the rule, and **PR #856 added exactly this guard to
`enrichment retry` in this very window** — the four `--filter` commands were not
swept.

**Fix** The `enrichment.go:361` guard verbatim in all four, plus a package-level
sweep test so the class stays closed.

## P6 — MEDIUM — the transcode CLI walk reports UPnP-routed rows as "missing source files"

**File** `cmd/bridge/upscale.go:603, 412-419, 488-490`

`runUpscaleBatch` walks the unfiltered `ListTracks`. A routed row has a `tracks`
row and no local file, so it fails `Resolve`+`Stat` and lands in
`counters.sourceMissing`, rendered as *"Skipped N track(s) with missing source
files (run `bridge scan` to reconcile)."* — advice that cannot work, because
`bridge scan` does not reconcile a routed row.

This is verbatim the defect PR #630 fixed for the analysis walk, whose rationale
is quoted in the file next door (`analyze.go:255-266`): on the author's own
hybrid fixture, 89 local tracks against 15,283 routed. `Store.TrackPathsLocal`
carries the anti-join and `autoOptimizeCandidateSQL` carries it; the transcode
CLI walk never got it, and `bridge render` is a new consumer.

The `dupe_suppressed` half is **not** a defect — `ListTracks`' docblock names CLI
batches as a deliberate full-store consumer.

## P7 — MEDIUM (measured) — `CanDecodeVia` ignores the extension gate the same package documents

**Files** `internal/transcode/transcode.go:1287` · `ffmpeg_probe.go:72`

CLAUDE.md: *"The probe is gated on the extension … so only a source whose decoder
is genuinely undecided pays one."* `Run` obeys it (`needsDecodeRouting`, with a
comment saying so). `CanDecodeVia` — the shared policy home for **four** per-track
call sites (the per-track enqueuer, both batch walks, the auto-optimize sweeper)
— calls `FFmpegSnapshot()` unconditionally, and `FFmpegSnapshot` re-checks binary
presence live on **every** call (two `exec.LookPath` PATH walks).

Measured (this machine, `-benchtime 2000x`):

| | ns/op | allocs/op |
|---|---|---|
| `CanDecodeVia` on a `.flac` | **20,881** | **51** |
| `MissingFFmpegBinaries` | 13,833 | 34 |
| `needsDecodeRouting` (the gate `Run` uses) | **23.71** | **0** |

**~880×**, on the sources that make up nearly the whole library. At 50k tracks
that is ~1.0 s of pure PATH-walking and 2.5M allocations per walk.

The gate is **provably** behaviour-preserving: `decodeRouteFor` reads `ff` only
in the `classDSD` arm and in `ff.Available() && class == classMP4`, and
`decodeClassForExt` maps exactly `{.m4a,.mp4,.m4b,.m4p}`→MP4 and
`{.dsf,.dff}`→DSD with `classSoxOnly` as the zero value — precisely the set
`needsDecodeRouting` selects.

**But it depends on P8, and must not land without it.**

## P8 — MEDIUM — `FFmpegInfo`'s zero value grants the MP4 fallback, contradicting its own docstring

**File** `internal/transcode/ffmpeg_probe.go:63, 72`

> `// FFmpegInfo is what ProbeFFmpeg learned. The zero value is "nothing known"`
> `// and grants nothing.`

```go
func (i FFmpegInfo) Available() bool { return len(i.MissingBinaries) == 0 }
```

On `FFmpegInfo{}`, `MissingBinaries` is nil, so `Available()` is **true**. No
production caller passes a literal zero today, so this is not a live defect — but
it is a live **trap for P7's fix**, which wants to pass a zero `FFmpegInfo` on the
gated path. Fix P8 first and P7 becomes safe by construction.

Every value that should answer true has `Path` set (`ProbeFFmpeg` assigns it
before probing; the missing-binaries early return does not), so
`i.Path != "" && len(i.MissingBinaries) == 0` makes the docstring true without
changing any production answer.

## P9 — LOW — `ProbeFFmpeg` swallows a run error when a partial listing parses

**File** `internal/transcode/ffmpeg_probe.go:118-126`

The docstring promises *"on any other failure the returned info carries
DecodersKnown=false alongside the error, so a caller that ignores the error still
fails closed."* When `cmd.CombinedOutput` errors **and** the partial output
happens to parse as a decoder table, `DecodersKnown` is true and the error is
dropped — a truncated listing is treated as authoritative and cached for 30 s.

Bounded, because capability derivation is conjunctive (all four `dsd_*`), so
truncation can only *remove* capability. The finding is the **contract
violation**, not a wrong route. Correct the docstring or the branch — but note
that failing closed on every `runErr` would also discard a *complete* listing
from an ffmpeg that exited non-zero for an unrelated reason, so the docstring is
the cheaper correction.

## P10 — LOW — `sameRateFamily` reports two rates in *neither* family as sharing one

**File** `internal/transcode/dsd_render_chain.go:180-182`

`(a%44100==0) == (b%44100==0) && (a%48000==0) == (b%48000==0)` returns **true**
for e.g. `(32000, 32000)`: `false == false` on both halves. The doc says it
"reports whether two rates share a 44.1k / 48k family."

Reachability is thin — every real pipe rate is fs/8 of a DSD rate and every
target is a 44.1/48 multiple — so this is latent, not live.
`(a%44100==0 && b%44100==0) || (a%48000==0 && b%48000==0)` is correct and
strictly stronger.

## P11 — LOW — operator-facing text and docblocks that the window falsified

Each is the class CLAUDE.md calls the dangerous one: *a false comment that
explains a design choice is how the next change gets made on the same reasoning.*

- `cmd/bridge/menu.go:525-528` — the docblock says
  `TestUninstallPromptDoesNotClaimDeletionIsImpossible` **scans the Fprint call
  sites in this file**, and explains why. The shipped test does the opposite: it
  drives `actUninstall` with a buffer and asserts on stdout — and its own
  docblock says the scanning version "was wrong twice over". Both halves of the
  surviving comment are false, and the reasoning it licenses is one this repo has
  already rejected twice, in this file, in this PR.
- `internal/admin/managed_controls.go:25-26` — names
  `TestManagedControlsAreEnforcedAtTheRouteTable`, which **does not exist**, and
  claims it "walks the registered routes". The real guard,
  `TestEveryManagedControlNameGatesARoute`, text-scans `admin.go` and asserts each
  control *name* gates *at least one* route — so a new route for an
  already-covered action added without `s.managed(...)` fails nothing. This
  comment is the justification for putting the gate at the route table at all.
- `cmd/bridge/upscale.go:103` — `bridge render`'s feature-gate refusal prints
  *"Transcoding (upscale + optimize) is disabled"*, naming two commands and not
  the third, which is the one the operator ran.
- `cmd/bridge/render.go:82-87` — `--gc` is documented as "deliberately NOT gated
  on the feature flag … cleanup must work on a bridge whose operator has just
  turned the feature off." True for `dsdRender.enabled`, **false** for
  `upscale.enabled`: `bootstrapTranscodeCmd` refuses before `runGC` is reached,
  so an operator who turns transcoding off cannot reclaim their sidecars.
- `cmd/bridge/optimize.go:3-10` — header still documents `OptimizeEligible` (PCM
  hi-res only) after #865 moved the gate to `OptimizeEligibleFor`, and still says
  "**Both** invoke `runUpscaleBatch`" where three now do.
- `cmd/bridge/upscale.go:63-64, 79-80` — `bootstrapTranscodeCmd` /
  `transcodeBootstrapResult` name two of their three callers.
- `cmd/bridge/auto_optimize.go:127-131` — a blank line detached `soxSnapshot`'s
  doc comment, so godoc and IDE hover lose it. `gofmt` does not object.
- `internal/transcode/dsd_render_chain.go:334-336` — says `appliedGainDB` /
  `truePeakDBTP` are pointers "so … an unmeasured (silent) source ships `null`".
  `dsdSettings` always passes `&appliedGainDB`; only `truePeakDBTP` can be null.
- `internal/transcode/batch.go` — `handleOptimizeEnqueueFailure` hardcodes
  `"submit optimize:"` for PCM-render batches too; `optimizeCandidates.add`'s
  docblock still says a projection "carries neither duration nor channels" after
  #863 started forwarding both; `WithRenderTempDir` names `Upscale.TempDir` where
  the field is `JobSpec.TempDir`.
- `internal/admin/handlers_variants_dir.go:183` — `probeUsedByKind`'s comment
  says "always returns BOTH keys" where it now pre-seeds three.

## P12 — LOW — console surfaces #876 did not reach

- `internal/admin/templates/jobs.html:210, 232` — `#jobs-backup-now` and
  `#jobs-upd-check` post to now-gated endpoints and render on a managed bridge.
  `grep '\.Managed' internal/admin/templates/` returns only diagnostics, library
  and settings. The operator gets "Snapshot failed" / "Check failed" — precisely
  the "a button that renders and then 403s reads as a broken console rather than
  as somebody else's job" the PR exists to remove.
- `internal/admin/static/app.js:3597, 5095-5200, 1547, 6932` — the feature trays
  already fetch `/api/settings` into `traySettings` (which carries
  `managedSettings`) and ignore it, leaving eleven managed fields flippable
  outside `/settings`. Pre-existing since #763/#782, but it is the console half
  of the posture #876 set out to complete.
- `internal/admin/static/app.js:4069-4075` — `hideManagedSettings` runs from a
  `.then()`, and `dropUnofferedFields` decides from live DOM `hidden` state at
  submit time. A Save issued before that fetch resolves — or any Save after it
  **fails** — supplies every managed field and the PATCH is refused whole, the
  nineteen-name wall #877 exists to fix.

## P13 — LOW — `GET /api/export` has no affordance and no documentation

`grep -rn "api/export" .` outside its own files returns exactly one line: the
route registration. No button, no link, no `ops/` note, no docs. "Everything a
person made, in one file — the portability answer" is reachable only by someone
who reads `admin.go` and types the URL, which does not meet the stated
motivation. This is the same gap as the missing CLAUDE.md section.

## P14 — LOW — the new parity guard has two blind spots

`internal/admin/settings_payload_test.go:60, 72` — the input-name regex
`name="([a-zA-Z][a-zA-Z0-9]*)"` silently skips any name containing `-` or `_`
(all 31 current names are alphanumeric, so it passes today), and
`strings.Contains(builder, name)` is unanchored, so a template `name="backupKeep"`
is satisfied by a builder line reading `backupKeepDays:`.

---

# Rejected, with the reason

Recorded so the next run does not re-raise them.

| Claim | Source | Why rejected |
|---|---|---|
| `renderDSD` applies +6 dB when the peak is unmeasured → clipping | PRISM w5-01 (HIGH) | `analyze.TruePeakDBTP`'s `ok=false` is the **digital-silence** contract; a real measurement failure returns `err`, which `renderDSD` propagates. +6 dB of silence is silence, and Stage C's `soxReportedClipping` fails the job rather than publishing. The reviewer conflated `ok=false` with `ClipGuardedGainDB`'s non-finite guard, which is a different condition. |
| DSD tracks projected optimize-eligible when `dsdRender` is off | PRISM w5-08 (HIGH) | `DSDRenderEligible`'s first line is `if !caps.Active() { return false }`. A batching artifact — the predicate was in another batch. |
| `purgeStaleRenderScratch` counts a concurrently-removed file as removed | PRISM w5-01 | Cosmetic, and defensible: the file *is* gone. Either behaviour is arguable; not worth a change. |
| Constant-time comparison needed on ticket lookup | (considered) | The lookup is `live[hex(sha256(raw))]` — the same digest-keyed-map shape as `s.sessions`, `auth.Store` and `pairing.Store`. Exploiting map-lookup timing needs SHA-256 inversion. |
| `ManagedControlVariantsDir` / `ManagedControlRoots` route gaps | PRISM w5-08 | Verified covered in both directions; all eight named routes carry `s.managed(...)`, and `TestManagedControlsRefuseTheRequest` drives the real stack with an envelope assertion plus a negative control. |
| `SetMaxOpenConns(1)`, moving `Enqueue`'s fire out of the lock, `ReplaceAll` in `stripBOMs`, importing the API's 2 s mtime tolerance into the scanner | standing FP classes | All documented deliberate decisions; cited, not re-derived. |

# Not taken in this batch, and why

- **P12's tray/async items** and **P13's affordance** are product-surface
  decisions (where a "Download my data" button belongs, whether trays should
  render read-only or vanish). Recorded; they want the user's call on placement,
  not a correctness fix.
- **The `renderScratchDir` shared-`/tmp` multi-tenant question** — `0700` on
  `/tmp/1-bit-bridge-render` means the first tenant to render owns it. Reaching a
  verdict needs the hosted layout, which lives in the private conductor repo.
- **`bridge render`/`upscale`/`optimize` write `track_variants` from a second
  process with no `refuseIfBridgeMayBeRunning` gate.** Pre-existing for all
  three; `render` merely joins the set. Out of window as a *defect*, worth its own
  decision.

# Wire impact

**None.** No `/v1` shape, DTO, `json:` tag or `PROTOCOL.md` section is touched by
any accepted item, so there is no Mirror-PR obligation and
`internal/version.ProtocolVersion` does not move. Verified: `PROTOCOL.md` is
byte-identical with the iOS mirror at the window's head.

# PRs

File-disjoint, so they go in parallel off `main` rather than stacked.

**Every confirmed finding is dispositioned here.** Nothing is left implicit.

| PR | Items | Files |
|---|---|---|
| #878 | **P0a**, **P0b** | `internal/api/upscale_batch.go`, `internal/manifest/extractors.go` |
| #879 | **P1** | `internal/admin/settings_apply.go`, `internal/config/config.go` |
| #880 | **P2**, **P3** | `internal/adminauth/ticket.go`, `internal/admin/handlers_login.go` |
| #881 | **P4** | `internal/admin/handlers_export.go` |
| #882 | **P5**, **P6** (partial), **P11** (cmd) | `cmd/bridge/*` |
| #883 | **P8** → **P7**, **P0c**, **P9**, **P10**, **P11** (transcode) | `internal/transcode/*` |
| #884 | the DSD measurements that ran nowhere | `.github/workflows/gate.yml`, `Makefile` |
| #885 | **P0e** | `internal/admin/player_audio.go`, `internal/manifest/catalog_refs.go` |
| #886 | **D1** + the rules from this run | `CLAUDE.md`, `ops/engineering-log.md` |

**Carried, with the reason:**

- **P0d — the one-sided clip guard.** ESCALATED, not deferred. Letting the
  clamp go negative would attenuate an over-modulated master to −1 dBTP like
  every other source, which changes what a rendition SOUNDS like; keeping it
  means a `> 0 dBTP` source can never get a rendition and the operator sees
  "sox reported clipping in stage C". That is a product decision about the
  bridge's own output, not a correctness fix, and it is the user's. The
  docblocks claiming the peak lands at or below −1 dBTP "by construction" are
  false either way and are corrected in #883.
- **P0c's second half — `Track.Channels` is written by the DFF extractor
  only**, so every DSF budgets as stereo while carrying a real duration, and a
  5.1 rip under-reserves 3×. Stamping `channelNum` in the DSF extractor is an
  EXTRACTION-LOGIC change, so it needs its own `ExtractorVersion` bump and a
  library-wide re-extract — which #878 has just spent. Batching a second bump
  into the same week would double that cost for one budget figure. Filed for
  the next extraction change to ride along with; the lane multiplier in #883
  is the half that removes the ENOSPC risk.
- **P6's routed-row half** (`ListTracks` enumerating UPnP-routed rows and
  reporting them as "missing source files, run `bridge scan`"). Needs a
  `ListTracksLocal` reader with the same anti-join `autoOptimizeCandidateSQL`
  already carries — a new store method, which is a different blast radius from
  the rest of #882. The wrong ADVICE is the user-visible half and is worth its
  own PR against a real hybrid fixture.
- **P12 (the console surfaces #876 did not reach) and P13 (the export has no
  affordance)** — product-surface decisions about where a control belongs, not
  correctness fixes. Recorded for the user.
- **P14** (the parity guard's two blind spots) — real but latent: all 31
  current input names are alphanumeric, so the regex gap bites nothing today.
  Filed with #886's rules so the next person touching that guard sees it.

