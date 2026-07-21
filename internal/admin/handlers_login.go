package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// sessionCookieName is the cookie key carrying the raw session
// token. The browser sends it on every same-origin request; the
// server validates via adminauth.Store.ValidateSession.
const sessionCookieName = "bridge_admin_session"

// loginPageData is the template envelope for login.html. Username
// pre-fills the field (single-user system; not a secret); Next is
// the (sanitised) post-login redirect target.
type loginPageData struct {
	LibraryName   string
	ServerVersion string
	Username      string
	Next          string
}

// pageLogin renders the standalone login form. Bypasses the page
// nav (handled by the login.html template not extending layout).
func (s *Server) pageLogin(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	next := r.URL.Query().Get("next")
	if !adminauth.IsSafeRelativePath(next) {
		next = ""
	}
	username := ""
	if s.deps.AdminAuth != nil {
		username = s.deps.AdminAuth.Username()
	}
	envelope := loginPageData{
		LibraryName:   cfg.LibraryName,
		ServerVersion: version.ServerVersion,
		Username:      username,
		Next:          next,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	// X-Frame-Options: DENY on the login page — prevents the login
	// form from being framed by ANY origin, including same-origin
	// (authenticated pages get the weaker SAMEORIGIN + frame-ancestors
	// 'self' from renderPage in public mode).
	w.Header().Set("X-Frame-Options", "DENY")
	if err := s.loginTmpl.ExecuteTemplate(w, "login", envelope); err != nil {
		logger.Error("render login", "err", err)
	}
}

// loginRequest is the JSON body for POST /login.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Next     string `json:"next,omitempty"`
}

// loginResponse is the success body. The next URL is sanitised
// server-side before being returned to the browser — the client
// MUST NOT trust its own posted `next` value for the redirect
// (defense in depth even though the JS already only navigates to
// the server-returned URL).
type loginResponse struct {
	Username string `json:"username"`
	Next     string `json:"next"`
}

// apiLogin verifies credentials, mints a session, sets the cookie,
// and returns a JSON envelope with the safe redirect target.
//
// Lives behind csrfGuard (which enforces Content-Type:
// application/json + Origin allowlist), so a cross-origin browser
// POST is blocked before reaching here. Rate-limit keyed on
// (clientIP, username) — the limiter is in-process and the
// (clientIP, username) tuple is the bot-resistant key.
//
// Generic error responses ("invalid credentials") — never leak
// which half (wrong user vs wrong password) failed.
func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	if s.deps.AdminAuth == nil || s.deps.LoginLimiter == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_disabled", "admin auth is not configured")
		return
	}
	// Cap the JSON body size; the request shape is tiny and 1 KB
	// is plenty (most usernames + passwords come well below).
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req loginRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "username and password are required")
		return
	}

	cfg := s.deps.CfgHolder.Load()
	clientIP := adminauth.ExtractClientIP(r, cfg.Deployment.AdminTLSTerminatedByProxy)

	if !s.deps.LoginLimiter.AllowAndReserve(clientIP, req.Username) {
		// Slow the attacker without surfacing 429 — they shouldn't
		// learn they've been throttled. Sleep on the handler
		// goroutine; net/http handles request cancellation via the
		// goroutine returning normally.
		//
		// Use NewTimer + defer Stop instead of `time.After`
		// (Gemini medium review on PR #290): `time.After` allocates
		// a Timer that survives until fire even when the context
		// cancels first. On an attacker-controlled, frequently-
		// cancelled path the leaked timers accumulate ~5 s of
		// memory + scheduling state per cancelled attempt.
		timer := time.NewTimer(adminauth.RateLimitDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
		logger.Warn("admin login rate-limited", "ip", clientIP, "username", req.Username)
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid credentials")
		return
	}

	if err := s.deps.AdminAuth.Verify(req.Username, req.Password); err != nil {
		// The attempt was already counted by AllowAndReserve's optimistic
		// reservation above (B43) — do NOT RecordFailure here, or it
		// double-counts. Log the SPECIFIC reason server-side; keep the
		// wire response generic.
		logger.Warn("admin login failed", "ip", clientIP, "username", req.Username, "err", err)
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid credentials")
		return
	}

	raw, err := s.deps.AdminAuth.CreateSession(req.Username)
	if err != nil {
		logger.Error("admin create session", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not create session")
		return
	}
	s.deps.LoginLimiter.RecordSuccess(clientIP, req.Username)
	s.setSessionCookie(w, raw)

	next := "/"
	if adminauth.IsSafeRelativePath(req.Next) {
		next = req.Next
	}
	writeJSON(w, http.StatusOK, loginResponse{Username: req.Username, Next: next})
}

// apiLogout invalidates the current session (if any) and clears
// the cookie. Returns 200 unconditionally — logout against an
// already-expired session is a no-op, not an error.
func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	if s.deps.AdminAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_disabled", "admin auth is not configured")
		return
	}
	if c, err := r.Cookie(sessionCookieName); err == nil {
		s.deps.AdminAuth.DeleteSession(c.Value)
	}
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// setSessionCookie writes the session cookie with the production
// security attributes (HttpOnly, SameSite=Strict, and Secure when
// the bridge thinks it's being served over HTTPS). HostOnly via no
// Domain attribute. Path=/ so the cookie is sent to every admin
// endpoint.
//
// **MaxAge is the SessionHardCap (7d), not SessionIdleTimeout
// (24h)** (Gemini medium review post-PR-#292). The browser
// deletes the cookie at MaxAge regardless of activity; setting
// MaxAge to the idle timeout effectively makes 24h a hard cap
// for the client side, with no way for the server's idle-bump
// behaviour to extend it. Setting MaxAge to the hard cap lets the
// server-side `ValidateSession` enforce the 24h idle window —
// if the operator is active, LastUsedAt bumps; if they walk away
// for >24h, the server returns ErrSessionExpired on the next
// request and triggers a /login redirect. The cookie itself
// survives a closed-tab + reopen within the 7d window.
func (s *Server) setSessionCookie(w http.ResponseWriter, raw string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure(),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(adminauth.SessionHardCap / time.Second),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure(),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// cookieSecure decides whether to set the Secure attribute on the
// session cookie. In public mode the cookie MUST be Secure
// regardless of whether the bridge or a reverse proxy terminates
// TLS — browsers refuse to send Secure cookies over plain HTTP, so
// a misconfigured operator gets a visible login failure instead of
// a silent credential leak.
//
// In loopback mode the cookie path is unreachable in practice
// (sessionMiddleware short-circuits), but keep Secure off for the
// loopback case so the test harness (httptest, plain http://) can
// exercise the flow.
func (s *Server) cookieSecure() bool {
	cfg := s.deps.CfgHolder.Load()
	return cfg.IsPublic()
}

// requireSession enforces an authenticated session. Used by the
// sessionMiddleware as the common gate; not registered as a route
// handler directly. Returns the session info via context for
// downstream handlers that want to log the operator's identity.
func (s *Server) requireSession(r *http.Request) (*adminauth.Session, error) {
	if s.deps.AdminAuth == nil {
		return nil, errors.New("admin auth not configured")
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil, adminauth.ErrSessionNotFound
	}
	return s.deps.AdminAuth.ValidateSession(c.Value)
}
