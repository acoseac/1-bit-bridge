## 2026-11-20 — B535: a console save over a bridge.yaml another writer changed merges instead of being refused until a restart

Branch `fix/b535-console-save-merge`, from main `d4c5d062` (#1526's merge). Four commits:
- `59d698a5`: room in `config-lifecycle.md`, B54's long form moved to the History;
- `7350d847`: the merge, its tests and the census;
- `0bd71123`: the records (the config lock's rule and its long form, the History's B535
  paragraphs, admin-console.md's B604 bullet);
- this record.

Every command below ran from the checkout's root with `GOTOOLCHAIN=go1.27.3` and the release tags
(`-tags ts_omit_webclient,ts_omit_debugeventbus`, `Makefile:9`), on Linux amd64, `TMPDIR` on disk
below the checkout's `.tmp/`. Each red and each control was predicted in writing before it ran.

### The hazard, and what was decided

After any other write to `bridge.yaml` beside a running serve (an edit, `bridge init`, an offline
`library add` or `remove`, a bridge older than the lock), every console save answered 500 "…restart
the bridge…", whatever it changed, until a restart: the holder compared the whole file's bytes with
the bytes it last read or wrote (`internal/config/runtime.go:238-241` at the base), only a landed
save moved that record (`:258`), and nothing reloads the file, so an edit that only added a comment
blocked every save. A plain merge was what B506 had stopped: a landed save writes the whole live
snapshot (`:251-254`), which differs from the file by `--addr`, the defaults, the resolved paths and
the environment.

The orchestrator's three DEFAULTs, confirmed on 2026-11-20, as built:
- merge, and no live reload: nothing of the other writer's is published, and the bridge loads it at
  its next start;
- a console change to a leaf another writer changed is refused until a restart, nothing saved or
  stored (`OutsideConflict`), checked at the save's `check` and again once `fn` returns;
- a recorded leaf under `deployment` refuses every console save until a restart
  (`OutsideDeployment`).

The premises file the brief names beside it was not on this host. The premises its own text cites
by line were read at the base instead, and held: the byte comparison, the save of the whole snapshot
and the record moved only by a landed save (`runtime.go:238-258`); `config.Edit` writing out every
default, every resolved path and the library name `Normalize` gives; the variants folder checked
against the live roots alone; `Load` turning the player off over an OpenHome port clash rather than
refusing; `usbid.Serial`'s one unexported field and its scalar YAML form; `rootChangeCases`
changing the scanner's roots without the holder or the file.

### The leaf comparison, and why it has that form

`compareTurn` runs at the turn, before `fn`, where the base's byte comparison stood. Equal bytes
discover nothing. Where they differ, both files are read as Load reads a file for its settings
(`readSettings`): `DecodeFile`, the defaults (`applyDefaultsAs(false)`), the relative paths resolved
against the directory Load takes from the name, then `normalize(false)`, and no `BRIDGE_*` variable.
So a writer that wrote out a default, a resolved path or the name `Normalize` gives has changed
nothing, and the live snapshot's own differences from the file (`--addr`, the overrides, the boot's
defaults) never enter: the comparison is file against file, never file against snapshot, which is
what the base's "Refused, not merged" paragraph found impossible. Where an enrich base URL is one
`normalize` refuses (which leaves the whole config as it was), the URLs are set aside, the rest
normalized, and each URL kept in `normalizeBaseURL`'s form or as written (`normalizeSettings`), so a
bad URL another writer left still compares field by field.

A leaf (`settingsChanged`, `walkSettings`) is every exported field yaml writes, by its yaml path,
reached through the structs that hold it and compared by `sameValue` (a list element by element, an
empty list and none alike, a struct of exported fields field by field); a list is one leaf, never
walked by its entries (`upnpUpstream.servers`); a type with a `MarshalYAML` of its own is one leaf
(`usbid.Serial`, whose one field is unexported, so a walk would find nothing in it); and a player
block that did not read, on either side, is one leaf, `output.player`, compared by the text it
keeps (`PlayerConfig.written` marshalled), its fields reading nothing. `TestEachSettingIsFoundToDifferAlone`
holds the walk to a census of the type it builds apart from it: over a config with every setting set
(`fillNonZero`, the serial included), a change of any one setting is found as that setting alone.

Each new leaf joins the holder's record (`outside`, under `mu`, sorted) at the turn that finds it,
with the bytes it was found in (`file`), whatever that save then does, and is logged once at Info by
its path and no value. A refused or failed save that kept neither would find the change again at the
next turn, but one that kept the bytes alone would find nothing and write the snapshot over the
other writer's (review 2's S1; X5 below). Only a restart forgets a leaf. A leaf a variable set at the
start never joins (`before.envSet`): B761's rule writes it, the file's value where the save left it
and the change where it made one (S4).

Every save then writes each recorded leaf from the turn's bytes (`fileOutside`: `DecodeFile`, the
field found by its yaml path on the saved copy and set from the decoded one, never through a pointer
the published snapshot holds, its path added to `filed`, so a field of Config's own at its zero value
is written as no key). A scan of Config's type (every exported field reached through structs and
list elements, by its tag) finds no nested leaf whose tag writes its zero value as a key but one
inside a list written whole, `upnpUpstream.servers[].name`, and B761's
`TestEveryLeafWhoseZeroIsAKeyIsAtTheTopLevel` holds every leaf a variable sets to that; a list is
written whole from the file, so no leaf is written at a zero value Load would read otherwise. Equal bytes skip only the discovery: once a save has merged, the next turn finds its own
bytes and still writes the recorded leaves (review 1's R4). The published snapshot is `next`.

`refuseMerge` runs at `check` and once `fn` returns, for every writer, whenever the record is not
empty: a change of a recorded leaf (`next` against `before`, a leaf meeting one it holds or is held
by, a player block and its fields) is `OutsideConflict`, naming the recorded leaves it meets; then
the file the save would write (`fileSaved` over a copy of `next`, marshalled) is loaded as the next
start loads it (`loadsAtNextStart`: `DecodeFile`, the defaults, the variables, the paths, then
`NormalizeAndValidateQuiet`, all without a word, and with the OpenHome surface on its port clash),
and one it would not load is `OutsideUnloadable`, carrying the recorded leaves and the load's own
error, which names no value. `Load`, `Validate`, `Edit` and the lock are unchanged.

The root handlers set `next.LibraryRoots` before they ask `check` (the add still only where it goes
from one root to two), so a root list another writer changed, and another writer's variants folder
below the root the console adds, are refused before the wipe or the prune (review 1's R1 and
review 2's S2); an add past two roots asks no check and is refused once `fn` returns. No other
console writer does work outside the clone in `fn`, and `UpdateChecked`'s doc gives one that does the
root handlers' order.

The console answers each kind in fixed words of its own, naming the leaves by their yaml paths and
never a value or a path (`changedOutsideRefusal`): in the save's own turn, make the change again or
restart; removed, put it back, then restart; repointed, restart; not decoding, correct it, then
restart; a deployment leaf, a conflict and a file the next start would refuse, restart, naming the
leaves. Status and code are unchanged (500 `save-config`).

The census (`cmd/bridge/config_writers_test.go`) names `compareTurn` as the holder's comparison in
place of `bytes.Equal`; it is still an `if` at the top of `UpdateChecked`, after the lock and before
the save, that returns on it, and nothing the sweep asks of the other writers changed.

### Where the change differs from the brief

- The quiet load needed quiet variants of three of `internal/config`'s own steps, beside the
  holder's file and the new one: `applyDefaultsAs(warn)` in `config.go`, and
  `applyEnvOverridesAs`, `applyEnvValueAs` and `readSwitchEnvAs` in `env.go`, each wrapped by the
  old name with `warn` true. `Load`, `Validate`, `Edit` and the lock did not change.
- Two tests outside the brief's list pinned the refusal by an outside edit that changed no setting
  their write changes: `TestAPlayerWriteOverAnEditedConfigIsRefused` renamed the library, and
  `TestTheSettingsCarryTheUpdateCheckSwitch` appended a comment. Each outside edit now changes the
  setting its write changes (the player's block; `update.checkEnabled`), so each still shows the
  refusal it was written for, now a conflict. Both went green again in the admin suite.
- The merged load's refusal is a `*ChangedOutsideError` of its own kind, so every
  `ErrChangedOutside` consumer keeps its class.
- The budget's sizes, against `d4c5d062`: `internal/config`'s production code 359 lines in
  `outside.go` and +204/-68 in `runtime.go` (the brief said about 250, with the comments it carries
  here), +14/-6 in `config.go` and +39/-12 in `env.go`; `handlers_api.go` +90/-32 (about 40); tests
  about 990 lines added (about 500).

### The reds, at `d4c5d062`

Run by `go test -overlay` with the base's production files and a names-only shim (`OutsideKind` and
its constants, `ChangedOutsideError`'s `Kind`, `Settings` and `Err`, `settingsChanged` answering nil,
`outsideSettingMessage`), as predicted:
- `internal/config`: `TestASaveKeepsASettingAnotherWriterChanged` (R1, the first Update refused by
  the bytes), `TestASettingAnotherWriterChangedIsKeptPastASaveThatDidNotLand` (S1),
  `TestAWriterThatWritesOutDefaultsChangesOnlyWhatItChanged` both cases (R3 and review 1's R3),
  `TestASaveKeepsAPlayerBlockThatDoesNotReadAsAnotherWriterLeftIt` (R4),
  `TestASaveKeepsASerialAnotherWriterChanged` (review 1's R2), both cases of
  `TestASettingAVariableSetIsNoOtherWritersChange` (S4), `TestEachSettingIsFoundToDifferAlone` (R6,
  126 settings found as nothing through the shim) and the name case of
  `TestUpdateRefusesAFileAnotherWriterChanged` on its naming assertion alone (R2); green as
  predicted: `TestAMergedSaveTheNextStartWouldRefuseIsRefused` (R5 and S3, refused by the bytes),
  `TestADeploymentSettingAnotherWriterChangedRefusesEverySave` (R11), all five cases of
  `TestARefusedUpdateRunsNoneOfItsChange` (R7) and `TestConfigCloneIsDeep`.
- `internal/admin`: `TestARootAddedBesideAnotherWritersChangeKeepsIt` (R9: 500, want 201, the wipe
  never ran), and the naming rows of `TestAConfigSaveRefusalSaysWhatToDoAndNamesNoPath` and
  `TestTheConsolesSavesAnswerARefusalWithoutThePath` (S6); green: the three cases of
  `TestARootChangeTheSaveRefusesLeavesTheLibraryAlone` (R8),
  `TestARootAddWhoseFileTheNextStartWouldRefuseWipesNothing` (review 1's R1),
  `TestAThirdRootWhoseFileTheNextStartWouldRefuseIsRefused` (S2), and the two tests above.
- `cmd/bridge`: `TestServeMergesASettingsSaveIntoAConfigAnotherWriterChanged` (R10, 500) and its
  twin `TestServeRefusesASettingsSaveOfASettingAnotherWriterChanged` on its naming assertion alone.

R1 logs one leaf, `scanIntervalSec`, which the test asserts, with no value; each case of R3 logs one,
`scanIntervalSec` (seen in the cases' `-v` output): the defaults, the resolved paths and the library
name `config.Edit` wrote recorded nothing.

### Controls

Each ran at `7350d847`, the mutation laid over the committed file by `go test -overlay` (no tracked
file written, `git status` clean after each), with `-count=1 -timeout 30m -vet=off -v`. CONFIG is
the whole `internal/config` package; ROOTS is `internal/admin`'s six root-change tests
(`TestARootChangeTheSaveRefusesLeavesTheLibraryAlone`, `TestARootAddedBesideAnotherWritersChangeKeepsIt`,
`TestARootAddWhoseFileTheNextStartWouldRefuseWipesNothing`,
`TestAThirdRootWhoseFileTheNextStartWouldRefuseIsRefused`,
`TestARootChangeRefusedJustBeforeItsIndexChangeLeavesTheLibraryAlone`,
`TestARootChangeRefusesANameRepointedInItsTurn`); SERVE is the two serve tests above. Every test a
row's lists ran and the row does not name stayed green, as predicted.

| | mutation | runs | predicted and seen |
| --- | --- | --- | --- |
| C1 | a landed save clears the record | CONFIG | R1 red at its second step alone: the file loads 21600, not 7200 |
| C2 | the conflict refusal off | CONFIG, SERVE | R2's name case red at the refusal, the file written over and "Renamed" stored, green at the file's Away (review 1's R6); R7's conflict case at its refusal and its work; serve's twin at the 200, its words and the file |
| C3 | `readSettings` without defaults or paths | CONFIG | R3's listen address refused, naming `listenAddress`; the library name green |
| C3b | `readSettings` without normalization | CONFIG | R3's library name refused, naming `libraryName`; the listen address green |
| C4 | an unread player block walked by its fields | CONFIG | R4 red at the block's `noSuchSetting` 1, not 2 |
| C5 | the merged load skipped | CONFIG | R5's variants case red at four (the refusal, the file written, the change stored, the file no longer loading); its OpenHome case at three, the load green |
| C6 | lists of structs left out of the walk | CONFIG | R6 red at `upnpUpstream.servers` alone |
| C7 | `check` asks `FileLock.Check` alone | CONFIG, ROOTS | R7's conflict case red at its work alone; R8's three cases and review 1's R1 red at the rendition unlinked alone |
| C8 | the root list set after `check` and the index change again | ROOTS | as C7's ROOTS |
| C9 | a recorded deployment leaf refuses nothing | CONFIG, `TestTheConsolesSavesAnswerARefusalWithoutThePath` | R11 red at both refusals, the file written and the change stored; R7's deployment case at its refusal and its work; the console's deployment case at 200, its words, its log line and its name |
| X2 | `check` lets a merged file the next start would refuse through | ROOTS | review 1's R1 red at the rendition unlinked alone |
| X3 | the check once `fn` returns skipped | the third root; R5; R2 | the third root red at 201, the file written, the roots moved and stored; R5 as C5; R2 as C2 |
| X4 | no type is one leaf of its own | CONFIG | the serial test red at the boot serial written; R6 at `output.player.device.serial` alone |
| X5 | the record put back when `fn` refuses, the bytes kept | CONFIG | S1 red at 21600, not 7200 |
| X6 | a variable's leaf recorded like any other | CONFIG | S4's two cases refused, naming `enrich.enabled` |
| X7 | a conflict answered without its leaves | the console's words; SERVE | the conflict row at its two names and the UPnP wrap at its one; serve's twin at its words alone |
| X8 | no OpenHome port clash in the merged load | CONFIG | R5's OpenHome case red at three, the load green |
| X9 | the merged load loud | CONFIG | `TestTheMergedFileIsLoadedAsTheNextStartLoadsItWithoutAWord` red at its words in four cases, the unread block also at the player-off reason; the fraction and the unknown key green |

### Runs

Before the build-slot session, at the code commit's tree: `go vet` of `internal/config`,
`internal/admin` and `cmd/bridge` for linux/amd64, windows, darwin and linux/386, exit 0; the pinned
gofmt (`go run cmd/gofmt -l`) over every changed Go file, nothing listed; the whole non-race suites,
one package at a time with `-count=1 -timeout 45m` (a longer bound than the brief's, never reached):
`internal/admin` ok in 274.3 s, `cmd/bridge` 587.2 s, `internal/updater` 25.4 s, `internal/api`
39.0 s.

The build-slot session, one script at `0bd71123`, each step's command line and answer logged, the
slot free at once:
1. `GOOS=windows go vet -tags ts_omit_webclient,ts_omit_debugeventbus ./internal/config/
   ./internal/admin/ ./cmd/bridge/` and the same with `GOOS=darwin`: exit 0, nothing said.
2. `make fmt vet docs-check`: exit 0, the tree unchanged after it.
3. The B535 tests, each package alone, `go test -tags ts_omit_webclient,ts_omit_debugeventbus -race
   -count=20 -timeout 30m -v`, with a `TMPDIR` of their own: `internal/config`'s 13 (R1 to R7,
   R11, the serial, S1, S3, S4 and `TestConfigCloneIsDeep`), 260 of 260 top-level runs and 400
   subtests passed in 8.8 s; `internal/admin`'s 10 (R8, R9, review 1's R1, S2, the two tests of
   S6, the two whose fixtures changed, and B506's two root-change tests that ask `check`), 200 of
   200 and 760 subtests in 432.8 s; `cmd/bridge`'s 4 (R10 and its twin, the census and its
   self-test), 80 of 80 in 76.1 s. None failed, and no data race.
4. `make fmt vet test build-all` at its defaults but `TMPDIR` (below the checkout's `.tmp/`),
   whose test step is `go test -tags "ts_omit_webclient,ts_omit_debugeventbus" -p 4 -race -timeout
   30m ./...`: exit 0 in 2,145.8 s, the tree unchanged after it. Its test step: 91 packages ok,
   none failed; `cmd/bridge` 1,726.0 s of its 30 minutes, `internal/manifest` 1,615.6 s,
   `internal/admin` 1,237.6 s, `internal/backup` 660.8 s, `internal/config` 16.6 s. Its
   `build-all`: check-build-tags OK, the six release binaries built.

A record holds no run made on its own commit: `make docs-check` and the citation guard on this
record's commit are in the report that carries it.

### What stays open

- A console save still writes `--addr`'s value and the live defaults and resolved paths into the
  file for a leaf no other writer changed, as before (the brief's P10, out of scope).
- The merged load is `Load`'s, not serve's second line (`serveConfigRefusal`: two roots with one
  folder name, the Tailscale mode). Only another writer's own roots can trip the first, since a
  console change of the roots meets a recorded root list as a conflict.
- A recorded leaf stays recorded until a restart even when another writer puts it back, so the
  console then refuses a change of it until the restart.
- The console says nothing of a recorded leaf before a save meets it: the Info line in the log is
  all, and the Settings page shows the live value, not the file's.
