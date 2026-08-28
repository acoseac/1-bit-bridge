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

Populated **exactly when the status could have come out differently on another
bridge** — i.e. when it depended on runtime wiring rather than on a static
property of the field.

- `autoOptimizeEnabled` → `restart` because no sweeper is wired here: a reason.
- `mdnsEnabled` → `restart` because no mDNS lifecycle is wired: a reason.
- `tailscaleMode` → `restart`, naming which transition it was: a reason.
- `listenAddress` → `restart` because listeners bind once: **not** a reason. It
  is true on every bridge, and spelling it out for all the restart-bound fields
  would be twenty near-identical strings the reader learns to skip — at which
  point the ones that carry information get skipped along with them.

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
| `updateAutoInstall` | **A** | `live` | Read at the top of every poll cycle. Symmetric in both directions — see the note below. |
| `updateQuietHours` | **A** | `live` | Resolved at the auto-install gate, not at construction. |
| `updateCheckIntervalHours` | **A** | `live` | The poll loop re-reads the cadence before each wait, clamped by the same floor/default `New()` applies. |
| `upscaleEnabled` | C | `restart` | `transcode.Pool` + coordinator + five API adapters + the sox precheck. |
| `analysisEnabled` | C | `restart` | `analyze.Pool` + sweeper + the API store adapter. |
| `smartPlaylistsEnabled` | B | `restart` | Regenerator goroutine + an API store field. Pure SQL over the existing manifest — no toolchain, no pool. |
| `optimizeEnabled` | B | `restart` | An API field plus whether the auto-optimize sweeper is wired at all. |
| `libraryWatchEnabled` | C | `restart` | fsnotify watcher with a `scanWG` / `closing` drain contract guarding a SQLite-corruption vector. |
| `enrichMusicBrainzBaseURL` | B | `restart` | Client built at boot. **The base URL also derives the pacing** — see the invariant in `CLAUDE.md`. |
| `enrichCoverArtBaseURL` | B | `restart` | As above. |
| `atlasEnabled` | B / C | `restart` | Deliberately whole: an API field *and* a file-backed harvest state store. Stays boot-bound under rule 1. |
| `fingerprintEnabled` | B | `restart` | Sweeper wired only when the boot precheck (fpcalc + AcoustID key) passed. |
| `fingerprintApiKey` | B | `restart` | Read once by the boot precheck. A blank submit is a documented no-op and reports `unchanged`. |
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

## What a control plane must restart for

Everything whose status comes back `restart`. As of the current tree that is the
class C and class D rows above, plus any class B field not yet converted, plus
the conditional cases when the subsystem is not wired on that tenant.

Two of them will never convert, and a control plane should treat them as
provisioning-time settings rather than runtime ones:

- `listenAddress`, `adminAddress` — listener binds.

Three more are recommended to stay restart-bound. They are provisioning
decisions per tenant, not user-facing toggles, so a supervised restart at
provision time costs nothing:

- `upscaleEnabled`, `analysisEnabled` — worker pools whose enqueue / stop /
  publisher-drain ordering has a history of production panics.
- `dlnaEnabled` — a cloud tenant has no LAN to advertise on.

`libraryWatchEnabled` is the one deliberate maybe. Re-open it if tenants mount
storage *after* boot, where the periodic scan becomes the only discovery path and
its latency is the tenant's first impression.

---

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
| `TestSweepLoopRereadsIntervalEveryIteration` | The provider is consulted per iteration, not cached — a provider read once is exactly as restart-bound as the duration it replaced. |
| `TestSweepLoopRearmDoesNotSweep` | A rearm re-reads the schedule and never runs the work. |
| `TestSweepLoopDormantIntervalIsResumable` | `0 → N` is observable, i.e. the loop parks rather than exits. |
| `TestSweepLoopDormantClearsScheduledNext` | No stale "next run at …" after the cadence is disabled. |
| `TestCadenceChangeFiresTheRearm` | Which fields fire the rearm, and which correctly do not. |
| `TestUnchangedCadenceDoesNotFireTheRearm` | A same-value save cannot push the next run out by a full interval. |
| `TestLiveProvidersOverrideStaticOptions` / `TestLiveCheckIntervalIsClamped` / `TestNilLiveProvidersKeepStaticBehaviour` | The updater reads its three settings at decision time, clamped, without regressing callers that pass none. |

All of the above are negative-control-verified: each was run against a mutated
build and confirmed to fail. A mutation that removes the only use of a symbol is
a **build break, not a control** — treat that as "control invalid" and mutate
differently.
