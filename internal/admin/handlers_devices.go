package admin

import (
	"net/http"
	"time"
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
