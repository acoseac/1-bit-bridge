package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// --- response shapes ---

type statsResponse struct {
	LibraryName     string    `json:"libraryName"`
	ProtocolVersion int       `json:"protocolVersion"`
	ServerVersion   string    `json:"serverVersion"`
	UptimeSec       int64     `json:"uptimeSec"`
	StartedAt       time.Time `json:"startedAt"`
	TracksIndexed   int       `json:"tracksIndexed"`
	IsScanning      bool      `json:"isScanning"`
	ScanProgress    int64     `json:"scanProgress"`
	LastFullScan    time.Time `json:"lastFullScan,omitempty"`
	DBBytes         int64     `json:"dbBytes"`
	Fingerprint     string    `json:"fingerprint"`
	DeviceCount     int       `json:"deviceCount"`
	ListenAddress   string    `json:"listenAddress"`
	AdminAddress    string    `json:"adminAddress"`
}

type rootRow struct {
	Path   string `json:"path"`
	Tracks int    `json:"tracks"`
}

type tokenRow struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt,omitempty"`
}

type pairResult struct {
	RawToken    string `json:"rawToken"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	URL         string `json:"url"`       // operator-supplied bridge URL for iOS
	PairURL     string `json:"pairURL"`   // bridge://pair?... for QR
	QRDataURL   string `json:"qrDataURL"` // data:image/png;base64,... rendered QR
}

type settingsResponse struct {
	LibraryName     string `json:"libraryName"`
	ListenAddress   string `json:"listenAddress"`
	AdminAddress    string `json:"adminAddress"`
	DataDir         string `json:"dataDir"`
	ScanIntervalSec int    `json:"scanIntervalSec"`
	TLSCertPath     string `json:"tlsCertPath,omitempty"`
	TLSKeyPath      string `json:"tlsKeyPath,omitempty"`
}

// --- GET /api/stats ---

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	tracks, _ := s.deps.Manifest.CountTracks()
	dbBytes := dbSize(filepath.Join(s.deps.Cfg.DataDir, "bridge.db"))

	writeJSON(w, http.StatusOK, statsResponse{
		LibraryName:     s.deps.Cfg.LibraryName,
		ProtocolVersion: version.ProtocolVersion,
		ServerVersion:   version.ServerVersion,
		UptimeSec:       int64(now.Sub(s.deps.StartedAt).Seconds()),
		StartedAt:       s.deps.StartedAt,
		TracksIndexed:   tracks,
		IsScanning:      s.deps.Scanner.IsScanning(),
		ScanProgress:    s.deps.Scanner.ScanProgress(),
		LastFullScan:    s.deps.Scanner.LastFullScan(),
		DBBytes:         dbBytes,
		Fingerprint:     s.deps.Fingerprint,
		DeviceCount:     len(s.deps.Auth.List()),
		ListenAddress:   s.deps.Cfg.ListenAddress,
		AdminAddress:    s.deps.Cfg.AdminAddress,
	})
}

// --- POST /api/scan ---

func (s *Server) apiScan(w http.ResponseWriter, r *http.Request) {
	// Scanner serializes concurrent Scan() via its own mu; we fire and
	// forget. If one is already running, this call blocks the goroutine
	// (not the HTTP response) until the running scan finishes, then
	// starts a fresh walk. Uses the admin's scan context (tied to server
	// shutdown) rather than r.Context so a client disconnect doesn't kill
	// the rescan mid-walk.
	ctx := s.scanCtx()
	go func() {
		if _, err := s.deps.Scanner.Scan(ctx); err != nil {
			if !errors.Is(err, ctx.Err()) {
				fmt.Fprintf(os.Stderr, "admin: triggered scan: %v\n", err)
			}
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

// --- GET /api/roots ---

func (s *Server) apiRootsList(w http.ResponseWriter, r *http.Request) {
	roots := s.deps.Scanner.Roots()
	multi := len(roots) > 1
	out := make([]rootRow, 0, len(roots))
	for _, root := range roots {
		var n int
		if multi {
			n, _ = s.deps.Manifest.CountTracksByPrefix(filepath.Base(root) + "/")
		} else {
			n, _ = s.deps.Manifest.CountTracks()
		}
		out = append(out, rootRow{Path: root, Tracks: n})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- POST /api/roots {path} ---

func (s *Server) apiRootsAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad-json", err.Error())
		return
	}
	req.Path = strings.TrimSpace(req.Path)
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "path-required", "path must not be empty")
		return
	}
	abs, err := filepath.Abs(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad-path", err.Error())
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		writeError(w, http.StatusBadRequest, "not-found", err.Error())
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, "not-a-dir", abs+" is not a directory")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.deps.Scanner.Roots()
	if slices.Contains(current, abs) {
		writeError(w, http.StatusConflict, "already-exists", "root already configured")
		return
	}
	// Basename-collision check — iOS routes multi-root by basename so two
	// roots with the same final path segment would mask each other.
	newList := append(append([]string(nil), current...), abs)
	if err := bridgefs.ValidateRoots(newList); err != nil {
		writeError(w, http.StatusConflict, "basename-collision", err.Error())
		return
	}

	// Apply: a single↔multi transition changes the stored path form
	// (bare "Artist/…" vs "<basename>/Artist/…"), so the manifest has
	// to be wiped and re-populated from a fresh scan.
	//
	// Commit order matches apiRootsRemove: run the destructive
	// manifest op FIRST, persist the root list only after it succeeds.
	// The reverse (save config → wipe tracks) can leave disk in a
	// state where the config advertises the new root but the manifest
	// still serves the previous form's tracks — the next restart
	// would serve phantom paths. With this order, a wipe failure
	// means the config file is untouched; a `Cfg.Save` failure after
	// a successful wipe means the next scan simply re-populates —
	// every failure window lands in a state the scanner can heal.
	willTransition := len(current) == 1 // 1 → N: storage form flips
	if willTransition {
		if err := s.deps.Manifest.WipeAllTracks(); err != nil {
			writeError(w, http.StatusInternalServerError, "wipe-tracks", err.Error())
			return
		}
	}
	// Snapshot prev roots before mutating so we can roll back on a
	// failed Save. Mirrors the apiRootsRemove path: without the
	// rollback, a failed Save leaves in-memory holding the new list
	// while disk has the old — and any later successful Save from an
	// unrelated edit (library-name change, bind-address change, etc.)
	// silently commits the addition the operator had seen fail.
	prevRoots := append([]string(nil), s.deps.Cfg.LibraryRoots...)
	s.deps.Cfg.LibraryRoots = newList
	if err := s.deps.Cfg.Save(s.deps.CfgPath); err != nil {
		s.deps.Cfg.LibraryRoots = prevRoots
		writeError(w, http.StatusInternalServerError, "save-config", err.Error())
		return
	}
	s.deps.Scanner.SetRoots(newList)
	s.deps.Resolver.SetRoots(newList)
	go func() { _, _ = s.deps.Scanner.Scan(s.scanCtx()) }()
	writeJSON(w, http.StatusCreated, rootRow{Path: abs, Tracks: 0})
}

// --- DELETE /api/roots {path} ---

func (s *Server) apiRootsRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad-json", err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.deps.Scanner.Roots()
	idx := slices.Index(current, req.Path)
	if idx < 0 {
		writeError(w, http.StatusNotFound, "unknown-root", "not in current library")
		return
	}
	if len(current) == 1 {
		writeError(w, http.StatusBadRequest, "last-root",
			"can't remove the last remaining library root — add another root first")
		return
	}

	newList := slices.Delete(append([]string(nil), current...), idx, idx+1)

	// Drop tracks under the removed root so /v1/manifest stops advertising
	// paths that will never resolve. If the removal takes us back to
	// single-root, wipe instead (storage form flips again).
	willCollapse := len(newList) == 1
	removedBasename := filepath.Base(current[idx])

	// Commit order matters: run the destructive manifest op FIRST, and
	// only persist the root list + broadcast SetRoots after it succeeds.
	// The reverse (save config → wipe tracks) can leave disk in a state
	// where the root is gone from `bridge.yaml` but `/v1/manifest` still
	// advertises its tracks — the next restart would then serve phantom
	// paths with no hope of resolving. With this order, a manifest failure
	// means the config file is untouched; a `Cfg.Save` failure after a
	// successful wipe means the next scan simply re-populates — every
	// failure window lands in a state the scanner can heal.
	if willCollapse {
		if err := s.deps.Manifest.WipeAllTracks(); err != nil {
			writeError(w, http.StatusInternalServerError, "wipe-tracks", err.Error())
			return
		}
	} else {
		if _, err := s.deps.Manifest.DeleteTracksByPrefix(removedBasename + "/"); err != nil {
			writeError(w, http.StatusInternalServerError, "delete-tracks", err.Error())
			return
		}
	}
	// Capture prev roots before mutating so we can roll back on a
	// Save failure. Without this, a failed Save leaves the in-memory
	// Cfg holding the new list while disk still has the old — any
	// later Save call from an unrelated path (library-name edit,
	// bind-address change, etc.) would silently commit a removal the
	// operator had already seen fail.
	prevRoots := append([]string(nil), s.deps.Cfg.LibraryRoots...)
	s.deps.Cfg.LibraryRoots = newList
	if err := s.deps.Cfg.Save(s.deps.CfgPath); err != nil {
		s.deps.Cfg.LibraryRoots = prevRoots
		writeError(w, http.StatusInternalServerError, "save-config", err.Error())
		return
	}
	s.deps.Scanner.SetRoots(newList)
	s.deps.Resolver.SetRoots(newList)
	go func() { _, _ = s.deps.Scanner.Scan(s.scanCtx()) }()
	w.WriteHeader(http.StatusNoContent)
}

// --- GET /api/tokens ---

func (s *Server) apiTokensList(w http.ResponseWriter, r *http.Request) {
	tokens := s.deps.Auth.List()
	out := make([]tokenRow, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, tokenRow{
			ID:         t.ID,
			Name:       t.Name,
			CreatedAt:  t.CreatedAt,
			LastUsedAt: t.LastUsedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- POST /api/tokens {name, url} ---

func (s *Server) apiTokensMint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"` // bridge URL iOS will dial (e.g. https://host.local:7788)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad-json", err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.URL = strings.TrimSpace(req.URL)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name-required", "name must not be empty")
		return
	}
	if req.URL == "" {
		req.URL = defaultBridgeURL(s.deps.Cfg.ListenAddress)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rawToken, tok, err := s.deps.Auth.Mint(req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mint", err.Error())
		return
	}
	// alternates baked into the pairing QR so iOS learns every
	// reachable endpoint (LAN IPv4/IPv6, `.local`, Tailscale) at the
	// moment of pairing. Empty slice if enumeration fails — the
	// operator-supplied primary URL is always the first entry, so the
	// QR always pairs even on an interface-less environment.
	alternates := pairAlternates(req.URL, s.deps.Cfg.ListenAddress)
	pairURL := buildPairURL(req.URL, rawToken, s.deps.Fingerprint, s.deps.Cfg.LibraryName, alternates)
	qrData, err := qrDataURL(pairURL)
	if err != nil {
		// QR render failures don't block the pairing — the user can still
		// copy/paste the token + fingerprint manually.
		qrData = ""
	}
	writeJSON(w, http.StatusCreated, pairResult{
		RawToken:    rawToken,
		ID:          tok.ID,
		Name:        tok.Name,
		Fingerprint: s.deps.Fingerprint,
		URL:         req.URL,
		PairURL:     pairURL,
		QRDataURL:   qrData,
	})
}

// --- DELETE /api/tokens/{id} ---

func (s *Server) apiTokensRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.deps.Auth.Revoke(id); err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			writeError(w, http.StatusNotFound, "unknown-token", id)
			return
		}
		writeError(w, http.StatusInternalServerError, "revoke", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- GET /api/settings ---

func (s *Server) apiSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, settingsResponse{
		LibraryName:     s.deps.Cfg.LibraryName,
		ListenAddress:   s.deps.Cfg.ListenAddress,
		AdminAddress:    s.deps.Cfg.AdminAddress,
		DataDir:         s.deps.Cfg.DataDir,
		ScanIntervalSec: s.deps.Cfg.ScanIntervalSec,
		TLSCertPath:     s.deps.Cfg.TLSCertPath,
		TLSKeyPath:      s.deps.Cfg.TLSKeyPath,
	})
}

// --- PATCH /api/settings ---

// settingsPatch is a partial update. Pointer fields distinguish "not
// supplied" from "supplied as empty/zero" so the operator can't
// accidentally clear a field by omitting it.
type settingsPatch struct {
	LibraryName     *string `json:"libraryName,omitempty"`
	ListenAddress   *string `json:"listenAddress,omitempty"`
	AdminAddress    *string `json:"adminAddress,omitempty"`
	ScanIntervalSec *int    `json:"scanIntervalSec,omitempty"`
}

type settingsPatchResponse struct {
	RestartRequired bool `json:"restartRequired"`
}

func (s *Server) apiSettingsPatch(w http.ResponseWriter, r *http.Request) {
	var p settingsPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "bad-json", err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Snapshot the current config so a failed Validate rolls us back cleanly.
	backup := *s.deps.Cfg
	restart := false

	if p.LibraryName != nil {
		s.deps.Cfg.LibraryName = strings.TrimSpace(*p.LibraryName)
		// Library name reaches iOS via /v1/health, which reads the live
		// cfg each request — no restart needed.
	}
	if p.ListenAddress != nil {
		if *p.ListenAddress != s.deps.Cfg.ListenAddress {
			s.deps.Cfg.ListenAddress = *p.ListenAddress
			restart = true
		}
	}
	if p.AdminAddress != nil {
		if *p.AdminAddress != s.deps.Cfg.AdminAddress {
			s.deps.Cfg.AdminAddress = *p.AdminAddress
			restart = true
		}
	}
	if p.ScanIntervalSec != nil {
		s.deps.Cfg.ScanIntervalSec = *p.ScanIntervalSec
		// Periodic ticker picks up the new interval on the next tick; if
		// users want it immediately they can hit "Scan now". Not worth a
		// restart banner.
	}

	if err := s.deps.Cfg.Validate(); err != nil {
		*s.deps.Cfg = backup
		writeError(w, http.StatusBadRequest, "validate", err.Error())
		return
	}
	if err := s.deps.Cfg.Save(s.deps.CfgPath); err != nil {
		*s.deps.Cfg = backup
		writeError(w, http.StatusInternalServerError, "save-config", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settingsPatchResponse{RestartRequired: restart})
}

// --- POST /api/restart ---

func (s *Server) apiRestart(w http.ResponseWriter, r *http.Request) {
	// Respond first, exit after a brief delay so the browser sees the 202.
	w.WriteHeader(http.StatusAccepted)
	go func() {
		time.Sleep(100 * time.Millisecond)
		s.restart()
	}()
}

// --- GET /api/pair-qr?data=<url> ---
//
// Renders an arbitrary URL as a QR PNG. Useful for debugging and for the
// admin UI's "re-show QR" flow after a page reload (tokens are never
// stored plaintext on the server, so we can't regenerate the original
// pairing QR — but the admin UI can stash the pairURL in memory and
// repost it here to re-render if the modal needs to reappear).
func (s *Server) apiPairQR(w http.ResponseWriter, r *http.Request) {
	data := r.URL.Query().Get("data")
	if data == "" {
		writeError(w, http.StatusBadRequest, "data-required", "data query param required")
		return
	}
	png, err := qrPNG(data)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "qr", err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, code int, short, msg string) {
	writeJSON(w, code, map[string]string{"error": short, "message": msg})
}

// dbSize returns the bridge.db file size, or 0 if stat fails. Used for
// the dashboard — a missing DB right after `bridge init` legitimately
// stats to zero and that's fine.
func dbSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// Statically assert the Manifest store has the helpers we need. A missing
// method here will fail the build rather than at first admin request.
var _ = (*manifest.Store)(nil)
