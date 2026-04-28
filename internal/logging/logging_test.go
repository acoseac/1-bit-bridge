package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// TestComponentBeforeInit pins: a logger created before Init()
// still works (writes to whatever slog.Default() routes to) without
// panicking. This is the "package init logs from a test that
// imports without calling Init" scenario.
func TestComponentBeforeInit(t *testing.T) {
	resetOnce()
	logger := Component("scanner")
	if logger == nil {
		t.Fatal("Component returned nil")
	}
	// Smoke test — must not panic.
	logger.Info("smoke", "x", 1)
}

// TestComponentAttributesIncluded pins: every record carries the
// `component=<name>` attribute when written through the configured
// handler.
func TestComponentAttributesIncluded(t *testing.T) {
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

// TestPostInitRedirect pins the load-bearing contract on PR #77's
// dynamic handler: a logger CREATED before Init runs must pick up
// the handler Init installs once Init runs (Windows-service log-
// redirect scenario). Pre-fix this regressed because Component
// captured the pre-Init handler and stuck with it.
func TestPostInitRedirect(t *testing.T) {
	resetOnce()

	// 1. Create a component logger BEFORE Init — simulates a
	//    package-level `var logger = logging.Component(...)`.
	preInit := Component("scanner")

	// 2. Init against a buffer so we can assert post-Init records
	//    land there.
	var buf bytes.Buffer
	Init(&buf)

	// 3. Log via the pre-Init logger. The dynamic handler should
	//    resolve slog.Default() at log time, picking up the buffer
	//    handler Init installed.
	preInit.Info("after-init")

	out := buf.String()
	if !strings.Contains(out, "after-init") {
		t.Errorf("pre-Init logger didn't pick up post-Init handler; buffer = %q", out)
	}
	if !strings.Contains(out, "component=scanner") {
		t.Errorf("component attr lost across Init; buffer = %q", out)
	}
}

// TestWithAttrsAndGroup pins the dynamic-handler's With*
// indirection: chained .With(...) calls must accumulate the
// attrs into the published record.
func TestWithAttrsAndGroup(t *testing.T) {
	resetOnce()
	var buf bytes.Buffer
	Init(&buf)

	base := Component("scanner")
	child := base.With("phase", "walk").With(slog.Int("workers", 4))
	child.Info("started")

	out := buf.String()
	for _, want := range []string{"phase=walk", "workers=4", "component=scanner"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in: %q", want, out)
		}
	}
}

// resetOnce zeroes the package-level sync.Once so a subsequent
// Init() reconfigures the handler. Test-only — production calls
// Init exactly once at startup. We also reset slog.Default() so
// post-test pollution doesn't bleed into TestComponentBeforeInit's
// expectation that nothing has been configured yet.
func resetOnce() {
	once = sync.Once{}
	slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelInfo})))
}
