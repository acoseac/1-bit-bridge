package updater

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Backlog B692: the updater's own paths (Install, the operator's Rollback,
// the refused candidate's hold, the auto-installer's restamp) hold the
// install marker's lock (update-state.lock) for their decision, their marker
// writes and their swap, as the early rollback, serve's admission, the boot's
// housekeeping and every migrating store open have since B613.

// withinTheTest bounds a wait for an event that must come.
const withinTheTest = 20 * time.Second

// gate is a one-shot signal a test opens once.
type gate struct {
	once sync.Once
	ch   chan struct{}
}

func newGate() *gate { return &gate{ch: make(chan struct{})} }

func (g *gate) open() { g.once.Do(func() { close(g.ch) }) }

// awaitGate fails the test unless g opens within the test.
func awaitGate(t *testing.T, g *gate, what string) {
	t.Helper()
	select {
	case <-g.ch:
	case <-time.After(withinTheTest):
		t.Fatalf("%s never happened", what)
	}
}

// signalWaits returns where each take of the install marker's lock that finds
// it held and waits says so, for the rest of the test.
func signalWaits(t *testing.T) <-chan struct{} {
	t.Helper()
	waits := make(chan struct{}, 1)
	t.Cleanup(SetStateLockWaitingForTest(func(string) {
		select {
		case waits <- struct{}{}:
		default:
		}
	}))
	return waits
}

// holdStateLock holds dir's install marker's lock for the rest of the test,
// as another process would, with LockState's wait at wait.
func holdStateLock(t *testing.T, dir string, wait time.Duration) {
	t.Helper()
	shortStateLockWait(t, wait)
	l, err := LockState(context.Background(), dir)
	if err != nil || !l.Held() {
		t.Fatalf("hold the install marker's lock: %+v, %v", l, err)
	}
	t.Cleanup(l.Release)
}

// theMarkerLockRefusals are the answers an updater write can meet at the
// install marker's lock other than its caller's stop: each refuses the write,
// nothing written and nothing swapped, with a message that names no path.
var theMarkerLockRefusals = []struct {
	name string
	lay  func(t *testing.T, dir string)
}{
	{"held by another process past the wait", func(t *testing.T, dir string) { holdStateLock(t, dir, 100*time.Millisecond) }},
	{"no lock to give", func(t *testing.T, _ string) {
		shortStateLockWait(t, 100*time.Millisecond)
		stubStateTryLock(t, syscall.ENOLCK)
	}},
	{"an I/O error", func(t *testing.T, _ string) { stubStateTryLock(t, syscall.EIO) }},
	{"a filesystem that keeps none", func(t *testing.T, _ string) { stubStateTryLock(t, syscall.EOPNOTSUPP) }},
}

// requireAMarkerLockRefusal checks err is the refusal of an updater write
// for the install marker's lock, naming none of paths.
func requireAMarkerLockRefusal(t *testing.T, err error, paths ...string) {
	t.Helper()
	if !errors.Is(err, ErrMarkerLockUnavailable) {
		t.Fatalf("err = %v, want a refusal for the install marker's lock (ErrMarkerLockUnavailable)", err)
	}
	for _, p := range paths {
		if strings.Contains(err.Error(), p) {
			t.Errorf("the refusal %q names a path", err)
		}
	}
}

// rollbackLayout is what an install leaves for an operator's rollback: the
// binary of the version installed in dir's bin, the backup of the version it
// replaced beside it, and the install's marker in dir's data.
type rollbackLayout struct {
	dataDir, live string
	marker        State
}

func newRollbackLayout(t *testing.T) *rollbackLayout {
	t.Helper()
	dir := t.TempDir()
	l := &rollbackLayout{dataDir: filepath.Join(dir, "data"), live: filepath.Join(dir, "bin", "bridge")}
	for _, d := range []string{l.dataDir, filepath.Dir(l.live)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(l.live, []byte("bridge-binary-0.2.3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.live+".bak", []byte("bridge-binary-0.2.1"), 0o755); err != nil {
		t.Fatal(err)
	}
	l.marker = State{Status: "installing", TargetVersion: "0.2.3", PreviousVersion: "0.2.1",
		AttemptedAt: time.Now().UTC().Add(-time.Minute), SwapStarted: true}
	l.save(t, l.marker)
	return l
}

func (l *rollbackLayout) save(t *testing.T, st State) {
	t.Helper()
	if err := SaveState(l.dataDir, st); err != nil {
		t.Fatal(err)
	}
}

func (l *rollbackLayout) opts() InstallOptions {
	return InstallOptions{DataDir: l.dataDir, BinaryPath: l.live, Force: true}
}

// files are the bytes of the marker, the binary and the backup as they are
// now, an absent one empty.
func (l *rollbackLayout) files() [3]string {
	var out [3]string
	for i, p := range []string{StatePath(l.dataDir), l.live, l.live + ".bak"} {
		b, _ := os.ReadFile(p)
		out[i] = string(b)
	}
	return out
}

func (l *rollbackLayout) requireUnchanged(t *testing.T, before [3]string) {
	t.Helper()
	if after := l.files(); after != before {
		t.Errorf("the marker, the binary or the backup changed: %q, want %q", after, before)
	}
}

// drainOnCleanup waits, when the test ends, for done to close after unhold
// lets go of whatever the test holds it in, so nothing writes the test's
// directories after they are removed.
func drainOnCleanup(t *testing.T, unhold func(), done <-chan struct{}, what string) {
	t.Helper()
	t.Cleanup(func() {
		unhold()
		select {
		case <-done:
		case <-time.After(withinTheTest):
			t.Errorf("%s did not return", what)
		}
	})
}

type disarmAnswer struct {
	res DisarmResult
	err error
}

// migrateIn runs a store open's disarm of current over dir on a goroutine,
// as a migrating command of that version does before its first step.
func migrateIn(t *testing.T, dir, current string, unhold func()) (<-chan disarmAnswer, <-chan struct{}) {
	t.Helper()
	answer, done := make(chan disarmAnswer, 1), make(chan struct{})
	go func() {
		defer close(done)
		res, err := DisarmBeforeMigrating(dir, current)
		answer <- disarmAnswer{res, err}
	}()
	drainOnCleanup(t, unhold, done, "the migration")
	return answer, done
}

// rollBackIn runs the operator's Rollback on a goroutine.
func rollBackIn(t *testing.T, ctx context.Context, opts InstallOptions, unhold func()) (<-chan error, <-chan struct{}) {
	t.Helper()
	answer, done := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(done)
		answer <- New(Options{}).Rollback(ctx, opts)
	}()
	drainOnCleanup(t, unhold, done, "the rollback")
	return answer, done
}

// TestARollbackThatHoldsTheMarkersLockFirstVetoesTheMigration: the operator's
// rollback holds the install marker's lock from its read through its record
// and the backup's rename, so a store open of the version it rolls back from,
// arriving meanwhile, waits for it and then refuses to migrate under the veto
// the rollback recorded: the backup is back, the store not migrated past it.
func TestARollbackThatHoldsTheMarkersLockFirstVetoesTheMigration(t *testing.T) {
	l := newRollbackLayout(t)
	atRename, release := newGate(), newGate()
	opts := l.opts()
	opts.RestoreBackup = func(dst, ext string) error {
		atRename.open()
		<-release.ch
		return RollbackBinary(dst, ext)
	}
	waits := signalWaits(t)
	rolledBack, _ := rollBackIn(t, context.Background(), opts, release.open)
	awaitGate(t, atRename, "the rollback's rename")

	migrated, migrationDone := migrateIn(t, l.dataDir, "0.2.3", release.open)
	select {
	case <-waits:
	case <-migrationDone:
		t.Fatalf("the migration did not wait for the rollback that held the marker's lock: %+v", <-migrated)
	case <-time.After(withinTheTest):
		t.Fatal("the migration never waited for the marker's lock")
	}
	release.open()
	if err := <-rolledBack; err != nil {
		t.Fatalf("Rollback = %v", err)
	}
	if m := <-migrated; !errors.Is(m.err, ErrMigrationVetoed) {
		t.Fatalf("the migration after the rollback = %+v, want it vetoed", m)
	}
	if got := l.files(); got[1] != "bridge-binary-0.2.1" {
		t.Errorf("the binary is %q, want the backup put back", got[1])
	}
}

// TestAMigrationThatHoldsTheMarkersLockFirstRefusesTheRollback: a store open
// of the version an install put in place holds the install marker's lock
// while it records that it opened the data dir, and the operator's rollback,
// arriving meanwhile, waits for it, reads the record and refuses: the store
// may hold what the backup's version does not read. The binaries and the
// record stay as they are.
func TestAMigrationThatHoldsTheMarkersLockFirstRefusesTheRollback(t *testing.T) {
	l := newRollbackLayout(t)
	atRecord, release := newGate(), newGate()
	t.Cleanup(SetBeforeDisarmRecordForTest(func(string) {
		atRecord.open()
		<-release.ch
	}))
	waits := signalWaits(t)
	migrated, _ := migrateIn(t, l.dataDir, "0.2.3", release.open)
	awaitGate(t, atRecord, "the migration's record")

	rolledBack, rollbackDone := rollBackIn(t, context.Background(), l.opts(), release.open)
	select {
	case <-waits:
	case <-rollbackDone:
		t.Fatalf("the rollback did not wait for the migration that held the marker's lock: %v", <-rolledBack)
	case <-time.After(withinTheTest):
		t.Fatal("the rollback never waited for the marker's lock")
	}
	release.open()
	if m := <-migrated; m.err != nil || !m.res.Recorded {
		t.Fatalf("the migration = %+v, want its record made", m)
	}
	if err := <-rolledBack; !errors.Is(err, ErrRollbackRefused) {
		t.Fatalf("the rollback after the migration's record = %v, want it refused", err)
	}
	got := l.files()
	if got[1] != "bridge-binary-0.2.3" || got[2] != "bridge-binary-0.2.1" {
		t.Errorf("the binary and the backup are %q and %q, want them as the install left them", got[1], got[2])
	}
	if st, err := LoadState(l.dataDir); err != nil || st.DataDirOpenedBy != "0.2.3" || st.Status != "installing" {
		t.Errorf("the marker after the refused rollback = %+v (%v), want the migration's record over the install", st, err)
	}
}

// TestARollbackIsRefusedOnceTheVersionItRollsBackFromOpenedTheDataDir: the
// marker records that a version other than the backup's opened the data dir
// under the install (State.DataDirOpenedBy: serve's admission, or a store open
// that migrated), so its store may hold what the backup's version does not
// read, and the operator's rollback refuses, writing and renaming nothing,
// whether the install was confirmed or not.
func TestARollbackIsRefusedOnceTheVersionItRollsBackFromOpenedTheDataDir(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker func(State) State
	}{
		{"armed", func(st State) State { st.DataDirOpenedBy = "0.2.3"; return st }},
		{"confirmed", func(st State) State {
			st.DataDirOpenedBy, st.Status, st.InstalledAt = "0.2.3", "installed", time.Now().UTC()
			return st
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newRollbackLayout(t)
			l.save(t, tc.marker(l.marker))
			before := l.files()
			err := New(Options{}).Rollback(context.Background(), l.opts())
			if !errors.Is(err, ErrRollbackRefused) {
				t.Fatalf("Rollback = %v, want it refused", err)
			}
			if strings.Contains(err.Error(), l.dataDir) || strings.Contains(err.Error(), filepath.Dir(l.live)) {
				t.Errorf("the refusal %q names a path", err)
			}
			if !strings.Contains(err.Error(), "0.2.3") {
				t.Errorf("the refusal %q does not name the version that opened the data dir", err)
			}
			l.requireUnchanged(t, before)
		})
	}
}

// TestARollbackRecordsItsHoldAndVetoBeforeThePutBackAndKeepsTheRest: the
// operator's rollback reads the marker under the install marker's lock and
// writes it back changed (backlog B692): the install ended (Status "", so no
// boot acts on it), the version rolled back from held back as an operator's
// rollback and its store migrations vetoed, durably before the rename, and
// every other field as it was.
func TestARollbackRecordsItsHoldAndVetoBeforeThePutBackAndKeepsTheRest(t *testing.T) {
	l := newRollbackLayout(t)
	l.marker.RejectedVersion, l.marker.RejectedReason, l.marker.RejectedConfigDigest = "0.2.2", "0.2.2 refuses this bridge's settings: line 4: x", "a digest"
	l.save(t, l.marker)
	var atRename State
	opts := l.opts()
	opts.RestoreBackup = func(dst, ext string) error {
		st, err := LoadState(l.dataDir)
		if err != nil {
			t.Errorf("the marker at the rename: %v", err)
		}
		atRename = st
		return RollbackBinary(dst, ext)
	}
	if err := New(Options{}).Rollback(context.Background(), opts); err != nil {
		t.Fatalf("Rollback = %v", err)
	}
	want := l.marker
	want.Status = ""
	want.RejectedVersion, want.RejectedReason, want.RejectedConfigDigest = "0.2.3", "", ""
	want.MigrationVeto = "0.2.3"
	// Under way at the rename, and no more once it has landed (round 2).
	wantAtRename := want
	wantAtRename.RollbackStarted = true
	if !sameState(atRename, wantAtRename) {
		t.Errorf("the marker at the rename = %+v, want %+v: the hold and the veto recorded first, the rollback under way, the rest kept",
			atRename, wantAtRename)
	}
	got, err := LoadState(l.dataDir)
	if err != nil || !sameState(got, want) {
		t.Errorf("the marker after the rollback = %+v (%v), want %+v", got, err, want)
	}
	for _, v := range []string{"0.2.1", "0.2.3"} {
		if a := DecideBootAction(got, v, time.Now().UTC()); a != BootNoop {
			t.Errorf("a boot of %s over the rollback's marker = %v, want BootNoop", v, a)
		}
	}
	if _, err := DisarmBeforeMigrating(l.dataDir, "0.2.3"); !errors.Is(err, ErrMigrationVetoed) {
		t.Errorf("a migration of the version rolled back from = %v, want it vetoed", err)
	}
	if hold := HeldBack(l.dataDir, "", "0.2.3"); hold.Version != "0.2.3" || hold.Recorded {
		t.Errorf("HeldBack after the rollback = %+v, want 0.2.3 held as an operator's rollback", hold)
	}
	if f := l.files(); f[1] != "bridge-binary-0.2.1" || f[2] != "" {
		t.Errorf("the binary %q and the backup %q, want the backup put back", f[1], f[2])
	}
}

// sameState compares two markers, their times to the instant.
func sameState(a, b State) bool {
	if !a.AttemptedAt.Equal(b.AttemptedAt) || !a.InstalledAt.Equal(b.InstalledAt) {
		return false
	}
	a.AttemptedAt, a.InstalledAt = time.Time{}, time.Time{}
	b.AttemptedAt, b.InstalledAt = time.Time{}, time.Time{}
	return a == b
}

// TestARollbackWhoseRenameFailsPutsTheMarkerBack: a rename that fails leaves
// the binary as it was, so the marker goes back as the rollback read it.
func TestARollbackWhoseRenameFailsPutsTheMarkerBack(t *testing.T) {
	l := newRollbackLayout(t)
	before := l.files()
	opts := l.opts()
	failure := errors.New("the rename failed")
	opts.RestoreBackup = func(string, string) error { return failure }
	if err := New(Options{}).Rollback(context.Background(), opts); !errors.Is(err, failure) {
		t.Fatalf("Rollback = %v, want the rename's failure", err)
	}
	l.requireUnchanged(t, before)
}

// TestARollbackWithNoBackupRefusesWithoutAPath: with no backup beside the
// binary there is nothing to put back, and the rollback says so in words that
// name no path, before it writes anything.
func TestARollbackWithNoBackupRefusesWithoutAPath(t *testing.T) {
	l := newRollbackLayout(t)
	if err := os.Remove(l.live + ".bak"); err != nil {
		t.Fatal(err)
	}
	before := l.files()
	err := New(Options{}).Rollback(context.Background(), l.opts())
	if err == nil {
		t.Fatal("a rollback with no backup = nil, want its refusal")
	}
	if strings.Contains(err.Error(), filepath.Dir(l.live)) || strings.Contains(err.Error(), l.dataDir) {
		t.Errorf("the refusal %q names a path", err)
	}
	l.requireUnchanged(t, before)
}

// TestARollbackRefusedForTheMarkersLockWritesNothing: an operator's rollback
// that cannot hold the install marker's lock, for another process holding it
// past the wait, no lock to give, a failure, or a filesystem that keeps none,
// refuses before it reads, writes or renames anything, naming no path; a stop
// while it waits, or before an uncontended take, returns the stop.
func TestARollbackRefusedForTheMarkersLockWritesNothing(t *testing.T) {
	for _, tc := range theMarkerLockRefusals {
		t.Run(tc.name, func(t *testing.T) {
			l := newRollbackLayout(t)
			tc.lay(t, l.dataDir)
			before := l.files()
			err := New(Options{}).Rollback(context.Background(), l.opts())
			requireAMarkerLockRefusal(t, err, l.dataDir, filepath.Dir(l.live))
			l.requireUnchanged(t, before)
		})
	}
	t.Run("stopped while it waits", func(t *testing.T) {
		l := newRollbackLayout(t)
		holdStateLock(t, l.dataDir, withinTheTest)
		waits := signalWaits(t)
		before := l.files()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rolledBack, _ := rollBackIn(t, ctx, l.opts(), cancel)
		select {
		case <-waits:
		case err := <-rolledBack:
			t.Fatalf("the rollback did not wait for the marker's lock another process held: %v", err)
		case <-time.After(withinTheTest):
			t.Fatal("the rollback never waited for the marker's lock")
		}
		cancel()
		if err := <-rolledBack; !errors.Is(err, context.Canceled) || errors.Is(err, ErrMarkerLockUnavailable) {
			t.Fatalf("a rollback stopped in its wait = %v, want the stop and no refusal of the lock's", err)
		}
		l.requireUnchanged(t, before)
	})
	t.Run("stopped before an uncontended take", func(t *testing.T) {
		l := newRollbackLayout(t)
		before := l.files()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := New(Options{}).Rollback(ctx, l.opts()); !errors.Is(err, context.Canceled) {
			t.Fatalf("a rollback whose context had ended = %v, want the stop", err)
		}
		l.requireUnchanged(t, before)
	})
}

// TestAnSCMStopFitsInsideTheMarkersLockWait: an install or an operator's
// rollback on Windows holds the install marker's lock while it waits for the
// SCM to stop the service, and every other taker of the lock waits
// defaultStateLockWait for it, so that stop takes at most half the wait,
// leaving the other half for the marker's writes and the renames.
func TestAnSCMStopFitsInsideTheMarkersLockWait(t *testing.T) {
	if 2*scmStopWait > defaultStateLockWait {
		t.Fatalf("scmStopWait %s is more than half the install marker's lock's wait %s: a migrating store open or a start "+
			"would give up on the lock while an install on Windows still waits for the service to stop", scmStopWait, defaultStateLockWait)
	}
}

// TestTheRestampRefusedForTheMarkersLockWritesNothing: the auto-installer's
// restamp before its restart reads and writes the marker only holding the
// install marker's lock, and leaves it as it is where it cannot hold it.
func TestTheRestampRefusedForTheMarkersLockWritesNothing(t *testing.T) {
	stale := time.Now().UTC().Add(-2 * time.Hour)
	setUp := func(t *testing.T) (*Updater, string) {
		dir := t.TempDir()
		if err := SaveState(dir, State{Status: "installing", TargetVersion: "0.2.3", PreviousVersion: "0.2.1",
			AttemptedAt: stale, SwapStarted: true}); err != nil {
			t.Fatal(err)
		}
		return New(Options{AutoInstallOpts: &InstallOptions{DataDir: dir}}), dir
	}
	for _, tc := range theMarkerLockRefusals {
		t.Run(tc.name, func(t *testing.T) {
			u, dir := setUp(t)
			tc.lay(t, dir)
			before, _ := os.ReadFile(StatePath(dir))
			u.refreshInstallAttemptedAt(context.Background())
			if after, _ := os.ReadFile(StatePath(dir)); !bytes.Equal(after, before) {
				t.Errorf("the restamp wrote the marker without its lock: %s", after)
			}
		})
	}
	t.Run("the lock free", func(t *testing.T) {
		u, dir := setUp(t)
		u.refreshInstallAttemptedAt(context.Background())
		if st, err := LoadState(dir); err != nil || !st.AttemptedAt.After(stale) {
			t.Errorf("the restamp with its lock = %+v (%v), want the install re-stamped", st, err)
		}
	})
}
