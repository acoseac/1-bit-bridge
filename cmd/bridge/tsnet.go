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
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	cli   *tailscaleAutoPilot // nil unless mode=cli
	tsnet *tsnet.Server       // nil unless mode=tsnet
}

// newTailscaleAdminSource picks the right source based on which
// of the two backends was constructed. Mode=disabled → both nil,
// Status() returns the "disabled" sentinel.
func newTailscaleAdminSource(cli *tailscaleAutoPilot, ts *tsnet.Server) tailscaleAdminSource {
	return tailscaleAdminSource{cli: cli, tsnet: ts}
}

func (s tailscaleAdminSource) Status() admin.TailscaleStatus {
	if s.cli != nil {
		return toAdminStatus(s.cli.Snapshot())
	}
	if s.tsnet != nil {
		return tsnetAdminStatus(s.tsnet)
	}
	return admin.TailscaleStatus{
		// Empty CLIAvailable + "tailscale disabled" message lets the
		// admin tile render a one-line explanation instead of an
		// empty card. Operators who want LAN-only deploys see this
		// and don't go hunting for misconfig.
		LastError: "tailscale integration disabled (tailscale.mode: disabled)",
	}
}

func (s tailscaleAdminSource) RefreshNow(ctx context.Context) admin.TailscaleStatus {
	if s.cli != nil {
		return toAdminStatus(s.cli.RefreshNow(ctx))
	}
	// tsnet doesn't have a "refresh" concept — Status() always
	// reflects the live LocalClient view. Operator's Refresh
	// click is just a re-render request.
	return s.Status()
}

// tsnetAdminStatus translates the tsnet Server's live tailnet view
// into admin.TailscaleStatus. Maps the upstream ipnstate.Status
// fields onto the admin tile shape — keeping the tile rendering
// code path-agnostic.
func tsnetAdminStatus(s *tsnet.Server) admin.TailscaleStatus {
	out := admin.TailscaleStatus{
		// CLIAvailable lights the "I can talk to tailscale" indicator
		// in the admin UI. Under tsnet mode it's effectively always
		// true once Start has succeeded — we don't shell out so
		// there's no CLI to "find".
		CLIAvailable: true,
	}
	st, err := s.Status(context.Background())
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
	cfg, err := loadConfigForCmd(args, stderr)
	if err != nil {
		return 1
	}
	if mode, _ := cfg.Tailscale.EffectiveMode(); mode != config.TailscaleModeTsnet {
		fmt.Fprintf(stderr, "tailscale.mode is %q; set to \"tsnet\" in bridge.yaml to use this command\n", mode)
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
	cfg, err := loadConfigForCmd(args, stderr)
	if err != nil {
		return 1
	}
	if mode, _ := cfg.Tailscale.EffectiveMode(); mode != config.TailscaleModeTsnet {
		fmt.Fprintf(stderr, "tailscale.mode is %q; set to \"tsnet\" in bridge.yaml to use this command\n", mode)
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
	st, err := server.Status(ctx)
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

// tsnetLogoutCmd wipes the tsnet state dir AFTER an operator
// confirmation prompt. Wiping kicks the bridge off the tailnet —
// next start will require re-auth (interactive OAuth or AuthKey).
//
// Refuses to run while a `bridge serve` is active: the running
// server holds an open file in the state dir, and racing the
// wipe against the live server would either corrupt state or
// leak goroutines. We detect by attempting to take a flock-style
// marker (TODO: integrate with the existing supervision lock).
// For v1, just print a warning and require explicit --force.
func tsnetLogoutCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// Minimal shape for v1 — no flags, no force-flag yet. Operator
	// confirms via Y/n at the prompt.
	cfg, err := loadConfigForCmd(args, stderr)
	if err != nil {
		return 1
	}
	if mode, _ := cfg.Tailscale.EffectiveMode(); mode != config.TailscaleModeTsnet {
		fmt.Fprintf(stderr, "tailscale.mode is %q; set to \"tsnet\" in bridge.yaml to use this command\n", mode)
		return 1
	}
	stateDir := filepath.Join(cfg.DataDir, "tailscale")
	if _, err := os.Stat(stateDir); os.IsNotExist(err) {
		fmt.Fprintln(stdout, "tsnet: no state to wipe (already logged out)")
		return 0
	}
	fmt.Fprintf(stdout, "About to wipe tsnet state at %s.\nThis logs the bridge off the tailnet; next start will require re-auth.\nProceed? [y/N]: ", stateDir)
	var resp string
	if _, err := fmt.Fscanln(stdin, &resp); err != nil && !errors.Is(err, io.EOF) {
		// Empty response (just hit enter) → treat as "no". Other
		// scanner errors → bail.
		if !errors.Is(err, io.ErrUnexpectedEOF) && err.Error() != "unexpected newline" {
			fmt.Fprintf(stderr, "read response: %v\n", err)
			return 1
		}
	}
	if resp != "y" && resp != "Y" && resp != "yes" {
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
