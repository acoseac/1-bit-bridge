package tsnet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// Server wraps tailscale.com/tsnet.Server with the bridge-specific
// lifecycle (interactive auth, slog-routed logging, secure state-dir
// guard, idempotent Start/Close). Construct with NewServer; call
// Start once to bring the node up; call Close on shutdown.
type Server struct {
	cfg     Config
	server  *tsnet.Server
	log     *slog.Logger
	started bool
	authURL string // captured from UserLogf during Start; surfaced via AuthURL()
}

// Config carries everything NewServer needs. Mode is unused here —
// the dispatcher in cmd/bridge/main.go branches on it before
// constructing a *Server. Logger MUST be non-nil.
type Config struct {
	// AuthKey, if non-empty, is used on first run. Empty triggers
	// interactive OAuth (Server.Start prints the AuthURL via the
	// logger and blocks until the operator authorizes in a browser).
	// Once tsnet has persisted state, this field is ignored.
	//
	// Tailscale-native idiom is the TS_AUTHKEY environment variable;
	// we read this field FIRST and fall back to the env var if
	// empty. Yaml config is the third-tier escape hatch.
	AuthKey string
	// Hostname is the magic-DNS hostname tsnet registers with.
	// Empty falls back to the deviceName / library name supplied
	// by the caller via this field.
	Hostname string
	// StateDir is the on-disk location of tsnet's state store.
	// MUST be set explicitly — empty causes tsnet to fall into
	// ephemeral mode (re-registers as a new device on every restart,
	// surprises the operator's admin console). Typically
	// <dataDir>/tailscale/, mode 0700 on POSIX.
	StateDir string
	// Logger receives both UserLogf (operator-facing AuthURL +
	// status) and Logf (verbose backend). The two channels are
	// adapted into different slog levels — see logfFromSlog and
	// noisyLogfFromSlog.
	Logger *slog.Logger
}

// NewServer validates the configuration and constructs an unstarted
// Server. The actual tailnet-up dance happens in Start.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		return nil, errors.New("tsnet: Config.Logger is required")
	}
	if cfg.StateDir == "" {
		// An empty state dir would put tsnet into "fall back to
		// os.UserConfigDir-derived ephemeral mode" territory. That
		// re-registers the bridge as a brand-new tailnet node on
		// every restart, eating IPs from the operator's pool and
		// surprising them with a fresh device entry each boot.
		// Refuse loudly here.
		return nil, errors.New("tsnet: Config.StateDir is required (empty triggers ephemeral mode)")
	}
	return &Server{cfg: cfg, log: cfg.Logger}, nil
}

// Start brings the tailnet node up. Idempotent: a second Start on
// an already-started server is a no-op.
//
// First-run behaviour: if no AuthKey is configured AND no persisted
// state exists, tsnet emits an AuthURL via UserLogf — Start blocks
// until the operator opens the URL and authorizes the device.
//
// Subsequent runs (state persisted) re-authenticate from the state
// store and return as soon as the tailnet is reachable.
func (s *Server) Start(ctx context.Context) error {
	if s.started {
		return nil
	}

	if err := os.MkdirAll(s.cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("tsnet: create state dir %s: %w", s.cfg.StateDir, err)
	}
	if err := assertSecureDir(s.cfg.StateDir); err != nil {
		return fmt.Errorf("tsnet: state dir security check failed: %w", err)
	}

	// AuthKey precedence: explicit Config.AuthKey > TS_AUTHKEY env
	// var > empty (triggers interactive OAuth on first run).
	authKey := s.cfg.AuthKey
	if authKey == "" {
		authKey = os.Getenv("TS_AUTHKEY")
	}

	fresh := !hasPersistedTsnetState(s.cfg.StateDir)
	if fresh && authKey == "" {
		s.log.Warn("tsnet: no persisted state and no AuthKey — interactive auth required",
			slog.String("stateDir", s.cfg.StateDir))
	}

	s.server = &tsnet.Server{
		Dir:       s.cfg.StateDir,
		Hostname:  s.cfg.Hostname,
		AuthKey:   authKey,
		Ephemeral: false,
		// UserLogf carries operator-facing messages — the AuthURL is
		// the load-bearing one. Route to a level-INFO-pinned adapter
		// that also captures any login URL into s.authURL so
		// `bridge tsnet auth` can echo it explicitly.
		UserLogf: s.userLogf(),
		// Logf is the firehose of magicsock / netcheck / control-plane
		// chatter. Filtered down to error-flavoured lines via a
		// string-match heuristic (tsnet's Logf doesn't carry a level).
		Logf: noisyLogfFromSlog(s.log),
	}

	if err := s.server.Start(); err != nil {
		return fmt.Errorf("tsnet: server start: %w", err)
	}
	if _, err := s.server.Up(ctx); err != nil {
		// On Up failure, close the partially-started server so
		// subsequent Start retries don't run into a half-up state.
		_ = s.server.Close()
		s.server = nil
		return fmt.Errorf("tsnet: bring node up: %w", err)
	}
	s.started = true
	return nil
}

// ListenTLS returns an HTTPS listener with auto-renewing Let's
// Encrypt certs. Backed by tsnet's in-process ACME — there's no
// on-disk cert material. addr should be ":443" or ":7798"; the
// network is always "tcp".
func (s *Server) ListenTLS(addr string) (net.Listener, error) {
	if !s.started || s.server == nil {
		return nil, errors.New("tsnet: ListenTLS called before Start")
	}
	return s.server.ListenTLS("tcp", addr)
}

// Status returns the live tailnet view (peer reachability, magic-DNS
// hostname, assigned IPs). Returned shape is the upstream
// ipnstate.Status so admin templates that already parse `tailscale
// status --json` output don't need a translation layer.
func (s *Server) Status(ctx context.Context) (*ipnstate.Status, error) {
	if !s.started || s.server == nil {
		return nil, errors.New("tsnet: Status called before Start")
	}
	lc, err := s.server.LocalClient()
	if err != nil {
		return nil, fmt.Errorf("tsnet: local client: %w", err)
	}
	return lc.Status(ctx)
}

// CertDomains returns the magic-DNS names tsnet will serve LE certs
// for. Empty slice before Start.
func (s *Server) CertDomains() []string {
	if !s.started || s.server == nil {
		return nil
	}
	return s.server.CertDomains()
}

// AuthURL returns the most recent interactive-auth URL captured from
// UserLogf, or empty if none was emitted. Used by `bridge tsnet
// auth` to echo the URL explicitly to the operator's terminal in
// case the slog output was filtered or piped elsewhere.
func (s *Server) AuthURL() string {
	return s.authURL
}

// Close tears down the embedded tsnet.Server. MUST be called on
// bridge shutdown to drain magicsock / netcheck / control-plane
// goroutines — without it, every Start/Close cycle (e.g. integration
// tests) leaks goroutines until process exit.
func (s *Server) Close() error {
	if s.server == nil {
		return nil
	}
	err := s.server.Close()
	s.server = nil
	s.started = false
	return err
}

// userLogf returns a logger.Logf-shaped adapter that:
//   - captures any line that looks like an interactive-auth URL into
//     s.authURL (so AuthURL() can echo it later)
//   - forwards the line to slog at INFO level
//
// String-matching the AuthURL is fragile against tsnet upstream
// wording changes; the failure mode is "AuthURL() returns empty
// even though tsnet emitted a URL", which surfaces in QA when a
// fresh-state bridge can't be authed via the CLI helper. Acceptable
// trade — the operator can always read the URL from the slog output
// directly.
func (s *Server) userLogf() func(string, ...any) {
	return func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if u := extractTailscaleAuthURL(msg); u != "" {
			s.authURL = u
		}
		s.log.Info(msg, slog.String("source", "tsnet.user"))
	}
}

// noisyLogfFromSlog adapts tsnet's verbose backend Logf channel
// (magicsock, netcheck, control-plane keepalives) into slog. tsnet's
// Logf doesn't carry log levels, so we string-match for
// error-flavoured lines and drop the rest. On a quiet steady-state
// connection this discards essentially everything; on a pathological
// connection it surfaces the real failure.
func noisyLogfFromSlog(log *slog.Logger) func(string, ...any) {
	return func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		lower := strings.ToLower(msg)
		switch {
		case strings.Contains(lower, "panic"),
			strings.Contains(lower, "fatal"):
			log.Error(msg, slog.String("source", "tsnet.backend"))
		case strings.Contains(lower, "error"),
			strings.Contains(lower, "failed"):
			log.Warn(msg, slog.String("source", "tsnet.backend"))
		default:
			// Drop on floor. 90%+ of tsnet's backend logs land here.
		}
	}
}

// extractTailscaleAuthURL returns the AuthURL embedded in a tsnet
// UserLogf message, or empty if none. tsnet emits messages of the
// form:
//
//	"To authenticate, visit: https://login.tailscale.com/a/<token>"
//	"https://login.tailscale.com/a/<token>"
//
// We match either prefix and pull the URL through the next
// whitespace boundary.
func extractTailscaleAuthURL(msg string) string {
	const marker = "https://login.tailscale.com/"
	idx := strings.Index(msg, marker)
	if idx < 0 {
		return ""
	}
	tail := msg[idx:]
	// URL ends at the first whitespace / control char.
	end := len(tail)
	for i, r := range tail {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			end = i
			break
		}
	}
	return tail[:end]
}

// hasPersistedTsnetState reports whether the state dir holds an
// already-authenticated tsnet identity. Used by the startup
// misconfig sentinel to distinguish "first run, will need
// interactive auth" from "state was deleted, will silently
// re-register as new device" (a different operator-facing message
// is appropriate for each).
//
// tsnet's default FileStore writes its state to
// <Dir>/tailscaled.state — file existence is the canonical signal.
func hasPersistedTsnetState(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, "tailscaled.state"))
	return err == nil
}
