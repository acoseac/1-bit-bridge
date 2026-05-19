package api

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestDiagnostics_RequiresAuth confirms the endpoint goes through the
// shared authed() middleware — the unauthed probe surfaces as 401 like
// every other authed route.
func TestDiagnostics_RequiresAuth(t *testing.T) {
	hs, _ := newTestServer(t)
	resp, err := http.Get(hs.URL + "/v1/diagnostics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthed: want 401, got %d", resp.StatusCode)
	}
}

// TestDiagnostics_AuthedReturnsExpectedShape walks the happy path
// end-to-end. Validates the wire-stable field names + types iOS
// decodes into `BridgeDiagnosticsResponse`.
func TestDiagnostics_AuthedReturnsExpectedShape(t *testing.T) {
	hs, raw := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/diagnostics", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: want 200, got %d body=%s", resp.StatusCode, body)
	}
	var got DiagnosticsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Tailscale state is always one of the four documented strings.
	allowed := map[string]bool{"down": true, "starting": true, "running": true, "disabled": true}
	if !allowed[got.TailscaleNodeState] {
		t.Errorf("TailscaleNodeState: unexpected %q", got.TailscaleNodeState)
	}
	// LogEventCounts must carry exactly the four whitelisted keys —
	// no fewer (would mean a missing branch), no more (would mean
	// the whitelist gate leaked).
	want := map[string]struct{}{"DEBUG": {}, "INFO": {}, "WARN": {}, "ERROR": {}}
	if len(got.LogEventCounts) != len(want) {
		t.Errorf("LogEventCounts: want exactly %d keys, got %d (%v)", len(want), len(got.LogEventCounts), got.LogEventCounts)
	}
	for k := range got.LogEventCounts {
		if _, ok := want[k]; !ok {
			t.Errorf("LogEventCounts: unexpected key %q", k)
		}
	}
	if got.ServerUptimeSeconds < 0 {
		t.Errorf("ServerUptimeSeconds went negative: %d", got.ServerUptimeSeconds)
	}
}

// TestDiagnostics_DoesNotBlockOnSlowOperations is the load-bearing
// performance contract: the handler must return within hundreds of
// milliseconds even on an unloaded bridge, because it shares the
// public listener with playback / list / download. A regression that
// reintroduces a synchronous SQLite query / sox subprocess / disk
// scan would push this past 100 ms and fail the test.
func TestDiagnostics_DoesNotBlockOnSlowOperations(t *testing.T) {
	hs, raw := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/diagnostics", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// 100 ms is comfortable headroom — the handler should return in
	// well under 1 ms on the steady state.
	if elapsed > 100*time.Millisecond {
		t.Fatalf("handler took %v — must stay under 100 ms; possible slow-operation regression", elapsed)
	}
}

// TestDiagnostics_TailscaleStateStringMapping covers every state
// value the metrics package can surface. The stringification is the
// wire-stable surface iOS switches on; an integer regression in
// metrics would otherwise propagate silently through the diagnostics
// endpoint.
func TestDiagnostics_TailscaleStateStringMapping(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "down"},
		{1, "starting"},
		{2, "running"},
		{3, "disabled"},
		{99, "down"}, // unknown int collapses to "down" — honest fallback
	}
	for _, c := range cases {
		got := tailscaleStateString(c.in)
		if got != c.want {
			t.Errorf("tailscaleStateString(%d): want %q got %q", c.in, c.want, got)
		}
	}
}
