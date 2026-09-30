package integrity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The probe that begins a VariantWatcher tick called its variants directory
// healthy when it held any entry at all (backlog B223). A clean unmount
// leaves the path naming the local directory under the mountpoint, and
// anything written there while the volume is away made that directory
// "healthy": a Finder .DS_Store, a README, the folders a render makes before
// sox writes (a render that fails leaves them). Every row then read as a
// rendition that is gone, the mass-delete check walked the directory, found
// no sidecar and let the deletions through: the review's scratch test, one
// tick, {Rows:40 Deleted:40}. A tree that holds no rendition at all is what
// an unmounted volume looks like, whatever else is in it, so the probe now
// counts renditions, not entries.

// lostMountShapes are what a local directory under a mountpoint can hold
// while the volume is away, none of it a rendition.
var lostMountShapes = []struct {
	name  string
	files []string // files written into the local directory (relative)
	dirs  []string // directories made in it (relative)
}{
	{name: "a .DS_Store", files: []string{".DS_Store"}},
	{name: "the folders a failed render left", dirs: []string{"Artist/Album"}},
	{name: "a README and a Thumbs.db", files: []string{"README.txt", "Thumbs.db"}},
	{name: "a .flac that is not a rendition", files: []string{"Artist/Album/01.flac"}},
	{name: "renditions only in a Trash", files: []string{".Trashes/501/01.flac." + mountVariant + ".flac"}},
}

// TestVariantWatcherRefusesAMountpointHoldingNoRendition — the backlog's
// reproduction: forty rows whose renditions sit on the volume, the volume
// unmounted before the tick, and a local directory at the path holding
// something that is not a rendition. On main the tick deleted all forty.
// It now refuses as the mount-loss kind, before it classifies a row, and
// says the directory holds no rendition.
func TestVariantWatcherRefusesAMountpointHoldingNoRendition(t *testing.T) {
	for _, shape := range lostMountShapes {
		t.Run(shape.name, func(t *testing.T) {
			dir := mountedVariantsDir(t)
			var rows []VariantSnapshot
			for i := 0; i < 40; i++ {
				rows = append(rows, presentRow(t, dir, i))
			}
			unmountVolume(t, dir, shape.files...)
			for _, d := range shape.dirs {
				if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(d)), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			store := &mountHooks{}
			w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, store, nil, staticDir(dir), time.Hour, 20)
			rec := loggingtest.Record(t)

			r := w.tick(context.Background())
			if !r.Skipped || r.Deleted != 0 || len(store.deleted()) != 0 {
				t.Fatalf("a tick over an unmounted volume whose mountpoint holds %s: report %+v, deleted %d row(s); want it skipped, nothing deleted",
					shape.name, r, len(store.deleted()))
			}
			requireRefusedAsAnUnmount(t, w, store, r, rec, "holds no rendition")
		})
	}
}

// TestVariantWatcherStillDeletesWhenTheTreeHoldsARendition — the positive
// control: a rendition anywhere in the tree (here one directory deep, beside
// a .DS_Store) says the volume is there, so a row whose sidecar is gone is
// still deleted, as it always was.
func TestVariantWatcherStillDeletesWhenTheTreeHoldsARendition(t *testing.T) {
	dir := mountedVariantsDir(t)
	var rows []VariantSnapshot
	for i := 0; i < 5; i++ {
		rows = append(rows, presentRow(t, dir, i))
	}
	writeSidecar(t, filepath.Join(dir, ".DS_Store"))
	gone := rows[4]
	if err := os.Remove(gone.SidecarPath); err != nil {
		t.Fatal(err)
	}
	store := &mountHooks{}
	w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, store, nil, staticDir(dir), time.Hour, 20)

	r := w.tick(context.Background())
	if r.Skipped || r.Deleted != 1 {
		t.Fatalf("report %+v, want the one row whose rendition is gone deleted", r)
	}
}
