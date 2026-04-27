package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
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
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt time.Time  `json:"lastUsedAt,omitempty"`
	RotatedAt  time.Time  `json:"rotatedAt,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
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
	// Phase C update settings — auto-install opt-in + cadence +
	// quiet-hours window. All optional in the YAML and on the wire.
	UpdateAutoInstall        bool   `json:"updateAutoInstall"`
	UpdateQuietHours         string `json:"updateQuietHours,omitempty"`
	UpdateCheckIntervalHours int    `json:"updateCheckIntervalHours,omitempty"`
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

// --- GET /api/endpoints ---

// adminEndpointEntry is the JSON shape for the admin console's
// "Reachable endpoints" panel. Mirrors the per-URL shape iOS sees
// in `/v1/health.endpoints`, plus a class label so the operator
// can tell at a glance which interface is which (LAN / Tailscale
// / mDNS / Public).
//
// The Class string is stable per `advertise.Class.String()` —
// admin-side wire shape, not the device-facing `/v1/*` protocol.
type adminEndpointEntry struct {
	URL   string `json:"url"`
	Class string `json:"class"`
}

// apiEndpoints returns the live set of advertised endpoints —
// computed fresh on each call from `net.Interfaces()` so a
// just-connected Tailscale interface (or a just-dropped LAN one)
// is reflected immediately. Mirrors the per-call enumeration in
// `s.reachableEndpoints()` over in `internal/api`, but admin-
// scoped so the iOS-facing handler stays untouched.
//
// No reachability indicator from the bridge side — only the iOS
// client knows reachability from its network position. Operators
// see the list of addresses; iOS sees per-URL reachability via
// the unified probe (paired iOS PR #150).
func (s *Server) apiEndpoints(w http.ResponseWriter, r *http.Request) {
	_, portStr, err := net.SplitHostPort(s.deps.Cfg.ListenAddress)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "bad_listen", "could not parse listen address")
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "bad_port", "invalid listen port")
		return
	}
	// Reject out-of-range ports up-front rather than letting them
	// reach `advertise.Endpoints` and produce overflowed values in
	// the URL strings. Ports must be 1..65535 by RFC 6335.
	if port < 0 || port > 65535 {
		writeError(w, http.StatusInternalServerError, "bad_port",
			"listen port out of range (must be 1..65535)")
		return
	}
	// Listen address ":0" is the OS-pick-a-port mode the codebase
	// supports for testing — `cmd/bridge` binds first then logs the
	// actual port. The configured address still reads `:0`, so the
	// admin handler can't synthesise a useful URL here. Return an
	// empty list instead of HTTP 500 so the devices-page panel
	// renders "No external addresses detected" honestly. (Qodo
	// flagged on PR #69 review — without this, the panel poll
	// 500's every 30s and the operator can't tell whether the
	// bridge is misconfigured or simply in port-zero mode.)
	if port == 0 {
		writeJSON(w, http.StatusOK, []adminEndpointEntry{})
		return
	}
	eps := advertise.Endpoints(advertise.Params{Port: port})
	out := make([]adminEndpointEntry, 0, len(eps))
	for _, e := range eps {
		out = append(out, adminEndpointEntry{
			URL:   e.URL,
			Class: e.Class.String(),
		})
	}
	writeJSON(w, http.StatusOK, out)
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
		// Compensating scan: if we reached here via the transition
		// branch, WipeAllTracks has already emptied the manifest but
		// the config was never persisted — /v1/manifest will serve
		// zero tracks until the next scheduled/manual scan. Kick off
		// a best-effort scan against the RESTORED (previous) roots
		// so the outage is bounded by one scan-duration rather than
		// one scan-interval. The 500 the user sees is the real
		// failure (Save); a compensating-scan failure is an
		// additional recovery attempt, so we log (not return) any
		// error it produces for server-side observability.
		if willTransition {
			s.spawnBackgroundScan("compensating-scan (add)")
		}
		writeError(w, http.StatusInternalServerError, "save-config", err.Error())
		return
	}
	s.deps.Scanner.SetRoots(newList)
	s.deps.Resolver.SetRoots(newList)
	s.spawnBackgroundScan("post-add scan")
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
		// Compensating scan — same rationale as the Add path: the
		// manifest op above already mutated /v1/manifest (wipe or
		// prefix-delete), and without a rescan the manifest sits in
		// a post-mutation state until the next scheduled scan.
		// Rescan against the restored (previous) roots to bound
		// the outage by one scan-duration. Errors logged (not
		// returned) — the 500 the user sees is the real Save
		// failure; a compensating-scan error is additional recovery
		// and only matters for operator observability.
		s.spawnBackgroundScan("compensating-scan (remove)")
		writeError(w, http.StatusInternalServerError, "save-config", err.Error())
		return
	}
	s.deps.Scanner.SetRoots(newList)
	s.deps.Resolver.SetRoots(newList)
	s.spawnBackgroundScan("post-remove scan")
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
			RotatedAt:  t.RotatedAt,
			ExpiresAt:  t.ExpiresAt,
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
		LibraryName:              s.deps.Cfg.LibraryName,
		ListenAddress:            s.deps.Cfg.ListenAddress,
		AdminAddress:             s.deps.Cfg.AdminAddress,
		DataDir:                  s.deps.Cfg.DataDir,
		ScanIntervalSec:          s.deps.Cfg.ScanIntervalSec,
		TLSCertPath:              s.deps.Cfg.TLSCertPath,
		TLSKeyPath:               s.deps.Cfg.TLSKeyPath,
		UpdateAutoInstall:        s.deps.Cfg.Update.AutoInstall,
		UpdateQuietHours:         s.deps.Cfg.Update.QuietHours,
		UpdateCheckIntervalHours: s.deps.Cfg.Update.CheckIntervalHours,
	})
}

// --- PATCH /api/settings ---

// settingsPatch is a partial update. Pointer fields distinguish "not
// supplied" from "supplied as empty/zero" so the operator can't
// accidentally clear a field by omitting it.
type settingsPatch struct {
	LibraryName              *string `json:"libraryName,omitempty"`
	ListenAddress            *string `json:"listenAddress,omitempty"`
	AdminAddress             *string `json:"adminAddress,omitempty"`
	ScanIntervalSec          *int    `json:"scanIntervalSec,omitempty"`
	UpdateAutoInstall        *bool   `json:"updateAutoInstall,omitempty"`
	UpdateQuietHours         *string `json:"updateQuietHours,omitempty"`
	UpdateCheckIntervalHours *int    `json:"updateCheckIntervalHours,omitempty"`
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
	if p.UpdateAutoInstall != nil {
		if *p.UpdateAutoInstall != s.deps.Cfg.Update.AutoInstall {
			s.deps.Cfg.Update.AutoInstall = *p.UpdateAutoInstall
			// AutoInstall is wired into the updater at constructor
			// time (cmd/bridge/main.go reads cfg.Update.AutoInstall
			// once when building updater.Options). Toggling it at
			// runtime requires a restart for the change to bind.
			restart = true
		}
	}
	if p.UpdateQuietHours != nil {
		if *p.UpdateQuietHours != s.deps.Cfg.Update.QuietHours {
			s.deps.Cfg.Update.QuietHours = *p.UpdateQuietHours
			restart = true
		}
	}
	if p.UpdateCheckIntervalHours != nil {
		if *p.UpdateCheckIntervalHours != s.deps.Cfg.Update.CheckIntervalHours {
			s.deps.Cfg.Update.CheckIntervalHours = *p.UpdateCheckIntervalHours
			restart = true
		}
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

// --- GET /api/updates ---
//
// Returns the cached updater Status. Cheap (mutex-protected snapshot).
// Used by the dashboard tile to refresh "Update available" without
// re-polling GitHub on every browser tick.
//
// When no Updater is wired (test harnesses, future opt-out flag) the
// response is a stub showing the bridge's own version and a
// "not-configured" Channel — the dashboard JS treats this the same as
// "no update info" and hides the tile's action button.
func (s *Server) apiUpdatesGet(w http.ResponseWriter, r *http.Request) {
	if s.deps.Updater == nil {
		writeJSON(w, http.StatusOK, UpdateStatus{
			CurrentVersion: version.ServerVersion,
			Channel:        "not-configured",
		})
		return
	}
	writeJSON(w, http.StatusOK, s.deps.Updater.Status())
}

// --- POST /api/updates/check ---
//
// Forces an out-of-schedule poll. Used by the dashboard's "Check now"
// button. The handler waits for the poll to return so the JSON
// response carries the post-check status — operator gets a single
// round-trip, no second fetch needed.
//
// Uses r.Context() so a browser disconnect cancels the (potentially
// slow) GitHub call rather than letting it run uselessly to completion.
func (s *Server) apiUpdatesCheck(w http.ResponseWriter, r *http.Request) {
	if s.deps.Updater == nil {
		writeError(w, http.StatusServiceUnavailable, "no-updater", "updater is not configured")
		return
	}
	writeJSON(w, http.StatusOK, s.deps.Updater.CheckNow(r.Context()))
}

// --- POST /api/updates/install[?force=1] ---
//
// Downloads, verifies, swaps the binary, and arms the rollback
// marker for the most recent release the updater has cached. Does
// NOT trigger restart itself — operator hits the existing
// /api/restart endpoint to actually load the new binary, OR the
// admin UI's "Install & restart" button calls install then restart
// in sequence. Splitting the two keeps the failure modes
// distinguishable in the UI ("install OK but restart hung" vs
// "install failed").
//
// Returns:
//   - 200 with the post-install Status on success
//   - 400 when no update is available
//   - 409 when downloads are inflight (use ?force=1 to override)
//   - 501 on Windows (Phase B is darwin/linux; Windows is a follow-up)
//   - 403 when the binary path isn't writable (system install needs sudo)
//   - 502 on download / verify / swap failures
func (s *Server) apiUpdatesInstall(w http.ResponseWriter, r *http.Request) {
	if s.deps.Updater == nil {
		writeError(w, http.StatusServiceUnavailable, "no-updater", "updater is not configured")
		return
	}
	force := r.URL.Query().Get("force") == "1"
	st, err := s.deps.Updater.Install(r.Context(), force)
	if err != nil {
		code, short := classifyUpdateError(err)
		writeError(w, code, short, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// --- POST /api/updates/rollback[?force=1] ---
//
// Restores bridge.bak over the live binary. Used when the operator
// has installed an update but wants to revert before the boot-time
// rollback path would fire (i.e. the new version starts up but
// behaves badly). Requires a recent install attempt — otherwise
// .bak is missing and the rollback fails with a clear message.
//
// Returns:
//   - 204 on success (no body — operator clicks Restart next)
//   - 409 when downloads are inflight (use ?force=1)
//   - 501 on Windows
//   - 502 on rollback I/O failure (.bak missing, etc.)
func (s *Server) apiUpdatesRollback(w http.ResponseWriter, r *http.Request) {
	if s.deps.Updater == nil {
		writeError(w, http.StatusServiceUnavailable, "no-updater", "updater is not configured")
		return
	}
	force := r.URL.Query().Get("force") == "1"
	if err := s.deps.Updater.Rollback(force); err != nil {
		code, short := classifyUpdateError(err)
		writeError(w, code, short, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// classifyUpdateError maps the typed sentinel errors the
// UpdateProvider adapter wraps onto sensible HTTP status codes +
// short error codes. The adapter (cmd/bridge/main.go) is
// responsible for wrapping internal/updater's own typed errors with
// the admin-side sentinels (admin.ErrUpdateXxx) via fmt.Errorf
// "%w: …", so this classification stays robust under refactoring of
// the underlying error messages.
//
// PR #42 review (Gemini) flagged the original string-contains
// implementation as fragile — fixed.
func classifyUpdateError(err error) (status int, short string) {
	switch {
	case errors.Is(err, ErrUpdateNoUpdate):
		return http.StatusBadRequest, "no-update"
	case errors.Is(err, ErrUpdateActiveSessions):
		return http.StatusConflict, "active-sessions"
	case errors.Is(err, ErrUpdateNotSupported):
		return http.StatusNotImplemented, "platform-unsupported"
	case errors.Is(err, ErrUpdatePathNotWritable):
		return http.StatusForbidden, "path-not-writable"
	default:
		return http.StatusBadGateway, "install-failed"
	}
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
