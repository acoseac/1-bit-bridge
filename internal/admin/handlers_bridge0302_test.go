package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestRootsRemoveNormalizesPath is the F5/F7 regression: apiRootsRemove
// must normalize the incoming path (TrimSpace + filepath.Abs) the same
// way apiRootsAdd does, so an untrimmed / trailing-slash form of a
// stored absolute root still matches instead of false-tripping a 404.
func TestRootsRemoveNormalizesPath(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	h := srv.Handler()

	// Add a second root so removal isn't blocked by the last-root guard.
	extra := filepath.Join(filepath.Dir(cfg.DataDir), "Extra")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	if code := doJSON(t, h, "POST", "/api/roots", map[string]string{"path": extra}, nil); code != http.StatusCreated {
		t.Fatalf("add root: %d", code)
	}

	// Remove using a leading/trailing-space + trailing-slash variant of
	// the stored absolute path. Pre-fix (raw slices.Index) this 404'd.
	messy := "  " + extra + string(filepath.Separator) + "  "
	if code := doJSON(t, h, "DELETE", "/api/roots", map[string]string{"path": messy}, nil); code != http.StatusNoContent {
		t.Errorf("remove with untrimmed/trailing-slash path: got %d, want 204", code)
	}
	if got := srv.deps.Scanner.Roots(); len(got) != 1 {
		t.Errorf("roots after remove = %v, want 1", got)
	}
}

// TestRootsRemoveUnknownPathStill404 guards that normalization doesn't
// mask a genuine miss: a path that isn't a configured root still 404s.
func TestRootsRemoveUnknownPathStill404(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	extra := filepath.Join(filepath.Dir(cfg.DataDir), "Extra")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	if code := doJSON(t, h, "POST", "/api/roots", map[string]string{"path": extra}, nil); code != http.StatusCreated {
		t.Fatalf("add root: %d", code)
	}
	code := doJSON(t, h, "DELETE", "/api/roots", map[string]string{"path": "/definitely/not/a/root"}, nil)
	if code != http.StatusNotFound {
		t.Errorf("remove unknown root: got %d, want 404", code)
	}
}

// TestRootsRemoveEmptyPathRejected guards the Gemini review finding: a
// whitespace-only path trims to "" and filepath.Abs("") would resolve to
// the process CWD — reject with 400 instead of matching a root or 404ing.
func TestRootsRemoveEmptyPathRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	code := doJSON(t, srv.Handler(), "DELETE", "/api/roots", map[string]string{"path": "   "}, nil)
	if code != http.StatusBadRequest {
		t.Errorf("remove whitespace-only path: got %d, want 400", code)
	}
}

// TestSoxAvailabilityProbeRunsUnlocked is the F8 regression: the probe
// (a ≤2 s `sox --help` shell-out in prod) must run WITHOUT
// soxAvailabilityMu held, or concurrent SSE snapshot callers block on
// it. TryLock succeeds iff the lock is free while the probe runs — with
// the pre-fix deferred unlock it would be held and this fails.
func TestSoxAvailabilityProbeRunsUnlocked(t *testing.T) {
	srv, _, _ := newTestServer(t)
	probedUnlocked := false
	srv.deps.UpscalePrecheck = func() error {
		if srv.soxAvailabilityMu.TryLock() {
			probedUnlocked = true
			srv.soxAvailabilityMu.Unlock()
		}
		return nil
	}
	_ = srv.cachedSoxAvailability()
	if !probedUnlocked {
		t.Error("UpscalePrecheck ran while soxAvailabilityMu was held; the probe must run unlocked")
	}
}
