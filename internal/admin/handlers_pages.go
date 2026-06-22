package admin

import (
	"encoding/json"
	"html/template"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/version"
)

// tmplFuncs are helpers every page template has access to. Keep minimal —
// templates should stay mostly declarative; computation belongs in the
// handler before Execute.
var tmplFuncs = template.FuncMap{
	"bytesHuman":  bytesHuman,
	"uptimeHuman": uptimeHuman,
	"timeAgo":     timeAgo,
	"formatTime":  func(t time.Time) string { return t.Format("2006-01-02 15:04:05 MST") },
	"basename":    filepath.Base,
	// json embeds a value as a JSON literal inside a <script
	// type="application/json"> block for client-side hydration. Returns
	// template.JS so html/template inserts it verbatim; json.Marshal
	// HTML-escapes <>& so there's no </script> breakout.
	"json": func(v any) (template.JS, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return template.JS(b), nil
	},
}

// pageData is the common envelope every template receives. Page-specific
// data lives under Data so the layout.html can pull nav-relevant bits
// (active tab, library name) from the outer struct.
//
// **`IsPublic`** flags the bridge's deployment posture so templates can
// hide nav entries / chrome that don't apply to a public deployment.
// Today: layout.html uses it to suppress the UPnP nav link (the
// upnpUpstream feature is `Config.Validate`-rejected in public mode
// because SSDP multicast is LAN-only AND the upstream's RFC1918 byte
// URLs are unreachable from a public VPS). upnp.html uses it to render
// a "Not available on public deployments" panel for the case where
// the operator bookmarked the URL directly.
type pageData struct {
	ActiveTab       string
	ActiveSection   string
	LibraryName     string
	Fingerprint     string
	ServerVersion   string
	ProtocolVersion int
	IsPublic        bool
	Data            any
}

// sectionForTab maps a page's ActiveTab to its top-level nav SECTION so the
// header highlights the parent entry while the in-page sub-tab bar tracks the
// specific page. Library groups Browse / Inspector / Jobs; Listening groups
// Playlists & history / Smart mixes. Standalone pages are their own section.
func sectionForTab(tab string) string {
	switch tab {
	case "library", "library_inspector", "jobs":
		return "library"
	case "data", "smartmixes":
		return "listening"
	default:
		return tab
	}
}

func (s *Server) renderPage(w http.ResponseWriter, active string, data any) {
	cfg := s.deps.CfgHolder.Load()
	t, ok := s.pageTmpls[active]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	envelope := pageData{
		ActiveTab:       active,
		ActiveSection:   sectionForTab(active),
		LibraryName:     cfg.LibraryName,
		Fingerprint:     s.deps.Fingerprint,
		ServerVersion:   version.ServerVersion,
		ProtocolVersion: version.ProtocolVersion,
		IsPublic:        cfg.IsPublic(),
		Data:            data,
	}
	if err := t.ExecuteTemplate(w, "layout", envelope); err != nil {
		logger.Error("render", "page", active, "err", err)
	}
}

func (s *Server) pageDashboard(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	tracks, _ := s.deps.Manifest.CountTracks(r.Context())
	dbBytes := dbSize(filepath.Join(cfg.DataDir, "bridge.db"))
	// Library composition for first paint (live updates come from the
	// SSE stats frame via app.js applyStats). Best-effort — a SQL
	// hiccup leaves the breakdown zeroed.
	rollup, _ := s.deps.Manifest.RollupByPrefix(r.Context(), "")
	var variantFiles int
	var variantBytes int64
	if byKind, err := s.deps.Manifest.VariantStatsByKind(r.Context()); err == nil {
		for _, st := range byKind {
			variantFiles += st.Files
			variantBytes += st.Bytes
		}
	}
	data := map[string]any{
		"Uptime":              time.Since(s.deps.StartedAt),
		"StartedAt":           s.deps.StartedAt,
		"TracksIndexed":       tracks,
		"TracksWithUpscaled":  rollup.UpscaledTrackCount,
		"TracksWithOptimized": rollup.OptimizedTrackCount,
		"VariantFiles":        variantFiles,
		"VariantBytes":        variantBytes,
		"IsScanning":          s.deps.Scanner.IsScanning(),
		"ScanProgress":        s.deps.Scanner.ScanProgress(),
		"LastFullScan":        s.deps.Scanner.LastFullScan(),
		"DBBytes":             dbBytes,
		"DeviceCount":         len(s.deps.Auth.List()),
		"Roots":               s.deps.Scanner.Roots(),
		"Update":              s.dashboardUpdateStatus(),
		// The Update tile's "Install & restart" button POSTs
		// /api/restart after install, then auto-reloads the page
		// after 2.5 s assuming the service manager will respawn.
		// On an unsupervised process the auto-reload races a
		// listener that's never coming back. Thread the flag
		// through so the JS can drop the auto-reload + tell the
		// operator to restart manually. (Qodo on PR #124.)
		"IsSupervised": s.deps.IsSupervised,
	}
	s.renderPage(w, "dashboard", data)
}

// dashboardUpdateStatus returns the UpdateStatus the dashboard tile
// should render at first paint. Subsequent updates come from the JS
// tick hitting /api/updates. Nil-safe — returns a placeholder
// "not-configured" status when no updater is wired so the template
// doesn't have to branch on nil.
func (s *Server) dashboardUpdateStatus() UpdateStatus {
	if s.deps.Updater == nil {
		return UpdateStatus{CurrentVersion: version.ServerVersion, Channel: "not-configured"}
	}
	return s.deps.Updater.Status()
}

func (s *Server) pageLibrary(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	roots := s.deps.Scanner.Roots()
	multi := len(roots) > 1
	rows := make([]libraryRootRow, 0, len(roots))
	for _, root := range roots {
		// Single-root scans store paths WITHOUT the root-basename
		// prefix, so RollupByPrefix("") gives the whole library; the
		// multi-root form prefixes every path with the basename.
		// RollupByPrefix appends the "/%" LIKE suffix itself, so pass
		// the bare basename — basename+"/" would build a "<base>//%"
		// pattern that matches nothing (CodeRabbit on PR #340).
		prefix := ""
		if multi {
			prefix = filepath.Base(root)
		}
		ru, err := s.deps.Manifest.RollupByPrefix(r.Context(), prefix)
		if err != nil {
			logger.Warn("library page: rollup", "path", root, "err", err)
		}
		rows = append(rows, libraryRootRow{
			Path:            root,
			Tracks:          ru.TrackCount,
			UpscaledTracks:  ru.UpscaledTrackCount,
			OptimizedTracks: ru.OptimizedTrackCount,
			UpscaledBytes:   ru.UpscaledSizeBytes,
			OptimizedBytes:  ru.OptimizedSizeBytes,
		})
	}
	data := libraryPageData{
		Roots:       rows,
		VariantsDir: cfg.Upscale.EffectiveVariantsDir(cfg.DataDir),
	}
	// Global per-kind cache summary for the "Transcoded cache" header.
	if byKind, err := s.deps.Manifest.VariantStatsByKind(r.Context()); err != nil {
		logger.Warn("library page: variant stats", "err", err)
	} else {
		up := byKind["upscale"]
		opt := byKind["optimize"]
		data.UpscaledVariants = up.Files
		data.OptimizedVariants = opt.Files
		data.UpscaledBytes = up.Bytes
		data.OptimizedBytes = opt.Bytes
	}
	s.renderPage(w, "library", data)
}

func (s *Server) pageDevices(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	tokens := s.deps.Auth.List()
	rows := make([]tokenRow, 0, len(tokens))
	for _, t := range tokens {
		rows = append(rows, tokenRow{
			ID: t.ID, Name: t.Name,
			CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt,
		})
	}
	data := map[string]any{
		"Tokens":     rows,
		"DefaultURL": defaultBridgeURL(cfg),
	}
	s.renderPage(w, "devices", data)
}

// pageUPnP serves the dedicated /upnp page (Configured / Discovered /
// Add manually sections). The template is hidden behind a feature-
// disabled empty state when the operator hasn't set
// `upnpUpstream.enabled = true` in bridge.yaml; the JS detects this
// from the `/api/upnp/servers` probe (returns enabled=false) and
// renders an informational placeholder.
//
// **Why a dedicated page** (vs the legacy embedded panel in
// `/devices`): the v1 surface was a read-only telemetry block;
// adding discovery + CRUD inflates it into a page-sized concern.
// Splitting the concerns gives Devices a single purpose (paired iOS
// clients) and UPnP its own page with room to grow (future:
// per-server diagnostics, advanced container picker, walk progress).
func (s *Server) pageUPnP(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	// Surface the operator-side feature flag so the template can
	// render a one-line "enable upnpUpstream.enabled in bridge.yaml"
	// hint when the flag is false. The JS probe still drives the
	// dynamic empty state (enabled = wired-up-on-the-server-side),
	// but this snapshot lets the SSR-rendered page already carry the
	// right initial copy.
	//
	// **`IsPublic`** flows in via the layout envelope (`pageData`) so
	// the template can pivot between "enable it in YAML" (loopback
	// deploy) and "feature not available on public deployments"
	// (public deploy — Config.Validate rejects the enable bit).
	data := map[string]any{
		"FeatureEnabled": cfg != nil && cfg.UPnPUpstream.Enabled,
	}
	s.renderPage(w, "upnp", data)
}

func (s *Server) pageSettings(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	data := settingsResponse{
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
		AnalysisEnabled:          cfg.Analysis.Enabled,
		SmartPlaylistsEnabled:    cfg.SmartPlaylists.Enabled,
		IsSupervised:             s.deps.IsSupervised,
		BackupIntervalHours:      cfg.Backup.EffectiveIntervalHours(),
		BackupKeep:               cfg.Backup.EffectiveKeep(),
		IsPublic:                 cfg.IsPublic(),
		DLNAEnabled:              cfg.DLNA.Enabled,
		DLNAListenAddress:        cfg.DLNA.EffectiveDLNAListenAddress(),
		DLNABlockedByPublic:      cfg.IsPublic(),
	}
	// v1.2 Audio quality section: pre-compute the boolean +
	// install hint so the template doesn't need a `deref`
	// helper or a runtime.GOOS switch. The hint is OS-aware
	// for the BRIDGE host (`runtime.GOOS` here resolves on
	// the server, not the browser viewing the admin UI) so
	// an operator on Windows sees the choco hint, on Linux
	// sees the apt/dnf/pacman variants, and on macOS sees
	// the brew one. `printSoxInstallHint` in cmd/bridge
	// keeps the same coverage for the CLI.
	if s.deps.UpscalePrecheck != nil {
		ok := s.deps.UpscalePrecheck() == nil
		data.UpscaleSoxAvailable = &ok
		data.UpscaleSoxMissing = !ok
		if !ok {
			data.UpscaleSoxInstallHint = soxInstallHintForCurrentOS()
		}
	}
	// FLAC sub-check: the bridge forces `-t flac`, so a sox WITHOUT FLAC
	// passes the availability check above but fails every job at runtime.
	// Only flag it when sox itself IS present — a missing sox is already
	// covered by the install-hint block above (the template uses else-if).
	if hasFLAC, known := s.soxFLACStatus(); known {
		data.UpscaleSoxHasFLAC = &hasFLAC
		if !data.UpscaleSoxMissing && !hasFLAC {
			data.UpscaleSoxFLACMissing = true
			data.UpscaleSoxFormatHint = soxFormatHintForCurrentOS()
		}
	}
	s.renderPage(w, "settings", data)
}

// soxInstallHintForCurrentOS returns the package-manager one-
// liner for installing sox on the bridge's host OS. Multi-line
// for Linux because distro coverage is meaningful (apt /
// dnf / pacman are mutually exclusive). Mirrors the CLI's
// `printSoxInstallHint` in `cmd/bridge/upscale.go` — keep the
// two in sync if a future package manager joins the table.
func soxInstallHintForCurrentOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "brew install sox"
	case "linux":
		return "Debian/Ubuntu:  sudo apt install sox\n" +
			"Fedora:         sudo dnf install sox\n" +
			"Arch:           sudo pacman -S sox"
	case "windows":
		return "choco install sox.portable\n" +
			"(or download from https://sourceforge.net/projects/sox/)"
	default:
		return "Install sox via your platform's package manager, or see https://sox.sourceforge.net"
	}
}

// soxFormatHintForCurrentOS returns the per-OS one-liner for the narrower
// case where sox IS installed but its build lacks FLAC support — the bridge
// forces `-t flac`, so a FLAC-less sox fails at runtime. On Debian/Ubuntu
// FLAC ships in a separate plugin package (libsox-fmt-all); Fedora/Arch/
// brew/choco bundle it, so the fix there is a reinstall. Mirrors the CLI's
// `printSoxFormatHint` in cmd/bridge/upscale.go — keep the two in sync.
func soxFormatHintForCurrentOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "brew reinstall sox   # the Homebrew bottle includes FLAC"
	case "linux":
		return "Debian/Ubuntu:  sudo apt install libsox-fmt-all\n" +
			"Fedora:         sudo dnf install sox        # bundles FLAC\n" +
			"Arch:           sudo pacman -S sox          # bundles FLAC"
	case "windows":
		return "choco install sox.portable\n" +
			"(or download a full build from https://sourceforge.net/projects/sox/)"
	default:
		return "Reinstall sox with FLAC support, or see https://sox.sourceforge.net"
	}
}

// --- tmpl helpers ---

func bytesHuman(n int64) string {
	const kb, mb, gb = 1024.0, 1024.0 * 1024.0, 1024.0 * 1024.0 * 1024.0
	f := float64(n)
	switch {
	case f >= gb:
		return formatUnit(f/gb, "GB")
	case f >= mb:
		return formatUnit(f/mb, "MB")
	case f >= kb:
		return formatUnit(f/kb, "KB")
	default:
		return formatUnit(f, "B")
	}
}

func formatUnit(v float64, unit string) string {
	if v >= 10 {
		return formatRound(v, 0) + " " + unit
	}
	return formatRound(v, 1) + " " + unit
}

func formatRound(v float64, digits int) string {
	// fmt.Sprintf %.*f, but inline to avoid importing fmt here.
	if digits == 0 {
		return itoa(int64(v + 0.5))
	}
	scaled := int64(v*10 + 0.5)
	return itoa(scaled/10) + "." + itoa(scaled%10)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func uptimeHuman(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= 24*time.Hour:
		days := int(d / (24 * time.Hour))
		rem := d - time.Duration(days)*24*time.Hour
		return itoa(int64(days)) + "d " + rem.String()
	default:
		return d.String()
	}
}

func timeAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Second)
	if d < time.Minute {
		return itoa(int64(d.Seconds())) + "s ago"
	}
	if d < time.Hour {
		return itoa(int64(d.Minutes())) + "m ago"
	}
	if d < 24*time.Hour {
		return itoa(int64(d.Hours())) + "h ago"
	}
	return itoa(int64(d.Hours()/24)) + "d ago"
}
