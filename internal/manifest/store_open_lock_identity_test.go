package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// requireOneHoldAcrossSpellings holds the store at first exclusively, then
// takes this process's shared hold through second, another spelling of the
// same lock file, and an exclusive one through it again: each is counted on
// the first hold, one registry entry, one descriptor. Let go in any order,
// the lock is free for another process at the end.
func requireOneHoldAcrossSpellings(t *testing.T, first, second string) {
	t.Helper()
	hold, err := HoldStoreExclusive(first)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			hold.Release()
		}
	}()
	shared, err := holdStoreShared(second)
	if err != nil {
		t.Fatalf("an opener through another spelling of the lock file, under this process's exclusive hold = %v, want it counted on the hold", err)
	}
	again, err := HoldStoreExclusive(second)
	if err != nil {
		shared()
		t.Fatalf("an exclusive hold through another spelling of the lock file this process holds = %v, want it counted", err)
	}
	storeLocks.Lock()
	n := len(storeLocks.byKey)
	storeLocks.Unlock()
	if n != 1 {
		t.Errorf("the registry holds %d entries for one lock file, want one", n)
	}
	if got := otherProcessTries(t, first); !strings.HasPrefix(got, "refused") {
		t.Errorf("another process's try while this one holds the lock = %q, want refused", got)
	}
	again.Release()
	shared()
	hold.Release()
	released = true
	storeLocks.Lock()
	n = len(storeLocks.byKey)
	storeLocks.Unlock()
	if n != 0 {
		t.Errorf("the registry holds %d entries once every hold is let go, want none", n)
	}
	waitForOtherProcessToHold(t, first)
}

// TestALockFileWhoseIdentityCannotBeReadHoldsNothing: a hold whose lock
// file's identity cannot be read (its stat fails) is refused
// (ErrStoreLockUnavailable), shared and exclusive alike, with no registry
// entry left and the lock never taken: a live entry's identity is never
// missing, so a hold through another spelling of its file always finds it
// (B653 PR 2a's third round: an entry kept with none was passed over, and a
// second descriptor met this process's own lock).
func TestALockFileWhoseIdentityCannotBeReadHoldsNothing(t *testing.T) {
	storeLockSeamMu.Lock()
	prev := storeLockStat
	storeLockStat = func(*os.File) (os.FileInfo, error) { return nil, errors.New("the stat failed") }
	storeLockSeamMu.Unlock()
	t.Cleanup(func() {
		storeLockSeamMu.Lock()
		storeLockStat = prev
		storeLockSeamMu.Unlock()
	})
	path := filepath.Join(t.TempDir(), "bridge.db")
	if release, err := holdStoreShared(path); !errors.Is(err, ErrStoreLockUnavailable) {
		if err == nil {
			release()
		}
		t.Errorf("a shared hold whose lock file's identity cannot be read = %v, want ErrStoreLockUnavailable", err)
	}
	if h, err := HoldStoreExclusive(path); !errors.Is(err, ErrStoreLockUnavailable) {
		h.Release()
		t.Errorf("an exclusive hold whose lock file's identity cannot be read = %v, want ErrStoreLockUnavailable", err)
	}
	storeLocks.Lock()
	n := len(storeLocks.byKey)
	storeLocks.Unlock()
	if n != 0 {
		t.Errorf("the registry keeps %d entries for holds it refused, want none", n)
	}
	waitForOtherProcessToHold(t, path)
}

// TestTwoSpellingsOfOneLockFileAreOneHold: the registry finds a hold by the
// lock file's identity, never by its spelling alone (B653 PR 2a's second
// round). On a volume that folds case (macOS's APFS by default)
// filepath.EvalSymlinks keeps each component as written, so a data directory
// spelled in another case, or the database named in another case, keys
// apart from the hold: a second descriptor met this process's own exclusive
// lock, and an opener under bridge restore's hold refused itself. A hard link
// to the lock file from another directory is that same pair of spellings on
// any volume, and runs wherever hard links can be made.
func TestTwoSpellingsOfOneLockFileAreOneHold(t *testing.T) {
	t.Run("a hard link to the lock file from another directory", func(t *testing.T) {
		a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
		for _, d := range []string{a, b} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		first, second := filepath.Join(a, "bridge.db"), filepath.Join(b, "bridge.db")
		openAndClose(t, first)
		if err := os.Link(StoreOpenLockPath(first), StoreOpenLockPath(second)); err != nil {
			t.Skipf("no hard link can be made here: %v", err)
		}
		requireOneHoldAcrossSpellings(t, first, second)
	})

	for _, c := range []struct{ name, dir, base string }{
		{"the data directory in another case", "data", "bridge.db"},
		{"the database in another case", "Data", "Bridge.db"},
	} {
		t.Run(c.name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "Data")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			first := filepath.Join(dir, "bridge.db")
			openAndClose(t, first)
			second := filepath.Join(parent, c.dir, c.base)
			a, errA := os.Lstat(StoreOpenLockPath(first))
			b, errB := os.Lstat(StoreOpenLockPath(second))
			if errA != nil || errB != nil || !os.SameFile(a, b) {
				t.Skipf("the volume at %s is case-sensitive (%v, %v): two spellings in another case name two files there, "+
					"so this case runs only where the volume folds case, as macOS's APFS does by default", parent, errA, errB)
			}
			requireOneHoldAcrossSpellings(t, first, second)
		})
	}

	t.Run("a link at the lock file of the second spelling matches nothing", func(t *testing.T) {
		a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
		for _, d := range []string{a, b} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		first, second := filepath.Join(a, "bridge.db"), filepath.Join(b, "bridge.db")
		openAndClose(t, first)
		if err := os.Symlink(StoreOpenLockPath(first), StoreOpenLockPath(second)); err != nil {
			t.Skipf("no symbolic link can be made here: %v", err)
		}
		hold, err := HoldStoreExclusive(first)
		if err != nil {
			t.Fatal(err)
		}
		defer hold.Release()
		if release, err := holdStoreShared(second); err == nil {
			release()
			t.Fatal("an opener whose lock file is a link to the held one was counted on the hold, want it refused as a link")
		} else if !errors.Is(err, ErrStoreLockUnavailable) || !errors.Is(err, errStoreSymlink) {
			t.Fatalf("an opener whose lock file is a link = %v, want the link refused", err)
		}
	})
}
