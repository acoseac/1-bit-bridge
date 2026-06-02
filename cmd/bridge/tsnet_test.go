package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestTailscaleAdminSourceStampsModeAndPublic verifies the snapshot
// carries the configured mode + public posture, and that the disabled-
// branch message is mode-aware: when cfg already reads cli/tsnet but the
// auto-pilot isn't running (both backends nil — e.g. a Settings mode
// change awaiting restart), the message must NOT tell the operator to
// "set tailscale.mode to cli" (which is already set). This is the
// operator-screenshot bug.
func TestTailscaleAdminSourceStampsModeAndPublic(t *testing.T) {
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}}
	cfg.Tailscale.Mode = "cli"
	holder := config.NewRuntimeConfig(cfg)
	src := newTailscaleAdminSource(nil, nil, "/etc/onebit/bridge.yaml", holder)

	st := src.Status()
	if st.Mode != "cli" {
		t.Errorf("Mode = %q, want cli", st.Mode)
	}
	if st.PublicMode {
		t.Errorf("PublicMode = true for a loopback config")
	}
	// Mode-aware message: must reference the configured mode and NOT
	// instruct setting a mode that's already set.
	if !strings.Contains(st.LastError, "cli") {
		t.Errorf("message should mention the configured cli mode, got %q", st.LastError)
	}
	if strings.Contains(st.LastError, "set tailscale.mode to") {
		t.Errorf("message must not tell the operator to set a mode that's already cli, got %q", st.LastError)
	}

	// Genuine disabled mode keeps the original recovery hint.
	cfg2 := &config.Config{LibraryRoots: []string{t.TempDir()}}
	cfg2.Tailscale.Mode = "disabled"
	src2 := newTailscaleAdminSource(nil, nil, "/etc/onebit/bridge.yaml", config.NewRuntimeConfig(cfg2))
	st2 := src2.Status()
	if st2.Mode != "disabled" {
		t.Errorf("Mode = %q, want disabled", st2.Mode)
	}
	if !strings.Contains(st2.LastError, "set tailscale.mode to") {
		t.Errorf("disabled mode should keep the recovery hint, got %q", st2.LastError)
	}
}

// TestTsnetCmdDispatchUnknown — `bridge tsnet zzz` must return a
// usage-error exit code instead of crashing or silently exiting 0.
// Same shape as the parent dispatcher's unknown-subcommand handler.
func TestTsnetCmdDispatchUnknown(t *testing.T) {
	var out, errBuf bytes.Buffer
	rc := tsnetCmd(context.Background(), []string{"unknown-verb"}, strings.NewReader(""), &out, &errBuf)
	if rc != 2 {
		t.Errorf("tsnetCmd unknown verb: rc = %d, want 2", rc)
	}
	if !strings.Contains(errBuf.String(), "unknown tsnet subcommand") {
		t.Errorf("expected error mentioning unknown subcommand, got: %q", errBuf.String())
	}
}

// TestTsnetCmdDispatchEmpty — `bridge tsnet` (no subcommand) prints
// usage and exits 2. Mirrors the parent dispatcher's no-args
// behavior.
func TestTsnetCmdDispatchEmpty(t *testing.T) {
	var out, errBuf bytes.Buffer
	rc := tsnetCmd(context.Background(), nil, strings.NewReader(""), &out, &errBuf)
	if rc != 2 {
		t.Errorf("tsnetCmd empty args: rc = %d, want 2", rc)
	}
	if !strings.Contains(errBuf.String(), "usage:") {
		t.Errorf("expected usage hint, got: %q", errBuf.String())
	}
}

// TestTsnetCmdHelp — `bridge tsnet help` (and -h, --help) print
// usage on stdout and exit 0. Operators discovering the surface
// through `--help` shouldn't see an error code.
func TestTsnetCmdHelp(t *testing.T) {
	for _, flag := range []string{"help", "-h", "--help"} {
		var out, errBuf bytes.Buffer
		rc := tsnetCmd(context.Background(), []string{flag}, strings.NewReader(""), &out, &errBuf)
		if rc != 0 {
			t.Errorf("tsnetCmd %s: rc = %d, want 0", flag, rc)
		}
		if !strings.Contains(out.String(), "auth") || !strings.Contains(out.String(), "status") || !strings.Contains(out.String(), "logout") {
			t.Errorf("tsnetCmd %s: usage missing subcommands, got: %q", flag, out.String())
		}
	}
}

// TestTsnetSubcommandRefusesNonTsnetMode — auth/status/logout all
// load bridge.yaml AND check `tailscale.mode == tsnet`. Refusing
// when mode is cli/disabled/empty prevents the subcommands from
// silently mutating tsnet state on a host running the CLI flow.
func TestTsnetSubcommandRefusesNonTsnetMode(t *testing.T) {
	cfgPath := writeMinimalConfig(t, config.TailscaleConfig{Mode: "cli"})
	for _, sub := range []string{"auth", "status", "logout"} {
		t.Run(sub, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			rc := tsnetCmd(context.Background(),
				[]string{sub, "--config", cfgPath},
				strings.NewReader("n\n"),
				&out, &errBuf)
			if rc != 1 {
				t.Errorf("rc = %d, want 1 (mode mismatch)", rc)
			}
			combined := out.String() + errBuf.String()
			if !strings.Contains(combined, "tsnet") {
				t.Errorf("error should mention tsnet, got out=%q err=%q", out.String(), errBuf.String())
			}
		})
	}
}

// TestTsnetLogoutNoState — `bridge tsnet logout` on a host with no
// existing state dir is a no-op and exits 0 (already-logged-out is
// the same observable shape as just-logged-out).
func TestTsnetLogoutNoState(t *testing.T) {
	cfgPath := writeMinimalConfig(t, config.TailscaleConfig{Mode: "tsnet"})
	var out, errBuf bytes.Buffer
	rc := tsnetCmd(context.Background(),
		[]string{"logout", "--config", cfgPath},
		strings.NewReader(""),
		&out, &errBuf)
	if rc != 0 {
		t.Errorf("rc = %d, want 0 (no-op when no state); err=%q", rc, errBuf.String())
	}
	if !strings.Contains(out.String(), "no state") {
		t.Errorf("expected 'no state' message, got %q", out.String())
	}
}

// TestTsnetLogoutDeclineCancels — typing anything other than the
// constant confirm phrase MUST leave the state dir intact AND exit
// 0 (cancel is a graceful no-op, not a failure). Round-1 of PR #139
// raised the confirm bar from "y" to a typed phrase to make
// destructive misuse vanishingly unlikely.
func TestTsnetLogoutDeclineCancels(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := writeMinimalConfigInDir(t, tmp, config.TailscaleConfig{Mode: "tsnet"})
	stateDir := filepath.Join(tmp, "tailscale")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	stateFile := filepath.Join(stateDir, "tailscaled.state")
	if err := os.WriteFile(stateFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for _, decline := range []string{"y\n", "yes\n", "n\n", "\n", "WIPE-TYPO\n"} {
		t.Run(strings.TrimSpace(decline), func(t *testing.T) {
			var out, errBuf bytes.Buffer
			rc := tsnetCmd(context.Background(),
				[]string{"logout", "--config", cfgPath, "--force"},
				strings.NewReader(decline),
				&out, &errBuf)
			if rc != 0 {
				t.Errorf("rc = %d, want 0 (decline)", rc)
			}
			if !strings.Contains(out.String(), "cancelled") {
				t.Errorf("expected 'cancelled' in output, got %q", out.String())
			}
			if _, err := os.Stat(stateFile); err != nil {
				t.Errorf("state file removed despite decline: %v", err)
			}
		})
	}
}

// TestTsnetLogoutConfirmWipes — typing the constant confirm phrase
// (`WIPE`) wipes the state dir. Exit 0, state file is gone.
func TestTsnetLogoutConfirmWipes(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := writeMinimalConfigInDir(t, tmp, config.TailscaleConfig{Mode: "tsnet"})
	stateDir := filepath.Join(tmp, "tailscale")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	stateFile := filepath.Join(stateDir, "tailscaled.state")
	if err := os.WriteFile(stateFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var out, errBuf bytes.Buffer
	rc := tsnetCmd(context.Background(),
		[]string{"logout", "--config", cfgPath, "--force"},
		strings.NewReader("WIPE\n"),
		&out, &errBuf)
	if rc != 0 {
		t.Errorf("rc = %d, want 0 (confirm wipe); err=%q", rc, errBuf.String())
	}
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Errorf("state file should have been wiped, stat err = %v", err)
	}
}

// TestTsnetLogoutRefusesWhileRunning — when the admin port responds,
// logout refuses unless --force is set.
func TestTsnetLogoutRefusesWhileRunning(t *testing.T) {
	tmp := t.TempDir()
	// Spin up a tiny HTTP server to simulate a running bridge admin.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go srv.Serve(ln) //nolint:errcheck
	defer srv.Close()

	cfgPath := writeMinimalConfigWithAdminAddr(t, tmp, config.TailscaleConfig{Mode: "tsnet"}, ln.Addr().String())
	stateDir := filepath.Join(tmp, "tailscale")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "tailscaled.state"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Without --force: should refuse.
	var out, errBuf bytes.Buffer
	rc := tsnetCmd(context.Background(),
		[]string{"logout", "--config", cfgPath},
		strings.NewReader("WIPE\n"),
		&out, &errBuf)
	if rc != 1 {
		t.Errorf("rc = %d, want 1 (refuse while running)", rc)
	}
	if !strings.Contains(errBuf.String(), "appears to be running") {
		t.Errorf("expected running-instance error, got %q", errBuf.String())
	}

	// With --force: should proceed to confirm prompt and wipe.
	out.Reset()
	errBuf.Reset()
	rc = tsnetCmd(context.Background(),
		[]string{"logout", "--config", cfgPath, "--force"},
		strings.NewReader("WIPE\n"),
		&out, &errBuf)
	if rc != 0 {
		t.Errorf("--force: rc = %d, want 0; err=%q", rc, errBuf.String())
	}
}

// TestTailscaleAdminSourceDisabledReturnsSentinel — when both CLI
// and tsnet backends are nil (mode=disabled), Status() returns the
// sentinel "tailscale disabled" tile so the admin UI doesn't render
// an empty card. The sentinel must also name the recovery surface
// — `tailscale.mode` in the actual runtime config path — so an
// operator who didn't mean to disable Tailscale can recover without
// grepping the source. A regression that strips the recovery hint
// (e.g. shortening the message to just "disabled") would otherwise
// pass silently and leave operators stranded.
//
// Sub-cases pin BOTH the threading (the configured path appears in
// the message verbatim, NOT the hardcoded default) AND the visual
// contract (the path is %q-quoted so paths containing spaces stay
// unambiguous against the already-quoted "cli" / "tsnet" mode names
// — Gemini on the original deferral plan).
func TestTailscaleAdminSourceDisabledReturnsSentinel(t *testing.T) {
	cases := []struct {
		name       string
		configPath string
	}{
		{"non-default path", "/etc/onebit/test.yaml"},
		// Path-with-spaces is the load-bearing reason for %q quoting
		// in the format string. A future refactor that drops %q would
		// render `... in /Users/admin/My Bridge/bridge.yaml and
		// restart...` — visually ambiguous against the quoted mode
		// names. The strconv.Quote-based assertion below fails in
		// that case.
		{"path with spaces", "/Users/admin/My Bridge/bridge.yaml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := newTailscaleAdminSource(nil, nil, c.configPath, nil)
			st := src.Status()
			if st.LastError == "" {
				t.Fatalf("disabled mode should populate LastError, got %+v", st)
			}
			if !strings.Contains(st.LastError, "disabled") {
				t.Errorf("LastError should mention disabled, got %q", st.LastError)
			}
			if !strings.Contains(st.LastError, "tailscale.mode") {
				t.Errorf("LastError should name the config knob (tailscale.mode), got %q", st.LastError)
			}
			// %q quoting in the format string renders the path as
			// `"<path>"` (literal quote bytes). Asserting against
			// strconv.Quote(...) locks both the threading AND the
			// visual contract — drops to %s, no quoting, missing
			// path, or substituting the default would all fail.
			wantQuoted := strconv.Quote(c.configPath)
			if !strings.Contains(st.LastError, wantQuoted) {
				t.Errorf("LastError should name the runtime config path as %s, got %q", wantQuoted, st.LastError)
			}
			// Naming the valid modes explicitly is the most actionable
			// half of the recovery hint — without "cli" or "tsnet" the
			// operator still has to dig into docs to learn what value
			// to set. Locking these substrings prevents a future trim
			// from silently regressing the recovery instruction
			// (Gemini on PR #148).
			if !strings.Contains(st.LastError, "cli") {
				t.Errorf("LastError should mention the cli mode, got %q", st.LastError)
			}
			if !strings.Contains(st.LastError, "tsnet") {
				t.Errorf("LastError should mention the tsnet mode, got %q", st.LastError)
			}
		})
	}
}

// TestTailscaleAdminSourceDisabled_FallsBackToDefaultConfigName pins
// the empty-path defensive branch in displayConfigPath: if any future
// caller ever constructs the source without threading a path through,
// the recovery hint stays usable by falling back to "bridge.yaml".
// Asserts the quoted form (`"bridge.yaml"`) so the same %q visual
// contract from the main test holds for the fallback case too.
func TestTailscaleAdminSourceDisabled_FallsBackToDefaultConfigName(t *testing.T) {
	src := newTailscaleAdminSource(nil, nil, "", nil)
	st := src.Status()
	if st.LastError == "" {
		t.Fatalf("disabled mode should populate LastError, got %+v", st)
	}
	wantQuoted := strconv.Quote("bridge.yaml")
	if !strings.Contains(st.LastError, wantQuoted) {
		t.Errorf("empty configPath should fall back to %s, got %q", wantQuoted, st.LastError)
	}
}

// TestTailscaleAdminSourceRefreshNowDelegates — RefreshNow on an
// all-nil source returns the same sentinel as Status (no panic, no
// crash).
func TestTailscaleAdminSourceRefreshNowOnDisabled(t *testing.T) {
	src := newTailscaleAdminSource(nil, nil, "/etc/onebit/test.yaml", nil)
	st := src.RefreshNow(context.Background())
	if st.LastError == "" {
		t.Errorf("RefreshNow on disabled should populate LastError, got %+v", st)
	}
}

// MARK: - Helpers

// writeMinimalConfig writes a minimal bridge.yaml in t.TempDir()
// with the supplied TailscaleConfig and returns the path.
func writeMinimalConfig(t *testing.T, ts config.TailscaleConfig) string {
	t.Helper()
	return writeMinimalConfigInDir(t, t.TempDir(), ts)
}

// writeMinimalConfigInDir writes the config in a caller-supplied
// directory. Used by the logout tests so they can pre-seed a state
// dir under the SAME dataDir the config will resolve to.
func writeMinimalConfigInDir(t *testing.T, dir string, ts config.TailscaleConfig) string {
	t.Helper()
	libRoot := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll lib: %v", err)
	}
	// Quote path scalars: t.TempDir() on Windows produces paths with
	// `:` and `\` which would either break YAML parsing or be
	// silently parsed wrong as bare scalars. CodeRabbit Major on
	// PR #139.
	yaml := fmt.Sprintf(`libraryRoots:
  - %q
listenAddress: ":7788"
adminAddress: "127.0.0.1:7789"
dataDir: %q
scanIntervalSec: 600
libraryName: "test"
tailscale:
  mode: %q
`, libRoot, dir, ts.Mode)
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	return cfgPath
}

// silenceUnusedImport keeps the admin import alive — admin.TailscaleStatus
// is the return shape of newTailscaleAdminSource.Status(); the type
// itself is exercised by the disabled-sentinel tests above.
var _ = admin.TailscaleStatus{}

// silenceUnusedIO keeps io.EOF imported — the tsnetLogoutCmd helper
// references io.EOF in its scanner-error path, and tests of that
// path don't exercise the symbol via the function call alone.
var _ = io.EOF

// writeMinimalConfigWithAdminAddr is like writeMinimalConfigInDir
// but overrides the adminAddress — used by the running-instance
// detection test to point at a test-local HTTP server.
func writeMinimalConfigWithAdminAddr(t *testing.T, dir string, ts config.TailscaleConfig, adminAddr string) string {
	t.Helper()
	libRoot := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll lib: %v", err)
	}
	yaml := fmt.Sprintf(`libraryRoots:
  - %q
listenAddress: ":7788"
adminAddress: %q
dataDir: %q
scanIntervalSec: 600
libraryName: "test"
tailscale:
  mode: %q
`, libRoot, adminAddr, dir, ts.Mode)
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	return cfgPath
}
