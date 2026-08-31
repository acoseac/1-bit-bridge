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
// URLs are unreachable from a public VPS) and to render the sign-out
// button (loopback installs have no session to end). upnp.html uses it
// to render a "Not available on public deployments" panel for the case
// where the operator bookmarked the URL directly; data.html gates its
// "never leaves the loopback admin console" copy on it. renderPage
// itself reads it for the frame-ancestors clickjacking headers.
type pageData struct {
	ActiveTab     string
	ActiveSection string
	// PlayerNav is the player sub-section that has a sidebar entry of
	// its OWN, or "" when the current player route is covered by Browse.
	// Empty on every operator page.
	//
	// It exists because every player route renders the "player" tab and
	// the "player" section, so tab-or-section matching cannot tell
	// /albums from /mixes — and with Smart mixes now pointing into the
	// player, two rail entries would light at once. layout.html branches
	// on this; boot.js applies the same rule client-side for navigations
	// that never reach the server.
	PlayerNav       string
	LibraryName     string
	Fingerprint     string
	ServerVersion   string
	ProtocolVersion int
	IsPublic        bool
	Data            any
}

// playerNavEntry maps a player sub-section to the sidebar entry that
// owns it. Only sections with their own rail entry appear here;
// everything else belongs to Browse and returns "".
//
// NO SECTION CURRENTLY HAS ONE. Playlists and Smart mixes did until the
// sidebar's Library group was found to be offering a second route to two
// views that Browse's own section rail already lists; both entries were
// removed and every player route now lights Browse.
//
// The seam is kept rather than deleted because it is a three-way contract
// — this function, the data-player-section attribute in layout.html, and
// updateSidebarNav in boot.js — and boot.js's two-pass highlight exists
// specifically so a section-keyed entry does not light alongside a
// tab-keyed one. That subtlety is what a future re-add would get wrong if
// it had to be rebuilt from nothing. The parity tests in
// page_init_parity_test.go assert the empty case explicitly, so adding a
// case here without the other two sides fails immediately.
//
// A DETAIL route must fold onto the same entry as its grid (return the
// same string for "mix" as for "mixes"), so drilling in keeps that entry
// lit rather than jumping the highlight back to Browse.
func playerNavEntry(section string) string {
	_ = section
	return ""
}

// sectionForTab maps a page's ActiveTab to its top-level nav SECTION so
// the header highlights the parent entry while the in-page sub-tab bar
// tracks the specific page.
func sectionForTab(tab string) string {
	switch tab {
	case "player":
		return "player"
	case "stats", "settings":
		return tab
	default:
		// Everything else is an operator surface and lives under the
		// single Server entry. Written as a default rather than an
		// enumeration deliberately: a new operator page should appear
		// under Server automatically, and the alternative — forgetting
		// to add it — leaves its nav entry unhighlighted with nothing
		// failing anywhere.
		return "server"
	}
}

// activePlayerNav resolves the sidebar entry for a player page, reusing
// playerSectionFor so the section is derived exactly once — the same
// value pagePlayer seeds the shell with.
func activePlayerNav(active string, r *http.Request) string {
	if active != "player" || r == nil {
		return ""
	}
	section, _ := playerSectionFor(r)
	return playerNavEntry(section)
}

func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, active string, data any) {
	s.renderPageStatus(w, r, active, http.StatusOK, data)
}

// renderPageStatus is renderPage with an explicit status code, for the
// one page that is not a 200. Each branch writes the status immediately
// before executing its template and never earlier — WriteHeader freezes
// the header map, so an early call would silently drop Content-Type,
// Cache-Control, Vary, the public-mode framing guards, and the three
// X-Bridge-* headers the partial-boost router reads.
func (s *Server) renderPageStatus(w http.ResponseWriter, r *http.Request, active string, status int, data any) {
	cfg := s.deps.CfgHolder.Load()
	t, ok := s.pageTmpls[active]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// A response's shape depends on X-Bridge-Partial, so any cache between
	// here and the browser must key on it — otherwise a stored fragment
	// could answer a full-page request or vice versa. Cache-Control above
	// is already no-store, but Vary is the correct primitive and costs
	// nothing.
	w.Header().Set("Vary", "X-Bridge-Partial")
	if cfg.IsPublic() {
		// Clickjacking guard for the internet-facing console:
		// authenticated pages carry destructive buttons (revoke,
		// delete-all-variants, restart), and only the /login page
		// previously sent X-Frame-Options. frame-ancestors is the
		// modern primitive; XFO SAMEORIGIN covers legacy browsers.
		// Loopback mode stays as-is, and a full CSP remains future
		// work (see the layout.html head note) — inline scripts rule
		// out script-src for now.
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'self'")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	}
	envelope := pageData{
		ActiveTab:       active,
		ActiveSection:   sectionForTab(active),
		PlayerNav:       activePlayerNav(active, r),
		LibraryName:     cfg.LibraryName,
		Fingerprint:     s.deps.Fingerprint,
		ServerVersion:   version.ServerVersion,
		ProtocolVersion: version.ProtocolVersion,
		IsPublic:        cfg.IsPublic(),
		Data:            data,
	}

	// Partial-boost (PR 11): when the client router fetches a page to
	// swap into the live document, it sends X-Bridge-Partial: 1 and we
	// render just the "content" block — the same inner HTML <main> would
	// hold — instead of the whole "layout". The persistent chrome (the
	// <head>, the header/nav, and crucially the player module's <audio>
	// element and now-playing bar, which live on <body>) is left
	// untouched, so playback survives the navigation.
	//
	// The two X-Bridge-* headers are the authoritative active-tab/section
	// for the fetched page: the client updates body[data-active] /
	// [data-section] and the top-nav highlight from them rather than
	// re-deriving the mapping in JS, so the server stays the single
	// source of truth for sectionForTab. A full-page fallback
	// (location.assign) covers any client that can't or won't boost, so
	// this path is a pure enhancement.
	if r != nil && r.Header.Get("X-Bridge-Partial") == "1" {
		w.Header().Set("X-Bridge-Active", active)
		w.Header().Set("X-Bridge-Section", sectionForTab(active))
		// Third value, for the same reason as the other two: the client
		// must not re-derive which sidebar entry owns a player route.
		// Sent only when there IS one — an absent header means Browse.
		if nav := envelope.PlayerNav; nav != "" {
			w.Header().Set("X-Bridge-Player-Nav", nav)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
		}
		if err := t.ExecuteTemplate(w, "content", envelope); err != nil {
			logger.Error("render partial", "page", active, "err", err)
		}
		return
	}

	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	if err := t.ExecuteTemplate(w, "layout", envelope); err != nil {
		logger.Error("render", "page", active, "err", err)
	}
}

// pageStats renders the operator dashboard. It moved off "/" when the
// library player took the root; the template and every element id are
// unchanged, which is what lets applyStats / applyComposition /
// applySources / applyEnrichment keep working with no JS edit.
func (s *Server) pageStats(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	dbBytes := dbSize(filepath.Join(cfg.DataDir, "bridge.db"))
	// Library composition for first paint (live updates come from the
	// SSE stats frame via app.js applyStats). Best-effort — a SQL
	// hiccup leaves the breakdown zeroed. RollupByPrefix("") runs the
	// same `SELECT COUNT(*) FROM tracks` fast path CountTracks would, so
	// rollup.TrackCount IS the indexed-track total — a separate
	// CountTracks call was redundant AND opened a divergence window with
	// the SSE readStatsDBPart path (which sources TracksIndexed from the
	// same rollup).
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
		"TracksIndexed":       rollup.TrackCount,
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
		// Harmonic coverage, inherited from the retired /smartmixes
		// page. A key distribution is a fact about the library, like
		// the composition bars above it — not a control — so this is
		// where it belongs. Cheap: a GROUP BY on real
		// track_analysis columns, no json_extract.
		"KeyCoverage": s.keyCoverage(r.Context()),
	}
	s.renderPage(w, r, "stats", data)
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
		// Cumulative recovered-panic count from the scanner — the
		// "N files unreadable" hint PanickedCount's docblock promises
		// this page surfaces. Zero renders nothing.
		FilesUnreadable: s.deps.Scanner.PanickedCount(),
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
	s.renderPage(w, r, "library", data)
}

func (s *Server) pageDevices(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	tokens := s.deps.Auth.List()
	rows := make([]tokenRow, 0, len(tokens))
	for _, t := range tokens {
		// ExpiresAt + RotatedAt are as load-bearing as the rest: the
		// template renders "never" for a nil ExpiresAt, and `#tokens-body`
		// is never repainted from /api/tokens — the only client-side write
		// to `.expires-cell` is the in-session echo of the PATCH response.
		// Omitting them here (as this did until 2026-08-06) meant an expiry
		// set in one session reverted to "never" on the next page load,
		// while auth.Store went on enforcing it and the device stopped
		// working. apiTokensList already populates both; keep the two
		// projections in step.
		rows = append(rows, tokenRow{
			ID: t.ID, Name: t.Name,
			CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt,
			RotatedAt: t.RotatedAt, ExpiresAt: t.ExpiresAt,
			ClientVersion: t.LastClientVersion,
		})
	}
	data := map[string]any{
		"Tokens":     rows,
		"DefaultURL": defaultBridgeURL(cfg),
	}
	s.renderPage(w, r, "devices", data)
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
	s.renderPage(w, r, "upnp", data)
}

func (s *Server) pageSettings(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	// Shared builder — the single source of truth for every config-derived
	// settings field, so this server-rendered page and the JSON apiSettingsGet
	// can't diverge (they previously did: this handler omitted the enrich /
	// atlas / mDNS / Tailscale fields, so the General tab rendered them blank
	// and a Save would clobber them). Handler-specific sox hints layered below.
	data := settingsResponseFromConfig(cfg, s.deps.IsSupervised)
	// Live update status for the Updates panel (moved here from Stats).
	// Same nil-safe accessor the dashboard used; a bridge with no
	// updater wired renders the panel's empty state rather than
	// failing.
	upd := s.dashboardUpdateStatus()
	data.Update = &upd
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
	s.renderPage(w, r, "settings", data)
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
	// A future timestamp (clock skew between the bridge and whatever
	// stamped the row, NTP stepping, or sub-second rounding on a
	// just-written value) makes d negative — and every negative duration
	// satisfies `d < time.Minute`, so the dashboard would render "-5s
	// ago". Collapse the whole negative range to "just now".
	if d < 0 {
		return "just now"
	}
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

// playerRoutes are the player's client-side routes, registered
// server-side so a deep link (a bookmark, a shared URL, a reload) works
// on a cold load. Each renders the same shell; the module reads
// location.pathname and mounts the matching view.
//
// Registered as real routes rather than served from a hash fragment on
// purpose: TestTemplateHrefsResolveToRegisteredRoutes drops "#" hrefs,
// so a hash router would silently disable the guard that catches dead
// nav links.
var playerRoutes = []string{
	"/albums", "/artists", "/favorites", "/playlists", "/mixes",
	"/composers", "/genres", "/folders", "/search", "/tracks", "/sources",
	"/album/{id}", "/artist/{id}", "/genre/{id}", "/composer/{id}",
	"/playlist/{id}", "/mix/{slug}",
}

// playerPageData is the seed the shell hydrates from, so the first view
// paints without a redundant round-trip for the parts that are cheap.
type playerPageData struct {
	Section      string `json:"section"`
	ID           string `json:"id"`
	Query        string `json:"query"`
	AtlasEnabled bool   `json:"atlasEnabled"`
	MixesEnabled bool   `json:"mixesEnabled"`
	LibraryName  string `json:"libraryName"`
	// SourcesEnabled gates the Sources entry in the player rail. False
	// on a bridge with no UPnP upstreams configured, where the facet
	// would offer exactly one choice ("This bridge") and mean nothing —
	// unlike Smart Mixes, whose page is where its own switch lives,
	// there is nothing to go there for.
	SourcesEnabled bool `json:"sourcesEnabled"`
}

// pagePlayer renders the player shell for every player route.
func (s *Server) pagePlayer(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	section, id := playerSectionFor(r)
	s.renderPage(w, r, "player", playerPageData{
		Section:        section,
		ID:             id,
		Query:          r.URL.Query().Get("q"),
		AtlasEnabled:   cfg.Atlas.Enabled,
		MixesEnabled:   cfg.SmartPlaylists.EffectiveEnabled(),
		LibraryName:    cfg.LibraryName,
		SourcesEnabled: s.sourcesFacetWorthShowing(),
	})
}

// sourcesFacetWorthShowing reports whether this library actually draws on
// more than one place, which is the only case where a source facet says
// anything.
//
// ONE signal, deliberately, and it is not the config. apiPlayerSources
// emits an upstream row only for a source that HAS tracks — a row that
// filtered to nothing would be a dead end — so with no routed tracks the
// facet has exactly one row to show no matter what the config says. An
// earlier version also returned true for a configured upstream, on the
// reasoning that hiding the facet would hide something the operator had
// just set up; that put a rail entry in front of a page that then said
// nothing. Server -> UPnP is where a configured-but-unwalked upstream is
// visible, and it belongs there.
//
// Reading the library rather than the config also covers the case a
// config check gets backwards: routed rows OUTLIVE their config row.
// Removing the last upstream leaves the ingest with nothing to start, so
// its orphan sweep never runs and those tracks stay in the manifest
// indefinitely — exactly when the facet is the only surface that explains
// where they came from.
//
// The count comes from the cached stats part the dashboard already reads
// every 5s, not from a catalog build, which is the one thing on this path
// that can be slow on a cold snapshot.
func (s *Server) sourcesFacetWorthShowing() bool {
	_, routed := s.trackSourceCounts()
	return routed > 0
}

// playerSectionFor derives (section, id) from the request path. The
// wildcard value comes from r.PathValue, which is already
// percent-decoded per segment.
func playerSectionFor(r *http.Request) (section, id string) {
	p := strings.Trim(r.URL.Path, "/")
	if p == "" {
		return "albums", ""
	}
	head, _, _ := strings.Cut(p, "/")
	switch head {
	case "album", "artist", "genre", "composer", "playlist":
		return head, r.PathValue("id")
	case "mix":
		return "mix", r.PathValue("slug")
	default:
		return head, ""
	}
}

// notFound is the catch-all registered on "/". Two shapes, because two
// kinds of caller land here: an API client gets the same JSON error
// envelope every other admin endpoint returns, and a browser gets a real
// page with the nav intact.
//
// Written because guessing "/roots" and "/duplicates" from the sidebar
// labels — the real paths are /library and /library/duplicates — landed
// on unstyled black-on-white "404 page not found" with no way back. On a
// hosted bridge that page is what a stale bookmark or a mistyped URL
// reaches, so it is worth the twenty lines.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	// ActiveTab "notfound" matches no nav entry, so nothing is
	// highlighted — which is correct: the reader is nowhere.
	s.renderPageStatus(w, r, "notfound", http.StatusNotFound, map[string]any{"Path": r.URL.Path})
}
