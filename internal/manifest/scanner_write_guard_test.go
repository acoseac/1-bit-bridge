package manifest

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestAWriteWithNoLinkOnItsWayResolvesNoOtherRoot pins B776's round 2: the
// configured roots are resolved, to find where a write through a link lands
// (landingRoots), only once a path's folder is found to resolve elsewhere, so
// a write into a healthy root with no link on its way waits on no other
// root's volume (one on a network mount that hangs would stall it), and a
// request through a link resolves each root once, however many paths it
// names.
func TestAWriteWithNoLinkOnItsWayResolvesNoOtherRoot(t *testing.T) {
	// The temporary folder's own links (macOS's /var, Windows' short names)
	// resolved first, so that nothing on the way to a root is one.
	base := fsutil.EvalSymlinksOrClean(t.TempDir())
	music, other := filepath.Join(base, "music"), filepath.Join(base, "other")
	writeTracks(t, filepath.Join(music, "A", "one.flac"), filepath.Join(other, "B", "two.flac"))
	s := NewScanner([]string{music, other}, openStoreIn(t, t.TempDir()), "")
	var resolved []string
	s.resolveRootHook = func(root string) string {
		resolved = append(resolved, root)
		return fsutil.EvalSymlinksOrClean(root)
	}
	ctx := context.Background()
	if err := s.RefuseWritesUnder(ctx, []string{
		filepath.Join(music, "A", "new.flac"),
		filepath.Join(music, "New", "Album", "x.flac"),
	}); err != nil {
		t.Fatalf("a write into a healthy root: %v", err)
	}
	if len(resolved) != 0 {
		t.Fatalf("a write with no link on its way resolved the roots %q", resolved)
	}

	t.Run("through a link", func(t *testing.T) {
		link := filepath.Join(music, "Linked")
		if err := os.Symlink(filepath.Join(other, "B"), link); err != nil {
			t.Skipf("this host cannot create a symlink: %v", err)
		}
		resolved = nil
		if err := s.RefuseWritesUnder(ctx, []string{
			filepath.Join(link, "x.flac"),
			filepath.Join(link, "Disc 2", "y.flac"),
		}); err != nil {
			t.Fatalf("a write through a link into a healthy root: %v", err)
		}
		slices.Sort(resolved)
		if want := []string{music, other}; !slices.Equal(resolved, want) {
			t.Fatalf("a write through a link resolved the roots %q, want each once: %q", resolved, want)
		}
	})
}
