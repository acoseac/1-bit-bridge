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

// maxLoginUsernameLen bounds the username a login attempt may carry.
//
// The value is attacker-controlled and is concatenated verbatim into
// the rate limiter's map key (adminauth.bucketKey), so without a cap
// the limiter's memory bound is stated in ENTRIES while an attacker
// controls the BYTES: maxBuckets (10 000) keys × the ~4 KB the body
// reader allows ≈ 40 MB pinned until the hourly janitor sweep, against
// the ~1.2 MB the maxBuckets docstring budgets for. Unauthenticated,
// and it composes with the eviction spray the two-tier scan defends.
//
// The bound also covers the two logger.Warn lines below, which echo
// req.Username verbatim: uncapped, every rejected attempt writes up to
// ~4 KB of attacker-chosen text into the operator's log.
//
// Bounded here rather than inside bucketKey so the client gets a clean
// 400 instead of a silently-truncated key, and so nothing over-long
// reaches the limiter or the log at all. 64 bytes is well past any real
// admin username (the store is single-user; the field pre-fills from
// Store.Username()). Bytes, not runes — the memory is what's at stake.
const maxLoginUsernameLen = 64

// loginPageData is the template envelope for login.html. Username
// pre-fills the field (single-user system; not a secret); Next is
// the (sanitised) post-login redirect target.
type loginPageData struct {
	LibraryName   string
	ServerVersion string
	Username      string
	Next          string
	// Notice is a neutral explanation shown above the form, currently only for
	// a login link that did not redeem. Empty renders nothing.
	Notice string
}

// staleLinkNotice is what a user sees after a login link fails to redeem.
//
// It names the two recoverable causes together, which is exactly what
// ErrTicketInvalid means and therefore leaks nothing about whether a given
// ticket ever existed. The instruction is the point: the natural response to a
// login form is to click the link again, and that can never work.
// The instruction stays SOURCE-NEUTRAL. These links are minted from two places
// — the hosted uploader's share sheet and `bridge admin login-link` in a shell
// — and naming either one tells the other half of the operators to go somewhere
// that does not exist for them.
const staleLinkNotice = "That sign-in link has expired or was already used — " +
	"links are single-use. Request a new one to sign in."

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
	notice := ""
	if r.URL.Query().Get("link") == "stale" {
		notice = staleLinkNotice
	}
	envelope := loginPageData{
		LibraryName:   cfg.LibraryName,
		ServerVersion: version.ServerVersion,
		Username:      username,
		Next:          next,
		Notice:        notice,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	// Framing guard on the login page — DENY + frame-ancestors 'none'
	// (legacy + modern primitives) refuse embedding by ANY origin,
	// including same-origin, in BOTH modes (authenticated pages get
	// the weaker SAMEORIGIN + frame-ancestors 'self' from renderPage,
	// and only in public mode).
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
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
		writeError(w, http.StatusServiceUnavailable, "auth_disabled", msgAuthNotConfigured)
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
	// Bound BEFORE the value reaches the rate limiter — see
	// maxLoginUsernameLen. Not a credentials check, so it's a 400 and
	// not the generic 401: refusing a structurally invalid request
	// discloses nothing about which account exists.
	if len(req.Username) > maxLoginUsernameLen {
		writeError(w, http.StatusBadRequest, "bad_request", "username is too long")
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

// msgAuthNotConfigured is the one wording for "this build has no admin auth",
// used by every handler that needs it.
const msgAuthNotConfigured = "admin auth is not configured"

// loginTicketPageData feeds login_ticket.html — the interstitial a login
// link lands on before anything is redeemed.
type loginTicketPageData struct {
	LibraryName   string
	ServerVersion string
	// Ticket is the raw credential, carried ONLY as the query of the form's
	// POST action — never in a link, never in a script, never logged.
	Ticket string
}

// loginTicketReferrerPolicy is the referrer policy both halves of the
// login-link flow serve, and the template's `<meta name="referrer">` MUST
// carry the same value — the meta is parsed after the header and is what the
// document ends up with.
//
// `strict-origin`, NOT `no-referrer`, and the difference is the whole
// uploader link (2026-09-12, the second field report on it, one build after
// the first): the ticket travels in this page's URL, so the referrer policy
// exists to keep that address out of every onward request. `no-referrer` does
// that — and ALSO turns the interstitial's own button into a request the CSRF
// guard refuses. Per the Fetch standard ("append a request `Origin` header"),
// a non-GET request whose mode is not `cors` — a plain form submission, i.e.
// a navigation — serializes its `Origin` as `null` when the document's
// referrer policy is `no-referrer`. Safari on both iOS and macOS did exactly
// that: Continue arrived as `POST /login/ticket` with `Origin: null`,
// originMatchesAdmin refused it, and the phone saved the 36-byte
// "admin refused: cross-origin request" as `ticket.txt`. The console's own
// requests never showed it because `fetch()` defaults to `cors` mode, which
// that rule exempts — this page is the first plain form the admin serves
// whose submission has to pass the Origin allowlist (the login form two
// functions up serves `same-origin` and never hit it either).
//
// Under `strict-origin` the same rule leaves the Origin intact (it nulls only
// on an https→http downgrade, which the redemption is not), and the Referer
// carries the ORIGIN alone — never the path, so the ticket still reaches no
// onward request, same-origin subresource fetches included, which is a
// stricter version of what `no-referrer` was chosen for. Pinned by
// TestLoginTicketRedeemsUnderTheOriginABrowserSends, which derives the Origin
// from the served policy the way the spec does and submits it.
const loginTicketReferrerPolicy = "strict-origin"

// setLoginTicketHeaders is the header set both halves of the login-link flow
// share. The ticket travels in a URL, so nothing between the browser and the
// bridge may cache the exchange, and no onward request may carry the address
// as a referrer — see loginTicketReferrerPolicy for why that is
// `strict-origin` and not `no-referrer`.
func setLoginTicketHeaders(w http.ResponseWriter) {
	w.Header().Set("Referrer-Policy", loginTicketReferrerPolicy)
	w.Header().Set("Cache-Control", "no-store")
}

// pageLoginTicket is the GET half of a login link: it renders a one-button
// interstitial and REDEEMS NOTHING.
//
// The link exists so the hosted control plane can open an authenticated
// console for the account that owns a tenant, without the user transcribing a
// generated password — and so a self-hosting operator can do the same from a
// shell with `bridge admin login-link`. The ticket travels in a URL, which is
// why it is single-use and short-lived (adminauth.LoginTicketTTL by default).
//
// It used to redeem on this GET, and the guards below — HEAD refused, a
// declared prefetch refused — only cover the probers that SAY what they are.
// The one that spent the ticket in the field said nothing of the sort: a link
// PREVIEW. iOS's share sheet, Messages on both ends, Slack, a mail client —
// each loads the URL through a real browser engine to draw a card, and that
// load declares itself a navigation (`Sec-Fetch-Mode: navigate`, a Safari UA),
// exactly like the human's click that follows it. Redeem-on-GET cannot tell
// the two apart, so the credential was spent while the share sheet was still
// opening, and the human landed on `/login?link=stale` every time
// (2026-09-12, the hosted uploader link). What a previewer never does is press
// a button: the GET renders a form whose POST is the redemption, and the
// human's one click is what spends the ticket.
//
// Nothing here touches the ticket store, for the same reason the miss branch
// was un-oracled: an unauthenticated GET that answered "is this ticket live"
// would be one, and it would put a file read under the mutex every
// authenticated console request takes. A ticket that never existed gets the
// same page as a live one; the POST is where the answer is.
func (s *Server) pageLoginTicket(w http.ResponseWriter, r *http.Request) {
	setLoginTicketHeaders(w)
	// Go's ServeMux matches a "GET " pattern for HEAD as well
	// (net/http/server.go: "a pattern with the method GET matches both GET
	// and HEAD requests"). Nothing is consumed on a GET any more, so a HEAD
	// could be answered — but 405 is the honest reply to a prober asking
	// about the method, and it keeps the page off the shapes a scanner
	// harvests.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"a login link must be opened with GET")
		return
	}
	if s.deps.AdminAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_disabled", msgAuthNotConfigured)
		return
	}
	// A request that POSITIVELY declares itself a prefetch or a subresource
	// gets nothing to render — not for the ticket's sake (the GET spends
	// nothing now) but so a speculative loader does not warm a page it will
	// never show. Fails OPEN when the headers are absent, as before.
	if isNonNavigationFetch(r.Header) {
		writeError(w, http.StatusForbidden, "not_a_navigation",
			"a login link must be opened by navigating to it")
		return
	}
	ticket := r.URL.Query().Get("t")
	if ticket == "" {
		// Nothing to continue with. The same page the POST would send a bad
		// ticket to — a link with no ticket is a stale link's shape.
		http.Redirect(w, r, "/login?link=stale", http.StatusFound)
		return
	}
	cfg := s.deps.CfgHolder.Load()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Framing guard, as on the login form: this page carries a button that
	// spends a credential, so no origin may embed it.
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	envelope := loginTicketPageData{
		LibraryName:   cfg.LibraryName,
		ServerVersion: version.ServerVersion,
		Ticket:        ticket,
	}
	if err := s.loginTmpl.ExecuteTemplate(w, "login_ticket", envelope); err != nil {
		logger.Error("render login ticket page", "err", err)
	}
}

// apiRedeemLoginTicket is the POST half: the interstitial's one button. It
// exchanges the ticket for a console session and redirects, so the address
// bar never keeps a working credential and a reload cannot replay one.
//
// The form posts an EMPTY body with the ticket in the action's query —
// csrfGuard admits a bodiless POST without a Content-Type check, and a
// cross-site page cannot forge this request without the ticket itself, which
// is the whole secret. The Origin allowlist still applies.
func (s *Server) apiRedeemLoginTicket(w http.ResponseWriter, r *http.Request) {
	setLoginTicketHeaders(w)
	if s.deps.AdminAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_disabled", msgAuthNotConfigured)
		return
	}
	// A POST is not something a previewer or a prefetcher issues, but the
	// guard is free and keeps the two halves' refusals identical.
	if isNonNavigationFetch(r.Header) {
		writeError(w, http.StatusForbidden, "not_a_navigation",
			"a login link must be opened by navigating to it")
		return
	}
	username, err := s.deps.AdminAuth.RedeemLoginTicket(r.URL.Query().Get("t"))
	if err != nil {
		if !errors.Is(err, adminauth.ErrTicketInvalid) {
			// The ticket store could not be written — a full or read-only
			// disk, not anything the holder of this link did. Redemption
			// established NOTHING about the ticket, and the record is still on
			// disk, so `link=stale` here would be false twice over: it names a
			// cause that was never determined, and its advice is to fetch a
			// fresh link, which will fail in exactly the same way. This branch
			// is also the only signal an operator would get that the store has
			// stopped being writable.
			logger.Error("admin redeem login ticket", "err", err)
			writeError(w, http.StatusInternalServerError, "ticket_store_unavailable",
				"the login-ticket store could not be read or written")
			return
		}
		// Still no explanation of WHICH of unknown, expired or already-used
		// applies — but `link=stale` lets the form say that the link was the
		// problem, which is the difference between "this is broken" and "get a
		// fresh one". It reveals nothing: it is the exact union of the three,
		// and it is the same answer for a ticket that never existed.
		//
		// Load-bearing, because the obvious recovery is futile: re-opening the
		// SAME link can never work once any touch has spent it, and without
		// this the page gives a user no reason to think otherwise.
		http.Redirect(w, r, "/login?link=stale", http.StatusFound)
		return
	}
	raw, err := s.deps.AdminAuth.CreateSession(username)
	if err != nil {
		logger.Error("admin create session from ticket", "err", err)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.setSessionCookie(w, raw)
	// Always the console root. A `next` parameter was considered and removed:
	// validating it would make this the only handler passing a caller-supplied
	// value to http.Redirect, and on a login path the safer answer is to have
	// nothing to validate. Nothing needs it — the control plane wants the root,
	// and the login form has its own.
	http.Redirect(w, r, "/", http.StatusFound)
}

// isNonNavigationFetch reports whether a request POSITIVELY declares itself
// something other than a user navigation — a prefetch, a prerender, a preview
// or a subresource fetch. Note what it cannot see: a link preview drawn by a
// real browser engine declares itself a navigation, which is why redemption
// moved off the GET altogether (pageLoginTicket).
//
// Absent headers are not a declaration, so they pass. `Sec-Fetch-Mode` is sent
// by every current browser on a top-level navigation; the rest are the older
// and vendor spellings of "I am fetching this speculatively".
func isNonNavigationFetch(h http.Header) bool {
	if m := h.Get("Sec-Fetch-Mode"); m != "" && !strings.EqualFold(m, "navigate") {
		return true
	}
	for _, k := range []string{"Sec-Purpose", "Purpose", "X-Purpose", "X-Moz"} {
		if declaresSpeculation(h.Get(k)) {
			return true
		}
	}
	return false
}

// declaresSpeculation reports whether a purpose-style header value carries a
// token naming a speculative fetch.
//
// TOKENS, not the whole value and not a substring. `Sec-Purpose` is a
// structured field that really does arrive as `prefetch;anonymous-client-ip`,
// and the three legacy spellings are specified nowhere at all, so any of them
// can pick up a parameter or arrive as a list in front of a proxy. Matching the
// whole value misses those and fails OPEN — which spends the ticket this guard
// exists to protect. A bare substring match would close that hole and open a
// worse one, refusing a real navigation whose value merely contained one of
// these words.
//
// Split on both `,` and `;` so a list and a parameterised single value are the
// same shape, and strip the quotes a structured-field string may carry.
func declaresSpeculation(v string) bool {
	for _, tok := range strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ';'
	}) {
		switch strings.ToLower(strings.Trim(tok, " \t\"")) {
		case "prefetch", "prerender", "preview", "instant":
			return true
		}
	}
	return false
}

// apiLogout invalidates the current session (if any) and clears
// the cookie. Returns 200 unconditionally — logout against an
// already-expired session is a no-op, not an error.
func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	if s.deps.AdminAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_disabled", msgAuthNotConfigured)
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
