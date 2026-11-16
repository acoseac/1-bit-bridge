package fsutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestARenameThatNeverReplacesLeavesAnEntryAtItsTarget pins RenameNoReplace:
// an entry at its destination, there before the call, made just before it or
// made by a creator racing it, is never replaced, and every other answer is
// the one its doc names, with nothing renamed and nothing copied. The rows
// that rename for real run in the test's temporary directory and, on Linux,
// in /dev/shm, a tmpfs; the answers one host cannot give are driven through
// the call's seam (noReplaceRenameSys). Windows' own rows, a held source
// tried again, are in rename_noreplace_windows_test.go.
func TestARenameThatNeverReplacesLeavesAnEntryAtItsTarget(t *testing.T) {
	if !noReplaceHasACall() {
		t.Run("a platform with no such call renames nothing", noReplaceNoCallRow)
		return
	}
	for _, fsys := range noReplaceFilesystems() {
		t.Run(fsys.name, func(t *testing.T) {
			t.Run("an occupied destination is refused", func(t *testing.T) {
				noReplaceOccupiedRow(t, fsys.dir(t))
			})
			t.Run("a link to nothing at the destination is refused", func(t *testing.T) {
				noReplaceDanglingLinkRow(t, fsys.dir(t))
			})
			t.Run("a destination made just before the call is refused", func(t *testing.T) {
				noReplaceMadeJustBeforeRow(t, fsys.dir(t))
			})
			t.Run("a creator racing the rename keeps its entry", func(t *testing.T) {
				noReplaceRaceRow(t, fsys.dir(t))
			})
			t.Run("a free destination takes the rename", func(t *testing.T) {
				noReplaceFreeRow(t, fsys.dir(t))
			})
		})
	}
	t.Run("an unsupported answer renames nothing", noReplaceUnsupportedRow)
	t.Run("a second filesystem's answer is passed on as it is", noReplaceCrossDeviceRow)
	t.Run("a context that has ended renames nothing", noReplaceEndedContextRow)
	t.Run("a path holding a NUL byte is refused as invalid", noReplaceNULRow)
	t.Run("the schedule is atomicwrite's", noReplaceScheduleRow)
	for _, row := range noReplacePlatformRows {
		t.Run(row.name, row.run)
	}
}

// noReplaceRaceRounds is how many rounds the race row runs on each
// filesystem.
const noReplaceRaceRounds = 1000

// noReplaceRaceSpread is how many head starts the race row sweeps, from none
// up, each one noReplaceSpin's unit longer than the one before it.
const noReplaceRaceSpread = 64

// noReplaceAwait waits for release, yielding while it waits, so that a
// waiting goroutine holds no processor another needs.
func noReplaceAwait(release *atomic.Bool) {
	for !release.Load() {
		runtime.Gosched()
	}
}

// noReplaceSpinCount is what noReplaceSpin counts on, so that its loop is
// not optimized away.
var noReplaceSpinCount atomic.Int64

// noReplaceSpin spins for units of 64 atomic additions each, a fraction of
// a microsecond a unit without the race detector, a head start that makes
// no system call.
func noReplaceSpin(units int) {
	for range units * 64 {
		noReplaceSpinCount.Add(1)
	}
}

// noReplaceErrno is an answer of the call that a seam row drives, with the
// name its subtest takes.
type noReplaceErrno struct {
	name string
	err  error
}

// noReplaceErrnos are the call's answers the seam rows drive: those that say
// no such rename can be made here, and the one for a destination on another
// filesystem. Linux's and macOS's are written here, apart from the code's own
// reading of them, so that one the code misses fails;
// rename_noreplace_windows_test.go puts Windows' in their place.
var noReplaceErrnos = struct {
	unsupported []noReplaceErrno
	crossDevice noReplaceErrno
}{
	unsupported: []noReplaceErrno{
		{"EINVAL", syscall.EINVAL},
		{"ENOSYS", syscall.ENOSYS},
		{"ENOTSUP", syscall.ENOTSUP},
		{"EOPNOTSUPP", syscall.EOPNOTSUPP},
	},
	crossDevice: noReplaceErrno{"EXDEV", syscall.EXDEV},
}

// noReplaceRow is a row of TestARenameThatNeverReplacesLeavesAnEntryAtItsTarget
// that only some platforms run.
type noReplaceRow struct {
	name string
	run  func(t *testing.T)
}

// noReplacePlatformRows are the rows only this platform runs, which
// rename_noreplace_windows_test.go adds to on Windows.
var noReplacePlatformRows []noReplaceRow

// noReplaceHasACall reports whether this package makes a rename that never
// replaces on this platform: Linux's, macOS's and Windows' (Android and iOS
// build Linux's and macOS's files).
func noReplaceHasACall() bool {
	switch runtime.GOOS {
	case "linux", "android", "darwin", "ios", "windows":
		return true
	}
	return false
}

// noReplaceFilesystem is a filesystem the rows that rename for real run on:
// dir makes a fresh directory there for one row, removed at its end.
type noReplaceFilesystem struct {
	name string
	dir  func(t *testing.T) string
}

// noReplaceFilesystems are the filesystems those rows run on: the test's
// temporary directory's, and on Linux the tmpfs at /dev/shm, where it is
// there and this user can make a directory in it.
func noReplaceFilesystems() []noReplaceFilesystem {
	filesystems := []noReplaceFilesystem{{
		name: "in the test's temporary directory",
		dir:  func(t *testing.T) string { return t.TempDir() },
	}}
	if runtime.GOOS != "linux" {
		return filesystems
	}
	// Named without its path: a slash in a subtest's name splits it into
	// levels of its own for -run.
	return append(filesystems, noReplaceFilesystem{
		name: "on tmpfs",
		dir: func(t *testing.T) string {
			t.Helper()
			if info, err := os.Stat("/dev/shm"); err != nil || !info.IsDir() {
				t.Skipf("no /dev/shm here to rename in (%v)", err)
			}
			dir, err := os.MkdirTemp("/dev/shm", "fsutil-noreplace-")
			if err != nil {
				t.Skipf("/dev/shm takes no directory from this user: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			return dir
		},
	})
}

// noReplaceOccupiedRow pins the plainest refusal: a file at to before the
// call is refused as existing, at the first try, and both files hold
// afterwards the bytes they held before it.
func noReplaceOccupiedRow(t *testing.T, dir string) {
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	src := noReplaceWrite(t, from, "the source's bytes")
	dst := noReplaceWrite(t, to, "the destination's bytes, there first")
	tries := noReplaceCountTries(t, nil)
	err := RenameNoReplace(from, to)
	noReplaceRequireRefused(t, err, from, to)
	noReplaceRequireOneRealTry(t, tries)
	noReplaceRequireBytes(t, from, src)
	noReplaceRequireBytes(t, to, dst)
}

// noReplaceDanglingLinkRow pins that a link to nothing at to is an entry,
// which a check by Stat would take for nothing there: refused as existing,
// the link as it was, and nothing made where it leads.
func noReplaceDanglingLinkRow(t *testing.T, dir string) {
	from, to, nowhere := filepath.Join(dir, "from"), filepath.Join(dir, "to"), filepath.Join(dir, "nowhere")
	src := noReplaceWrite(t, from, "the source's bytes")
	symlinkOrSkip(t, nowhere, to)
	tries := noReplaceCountTries(t, nil)
	err := RenameNoReplace(from, to)
	noReplaceRequireRefused(t, err, from, to)
	noReplaceRequireOneRealTry(t, tries)
	if target, err := os.Readlink(to); err != nil || filepath.Clean(target) != nowhere {
		t.Fatalf("Readlink(%s) = %q, %v; want the link to %s as it was", to, target, err, nowhere)
	}
	noReplaceRequireAbsent(t, nowhere)
	noReplaceRequireBytes(t, from, src)
}

// noReplaceMadeJustBeforeRow pins that an entry the seam makes at to just
// before the call, where another program's create would land between a
// check and a rename, is refused as existing and kept as it was made, the
// source still where it was.
func noReplaceMadeJustBeforeRow(t *testing.T, dir string) {
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	src := noReplaceWrite(t, from, "the source's bytes")
	made := []byte("made at the destination just before the call")
	var madeAt string
	var madeErr error
	tries := noReplaceCountTries(t, func(try int32, _, at string) {
		if try == 1 {
			madeAt, madeErr = at, os.WriteFile(at, made, 0o600)
		}
	})
	err := RenameNoReplace(from, to)
	if madeAt != to || madeErr != nil {
		t.Fatalf("the seam made %q (%v); want %s made just before the call", madeAt, madeErr, to)
	}
	noReplaceRequireRefused(t, err, from, to)
	noReplaceRequireOneRealTry(t, tries)
	noReplaceRequireBytes(t, to, made)
	noReplaceRequireBytes(t, from, src)
}

// noReplaceRaceRow races a creator against the rename for
// noReplaceRaceRounds rounds, each with a fresh source holding bytes of its
// own and a fresh destination name. The rename and a creator, which makes
// the destination with O_EXCL and bytes of its own, each wait on a goroutine
// of its own for one release, and one of them then spins before it goes, by
// a head start that sweeps across the rounds, the two sides taking turns, so
// that the create lands before the rename's call, inside it and after it.
// Each round must end one of two ways, nothing lost: the rename landed (the
// destination holds the source's bytes, the source is gone, and the
// creator's create was refused as existing), or the rename was refused as
// existing (the destination holds the creator's bytes, and the source is
// still there with its own). Never both, and never the creator's bytes
// replaced.
func noReplaceRaceRow(t *testing.T, dir string) {
	var landed, refused int
	var lost []string
	for round := range noReplaceRaceRounds {
		from := filepath.Join(dir, fmt.Sprintf("from-%d", round))
		to := filepath.Join(dir, fmt.Sprintf("to-%d", round))
		src := noReplaceWrite(t, from, fmt.Sprintf("the source of round %d", round))
		mine := []byte(fmt.Sprintf("the creator's entry of round %d", round))
		head := (round / 2) % noReplaceRaceSpread
		var release atomic.Bool
		var ready sync.WaitGroup
		ready.Add(2)
		renamed := make(chan error, 1)
		created := make(chan error, 1)
		go func() {
			ready.Done()
			noReplaceAwait(&release)
			if round%2 == 0 {
				noReplaceSpin(head)
			}
			renamed <- RenameNoReplace(from, to)
		}()
		go func() {
			ready.Done()
			noReplaceAwait(&release)
			if round%2 == 1 {
				noReplaceSpin(head)
			}
			created <- noReplaceCreate(to, mine)
		}()
		ready.Wait()
		release.Store(true)
		err, createErr := <-renamed, <-created
		atTo, toErr := os.ReadFile(to)
		atFrom, fromErr := os.ReadFile(from)
		switch {
		case err == nil && errors.Is(createErr, fs.ErrExist) &&
			toErr == nil && bytes.Equal(atTo, src) && errors.Is(fromErr, fs.ErrNotExist):
			landed++
		case errors.Is(err, fs.ErrExist) && createErr == nil &&
			toErr == nil && bytes.Equal(atTo, mine) && fromErr == nil && bytes.Equal(atFrom, src):
			refused++
		default:
			lost = append(lost, fmt.Sprintf("round %d: the rename answered %v and the creator %v; "+
				"%s holds %q (%v) and %s holds %q (%v)", round, err, createErr, to, atTo, toErr, from, atFrom, fromErr))
		}
	}
	t.Logf("%d rounds: the rename landed in %d, the creator's entry was there first in %d, and %d lost an entry or answered otherwise",
		noReplaceRaceRounds, landed, refused, len(lost))
	if len(lost) > 0 {
		t.Fatalf("%d of %d rounds lost an entry or answered otherwise; the first: %s", len(lost), noReplaceRaceRounds, lost[0])
	}
}

// noReplaceFreeRow pins the rename itself: with nothing at to it lands, at
// the first try, to holding the source's bytes and from gone.
func noReplaceFreeRow(t *testing.T, dir string) {
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	src := noReplaceWrite(t, from, "the source's bytes")
	tries := noReplaceCountTries(t, nil)
	if err := RenameNoReplace(from, to); err != nil {
		t.Fatalf("RenameNoReplace(%s, %s) = %v; want the rename", from, to, err)
	}
	noReplaceRequireOneRealTry(t, tries)
	noReplaceRequireBytes(t, to, src)
	noReplaceRequireAbsent(t, from)
}

// noReplaceUnsupportedRow drives each of the platform's answers that say no
// such rename can be made here through the call's seam: each matches
// ErrNoReplaceUnsupported, errors.ErrUnsupported and the answer itself, is
// asked once, and renames nothing, so no fallback moved the source.
func noReplaceUnsupportedRow(t *testing.T) {
	for _, answer := range noReplaceErrnos.unsupported {
		t.Run(answer.name, func(t *testing.T) {
			dir := t.TempDir()
			from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
			src := noReplaceWrite(t, from, "the source's bytes")
			noReplaceAnswerWith(t, func(string, string) error { return answer.err })
			tries := noReplaceCountTries(t, nil)
			err := RenameNoReplace(from, to)
			noReplaceRequireLinkError(t, err, from, to)
			if !errors.Is(err, ErrNoReplaceUnsupported) || !errors.Is(err, errors.ErrUnsupported) || !errors.Is(err, answer.err) {
				t.Fatalf("RenameNoReplace = %v; want ErrNoReplaceUnsupported, errors.ErrUnsupported and %s, all matched", err, answer.name)
			}
			if errors.Is(err, fs.ErrExist) {
				t.Fatalf("RenameNoReplace = %v, which matches fs.ErrExist; want no such rename alone", err)
			}
			if n := tries.Load(); n != 1 {
				t.Fatalf("the call was asked %d times; want once", n)
			}
			noReplaceRequireBytes(t, from, src)
			noReplaceRequireAbsent(t, to)
		})
	}
}

// noReplaceCrossDeviceRow drives the call's answer for a destination on
// another filesystem through its seam: passed on as it is, the
// *os.LinkError's Err that very answer, never ErrNoReplaceUnsupported, asked
// once, and nothing renamed or copied.
func noReplaceCrossDeviceRow(t *testing.T) {
	answer := noReplaceErrnos.crossDevice
	dir := t.TempDir()
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	src := noReplaceWrite(t, from, "the source's bytes")
	noReplaceAnswerWith(t, func(string, string) error { return answer.err })
	tries := noReplaceCountTries(t, nil)
	err := RenameNoReplace(from, to)
	link := noReplaceRequireLinkError(t, err, from, to)
	if link.Err != answer.err {
		t.Fatalf("RenameNoReplace = %v; want %s passed on as it is", err, answer.name)
	}
	if errors.Is(err, ErrNoReplaceUnsupported) || errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("RenameNoReplace = %v, which reads as no such rename; want %s alone", err, answer.name)
	}
	if n := tries.Load(); n != 1 {
		t.Fatalf("the call was asked %d times; want once", n)
	}
	noReplaceRequireBytes(t, from, src)
	noReplaceRequireAbsent(t, to)
}

// noReplaceEndedContextRow pins that a context that has ended before the
// first try renames nothing and asks nothing: its own error, not the call's.
func noReplaceEndedContextRow(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	src := noReplaceWrite(t, from, "the source's bytes")
	tries := noReplaceCountTries(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := RenameNoReplaceCtx(ctx, from, to)
	var link *os.LinkError
	if !errors.Is(err, context.Canceled) || errors.As(err, &link) {
		t.Fatalf("RenameNoReplaceCtx(an ended context) = %v; want the context's own error", err)
	}
	if n := tries.Load(); n != 0 {
		t.Fatalf("the call was asked %d times; want none", n)
	}
	noReplaceRequireBytes(t, from, src)
	noReplaceRequireAbsent(t, to)
}

// noReplaceNULRow pins that a path holding a NUL byte is refused as
// os.Rename refuses it, EINVAL, before any call, and never read as no such
// rename, which the name's conversion's own EINVAL would read as.
func noReplaceNULRow(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	src := noReplaceWrite(t, from, "the source's bytes")
	tries := noReplaceCountTries(t, nil)
	for _, paths := range [][2]string{{from + "\x00", to}, {from, to + "\x00"}} {
		err := RenameNoReplace(paths[0], paths[1])
		noReplaceRequireLinkError(t, err, paths[0], paths[1])
		if !errors.Is(err, syscall.EINVAL) || errors.Is(err, ErrNoReplaceUnsupported) {
			t.Fatalf("RenameNoReplace(%q, %q) = %v; want EINVAL alone", paths[0], paths[1], err)
		}
	}
	if n := tries.Load(); n != 0 {
		t.Fatalf("the call was asked %d times; want none", n)
	}
	noReplaceRequireBytes(t, from, src)
	noReplaceRequireAbsent(t, to)
}

// noReplaceScheduleRow holds the schedule copied into noReplaceTries to
// atomicwrite's, read from atomicwrite's source (its renameBackoff), since
// atomicwrite imports this package and a test here cannot import it: the
// same waits in the same order, and noReplaceBudget their sum.
func noReplaceScheduleRow(t *testing.T) {
	want := noReplaceAtomicwriteBackoff(t)
	var got []time.Duration
	var sum time.Duration
	for try := range noReplaceTries {
		got = append(got, noReplaceWait(try))
		sum += noReplaceWait(try)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the waits before each try are %v; atomicwrite's renameBackoff is %v", got, want)
	}
	if sum != noReplaceBudget {
		t.Fatalf("the waits sum to %v; noReplaceBudget is %v", sum, noReplaceBudget)
	}
}

// noReplaceNoCallRow pins a platform where this package makes no such
// rename: ErrNoReplaceUnsupported, and nothing renamed.
func noReplaceNoCallRow(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	src := noReplaceWrite(t, from, "the source's bytes")
	err := RenameNoReplace(from, to)
	noReplaceRequireLinkError(t, err, from, to)
	if !errors.Is(err, ErrNoReplaceUnsupported) || !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("RenameNoReplace = %v; want ErrNoReplaceUnsupported", err)
	}
	noReplaceRequireBytes(t, from, src)
	noReplaceRequireAbsent(t, to)
}

// noReplaceCountTries puts a counter in beforeNoReplaceRename for the row,
// put back at its end, and returns it: each try's number, from 1, with also,
// where given, run with that number just before the try's call, on the
// goroutine that renames.
func noReplaceCountTries(t *testing.T, also func(try int32, from, to string)) *atomic.Int32 {
	t.Helper()
	tries := new(atomic.Int32)
	hook := func(from, to string) {
		try := tries.Add(1)
		if also != nil {
			also(try, from, to)
		}
	}
	prev := beforeNoReplaceRename.Swap(&hook)
	t.Cleanup(func() { beforeNoReplaceRename.Store(prev) })
	return tries
}

// noReplaceAnswerWith puts answer in the call's place for the row, put back
// at its end.
func noReplaceAnswerWith(t *testing.T, answer func(from, to string) error) {
	t.Helper()
	prev := noReplaceRenameSys.Swap(&answer)
	t.Cleanup(func() { noReplaceRenameSys.Store(prev) })
}

// noReplaceCreate makes path with O_EXCL and writes body to it, as a program
// that never replaces an entry would: it answers the create's error,
// fs.ErrExist where something is there, and then writes nothing.
func noReplaceCreate(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// noReplaceWrite writes body at path and returns the bytes read back from
// it, what the row compares the file with afterwards.
func noReplaceWrite(t *testing.T, path, body string) []byte {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return noReplaceRead(t, path)
}

// noReplaceRead returns the bytes at path, failing the row where it cannot.
func noReplaceRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

// noReplaceRequireBytes fails the row unless path holds want.
func noReplaceRequireBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	if got := noReplaceRead(t, path); !bytes.Equal(got, want) {
		t.Fatalf("%s holds %q; want %q", path, got, want)
	}
}

// noReplaceRequireAbsent fails the row unless nothing is at path.
func noReplaceRequireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Lstat(%s) = %v; want nothing there", path, err)
	}
}

// noReplaceRequireLinkError fails the row unless err is an *os.LinkError
// naming the rename of from to to, and returns it.
func noReplaceRequireLinkError(t *testing.T, err error, from, to string) *os.LinkError {
	t.Helper()
	var link *os.LinkError
	if !errors.As(err, &link) || link.Op != "rename" || link.Old != from || link.New != to {
		t.Fatalf("RenameNoReplace(%q, %q) = %v (%T); want an *os.LinkError naming the rename", from, to, err, err)
	}
	return link
}

// noReplaceRequireRefused fails the row unless err is the answer for an
// entry at to: an *os.LinkError naming the rename that matches fs.ErrExist
// and never ErrNoReplaceUnsupported.
func noReplaceRequireRefused(t *testing.T, err error, from, to string) {
	t.Helper()
	noReplaceRequireLinkError(t, err, from, to)
	if !errors.Is(err, fs.ErrExist) || errors.Is(err, ErrNoReplaceUnsupported) {
		t.Fatalf("RenameNoReplace(%s, %s) = %v; want fs.ErrExist alone", from, to, err)
	}
}

// noReplaceRequireOneRealTry fails the row unless its real call was asked
// once. On Windows a scanner's handle on a file just written can answer a
// try with a sharing violation, which is tried again, so there the row asks
// only that the tries stayed within noReplaceTries.
func noReplaceRequireOneRealTry(t *testing.T, tries *atomic.Int32) {
	t.Helper()
	n := tries.Load()
	if runtime.GOOS == "windows" {
		if n < 1 || n > noReplaceTries {
			t.Fatalf("the call was asked %d times; want from 1 to %d", n, noReplaceTries)
		}
		return
	}
	if n != 1 {
		t.Fatalf("the call was asked %d times; want once", n)
	}
}

// noReplaceAtomicwriteBackoff reads atomicwrite's renameBackoff from its
// source: each element 0, or an integer times one of time's units, the
// shapes it is written in; any other fails the row, to be read here anew.
func noReplaceAtomicwriteBackoff(t *testing.T) []time.Duration {
	t.Helper()
	path := filepath.Join("..", "atomicwrite", "atomicwrite.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("reading atomicwrite's schedule: %v", err)
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range value.Names {
				if name.Name != "renameBackoff" || i >= len(value.Values) {
					continue
				}
				list, ok := value.Values[i].(*ast.CompositeLit)
				if !ok {
					t.Fatalf("%s: renameBackoff is a %T, which this row does not read", path, value.Values[i])
				}
				var waits []time.Duration
				for _, elt := range list.Elts {
					wait, ok := noReplaceDurationOf(elt)
					if !ok {
						t.Fatalf("%s: renameBackoff holds a %T this row does not read", path, elt)
					}
					waits = append(waits, wait)
				}
				return waits
			}
		}
	}
	t.Fatalf("%s declares no renameBackoff", path)
	return nil
}

// noReplaceDurationOf is the value of one element of renameBackoff: an
// integer, or an integer times time.Nanosecond, Microsecond, Millisecond or
// Second.
func noReplaceDurationOf(e ast.Expr) (time.Duration, bool) {
	units := map[string]time.Duration{
		"Nanosecond": time.Nanosecond, "Microsecond": time.Microsecond,
		"Millisecond": time.Millisecond, "Second": time.Second,
	}
	switch e := e.(type) {
	case *ast.BasicLit:
		n, err := strconv.ParseInt(e.Value, 0, 64)
		return time.Duration(n), e.Kind == token.INT && err == nil
	case *ast.BinaryExpr:
		count, isLit := e.X.(*ast.BasicLit)
		unit, isSel := e.Y.(*ast.SelectorExpr)
		if !isLit || !isSel || e.Op != token.MUL || count.Kind != token.INT {
			return 0, false
		}
		if pkg, ok := unit.X.(*ast.Ident); !ok || pkg.Name != "time" {
			return 0, false
		}
		n, err := strconv.ParseInt(count.Value, 0, 64)
		per, ok := units[unit.Sel.Name]
		return time.Duration(n) * per, ok && err == nil
	}
	return 0, false
}
