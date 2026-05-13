// Tsnet integration glue for `bridge serve` (mode dispatch) AND
// the `bridge tsnet auth|status|logout` operator subcommands.
//
// Lives alongside cmd/bridge/tailscale.go so the two integration
// paths share a directory but have clearly-distinct files. The
// internal/tsnet module owns the wrapper; this file owns the
// process-level wiring (config → Server, admin adapter, CLI
// subcommands).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/tsnet"
)

// loadConfigForCmd parses a `--config` flag from args and loads the
// resulting bridge.yaml. Shared by all `bridge tsnet` subcommands —
// they all need the same config + the same flag shape, and a
// helper avoids three near-identical flag.Parse blocks.
func loadConfigForCmd(args []string, stderr io.Writer) (*config.Config, error) {
	fs := flag.NewFlagSet("tsnet", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return nil, err
	}
	return cfg, nil
}

// loadConfigAndRequireTsnetMode is the shared gate for all `bridge
// tsnet` subcommands. Loads the config AND verifies tailscale.mode
// is exactly "tsnet". Surfaces EffectiveMode's typo-detection
// error explicitly — pre-fix, the subcommands collapsed
// `EffectiveMode()` errors into the generic "set mode to tsnet"
// path, which hid config typos and reported the wrong current
// mode. CodeRabbit round-2 on PR #139.
func loadConfigAndRequireTsnetMode(args []string, stderr io.Writer) (*config.Config, error) {
	cfg, err := loadConfigForCmd(args, stderr)
	if err != nil {
		return nil, err
	}
	mode, err := cfg.Tailscale.EffectiveMode()
	if err != nil {
		fmt.Fprintf(stderr, "tailscale.mode: %v\n", err)
		return nil, err
	}
	if mode != config.TailscaleModeTsnet {
		fmt.Fprintf(stderr, "tailscale.mode is %q; set to \"tsnet\" in bridge.yaml to use this command\n", mode)
		return nil, fmt.Errorf("mode mismatch: %s", mode)
	}
	return cfg, nil
}

// newTsnetServer builds a configured *tsnet.Server from cfg. Returns
// an error if state-dir setup fails AT CONSTRUCTION (perms / path).
// The actual tailnet-Up dance happens in Server.Start, called from
// runServe's listener-spawn goroutine.
//
// Logger writes to stderr at debug level — production routes this
// through the bridge's existing slog setup (cfg.LogLevel) once the
// app-wide handler is wired. For now stderr is a deliberate
// debugging-friendly default: `bridge tsnet auth` operators want
// to see status updates, and the noise filter already drops the
// firehose.
func newTsnetServer(cfg *config.Config, stderr io.Writer) (*tsnet.Server, error) {
	stateDir := filepath.Join(cfg.DataDir, "tailscale")
	hostname := cfg.Tailscale.Hostname
	if hostname == "" {
		hostname = cfg.LibraryName
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	return tsnet.NewServer(tsnet.Config{
		AuthKey:  cfg.Tailscale.AuthKey,
		Hostname: hostname,
		StateDir: stateDir,
		Logger:   logger,
	})
}

// tailscaleAdminSource is admin.TailscaleStatusProvider-compatible
// AND backed by either the CLI auto-pilot (mode=cli) or the tsnet
// Server (mode=tsnet). The admin tile is a single rendering surface;
// branching on mode happens here, not in the handlers.
//
// When BOTH paths are nil (mode=disabled), Status() returns a
// sentinel "tailscale disabled" tile so the admin UI renders an
// explanatory message instead of an empty card. Real tile design
// for the disabled case is deferred — for now we ship a clear
// short string the operator can read.
type tailscaleAdminSource struct {
	cli        *tailscaleAutoPilot // nil unless mode=cli
	tsnet      *tsnet.Server       // nil unless mode=tsnet
	configPath string              // absolute path of the runtime config
	// file (post-filepath.Abs); surfaced in the disabled-mode recovery
	// message so operators running with --config <other> see the actual
	// file to edit instead of the hardcoded default. Callers MUST pass
	// the absolute form — passing the raw --config flag value would
	// surface a relative path that's ambiguous once the operator is no
	// longer in the bridge's startup CWD (Qodo + Gemini on PR #152).
	// Empty string falls back to "bridge.yaml" via displayConfigPath so
	// the message stays usable if any future caller ever constructs the
	// struct without one.
}

// newTailscaleAdminSource picks the right source based on which
// of the two backends was constructed. Mode=disabled → both nil,
// Status() returns the "disabled" sentinel; configPath is the
// absolute path of the runtime config file, surfaced in that
// sentinel — the canonical form admin.Cfg already uses, so the
// message points operators at the same file the bridge is
// operating on regardless of CWD changes post-boot.
func newTailscaleAdminSource(cli *tailscaleAutoPilot, ts *tsnet.Server, configPath string) tailscaleAdminSource {
	return tailscaleAdminSource{cli: cli, tsnet: ts, configPath: configPath}
}

// displayConfigPath returns the operator-facing config-file reference.
// Surfaces the actual --config value when set; falls back to the
// default file name when empty. Defensive — keeps the recovery hint
// usable if a future caller ever constructs the source without
// threading a path through.
func (s tailscaleAdminSource) displayConfigPath() string {
	if s.configPath == "" {
		return "bridge.yaml"
	}
	return s.configPath
}

// adminStatusTimeout caps how long admin handlers will wait on the
// tsnet LocalClient before degrading to a "stalled" tile. Without
// this, a hung control-plane connection makes the admin UI
// unresponsive instead of surfacing the failure. CodeRabbit Major
// + Gemini medium on PR #139.
const adminStatusTimeout = 5 * time.Second

func (s tailscaleAdminSource) Status() admin.TailscaleStatus {
	if s.cli != nil {
		return toAdminStatus(s.cli.Snapshot())
	}
	if s.tsnet != nil {
		ctx, cancel := context.WithTimeout(context.Background(), adminStatusTimeout)
		defer cancel()
		return tsnetAdminStatus(ctx, s.tsnet)
	}
	return admin.TailscaleStatus{
		// Empty CLIAvailable + "tailscale disabled" message lets the
		// admin tile render a one-line explanation instead of an
		// empty card. Operators who want LAN-only deploys see this
		// and don't go hunting for misconfig.
		//
		// The message names the config knob and the actual runtime
		// config path (via displayConfigPath) so an operator who
		// DIDN'T mean to disable Tailscale can recover without
		// grepping the source. %q (not %s) for the config path:
		// paths can contain spaces, and matching the visual
		// treatment of "cli" / "tsnet" keeps the rendered sentence
		// unambiguous.
		LastError: fmt.Sprintf(
			"Tailscale integration disabled. To enable, set tailscale.mode to %q or %q in %q and restart the bridge.",
			"cli", "tsnet", s.displayConfigPath(),
		),
	}
}

func (s tailscaleAdminSource) RefreshNow(ctx context.Context) admin.TailscaleStatus {
	if s.cli != nil {
		return toAdminStatus(s.cli.RefreshNow(ctx))
	}
	if s.tsnet != nil {
		// Honour the caller's context (Gemini medium on PR #139:
		// the prior implementation ignored ctx and used
		// context.Background, defeating any caller-side timeout).
		// Cap with adminStatusTimeout so a hung tsnet doesn't sit
		// past the admin handler's expectations even when the
		// caller passed a longer ctx.
		bounded, cancel := context.WithTimeout(ctx, adminStatusTimeout)
		defer cancel()
		return tsnetAdminStatus(bounded, s.tsnet)
	}
	// Disabled mode: no live state to refresh. Return the same
	// sentinel Status() returns.
	return s.Status()
}

// tsnetAdminStatus translates the tsnet Server's live tailnet view
// into admin.TailscaleStatus. Maps the upstream ipnstate.Status
// fields onto the admin tile shape — keeping the tile rendering
// code path-agnostic. Honours `ctx` so admin handlers can bound
// the live LocalClient call.
func tsnetAdminStatus(ctx context.Context, s *tsnet.Server) admin.TailscaleStatus {
	out := admin.TailscaleStatus{
		// CLIAvailable lights the "I can talk to tailscale" indicator
		// in the admin UI. Under tsnet mode it's effectively always
		// true once Start has succeeded — we don't shell out so
		// there's no CLI to "find".
		CLIAvailable: true,
	}
	st, err := s.Status(ctx)
	if err != nil {
		out.LastError = fmt.Sprintf("tsnet status: %v", err)
		return out
	}
	if st.Self != nil {
		out.NodeName = st.Self.HostName
		if len(st.Self.TailscaleIPs) > 0 {
			out.MagicDNSName = st.Self.DNSName
		}
	}
	domains := s.CertDomains()
	out.HTTPSCertsEnabled = len(domains) > 0
	// Cert presence + expiry — tsnet manages LE certs in-process so
	// we don't have a "file on disk" notion. Mark CertPresent true
	// when we have at least one cert domain; CertNotAfter is left
	// zero (the upstream LocalClient doesn't expose the parsed
	// cert expiry; surfacing this would require a separate ACME
	// API call).
	out.CertPresent = out.HTTPSCertsEnabled
	return out
}

// tsnetCmd is the dispatcher for `bridge tsnet [auth|status|logout]`.
// Wired into run()'s subcommand switch.
func tsnetCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: bridge tsnet <auth|status|logout> [flags]")
		return 2
	}
	switch args[0] {
	case "auth":
		return tsnetAuthCmd(ctx, args[1:], stdout, stderr)
	case "status":
		return tsnetStatusCmd(ctx, args[1:], stdout, stderr)
	case "logout":
		return tsnetLogoutCmd(args[1:], stdin, stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, `bridge tsnet — embedded tailnet node management

Subcommands:
  auth    Authenticate this bridge to the tailnet (interactive on first run)
  status  Print the current tailnet status as JSON
  logout  Wipe the tsnet state dir (operator-confirmed; requires re-auth on next start)`)
		return 0
	}
	fmt.Fprintf(stderr, "unknown tsnet subcommand: %s\n", args[0])
	return 2
}

// tsnetAuthCmd runs the interactive-auth flow. Loads the bridge
// config, constructs a Server with no AuthKey override, calls
// Start in a goroutine, polls AuthURL() until non-empty, prints
// it, and waits for Start to return (signalling the operator
// completed the browser auth).
//
// On a host that already has persisted state, Start returns
// immediately without printing anything — operator sees "already
// authenticated" and exits clean.
func tsnetAuthCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := loadConfigAndRequireTsnetMode(args, stderr)
	if err != nil {
		return 1
	}
	server, err := newTsnetServer(cfg, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "construct tsnet server: %v\n", err)
		return 1
	}
	defer server.Close()

	// Up to 5 minutes of human time for the browser auth. On a
	// fresh tailnet account that's plenty; on already-authed state
	// Start returns near-instantly so the timeout doesn't bite.
	authCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	startErr := make(chan error, 1)
	go func() { startErr <- server.Start(authCtx) }()

	// Poll for AuthURL surfacing. Print it the moment it lands so
	// the operator can click immediately. If no URL ever surfaces
	// (already-authed path), Start returns and we exit clean.
	urlPrinted := false
	pollT := time.NewTicker(200 * time.Millisecond)
	defer pollT.Stop()

	for {
		select {
		case err := <-startErr:
			if err != nil {
				fmt.Fprintf(stderr, "tsnet auth: %v\n", err)
				return 1
			}
			if !urlPrinted {
				fmt.Fprintln(stdout, "tsnet: already authenticated (state present from prior run)")
			} else {
				fmt.Fprintln(stdout, "tsnet: authentication complete")
			}
			return 0
		case <-pollT.C:
			if !urlPrinted {
				if u := server.AuthURL(); u != "" {
					fmt.Fprintf(stdout, "Open this URL in a browser to authenticate the bridge:\n  %s\n", u)
					urlPrinted = true
				}
			}
		case <-ctx.Done():
			fmt.Fprintln(stderr, "tsnet auth: cancelled")
			return 1
		}
	}
}

// tsnetStatusCmd prints the tsnet Server's live status as JSON.
// Caller-friendly for scripts (parseable) and operators (pretty-
// printed). Doesn't require interactive auth — works on any
// previously-auth'd state.
func tsnetStatusCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := loadConfigAndRequireTsnetMode(args, stderr)
	if err != nil {
		return 1
	}
	server, err := newTsnetServer(cfg, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "construct tsnet server: %v\n", err)
		return 1
	}
	defer server.Close()

	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := server.Start(startCtx); err != nil {
		fmt.Fprintf(stderr, "tsnet start: %v\n", err)
		return 1
	}
	// Bound the live LocalClient call (CodeRabbit Major round-2
	// on PR #139). A stalled control plane shouldn't hang
	// `bridge tsnet status` on the operator's terminal — fail
	// fast at adminStatusTimeout so the caller can see the
	// failure mode and try again.
	statusCtx, statusCancel := context.WithTimeout(ctx, adminStatusTimeout)
	defer statusCancel()
	st, err := server.Status(statusCtx)
	if err != nil {
		fmt.Fprintf(stderr, "tsnet status: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		fmt.Fprintf(stderr, "encode status: %v\n", err)
		return 1
	}
	return 0
}

// logoutConfirmPhrase is the operator-typed string `tsnetLogoutCmd`
// requires before wiping state. A single "y" / "yes" was too easy
// to fat-finger (CodeRabbit Major on PR #139); requiring a
// destructive-action-flavoured phrase makes accidental wipe
// vanishingly unlikely.
const logoutConfirmPhrase = "WIPE"

// tsnetLogoutCmd wipes the tsnet state dir AFTER an operator
// confirmation prompt. Wiping kicks the bridge off the tailnet —
// next start will require re-auth (interactive OAuth or AuthKey).
//
// Detects a running `bridge serve` by probing the admin port; if
// the bridge is live, refuses unless `--force` is set. Wiping
// state under a live server can leave runtime/disk state
// inconsistent.
func tsnetLogoutCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tsnet logout", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	force := fs.Bool("force", false, "skip running-instance check")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 1
	}
	mode, modeErr := cfg.Tailscale.EffectiveMode()
	if modeErr != nil {
		fmt.Fprintf(stderr, "tailscale config: %v\n", modeErr)
		return 1
	}
	if mode != config.TailscaleModeTsnet {
		fmt.Fprintf(stderr, "tailscale.mode is %q, not %q — nothing to wipe\n", mode, config.TailscaleModeTsnet)
		return 1
	}

	stateDir := filepath.Join(cfg.DataDir, "tailscale")
	if _, err := os.Stat(stateDir); os.IsNotExist(err) {
		fmt.Fprintln(stdout, "tsnet: no state to wipe (already logged out)")
		return 0
	}

	// Detect a running bridge by probing the admin port. Same
	// pattern as tryLibraryViaAdmin (cmd/bridge/library.go).
	if !*force {
		if running := isAdminAlive(cfg); running {
			fmt.Fprintln(stderr, "tsnet logout: a bridge instance appears to be running (admin port responded).")
			fmt.Fprintln(stderr, "  stop the bridge first (`bridge stop` or the platform service manager),")
			fmt.Fprintln(stderr, "  or pass --force to override.")
			return 1
		}
	}

	fmt.Fprintf(stdout, `About to wipe tsnet state at %s.

This logs the bridge off the tailnet; next start will require re-auth.

Type %q to confirm, anything else to cancel: `, stateDir, logoutConfirmPhrase)

	// Use a bufio.Scanner instead of fmt.Fscanln. Fscanln stops at
	// the first whitespace AND surfaces a brittle "unexpected
	// newline" string we previously matched against — Gemini medium
	// on PR #139. Scanner reads whole lines and gives us a clean
	// no-error path on a bare newline.
	scanner := bufio.NewScanner(stdin)
	if !scanner.Scan() {
		// Stdin closed without a line of input (e.g. piped from
		// /dev/null). Treat as cancel — destructive action shouldn't
		// run on empty input.
		if err := scanner.Err(); err != nil {
			fmt.Fprintf(stderr, "read response: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "cancelled")
		return 0
	}
	resp := strings.TrimSpace(scanner.Text())
	if resp != logoutConfirmPhrase {
		fmt.Fprintln(stdout, "cancelled")
		return 0
	}
	if err := os.RemoveAll(stateDir); err != nil {
		fmt.Fprintf(stderr, "wipe state: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "tsnet: state wiped. Next `bridge serve` will require re-auth.")
	return 0
}

// isAdminAlive probes the configured admin address with a short
// timeout. Returns true if the admin API responds, indicating a
// running bridge instance. Connection refused / timeout = not
// running.
func isAdminAlive(cfg *config.Config) bool {
	addr := cfg.AdminAddress
	if addr == "" {
		addr = config.DefaultAdminAddress
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/stats", nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
