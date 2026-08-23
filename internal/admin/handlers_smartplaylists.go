package admin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/smartplaylist"
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
	if cfg == nil || !cfg.SmartPlaylists.EffectiveEnabled() {
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

// apiSmartPlaylistRegenerateOne handles POST /api/smart-playlists/{slug}/regenerate
// — rebuild ONE family in place, leaving the other cached mixes untouched.
// The full engine still runs (families share their assembled inputs), but
// only the requested slug's row is committed. Same feature gate + analysis
// gate as the wholesale handler above.
func (s *Server) apiSmartPlaylistRegenerateOne(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	if cfg == nil || !cfg.SmartPlaylists.EffectiveEnabled() {
		writeError(w, http.StatusNotFound, "smart_playlists_disabled",
			"smart playlists are not enabled in bridge.yaml")
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing family slug")
		return
	}
	analysisOn := s.deps.AnalysisActive != nil && s.deps.AnalysisActive()
	opts := smartplaylistgen.DefaultOptions(time.Now().UnixNano(), analysisOn)
	generated, existed, n, err := smartplaylistgen.RegenerateFamily(r.Context(), s.deps.Manifest, opts, slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !generated && !existed {
		writeError(w, http.StatusNotFound, "unknown_family", "no smart mix with that slug")
		return
	}
	// removed=true → the fresh run no longer populates this family and its
	// cached row was dropped (matching the wholesale path's semantics); the
	// UI explains rather than pretending the regenerate failed.
	writeJSON(w, http.StatusOK, map[string]any{
		"regenerated": generated,
		"removed":     !generated && existed,
		"itemCount":   n,
	})
}

// smartMixSavedByToken is the device_token provenance stamped on playlists
// created from the admin console's "Save as playlist" affordance. The
// playlists table requires a non-empty last-writer token; the admin console
// is not a paired device, so a fixed sentinel stands in. It is never exposed
// on /v1 (wire summaries carry no device token) and renders as the
// "admin-co…" prefix on the Data page.
const smartMixSavedByToken = "admin-console"

// maxSavedPlaylistNameLen mirrors internal/api's maxPlaylistNameLen so an
// admin-saved playlist can't exceed what the wire write path accepts.
const maxSavedPlaylistNameLen = 1024

// newPlaylistUUID mints a canonical lowercase UUID v4 string. LOAD-BEARING
// shape: the iOS restore path parses playlist ids with UUID(uuidString:) and
// silently SKIPS non-UUID ids (PlaylistSyncCoordinator), so an admin-saved
// mix must carry a real UUID or it would be invisible to every device.
func newPlaylistUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// apiSmartPlaylistSaveAsPlaylist handles POST /api/smart-playlists/{slug}/save-as-playlist
// — snapshot the CACHED mix contents into a regular (user-wide) playlist that
// devices see via the normal playlist list/restore flow. A frozen copy by
// design: the mix keeps regenerating, the saved playlist doesn't follow.
// Body: optional {"name": "..."} — defaults to "<title> — <date>".
// Deliberately NOT gated on cfg.SmartPlaylists.EffectiveEnabled(): the snapshot only
// reads the existing cache, and the per-slug cover routes set the precedent.
func (s *Server) apiSmartPlaylistSaveAsPlaylist(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing family slug")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeOptionalJSONBody(w, r, &body) {
		return
	}
	rows, err := s.deps.Manifest.LoadSmartPlaylists(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var row *manifest.StoredSmartPlaylist
	for i := range rows {
		if rows[i].Slug == slug {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		writeError(w, http.StatusNotFound, "unknown_family", "no smart mix with that slug")
		return
	}
	tracks := smartMixTracksForView(*row)
	if len(tracks) == 0 {
		writeError(w, http.StatusConflict, "empty_mix", "the mix has no tracks to save")
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = row.Title + " — " + time.Now().Format("Jan 2, 2006")
	}
	if len(name) > maxSavedPlaylistNameLen {
		writeError(w, http.StatusBadRequest, "bad_request", "playlist name too long")
		return
	}
	id, err := newPlaylistUUID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not mint playlist id")
		return
	}

	// Positions are re-indexed 0..N-1 from display order: the flattened
	// time-of-day pools repeat their per-hour positions, and the
	// (playlist_id, position) primary key requires uniqueness.
	items := make([]manifest.PlaylistItemRow, 0, len(tracks))
	for i, tr := range tracks {
		items = append(items, manifest.PlaylistItemRow{
			Position: i, Path: tr.Path, Title: tr.Title, Artist: tr.Artist,
		})
	}
	p := manifest.PlaylistRow{ID: id, Name: name, LastModifiedAt: time.Now().UnixNano()}
	if err := s.deps.Manifest.UpsertPlaylist(r.Context(), smartMixSavedByToken, p, items); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.copySmartMixCoverToPlaylist(r.Context(), slug, id)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "name": name, "trackCount": len(items),
	})
}

// copySmartMixCoverToPlaylist clones a smart mix's operator-uploaded cover
// onto a freshly-saved playlist (same bytes + image hash, playlist scope) so
// the snapshot keeps its art on the Data page and the /v1 playlist DTOs.
// Best-effort: any failure logs a warning and leaves the playlist saved
// without a cover — a cosmetic copy must not fail the save.
func (s *Server) copySmartMixCoverToPlaylist(ctx context.Context, slug, playlistID string) {
	c, ok, err := s.deps.Manifest.GetPlaylistCover(ctx, manifest.CoverScopeSmartMix, slug)
	if err != nil {
		logger.Warn("save-as-playlist: cover lookup", "slug", slug, "err", err)
		return
	}
	if !ok {
		return
	}
	cfg := s.deps.CfgHolder.Load()
	if cfg == nil {
		return
	}
	src := manifest.PlaylistCoverPath(cfg.DataDir, manifest.CoverScopeSmartMix, slug, c.Ext)
	raw, err := os.ReadFile(src)
	if err != nil {
		logger.Warn("save-as-playlist: read mix cover", "slug", slug, "err", err)
		return
	}
	dst := manifest.PlaylistCoverPath(cfg.DataDir, manifest.CoverScopePlaylist, playlistID, c.Ext)
	if err := atomicwrite.WriteBytes(dst, raw, ".cover-"); err != nil {
		logger.Warn("save-as-playlist: write playlist cover", "id", playlistID, "err", err)
		return
	}
	if err := s.deps.Manifest.SetPlaylistCover(ctx, manifest.PlaylistCover{
		Scope: manifest.CoverScopePlaylist, Key: playlistID,
		ImageHash: c.ImageHash, Ext: c.Ext, UpdatedAt: time.Now().UnixNano(),
	}); err != nil {
		logger.Warn("save-as-playlist: record playlist cover", "id", playlistID, "err", err)
		// Don't leave a JPEG on disk with no DB mapping (mirrors uploadCover).
		if rmErr := os.Remove(dst); rmErr != nil && !os.IsNotExist(rmErr) {
			logger.Warn("save-as-playlist: cleanup orphaned cover file", "err", rmErr)
		}
	}
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
	// KeyCoverage maps each Camelot wheel code ("8A" / "11B") to the
	// number of analyzed tracks in that key — the harmonic-coverage wheel.
	// Empty when analysis hasn't run or the query failed.
	KeyCoverage map[string]int
}

// camelotCode renders a Camelot wheel position as its canonical code,
// e.g. {Num:8, Minor:true} → "8A", {Num:11, Minor:false} → "11B".
func camelotCode(c smartplaylist.Camelot) string {
	letter := "B"
	if c.Minor {
		letter = "A"
	}
	return strconv.Itoa(c.Num) + letter
}

// pageSmartMixes renders the cached smart-playlist families (slug / kind /
// title / refreshed-at / member tracks) with a "Regenerate now" affordance.
// Read-only: the regenerate POST + the (PR-pending) cover upload run through
// the JSON admin API, not this page handler.
func (s *Server) pageSmartMixes(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	enabled := cfg != nil && cfg.SmartPlaylists.EffectiveEnabled()

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
	// Harmonic coverage for the Camelot wheel — count analyzed tracks per
	// wheel code, reusing smartplaylist.ToCamelot (the sequencer's own
	// mapping, single source of truth). Best-effort: a query error leaves
	// the wheel empty rather than failing the page.
	keyCoverage := map[string]int{}
	if kd, kdErr := s.deps.Manifest.KeyDistribution(r.Context()); kdErr != nil {
		logger.Warn("pageSmartMixes: key distribution", "err", kdErr)
	} else {
		for _, kc := range kd {
			if c, ok := smartplaylist.ToCamelot(kc.KeyRoot, kc.KeyMode); ok {
				keyCoverage[camelotCode(c)] += kc.Count
			}
		}
	}

	s.renderPage(w, "smartmixes", smartMixPageData{Enabled: enabled, Families: fams, KeyCoverage: keyCoverage})
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
