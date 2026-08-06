package admin

import (
	"errors"
	"strings"
	"testing"
)

// TestPruneWarningTextSurfacesBothErrors pins that a completed snapshot
// whose prune reported TWO independent problems tells the operator about
// both.
//
// The prune's own error and the orphan sweep's are deliberately separate
// values — `backup.PruneResult.ReapErr` exists because an unreadable
// snapshot manifest is permanent and is NOT a failure of the keep-policy
// prune. An earlier switch here returned on the prune error first, so
// whenever both were set the orphan-sweep condition vanished silently:
// exactly the separation the split was introduced to provide, undone at the
// response boundary.
func TestPruneWarningTextSurfacesBothErrors(t *testing.T) {
	pruneErr := errors.New("remove /b/2020: permission denied")
	reapErr := errors.New("read manifest /b/2019: unexpected end of JSON input")

	for _, tc := range []struct {
		name         string
		prune, reap  error
		wantContains []string
		wantEmpty    bool
	}{
		{name: "neither", wantEmpty: true},
		{name: "prune only", prune: pruneErr, wantContains: []string{"permission denied"}},
		{name: "reap only", reap: reapErr, wantContains: []string{"orphan sweep", "end of JSON input"}},
		{
			name: "both", prune: pruneErr, reap: reapErr,
			wantContains: []string{"permission denied", "orphan sweep", "end of JSON input"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pruneWarningText(tc.prune, tc.reap)
			if tc.wantEmpty {
				if got != "" {
					t.Errorf("no errors should produce no warning, got %q", got)
				}
				return
			}
			if got == "" {
				t.Fatal("a reported problem produced no warning")
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("warning %q is missing %q", got, want)
				}
			}
		})
	}
}
