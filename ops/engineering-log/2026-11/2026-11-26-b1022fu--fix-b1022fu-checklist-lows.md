## 2026-11-26 — B1022FU: the DAC line's advice names the config bridge doctor was given, and the checklist's tests read no inherited environment

### What

Three follow-ups to B1022's DAC line (`2026-11-25-b1022-player-on--fix-b1022-checklist-player-dac.md`):

- `bridge doctor --config <path>`, with the bridge's own player on and its card not known, advised
  `bridge output dac inspect` with no `--config`. That command takes `--config`
  (`runDACInspect`) and, run as printed, resolves its own (`loadCLIConfig`: `./bridge.yaml`
  first, then the platform's), so it could read another binding than the doctor read.
- The doctor's file-decided checklist cases read the process's environment: only the case that
  turns the player on through the environment set `BRIDGE_OUTPUT_PLAYER_ENABLED`, so a shell or
  a CI job exporting it true turned the player on in `TestBridgeDoctorPrintsThePlayerChecklist`
  and in the player-off rows of `TestBridgeDoctorNeverPointsMPDAtADACWhileTheBridgesOwnPlayerIsOn`.
- `outputChecklist` asks serve's boot gate (`BridgePlayerOn`) only where `checklistInput` found no
  element of the player's, and nothing pinned that order.

A second DAC of one name in the warning `<A> is MPD's output, and the checklist cannot tell
whether the bridge's own player plays through it too` was looked at and left: that warning lists
only the card MPD's output names, and a body with several candidates marks none as MPD's output,
so a second DAC of the same name never reaches it. It stays a residual, its words unchanged.

### The change

Production: `internal/outputcheck/outputcheck.go`, `cmd/bridge/output_checklist.go` and
`cmd/bridge/doctor.go`, 58 lines added and 18 removed (25 of the added code, 31 comment, 2 blank).

- `outputcheck.Input.InspectConfig`: the config the DAC line's `bridge output dac inspect` names,
  one shell word for its `--config`, "" for none. `playerDACLine`'s two warnings with the player
  on and its card not known name it: "Check the player's DAC binding (`bridge output dac inspect
  --config <word>`, or the console's Player page) before changing MPD's output.", and the same
  command in the warning beside MPD's output named. Empty, the words are as they were, word for
  word, and no other line reads it.
- `outputChecklistSource.TypedConfig`: the `--config` bridge doctor was given, as typed.
  `outputChecklist` quotes it with `shellWord` (bare where every byte is one the installer's
  `sh_quote` leaves bare, single-quoted otherwise, a quote written `'\''`) into the input, and
  sets nothing for an empty one. As typed, never the resolved path: a relative `--config
  bridge.yaml` is advised as typed, which names the same file from where the doctor was run;
  the resolved path is set too for a run that finds `./bridge.yaml` or the platform config and
  for the launcher's row, which pass a path no one typed, and the checklist keeps it for its store
  guard (`doctorStoreGate`) and its shared read's key (`outputsReadKey`).
- The wiring: `buildDoctorDeps(cfgPath)`, which `doctorCmd` alone calls in production, hands its
  `cfgPath` to `buildDoctorDepsRun` as the typed path, and it reaches the source through
  `doctorOutputChecklistTyped`; `buildDoctorDepsWith` (the launcher's row, through
  `buildDoctorDepsFor`) and the console's runner (`adminDoctorRunnerServing`) hand none, and
  serve's literal sets none. `doctorOutputChecklist` keeps its signature for its callers, naming
  none.
- `doctorOutputsSeam`: nil in production, the seam `buildDoctorDepsRun` hands the doctor's
  checklist, so a test drives `doctorCmd` and the launcher's row (`actDoctor`) with Linux, MPD and
  the cards stood in. Until now every doctor test of the checklist replaced `d.OutputChecklist` or
  called `doctorOutputChecklist` itself, so none could see what the wiring passed.

The player and its checklist are Linux's, so the advice reaches a POSIX shell. A path holding a
backquote is still one shell word, single-quoted, but it breaks the backquoted span the line shows
the command in; that is left.

### Tests

- `TestTheDACLineNamesTheConfigBridgeDoctorWasGiven` (`internal/outputcheck`): the two warnings
  with a config named, three rows, each fix word for word; and eight states (the player's card
  known four ways, the player off three ways, a box not read) whose four lines with a config named
  are the lines of the input naming none. The existing table, `TestTheDACLineNeverPointsMPDAtTheBridgesOwnPlayersDAC`,
  is unchanged and keeps every row's words.
- `TestEveryLineIsGreenOrSaysItsFixInOneSentence`: two rows with a config named.
- `TestBridgeDoctorNamesTheConfigItWasGivenInTheCommandItAdvises`: `doctorCmd` with `--config`
  paths holding a space, a double quote (skipped on Windows, whose file names take none) and
  `$HOME`; the printed fix word for word, the path single-quoted, and a POSIX shell (`sh -c 'set
  -- …'`, off Windows) reading the advised command back as `bridge output dac inspect --config`
  and the path.
- `TestOnlyBridgeDoctorsOwnConfigFlagIsNamedInTheAdvice`: one file, through the doctors' own
  wiring: `--config bridge.yaml` and `--config ./bridge.yaml` named as typed; found as
  `./bridge.yaml`, and named by the launcher's row by its absolute path, as it was.
- `TestTheConsolesDoctorNamesNoConfigInTheAdvice`: the console's doctor (`adminDoctorRunner`) run
  with serve's config, its path holding a space, over a source that says the player is on: the
  fix as it was.
- `TestServeChecklistReadsTheLiveRootsAndTheRoute`: serve's literal sets no `TypedConfig`.
- `TestThePlayersElementWinsOverTheBootGate`: six rows. The player's element with its card or with
  `dac` null, the gate shut or open: the line is the element's and the gate is asked no time; no
  element: the gate is asked once and its answer is the line's.
- `writePlayerConfig`, the doctor tests' shared fixture, sets `BRIDGE_OUTPUT_PLAYER_ENABLED`
  empty, which the loader takes no override from (`applyEnvOverridesAs`), and the environment's
  row sets it true after the fixture and before its load.

### Red first

- The environment: at the base, with `BRIDGE_OUTPUT_PLAYER_ENABLED=true` exported for the run,
  `TestBridgeDoctorPrintsThePlayerChecklist` red (the player-on warning in place of "No DAC found
  on this box") and the two player-off rows of `TestBridgeDoctorNeverPointsMPDAtADACWhileTheBridgesOwnPlayerIsOn`
  red; after the fix, with it exported, those and every checklist and doctor-gather test green,
  and green with it unset.
- The element's precedence: regression pins, green at the base; their controls are below.
- The advice: over the plumbing alone (the fields, the parameters and the seam, nothing reading
  them), the three named rows of the `outputcheck` table red and its eight as-it-was rows green;
  the three special-path cases and the two `--config` cases red, and the found, launcher, console
  and serve cases green.

### Controls

Each a mutation of the committed file in the working tree, its reds predicted before the run, put
back from the commit after, `git status` clean; all red exactly as predicted:

| Mutation | Red |
|---|---|
| the gate asked first, its answer still taken only where no element was found | the four element rows, by their count alone |
| the gate decides, over the element | the four element rows: three by their line and their count, one by its count |
| the typed path handed over unquoted | the three special-path cases; with their word check made non-fatal for the run, the shell read each back as other words: the space split it, the double quotes were taken away, `$HOME` expanded |
| the resolved path named in place of the typed one | the four cases of `TestOnlyBridgeDoctorsOwnConfigFlagIsNamedInTheAdvice` |
| `buildDoctorDeps` handing no typed path | the three special-path cases and the two `--config` cases |
| the empty guard dropped, so none is quoted as `''` | twelve: the found and launcher cases, the console's doctor, three element rows, five doctor rows, the console's case with no element |
| serve's literal given `TypedConfig: *configPath`, on disk | `TestServeChecklistReadsTheLiveRootsAndTheRoute` alone |
| the named config put in the player-off fix for no DAC | that state's as-it-was row alone |

### The rules

`output.md`'s B1022 line is unchanged: none of its bolded sentences changed. Its long form in
`ops/History-rules-output.md` gains the words of the two fixes with a config named, who names one
and who does not, the element's precedence over the boot gate, the new tests, and this entry
among its records.

### Runs

One build-slot session at `f8e96fbd`, on Go 1.27.3 with the release tags, `git status` clean after
each step:

- `make fmt vet test build-all docs-check` with `TEST_PKGS='-count=1 ./...'`: `make fmt` rewrote
  nothing; `make vet` clean; `make test`, 93 packages ok under `-race` (`cmd/bridge` 1,889 s,
  `internal/admin` 1,537 s, `internal/manifest` 1,471 s); `make build-all`, its build-tag check
  OK and six binaries; `make docs-check` ok.
- `make vet` for linux/amd64, linux/arm64, windows/amd64, windows/arm64, darwin/amd64, linux/386
  and linux/arm: clean.
- `cmd/bridge`'s source guards by name, `TestNoDocblockNamesAnotherDeclaration`,
  `TestEveryCitedTestNameExists`, `TestNoBlankKeepers`, `TestEveryBackgroundGoroutineDrainsOnCleanup`,
  `TestOnlyHostTreesNamesTheHostsTrees` and `TestTheDoctorsOutputsBuilderReadsNoHostTree`: green.
  A test name planted in this entry failed the citation guard, naming the entry, and it passed
  once the bytes were put back.
- The pinned gofmt over the changed Go files: none listed.
- `cmd/bridge` whole, without `-race`, with `BRIDGE_OUTPUT_PLAYER_ENABLED=true` exported for the
  run: the checklist's file-decided cases and every test this change adds green. Thirteen other
  tests of the package fail there, each because the exported variable turns the player on where
  its config leaves it off, the product's answer read off a load that applied it
  (`TestPriorOutputResolvesMergeKeysAsLoadDoes` and `TestPriorOutputOfAFileTheDecoderRefusesElsewhere`
  build the block they expect from such a load, while the rewrite writes the file's own value):
  `TestBothDoctorsFailEveryDataDirectoryServeRefuses`,
  `TestTheConsolesDoctorNamesThePlayersStorageAsBridgeDoctorDoes`,
  `TestPriorOutputResolvesMergeKeysAsLoadDoes`, `TestPriorOutputOfAFileTheDecoderRefusesElsewhere`,
  `TestOutputDACInspectWorksWithoutAConfigOrABinding`,
  `TestServeStartsThePlayersWatcherWhenTheConfigTurnsItOn`,
  `TestTheConsoleSaysWhatTheOpenHomeSwitchWaitsFor`, `TestTheConsoleTurnsTheRunningPlayerOffAndOnAgain`,
  `TestTheConsoleTurnsAPlayerOnForTheNextStart`, `TestAPlayerTurnedOffIsNotOfferedJustAfterTheRestart`,
  `TestServeSharesDiscoveryAcrossItsEligibleDevices`,
  `TestServeRunsTheOpenHomeSurfaceOnlyWithItsOptInAndThePlayer` and
  `TestServeStartsThePlayerOnlyWhenEveryGateHolds`. They are outside this change and left to a
  follow-up; each passes with the variable unset, as the gate above ran them.
