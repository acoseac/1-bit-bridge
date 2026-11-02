package manifest

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// Backlog B185: two processes opening an older database at once (a `bridge
// doctor` run with the new binary beside the new serve's first start) ran
// the migration ladder side by side, and a step that checks before it
// writes failed the slower one. OpenStore now runs the ladder under a lock
// on the database's -migrate.lock (lockLadder).

// databaseAtVersion writes a database at user_version head, as a binary
// whose ladder ended there left it.
func databaseAtVersion(t *testing.T, head int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("v%d.db", head))
	db, err := sql.Open("sqlite", dsn.File(path, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, m := range migrations {
		if m.version > head {
			continue
		}
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("apply migration %d (%s): %v", m.version, m.name, err)
		}
		if m.post != nil {
			if err := m.post(db); err != nil {
				t.Fatalf("migration %d (%s) post: %v", m.version, m.name, err)
			}
		}
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, head)); err != nil {
		t.Fatal(err)
	}
	return path
}

// versionOnDisk reads path's user_version through a connection of its own.
func versionOnDisk(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(path, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	return readUserVersion(t, db)
}

// holdLadderLock takes path's ladder lock as another opener would, and
// returns what releases it.
func holdLadderLock(t *testing.T, path string) func() {
	t.Helper()
	f, err := os.OpenFile(path+ladderLockSuffix, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.TryLock(f); err != nil {
		f.Close()
		t.Fatalf("take the ladder lock: %v", err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = fsutil.Unlock(f)
			_ = f.Close()
		})
	}
	t.Cleanup(release)
	return release
}

var latestVersion = migrations[len(migrations)-1].version

// TestConcurrentOpensOfAnOlderDatabaseAllSucceed is the reviewer's probe in
// small: three opens at once of one database a v0.2.0 binary left
// (user_version 44), ten rounds. Each open ran the ladder from v45 on the
// same file. On the parent commit the first round failed in every run,
// with "add playlists.deleted_by: duplicate column name" (v45) or "add
// tracks.folder_art_key: duplicate column name" (v49); with only those
// tolerated, migration 52's epoch row failed one run in three ("UNIQUE
// constraint failed: backup_epoch.id").
func TestConcurrentOpensOfAnOlderDatabaseAllSucceed(t *testing.T) {
	const rounds, openers = 10, 3
	for round := range rounds {
		path := databaseAtVersion(t, 44)
		start := make(chan struct{})
		errs := make([]error, openers)
		var wg sync.WaitGroup
		for i := range openers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				s, err := OpenStore(path)
				if err == nil {
					err = s.Close()
				}
				errs[i] = err
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d, opener %d: %v", round+1, i+1, err)
			}
		}
		if v := versionOnDisk(t, path); v != latestVersion {
			t.Fatalf("round %d: user_version %d, want %d", round+1, v, latestVersion)
		}
	}
}

// TestAnOpenerWaitsForAnotherOpenersLadder: while another opener holds the
// ladder lock, OpenStore runs no step of the ladder; once it is released,
// it runs the ladder and opens.
func TestAnOpenerWaitsForAnotherOpenersLadder(t *testing.T) {
	path := databaseAtVersion(t, 44)
	release := holdLadderLock(t, path)
	waiting := make(chan struct{})
	var once sync.Once
	t.Cleanup(setLadderLockWaiting(func(string) { once.Do(func() { close(waiting) }) }))

	type opened struct {
		s   *Store
		err error
	}
	done := make(chan opened, 1)
	go func() {
		s, err := OpenStore(path)
		done <- opened{s, err}
	}()
	select {
	case <-waiting:
	case o := <-done:
		if o.s != nil {
			o.s.Close()
		}
		t.Fatalf("OpenStore returned (err=%v) while another opener held the ladder lock", o.err)
	}
	if v := versionOnDisk(t, path); v != 44 {
		t.Fatalf("user_version %d while the opener waited, want 44: it ran the ladder without the lock", v)
	}
	release()
	o := <-done
	if o.err != nil {
		t.Fatalf("open after the lock was released: %v", o.err)
	}
	defer o.s.Close()
	if v := readUserVersion(t, o.s.db); v != latestVersion {
		t.Errorf("user_version %d, want %d", v, latestVersion)
	}
}

// ladderNoLockMessage and ladderUnlockedMessage are how OpenStore says it
// migrates without its ladder lock: where the lock's answer is one a
// filesystem that keeps no locks gives (fsutil.KeepsNoLocks), saying another
// process may still hold one, since ENOLCK, one of those answers, does not
// show it holds none; and where the answer is any other or the lock file
// could not be opened.
const (
	ladderNoLockMessage   = "no migration lock was granted: another process may still hold one, and the ladder runs without exclusive use of the database"
	ladderUnlockedMessage = "could not take the migration lock; migrating without it"
)

// ladderLockKindCase is one answer of the ladder lock's and whether
// fsutil.KeepsNoLocks reads it.
type ladderLockKindCase struct {
	name    string
	answer  error
	noLocks bool
}

// TestTheMigrationSaysWhichKindOfLockItCouldNotTake opens a database at
// v44 whose ladder lock answers each kind of failure that is not another
// opener holding it (B537 item 1), and requires the open to migrate without
// the lock, as before, and to say which kind it met: an answer
// fsutil.KeepsNoLocks reads (errors.ErrUnsupported, which it reads on every
// platform; the platform's own in the unix and Windows halves) with a line
// of its own that does not claim the filesystem keeps no locks, which ENOLCK,
// one of those answers, does not prove; any other answer (an I/O error), and
// a lock file that cannot be opened (a directory standing where it goes),
// with the line it always had. A held lock is waited for
// (TestAnOpenerWaitsForAnotherOpenersLadder), and past the bound refused
// (TestAnOpenerGivesUpOnALadderLockHeldPastTheBound).
func TestTheMigrationSaysWhichKindOfLockItCouldNotTake(t *testing.T) {
	for _, tc := range append([]ladderLockKindCase{
		{"no lock on this platform", errors.ErrUnsupported, true},
		{"an I/O error", syscall.EIO, false},
	}, platformLadderLockKindCases...) {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(setLadderTryLock(func(f *os.File) error { return &os.PathError{Op: "flock", Path: f.Name(), Err: tc.answer} }))
			requireMigratedSaying(t, databaseAtVersion(t, 44), tc.noLocks)
		})
	}
	t.Run("a lock file that cannot be opened", func(t *testing.T) {
		path := databaseAtVersion(t, 44)
		if err := os.Mkdir(path+ladderLockSuffix, 0o700); err != nil {
			t.Fatal(err)
		}
		requireMigratedSaying(t, path, false)
	})
}

// requireMigratedSaying opens path, requires the ladder to have run to the
// latest version, and requires one line of noLocks's kind and none of the
// other.
func requireMigratedSaying(t *testing.T, path string, noLocks bool) {
	t.Helper()
	rec := loggingtest.Record(t)
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore without its ladder lock: %v", err)
	}
	defer s.Close()
	if v := readUserVersion(t, s.db); v != latestVersion {
		t.Errorf("user_version %d, want %d: the ladder did not run without its lock", v, latestVersion)
	}
	said, other := rec.Lines(ladderNoLockMessage), rec.Lines(ladderUnlockedMessage)
	if !noLocks {
		said, other = other, said
	}
	if len(said) != 1 || len(other) != 0 {
		t.Errorf("the open said %q and %q, want one line of its kind (no locks: %v) and none of the other; every line:\n%s",
			said, other, noLocks, strings.Join(rec.All(), "\n"))
	}
}

// TestAnOpenerGivesUpOnALadderLockHeldPastTheBound: a holder that keeps the
// lock for all of ladderLockWait fails the open with errLadderLockTimeout,
// and the open runs no step of the ladder. Proceeding without the lock
// would run it beside a holder that may still be migrating.
func TestAnOpenerGivesUpOnALadderLockHeldPastTheBound(t *testing.T) {
	path := databaseAtVersion(t, 44)
	holdLadderLock(t, path)
	prevWait, prevPoll := ladderLockWait, ladderLockPoll
	ladderLockWait, ladderLockPoll = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { ladderLockWait, ladderLockPoll = prevWait, prevPoll })

	start := time.Now()
	s, err := OpenStore(path)
	if err == nil {
		s.Close()
		t.Fatal("OpenStore opened while another opener held the ladder lock past the bound")
	}
	if !errors.Is(err, errLadderLockTimeout) {
		t.Fatalf("err = %v, want errLadderLockTimeout", err)
	}
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Errorf("gave up after %s, before the bound", waited)
	}
	if v := versionOnDisk(t, path); v != 44 {
		t.Errorf("user_version %d after the refused open, want 44", v)
	}
}
