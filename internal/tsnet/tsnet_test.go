package tsnet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// silentLogger is a slog.Logger writing to io.Discard — used by tests
// that don't care about log content. Tests that need to assert on
// log output use captureLogger.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// captureLogger returns a logger that writes JSON to the supplied
// buffer, plus a level-filtering handler so tests can assert at any
// level. JSON output makes substring assertions easy without
// fighting text-handler whitespace.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestNewServerRequiresLogger — Config.Logger is the slog channel
// for both UserLogf (operator-facing AuthURL + status) and Logf
// (verbose backend). NewServer with a nil logger would crash
// downstream when the adapters fire, so refuse at construction.
func TestNewServerRequiresLogger(t *testing.T) {
	_, err := NewServer(Config{StateDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "Logger") {
		t.Fatalf("want error mentioning Logger, got %v", err)
	}
}

// TestNewServerRefusesEmptyStateDir — empty StateDir would put
// upstream tsnet into ephemeral mode (silent re-register-as-new on
// every restart). Refuse at construction so operators see a clear
// error instead of "why does my admin console keep showing new
// devices?" days later.
func TestNewServerRefusesEmptyStateDir(t *testing.T) {
	_, err := NewServer(Config{Logger: silentLogger()})
	if err == nil || !strings.Contains(err.Error(), "StateDir") {
		t.Fatalf("want error mentioning StateDir, got %v", err)
	}
	// And the error message must hint at the failure mode so
	// operators don't have to grep the source to understand why.
	if !strings.Contains(err.Error(), "ephemeral") {
		t.Errorf("error should mention ephemeral mode, got %q", err.Error())
	}
}

// TestCloseBeforeStartIsNoOp — Close on a never-started Server
// shouldn't panic or return an error. Caller-friendly because
// `defer s.Close()` is the canonical shutdown pattern.
func TestCloseBeforeStartIsNoOp(t *testing.T) {
	s, err := NewServer(Config{Logger: silentLogger(), StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close before Start: %v", err)
	}
	// Idempotent — second Close is also a no-op.
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestStatusBeforeStartReturnsError — same shape: Status / ListenTLS
// require a started server. Calling either before Start should
// return a typed error rather than panic.
func TestStatusAndListenTLSBeforeStartReturnError(t *testing.T) {
	s, err := NewServer(Config{Logger: silentLogger(), StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if _, err := s.Status(context.Background()); err == nil {
		t.Errorf("Status before Start should error")
	}
	if _, err := s.ListenTLS(":0"); err == nil {
		t.Errorf("ListenTLS before Start should error")
	}
}

// TestListenPacketBeforeStartReturnsError — ListenPacket is the
// HTTP/3 (QUIC) entry point for tailnet UDP binds; pre-Start it must
// return the documented "called before Start" error rather than
// panic or return a half-constructed PacketConn. cmd/bridge/main.go's
// serve handler relies on this guard: a PR #264 regression placed
// ListenPacket synchronously in main BEFORE the tsnet startup
// goroutine fired Start(), so the call always tripped this guard
// and HTTP/3 over tailnet silently fell back to HTTP/2 on every
// boot. The fix moved the call into the goroutine after Start
// succeeds; this test pins the wrapper invariant the caller now
// depends on.
func TestListenPacketBeforeStartReturnsError(t *testing.T) {
	s, err := NewServer(Config{Logger: silentLogger(), StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	pconn, err := s.ListenPacket("udp", ":0")
	if err == nil {
		_ = pconn.Close()
		t.Fatalf("ListenPacket before Start should error, got pconn=%v", pconn)
	}
	if !strings.Contains(err.Error(), "before Start") {
		t.Errorf("error should mention before-Start guard, got %q", err.Error())
	}
}

// TestExtractTailscaleAuthURL — the AuthURL string heuristic. tsnet
// emits user-facing messages like:
//
//	"To authenticate, visit: https://login.tailscale.com/a/abc123"
//	"https://login.tailscale.com/a/abc123\n"
//
// We must extract the URL from either form. Failure mode: AuthURL()
// returns empty even though tsnet emitted a URL — operator falls
// back to reading the slog output directly.
func TestExtractTailscaleAuthURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain url", "https://login.tailscale.com/a/abc123",
			"https://login.tailscale.com/a/abc123"},
		{"with prefix", "To authenticate, visit: https://login.tailscale.com/a/abc123",
			"https://login.tailscale.com/a/abc123"},
		{"with trailing newline", "To authenticate, visit: https://login.tailscale.com/a/abc123\nfollowing text",
			"https://login.tailscale.com/a/abc123"},
		{"with trailing space", "go to https://login.tailscale.com/a/abc123 now",
			"https://login.tailscale.com/a/abc123"},
		{"no url", "everything is fine, peer up", ""},
		{"different domain", "go to https://example.com/auth", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractTailscaleAuthURL(c.in)
			if got != c.want {
				t.Errorf("extractTailscaleAuthURL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestUserLogfCapturesAuthURL — feed a synthetic AuthURL message
// through the UserLogf adapter and verify it lands in s.authURL,
// where AuthURL() can echo it. This is the load-bearing path for
// `bridge tsnet auth` to print the URL on first-run.
func TestUserLogfCapturesAuthURL(t *testing.T) {
	s, err := NewServer(Config{Logger: silentLogger(), StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	logf := s.userLogf()
	logf("To authenticate, visit: https://login.tailscale.com/a/test-token-xyz")
	if got := s.AuthURL(); got != "https://login.tailscale.com/a/test-token-xyz" {
		t.Errorf("AuthURL() = %q after AuthURL message", got)
	}
}

// TestUserLogfDoesNotCaptureNonURL — non-AuthURL messages flow
// through to the logger but don't pollute s.authURL.
func TestUserLogfDoesNotCaptureNonURL(t *testing.T) {
	s, err := NewServer(Config{Logger: silentLogger(), StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	s.userLogf()("connecting to control")
	s.userLogf()("magicsock: derp-3 (San Francisco)")
	if got := s.AuthURL(); got != "" {
		t.Errorf("AuthURL() = %q after non-URL messages", got)
	}
}

// TestNoisyLogfFromSlogDropsKeepalives — tsnet's Logf channel is a
// firehose of magicsock / netcheck / control-plane heartbeat
// messages. The adapter forwards only error-flavoured lines and
// drops everything else. Without this filter, a single bridge
// would emit thousands of debug lines per minute into operator logs.
func TestNoisyLogfFromSlogDropsKeepalives(t *testing.T) {
	var buf bytes.Buffer
	log := captureLogger(&buf)
	logf := noisyLogfFromSlog(log)

	logf("magicsock: derp-3 (San Francisco) keepalive")
	logf("netcheck: probing 1ms")
	logf("control plane: heartbeat ok")

	if buf.Len() != 0 {
		t.Errorf("noisyLogf should drop benign lines, got %s", buf.String())
	}
}

// TestNoisyLogfFromSlogPropagatesErrors — error-flavoured messages
// MUST surface so a real failure isn't lost in the noise filter.
// Both `error` and `failed` substring matches; case-insensitive.
func TestNoisyLogfFromSlogPropagatesErrors(t *testing.T) {
	cases := []string{
		"error: dial tcp 100.64.0.1: connection refused",
		"FAILED to bring up node",
		"control plane: error from server",
		"PANIC: out of memory",
	}
	for _, c := range cases {
		var buf bytes.Buffer
		log := captureLogger(&buf)
		noisyLogfFromSlog(log)(c)
		if buf.Len() == 0 {
			t.Errorf("noisyLogf dropped error-flavoured line: %q", c)
		}
	}
}

// TestHasPersistedTsnetStateDetectsTailscaledState — the file
// `<stateDir>/tailscaled.state` is the canonical signal that tsnet
// has persisted machine identity. The startup misconfig sentinel
// uses this to distinguish "first run, will need interactive auth"
// from "state was wiped, will silently re-register" (different
// operator-facing log message for each).
func TestHasPersistedTsnetStateDetectsTailscaledState(t *testing.T) {
	dir := t.TempDir()
	if hasPersistedTsnetState(dir) {
		t.Errorf("empty state dir reports persisted state")
	}
	stateFile := filepath.Join(dir, "tailscaled.state")
	if err := os.WriteFile(stateFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write state file: %v", err)
	}
	if !hasPersistedTsnetState(dir) {
		t.Errorf("state file present, hasPersistedTsnetState = false")
	}
}

// TestAssertSecureDirMissingPath — non-existent state dir surfaces
// the underlying os.Stat error. Caller is expected to MkdirAll
// before calling assertSecureDir; this tests the error shape when
// the call order is wrong.
func TestAssertSecureDirMissingPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	if err := assertSecureDir(dir); err == nil {
		t.Errorf("assertSecureDir on missing path should error")
	}
}

// TestAssertSecureDirRejectsFile — passing a regular file (not a
// directory) is a config typo; surface a clear error instead of
// letting tsnet trip on it later.
func TestAssertSecureDirRejectsFile(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := assertSecureDir(f); err == nil {
		t.Errorf("assertSecureDir on regular file should error")
	}
}

// TestAssertSecureDirRejectsWorldReadableOnUnix — POSIX-only check.
// Windows path is the no-op-ish sanity branch in state_windows.go,
// so this test is build-gated to !windows. The 0700 contract
// matters because the state dir holds the bridge's tailnet machine
// identity — see state_unix.go for the threat-model rationale.
func TestAssertSecureDirRejectsWorldReadableOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only check; Windows uses ACLs (state_windows.go)")
	}
	dir := t.TempDir()
	// t.TempDir defaults to 0700; force loosening to verify the
	// check trips.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	err := assertSecureDir(dir)
	if err == nil {
		t.Errorf("assertSecureDir on 0755 should error")
	}
	if err != nil && !strings.Contains(err.Error(), "0700") {
		t.Errorf("error should mention 0700, got %v", err)
	}
}

// TestAssertSecureDirRejectsTooStrictPermsOnUnix — 0500 has no
// group/world bits but ALSO drops owner write, which tsnet needs
// to write state. The earlier `& 0o077 != 0` check would have
// silently accepted 0500 (Qodo bug #2 on PR #138). Now we require
// exactly 0700.
func TestAssertSecureDirRejectsTooStrictPermsOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only check")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	defer os.Chmod(dir, 0o700) // restore so t.TempDir cleanup works
	if err := assertSecureDir(dir); err == nil {
		t.Errorf("assertSecureDir on 0500 should error (no owner write)")
	}
}

// TestAssertSecureDirAcceptsWellFormedDir — the happy path: 0700
// directory owned by the test process. t.TempDir produces 0755 on
// macOS (and varies by FS elsewhere), so we explicitly chmod 0700
// before the check.
func TestAssertSecureDirAcceptsWellFormedDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("Chmod 0700: %v", err)
	}
	if err := assertSecureDir(dir); err != nil {
		t.Errorf("assertSecureDir on 0700 dir: %v", err)
	}
}

// TestUnstartedServerInitialState — pre-Start, AuthURL is empty
// and CertDomains is nil. The previous version of this test was
// named TestStartIsIdempotent but never actually called Start
// (CodeRabbit nitpick on PR #138) — the real idempotency contract
// requires a live control plane and is integration-tested at the
// cmd/bridge level. Renamed to reflect what it actually exercises.
func TestUnstartedServerInitialState(t *testing.T) {
	s, err := NewServer(Config{Logger: silentLogger(), StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if got := s.AuthURL(); got != "" {
		t.Errorf("AuthURL() = %q on unstarted server, want empty", got)
	}
	if got := s.CertDomains(); len(got) != 0 {
		t.Errorf("CertDomains() = %v on unstarted server, want empty", got)
	}
}

// TestUserLogfRaceFreeWithAuthURL — verifies the userLogf write +
// AuthURL read pair are race-free under -race. Without the authMu
// guard added in the round-1 fixes, this test would trip the
// race detector on every run.
//
// Designed for `go test -race`; the test always passes under the
// non-race build but the race detector is the actual assertion.
func TestUserLogfRaceFreeWithAuthURL(t *testing.T) {
	s, err := NewServer(Config{Logger: silentLogger(), StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	logf := s.userLogf()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			logf("To authenticate, visit: https://login.tailscale.com/a/token-%d", i)
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		_ = s.AuthURL()
	}
	<-done
	// Final value should be a real captured URL.
	if got := s.AuthURL(); got == "" || !strings.HasPrefix(got, "https://login.tailscale.com/") {
		t.Errorf("AuthURL after concurrent writes = %q", got)
	}
}

// silenceUnusedImport keeps `errors` from being elided when test
// imports change. errors.Is is the kind of check we'll want as
// soon as Status / ListenTLS gain typed errors — currently the
// stub-shape return `errors.New(...)` strings are matched by
// substring. Don't remove until typed errors land.
var _ = errors.Is
