package updater

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// Backlog B660's third round, in the updater: a running start is the lock its
// own token names, never the data dir's lock, which a command holds too; a
// migration reads it from the marker it reads under the install marker's lock;
// the take-back clears the token; and the previous release reads a marker
// that carries one.

// aStartsToken and anotherStartsToken are tokens of two starts.
const (
	aStartsToken       = "00112233445566778899aabbccddeeff"
	anotherStartsToken = "ffeeddccbbaa99887766554433221100"
)

// withToken is st as the start whose token is token counted it.
func withToken(st State, token string) State {
	st.RunningStart = token
	return st
}

// holdStartsLock holds, from another open of this process, the lock of the
// start whose token is token in dir, as that start's serve would, until the
// test ends or release.
func holdStartsLock(t *testing.T, dir, token string) (release func()) {
	t.Helper()
	name := "serve-start-" + token + ".lock"
	f, err := fsutil.OpenLockFile(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.TryLock(f); err != nil {
		_ = f.Close()
		t.Skipf("no lock can be taken on %s here: %v", name, err)
	}
	done := false
	release = func() {
		if !done {
			done = true
			_ = fsutil.Unlock(f)
			_ = f.Close()
		}
	}
	t.Cleanup(release)
	return release
}

// TestARunningStartIsTheLockItsTokenNames: the count holds a start running
// now only where the marker names that start's token and the lock the token
// names is held: the data dir's lock held by a command, a token whose lock
// nobody holds (a start killed with it), another start's lock and no token
// are none. Until the third round the data dir's lock alone answered.
func TestARunningStartIsTheLockItsTokenNames(t *testing.T) {
	dir := t.TempDir()
	counted := withToken(probationMarker(1), aStartsToken)
	holdServeLock(t, dir)
	if RunningServeCounted(dir, counted, "0.2.4") {
		t.Error("the data dir's lock held, by a command, and no start's lock: read as a start running")
	}
	if RunningServeCounted(dir, probationMarker(1), "0.2.4") {
		t.Error("a count with no token, the data dir's lock held: read as a start running")
	}
	release := holdStartsLock(t, dir, aStartsToken)
	if !RunningServeCounted(dir, counted, "0.2.4") {
		t.Error("the start's own lock held does not read as that start running")
	}
	if RunningServeCounted(dir, withToken(probationMarker(1), anotherStartsToken), "0.2.4") {
		t.Error("another start's lock held reads as the marker's start running")
	}
	if RunningServeCounted(dir, withToken(probationMarker(0), aStartsToken), "0.2.4") {
		t.Error("a token with no count reads as a counted start running")
	}
	release()
	if RunningServeCounted(dir, counted, "0.2.4") {
		t.Error("the start's lock let go still reads as the start running")
	}
	for _, token := range []string{"../" + aStartsToken, aStartsToken + "/x", "not hex"} {
		if RunningServeCounted(dir, withToken(probationMarker(1), token), "0.2.4") {
			t.Errorf("the token %q reads as a start running", token)
		}
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "serve-start-*")); len(entries) != 1 {
		t.Errorf("the questions made or removed lock files: %v, want only the one the test made", entries)
	}
}

// TestAMigrationReadsTheRunningStartUnderTheMarkersLock: a migrating open
// that waits for the install marker's lock asks whether a start is running of
// the marker it reads under that lock: a stop's take-back that lands in its
// wait (two starts counted to one, the token cleared, its serve still tearing
// down with both locks of its own) leaves one start before now that did not
// end cleanly, and the open is held. Until the third round it asked before
// its wait, and took the running start off a count the take-back had lowered.
func TestAMigrationReadsTheRunningStartUnderTheMarkersLock(t *testing.T) {
	dir := t.TempDir()
	mustSave(t, dir, withToken(probationMarker(2), aStartsToken))
	holdServeLock(t, dir)
	holdStartsLock(t, dir, aStartsToken)
	// The take-back holds the install marker's lock; the open waits for it.
	takeBack, err := LockState(t.Context(), dir)
	if err != nil || !takeBack.Held() {
		t.Fatalf("hold the install marker's lock: %+v, %v", takeBack, err)
	}
	t.Cleanup(takeBack.Release)
	waiting := make(chan struct{}, 1)
	t.Cleanup(SetStateLockWaitingForTest(func(string) {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}))
	errc := make(chan error, 1)
	go func() {
		_, err := DisarmBeforeMigrating(dir, "0.2.4")
		errc <- err
	}()
	select {
	case <-waiting:
	case <-time.After(20 * time.Second):
		t.Fatal("the migrating open never waited for the install marker's lock")
	}
	// The take-back's write, under the lock the open waits for.
	mustSave(t, dir, probationMarker(1))
	takeBack.Release()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrMigrationHeld) {
			t.Fatalf("a migrating open whose wait a take-back landed in = %v, want it held: one start before now did not end cleanly", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the migrating open never answered")
	}
	if st := mustLoad(t, dir); st.StoreMigratedBy != "" {
		t.Errorf("the marker after the held open = %+v, want no migration recorded", st)
	}
}

// TestTheTakeBackClearsItsStartsToken: a start's take-back of its count clears
// its token in the one write, so no question after it reads the start as
// running while its serve still holds its locks.
func TestTheTakeBackClearsItsStartsToken(t *testing.T) {
	dir := t.TempDir()
	mustSave(t, dir, withToken(probationMarker(2), aStartsToken))
	took, err := TakeBackFailedStart(t.Context(), dir, "0.2.4", 2, nil)
	if err != nil || !took {
		t.Fatalf("the take-back = %v, %v; want it written", took, err)
	}
	if st := mustLoad(t, dir); st.FailedStarts != 1 || st.RunningStart != "" {
		t.Errorf("the marker after the take-back = %+v, want the count taken back and the token cleared in the one write", st)
	}
}

// TestThePreviousReleaseReadsAMarkerWithARunningStart: 0.2.3 reads the marker
// with a plain json.Unmarshal into its State, frozen as state023, which drops
// a field it does not know: a marker carrying a running start's token reads as
// the install it is, and the crash rollback's record over it as a rollback
// done for 0.2.3.
func TestThePreviousReleaseReadsAMarkerWithARunningStart(t *testing.T) {
	st := withToken(probationMarker(ProbationFailedStarts), aStartsToken)
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"runningStart": "`+aStartsToken+`"`) {
		t.Fatalf("the marker as written does not carry the start's token:\n%s", raw)
	}
	var old state023
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatalf("0.2.3's read of a marker with a running start: %v", err)
	}
	if old.Status != "installing" || old.TargetVersion != "0.2.4" || old.PreviousVersion != "0.2.3" || !old.SwapStarted {
		t.Errorf("0.2.3 reads the marker as %+v, want the install as it is", old)
	}
	record, err := json.MarshalIndent(CrashRollbackRecord(st, "0.2.4"), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var oldRecord state023
	if err := json.Unmarshal(record, &oldRecord); err != nil {
		t.Fatalf("0.2.3's read of the crash rollback's record over it: %v", err)
	}
	if a := decide023(oldRecord, "0.2.3"); a != BootRollbackFinished {
		t.Errorf("0.2.3's boot over the record = %v, want BootRollbackFinished", a)
	}
	// And this build's own read of what it wrote.
	dir := t.TempDir()
	if err := os.WriteFile(StatePath(dir), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mustLoad(t, dir); got.RunningStart != aStartsToken {
		t.Errorf("this build reads the token back as %q", got.RunningStart)
	}
}
