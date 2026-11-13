package adminauth

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The tests in this file follow the store's interprocess lock (backlog
// B881's second round): every writer holds it from the re-read that gates
// its commit through its rename (lockStoreLocked), so another process's
// whole write cannot land between the two. Two stores on one file stand for
// the two processes, as in store_crossprocess_test.go: the lock belongs to
// the open file, so one store's open is refused while another's holds it,
// as another process's would be.

// storeGiveUp is when a test stops waiting for an event: ten seconds before
// the test binary's deadline, or never without one.
func storeGiveUp(t *testing.T) <-chan time.Time {
	deadline, ok := t.Deadline()
	if !ok {
		return nil
	}
	return time.After(max(time.Until(deadline)-10*time.Second, 0))
}

// parkBeforeRename makes s's next writes stop between their re-read and
// their rename, the store's lock held, until the returned release is
// called. parked is closed when the first one gets there.
func parkBeforeRename(t *testing.T, s *Store) (parked <-chan struct{}, release func()) {
	t.Helper()
	at, go_ := make(chan struct{}), make(chan struct{})
	var arrived, released sync.Once
	hook := func() {
		arrived.Do(func() { close(at) })
		<-go_
	}
	s.renameHook.Store(&hook)
	release = func() { released.Do(func() { close(go_) }) }
	t.Cleanup(release)
	return at, release
}

// noticeLockWait returns a channel closed the first time a write of s finds
// the store's lock held by another.
func noticeLockWait(s *Store) <-chan struct{} {
	waited := make(chan struct{})
	var once sync.Once
	hook := func() { once.Do(func() { close(waited) }) }
	s.lockWaitHook.Store(&hook)
	return waited
}

// TestAResetBesideASessionWriteParkedBeforeItsRenameIsNotUndone is the
// finding on B881's first round. The serving bridge's login passed its
// admission and its write's re-read, and stalled before its rename. With no
// lock between processes, `bridge admin reset-password` then committed,
// confirmed (its write had not been put back yet) and reported success, and
// the stalled rename landed over it: the old hash, the old sign-out marker
// and the new session, which a matching stat kept, while Verify took the
// leaked password again. The whole reset undone.
//
// With the lock, the stalled write holds it, the reset meets it and waits,
// and once the write has landed the reset's own re-read finds it and builds
// from it: the file holds the new hash, the session is ended, and the old
// password is refused.
func TestAResetBesideASessionWriteParkedBeforeItsRenameIsNotUndone(t *testing.T) {
	a, path, established, _ := runningBridge(t)
	checked := admit(t, a, "admin", leakedPassword)
	parked, release := parkBeforeRename(t, a)
	b, err := OpenStore(path) // `bridge admin reset-password`
	if err != nil {
		t.Fatal(err)
	}
	resetWaits := noticeLockWait(b)

	type created struct {
		raw string
		err error
	}
	login := make(chan created, 1)
	go func() {
		raw, err := a.CreateSession(checked)
		login <- created{raw, err}
	}()
	select {
	case <-parked:
	case <-storeGiveUp(t):
		t.Fatal("the login's write never reached its rename")
	}

	reset := make(chan error, 1)
	go func() { reset <- b.ResetPassword("admin", rotatedPassword, EndSessions) }()
	var resetErr error
	resetAnswered := false
	select {
	case <-resetWaits:
	case resetErr = <-reset:
		resetAnswered = true
		t.Errorf("the reset answered (%v) while a session's write was stopped between its re-read and its rename: "+
			"it never met the store's lock", resetErr)
	case <-storeGiveUp(t):
		t.Fatal("the reset neither waited for the store's lock nor answered")
	}
	release()
	a.renameHook.Store(nil)

	var made created
	select {
	case made = <-login:
	case <-storeGiveUp(t):
		t.Fatal("the login never answered")
	}
	if !resetAnswered {
		select {
		case resetErr = <-reset:
		case <-storeGiveUp(t):
			t.Fatal("the reset never answered")
		}
	}
	if made.err != nil {
		t.Fatalf("the login whose write was parked = %v, want it signed in: it was checked and written before the reset", made.err)
	}
	if resetErr != nil {
		t.Fatalf("the reset = %v, want it done once the login's write let the lock go", resetErr)
	}

	requireRotationOnDisk(t, path, "a reset beside a session write parked before its rename")
	if fileSignOut(t, path).IsZero() {
		t.Error("the file holds no sign-out: the reset's marker was written over")
	}
	for name, raw := range map[string]string{"the parked login's": made.raw, "the one signed in before": established} {
		if _, err := a.ValidateSession(raw); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("%s session on the running bridge after the reset = %v, want ErrSessionNotFound", name, err)
		}
	}
	requireEndedOnDisk(t, path, "the reset", made.raw, established)
	if _, err := a.Verify("admin", leakedPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("the running bridge accepts the leaked password after the reset (err=%v)", err)
	}
}

// TestASessionWriteParkedBeforeTheLockFindsTheResetAndWritesNothing is the
// other side: the login's write stops after its staging, before it takes
// the lock (beforeCommitHook), and the reset takes the lock, commits and
// lets it go. The login's write then takes the lock, its re-read finds the
// file moved, it builds again from the reset's file, and its admission
// refuses it there: ErrCredentialsChanged, and the file byte for byte what
// the reset wrote. The re-read did this before the lock too; the lock must
// not stop it.
func TestASessionWriteParkedBeforeTheLockFindsTheResetAndWritesNothing(t *testing.T) {
	a, path, _, _ := runningBridge(t)
	checked := admit(t, a, "admin", leakedPassword)
	renamed := atomic.Int32{}
	count := func() { renamed.Add(1) }
	a.renameHook.Store(&count)

	parked, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	beforeCommitHook = func() {
		if calls.Add(1) == 1 { // the login's; the reset's own write goes by
			close(parked)
			<-release
		}
	}
	type created struct {
		raw string
		err error
	}
	login := make(chan created, 1)
	t.Cleanup(func() {
		// The login's goroutine reads the hook: let it go and wait for it
		// before the hook is put back.
		select {
		case <-release:
		default:
			close(release)
		}
		<-login
		beforeCommitHook = nil
	})
	go func() {
		raw, err := a.CreateSession(checked)
		login <- created{raw, err}
	}()
	select {
	case <-parked:
	case <-storeGiveUp(t):
		t.Fatal("the login's write never reached its staging hook")
	}

	b, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ResetPassword("admin", rotatedPassword, EndSessions); err != nil {
		t.Fatalf("the reset beside a write parked before the lock = %v, want it done: the lock was free", err)
	}
	rotated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	var made created
	select {
	case made = <-login:
		login <- made // for the cleanup's wait
	case <-storeGiveUp(t):
		t.Fatal("the login never answered")
	}
	if !errors.Is(made.err, ErrCredentialsChanged) || made.raw != "" {
		t.Errorf("the login whose write found the reset = (%t, %v), want no token and ErrCredentialsChanged", made.raw != "", made.err)
	}
	if n := renamed.Load(); n != 0 {
		t.Errorf("the login's write reached its rename %d times, want never", n)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, rotated) {
		t.Errorf("the refused login wrote over the reset:\n%s", got)
	}
	requireRotationOnDisk(t, path, "a login refused after the reset")
}

// holdStoreLock takes the store's lock beside path as another process
// would, through an open of its own, and returns what lets it go.
func holdStoreLock(t *testing.T, path string) (release func()) {
	t.Helper()
	f, err := fsutil.OpenLockFile(filepath.Dir(path), LockFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.TryLock(f); err != nil {
		f.Close()
		t.Fatalf("take the store's lock for the test: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = fsutil.Unlock(f)
			_ = f.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// waitedTheBound requires that a write against a held lock answered only
// once its wait was out.
func waitedTheBound(t *testing.T, what string, started time.Time, bound time.Duration) {
	t.Helper()
	if took := time.Since(started); took < bound {
		t.Errorf("%s answered after %s, before its %s wait for the store's lock was out", what, took, bound)
	}
}

// TestAStoreLockHeldPastItsWaitLeavesEveryWriteAsAStoreThatCannotBeWritten:
// a writer that waited out its bound for a lock another holds writes
// nothing and answers as it answers a store it cannot write. The activity
// write is logged and the session check still answers; a logout and
// EndOtherSessions end their sessions in memory, kept pending, the second
// saying ErrSessionsNotSaved; a login fails and keeps no session; the
// commands' writes answer ErrStoreBusy. The file is as it was, and once
// the lock is let go the next write lands what was pending.
func TestAStoreLockHeldPastItsWaitLeavesEveryWriteAsAStoreThatCannotBeWritten(t *testing.T) {
	const bound = 150 * time.Millisecond
	a, path, established, clock := runningBridge(t)
	a.lockWait = bound
	loggedOut := signIn(t, a, "admin", leakedPassword)
	endedElsewhere := signIn(t, a, "admin", leakedPassword)
	checked := admit(t, a, "admin", leakedPassword)
	b, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	b.lockWait = bound
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := loggingtest.Record(t)
	release := holdStoreLock(t, path)

	// The session check whose activity write is due.
	clock.t = clock.t.Add(sessionFlushInterval + time.Second)
	started := time.Now()
	if _, err := a.ValidateSession(established); err != nil {
		t.Errorf("a session check whose activity write found the lock held = %v, want the session", err)
	}
	waitedTheBound(t, "the activity write", started, bound)
	if got := rec.Carrying(ErrStoreBusy.Error()); len(got) != 1 {
		t.Errorf("the activity write that waited out the lock logged %d lines naming it, want 1", len(got))
	}

	started = time.Now()
	a.DeleteSession(loggedOut)
	waitedTheBound(t, "the logout", started, bound)
	if _, err := a.ValidateSession(loggedOut); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("the logged-out session on the running bridge = %v, want ErrSessionNotFound", err)
	}

	started = time.Now()
	if n, err := a.EndOtherSessions(established); n != 1 || !errors.Is(err, ErrSessionsNotSaved) || !errors.Is(err, ErrStoreBusy) {
		t.Errorf("EndOtherSessions against a held lock = (%d, %v), want (1, ErrSessionsNotSaved wrapping ErrStoreBusy)", n, err)
	}
	waitedTheBound(t, "EndOtherSessions", started, bound)

	started = time.Now()
	if raw, err := a.CreateSession(checked); !errors.Is(err, ErrStoreBusy) || raw != "" {
		t.Errorf("a login against a held lock = (%t, %v), want no token and ErrStoreBusy", raw != "", err)
	}
	waitedTheBound(t, "the login", started, bound)

	started = time.Now()
	if err := b.ResetPassword("admin", rotatedPassword, EndSessions); !errors.Is(err, ErrStoreBusy) {
		t.Errorf("reset-password against a held lock = %v, want ErrStoreBusy", err)
	}
	waitedTheBound(t, "reset-password", started, bound)
	started = time.Now()
	if err := b.SignOutEverywhere(); !errors.Is(err, ErrStoreBusy) {
		t.Errorf("sign-out-everywhere against a held lock = %v, want ErrStoreBusy", err)
	}
	waitedTheBound(t, "sign-out-everywhere", started, bound)

	if got, _ := os.ReadFile(path); !bytes.Equal(got, before) {
		t.Fatalf("writes that never had the lock changed the file:\n%s", got)
	}
	if n := a.SessionCount(); n != 1 {
		t.Errorf("the running bridge holds %d sessions, want the caller's alone (the login kept none)", n)
	}

	// The contention clears: the next write lands what was pending.
	release()
	if err := a.FlushSessions(); err != nil {
		t.Fatalf("the flush once the lock is free = %v", err)
	}
	c := reopen(t, path)
	if _, err := c.ValidateSession(established); err != nil {
		t.Errorf("the caller's session is not in the file once the lock is free: %v", err)
	}
	requireEndedOnDisk(t, path, "the pending logout and end, written once the lock is free", loggedOut, endedElsewhere)
	if n := c.SessionCount(); n != 1 {
		t.Errorf("the file holds %d sessions once the lock is free, want the caller's alone", n)
	}
	if err := b.ResetPassword("admin", rotatedPassword, EndSessions); err != nil {
		t.Errorf("reset-password once the lock is free = %v", err)
	}
	requireRotationOnDisk(t, path, "reset-password once the lock is free")
}

// TestTheStoresLockMadeAsRootTakesTheStoreFilesOwner: the first write after
// a `sudo bridge admin reset-password` beside a service install makes the
// lock file as root, and it must belong to whoever owns the store file, or
// the service user cannot open it for its next write. It takes the store
// file's owner, and the directory's only while there is no store file, as
// fsutil.OpenLockFileLike gives it. Driven through
// fsutil.SimulateRootOwnersForTest, since the chown itself needs root;
// fsutil's own tests run as root pin the real one.
func TestTheStoresLockMadeAsRootTakesTheStoreFilesOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; no owner is given there")
	}
	const dirUID, dirGID = 4300, 4301   // root's, standing in: not 0:0, so a run as root still sees the change
	const fileUID, fileGID = 4242, 4243 // the service user's
	path := filepath.Join(t.TempDir(), "adminauth.json")
	seed, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.SetInitialPassword("admin", leakedPassword); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(filepath.Dir(path), LockFileName)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}

	changes, restore := fsutil.SimulateRootOwnersForTest(func(p string) (int, int) {
		if p == path {
			return fileUID, fileGID
		}
		return dirUID, dirGID
	})
	defer restore()
	cli, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.ResetPassword("admin", rotatedPassword, EndSessions); err != nil {
		t.Fatal(err)
	}
	want := fsutil.OwnerChange{Dst: lockPath, UID: fileUID, GID: fileGID}
	found := false
	for _, c := range changes() {
		if c.Dst == lockPath {
			if c != want {
				t.Errorf("the lock file made as root was given %+v, want %+v", c, want)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("no owner was given to the lock file made as root (changes %+v)", changes())
	}
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o600 != 0o600 {
		t.Errorf("the lock file's mode %v does not let its owner open it to read and write", info.Mode().Perm())
	}
}

// TestAStorageThatGivesNoLockIsWrittenUnlockedAndSaidOnce: where the lock
// answers that no lock can be had (fsutil.KeepsNoLocks: here ENOLCK, or
// ERROR_NOT_SUPPORTED on Windows), every write goes on as it did before
// the lock, guarded by its re-read and a command's confirmation, and the
// first of the process says so in the log, once, whichever store it is.
func TestAStorageThatGivesNoLockIsWrittenUnlockedAndSaidOnce(t *testing.T) {
	prev := unlockedWarned.Swap(false)
	t.Cleanup(func() { unlockedWarned.Store(prev) })
	rec := loggingtest.Record(t)
	noLock := func(*os.File) error { return &os.PathError{Op: "lock", Path: LockFileName, Err: noLocksErrno} }

	a, path, established, _ := runningBridge(t)
	a.tryLock = noLock
	signedIn := signIn(t, a, "admin", leakedPassword)
	a.DeleteSession(established)
	b, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	b.tryLock = noLock
	if err := b.ResetPassword("admin", rotatedPassword, KeepSessions); err != nil {
		t.Fatalf("reset-password where the storage gives no lock = %v, want it written", err)
	}

	c := reopen(t, path)
	if _, err := c.ValidateSession(signedIn); err != nil {
		t.Errorf("the login written without the lock is not in the file: %v", err)
	}
	if _, err := c.ValidateSession(established); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("the logout written without the lock did not land (err=%v)", err)
	}
	requireRotationOnDisk(t, path, "reset-password written without the lock")
	if got := rec.Lines(msgUnlockedLog); len(got) != 1 {
		t.Errorf("three writes without the lock, from two stores, logged the warning %d times, want once", len(got))
	}
	if got := rec.Failures(msgUnlockedLog); len(got) != 1 {
		t.Errorf("the warning is not at Warn: %v", got)
	}
}

// TestALockThatCannotBeTakenKeepsItsCause: a failure to open the lock file,
// or to take its lock, that says nothing of another holder or of a storage
// without locks is the write's error with its own cause, never
// ErrStoreBusy, and never a write made without the lock.
func TestALockThatCannotBeTakenKeepsItsCause(t *testing.T) {
	t.Run("a lock answering a permission error", func(t *testing.T) {
		a, path, established, _ := runningBridge(t)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		a.tryLock = func(*os.File) error { return &os.PathError{Op: "lock", Path: LockFileName, Err: fs.ErrPermission} }
		raw, err := a.CreateSession(admit(t, a, "admin", leakedPassword))
		if !errors.Is(err, fs.ErrPermission) || errors.Is(err, ErrStoreBusy) || raw != "" {
			t.Errorf("a login whose lock answered a permission error = (%t, %v), want no token and the permission error, not ErrStoreBusy",
				raw != "", err)
		}
		a.DeleteSession(established)
		if got, _ := os.ReadFile(path); !bytes.Equal(got, before) {
			t.Errorf("writes whose lock failed changed the file:\n%s", got)
		}
		a.tryLock = fsutil.TryLock
		if err := a.FlushSessions(); err != nil {
			t.Fatal(err)
		}
		requireEndedOnDisk(t, path, "the logout kept pending while its lock failed", established)
	})
	t.Run("a lock file this user cannot open", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("a file's mode does not refuse its owner's open on Windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("root opens a file whatever its mode")
		}
		_, path, _, _ := runningBridge(t)
		lockPath := filepath.Join(filepath.Dir(path), LockFileName)
		if err := os.Chmod(lockPath, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(lockPath, 0o600) })
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		b, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		err = b.ResetPassword("admin", rotatedPassword, EndSessions)
		if !errors.Is(err, fs.ErrPermission) || errors.Is(err, ErrStoreBusy) {
			t.Errorf("reset-password beside a lock file it cannot open = %v, want the permission error, not ErrStoreBusy", err)
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, before) {
			t.Errorf("reset-password wrote without the lock it could not open:\n%s", got)
		}
	})
}
