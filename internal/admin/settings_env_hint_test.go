package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheSettingsPageSaysASaveKeepsTheFilesOwnValueForAVariable: a console
// save keeps bridge.yaml's own value for each setting a BRIDGE_* variable
// set at the start, unless the save changes it, and a change made in the
// console is saved and set by the variable again at the next start
// (config.filedEnvValues, backlog B761). The Settings page says both, since
// what the page shows for such a setting is the variable's value, not the
// file's.
func TestTheSettingsPageSaysASaveKeepsTheFilesOwnValueForAVariable(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = testConsoleHost
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings: status %d", w.Code)
	}
	// The page as a reader reads it: its runs of white space, the template's
	// line breaks among them, as one space each.
	page := strings.Join(strings.Fields(w.Body.String()), " ")
	for _, said := range []string{
		"<code>BRIDGE_*</code> environment variable sets a setting",
		"keeps bridge.yaml&rsquo;s own value for such a setting unless you change it",
		"the variable sets the setting again at the next start",
	} {
		if !strings.Contains(page, said) {
			t.Errorf("the Settings page does not say %q", said)
		}
	}
}
