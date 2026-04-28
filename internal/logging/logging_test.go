package logging

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// TestComponentBeforeInit pins: a logger created before Init() still
// writes to stderr (test fallback path) without panicking. This is
// the "package init logs from a test that imports without calling
// Init" scenario.
func TestComponentBeforeInit(t *testing.T) {
	// Reset package state for the test. (Tests in this package run
	// serially per Go conventions.)
	root = nil

	logger := Component("scanner")
	if logger == nil {
		t.Fatal("Component returned nil")
	}
	// Smoke test — must not panic.
	logger.Info("smoke", "x", 1)
}

// TestComponentAttributesIncluded pins: every record carries the
// `component=<name>` attribute when written through the configured
// root.
func TestComponentAttributesIncluded(t *testing.T) {
	// Reset and Init against a buffer so we can assert the output.
	root = nil
	resetOnce()

	var buf bytes.Buffer
	Init(&buf)
	logger := Component("scanner")
	logger.Info("hello", "rows", 42)

	out := buf.String()
	if !strings.Contains(out, "component=scanner") {
		t.Errorf("missing component attr in: %q", out)
	}
	if !strings.Contains(out, "rows=42") {
		t.Errorf("missing rows attr in: %q", out)
	}
	if !strings.Contains(out, `msg=hello`) {
		t.Errorf("missing message in: %q", out)
	}
}

// resetOnce zeroes the package-level sync.Once so a subsequent
// Init() reconfigures the handler. Test-only — production calls
// Init exactly once at startup.
func resetOnce() {
	once = sync.Once{}
}
