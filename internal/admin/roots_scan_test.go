package admin

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestARootChangeThroughTheHandlerDoesNotLeaveTheOldRows drives
// POST /api/roots and DELETE /api/roots while a scan is parked in the
// extract hook. Collapse, removing one of three roots, and adding a
// second root must leave only the path form the roots have afterwards.
func TestARootChangeThroughTheHandlerDoesNotLeaveTheOldRows(t *testing.T) {
	t.Run("collapse", func(t *testing.T) {
		runHandlerRootChange(t, "collapse",
			[]string{"Album/01.flac"},
			[]string{"Music/Album/01.flac", "Extra/Album/01.flac"})
	})
	t.Run("remove", func(t *testing.T) {
		runHandlerRootChange(t, "remove",
			[]string{"Music/Album/01.flac", "Third/Album/01.flac"},
			[]string{"Extra/Album/01.flac"})
	})
	t.Run("add", func(t *testing.T) {
		runHandlerRootChange(t, "add",
			[]string{"Music/Album/01.flac", "Extra/Album/01.flac"},
			[]string{"Album/01.flac"})
	})
}

func runHandlerRootChange(t *testing.T, kind string, present, absent []string) {
	t.Helper()
	srv, cfg, _ := newTestServer(t)
	music := cfg.LibraryRoots[0]
	srv.deps.Scanner.SetDeleteThreshold(3)
	h := srv.Handler()

	extra := filepath.Join(filepath.Dir(music), "Extra")
	third := filepath.Join(filepath.Dir(music), "Third")
	switch kind {
	case "collapse":
		writeAlbumFLAC(t, music)
		writeAlbumFLAC(t, extra)
		if code := doJSON(t, h, http.MethodPost, "/api/roots", map[string]string{"path": extra}, nil); code != http.StatusCreated {
			t.Fatalf("add extra: status %d", code)
		}
		waitForBackgroundScans(t, srv)
		rewriteAlbumFLAC(t, music)
		rewriteAlbumFLAC(t, extra)
	case "remove":
		writeAlbumFLAC(t, music)
		writeAlbumFLAC(t, extra)
		writeAlbumFLAC(t, third)
		if code := doJSON(t, h, http.MethodPost, "/api/roots", map[string]string{"path": extra}, nil); code != http.StatusCreated {
			t.Fatalf("add extra: status %d", code)
		}
		waitForBackgroundScans(t, srv)
		if code := doJSON(t, h, http.MethodPost, "/api/roots", map[string]string{"path": third}, nil); code != http.StatusCreated {
			t.Fatalf("add third: status %d", code)
		}
		waitForBackgroundScans(t, srv)
		rewriteAlbumFLAC(t, music)
		rewriteAlbumFLAC(t, extra)
		rewriteAlbumFLAC(t, third)
	case "add":
		writeAlbumFLAC(t, music)
		writeAlbumFLAC(t, extra)
	default:
		t.Fatalf("unknown kind %s", kind)
	}

	removalDone := make(chan struct{})
	var closeRemoval sync.Once
	release := func() { closeRemoval.Do(func() { close(removalDone) }) }
	t.Cleanup(release)

	sc := srv.deps.Scanner
	parked := make(chan struct{})
	var once sync.Once
	sc.SetAfterExtractHookForTests(func(string) {
		once.Do(func() {
			close(parked)
			select {
			case <-sc.InFlightScanDone():
			case <-removalDone:
			}
		})
	})
	t.Cleanup(func() { sc.SetAfterExtractHookForTests(nil) })

	scanFinished := make(chan struct{})
	var scanErr error
	go func() {
		_, scanErr = sc.Scan(context.Background())
		close(scanFinished)
	}()
	t.Cleanup(func() {
		release()
		select {
		case <-scanFinished:
		case <-time.After(5 * time.Second):
		}
	})
	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("scan did not reach the extract hook")
	}

	switch kind {
	case "collapse", "remove":
		if code := doJSON(t, h, http.MethodDelete, "/api/roots", map[string]string{"path": extra}, nil); code != http.StatusNoContent {
			t.Fatalf("remove: status %d", code)
		}
	case "add":
		if code := doJSON(t, h, http.MethodPost, "/api/roots", map[string]string{"path": extra}, nil); code != http.StatusCreated {
			t.Fatalf("add: status %d", code)
		}
	}
	release()
	select {
	case <-scanFinished:
	case <-time.After(30 * time.Second):
		t.Fatal("parked scan did not finish")
	}
	if scanErr != nil && !errors.Is(scanErr, context.Canceled) {
		t.Fatal(scanErr)
	}
	waitForBackgroundScans(t, srv)

	got, err := srv.deps.Manifest.TrackPaths(context.Background())
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
			t.Errorf("still holding %s in %v", p, got)
		}
	}
}

func writeAlbumFLAC(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "Album")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "01.flac"), []byte("not-a-real-flac"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rewriteAlbumFLAC(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "Album", "01.flac"), []byte("not-a-real-flac-again"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitForBackgroundScans(t *testing.T, srv *Server) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		srv.bgScans.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("background scan did not finish")
	}
}
