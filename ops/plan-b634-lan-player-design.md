# B634: the bridge player's LAN page: browse, queue, play and switch the DAC from a browser, behind a passcode

Design, round 1. Docs only: this file is the round's one change. Base: main at 4678b983. Every
`file:line` below is at that commit unless it names another.

**Inputs.** The owner's decision of 2026-11-12 (W2, D1, C1, O1, the page, the closing criteria), the
brief's review (Codex and Carbon, 2026-11-13, ten points, each settled below and indexed in §15),
`ops/plan-b728-dsd-capability-design.md` §6, §7.2 and §11, and B9 PR 2.

**B9 PR 2's commit 29641b98 (#1453) is not on the build host this round ran on**: the bridge's
object store there does not hold it (no ref names it, no loose object is there, and `git log`
does not know it). This design reads B9 PR 2 at the tip of `feat/b728-b9-pr2` that the store holds,
e4493648 (round 5), whose
`internal/player/playback/dsd_choice.go` carries the configuration lease's paragraph at lines 26-33
and `takeLease` at line 308, the lines the brief cites, and §7.2 of the B728 design as that branch
rewrote it (8b3c4278, "the design says a choice is written only while the player is stopped"). It
does not fall back on main's §7.2, whose write beside playback B9 withdrew (§2.3).

**One probe ran this round** (a scratch test in `internal/adminauth`, with `-race` and the tags,
then deleted, the tree clean after): on go1.27.3, a plain-HTTP request to a listener that serves
TLS is answered `HTTP/1.0 400 Bad Request` and `Client sent an HTTP request to an HTTPS server.`,
then closed, and the handler never runs; and golang.org/x/crypto v0.58.0's bcrypt refuses a secret
of 73 bytes ("password length exceeds 72 bytes") and takes one of 72. §5.2 and §6.2 rest on them.

## 0. At a glance

- **The live switch.** One binding seam in serve's player, `playerBinding`, which every reader of
  the player's binding reads at each use. A switch moves it inside the engine's maintenance hold,
  entered at the generation the operator saw, and B9 PR 2's configuration lease, after an
  open-check-close of the target and the save. It ends stopped on the new DAC, with no restart. No
  engine is replaced, so nothing is handed over (§2).
- **One rule for every configuration operation** (a DAC switch, the tone, a DSD write): it is made
  only from a stopped player. The switch and the tone take the operator's go-ahead, which the
  hold's own release turns into the stop; a DSD write never does: "Stop playback to change DSD"
  (§2.3).
- **The tone** is backlog B602, B601's remainder, built in PR 1 for the console and the page alike.
  Its one statement is §3's, which §10's row 6 cites: the tone the owner's decision lists for the page,
  and the one the closing criteria ask a browser alone to play.
- **The page** is a small app of its own on a listener of its own: the web player's catalog with
  hardware eligibility, the queue and the transport through the one engine with generation checks,
  Signal Path from GET /v1/outputs' own facts, the switch, the tone, and the DSD choice through B9
  PR 3's write and B13's controls (§4).
- **Browser access.** One IPv4 socket per served LAN address, on the OpenHome surface's binding and
  admission, moved into a package both use; HTTPS on the bridge's own certificate; plain HTTP gets
  net/http's fixed 400 line and nothing else; no HSTS; `output.player.lanPage`, off by default, port
  7793, read live (§5).
- **The passcode** lives in a file of its own in the data directory, as the console's account does
  in `adminauth.json`: the credential is the file's, the sessions are serve's, in memory (§6).
- **PROTOCOL.md.** PR 1 rewords three passages (the player's `dac`, `binding_changed`, the
  redaction) from "the boot binding" to the binding the player runs on now: a Mirror-PR pair. PR 2
  and PR 3 change none (§8).
- **The PRs.** PR 1: the shared controls, the live switch and the tone. PR 2: browser access. PR 3:
  the page's controls, coexistence and the acceptance run (§9).
- **The budget held.** No engine command (the switch, the tone and the page's edits use
  `EnterMaintenance` and `IfGeneration` as they are), no second admission point (the engine's hold
  and B9 PR 2's lease, the lease taken for the switch and the tone through the writes' turn its DSD
  writes take), no second queue owner, no `/v1` route, no admin route on the LAN listener, no mixer
  write. One dependency is open: B9 PR 3's engine reservation, which this design takes to be that
  hold (§2.3, §14).

## 1. The hazard, and what stays impossible

At 4678b983 a browser on the LAN reaches nothing: the console binds loopback
(`internal/config/config.go:2894`, checked at `:4060`) and refuses another source or Host
(`internal/admin/admin.go:2634`, `:2671`), and the OpenHome surface is off by default
(`internal/config/player.go:138-147`). Through an SSH tunnel, choosing another DAC
(`internal/admin/handlers_player_output.go:668`) stops playback, and the player keeps the DAC it
started with until the bridge restarts (`:751`), because serve builds it from the boot binding
(`cmd/bridge/serve_player.go:342`, `:351`, `:479`, `:486`; the log at `:688`).

| Must stay impossible | How | Proven by |
|---|---|---|
| A LAN request reaching any admin route (`auth-tls.md:38-41`) | The page's listener serves its own mux (`internal/playerpage`), which imports neither `internal/admin` nor `internal/api` and registers an allowlist of its own patterns; the console stays on loopback, unchanged | `TestTheLANListenerServesNoAdminRoute` (PR 2) |
| A passcode or a session token crossing the LAN in clear | The listener speaks TLS after admission and nothing else; plain HTTP gets net/http's fixed 400 line and a close; the cookie is `Secure` and `__Host-` | `TestAPlainHTTPRequestGetsNoPageFormOrCookie`, `TestThePasscodeTravelsOnlyOverTLS` (PR 2) |
| Native or DoP from the page without first use; a DSD or DAC change while anything plays | B9 PR 2's `policy.Choose` (`first_use_unacknowledged`, `dop_confirmation_required`) inside B9 PR 3's write, which the page reaches through the control layer; the one rule of §2.3 | `TestThePagesDSDWriteNeedsTheFirstUseAcknowledgement` (PR 3, once B9 PR 3 lands), `TestASwitchWhilePlayingNeedsTheGoAheadAndEndsStopped` (PR 1) |
| The bridge stopping MPD, or anything else, to take the DAC | The switch reads the target's holder from /proc/asound before its hold, and opens the target non-blocking under it; EBUSY is a refusal naming the holder; nothing signals, stops or reconfigures another process | `TestASwitchToABusyDACNamesItsHolderAndLeavesItAlone` (PR 1) |
| A stale tab's edit landing after an app cast or a controller changed the queue | Every page write but Stop and Pause carries the generation the page last read, as `IfGeneration` on its first engine command; the later steps of a compound edit name the entries the edit itself made | `TestAStaleGenerationEditIsRefusedAndChangesNothing`, `TestACastMidWayThroughAPageEditRefusesTheRest` (PR 3) |

## 2. The live switch

### 2.1 The rebind seam, and why this one

**Chosen: a binding its readers take under the lease.** Serve's player holds the binding it runs on
in one seam, `playerBinding` (PR 1, `cmd/bridge/serve_player_binding.go`): the boot snapshot's
`output.player.device` at the player's start, and the binding a switch moved it to since. Every
reader asks it at each use (`Current`) and keeps no copy. The switch is its only writer, and moves
it only inside the engine's maintenance hold and the backend's configuration lease, once the
binding's save has landed (§2.4). The seam closes a channel at each move (`Moved`, as the config
holder's `Changed` does, `config-lifecycle.md:267-276`), on which serve's supervisor wakes. It keeps
the bindings it has held this run (a handful) for the element's redaction (§2.2, R10).

Why this one:

1. **The exclusion is one engine's, from admission to publication.** The engine that enters the
   hold is the engine that exits it. The brief's review asks for exclusion through any handover to
   a replacement engine (Codex 1); with this seam there is none to make (§2.5).
2. **What the controllers hold stays valid.** The engine's run, its queue, its IDs and tokens, its
   generation and the OpenHome surface (its sockets, its SSDP registration, the controllers'
   connections) are untouched. A restarted run draws a new run ID, and a generation from another
   run never matches (`internal/player/engine/snapshot.go:64-74`), so every generation a page or a
   controller holds would be refused, and the surface would be stopped and started, or re-pointed.
3. **Nothing a start does is redone inside a request**: no identity, floor, seal or queue preflight
   under the data directory's lock, no DSD state open or migration, no engine stop bound
   (`playerEngineStopBound`, 7 s) or backend bound inside the switch.
4. **The backend keeps its DSD state, its probe cache and its workers.** The new DAC gets its
   effective set as a hotplug change gives one: the supervisor forces a refresh and owes the probe
   at the first snapshot of the new binding.
5. **The cost is named and guarded**: every reader must read the seam (§2.2's guard), and a cache
   keyed on the unit is let go as a hotplug change lets it go (the engine's `Discard` on a report's
   `UnitChanged`, `internal/player/hotplug/report.go:79-88`; `player.md:123`).

**Lost: an in-process restart of the player's run on the saved binding.** It must hand the
exclusion from the old engine's hold to a new engine that holds nothing yet, or leave a window in
which SOAP meets no engine (501) and then a fresh one; it draws a new run ID, so every generation
held is refused; it restarts or re-points the OpenHome surface; and it puts the start's whole path,
and the stop's bounds, inside the switch.

### 2.2 Every reader of the binding the player started with

Found by search, not from the brief's list: every read of `output.player`'s `Device` in `cmd/bridge`
and what each copy feeds, every field and closure of type `config.PlayerDevice`, and every consumer
of the hotplug feed's binding.

| # | Reader | At 4678b983 | In PR 1 |
|---|---|---|---|
| R1 | `bridgePlayer.device` | `serve_player.go:254-255`, set at `:342` | removed: the seam |
| R2 | The boot settings a nil `Live` falls back to | `serve_player.go:344-347` | the seam (only a test passes a nil `Live`) |
| R3 | `playerOutput.boot`: `readLive`'s `foreign` and its log line, `fromSnapshot`'s `snap.foreign`, `withheld`'s `binding_changed` | `serve_player.go:351`, `:659-720` (log `:688`) | a snapshot of another binding than the seam's is withheld `binding_changed`, which only a switch's first moments can show; the live config's binding is no longer compared, since the control layer saves it and moves the seam together (§2.4); the log line goes |
| R4 | `boundFromFeed(feed, boot)`, the DSD policy's `Bound`, a closure that captured the device | `serve_player.go:479`; `serve_player_dsd.go:61-80`, `:116` | `boundFromFeed(feed, seam)`: the seam at each call |
| R5 | The backend's `Locator`, `playback.BindingLocator{Device: player.Device}`, asked at every open and every reopen after a pause's release | `serve_player.go:486`; `internal/player/playback/content.go:104-132` | `seamLocator`: a `BindingLocator` over `Current()` at each `Locate` |
| R6 | `probeConditions(snap, p.device)` | `serve_player.go:581`; `serve_player_dsd.go:101-107` | the seam |
| R7 | `playerDSD.boot`: the migration's part for the DAC the binding resolves to | `serve_player_dsd.go:175`, `:225`, `:383` | the seam |
| R8 | `consolePlayer.bootDevice`: `pending`'s `dac` entry, `withheld`, `running.binding`, `dac`, `DACs`' running card, `Chosen`'s substitution | `serve_player_console.go:84`, `:150`; `:217`, `:291`, `:309`, `:408`, `:706`, `:837-838` | the seam; for a running player the `dac` pending entry and the `binding_changed` withholding go |
| R9 | GET /v1/outputs' DAC read, `readPlayerDAC(ctx, p.trees, p.device)` | `serve_player_output.go:134` | the seam at each read |
| R10 | The element's redaction of the binding's serial and port (`bridgeDSDFacts`, `playerText`) | `serve_player_output.go:177`, `:317-325` | every binding the seam has held this run: a set published for the DAC just left may still be the one read |
| R11 | The element's DAC gate, `SameBinding(snap.Binding(), b.p.device)` | `serve_player_output_dac.go:25` | the seam |
| R12 | The console doctor's player-dac note, "the config names another DAC than its player started with" | `main.go:5035-5039`; `doctor_player_dac.go:97-100`, `:471-476` | the seam: for a running player it no longer fires; for an engine that does not run, it compares the live config with the seam |
| R13 | The hotplug watcher's settings, which read the LIVE binding (`livePlayerSettings`, `hotplug.SettingsFrom`) | `output_hotplug.go:46-58`; `internal/player/hotplug/settings.go:27-33` | unchanged: the switch saves before it moves the seam, so the watcher is reading the new binding as the seam moves |
| R14 | PROTOCOL.md: the bridge object's `dac`, `binding_changed`'s row, the redaction | `PROTOCOL.md:925`, `:940`, `:1034` | the player's binding now (PR 1's Mirror pair, §8) |

**Statements that become false, and go with their readers**: the log line at `serve_player.go:688`;
the comment at `main.go:4975-4979` ("The player keeps the DAC it started with until serve next
starts"); the console's `playerStopsDAC` (`handlers_player_output.go:751`), its `dac` pending text
(`serve_player_console.go:218-219`) and its `binding_changed` sentence (`:393-394`); and the rules at
`player.md:160` and `:168`, `admin-console.md:950` and `output.md:86` (§12).

**Not readers**: the OpenHome surface, which reads the boot snapshot's room and port
(`serve_player_openhome.go:99`, `:120`, `:133`) and never the binding; the engine, whose
`engine.Config` holds no binding; and the CLI's own reads of its config (`output_dac_bind.go`,
`output_play.go`, `output_dsdtest.go`, `bridge doctor`'s at `doctor_player_dac.go:228`), which are
other processes or no serve at all.

**How the list stays complete.** An enumeration is the defect (CLAUDE.md, LOUPE 2026-09-09), so PR 1
makes the boot binding unreachable but through the seam, and fails the build where a new reader
reaches it another way:

1. **`TestTheBindingIsReadThroughTheSeamAlone`** (cmd/bridge, go/ast over the package's non-test
   files, as `TestOnlyHostTreesNamesTheHostsTrees` walks them). It fails on (a) a read of
   `output.player`'s `Device`, directly (`X.Output.Player.Device`) or through a local bound to the
   block (`player := X.Output.Player`, then `player.Device`), outside an allowlist of functions,
   each with its reason: the seam's one boot read (`newPlayerBinding`), `planServePlayer`
   (`Configured` alone), the control layer's reads of the live config it saves, and the CLI's own
   (`output_dac_bind.go`, `output_play.go`, `output_dsdtest.go`, `bridge doctor`); (b) a struct
   field, a map or slice element or a package variable of type `config.PlayerDevice` or
   `*config.PlayerDevice` but the seam's own and an allowlist (`consoleSelection.devices`, the
   candidates of a choice; `dacJudgement.dev`, the doctor's own config), each with its reason; and
   (c) a function with a `config.PlayerDevice` parameter that returns a function literal, the shape
   `boundFromFeed` has today. A new reader has nowhere to take the binding from but these, so it
   fails, the test naming its file and line and saying to read `playerBinding.Current` at each use.
2. **`TestEveryReaderOfTheBindingFollowsASwitch`** (a serve boot test, two DACs on sysfstest trees):
   after a switch from A to B every reader R3 to R12 shows B. The engine's output report is B's;
   the backend's next Start locates B (the stand-in backend records the unit); the DSD policy's
   `Bound`, the probe's conditions and the migration's part are B's; the console's running binding
   and DAC are B's, with no pending `dac`; GET /v1/outputs' `dac` is B's card, with A's serial and
   port redacted wherever they show; and the console doctor's note is quiet.
3. **Its controls** (PR 1): a field holding the boot device added to `bridgeOutput`; the locator
   built from `w.Cfg.Output.Player.Device` again; `boundFromFeed` capturing the device again. Each
   is red in the structural test, the second and third in the behavioural one too.

### 2.3 One rule: stopped, the hold and the lease, and the go-ahead

**B9's revised contract, not §7.2's fallback** (Codex 2, Carbon 1). Main's §7.2 wrote a DSD choice
beside playback with `interrupt` and `would_interrupt`. B9 PR 2 withdrew it (the owner, 2026-11-10:
"Stop playback to change DSD"), and its §7.2 (8b3c4278) is the contract here: a DSD write is made
only while the player is stopped, `playback_not_stopped` otherwise, with no `interrupt` and no
`would_interrupt`. Its two halves are the engine owner's stopped check, which grants the reservation
in the same step (B9 PR 3's, not built), and the backend's configuration lease (B9 PR 2's
`takeLease`), which admits only in the step under the backend's lock that finds no session's record,
nothing holding the device and no release unconfirmed. **This design takes B9 PR 3's engine
reservation to be the engine's maintenance hold entered at the generation of a stopped snapshot**:
"checks the transport is stopped and grants the reservation in one step", with no engine command
added, and the very hold the switch and the tone take, so there is one engine admission point for
every configuration operation. If B9 PR 3 builds a reservation of another kind, that dependency is
unresolved, and PR 1's helper (`enterStopped`) is the one to reconcile with it (§14, question 2).

**The rule.** Every configuration operation (a DAC switch, the tone, a DSD write), from any surface
(the console, the page, and B9 PR 3's route for the app), runs inside two holds and nowhere else:

- **the engine's maintenance hold** (`EnterMaintenance`, `internal/player/engine/engine.go:541`),
  entered at a generation the operation names, which the owner checks in the step that closes every
  mutation (`maintenance.go:25-74`);
- **the backend's configuration lease** (`takeLease`, `dsd_choice.go:308` at e4493648), taken after
  the backend's writes' turn, B9 PR 2's order.

The hold is entered in one of two ways, and no other:

- **from a stopped player**: the snapshot at the named generation is `Stopped`, with no transition
  under way (`Pending` none), no release unconfirmed and no hold. The owner's generation check makes
  "still stopped" and "now closed" one step.
- **with the operator's go-ahead** (`interrupt: true`), which the switch and the tone take and a DSD
  write does not: the hold is entered at the generation the operator was shown, and its own step
  releases what plays (`releaseSession` with `IntentStop`, `maintenance.go:64-66`); it answers once
  that release is confirmed. This is the only way any of these operations stops playback.

Without the go-ahead, a player that is not stopped refuses the operation, nothing changed. Refused
are: **starting** (`Playing` while `Starting`), **playing**, **paused** (the device held, or let go by
the pause's release: the engine is `Paused` either way), **releasing** (whatever follows the
release), **a release unconfirmed**, and **another operation's hold**. The word says what the
operator may do next:

| Operation | Not stopped, no go-ahead | With the go-ahead |
|---|---|---|
| DAC switch | 409 `would_interrupt`, its words saying what it stops | taken: the hold's release stops it |
| Tone | 409 `would_interrupt` | taken |
| DSD write | 409 `playback_not_stopped` ("Stop playback to change DSD") | not offered: B9's write has no go-ahead |
| Any, while another operation holds the turn or the hold | 409 `configuration_busy` | the same |
| Any, at a generation that is not the engine's now | 409 `stale`, with the state now | the same |

One predicate decides "stopped" for all three. The two words differ only in what they invite:
`would_interrupt` an answer that sends the go-ahead (the console's B601 word, and B12's check's in
the B728 design's table), `playback_not_stopped` a Stop first. Both pages ask in the server's words,
never from their last status (`admin-console.md:950`).

**The app's DSD write** (B9 PR 3's `PUT /v1/outputs/{id}/dsd`) names no generation. The control
layer reads the snapshot, takes its generation when it is stopped (`playback_not_stopped`
otherwise), and enters the hold at it. A generation that an edit moved meanwhile (the hold refuses
`conflict`) is read again, at most three times; a player that started meanwhile is
`playback_not_stopped`.

### 2.4 The switch, step by step

The console's `POST /api/output/player/dac` and the page's `POST /api/lan/dac` send one body,
`{"selection", "binding", "generation", "interrupt"}`, and the control layer answers both alike.

0. **Eligibility**, before anything is read or written (B601's `playerRefusal`,
   `handlers_player_output.go:363-387`, moved into the control layer): a public bridge, 403
   `player_not_available`; `managedSettings` naming `playerDevice`, 403 `managed-setting`; a host
   that is not Linux, 409 `player_not_supported`; a player block that does not read, 409
   `player_block_unreadable`.
1. **The control layer's turn**, tried and never waited for: one configuration operation at a time,
   across every surface (§2.9). Held: 409 `configuration_busy`.
2. **The phase** (§2.7). No engine (off, unbound, invalid, a start that failed, an engine that
   ended): a configuration-only save, B601's as it is today, answered `restart`. Starting: 409
   `player_starting`. Stopping: 503 `stopping`. Running: the switch goes on.
3. **The target, read without opening it.** `Chosen`'s bracketed reading
   (`serve_player_console.go:806-841`, bounded by `consoleReadBound`, 3 s): the token held and fresh
   (10 minutes), its card the same unit at the same enumeration, the binding of that kind the one
   the list offered (`stale_selection`, `binding_refused`, `box_unread`, as today). A binding the
   seam names already: 200 `unchanged`, nothing done. Then the same reading's /proc/asound entry
   for the target's playback substream: one open with an owner is 409 `device_busy` naming the
   holder (§2.6), before anything is stopped.
4. **The hold** (§2.3), at `generation`: `stale`, `would_interrupt`, `configuration_busy`, and a
   hold whose release was not confirmed in time (`Hold.ReleaseConfirmed` false) exited again and
   answered 409 `release_unconfirmed`. From here the renewer runs (§2.5).
5. **The lease.** The backend's writes' turn, tried (a DSD write under way: `configuration_busy`),
   then the lease. A silent probe holding the device slot is waited for within 2 s; a native tail
   still retiring after the hold's release (B9 PR 2's records) is asked again within the same 2 s;
   past it, `configuration_busy`. The lease has one holder at a time only because its holders take
   the writes' turn first: `takeLease` does not ask whether a lease is held, since B9 PR 2's writes
   are one at a time by that turn. So `Backend.Reserve` takes the turn before the lease, as
   `ChooseDSD` does, and a test holds a DSD write in its turn and requires the switch refused.
6. **The target, opened**: one bounded open-check-close (§2.6). Busy: 409 `device_busy` with the
   holder. Refused: 409 `permission_denied`. Gone, or another unit at the endpoint: 409
   `stale_selection`. No answer within 2 s: 503 `dac_unchecked`.
7. **The save**: the console's own write, B601's body moved into the control layer unchanged, under
   the control layer's turn, which takes over the console's lock for these writes (§2.9). The
   holder's `UpdateChecked` (the turn of bridge.yaml's lock), inside which the box is read once more
   and the selection must still be the same unit at the same enumeration building the same binding
   (`stale_selection`), the binding is written, and the clone validated (`validatePlayerClone`). A
   refusal of the holder (another writer's lock, the file changed outside, a name repointed) saves
   nothing (`writeSaveConfigErr`'s classes, `admin-console.md:946`). A save the holder answers nil
   has landed: a config save syncs its staged file, and tolerates a directory sync that fails
   (`internal/atomicwrite/atomicwrite.go:382-388`).
8. **The rebind**: `playerBinding.Set(binding)`, one atomic store, its `Moved` closed. Nothing else
   changes in this step.
9. **Publication.** Serve's supervisor, woken by `Moved`, takes the watcher's latest snapshot: one of
   the new binding is reported, its own verdict, `UnitChanged` set, which discards prepared work; one
   of the old binding is withheld `binding_changed` until the watcher, which follows the saved
   config (R13), publishes the new one. The supervisor forces the DSD policy's refresh and owes the
   probe at the new DAC; the probe runs once the lease is let go (a probe takes no slot under a
   lease). The switch waits for the engine to have taken a report of the new binding, at most 5 s
   (the watcher's longest hold of a transient card, `holdMax`, 3.5 s, and a reading,
   `internal/player/hotplug/timing.go:47-55`); past that it answers with the DAC's state not read
   yet, the output withheld meanwhile.
10. **The end**: the lease and the writes' turn let go, the renewer stopped, the hold exited
    (`ExitMaintenance`: the transport stopped, never resumed, `player.md:127`), the turn let go.
11. **The answer**: 200 `switched`, with the state: the new binding, its DAC's state, the transport
    `stopped`. One Info line names the binding by its label and the unit by its description, never a
    serial.

| Word | Status | When | What changed |
|---|---|---|---|
| `player_not_available`, `managed-setting`, `player_not_supported`, `player_block_unreadable` | 403, 403, 409, 409 | step 0 | nothing |
| `configuration_busy` | 409 | steps 1, 4, 5: another switch, the tone, a DSD write, or a probe past its wait | nothing |
| `player_starting`, `stopping` | 409, 503 | step 2 | nothing |
| `stale_selection` | 409 | steps 3, 6, 7: the token unknown or older than 10 minutes; its card unplugged, replugged, or another unit at its number; the binding the box gives now not the one listed | nothing but, at steps 6 and 7, the go-ahead's stop |
| `binding_refused`, `box_unread` | 409, 503 | steps 3 and 7 | nothing but, at step 7, the go-ahead's stop |
| `device_busy`, with `holder` | 409 | step 3 (an owner in /proc/asound) or step 6 (EBUSY) | nothing; at step 6 the go-ahead's stop |
| `permission_denied` | 409 | step 6 (EACCES, EPERM) | the go-ahead's stop |
| `dac_unchecked` | 503 | step 6, no answer within its bound | the go-ahead's stop; a node the kernel holds in the open stays held until the kernel returns |
| `stale`, `would_interrupt`, `release_unconfirmed` | 409 | step 4 | nothing; `release_unconfirmed` leaves what the late release leaves (`player.md:118`) |
| the save's refusals (`save-config` and its classes) | 409, 500 | step 7 | nothing saved; the go-ahead's stop |

**The go-ahead's stop stands**: a refusal past step 4 leaves the player stopped on the binding it
had, and the answer says that playback was stopped and the DAC was not switched.

**An unplug mid-switch**: before step 3, 6 or 7, `stale_selection`
(`TestASelectionThatWentStaleUnderTheHoldIsRefused`: the target replugged between step 3 and step 6,
and again between step 6 and the save, each refused with nothing saved, the go-ahead's stop said).
After the save, the switch completes, and the player is stopped on the new binding with its output
unavailable (`device_missing`), which every surface shows; a replug makes it available again and plays
nothing (`player.md:123`).

### 2.5 The exclusion outlives every step

- **Renewal.** A hold expires after `MaintenanceTTL`, a minute by default (`engine.go:31`), and its
  expiry admits mutations again (`maintenance.go:112-120`). The switch runs a renewer from the
  hold's answer to its exit: `RenewMaintenance` at a third of the time left before the hold's own
  `Expires`, so two renewals can be missed before it lapses. The steps are bounded well inside a
  minute, but the save's write is not (a stalled disk holds an fsync as long as it likes): the
  renewer, not a deadline, keeps the hold through it. A renewal refused (`ReasonLease`: the hold is
  gone, which only an expiry the renewer missed makes) is the switch's fault: before the save, the
  switch stops there with nothing saved; after it, the switch completes under the lease alone (no
  Start registers while it is held) and says so at Warn.
- **Both holds, each for its own work.** The hold refuses every controller's mutation (SOAP's fault
  501, the pages' 409 `configuration_busy`), and a Stop answers and does nothing
  (`internal/player/engine/owner.go:576-581`); output reports are still taken
  (`owner.go:568-569`), which step 9 needs. The lease refuses the backend's own work: a probe takes
  no slot, and a Start the engine admitted before the hold, if any, waits before it registers.
  Neither alone suffices: the lease alone lets the engine admit a Play whose Start waits for the
  lease and plays once it is let go; the hold alone leaves the probe and the DSD writes' turn to
  the backend.
- **Browser cancellation.** An operation runs on the control layer's own goroutine, on the player's
  lifetime context, never the request's. The request waits for its end at most 30 s, and answers
  202 `switching` past that. A browser that closes, or a page that navigates away, cancels nothing.
  The state both pages read carries the last operation and how it ended (`operation`: its kind,
  state, word and sentence).
- **Shutdown.** Serve's teardown refuses the page's requests and stops its listener first (PR 2); the
  player's shutdown then stops the OpenHome surface, cancels the control layer's context and joins its
  operation, bounded (10 s, which the player's stop bound gains, `servePlayerBounds`), and only then
  stops the engine (§9, PR 1's wiring). The operation looks at that context before the hold, before
  the lease, before the target's open and before the save, and stops there, nothing saved, the hold
  exited. A save under way is finished or refused as the holder's save always is (nothing
  interrupts its rename), the seam moved after it, and the publication skipped. A save stalled past
  the join's bound is abandoned and said: the next start reads whichever binding the file holds,
  stopped.
- **Panics.** The operation recovers its own, lets the lease and the writes' turn go, exits the
  hold, and logs the stack without the panic's value (`player.md:161`).
- **No handover.** No engine is replaced (§2.1). The brief's cases become: SOAP writes sent while the
  save is stalled past the TTL are refused (the renewer holds); SOAP writes sent during the rebind
  and the publication are refused (the hold is exited last); the first SOAP write after the hold
  exits is admitted, the last admitted command winning (C1).

Cases (PR 1): `TestASwitchStalledPastTheMaintenanceTTLStillRefusesSOAPWrites` (the engine's TTL set to
1 s through `servePlayerSeams.engineConfig`, the save parked 3 s at a seam of the control layer's;
SOAP Play, Insert, DeleteAll, SeekId and SetRepeat sent during the park, during the rebind and
during the publication each answer fault 501 and change neither the queue nor the transport; after
the answer a Play is admitted), `TestABrowserThatLeavesMidSwitchCancelsNothing` and
`TestAShutdownDuringASwitchLeavesTheFileOrTheBindingWhole`.

### 2.6 The target: an open-check-close, and its holder named

`Chosen` (`serve_player_console.go:806`) checks the unit's identity and enumeration without opening
the device (Codex 3, Carbon 2). The switch adds two looks at the target:

- **Before the hold** (step 3), from the bracketed reading `Chosen` takes anyway: the target
  endpoint's playback substreams in /proc/asound. One that is open with an owner refuses at once
  (`device_busy`), with the player not stopped. This opens nothing, so an MPD about to open the DAC
  never meets the bridge in its way. An owner that is this bridge's own process is no holder: it is
  the player's own session on the same DAC (a switch of the binding's kind, a serial binding in place
  of a uniqueModel one), which the hold's release closes before step 6
  (`TestASwitchToTheSameDACUnderAnotherKindIsNotItsOwnHolder`).
- **Under the hold and the lease** (step 6), before the save: `alsapcm.Probe` (B728 B4,
  `internal/alsapcm/probe.go:88-112`) through the backend's injected prober (`playback.Config`'s,
  which no test reaches with a real node: `player.md:164`), on the target endpoint, non-blocking,
  with one PCM candidate and the target's own check after open (`binding.Checker` over the step's
  reading, wanting the target's resolution, `player.md:96`). EBUSY is `device_busy`; EACCES and
  EPERM are `permission_denied` (the console's B676 word, pointing at the doctor's player-dac line);
  a node that is not the unit, or one gone, is `stale_selection`. It opens, asks INFO and one
  HW_REFINE, and closes: it commits nothing, writes nothing, and holds the node for milliseconds. Its
  bound is 2 s, on a goroutine of its own; a call the kernel holds past it is abandoned and said, and
  its node closes when the kernel returns.
- **The check is a moment, not a reservation** (D1: the kernel decides access). Between the check
  and the first Start on the new DAC another process can take it; that Start then fails
  `device_busy`, which every surface shows with the holder (`lastError`, `player.md:153`). The switch
  claims only that the target was free, and was the unit the list showed, when it was checked, and
  that the binding saved is that unit's.
- **The holder** is named from /proc/asound's substream status (`internal/alsa`'s `Owner`,
  `reader.go:91-111`): the thread's and the process's names and the pid where they read, as
  `bridge output pcm-test` names one (`player.md:64`). **An owner nobody can name is unknown, never
  an error** (`output.md:30`): a hidden /proc (hidepid), an owner outside this pid namespace
  (`owner_pid` 0) or a thread gone since leaves the names null, and the refusal says "held by a
  process the bridge cannot name". At step 6 the holder is read afresh after the EBUSY; one that
  reading no longer shows (it closed in between) leaves the refusal saying that the DAC was busy and
  naming nobody. Nothing signals, stops or reconfigures the holder, and no word suggests stopping MPD
  from the bridge.

Cases (PR 1): `TestASwitchToABusyDACNamesItsHolderAndLeavesItAlone` (the stand-in holder's process
still runs and still holds the node after the refusal, and the player was not stopped, since the busy
reading came first); `TestASwitchToADACHeldByAnUnnamedProcessSaysSo` (an `owner_pid` of 0, and a
hidden /proc: `holder` null and the words); `TestASwitchToADACThatBecameBusyUnderTheHoldIsRefused`
(free at step 3, EBUSY at step 6: refused, nothing saved, the player stopped by the go-ahead and
saying so).

### 2.7 No engine is a real state

Serve builds no engine when the player is disabled or unbound (`planServePlayer`,
`serve_player.go:109-138`), and the console saves a binding in those states today (Codex 4). PR 1
keeps those configuration-only saves, and names each phase apart:

| Phase | When | What the backend may hold | DAC switch | Tone, DSD write | Queue and transport |
|---|---|---|---|---|---|
| `off` | `output.player.enabled` not true at boot (`:121-122`) | nothing: no backend was built | configuration-only save, `restart` | 409 `player_not_running` | 409 `player_not_running` |
| `unbound` | enabled, with no binding (`:123-128`, `not_configured`) | nothing | configuration-only save of the first binding, `restart` | `player_not_running` | `player_not_running` |
| `invalid` | a pause release the engine does not take (`:132-136`, `config_invalid`) | nothing | configuration-only save, `restart` | `player_not_running` | `player_not_running` |
| `starting` | the supervisor builds the player (`ready` open) | a backend built and not yet run | 409 `player_starting` | 409 `player_starting` | 409 `player_starting` |
| `start_failed` | the start ended with no engine (`p.failed`) | nothing: no session ever started, and a refused start removed the identity it made (`:453-460`) | configuration-only save, `restart` | `player_not_running` | `player_not_running` |
| `running` | the engine runs | its sessions | the live switch (§2.4) | §2.3 | §4.3 |
| `abandoned` | the engine's Run ended in a panic (`run.panicked`, `:525`) | a session's device until the process exits (`releaseAll` tried, `:846-853`) and its share of the data directory's lock | configuration-only save, `restart`, the answer and the status saying the device may still be held until the bridge restarts | `player_not_running` | `player_not_running` |
| `stopping` | serve's teardown began | what its release has not let go yet | 503 `stopping` | 503 `stopping` | 503 `stopping` |

**Activation still needs a restart.** Starting a player that did not start (the first binding, the
player turned on, a pause release corrected, a failed start's cause fixed) is serve's boot decision
(`player.md:158`), and this design adds no in-process start: the configuration-only save answers
`restart`, and the console's restart, or `bridge restart`, does it. The page offers no restart, which
is administration (§5.4). Since the page needs the console or the CLI to set its passcode, a box's
first setup (the player on, its first DAC, the passcode, the page on) is the console's or the
CLI's, and the restart after it is too.

Console-route cases (PR 1): `TestTheConsoleSavesTheDACOfADisabledPlayerForTheNextStart`,
`TestTheConsoleSavesTheFirstDACOfAnUnboundPlayerForTheNextStart` and
`TestTheConsoleSavesTheDACAfterAFailedStartForTheNextStart` (each `restart`, the file holding the
binding, no engine built, nothing opened), and
`TestTheConsoleSaysAnAbandonedEnginesDeviceMayStillBeHeld` (a Run panicked through `servePlayerSeams.runs`:
the save made, the answer and the status saying so).

### 2.8 What a crash leaves

- **Before the save's rename**: the old binding in the file; the next start runs on it, stopped.
- **After the rename and before the directory's sync**: either binding. Each is a whole, valid file,
  and the next start runs on the one the file holds, stopped.
- **After the sync and before the rebind**: the new binding; the next start runs on it, stopped.
- **In every case**: no hold (maintenance is the engine's memory), no lease, and no half-moved seam
  (one atomic store). The queue file, the player's identity and its DSD state are untouched by a
  switch, and the DSD state is keyed by the DAC's model (`player.md:209`), so the next start applies
  the new DAC's own choice. The transport starts stopped (`internal/player/engine/doc.go:301-303`).

### 2.9 The control layer and its turn, not the console's lock

The console's three player writes hold `Server.mu` (`lockPlayerWrite`,
`handlers_player_output.go:399-407`) from their read of `before` to their answer, because they are
the only writers of `output.player` in the process (`admin-console.md:944`). That lock is private to
`admin.Server`, and the page is not the console (Carbon 3). PR 1 moves the writers rather than share
the lock:

- **One control layer** in cmd/bridge (`playerControls`, `serve_player_controls.go`) makes every
  write of the player: the player's switch, the OpenHome opt-in and the DAC (moved from
  `internal/admin`, with their checks and words), the tone, the DSD write B9 PR 3's route calls, and
  in PR 3 the page's queue and transport commands. `internal/admin`'s routes decode and guard the
  request (csrfGuard, the body, loopback) and call it through the interface they consume already
  (`admin.PlayerOutput`, `handlers_player_output.go:67-101`, grown by the writes);
  `internal/playerpage`'s routes do the same through an interface of their own. Neither package
  writes `output.player`.
- **Its turn** (a one-slot channel, tried, never queued) is held by each configuration operation from
  its read of `before` to its answer, so `before` is the player configuration its update changes:
  the guarantee `Server.mu` gave the three writes. Queue and transport commands take no turn; they
  are the engine's, which admits each whole (§7).
- **The holder's lock stays where it is.** `UpdateChecked` clones inside it
  (`internal/config/runtime.go:221-261`), so a settings PATCH, which takes no player key, and a
  player write never lose each other's fields; and each port-clash check runs on its clone in the
  turn (`openhome.md:21`; §5.4).
- **The order**: the control layer's turn, the engine's hold, the backend's writes' turn and its
  lease, then the holder's lock and bridge.yaml's, inside which the save's own reading of the box
  takes the console view's reading slot, as B601's save takes it today. Step 3's reading takes that
  slot with no lock held and lets it go. None of these is taken while a later one is held.
- **The rule moves with the lock**: `admin-console.md:944` becomes the turn's rule (§12), and its two
  tests (`TestTheOpenHomeSwitchWaitsItsTurnBehindAnotherPlayerWrite`,
  `TestAWriteBehindADACChoiceAsksBeforeStoppingThePlayItLetStart`) wait on the turn, the per-Server
  hook `testPlayerWriteWaits` becoming the control layer's seam.

## 3. The quiet PCM tone (B602)

**The one statement** (the same in §0, §4.5 and §10's row 6): *The quiet PCM tone is a 1.7 s check of
the player's DAC that the console or the page asks for: 0.25 s of digital silence, 0.6 s at 880 Hz on
the left, 0.6 s at 1320 Hz on the right, 0.25 s of silence, with 20 ms raised-cosine edges and a peak
of −30 dBFS (`bridge output pcm-test`'s ceiling, `player.md:72`); PCM only, never DSD, through the
player's binding as it resolves now, with the check after open; played only by the one rule (§2.3: a
stopped player, or the operator's go-ahead), inside the engine's hold and the backend's lease; ended
by any Stop within the sink's release bound; leaving the player stopped; recording nothing durable,
and claiming nothing but that the bridge opened the DAC and wrote the tone. It is backlog B602, B601's
remainder ("it needs a DAC reservation and an identity check the PCM harness lacks", the engineering
log, 2026-10-31), built in PR 1 for the console and the page alike.*

- **Its format**: the first of 48 kHz at 24 bits, 44.1 kHz at 24, 48 kHz at 16 and 44.1 kHz at 16
  that the DAC takes, packed by the player's §1 table (`player.md:138`), the depth's own container
  first, through the backend's own open under the lease (`Reservation.Tone`): no second packer.
- **Its frames**: a deterministic sine at the levels stated, generated in `internal/player/tone`
  (new, small), whose level ceiling is the one constant `bridge output pcm-test` keeps too.
- **Its stops**: the console's or the page's Stop (the control layer ends the tone, then sends the
  engine's Stop, which does nothing in maintenance); an OpenHome Stop (the surface is handed an
  `openhome.Engine` whose Stop ends a tone first and then calls the engine's own, PR 1's wrapper in
  `serve_player_openhome.go`, so "every Stop is the engine's own command" holds, `openhome.md:75`);
  the DAC unplugged (the sink's fault); a fault; its bound, 5 s; serve's shutdown.
- **Its answer**: `played` (every frame written and drained), `stopped` (by which Stop), a refusal by
  §2.3's words, `device_busy` or `permission_denied` where the DAC's open refuses (the holder named as
  §2.6 names it), or the binding's own state where the DAC is not there to open (`device_missing` and
  the rest of the closed list, `PROTOCOL.md:931-950`).
- **What it leaves alone**: the queue, the selection and the engine's track (`TrackCount`,
  `Details`). The element is `unavailable` (`maintenance`) while it plays.

## 4. The page

### 4.1 What it is

A small app of its own (PR 2's shell, PR 3's controls), served by `internal/playerpage` on the LAN
listener (§5): one HTML page, its script and its style, embedded (`static/[^.]*`,
`admin-console.md:75`), and a JSON API under `/api/lan/` that nothing else serves. **It is labelled as
the bridge's hardware player**: its title is the player's room, as a controller lists it
(`openhome.FriendlyName`), and its first line says that it plays on the DAC connected to the box,
apart from the console's web player, which plays in the browser. It reuses none of the console's
scripts, which call console routes. The copy a user sees outside the page (docs, release notes) waits
for the owner's OK (§11). **It offers no administration**: no switch of the player or of its OpenHome
opt-in, no restart, no passcode change, no setting of the page itself and no other setting. Those stay
on the loopback console and the CLI.

### 4.2 Browse: the catalog with hardware eligibility

- **The data is the web player's catalog**: the one cached `librarycat.Catalog`
  (`internal/librarycat/catalog.go:170-193`) the console's `/api/player/*` reads slice
  (`admin.go:2004-2029`, `handlers_player.go:254-267`), handed to the page as a function
  (`admin.Server.LibraryCatalog`, exported in PR 3), never a second index. An album's detail reads its
  rows by path from the store, as the console's does (`catalog.go:108-113`). Albums, artists, genres,
  composers and search.
- **The DTOs are the page's own.** `playerPlayabilityDTO` (`handlers_player.go:866-889`) answers for
  a browser (DSD `none`, a rendition to prefer, upstream sources included) and is not used (Codex 5).
- **Each track carries** its `ref`, the DLNA track id of the original at its path
  (`library.Reference(path)`, `dlna.TrackID("", path)`), never a rendition's id or a path; and
  `hardware`: `playable` (a PCM file of a container the player plays, or DSD at a rate the effective
  set plays natively or as DoP, with that transport's word); `converted` (a DSD rate the set converts,
  worded "plays converted to PCM, <rate>, <bits> bits", never bit-exact); or `not_playable` with a
  word: `routed` (an
  upstream's track, `librarycat.Row.RoutedUDN`), `sacd_virtual_track` (`manifest.IsSACDVirtualPath`),
  `unknown_container` (WavPack or Monkey's Audio, `formatgate.Gated`), `lossy` (MP3, AAC, Ogg, which
  the decoders refuse, `player.md:32`), `dsd_refused` with the set's word for the rate
  (`dsdPlayback.refused`), or `dsd_off`. The words are the resolver's and the policy's where they
  exist (`internal/player/library/library.go:96-126`).
- **A hint, never the gate.** Every queue edit asks `library.Resolver.Member` (`library.go:490`) of
  each `ref` before `InsertAfter`, the check an OpenHome Insert makes (`openhome.md:74`): a refused
  member is 409 `not_playable` with its word, nothing inserted. The engine's skip, with its reason, is
  the last word, and the queue shows it.
- **Renditions are not offered.** The hardware player plays the original, bit-exact for PCM: a
  track's upscaled or CarPlay renditions are neither listed nor sent.
- Cases (PR 3): `TestThePageOffersADSDTrackTheSetPlaysAndSaysHow` (native, DoP and converted rates,
  the converted one never called bit-exact), `TestThePageMarksARoutedTrackAndTheServerRefusesItsRef`
  (the ref sent anyway is refused by `Member`), `TestATrackWithRenditionsQueuesItsOriginal` (the ref is
  the original's; `Member` and the engine's open take the original's path) and
  `TestAWavPackOrAnSACDTrackIsMarkedAndRefused`.

### 4.3 The queue, the transport and seeking

- **One engine owns the queue and the state** (`player.md:115`). The page's commands go through the
  control layer to the engine's own methods; no queue is kept anywhere else.
- **Generations.** The page's state read carries the engine's generation (`Snapshot.Generation`,
  opaque to the page). Every write but Stop and Pause carries the generation the page last read, which
  the control layer passes as `IfGeneration` on its first engine command
  (`internal/player/engine/snapshot.go:99-104`; the owner checks it in the command's own step,
  `owner.go:582-583`): a mismatch is 409 `stale` with the state now, nothing changed, and the page
  reads again and says the queue changed elsewhere. Stop and Pause only make the player quieter, and
  carry none. Each answer carries the new state, so a second click uses the generation the first
  made.
- **Compound edits** (play an album now, add an album): the first step carries the generation
  (DeleteAll, or the first InsertAfter), and each later step names an entry the edit itself made
  (`InsertAfter` after the previous `NewId`, then `PlayID` of the first). A cast's DeleteAll that
  lands mid-way removes those entries, so the rest is refused (`not_found`, answered 409 `stale`: "the
  queue was replaced while the bridge was editing it"), and the steps that landed show as they are.
  Another controller's Insert between the steps does not stop the edit: its entries go where it put
  them, the last admitted command winning (C1). No engine command is added: the engine answers no
  step's generation, and its generation also moves on a backend's events (a track heard), so a chain
  of expected generations would refuse an edit for a track boundary.
- **The operations**: play now (replace the queue and play from an entry), add after the selection or
  at the end, remove, clear, play an entry, play, pause, stop, next, previous, seek to a second
  (`SeekAbsolute`; a DSD seek is the engine's refusal, `seek_unsupported`, and the page offers no seek
  where `timeSeek.dsd` is `unsupported`), and repeat. Shuffle is the engine's refusal in this release
  and is not offered.
- **Bounds**: a request's refs at most `engine.MaxEntries` (1,000, `queue.go:13`) less the queue's
  length (the engine refuses `full`), a body at most 64 KiB, a compound edit within 15 s.
- **Naming the queue**: an entry's reference is named through the resolver's own index and the
  library's row (`library.Resolver.StoredPath`, as `consolePlayer.name` names the track heard,
  `serve_player_console.go:461-491`), cached by reference; a controller's metadata is never read for
  it (`player.md:168`).
- Cases (PR 3): `TestAStaleGenerationEditIsRefusedAndChangesNothing` (an app cast through SOAP lands
  between the page's read and its write: the write is 409 `stale`, the cast's queue whole),
  `TestACastMidWayThroughAPageEditRefusesTheRest`, `TestThePageEditsTheQueueThroughTheEngine`,
  `TestThePageDrivesTheTransportThroughTheEngine` and
  `TestThePageSeeksWithinAPCMTrackAndOffersNoSeekForDSD`.

### 4.4 Signal Path and evidence

- **From GET /v1/outputs' own facts, in-process** (`bridgeOutput.facts`,
  `serve_player_output.go:160-188`), never a reading of the page's own: the element's state, reason
  and lastError; the DAC; `dsdPlayback`, `dsdDAC` and `playback`; and the console's
  `permission_denied` (`admin-console.md:951`).
- **Truthful by construction.** The page words each transport from `playback.transport` and the plan:
  PCM "bit-exact: the samples reach the DAC as the file holds them" (`player.md:31`); DoP "DSD over
  PCM (DoP)"; native "native DSD"; converted "converted to PCM: <rate>, <bits> bits, <passband>,
  <dither>", from the plan alone (`player.md:226`), **never "bit-exact" or "lossless"**; and the
  evidence in the words of the B728 design's §1.3, with no "listener confirmed" until B12. An
  observation is shown only for the session the engine plays now, so after a switch none shows until
  something plays on the new DAC.
- **No reading a second.** The DAC's projection is read once per hotplug generation and per switch,
  never per poll: the page polls its state every second while it is visible (the queue's entries only
  when the generation moved), and a box reading a second would be the page's cost alone.
- Cases (PR 3): `TestThePageNeverCallsAConvertedOccurrenceBitExact` (the served words, and a sweep of
  the page's script, comments stripped, for the words beside `converted`),
  `TestASwitchShowsNoObservationOfTheDACItLeft` and `TestThePageSaysWhatTheElementSays` (every state and
  reason `playerState` gives, `serve_player_output.go:382-412`, worded, none missing:
  `bridgeOutputReasons`' closed list).

### 4.5 The switch, the tone and DSD on the page

- **The switch**: the DAC list is the console's (`consolePlayer.DACs`: one bounded reading, selection
  tokens kept server-side, no serial but its label); a choice is §2.4's switch, asked in the server's
  words on `would_interrupt`.
- **The tone**: §3's one statement.
- **DSD, only through B9 PR 3's write and B13's controls.** The control layer's DSD write is the
  function B9 PR 3's route calls (§2.3: the hold at a stopped generation, then
  `playback.Backend.ChooseDSD`), with the page's generation. The page's DSD panel is B13's console
  panel: each mode's option from `dsdDAC.options`; the first-use sheet in B13's words (the B728
  design's §6.1: turn the amplifier or the headphones down; a large Stop stays on screen); the DoP
  confirmation for a DAC with no DoP claim (its §7.4); and the write carries `firstUseAcknowledged`
  and `confirmDoP` only from them. The server refuses either missing (`first_use_unacknowledged`,
  `dop_confirmation_required`, in `policy.Choose`), whatever the page sent.
- **The large Stop**: while a transport's first use is acknowledged and not yet observed
  (`dsdDAC.firstUse`), the now-playing view shows a large Stop, the engine's Stop.
- **The chime is B12's.** Until B12 lands the page offers no check, no answer can be recorded, and
  "listener confirmed" never shows.
- Cases (PR 3): `TestThePagesDSDWriteNeedsTheFirstUseAcknowledgement` (once B9 PR 3 lands: native, and
  DoP on a DAC with no DoP claim, each without its acknowledgement or confirmation, refused with the
  state's revision unchanged; with them, written and read back), and
  `TestThePageShowsTheLargeStopUntilFirstUseIsObserved` (once B13 lands).

### 4.6 The page's routes

| Route | What it does | Session | Generation |
|---|---|---|---|
| `GET /` | the page; the login form; or "no passcode is set", with no form | no | no |
| `GET /static/*` | the page's script, style and icons | no | no |
| `POST /api/lan/login` | a passcode for a session | no | no |
| `POST /api/lan/logout` | ends this session | yes | no |
| `GET /api/lan/state` | the player now and the last operation; the queue when `since` differs | yes | no |
| `GET /api/lan/library/*` | albums, an album, artists, an artist, genres, composers, search: the catalog with hardware eligibility | yes | no |
| `GET /api/lan/artwork/*` | cover art, the console's artwork reads | yes | no |
| `POST /api/lan/queue` | play now, add, remove, clear | yes | yes |
| `POST /api/lan/transport` | play, pause, stop, next, previous, play an entry, seek, repeat | yes | yes, but stop and pause |
| `GET /api/lan/dacs` | the DAC list, with selection tokens | yes | no |
| `POST /api/lan/dac` | the switch | yes | yes |
| `POST /api/lan/tone`, `DELETE /api/lan/tone` | the tone, and its stop | yes | yes, the POST |
| `PUT /api/lan/dsd` | the DSD write | yes | yes |

Anything else is 404. Every mutation is JSON, which forces a CORS preflight the page never answers,
and carries the page's Origin (§5.3).

## 5. Browser access

### 5.1 The listener: the OpenHome surface's binding and admission, shared

- **One IPv4 socket per served LAN address**, the addresses `lan.Look` serves (up and running,
  multicast, not loopback or point-to-point, an RFC 1918 or 169.254 address, no Tailscale or CGNAT
  address: `player.md:100`), followed every 5 s; never a wildcard, and never on DLNA's, the API's or
  the console's listener (`openhome.md:22`).
- **Shared, not copied.** PR 2 moves the OpenHome surface's binding and admission into
  `internal/player/lanlisten`: the sockets for each address and their following (the listing,
  `bind` at `internal/player/openhome/listener.go:541`, retirement), the gate (`:787-878`, admission
  at `:804-849`) with its source rule, the restriction to the arrival interface
  (`restrict_linux.go:21-35`: SO_BINDTOIFINDEX, whose effect B527 measures) and the shape of the
  limits. The OpenHome surface keeps its SSDP and its SOAP on top and its tests as they are: the move
  is PR 2's first commit, with the OpenHome suite green before and after. The page is a second user
  of `lanlisten`, with a port and limits of its own.
- **Admission at accept, before anything else**: the connection's own address served now, and a
  source inside the on-link prefix of the address it arrived on, of the LAN's classes, never loopback
  and never one of the box's own (`openhome.md:23`). A refused connection is closed at once, unread
  and unanswered, before any TLS byte, counted against nothing, and logged once per kind and source
  (32 at most). Then the TLS handshake, then HTTP.
- **Limits**: 32 connections and 8 per source, counted after admission; 8 request slots, 503 at once
  past them; deadlines of 5 s for the header and the handshake, 15 s for the request, 30 s for the
  write (a catalog page) and 60 s for an idle connection; 16 KiB of headers; 64 KiB of body.
- **Its stop** refuses every request (503), closes the listeners, shuts the servers down within a
  grace (2 s), and closes what is still open. Serve stops it first in the player's teardown, before
  the OpenHome surface and the engine (§2.5).

### 5.2 HTTPS on the bridge's certificate

- **The pair serve loaded** (`servertls.LoadOrGenerateWithOptions`, `cmd/bridge/main.go:3775`;
  `internal/tls/tls.go:132-174`): the certificate the API presents and the phone pins. Nothing mints,
  rotates or reloads it for the page; `bridge cert rotate`, with serve stopped, changes it for the
  page too. TLS 1.2 at least; ALPN h2 and http/1.1. The server's `ErrorLog` is handshakelog's
  (`handshakelog.Wrap`), so a failed handshake's line names no client
  (`TestEveryServeHTTPServerRedactsPeerAddresses` finds every `http.Server`); this round's probe showed
  that net/http's own line for a plain-HTTP request names one.
- **Plain HTTP** to the page's port gets net/http's fixed `HTTP/1.0 400 Bad Request` with "Client sent
  an HTTP request to an HTTPS server.", and a close: no page, no form, no cookie, and the handler never
  runs (this round's probe). There is no redirect: a redirect sent over plain HTTP is a page an
  attacker on the path rewrites.
- **No HSTS** (`auth-tls.md:749`): the page is reached at a LAN name or address the browser may reach
  other services at.
- **The first visit.** The browser warns that the certificate is not trusted, since it is
  self-signed, and may say that it does not name the address used. The console's Player page shows the
  URLs the page listens at (the box's `.local` name where mDNS is on and the certificate names it,
  then each address bound) and the certificate's SHA-256 fingerprint, the one `bridge cert info`
  prints; the user compares it with the one the browser's certificate viewer shows, and goes on only
  on a match. The page itself shows no fingerprint: a page an attacker serves can show any.
- **The alternatives** that lost are in §13. None keeps every constraint the brief sets.

### 5.3 Host, Origin and the headers

- **Host**: the address and port the request arrived on, or a DNS name the served certificate names
  (the `.local` name among them, `localhost` and every other loopback name excepted) with that port.
  Anything else is 421 and logged once per name (16 at most), as the console's `loopbackHostOnly` does
  (`admin.go:2650-2732`). An empty Host is 400: a request over HTTP/1.1 or h2 always carries one.
- **Mutations**: `application/json` only; an `Origin` that is present and is `https://` and the
  request's Host, refused if missing or mismatched (every browser sends it on a POST, a PUT or a
  DELETE the page's own script makes); and the session cookie. The login is a script's JSON POST,
  never a form: a form POST from a page served under `no-referrer` carries `Origin: null`
  (`auth-tls.md:319-344`).
- **Headers on every answer**: `Content-Security-Policy: default-src 'self'; img-src 'self' data:;
  frame-ancestors 'none'; base-uri 'none'; form-action 'none'`, `X-Content-Type-Options: nosniff`,
  `Referrer-Policy: same-origin`, and `Cache-Control: no-store` on every API answer; no CORS header.
- **The cookie**: `__Host-bridge-page`, `Secure`, `HttpOnly`, `SameSite=Strict`, `Path=/`, no
  `Domain`, its `Max-Age` the session's remaining life.

### 5.4 The settings: schema, port, clash, managedSettings, on and off

- **Schema**: `output.player.lanPage: {enabled, port}`, inside the player block
  (`internal/config/player.go:108-163`), written only when set. `enabled` is off by default. `port` is
  nil for `DefaultLANPagePort`, 7793 (after the API's 7788, the console's 7789, DLNA's 7790, the
  loopback track route's 7791 and OpenHome's 7792, `player.go:248-252`), or 0 for a port the kernel
  picks for each address (tests), validated as `openhomePort` is: a word, a list or a fraction leaves
  the block unread, and a number outside 0 to 65535 is `Problem`'s refusal.
- **Why inside the block**: an older release's player reader refuses a key it does not know and turns
  the player off with a logged reason (`player.md:90`), where a key beside the block under `output`
  would fail the whole load. **The residual**: rolled back to a release before PR 2 with the page on,
  the player is off until the key is removed, and that release's line names the key.
- **Port clash**: the page's port may not be one serve binds now (the boot snapshot's `listenAddress`,
  `adminAddress`, `dlna.listenAddress` and `output.loopbackPort`, and `openhomePort` while the surface
  runs), nor one the live config would bind at the next start. The console's switch refuses a clash
  (409 `lanpage-port-clash`, naming the setting and never the port), the settings save refuses moving
  a listener onto the page's port while the page is on, and a clash found at a start leaves the page
  off, not the player, said once.
- **Public mode and 1-bit Cloud**: never. A public bridge, a hosted tenant included, starts no
  listener and no goroutine of the page's (`Config.IsPublic()`, as the player is refused,
  `player.go:643-645`), and the console's routes for it answer 403 `player_not_available`.
- **managedSettings**: `playerLANPage`, which a managed `playerEnabled` owns too
  (`managedSettingImplies`, `config-lifecycle.md:67-82`): the console hides the page's panel and its
  routes refuse it (403 `managed-setting`), the passcode's included.
- **On and off**: the console's Player page (`POST /api/output/player/lanpage {enabled}`), through the
  control layer and the holder. **Every field is read live**, by one consumer: serve's page supervisor
  (`cmd/bridge/serve_player_page.go`) follows the holder's `Changed` and starts, stops or rebinds the
  listener, so no field's halves split (`config-lifecycle.md:139-149`). Turning it off stops the
  listener within its bound and ends every session. The supervisor runs the page only while the live
  config enables the player on a host that runs it (`PlayerEnabledAsBooted`, `player.go:626-652`) and
  serve holds its data directory's lock; it needs no running engine (§2.7). No CLI command writes the
  switch: a CLI write of bridge.yaml beside a running serve makes every console save after it refused
  (`config-lifecycle.md:277`). The page is turned on from the console, or by an edit of the file
  while serve is stopped.

## 6. The passcode

### 6.1 Its home, and why not bridge.yaml

**`<dataDir>/player-page-passcode.json`, mode 0600**: `{"version": 1, "id": "<128 random bits, hex>",
"hash": "<bcrypt, cost 12>", "setAt": "<RFC 3339>", "setBy": "console" or "cli"}`, where an empty
`hash` is no passcode. Not in bridge.yaml (Carbon 6), for three reasons:

- **A CLI change there would not be picked up**: a running serve's holder reloads nothing, and a CLI
  write of bridge.yaml beside it makes every console save after it refused (`ChangedOutsideError`,
  `config-lifecycle.md:277`).
- **The guard that keeps an unreadable `output.player` as written**: a player block that does not
  read is kept as the file held it, and nothing may set a field on it (`Unread`,
  `player.go:468-477`; bind refuses it, and the console answers `player_block_unreadable`), so a
  passcode kept in that block could neither be changed nor checked while the block is unread; and a
  key an older release does not know turns its player off.
- **A credential would ride every copy of the config** (a backup, a support bundle, a restore), and
  widen what a leaked config costs beyond the serial it holds now.

**Kept as `adminauth.json` is kept** (`auth-tls.md:147-185`). Two processes write it, serve's console
and the CLI, so every write re-reads the file just before its rename and builds again from a fresh read
when its bytes changed (`commitLocked`'s rule), keeps the owner of the file it replaces (`KeepOwner`,
`config-lifecycle.md:621`), and stages, syncs, renames and syncs its directory. **The credential is the
file's, the sessions are serve's**: every login and every session check reads the file again whenever
a stat says it changed (its size, its mtime, `os.SameFile`: `refreshIfChangedLocked`'s rule), so a CLI
change reaches a running serve at its next request. A file that cannot be read refuses every login and
every session (503 `passcode_unreadable`), logged once a state; a missing file is no passcode. Backups
do not carry it: a restore leaves the box's own, and a passcode is set again in seconds.

### 6.2 Set, change, reset when forgotten, clear

- **Set or change**: on the console's Player page (`POST /api/output/player/lanpage/passcode`, loopback,
  JSON, typed twice on the page), or `bridge output passcode set` (asked twice, without echo; `--stdin`
  reads one line for a script). Either may generate one instead: 12 characters from an alphabet with no
  look-alikes, shown once.
- **Its rules**: 8 to 64 characters, at most 72 bytes in UTF-8 after NFC normalization (bcrypt's limit,
  which this round's probe measured), no control characters. A passcode is compared after the same
  normalization (`golang.org/x/text/unicode/norm`, which the bridge imports already), so one typed on
  another platform's keyboard matches.
- **Reset when forgotten**: set a new one. The old one is never asked for, since the console and the CLI
  are trusted already (loopback; a shell on the box). The page offers no recovery, and no change of
  the passcode either: it is set on the console or with the CLI alone.
- **Clear**: the console, or `bridge output passcode clear`, writes the record with no hash; the page
  then shows that no passcode is set, with no form.
- **Sign every browser out, the passcode kept**: the console, or `bridge output passcode sign-out`,
  writes a new `id` with the hash kept.
- **Every rotation ends every session**: a set, a clear and a sign-out each write a new `id`, and a
  session made under another `id` is refused at its next request (§6.3). The CLI asks
  `refuseDataDirAway` first (`cli-serve.md`'s B727 bullet), takes no lock of the data directory, which
  serve holds, and says that the passcode is set and every browser signed out, never the passcode;
  `bridge output passcode status` says whether one is set, since when and by what, never its length.

### 6.3 Sessions

- 256 random bits in the cookie; the server keeps their SHA-256, in memory alone, with the passcode
  `id` each was made under, compared in constant time; at most 64, the oldest dropped; 7 days from the
  login at most.
- A session ends at its logout, its 7 days, a set, clear or sign-out (another `id`), the page turned
  off, and serve's restart (memory alone). Persisting them was declined (§13): a restart costs one
  passcode entry.

### 6.4 Lockout bounds

- **Per source**: `adminauth.RateLimiter.AllowAndReserve` (`internal/adminauth/ratelimit.go:263`),
  keyed by the source's IPv4 address and `player-page`: 5 failures in 15 minutes, then every attempt is
  answered after the limiter's 5 s with the refusal a wrong passcode gets (no 429, nothing that says a
  throttle); a success clears the source's bucket (`RecordSuccess`), and the reservation is the failure
  count (`auth-tls.md:412-414`).
- **Bridge-wide**: one passcode check at a time (bcrypt at cost 12, a turn of one), waited for at most
  10 s and then refused as above. Many sources together get at most a few checks a second on a Pi, and
  eight characters even from a lowercase alphabet take centuries at that rate.
- **No permanent lockout**: a lockout every host on the LAN could set would be a lever against the
  owner, and the console and the CLI always set a new passcode.
- The console shows the refused logins since the start and the last refusal's time; the source is in
  the log, not on any page.

### 6.5 What pages and logs may say

- **Never, anywhere**: the passcode, its hash, a session token or its hash, the cookie's value, a
  request's Cookie header. No answer but the login's sets the cookie, and none echoes a passcode.
- **A refused login** is one Warn line naming the source address (canonical, no port) and the reason's
  word (`wrong`, `throttled`, `none_set`, `unreadable`), once per source a window, and nothing else of
  the request. That makes a second family of lines that name a client, beside the console's failed
  logins (`auth-tls.md:786-801`), so PR 2 adds its bullet there.
- **The page's refusal** is one sentence for every case ("That passcode was not accepted."), never how
  many tries are left.
- **The console** shows whether a passcode is set, since when and by the console or the CLI, never its
  length.
- **Error bodies** carry fixed messages, never an error's text or a path (`auth-tls.md:802-823`).
- **The passcode covers this page alone**, and the page says so on its login and in its about panel:
  OpenHome controllers on the LAN, the 1-bit app's casting among them, need none, as before.

## 7. Coexistence: the app, a controller and the page at once

| Surface | How it writes | Its guard | A stale or losing write is answered |
|---|---|---|---|
| The 1-bit app's cast | SOAP: DeleteAll, Insert, SeekId (`internal/player/openhome/writes.go:112-146`) | none: the last admitted command wins (C1) | each command as the engine answers it, in arrival order |
| Another OpenHome controller | SOAP | none | the same |
| The app's DSD write (B9 PR 3) | `PUT /v1/outputs/{id}/dsd`, through the control layer | §2.3, and the DSD state's revision | `playback_not_stopped`, `stale`, `configuration_busy` |
| The page | the control layer | its generation on the first engine command; the turn for a configuration operation | 409 `stale` with the state now; `configuration_busy` |
| The console | the control layer | the turn; its generation for the switch, the tone and DSD | the same |

- **Which wins.** The engine's owner takes every command in arrival order and runs each whole
  (`player.md:115`). SOAP's commands carry no generation (`writes.go:119`, `:144`), so a controller's
  last admitted command wins, as today. A page's command wins only where nothing visible changed since
  the page read the state; otherwise it changes nothing and says so. While a configuration operation
  runs, every surface's mutation is refused (SOAP's fault 501, the pages' 409 `configuration_busy`), and
  a Stop answers and does nothing.
- **What each shows.** One engine snapshot, read by all: the page every second (the queue only when the
  generation moved), the console every 5 s, the app through its OpenHome polling (IdArrayChanged's
  token, Time, Info) and GET /v1/outputs. A page whose write was refused stale reads again and says
  "The queue changed since this page read it (the 1-bit app or another controller changed it). Nothing
  was changed." While an operation runs, the element is `unavailable` with `maintenance` (or
  `binding_changed` for a switch's first moments, before the new DAC is read), and both pages say which
  operation runs.
- **With the browser and the phone closed**, playback is the engine's and the backend's: nothing the
  page or the app holds is needed for the next track, a pause's release or a stop.

## 8. PROTOCOL.md

- **PR 1 rewords three passages, and is a Mirror-PR pair**: the iOS repo copies the file,
  `docs/BridgeProtocol.md`, and moves `BridgeOutputReadingTests.test_protocolMirrorSHA256`'s pin.
  1. `PROTOCOL.md:925`: "`dac` describes the one card resolved for the player's boot binding" becomes
     "the one card resolved for the player's binding: the one it started with, or the one a DAC switch
     on the console or the player's page moved it to since".
  2. `:940`: `binding_changed`, "`output.player.device` names another DAC than the one it started
     with", becomes "the player is moving to another DAC (a switch is under way) and has not read the
     box for it yet".
  3. `:1034`: "as the boot binding names it" becomes "as the player's binding names it, or a binding it
     has run on since it started".

  No member, word, route, flag or `ProtocolVersion` changes. The element's `id` is the player's UUID
  (`:903`), so a switch keeps it, and its `dac`, `dsdPlayback` (a new `policyGeneration`) and `dsdDAC`
  (another `dacKey`) change as a hotplug change already changes them. The iOS side checks its own words
  for `binding_changed` and any assumption that `dac` is fixed for a serve run (§14, question 4).
- **PR 2 and PR 3 change none**: no `/v1` route (O1: the app never switches the DAC, and O2 is
  deferred), no member, no `/v1/health` flag (the page is not the app's), and the page's `/api/lan/*`
  is its own, as the console's `/api/*` is not the app's contract.

## 9. The PRs

**Common runs**, for each PR, in the build slot: `-race -count=50` on each new case, five lanes at most
(half the cores); `go vet` with the Makefile's tags for linux/amd64 and linux/386 (CI's 32-bit vet), and
for windows/amd64 and darwin/arm64 on the touched packages; each touched package with `go test -race
-timeout 120m`; the pinned gofmt on the touched files; `make test`, `make build-all` and `make
docs-check`. **Controls**: each fix reverted as an uncommitted edit, run with the cases it names,
restored from memory, `git status` clean after. **Rules**: each PR's rules patch (§12) moves long forms
out where a file is near its budget (`player.md` 99,798 bytes, `config-lifecycle.md` 99,932,
`cli-serve.md` 97,290, of 100 KB), with `scripts/doc-split/move_entry.py`.

### PR 1: the shared controls, the live switch and the tone (B602)

- **Scope**: §2, §3 and the console's Player page.
- **Files**: `cmd/bridge/serve_player_binding.go` (new: `playerBinding`, `seamLocator`);
  `cmd/bridge/serve_player_controls.go` (new: `playerControls`, its turn, the phases, `enterStopped`
  with the renewer, the switch, the tone, the player's switch and OpenHome writes moved from
  `internal/admin`, and the entry B9 PR 3's route calls); `cmd/bridge/serve_player.go`,
  `serve_player_dsd.go`, `serve_player_console.go`, `serve_player_output.go`,
  `serve_player_output_dac.go`, `serve_player_openhome.go` (the Stop wrapper), `doctor_player_dac.go`
  and `main.go` (R1 to R12, the wiring, the teardown's order); `internal/player/playback/reserve.go` and
  `tone.go` (new: `Backend.Reserve`, the writes' turn and B9 PR 2's lease for the switch and the tone;
  `Reservation.CheckTarget`, `Reservation.Tone`, `Reservation.Release`); `internal/player/tone` (new);
  `internal/admin/handlers_player_output.go`, `admin.go`, `static/app.js` and `templates/output.html`
  (the switch's answers and refusals, the tone's button, the go-ahead asked in the server's words, and
  the status's `running.generation`, which the page sends back; `lockPlayerWrite` retired with its
  hook); `PROTOCOL.md` (§8); `ops/settings-apply-semantics.md` (the
  DAC row live while the player runs, the tone); `ops/engineering-log.md`; the rules patch (§12).
- **Guarded paths**: none planned (`internal/player/qualification/version_gate.go:20-36`; `reserve.go`
  and `tone.go` are new files beside `native.go` and `edges.go`, which it does not change). CI's
  `CheckVersion` decides, and the version moves if the PR finds it must touch one.
- **Depends on**: B9 PR 2 (#1453) merged (the lease, `ChooseDSD`, the writes' turn); B9 PR 3's engine
  reservation decided as the hold (§14, question 2). Not B12 or B13.
- **Red first**, through the console's route and serve's player, each red on the PR's base:
  `TestTheConsoleSwitchesTheRunningPlayerToAnotherDAC` (on the base: `restart`, the output withheld
  `binding_changed`, the next Play opening A); `TestASwitchWhilePlayingNeedsTheGoAheadAndEndsStopped`
  (on the base, with the go-ahead: saved and withheld, no switch); `TestASwitchCompletesStopped` (from
  playing, paused, and paused with the device released: stopped, no session, the new DAC's report
  taken); `TestASwitchToABusyDACNamesItsHolderAndLeavesItAlone`,
  `TestASwitchToADACHeldByAnUnnamedProcessSaysSo` and
  `TestASwitchToADACThatBecameBusyUnderTheHoldIsRefused` (on the base, saved with no look);
  `TestASelectionThatWentStaleUnderTheHoldIsRefused` (on the base, refused before anything stops, so
  its "the go-ahead's stop said" is red); `TestASwitchToTheSameDACUnderAnotherKindIsNotItsOwnHolder`;
  `TestSOAPWritesAreRefusedThroughoutASwitch` and
  `TestASwitchStalledPastTheMaintenanceTTLStillRefusesSOAPWrites` (no hold on the base);
  `TestASwitchIsRefusedWhileADSDWriteOrAProbeHoldsTheBackend`; `TestASwitchedDACUnpluggedAndReplugged`
  (stopped, unavailable `device_missing`, available again, nothing played); `TestASwitchSurvivesARestart`
  (live now, and the next serve of the data directory on B, stopped, its queue kept);
  `TestEveryReaderOfTheBindingFollowsASwitch`; `TestTheBindingIsReadThroughTheSeamAlone`;
  `TestTheConsoleSaysAnAbandonedEnginesDeviceMayStillBeHeld`;
  `TestTheToneIsQuietPCMAndLeavesThePlayerStopped` (the stand-in DAC's peak at −30 dBFS, left then right,
  PCM, the transport stopped, the queue's generation as it was); `TestEveryStopEndsTheTone` (the
  console's, the page's control call, an OpenHome Stop through the surface, an unplug, the bound, and
  shutdown); and `TestABrowserThatLeavesMidSwitchCancelsNothing`,
  `TestAShutdownDuringASwitchLeavesTheFileOrTheBindingWhole`.
- **Kept green** (true on the base, and to stay so): B601's stale-selection cases,
  `TestAChoiceOfACardThatChangedSinceTheListIsStale` and `TestADACChoiceIsCheckedAgainWhenItsTurnComes`,
  now through the switch's steps 3 and 7; the three no-engine console cases of §2.7; and B601's and
  B612's console tests, which wait on the turn in place of the console's lock. B601's
  `TestTheConsoleChoosesAnotherDACAndBack`, which pins the withholding a choice makes today, is
  rewritten to switch and switch back.
- **Controls**: the hold skipped (the SOAP cases red); the renewer removed (the stalled save red); the
  target's open skipped (the busy-under-the-hold case red), and the /proc look skipped (the busy case's
  "not stopped" assertion red); the seam not moved (every switch case red); the locator back on the boot
  binding (the follows case and the structural case red); the publication's wait removed (the
  completes-stopped case's "the new DAC's report taken" red); the go-ahead ignored (the go-ahead case
  red); the turn removed (the DSD-write case red); the tone at −20 dBFS (the tone case red).
- **Tier**: player (standard, and CI's native-qualification job); hardware (the owner's Pi with two USB
  DACs and MPD holding one: each switch both ways, stopped and while playing with the go-ahead; the busy
  refusal naming MPD while MPD plays on; the tone on each DAC; an unplug and a replug of the new DAC; a
  restart); mirror (§8).

### PR 2: browser access

- **Scope**: §5 and §6, and the page's shell: the login, the logout, and the state shown read-only.
- **Files**: `internal/player/lanlisten/` (new: moved from `internal/player/openhome/listener.go` and its
  `restrict_*.go`, with their tests; `internal/player/openhome` uses it); `internal/playerpage/` (new: the
  server, with `lanlisten`, TLS after admission, Host, Origin, the headers, the limits and the stop; the
  passcode's store, `passcode.go`; the sessions, `session.go`; the login with the limiter; the routes PR
  2 serves, `/`, `/static/*`, login, logout and state; `static/`, the shell); `internal/config/player.go`
  (`lanPage`: the reader, `Problem`, `EffectiveLANPagePort`, the clash) and the managed setting
  `playerLANPage` with its implication; `cmd/bridge/serve_player_page.go` (new: the live supervisor, and
  its wiring to the pair serve loaded, the control layer and the state's facts); `main.go` (started after
  the console, stopped first); `cmd/bridge/output_passcode.go` (new: `bridge output passcode
  set|clear|sign-out|status`); `internal/admin/handlers_player_page.go` (new), `admin.go`,
  `static/app.js` and `templates/output.html` (the page's panel: switch, port, URLs, fingerprint,
  passcode, sign-out, refused logins; the settings save's clash refusal); `ops/settings-apply-semantics.md`,
  `ops/engineering-log.md`, the rules patch (§12).
- **Depends on**: PR 1 (the control layer the page reads its state through). B527, which measures
  SO_BINDTOIFINDEX on the Pi, does not block: the page's admission rests on the source check at accept,
  as the surface's does.
- **Security cases, each a working authorised HTTPS request beside its refusal, each control that one
  guard removed with the listener kept** (Codex 6; not "red on the base", where every refusal holds
  trivially):
  - `TestTheLANListenerServesNoAdminRoute`: a session's `GET /api/lan/state` is 200 over HTTPS; every
    pattern the console's mux registers (`admin.Routes`, the list `Handler` registers from, exported for
    the test), asked with the session and without, is 404. Control: the console's handler mounted as the
    page's fallback: red (403 and 200, not 404).
  - `TestAPlainHTTPRequestGetsNoPageFormOrCookie`: HTTPS `GET /` is 200 with its login form; plain HTTP to
    the same port gets the fixed 400, no `Set-Cookie`, no `<form`, no `<html`, and no session is made.
    Control: the listener serving plain HTTP beside TLS (a sniffing listener): red.
  - `TestThePasscodeTravelsOnlyOverTLS`: the login over HTTPS sets the cookie with `Secure`, `HttpOnly`,
    `SameSite=Strict` and its `__Host-` name; the same POST in plain HTTP makes no session. Control: the
    `Secure` flag dropped: red.
  - `TestASourceOutsideTheOnLinkPrefixIsRefusedAtAccept`: on lantest's loopback box, as the OpenHome
    suite's admission tests run, a source in the prefix logs in and reads; one outside it is closed before
    any TLS byte and counted against nothing. Control: the source check skipped for the page: red.
  - `TestAPublicBridgeStartsNoPageListener`: a loopback serve with the page on binds and answers; the same
    config in public mode binds nothing, and the console's switch is 403. Control: the public check dropped
    from the page's plan: red.
  - `TestARequestNamingAnotherHostIsRefused` (421) and `TestAMutationWithAnotherOriginOrNoneIsRefused` (403
    `cross_origin`, nothing changed), each beside the same request with the right Host or Origin answered.
    Control: each check dropped alone: red.
  - `TestAPasscodeChangeEndsEverySession`: two sessions read; `bridge output passcode set` (the CLI's code
    path, writing the file as another process does); both are refused at their next request, and the new
    passcode logs in; then again through the console's route. Control: the session's `id` not compared:
    red.
  - `TestTheLoginIsThrottledPerSourceAndNeverLocksOut`: five wrong, then the right one refused after the
    delay; another source logs in; the first logs in after the window; a success clears it. Control: the
    limiter skipped: red.
  - `TestAPasscodeIsNeverLoggedOrEchoed`: `loggingtest`'s recorder at every level, the refusal's line
    present and the secret absent (`build-ci-tests.md`'s rule).
  - `TestThePageIsOffByDefaultAndFollowsItsSwitchLive`, `TestThePagesPortClashIsRefusedAndNamesTheSetting`,
    `TestAManagedPlayerSwitchOwnsThePage` and `TestASnapshotCarriesNoPagePasscode` (backup's sources
    are named one by one, `internal/backup/backup.go:170-188`, and stay so).
- **Structural**: `TestThePageServesItsOwnRoutesAlone` (the page mux's patterns are the allowlist, none
  outside `/`, `/static/` and `/api/lan/`), `TestThePagePackageImportsNoConsoleOrAPI`,
  `TestThePagesAssetsNameNoConsoleRoute` (comments stripped), and the OpenHome suite whole after the move.
- **Tier**: standard. The owner's LAN check on the Pi (a phone's and a laptop's browser reach the page by
  the `.local` name and by address; the warning answered by the fingerprint; plain HTTP answered 400) runs
  with PR 3's hardware run, or before it.

### PR 3: the page plays; coexistence; the acceptance run

- **Scope**: §4 (browse, the queue, the transport, seeking, Signal Path, the switch's and the tone's
  controls on the page, DSD), §7 and §10.
- **Files**: `internal/playerpage/` (the routes, and the page's interface);
  `internal/admin/handlers_player.go` (`LibraryCatalog` exported for the page's catalog);
  `cmd/bridge/serve_player_controls.go` (the queue and transport commands, compound edits, naming);
  `cmd/bridge/serve_player_page.go` (the wiring); `ops/engineering-log.md`; the rules patch (§12).
- **Depends on**: PR 1 and PR 2. Its DSD rows depend on B9 PR 3 (the write and the function its route
  calls) and B13 (the console's DSD panel and its first-use sheet, whose words and view the page takes);
  if either is late, PR 3 ships without the DSD rows and a PR 3b adds them. B12, the chime: deferred
  (§11).
- **Red first** (the routes are 404 on the base): §4.2 to §4.5's cases;
  `TestThePageTheAppAndAControllerAtOnce` (an app cast through SOAP, a second controller's Insert and the
  page's edit in one test: the cast's queue whole, the controller's entry where it put it, the page's stale
  edit refused, every surface reading one snapshot); `TestPlaybackGoesOnWithNoPageOrPhone` (a session
  started from the page; every page connection and the app's closed; the next track heard, a pause's
  release and a stop all go on); `TestThePagesDSDWriteNeedsTheFirstUseAcknowledgement` (once B9 PR 3
  lands); `TestThePageSwitchesTheDAC`; and
  `TestThePageIsLabelledTheBridgesHardwarePlayerAndSaysWhatThePasscodeCovers` (structural).
- **Controls**: the first step's generation dropped (the stale case red); the membership check skipped
  (the routed case red); a converted rate worded with PCM's sentence (the bit-exact case red); the
  observation shown whatever its session (the switch's observation case red).
- **Tier**: standard; hardware: the owner's acceptance run on the Pi (§10).

### Order and dependencies

B9 PR 2 (#1453), then PR 1, then PR 2, then PR 3; B9 PR 3 and B13 before PR 3's DSD rows; B12 after it
all. PR 2's move into `lanlisten` can be written beside PR 1, and merges after it.

## 10. The acceptance list

| # | B634 closes when | PR | Proven by | Tier |
|---|---|---|---|---|
| 1 | a browser alone browses the bridge's own library | 3 (2: the listener) | `TestThePageOffersADSDTrackTheSetPlaysAndSaysHow`, `TestThePageMarksARoutedTrackAndTheServerRefusesItsRef`, `TestATrackWithRenditionsQueuesItsOriginal` | standard; Pi |
| 2 | ... edits the queue | 3 | `TestThePageEditsTheQueueThroughTheEngine`, `TestAStaleGenerationEditIsRefusedAndChangesNothing` | standard; Pi |
| 3 | ... drives the transport and seeks | 3 | `TestThePageDrivesTheTransportThroughTheEngine`, `TestThePageSeeksWithinAPCMTrackAndOffersNoSeekForDSD` | standard; Pi |
| 4 | ... reads Signal Path and the evidence | 3 | `TestThePageSaysWhatTheElementSays`, `TestThePageNeverCallsAConvertedOccurrenceBitExact`, `TestASwitchShowsNoObservationOfTheDACItLeft` | standard; Pi |
| 5 | ... switches the DAC live and durably (Stop-and-select) | 1; the page's control 3 | `TestTheConsoleSwitchesTheRunningPlayerToAnotherDAC`, `TestASwitchWhilePlayingNeedsTheGoAheadAndEndsStopped`, `TestASwitchSurvivesARestart`, `TestThePageSwitchesTheDAC` | player; Pi |
| 6 | ... plays the quiet PCM tone: §3's one statement | 1; the page's control 3 | `TestTheToneIsQuietPCMAndLeavesThePlayerStopped`, `TestEveryStopEndsTheTone` | player; Pi |
| 7 | ... chooses DSD with the first-use sheet and the large Stop; the chime once B12 lands | 3, once B9 PR 3 and B13 land | `TestThePagesDSDWriteNeedsTheFirstUseAcknowledgement`, `TestThePageShowsTheLargeStopUntilFirstUseIsObserved` | standard; Pi |
| 8 | a stale selection is refused | 1 | `TestAChoiceOfACardThatChangedSinceTheListIsStale`, `TestADACChoiceIsCheckedAgainWhenItsTurnComes` (B601's, kept through the switch), `TestASelectionThatWentStaleUnderTheHoldIsRefused` | standard |
| 9 | maintenance excludes SOAP mutations | 1 | `TestSOAPWritesAreRefusedThroughoutASwitch`, `TestASwitchStalledPastTheMaintenanceTTLStillRefusesSOAPWrites` | standard |
| 10 | a selection completes stopped | 1 | `TestASwitchCompletesStopped` | standard; Pi |
| 11 | a busy device, its holder named | 1 | `TestASwitchToABusyDACNamesItsHolderAndLeavesItAlone`, `TestASwitchToADACHeldByAnUnnamedProcessSaysSo` | standard; Pi, with MPD |
| 12 | unplug and replug | 1 | `TestASwitchedDACUnpluggedAndReplugged` | standard; Pi |
| 13 | restart | 1 | `TestASwitchSurvivesARestart` | standard; Pi |
| 14 | browser, app and OpenHome at once | 3 | `TestThePageTheAppAndAControllerAtOnce` | standard; Pi |
| 15 | playback goes on with the browser and the phone closed | 3 | `TestPlaybackGoesOnWithNoPageOrPhone` | standard; Pi |
| 16 | the display is truthful | 3 | row 4's tests | standard |
| 17 | mirrors land where the wire changes | 1 | §8's iOS mirror, merged with PR 1 | mirror |
| 18 | the owner runs it on the Pi | 3 | the run sheet below | hardware |
| S1 | no LAN request reaches an admin route | 2 | `TestTheLANListenerServesNoAdminRoute` | standard |
| S2 | no passcode or session token in clear | 2 | `TestAPlainHTTPRequestGetsNoPageFormOrCookie`, `TestThePasscodeTravelsOnlyOverTLS` | standard |
| S3 | a source outside the on-link prefix refused at accept | 2 | `TestASourceOutsideTheOnLinkPrefixIsRefusedAtAccept` | standard |
| S4 | no listener in public mode | 2 | `TestAPublicBridgeStartsNoPageListener` | standard |
| S5 | a passcode change ends open sessions | 2 | `TestAPasscodeChangeEndsEverySession` | standard |

**The owner's run on the Pi** (PR 3's hardware tier, one sheet, its numbers and observations recorded in
the PR): a phone's browser and a laptop's on the LAN reach the page by the `.local` name and by address,
the warning answered by the fingerprint the console shows; browse, queue an album, play, pause, seek and
skip; switch DACs while playing (asked, then stopped and switched), and play on the new DAC; with MPD
playing on the other DAC, its switch refused naming MPD, and MPD playing on; the tone on each DAC; unplug
and replug the new DAC; restart the bridge; the 1-bit app casting while the page is open; both closed while
a track plays, and the next track heard; native or DoP chosen on a DAC that takes it, with the sheet and
the large Stop.

## 11. Deferred, each with its residual

- **CoreAudio and WASAPI**: the player and the page are Linux and ALSA only (O1). On macOS and Windows the
  page never starts, the console's routes answer `player_not_supported`, and nothing else changes.
- **The app switching the DAC (O2)**: no `/v1` route. The app reads the new `dac` after a switch the
  console or the page made, and cannot make one.
- **B671 and B755**: deferred untouched. Their backlog entries are not on this host, so this design states
  no residual of its own for them and changes nothing they name; the orchestrator confirms (§14, question
  3).
- **Authentication for OpenHome controllers**: the surface stays an opt-in that trusts the LAN
  (`openhome.md:21`). The page says its passcode does not cover controllers: a controller on the LAN can
  still play, edit the queue and stop, as today, but cannot switch the DAC, play the tone or write DSD,
  which no SOAP action does.
- **GENA**: stays 501 (`openhome.md:32`); controllers poll.
- **Named-controller claims**: no surface says which controller made a change; the page says that the
  queue changed elsewhere, never who changed it.
- **Outward-facing copy**: 1-bit.app's docs, the release notes and the app's words for the page wait for
  the owner's OK; the PRs carry working copy on the page and the console alone.
- **The chime, until B12 lands**: no check is offered and no answer recorded, so "listener confirmed" never
  shows; B12, or a PR after it, adds the chime to the page through the same control layer.
- **IPv6**: the page listens on IPv4 alone, as the OpenHome surface does: a LAN reached over IPv6 alone
  cannot open it.

## 12. Rules the PRs change (proposed wording, not applied)

This round changes no rules file. Each PR's worker writes its patch from these:

- **`player.md:160`** (PR 1): "Serve's player runs on one binding, `playerBinding`: the boot snapshot's at
  its start, and the one a switch moved it to since, which every reader reads at each use and the switch
  alone moves, inside the engine's maintenance hold and the backend's configuration lease, once the save
  has landed. The live config's `enabled` still withholds the output at once (`disabled`); a snapshot of
  another binding than the seam's is withheld `binding_changed`, which only a switch's first moments
  show. **Don't keep a copy of the binding, read the boot snapshot's, or move the seam outside the hold,
  the lease and a landed save.** `TestTheBindingIsReadThroughTheSeamAlone`,
  `TestEveryReaderOfTheBindingFollowsASwitch`."
- **`player.md:168` and `admin-console.md:950`** (PR 1): the DAC choice switches a running player live, by
  §2.4, answering `switched`, `unchanged`, or `restart` where no engine runs; its refusals; the tone,
  B602's, by §3. **Don't save a binding the target's open did not check while the player runs, stop a
  holder, or switch outside the hold.**
- **`admin-console.md:944`** (PR 1): the control layer's turn replaces `Server.mu` for the player's
  writes, from their read of `before` to their answer. **Don't write `output.player` but through the
  control layer, or read `before` outside its turn.**
- **`output.md:86`** (PR 1): "resolved from its boot USB binding" becomes "resolved from the player's
  binding (`playerBinding`)".
- **`cli-serve.md`** (PR 1 and PR 2): the console view's bullet reads the seam; serve's teardown stops
  the page first, and the player's shutdown then stops the OpenHome surface, joins the control layer's
  operation within its bound, and only then stops the engine; the page's supervisor follows the live
  config.
- **`openhome.md`** (PR 2): the surface's binding and admission are `internal/player/lanlisten`'s, which
  the page shares; `internal/playerpage/**` joins its globs, the file widened to the player's LAN surfaces
  (it has the room: 44,178 bytes).
- **`auth-tls.md`** (PR 2): the page's posture (HTTPS on the bridge's pair, no HSTS, Host and Origin, the
  cookie), the passcode's file and its rules, and the refused-login lines that name a client.
- **`config-lifecycle.md`** (PR 2): `playerEnabled` owns `playerLANPage` too; `output.player.lanPage` is
  read live, by one consumer.

## 13. Alternatives weighed

Each in one line, with why it lost.

1. **Rebind by an in-process restart of the player's run**: a handover of exclusion between two engines,
   a new run ID that refuses every generation held, the OpenHome surface restarted, the start's path
   inside a request (§2.1).
2. **No live switch, the console's restart after a save** (today): the hazard itself, and a restart is
   administration, which the page may not reach.
3. **Plain HTTP with the passcode**: the passcode and the cookie cross the LAN in clear.
4. **A second certificate for the page**: a new mint and a second fingerprint to compare, which the brief
   rules out.
5. **A local CA installed in each browser**: a trust-store change on every device, and a CA key on the
   box.
6. **ACME**: a public domain and the internet, which is public mode's posture, not a LAN box's.
7. **A PAKE login over plain HTTP**: the script that runs it is itself served in clear and rewritable on
   the path.
8. **The page on the API's listener (7788)**: that listener answers Tailscale and any source with bearer
   tokens; W2 asks for a separate, player-only listener.
9. **The page on the OpenHome surface's listener (7792)**: the surface is an unauthenticated opt-in with
   SOAP's limits and may be off while the page is on; two auth models on one socket.
10. **The console on the LAN with a login** (public mode's shape): administration stays loopback-only.
11. **The passcode in bridge.yaml**: not picked up from the CLI, blocks console saves, sits behind the
    unread block's guard, and rides every copy of the config (§6.1).
12. **The passcode as the console's account in `adminauth.json`**: a loopback bridge has no account, and a
    public bridge's account would then open a page public mode must not run.
13. **Sessions persisted across restarts**: another file serve writes beside the credential, for the cost
    of one passcode entry after a restart.
14. **A bridge-wide lockout after N failures**: a lever any host on the LAN could pull against the owner;
    a per-source limiter and one check at a time bound guessing without it.
15. **A compound edit made one engine command**: an engine command, which the budget refuses; the first
    step's generation and the edit's own entries give the same guarantee (§4.3).
16. **A chain of expected generations across a compound edit's steps**: the engine answers no step's
    generation, and its generation moves on a track heard, so an edit would be refused for a track
    boundary.
17. **Pushing the state over SSE**: a held stream per tab, against a once-a-second read the console's
    pattern already bounds.
18. **A new index for the page's browse**: the budget's "not a new index"; the web player's catalog serves
    it.
19. **The console's web-player scripts reused on the page**: they call console routes, which the page must
    not serve.
20. **`Chosen` alone, with no open of the target** (today): it proves identity, never that the DAC is free
    (Codex 3).
21. **Opening the target before the hold as well as under it**: a second open of what may be another
    player's DAC, in whose window that player's own open fails; the /proc look before the hold opens
    nothing.
22. **The hold alone, or the lease alone**: each leaves the other's work admitted (§2.5).
23. **The go-ahead as a Stop command, then the hold**: a window between them for another controller's Play;
    the hold's own release makes stop and close one step.
24. **The tone as a queue entry**: an edit every controller sees, through a resolver that opens library
    files alone.
25. **The tone through `bridge output pcm-test`**: a CLI harness that opens the DAC without the lease and
    is never called from the server (`player.md:72`).

## 14. Questions for the orchestrator

1. **29641b98 is not on this host.** The design reads B9 PR 2 at e4493648 and its §7.2 at 8b3c4278. Does
   29641b98 change the lease (`dsd_choice.go:26-33`, `:308`) or §7.2?
2. **B9 PR 3's engine reservation**: will it be the maintenance hold entered at a stopped generation, the
   one admission the switch and the tone take (§2.3)? If it is another kind, which does B634 take?
3. **B671 and B755**: their backlog entries are not on this host. What residuals should §11 state?
4. **iOS**: does the app word `binding_changed` as "restart the bridge", or assume the bridge element's
   `dac` is fixed for a serve run? The Mirror PR of §8 should check.
5. **The owner's OK** on the defaults: the page's port 7793; the passcode's 8 to 64 characters; sessions in
   memory, so that a restart signs browsers out.

## 15. The brief's review, point by point

| Review point | Settled in |
|---|---|
| 1. Exclusion that outlives the switch (Codex 1) | §2.1, §2.5 |
| 2. B9's fallback; one rule (Codex 2, Carbon 1) | §2.3 |
| 3. A chosen DAC isn't proven free (Codex 3, Carbon 2) | §2.4 step 6, §2.6 |
| 4. No engine is a real state (Codex 4) | §2.7 |
| 5. Playability for the hardware player (Codex 5) | §4.2 |
| 6. Refusal tests that can fail (Codex 6) | §9, PR 2's security cases |
| 7. The lock (Carbon 3) | §2.9 |
| 8. The listener's settings (Carbon 4) | §5.4 |
| 9. The tone's scope (Carbon 5) | §3's one statement, §0, §10 row 6 |
| 10. The passcode's home (Carbon 6) | §6.1 |
