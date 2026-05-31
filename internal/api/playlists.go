package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// PlaylistStore is the optional backing store for the /v1/playlists
// backup endpoints. Nil-safe — when unwired the routes return 404
// (feature-off). *manifest.Store satisfies it in production.
type PlaylistStore interface {
	UpsertPlaylist(ctx context.Context, deviceToken string, p manifest.PlaylistRow, items []manifest.PlaylistItemRow) error
	GetPlaylist(ctx context.Context, deviceToken, id string) (*manifest.PlaylistRow, []manifest.PlaylistItemRow, error)
	ListPlaylists(ctx context.Context, deviceToken string) ([]manifest.PlaylistSummary, error)
	TombstonePlaylist(ctx context.Context, deviceToken, id string) (bool, error)
}

// WithPlaylistStore wires the playlist-backup feature. Advertises the
// "playlistBackup" health-feature flag when set. Returns the receiver.
func (s *Server) WithPlaylistStore(ps PlaylistStore) *Server {
	s.playlistStore = ps
	return s
}

// playlistMaxBodyBytes caps PUT /v1/playlists/{id}. A ~2,000-track
// cross-bridge playlist with foreign references + title/artist fallback
// strings can reach a few MB; 16 MiB leaves comfortable headroom. This is
// a public-API write path, NOT an admin form, so the 1 MiB admin cap is
// far too tight here.
const playlistMaxBodyBytes = 16 << 20

// --- wire DTOs (the playlists contract; see PROTOCOL.md Appendix A.2/A.3) ---

type playlistItemDTO struct {
	Position          int    `json:"position"`
	Path              string `json:"path,omitempty"`              // local, resolvable on this bridge
	OriginFingerprint string `json:"originFingerprint,omitempty"` // foreign: owning bridge fp / "local" / "smb"
	OriginPath        string `json:"originPath,omitempty"`
	Title             string `json:"title,omitempty"`
	Artist            string `json:"artist,omitempty"`
}

type playlistDTO struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	LastModifiedAt int64             `json:"lastModifiedAt"` // UnixNano UTC (LWW guard key)
	Items          []playlistItemDTO `json:"items"`
}

type playlistSummaryDTO struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	TrackCount     int    `json:"trackCount"`
	LastModifiedAt int64  `json:"lastModifiedAt"`
}

type playlistsListResponse struct {
	Playlists []playlistSummaryDTO `json:"playlists"`
}

type playlistStoredResponse struct {
	ID     string `json:"id"`
	Stored bool   `json:"stored"`
}

type playlistDeletedResponse struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
}

// playlistStaleResponse is the 409 body: the error envelope plus the full
// server copy so iOS can reconcile in one round-trip.
type playlistStaleResponse struct {
	Error   string      `json:"error"`
	Message string      `json:"message"`
	Server  playlistDTO `json:"server"`
}

func toPlaylistDTO(p *manifest.PlaylistRow, items []manifest.PlaylistItemRow) playlistDTO {
	out := playlistDTO{ID: p.ID, Name: p.Name, LastModifiedAt: p.LastModifiedAt}
	out.Items = make([]playlistItemDTO, 0, len(items))
	for _, it := range items {
		out.Items = append(out.Items, playlistItemDTO{
			Position:          it.Position,
			Path:              it.Path,
			OriginFingerprint: it.OriginFingerprint,
			OriginPath:        it.OriginPath,
			Title:             it.Title,
			Artist:            it.Artist,
		})
	}
	return out
}

// requirePlaylistFeature returns the device token, or writes the
// appropriate error and returns ("", false). Used at the top of every
// playlist handler.
func (s *Server) requirePlaylistFeature(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.playlistStore == nil {
		writeError(w, http.StatusNotFound, "playlist_backup_not_supported",
			"this bridge does not store playlist backups")
		return "", false
	}
	dt := deviceTokenFromContext(r.Context())
	if dt == "" {
		writeError(w, http.StatusBadRequest, "device_token_required",
			"playlist backup requires the X-Device-Token header")
		return "", false
	}
	return dt, true
}

// listPlaylists handles GET /v1/playlists — summaries for the caller's
// device token.
func (s *Server) listPlaylists(w http.ResponseWriter, r *http.Request) {
	dt, ok := s.requirePlaylistFeature(w, r)
	if !ok {
		return
	}
	rows, err := s.playlistStore.ListPlaylists(r.Context(), dt)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to list playlists", err)
		return
	}
	resp := playlistsListResponse{Playlists: make([]playlistSummaryDTO, 0, len(rows))}
	for _, p := range rows {
		resp.Playlists = append(resp.Playlists, playlistSummaryDTO{
			ID: p.ID, Name: p.Name, TrackCount: p.TrackCount, LastModifiedAt: p.LastModifiedAt,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// getPlaylist handles GET /v1/playlists/{id} — full playlist for restore.
func (s *Server) getPlaylist(w http.ResponseWriter, r *http.Request) {
	dt, ok := s.requirePlaylistFeature(w, r)
	if !ok {
		return
	}
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "playlist id is required")
		return
	}
	p, items, err := s.playlistStore.GetPlaylist(r.Context(), dt, id)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to read playlist", err)
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such playlist")
		return
	}
	writeJSON(w, http.StatusOK, toPlaylistDTO(p, items))
}

// putPlaylist handles PUT /v1/playlists/{id} — upsert with the backup-
// hygiene LWW guard.
func (s *Server) putPlaylist(w http.ResponseWriter, r *http.Request) {
	dt, ok := s.requirePlaylistFeature(w, r)
	if !ok {
		return
	}
	pathID := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	if pathID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "playlist id is required")
		return
	}

	var body playlistDTO
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, playlistMaxBodyBytes))
	if err := dec.Decode(&body); err != nil {
		writeErrorLog(w, r, http.StatusBadRequest, "bad_request",
			"request body must be a playlist JSON object", err)
		return
	}
	// The path id is authoritative; tolerate a mismatched/empty body id.
	id := pathID
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "playlist name is required")
		return
	}

	items := make([]manifest.PlaylistItemRow, 0, len(body.Items))
	for _, it := range body.Items {
		hasLocal := it.Path != ""
		hasForeign := it.OriginFingerprint != "" || it.OriginPath != ""
		if hasLocal == hasForeign {
			// Each item is EITHER local (path) XOR foreign
			// (originFingerprint+originPath) — never both, never neither.
			writeError(w, http.StatusBadRequest, "bad_request",
				"each playlist item must set either path or originFingerprint+originPath, not both")
			return
		}
		items = append(items, manifest.PlaylistItemRow{
			Position:          it.Position,
			Path:              strings.ReplaceAll(it.Path, `\`, "/"),
			OriginFingerprint: it.OriginFingerprint,
			OriginPath:        strings.ReplaceAll(it.OriginPath, `\`, "/"),
			Title:             it.Title,
			Artist:            it.Artist,
		})
	}

	row := manifest.PlaylistRow{ID: id, DeviceToken: dt, Name: body.Name, LastModifiedAt: body.LastModifiedAt}
	switch err := s.playlistStore.UpsertPlaylist(r.Context(), dt, row, items); {
	case errors.Is(err, manifest.ErrPlaylistStale):
		// Re-read the server copy so iOS can reconcile in one round-trip.
		sp, sItems, gerr := s.playlistStore.GetPlaylist(r.Context(), dt, id)
		if gerr != nil || sp == nil {
			writeError(w, http.StatusConflict, "stale", "server copy is newer")
			return
		}
		writeJSON(w, http.StatusConflict, playlistStaleResponse{
			Error: "stale", Message: "server copy is newer", Server: toPlaylistDTO(sp, sItems),
		})
		return
	case errors.Is(err, manifest.ErrPlaylistOwnedByOther):
		writeError(w, http.StatusConflict, "playlist_conflict",
			"a playlist with this id belongs to another device")
		return
	case err != nil:
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to store playlist", err)
		return
	}
	writeJSON(w, http.StatusOK, playlistStoredResponse{ID: id, Stored: true})
}

// deletePlaylist handles DELETE /v1/playlists/{id} — tombstone.
func (s *Server) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	dt, ok := s.requirePlaylistFeature(w, r)
	if !ok {
		return
	}
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "playlist id is required")
		return
	}
	deleted, err := s.playlistStore.TombstonePlaylist(r.Context(), dt, id)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to delete playlist", err)
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "not_found", "no such playlist")
		return
	}
	writeJSON(w, http.StatusOK, playlistDeletedResponse{ID: id, Deleted: true})
}
