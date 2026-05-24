package admin

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Shared writeError codes / messages whose literals were flagged for
// duplication by SonarCloud (go:S1192). Extracted so a copy edit only
// happens once and code-points stay grep-able. Not exhaustive — only
// the codes the rule flagged at 3+ duplicates land here.
const (
	errCodeBadJSON         = "bad-json"
	errCodeSaveConfig      = "save-config"
	errCodeNoUpdater       = "no-updater"
	errMsgUpdaterNotConfig = "updater is not configured"
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
	URL         string `json:"url"`     // operator-supplied bridge URL (head of Alternates)
	PairURL     string `json:"pairURL"` // bridge://pair?... for QR
	// Alternates is the full ordered URL list baked into the
	// QR's `urls=` field — operator-supplied URL first, then every
	// other reachable endpoint the bridge self-discovered (LAN IPv4/
	// IPv6, .local mDNS, Tailscale magicdns). Surfaced to the admin
	// pair modal so the operator sees what the iOS app will roam
	// across, not just the single primary URL (a missing alternates
	// list misled an operator into thinking only one URL had been
	// shared with the device).
	Alternates []string `json:"alternates,omitempty"`
	QRDataURL  string   `json:"qrDataURL"` // data:image/png;base64,... rendered QR
}

type settingsResponse struct {
	LibraryName     string `json:"libraryName"`
	ListenAddress   string `json:"listenAddress"`
	AdminAddress    string `json:"adminAddress"`
	DataDir         string `json:"dataDir"`
	ScanIntervalSec int    `json:"scanIntervalSec"`
	TLSCertPath     string `json:"tlsCertPath,omitempty"`
	TLSKeyPath      string `json:"tlsKeyPath,omitempty"`
	// CustomEndpoints is the operator-supplied URL list. JSON shape
	// is the raw []string for programmatic clients (curl-driven
	// config). The settings template renders via the sibling
	// CustomEndpointsText field (joined with "\n") so the textarea
	// input round-trips cleanly through form-encoding.
	CustomEndpoints []string `json:"customEndpoints,omitempty"`
	// CustomEndpointsText is the newline-joined form of
	// CustomEndpoints, populated server-side for template
	// consumption. Not part of the public JSON contract — emitted
	// with `omitempty` so curl callers don't see a redundant field
	// when the slice is empty. Template-only sibling; programmatic
	// PATCH consumers send `customEndpoints` (array form) which
	// the patch handler accepts directly.
	CustomEndpointsText string `json:"customEndpointsText,omitempty"`
	// Phase C update settings — auto-install opt-in + cadence +
	// quiet-hours window. All optional in the YAML and on the wire.
	UpdateAutoInstall        bool   `json:"updateAutoInstall"`
	UpdateQuietHours         string `json:"updateQuietHours,omitempty"`
	UpdateCheckIntervalHours int    `json:"updateCheckIntervalHours,omitempty"`
	// v1.2 Upscale settings — operator opt-in flag for the
	// offline PCM-upscaling feature. Disabled by default per
	// the bridge.yaml schema; flipping this here writes the
	// config back to disk and surfaces a "restart required"
	// banner on the response so the operator knows to bounce
	// `bridge serve` for the long-lived transcode.Pool to
	// instantiate (or shut down).
	UpscaleEnabled bool `json:"upscaleEnabled"`
	// PR 4: tailscale + mDNS posture.
	// TailscaleMode mirrors cfg.Tailscale.EffectiveMode (one of
	// "cli", "tsnet", "disabled"). IsPublic flags the
	// deployment posture so the UI can show a "default disabled
	// in public mode" caption without re-querying.
	TailscaleMode string `json:"tailscaleMode"`
	// MDNSEnabled reflects EffectiveMDNSEnabled — the resolved
	// posture-aware value, not the raw pointer. Loopback
	// defaults true; public defaults false. Validate refuses
	// the public+true combination.
	MDNSEnabled bool `json:"mdnsEnabled"`
	// IsPublic surfaces cfg.IsPublic() so the Settings UI can
	// gate mDNS / Tailscale controls (e.g. hide the mDNS
	// checkbox in public mode — Validate would refuse an
	// enabled toggle anyway).
	IsPublic bool `json:"isPublic"`
	// UpscaleSoxAvailable reports whether `sox` is on PATH so
	// the Settings UI can warn the operator before they
	// enable the feature against a host that can't actually
	// run it. Computed at request time via PrecheckSox; nil
	// when the precheck hasn't been wired into the admin
	// dependencies (test harnesses).
	UpscaleSoxAvailable *bool `json:"upscaleSoxAvailable,omitempty"`
	// UpscaleSoxMissing is the template-only convenience
	// boolean (true iff the precheck succeeded AND sox was
	// reported unavailable). Lets the Settings template
	// avoid a custom `deref` helper that html/template
	// doesn't ship with. `json:"-"` keeps the JSON API
	// surface clean — PATCH consumers care about the
	// tri-state via UpscaleSoxAvailable.
	UpscaleSoxMissing bool `json:"-"`
	// UpscaleSoxInstallHint is the OS-appropriate package-
	// manager one-liner for installing sox. Computed
	// server-side via the bridge's runtime.GOOS so the
	// operator viewing the admin UI from any browser sees
	// the hint relevant to the BRIDGE host (where sox
	// needs to be installed), not the browser host.
	// Multi-line strings work via the template's `pre`
	// rendering. Empty when sox is already available.
	UpscaleSoxInstallHint string `json:"-"`
	// UpscaleStoragePath is the absolute on-disk directory the
	// long-lived transcode pool writes converted sidecar files
	// to. Surfaced to the operator on the Settings → Audio tab
	// AND the Library Inspector drawer so "where did my variants
	// land?" is answerable without an SSH session. Always
	// `<DataDir>/transcoded` today (see
	// `transcode.OutputDirFor`); a future operator-controlled
	// path would update this field, the runtime wiring, and
	// `bridge upscale --gc`'s walker together.
	UpscaleStoragePath string `json:"upscaleStoragePath,omitempty"`
	// IsSupervised reports whether the current bridge process is
	// running under launchd / systemd / Windows SCM — i.e.
	// whether `os.Exit(0)` will trigger an automatic relaunch.
	// The Settings page's "Restart now" button reads this to
	// pick honest button text and confirm wording: an
	// unsupervised bridge is told it's a STOP, not a restart.
	// Pre-fix the confirm dialog claimed the page would become
	// unreachable "until the service manager relaunches it"
	// even when there was no service manager — the lie this
	// field exists to retire. Always emitted (no `omitempty`)
	// so the JS doesn't have to disambiguate "field absent"
	// from "supervisor unknown" — false IS the supervisor-
	// unknown answer.
	IsSupervised bool `json:"isSupervised"`
	// Backup tile data — moved from the dashboard envelope so the
	// Settings page's new Backups tab can render the operator-facing
	// retention copy without a second handler round trip. The
	// dashboard no longer surfaces this section.
	BackupIntervalHours int `json:"backupIntervalHours,omitempty"`
	BackupKeep          int `json:"backupKeep,omitempty"`
}

// --- GET /api/stats ---

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.getStatsSnapshot())
}

// getStatsSnapshot is the shared builder for the dashboard stats
// payload. Used by both the REST handler (apiStats) and the SSE
// handler (apiEvents). Cheap reads only — no DB writes, no external
// network. Suitable for sub-second polling.
func (s *Server) getStatsSnapshot() statsResponse {
	cfg := s.deps.CfgHolder.Load()
	now := time.Now().UTC()
	// No request context here (called from SSE event publisher
	// too). Use Background — admin dashboard stats are best-
	// effort and not user-cancellable anyway.
	tracks, _ := s.deps.Manifest.CountTracks(context.Background())
	dbBytes := dbSize(filepath.Join(cfg.DataDir, "bridge.db"))
	return statsResponse{
		LibraryName:     cfg.LibraryName,
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
		ListenAddress:   cfg.ListenAddress,
		AdminAddress:    cfg.AdminAddress,
	}
}

// getStatsSSESnapshot returns the same payload as getStatsSnapshot
// but with UptimeSec zeroed. The SSE handler diffs serialised JSON
// frame-to-frame and only emits on change; UptimeSec increments
// every second and would otherwise force a frame on every tick,
// defeating the diff optimisation. The dashboard never renders
// UptimeSec in its live tick (uptime is server-rendered from
// StartedAt at first paint via the Go template), so zeroing it on
// the wire breaks nothing on the frontend.
func (s *Server) getStatsSSESnapshot() statsResponse {
	snap := s.getStatsSnapshot()
	snap.UptimeSec = 0
	return snap
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
	out, err := s.getEndpointsSnapshot()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.code, err.msg)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// endpointsErr is the typed error getEndpointsSnapshot returns so
// the REST handler can map it to the existing HTTP status + short
// code surface. The SSE handler only ever logs and skips — it
// can't surface 5xx mid-stream.
type endpointsErr struct {
	code string
	msg  string
}

func (e *endpointsErr) Error() string { return e.code + ": " + e.msg }

// getEndpointsSnapshot enumerates the live set of advertised endpoints —
// computed fresh on each call from `net.Interfaces()` so a just-
// connected Tailscale interface (or a just-dropped LAN one) is
// reflected immediately. Shared by apiEndpoints (REST) and
// apiEvents (SSE).
//
// Returns (entries, nil) on success, ([], nil) for the deliberate
// `:0` test-binding case, or (nil, *endpointsErr) on listen-address
// parse failure. The error type is opaque to callers — REST maps
// to 500 + short code; SSE just skips the publish.
func (s *Server) getEndpointsSnapshot() ([]adminEndpointEntry, *endpointsErr) {
	cfg := s.deps.CfgHolder.Load()
	_, portStr, err := net.SplitHostPort(cfg.ListenAddress)
	if err != nil {
		return nil, &endpointsErr{code: "bad_listen", msg: "could not parse listen address"}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, &endpointsErr{code: "bad_port", msg: "invalid listen port"}
	}
	// Reject out-of-range ports up-front rather than letting them
	// reach `advertise.Endpoints` and produce overflowed values in
	// the URL strings. Ports must be 1..65535 by RFC 6335.
	if port < 0 || port > 65535 {
		return nil, &endpointsErr{code: "bad_port", msg: "listen port out of range (must be 1..65535)"}
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
		return []adminEndpointEntry{}, nil
	}
	eps := advertise.Endpoints(advertise.Params{
		Port:            port,
		CustomEndpoints: cfg.CustomEndpoints,
	})
	out := make([]adminEndpointEntry, 0, len(eps))
	for _, e := range eps {
		out = append(out, adminEndpointEntry{
			URL:   e.URL,
			Class: e.Class.String(),
		})
	}
	return out, nil
}

// --- POST /api/scan ---

func (s *Server) apiScan(w http.ResponseWriter, r *http.Request) {
	// Route through spawnBackgroundScan so the goroutine is tracked
	// by s.bgScans — admin shutdown waits for the WG (capped at 5s
	// grace) and a process exit during a mid-write scan won't
	// corrupt SQLite. The previous raw `go func()` was untracked.
	s.spawnBackgroundScan("triggered scan")
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
			n, _ = s.deps.Manifest.CountTracksByPrefix(r.Context(), filepath.Base(root)+"/")
		} else {
			n, _ = s.deps.Manifest.CountTracks(r.Context())
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errCodeBadJSON, err.Error())
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
	cfg := s.deps.CfgHolder.Load()

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
		if err := s.deps.Manifest.WipeAllTracks(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, "wipe-tracks", err.Error())
			return
		}
	}
	// Clone cfg before mutating. With copy-on-write semantics the live
	// snapshot is only updated by the Store() call below; if Save fails
	// we simply return early and the clone is discarded — no manual
	// rollback required.
	next := config.Clone(cfg)
	next.LibraryRoots = newList
	if err := next.Save(s.deps.CfgPath); err != nil {
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
		writeError(w, http.StatusInternalServerError, errCodeSaveConfig, err.Error())
		return
	}
	s.deps.Scanner.SetRoots(newList)
	s.deps.Resolver.SetRoots(newList)
	s.deps.CfgHolder.Store(next)
	s.spawnBackgroundScan("post-add scan")
	writeJSON(w, http.StatusCreated, rootRow{Path: abs, Tracks: 0})
}

// --- DELETE /api/roots {path} ---

func (s *Server) apiRootsRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errCodeBadJSON, err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.deps.CfgHolder.Load()

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
		if err := s.deps.Manifest.WipeAllTracks(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, "wipe-tracks", err.Error())
			return
		}
	} else {
		if _, err := s.deps.Manifest.DeleteTracksByPrefix(r.Context(), removedBasename+"/"); err != nil {
			writeError(w, http.StatusInternalServerError, "delete-tracks", err.Error())
			return
		}
	}
	// Clone cfg before mutating. With copy-on-write semantics the live
	// snapshot is only updated by the Store() call below; if Save fails
	// we simply return early and the clone is discarded — no manual
	// rollback required.
	next := config.Clone(cfg)
	next.LibraryRoots = newList
	if err := next.Save(s.deps.CfgPath); err != nil {
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
		writeError(w, http.StatusInternalServerError, errCodeSaveConfig, err.Error())
		return
	}
	s.deps.Scanner.SetRoots(newList)
	s.deps.Resolver.SetRoots(newList)
	s.deps.CfgHolder.Store(next)
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
	cfg := s.deps.CfgHolder.Load()
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"` // bridge URL iOS will dial (e.g. https://host.local:7788)
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errCodeBadJSON, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.URL = strings.TrimSpace(req.URL)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name-required", "name must not be empty")
		return
	}
	if req.URL == "" {
		req.URL = defaultBridgeURL(cfg.ListenAddress)
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
	alternates := ensurePrimaryFirst(req.URL, pairAlternates(req.URL, cfg.ListenAddress))
	pairURL := buildPairURL(req.URL, rawToken, s.deps.Fingerprint, cfg.LibraryName, alternates)
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
		Alternates:  alternates,
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
	cfg := s.deps.CfgHolder.Load()
	resp := settingsResponse{
		LibraryName:              cfg.LibraryName,
		ListenAddress:            cfg.ListenAddress,
		AdminAddress:             cfg.AdminAddress,
		DataDir:                  cfg.DataDir,
		ScanIntervalSec:          cfg.ScanIntervalSec,
		TLSCertPath:              cfg.TLSCertPath,
		TLSKeyPath:               cfg.TLSKeyPath,
		CustomEndpoints:          cfg.CustomEndpoints,
		CustomEndpointsText:      strings.Join(cfg.CustomEndpoints, "\n"),
		UpdateAutoInstall:        cfg.Update.AutoInstall,
		UpdateQuietHours:         cfg.Update.QuietHours,
		UpdateCheckIntervalHours: cfg.Update.CheckIntervalHours,
		UpscaleEnabled:           cfg.Upscale.Enabled,
		UpscaleStoragePath:       cfg.Upscale.EffectiveVariantsDir(cfg.DataDir),
		IsSupervised:             s.deps.IsSupervised,
		// Same backup fields the page-template handler emits
		// (`pageSettings`). Without these the JSON API drops them
		// via `omitempty` and any programmatic GET /api/settings
		// caller (curl, future iOS / external tooling) sees a
		// payload missing the retention policy values. Keep the
		// two handlers in lockstep. (CodeRabbit on PR #129.)
		BackupIntervalHours: cfg.Backup.EffectiveIntervalHours(),
		BackupKeep:          cfg.Backup.EffectiveKeep(),
		// PR 4 — surface the resolved posture-aware values so
		// the Settings UI doesn't have to re-derive them.
		MDNSEnabled: cfg.EffectiveMDNSEnabled(),
		IsPublic:    cfg.IsPublic(),
	}
	// Tailscale mode: tolerate an unknown YAML value by falling
	// back to the effective default — the UI shows a recognizable
	// selection even if Validate is currently rejecting the
	// config (unlikely on the happy path; defensive).
	if tm, err := cfg.Tailscale.EffectiveMode(); err == nil {
		resp.TailscaleMode = string(tm)
	} else {
		resp.TailscaleMode = string(config.TailscaleModeCLI)
	}
	// Probe sox availability so the Settings UI can warn the
	// operator before they enable the feature. Cheap (one
	// exec.LookPath + a 2 s --version check inside
	// PrecheckSox); fires on every settings page load, but
	// the operator opens this page rarely. Logged-but-
	// swallowed on failure so a temporarily slow PATH lookup
	// doesn't break the whole settings tile.
	if s.deps.UpscalePrecheck != nil {
		ok := s.deps.UpscalePrecheck() == nil
		resp.UpscaleSoxAvailable = &ok
	}
	writeJSON(w, http.StatusOK, resp)
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
	// CustomEndpoints accepts the array form (programmatic clients)
	// or the textarea form via CustomEndpointsText (web UI). Sending
	// both is supported but redundant; the array form wins.
	CustomEndpoints     *[]string `json:"customEndpoints,omitempty"`
	CustomEndpointsText *string   `json:"customEndpointsText,omitempty"`
	// v1.2 Upscale opt-in. Toggling this writes the change
	// to disk; the long-lived transcode.Pool inside `bridge
	// serve` is wired at constructor time, so flipping the
	// flag at runtime triggers a `RestartRequired: true`
	// response.
	UpscaleEnabled *bool `json:"upscaleEnabled,omitempty"`
	// PR 4: TailscaleMode dropdown (cli|tsnet|disabled).
	// Hot-reload matrix:
	//   - any → disabled:    no restart (Deps.TailscaleDisable
	//                        callback cancels auto-pilot + clears LE cert).
	//   - disabled → cli:    RestartRequired:true (auto-pilot needs
	//                        to spin up fresh + wire into certManager).
	//   - disabled → tsnet:  RestartRequired:true (tsnet.Server is
	//                        wired at startup; listener composition
	//                        changes shape).
	//   - cli ↔ tsnet:       RestartRequired:true (same rewiring as
	//                        above; both modes are wired at startup).
	TailscaleMode *string `json:"tailscaleMode,omitempty"`
	// PR 4: MDNSEnabled checkbox. Hot-reloadable in BOTH
	// directions — main.go's mdnsLifecycle.Set(enabled) starts
	// or stops the Bonjour advertiser without a restart.
	// Validate refuses public+true (no LAN to advertise on).
	MDNSEnabled *bool `json:"mdnsEnabled,omitempty"`
}

type settingsPatchResponse struct {
	RestartRequired bool `json:"restartRequired"`
}

func (s *Server) apiSettingsPatch(w http.ResponseWriter, r *http.Request) {
	var p settingsPatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, errCodeBadJSON, err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.deps.CfgHolder.Load()
	next := config.Clone(cfg)
	restart := false

	if p.LibraryName != nil {
		next.LibraryName = strings.TrimSpace(*p.LibraryName)
		// Library name reaches iOS via /v1/health, which reads the live
		// cfg each request — no restart needed.
	}
	if p.ListenAddress != nil {
		if *p.ListenAddress != next.ListenAddress {
			next.ListenAddress = *p.ListenAddress
			restart = true
		}
	}
	if p.AdminAddress != nil {
		if *p.AdminAddress != next.AdminAddress {
			next.AdminAddress = *p.AdminAddress
			restart = true
		}
	}
	if p.ScanIntervalSec != nil {
		if *p.ScanIntervalSec != next.ScanIntervalSec {
			next.ScanIntervalSec = *p.ScanIntervalSec
			// scanner.RunPeriodic creates a static time.NewTicker at
			// startup and never re-evaluates the interval; the new value
			// only takes effect after a restart.
			restart = true
		}
	}
	if p.UpdateAutoInstall != nil {
		if *p.UpdateAutoInstall != next.Update.AutoInstall {
			next.Update.AutoInstall = *p.UpdateAutoInstall
			// AutoInstall is wired into the updater at constructor
			// time (cmd/bridge/main.go reads cfg.Update.AutoInstall
			// once when building updater.Options). Toggling it at
			// runtime requires a restart for the change to bind.
			restart = true
		}
	}
	if p.UpdateQuietHours != nil {
		if *p.UpdateQuietHours != next.Update.QuietHours {
			next.Update.QuietHours = *p.UpdateQuietHours
			restart = true
		}
	}
	if p.UpdateCheckIntervalHours != nil {
		if *p.UpdateCheckIntervalHours != next.Update.CheckIntervalHours {
			next.Update.CheckIntervalHours = *p.UpdateCheckIntervalHours
			restart = true
		}
	}
	// Custom endpoints: array form takes precedence; textarea form is
	// split on newlines (also tolerates `,` so curl-driven flat
	// strings work). Validate() runs at the end and prunes invalid
	// entries — we don't reject the whole patch on per-entry typos.
	// Live behaviour: handlers read `cfg.CustomEndpoints` per-request
	// (Endpoints / /v1/health), so config-on-disk + in-memory cfg
	// updates suffice — no restart_required for this field alone.
	// Cert SAN coverage for new entries is operator-driven via the
	// admin Cert tile (PR feat/tls-broader-sans).
	if p.UpscaleEnabled != nil {
		if *p.UpscaleEnabled != next.Upscale.Enabled {
			next.Upscale.Enabled = *p.UpscaleEnabled
			// Pool / sox-precheck / api wiring happens once at
			// `bridge serve` startup. A live flip would have to
			// instantiate (or shut down) the Pool, change the
			// /v1/health response, AND reconfigure the variant-
			// store hook — invasive enough that surfacing
			// "restart required" is the right call for v1.2.
			// A future iteration could hot-apply via a
			// runtime hook, but the operator-friction gain
			// isn't worth the rewiring complexity until a
			// user requests it.
			restart = true
		}
	}
	if p.CustomEndpoints != nil {
		next.CustomEndpoints = *p.CustomEndpoints
	} else if p.CustomEndpointsText != nil {
		next.CustomEndpoints = splitCustomEndpointsText(*p.CustomEndpointsText)
	}

	// PR 4 — Tailscale mode dropdown. Hot-reload matrix:
	//   any  → disabled:  no restart; cmd-side TailscaleDisable
	//                     callback cancels the auto-pilot ctx +
	//                     clears the LE cert from certManager.
	//   any  → cli|tsnet: RestartRequired — auto-pilot + listener
	//                     composition need a clean boot.
	// Same posture-default rule from applyDefaults applies on
	// validation: the new value is taken literally (empty string
	// is rejected by EffectiveMode → caught by next.Validate
	// below if user typoed).
	tailscaleWasDisabled := false
	tailscaleNowDisabled := false
	tailscaleHotReload := false
	if p.TailscaleMode != nil {
		prevMode, _ := next.Tailscale.EffectiveMode()
		tailscaleWasDisabled = prevMode == config.TailscaleModeDisabled
		// Empty payload is ambiguous: EffectiveMode() resolves
		// "" to "cli" (historical default), but applyDefaults
		// sets it to "disabled" in public mode. The PATCH
		// surface tolerates whitespace but rejects the bare-
		// empty form so an operator who accidentally clears the
		// dropdown doesn't silently flip into the wrong mode.
		// (Gemini medium on PR #294 caught the divergence.)
		trimmed := strings.TrimSpace(*p.TailscaleMode)
		if trimmed == "" {
			writeError(w, http.StatusBadRequest, "validate",
				"tailscaleMode: must be one of cli|tsnet|disabled (empty payload not accepted — would silently differ between loopback and public defaults)")
			return
		}
		next.Tailscale.Mode = trimmed
		newMode, modeErr := next.Tailscale.EffectiveMode()
		if modeErr != nil {
			writeError(w, http.StatusBadRequest, "validate", modeErr.Error())
			return
		}
		tailscaleNowDisabled = newMode == config.TailscaleModeDisabled
		// Hot-reload contract: only the cli → disabled
		// transition fires the in-process Disable callback.
		// All other transitions need a restart:
		//   - disabled → cli|tsnet: auto-pilot + listener wiring
		//     need a clean boot.
		//   - cli ↔ tsnet:          same as above.
		//   - tsnet → disabled:     the embedded tsnet.Server
		//     and its listeners are wired at startup and can't
		//     be torn down mid-process; without a restart they
		//     would keep running until SIGINT (Gemini high on
		//     PR #294).
		tailscaleHotReload = newMode != prevMode &&
			tailscaleNowDisabled &&
			prevMode == config.TailscaleModeCLI
		if newMode != prevMode && !tailscaleHotReload {
			restart = true
		}
	}
	// PR 4 — mDNS toggle. Hot-reloadable in BOTH directions.
	mdnsWasEnabled := next.EffectiveMDNSEnabled()
	mdnsNowEnabled := mdnsWasEnabled
	if p.MDNSEnabled != nil {
		v := *p.MDNSEnabled
		next.MDNS.Enabled = &v
		mdnsNowEnabled = v
	}

	if err := next.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "validate", err.Error())
		return
	}
	if err := next.Save(s.deps.CfgPath); err != nil {
		writeError(w, http.StatusInternalServerError, errCodeSaveConfig, err.Error())
		return
	}
	s.deps.CfgHolder.Store(next)

	// Fire hot-reload side effects AFTER persisting + publishing
	// the new config. Order matters: the mdns-toggle + tailscale-
	// disable callbacks read the same RuntimeConfig (via cfgHolder)
	// for their up-to-date view, and we want them to see the
	// already-Store'd next snapshot.
	if p.TailscaleMode != nil && tailscaleHotReload && !tailscaleWasDisabled && s.deps.TailscaleDisable != nil {
		s.deps.TailscaleDisable()
	}
	if p.MDNSEnabled != nil && mdnsNowEnabled != mdnsWasEnabled && s.deps.MDNSToggle != nil {
		s.deps.MDNSToggle(mdnsNowEnabled)
	}

	writeJSON(w, http.StatusOK, settingsPatchResponse{RestartRequired: restart})
}

// upscaleStatsResponse is the JSON shape /api/upscale/stats
// returns. Three sources combined into one tile-friendly
// payload:
//
//   - Live pool counters (workers / queue / inflight / lifetime
//     totals) — present only when the feature is active. The
//     Settings page renders "Feature off" when these are nil.
//   - Cached-variants count + total bytes from `track_variants`.
//     Survives across restarts and reflects the operator's
//     historical conversion work — useful even when the feature
//     is currently off.
//   - Sox-availability probe — same closure as the Settings
//     page consumes so the two surfaces agree about whether
//     the host can run conversions.
type upscaleStatsResponse struct {
	// Enabled mirrors `cfg.Upscale.Enabled` AND the live
	// presence of the pool — false when the feature was on
	// at startup but the sox-precheck demoted it to off.
	Enabled bool `json:"enabled"`
	// SoxAvailable reports the live `transcode.PrecheckSox`
	// result. Nil when the precheck closure isn't wired
	// (test harnesses).
	SoxAvailable *bool `json:"soxAvailable,omitempty"`
	// Pool reports the live worker-pool snapshot. Nil when
	// the feature is off (no pool to query).
	Pool *UpscalePoolStats `json:"pool,omitempty"`
	// CachedVariants is the number of rows in `track_variants`
	// — represents historical conversion work that survives
	// across restarts and a feature-flag round trip. May be
	// non-zero even when `Enabled` is false (operator
	// disabled the feature without running --gc).
	CachedVariants int `json:"cachedVariants"`
	// CachedBytes is the total size of all sidecar files,
	// summed from `track_variants.size_bytes`. Helps the
	// operator gauge disk usage before deciding to re-enable
	// or `--gc`.
	CachedBytes int64 `json:"cachedBytes"`
	// StoragePath is the absolute on-disk directory the
	// runtime pool writes converted sidecars to. Same value
	// surfaced on /api/settings.upscaleStoragePath; included
	// here too so the Settings page's live "Upscale stats"
	// tile and the Library Inspector's drawer can render
	// "Stored at <path>" without a second handler round
	// trip. Computed via `cfg.Upscale.EffectiveVariantsDir(cfg.DataDir)`
	// — always populated regardless of whether the pool
	// itself is running.
	StoragePath string `json:"storagePath,omitempty"`
}

// apiUpscaleStats: GET /api/upscale/stats
//
// Returns a snapshot of the upscale feature's runtime + on-disk
// state. Used by the Settings page tile that shows "12 cached
// variants (4.2 GB) — 3 jobs in flight". Cheap (single SQL
// COUNT + a mutex-protected pool snapshot + a TTL-cached sox
// precheck); fires on dashboard refresh every 5 s.
//
// **`enabled` reports live runtime state**, NOT the persisted
// config (CodeRabbit major on PR #110). The two diverge in two
// real cases: (a) startup demoted the feature when sox-precheck
// failed even though `cfg.Upscale.Enabled == true`, (b) the
// operator just PATCHed `upscaleEnabled = false` but the
// long-lived Pool is still alive until restart. Both surface as
// `pool == nil` from the closure (which gates on the config
// flag too — see cmd/bridge wiring); we report
// `enabled = (pool != nil)` so the iOS-facing /v1/health
// semantics and the admin tile agree about what "active"
// means. The Settings PATCH form reads the persisted
// `cfg.Upscale.Enabled` from `/api/settings.upscaleEnabled`
// separately for the toggle's initial state.
func (s *Server) apiUpscaleStats(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	var resp upscaleStatsResponse
	resp.StoragePath = cfg.Upscale.EffectiveVariantsDir(cfg.DataDir)
	if avail := s.cachedSoxAvailability(); avail != nil {
		resp.SoxAvailable = avail
	}
	if s.deps.UpscaleStats != nil {
		resp.Pool = s.deps.UpscaleStats()
	}
	resp.Enabled = (resp.Pool != nil)
	if s.deps.Manifest != nil {
		count, bytes, err := s.deps.Manifest.CountVariants(r.Context())
		if err != nil {
			// Log + degrade: caller still gets the live
			// fields. A SQL failure here is the kind of
			// thing that should be visible in logs but not
			// turn the whole tile into an error state.
			logger.Warn("upscale stats: count variants", "err", err)
		} else {
			resp.CachedVariants = count
			resp.CachedBytes = bytes
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// soxAvailabilityCacheTTL bounds how long the cached precheck
// result is reused before re-probing. 30 s feels right: an
// operator installing sox sees the Settings UI reflect it
// within at most 30 s without us spending up to 2 s on the
// probe per 5 s stats poll (CodeRabbit major on PR #110 — the
// previous per-call precheck shelled out 12×/min on every open
// Settings tab).
const soxAvailabilityCacheTTL = 30 * time.Second

// cachedSoxAvailability returns the most recent precheck result
// or runs a fresh probe when the cache is older than
// soxAvailabilityCacheTTL. Returns nil when no precheck closure
// is wired (test harnesses).
func (s *Server) cachedSoxAvailability() *bool {
	if s.deps.UpscalePrecheck == nil {
		return nil
	}
	now := time.Now()
	s.soxAvailabilityMu.Lock()
	defer s.soxAvailabilityMu.Unlock()
	if !s.soxAvailabilityAt.IsZero() && now.Sub(s.soxAvailabilityAt) < soxAvailabilityCacheTTL {
		v := s.soxAvailability
		return &v
	}
	v := s.deps.UpscalePrecheck() == nil
	s.soxAvailability = v
	s.soxAvailabilityAt = now
	return &v
}

// splitCustomEndpointsText parses the textarea form of the custom-
// endpoints field. Tolerates newline OR comma separators (paste-
// friendly curl-from-spreadsheet input) and trims surrounding
// whitespace per entry. Empty entries drop silently. Validation /
// HTTPS-only filtering happens downstream in `cfg.Validate()`.
func splitCustomEndpointsText(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	// Replace commas with newlines so the single Split below handles
	// both delimiters. Two passes would be equivalent; one is faster.
	normalised := strings.ReplaceAll(s, ",", "\n")
	lines := strings.Split(normalised, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
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
	writeJSON(w, http.StatusOK, s.getUpdatesSnapshot())
}

// getUpdatesSnapshot returns the cached updater status, or the
// "not-configured" stub when no updater is wired (test harnesses,
// future opt-out flag). Shared by apiUpdatesGet (REST) and
// apiEvents (SSE).
func (s *Server) getUpdatesSnapshot() UpdateStatus {
	if s.deps.Updater == nil {
		return UpdateStatus{
			CurrentVersion: version.ServerVersion,
			Channel:        "not-configured",
		}
	}
	return s.deps.Updater.Status()
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
		writeError(w, http.StatusServiceUnavailable, errCodeNoUpdater, errMsgUpdaterNotConfig)
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
		writeError(w, http.StatusServiceUnavailable, errCodeNoUpdater, errMsgUpdaterNotConfig)
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
		writeError(w, http.StatusServiceUnavailable, errCodeNoUpdater, errMsgUpdaterNotConfig)
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
