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
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

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
	fingerprint string
	startedAt   time.Time
}

// ManifestProvider is the interface /v1/manifest and /v1/health use to
// read the indexed library state. internal/manifest implements it via
// the Scanner + Store pair; tests can pass a small stub.
type ManifestProvider interface {
	BuildManifest(since time.Time) (any, error)
	IsScanning() bool
	LastFullScan() time.Time
	TracksIndexed() int
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
	return protocolHeader(mux)
}

// HealthResponse is the /v1/health JSON body. Field ordering must stay
// stable because the iOS side golden-decodes this shape (see PROTOCOL.md).
type HealthResponse struct {
	ProtocolVersion int       `json:"protocolVersion"`
	ServerVersion   string    `json:"serverVersion"`
	LibraryName     string    `json:"libraryName"`
	LibraryRoots    []string  `json:"libraryRoots"`
	CertFingerprint string    `json:"certFingerprint"`
	StartedAt       time.Time `json:"startedAt"`
	ScanState       ScanState `json:"scanState"`
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
	}
	writeJSON(w, http.StatusOK, resp)
}

// manifestHandler serves GET /v1/manifest?since=<rfc3339> with the current
// library index (or a since-filtered delta). Returns 503 if no manifest
// provider is wired up yet (early boot / test harness).
func (s *Server) manifestHandler(w http.ResponseWriter, r *http.Request) {
	if s.manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "scan_in_progress", "manifest not ready")
		return
	}
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		parsed, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request",
				"since must be an RFC3339 timestamp, got "+v)
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
		w.Header().Set("X-Bridge-Protocol", fmt.Sprintf("%d", version.ProtocolVersion))
		next.ServeHTTP(w, r)
	})
}

// authed wraps a handler so it only runs if the request carries a valid
// bearer token. Unauthenticated requests return a 401 JSON error. The
// matched Token is passed to the wrapped handler via the request context
// so downstream handlers can log which client they're serving.
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := extractBearer(r)
		if raw == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		if _, ok := s.store.Validate(raw); !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
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
