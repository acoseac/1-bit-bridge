## 2026-11-19 — B894: the crash rollback asks its backup before the install marker's lock (B898's F1)

Branch `fix/b894-rollback-lock-bound`, from main `f3255e6a` (five commits past
`6282e5a6`, the premise's base; the lines below are `f3255e6a`'s, which match
the premise's): `91cc0b1f` the tests, red; `ca1ddc11` the fix; then the
records. The rule is `config-lifecycle.md`'s B894 line, its long form the
section "The crash rollback asks its backup before the install marker's lock"
in `ops/History-rules-config-lifecycle.md`.

### The hold, before and after

- At the base `recoverCrashedInstall` took `update-state.lock` at
  `serve_crashed_install.go:100`, held it to its return (`defer` at `:110`),
  and ran under it the backup's `version` (`:141`: 10 s, 12 s once killed
  with its pipes' wait) and its settings check (`:155`: 30 s, 32 s once
  killed, `updater.ConfigCheckBound`), then the record (0.75 s of rename
  retries), the restore (none on Unix, 7.9 s of waits on Windows) and, after
  a failed restore, the marker put back (0.75 s). Every other taker waits
  30 s.
- The pin, `TestEveryRollbackAtAStartFitsInsideTheMarkersLockWait`, summed the
  crash row at 51.4 s (2 × 0.75 + 7.9 + 10 + 32) and asserted it past the
  wait (`past: true`). Reachable worst: 49.4 s on Windows (both checks at
  their bounds, the place refused, the put-back at its last attempt, the
  marker put back), 42.75 s on any OS for a check that does not answer, and
  32.75 s for its likeliest form, a fast version beside a check that hangs,
  at every start of a crash loop.
- After: both runs come before the lock, and the row is 2 × 0.75 + 7.9 =
  9.4 s, the operator's rollback's figure. The pin counts a run's bound only
  where that run found the lock held: the crash row asks for no bound and
  `past: false`; the early and the boot rows keep `[VersionProbeTimeout]`
  (19.4 s), their asks still under the lock.

### The decision

Option A: the runs before the lock, the decision made again under it, as
`Updater.Install` asks its candidate before the lock and decides again under
it. Rejected: a longer wait (one constant serves every taker, none can tell
who holds a kernel lock, every CLI taker would wait silently behind any
holder, the wait would follow the backup's timeouts, and a child would still
run under the lock), and documenting the overrun (the pin's exception kept for
good, the hold past the wait at every start whose check hangs). The long form
says the rest: what can change between the runs and the lock and what catches
it, the identity on Windows, cancellation, and the recording of a refusal
decided before the lock.

### The reds at the base

Predicted before the run, every prediction met. `go test -count=1 -v` of the
seven tests at `91cc0b1f` over `f3255e6a`'s code; each case's first failure
line, its stderr elided (line numbers are the test files' at `91cc0b1f`):

| Case | At the base |
| --- | --- |
| `TestTheCrashRollbackAsksItsBackupBeforeItTakesTheMarkersLock` | `serve_crash_rollback_lock_test.go:200: the backup was asked ["the backup's version with the install marker's lock held" "the backup's settings check with the install marker's lock held"]; want […lock free…]`; the rollback itself put the backup back (75) |
| `TestEveryRollbackAtAStartFitsInsideTheMarkersLockWait` | `serve_held_binary_test.go:858: the crash rollback made 2 writes, waited 7.9s and asked within [10s 32s]; its row counts 2, 7.9s and []` |
| `TestABackupChangedWhileItWasCheckedIsNotPutBack`, its four (replaced during its settings check; replaced between its version and its check; rewritten to another size, its time kept; its modification time alone moved) | `:321: the crash rollback = 75 (acted true) after 1 renames, want it to leave the install`, then not said, not recorded, the binary replaced, a rollback under way |
| `TestTheBackupIsNotPutBackOverSettingsItDidNotCheck`, its three (a comment appended while it checked them; gone while it checked them; unreadable before its runs) | `:378: the crash rollback = 75 (acted true) after 1 renames, …` |
| `TestTheCrashRollbackDecidesAgainOnTheMarkerItReadsUnderTheLock`: another install's marker | `:415: … = 75 (acted true) …`, the marker the case wrote overwritten by the rollback's record |
| the same: another version replaced; the install made again | `:450: … = 75 (acted true) …` |
| `TestAStopAroundTheBackupsChecksWritesNothing`: in the backup's version | `:501: update-state.json changed (<nil>): it must be left as it was` (the base recorded "the backup did not say its version"; the line was said) |
| the same: in the backup's settings check | green, as predicted: the base's own exception, said and not recorded |
| the same: as the settings check answered | `:495: the stopped crash rollback = 75 (acted true) after 1 renames, …` |
| `TestTheCrashRollbackFollowsAnInstallMadeWhileItsBackupWasChecked`: the install lands | `:545: the crash rollback over the install that landed = 75 (acted true) after 1 renames, …`, the install's marker and binaries moved again |
| the same: the install's swap is refused | green, as predicted: the rollback goes on (75), as it must after the fix |

The runs' stand-ins ask whether the lock is held by trying it through an open
of the lock file of their own: `fsutil.TryLock` is flock on unix and
LockFileEx on Windows, the lock of an open file, which refuses a second open in
one process (`TestTryLockRefusesASecondOpenOfTheFile`, run on every CI leg);
the reds above were run on Linux. A take is let go at once, and the file is
closed either way. The backup in these cases is a file of its own, not
`installBridgeCopy`'s hard link to the test binary, and a replacement holds its
bytes and its modification time, its identity asserted distinct before any
change, so that only the identity check tells them apart.

### The fix

`ca1ddc11`: the binary's path, the backup a regular file whose identity is
fixed through `fsutil.OpenAsFile` (its stat, the same file as the Lstat's,
closed at once), the settings' digest, the backup's version (held to the
unlocked read's `PreviousVersion`) and its settings check, all before
`LockState`; the lock's refusals and a filesystem that keeps none said as
before; the context asked once the lock is held; the marker read again and
`CrashRollback` again; then, nothing run again, the version against the
marker read again, the same install (`TargetVersion`, `AttemptedAt`), the
backup's identity (`os.SameFile` against the handle's, its size, its
modification time) and the digest read again; the record, the restore and
everything after unchanged. A refusal decided before the lock goes through
`refuseCrashRollbackBeforeTheLock`, which takes the lock (never while it is
held), reads the marker again, records the refusal where the same install
still asks, the marker's own why where it refuses, nothing for another
install, and says the line, recording nothing, where the lock cannot be held,
the marker cannot be read again or the start was stopped. The comments that
named the old span say the new one: the function's doc and
`recoverCrashedRefusedInstall`'s, `defaultStateLockWait`'s list,
`backupProbeBound`'s and `ConfigCheckBound`'s (the export kept: the pin's
stand-in still counts it where a check would run under the lock).
`checksoff.go`'s "in its order" still reads true (the marker's checks, then the
backup's file, then its answers) and is unchanged.

### The negative controls

Each a mutation of `serve_crashed_install.go` at `ca1ddc11`, laid over the
committed file with `go test -overlay` (the tracked file never edited, and
checked unchanged after each batch), predicted in writing before its run, the
seven tests run whole (C1 the two it is about):

| Control | Red | Green |
| --- | --- | --- |
| C1 the lock, its checks and the context check moved before the binary's path, so both runs are under the lock | the lock test (both "with the install marker's lock held"), the pin (`[10s 32s]` against `[]`) | — (only those two run: a refusal under C1 would take the lock again while holding it) |
| C2 no `os.SameFile` under the lock | replaced during its settings check, replaced between its version and its check | every other case |
| C3 no size compare | rewritten to another size, its time kept | every other case |
| C4 no modification-time compare | its modification time alone moved | every other case |
| C5 the digest compare never true | a comment appended while it checked them | every other case (gone, unreadable: their reads fail) |
| C6 no same-install comparison | the install made again | every other case |
| C7 no version compare under the lock | another version replaced | every other case |
| C8 the first identity a bare Lstat (no handle, no compare with one) | none, as predicted on Linux, where an Lstat carries its device and inode as it is taken | every case; its red (both replacements) is Windows' alone, not run here |
| C9 no context check after the main path's lock | as the settings check answered | every other case |
| C10 no context check after the helper's lock | in the backup's version (`:501`, the refusal recorded) | every other case |
| C11 both digest reads' errors ignored | gone while it checked them (refused, but as changed: `:379` wanted "could not be read again"); unreadable before its runs (75: two empty digests compare equal) | every other case |

C8 shows something more: the fix's own early `os.SameFile(fi, identity)` loads
the Lstat's file ID on Windows as it compares, so a control that only swapped
the handle's identity for the Lstat's under the lock would stay green even
there; C8 removes the handle and that compare together.

### Windows

- The identity is taken from a handle's stat, closed at once, before the runs
  and the restore (whose probes and renames need the deletion Go's `os.Open`
  on Windows does not share). Forcing the Lstat's lazy load at once would work
  too, but rests on Go keeping what `os.SameFile` loaded, which it does not
  document.
- C8's red needs a windows-latest run of a tree carrying the mutation; the
  branch's own gate shows the chosen check green (c1 and c6 among the cases).
- New, and accepted: an install whose swap comes while a run executes the
  backup is refused at its vacate on Windows (a rename onto a mapped image,
  Access is denied: `TestARenameOntoTheRunningImageIsAnsweredAtOnce`), its
  marker put back as it read it, nothing moved. Where it matters, a check that
  hangs, the install was refused before too, after its 30 s wait; it is refused
  at once now on Windows and lands elsewhere. Its case,
  `TestTheCrashRollbackFollowsAnInstallMadeWhileItsBackupWasChecked`, lays out
  both outcomes on every host.

### The records

The rule went into `config-lifecycle.md` beside B660's and B760's bullets, as
"…and" after them, with its `paths:` extended to
`cmd/bridge/serve_crashed_install.go` (`scripts/rules-for.py` now lists
`cli-serve.md` and `config-lifecycle.md` for it), since `cli-serve.md` had 281
bytes left and the rule's line is 1,031; `cli-serve.md`'s lock-span line (68)
says the new span and points at it. Both files stay under 100,000 bytes (99,825
and 99,783). The History file's B898 lines that named F1 as open (the crash
row, and its open list) and B760's narrative of the lock span are corrected.

### The runs

At `87bb0b29` (the records' head), one build-slot session, the slot free at
once:

- `make fmt` (the tree unchanged), `make vet` and `make docs-check`, and
  `GOOS=windows` and `GOOS=darwin` `go vet` of `./cmd/bridge/` and
  `./internal/updater/` with the tags: clean.
- The seven tests, `-race -count=20`: ok, 18.3 s. The crash-rollback suites
  (every test in `serve_crash_rollback_test.go`,
  `serve_crash_rollback_refused_test.go`, `serve_crash_rollback_lock_test.go`
  and `serve_held_binary_test.go`), `-race`, twenty runs as four lanes of five
  from one race binary, started 20 s apart: every lane PASS, 591 s (one run
  alone, 102 s).
- `make test` with `TEST_PKGS='-count=1 -timeout 45m ./internal/updater/'`: ok,
  41 s; with `./cmd/bridge/`: ok, 1,614 s. The Makefile's 30 m is close to
  cmd/bridge's time under `-race`, so these runs give it 45 m, as recent gates
  did.
- `make fmt vet test build-all P=8 TEST_PKGS='-count=1 -timeout 45m ./...'`: 91
  packages ok and none failed (cmd/bridge 1,753.8 s, manifest 1,647.1 s, admin
  1,275.8 s), the six targets built, 1,806 s.

`d2439800` then gave `crashInstallAtTheLimit` a layout of its own: it had
taken `newRefusedInstall`'s and removed the backup at once, which on Windows is
a copy of the test binary (about 89 MB) that a scanner may still hold as it is
removed. After it: `go vet` for linux, windows and darwin clean; the seven
tests and cmd/bridge's three doc guards `-race -count=20` ok; the eleven
controls again, the same reds and C8 green; and the reds over `f3255e6a`'s
`serve_crashed_install.go` (laid over with `go test -overlay`), the same
verdicts, each line 19 further down. A second slot session on `d2439800`:
`make vet` and `make docs-check` clean, and `make test` with
`TEST_PKGS='-count=1 -timeout 45m ./cmd/bridge/'` ok, 1,616 s. The commit
that adds this section changes this file alone; `make docs-check` and the
citation guard ran over it again.

### What stays open

- The early and the boot rollbacks still ask their backup's version under the
  lock (10 s each; their rows fit, 19.4 s).
- A step under way takes what its filesystem takes, which no schedule bounds
  (`statelock.go`'s list).
- `bridge update`'s words behind a crash loop say to try again once it is
  done, which may not come while the loop goes on; stopping the service first
  ends it.
- No taker's wait knows its holder (a per-taker or holder-aware wait).
- One binary shared by two data dirs, which the marker's lock never covered.
- B894 and B898's F1 are one hazard under two backlog ids.
- B898's other follow-ups (F2, F3, F4, F7, F8, F9) are as they were.
- C8's red on windows-latest, from a tree carrying the mutation.
