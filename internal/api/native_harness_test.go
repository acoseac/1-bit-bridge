package api

import (
	"strings"
	"testing"
)

// TestServeHasNoNativeProbeRoute pins the CLI-only qualification boundary.
func TestServeHasNoNativeProbeRoute(t *testing.T) {
	s := &Server{}
	for _, r := range s.routeRegistry() {
		if strings.Contains(r.pattern, "dsd-test") {
			t.Fatalf("native probe exposed: %s", r.pattern)
		}
	}
}
