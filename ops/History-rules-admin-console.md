# Long form: entries moved from `.claude/rules/admin-console.md`

> Moved verbatim from `.claude/rules/admin-console.md` on 2026-11-16 so it stops loading on its own. **These entries are still binding.** The source keeps each one as a single line ending `→ History-rules-admin-console.md`; change an entry here and its line there in the same commit. Each `##` names the section the entries came from.

## Admin console and the web player

- **A restore puts the file back under the root it was trashed from**
  (2026-10-09, backlog B177). `Restore` mapped the stored manifest path
  through `SplitRoot` of the roots configured now. That path is
  `<basename>/…` with more than one root and the path within the root
  with one, so after a collapse `Music/Artist/Album/01.flac` landed at
  `<surviving>/Music/Artist/Album/01.flac` and still reported restored,
  and after a second root was added a file that lived in a top folder
  named like the root landed one level too high. Trash writes a sidecar
  beside the audio file, `.<name>.bridge-root`, holding the absolute
  root `SplitRoot` returned and the path relative to that root. Restore
  builds the manifest path for the root count in force now from that
  pair and takes the destination from `Resolver.Resolve`. A recorded
  root that `EvalSymlinksOrClean` does not match to a configured root
  answers `ErrRootUnavailable` ("the library root this path belongs to
  is not configured") and the file stays in the trash; a renamed root
  path is that refusal. A record that does not parse, or whose root and
  suffix do not resolve back to themselves, is refused as unreadable.
  An entry with no sidecar was written before the root was recorded.
  When its first segment equals the basename of the root the file sits
  under, restore refuses: that segment is either the old multi-root
  prefix or a real folder of that name, and the entry does not say
  which. The same refusal covers a single-root library whose top folder
  is named exactly like the root. When the first segment equals another
  configured root's basename, the older "different library root" refusal
  stays. Otherwise the whole stored path is the path within the root,
  and it is restored under the root the file sits under, through the
  resolver, in the current layout. List still shows a refused entry.
  The cost is that a trash written before this change under more than
  one root, and one whose path begins with its root's name, is put back
  by hand from `.bridge-trash`. The sidecar's name begins with a dot, so
  List and the sweep's byte count skip it, and a successful restore or
  purge removes it before the empty stamp is pruned. The sidecar path
  that is written or removed is the trash directory joined with the
  relative remainder of the sidecar, and a remainder that leaves the
  trash directory is refused. A recorded root that is not an absolute
  path is an unreadable record. A trash whose record cannot be written
  attempts to move the file back. When that move succeeds, the empty
  stamp directory is removed and the entry fails with the record reason.
  When the move back fails, the file stays in `.bridge-trash`, a warning
  is logged, and the entry fails with a reason that says it is still in
  the trash. That result still carries the spelled path and the folder,
  as a completed trash does, so the handler retires the row and rescans;
  a spelling that could not be read is a full scan. A file that came back
  records neither.
  (`TestRestoreAfterARootCollapsePutsTheFileBackUnderTheSurvivingRoot`,
  `TestRestoreAfterARootIsAddedKeepsAFolderNamedLikeTheRoot`,
  `TestRestoreThroughTheAPIFollowsTheRootTheFileWasTrashedUnder`,
  `TestRestoreRefusesAnEntryWhoseRecordedRootIsGone`,
  `TestRestoreRefusesAnEntryRecordedUnderADifferentRoot`,
  `TestAnOldEntryWhosePathStartsWithTheRootNameIsRefused`,
  `TestAnOldEntryThatNamesNoRootBasenameRestoresUnderTheRootItSitsUnder`,
  `TestATornRootRecordIsRefused`,
  `TestAFailedRootRecordMovesTheFileBackAndRemovesTheStamp`,
  `TestAFailedRootRecordWhoseMoveBackFailsLeavesTheFileInTheTrash`,
  `TestAFileLeftInTheTrashRetiresItsRowAndRescansItsFolder`,
  `TestAnOriginRecordOutsideTheTrashIsNotWritten`,
  `TestAnOriginRecordOutsideTheTrashIsNotRemoved`,
  `TestARecordedRootThatIsNotAbsoluteCannotBeRead`.)

- **`/api/jobs` is guarded in BOTH directions, down to the LEAF.**
  `TestSettingsPrereqsOnlyReadRealJobsFields` walks JS→Go (every `jobs.<field>`
  names a real field) and cannot see a field nobody reads;
  `TestEveryJobsFieldIsRenderedSomewhere` walks Go→JS. #891's stated purpose was
  "a Jobs card" and it touched no template and no JS — the endpoint ran a
  full-table scan every thirty seconds for numbers no pixel consumed, and the DTO
  test passed. **The first version of that guard walked the twelve CONTAINERS
  and was satisfied by `j.lyrics` alone** — so "`lyrics` was the only field with
  zero reads" was true at the only level it looked, and the nine numbers inside
  were as unguarded as `lyrics` had been. Walked recursively it found six more
  leaves marshalled for nobody (`scanner.isScanning`, `scanner.lastFullScan`,
  `enrichment.source` — all read off `/api/stats` instead — plus
  `analysis.intervalSec`, `coverage.totalLocal`, `smartMixes.analysisAssisted`).
  Two mechanisms make the recursive form work, and both are negative-controlled:
  it resolves the JS's local and parameter aliases (`const lyr = j.lyrics`;
  `renderAnalysisCoverage(an.coverage)` → `cov.eligible`), and it strips
  COMMENTS ONLY — most nested reads sit inside `${…}` template interpolations,
  which `stripJSNoise` blanks. Recursion stops at the exported shared types
  (`*JobRunState` and kin), which have other consumers. **A guard that checks
  containers proves nothing about their contents.** One of those stops is
  `*AnalysisSweepState`, and a field added to its `last` breakdown and never
  rendered leaves this guard green (measured on #992), so that breakdown is
  pinned the other way: `TestDescribeAnalysisSweepAccountsForEveryTrack`
  executes the line that renders it. **A leaf the server sends as a KEY
  for the console to word needs its wording pinned too**: the guard proves
  `maintenance.orphanSidecarGCRefusal` is read, not that every value it
  can take has words, so `TestEveryOrphanRefusalKindIsWorded` runs
  `describeOrphanGCRefusal` under node for each
  `integrity.OrphanRefusalKinds()` entry (2026-09-28), and refuses one that
  comes back blank, `undefined` or `null` (what `console.log` prints for a
  case that returns nothing), as its key, as the fallback, or in another
  kind's words. **And a list that stands for every value is pinned to the
  declarations**: `TestEveryOrphanRefusalKindIsListed` reads the integrity
  package's source for every constant of type `OrphanRefusalKind` and
  requires `OrphanRefusalKinds()` to hold exactly those, since a kind the
  list omits is worded by nobody and the wording test passes over it.
  The variant watcher's key (`maintenance.variantIntegrityRefusal`,
  2026-09-29) has the same pair, `TestEveryVariantRefusalKindIsWorded` and
  `TestEveryVariantRefusalKindIsListed`, over the same helpers
  (`requireEveryRefusalKindIsWorded`, `requireEveryKindIsListed`): **a
  third refusing sweep takes both helpers, not copies of them.**

- **The player bar's control routes (B634, `handlers_player_control.go`) refuse where no player is offered (no control wired, a public bridge, a host that is not Linux, a boot plan that lists none) before anything is read, asking neither the player block's Unread nor the managed settings; take one JSON object of at most 8 MiB, at most 20,000 paths of 4,096 bytes, each bound its own refusal; answer a command's record under the publication captured after it; and the event stream sends the player frame only where a player is offered.** The eight routes are GET `now`, `queue` and `operation`, and POST `queue` (a replace: an album or a playlist by its id, expanded on the server, or a list of paths), `queue/append`, `queue/delete`, `transport` and `switch-out`, all under `/api/output/player/`; each refusal says no player is offered here (`player_not_wired`, `player_not_available`, `player_not_supported`, `player_not_offered`), which GET now answers at once, so the bar tells a bridge that offers none from a stream not yet open. A long playlist travels by its id, not as its paths. A command's identity is checked by its shape (a tab of 32 lower-case hexadecimal digits, a count above 0, an epoch); its digest is the route and the command's decoded fields without the identity. A catalog row carries `bridge`, how the bridge's own player plays it (`BridgeWayOf`, the one function a replace's checks run too), while the player runs, and none otherwise. **Don't take the console's lock or bridge.yaml's in a control route, write the config from one, or refuse a Stop over an unreadable player block.** `TestTheControlRoutesRefuseWhereThePlayerCannotRun`, `TestAnUnreadablePlayerBlockStopsNothingTheControlAsks`, `TestTheControlRoutesAreGuardedLikeEveryConsoleWrite`, `TestAControlBodyHasBoundsOfItsOwn`, `TestEveryControlAnswerHasItsStatus`, `TestEachExcludedDeploymentSaysNoPlayerIsOfferedHere` and `TestTheCatalogSaysWhatTheBridgesPlayerPlays` pin it.
