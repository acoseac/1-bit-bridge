package admin

import (
	"net/http"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// Admin device + playlist-backup surfaces. Loopback-only, owner-visible,
// read-only. Device tokens are the client's recovery secrets, so they are
// truncated to a short prefix for display correlation — never returned in
// full on any surface.

const deviceTokenDisplayPrefix = 8

func redactDeviceToken(t string) string {
	if len(t) <= deviceTokenDisplayPrefix {
		return t
	}
	return t[:deviceTokenDisplayPrefix] + "…"
}

// deviceRow is the admin-wire DTO for a registered device. Distinct from
// manifest.DeviceRegistration so a future schema change can't leak onto
// the wire.
type deviceRow struct {
	DeviceTokenPrefix string `json:"deviceTokenPrefix"`
	TokenID           string `json:"tokenId"`
	DeviceName        string `json:"deviceName"`
	FirstSeenAt       string `json:"firstSeenAt"`
	LastSeenAt        string `json:"lastSeenAt"`
}

// apiDevicesList handles GET /api/devices — registered devices, newest
// activity first.
func (s *Server) apiDevicesList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeJSON(w, http.StatusOK, map[string]any{"devices": []deviceRow{}})
		return
	}
	regs, err := s.deps.Manifest.ListDeviceRegistrations(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]deviceRow, 0, len(regs))
	for _, d := range regs {
		out = append(out, deviceRow{
			DeviceTokenPrefix: redactDeviceToken(d.DeviceToken),
			TokenID:           d.TokenID,
			DeviceName:        d.DeviceName,
			FirstSeenAt:       d.FirstSeenAt.UTC().Format(time.RFC3339),
			LastSeenAt:        d.LastSeenAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

// playlistBackupRow is the admin-wire DTO for a backed-up playlist.
type playlistBackupRow struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	DeviceTokenPrefix string `json:"deviceTokenPrefix"`
	TrackCount        int    `json:"trackCount"`
	LastModifiedAt    string `json:"lastModifiedAt"`
	UpdatedAt         string `json:"updatedAt"`
}

// apiPlaylistsList handles GET /api/playlists — every device's backed-up
// playlists, most-recently-updated first.
func (s *Server) apiPlaylistsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeJSON(w, http.StatusOK, map[string]any{"playlists": []playlistBackupRow{}})
		return
	}
	rows, err := s.deps.Manifest.ListAllPlaylistsForAdmin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]playlistBackupRow, 0, len(rows))
	for _, p := range rows {
		out = append(out, playlistBackupRow{
			ID:                p.ID,
			Name:              p.Name,
			DeviceTokenPrefix: redactDeviceToken(p.DeviceToken),
			TrackCount:        p.TrackCount,
			LastModifiedAt:    time.Unix(0, p.LastModifiedAt).UTC().Format(time.RFC3339),
			UpdatedAt:         time.Unix(0, p.UpdatedAt).UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"playlists": out})
}

// favoriteTrackAdminRow / favoriteAlbumAdminRow are the admin-wire DTOs for
// the iOS favorites backup document (GET /api/favorites).
type favoriteTrackAdminRow struct {
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	Album       string `json:"album"`
	Path        string `json:"path,omitempty"`
	OriginPath  string `json:"originPath,omitempty"`
	Foreign     bool   `json:"foreign"`
	FavoritedAt string `json:"favoritedAt"`
}

type favoriteAlbumAdminRow struct {
	AlbumArtist string `json:"albumArtist"`
	Album       string `json:"album"`
	Year        int    `json:"year,omitempty"`
	FavoritedAt string `json:"favoritedAt"`
}

// apiFavorites handles GET /api/favorites — the stored favorites backup
// document (hearted tracks + albums), newest heart first. `stored: false`
// with empty sets when no device has pushed favorites yet (the singleton
// never-stored state). Loopback-only, read-only.
func (s *Server) apiFavorites(w http.ResponseWriter, r *http.Request) {
	empty := map[string]any{
		"stored": false,
		"tracks": []favoriteTrackAdminRow{},
		"albums": []favoriteAlbumAdminRow{},
	}
	if s.deps.Manifest == nil {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	meta, tracks, albums, err := s.deps.Manifest.ListFavoritesForAdmin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if meta == nil {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	trackRows := make([]favoriteTrackAdminRow, 0, len(tracks))
	for _, t := range tracks {
		trackRows = append(trackRows, favoriteTrackAdminRow{
			Title:       t.Title,
			Artist:      t.Artist,
			Album:       t.Album,
			Path:        t.Path,
			OriginPath:  t.OriginPath,
			Foreign:     t.Foreign,
			FavoritedAt: time.Unix(0, t.FavoritedAt).UTC().Format(time.RFC3339),
		})
	}
	albumRows := make([]favoriteAlbumAdminRow, 0, len(albums))
	for _, a := range albums {
		albumRows = append(albumRows, favoriteAlbumAdminRow{
			AlbumArtist: a.AlbumArtist,
			Album:       a.Album,
			Year:        a.Year,
			FavoritedAt: time.Unix(0, a.FavoritedAt).UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stored":            true,
		"lastModifiedAt":    time.Unix(0, meta.LastModifiedAt).UTC().Format(time.RFC3339),
		"updatedAt":         time.Unix(0, meta.UpdatedAt).UTC().Format(time.RFC3339),
		"deviceTokenPrefix": redactDeviceToken(meta.DeviceToken),
		"tracks":            trackRows,
		"albums":            albumRows,
	})
}

// historyBucketRow is the admin-wire DTO for a (label, count) aggregate.
type historyBucketRow struct {
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// historyTopTrackRow is the top-tracks DTO. It keeps `label` (the path)
// so the existing renderer and its title= tooltip carry on working, and
// adds the resolved metadata beside it. Title/Artist are omitempty: the
// client treats absence as "this path no longer resolves" and falls back
// to the basename, which is exactly what it showed before.
type historyTopTrackRow struct {
	Label  string `json:"label"`
	Title  string `json:"title,omitempty"`
	Artist string `json:"artist,omitempty"`
	Count  int64  `json:"count"`
}

// apiHistorySummary handles GET /api/history — owner-visible playback
// telemetry overview: total event count + codec / route histograms + top
// tracks (all aggregated across devices). Loopback-only, read-only.
func (s *Server) apiHistorySummary(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"totalEvents": 0,
			"codecs":      []historyBucketRow{},
			"routes":      []historyBucketRow{},
			"topTracks":   []historyTopTrackRow{},
		})
		return
	}
	ctx := r.Context()
	total, err := s.deps.Manifest.HistoryEventCount(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	codecs, err := s.deps.Manifest.CodecHistogram(ctx, "")
	if err != nil {
		logging.Component("admin.devices").Warn("CodecHistogram failed", "err", err)
	}
	routes, err := s.deps.Manifest.RouteHistogram(ctx, "")
	if err != nil {
		logging.Component("admin.devices").Warn("RouteHistogram failed", "err", err)
	}
	top, err := s.deps.Manifest.TopTracks(ctx, 20)
	if err != nil {
		logging.Component("admin.devices").Warn("TopTracks failed", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"totalEvents": total,
		"codecs":      toBucketRows(codecs),
		"routes":      toBucketRows(routes),
		"topTracks":   toTopTrackRows(top),
	})
}

func toTopTrackRows(in []manifest.HistoryTopTrack) []historyTopTrackRow {
	out := make([]historyTopTrackRow, 0, len(in))
	for _, b := range in {
		out = append(out, historyTopTrackRow{
			Label: b.Path, Title: b.Title, Artist: b.Artist, Count: b.Count,
		})
	}
	return out
}

func toBucketRows(in []manifest.HistoryBucket) []historyBucketRow {
	out := make([]historyBucketRow, 0, len(in))
	for _, b := range in {
		out = append(out, historyBucketRow{Label: b.Label, Count: b.Count})
	}
	return out
}
