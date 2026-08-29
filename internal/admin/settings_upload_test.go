package admin

import (
	"net/http"
	"testing"
)

// TestUploadEnabledReportsLive — the routes are wired unconditionally and the
// handlers read the flag per request, so a flip needs no restart. A `restart`
// verdict here would send the operator to bounce a bridge for nothing.
func TestUploadEnabledReportsLive(t *testing.T) {
	srv, _, _ := newTestServer(t)
	var out map[string]any
	code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
		map[string]any{"uploadEnabled": true}, &out)
	if code != http.StatusOK {
		t.Fatalf("patch = %d (%v)", code, out)
	}
	fields, _ := out["fields"].(map[string]any)
	row, _ := fields["uploadEnabled"].(map[string]any)
	if row == nil {
		t.Fatalf("no field report for uploadEnabled: %v", out)
	}
	if status, _ := row["status"].(string); status != "live" {
		t.Errorf("uploadEnabled status = %q, want live (%v)", status, row)
	}
	if req, _ := out["restartRequired"].(bool); req {
		t.Error("restartRequired=true for a field that hot-applies")
	}
	// And the flip actually took effect for the handlers, not just on disk.
	var sess map[string]any
	if code := doJSON(t, srv.Handler(), "POST", "/api/upload/sessions",
		map[string]any{"files": []any{map[string]any{"path": "x.flac", "size": 1}}}, &sess); code != http.StatusCreated {
		t.Errorf("upload still refused after the toggle reported live: %d (%v)", code, sess)
	}
}
