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
		// The new version refuses the host's settings: a conflict with them,
		// its own code, never the generic install-failed.
		{"config-refused", ErrUpdateConfigRefused, http.StatusConflict, "config-refused"},
		{"config-check-failed", ErrUpdateConfigCheckFailed, http.StatusBadGateway, "config-check-failed"},
		// The install marker's lock another process holds, or that the data
		// dir's filesystem cannot give, and a rollback over a store a newer
		// version opened (backlog B692): the host's state refuses them, and
		// nothing was done.
		{"marker-locked", ErrUpdateMarkerLocked, http.StatusConflict, "marker-locked"},
		{"rollback-refused", ErrUpdateRollbackRefused, http.StatusConflict, "rollback-refused"},
		// Update checks off (backlog B761): the switch refuses the check or
		// the install, and nothing went wrong.
		{"update-checks-off", ErrUpdateChecksOff, http.StatusConflict, "update-checks-off"},
		{"wrapped update-checks-off",
			fmt.Errorf("%w: no release is installed while they are off", ErrUpdateChecksOff),
			http.StatusConflict, "update-checks-off"},
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
