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
	opts := smartplaylistgen.DefaultOptions(time.Now().UnixNano(), cfg.Analysis.Enabled)
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
