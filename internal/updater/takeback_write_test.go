package updater

import (
	"os"
	"runtime"
	"testing"
)

// TestATakeBackWhoseWriteIsRefusedKeepsTheCount: a take-back whose write is
// refused answers the refusal, saying that it reached its write (backlog
// B760). It skips where its refusal cannot be made: as root, and on Windows.
func TestATakeBackWhoseWriteIsRefusedKeepsTheCount(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a directory's mode stops no write here")
	}
	dir := t.TempDir()
	mustSave(t, dir, probationMarker(1))
	// The marker's lock file, as every admission makes it before.
	lock, err := LockState(t.Context(), dir)
	if err != nil || !lock.Held() {
		t.Fatalf("take the install marker's lock: %+v, %v", lock, err)
	}
	lock.Release()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if reached, err := TakeBackFailedStart(t.Context(), dir, "0.2.4", 1, nil); err == nil || !reached {
		t.Fatalf("a take-back whose write the data dir refuses = %v, %v; want its refusal, its write reached", reached, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := mustLoad(t, dir); got.FailedStarts != 1 {
		t.Errorf("the marker after a refused take-back = %+v, want the count kept", got)
	}
}
