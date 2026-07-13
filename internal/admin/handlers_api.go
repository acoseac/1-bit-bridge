package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	// Library composition — an honest breakdown of what the bridge
	// holds. TracksIndexed above is originals-only (the `tracks`
	// table never includes variants); these surface the variant
	// coverage so the dashboard can show Originals vs Upscaled vs
	// Optimized vs total variant files instead of a single ambiguous
	// "total tracks". TracksWith* are DISTINCT source-track counts
	// (a track with several upscaled targets counts once); VariantFiles
	// is the raw sidecar-file count. Populated from RollupByPrefix("")
	// + VariantStatsByKind.
	TracksWithUpscaled  int   `json:"tracksWithUpscaled"`
	TracksWithOptimized int   `json:"tracksWithOptimized"`
	VariantFiles        int   `json:"variantFiles"`
	VariantBytes        int64 `json:"variantBytes"`
	// UPnPRoutedTracks is the count of tracks in the manifest whose
	// bytes the bridge proxies from an upstream UPnP MediaServer (PR
	// #353 admin surface). Always emitted; zero when the feature
	// isn't enabled.
	UPnPRoutedTracks int    `json:"upnpRoutedTracks"`
	DBBytes          int64  `json:"dbBytes"`
	Fingerprint      string `json:"fingerprint"`
	DeviceCount      int    `json:"deviceCount"`
	ListenAddress    string `json:"listenAddress"`
	AdminAddress     string `json:"adminAddress"`
}

type rootRow struct {
	Path   string `json:"path"`
	Tracks int    `json:"tracks"`
}

// libraryRootRow is the Library *page* (HTML template) per-root row. It
// carries per-kind variant coverage that the JSON `/api/roots` shape
// (rootRow) deliberately does NOT — keeping the two separate so the
// roots API never emits zero-valued upscaled/optimized fields it doesn't
// populate (CodeRabbit on PR #340).
type libraryRootRow struct {
	Path            string
	Tracks          int
	UpscaledTracks  int
	OptimizedTracks int
	UpscaledBytes   int64
	OptimizedBytes  int64
}

// libraryPageData is the template payload for the Library page: the
// per-root rows plus a "Transcoded cache" summary (where variant
// sidecars live on disk + the global per-kind file count and size).
type libraryPageData struct {
	Roots             []libraryRootRow
	VariantsDir       string
	UpscaledVariants  int
	OptimizedVariants int
	UpscaledBytes     int64
	OptimizedBytes    int64
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
	// Audio analysis opt-in (waveform sidecars for the iOS scrubber).
	// Same restart-required contract as upscale. Shares the sox-missing
	// warning fields above (both features decode through sox).
	AnalysisEnabled bool `json:"analysisEnabled"`
	// Smart-playlist generation opt-in (the iOS Home tab's mixes). Same
	// restart-required contract — the daily regenerator is wired at
	// `bridge serve` startup.
	SmartPlaylistsEnabled bool `json:"smartPlaylistsEnabled"`
	// Enrich upstream base-URL overrides (from the `enrich` config block).
	// Empty = public MusicBrainz / Cover Art Archive defaults; point both at
	// a self-hosted Atlas mirror to keep enrichment on-network.
	// Restart-required.
	EnrichMusicBrainzBaseURL string `json:"enrichMusicBrainzBaseURL"`
	EnrichCoverArtBaseURL    string `json:"enrichCoverArtBaseURL"`
	// AtlasEnabled is the rich-tier Atlas metadata opt-in (bios / descriptions
	// / genres via the app ferry — distinct from the Enrich base URLs above,
	// which are the artwork + MusicBrainz source). Restart-required.
	AtlasEnabled bool `json:"atlasEnabled"`
	// EnrichSource / EnrichAtlasURL are template-only conveniences for the
	// Settings → Enrichment tab's source picker, DERIVED from the two base
	// URLs above by deriveEnrichSource (the URLs stay the single source of
	// truth — there is no separate `enrich.source` config field, so an
	// existing Atlas-pointed bridge.yaml auto-detects as "atlas" with zero
	// migration). `json:"-"` keeps the JSON API surface clean: programmatic
	// PATCH consumers keep sending the raw base URLs. Same pattern as
	// UpscaleSoxMissing.
	EnrichSource   string `json:"-"`
	EnrichAtlasURL string `json:"-"`
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
	// DLNAEnabled is the persisted cfg.DLNA.Enabled flag — the
	// operator opt-in for the UPnP/DLNA server. Restart-required
	// to take effect (the listener + SSDP advertisers are wired
	// at startup, same as upscaleEnabled).
	DLNAEnabled bool `json:"dlnaEnabled"`
	// DLNAListenAddress is the resolved TCP bind for the DLNA
	// HTTP server (default :7790), shown next to the toggle.
	DLNAListenAddress string `json:"dlnaListenAddress,omitempty"`
	// DLNABlockedByPublic is true when the deployment is in public
	// mode, where ShouldEnableDLNA force-disables the feature
	// regardless of the toggle (SSDP multicast has no meaning on a
	// public VPS). The UI shows a "not applicable in public mode"
	// caption so the operator isn't misled by a toggle that won't
	// take effect.
	DLNABlockedByPublic bool `json:"dlnaBlockedByPublic"`
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
	// UpscaleSoxHasFLAC reports whether the host sox build has FLAC
	// support. The bridge forces `-t flac` for every conversion, so a
	// sox WITHOUT FLAC passes the availability check but fails every
	// job at runtime. nil when not wired OR `sox --help` couldn't be
	// parsed (conservative — the UI doesn't assert a guess). Admin-only;
	// deliberately NOT on the public /v1/upscale/stats wire.
	UpscaleSoxHasFLAC *bool `json:"upscaleSoxHasFLAC,omitempty"`
	// UpscaleSoxFLACMissing is the template-only convenience boolean:
	// sox IS present but lacks FLAC (so the tile shows the format-fix
	// hint instead of the install hint). json:"-" keeps the JSON API's
	// FLAC signal on the UpscaleSoxHasFLAC tri-state.
	UpscaleSoxFLACMissing bool `json:"-"`
	// UpscaleSoxFormatHint is the OS-appropriate "reinstall sox with
	// FLAC" one-liner (runtime.GOOS resolves on the bridge host).
	// Empty unless UpscaleSoxFLACMissing. Template-only.
	UpscaleSoxFormatHint string `json:"-"`
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

// snapshotDBTimeout bounds the best-effort DB reads behind the dashboard
// stats / upscale / analysis SSE snapshots. These run from the
// per-connection SSE publisher (stats + composition via a
// context.Background(); the payloads are best-effort, not user-
// cancellable), so without a deadline a wedged read — SQLite lock
// contention past busy_timeout, a stalled volume — would block that
// publisher goroutine indefinitely and the connection couldn't even
// observe its own client-disconnect / shutdown until the read returned.
// modernc.org/sqlite honours context cancellation, so the deadline
// actually interrupts the query. 2s is far above the normal time for
// these COUNT / small-GROUP-BY reads, so a fire signals a genuine stall
// rather than a slow-but-valid query. (The heavier full-table
// composition scan gets its own, generous ceiling — see
// compositionDBTimeout.)
const snapshotDBTimeout = 2 * time.Second

// statsDBPart is the DB-derived subset of statsResponse, cached as a unit
// so a transient SQL error or snapshotDBTimeout during a tick serves the
// last good numbers instead of zeroing the dashboard tiles. Pre-fix the
// reads zeroed silently on error; this just makes the degrade graceful,
// the same way getCompositionSnapshot already serves its last-good
// composition on a FormatDistribution error.
type statsDBPart struct {
	tracks          int
	upscaledTracks  int
	optimizedTracks int
	variantFiles    int
	variantBytes    int64
	upnpRouted      int
}

// readStatsDBPart runs the three best-effort stats DB reads under ctx and
// returns them as a unit. Returns the FIRST error encountered (with a
// zero part) so the caller falls back to the cached last-good values as
// a whole rather than mixing a fresh field with stale siblings.
//
// The total track count comes from rollup.TrackCount: the global
// RollupByPrefix("") fast path runs `SELECT COUNT(*) FROM tracks` —
// byte-for-byte what CountTracks runs — so a separate CountTracks call
// would be a redundant 4th round-trip AND open a divergence window
// between the two queries under concurrent writes. One query keeps the
// total + variant rollup consistent. (Gemini on PR #443.)
func (s *Server) readStatsDBPart(ctx context.Context) (statsDBPart, error) {
	var p statsDBPart
	rollup, err := s.deps.Manifest.RollupByPrefix(ctx, "")
	if err != nil {
		return statsDBPart{}, fmt.Errorf("library rollup: %w", err)
	}
	byKind, err := s.deps.Manifest.VariantStatsByKind(ctx)
	if err != nil {
		return statsDBPart{}, fmt.Errorf("variant stats by kind: %w", err)
	}
	upnpRouted, err := s.deps.Manifest.CountUPnPRoutingTotal(ctx)
	if err != nil {
		return statsDBPart{}, fmt.Errorf("upnp routing count: %w", err)
	}
	for _, st := range byKind {
		p.variantFiles += st.Files
		p.variantBytes += st.Bytes
	}
	p.tracks = rollup.TrackCount
	p.upscaledTracks = rollup.UpscaledTrackCount
	p.optimizedTracks = rollup.OptimizedTrackCount
	p.upnpRouted = upnpRouted
	return p, nil
}

// getStatsSnapshot is the shared builder for the dashboard stats
// payload. Used by both the REST handler (apiStats) and the SSE
// handler (apiEvents). Cheap reads only — no DB writes, no external
// network. Suitable for sub-second polling. The DB reads are bounded by
// snapshotDBTimeout and degrade to the last-good statsDBPart on
// error/timeout so a tick during lock contention doesn't flash zeros.
func (s *Server) getStatsSnapshot() statsResponse {
	cfg := s.deps.CfgHolder.Load()
	now := time.Now().UTC()
	dbBytes := dbSize(filepath.Join(cfg.DataDir, "bridge.db"))

	// No request context here (the SSE publisher also calls this), so a
	// wedged read would otherwise block that goroutine indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), snapshotDBTimeout)
	defer cancel()
	part, err := s.readStatsDBPart(ctx)
	s.statsMu.Lock()
	if err != nil {
		logger.Warn("stats: db read degraded, serving last-good",
			"err", err, "haveCache", s.statsDBValid)
		if s.statsDBValid {
			part = s.statsDB
		}
	} else {
		s.statsDB = part
		s.statsDBValid = true
	}
	s.statsMu.Unlock()

	return statsResponse{
		LibraryName:         cfg.LibraryName,
		ProtocolVersion:     version.ProtocolVersion,
		ServerVersion:       version.ServerVersion,
		UptimeSec:           int64(now.Sub(s.deps.StartedAt).Seconds()),
		StartedAt:           s.deps.StartedAt,
		TracksIndexed:       part.tracks,
		IsScanning:          s.deps.Scanner.IsScanning(),
		ScanProgress:        s.deps.Scanner.ScanProgress(),
		LastFullScan:        s.deps.Scanner.LastFullScan(),
		TracksWithUpscaled:  part.upscaledTracks,
		TracksWithOptimized: part.optimizedTracks,
		VariantFiles:        part.variantFiles,
		VariantBytes:        part.variantBytes,
		UPnPRoutedTracks:    part.upnpRouted,
		DBBytes:             dbBytes,
		Fingerprint:         s.deps.Fingerprint,
		DeviceCount:         len(s.deps.Auth.List()),
		ListenAddress:       cfg.ListenAddress,
		AdminAddress:        cfg.AdminAddress,
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

// --- library composition (master-quality breakdown) ---

// compositionBar is one labelled segment of a distribution bar — a
// sampling-density tier, a DSD tier, or a codec. Admin-local wire DTO.
type compositionBar struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// compositionResponse is the dashboard "Library composition" master-
// quality breakdown: three distribution bars over the same track set.
// PCM + DSD partition the library (every track is in exactly one), so
// sum(PCM)+sum(DSD)==Total; Codecs is an orthogonal view that also sums
// to Total. Emitted on the SSE `composition` event, consumed by app.js
// applyComposition. Only non-zero buckets, in deterministic order, so
// the SSE byte-diff stays stable across idle ticks.
type compositionResponse struct {
	Total  int              `json:"total"`
	PCM    []compositionBar `json:"pcm"`
	DSD    []compositionBar `json:"dsd"`
	Codecs []compositionBar `json:"codecs"`
}

// pcmTierOrder / dsdTierOrder fix the display + wire order of the PCM
// sampling-density and DSD-rate tiers (deterministic SSE diff).
var (
	pcmTierOrder = []string{"44.1–48 kHz", "88.2–96 kHz", "176.4–192 kHz", "≥352.8 kHz (DXD)", "32-bit PCM", "Unknown"}
	dsdTierOrder = []string{"DSD64", "DSD128", "DSD256", "DSD512", "DSD (other)"}
)

// pcmTier classifies a non-DSD track by sampling density. Labels are
// rate-honest (no bit-depth claim a merged bucket could contradict);
// 32-bit PCM is called out separately and an unparseable rate lands in
// "Unknown" so the bars still reconcile to the library total.
func pcmTier(rate, bits int) string {
	switch {
	case rate <= 0:
		return "Unknown"
	case bits >= 32:
		return "32-bit PCM"
	case rate <= 48000:
		return "44.1–48 kHz"
	case rate <= 96000:
		return "88.2–96 kHz"
	case rate <= 192000:
		return "176.4–192 kHz"
	default:
		return "≥352.8 kHz (DXD)"
	}
}

// dsdTier classifies a DSD track by modulation rate. `>=` thresholds
// (not exact equality) tolerate minor rate variance and keep ordering.
func dsdTier(rate int) string {
	switch {
	case rate >= 22579200:
		return "DSD512"
	case rate >= 11289600:
		return "DSD256"
	case rate >= 5644800:
		return "DSD128"
	case rate >= 2822400:
		return "DSD64"
	default:
		return "DSD (other)"
	}
}

// buildComposition buckets raw FormatDistribution groups into the three
// distribution bars. Pure + deterministic (fixed tier order; codecs by
// count desc then label asc) so identical inputs marshal to byte-
// identical JSON for the SSE diff.
func buildComposition(groups []manifest.FormatGroup) compositionResponse {
	pcm := map[string]int{}
	dsd := map[string]int{}
	codecs := map[string]int{}
	total := 0
	for _, g := range groups {
		total += g.Count
		label := g.Codec
		if label == "" {
			label = "Unknown"
		}
		codecs[label] += g.Count
		if g.IsDSD {
			dsd[dsdTier(g.SampleRate)] += g.Count
			continue
		}
		pcm[pcmTier(g.SampleRate, g.BitsPerSample)] += g.Count
	}
	orderedBars := func(counts map[string]int, order []string) []compositionBar {
		bars := []compositionBar{}
		for _, label := range order {
			if n := counts[label]; n > 0 {
				bars = append(bars, compositionBar{Label: label, Count: n})
			}
		}
		return bars
	}
	codecBars := make([]compositionBar, 0, len(codecs))
	for label, n := range codecs {
		codecBars = append(codecBars, compositionBar{Label: label, Count: n})
	}
	slices.SortFunc(codecBars, func(a, b compositionBar) int {
		if a.Count != b.Count {
			return b.Count - a.Count // count desc
		}
		return strings.Compare(a.Label, b.Label) // label asc — deterministic tie-break
	})
	return compositionResponse{
		Total:  total,
		PCM:    orderedBars(pcm, pcmTierOrder),
		DSD:    orderedBars(dsd, dsdTierOrder),
		Codecs: codecBars,
	}
}

// compositionCacheTTL bounds how long a bucketed composition snapshot is
// reused before FormatDistribution is re-scanned. Format only changes
// after a scan, so 60s is generous and keeps the full-table json_extract
// off the SSE hot path.
const compositionCacheTTL = 60 * time.Second

// compositionDBTimeout is a deliberately generous ceiling on the
// full-table FormatDistribution scan — a hang-breaker, NOT a perf gate.
// Unlike the small stats reads (snapshotDBTimeout = 2s), this scan can
// legitimately take several seconds on a large library, so a tight
// timeout would false-trip and the composition tile would never
// populate. SQLite's 5s busy_timeout already bounds lock contention
// (the read errors and we serve last-good); this only breaks a
// pathological I/O hang that would otherwise wedge the singleflight
// leader — and every caller queued behind it — forever.
const compositionDBTimeout = 60 * time.Second

// getCompositionSnapshot returns the cached master-quality breakdown,
// recomputing via a full-table json_extract scan only when the cache is
// older than compositionCacheTTL. The recompute is single-flighted so N
// SSE connections hitting the 30s tick (or initial-emit) after expiry
// collapse to ONE scan, not N concurrent ones. Best-effort: a SQL error
// serves the last good snapshot rather than failing the tile.
func (s *Server) getCompositionSnapshot() compositionResponse {
	if s.deps.Manifest == nil {
		return compositionResponse{}
	}
	s.compositionMu.Lock()
	if !s.compositionAt.IsZero() && time.Since(s.compositionAt) < compositionCacheTTL {
		snap := s.composition
		s.compositionMu.Unlock()
		return snap
	}
	s.compositionMu.Unlock()
	v, _, _ := s.compositionSF.Do("composition", func() (any, error) {
		// Re-check under the flight: a prior flight may have refreshed
		// the cache while this caller was queued behind Do.
		s.compositionMu.Lock()
		if !s.compositionAt.IsZero() && time.Since(s.compositionAt) < compositionCacheTTL {
			snap := s.composition
			s.compositionMu.Unlock()
			return snap, nil
		}
		s.compositionMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), compositionDBTimeout)
		defer cancel()
		groups, err := s.deps.Manifest.FormatDistribution(ctx)
		if err != nil {
			logger.Warn("composition: format distribution", "err", err)
			s.compositionMu.Lock()
			snap := s.composition // last good (possibly zero)
			s.compositionMu.Unlock()
			return snap, nil
		}
		snap := buildComposition(groups)
		s.compositionMu.Lock()
		s.composition = snap
		s.compositionAt = time.Now()
		s.compositionMu.Unlock()
		return snap, nil
	})
	if snap, ok := v.(compositionResponse); ok {
		return snap
	}
	return compositionResponse{}
}

// --- enrichment progress (dashboard legibility) ---

// enrichmentResponse is the dashboard "Enrichment" card payload: the derived
// pending / matched / missing split + last-enriched time + a coarse ETA. All of
// it is derived from existing columns (no enrich_status column, no migration) —
// see manifest.Store.EnrichmentBreakdown. Emitted on the SSE `enrichment` event
// + served by GET /api/enrichment. Admin-local wire DTO.
type enrichmentResponse struct {
	Pending            int        `json:"pending"`
	Matched            int        `json:"matched"`
	Missing            int        `json:"missing"`
	LastEnrichedAt     *time.Time `json:"lastEnrichedAt,omitempty"`
	EtaSecondsEstimate int64      `json:"etaSecondsEstimate"`
	// Source labels which enrichment upstream this bridge queries —
	// "musicbrainz" / "atlas" / "custom", derived from the live config by
	// deriveEnrichSource. Config-derived and stable, so it never churns the
	// SSE byte-diff.
	Source string `json:"source,omitempty"`
	// Coverage stats for the rich-tier facets, each nil (omitted) when the
	// backing data source isn't wired: artist images need the
	// Deps.ArtistImageMBIDs closure; bios/descriptions come from the
	// artist_atlas / release_atlas tables. All three ride the 60s
	// enrichment-meta cache (getEnrichmentMetaSnapshot).
	ArtistImages      *coverageCounts `json:"artistImages,omitempty"`
	ArtistBios        *coverageCounts `json:"artistBios,omitempty"`
	AlbumDescriptions *coverageCounts `json:"albumDescriptions,omitempty"`
	// Booklets reports PDF album booklets known available upstream vs
	// already cached on the bridge's disk. Nil (omitted) on bridges
	// without the booklet wiring or when none are available yet.
	Booklets *bookletCounts `json:"booklets,omitempty"`
}

// bookletCounts is the available/cached pair for the booklet stat row.
type bookletCounts struct {
	Available int `json:"available"`
	Cached    int `json:"cached"`
}

// coverageCounts is a have/missing pair for one enrichment facet
// (admin-local wire DTO — rendered as "N have · M missing" on the card).
type coverageCounts struct {
	Have    int `json:"have"`
	Missing int `json:"missing"`
}

// enrichmentMetaPart is the cached slow half of the enrichment card: the
// coverage stats whose recompute costs a full-table CTE + an os.ReadDir.
type enrichmentMetaPart struct {
	ArtistImages      *coverageCounts
	ArtistBios        *coverageCounts
	AlbumDescriptions *coverageCounts
	Booklets          *bookletCounts
}

// Enrichment ETA is a deliberately rough reassurance number, not a promise. The
// single-goroutine enricher pays the MusicBrainz pace (MBMinInterval, ~1.1s)
// once per ALBUM, then sibling tracks on that album hit the in-memory albumCache
// for free — so `pending * 1.1s` overshoots by ~the average tracks-per-album.
// Dividing by avgTracksPerAlbum lands the estimate in the right order of
// magnitude (a 50k fresh scan → ~1.5h, not the ~15h a per-track multiply shows).
// It still ignores CAA/iTunes/Deezer pacing, so treat it as a ballpark.
const (
	avgTracksPerAlbum = 10.0
	enrichPaceSeconds = 1.1
)

// enrichmentCacheTTL bounds how long an EnrichmentBreakdown snapshot is reused.
// Short enough to visibly watch the queue drain on the 30s SSE tick, long enough
// to keep the full-table json_extract scan off the hot path (and to collapse N
// open tabs to one scan via the singleflight).
const enrichmentCacheTTL = 15 * time.Second

// enrichmentDBTimeout is a generous hang-breaker on the full-table scan, NOT a
// perf gate — same rationale as compositionDBTimeout. The matched/missing split
// is a full-table json_extract that legitimately takes seconds on a large
// library, so a tight timeout would false-trip and the card would never
// populate. The read holds no s.mu (WAL concurrent-reader), so it can't stall a
// writer regardless; this only breaks a pathological I/O hang that would
// otherwise wedge the singleflight leader and everyone queued behind it.
const enrichmentDBTimeout = 60 * time.Second

// getEnrichmentSnapshot composes the full enrichment-card payload: the
// 15s-cached pending/matched/missing breakdown, the config-derived source
// label, and the 60s-cached coverage stats. Every input is cached, so the
// marshalled bytes only change when data changes — SSE diff-suppression
// stays stable.
func (s *Server) getEnrichmentSnapshot() enrichmentResponse {
	snap := s.getEnrichmentBreakdownPart()
	if cfg := s.deps.CfgHolder.Load(); cfg != nil {
		snap.Source, _ = deriveEnrichSource(cfg.Enrich.MusicBrainzBaseURL, cfg.Enrich.CoverArtBaseURL)
	}
	meta := s.getEnrichmentMetaSnapshot()
	snap.ArtistImages = meta.ArtistImages
	snap.ArtistBios = meta.ArtistBios
	snap.AlbumDescriptions = meta.AlbumDescriptions
	snap.Booklets = meta.Booklets
	return snap
}

// getEnrichmentBreakdownPart returns the cached enrichment breakdown, recomputing
// via a full-table scan only when the cache is older than enrichmentCacheTTL. The
// recompute is single-flighted so concurrent SSE ticks / initial-emits collapse
// to ONE scan. Best-effort: a SQL error serves the last good snapshot rather
// than failing the card. Mirrors getCompositionSnapshot.
func (s *Server) getEnrichmentBreakdownPart() enrichmentResponse {
	if s.deps.Manifest == nil {
		return enrichmentResponse{}
	}
	s.enrichmentMu.Lock()
	if !s.enrichmentAt.IsZero() && time.Since(s.enrichmentAt) < enrichmentCacheTTL {
		snap := s.enrichment
		s.enrichmentMu.Unlock()
		return snap
	}
	s.enrichmentMu.Unlock()
	v, _, _ := s.enrichmentSF.Do("enrichment", func() (any, error) {
		// Re-check under the flight: a prior flight may have refreshed the
		// cache while this caller was queued behind Do.
		s.enrichmentMu.Lock()
		if !s.enrichmentAt.IsZero() && time.Since(s.enrichmentAt) < enrichmentCacheTTL {
			snap := s.enrichment
			s.enrichmentMu.Unlock()
			return snap, nil
		}
		s.enrichmentMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), enrichmentDBTimeout)
		defer cancel()
		pending, matched, missing, last, err := s.deps.Manifest.EnrichmentBreakdown(ctx)
		if err != nil {
			logger.Warn("enrichment: breakdown", "err", err)
			s.enrichmentMu.Lock()
			snap := s.enrichment // last good (possibly zero)
			s.enrichmentMu.Unlock()
			return snap, nil
		}
		snap := enrichmentResponse{
			Pending:            pending,
			Matched:            matched,
			Missing:            missing,
			LastEnrichedAt:     last,
			EtaSecondsEstimate: int64(math.Round((float64(pending) / avgTracksPerAlbum) * enrichPaceSeconds)),
		}
		s.enrichmentMu.Lock()
		s.enrichment = snap
		s.enrichmentAt = time.Now()
		s.enrichmentMu.Unlock()
		return snap, nil
	})
	if snap, ok := v.(enrichmentResponse); ok {
		return snap
	}
	return enrichmentResponse{}
}

// enrichmentMetaCacheTTL bounds how long the coverage stats (artist images /
// bios / album descriptions) are reused. The recompute is a full-table CTE +
// one os.ReadDir — composition-class cost, so composition-class TTL (60s).
const enrichmentMetaCacheTTL = 60 * time.Second

// getEnrichmentMetaSnapshot returns the cached coverage stats, recomputing
// only when older than enrichmentMetaCacheTTL. Single-flighted + last-good
// on error, mirroring getCompositionSnapshot. Facets whose data source is
// unavailable stay nil so the wire field is omitted rather than zero-lying.
func (s *Server) getEnrichmentMetaSnapshot() enrichmentMetaPart {
	if s.deps.Manifest == nil {
		return enrichmentMetaPart{}
	}
	s.enrichmentMetaMu.Lock()
	if !s.enrichmentMetaAt.IsZero() && time.Since(s.enrichmentMetaAt) < enrichmentMetaCacheTTL {
		snap := s.enrichmentMeta
		s.enrichmentMetaMu.Unlock()
		return snap
	}
	s.enrichmentMetaMu.Unlock()
	v, _, _ := s.enrichmentMetaSF.Do("enrichment-meta", func() (any, error) {
		// Re-check under the flight: a prior flight may have refreshed the
		// cache while this caller was queued behind Do.
		s.enrichmentMetaMu.Lock()
		if !s.enrichmentMetaAt.IsZero() && time.Since(s.enrichmentMetaAt) < enrichmentMetaCacheTTL {
			snap := s.enrichmentMeta
			s.enrichmentMetaMu.Unlock()
			return snap, nil
		}
		s.enrichmentMetaMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), enrichmentDBTimeout)
		defer cancel()
		var snap enrichmentMetaPart
		b, err := s.deps.Manifest.AtlasMetaBreakdownCounts(ctx)
		if err != nil {
			logger.Warn("enrichment: atlas-meta breakdown", "err", err)
			s.enrichmentMetaMu.Lock()
			snap = s.enrichmentMeta // last good (possibly zero)
			s.enrichmentMetaMu.Unlock()
			return snap, nil
		}
		// A facet whose MBID universe is empty (nothing enriched yet, or a
		// library with no MB matches) stays nil so the card doesn't render
		// noise rows of "0 have · 0 missing".
		if b.ArtistsTotal > 0 {
			snap.ArtistBios = &coverageCounts{Have: b.ArtistBiosFound, Missing: b.ArtistsTotal - b.ArtistBiosFound}
		}
		if b.ReleasesTotal > 0 {
			snap.AlbumDescriptions = &coverageCounts{Have: b.ReleaseDescsFound, Missing: b.ReleasesTotal - b.ReleaseDescsFound}
		}
		snap.ArtistImages = s.artistImageCoverage(ctx)
		// Booklets (v1.8): cheap two-COUNT read; omitted while nothing is
		// available (unwired bridges keep an empty table → nil facet).
		if avail, cached, berr := s.deps.Manifest.BookletCounts(ctx); berr != nil {
			logger.Warn("enrichment: booklet counts", "err", berr)
		} else if avail > 0 {
			snap.Booklets = &bookletCounts{Available: avail, Cached: cached}
		}
		s.enrichmentMetaMu.Lock()
		s.enrichmentMeta = snap
		s.enrichmentMetaAt = time.Now()
		s.enrichmentMetaMu.Unlock()
		return snap, nil
	})
	if snap, ok := v.(enrichmentMetaPart); ok {
		return snap
	}
	return enrichmentMetaPart{}
}

// artistImageCoverage intersects the library's distinct artist MBIDs with the
// on-disk artist-image cache (via the nil-safe Deps.ArtistImageMBIDs closure).
// Returns nil (facet omitted) when the closure isn't wired or either read
// fails — never a zero-lying pair. Only called inside the enrichment-meta
// flight, so both reads ride the 60s TTL.
func (s *Server) artistImageCoverage(ctx context.Context) *coverageCounts {
	if s.deps.ArtistImageMBIDs == nil {
		return nil
	}
	files, err := s.deps.ArtistImageMBIDs()
	if err != nil {
		logger.Warn("enrichment: artist image dir", "err", err)
		return nil
	}
	mbids, err := s.deps.Manifest.DistinctArtistMBIDs(ctx)
	if err != nil {
		logger.Warn("enrichment: distinct artist mbids", "err", err)
		return nil
	}
	if len(mbids) == 0 {
		return nil // empty universe — omit rather than render "0 have · 0 missing"
	}
	have := 0
	for _, m := range mbids {
		if _, ok := files[strings.ToLower(m)]; ok {
			have++
		}
	}
	return &coverageCounts{Have: have, Missing: len(mbids) - have}
}

// apiEnrichment serves GET /api/enrichment — the REST twin of the `enrichment`
// SSE event (same getEnrichmentSnapshot source), for scripting / first paint.
func (s *Server) apiEnrichment(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.getEnrichmentSnapshot())
}

// enrichRetryMinInterval is the POST /api/enrichment/retry rate guard: a
// second click inside the window is refused with 429. The underlying resets
// are idempotent — the guard just keeps a panic-clicked button from
// repeatedly re-nudging the enricher + harvest submit.
const enrichRetryMinInterval = 60 * time.Second

// enrichmentRetryResponse reports what the retry actually did: how many
// tracks were re-queued for the enricher, and whether the Atlas harvest
// client was nudged into a full re-submit (false when harvest isn't wired).
type enrichmentRetryResponse struct {
	ResetTracks        int64 `json:"resetTracks"`
	HarvestResubmitted bool  `json:"harvestResubmitted"`
}

// apiEnrichmentRetry handles POST /api/enrichment/retry — the dashboard's
// "Retry missing" button. Three facets, each via its correct mechanism:
//
//  1. Tracks enriched without artwork or an artist match get enriched_at
//     reset (ResetEnrichedMisses) so the enricher worker re-runs them.
//  2. Tracks whose artist RESOLVED but whose cached artist image file is
//     missing on disk get the same reset via the MBID-set overload — the
//     file check can't be expressed in SQL, so the missing set is computed
//     here from the artwork cache dir.
//  3. Bios / album descriptions are NOT enricher-owned (they arrive via the
//     Atlas harvest results), so their retry is a forced full re-submit on
//     the harvest client's next tick (HarvestForceSubmit, nil-safe).
func (s *Server) apiEnrichmentRetry(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "not-wired", "manifest store not available")
		return
	}
	s.enrichRetryMu.Lock()
	if !s.enrichRetryAt.IsZero() && time.Since(s.enrichRetryAt) < enrichRetryMinInterval {
		s.enrichRetryMu.Unlock()
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"retry already triggered — wait a minute before retrying again")
		return
	}
	s.enrichRetryAt = time.Now()
	s.enrichRetryMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), enrichmentDBTimeout)
	defer cancel()
	reset, err := s.deps.Manifest.ResetEnrichedMisses(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reset-failed", err.Error())
		return
	}
	// Facet 2: artist image gaps (extracted helper — see its doc).
	reset += s.resetArtistImageGaps(ctx)
	resubmitted := false
	if s.deps.HarvestForceSubmit != nil {
		resubmitted = s.deps.HarvestForceSubmit()
	}
	// Invalidate the breakdown cache so the pending-count jump lands on the
	// next SSE tick instead of after the 15s TTL.
	s.enrichmentMu.Lock()
	s.enrichmentAt = time.Time{}
	s.enrichmentMu.Unlock()
	logger.Info("enrichment retry triggered", "resetTracks", reset, "harvestResubmitted", resubmitted)
	writeJSON(w, http.StatusOK, enrichmentRetryResponse{
		ResetTracks:        reset,
		HarvestResubmitted: resubmitted,
	})
}

// resetArtistImageGaps re-queues enriched tracks whose resolved artist lacks
// a cached image file — one dir read + one distinct-MBID query computing the
// missing set directly (calling artistImageCoverage first would duplicate
// both reads; Gemini on PR #495). Best-effort: any failure degrades to 0
// (covers-only retry) rather than failing the caller's request.
// ResetEnrichedByArtistMBIDs no-ops on an empty set.
func (s *Server) resetArtistImageGaps(ctx context.Context) int64 {
	if s.deps.ArtistImageMBIDs == nil {
		return 0
	}
	files, err := s.deps.ArtistImageMBIDs()
	if err != nil {
		return 0
	}
	mbids, err := s.deps.Manifest.DistinctArtistMBIDs(ctx)
	if err != nil {
		return 0
	}
	var missing []string
	for _, m := range mbids {
		if _, ok := files[strings.ToLower(m)]; !ok {
			missing = append(missing, m)
		}
	}
	n, err := s.deps.Manifest.ResetEnrichedByArtistMBIDs(ctx, missing)
	if err != nil {
		logger.Warn("enrichment retry: artist-image reset", "err", err)
	}
	return n
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

// normalizeRootPathReq trims + absolutizes a library-root path from an
// admin roots request. On a rejectable input (empty, or unresolvable by
// filepath.Abs) it writes the error response and returns ok=false so the
// caller returns immediately. Shared by apiRootsAdd + apiRootsRemove so
// both agree on the canonical absolute form Scanner.Roots() stores — a
// mismatch there makes remove false-trip a 404 against an added root.
// filepath.Abs("") resolves to the process CWD, so rejecting empty is
// load-bearing, not cosmetic.
func normalizeRootPathReq(w http.ResponseWriter, raw string) (abs string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		writeError(w, http.StatusBadRequest, "path-required", "path must not be empty")
		return "", false
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad-path", err.Error())
		return "", false
	}
	return abs, true
}

func (s *Server) apiRootsAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errCodeBadJSON, err.Error())
		return
	}
	abs, ok := normalizeRootPathReq(w, req.Path)
	if !ok {
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
	// (bare "Artist/…" vs "<basename>/Artist/…"), so filesystem tracks
	// have to be wiped and re-populated from a fresh scan.
	// WipeFilesystemTracks SPARES UPnP-routed rows — their "<server>/…"
	// form is independent of the FS root count, and a bare WipeAllTracks
	// here would CASCADE-destroy the entire upstream library + its cached
	// enrichment, forcing a full re-ingest.
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
		if err := s.deps.Manifest.WipeFilesystemTracks(r.Context()); err != nil {
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
		// branch, WipeFilesystemTracks has already cleared the
		// filesystem rows (UPnP-routed rows were spared) but the config
		// was never persisted — /v1/manifest will serve only the
		// upstream tracks until the next scheduled/manual scan. Kick off
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

	// Normalize identically to apiRootsAdd (shared helper) so the cleaned
	// absolute form matches what Scanner.Roots() stores — otherwise a
	// relative / untrimmed / trailing-slash input (e.g. "/Music/" or
	// " ./Music") false-trips the slices.Index 404 below.
	abs, ok := normalizeRootPathReq(w, req.Path)
	if !ok {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.deps.CfgHolder.Load()

	current := s.deps.Scanner.Roots()
	idx := slices.Index(current, abs)
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
	// single-root, the storage form flips again so EVERY filesystem row
	// must be re-derived — use WipeFilesystemTracks (which spares
	// UPnP-routed rows) rather than a prefix delete.
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
		if err := s.deps.Manifest.WipeFilesystemTracks(r.Context()); err != nil {
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
		req.URL = defaultBridgeURL(cfg)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rawToken, tok, err := s.deps.Auth.Mint(req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mint", err.Error())
		return
	}
	// Bake the fingerprint the device will actually capture when it dials
	// req.URL (public-domain LE cert for a public dial URL, self-signed
	// otherwise) so the iOS first-contact pin check can't reject pairing.
	fp := pairFingerprint(req.URL, s.deps.Fingerprint, s.deps.FingerprintForHost)
	// alternates baked into the pairing QR so iOS learns every
	// reachable endpoint (LAN IPv4/IPv6, `.local`, Tailscale) at the
	// moment of pairing. Empty slice if enumeration fails — the
	// operator-supplied primary URL is always the first entry, so the
	// QR always pairs even on an interface-less environment.
	alternates := ensurePrimaryFirst(req.URL, pairAlternates(req.URL, cfg))
	pairURL := buildPairURL(req.URL, rawToken, fp, cfg.LibraryName, alternates)
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
		Fingerprint: fp,
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

// settingsResponseFromConfig builds the config-derived portion of the
// settings payload shared by the JSON API (apiSettingsGet) and the
// server-rendered page (pageSettings) — the single source of truth for every
// field that mirrors the live config, so the two handlers can't drift. They
// previously did: pageSettings silently omitted the enrich / atlas / mDNS /
// Tailscale fields, so the Settings → General tab rendered them blank (the
// enrich URLs showed only their public-default placeholders; rich-metadata +
// mDNS unchecked; Tailscale mode unselected) and a Save would clobber those
// config values back to defaults. Handler-specific extras — the
// sox-availability bool + the install/format hint strings — are layered on by
// each caller afterward.
func settingsResponseFromConfig(cfg *config.Config, isSupervised bool) settingsResponse {
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
		AnalysisEnabled:          cfg.Analysis.Enabled,
		SmartPlaylistsEnabled:    cfg.SmartPlaylists.Enabled,
		EnrichMusicBrainzBaseURL: cfg.Enrich.MusicBrainzBaseURL,
		EnrichCoverArtBaseURL:    cfg.Enrich.CoverArtBaseURL,
		AtlasEnabled:             cfg.Atlas.Enabled,
		IsSupervised:             isSupervised,
		BackupIntervalHours:      cfg.Backup.EffectiveIntervalHours(),
		BackupKeep:               cfg.Backup.EffectiveKeep(),
		MDNSEnabled:              cfg.EffectiveMDNSEnabled(),
		IsPublic:                 cfg.IsPublic(),
		DLNAEnabled:              cfg.DLNA.Enabled,
		DLNAListenAddress:        cfg.DLNA.EffectiveDLNAListenAddress(),
		DLNABlockedByPublic:      cfg.IsPublic(),
	}
	// Tailscale mode: tolerate an unknown YAML value by falling back to the
	// effective default so the UI shows a recognizable selection even if
	// Validate is currently rejecting the config (defensive).
	if tm, err := cfg.Tailscale.EffectiveMode(); err == nil {
		resp.TailscaleMode = string(tm)
	} else {
		resp.TailscaleMode = string(config.TailscaleModeCLI)
	}
	resp.EnrichSource, resp.EnrichAtlasURL = deriveEnrichSource(
		cfg.Enrich.MusicBrainzBaseURL, cfg.Enrich.CoverArtBaseURL)
	return resp
}

// Enrichment-source labels for the Settings picker + the dashboard card.
// "musicbrainz" = both base URLs empty (public defaults); "atlas" = the
// canonical Atlas-mirror shape (MB = <atlas>/ws/2, CoverArt = <atlas>);
// anything else = "custom" (hand-tuned mirrors — the Advanced fields).
const (
	enrichSourceMusicBrainz = "musicbrainz"
	enrichSourceAtlas       = "atlas"
	enrichSourceCustom      = "custom"
)

// deriveEnrichSource classifies the stored enrich base URLs into the
// Settings picker's source state, returning the Atlas base URL when the
// shape matches. Both inputs are trailing-slash-trimmed defensively —
// applyEnrichBase and Config.Validate already normalize persisted values,
// but env-var overrides (BRIDGE_MUSICBRAINZ_BASE_URL) reach the live config
// unnormalized and must not drop the operator into "custom" over a slash.
func deriveEnrichSource(mbBase, caBase string) (source, atlasURL string) {
	mb := strings.TrimRight(strings.TrimSpace(mbBase), "/")
	ca := strings.TrimRight(strings.TrimSpace(caBase), "/")
	switch {
	case mb == "" && ca == "":
		return enrichSourceMusicBrainz, ""
	case ca != "" && strings.HasSuffix(mb, "/ws/2") && strings.TrimSuffix(mb, "/ws/2") == ca:
		return enrichSourceAtlas, ca
	default:
		return enrichSourceCustom, ""
	}
}

func (s *Server) apiSettingsGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	resp := settingsResponseFromConfig(cfg, s.deps.IsSupervised)
	// Probe sox availability so the Settings UI can warn before the operator
	// enables the feature. Cheap (LookPath + a 2 s --version check inside
	// PrecheckSox); logged-but-swallowed on failure. The JSON API needs only
	// the bool — the page render adds the install/format hint strings.
	if s.deps.UpscalePrecheck != nil {
		ok := s.deps.UpscalePrecheck() == nil
		resp.UpscaleSoxAvailable = &ok
	}
	if hasFLAC, known := s.soxFLACStatus(); known {
		resp.UpscaleSoxHasFLAC = &hasFLAC
	}
	writeJSON(w, http.StatusOK, resp)
}

// soxFLACStatus returns whether the host sox build has FLAC support via the
// nil-safe UpscaleSoxFLAC closure. known is false when the closure isn't
// wired (test harness) OR `sox --help` couldn't be parsed — callers then
// omit the FLAC field rather than asserting a guess.
func (s *Server) soxFLACStatus() (hasFLAC, known bool) {
	if s.deps.UpscaleSoxFLAC == nil {
		return false, false
	}
	return s.deps.UpscaleSoxFLAC()
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
	// Audio analysis opt-in. Same restart-required contract as
	// upscale — the serve-side `waveform` health flag + /v1/waveform
	// wiring are decided once at startup.
	AnalysisEnabled *bool `json:"analysisEnabled,omitempty"`
	// Smart-playlist generation opt-in. Restart-required: the daily
	// regenerator goroutine is launched once at `bridge serve` startup
	// (same rationale as UpscaleEnabled / AnalysisEnabled).
	SmartPlaylistsEnabled *bool `json:"smartPlaylistsEnabled,omitempty"`
	// Enrich upstream base-URL overrides. Restart-required: the enricher's
	// MB / Cover Art clients are constructed once at `bridge serve` startup.
	// Config.Validate normalizes (trailing slash) + validates (absolute
	// http/https) these before save.
	EnrichMusicBrainzBaseURL *string `json:"enrichMusicBrainzBaseURL,omitempty"`
	EnrichCoverArtBaseURL    *string `json:"enrichCoverArtBaseURL,omitempty"`
	// AtlasEnabled is the rich-tier Atlas metadata opt-in. Restart-required:
	// the /v1/atlas-ingest + /v1/atlas-meta routes and the atlasEnrichment
	// health flag are wired once at `bridge serve` startup.
	AtlasEnabled *bool `json:"atlasEnabled,omitempty"`
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
	// DLNAEnabled toggles the UPnP/DLNA server. Restart-required:
	// the listener + SSDP advertisers are wired once at `bridge
	// serve` startup (same rationale as UpscaleEnabled).
	DLNAEnabled *bool `json:"dlnaEnabled,omitempty"`
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
	if p.AnalysisEnabled != nil {
		if *p.AnalysisEnabled != next.Analysis.Enabled {
			next.Analysis.Enabled = *p.AnalysisEnabled
			// Same rationale as upscale: the serve-side `waveform`
			// health flag + /v1/waveform wiring are decided once at
			// startup, so a runtime flip needs a restart to take
			// effect. Idempotent same-value submissions skip the banner.
			restart = true
		}
	}
	if p.SmartPlaylistsEnabled != nil {
		if *p.SmartPlaylistsEnabled != next.SmartPlaylists.Enabled {
			next.SmartPlaylists.Enabled = *p.SmartPlaylistsEnabled
			// The daily smart-playlist regenerator goroutine is launched
			// once at startup (cmd/bridge/main.go), so a runtime flip
			// needs a restart. Idempotent same-value submissions skip the
			// banner.
			restart = true
		}
	}
	// Enrich upstream base URLs (#406's config). Trim to match
	// normalizeBaseURL so a re-submit of the stored value doesn't spuriously
	// flag a restart; Config.Validate() below does the authoritative
	// normalize + http(s) validation. Restart-required (clients wired once).
	applyEnrichBase := func(in *string, dst *string) {
		if in == nil {
			return
		}
		if v := strings.TrimRight(strings.TrimSpace(*in), "/"); v != *dst {
			*dst = v
			restart = true
		}
	}
	applyEnrichBase(p.EnrichMusicBrainzBaseURL, &next.Enrich.MusicBrainzBaseURL)
	applyEnrichBase(p.EnrichCoverArtBaseURL, &next.Enrich.CoverArtBaseURL)
	if p.AtlasEnabled != nil {
		if *p.AtlasEnabled != next.Atlas.Enabled {
			next.Atlas.Enabled = *p.AtlasEnabled
			// Restart-required: the /v1/atlas-ingest + /v1/atlas-meta routes
			// and the atlasEnrichment health flag are wired once at startup.
			// Idempotent same-value submits skip the banner.
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
	if p.DLNAEnabled != nil {
		if *p.DLNAEnabled != next.DLNA.Enabled {
			next.DLNA.Enabled = *p.DLNAEnabled
			// The DLNA HTTP listener + SSDP advertisers bind once at
			// `bridge serve` startup (dlna_wiring.startDLNAIfEnabled).
			// A live flip would have to spin up / tear down the
			// listener and the per-interface SSDP advertisers — same
			// startup-wired shape as upscaleEnabled, so restart-required
			// is the honest answer rather than a partial hot-apply.
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
	// Per-kind split of CachedVariants / CachedBytes. The combined
	// totals above conflated upscale + optimize variants — a library
	// with only CarPlay-optimize sidecars still showed them all under
	// "cached variants", reading as upscaled work that never happened.
	// These four fields let the Settings → Audio quality tile show
	// "Upscaled: N (X)" and "Optimized: M (Y)" honestly; they sum to
	// the combined totals. Populated from `Store.VariantStatsByKind`.
	UpscaledVariants  int   `json:"upscaledVariants"`
	UpscaledBytes     int64 `json:"upscaledBytes"`
	OptimizedVariants int   `json:"optimizedVariants"`
	OptimizedBytes    int64 `json:"optimizedBytes"`
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
	writeJSON(w, http.StatusOK, s.getUpscaleStatsSnapshot(r.Context()))
}

// getUpscaleStatsSnapshot builds the upscale runtime + on-disk snapshot
// shared by the REST handler (apiUpscaleStats) and the SSE `upscale`
// event publisher — the single source of truth, like getStatsSnapshot ↔
// getStatsSSESnapshot. See apiUpscaleStats' doc for the live-vs-persisted
// `enabled` semantics. Cheap: one VariantStatsByKind GROUP BY (small,
// PK-planned table) + a mutex-guarded pool snapshot + the TTL-cached sox
// precheck. No monotonic-per-tick field, so it diff-suppresses cleanly on
// an idle bridge (Enqueued/Done/Failed only move when jobs run).
func (s *Server) getUpscaleStatsSnapshot(ctx context.Context) upscaleStatsResponse {
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
		// Bound the read so a wedged query can't hold the SSE publisher
		// past snapshotDBTimeout — the connection ctx alone only cancels
		// on client disconnect, not on a slow/stalled query.
		dbCtx, cancel := context.WithTimeout(ctx, snapshotDBTimeout)
		defer cancel()
		// Per-kind split drives both the combined totals (back-compat)
		// and the honest "Upscaled / Optimized" breakdown. One query
		// instead of the prior kind-agnostic CountVariants.
		byKind, err := s.deps.Manifest.VariantStatsByKind(dbCtx)
		if err != nil {
			// Log + degrade: caller still gets the live fields. A SQL
			// failure here should be visible in logs but not turn the
			// whole tile into an error state.
			logger.Warn("upscale stats: variant stats by kind", "err", err)
		} else {
			up := byKind["upscale"]
			opt := byKind["optimize"]
			resp.UpscaledVariants = up.Files
			resp.UpscaledBytes = up.Bytes
			resp.OptimizedVariants = opt.Files
			resp.OptimizedBytes = opt.Bytes
			// Combined totals sum every kind (including a stray
			// "unknown" bucket) so the back-compat fields still
			// account for all rows the way CountVariants did.
			for _, st := range byKind {
				resp.CachedVariants += st.Files
				resp.CachedBytes += st.Bytes
			}
		}
	}
	return resp
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

	// Fast path: serve a fresh cached value, then release the lock. The
	// unlocks are EXPLICIT (no defer) because the whole point is to run
	// UpscalePrecheck() UNLOCKED — it can shell out to `sox --help` for
	// up to 2 s, and cachedSoxAvailability is called on the SSE snapshot
	// path (getUpscaleStatsSnapshot / getAnalysisStatsSnapshot). A
	// deferred unlock would hold soxAvailabilityMu across that probe and
	// block every concurrent SSE connection / Settings tab.
	s.soxAvailabilityMu.Lock()
	if !s.soxAvailabilityAt.IsZero() && now.Sub(s.soxAvailabilityAt) < soxAvailabilityCacheTTL {
		v := s.soxAvailability
		s.soxAvailabilityMu.Unlock()
		return &v
	}
	s.soxAvailabilityMu.Unlock()

	// Probe unlocked. Concurrent cache-miss callers may each invoke
	// UpscalePrecheck, but the wired soxToolchainCache (cmd/bridge, its
	// own mutex + TTL) dedupes the actual exec — at most one real
	// `sox --help` runs; the rest are warm-cache hits.
	v := s.deps.UpscalePrecheck() == nil

	s.soxAvailabilityMu.Lock()
	s.soxAvailability = v
	s.soxAvailabilityAt = time.Now() // fresh timestamp captured post-probe
	s.soxAvailabilityMu.Unlock()
	return &v
}

// analysisStatsResponse is the JSON shape /api/analysis/stats returns.
// Simpler than upscaleStatsResponse: audio-analysis generation is
// CLI-driven (`bridge analyze`), so there's no long-lived serve-side
// pool to snapshot — just the enabled gate, sox availability, and the
// on-disk cached-waveform totals. Field-compatible with the iOS-facing
// /v1/analysis/stats shape (minus the pool).
type analysisStatsResponse struct {
	Enabled         bool   `json:"enabled"`
	SoxAvailable    *bool  `json:"soxAvailable,omitempty"`
	CachedWaveforms int    `json:"cachedWaveforms"`
	CachedBytes     int64  `json:"cachedBytes"`
	StoragePath     string `json:"storagePath,omitempty"`
}

// apiAnalysisStats: GET /api/analysis/stats — the admin tile's data
// source for the Audio analysis section. Cheap (one SQL COUNT + the
// TTL-cached sox precheck).
func (s *Server) apiAnalysisStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.getAnalysisStatsSnapshot(r.Context()))
}

// getAnalysisStatsSnapshot builds the audio-analysis snapshot shared by
// the REST handler (apiAnalysisStats) and the SSE `analysis` event
// publisher. Cheap (one CountAnalysis COUNT + the TTL-cached sox
// precheck); diff-suppresses cleanly when idle (counts move only after
// `bridge analyze` runs).
func (s *Server) getAnalysisStatsSnapshot(ctx context.Context) analysisStatsResponse {
	cfg := s.deps.CfgHolder.Load()
	var resp analysisStatsResponse
	// Tracks analyze.WaveformDirSubdir ("waveforms"); inlined to avoid
	// an admin → analyze import (config does the same for transcode).
	resp.StoragePath = filepath.Join(cfg.DataDir, "waveforms")
	avail := s.cachedSoxAvailability()
	if avail != nil {
		resp.SoxAvailable = avail
	}
	// Enabled mirrors the LIVE runtime state (startup-computed
	// `analysisActive`) so the tile agrees with /v1/health's `waveform`
	// flag even between a restart-required PATCH and the actual restart.
	// Falls back to the persisted-config + sox derivation when the
	// closure isn't wired (test harnesses). (CodeRabbit on #395.)
	if a := s.deps.AnalysisActive; a != nil {
		resp.Enabled = a()
	} else {
		resp.Enabled = cfg.Analysis.Enabled && avail != nil && *avail
	}
	if s.deps.Manifest != nil {
		// Bound the read (see getUpscaleStatsSnapshot) so a stalled
		// query can't pin the SSE publisher past snapshotDBTimeout.
		dbCtx, cancel := context.WithTimeout(ctx, snapshotDBTimeout)
		defer cancel()
		count, bytes, err := s.deps.Manifest.CountAnalysis(dbCtx)
		if err != nil {
			logger.Warn("analysis stats: count analysis", "err", err)
		} else {
			resp.CachedWaveforms = count
			resp.CachedBytes = bytes
		}
	}
	return resp
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
