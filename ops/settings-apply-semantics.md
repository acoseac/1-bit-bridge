# Settings apply semantics

Which `PATCH /api/settings` fields take effect immediately, which need a
supervised restart, and — for the hosted product — which ones a control plane
must schedule a bounce for.

**Audience:** the cloud control plane, and anyone changing how a config field is
consumed. **Scope:** the admin surface only. Nothing here is on `/v1`;
`ProtocolVersion` is untouched by all of it.

Kept in `ops/`, not `docs/`, per the repo rule — `docs/` is a public website. This
file names no hosts, no keys and no unfixed weaknesses, so the placement is
convention rather than necessity.

---

## The response

```json
{
  "restartRequired": true,
  "fields": {
    "libraryName":         { "status": "live" },
    "libraryWatchEnabled": { "status": "restart" },
    "backupKeep":          { "status": "unchanged" },
    "autoOptimizeEnabled": { "status": "restart",
                             "reason": "no auto-optimize sweeper is wired on this bridge …" }
  }
}
```

`fields` carries an entry for every field the request **supplied**, including
ones that turned out to be unchanged. Absence means "you did not send it", which
is a different fact from `unchanged` ("you sent it and it was already that").

| status | meaning | control-plane action |
|---|---|---|
| `live` | in effect now | none |
| `restart` | persisted to `bridge.yaml`; the consumer captured the old value at boot | schedule a supervised restart |
| `unchanged` | supplied at its current value; nothing was written | none |

`restartRequired` is the legacy blanket boolean, **derived** from the map
(`applyReport.needsRestart`) rather than tracked beside it, and carried
indefinitely — the bridge is open-source and self-hosted, so the set of scripts
reading that key is unknowable. Deriving it is what stops the two drifting;
`TestRestartRequiredIsDerivedFromTheFieldReport` pins it in both directions.

### `reason`

Populated **exactly when the outcome depended on this bridge's runtime state**
rather than on a static property of the field. Two shapes qualify:

**The status could have been different elsewhere.**

- `autoOptimizeEnabled` → `restart` because no sweeper is wired here.
- `mdnsEnabled` → `restart` because no mDNS lifecycle is wired.
- `tailscaleMode` → `restart`, naming which transition it was.

**The status is `live` and the change applied, but the feature is inert for a
reason the operator can act on.**

- `fingerprintEnabled` → `live`, "saved, but fpcalc is not installed on this
  bridge, so no fingerprinting will run". A restart would change nothing, so
  `restart` would be a lie — and silence would have the operator move the switch,
  read "Saved.", and never learn that nothing will happen.

**What does not qualify:** `listenAddress` → `restart` because listeners bind
once. True on every bridge, and spelling it out for all the restart-bound fields
would be twenty near-identical strings the reader learns to skip — at which point
the ones that carry information get skipped along with them.

### Why the value is an object

`{"scanIntervalSec": "restart"}` is shorter and one breaking change away from
being wrong. The honesty rule below **is** a reason-generating rule, so the shape
that needs `reason` is the shape we already have. Adding a key to an object is
additive; widening a string into an object is not.

---

## Two rules that keep the report honest

### 1. Never split a field's halves

Either **every** consumer of a config field reads it live, or **every** consumer
takes it at boot. Never one of each.

The failure this prevents: hot-applying a cheap struct field (so `/v1/health`
starts advertising the capability) while reporting `restart` (so the settings
response calls the same change pending). The two surfaces contradict each other
in the same breath, and a `partial` status would only name the contradiction
rather than remove it.

Applied:

- `optimizeEnabled` → fully live, by making the auto-optimize sweeper
  unconditional (the same move fingerprint makes).
- `atlasEnabled` → fully boot-bound, staying alongside its file-backed harvest
  state store.

With the rule applied there is no remaining field a fourth status would describe.

### 2. When a change cannot take effect, say so

The rule the auto-optimize toggle established. A field that hot-applies *when the
subsystem is wired* must still answer `restart` on a bridge where it is not —
with a reason. Reporting a silent success there has the operator flip the switch,
watch nothing happen, and have nothing to act on.

This is why the answer is computed **inside** the `CfgHolder.Update` closure, at
the same sites that used to set `restart = true`, and never derived afterwards
from a static field → semantics table: a table computed outside the closure
cannot see this bridge's wiring.

---

## The matrix

Classes describe *how the value is consumed*, not how important it is.

- **A — hot.** Read from the live holder per use, or backed by a real start/stop
  lifecycle.
- **B — convertible.** A captured scalar or a static ticker where a closure over
  the holder would do.
- **C — lifecycle.** Needs runtime start/stop of a boot-wired subsystem.
  Emptied by the always-construct-never-stop move — see below.
- **D — permanent.** Restart-bound for a reason that will not change.

| Field | Class | Status today | Notes |
|---|---|---|---|
| `libraryName` | A | `live` | Read per request by `/v1/health`, admin pages, pair URL. The mDNS *instance name* only re-reads on the next `Set(true)`, so a rename does not re-advertise Bonjour until mDNS is toggled. |
| `customEndpoints` | A | `live` | Read per request by `advertise.Endpoints()` and `/v1/health`. `customEndpointsText` is the textarea form of the same field and reports under this key — the array form wins when both are sent. Cert SAN coverage for a new host stays operator-driven. |
| `duplicatesFilter` | A | `live` | Closure over the holder, read per stamping pass; the PATCH nudges `TriggerDuplicatesPass`. |
| `autoOptimizeEnabled` | A | `live` / `restart`+reason | Shared `enabledFn` read per sweep; nudged in both directions. `restart` with a reason when no sweeper is wired. |
| `mdnsEnabled` | A | `live` / `restart`+reason | `mdnsLifecycle.Set(bool)` — a real start/stop. `restart` with a reason when no lifecycle is wired. |
| `tailscaleMode` | A / C | `live` (cli→disabled) / `restart`+reason | Only `cli → disabled` hot-applies, via `TailscaleDisable`. Every other transition rewires the auto-pilot and the listener composition; the reason names which transition it was. |
| `listenAddress` | D | `restart` | The `/v1` listener bind. |
| `adminAddress` | D | `restart` | The admin listener bind, and the loopback-only enforcement point. |
| `scanIntervalSec` | **A** | `live` | `RunPeriodic` and the analysis sweeper both re-read a provider before each wait; the cadence rearm wakes them so the new value binds now. |
| `backupIntervalHours` | **A** | `live` | Provider re-read before each wait. The ticker goroutine is now started **unconditionally** and parks when the interval is 0, which is what makes `0 → N` observable. |
| `backupKeep` | **A** | `live` | Read at prune time, which is the only moment retention means anything. No rearm — there is no parked wait to disturb. |
| `retentionPlaybackHistoryDays` | **A** | `live` | The daily sweeper reads `cfg` through the holder at the top of every pass, so the next tick sees the change. No rearm: the interval is a compile-time const, not a config field, so there is no parked wait to disturb — the same shape as `backupKeep` above. Validation refuses 1–89 (the bounded smart-mix windows run to 90 days) and anything above `MaxRetentionDays` (past ~349 years the cutoff timestamp overflows and the reap deletes the whole table). |
| `retentionDeviceRegistrationDays` | **A** | `live` | Same loop, same pass. Orphaned registrations — rows bound to a revoked token — are reaped regardless of this setting; this is the policy half only. |
| `updateAutoInstall` | **A** | `live` | Read at the top of every poll cycle. Symmetric in both directions — see the note below. |
| `updateQuietHours` | **A** | `live` | Resolved at the auto-install gate, not at construction. |
| `updateCheckIntervalHours` | **A** | `live` | The poll loop re-reads the cadence before each wait (rearm-woken), clamped by the same floor/default `New()` applies. |
| `upscaleEnabled` | **A** | `live` (+reason when sox is unusable) | Pool constructed unconditionally and never stopped before shutdown; one shared live predicate gates the health flag, the manifest variant gate and every enqueue path. The sox probe is lazy + TTL-cached. |
| `uploadEnabled` | **A** | `live` (+reason when the subsystem is absent) | The upload manager is constructed unconditionally at boot; the handlers read `cfg.Upload.Enabled` from the runtime holder per request, so a flip binds on the next call with nothing to nudge and no parked wait to disturb. `restart` with a reason only when `Deps.Upload` is nil, which is a wiring fault rather than a setting — reported so a misconfiguration does not read as the operator's own choice. Deleting content is a SEPARATE gate: enabling an additive feature must never silently enable a destructive one. |
| `allowDelete` | **A** | `live` (+reason when the subsystem is absent) | The trash manager reads the gate LIVE on every mutating call, so a flip binds on the next one. Its own gate, deliberately never folded into `uploadEnabled`: enabling an additive feature must not silently enable a destructive one. A nil gate fails CLOSED — the safe direction for the only thing in the bridge that removes library content. |
| `analysisEnabled` | **A** | `live` (+reason when sox is unusable) | Same shape as upscale. |
| `smartPlaylistsEnabled` | **A** | `live` | Store wired unconditionally; the health flag and the endpoint both key off one `smartPlaylistsActive()`. The regenerator is started unconditionally and gated per run. |
| `optimizeEnabled` | **A** | `live` | Health advertisement, admin projection gate and pre-generation sweeper all read it live. The sweeper is wired unconditionally within an active upscale pool. |
| `libraryWatchEnabled` | C | `restart` | fsnotify watcher with a `scanWG` / `closing` drain contract guarding a SQLite-corruption vector. |
| `enrichMusicBrainzBaseURL` | **A** | `live` | Read per use, and the politeness pacing **re-derives from the same live value** — see below. |
| `enrichCoverArtBaseURL` | **A** | `live` | As above. |
| `atlasEnabled` | B / C | `restart` | Deliberately whole: an API field *and* a file-backed harvest state store. **Stays boot-bound under rule 1** — converting only the cheap half is the split this rule forbids. |
| `fingerprintEnabled` | **A** | `live` (+reason when degraded) | Sweeper started unconditionally behind one shared live predicate; the fpcalc probe is lazy + TTL-cached rather than a boot snapshot. |
| `fingerprintApiKey` | **A** | `live` | The AcoustID client reads the key per request. A blank submit is a documented no-op and reports `unchanged`.<br>`ACOUSTID_API_KEY` still wins when set. |
| `dlnaEnabled` | C | `restart` | HTTP listener + per-interface SSDP advertisers. |

### Endpoints outside the settings PATCH

| Endpoint | Semantics |
|---|---|
| `POST` / `DELETE /api/roots` | Hot — `Scanner.SetRoots` + `Resolver.SetRoots` + a background rescan. The single ↔ multi-root storage-form flip is handled by `WipeFilesystemTracks`. |
| `POST /api/upscale/variants-dir` | Hot — resolved from the live holder per submit and per disk pre-flight. |

### Not reachable through this surface at all

`deployment.mode`, `dataDir`, `tlsCertPath` / `tlsKeyPath`, and the tsnet state
directory are config-file edits only. Autocert has its own handler and is out of
scope here.

---

## Cadence fields: what "live" means, precisely

A schedule that is re-read *eventually* is not the same as one that is re-read
*now*. Reporting `live` for a 6 h scan interval whose loop will not consult the
new value until the old one elapses is technically true and practically
indistinguishable from being ignored.

So the cadence conversion has two halves, and they ship together:

1. **The provider.** Every loop reads a `func() time.Duration` before each wait
   instead of building one `time.NewTicker` up front. A timer per iteration
   rather than a ticker, because a ticker cannot change period.
2. **The rearm.** `Deps.TriggerCadenceRearm` pokes every such loop when a cadence
   field changes, so it re-reads immediately.

The rearm is **not** a work-nudge. A nudge asks a sweeper to do the work now; a
rearm asks it only to re-read its schedule. Collapsing them would turn "I changed
the backup cadence" into "run a backup", which on a large library is a materially
different thing to have asked for. Separate channels, separate fan-out.

It fires **only on an actual change**, because it restarts the wait: firing it on
a same-value save would push the next scheduled run out by a full interval every
time the operator pressed Save on an unrelated field.

**Dormant is a resumable state.** `interval() <= 0` parks a loop instead of
ending it, and the backup ticker is started unconditionally. The old shape
returned outright — so "disabled" was terminal for the process, and setting
`backupIntervalHours` back to 24 had no loop alive to notice. Costs one parked
goroutine on a bridge with backups off.

### `updateAutoInstall` is symmetric, deliberately

An asymmetry that hot-applied only the OFF direction was considered and rejected:
it is a hidden state machine, and the ON direction cannot surprise anyone.
`maybeAutoInstall` runs **only** from the poll loop — the admin "Check now" path
deliberately does not call it — the cadence floor is 1 h with a 6 h default, and
the install still has to clear the quiet-hours window and the in-flight sessions
gate (re-checked *after* the download, since a stream may have started during
it). There is no path from "operator ticked the box" to "restart within seconds".

The install options and the restart callback are now wired **unconditionally**
rather than only when auto-install was on at boot. They are inert on their own —
`maybeAutoInstall` returns at its first line unless the gate is on — and leaving
them boot-gated is what would have made the toggle a lie: a bridge that started
with auto-install off would flip the switch, hit the "install opts missing"
defensive branch, and never install anything.

---

## Always construct, never stop

The pools looked like class C — "needs runtime start/stop" — and that framing
was wrong in a way that cost two fields their hot-apply for a while.

**The dangerous half of a pool lifecycle is stopping one.** `Stop`'s ordering
against `Enqueue`, the publisher drain, the fire-state-change-under-lock rule:
those are the invariants with a history of live panics. **Constructing** a pool
is cheap and safe, and an idle pool is a handful of goroutines parked on a
channel.

So both pools are now built unconditionally at boot and never stopped before
shutdown. The flag decides whether the feature **does** anything, not whether it
**exists** — the same wired-vs-active split as the feature gates below, applied
to something that merely looked heavier.

Two things fall out:

- **The sox probe had to become lazy + TTL-cached**, because a boot probe is
  itself a boot snapshot: an operator who installs sox and then enables the
  feature should not have to bounce the bridge to be believed. Same shape as the
  fingerprint toolchain probe.
- **The manifest provider needed a live gate too.** Its `upscaleEnabled` atomic
  is written once at startup, so after a PATCH it would keep stripping (or keep
  emitting) variants against a value `/v1/health` had already moved past —
  a client's manifest disagreeing with health about whether the feature is on.
  `SetUpscaleEnabledSource` supersedes the stored boolean; nil restores it, which
  is what every caller that only calls `SetUpscaleEnabled` keeps getting.

When sox is missing or FLAC-less the toggle still reports `live` **with a
reason**: the setting did apply, a restart would not install sox, and the
alternative is an operator watching a switch they just moved do nothing.

---

## Feature gates: wired vs. active

The conversion that made `smartPlaylistsEnabled`, `optimizeEnabled` and
`fingerprintEnabled` hot is one move repeated three times: **split "is this
feature wired on this bridge" from "is it switched on right now."**

Wired is a boot fact — the pool exists, the toolchain is present, the store is
attached — and it genuinely cannot change mid-process. Active is the operator's
toggle, and it can. Folding both into one boot-time nil check is what made these
restart-bound; separating them is what makes the toggle live without touching any
lifecycle.

Concretely:

- The subsystem is wired **unconditionally** (a store attached, a sweeper
  goroutine started), because a component that only exists when the flag was on
  at boot makes the flag restart-bound however live the rest of the path is.
- Every consumer reads **one shared predicate**. Three copies of the same gates
  is how a card comes to claim "active" while every sweep short-circuits — the
  divergence `/v1/upscale/stats` already exists to prevent.
- A disabled pass records **no status**. Reporting one puts a "last run"
  timestamp on the Jobs card for work that never happened, which is worse than
  looking stale: it says the feature is doing something.

`fingerprint`'s prerequisite probe is **lazy and TTL-cached**, not a boot
snapshot. `fpcalc -version` is a fork-exec, and charging every bridge for it to
support a feature that is off by default is the wrong trade; the cached probe
runs the first time anything asks. Its failure is logged **once per process** —
it is consulted on every sweep and every card render, so an unconditional line
would be the per-minute spam the SSDP send-suppression exists to prevent.

`atlasEnabled` was deliberately **not** converted. Its cheap half (an API field)
would convert in a line, but its other half opens a file-backed harvest state
store at boot. Converting one and not the other is exactly the split rule 1
forbids, and the whole-field conversion is a lifecycle change this stack does not
take on.

---

## The enrich base URLs: pacing travels with the host

`pacing.go` makes the politeness interval a function of the base URL rather than
a separate knob, deliberately: it is a contract with two specific HOSTS
(musicbrainz.org at 1.1 s, coverartarchive.org at 500 ms), and a self-hosted
mirror is neither. That is why the pacing is derived rather than configured.

Making the base live therefore has to make the interval live **with it**.
`MinInterval()` re-derives from the same live value; a live base with a frozen
interval is the one mistake in this area that reaches a third party — an operator
clears the mirror URL, the client starts calling public MusicBrainz, and it does
so at the self-hosted 150 ms, roughly 6.7 rps against a service that asks
anonymous clients for one.

**The straddle is safe by construction, not by luck.** Base and interval are read
separately, so a change can land between them. The pacing gap is measured since
the last request to the OLD host, so the first request to the NEW one arrives
with no prior traffic to it at all, and every request after it is paced by the
new host's own interval. The worst case is a single request that waited longer or
shorter than the new host requires while that host has seen nothing from us.

Two smaller rules the tests pin: an empty or whitespace-only live value falls back
to the constructed base (a cleared config field must resolve to the public default,
not to a host-less URL), and a nil provider keeps the captured base and its
captured pacing entirely, which is what every caller other than the serve path
wants.

The enricher needed converting too. It captured the interval into an exported
field at construction, so a live client alone would not have been enough — the
sleeps read the enricher's copy. `mbMinInterval()` / `caaMinInterval()` ask the
client when it has a live base and fall back to the field otherwise, so tests and
the CLI that set those fields directly are unaffected.

---

## Restarting without anyone noticing

A supervised restart is ~2 seconds and the control plane owns the process, so
for a restart-bound field the fix is for the control plane to bounce on the
operator's behalf. The blocker was never the restart — it was that the person
whose stream got cut never asked for anything.

`POST /api/restart` therefore waits for in-flight `/v1/read` + `/v1/download` to
finish first, and **reports whether it managed it**:

```json
{ "restarting": true, "drained": true, "inflight": 0, "waitedMs": 412 }
```

- **Draining is the default**, including for a bodyless POST — a caller that did
  not think about it gets the safer behaviour. `{"drain": false}` opts out.
- **Idle costs nothing.** Zero in-flight returns immediately; only a real stream
  makes anyone wait.
- **Bounded, and honest at the bound.** A stream can outlive any deadline, so the
  wait is capped (30 s default, 5 min ceiling — the admin server sets no
  `WriteTimeout`, so nothing else would stop a caller holding the request open).
  On timeout it restarts anyway and says `drained: false` with the count and a
  reason. Reporting a clean drain it could not verify would have a control plane
  record a graceful restart and never learn it interrupted someone.
- **`inflight: -1` means unknown**, not zero — a bridge with no session tracker
  wired cannot know what it is interrupting, and says so.

The wait is deliberately **not** tied to the request context: the restart was
already requested, and a control plane whose HTTP client gives up mid-drain still
wants the bounce.

The machinery is `updater.Tracker`, which the auto-installer has gated on since
PR #42. It simply was never wired into the admin restart path.

---

## Managed settings: hiding what the tenant does not own

`deployment.managedSettings` names settings whose value belongs to an external
control plane. A managed field is **hidden by the console and refused by the
PATCH** (403 `managed-setting`).

It exists because four of the restart-bound fields are not a choice a hosted
tenant gets to make at all:

| Field | Why it is not the tenant's |
|---|---|
| `listenAddress`, `adminAddress` | The control plane's binds. |
| `dlnaEnabled` | A cloud host has no LAN. DLNA is meaningless there. |
| `libraryWatchEnabled` | Object storage has no inotify. A provisioning decision. |

Rendering those as switches is worse than hiding them: **a control the operator
cannot act on is a control that will eventually be flipped, do nothing, and be
reported as a bug.**

Three rules:

- **Refused, never silently dropped.** "Reports success and changes nothing" is
  the exact failure the per-field report exists to remove; reintroducing it for
  managed fields would be a regression wearing a feature's clothes.
- **The refusal is atomic.** A patch mixing a managed field with a free one is
  refused whole, so a caller cannot half-apply and be told it worked.
- **Membership is by reflection over `settingsPatch`, not a hand-listed set.** A
  field added later is covered automatically; a list would leave the newest field
  silently changeable on a managed bridge with nothing to notice.
  `TestEveryPatchFieldCanBeManaged` pins it.

Empty (the default, and every self-hosted install) manages nothing and changes no
behaviour.

**Two controls hide themselves without needing the list**, because public mode
already refuses them outright: `dlnaEnabled` (`startDLNAIfEnabled` returns early
when `IsPublic` — SSDP multicast has no meaning on a public VPS) and
`mdnsEnabled` (no LAN to advertise on). Both are hidden by `IsPublic`, derived,
no config. A control the bridge itself ignores is worse than no control: the
operator flips it, nothing happens, and the only explanation is a note they have
to read.

**`tailscaleMode` is deliberately NOT in that group.** It reads like a LAN
concern but is not refused in public mode — it is merely *defaulted* to
`disabled` when unset, and switching it to `cli`/`tsnet` genuinely works there.
Hiding it automatically would remove a working control. A hosted deployment that
does not want it should name it in `managedSettings`.

**There is no iOS half to this.** These are admin-surface settings; the iOS app's
per-bridge toggles are playlist backup, favorites and playback history, none of
which are in this set. A cloud tenant with `dlnaEnabled` forced off simply never
sees the `dlnaServer` health flag, which is already the right behaviour.

---

## What a control plane must restart for

**Six fields. Everything else applies live.**

| Field | Why it stays restart-bound | Recommendation |
|---|---|---|
| `listenAddress` | The `/v1` listener bind | Provisioning-time setting |
| `adminAddress` | The admin listener bind | Provisioning-time setting |
| `upscaleEnabled` | `transcode.Pool` — enqueue / stop / publisher-drain ordering has a history of production panics | Provisioning-time setting |
| `analysisEnabled` | `analyze.Pool` — same invariants | Provisioning-time setting |
| `dlnaEnabled` | HTTP listener + per-interface SSDP advertisers | Irrelevant to a cloud tenant: no LAN to advertise on |
| `libraryWatchEnabled` | fsnotify watcher behind a `scanWG` / `closing` drain guarding a SQLite-corruption vector | The one deliberate maybe — see below |
| `atlasEnabled` | An API field *and* a file-backed harvest state store; converting one half is what rule 1 forbids | Convertible, but as a whole-field lifecycle change |
| `tailscaleMode` | Every transition except `cli → disabled` rewires the auto-pilot and the listener composition | Provisioning-time setting |

Two of them (`listenAddress`, `adminAddress`) will never convert. The rest are
**provisioning decisions per tenant rather than user-facing toggles**, so a
supervised restart at provision time costs nothing — which is why the stack
stopped here rather than taking on runtime pool teardown.

Do not read this table as the source of truth on its own:
`TestMatrixDocMatchesWhatTheHandlerReports` drives the real handler for every row
above, so the answer the bridge gives is the answer the control plane should
trust, and this file is checked against it rather than the other way round.

### When to reopen `libraryWatchEnabled`

If tenants mount storage **after** boot. There the periodic full scan becomes the
only discovery path and its latency is the tenant's first impression of the
product. `mdnsLifecycle` proves the shape works; what makes it a maybe rather
than a yes is that the drain contract guards a corruption vector, and a test for
that drain was once vacuous — it passed with the drain removed — until it was
rewritten around a hook that parks a dispatch at exactly the right instant.

## Tests that hold this together

| Test | Pins |
|---|---|
| `TestEveryPatchFieldIsReported` | Every field `settingsPatch` accepts comes back in the report. Catches a new field added without a report site — otherwise invisible, since it still saves and still answers 200. |
| `TestReportNamesAreRealPatchFields` | The other direction: no report site names a field the struct does not have. |
| `TestRestartRequiredIsDerivedFromTheFieldReport` | The legacy rollup stays in lockstep with the map. |
| `TestMixedPatchReportsPerFieldNotPerRequest` | The motivating case: one request, three different answers. |
| `TestReasonIsPresentOnlyWhenTheAnswerWasConditional` | `reason` on the conditional cases, absent on the static ones. |
| `TestFieldApplyWireShape` | Object values, `reason` omitted when empty (key-absence via a decoded map, never a substring probe). |
| `TestTrayBadgesAgreeWithWhatTheServerReports` | The UI badge agrees with what the server actually reports for that field — the check that makes a stale badge fail loudly when a field is converted. |
| `TestFeatureTrayRestartBadgesAgreeWithSettings` | The two UI predictions agree with each other. |
| `TestSettingsPageBadgesAgreeWithWhatTheServerReports` | The **Settings page's own** badges against what the handler reports. The two tray tests iterate TRAY rows and consult the page only for fields a tray contains, so a Settings-page-only field (`updateQuietHours`, `fingerprintApiKey`, the `enrichSource` picker) was invisible to both — all three kept a stale badge through the conversion PRs and shipped. Scrapes the page rather than a hand-listed set, so a badge on a new field is checked automatically. |
| `TestMatrixDocMatchesWhatTheHandlerReports` | **This file's matrix against the real handler**, row by row, plus every PATCH field having a row at all. A doc that has drifted is worse than no doc: it sends an operator to bounce a bridge that already applied the change, or tells a control plane a field is live when it is still waiting. |
| `TestSmartPlaylistsHealthFlagAndEndpointMoveTogether` | Rule 1 where it would actually break: `/v1/health` and the endpoint agree across all four combinations of wired × enabled. |
| `TestAcousticActiveGatesBothSites` | The fingerprint gate is checked at the skip-reason site too, so a disabled bridge does not report "no fingerprint match" for work that never ran. |
| `TestPacingFollowsTheLiveBase` | The politeness interval re-derives with the base — the one mistake in this area that reaches a third party. |
| `TestSweepLoopRereadsIntervalEveryIteration` | The provider is consulted per iteration, not cached — a provider read once is exactly as restart-bound as the duration it replaced. |
| `TestSweepLoopRearmDoesNotSweep` | A rearm re-reads the schedule and never runs the work. |
| `TestSweepLoopDormantIntervalIsResumable` | `0 → N` is observable, i.e. the loop parks rather than exits. |
| `TestSweepLoopDormantClearsScheduledNext` | No stale "next run at …" after the cadence is disabled. |
| `TestCadenceChangeFiresTheRearm` | Which fields fire the rearm, and which correctly do not. |
| `TestUnchangedCadenceDoesNotFireTheRearm` | A same-value save cannot push the next run out by a full interval. |
| `TestLiveProvidersOverrideStaticOptions` / `TestLiveCheckIntervalIsClamped` / `TestNilLiveProvidersKeepStaticBehaviour` | The updater reads its three settings at decision time, clamped, without regressing callers that pass none. |

All of the above are negative-control-verified: each was run against a mutated
build and confirmed to fail. Two failure modes to watch for, both hit while
writing these:

- **A mutation that removes the only use of a symbol is a build break, not a
  control.** Treat that as "control invalid" and mutate differently — e.g.
  `(flag || true)` rather than deleting the flag's only reference.
- **A control that edits the wrong text is also invalid, and it looks like a
  pass.** Re-adding a badge with a first-match string replace hit the prose
  *"a free AcoustID application key"* instead of the label; the test correctly
  ignored it and reported green. Anchor the mutation on the thing the test
  actually reads.

And a third, about the tests rather than the controls: **a scraping test that
skips a field passes while checking nothing.** The page-badge test above
initially skipped all three defects it was written for, because its
value-generator had no case for string fields. Every scraper here carries a
minimum-count guard for that reason; when adding one, make it fail loudly on an
empty scrape rather than trusting the loop body.
