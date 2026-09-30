package integrity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// The VariantWatcher asked whether its variants directory looked unmounted
// ONCE, before its first pass (backlog B203). A clean unmount during the
// tick leaves the path naming the local directory under the mountpoint:
// every row classified after it reads as a rendition that is gone, the
// relocation check walks that empty directory and finds no sidecars, and
// the tick deleted the rows. The review's scratch test: 39 of 40. These
// tests unmount the volume from inside the tick, through the one call the
// watcher makes while it classifies (an adoption), and stand the unmount
// in with what it leaves on disk: the volume's directory moved aside, and
// a local directory at the path.

// mountVariant is the variant every row here names.
const mountVariant = "upscaled-v2-176400-24"

// mountHooks is a fakeDeleter that also runs a hook when a named source
// path's row is adopted: the moment inside a tick's first pass at which a
// test unmounts or remounts the volume.
type mountHooks struct {
	fakeDeleter
	onAdopt map[string]func()
}

// AdoptVariantSidecar records the adoption and then runs the source
// path's hook, on the tick's own goroutine.
func (m *mountHooks) AdoptVariantSidecar(sourcePath, variantID, newSidecarPath string) error {
	if err := m.fakeDeleter.AdoptVariantSidecar(sourcePath, variantID, newSidecarPath); err != nil {
		return err
	}
	if hook := m.onAdopt[sourcePath]; hook != nil {
		hook()
	}
	return nil
}

// mountSource names row i's source file.
func mountSource(i int) string { return fmt.Sprintf("Artist/Album/%02d.flac", i) }

// writeSidecar writes a ten-byte file at p, making its directories.
func writeSidecar(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// presentRow is row i with its sidecar at its canonical place under dir.
func presentRow(t *testing.T, dir string, i int) VariantSnapshot {
	t.Helper()
	p := transcode.VariantSidecarPath(dir, mountSource(i), mountVariant)
	writeSidecar(t, p)
	return VariantSnapshot{SourcePath: mountSource(i), VariantID: mountVariant, SidecarPath: p, SizeBytes: 10}
}

// movedRow is row i recorded under a tree that is not there, so the tick
// looks for it at its canonical place under the variants directory, and
// adopts it when a ten-byte file is there.
func movedRow(oldDir string, i int) VariantSnapshot {
	return VariantSnapshot{SourcePath: mountSource(i), VariantID: mountVariant,
		SidecarPath: transcode.VariantSidecarPath(oldDir, mountSource(i), mountVariant), SizeBytes: 10}
}

// unmountVolume stands in for a clean unmount of the volume mounted at dir:
// the volume's directory moves aside, and dir names a new local directory,
// holding the files local names (relative paths). It returns where the
// volume went, for remountVolume.
func unmountVolume(t *testing.T, dir string, local ...string) (volume string) {
	t.Helper()
	volume = dir + ".volume"
	if err := atomicwrite.RenameWithRetry(dir, volume); err != nil {
		t.Fatalf("unmount: %v", err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("unmount: %v", err)
	}
	for _, rel := range local {
		writeSidecar(t, filepath.Join(dir, filepath.FromSlash(rel)))
	}
	return volume
}

// remountVolume puts the volume back at dir, moving the local directory
// aside: the same directory the tick began on is at the path again.
func remountVolume(t *testing.T, dir, volume string) {
	t.Helper()
	if err := atomicwrite.RenameWithRetry(dir, dir+".local"); err != nil {
		t.Fatalf("remount: %v", err)
	}
	if err := atomicwrite.RenameWithRetry(volume, dir); err != nil {
		t.Fatalf("remount: %v", err)
	}
}

// mountedVariantsDir is a variants directory on its "volume", in a
// directory of its own so the unmount's renames stay beside it.
func mountedVariantsDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "mnt", "variants")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// requireRefusedAsAnUnmount fails the test unless the tick deleted nothing
// and refused as the mount-loss kind, with one WARN that says why.
func requireRefusedAsAnUnmount(t *testing.T, w *VariantWatcher, store *mountHooks, r SweepReport, rec *loggingtest.Recorder, reason string) {
	t.Helper()
	if r.Deleted != 0 || len(store.deleted()) != 0 {
		t.Errorf("a tick whose variants directory was unmounted under it deleted %d row(s): %v (report %+v)", r.Deleted, store.deleted(), r)
	}
	if got := w.Status().Refusing; got != VariantRefusalVariantsDir {
		t.Errorf("the tick refused as %q, want %q (report %+v)", got, VariantRefusalVariantsDir, r)
	}
	requireLinesSay(t, rec.Failures(), 1, "one WARN, the mount-loss refusal's", msgVariantsDirUnavailable, reason)
}

// TestVariantWatcherRefusesATickWhoseVolumeIsUnmountedDuringIt — the
// review's shape: forty rows, the first adopted, and the volume unmounted
// at that adoption, leaving an empty directory at the path. On main the
// other 39 rows read as gone and were deleted; the tick now refuses as an
// unmount and deletes none. The next tick's own probe finds the empty
// mountpoint, and it continues the same streak: no second WARN.
func TestVariantWatcherRefusesATickWhoseVolumeIsUnmountedDuringIt(t *testing.T) {
	dir := mountedVariantsDir(t)
	oldDir := filepath.Join(t.TempDir(), "old-host", "variants")
	rows := []VariantSnapshot{movedRow(oldDir, 0)}
	writeSidecar(t, CanonicalSidecarPath(dir, rows[0]))
	for i := 1; i < 40; i++ {
		rows = append(rows, presentRow(t, dir, i))
	}
	store := &mountHooks{onAdopt: map[string]func(){
		mountSource(0): func() { unmountVolume(t, dir) },
	}}
	w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, store, nil, staticDir(dir), time.Hour, 20)
	rec := loggingtest.Record(t)

	r := w.tick(context.Background())
	requireRefusedAsAnUnmount(t, w, store, r, rec, "no longer the directory this sweep began on")
	if r.Adopted != 1 {
		t.Errorf("the adoption made before the unmount: report %+v, want 1 adopted", r)
	}

	if r := w.tick(context.Background()); !r.Skipped || r.Deleted != 0 {
		t.Errorf("the next tick over the still-unmounted volume: report %+v, want it skipped", r)
	}
	requireRefusedAsAnUnmount(t, w, store, r, rec, "no longer the directory this sweep began on")
}

// TestVariantWatcherRefusesATickWhoseDirectoryIsReplacedByANonEmptyOne —
// the local directory an unmount leaves need not be empty (something wrote
// into the mountpoint while the volume was away), so asking again whether
// the directory looks unmounted is not enough: it looks healthy. Only its
// identity says it is not the directory the tick began on.
func TestVariantWatcherRefusesATickWhoseDirectoryIsReplacedByANonEmptyOne(t *testing.T) {
	dir := mountedVariantsDir(t)
	oldDir := filepath.Join(t.TempDir(), "old-host", "variants")
	rows := []VariantSnapshot{movedRow(oldDir, 0)}
	writeSidecar(t, CanonicalSidecarPath(dir, rows[0]))
	for i := 1; i < 40; i++ {
		rows = append(rows, presentRow(t, dir, i))
	}
	store := &mountHooks{onAdopt: map[string]func(){
		mountSource(0): func() { unmountVolume(t, dir, "README.txt") },
	}}
	w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, store, nil, staticDir(dir), time.Hour, 20)
	rec := loggingtest.Record(t)

	r := w.tick(context.Background())
	requireRefusedAsAnUnmount(t, w, store, r, rec, "no longer the directory this sweep began on")
}

// TestVariantWatcherRefusesATickWhoseVolumeGoesAfterItsLastMissingRow —
// every missing row was classified while the volume was there, and the
// tree held a sidecar no row names, which is the relocation the mass-delete
// check refuses. The volume goes after the last of them, so the check walks
// the empty directory the unmount left, finds no sidecars, and lets 39
// deletions through. A check made as each row reads as missing cannot see
// this; the tick asks again after the relocation check, before it deletes.
func TestVariantWatcherRefusesATickWhoseVolumeGoesAfterItsLastMissingRow(t *testing.T) {
	dir := mountedVariantsDir(t)
	oldDir := filepath.Join(t.TempDir(), "old-host", "variants")
	var rows []VariantSnapshot
	for i := 0; i < 39; i++ {
		rows = append(rows, movedRow(oldDir, i))
	}
	last := movedRow(oldDir, 39)
	writeSidecar(t, CanonicalSidecarPath(dir, last))
	rows = append(rows, last)
	writeSidecar(t, transcode.VariantSidecarPath(dir, "Other/Album/01.flac", mountVariant))
	store := &mountHooks{onAdopt: map[string]func(){
		mountSource(39): func() { unmountVolume(t, dir) },
	}}
	w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, store, nil, staticDir(dir), time.Hour, 20)
	rec := loggingtest.Record(t)

	r := w.tick(context.Background())
	requireRefusedAsAnUnmount(t, w, store, r, rec, "no longer the directory this sweep began on")
	if r.Refused != 39 {
		t.Errorf("report %+v, want the 39 rows it had found missing counted as refused", r)
	}
}

// TestVariantWatcherRefusesATickWhoseVolumeWentAndCameBackDuringIt — the
// volume goes and comes back inside the first pass, so the directory at
// the path is the one the tick began on again by the time the pass ends;
// the rows classified while it was away read as gone, four of them, under
// the mass-delete floor. Only a check made as each row reads as missing
// sees it.
func TestVariantWatcherRefusesATickWhoseVolumeWentAndCameBackDuringIt(t *testing.T) {
	dir := mountedVariantsDir(t)
	oldDir := filepath.Join(t.TempDir(), "old-host", "variants")
	rows := []VariantSnapshot{movedRow(oldDir, 0)}
	writeSidecar(t, CanonicalSidecarPath(dir, rows[0]))
	for i := 1; i < 5; i++ {
		rows = append(rows, presentRow(t, dir, i))
	}
	back := movedRow(oldDir, 5)
	rows = append(rows, back)
	for i := 6; i < 40; i++ {
		rows = append(rows, presentRow(t, dir, i))
	}
	// Row 5's file is in the local directory the unmount leaves, so the
	// tick adopts it while the volume is away: the moment it comes back.
	backRel, err := filepath.Rel(dir, CanonicalSidecarPath(dir, back))
	if err != nil {
		t.Fatal(err)
	}
	var volume string
	store := &mountHooks{onAdopt: map[string]func(){
		mountSource(0): func() { volume = unmountVolume(t, dir, filepath.ToSlash(backRel)) },
		mountSource(5): func() { remountVolume(t, dir, volume) },
	}}
	w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, store, nil, staticDir(dir), time.Hour, 20)
	rec := loggingtest.Record(t)

	r := w.tick(context.Background())
	requireRefusedAsAnUnmount(t, w, store, r, rec, "no longer the directory this sweep began on")
}

// TestVariantWatcherDeletesWhatIsGoneWhileItsDirectoryStays — the control:
// the same forty rows with the volume left alone, and three sidecars
// removed by hand, are three deletions, and the tick refuses nothing.
func TestVariantWatcherDeletesWhatIsGoneWhileItsDirectoryStays(t *testing.T) {
	dir := mountedVariantsDir(t)
	var rows []VariantSnapshot
	for i := 0; i < 40; i++ {
		rows = append(rows, presentRow(t, dir, i))
	}
	for _, i := range []int{3, 17, 31} {
		if err := os.Remove(rows[i].SidecarPath); err != nil {
			t.Fatal(err)
		}
	}
	store := &mountHooks{}
	w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, store, nil, staticDir(dir), time.Hour, 20)
	rec := loggingtest.Record(t)

	r := w.tick(context.Background())
	if r.Deleted != 3 || r.Present != 37 || r.Refused != 0 {
		t.Errorf("report %+v, want 37 present and 3 deleted", r)
	}
	if got := w.Status().Refusing; got != "" {
		t.Errorf("the tick refused as %q over a directory nobody touched", got)
	}
	if got := rec.Failures(); len(got) != 1 {
		t.Errorf("want only the summary's WARN (it deleted), got %d line(s): %v", len(got), got)
	}
}
