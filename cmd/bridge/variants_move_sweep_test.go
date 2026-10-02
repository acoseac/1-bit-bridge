package main

import (
	"context"
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
	dir := filepath.Join(t.TempDir(), "variants")
	to := filepath.Join(t.TempDir(), "new-disk", "variants")
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	store, _ := relocatedStoreAt(t, dbPath, dir, dir, 40)

	listed, err := store.AllVariants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gone := []manifest.VariantRow{listed[3], listed[31]}
	for _, v := range gone {
		if err := os.Remove(v.SidecarPath); err != nil {
			t.Fatal(err)
		}
	}
	moved := listed[10:16]

	// The move's own store, over the same database, as the CLI opens one.
	mover, err := manifest.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mover.Close() })
	var moveErr error
	lister := &listThenAct{inner: &integrityVariantListerAdapter{store: store}, act: func() {
		moveErr = moveRows(ctx, mover, moved, to)
	}}

	r, published := runOneVariantSweep(t, store, lister, dir)
	if moveErr != nil {
		t.Fatal(moveErr)
	}
	requireMovedRowsAt(t, store, moved, to)
	requireRowsGone(t, store, gone)
	if r.Deleted != len(gone) || r.Changed != len(moved) {
		t.Errorf("report %+v, want %d deleted (the hand-removed sidecars' rows only) and %d changed (the moved ones)",
			r, len(gone), len(moved))
	}
	if len(published) != len(gone) {
		t.Errorf("published %v as deleted, want only the %d hand-removed sidecars' sources", published, len(gone))
	}
	rows, err := store.AllVariants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 40-len(gone) {
		t.Errorf("%d rows after the sweep, want %d", len(rows), 40-len(gone))
	}

	// The next tick lists the moved rows as they are now, at X, and finds
	// their sidecars there, while the variants directory is still the old one.
	next, published := runOneVariantSweep(t, store, &integrityVariantListerAdapter{store: store}, dir)
	if next.Deleted != 0 || next.Changed != 0 || next.Present != 40-len(gone) || len(published) != 0 {
		t.Errorf("the next tick: report %+v, published %v; want every one of the %d rows present", next, published, 40-len(gone))
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
