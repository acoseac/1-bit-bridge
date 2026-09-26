package updater

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkScratch stages an `install-*` dir holding a stand-in for the
// downloaded archive, aged to the given mtime.
func mkScratch(t *testing.T, dataDir, name string, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(dataDir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "archive.tar.gz"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-age)
	if err := os.Chtimes(dir, ts, ts); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestReapScratchDirs pins the abandoned-scratch sweep.
//
// The per-attempt `install-<random>` layout removed the self-healing the
// old shared `<DataDir>/updates/` dir had (it was RemoveAll'd wholesale
// every attempt). A kill / OOM / power loss mid-install now strands the
// archive plus the extracted binary in DataDir permanently, because the
// only thing that removes a scratch dir is its own deferred cleanup.
//
// The mtime window is the load-bearing half: it is all that separates an
// abandoned dir from one another process is actively filling, since the
// in-process try-lock can't see the `bridge update` CLI.
func TestReapScratchDirs(t *testing.T) {
	dataDir := t.TempDir()

	old := mkScratch(t, dataDir, "install-abandoned", scratchReapAge+time.Hour)
	fresh := mkScratch(t, dataDir, "install-inflight", time.Minute)
	// Neighbours that must survive: DataDir also holds the DB, certs and
	// update-state.json.
	unrelatedDir := filepath.Join(dataDir, "artwork")
	if err := os.MkdirAll(unrelatedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(unrelatedDir, time.Now().Add(-72*time.Hour), time.Now().Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	unrelatedFile := filepath.Join(dataDir, "bridge.db")
	if err := os.WriteFile(unrelatedFile, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := ReapScratchDirs(dataDir, time.Now()); got != 1 {
		t.Errorf("ReapScratchDirs() = %d, want 1", got)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("abandoned scratch dir survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("recently-touched scratch dir was reaped — a concurrent install would lose its files mid-swap")
	}
	if _, err := os.Stat(unrelatedDir); err != nil {
		t.Error("unrelated DataDir subdirectory was reaped")
	}
	if _, err := os.Stat(unrelatedFile); err != nil {
		t.Error("unrelated DataDir file was reaped")
	}
}

// TestReapScratchDirsRefusesEmptyRoot pins that an unset DataDir never
// sweeps the working directory. It used to give the reason as
// os.ReadDir("") reading the working directory, which is false (it fails
// with ENOENT, measured with go1.26.6 on macOS, Linux and Windows). It
// asserted only the count, from a working directory with nothing in it to
// reap, so it passed with the guard deleted and would have passed a sweep
// of the working directory as well. The real hazard is a root resolved
// before the listing: filepath.Clean("") is ".", and filepath.Abs("") is
// the working directory. So the working directory here holds an abandoned
// install scratch dir, which such a sweep would remove.
func TestReapScratchDirsRefusesEmptyRoot(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	stale := mkScratch(t, cwd, scratchDirPrefix+"abandoned", 3*time.Hour)
	if got := ReapScratchDirs("", time.Now()); got != 0 {
		t.Errorf("ReapScratchDirs(\"\") = %d, want 0", got)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("an abandoned scratch dir in the working directory was swept: %v", err)
	}
}

// TestReapScratchDirsToleratesMissingDir pins fail-open: reclaiming disk
// must never be able to fail an install.
func TestReapScratchDirsToleratesMissingDir(t *testing.T) {
	if got := ReapScratchDirs(filepath.Join(t.TempDir(), "does-not-exist"), time.Now()); got != 0 {
		t.Errorf("ReapScratchDirs(missing) = %d, want 0", got)
	}
}
