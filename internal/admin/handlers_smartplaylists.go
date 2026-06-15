package admin

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/smartplaylistgen"
)

// smartPlaylistSummaryRow is the per-family admin (loopback) view.
type smartPlaylistSummaryRow struct {
	Slug        string `json:"slug"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Position    int    `json:"position"`
	RefreshedAt int64  `json:"refreshedAt"`
	ItemCount   int    `json:"itemCount"`
}

// apiSmartPlaylistsList handles GET /api/smart-playlists — loopback inspection
// of the cached generated families (slug / kind / title / position /
// refreshedAt + resolved item count). Empty before the first regeneration.
func (s *Server) apiSmartPlaylistsList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deps.Manifest.LoadSmartPlaylists(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]smartPlaylistSummaryRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, smartPlaylistSummaryRow{
			Slug: row.Slug, Kind: row.Kind, Title: row.Title,
			Position: row.Position, RefreshedAt: row.RefreshedAt,
			ItemCount: smartPlaylistItemCount(row),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"playlists": out})
}

// apiSmartPlaylistsRegenerate handles POST /api/smart-playlists/regenerate —
// the operator "Regenerate now" affordance, forcing an immediate rebuild
// instead of waiting for the daily ticker. CSRF-guarded by the mux wrapper.
// Synchronous: a loopback operator action, fast on a typical library.
func (s *Server) apiSmartPlaylistsRegenerate(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	if cfg == nil || !cfg.SmartPlaylists.Enabled {
		writeError(w, http.StatusNotFound, "smart_playlists_disabled",
			"smart playlists are not enabled in bridge.yaml")
		return
	}
	// Use the LIVE runtime analysis gate (matches the scheduled ticker, which
	// passes the sox-resolved analysisActive) rather than cfg.Analysis.Enabled,
	// which diverges when sox is missing. nil-safe for tests that omit it.
	analysisOn := s.deps.AnalysisActive != nil && s.deps.AnalysisActive()
	opts := smartplaylistgen.DefaultOptions(time.Now().UnixNano(), analysisOn)
	n, err := smartplaylistgen.Regenerate(r.Context(), s.deps.Manifest, opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"families": n})
}

// smartPlaylistItemCount decodes a cached row's blob to report its size. The
// time-of-day family stores per-hour pools, so its count is the distinct
// track paths across all hours; every other family is a flat list.
func smartPlaylistItemCount(row manifest.StoredSmartPlaylist) int {
	if row.Kind == "timeOfDay" {
		var hb manifest.SmartPlaylistHourlyBlob
		if json.Unmarshal(row.ItemsJSON, &hb) != nil {
			return 0
		}
		seen := map[string]struct{}{}
		for _, items := range hb.Hourly {
			for _, it := range items {
				seen[it.Path] = struct{}{}
			}
		}
		return len(seen)
	}
	var items []manifest.SmartPlaylistItem
	if json.Unmarshal(row.ItemsJSON, &items) != nil {
		return 0
	}
	return len(items)
}

// --- /smartmixes admin page (read-only render of the cached families) ---

type smartMixTrackView struct {
	Position int
	Title    string
	Artist   string
	Path     string
}

type smartMixFamilyView struct {
	Slug        string
	Kind        string
	Title       string
	Subtitle    string
	RefreshedAt time.Time
	ItemCount   int
	HasCover    bool
	Tracks      []smartMixTrackView
}

type smartMixPageData struct {
	Enabled  bool
	Families []smartMixFamilyView
}

// pageSmartMixes renders the cached smart-playlist families (slug / kind /
// title / refreshed-at / member tracks) with a "Regenerate now" affordance.
// Read-only: the regenerate POST + the (PR-pending) cover upload run through
// the JSON admin API, not this page handler.
func (s *Server) pageSmartMixes(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	enabled := cfg != nil && cfg.SmartPlaylists.Enabled

	// Which families already have an operator-uploaded cover (best-effort).
	covers, _ := s.deps.Manifest.PlaylistCoversByScope(r.Context(), manifest.CoverScopeSmartMix)

	var fams []smartMixFamilyView
	rows, err := s.deps.Manifest.LoadSmartPlaylists(r.Context())
	if err != nil {
		logger.Error("pageSmartMixes: load", "err", err)
	} else {
		fams = make([]smartMixFamilyView, 0, len(rows))
		for _, row := range rows {
			_, hasCover := covers[row.Slug]
			// Leave RefreshedAt as the zero Time when never refreshed
			// (RefreshedAt == 0) so the template renders "not yet refreshed"
			// rather than "55 years ago" — time.Unix(0, 0) is the 1970 epoch
			// (Gemini MEDIUM on PR #401).
			var refreshed time.Time
			if row.RefreshedAt > 0 {
				refreshed = time.Unix(0, row.RefreshedAt)
			}
			fams = append(fams, smartMixFamilyView{
				Slug:        row.Slug,
				Kind:        row.Kind,
				Title:       row.Title,
				Subtitle:    row.Subtitle,
				RefreshedAt: refreshed,
				ItemCount:   smartPlaylistItemCount(row),
				HasCover:    hasCover,
				Tracks:      smartMixTracksForView(row),
			})
		}
	}
	s.renderPage(w, "smartmixes", smartMixPageData{Enabled: enabled, Families: fams})
}

// smartMixTracksForView decodes a cached row into display rows. The
// time-of-day family stores per-UTC-hour pools; flatten to distinct tracks in
// hour order so the page shows a stable representative list.
func smartMixTracksForView(row manifest.StoredSmartPlaylist) []smartMixTrackView {
	var items []manifest.SmartPlaylistItem
	if row.Kind == "timeOfDay" {
		var hb manifest.SmartPlaylistHourlyBlob
		if json.Unmarshal(row.ItemsJSON, &hb) != nil {
			return nil
		}
		seen := map[string]struct{}{}
		for h := 0; h < 24; h++ {
			for _, it := range hb.Hourly[h] {
				if _, ok := seen[it.Path]; ok {
					continue
				}
				seen[it.Path] = struct{}{}
				items = append(items, it)
			}
		}
	} else if json.Unmarshal(row.ItemsJSON, &items) != nil {
		return nil
	}
	out := make([]smartMixTrackView, 0, len(items))
	for _, it := range items {
		out = append(out, smartMixTrackView{Position: it.Position, Title: it.Title, Artist: it.Artist, Path: it.Path})
	}
	return out
}
