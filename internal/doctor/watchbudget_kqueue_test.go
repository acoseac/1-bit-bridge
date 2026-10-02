//go:build darwin

package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestTheWatchBudgetCheckGradesTheLibraryByTheWatchersOwnCount: on macOS the
// pre-flight counts the library as the watcher does (a folder it lists and
// every entry in one, each an open file) and grades it against the
// watcher's budget, half of the open-file limit and a quarter of the
// system's at most: past it the watcher will stay off, over 80% of it the
// watcher stops once the library grows past it, and under that the row is
// ok. The library here is the root, one album folder and nine files: 11
// open files.
func TestTheWatchBudgetCheckGradesTheLibraryByTheWatchersOwnCount(t *testing.T) {
	lib := t.TempDir()
	album := filepath.Join(lib, "Album")
	if err := os.Mkdir(album, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		if err := os.WriteFile(filepath.Join(album, fmt.Sprintf("%02d.flac", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name   string
		limits manifest.WatchFDLimits
		status Status
		want   string
	}{
		{"past the budget", manifest.WatchFDLimits{Process: 20}, Warn, "share of 10 open files (half of this process's open-file limit of 20"},
		{"past a budget the system caps", manifest.WatchFDLimits{Process: 1000, System: 40}, Warn, "share of 10 open files (a quarter of the system's open-file table of 40"},
		{"over 80% of it", manifest.WatchFDLimits{Process: 26}, Warn, "11 open files (2 folders and 9 entries in them) of the watcher's 13"},
		{"within it", manifest.WatchFDLimits{Process: 40}, OK, "11 open files (2 folders and 9 entries in them) of the watcher's 20"},
	} {
		t.Run(c.name, func(t *testing.T) {
			standInWatchFileLimits(t, c.limits, nil)
			got := checkWatchLimit(t.Context(), Deps{LibraryWatchEnabled: true, LibraryRoots: []string{lib}})
			if got.Name != checkNameWatcherFDBudget || got.Status != c.status || !strings.Contains(got.Summary, c.want) {
				t.Errorf("checkWatchLimit = %+v, want %s with %q", got, c.status, c.want)
			}
			if c.status == Warn && got.Hint != watchBudgetHint {
				t.Errorf("hint = %q, want %q", got.Hint, watchBudgetHint)
			}
		})
	}
}

// TestTheWatchBudgetCheckSaysWhyItGradesNothing: the watcher off, no root
// yet, and a limit that cannot be read each answer without counting.
func TestTheWatchBudgetCheckSaysWhyItGradesNothing(t *testing.T) {
	standInWatchFileLimits(t, manifest.WatchFDLimits{}, errors.New("no rlimit here"))
	for _, c := range []struct {
		name   string
		d      Deps
		status Status
		want   string
	}{
		{"watcher off", Deps{LibraryRoots: []string{t.TempDir()}}, OK, "library watcher disabled"},
		{"no roots", Deps{LibraryWatchEnabled: true}, OK, "no library roots configured yet"},
		{"no limit", Deps{LibraryWatchEnabled: true, LibraryRoots: []string{t.TempDir()}}, Warn, "could not read the open-file limit: no rlimit here"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := checkWatchLimit(t.Context(), c.d)
			if got.Status != c.status || !strings.Contains(got.Summary, c.want) {
				t.Errorf("checkWatchLimit = %+v, want %s with %q", got, c.status, c.want)
			}
		})
	}
}

// standInWatchFileLimits has checkWatchLimit read limits and err for the rest
// of the test. The check runs on the test's goroutine, so nothing reads the
// seam when it is put back.
func standInWatchFileLimits(t *testing.T, limits manifest.WatchFDLimits, err error) {
	t.Helper()
	prev := watchFileLimits
	watchFileLimits = func() (manifest.WatchFDLimits, error) { return limits, err }
	t.Cleanup(func() { watchFileLimits = prev })
}
