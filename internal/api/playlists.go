package api

import (
	"bytes"
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
//
// Playlists are USER-WIDE: every paired device belongs to the bridge
// operator, so reads/writes/deletes are id-scoped, not device-scoped —
// restore is initiable from any device (advertised via the
// `playlistsCrossDevice` health feature flag). The deviceToken on the
// upsert records last-writer provenance only.
type PlaylistStore interface {
	UpsertPlaylist(ctx context.Context, deviceToken string, p manifest.PlaylistRow, items []manifest.PlaylistItemRow) error
	GetPlaylist(ctx context.Context, id string) (*manifest.PlaylistRow, []manifest.PlaylistItemRow, error)
	ListPlaylists(ctx context.Context) ([]manifest.PlaylistSummary, error)
	TombstonePlaylist(ctx context.Context, id string) (bool, error)
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

// Per-field bounds, enforced after the body-size cap. A well-formed payload
// can still be abusive (a 1-char-name playlist with 10M items), so bound the
// name length, item count, and id length explicitly. Clients send lowercase
// UUIDs (36 chars); 128 leaves headroom for any future id scheme while
// keeping a multi-KB id out of the TEXT PRIMARY KEY.
const (
	maxPlaylistNameLen = 1024
	maxPlaylistItems   = 50000
	maxPlaylistIDLen   = 128
)

// errPlaylistTooManyItems is the sentinel cappedPlaylistItems.UnmarshalJSON
// returns when the incoming array exceeds maxPlaylistItems. putPlaylist maps
// it to a clean 400 without the full oversized array ever being built.
var errPlaylistTooManyItems = errors.New("playlist has too many items")

// errPlaylistItemsNotArray is returned when the "items" field is present but
// is neither a JSON array nor null. Surfaces as a generic 400 (same status a
// stdlib type-mismatch would produce), keeping the wire contract intact.
var errPlaylistItemsNotArray = errors.New("playlist items must be a JSON array")

// cappedPlaylistItems is []playlistItemDTO with a decode-time item-count cap.
// The 16 MiB MaxBytesReader body cap bounds the raw bytes, but a crafted body
// of minimal items (`{}` is ~3 bytes with its separator) still decodes to
// ~1M structs (~100-200 MB transient) before the post-decode length guard
// fires — a ~10x amplification from a single authed PUT, painful on Pi-class
// hosts. No single body-size cap fixes this (a parseable item can be ~3 bytes
// while a legitimate cross-bridge item is hundreds), so the fix streams the
// array and aborts at maxPlaylistItems, bounding the struct expansion
// regardless of per-item wire size.
//
// Only the request-decode path invokes this — marshalling is unaffected (a
// named slice type serialises exactly like its underlying slice), so the
// response DTOs that embed playlistDTO are unchanged, and stored playlists
// (already capped on write) round-trip cleanly.
type cappedPlaylistItems []playlistItemDTO

func (c *cappedPlaylistItems) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	// Tolerate JSON null (→ nil slice), matching stdlib slice-decode.
	if tok == nil {
		*c = nil
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return errPlaylistItemsNotArray
	}
	items := make([]playlistItemDTO, 0)
	for dec.More() {
		// Guard BEFORE decoding the (cap+1)th element so the whole oversized
		// array is never materialised.
		if len(items) >= maxPlaylistItems {
			return errPlaylistTooManyItems
		}
		var it playlistItemDTO
		if err := dec.Decode(&it); err != nil {
			return err
		}
		items = append(items, it)
	}
	// Consume the closing ']' so a trailing-garbage array is rejected.
	if _, err := dec.Token(); err != nil {
		return err
	}
	*c = items
	return nil
}

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
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	LastModifiedAt int64               `json:"lastModifiedAt"` // UnixNano UTC (LWW guard key)
	Items          cappedPlaylistItems `json:"items"`
	// ImageHash — SHA-256 hex of the operator-uploaded custom cover (scope
	// 'playlist', key = id), served at GET /v1/playlist-image/{id}. Omitted
	// when none (iOS uses the auto-mosaic). Additive (no ProtocolVersion bump).
	ImageHash string `json:"imageHash,omitempty"`
}

type playlistSummaryDTO struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	TrackCount     int    `json:"trackCount"`
	LastModifiedAt int64  `json:"lastModifiedAt"`
	ImageHash      string `json:"imageHash,omitempty"`
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
// playlist handler. The header stays REQUIRED on all four routes even
// though reads are now user-wide — it identifies the writer on PUT
// (last-writer provenance) and keeps the device-registration binding
// fresh on every call; the documented contract is unchanged.
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

// listPlaylists handles GET /v1/playlists — summaries across ALL of the
// user's devices (user-wide backups; restore from any device).
func (s *Server) listPlaylists(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requirePlaylistFeature(w, r); !ok {
		return
	}
	rows, err := s.playlistStore.ListPlaylists(r.Context())
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to list playlists", err)
		return
	}
	covers := s.coverHashesForScope(r.Context(), manifest.CoverScopePlaylist)
	resp := playlistsListResponse{Playlists: make([]playlistSummaryDTO, 0, len(rows))}
	for _, p := range rows {
		resp.Playlists = append(resp.Playlists, playlistSummaryDTO{
			ID: p.ID, Name: p.Name, TrackCount: p.TrackCount, LastModifiedAt: p.LastModifiedAt,
			ImageHash: covers[p.ID],
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// getPlaylist handles GET /v1/playlists/{id} — full playlist for restore,
// regardless of which device backed it up.
func (s *Server) getPlaylist(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requirePlaylistFeature(w, r); !ok {
		return
	}
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "playlist id is required")
		return
	}
	p, items, err := s.playlistStore.GetPlaylist(r.Context(), id)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to read playlist", err)
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such playlist")
		return
	}
	dto := toPlaylistDTO(p, items)
	dto.ImageHash = s.coverHashForKey(r.Context(), manifest.CoverScopePlaylist, id)
	writeJSON(w, http.StatusOK, dto)
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
	if len(pathID) > maxPlaylistIDLen {
		writeError(w, http.StatusBadRequest, "bad_request", "playlist id is too long")
		return
	}

	var body playlistDTO
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, playlistMaxBodyBytes))
	if err := dec.Decode(&body); err != nil {
		// The item-count cap is enforced DURING decode (see
		// cappedPlaylistItems) so a crafted array of minimal items can't
		// balloon into ~1M structs before a post-decode length check. Map
		// the sentinel to the same 400 the explicit check used to return.
		if errors.Is(err, errPlaylistTooManyItems) {
			writeError(w, http.StatusBadRequest, "bad_request", "playlist has too many items")
			return
		}
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
	// Defensive bounds against oversized / malformed payloads (Gemini on
	// PR #335). The 16 MiB body cap is the outer guard; these keep a
	// well-formed-but-abusive payload bounded too.
	if len(body.Name) > maxPlaylistNameLen {
		writeError(w, http.StatusBadRequest, "bad_request", "playlist name is too long")
		return
	}
	if body.LastModifiedAt <= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "lastModifiedAt must be a positive UnixNano value")
		return
	}
	// NB: the maxPlaylistItems ceiling is enforced during decode by
	// cappedPlaylistItems.UnmarshalJSON (returning errPlaylistTooManyItems,
	// mapped to 400 above), so body.Items is already bounded here.

	items := make([]manifest.PlaylistItemRow, 0, len(body.Items))
	seenPositions := make(map[int]struct{}, len(body.Items))
	for _, it := range body.Items {
		// Position must be non-negative and unique — a duplicate position
		// would otherwise hit the (playlist_id, position) PK and surface as
		// a 500 instead of a clean 400 (Gemini HIGH on PR #335).
		if it.Position < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "playlist item position must be non-negative")
			return
		}
		if _, dup := seenPositions[it.Position]; dup {
			writeError(w, http.StatusBadRequest, "bad_request", "duplicate playlist item position")
			return
		}
		seenPositions[it.Position] = struct{}{}
		// Strict local-XOR-foreign: a local item has ONLY path; a foreign
		// item has BOTH originFingerprint AND originPath. Partially-filled
		// items (e.g. fingerprint without path) are rejected.
		isLocal := it.Path != "" && it.OriginFingerprint == "" && it.OriginPath == ""
		isForeign := it.Path == "" && it.OriginFingerprint != "" && it.OriginPath != ""
		if !isLocal && !isForeign {
			writeError(w, http.StatusBadRequest, "bad_request",
				"each playlist item must set either path (local) or both originFingerprint and originPath (foreign), and not mix them")
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
		sp, sItems, gerr := s.playlistStore.GetPlaylist(r.Context(), id)
		if gerr != nil || sp == nil {
			writeError(w, http.StatusConflict, "stale", "server copy is newer")
			return
		}
		writeJSON(w, http.StatusConflict, playlistStaleResponse{
			Error: "stale", Message: "server copy is newer", Server: toPlaylistDTO(sp, sItems),
		})
		return
	case err != nil:
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to store playlist", err)
		return
	}
	writeJSON(w, http.StatusOK, playlistStoredResponse{ID: id, Stored: true})
}

// deletePlaylist handles DELETE /v1/playlists/{id} — tombstone. User-wide:
// any paired device can delete any playlist (the delete then propagates
// to the user's other devices on their next sweep).
func (s *Server) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requirePlaylistFeature(w, r); !ok {
		return
	}
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "playlist id is required")
		return
	}
	deleted, err := s.playlistStore.TombstonePlaylist(r.Context(), id)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to delete playlist", err)
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "not_found", "no such playlist")
		return
	}
	// Orphan cleanup: drop any custom cover for this playlist so its JPEG
	// doesn't linger on disk after the playlist is gone (best-effort).
	s.pruneCover(r.Context(), manifest.CoverScopePlaylist, id)
	writeJSON(w, http.StatusOK, playlistDeletedResponse{ID: id, Deleted: true})
}
