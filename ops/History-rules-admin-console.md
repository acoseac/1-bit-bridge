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
