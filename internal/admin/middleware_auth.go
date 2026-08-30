package admin

import (
	"errors"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
)

// boundaryMiddleware enforces the loopback gate ONLY in loopback
// mode. In public mode the gate is bypassed — the admin listener
// is expected to bind a non-loopback interface (the new
// adminauth-backed session middleware takes over as the trust
// boundary).
//
// Per-request dispatch (not Server-construction-time) so a config
// hot-reload of `deployment.mode` takes effect on the next
// request without a Handler rebuild.
func (s *Server) boundaryMiddleware(next http.Handler) http.Handler {
	guarded := loopbackOnly(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.deps.CfgHolder.Load()
		if cfg != nil && cfg.IsPublic() {
			setHSTS(w, r)
			next.ServeHTTP(w, r)
			return
		}
		guarded.ServeHTTP(w, r)
	})
}

// hstsMaxAge is one year in seconds, the value the header is only
// meaningful at: a short max-age leaves a window on every device that
// has not visited recently, which is the window an attacker on the path
// wants.
const hstsMaxAge = 31536000

// setHSTS declares that this origin is HTTPS-only.
//
// Public mode ONLY, and never in loopback: the loopback console is
// plain http://127.0.0.1:7789, and pinning HSTS for `localhost` would
// poison that host name in the operator's browser for every other local
// service they run — an unrelated dev server on 127.0.0.1 would start
// failing, and the fix is buried in chrome://net-internals.
//
// Skipped on a plain-HTTP request even in public mode. A bridge behind
// a TLS-terminating proxy serves the admin console over http on a
// private interface, and a browser ignores HSTS over http anyway — but
// asserting it there would be a claim the bridge cannot back.
//
// No `includeSubDomains` and no `preload`. Both are decisions about a
// whole DNS name that the bridge does not own: it is one service on a
// domain that may host others, and preload is effectively irreversible.
// An operator who wants either should send it from their proxy, where
// the scope is theirs to decide.
func setHSTS(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil {
		return
	}
	w.Header().Set("Strict-Transport-Security",
		"max-age="+strconv.Itoa(hstsMaxAge))
}

// sessionMiddleware enforces a valid admin session for every
// request EXCEPT the bypass list (login routes, static assets,
// favicons). Only active when:
//   - cfg.IsPublic() is true AND
//   - AdminAuth is wired AND
//   - the request is not on the bypass list.
//
// Loopback installs (cfg.IsPublic() == false) skip the gate
// entirely — preserves the historical no-auth contract.
//
// Behaviour on auth failure:
//   - HTML clients (Accept: text/html OR no /api/ prefix) get a
//     302 to /login?next=<safe-encoded-current-path>.
//   - API clients (/api/* path prefix) get a JSON 401.
//
// The `next` parameter is server-side validated via
// IsSafeRelativePath when reconstructing the URL — it's also
// validated again on the login handler's consumption side, so
// double-encoded coercion attempts (`%2F%2Fattacker.com`) get
// rejected by both layers.
func (s *Server) sessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.deps.CfgHolder.Load()
		if cfg == nil || !cfg.IsPublic() {
			next.ServeHTTP(w, r)
			return
		}
		// The bypass check runs BEFORE the auth-configured guard below,
		// and only the probes depend on that ordering.
		//
		// Liveness must not be contingent on the credential file: a
		// bridge in public mode with no admin store is misconfigured,
		// but it is RUNNING, and answering 503 there tells an
		// orchestrator to restart it — which cannot fix a missing file
		// and produces a restart loop instead. readyz reports that
		// state honestly (see Server.ready) so the instance still
		// leaves rotation; it just does not get killed for it.
		//
		// Nothing else moves: the login form, static assets and
		// /metrics are all reached only when an auth store exists, and
		// path.Clean-normalizing first keeps a crafted path from
		// prefix-matching a bypass entry.
		if cp := path.Clean(r.URL.Path); cp == r.URL.Path && isProbePath(cp) {
			next.ServeHTTP(w, r)
			return
		}
		if s.deps.AdminAuth == nil {
			// Misconfiguration: public mode without an auth store.
			// Refuse rather than letting unauthenticated traffic
			// through. cmd/bridge should refuse-to-start in this
			// state — this is belt-and-braces.
			http.Error(w, "admin refused: auth not configured", http.StatusServiceUnavailable)
			return
		}
		// Normalize before the bypass check so a crafted path like
		// /favicon/../api/settings can't prefix-match a bypass entry and skip
		// the session gate. A path that changes under path.Clean carried
		// redundant/traversal segments and never bypasses (falls through to auth).
		if cp := path.Clean(r.URL.Path); cp == r.URL.Path && isAuthBypassPath(cp) {
			next.ServeHTTP(w, r)
			return
		}

		if _, err := s.requireSession(r); err == nil {
			next.ServeHTTP(w, r)
			return
		} else if !errors.Is(err, adminauth.ErrSessionNotFound) && !errors.Is(err, adminauth.ErrSessionExpired) {
			// Internal error — surface as 500 so the operator
			// notices instead of being silently redirected.
			logger.Error("session validation", "err", err)
			http.Error(w, "admin refused: session validation failed", http.StatusInternalServerError)
			return
		}

		// Authentication required. /api/* gets JSON 401; pages
		// get a redirect to /login with the current path
		// preserved as `next`.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "session required")
			return
		}
		// Safe-by-construction redirect URL — IsSafeRelativePath
		// rejects anything that could coerce to an off-origin URL.
		// Renamed from `next` to `redirectTarget` to avoid
		// shadowing the `next http.Handler` parameter at function
		// head (CodeRabbit trivial review post-PR-#292).
		redirectTarget := r.URL.Path
		if r.URL.RawQuery != "" {
			redirectTarget += "?" + r.URL.RawQuery
		}
		if !adminauth.IsSafeRelativePath(redirectTarget) {
			redirectTarget = "/"
		}
		loginURL := "/login?next=" + url.QueryEscape(redirectTarget)
		http.Redirect(w, r, loginURL, http.StatusFound)
	})
}

// isAuthBypassPath returns true for the small set of routes that
// must work without a session (login form itself, static assets,
// favicons, Prometheus metrics). Anything else requires auth in
// public mode.
func isAuthBypassPath(path string) bool {
	switch {
	case path == "/login":
		return true
	case strings.HasPrefix(path, "/static/"):
		return true
	case strings.HasPrefix(path, "/favicon"):
		return true
	case isProbePath(path):
		return true
	case path == "/metrics":
		// /metrics is gated by its own loopbackOnly wrap at
		// registration (see admin.go), so a same-host scraper needs no
		// session cookie. Without this bypass, a local Prometheus
		// scrape in public mode gets a 302 to /login and breaks.
		return true
	}
	return false
}

// isProbePath reports whether a path is an orchestrator probe.
//
// Separate from isAuthBypassPath (which calls it) because the probes
// are bypassed EARLIER than everything else in that set — ahead of the
// auth-configured guard, not just ahead of the session check. Keeping
// the predicate in one place is what stops the two call sites drifting
// into disagreeing about which paths are probes.
//
// An orchestrator holds no session and must not be handed a 302 to
// /login: a redirect reads as "healthy" to most health checkers, which
// is the worst possible answer from a probe. Bypassing the gate is safe
// because these disclose a status code and nothing else.
func isProbePath(path string) bool {
	return path == "/healthz" || path == "/readyz"
}
