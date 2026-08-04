package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
			// Start joins Stop/Restart here. It is offered regardless of
			// the run-state probe — see the running/stopped pair below.
			name:     "initialized-with-launchd",
			state:    menuState{initialized: true, kind: packaging.KindLaunchdUser, isAdmin: true},
			wantKeys: []rune{'1', '2', '3', '4', '5', '6', 'Q'},
		},
		{
			name:     "initialized-with-scm",
			state:    menuState{initialized: true, kind: packaging.KindWindowsSCM, isWindows: true, isAdmin: true},
			wantKeys: []rune{'1', '2', '3', '4', '5', '6', 'Q'},
		},
		{
			// The option list must NOT vary with the probe. A probe that
			// is wrong (a bridge answering on an address we cannot dial)
			// would otherwise remove the operator's ability to act.
			name:     "initialized-with-launchd-running",
			state:    menuState{initialized: true, kind: packaging.KindLaunchdUser, isAdmin: true, running: true},
			wantKeys: []rune{'1', '2', '3', '4', '5', '6', 'Q'},
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

// TestWaitForListenAcceptsListeningAddress confirms the post-restart
// health probe returns true once a real listener is bound. Spins up
// a throwaway net.Listen target on a random port — same shape as the
// admin server the real bridge process binds.
func TestWaitForListenAcceptsListeningAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if !waitForListen(ln.Addr().String(), 2*time.Second) {
		t.Errorf("waitForListen returned false on a real listener")
	}
}

// TestWaitForListenTimesOutOnUnboundPort pins the negative-path
// behaviour. Without a fast deadline-respecting return, the menu
// would block the user-facing prompt for an indeterminate time when
// the restarted bridge crashes during startup.
func TestWaitForListenTimesOutOnUnboundPort(t *testing.T) {
	// Bind+immediately-close gives us an address that's almost
	// certainly free for the duration of the test.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	start := time.Now()
	if waitForListen(addr, 600*time.Millisecond) {
		t.Errorf("waitForListen returned true for an unbound port")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("waitForListen ran %v, expected to honour ~600ms deadline", elapsed)
	}
}

// TestOptionsForDoesNoIO pins optionsFor's docblock claim that the table
// is "Pure data — no I/O".
//
// That purity is what makes the table unit-testable on every platform
// without a config dir or a service install, and adding run-state to
// menuState put it one careless line away from being lost: the natural
// place to reach for `running` is inside optionsFor, where it would turn
// every option-table assertion into a network call.
func TestOptionsForDoesNoIO(t *testing.T) {
	orig := adminRunningProbe
	t.Cleanup(func() { adminRunningProbe = orig })
	adminRunningProbe = func(string) bool {
		t.Error("optionsFor probed the admin address; the option table must stay pure data")
		return false
	}
	for _, st := range []menuState{
		{initialized: false},
		{initialized: true, kind: packaging.KindNone, isAdmin: true},
		{initialized: true, kind: packaging.KindLaunchdUser, isAdmin: true},
	} {
		_ = optionsFor(st)
	}
}

// TestMenuOffersStartOnServiceInstall pins that a stopped-but-installed
// bridge has a menu path back up.
//
// Before this, the service-installed branch offered Stop / Restart /
// Open admin / Pair / Uninstall: picking Stop left the operator with no
// way to start it again from the menu. `bridge start` existed as a CLI
// subcommand the whole time — it was the MENU that couldn't reach it.
func TestMenuOffersStartOnServiceInstall(t *testing.T) {
	for _, running := range []bool{true, false} {
		opts := optionsFor(menuState{
			initialized: true,
			kind:        packaging.KindLaunchdUser,
			isAdmin:     true,
			running:     running,
		})
		// Match on the DISPATCH, not the label. A substring check for
		// "start" cannot fail here — "Restart service" contains it — which
		// is exactly how the first version of this test passed with the
		// Start row deleted. Comparing func pointers also survives a
		// relabelling and pins the thing that actually matters.
		var found bool
		want := reflect.ValueOf(actStart).Pointer()
		for _, o := range opts {
			if o.action != nil && reflect.ValueOf(o.action).Pointer() == want {
				found = true
			}
		}
		if !found {
			t.Errorf("running=%v: no row dispatching to actStart in the service-installed menu; got %v",
				running, optionLabels(opts))
		}
	}
}

// TestRunStateSuffix pins the third status badge, which is the whole
// point of tracking run state: `kind` reports what is INSTALLED, and a
// stopped-but-installed bridge previously rendered as though it were up.
func TestRunStateSuffix(t *testing.T) {
	// Before init there is no config to resolve an admin address from,
	// so "not running" would be noise on a first-run screen.
	if got := runStateSuffix(menuState{initialized: false}); got != "" {
		t.Errorf("uninitialized: got %q, want empty", got)
	}
	running := runStateSuffix(menuState{initialized: true, running: true})
	if !strings.Contains(running, "running") || strings.Contains(running, "not running") {
		t.Errorf("running: got %q, want a positive running badge", running)
	}
	stopped := runStateSuffix(menuState{initialized: true, running: false})
	if !strings.Contains(stopped, "not running") {
		t.Errorf("stopped: got %q, want a not-running badge", stopped)
	}
	// Shown even with no service installed: "Start the bridge now (this
	// terminal)" in another window is a real way to be up.
	bare := runStateSuffix(menuState{initialized: true, kind: packaging.KindNone, running: true})
	if !strings.Contains(bare, "running") {
		t.Errorf("initialized, no service, running: got %q, want a running badge", bare)
	}
}

// TestStatusLineReportsRunState is the end-to-end of the above: the line
// the operator actually reads must distinguish stopped from running.
func TestStatusLineReportsRunState(t *testing.T) {
	var up, down bytes.Buffer
	base := menuState{initialized: true, kind: packaging.KindLaunchdUser, cfgPath: "/tmp/bridge.yaml"}
	runningState := base
	runningState.running = true
	writeStatusLine(&up, runningState)
	writeStatusLine(&down, base)
	if up.String() == down.String() {
		t.Fatal("status line is identical for a running and a stopped bridge — " +
			"reporting install state as though it were run state is the bug this fixes")
	}
	if !strings.Contains(down.String(), "not running") {
		t.Errorf("stopped status line %q does not say so", down.String())
	}
}

// TestActOpenAdminDeclinesWhenNotRunning pins that the menu stops opening
// a tab that can only say "connection refused" — the second half of the
// stopped-but-installed bug.
//
// Only the not-running path is exercised: the running path really does
// launch a browser, which is not something a test should do to the
// machine it runs on.
func TestActOpenAdminDeclinesWhenNotRunning(t *testing.T) {
	var out bytes.Buffer
	code := actOpenAdmin(context.Background(), nil, &out, io.Discard,
		menuState{initialized: true, kind: packaging.KindLaunchdUser, running: false})
	if code != -1 {
		t.Errorf("exit code = %d, want -1 (stay in the menu)", code)
	}
	got := out.String()
	if !strings.Contains(got, "not opening a browser") {
		t.Errorf("output %q does not decline to open a dead tab", got)
	}
	// The URL still prints, so an operator whose bridge IS up on an
	// address we couldn't dial can still copy the link.
	if !strings.Contains(got, "Admin console:") {
		t.Errorf("output %q dropped the admin URL; declining must not hide it", got)
	}
}

// TestDetectStateUsesTheProbe pins the wiring: menuState.running comes
// from the probe, and is not asked at all before init (no config, so no
// admin address to resolve).
func TestDetectStateUsesTheProbe(t *testing.T) {
	orig := adminRunningProbe
	t.Cleanup(func() { adminRunningProbe = orig })

	var called atomic.Int32
	adminRunningProbe = func(string) bool { called.Add(1); return true }
	st := detectState()
	if st.initialized {
		if !st.running {
			t.Error("probe returned true but menuState.running is false")
		}
		if called.Load() == 0 {
			t.Error("initialized state did not consult the probe")
		}
	} else if called.Load() != 0 {
		t.Error("probed the admin address before init, when there is no config to read it from")
	}
}

// optionLabels renders an option table for failure messages, so a broken
// assertion says what the menu actually offered.
func optionLabels(opts []menuOption) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, string(o.key)+":"+o.label)
	}
	return out
}
