package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The console's undo for a playlist delete.
//
// DELETE /v1/playlists/{id} writes a tombstone and nothing else: the row
// stays, `playlist_items` stays, and the id joins the `deletedIds` feed so
// the delete propagates to the user's other devices. That is the right wire
// behaviour and it left the operator with no way back — on 2026-09-20 a
// freshly-paired phone sent 17 DELETEs in 26 seconds (an app-side
// pending-delete queue replayed against a bridge that listed the same ids),
// and the only recovery was `UPDATE playlists SET deleted = 0` by hand
// against a live database.
//
// Two reads make that a button instead: the list of what is tombstoned, and
// the flip back. Both sit behind the console's existing posture — the
// loopback-only middleware in CLI mode, session auth in public mode — like
// every other route in this table. Neither is a managed control: restoring
// the operator's OWN playlist data is not an action a control plane owns,
// the way a restart or a library-root change is.

// deletedPlaylistRow is the admin-wire DTO for one tombstoned playlist.
// Distinct from manifest.AdminDeletedPlaylist per the wire-type rule.
//
// Two devices, because they answer different questions. `deviceTokenPrefix`
// is who last WROTE it — the same field the live list shows, so a row keeps
// its identity across the delete. `deletedBy*` is who asked for the
// tombstone, which is the one an operator looking at a mass delete needs and
// the one nothing recorded before migration v45.
type deletedPlaylistRow struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	TrackCount        int    `json:"trackCount"`
	DeviceTokenPrefix string `json:"deviceTokenPrefix"`
	DeviceName        string `json:"deviceName,omitempty"`
	LastModifiedAt    string `json:"lastModifiedAt"`
	DeletedAt         string `json:"deletedAt"`

	// DeletedByPrefix is empty for a tombstone written before v45, and for
	// one whose caller sent no X-Device-Token. The console renders that as
	// "unknown device" rather than inventing an attribution.
	DeletedByPrefix  string `json:"deletedByPrefix,omitempty"`
	DeletedByName    string `json:"deletedByName,omitempty"`
	DeletedByTokenID string `json:"deletedByTokenId,omitempty"`
}

// playlistDeleteBurstRow is the largest mass delete among the listed
// tombstones, or absent when none reaches the threshold.
//
// Computed server-side, by manifest.LargestPlaylistDeleteRun — the same
// window and threshold the bridge's own "playlist mass delete" WARN fires
// on, over the whole `deleted_by` token. The console used to reconstruct
// this from `deletedByPrefix`, which is eight redacted characters, and
// from pairwise gaps rather than a fixed window; it could not agree with
// the journal even in principle (CodeRabbit on #942).
type playlistDeleteBurstRow struct {
	Count        int    `json:"count"`
	DeviceName   string `json:"deviceName,omitempty"`
	DevicePrefix string `json:"devicePrefix"`
	// SpanSec is truncated whole seconds, so a run that landed inside one
	// second reports 0 and the console words it as such rather than
	// rounding a real measurement up to make a nicer sentence.
	SpanSec int `json:"spanSec"`
}

type deletedPlaylistsResponse struct {
	Deleted []deletedPlaylistRow    `json:"deleted"`
	Burst   *playlistDeleteBurstRow `json:"burst,omitempty"`
}

type playlistRestoredResponse struct {
	ID       string `json:"id"`
	Restored bool   `json:"restored"`
}

// apiDeletedPlaylistsList handles GET /api/playlists/deleted — every
// tombstoned playlist, most-recently-deleted first.
//
// Unbounded on purpose. Tombstones are one row per playlist a device ever
// deleted and never revived, so the set is small by construction; capping it
// would hide exactly the rows a mass delete just created, which is the case
// this panel exists for.
func (s *Server) apiDeletedPlaylistsList(w http.ResponseWriter, r *http.Request) {
	empty := deletedPlaylistsResponse{Deleted: []deletedPlaylistRow{}}
	if s.deps.Manifest == nil {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	rows, err := s.deps.Manifest.ListDeletedPlaylistsForAdmin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := empty
	out.Deleted = make([]deletedPlaylistRow, 0, len(rows))
	for _, p := range rows {
		row := deletedPlaylistRow{
			ID:                p.ID,
			Name:              p.Name,
			TrackCount:        p.TrackCount,
			DeviceTokenPrefix: redactDeviceToken(p.WroteLastToken),
			DeviceName:        p.WroteLastName,
			LastModifiedAt:    nsToRFC3339(p.LastModifiedAt),
			DeletedAt:         nsToRFC3339(p.DeletedAt),
			DeletedByName:     p.DeletedByName,
			DeletedByTokenID:  p.DeletedByTokenID,
		}
		// redactDeviceToken("") is "" — but say so explicitly, because an
		// empty prefix is a MEANINGFUL state here (pre-v45 tombstone, or a
		// delete with no device token) and must not read as a redaction bug.
		if p.DeletedByToken != "" {
			row.DeletedByPrefix = redactDeviceToken(p.DeletedByToken)
		}
		out.Deleted = append(out.Deleted, row)
	}
	// Grouped from the rows just read, not from a second query: the panel
	// must describe the list it is showing.
	if run, ok := manifest.LargestPlaylistDeleteRun(rows); ok {
		out.Burst = &playlistDeleteBurstRow{
			Count:        run.Count,
			DeviceName:   run.DeviceName,
			DevicePrefix: redactDeviceToken(run.DeviceToken),
			SpanSec:      int(time.Duration(run.SpanNS) / time.Second),
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// apiPlaylistRestore handles POST /api/playlists/{id}/restore — lift one
// tombstone.
//
// 404 when no TOMBSTONED row matched, which covers both an unknown id and
// one that is already live. Restoring a live playlist is not an error in the
// store (it changes nothing), but the console only ever offers the button
// for a row it just listed as deleted, so a miss here means the operator's
// view is stale — and telling them that is more use than a cheerful 200
// about a row nothing touched.
//
// The playlist's items come back with it: DELETE only ever set a flag. What
// does NOT come back is an operator-uploaded cover — the DELETE unlinked the
// JPEG (api.pruneCover), so a restored playlist falls back to the
// auto-mosaic. The console says so beside the button rather than leaving the
// operator to notice.
func (s *Server) apiPlaylistRestore(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", errMsgManifestNotWired)
		return
	}
	// Lowercased to match the /v1 handlers: ids are client UUIDs and every
	// other path that takes one folds case, so a console link built from a
	// listing must resolve the same row the phone's DELETE did.
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "playlist id is required")
		return
	}
	restored, err := s.deps.Manifest.RestorePlaylist(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !restored {
		writeError(w, http.StatusNotFound, errCodeNotFound,
			"no deleted playlist with that id")
		return
	}
	logger.Info("playlist restored from console", "playlistId", id)
	writeJSON(w, http.StatusOK, playlistRestoredResponse{ID: id, Restored: true})
}
