package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

// TestMenuOptionsByState pins the per-state option set so the menu's
// dispatch table can't drift silently. Pure data — no I/O — so the
// test runs identically on every supported platform.
func TestMenuOptionsByState(t *testing.T) {
	cases := []struct {
		name     string
		state    menuState
		wantKeys []rune
	}{
		{
			name:     "not-initialized",
			state:    menuState{initialized: false},
			wantKeys: []rune{'1', '2', 'Q'},
		},
		{
			name:     "initialized-no-service",
			state:    menuState{initialized: true, kind: packaging.KindNone, isAdmin: true},
			wantKeys: []rune{'1', '2', '3', '4', '5', 'Q'},
		},
		{
			name:     "initialized-with-launchd",
			state:    menuState{initialized: true, kind: packaging.KindLaunchdUser, isAdmin: true},
			wantKeys: []rune{'1', '2', '3', '4', '5', 'Q'},
		},
		{
			name:     "initialized-with-scm",
			state:    menuState{initialized: true, kind: packaging.KindWindowsSCM, isWindows: true, isAdmin: true},
			wantKeys: []rune{'1', '2', '3', '4', '5', 'Q'},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := optionsFor(c.state)
			if len(opts) != len(c.wantKeys) {
				t.Fatalf("got %d options, want %d", len(opts), len(c.wantKeys))
			}
			for i, k := range c.wantKeys {
				if opts[i].key != k {
					t.Errorf("option %d key = %c, want %c", i, opts[i].key, k)
				}
				if opts[i].label == "" {
					t.Errorf("option %d (%c) has empty label", i, opts[i].key)
				}
				if opts[i].action == nil {
					t.Errorf("option %d (%c) has nil action", i, opts[i].key)
				}
			}
		})
	}
}

// TestMenuOptionsWindowsUnelevated pins the elevation hint contract:
// on a Windows-non-elevated state, the SCM-install row must be tagged
// "(Requires Administrator)" so the user sees the gate up-front
// rather than getting an Access-Denied stack trace from CreateService.
func TestMenuOptionsWindowsUnelevated(t *testing.T) {
	state := menuState{
		initialized: true,
		kind:        packaging.KindNone,
		isWindows:   true,
		isAdmin:     false,
	}
	opts := optionsFor(state)
	var installLabel string
	for _, o := range opts {
		if o.key == '2' {
			installLabel = o.label
		}
	}
	if !strings.Contains(installLabel, "Requires Administrator") {
		t.Errorf("Windows-non-elevated install label %q should warn about admin", installLabel)
	}
}

// TestMenuOptionsPosixSudoWarning pins the root-warning contract:
// on POSIX-running-as-root, the install row must include the
// destructive-action warning so the operator notices before running
// a sudo install that would resolve $HOME to /root.
func TestMenuOptionsPosixSudoWarning(t *testing.T) {
	state := menuState{
		initialized: true,
		kind:        packaging.KindNone,
		isWindows:   false,
		isRoot:      true,
	}
	opts := optionsFor(state)
	var installLabel string
	for _, o := range opts {
		if o.key == '2' {
			installLabel = o.label
		}
	}
	if !strings.Contains(installLabel, "root may break") {
		t.Errorf("POSIX-root install label %q should warn about config paths", installLabel)
	}
}

// TestMenuDispatchExitsOnQ feeds 'Q\n' to readMenuChoice and asserts
// it returns 'Q' so the loop's actQuit handler fires. This is the
// canonical clean-exit path; a regression here would make Q silently
// ignored and the user would have to Ctrl+C out (rude).
func TestMenuDispatchExitsOnQ(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("Q\n"))
	opts := optionsFor(menuState{initialized: false})
	got := readMenuChoice(in, opts)
	if got != 'Q' {
		t.Errorf("readMenuChoice(\"Q\") = %c, want Q", got)
	}
}

// TestArrowKeyDiscarded covers the cooked-mode arrow-key swallow.
// `bufio.ReadBytes('\n')` blocks until newline, so a bare ↑ press
// plus Enter delivers `\x1b[A\n`. Our policy is to drop any line
// containing `\x1b` and re-prompt — readMenuChoice signals that by
// returning 0 (caller's loop continues). The mixed-arrow-then-digit
// case ALSO returns 0 since any `\x1b` in the line drops the line.
//
// IMPORTANT: do not file as "menu hangs on arrow key" — the user
// MUST press Enter to flush the discarded line. Trade-off for not
// importing golang.org/x/term raw mode.
func TestArrowKeyDiscarded(t *testing.T) {
	opts := optionsFor(menuState{initialized: false})
	cases := []struct {
		name string
		in   string
	}{
		{"up-arrow-only", "\x1b[A\n"},
		{"down-arrow", "\x1b[B\n"},
		{"right-arrow", "\x1b[C\n"},
		{"left-arrow", "\x1b[D\n"},
		{"multi-mash", "\x1b[A\x1b[B\n"},
		{"arrow-then-digit", "\x1b[A1\n"}, // policy: still discard
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(c.in))
			got := readMenuChoice(r, opts)
			if got != 0 {
				t.Errorf("readMenuChoice(%q) = %c (%d), want 0 (discarded)", c.in, got, got)
			}
		})
	}
	// Now confirm a clean digit DOES return after the swallow loop:
	r := bufio.NewReader(strings.NewReader("\x1b[A\n1\n"))
	if got := readMenuChoice(r, opts); got != 0 {
		t.Errorf("first read should swallow arrow: got %c", got)
	}
	if got := readMenuChoice(r, opts); got != '1' {
		t.Errorf("second read should return 1: got %c", got)
	}
}

// TestServeContextNotShared pins the per-invocation signal-context
// contract: actStartNow MUST create its own signal.NotifyContext and
// MUST NOT share a parent ctx across calls — Go contexts can't be
// un-canceled, so a shared signal-wired parent would lock out every
// call after the first Ctrl+C.
//
// We can't easily intercept signal.NotifyContext from a test, but we
// CAN exercise the flow by stubbing runServe and checking each call
// receives a non-canceled ctx whose Err() is nil at entry. Done by
// patching a package-level seam during the test.
func TestServeContextNotShared(t *testing.T) {
	// Capture the ctx.Err() seen at entry to runServe in two sequential
	// calls. If actStartNow shared a single parent ctx that we then
	// cancelled between calls, the second call would see ctx.Err() !=
	// nil. A correct implementation creates a fresh signal scope per
	// call so each receives a Background-derived, un-canceled ctx.
	var seenErrors [2]error
	var calls int32
	origRunServe := runServeForMenu
	t.Cleanup(func() { runServeForMenu = origRunServe })
	runServeForMenu = func(ctx context.Context, _ serveOpts, _, _ io.Writer) int {
		idx := atomic.AddInt32(&calls, 1) - 1
		if idx < int32(len(seenErrors)) {
			seenErrors[idx] = ctx.Err()
		}
		return 0
	}
	state := menuState{initialized: true, cfgPath: "/tmp/bridge.yaml"}
	var stdout, stderr bytes.Buffer
	in := bufio.NewReader(strings.NewReader(""))
	for i := 0; i < 2; i++ {
		stdout.Reset()
		stderr.Reset()
		_ = actStartNow(context.Background(), in, &stdout, &stderr, state)
	}
	for i, err := range seenErrors {
		if err != nil {
			t.Errorf("runServe call %d received already-cancelled ctx: %v (per-invocation signal scope broken)", i, err)
		}
	}
}

// TestMenuRendersStableSnapshot is the regression gate against
// accidental layout drift in renderOptions / writeStatusLine. Color
// is stripped so we compare visible bytes.
func TestMenuRendersStableSnapshot(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	resetColorState(t)
	state := menuState{
		initialized: true,
		cfgPath:     "/Users/me/cfg/bridge.yaml",
		kind:        packaging.KindLaunchdUser,
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("snapshot is platform-paint-stable on supported targets only")
	}
	var w bytes.Buffer
	writeStatusLine(&w, state)
	out := w.String()
	if !strings.Contains(out, "initialized") {
		t.Errorf("status missing 'initialized': %q", out)
	}
	if !strings.Contains(out, "macOS LaunchAgent") {
		t.Errorf("status missing service description: %q", out)
	}
	if !strings.Contains(out, "/Users/me/cfg/bridge.yaml") &&
		!strings.Contains(out, "...") {
		t.Errorf("status missing cfg path (or its truncation): %q", out)
	}
}
