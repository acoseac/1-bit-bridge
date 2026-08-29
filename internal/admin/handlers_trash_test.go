package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/trash"
)

func enableDelete(t *testing.T, srv *Server, on bool) {
	t.Helper()
	cfg := config.Clone(srv.deps.CfgHolder.Load())
	cfg.Library.AllowDelete = on
	srv.deps.CfgHolder.Store(cfg)
}

// wireTrash attaches a manager reading the live gate, as production does.
func wireTrash(t *testing.T, srv *Server) {
	t.Helper()
	srv.deps.TrashManager = trash.New(
		srv.deps.Scanner.Roots,
		func() bool {
			c := srv.deps.CfgHolder.Load()
			return c != nil && c.Library.AllowDelete
		},
		trash.DefaultTTL,
	)
	srv.deps.Trash = srv.deps.TrashManager.Reclaimable
}

func seedLibraryFile(t *testing.T, root, rel, body string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestDeleteRefusedWhenAllowDeleteIsOff, and the control that uploads alone do
// NOT turn it on. Enabling an additive feature must never silently enable a
// destructive one — that is the whole reason these are two gates.
func TestDeleteRefusedWhenAllowDeleteIsOff(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	resetSpaceCacheForTest()
	t.Cleanup(resetSpaceCacheForTest)
	wireTrash(t, srv)
	seedLibraryFile(t, cfg.LibraryRoots[0], "A/x.flac", "audio")

	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/library/trash"},
		{"GET", "/api/library/trash"},
		{"POST", "/api/library/trash/restore"},
		{"DELETE", "/api/library/trash"},
	} {
		var out map[string]any
		code := doJSON(t, srv.Handler(), tc.method, tc.path, map[string]any{
			"paths": []string{"A/x.flac"}, "ids": []string{"1/A/x.flac"},
		}, &out)
		if code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403 while deleting is off", tc.method, tc.path, code)
		}
	}

	// CONTROL: turning uploads on must not turn deleting on.
	live := config.Clone(srv.deps.CfgHolder.Load())
	live.Upload.Enabled = true
	srv.deps.CfgHolder.Store(live)
	var out map[string]any
	if code := doJSON(t, srv.Handler(), "POST", "/api/library/trash",
		map[string]any{"paths": []string{"A/x.flac"}}, &out); code != http.StatusForbidden {
		t.Errorf("enabling uploads also enabled deleting: %d", code)
	}
	if _, err := os.Stat(filepath.Join(cfg.LibraryRoots[0], "A", "x.flac")); err != nil {
		t.Error("a file was deleted while the delete gate was off")
	}
}

func TestTrashRoundTripThroughTheAPI(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	resetSpaceCacheForTest()
	t.Cleanup(resetSpaceCacheForTest)
	wireTrash(t, srv)
	enableDelete(t, srv, true)
	root := cfg.LibraryRoots[0]
	src := seedLibraryFile(t, root, "Artist/Album/01.flac", "audio!")

	// The row must exist for the retire step to have something to do.
	if err := srv.deps.Manifest.UpsertTrack(t.Context(), &manifest.Track{
		Path: "Artist/Album/01.flac", Size: 6,
	}); err != nil {
		t.Fatal(err)
	}

	var res map[string]any
	if code := doJSON(t, srv.Handler(), "POST", "/api/library/trash",
		map[string]any{"paths": []string{"Artist/Album/01.flac"}}, &res); code != http.StatusOK {
		t.Fatalf("trash = %d (%v)", code, res)
	}
	if ok, _ := res["ok"].(float64); int(ok) != 1 {
		t.Fatalf("trash result = %v", res)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("the original path still exists")
	}

	// It is listed, with an expiry.
	var listed []map[string]any
	doJSON(t, srv.Handler(), "GET", "/api/library/trash", nil, &listed)
	if len(listed) != 1 {
		t.Fatalf("trash listing has %d entries, want 1", len(listed))
	}
	if listed[0]["expiresAt"] == "" || listed[0]["expiresAt"] == nil {
		t.Error("no expiry reported — the operator cannot tell how long they have to undo")
	}
	id, _ := listed[0]["id"].(string)

	// And the space widget reports it as reclaimable — trash does NOT free
	// space, so this is the number that tells the operator they are not done.
	var sp map[string]any
	doJSON(t, srv.Handler(), "GET", "/api/library/space", nil, &sp)
	if got, _ := sp["reclaimableBytes"].(float64); int(got) != 6 {
		t.Errorf("reclaimableBytes = %v, want 6", sp["reclaimableBytes"])
	}

	// Restore puts it back.
	var rr map[string]any
	if code := doJSON(t, srv.Handler(), "POST", "/api/library/trash/restore",
		map[string]any{"ids": []string{id}}, &rr); code != http.StatusOK {
		t.Fatalf("restore = %d (%v)", code, rr)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("restored file missing: %v", err)
	}
}

// TestPurgeIsWhatActuallyReclaims — the honest tension in the design: trashing
// frees nothing, so an operator who deleted to make room and stopped there is
// still full.
func TestPurgeIsWhatActuallyReclaims(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	resetSpaceCacheForTest()
	t.Cleanup(resetSpaceCacheForTest)
	wireTrash(t, srv)
	enableDelete(t, srv, true)
	seedLibraryFile(t, cfg.LibraryRoots[0], "A/x.flac", "0123456789")

	var res map[string]any
	doJSON(t, srv.Handler(), "POST", "/api/library/trash",
		map[string]any{"paths": []string{"A/x.flac"}}, &res)
	if got := srv.deps.Trash(cfg.LibraryRoots[0]); got != 10 {
		t.Fatalf("reclaimable after trashing = %d, want 10", got)
	}
	// A bodyless DELETE means "empty the trash".
	var pr map[string]any
	if code := doJSON(t, srv.Handler(), "DELETE", "/api/library/trash", nil, &pr); code != http.StatusOK {
		t.Fatalf("purge = %d (%v)", code, pr)
	}
	if got, _ := pr["bytes"].(float64); int(got) != 10 {
		t.Errorf("purge reported %v bytes, want 10", pr["bytes"])
	}
	if got := srv.deps.Trash(cfg.LibraryRoots[0]); got != 0 {
		t.Errorf("reclaimable = %d after emptying the trash", got)
	}
}

// TestTrashRefusesPathsOutsideTheRoot — the API layer must not be the only
// thing standing between a hostile path and the filesystem, but it must also
// not be the thing that lets one through.
func TestTrashRefusesPathsOutsideTheRoot(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	resetSpaceCacheForTest()
	t.Cleanup(resetSpaceCacheForTest)
	wireTrash(t, srv)
	enableDelete(t, srv, true)
	root := cfg.LibraryRoots[0]
	outside := filepath.Join(filepath.Dir(root), "outside.flac")
	if err := os.WriteFile(outside, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	doJSON(t, srv.Handler(), "POST", "/api/library/trash", map[string]any{
		"paths": []string{"../outside.flac", "/etc/passwd", ".bridge-trash/1/x.flac"},
	}, &res)
	if failed, _ := res["failed"].(float64); int(failed) != 3 {
		t.Errorf("failed = %v, want 3 — a hostile path was accepted (%v)", res["failed"], res)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("a file OUTSIDE the library root was deleted")
	}
}

func TestAllowDeleteReportsLive(t *testing.T) {
	srv, _, _ := newTestServer(t)
	wireTrash(t, srv)
	var out map[string]any
	if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
		map[string]any{"allowDelete": true}, &out); code != http.StatusOK {
		t.Fatalf("patch = %d (%v)", code, out)
	}
	fields, _ := out["fields"].(map[string]any)
	row, _ := fields["allowDelete"].(map[string]any)
	if row == nil {
		t.Fatalf("no field report for allowDelete: %v", out)
	}
	if status, _ := row["status"].(string); status != "live" {
		t.Errorf("allowDelete status = %q, want live", status)
	}
}
