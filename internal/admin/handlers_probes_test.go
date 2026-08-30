package admin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProbesAnswerWithoutASession is the reason these exist. Verified
// against the live public bridge before the change:
//
//	GET :7789/healthz -> 302 /login
//	GET :7789/readyz  -> 302 /login
//
// A 302 is a "healthy" response to most health checkers, so an
// orchestrator pointed at either would have reported a wedged process
// as fine. They must bypass the session gate in BOTH modes.
func TestProbesAnswerWithoutASession(t *testing.T) {
	for _, public := range []bool{false, true} {
		srv, cfg, _ := newTestServer(t)
		if public {
			cfg.Deployment.Mode = "public"
			srv.deps.CfgHolder.Store(cfg)
		}
		ts := httptest.NewServer(srv.Handler())

		res, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		ts.Close()

		if res.StatusCode != http.StatusOK {
			t.Errorf("public=%v: GET /healthz = %d, want 200", public, res.StatusCode)
		}
		if len(body) != 0 {
			t.Errorf("public=%v: /healthz returned a body (%q). These sit ahead of the "+
				"session gate, so they are the most exposed surface on the listener — "+
				"a probe needs one bit and gets one bit.", public, body)
		}
	}
}

// TestReadyzReportsScanState: liveness and readiness must not be the
// same answer, or an orchestrator restarts a bridge that is merely
// still doing its first scan — which makes it start that scan again,
// forever.
func TestReadyzReportsScanState(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(p string) (int, string) {
		res, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	// A fresh store has never completed a scan.
	if code, _ := get("/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("cold /readyz = %d, want 503 — no scan has finished, so there is "+
			"no manifest to serve", code)
	}
	// Liveness is unaffected: the process is up either way, and a
	// restart would not help.
	if code, _ := get("/healthz"); code != http.StatusOK {
		t.Errorf("cold /healthz = %d, want 200 — a bridge mid-first-scan is alive", code)
	}

	if _, err := srv.deps.Scanner.Scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if code, body := get("/readyz"); code != http.StatusOK {
		t.Errorf("warm /readyz = %d %q, want 200", code, body)
	}
}

// TestReadyIsNotFalseDuringARescan: a periodic rescan on a live bridge
// must not drop it out of rotation. It serves the previous manifest
// perfectly well throughout.
func TestReadyIsNotFalseDuringARescan(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if _, err := srv.deps.Scanner.Scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !srv.ready() {
		t.Fatal("not ready after a completed scan")
	}
	// The predicate is "has one ever finished", not "is one running".
	// This is the assertion that fails if someone changes it to
	// !IsScanning().
	if got := srv.ready(); !got {
		t.Error("ready() must stay true across rescans")
	}
}

// TestLivenessAndReadinessDisagreeOnAMisconfiguredBridge pins the one
// place these two deliberately give different answers.
//
// Public mode with no credential store: the process is running (so a
// restart fixes nothing and would loop), but the console cannot serve
// anyone (so the instance must leave rotation). That is precisely the
// split the two endpoints exist for, and it is the case that would be
// lost if someone "simplified" readyz into an alias for healthz.
func TestLivenessAndReadinessDisagreeOnAMisconfiguredBridge(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	if _, err := srv.deps.Scanner.Scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	cfg.Deployment.Mode = "public"
	srv.deps.CfgHolder.Store(cfg)
	srv.deps.AdminAuth = nil

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	live, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	live.Body.Close()
	if live.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 — the process is up, and restarting it "+
			"cannot conjure a credential file", live.StatusCode)
	}

	rdy, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	rdy.Body.Close()
	if rdy.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503 — the console cannot serve anyone, so the "+
			"instance must leave rotation", rdy.StatusCode)
	}
}
