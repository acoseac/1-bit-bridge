package admin

import (
	"errors"
	"net/http"
	"net/url"
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
			next.ServeHTTP(w, r)
			return
		}
		guarded.ServeHTTP(w, r)
	})
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
		if s.deps.AdminAuth == nil {
			// Misconfiguration: public mode without an auth store.
			// Refuse rather than letting unauthenticated traffic
			// through. cmd/bridge should refuse-to-start in this
			// state — this is belt-and-braces.
			http.Error(w, "admin refused: auth not configured", http.StatusServiceUnavailable)
			return
		}
		if isAuthBypassPath(r.URL.Path) {
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
	case path == "/metrics":
		// /metrics is gated by its own loopbackOnly wrap at
		// registration (see admin.go), so a same-host scraper needs no
		// session cookie. Without this bypass, a local Prometheus
		// scrape in public mode gets a 302 to /login and breaks.
		return true
	}
	return false
}
