## 2026-11-20 — B9P3BFU: B9 PR 3b's follow-ups, three tests that discriminate and the refusal's words for the console's tone

Branch `fix/b9p3b-followups`, from main `dd688b15`: `aa98183f` the first
item, `60fb963e` the third, `0e3061ea` the second, `09f1d1dd` the fourth, then
this record. The four are follow-ups B728 B9 PR 3b's review rounds named, kept
for after its merge; its record is `ops/engineering-log.md:104270`, "## 2026-11-16
— B728 B9 PR 3b: the DSD choice's route, the DAC's token and the publication
count". None is a defect of the product: three tests that did not show what
they claimed, and a protocol sentence the iOS mirror copies. The rules are
`output.md`'s bullets under "The player's DSD choice: its write (B728 B9)",
their long forms in `ops/History-rules-output.md`.

### 1. The refresh a write's containment asks is heard ending

`TestAPublicationThatPanicsAnswersWithNoEpoch`
(`internal/player/playback/dsd_choice_publish_internal_test.go`): a choice's
write whose publication panics is contained (`containDSD`) and asks a refresh
once its hold of its reading ends (`publishChoice`, `dsd_choice.go`). The test
joined the backend's goroutines (`Backend.Wait`) and then made a refresh of
its own (`RefreshDSD(true)`) before it read what was published, so the state
written showed whether or not the refresh the containment asked had run.

- **The seam** (`aa98183f`): `Seams.ChoiceRefreshed` (`backend.go:212`), nil
  in production, runs on the refreshes' goroutine once the first refresh run
  after a choice's write asked one has returned, whatever it published,
  nothing included. A write's two asks, `publishChoice`'s and
  `endChoiceReading`'s (`dsd_choice.go:532`, `:385`), go through
  `askChoiceRefresh` (`dsd_work.go:141`), which marks the ask
  (`Backend.choiceRefresh`, an `atomic.Bool`) before it asks; the run that
  begins next takes the mark (`:129`) and runs the seam once `RefreshDSD` has
  returned, so a later ask that replaces the write's kept work carries it.
- **The test** waits on the seam, bounded, and reads the published pair
  before anything else refreshes: where the write's pair was not stored, that
  refresh publishes the state written, convert, at one publication past the
  one standing before the write; where it was, that refresh finds the write's
  own pair standing (convert, one past, the answer's) and publishes nothing,
  the path with no change the seam runs on too. `Backend.Wait` still joins
  the goroutine before the seams go: the seam reached is not its goroutine
  ended. The test's own refresh is gone.
- **Red first, and the controls**, each predicted in writing before it ran
  and laid over the committed files with `go test -overlay`, the tree checked
  clean after, each as predicted; B is the case where the pair was not
  stored, A where it was:

  | Control | Red | Green |
  | --- | --- | --- |
  | I1-RED the base's `dsd_work.go` and `dsd_choice.go`, the seam's field declared and never called | B and A, "the refresh the containment asked never ran" (`:110`), 10 s each | |
  | I1-C1 the containment asks no refresh (`owed = true` dropped from `publishChoice`'s recover) | B and A (`:110`) | the base's test under the same mutation, B and A: its own refresh hid the missing one, the gap this item names |
  | I1-C2 the seam only where the refresh moved the publication | A (`:110`) | B |
  | I1-C3 the seam run, the refresh it was asked for not made | B: "left native published" (`:114`), "publication 2 … want one publication, 3" (`:120`) | A |

### 2. A real Start completes while the refresh a PUT waited for is parked

`TestAWriteWaitingForARefreshAnswersWithinItsTime`
(`cmd/bridge/serve_player_output_dsd_engine_test.go:832`, PR 3b's round 2)
has a Play's Start reach content that refuses every open (all_unplayable):
no route test had a Start complete. Serve's tests keep the real backend off
any device (`player.md`: no serve test reaches `alsapcm.Open`), and no sink
is made outside `internal/alsapcm` but by its fake kernel, which only the
native-integration overlay lends `cmd/bridge`
(`scripts/native-integration-overlay.py`).

- **The test** (`0e3061ea`):
  `TestNativeIntegrationAStartCompletesWhileTheRefreshAWriteWaitedForIsParked`,
  in `cmd/bridge/serve_player_output_dsd_integration_test.go` (build tag
  `nativeintegration`): the route, serve's adapter, the real engine and the
  real backend, whose `OpenSink` is the fake kernel's. A refresh started on a
  goroutine of its own parks in its reading of the DAC (`parkedChoiceBox`); a
  PUT with a route's time of 500 ms answers 500 persist_failed with its four
  members, the engine's reservation free; a Play's Start of a 32-bit PCM WAV
  returns (Play answers once its Start has, nil), the device opened once and
  open, before the refresh is let go. Let go, the refresh ends and the PUT
  that failed wrote nothing; a PUT while that session plays answers 409
  playback_not_stopped, the engine's not_stopped (the protocol table's step
  8); once a Stop is confirmed a PUT is written, 200, one write, the device
  still opened once and now closed. The fake DAC is given no time, so the
  session stays starting (nothing heard): the test waits for the transport
  playing alone.
- **A file of its own.** The harness's integration file,
  `cmd/bridge/output_dsdtest_integration_test.go`, is a guarded path of the
  qualification version (`internal/player/qualification/version_gate.go:36`,
  the prefix `cmd/bridge/output_dsdtest`): a change there moves
  `qualificationVersion`. The overlay script now enables every
  `cmd/bridge/*_integration_test.go` by its exact leading build tag, failing
  closed as before on a tag lost or a constraint added, the harness's file
  still required; its own tests (`TestNativeOverlayRequiresPlaybackSuite`,
  `TestQualificationOverlayFailsClosed`) pass as they are. CI's
  native-integration step (`gate.yml`, `-run '^TestNativeIntegration'`) runs
  it; its list of names it requires is unchanged.
- **Red first.** The behaviour is in place at the base (round 2's bounded
  `lockReading`), so the test's reds are its controls', each predicted in
  writing before it ran, the native overlay merged with the mutation, each as
  predicted (a first batch ran nothing: its runner put `-watchdog` ahead of
  the package, which `go test` read as a package):

  | Control | Red, the one failure line |
  | --- | --- |
  | I2-C1 a PCM Start that waits for the refreshes' lock (`Backend.Start` takes and lets go of `dsdMu` first) | "the Play, the refresh still parked: … backend_timeout: start did not answer within its deadline" (`:79`), at the engine's `StartTimeout` |
  | I2-C2 the write's wait for the refreshes' lock unbounded again (round 2's fix reverted) | "waiting for the PUT's answer, the refresh still parked" (`:69`), at the test's give-up |
  | I2-C3 the backend's lease kept by a write whose wait was given up | "the Play, the refresh still parked: … start did not answer within its deadline" (`:79`): the engine's reservation free and the Start held at the lease |
  | I2-C4 the Start's open made twice (a sink opened and closed before its own) | "the device was opened 2 times (open now: true) while the refresh is parked" (`:83`) |
  | I2-C5 the engine's not_stopped answered as another word (the adapter's case dropped) | "a write while the Start's session plays: 500 … persist_failed" (`:95`) |
  | I2-C6 a confirmed release leaves the session's record (`letGo` deletes none) | "a write once stopped: 409 … playback_not_stopped" (`:109`) |
  | I2-C7 the adapter keeps the engine's reservation (its deferred release dropped) | "the engine's reservation is held once the PUT answered" (`:71`) |

### 3. The tone route test: an active tone leaves GET as it was

`TestAWriteBesideTheConsolesToneIsRefusedAndLetsTheReservationGo`
(`cmd/bridge/serve_player_output_dsd_engine_test.go:908`) read GET
/v1/outputs' state as stopped while the console's tone held the device slot.
GET's transport is the engine's (`bridgeOutput.facts`,
`cmd/bridge/serve_player_output.go:160`), which the tone runs outside, and
the tone publishes no observation (B601), so a GET with no tone reads the
same: the assertion could not tell a tone from none, and removing the tone
cannot make it red. What the code promises, and the test now proves, is the
other way round: a tone that is demonstrably active leaves the element's
transport and its playback observation as they were.

- **The test** (`60fb963e`): the element of kind bridge is read before the
  tone (`state` `"stopped"`, no `playback`); the backend's `ToneStatus` reads
  `playing` on both sides of a GET made while the tone is admitted to the
  device slot and held there (`Seams.ToneAdmitted`, having opened nothing:
  serve's tests open no node); and that GET's `state` and `playback` are each
  compared with the GET before the tone, by `t.Errorf`, so the test runs on
  and a control's red is that field's line alone.
- **The controls**, predicted in writing, laid over with `go test -overlay`,
  each as predicted:

  | Control | Red, the one failure line |
  | --- | --- |
  | I3-C1 `facts` says `playing` while `ToneStatus` plays | `state "playing"; want "stopped", as before the tone` (`:983`) |
  | I3-C2 `Tone` stores a PCM observation as it is admitted | `playback {"bootId":…,"state":"open",…}; want absent, as before the tone` (`:983`) |
  | I3-O1 `takeLease` without the tone's check (the PR 3b record's first) | the write beside the tone 200, where 409 (`:989`) |
  | I3-O2 the adapter's deferred release dropped (its second) | "the engine's reservation is held once the PUT answered" (`:994`) |

### A correction to the PR 3b record

`ops/engineering-log.md:104569`, under "## 2026-11-16 — B728 B9 PR 3b: the
DSD choice's route, the DAC's token and the publication count", in its
section "Main merged in, through a branch's merge (2026-11-16)" (lines
104558-104572), counts this test's controls as two: the tone's check taken
out of `takeLease`, and the reservation's release taken out of the adapter.
Both are of its PUT half. Its GET half, "GET reads the player stopped", had
none, and could have none as it was written, a GET with no tone reading the
same. The test now has four: those two, run again on the strengthened test
(I3-O1, I3-O2), and this record's two of its GET half (I3-C1, I3-C2), each red
in exactly its assertion. The archive is frozen; this entry is the correction
(`ops/engineering-log/README.md`).

### 4. What a write answers during the console's tone

Read from the code: the tone runs outside the engine, so the engine grants
the write's configuration reservation beside it
(`cmd/bridge/serve_player_output_dsd.go:65-74`), and the backend's lease
refuses the write while the tone holds the device slot, from its admission
until its node's close has answered (`takeLease`'s `b.toning != nil`,
`internal/player/playback/dsd_choice.go:601`; a held close keeps the slot,
`internal/alsapcm`'s `TestAToneWhoseCloseIsHeldKeepsTheSlot`), with
`ChoicePlaybackNotStopped`, which the route answers 409 playback_not_stopped
with its four members (`internal/api/outputs_dsd_choice.go:216-217`; the
backend's half in `TestAChoicesWriteBesideAToneIsRefused`, the route's in
the test of item 3). That is the owner's "Stop playback to change DSD" held
for the tone, and nothing of the write's behaviour changes.

`PROTOCOL.md` says it at its three sites, in the same words: the bridge
object's transport line (the console's test tone is no transport: from its
start until it has let the DAC go, `state` reads `stopped`, `playback` is as
it was before it, and the PUT answers `409` `playback_not_stopped`), step 8
of the PUT's table, and the error table's `409` `playback_not_stopped` row.
The wording is additive: `ProtocolVersion` stays 1. Its SHA-256 at
`09f1d1dd` is `10c1e08b0221311447e73b2f453afa7e5b16fe4adbebe239ed0434e53c89997c`,
which the iOS mirror's copy (`docs/BridgeProtocol.md`) pins.

### The rules

`output.md`'s route bullet says the tone's refusal at step 8 (the engine's
reservation granted beside it, GET reading the player stopped, the backend's
lease refusing), its long form the same in its bold line and in the
adapter's paragraph; the write's bullet and the route's name the new test,
both copies. `output.md` stands at 97,420 bytes of 100,000.

### The runs

On Linux, at `09f1d1dd`, the pinned toolchain (`GOTOOLCHAIN=go1.27.3`), the
Makefile's tags, in one build-slot session (the slot free at once):

- Under `-race -count=20 -timeout 30m`, by name: item 1's test, ok, 1.1 s;
  item 3's, ok, 2.3 s; item 2's through the native overlay, ok, 12.2 s; the
  overlay script's own two tests in `internal/player/qualification`, ok,
  3.4 s.
- CI's two native-integration steps as `gate.yml` runs them: `cmd/bridge`,
  16 tests passed, every name the step requires among them, 20.0 s;
  `internal/player/playback`, 33 passed, 3.2 s.
- `make fmt` (the tree unchanged), `make vet` and `make docs-check`: clean.
  `GOOS=windows` and `GOOS=darwin` `go vet -tags
  ts_omit_webclient,ts_omit_debugeventbus` of `./internal/player/playback/`,
  `./cmd/bridge/` and `./internal/player/qualification/`: clean. Item 2's
  file is built only under its tag, through the overlay, on Linux.
- `make fmt vet test build-all P=8 TEST_PKGS='-count=1 -timeout 45m ./...'`:
  fmt and vet clean; `make test` 1,771.8 s, 91 packages ok, none failed
  (`cmd/bridge` 1,754.7 s, `internal/manifest` 1,647.9 s, `internal/admin`
  1,274.8 s), `cmd/bridge` given 45 m, as recent gates did, since its time
  under `-race` is close to the Makefile's 30 m here; `build-all`, the six
  targets, 34.1 s.
- The controls above, without `-race`: item 1's five in 56 s, item 3's four
  in 36 s, item 2's seven in 136 s.
