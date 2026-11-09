//go:build !windows

package analyze

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestTheRepairKeepsTheOwnersOfWhatItMakes: run as root over a tree another
// user owns, each directory the repair makes takes the owner of the directory
// it is made in, and the curve takes the owner of the file it replaces, or of
// its directory for a new one: fsutil.Mkdir's and fsutil.KeepOwner's rules,
// which the repair's calls through its root keep (backlog B652, round 2). The
// simulated subtests run as any user; the last runs where the test runs as
// root, and skips, saying so, elsewhere. Windows gives no owner, a file taking
// its directory's ACL.
func TestTheRepairKeepsTheOwnersOfWhatItMakes(t *testing.T) {
	simulated := func(t *testing.T, f *repairFixture, owners map[string]int) []fsutil.OwnerChange {
		t.Helper()
		changes, restore := fsutil.SimulateRootOwnersForTest(func(p string) (int, int) {
			u := owners[filepath.Clean(p)]
			return u, u
		})
		defer restore()
		if _, err := runWaveformRepair(context.Background(), f.spec, fakeCurveSource(48000, nil)); err != nil {
			t.Fatalf("repair: %v", err)
		}
		return changes()
	}
	t.Run("a new curve, simulated", func(t *testing.T) {
		f := newRepairFixture(t)
		artist, album := filepath.Join(f.dir, "Artist"), filepath.Join(f.dir, "Artist", "Album")
		got := simulated(t, f, map[string]int{f.dir: 4001, artist: 4002, album: 4003})
		want := []fsutil.OwnerChange{{Dst: artist, UID: 4001, GID: 4001}, {Dst: album, UID: 4002, GID: 4002},
			{Dst: f.spec.SidecarPath(), UID: 4003, GID: 4003}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("owners given = %+v, want each directory its parent's and the curve its directory's: %+v", got, want)
		}
	})
	t.Run("a partial copy replaced, simulated", func(t *testing.T) {
		f := newRepairFixture(t)
		final := f.spec.SidecarPath()
		writeSentinel(t, final, "cut short")
		got := simulated(t, f, map[string]int{f.dir: 4001, filepath.Dir(final): 4003, final: 4999})
		want := []fsutil.OwnerChange{{Dst: final, UID: 4999, GID: 4999}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("owners given = %+v, want the curve the owner of the copy it replaces: %+v", got, want)
		}
	})
	t.Run("as root", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("needs root to give a file away: the owners the kernel records are checked only as root; " +
				"the simulated subtests check the owners the repair gives")
		}
		f := newRepairFixture(t)
		if err := os.Chown(f.dir, 4242, 4242); err != nil {
			t.Fatal(err)
		}
		if _, err := runWaveformRepair(context.Background(), f.spec, fakeCurveSource(48000, nil)); err != nil {
			t.Fatalf("repair: %v", err)
		}
		for _, p := range []string{filepath.Join(f.dir, "Artist"), filepath.Join(f.dir, "Artist", "Album"), f.spec.SidecarPath()} {
			info, err := os.Lstat(p)
			if err != nil {
				t.Fatal(err)
			}
			if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != 4242 || st.Gid != 4242 {
				t.Errorf("%s is owned by %+v, want 4242:4242, the tree's owner", p, info.Sys())
			}
		}
	})
}
