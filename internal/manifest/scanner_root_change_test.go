package manifest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestARootChangeDuringAScanDoesNotLeaveTheOldRows parks a scan on the
// extract hook and changes the roots before that scan can write. The
// removed root's rows, and a collapsed library's old multi-root paths,
// must stay gone after the scan that follows. The missing-count threshold
// is 3, so a second scan only counts a reinserted row missing and the
// assertion still sees it.
func TestARootChangeDuringAScanDoesNotLeaveTheOldRows(t *testing.T) {
	t.Run("collapse", func(t *testing.T) {
		runRootChangeDuringScan(t, []string{"Keep", "Drop"}, []string{"Keep", "Drop"},
			[]string{"Album/01.flac"},
			[]string{"Drop/Album/01.flac", "Keep/Album/01.flac"},
			func(ctx context.Context, store *Store, sc *Scanner, abs map[string]string) error {
				if _, err := store.RecordFirstIndexedCarryAndWipe(ctx, true, "Keep"); err != nil {
					return err
				}
				sc.SetRoots([]string{abs["Keep"]})
				return nil
			})
	})
	t.Run("remove", func(t *testing.T) {
		runRootChangeDuringScan(t, []string{"Keep", "Drop", "Other"}, []string{"Keep", "Drop", "Other"},
			[]string{"Keep/Album/01.flac", "Other/Album/01.flac"},
			[]string{"Drop/Album/01.flac"},
			func(ctx context.Context, store *Store, sc *Scanner, abs map[string]string) error {
				if _, err := store.DeleteTracksByPrefix(ctx, "Drop/"); err != nil {
					return err
				}
				sc.SetRoots([]string{abs["Keep"], abs["Other"]})
				return nil
			})
	})
	t.Run("add", func(t *testing.T) {
		runRootChangeDuringScan(t, []string{"Keep"}, []string{"Keep", "Extra"},
			[]string{"Keep/Album/01.flac", "Extra/Album/01.flac"},
			[]string{"Album/01.flac"},
			func(ctx context.Context, store *Store, sc *Scanner, abs map[string]string) error {
				if _, err := store.RecordFirstIndexedCarryAndWipe(ctx, false, "Keep"); err != nil {
					return err
				}
				sc.SetRoots([]string{abs["Keep"], abs["Extra"]})
				return nil
			})
	})
}

func runRootChangeDuringScan(t *testing.T, initial, withFiles, present, absent []string, mutate func(context.Context, *Store, *Scanner, map[string]string) error) {
	t.Helper()
	base := t.TempDir()
	abs := map[string]string{}
	var roots []string
	for _, name := range withFiles {
		dir := filepath.Join(base, name)
		abs[name] = dir
		album := filepath.Join(dir, "Album")
		if err := os.MkdirAll(album, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(album, "01.flac"), []byte("not-a-real-flac"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range initial {
		roots = append(roots, abs[name])
	}

	store, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sc := NewScanner(roots, store, "")
	sc.SetDeleteThreshold(3)

	removalDone := make(chan struct{})
	var closeRemoval sync.Once
	release := func() { closeRemoval.Do(func() { close(removalDone) }) }
	defer release()

	parked, scanDone := sc.StartParkedScanForTests(removalDone)
	t.Cleanup(func() { sc.SetAfterExtractHookForTests(nil) })
	defer func() {
		select {
		case <-scanDone:
		case <-time.After(5 * time.Second):
		}
	}()
	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("scan did not reach the extract hook")
	}

	sc.CommitRootChange(func() {
		if err := mutate(context.Background(), store, sc, abs); err != nil {
			t.Errorf("root change: %v", err)
		}
	})
	release()
	var scanErr error
	select {
	case scanErr = <-scanDone:
	case <-time.After(30 * time.Second):
		t.Fatal("parked scan did not finish")
	}
	if scanErr != nil && !errors.Is(scanErr, context.Canceled) {
		t.Fatal(scanErr)
	}
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := store.TrackPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, p := range got {
		have[p] = true
	}
	for _, p := range present {
		if !have[p] {
			t.Errorf("missing %s in %v", p, got)
		}
	}
	for _, p := range absent {
		if have[p] {
			t.Errorf("still present %s in %v", p, got)
		}
	}
}
