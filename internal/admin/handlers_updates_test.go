package admin

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// TestClassifyUpdateError pins the sentinel → (HTTP status, short
// code) mapping the update install/rollback handlers rely on,
// including that a wrapped sentinel still classifies via errors.Is
// (the cmd/bridge adapter wraps with "%w: detail"). 409 covers both
// retry-later conflicts: active downloads (force=1 overrides) and a
// concurrent install already in flight (retry once it finishes).
func TestClassifyUpdateError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantShort  string
	}{
		{"no-update", ErrUpdateNoUpdate, http.StatusBadRequest, "no-update"},
		{"active-sessions", ErrUpdateActiveSessions, http.StatusConflict, "active-sessions"},
		{"install-in-flight", ErrUpdateInstallInFlight, http.StatusConflict, "install-in-flight"},
		{"platform-unsupported", ErrUpdateNotSupported, http.StatusNotImplemented, "platform-unsupported"},
		{"path-not-writable", ErrUpdatePathNotWritable, http.StatusForbidden, "path-not-writable"},
		{"wrapped install-in-flight",
			fmt.Errorf("%w: an install is already in progress", ErrUpdateInstallInFlight),
			http.StatusConflict, "install-in-flight"},
		{"unknown", errors.New("boom"), http.StatusBadGateway, "install-failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, short := classifyUpdateError(tc.err)
			if status != tc.wantStatus || short != tc.wantShort {
				t.Errorf("classifyUpdateError(%v) = (%d, %q), want (%d, %q)",
					tc.err, status, short, tc.wantStatus, tc.wantShort)
			}
		})
	}
}
