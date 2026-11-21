## 2026-11-21 — Bridge lows 13: the check hold's follow-up Lows (B971) and the config merge's (B974)

The follow-up Lows of #1535 (the engine's hold for the DSD check, B728 B12 PR 2) and #1536 (the
console's save merged into another writer's config, B535), on `fix/bridge-lows13` from main
`04db6164`. Each premise was read again at that base first; all eleven held. Each code change has
its test committed before it and run red at that commit; the pins of what the code already did are
green there and shown by negative controls. The rules changed are `output.md`'s check-hold bullet,
`config-lifecycle.md`'s B535 sentence and `admin-console.md`'s save-config bullet, each with its long
form in the same commit, and `CLAUDE.md`'s engine row.

### B971 (#1535's Lows)

1. **A grant's expiry, asked by its caller.** `takeGrant` handed a grant the owner sent before its
   expiry to a caller that read it after, the expiry's message not taken yet. It now gives back a
   grant whose hold's expiry has come on the engine's clock, after its context's check, and refuses it
   `lease_invalid` with the owner side's own detail (`expiredBeforeGrant`: no caller needs to tell the
   two apart, holding nothing either way). The expiry's warning is written once, by whichever of the
   two messages the owner takes first: `maintenanceExpiry` as before, or `checkGone`, which now says it
   where the hold's expiry has come; the expiry's message then finds the hold ended, or never comes,
   the end having stopped its timer. A line written on the caller's side was rejected: it would be
   written twice where the owner's message is taken first. Red at `71a528b8`:
   `TestAGrantReadAfterItsHoldExpiredIsGivenBack`, both cases ("the check whose grant was read past
   its hold's expiry: got <nil> (reason ""), want reason "lease_invalid""), and the two expiry rows of
   `TestTakeGrantAnswersWhatItsCallerMayHold` (the hold handed over, nothing given back).
2. **The wait's receives, one arm at a time.** `awaitGrant` reads the owner's answer at two receives,
   the main select's and the one after the engine's end. A test seam (`seams.checkArm`,
   `grantWait.only`: every arm but one nil for a turn, alsapcm's `armPick` pattern) forces each arm in
   a case of its own of `TestAGrantReadAfterItsCallersContextEndedIsGivenBack`: the owner's answer,
   the context's end (giveUp losing the handoff and reading the answer), the engine's end (the engine
   shut down first), and the select's own pick. `TestTakeGrantAnswersWhatItsCallerMayHold` calls
   `takeGrant` directly on an engine built and not run, so a grant given back stays in the owner's
   queue for the test to read.
3. **Two pins.** `TestACheckWhoseReleaseIsUnconfirmedPastItsExpiryIsRefusedForTheRelease`: the clock
   skipped past both the hold's expiry (a hold of a second) and the release's deadline, then the
   release acknowledged there, so the deadline's policy runs first and `deliver` meets both; it
   answers `release_unconfirmed`. `TestASessionLeavesTheSessionsInTheStepItsReleaseEnds` gains a paused
   session's own release of the device (`onDeviceReleased`) and that release overdue (`pauseOverdue`)
   then reported late.
4. **giveBack's own goroutine, joined by the harness.** The goroutine `giveBack` starts when the
   owner's queue is full now counts in `Engine.wg` alone, never in `live`; `post` still lets it go
   through `Done` at the shutdown and at `abandon`. Run waits on `wg` nowhere, as before: this is the
   tests' join, no shutdown guarantee. `TestAGrantGivenBackPastAFullQueueIsJoined` runs in a
   `testing/synctest` bubble: the owner held at a barrier's admission, its queue filled, a grant given
   back so the fallback runs (the caller answered at once, the queue still full), then the engine
   ended by its shutdown or by a panic on the owner, the goroutine held at its end (`seams.gaveBack`),
   and `synctest.Wait` once every other goroutine of the bubble is blocked: the wait the harness's join
   makes must not have returned. Red at `71a528b8`, both cases ("the wait the harness's join makes
   returned while giveBack's own goroutine was under way").
5. **OpenHome's still clock** counts the goroutines its at-once timers start (`newStillClock`, a
   WaitGroup its test's cleanup waits on, registered before the engine starts, so it runs after the
   engine's stop); the callback still never runs inside `At`. A test-helper change with no red run:
   nothing in the suite observes a goroutine of a test's clock outliving its test.
6. **Records.** "An error leaves no hold" is "an error leaves no hold the caller owns; the owner's
   cleanup may still be pending", in `output.md` and its long form; `CLAUDE.md`'s engine row names
   the residual before admission (the caller's wait bounded from the owner's admission, by its
   context alone before it, which PR 4's route closes with a bound of its own).

### B974 (#1536's Lows)

7. **One line says the left file's error.** The console's line for a save refused because the file
   another writer left would not load at the next start (`OutsideLeftUnloadable`, wrapped or not,
   `errors.As`) carries no error: the holder said why once for those bytes. Red at `44a04b27`:
   `TestALeftUnloadableRefusalIsSaidOnceAndTheOthersKeepTheirReasons`, two lines carrying the load's
   words after the first refusal and three after the second, and the wrapped refusal's line carrying
   it. Its merged-load and ordinary-failure cases, green there, hold that those keep their reasons.
   Reading of the brief: "each log exactly one detailed diagnostic" is read as the log holding one
   line with the error's words after the first refusal and still one after the second.
8. **Assertions.** The left-unloadable diagnostic's `err` attribute in
   `TestAFileThatDoesNotLoadIsSaidOnceAndItsSettingsAreKept`; and
   `TestTheMergedFileIsLoadedWithTheVariablesThisBridgeStartedWith`, roots given by a variable and a
   real setting changed by another writer, so the merged load runs (the comment of
   `TestAFileWhoseRootsAVariableGivesTakesAnotherWritersComment` records no setting, and its save never
   reached that load).
9. **Scalars hashed raw**, no code change: the long form says why (two spellings are two values to
   the block's reader).
10. **Recovery says what it kept.** The holder keeps the settings recorded while the file did not
    load (`unsaid`) and says each once, with the ordinary line, at the turn that finds bytes that load;
    none at a turn still unloadable. Every setting recorded then is said, the failing one's corrected
    value too: the brief's test note names the valid one, and the failing one is recorded alike, its
    line as true. Red at `44a04b27`: `TestTheSettingsRecordedWhileTheFileDidNotLoadAreSaidOnceItLoads`
    ("said 0 times"). Its last save first ran as a plain save, which finds the holder's own bytes and
    never reaches the turn's branch, so control D5 stayed green; since `8283b7ef` it follows another
    writer's comment, and D5 is red.
11. **Two guards.** `walkSettings` compares an opt-out by what it turns on only where the field's
    type is a pointer to a bool (`isSwitch`, read from the type, so a nil pointer answers too), an
    entry of another type compared by value; red at `44a04b27`:
    `TestASwitchPathOfAnotherTypeIsComparedByValue` ("panicked reflect: call of reflect.Value.IsNil
    on bool Value"). `filedSwitches` reads the three switches through typed `*bool` fields, no
    reflection, and needs no guard. `fileOutside`'s copy keeps no guard: `settingValue` answers by
    `Config`'s type alone, which `TestEverySettingPathNamesAFieldOfEveryConfigAlike` pins.

### Negative controls

Each through `go test -overlay -count=1 -v -timeout 300s -run <the item's tests>`, the mutation applied to HEAD's
bytes in memory, no tracked file touched (`git status` clean after each), predictions written first.

| Control | Mutation | Red, as predicted |
|---|---|---|
| C1 | `takeGrant`'s expiry check removed | both cases of `TestAGrantReadAfterItsHoldExpiredIsGivenBack`; `TestTakeGrantAnswersWhatItsCallerMayHold` |
| C2 | `checkGone`'s `logExpiry` removed | the give-back-first case ("said 0 times"); the expiry-first case green |
| C3 | the main select's receive returns `r.hold, r.err` | the owner's-answer case; both expiry cases; beyond the prediction, the cases whose turn the select left to its own pick where it took that arm (the select's-pick case, and the shut-down case of `TestAGrantGivenBackPastAFullQueueIsJoined`) |
| C4 | the receive after the engine's end returns `r.hold, r.err` | the engine's-end case alone |
| C5 | `deliver` asks the expiry before the unconfirmed release | `TestACheckWhoseReleaseIsUnconfirmedPastItsExpiryIsRefusedForTheRelease` (`lease_invalid`) |
| C6 | giveBack's goroutine uncounted | both cases of `TestAGrantGivenBackPastAFullQueueIsJoined` |
| C7 | giveBack's goroutine counted in `live` too | both cases ("1 goroutines counted for the owner to join") |
| C8 | giveBack's goroutine sends with no `Done` arm | the owner-panicking case; the shut-down case green, the owner draining its queue at its shutdown |
| C9 | `onDeviceReleased` lets the paused session go from `o.sess` | the own-release case; beyond the prediction, the overdue case, which reports the release late through it |
| C10 | `pauseOverdue` drops the session from the sessions owed | the overdue case |
| D1 | the console logs the error for every refusal | the left-unloadable and wrapped cases |
| D2 | a type assertion in place of `errors.As` | the wrapped case alone |
| D3 | every `ChangedOutsideError` taken for left-unloadable | the merged-load case |
| D4 | nothing said when the file loads again | `TestTheSettingsRecordedWhileTheFileDidNotLoadAreSaidOnceItLoads` |
| D5 | the said settings never forgotten | the same test, once its last save follows another writer's comment |
| D6 | the settings said at a turn still unloadable | that test and `TestAFileThatDoesNotLoadIsSaidOnceAndItsSettingsAreKept` |
| D7 | `isSwitch` dropped from the walk | `TestASwitchPathOfAnotherTypeIsComparedByValue` (the panic) |
| D8 | the merged file loaded with no variable | `TestTheMergedFileIsLoadedWithTheVariablesThisBridgeStartedWith`; the comment case green |
| D9 | the diagnostic without its `err` attribute | `TestAFileThatDoesNotLoadIsSaidOnceAndItsSettingsAreKept` |

### Runs

The changed packages under `go test -race -count=20 -timeout 60m`, one package each, four at once,
`TMPDIR` on disk (OpenHome's engine-backed tests skip on a volatile one), at `8283b7ef`:
`./internal/player/engine/` ok in 626.5 s, `./internal/player/openhome/` ok in 763.4 s,
`./internal/config/` ok in 318.9 s, and `./internal/admin/`'s save-refusal tests
(`TestALeftUnloadableRefusalIsSaidOnceAndTheOthersKeepTheirReasons`,
`TestAConfigSaveRefusalSaysWhatToDoAndNamesNoPath`, `TestTheConsolesSavesAnswerARefusalWithoutThePath`,
`TestAConfigSaveRefusalOfALinkToNothingSaysToFixTheLink`,
`TestTheUPnPServerSavesAnswerARefusalWithoutThePath`) ok in 228.0 s, the whole package's run
being `make test`'s. `cmd/bridge`'s source guards (`TestEveryCitedTestNameExists`,
`TestNoDocblockNamesAnotherDeclaration`, `TestNoBlankKeepers`,
`TestEveryBackgroundGoroutineDrainsOnCleanup`) passed with the release tags over the staged
records, and a ghost test name planted in this entry, staged, turned the citation guard red, its
bytes restored after. Vet for linux, darwin and windows, `make fmt vet docs-check` and the full
gate ran on this entry's own commit, so their results are in the PR's report, not here.
