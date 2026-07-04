package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

// TestLifecycleNoService asserts each lifecycle command emits the
// "no service unit installed" hint and returns exit code 2 when run
// against a binary that hasn't been `bridge init`-ed. Spec contract
// from the plan: "Refuse with a clear message if no service unit is
// installed" so an operator running `bridge start` against an
// uninstalled binary is told to run `bridge init` first rather than
// seeing a silent no-op.
func TestLifecycleNoService(t *testing.T) {
	if kind, _ := packaging.InstalledKind(); kind != packaging.KindNone {
		t.Skip("test environment has a service unit installed; skipping no-service contract test")
	}
	cases := []struct {
		name string
		fn   func([]string, *bytes.Buffer, *bytes.Buffer) int
	}{
		{"start", func(a []string, o, e *bytes.Buffer) int { return startCmd(a, o, e) }},
		{"stop", func(a []string, o, e *bytes.Buffer) int { return stopCmd(a, o, e) }},
		{"restart", func(a []string, o, e *bytes.Buffer) int { return restartCmd(a, o, e) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := tc.fn(nil, &stdout, &stderr)
			if code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), "no service unit installed") {
				t.Errorf("missing hint, stderr = %q", stderr.String())
			}
		})
	}
}

// ensureInstalled must surface a real InstalledKind() probe failure
// (e.g. a systemd D-Bus error) as a distinct message + false, NOT the
// misleading "no service unit installed" hint.
func TestEnsureInstalledSurfacesProbeError(t *testing.T) {
	orig := installedKindFunc
	t.Cleanup(func() { installedKindFunc = orig })
	installedKindFunc = func() (packaging.ServiceKind, error) {
		return packaging.KindNone, errors.New("systemd D-Bus unavailable")
	}

	var stderr bytes.Buffer
	if ensureInstalled(&stderr) {
		t.Errorf("ensureInstalled returned true on a probe error")
	}
	if !strings.Contains(stderr.String(), "could not determine service install state") {
		t.Errorf("missing probe-error message, stderr = %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "no service unit installed") {
		t.Errorf("emitted the misleading no-unit hint on a probe error, stderr = %q", stderr.String())
	}
}
