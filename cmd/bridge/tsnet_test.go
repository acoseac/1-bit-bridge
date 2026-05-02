package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

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

// TestTsnetLogoutDeclineCancels — answering "n" at the confirm
// prompt MUST leave the state dir intact AND exit 0 (cancel is
// a graceful no-op, not a failure).
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

	var out, errBuf bytes.Buffer
	rc := tsnetCmd(context.Background(),
		[]string{"logout", "--config", cfgPath},
		strings.NewReader("n\n"),
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
}

// TestTsnetLogoutConfirmWipes — answering "y" wipes the state dir.
// Exit 0, state file is gone.
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
		[]string{"logout", "--config", cfgPath},
		strings.NewReader("y\n"),
		&out, &errBuf)
	if rc != 0 {
		t.Errorf("rc = %d, want 0 (confirm wipe)", rc)
	}
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Errorf("state file should have been wiped, stat err = %v", err)
	}
}

// TestTailscaleAdminSourceDisabledReturnsSentinel — when both CLI
// and tsnet backends are nil (mode=disabled), Status() returns the
// sentinel "tailscale disabled" tile so the admin UI doesn't render
// an empty card.
func TestTailscaleAdminSourceDisabledReturnsSentinel(t *testing.T) {
	src := newTailscaleAdminSource(nil, nil)
	st := src.Status()
	if st.LastError == "" {
		t.Errorf("disabled mode should populate LastError, got %+v", st)
	}
	if !strings.Contains(st.LastError, "disabled") {
		t.Errorf("LastError should mention disabled, got %q", st.LastError)
	}
}

// TestTailscaleAdminSourceRefreshNowDelegates — RefreshNow on an
// all-nil source returns the same sentinel as Status (no panic, no
// crash).
func TestTailscaleAdminSourceRefreshNowOnDisabled(t *testing.T) {
	src := newTailscaleAdminSource(nil, nil)
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
	yaml := fmt.Sprintf(`libraryRoots:
  - %s
listenAddress: ":7788"
adminAddress: "127.0.0.1:7789"
dataDir: %s
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
