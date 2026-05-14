package admin

import (
	"html/template"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/transcode"
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
}

// pageData is the common envelope every template receives. Page-specific
// data lives under Data so the layout.html can pull nav-relevant bits
// (active tab, library name) from the outer struct.
type pageData struct {
	ActiveTab       string
	LibraryName     string
	Fingerprint     string
	ServerVersion   string
	ProtocolVersion int
	Data            any
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
		LibraryName:     cfg.LibraryName,
		Fingerprint:     s.deps.Fingerprint,
		ServerVersion:   version.ServerVersion,
		ProtocolVersion: version.ProtocolVersion,
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
	data := map[string]any{
		"Uptime":        time.Since(s.deps.StartedAt),
		"StartedAt":     s.deps.StartedAt,
		"TracksIndexed": tracks,
		"IsScanning":    s.deps.Scanner.IsScanning(),
		"ScanProgress":  s.deps.Scanner.ScanProgress(),
		"LastFullScan":  s.deps.Scanner.LastFullScan(),
		"DBBytes":       dbBytes,
		"DeviceCount":   len(s.deps.Auth.List()),
		"Roots":         s.deps.Scanner.Roots(),
		"Update":        s.dashboardUpdateStatus(),
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
	roots := s.deps.Scanner.Roots()
	multi := len(roots) > 1
	rows := make([]rootRow, 0, len(roots))
	for _, root := range roots {
		var n int
		if multi {
			n, _ = s.deps.Manifest.CountTracksByPrefix(r.Context(), filepath.Base(root)+"/")
		} else {
			n, _ = s.deps.Manifest.CountTracks(r.Context())
		}
		rows = append(rows, rootRow{Path: root, Tracks: n})
	}
	s.renderPage(w, "library", rows)
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
		"DefaultURL": defaultBridgeURL(cfg.ListenAddress),
	}
	s.renderPage(w, "devices", data)
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
		UpscaleStoragePath:       transcode.OutputDirFor(cfg.DataDir),
		IsSupervised:             s.deps.IsSupervised,
		BackupIntervalHours:      cfg.Backup.EffectiveIntervalHours(),
		BackupKeep:               cfg.Backup.EffectiveKeep(),
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
