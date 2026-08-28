package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestRestartDrainsInflightStreams is the point of the change: a restart
// applied on the operator's behalf must not cut someone off mid-track.
func TestRestartDrainsInflightStreams(t *testing.T) {
	srv, _, _ := newTestServer(t)
	var inflight atomic.Int64
	inflight.Store(2)
	srv.deps.InflightSessions = inflight.Load

	// Streams finish shortly after the request lands.
	go func() {
		time.Sleep(400 * time.Millisecond)
		inflight.Store(0)
	}()

	start := time.Now()
	var resp restartResponse
	code := doJSON(t, srv.Handler(), "POST", "/api/restart",
		map[string]any{"maxWaitSec": 5}, &resp)
	elapsed := time.Since(start)

	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	if !resp.Drained {
		t.Errorf("drained = false (inflight %d) — the handler returned while streams were "+
			"still running", resp.Inflight)
	}
	if resp.Inflight != 0 {
		t.Errorf("inflight = %d, want 0", resp.Inflight)
	}
	if elapsed < 300*time.Millisecond {
		t.Errorf("returned after %v — too fast to have waited for the streams", elapsed)
	}
}

// TestRestartReturnsImmediatelyWhenIdle — the wait must cost nothing in
// the common case. If an idle bridge paid the poll interval on every
// restart, the drain would be a tax on the path that never needed it.
func TestRestartReturnsImmediatelyWhenIdle(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.deps.InflightSessions = func() int64 { return 0 }

	start := time.Now()
	var resp restartResponse
	if code := doJSON(t, srv.Handler(), "POST", "/api/restart", nil, &resp); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	if !resp.Drained || resp.Inflight != 0 {
		t.Errorf("got %+v, want a clean immediate drain", resp)
	}
	if elapsed := time.Since(start); elapsed > restartDrainPoll {
		t.Errorf("idle restart took %v — it waited a poll interval it did not need", elapsed)
	}
}

// TestRestartReportsAnInterruptedStream is the honesty half. A stream can
// outlive any deadline, so the wait is bounded — and when it gives up it
// has to SAY it interrupted someone. Reporting a clean drain we could not
// verify would have a control plane record a graceful restart and never
// learn it cut a listener off.
func TestRestartReportsAnInterruptedStream(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.deps.InflightSessions = func() int64 { return 3 } // never drains

	var resp restartResponse
	code := doJSON(t, srv.Handler(), "POST", "/api/restart",
		map[string]any{"maxWaitSec": 1}, &resp)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 — a timed-out drain still restarts", code)
	}
	if resp.Drained {
		t.Error("drained = true while 3 streams were still in flight")
	}
	if resp.Inflight != 3 {
		t.Errorf("inflight = %d, want 3", resp.Inflight)
	}
	if resp.Reason == "" {
		t.Error("no reason given for an interrupted restart")
	}
	if !resp.Restarting {
		t.Error("restarting = false — the bridge must still restart; the drain is best-effort")
	}
}

// TestRestartWithoutATrackerSaysSo — the honesty rule again. A bridge
// with no session tracker cannot know what it is interrupting, and
// claiming a clean drain would be a lie a control plane would act on.
func TestRestartWithoutATrackerSaysSo(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if srv.deps.InflightSessions != nil {
		t.Fatal("fixture unexpectedly wires a tracker; this case needs it absent")
	}
	var resp restartResponse
	if code := doJSON(t, srv.Handler(), "POST", "/api/restart", nil, &resp); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	if resp.Drained {
		t.Error("claimed a clean drain with no tracker to verify it")
	}
	if resp.Inflight != -1 {
		t.Errorf("inflight = %d, want -1 — unknown is a different fact from zero",
			resp.Inflight)
	}
	if resp.Reason == "" {
		t.Error("no reason given")
	}
}

// TestRestartDrainCanBeDeclined — a caller that genuinely wants the old
// cut-everything behaviour can ask for it.
func TestRestartDrainCanBeDeclined(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.deps.InflightSessions = func() int64 { return 5 }

	start := time.Now()
	var resp restartResponse
	if code := doJSON(t, srv.Handler(), "POST", "/api/restart",
		map[string]any{"drain": false}, &resp); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	if resp.Drained {
		t.Error("drained = true when draining was declined")
	}
	if elapsed := time.Since(start); elapsed > restartDrainPoll {
		t.Errorf("declining the drain still waited %v", elapsed)
	}
}

// TestRestartBodylessPostStillDrains — the console has always POSTed
// without a body, and that path must get the safer behaviour rather than
// failing to decode.
func TestRestartBodylessPostStillDrains(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.deps.InflightSessions = func() int64 { return 0 }

	req := httptest.NewRequest("POST", "/api/restart", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp restartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	if !resp.Drained {
		t.Error("a bodyless POST must default to draining")
	}
}

// TestRestartDrainWaitIsClamped — the admin server sets no WriteTimeout
// (PR #75), so nothing else stops a caller pinning a request open.
func TestRestartDrainWaitIsClamped(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.deps.InflightSessions = func() int64 { return 1 }
	huge := int(maxRestartDrainWait/time.Second) + 100000
	req := restartRequest{MaxWaitSec: &huge}
	// Drive the wait directly with a tiny clamp check rather than
	// actually blocking for five minutes.
	if got := clampDrainWait(req); got != maxRestartDrainWait {
		t.Errorf("wait = %v, want it clamped to %v", got, maxRestartDrainWait)
	}
	zero := 0
	if got := clampDrainWait(restartRequest{MaxWaitSec: &zero}); got != defaultRestartDrainWait {
		t.Errorf("zero maxWaitSec = %v, want the default %v", got, defaultRestartDrainWait)
	}
	if got := clampDrainWait(restartRequest{}); got != defaultRestartDrainWait {
		t.Errorf("absent maxWaitSec = %v, want the default", got)
	}
}
