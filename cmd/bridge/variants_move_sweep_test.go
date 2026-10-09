package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/integrity"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// A `bridge variants move --to X` run while a bridge serves (what the
// console's variants panel and `bridge doctor` tell an operator to do)
// rewrites rows the VariantWatcher may already have listed. The tick
// judges the rows as it listed them, so a row the move relocated after
// the listing reads as gone at both of the places the tick looks (its old
// path, and its canonical place under the variants directory, which is not
// X), and the tick deleted it while its file sat at X (backlog B204; the
// review's scratch test: all 6 moved rows deleted, files intact at X).
// These tests run the real watcher over the real store, through the serve
// wiring's adapters, and the real move code, on a second store over the
// same database, as the CLI opens its own.

// listThenAct is the serve wiring's lister with a hook that runs once, after
// the first listing and before the tick sees it: the moment inside a tick at
// which another writer can change a row the tick has already listed.
type listThenAct struct {
	inner integrity.VariantLister
	once  sync.Once
	act   func()
}

// AllVariants lists through the serve wiring's adapter, then runs the hook.
func (l *listThenAct) AllVariants() ([]integrity.VariantSnapshot, error) {
	rows, err := l.inner.AllVariants()
	l.once.Do(l.act)
	return rows, err
}

// runOneVariantSweep starts a watcher over store, through the serve wiring's
// adapters, with lister in front of the store's, and returns the report of
// its boot tick and the paths it published as deleted. The watcher's context
// is cancelled once that tick has reported, and the watcher joined before
// the store's cleanup closes the store.
func runOneVariantSweep(t *testing.T, store *manifest.Store, lister integrity.VariantLister, dir string) (integrity.SweepReport, []string) {
	t.Helper()
	var (
		mu        sync.Mutex
		published []string
	)
	w := integrity.NewVariantWatcher(lister,
		&integrityVariantReconcilerAdapter{store: store},
		func(paths, _ []string) {
			mu.Lock()
			defer mu.Unlock()
			published = append(published, paths...)
		},
		func() string { return dir }, time.Hour, 20)
	reports := make(chan integrity.SweepReport, 1)
	w.SetOnTickComplete(func(r integrity.SweepReport) {
		select {
		case reports <- r:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(w.Start(ctx))
	select {
	case r := <-reports:
		mu.Lock()
		defer mu.Unlock()
		return r, append([]string(nil), published...)
	case <-serveGiveUp(t):
		t.Fatal("the variant watcher's boot tick never completed")
	}
	return integrity.SweepReport{}, nil
}

// TestAVariantSweepDuringAMoveKeepsTheRowsTheMoveRelocated — the review's
// shape: forty rows, six of them moved to X after the tick listed them,
// which is under the mass-delete floor. Two sidecars removed by hand are the
// positive control: they are gone at both locations and the same tick still
// deletes their rows. On main the six moved rows were deleted with them.
func TestAVariantSweepDuringAMoveKeepsTheRowsTheMoveRelocated(t *testing.T) {
	ctx := context.Background()
	m := newMoveShape(t)
	var moveErr error
	lister := &listThenAct{inner: &integrityVariantListerAdapter{store: m.store}, act: func() {
		moveErr = moveRows(ctx, m.mover, m.moved, m.to)
	}}

	r, published := runOneVariantSweep(t, m.store, lister, m.dir)
	if moveErr != nil {
		t.Fatal(moveErr)
	}
	m.requireOnlyTheGoneRowsDeleted(t)
	if r.Deleted != len(m.gone) || r.Changed != len(m.moved) {
		t.Errorf("report %+v, want %d deleted (the hand-removed sidecars' rows only) and %d changed (the moved ones)",
			r, len(m.gone), len(m.moved))
	}
	if len(published) != len(m.gone) {
		t.Errorf("published %v as deleted, want only the %d hand-removed sidecars' sources", published, len(m.gone))
	}

	// The next tick lists the moved rows as they are now, at X, and finds
	// their sidecars there, while the variants directory is still the old one.
	next, published := runOneVariantSweep(t, m.store, &integrityVariantListerAdapter{store: m.store}, m.dir)
	if next.Deleted != 0 || next.Changed != 0 || next.Present != moveShapeRows-len(m.gone) || len(published) != 0 {
		t.Errorf("the next tick: report %+v, published %v; want every one of the %d rows present", next, published, moveShapeRows-len(m.gone))
	}
}

// moveShapeRows is how many rows newMoveShape seeds.
const moveShapeRows = 40

// moveShape is the review's shape for a sweep during a move: moveShapeRows
// rows with their sidecars under dir; two of them (gone) with their sidecar
// removed by hand, the positive control, gone at both of the places a sweep
// looks; six (moved) for the move to relocate to `to` once the sweep has
// listed them, under the mass-delete floor; and the move's own store
// (mover), over the same database, as the CLI opens one.
type moveShape struct {
	store, mover *manifest.Store
	dir, to      string
	gone, moved  []manifest.VariantRow
}

// newMoveShape builds a moveShape in temporary directories of t.
func newMoveShape(t *testing.T) moveShape {
	t.Helper()
	m := moveShape{
		dir: filepath.Join(t.TempDir(), "variants"),
		to:  filepath.Join(t.TempDir(), "new-disk", "variants"),
	}
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	m.store, _ = relocatedStoreAt(t, dbPath, m.dir, m.dir, moveShapeRows)
	listed, err := m.store.AllVariants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.gone = []manifest.VariantRow{listed[3], listed[31]}
	for _, v := range m.gone {
		if err := os.Remove(v.SidecarPath); err != nil {
			t.Fatal(err)
		}
	}
	m.moved = listed[10:16]
	m.mover, err = manifest.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.mover.Close() })
	return m
}

// requireOnlyTheGoneRowsDeleted fails the test unless the moved rows record
// their place under to, with their sidecars there, the gone rows are deleted,
// and every other row is still in the store.
func (m moveShape) requireOnlyTheGoneRowsDeleted(t *testing.T) {
	t.Helper()
	requireMovedRowsAt(t, m.store, m.moved, m.to)
	requireRowsGone(t, m.store, m.gone)
	rows, err := m.store.AllVariants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := moveShapeRows - len(m.gone); len(rows) != want {
		t.Errorf("%d rows after the sweep, want %d", len(rows), want)
	}
}

// moveRows moves each of rows to its place under to with the move's own
// per-row pipeline, over the move's store, and returns the first failure.
// It runs on the watcher's goroutine, inside a tick, so it reports rather
// than failing the test.
func moveRows(ctx context.Context, mover *manifest.Store, rows []manifest.VariantRow, to string) error {
	for _, v := range rows {
		if err := moveOneVariant(ctx, mover, v, computeNewSidecarPath(to, v)); err != nil {
			return fmt.Errorf("move %s: %w", v.SourcePath, err)
		}
	}
	return nil
}

// requireMovedRowsAt fails the test for each of moved whose row is gone,
// does not record its place under to, or whose sidecar is not there.
func requireMovedRowsAt(t *testing.T, store *manifest.Store, moved []manifest.VariantRow, to string) {
	t.Helper()
	for _, v := range moved {
		want := computeNewSidecarPath(to, v)
		row, err := store.GetVariant(context.Background(), v.SourcePath, v.VariantID)
		switch {
		case err != nil:
			t.Fatal(err)
		case row == nil:
			t.Errorf("the sweep deleted the row of %s, which the move had relocated to %s", v.SourcePath, want)
		case row.SidecarPath != want:
			t.Errorf("row %s records %s, want the move's %s", v.SourcePath, row.SidecarPath, want)
		}
		if _, err := os.Stat(want); err != nil {
			t.Errorf("the moved sidecar of %s: %v", v.SourcePath, err)
		}
	}
}

// requireRowsGone fails the test for each of gone whose row is still there:
// the positive control, rows whose sidecar is gone at both locations.
func requireRowsGone(t *testing.T, store *manifest.Store, gone []manifest.VariantRow) {
	t.Helper()
	for _, v := range gone {
		if row, err := store.GetVariant(context.Background(), v.SourcePath, v.VariantID); err != nil || row != nil {
			t.Errorf("control: the row of %s, whose sidecar is gone at both locations, was kept (row %v, err %v)", v.SourcePath, row, err)
		}
	}
}

// TestAWatcherTickBetweenTheFileAndTheRowKeepsTheMovedRow is the window
// the compare-and-delete cannot see. The tick runs after the file step
// and before the row update. A file that has already left the recorded
// path is judged gone, and the snapshot still matches the DELETE. The
// two hand-removed sidecars are the positive control.
func TestAWatcherTickBetweenTheFileAndTheRowKeepsTheMovedRow(t *testing.T) {
	m := newMoveShape(t)
	prev := moveBeforeRowUpdate
	t.Cleanup(func() { moveBeforeRowUpdate = prev })
	row := m.moved[0]
	var report integrity.SweepReport
	moveBeforeRowUpdate = func() {
		report, _ = runOneVariantSweep(t, m.store, &integrityVariantListerAdapter{store: m.store}, m.dir)
	}
	newPath := computeNewSidecarPath(m.to, row)
	if err := moveOneVariant(context.Background(), m.mover, row, newPath); err != nil {
		t.Fatal(err)
	}
	requireMovedRowsAt(t, m.store, []manifest.VariantRow{row}, m.to)
	requireRowsGone(t, m.store, m.gone)
	if report.Deleted != len(m.gone) {
		t.Fatalf("sweep deleted %d rows, want the %d hand-removed ones", report.Deleted, len(m.gone))
	}
}

// TestAMoveOntoTheSameFileUpdatesTheRowAndKeepsIt: a --to that follows
// a link back to the variants directory is another spelling of the file.
// Removing the source name would remove the only copy.
func TestAMoveOntoTheSameFileUpdatesTheRowAndKeepsIt(t *testing.T) {
	m := newMoveShape(t)
	link := filepath.Join(t.TempDir(), "variants-link")
	if err := os.Symlink(m.dir, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	row := m.moved[0]
	newPath := computeNewSidecarPath(link, row)
	if err := moveOneVariant(context.Background(), m.mover, row, newPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(row.SidecarPath); err != nil {
		t.Fatalf("the only copy is gone: %v", err)
	}
	got, err := m.store.GetVariant(context.Background(), row.SourcePath, row.VariantID)
	if err != nil || got == nil {
		t.Fatalf("row: %v", err)
	}
	if got.SidecarPath != newPath {
		t.Fatalf("sidecar_path = %q, want %q", got.SidecarPath, newPath)
	}
}

// TestAMoveToAnotherSpellingOfTheSameFileKeepsTheOnlyCopy is the
// case-insensitive volume's form of the same-file guard.
func TestAMoveToAnotherSpellingOfTheSameFileKeepsTheOnlyCopy(t *testing.T) {
	m := newMoveShape(t)
	probe := filepath.Join(m.dir, "CaseProbe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.dir, "caseprobe")); err != nil {
		t.Skip("this volume tells the two spellings apart")
	}
	row := m.moved[0]
	base := filepath.Base(row.SidecarPath)
	flippedRunes := []rune(base)
	flippedAt := -1
	for i, r := range flippedRunes {
		switch {
		case r >= 'a' && r <= 'z':
			flippedRunes[i] = r - 'a' + 'A'
			flippedAt = i
		case r >= 'A' && r <= 'Z':
			flippedRunes[i] = r - 'A' + 'a'
			flippedAt = i
		}
		if flippedAt >= 0 {
			break
		}
	}
	if flippedAt < 0 {
		t.Fatal("filename has no letter to flip")
	}
	flipped := string(flippedRunes)
	newPath := filepath.Join(filepath.Dir(row.SidecarPath), flipped)
	if err := moveOneVariant(context.Background(), m.mover, row, newPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(row.SidecarPath); err != nil {
		t.Fatalf("the only copy is gone: %v", err)
	}
	got, err := m.store.GetVariant(context.Background(), row.SourcePath, row.VariantID)
	if err != nil || got == nil {
		t.Fatalf("row: %v", err)
	}
	if got.SidecarPath != newPath {
		t.Fatalf("sidecar_path = %q, want %q", got.SidecarPath, newPath)
	}
}

// TestAMoveCopiesWhenTheLinkCannotBeMade keeps the source name until
// the row points at the destination. A link that fails copies, and the
// source name is removed only after the update.
func TestAMoveCopiesWhenTheLinkCannotBeMade(t *testing.T) {
	m := newMoveShape(t)
	prevLink := linkSidecar
	prevHook := moveBeforeRowUpdate
	t.Cleanup(func() {
		linkSidecar = prevLink
		moveBeforeRowUpdate = prevHook
	})
	linkSidecar = func(string, string) error { return errors.New("cross-device") }
	row := m.moved[0]
	newPath := computeNewSidecarPath(m.to, row)
	sawBoth := false
	moveBeforeRowUpdate = func() {
		if _, err := os.Stat(row.SidecarPath); err != nil {
			t.Errorf("source name gone before the row update: %v", err)
		}
		if _, err := os.Stat(newPath); err != nil {
			t.Errorf("destination missing before the row update: %v", err)
		}
		sawBoth = true
	}
	if err := moveOneVariant(context.Background(), m.mover, row, newPath); err != nil {
		t.Fatal(err)
	}
	if !sawBoth {
		t.Fatal("the hook did not run")
	}
	if _, err := os.Stat(row.SidecarPath); !os.IsNotExist(err) {
		t.Fatalf("source name remains after the update: %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatal(err)
	}
	requireMovedRowsAt(t, m.store, []manifest.VariantRow{row}, m.to)
}
