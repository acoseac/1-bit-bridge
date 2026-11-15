package manifest

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// openAndClose opens the store at path and closes it again, requiring both.
func openAndClose(t *testing.T, path string) {
	t.Helper()
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestAnOpenStoreKeepsARestoreOut (backlog B653 PR 2a, T1): a real store open
// in another process keeps a restore's exclusive hold out, ErrStoreHeld,
// until that process closes its store, and until a SIGKILL ends one that
// holds it: the kernel lets go of what a killed process held.
func TestAnOpenStoreKeepsARestoreOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	openAndClose(t, path)

	t.Run("until it closes", func(t *testing.T) {
		h := startLockHelper(t, "open", path)
		h.expect("ready")
		if _, err := HoldStoreExclusive(path); !errors.Is(err, ErrStoreHeld) {
			t.Fatalf("an exclusive hold beside another process's open store = %v, want ErrStoreHeld", err)
		}
		h.send("close")
		h.expect("closed")
		hold, err := HoldStoreExclusive(path)
		if err != nil {
			t.Fatalf("an exclusive hold once the other process closed its store = %v", err)
		}
		hold.Release()
		h.stop()
	})

	t.Run("until it is killed", func(t *testing.T) {
		h := startLockHelper(t, "open", path)
		h.expect("ready")
		if _, err := HoldStoreExclusive(path); !errors.Is(err, ErrStoreHeld) {
			t.Fatalf("an exclusive hold beside another process's open store = %v, want ErrStoreHeld", err)
		}
		h.kill()
		hold, err := HoldStoreExclusive(path)
		if err != nil {
			t.Fatalf("an exclusive hold once the process that held the store was killed = %v", err)
		}
		hold.Release()
	})
}

// TestARestoreHoldKeepsEveryOpenerOut (T2, this package's half): with
// another process's restore holding the lock exclusively, OpenStore and
// PendingMigrations each refuse with ErrStoreRestoring, the data directory's
// entries byte for byte as they were, and over a data directory that holds
// no database, none of the database's files made. internal/backup's half
// does the same of a live snapshot.
func TestARestoreHoldKeepsEveryOpenerOut(t *testing.T) {
	for _, withStore := range []bool{true, false} {
		name := "over a database"
		if !withStore {
			name = "over no database"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "bridge.db")
			if withStore {
				openAndClose(t, path)
			}
			h := startLockHelper(t, "exclusive", path)
			h.expect("ready")
			before := dirState(t, dir)
			var reached atomic.Bool
			t.Cleanup(setOpenStoreBeforeSQLite(func(string) { reached.Store(true) }))
			if _, err := OpenStore(path); !errors.Is(err, ErrStoreRestoring) {
				t.Errorf("OpenStore beside a restore's hold = %v, want ErrStoreRestoring", err)
			}
			if _, err := PendingMigrations(path); !errors.Is(err, ErrStoreRestoring) {
				t.Errorf("PendingMigrations beside a restore's hold = %v, want ErrStoreRestoring", err)
			}
			if reached.Load() {
				t.Error("an opener refused by the restore's hold reached SQLite")
			}
			if after := dirState(t, dir); !maps.Equal(before, after) {
				t.Errorf("the data directory changed under the refused openers:\nbefore %v\nafter  %v", before, after)
			}
			if !withStore {
				requireNoDatabaseFiles(t, path)
			}
			h.stop()
		})
	}
}

// writeJournal stands an entry of kind ("file", "dir", "link") at the
// restore's journal's name beside the database at path, as an unfinished
// restore leaves one (B653 PR 2b writes it; this PR's checks are tested with
// one the test writes).
func writeJournal(t *testing.T, path, kind string) {
	t.Helper()
	j := RestoreJournalPath(path)
	switch kind {
	case "file":
		if err := os.WriteFile(j, []byte(`{"format":1}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "dir":
		if err := os.Mkdir(j, 0o700); err != nil {
			t.Fatal(err)
		}
	case "link":
		if err := os.Symlink(filepath.Join(filepath.Dir(path), "nothing-there"), j); err != nil {
			t.Skipf("no symbolic link can be made here: %v", err)
		}
	}
}

// TestAnOpenOverAnUnfinishedRestoreRefusesAndMakesNothing (T3): a journal
// at its name, a file, a directory or a link to nothing, and no database:
// every opener of the live database refuses with ErrRestoreIncomplete, and
// none of the database's files is made, where OpenStore precreated an empty
// store before (internal/manifest/store.go's Precreate). And a link at the
// lifetime lock's own file, leading to where the database would be, makes
// nothing either, a journal standing beside it (Codex on PR 2a's brief).
func TestAnOpenOverAnUnfinishedRestoreRefusesAndMakesNothing(t *testing.T) {
	for _, kind := range []string{"file", "dir", "link"} {
		t.Run("a journal that is a "+kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "bridge.db")
			writeJournal(t, path, kind)
			var reached atomic.Bool
			t.Cleanup(setOpenStoreBeforeSQLite(func(string) { reached.Store(true) }))
			if _, err := OpenStore(path); !errors.Is(err, ErrRestoreIncomplete) {
				t.Errorf("OpenStore over an unfinished restore = %v, want ErrRestoreIncomplete", err)
			}
			if _, err := PendingMigrations(path); !errors.Is(err, ErrRestoreIncomplete) {
				t.Errorf("PendingMigrations over an unfinished restore = %v, want ErrRestoreIncomplete", err)
			}
			if reached.Load() {
				t.Error("an opener refused by the journal reached SQLite")
			}
			requireNoDatabaseFiles(t, path)
			if err := CheckRestoreJournal(path); !errors.Is(err, ErrRestoreIncomplete) ||
				!strings.Contains(err.Error(), "bridge.db-restore.json") || strings.Contains(err.Error(), dir) {
				t.Errorf("the refusal = %v, want ErrRestoreIncomplete naming the journal by its base name alone", err)
			}
		})
	}

	t.Run("a link at the lock file to the database, a journal standing", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bridge.db")
		if err := os.Symlink("bridge.db", StoreOpenLockPath(path)); err != nil {
			t.Skipf("no symbolic link can be made here: %v", err)
		}
		writeJournal(t, path, "file")
		_, err := OpenStore(path)
		if !errors.Is(err, ErrStoreLockUnavailable) || !errors.Is(err, errStoreSymlink) {
			t.Errorf("OpenStore through a link at its lock file = %v, want ErrStoreLockUnavailable and the link's refusal", err)
		}
		if _, err := PendingMigrations(path); !errors.Is(err, ErrStoreLockUnavailable) {
			t.Errorf("PendingMigrations through a link at its lock file = %v, want ErrStoreLockUnavailable", err)
		}
		if _, err := HoldStoreExclusive(path); !errors.Is(err, ErrStoreLockUnavailable) {
			t.Errorf("HoldStoreExclusive through a link at its lock file = %v, want ErrStoreLockUnavailable", err)
		}
		requireNoDatabaseFiles(t, path)
	})
}

// TestTheJournalIsAskedUnderTheOpenersHold (T3b, carbon-2): an opener parked
// once its shared hold is taken and before it asks the journal keeps a
// restore's exclusive try out, in another process; a journal that stands by
// the time it asks refuses it, nothing made, so no restore can slip its
// journal in between the opener's check and its Precreate. And with a
// restore's exclusive hold taken and a journal written, an opener's try is
// refused before anything of the database is made: the directory holds no
// entry it did not, the lock file the restore made aside.
func TestTheJournalIsAskedUnderTheOpenersHold(t *testing.T) {
	t.Run("an opener parked between its hold and its journal", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bridge.db")
		parked, release := make(chan struct{}), make(chan struct{})
		var once atomic.Bool
		t.Cleanup(SetStoreHoldTakenForTest(func(p string) {
			if p == path && once.CompareAndSwap(false, true) {
				close(parked)
				<-release
			}
		}))
		released := false
		t.Cleanup(func() {
			if !released {
				close(release)
			}
		})
		done := make(chan error, 1)
		go func() {
			s, err := OpenStore(path)
			if err == nil {
				_ = s.Close()
			}
			done <- err
		}()
		select {
		case <-parked:
		case <-time.After(30 * time.Second):
			t.Fatal("the opener never reached its hold")
		}
		if got := otherProcessTries(t, path); !strings.HasPrefix(got, "refused") || !strings.Contains(got, ErrStoreHeld.Error()) {
			t.Fatalf("a restore's exclusive try beside an opener parked under its hold = %q, want refused as held", got)
		}
		// What a restore that slipped in would leave: its journal.
		writeJournal(t, path, "file")
		released = true
		close(release)
		select {
		case err := <-done:
			if !errors.Is(err, ErrRestoreIncomplete) {
				t.Fatalf("the opener, its journal asked under its hold = %v, want ErrRestoreIncomplete", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the opener never answered")
		}
		requireNoDatabaseFiles(t, path)
	})

	t.Run("a restore's hold and its journal", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bridge.db")
		h := startLockHelper(t, "exclusive", path)
		h.expect("ready")
		writeJournal(t, path, "file")
		before := dirState(t, dir)
		if _, err := OpenStore(path); !errors.Is(err, ErrStoreRestoring) {
			t.Errorf("OpenStore beside a restore's hold and its journal = %v, want ErrStoreRestoring", err)
		}
		if after := dirState(t, dir); !maps.Equal(before, after) {
			t.Errorf("the refused opener made something:\nbefore %v\nafter  %v", before, after)
		}
		requireNoDatabaseFiles(t, path)
		h.stop()
	})
}

// TestOneProcessHoldsTheStoreOnce (T4): one descriptor a process, counted.
// Two opens of one store in this process, the first closed: the hold still
// stands, through tries that answer as POSIX record locks do (the Linux NFS
// case, where closing any descriptor of the file lets the process's lock go).
// A shared acquire under this process's exclusive hold counts on it, and an
// exclusive acquire under its shared hold refuses.
func TestOneProcessHoldsTheStoreOnce(t *testing.T) {
	t.Run("two opens, the first closed, as record locks answer", func(t *testing.T) {
		if !recordLocksAvailable {
			t.Skip("Windows' locks belong to a handle: it has no record locks to share")
		}
		path := filepath.Join(t.TempDir(), "bridge.db")
		openAndClose(t, path)
		t.Cleanup(useRecordLocks())
		first, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		second, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		if got := otherProcessTries(t, path, lockHelperRecordLocksEnv+"=1"); !strings.HasPrefix(got, "refused") {
			t.Fatalf("another process's exclusive try with the second store still open = %q, want refused", got)
		}
		if err := second.Close(); err != nil {
			t.Fatal(err)
		}
		waitForOtherProcessToHold(t, path, lockHelperRecordLocksEnv+"=1")
	})

	t.Run("a shared acquire under this process's exclusive hold counts on it", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bridge.db")
		openAndClose(t, path)
		hold, err := HoldStoreExclusive(path)
		if err != nil {
			t.Fatal(err)
		}
		again, err := HoldStoreExclusive(path)
		if err != nil {
			t.Fatalf("a second exclusive hold in the process that holds it = %v, want it counted", err)
		}
		again.Release()
		s, err := OpenStore(path)
		if err != nil {
			t.Fatalf("an open under this process's exclusive hold = %v, want it counted on the hold", err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if got := otherProcessTries(t, path); !strings.HasPrefix(got, "refused") {
			t.Fatalf("another process's try while this one still holds it exclusively = %q, want refused", got)
		}
		hold.Release()
		waitForOtherProcessToHold(t, path)
	})

	t.Run("an exclusive acquire under its shared hold refuses", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bridge.db")
		s, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := HoldStoreExclusive(path); !errors.Is(err, ErrStoreHeld) {
			t.Fatalf("an exclusive hold in a process that has the store open = %v, want ErrStoreHeld", err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		hold, err := HoldStoreExclusive(path)
		if err != nil {
			t.Fatalf("an exclusive hold once the store closed = %v", err)
		}
		hold.Release()
	})
}

// TestNoLockFailureAdmitsAnOpenerBesideARestore (T5, codex-2): a lock that
// answers ENOLCK, a lock file whose open answers EACCES, and a lock that
// answers an I/O error each refuse every opener with ErrStoreLockUnavailable,
// before SQLite is reached and with nothing of the database made or changed;
// only the filesystem's definitive no-locks answer lets the openers go on,
// said once, and a restore refuses it. And a real restore's hold in another
// process beside an opener whose try answers ENOLCK: refused, never reaching
// SQLite. The ladder's policy, which goes on after any failure, would let the
// first two through.
func TestNoLockFailureAdmitsAnOpenerBesideARestore(t *testing.T) {
	refusing := []struct {
		name string
		set  func(t *testing.T)
	}{
		{"ENOLCK", func(t *testing.T) { setStoreLockTry(t, answering(enolck)) }},
		{"an I/O error", func(t *testing.T) { setStoreLockTry(t, answering(ioErr)) }},
		{"EACCES at the lock file's open", func(t *testing.T) {
			t.Cleanup(SetStoreLockOpenForTest(func(_ func(string, string, string, bool) (*os.File, error), dir, name, _ string, _ bool) (*os.File, error) {
				return nil, &os.PathError{Op: "open", Path: filepath.Join(dir, name), Err: fs.ErrPermission}
			}))
		}},
	}
	for _, c := range refusing {
		t.Run(c.name, func(t *testing.T) {
			for _, withStore := range []bool{true, false} {
				dir := t.TempDir()
				path := filepath.Join(dir, "bridge.db")
				if withStore {
					openAndClose(t, path)
				}
				before := dirState(t, dir)
				var reached atomic.Bool
				restoreHook := setOpenStoreBeforeSQLite(func(string) { reached.Store(true) })
				c.set(t)
				if _, err := OpenStore(path); !errors.Is(err, ErrStoreLockUnavailable) {
					t.Errorf("OpenStore = %v, want ErrStoreLockUnavailable", err)
				}
				if _, err := PendingMigrations(path); !errors.Is(err, ErrStoreLockUnavailable) {
					t.Errorf("PendingMigrations = %v, want ErrStoreLockUnavailable", err)
				}
				if _, err := HoldStoreExclusive(path); !errors.Is(err, ErrStoreLockUnavailable) {
					t.Errorf("HoldStoreExclusive = %v, want ErrStoreLockUnavailable", err)
				}
				restoreHook()
				if reached.Load() {
					t.Error("a refused opener reached SQLite")
				}
				after := dirState(t, dir)
				delete(after, filepath.Base(StoreOpenLockPath(path))) // the lock file a try opened
				delete(before, filepath.Base(StoreOpenLockPath(path)))
				if !maps.Equal(before, after) {
					t.Errorf("the refused openers changed the data directory:\nbefore %v\nafter  %v", before, after)
				}
				if !withStore {
					requireNoDatabaseFiles(t, path)
				}
			}
		})
	}

	t.Run("a definitive no-locks answer", func(t *testing.T) {
		rec := loggingtest.Record(t)
		noStoreLockSaid.Store(false)
		t.Cleanup(func() { noStoreLockSaid.Store(false) })
		setStoreLockTry(t, answering(noLocksErrno))
		path := filepath.Join(t.TempDir(), "bridge.db")
		for range 2 {
			openAndClose(t, path)
		}
		if _, err := PendingMigrations(path); err != nil {
			t.Errorf("PendingMigrations on storage that takes no locks = %v, want it to go on", err)
		}
		if _, err := HoldStoreExclusive(path); !errors.Is(err, ErrStoreLockUnavailable) {
			t.Errorf("a restore's hold on storage that takes no locks = %v, want ErrStoreLockUnavailable", err)
		}
		lines := 0
		for _, l := range rec.All() {
			if strings.Contains(l, "library database's lock was not granted") {
				lines++
			}
		}
		if lines != 1 {
			t.Errorf("the no-locks line was said %d times, want once a process:\n%s", lines, strings.Join(rec.All(), "\n"))
		}
	})

	t.Run("beside a real restore, ENOLCK refuses without reaching SQLite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bridge.db")
		openAndClose(t, path)
		h := startLockHelper(t, "exclusive", path)
		h.expect("ready")
		var reached atomic.Bool
		t.Cleanup(setOpenStoreBeforeSQLite(func(string) { reached.Store(true) }))
		setStoreLockTry(t, answering(enolck))
		if _, err := OpenStore(path); !errors.Is(err, ErrStoreLockUnavailable) {
			t.Errorf("an opener answered ENOLCK beside a restore's hold = %v, want ErrStoreLockUnavailable", err)
		}
		if reached.Load() {
			t.Error("the opener reached SQLite beside a restore's hold")
		}
		h.stop()
	})
}

// TestALockFileThisUserMayOnlyReadStillHoldsTheStore (T5b): a process whose
// open of the lock file for writing is refused (EACCES, through the seam: a
// real read-only file would fail the restore's own open for writing too, run
// as the same user, and pass for the wrong reason) takes its shared hold
// through a descriptor open for reading; the kernel's lock on it keeps this
// process's restore out, its registry holding nothing, until that process
// has ended.
func TestALockFileThisUserMayOnlyReadStillHoldsTheStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	openAndClose(t, path) // the lock file is there, as the service made it
	h := startLockHelper(t, "shared", path, lockHelperReadOnlyEnv+"=1")
	h.expect("opened for reading")
	h.expect("ready")
	if _, err := HoldStoreExclusive(path); !errors.Is(err, ErrStoreHeld) {
		t.Fatalf("a restore beside a hold through a descriptor open for reading = %v, want ErrStoreHeld", err)
	}
	h.stop()
	hold, err := HoldStoreExclusive(path)
	if err != nil {
		t.Fatalf("a restore once the reader ended = %v", err)
	}
	hold.Release()
}

// TestAClosedStoreHoldsItsLockUntilItsLastTransactionEnds (Codex on PR 2a's
// brief, finding 1): database/sql's Close closes the idle connections, and a
// transaction under way keeps its own until it ends, so a store closed while
// a SetUpscaleTarget is parked before its commit keeps another process's
// restore out until that transaction has ended, and then lets go.
func TestAClosedStoreHoldsItsLockUntilItsLastTransactionEnds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	parked, release := make(chan struct{}), make(chan struct{})
	s.SetUpscaleTargetBeforeCommitForTest(func(*sql.Tx) error {
		close(parked)
		<-release
		return nil
	})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	done := make(chan error, 1)
	go func() { done <- s.SetUpscaleTarget(context.Background(), 96000, 24) }()
	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("the write never reached its commit")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := otherProcessTries(t, path); !strings.HasPrefix(got, "refused") {
		t.Fatalf("another process's restore while a closed store's transaction is still open = %q, want refused", got)
	}
	released = true
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the parked write after its store's Close = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the parked write never answered")
	}
	waitForOtherProcessToHold(t, path)
}

// TestALockFileThatIsALinkIsNeverFollowed: the lifetime lock's file is opened
// without following a link at its name, for writing and for reading alike: a
// link to where the database would be makes nothing, and a link to a file
// opens nothing.
func TestALockFileThatIsALinkIsNeverFollowed(t *testing.T) {
	for _, write := range []bool{true, false} {
		dir := t.TempDir()
		db := filepath.Join(dir, "bridge.db")
		if err := os.Symlink("bridge.db", StoreOpenLockPath(db)); err != nil {
			t.Skipf("no symbolic link can be made here: %v", err)
		}
		f, err := fsutil.OpenLockFileNoFollow(dir, filepath.Base(StoreOpenLockPath(db)), db, 0o644, write)
		if err == nil {
			_ = f.Close()
			t.Fatalf("a lock file that is a link to nothing opened (write %v)", write)
		}
		if !errors.Is(err, fsutil.ErrLockFileNotAFile) {
			t.Errorf("the refusal = %v, want ErrLockFileNotAFile", err)
		}
		requireNoDatabaseFiles(t, db)
		if runtime.GOOS != "windows" {
			if err := os.WriteFile(db, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if f, err := fsutil.OpenLockFileNoFollow(dir, filepath.Base(StoreOpenLockPath(db)), db, 0o644, write); err == nil {
				_ = f.Close()
				t.Errorf("a lock file that is a link to a file opened (write %v)", write)
			}
		}
	}
}
