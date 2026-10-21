package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func getOutputChecklist(t *testing.T, s *Server) (outputChecklistResponse, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/output/checklist", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = testConsoleHost
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out outputChecklistResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return out, rec
}

// TestOutputChecklistIsInactiveOffAPlayer: a bridge that is no player (or
// a console with no checklist wired) answers inactive with no lines, and
// the panel stays hidden. It never answers an empty list as a clean bill.
func TestOutputChecklistIsInactiveOffAPlayer(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.deps.OutputChecklist = nil
	out, rec := getOutputChecklist(t, s)
	if out.Active || out.Lines == nil || len(out.Lines) != 0 {
		t.Fatalf("unwired: %+v (%s)", out, rec.Body.String())
	}
	s.deps.OutputChecklist = func(context.Context) ([]OutputCheckLine, bool) { return nil, false }
	out, rec = getOutputChecklist(t, s)
	if out.Active || len(out.Lines) != 0 {
		t.Fatalf("not a player: %+v (%s)", out, rec.Body.String())
	}
}

// TestOutputChecklistPassesTheLinesThrough: the console shows the lines it
// is handed, in order, each with its fix, and the answer is never cached:
// it says how the box is now.
func TestOutputChecklistPassesTheLinesThrough(t *testing.T) {
	s, _, _ := newTestServer(t)
	lines := []OutputCheckLine{
		{Key: "player", Title: "Player", Status: "ok", Summary: "Volumio 4.119 · MPD 0.24.0 on port 6600, reachable at 192.168.0.94"},
		{Key: "dac", Title: "DAC", Status: "warn", Summary: "iFi found, but MPD's output does not name it", Fix: "Point MPD's audio_output at the DAC."},
		{Key: "music", Title: "Music", Status: "ok", Summary: "Ready: can play music from another bridge or NAS"},
		{Key: "phone", Title: "Phone", Status: "warn", Summary: "No phone paired yet", Fix: "Scan the pairing QR."},
	}
	asked := 0
	s.deps.OutputChecklist = func(context.Context) ([]OutputCheckLine, bool) { asked++; return lines, true }
	out, rec := getOutputChecklist(t, s)
	if asked != 1 || !out.Active || len(out.Lines) != len(lines) {
		t.Fatalf("asked %d, %+v", asked, out)
	}
	for i := range lines {
		if out.Lines[i] != lines[i] {
			t.Fatalf("line %d is %+v, want %+v", i, out.Lines[i], lines[i])
		}
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control %q", got)
	}
}
