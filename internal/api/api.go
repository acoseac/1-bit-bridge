// Package api implements the HTTP/2 handlers for the v1 wire protocol (see
// PROTOCOL.md at the repo root).
//
// v1 endpoints in this package so far:
//
//	GET /v1/health     — no auth, liveness / pairing probe
//
// Upcoming: /v1/list, /v1/stat, /v1/read, /v1/download, /v1/manifest.
//
// Every response carries the X-Bridge-Protocol header. Authenticated
// endpoints run through the authed() middleware, which requires
// Authorization: Bearer <token> and validates via the auth.Store.
package api

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Server owns the http.Handler and the per-request state it needs.
type Server struct {
	cfg         *config.Config
	store       *auth.Store
	resolver    *bridgefs.Resolver
	manifest    ManifestProvider
	artworkDirs ArtworkDirProvider
	mbidProbe   MBIDProbe
	updater     UpdaterStatus
	fingerprint string
	startedAt   time.Time
}

// ManifestProvider is the interface /v1/manifest and /v1/health use to
// read the indexed library state. internal/manifest implements it via
// the Scanner + Store pair; tests can pass a small stub.
type ManifestProvider interface {
	BuildManifest(since time.Time) (any, error)
	// BuildManifestPage is the v1.1 paginated-manifest variant used
	// when the client asks for `?limit=`. `cursor=""` requests the
	// first page. Callers iterate until the returned page's
	// `NextCursor` is nil. Implementations return the same envelope
	// shape as BuildManifest so the JSON-serialization paths can be
	// shared in the handler.
	BuildManifestPage(cursor string, limit int) (any, error)
	IsScanning() bool
	LastFullScan() time.Time
	TracksIndexed() int
}

// MBIDProbe is an optional interface the artwork + artist-image
// handlers use to distinguish "MBID known to the server but not
// cached yet" (return 202 + Retry-After so iOS retries) from
// "genuinely unknown MBID" (return 404 so iOS treats as terminal).
//
// Nil-safe — when `s.mbidProbe` is nil the handlers fall back to the
// pre-v1.1 behaviour of 404-on-miss. `internal/manifest.Provider`
// satisfies this interface in production.
type MBIDProbe interface {
	HasTrackWithArtworkMBID(mbid string) bool
	HasTrackWithArtistMBID(mbid string) bool
}

// UpdaterStatus is the optional interface the /v1/health handler uses
// to populate the additive `latestServerVersion` / `updateAvailable` /
// `updateReleaseNotesURL` / `minClientVersion` fields. Nil-safe — when
// `s.updater` is nil those fields stay omitempty-empty and the wire
// shape matches the pre-Phase-A response exactly.
//
// `internal/updater.Updater` satisfies this interface in production.
// The `MinClientVersion` returned here is read from
// `version.MinClientVersion` so it advertises the floor THIS bridge
// needs from its iOS clients (not the floor a candidate update would
// require — that's a Phase B / C concern).
type UpdaterStatus interface {
	UpdateInfo() UpdateInfo
}

// UpdateInfo is the wire shape /v1/health embeds. Lives in this
// package (not internal/updater) so the iOS-facing fields don't import
// the updater type machinery and tests can hand-craft an UpdaterStatus
// stub.
type UpdateInfo struct {
	LatestVersion    string
	UpdateAvailable  bool
	ReleaseNotesURL  string
	MinClientVersion string
}

// New constructs a Server. fingerprint is the SHA-256 of the TLS cert, used
// for display in /v1/health (iOS pins by this value). mp can be nil during
// early boot / tests — /v1/manifest will return 503 until it's populated.
func New(cfg *config.Config, store *auth.Store, mp ManifestProvider, fingerprint string) *Server {
	return &Server{
		cfg:         cfg,
		store:       store,
		resolver:    bridgefs.New(cfg.LibraryRoots),
		manifest:    mp,
		fingerprint: fingerprint,
		startedAt:   time.Now().UTC(),
	}
}

// WithArtworkDirs attaches an artwork cache dir provider. Separate from
// New to avoid churning every call site when optional features are added.
func (s *Server) WithArtworkDirs(ad ArtworkDirProvider) *Server {
	s.artworkDirs = ad
	return s
}

// WithMBIDProbe attaches an MBIDProbe so the artwork / artist-image
// handlers can return 202 + Retry-After for known-but-uncached MBIDs
// instead of a flat 404. Optional — omitting it preserves v1.0
// behaviour (404 on every cache miss). `internal/manifest.Provider`
// satisfies the interface in production.
func (s *Server) WithMBIDProbe(p MBIDProbe) *Server {
	s.mbidProbe = p
	return s
}

// WithUpdater attaches an UpdaterStatus so /v1/health can advertise
// the latest available bridge release to iOS clients. Optional —
// omitting it preserves the pre-Phase-A wire shape (the four new
// `omitempty` fields stay absent from the JSON response).
func (s *Server) WithUpdater(u UpdaterStatus) *Server {
	s.updater = u
	return s
}

// Resolver returns the internal path Resolver so the admin console can
// call SetRoots when adding or removing a library root at runtime. The
// api handlers and admin handlers deliberately share the same Resolver
// instance — otherwise a hot add/remove would update one without the
// other and /v1/list would serve stale top-level entries.
func (s *Server) Resolver() *bridgefs.Resolver { return s.resolver }

// Handler returns the root http.Handler, pre-wrapped with the
// X-Bridge-Protocol middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("GET /v1/list", s.authed(s.list))
	mux.HandleFunc("GET /v1/stat", s.authed(s.stat))
	mux.HandleFunc("GET /v1/read", s.authed(s.read))
	mux.HandleFunc("GET /v1/download", s.authed(s.download))
	mux.HandleFunc("GET /v1/manifest", s.authed(s.manifestHandler))
	mux.HandleFunc("GET /v1/artwork/{mbid}", s.authed(s.artwork))
	mux.HandleFunc("GET /v1/artist-image/{mbid}", s.authed(s.artistImage))
	return protocolHeader(mux)
}

// HealthResponse is the /v1/health JSON body. Field ordering must stay
// stable because the iOS side golden-decodes this shape (see PROTOCOL.md).
//
// `Endpoints` is the additive v1 extension that lets iOS self-discover
// LAN ↔ Tailscale alternates without re-pairing. Empty when the bridge
// can't enumerate interfaces (e.g. test harness) — iOS falls back to
// the URL it was paired on. Adding fields here is backwards-compatible:
// the iOS decoder uses Codable with default values for anything it
// doesn't know about.
//
// The four `*Update*` / `MinClientVersion` fields are the Phase A
// additive extension (see PROTOCOL.md "Updates" section). They are
// only populated when an UpdaterStatus has been wired via WithUpdater
// AND the updater has at least one successful poll cached; absent
// otherwise so the wire shape stays identical to the pre-update
// response on a bridge that doesn't yet have an updater. iOS treats
// all four as optional and falls back to "no update info" rather
// than a hard error if any are missing.
type HealthResponse struct {
	ProtocolVersion       int       `json:"protocolVersion"`
	ServerVersion         string    `json:"serverVersion"`
	LibraryName           string    `json:"libraryName"`
	LibraryRoots          []string  `json:"libraryRoots"`
	CertFingerprint       string    `json:"certFingerprint"`
	StartedAt             time.Time `json:"startedAt"`
	ScanState             ScanState `json:"scanState"`
	Endpoints             []string  `json:"endpoints,omitempty"`
	LatestServerVersion   string    `json:"latestServerVersion,omitempty"`
	UpdateAvailable       bool      `json:"updateAvailable,omitempty"`
	UpdateReleaseNotesURL string    `json:"updateReleaseNotesURL,omitempty"`
	MinClientVersion      string    `json:"minClientVersion,omitempty"`
}

// ScanState reports the scanner's current status. Real fields populate once
// the manifest package lands; the shape is stubbed in for iOS decoder
// stability.
type ScanState struct {
	IsScanning    bool      `json:"isScanning"`
	LastFullScan  time.Time `json:"lastFullScan,omitempty"`
	TracksIndexed int       `json:"tracksIndexed"`
}

// ErrorResponse matches the shape documented in PROTOCOL.md ("short-code" +
// human detail). Keep this struct in sync with the error table there.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	scanState := ScanState{}
	if s.manifest != nil {
		scanState.IsScanning = s.manifest.IsScanning()
		scanState.LastFullScan = s.manifest.LastFullScan()
		scanState.TracksIndexed = s.manifest.TracksIndexed()
	}
	resp := HealthResponse{
		ProtocolVersion: version.ProtocolVersion,
		ServerVersion:   version.ServerVersion,
		LibraryName:     s.cfg.LibraryName,
		LibraryRoots:    libraryRootBasenames(s.cfg.LibraryRoots),
		CertFingerprint: s.fingerprint,
		StartedAt:       s.startedAt,
		ScanState:       scanState,
		Endpoints:       s.reachableEndpoints(),
	}
	if s.updater != nil {
		info := s.updater.UpdateInfo()
		resp.LatestServerVersion = info.LatestVersion
		resp.UpdateAvailable = info.UpdateAvailable
		resp.UpdateReleaseNotesURL = info.ReleaseNotesURL
		resp.MinClientVersion = info.MinClientVersion
	}
	writeJSON(w, http.StatusOK, resp)
}

// reachableEndpoints enumerates LAN + mDNS + Tailscale URLs for the
// bridge on every /v1/health call. Fresh on each call so adding /
// removing a network interface (Tailscale up, Wi-Fi down) takes effect
// on the next heartbeat without requiring a restart. Cost is a
// `net.Interfaces()` + `.Addrs()` walk — cheap enough to not warrant
// caching.
func (s *Server) reachableEndpoints() []string {
	_, portStr, err := net.SplitHostPort(s.cfg.ListenAddress)
	if err != nil {
		return nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return nil
	}
	return advertise.URLs(advertise.Params{Port: port})
}

// manifestHandler serves GET /v1/manifest?since=<rfc3339> with the current
// library index (or a since-filtered delta), OR — if `?limit=` is set —
// one page of a paginated full-manifest walk driven by `?cursor=`.
// Returns 503 if no manifest provider is wired up yet (early boot /
// test harness).
//
// Query parameter combinations:
//   - no params               → full manifest, v1.0 behaviour
//   - ?since=<rfc3339>        → since-delta (never paginated, by
//     construction small)
//   - ?limit=N[&cursor=<opq>] → paginated full manifest (v1.1). Empty
//     cursor requests the first page; the
//     caller iterates until NextCursor is nil.
//
// `?limit=` + `?since=` together returns 400 — mixing the two would
// need a composite (timestamp, path) cursor for ordering stability,
// and since-deltas are bounded anyway.
func (s *Server) manifestHandler(w http.ResponseWriter, r *http.Request) {
	if s.manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "scan_in_progress", "manifest not ready")
		return
	}
	q := r.URL.Query()
	sinceRaw := q.Get("since")
	limitRaw := q.Get("limit")

	if sinceRaw != "" && limitRaw != "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"`since` and `limit` are mutually exclusive; pagination applies to full-manifest requests only")
		return
	}

	// Paginated path.
	if limitRaw != "" {
		limit, err := strconv.Atoi(limitRaw)
		if err != nil || limit <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request",
				"limit must be a positive integer, got "+limitRaw)
			return
		}
		// Cap the page size at a reasonable ceiling so a client
		// requesting `limit=10000000` can't allocate a huge response
		// that DoSes the server or the iOS side's JSON decoder.
		const maxPageLimit = 5000
		if limit > maxPageLimit {
			limit = maxPageLimit
		}
		cursor := q.Get("cursor")
		body, err := s.manifest.BuildManifestPage(cursor, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, body)
		return
	}

	// Legacy single-shot path (full manifest or since-delta).
	var since time.Time
	if sinceRaw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, sinceRaw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request",
				"since must be an RFC3339 timestamp, got "+sinceRaw)
			return
		}
		since = parsed
	}
	body, err := s.manifest.BuildManifest(since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// libraryRootBasenames returns just the last path component for each
// configured root. We deliberately don't ship the full server-side absolute
// path to the iOS client — paths are library-relative on the wire, and the
// basename is enough context for the UI to show "Music", "Classical", etc.
func libraryRootBasenames(roots []string) []string {
	out := make([]string, len(roots))
	for i, r := range roots {
		out[i] = filepath.Base(r)
	}
	return out
}

// protocolHeader injects the X-Bridge-Protocol header on every response so
// the iOS side can catch version mismatches at handshake time without
// parsing the body.
func protocolHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Bridge-Protocol", strconv.Itoa(version.ProtocolVersion))
		next.ServeHTTP(w, r)
	})
}

// authed wraps a handler so it only runs if the request carries a valid
// bearer token. Unauthenticated requests return a 401 JSON error.
//
// On a successful Validate, the matched token's ID is fed into
// auth.Store.RecordClientVersion alongside the request's
// X-Client-Version header so the updater can later refuse auto-installs
// that would orphan an old iOS client. The pre-check against
// `tok.LastClientVersion` (cheap — `Validate` returns a copy under
// the same mutex it took for LastUsedAt) skips the whole lock-+
// linear-scan in the common case where the version hasn't changed
// since last request.
//
// The matched Token is otherwise not propagated to the wrapped handler
// — if a future endpoint needs to know which client it's serving,
// thread it through the request context here at that point.
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := extractBearer(r)
		if raw == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		tok, ok := s.store.Validate(raw)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
		}
		if cv := r.Header.Get("X-Client-Version"); cv != "" && cv != tok.LastClientVersion {
			s.store.RecordClientVersion(tok.ID, cv)
		}
		next(w, r)
	}
}

// extractBearer returns the token from an "Authorization: Bearer <token>"
// header, or "" if absent/malformed. The "Bearer" prefix is matched
// case-insensitively to match common client behavior.
func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "bearer "
	if len(h) < len(prefix) {
		return ""
	}
	if !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// writeJSON serializes v and writes it with application/json + the given
// status code. Call this for all success responses.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// writeError serializes an ErrorResponse with the given status + short code
// and human-readable message. Matches the table in PROTOCOL.md.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{Error: code, Message: message})
}
