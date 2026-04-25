package admin

import (
	"html/template"
	"log"
	"net/http"
	"path/filepath"
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
	t, ok := s.pageTmpls[active]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	envelope := pageData{
		ActiveTab:       active,
		LibraryName:     s.deps.Cfg.LibraryName,
		Fingerprint:     s.deps.Fingerprint,
		ServerVersion:   version.ServerVersion,
		ProtocolVersion: version.ProtocolVersion,
		Data:            data,
	}
	if err := t.ExecuteTemplate(w, "layout", envelope); err != nil {
		log.Printf("admin: render %s: %v", active, err)
	}
}

func (s *Server) pageDashboard(w http.ResponseWriter, r *http.Request) {
	tracks, _ := s.deps.Manifest.CountTracks()
	dbBytes := dbSize(filepath.Join(s.deps.Cfg.DataDir, "bridge.db"))
	data := map[string]any{
		"Uptime":               time.Since(s.deps.StartedAt),
		"StartedAt":            s.deps.StartedAt,
		"TracksIndexed":        tracks,
		"IsScanning":           s.deps.Scanner.IsScanning(),
		"ScanProgress":         s.deps.Scanner.ScanProgress(),
		"LastFullScan":         s.deps.Scanner.LastFullScan(),
		"DBBytes":              dbBytes,
		"DeviceCount":          len(s.deps.Auth.List()),
		"Roots":                s.deps.Scanner.Roots(),
		"Update":               s.dashboardUpdateStatus(),
		"BackupIntervalHours":  s.deps.Cfg.Backup.EffectiveIntervalHours(),
		"BackupKeep":           s.deps.Cfg.Backup.EffectiveKeep(),
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
			n, _ = s.deps.Manifest.CountTracksByPrefix(filepath.Base(root) + "/")
		} else {
			n, _ = s.deps.Manifest.CountTracks()
		}
		rows = append(rows, rootRow{Path: root, Tracks: n})
	}
	s.renderPage(w, "library", rows)
}

func (s *Server) pageDevices(w http.ResponseWriter, r *http.Request) {
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
		"DefaultURL": defaultBridgeURL(s.deps.Cfg.ListenAddress),
	}
	s.renderPage(w, "devices", data)
}

func (s *Server) pageSettings(w http.ResponseWriter, r *http.Request) {
	data := settingsResponse{
		LibraryName:     s.deps.Cfg.LibraryName,
		ListenAddress:   s.deps.Cfg.ListenAddress,
		AdminAddress:    s.deps.Cfg.AdminAddress,
		DataDir:         s.deps.Cfg.DataDir,
		ScanIntervalSec: s.deps.Cfg.ScanIntervalSec,
		TLSCertPath:     s.deps.Cfg.TLSCertPath,
		TLSKeyPath:      s.deps.Cfg.TLSKeyPath,
	}
	s.renderPage(w, "settings", data)
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
