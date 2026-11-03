package updater

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The marker's directory barrier (backlog B613's round 3). A barrier that
// fails after its rename or its unlink leaves the change live and not
// durable: the write's caller is told it failed, and a later start that
// short-cuts on what that change says (a version recorded already, no
// marker at all) must run the barrier again before it trusts it, refusing
// while it still fails. And a directory fsync the filesystem cannot do
// (EINVAL, ENOTSUP, EOPNOTSUPP) is no failure, said once; every other
// failure is.

// barrierFault stands a barrier in for the marker's that fails at one of its
// steps with err while failing is set, and counts the barriers run (its
// opens, failed or not). The fsync is never the platform's, so the barrier
// answers alike on every platform, Windows included, which has none.
type barrierFault struct {
	step    string
	err     error
	failing bool
	runs    int
}

func injectBarrierFault(t *testing.T, step string, err error) *barrierFault {
	t.Helper()
	f := &barrierFault{step: step, err: err, failing: true}
	prev := markerBarrier
	markerBarrier = &dirBarrier{
		open: func(name string) (*os.File, error) {
			f.runs++
			if f.failing && f.step == "open" {
				return nil, &os.PathError{Op: "open", Path: name, Err: f.err}
			}
			return os.Open(name)
		},
		sync: func(d *os.File) error {
			if f.failing && f.step == "sync" {
				return &os.PathError{Op: "sync", Path: d.Name(), Err: f.err}
			}
			return nil
		},
		close: func(d *os.File) error {
			err := d.Close()
			if f.failing && f.step == "close" {
				return &os.PathError{Op: "close", Path: d.Name(), Err: f.err}
			}
			return err
		},
	}
	t.Cleanup(func() { markerBarrier = prev })
	return f
}

// theDisarms are the two disarms a start runs before its first write: serve's
// admission and a migrating store open's.
var theDisarms = []struct {
	name string
	run  func(dataDir, current string) (DisarmResult, error)
}{
	{"serve's admission", func(dataDir, current string) (DisarmResult, error) {
		return AdmitBoot(context.Background(), dataDir, current, nil)
	}},
	{"a migration's", DisarmBeforeMigrating},
}

var barrierSteps = []string{"open", "sync", "close"}

// requireBarrierFailure fails unless err is the barrier's failure at step.
func requireBarrierFailure(t *testing.T, err error, step string, what string) {
	t.Helper()
	var be *BarrierError
	if !errors.As(err, &be) || be.Step != step || !errors.Is(err, syscall.EIO) {
		t.Fatalf("%s = %v, want the barrier's failure at its %s", what, err, step)
	}
}

// armedForBarrier is an install of 0.2.3 over 0.2.1, recent and swapped.
func armedForBarrier() State {
	return State{Status: "installing", TargetVersion: "0.2.3", PreviousVersion: "0.2.1",
		AttemptedAt: time.Now().UTC().Add(-time.Minute), SwapStarted: true}
}

// TestADisarmRunsTheBarrierAgainOverARecordItCouldNotMakeDurable is shape (a):
// the record's rename lands, its barrier fails, and the start refuses; the
// next start reads the record live and must not take it for durable.
func TestADisarmRunsTheBarrierAgainOverARecordItCouldNotMakeDurable(t *testing.T) {
	for _, d := range theDisarms {
		for _, step := range barrierSteps {
			t.Run(d.name+"/"+step, func(t *testing.T) {
				dir := t.TempDir()
				if err := SaveState(dir, armedForBarrier()); err != nil {
					t.Fatal(err)
				}
				fault := injectBarrierFault(t, step, syscall.EIO)
				_, err := d.run(dir, "0.2.3")
				requireBarrierFailure(t, err, step, "the first disarm")
				// The fault came after the rename: the record is live.
				if st, err := LoadState(dir); err != nil || st.DataDirOpenedBy != "0.2.3" {
					t.Fatalf("the marker after the failed barrier = %+v (%v), want the record live: the fault must come after the rename", st, err)
				}
				for attempt := 2; attempt <= 4; attempt++ {
					runs := fault.runs
					_, err := d.run(dir, "0.2.3")
					requireBarrierFailure(t, err, step, "a later disarm while the barrier fails")
					if fault.runs != runs+1 {
						t.Errorf("attempt %d ran the barrier %d times, want once", attempt, fault.runs-runs)
					}
				}
				fault.failing = false
				res, err := d.run(dir, "0.2.3")
				if err != nil || res.Recorded {
					t.Fatalf("the disarm once the barrier answers = %+v, %v; want nothing recorded again and no error", res, err)
				}
			})
		}
	}
}

// TestADisarmRunsTheBarrierAgainOverARemovalItCouldNotMakeDurable is shape
// (b): a marker that cannot be read is unlinked, the barrier fails, and the
// start refuses; the next start finds no marker, and must not take its
// absence for durable.
func TestADisarmRunsTheBarrierAgainOverARemovalItCouldNotMakeDurable(t *testing.T) {
	for _, d := range theDisarms {
		for _, step := range barrierSteps {
			t.Run(d.name+"/"+step, func(t *testing.T) {
				dir := t.TempDir()
				// A marker that cannot be read, and can be removed: an empty
				// directory where the file goes.
				if err := os.Mkdir(StatePath(dir), 0o700); err != nil {
					t.Fatal(err)
				}
				fault := injectBarrierFault(t, step, syscall.EIO)
				_, err := d.run(dir, "0.2.3")
				requireBarrierFailure(t, err, step, "the first disarm")
				if _, err := os.Lstat(StatePath(dir)); !os.IsNotExist(err) {
					t.Fatalf("the marker after the failed barrier: lstat %v, want it gone: the fault must come after the unlink", err)
				}
				for attempt := 2; attempt <= 4; attempt++ {
					runs := fault.runs
					_, err := d.run(dir, "0.2.3")
					requireBarrierFailure(t, err, step, "a later disarm while the barrier fails")
					if fault.runs != runs+1 {
						t.Errorf("attempt %d ran the barrier %d times, want once", attempt, fault.runs-runs)
					}
				}
				fault.failing = false
				if res, err := d.run(dir, "0.2.3"); err != nil || res.Recorded || res.Unreadable != nil {
					t.Fatalf("the disarm once the barrier answers = %+v, %v; want nothing done and no error", res, err)
				}
			})
		}
	}
}

// TestAClearRunsTheBarrierAgainOverARemovalItCouldNotMakeDurable is shape (b)
// for the clean-up itself: a second ClearStateDurably finds nothing to remove
// and still runs the barrier, refusing while it fails.
func TestAClearRunsTheBarrierAgainOverARemovalItCouldNotMakeDurable(t *testing.T) {
	for _, step := range barrierSteps {
		t.Run(step, func(t *testing.T) {
			dir := t.TempDir()
			if err := SaveState(dir, armedForBarrier()); err != nil {
				t.Fatal(err)
			}
			fault := injectBarrierFault(t, step, syscall.EIO)
			requireBarrierFailure(t, ClearStateDurably(dir), step, "the first clear")
			if _, err := os.Lstat(StatePath(dir)); !os.IsNotExist(err) {
				t.Fatalf("the marker after the failed barrier: lstat %v, want it gone", err)
			}
			for attempt := 2; attempt <= 4; attempt++ {
				runs := fault.runs
				requireBarrierFailure(t, ClearStateDurably(dir), step, "a later clear while the barrier fails")
				if fault.runs != runs+1 {
					t.Errorf("attempt %d ran the barrier %d times, want once", attempt, fault.runs-runs)
				}
			}
			fault.failing = false
			if err := ClearStateDurably(dir); err != nil {
				t.Fatalf("the clear once the barrier answers = %v", err)
			}
		})
	}
}

// forgetUnsupportedDirSync lets the once-a-process line be said again in this
// test, and again after it.
func forgetUnsupportedDirSync(t *testing.T) {
	t.Helper()
	unsupportedDirSyncSaid.Store(false)
	t.Cleanup(func() { unsupportedDirSyncSaid.Store(false) })
}

// unsupportedDirSyncLines are the lines a recorder kept that say the data
// dir cannot fsync a directory.
func unsupportedDirSyncLines(r *loggingtest.Recorder) []string {
	var lines []string
	for _, line := range r.All() {
		if strings.Contains(line, "cannot fsync a directory") {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestTheMarkersBarrierTakesOnlyADirectorySyncTheFilesystemCannotDo: a
// directory's fsync that answers EINVAL, ENOTSUP or EOPNOTSUPP is a
// filesystem that cannot do one, which counts as done, said once a process
// and naming no path, at the publication's barrier, the removal's and the
// disarm's alike; every other answer of the fsync, and any failure of the
// open or the close, is the barrier's failure.
func TestTheMarkersBarrierTakesOnlyADirectorySyncTheFilesystemCannotDo(t *testing.T) {
	notAnErrno := errors.New("not an errno")
	for _, tc := range []struct {
		step  string
		err   error
		taken bool
	}{
		{"sync", syscall.EINVAL, true},
		{"sync", syscall.ENOTSUP, true},
		{"sync", syscall.EOPNOTSUPP, true},
		{"sync", syscall.EBADF, false},
		{"sync", syscall.EIO, false},
		{"sync", syscall.ENOSPC, false},
		{"sync", syscall.EDQUOT, false},
		{"sync", syscall.EACCES, false},
		{"sync", syscall.EPERM, false},
		{"sync", syscall.EROFS, false},
		{"sync", syscall.ENOTTY, false},
		{"sync", notAnErrno, false},
		{"open", syscall.EINVAL, false},
		{"open", syscall.ENOTSUP, false},
		{"open", syscall.EBADF, false},
		{"open", syscall.EACCES, false},
		{"close", syscall.EINVAL, false},
		{"close", syscall.EIO, false},
	} {
		t.Run(tc.step+"/"+tc.err.Error(), func(t *testing.T) {
			forgetUnsupportedDirSync(t)
			r := loggingtest.Record(t)
			dir := t.TempDir()
			fault := injectBarrierFault(t, tc.step, tc.err)
			sites := []struct {
				name string
				run  func() error
			}{
				{"the publication", func() error { return SaveState(dir, armedForBarrier()) }},
				{"the removal", func() error { return ClearStateDurably(dir) }},
				{"the disarm's own", func() error { _, err := AdmitBoot(context.Background(), dir, "0.2.3", nil); return err }},
			}
			for _, s := range sites {
				runs := fault.runs
				err := s.run()
				if fault.runs != runs+1 {
					t.Errorf("%s ran the barrier %d times, want once", s.name, fault.runs-runs)
				}
				switch {
				case tc.taken && err != nil:
					t.Errorf("%s = %v, want a directory fsync the filesystem cannot do taken as done", s.name, err)
				case !tc.taken && !errors.Is(err, tc.err):
					t.Errorf("%s = %v, want the barrier's failure", s.name, err)
				}
			}
			lines := unsupportedDirSyncLines(r)
			switch {
			case tc.taken && len(lines) != 1:
				t.Errorf("said %d times that the directory cannot be fsynced, want once: %q", len(lines), lines)
			case !tc.taken && len(lines) != 0:
				t.Errorf("said the directory cannot be fsynced for a barrier that failed: %q", lines)
			}
			if carrying := r.Carrying(dir); len(carrying) != 0 {
				t.Errorf("a line names the data dir: %q", carrying)
			}
		})
	}
}

// TestDarwinsFullSyncFallsBackToAnOrdinaryFsync: Go's darwin Sync asks for
// F_FULLFSYNC and itself falls back to fsync on ENOTSUP alone; a full sync
// the filesystem does not support can also answer EINVAL or ENOTTY, and then
// the ordinary fsync is asked before anything is decided.
func TestDarwinsFullSyncFallsBackToAnOrdinaryFsync(t *testing.T) {
	for _, tc := range []struct {
		name        string
		full, plain error
		askPlain    bool
		want        error
	}{
		{"a full sync done", nil, nil, false, nil},
		{"EINVAL, then an ordinary fsync done", syscall.EINVAL, nil, true, nil},
		{"ENOTTY, then an ordinary fsync done", syscall.ENOTTY, nil, true, nil},
		{"EINVAL, then an ordinary fsync that fails", syscall.EINVAL, syscall.EIO, true, syscall.EIO},
		{"ENOTTY, then an ordinary fsync the filesystem cannot do", syscall.ENOTTY, syscall.EINVAL, true, syscall.EINVAL},
		{"ENOTSUP, which Go's Sync retried already", syscall.ENOTSUP, nil, false, syscall.ENOTSUP},
		{"EIO", syscall.EIO, nil, false, syscall.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := false
			full := func() error { return tc.full }
			plain := func() error { asked = true; return tc.plain }
			err := syncWithFallback(full, plain)
			if asked != tc.askPlain {
				t.Errorf("the ordinary fsync asked: %v, want %v", asked, tc.askPlain)
			}
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Errorf("syncWithFallback = %v, want %v", err, tc.want)
			}
		})
	}
	if err := syncWithFallback(func() error { return syscall.EINVAL }, nil); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("with no ordinary fsync to ask (off darwin) = %v, want the full sync's answer", err)
	}
}

// TestTheMarkersWriteRunsOnlyItsOwnBarrier: SaveState renames the marker
// into place with no barrier but its own, so a data dir that cannot be
// opened for its fsync fails the write with the marker's barrier's failure,
// in words that name no path, and no line is logged about it: atomicwrite's
// tolerant barrier, which logged the directory and the error and ran before
// the marker's own, is not run. POSIX only: the fixture is a directory this
// user can write and not read, which root and Windows ignore.
func TestTheMarkersWriteRunsOnlyItsOwnBarrier(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a directory this user cannot read: POSIX, and not as root")
	}
	r := loggingtest.Record(t)
	parent := t.TempDir()
	dir := parent + string(os.PathSeparator) + "data"
	if err := os.Mkdir(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := SaveState(dir, armedForBarrier())
	var be *BarrierError
	if !errors.As(err, &be) || be.Step != "open" || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("SaveState = %v, want the marker's barrier's failure at its open", err)
	}
	if strings.Contains(be.Error(), parent) {
		t.Errorf("the barrier's failure names the data dir: %q", be.Error())
	}
	if lines := r.All(); len(lines) != 0 {
		t.Errorf("the marker's write logged %q: its barrier is its own, and says nothing", lines)
	}
}
