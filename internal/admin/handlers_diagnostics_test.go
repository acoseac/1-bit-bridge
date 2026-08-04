package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// getDiagnostics drives the handler and decodes its body.
func getDiagnostics(t *testing.T, s *Server) (diagnosticsResponse, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/diagnostics", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	s.apiDiagnostics(rec, req)
	var out diagnosticsResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
		}
	}
	return out, rec
}

// TestDiagnosticsServesWithSubsystemsOff is the contract that matters
// most on this endpoint: it must answer when things are broken or
// switched off, because that is exactly when someone opens it.
//
// A test Server has no upscale pool wired and no tsnet node, which is
// also the shape of a plain loopback install. Erroring, or omitting the
// panel, would make the page useless in its primary situation.
func TestDiagnosticsServesWithSubsystemsOff(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.deps.UpscaleStats = nil

	out, rec := getDiagnostics(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with no upscale pool wired", rec.Code)
	}
	if out.UpscaleJobsInFlight != 0 || out.UpscaleJobsCompletedTotal != 0 {
		t.Errorf("upscale counters = %d/%d with no pool wired, want zeros",
			out.UpscaleJobsInFlight, out.UpscaleJobsCompletedTotal)
	}
	if out.LogEventCounts == nil {
		t.Error("logEventCounts is nil; the field should always be present so the UI has a map to iterate")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — these are point-in-time counters "+
			"and a cache hit shows stale numbers to someone watching for a change", got)
	}
}

// TestDiagnosticsReadsUpscalePoolWhenWired pins that the numbers come
// from the pool closure this server already carries, rather than a
// second snapshot path that could drift from the Jobs page.
func TestDiagnosticsReadsUpscalePoolWhenWired(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.deps.UpscaleStats = func() *UpscalePoolStats {
		return &UpscalePoolStats{Inflight: 3, Done: 40, Failed: 2}
	}

	out, _ := getDiagnostics(t, s)
	if out.UpscaleJobsInFlight != 3 {
		t.Errorf("inFlight = %d, want 3", out.UpscaleJobsInFlight)
	}
	// Completed is done + failed: a job that failed is finished, and
	// reporting only successes would make the pool look stalled while it
	// was steadily failing.
	if out.UpscaleJobsCompletedTotal != 42 {
		t.Errorf("completed = %d, want 42 (done 40 + failed 2)", out.UpscaleJobsCompletedTotal)
	}
}

// TestDiagnosticsUptimeFromStartedAt pins that uptime comes from the same
// StartedAt the rest of the console renders from, so the Diagnostics page
// can't contradict the dashboard.
func TestDiagnosticsUptimeFromStartedAt(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.deps.StartedAt = time.Now().Add(-90 * time.Second)

	out, _ := getDiagnostics(t, s)
	if out.ServerUptimeSeconds < 89 || out.ServerUptimeSeconds > 120 {
		t.Errorf("uptime = %ds, want ~90", out.ServerUptimeSeconds)
	}

	// An unset StartedAt must read as zero rather than as "56 years",
	// which is what time.Since on a zero Time produces.
	s.deps.StartedAt = time.Time{}
	out, _ = getDiagnostics(t, s)
	if out.ServerUptimeSeconds != 0 {
		t.Errorf("uptime = %d with an unset StartedAt, want 0", out.ServerUptimeSeconds)
	}
}

// TestDiagnosticsCacheRatioDistinguishesNoLookups pins the difference
// between "the cache is missing everything" and "nothing has asked it
// yet". Both produce a 0 ratio; only the second is normal, and the UI
// keys off MBCacheLookups to say so.
func TestDiagnosticsCacheRatioDistinguishesNoLookups(t *testing.T) {
	s, _, _ := newTestServer(t)
	out, _ := getDiagnostics(t, s)
	if out.MBCacheLookups == 0 && out.MBCacheHitRatio != 0 {
		t.Errorf("ratio = %v with zero lookups, want 0", out.MBCacheHitRatio)
	}
	// The field must be PRESENT in the payload even at zero, or the UI
	// cannot tell the two cases apart.
	var raw map[string]any
	_, rec := getDiagnostics(t, s)
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["mbCacheLookups"]; !ok {
		t.Error("mbCacheLookups absent from the payload; without it a 0 ratio is ambiguous")
	}
}

// TestTailscaleStateLabel pins the mapping this file duplicates from
// internal/api. The duplication is deliberate (the import direction
// forbids sharing), so each side needs its own pin or they drift.
func TestTailscaleStateLabel(t *testing.T) {
	for state, want := range map[int]string{
		0: "down", 1: "starting", 2: "running", 3: "disabled", 99: "down",
	} {
		if got := tailscaleStateLabel(state); got != want {
			t.Errorf("tailscaleStateLabel(%d) = %q, want %q", state, got, want)
		}
	}
}

// TestDiagnosticsPollActuallyPauses pins that the poll STOPS while the
// tab is hidden, not merely refreshes on return.
//
// The first version of initDiagnostics carried a comment saying it
// paused and did not — the interval kept firing every 5s in the
// background, which was the entire cost the comment claimed to avoid. A
// comment asserting behaviour the code lacks is the failure mode this
// repo has been bitten by before (review item 5.7), so it gets a test
// rather than a re-read.
func TestDiagnosticsPollActuallyPauses(t *testing.T) {
	b, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	i := strings.Index(js, "function initDiagnostics(")
	if i < 0 {
		t.Fatal("initDiagnostics not found in app.js")
	}
	body := js[i:]
	if j := strings.Index(body[1:], "\nfunction "); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "clearInterval") {
		t.Error("initDiagnostics never clears its interval; the poll runs on in a hidden tab")
	}
	if !strings.Contains(body, "document.hidden") {
		t.Error("initDiagnostics does not branch on document.hidden, so it cannot pause")
	}
}
