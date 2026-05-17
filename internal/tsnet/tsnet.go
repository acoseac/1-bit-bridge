package tsnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// ... rest of the struct definition ...

// ListenPacket wraps the underlying tsnet.Server.ListenPacket.
func (s *Server) ListenPacket(network, addr string) (net.PacketConn, error) {
	s.lifecycleMu.Lock()
	server, started := s.server, s.started
	s.lifecycleMu.Unlock()

	if !started || server == nil {
		return nil, errors.New("tsnet: ListenPacket called before Start")
	}
	return server.ListenPacket(network, addr)
}

// HTTP3TLSConfig returns a tls.Config suitable for use with http3.Server.
// It uses GetCertificate to dynamically fetch certificates from the
// tailnet engine, ensuring QUIC handshakes work over tsnet without
// on-disk cert files.
func (s *Server) HTTP3TLSConfig() *tls.Config {
	return &tls.Config{
		NextProtos: []string{"h3"}, // Required for HTTP/3 ALPN
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			s.lifecycleMu.Lock()
			srv := s.server
			started := s.started
			s.lifecycleMu.Unlock()

			if !started || srv == nil {
				return nil, errors.New("tsnet server not ready for TLS handshake")
			}
			lc, err := srv.LocalClient()
			if err != nil {
				return nil, err
			}
			return lc.GetCertificate(chi)
		},
	}
}

// Server wraps tailscale.com/tsnet.Server with the bridge-specific
// lifecycle (interactive auth, slog-routed logging, secure state-dir
// guard, idempotent Start/Close). Construct with NewServer; call
// Start once to bring the node up; call Close on shutdown.
//
// Concurrency model: Start and Close are caller-serialized in
// production (cmd/bridge/main.go single-threaded). Read paths
// (Status, ListenTLS, CertDomains, AuthURL) AND the userLogf
// callback (invoked from tsnet's internal goroutines during the
// Up() phase) need cheap, non-blocking access — so the lock
// surface is split:
//
//   - lifecycleMu protects started + server. Start releases this
//     lock during the long Up() I/O so userLogf can take authMu
//     without deadlocking. Close holds it through the upstream
//     Close() call (~ms).
//   - authMu protects authURL only. userLogf takes it for ~µs
//     per write; AuthURL() takes it for one read. Doesn't block
//     on Up() I/O.
//
// Caller-side: Start and Close are NOT safe to call concurrently.
// The wrapper detects double-Start (second caller sees the
// in-progress flag and returns an error) but doesn't try to be
// fully reentrant — cmd/bridge/main.go is the sole caller and
// runs them serially.
type Server struct {
	cfg Config
	log *slog.Logger

	// lifecycleMu protects started + server.
	lifecycleMu sync.Mutex
	server      *tsnet.Server
	started     bool
	starting    bool // detect double-Start

	// authMu protects authURL ONLY. Held briefly by userLogf
	// (background goroutine, write) and AuthURL() (caller, read).
	// Separate mutex so AuthURL reads don't block on Start's
	// long-running tsnet.Up() call.
	authMu  sync.Mutex
	authURL string
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
	// Precedence at startup (Tailscale-standard idiom):
	//   1. TS_AUTHKEY environment variable (preferred — keeps
	//      secrets out of yaml-on-disk)
	//   2. This field (fallback for ops who can't set env vars)
	//   3. Empty → triggers interactive OAuth on first run
	//
	// See startUnlocked for the resolution code.
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
// an already-started server is a no-op (returns nil). A concurrent
// Start while another is in flight returns an error rather than
// racing — cmd/bridge/main.go calls Start serially, so this is a
// programmer-error guard, not a performance path.
//
// First-run behaviour: if no AuthKey is configured AND no persisted
// state exists, tsnet emits an AuthURL via UserLogf — Start blocks
// until the operator opens the URL and authorizes the device.
//
// Subsequent runs (state persisted) re-authenticate from the state
// store and return as soon as the tailnet is reachable.
func (s *Server) Start(ctx context.Context) error {
	// Phase 1: short critical section to claim the start slot.
	s.lifecycleMu.Lock()
	if s.started {
		s.lifecycleMu.Unlock()
		return nil
	}
	if s.starting {
		s.lifecycleMu.Unlock()
		return errors.New("tsnet: Start already in progress")
	}
	s.starting = true
	s.lifecycleMu.Unlock()

	// Phase 2: do the actual work without holding the mutex —
	// tsnet.Server.Up() can block on interactive auth for minutes,
	// and userLogf (which writes authURL) runs from tsnet's own
	// goroutines during this window. Holding lifecycleMu through
	// Up would deadlock against any concurrent AuthURL() read.
	server, err := s.startUnlocked(ctx)

	// Phase 3: short critical section to publish the result.
	s.lifecycleMu.Lock()
	s.starting = false
	if err != nil {
		// Close any half-built tsnet.Server BEFORE returning so
		// retries don't leak goroutines. Pre-fix, only the Up()
		// error path closed; the Start() error path leaked. Qodo
		// bug #4 + CodeRabbit on PR #138.
		if server != nil {
			_ = server.Close()
		}
		s.lifecycleMu.Unlock()
		return err
	}
	s.server = server
	s.started = true
	s.lifecycleMu.Unlock()
	return nil
}

// startUnlocked does the I/O-heavy parts of Start without holding
// lifecycleMu. Returns the constructed tsnet.Server (or nil on
// pre-construction error) so Start can clean up under the lock.
func (s *Server) startUnlocked(ctx context.Context) (*tsnet.Server, error) {
	// MkdirAll doesn't tighten perms on an existing dir (Qodo bug #1).
	// Always Chmod after to ensure 0700 even if the dir pre-existed
	// with looser perms — assertSecureDir would otherwise refuse
	// to start AND the operator wouldn't know we tried to fix it
	// for them.
	if err := os.MkdirAll(s.cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("tsnet: create state dir %s: %w", s.cfg.StateDir, err)
	}
	if err := chmodStateDir(s.cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("tsnet: chmod state dir: %w", err)
	}
	if err := assertSecureDir(s.cfg.StateDir); err != nil {
		return nil, fmt.Errorf("tsnet: state dir security check failed: %w", err)
	}

	// AuthKey precedence (Tailscale standard idiom): TS_AUTHKEY env
	// var FIRST so secrets stay out of yaml; Config.AuthKey is the
	// fallback for ops who can't set env vars; empty triggers
	// interactive OAuth on first run.
	authKey := os.Getenv("TS_AUTHKEY")
	if authKey == "" {
		authKey = s.cfg.AuthKey
	}

	fresh := !hasPersistedTsnetState(s.cfg.StateDir)
	if fresh && authKey == "" {
		s.log.Warn("tsnet: no persisted state and no AuthKey — interactive auth required",
			slog.String("stateDir", s.cfg.StateDir))
	}

	server := &tsnet.Server{
		Dir:       s.cfg.StateDir,
		Hostname:  s.cfg.Hostname,
		AuthKey:   authKey,
		Ephemeral: false,
		UserLogf:  s.userLogf(),
		Logf:      noisyLogfFromSlog(s.log),
	}

	if err := server.Start(); err != nil {
		return server, fmt.Errorf("tsnet: server start: %w", err)
	}
	if _, err := server.Up(ctx); err != nil {
		return server, fmt.Errorf("tsnet: bring node up: %w", err)
	}
	return server, nil
}

// ListenTLS returns an HTTPS listener with auto-renewing Let's
// Encrypt certs. Backed by tsnet's in-process ACME — there's no
// on-disk cert material. addr should be ":443" or ":7798"; the
// network is always "tcp".
func (s *Server) ListenTLS(addr string) (net.Listener, error) {
	s.lifecycleMu.Lock()
	server, started := s.server, s.started
	s.lifecycleMu.Unlock()
	if !started || server == nil {
		return nil, errors.New("tsnet: ListenTLS called before Start")
	}
	return server.ListenTLS("tcp", addr)
}

// Status returns the live tailnet view (peer reachability, magic-DNS
// hostname, assigned IPs). Returned shape is the upstream
// ipnstate.Status so admin templates that already parse `tailscale
// status --json` output don't need a translation layer.
func (s *Server) Status(ctx context.Context) (*ipnstate.Status, error) {
	s.lifecycleMu.Lock()
	server, started := s.server, s.started
	s.lifecycleMu.Unlock()
	if !started || server == nil {
		return nil, errors.New("tsnet: Status called before Start")
	}
	lc, err := server.LocalClient()
	if err != nil {
		return nil, fmt.Errorf("tsnet: local client: %w", err)
	}
	return lc.Status(ctx)
}

// CertDomains returns the magic-DNS names tsnet will serve LE certs
// for. Empty slice before Start.
func (s *Server) CertDomains() []string {
	s.lifecycleMu.Lock()
	server, started := s.server, s.started
	s.lifecycleMu.Unlock()
	if !started || server == nil {
		return nil
	}
	return server.CertDomains()
}

// AuthURL returns the most recent interactive-auth URL captured from
// UserLogf, or empty if none was emitted. Used by `bridge tsnet
// auth` to echo the URL explicitly to the operator's terminal in
// case the slog output was filtered or piped elsewhere.
//
// Read path is on a separate mutex from the lifecycle state so
// AuthURL() polls during a long Up() don't block on Start.
func (s *Server) AuthURL() string {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return s.authURL
}

// Close tears down the embedded tsnet.Server. MUST be called on
// bridge shutdown to drain magicsock / netcheck / control-plane
// goroutines — without it, every Start/Close cycle (e.g. integration
// tests) leaks goroutines until process exit.
func (s *Server) Close() error {
	s.lifecycleMu.Lock()
	server := s.server
	s.server = nil
	s.started = false
	s.lifecycleMu.Unlock()
	if server == nil {
		return nil
	}
	return server.Close()
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
			// Hold authMu only for the write — keep the slog call
			// outside the critical section so a slow log handler
			// can't stall tsnet's internal goroutine.
			s.authMu.Lock()
			s.authURL = u
			s.authMu.Unlock()
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
