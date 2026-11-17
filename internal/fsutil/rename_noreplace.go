package fsutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrNoReplaceUnsupported is what RenameNoReplace's answer matches where no
// rename that never replaces its destination can be made: the filesystem
// does not take the call's flag, the kernel has no such call, or this
// package makes none on the platform. Nothing was renamed. It matches
// errors.ErrUnsupported, and the answer carries the call's own errno beside
// it, which errors.Is matches too.
var ErrNoReplaceUnsupported = fmt.Errorf("this filesystem has no rename that never replaces its destination: %w", errors.ErrUnsupported)

// RenameNoReplace renames from to to only while nothing is at to, the check
// and the rename made by one system call, so that an entry another program
// makes at to, however close to the rename, is never replaced: on Linux
// renameat2 with RENAME_NOREPLACE, on macOS renamex_np with RENAME_EXCL, and
// on Windows MoveFileEx with neither MOVEFILE_REPLACE_EXISTING nor
// MOVEFILE_COPY_ALLOWED. Whatever is at to counts: a file, a directory, a
// link, a link to nothing.
//
// It answers nil once the rename has landed: to names what from named, and
// from is gone. Any other answer renamed nothing, both entries as they were,
// and is an *os.LinkError naming the rename, as os.Rename's is, which
// carries the call's own answer:
//
//   - an entry at to: EEXIST, or on Windows ERROR_ALREADY_EXISTS or
//     ERROR_FILE_EXISTS, each of which syscall.Errno reads as fs.ErrExist;
//   - no such rename here: EINVAL, ENOSYS, ENOTSUP and EOPNOTSUPP on Linux
//     and macOS, ERROR_NOT_SUPPORTED and ERROR_INVALID_FUNCTION on Windows,
//     any answer that matches errors.ErrUnsupported, and every answer on
//     another platform, each matching ErrNoReplaceUnsupported as well;
//   - from and to on two filesystems: EXDEV, or ERROR_NOT_SAME_DEVICE on
//     Windows, as it is, never ErrNoReplaceUnsupported, and nothing copied;
//   - a path holding a NUL byte: EINVAL, as os.Rename answers it, with no
//     call made;
//   - anything else (no source, a folder that refuses the write): the
//     call's answer as it is.
//
// Linux also answers EINVAL for a directory renamed into itself, which reads
// here as no such rename; the restore it is made for renames a file.
//
// There is deliberately no fallback of any kind, neither another rename nor
// a copy (backlog B653 PR 2a). An Lstat that finds nothing at to,
// then a rename, is not a rename that never replaces: another program can
// make the entry between the two calls, the bridge's advisory locks do not
// bind another program, and the window between the two is real. Put in the
// call's place, that pair replaced the entry a creator racing it made in 459
// to 511 of 1,000 rounds on ext4 and in 360 to 395 on tmpfs (three runs
// each, Linux 7.0, 2026-11-15, the race row of
// TestARenameThatNeverReplacesLeavesAnEntryAtItsTarget), where the call
// itself lost none. Where the call is unsupported the answer says so, and
// the caller decides.
//
// It syncs nothing: a caller that needs the rename to outlive a crash syncs
// to's directory (SyncParentDir) once it has landed. Nothing in production
// calls it until backlog B653 PR 2b, the database-only restore, which puts
// the database it restored in place with it.
func RenameNoReplace(from, to string) error {
	return RenameNoReplaceCtx(context.Background(), from, to)
}

// RenameNoReplaceCtx is RenameNoReplace whose tries ask ctx: before each, the
// first included, and in each wait between them. A context that has ended
// renames nothing more and answers its own error, ctx.Err(). The ask and the
// call after it are two steps: ctx can end between them, and that try still
// runs.
//
// Only Windows tries again, and only where MoveFileEx answers a sharing
// violation or a refused access (ERROR_SHARING_VIOLATION,
// ERROR_ACCESS_DENIED), the answers it gives for a moment while an antivirus
// or the search indexer holds a file just written: atomicwrite's
// RenameWithRetry tries the two again for that reason, and these tries keep
// its schedule (noReplaceTries). Past it the last answer is returned, as
// RenameNoReplace returns it, so a refused access that lasts, a folder this
// user may not write, is returned only once the schedule has run, as
// RenameWithRetry returns it. Every other answer, and every answer on Linux
// and macOS, is final at its first try.
func RenameNoReplaceCtx(ctx context.Context, from, to string) error {
	if strings.IndexByte(from, 0) >= 0 || strings.IndexByte(to, 0) >= 0 {
		// os.Rename's answer for such a name. The call is never asked: the
		// conversion of the name refuses it with EINVAL, which would read
		// as no such rename.
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EINVAL}
	}
	var answer error
	for try := range noReplaceTries {
		if wait := noReplaceWait(try); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if hook := beforeNoReplaceRename.Load(); hook != nil {
			(*hook)(from, to)
		}
		if answer = noReplaceRenameCall(from, to); answer == nil || !noReplaceTransient(answer) {
			break
		}
	}
	return noReplaceAnswer(from, to, answer)
}

// noReplaceTries is the most tries RenameNoReplaceCtx makes, trying again
// while the call's answer is transient: atomicwrite.RenameWithRetry's
// schedule, copied because atomicwrite imports this package (its
// renameBackoff: five tries, the first at once and the others after 50,
// 100, 200 and 400 ms). The second try waits noReplaceFirstWait and each
// later one twice the wait before it, noReplaceBudget in all, and the
// schedule row of TestARenameThatNeverReplacesLeavesAnEntryAtItsTarget reads
// renameBackoff from atomicwrite's source to hold the two alike.
const (
	noReplaceTries     = 5
	noReplaceFirstWait = 50 * time.Millisecond
	noReplaceBudget    = 750 * time.Millisecond
)

// noReplaceWait is the wait before try, counted from 0: none before the
// first, noReplaceFirstWait before the second, and twice the wait before it
// before each after that.
func noReplaceWait(try int) time.Duration {
	if try == 0 {
		return 0
	}
	return noReplaceFirstWait << (try - 1)
}

// beforeNoReplaceRename runs, where a test has set it, just before each
// try's call with the paths that try renames, so that what it makes there
// meets the call itself, as another program's create would. Nil in
// production. It and noReplaceRenameSys are atomic pointers, so a test that
// sets one while another goroutine renames stays clean under the race
// detector.
var beforeNoReplaceRename atomic.Pointer[func(from, to string)]

// SetBeforeNoReplaceRenameForTest installs fn as beforeNoReplaceRename and
// returns what puts the one before it back: exported from a production file
// so that another package's test can make an entry at a rename's target just
// before the call (backlog B653 PR 2b1: the database-only restore never
// renames over one). Production code never calls it. Process-global: a test
// that uses it must not call t.Parallel(), and puts it back once every
// rename it watches has ended.
func SetBeforeNoReplaceRenameForTest(fn func(from, to string)) (restore func()) {
	var p *func(from, to string)
	if fn != nil {
		p = &fn
	}
	prev := beforeNoReplaceRename.Swap(p)
	return func() { beforeNoReplaceRename.Store(prev) }
}

// noReplaceRenameSys answers, where a test has set it, in place of the call:
// nil, or the errno the call would answer, so that answers one host cannot
// give (a filesystem without the flag, a second filesystem, a sharing
// violation) are driven on any. Nil in production, where each try makes the
// platform's call, platformRenameNoReplace.
var noReplaceRenameSys atomic.Pointer[func(from, to string) error]

// noReplaceRenameCall is one try's call: the platform's, or the answer a
// test put in its place.
func noReplaceRenameCall(from, to string) error {
	if sys := noReplaceRenameSys.Load(); sys != nil {
		return (*sys)(from, to)
	}
	return platformRenameNoReplace(from, to)
}

// noReplaceAnswer is what RenameNoReplace returns for the call's answer: nil
// for nil, and otherwise an *os.LinkError naming the rename, as os.Rename
// names it, whose Err is the answer, matched beside ErrNoReplaceUnsupported
// where the answer says no such rename can be made here.
func noReplaceAnswer(from, to string, answer error) error {
	if answer == nil {
		return nil
	}
	if noReplaceUnsupported(answer) {
		answer = fmt.Errorf("%w (%w)", ErrNoReplaceUnsupported, answer)
	}
	return &os.LinkError{Op: "rename", Old: from, New: to, Err: answer}
}
