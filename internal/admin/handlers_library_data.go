package admin

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// Shared error code / message literals for this surface (SonarCloud
// go:S1192 — these recur across the playlist/history handlers).
const (
	errCodeNotFound        = "not-found"
	errMsgManifestNotWired = "manifest not wired"
)

// Library-data admin surfaces: playlist-backup detail + export and the
// playback-history event log + export. Loopback-only, owner-visible,
// read-only — same trust model as handlers_devices.go. Device tokens are
// recovery secrets: the UI works in terms of the 8-char display prefix
// (deviceTokenPrefix), and these handlers resolve the prefix back to the
// full token SERVER-SIDE so the full value never crosses the wire.

// pageData renders the "Data" page (playlists + listening history). The
// page bootstraps empty and loads everything via the JSON endpoints
// below, so there's no first-paint data to thread through.
func (s *Server) pageData(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "data", map[string]any{})
}

// resolvePlaylistDeviceToken maps a (display-prefix, playlist-id) pair
// back to the owning device's FULL token by scanning the admin playlist
// list. Keeping the full token off the wire preserves the redaction
// invariant the Devices/Playlists summaries already honour. Returns
// ("", false) when no live playlist matches, or on an ambiguous prefix
// collision (astronomically unlikely at 8 hex chars, but fail closed).
func (s *Server) resolvePlaylistDeviceToken(r *http.Request, prefix, id string) (string, bool) {
	prefix = strings.TrimSuffix(prefix, "…")
	if prefix == "" || id == "" {
		return "", false
	}
	rows, err := s.deps.Manifest.ListAllPlaylistsForAdmin(r.Context())
	if err != nil {
		return "", false
	}
	var match string
	for _, p := range rows {
		if p.ID == id && strings.HasPrefix(p.DeviceToken, prefix) {
			if match != "" && match != p.DeviceToken {
				return "", false // ambiguous prefix collision
			}
			match = p.DeviceToken
		}
	}
	return match, match != ""
}

// resolveDeviceTokenByPrefix maps a device display-prefix to the full
// token via the device roster. Used by the per-device history filter.
func (s *Server) resolveDeviceTokenByPrefix(r *http.Request, prefix string) (string, bool) {
	prefix = strings.TrimSuffix(prefix, "…")
	if prefix == "" {
		return "", false
	}
	regs, err := s.deps.Manifest.ListDeviceRegistrations(r.Context())
	if err != nil {
		return "", false
	}
	var match string
	for _, d := range regs {
		if strings.HasPrefix(d.DeviceToken, prefix) {
			if match != "" && match != d.DeviceToken {
				return "", false
			}
			match = d.DeviceToken
		}
	}
	return match, match != ""
}

// playlistItemDTO is one ordered entry. Foreign items (owned by another
// bridge or a local/SMB source) carry origin* + Foreign=true; local items
// carry Path. Title/Artist are best-effort render fallbacks.
type playlistItemDTO struct {
	Position          int    `json:"position"`
	Path              string `json:"path,omitempty"`
	OriginFingerprint string `json:"originFingerprint,omitempty"`
	OriginPath        string `json:"originPath,omitempty"`
	Title             string `json:"title,omitempty"`
	Artist            string `json:"artist,omitempty"`
	Foreign           bool   `json:"foreign"`
}

type playlistDetailDTO struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	DeviceTokenPrefix string            `json:"deviceTokenPrefix"`
	LastModifiedAt    string            `json:"lastModifiedAt"`
	UpdatedAt         string            `json:"updatedAt"`
	Items             []playlistItemDTO `json:"items"`
}

// apiPlaylistDetail handles GET /api/playlists/detail?device=<prefix>&id=<id>
// — full ordered contents of one backed-up playlist.
func (s *Server) apiPlaylistDetail(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", errMsgManifestNotWired)
		return
	}
	prefix := r.URL.Query().Get("device")
	id := r.URL.Query().Get("id")
	// The prefix→token resolution stays as a consistency check (the row
	// the UI clicked must still exist under that last-writer prefix);
	// the store read itself is id-scoped now that playlists are
	// user-wide rather than device-scoped.
	if _, ok := s.resolvePlaylistDeviceToken(r, prefix, id); !ok {
		writeError(w, http.StatusNotFound, errCodeNotFound, "no matching playlist for that device")
		return
	}
	row, items, err := s.deps.Manifest.GetPlaylist(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if row == nil {
		writeError(w, http.StatusNotFound, errCodeNotFound, "playlist not found")
		return
	}
	writeJSON(w, http.StatusOK, playlistToDetailDTO(row.ID, row.Name, prefix, row.LastModifiedAt, row.UpdatedAt, items))
}

func playlistToDetailDTO(id, name, prefix string, lastModNS, updatedNS int64, items []manifest.PlaylistItemRow) playlistDetailDTO {
	out := playlistDetailDTO{
		ID:                id,
		Name:              name,
		DeviceTokenPrefix: strings.TrimSuffix(prefix, "…"),
		LastModifiedAt:    nsToRFC3339(lastModNS),
		UpdatedAt:         nsToRFC3339(updatedNS),
		Items:             make([]playlistItemDTO, 0, len(items)),
	}
	for _, it := range items {
		foreign := it.OriginFingerprint != "" || it.OriginPath != ""
		out.Items = append(out.Items, playlistItemDTO{
			Position:          it.Position,
			Path:              it.Path,
			OriginFingerprint: it.OriginFingerprint,
			OriginPath:        it.OriginPath,
			Title:             it.Title,
			Artist:            it.Artist,
			Foreign:           foreign,
		})
	}
	return out
}

// apiPlaylistExport handles GET /api/playlists/export?device=&id=&format=json|csv|m3u8
// — a downloadable copy of one playlist in the requested format.
func (s *Server) apiPlaylistExport(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", errMsgManifestNotWired)
		return
	}
	q := r.URL.Query()
	prefix, id, format := q.Get("device"), q.Get("id"), strings.ToLower(q.Get("format"))
	// Reject an unsupported format before the playlist lookup.
	if format != "json" && format != "csv" && format != "m3u8" {
		writeError(w, http.StatusBadRequest, "bad-format", "format must be one of json, csv, m3u8")
		return
	}
	if _, ok := s.resolvePlaylistDeviceToken(r, prefix, id); !ok {
		writeError(w, http.StatusNotFound, errCodeNotFound, "no matching playlist for that device")
		return
	}
	row, items, err := s.deps.Manifest.GetPlaylist(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if row == nil {
		writeError(w, http.StatusNotFound, errCodeNotFound, "playlist not found")
		return
	}
	base := "playlist-" + safeFilename(row.Name)
	switch format {
	case "json":
		dto := playlistToDetailDTO(row.ID, row.Name, prefix, row.LastModifiedAt, row.UpdatedAt, items)
		setDownloadHeaders(w, "application/json", base+".json")
		_ = json.NewEncoder(w).Encode(dto)
	case "csv":
		setDownloadHeaders(w, "text/csv; charset=utf-8", base+".csv")
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"position", "title", "artist", "path", "originFingerprint", "originPath"})
		for _, it := range items {
			_ = cw.Write([]string{
				strconv.Itoa(it.Position), it.Title, it.Artist,
				it.Path, it.OriginFingerprint, it.OriginPath,
			})
		}
		cw.Flush()
	case "m3u8":
		setDownloadHeaders(w, "audio/x-mpegurl", base+".m3u8")
		s.writeM3U8(w, row.Name, items)
	default:
		writeError(w, http.StatusBadRequest, "bad-format", "format must be one of json, csv, m3u8")
	}
}

// writeM3U8 emits an extended-M3U playlist. Local items resolve to an
// absolute filesystem path (playable by a player on the bridge host, e.g.
// VLC opened on this machine); items that don't resolve and foreign items
// are emitted as comments so the file stays valid and self-documenting.
func (s *Server) writeM3U8(w http.ResponseWriter, name string, items []manifest.PlaylistItemRow) {
	fmt.Fprintln(w, "#EXTM3U")
	fmt.Fprintf(w, "#PLAYLIST:%s\n", name)
	for _, it := range items {
		// Trim the fields individually then join only when both are
		// present — joining-then-trimming would corrupt legit names that
		// begin/end with a hyphen (e.g. the artist "-M-"). Gemini on PR #341.
		artist := strings.TrimSpace(it.Artist)
		songTitle := strings.TrimSpace(it.Title)
		title := songTitle
		switch {
		case artist != "" && songTitle != "":
			title = artist + " - " + songTitle
		case artist != "":
			title = artist
		}
		if it.OriginFingerprint != "" || it.OriginPath != "" {
			// Foreign item — opaque to this bridge, not locally playable.
			fmt.Fprintf(w, "# foreign (%s): %s\n", it.OriginFingerprint, it.OriginPath)
			continue
		}
		abs, err := s.deps.Resolver.Resolve(it.Path)
		if err != nil {
			fmt.Fprintf(w, "# unresolved: %s\n", it.Path)
			continue
		}
		fmt.Fprintf(w, "#EXTINF:-1,%s\n%s\n", title, abs)
	}
}

// --- history events ---

type historyEventDTO struct {
	ID           int64   `json:"id"`
	Path         string  `json:"path"`
	StartedAt    string  `json:"startedAt"`
	DurationUsed float64 `json:"durationUsed"`
	Codec        string  `json:"codec,omitempty"`
	Route        string  `json:"route,omitempty"`
	DeviceName   string  `json:"deviceName,omitempty"`
	VariantID    string  `json:"variantId,omitempty"`
	OutputRate   int     `json:"outputRate,omitempty"`
	IsDoP        bool    `json:"isDop"`
}

// apiHistoryEvents handles GET /api/history/events?device=<prefix>&limit=&after=
// — a paginated event log. Without a device param it's a GLOBAL all-devices
// feed; with one it's scoped to that device. Cursor is the last id seen.
func (s *Server) apiHistoryEvents(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeJSON(w, http.StatusOK, map[string]any{"events": []historyEventDTO{}, "nextCursor": 0})
		return
	}
	q := r.URL.Query()
	token := ""
	if prefix := q.Get("device"); prefix != "" {
		t, ok := s.resolveDeviceTokenByPrefix(r, prefix)
		if !ok {
			writeError(w, http.StatusNotFound, errCodeNotFound, "no matching device")
			return
		}
		token = t
	}
	limit := 200
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = v
	}
	var after int64
	if v, err := strconv.ParseInt(q.Get("after"), 10, 64); err == nil {
		after = v
	}
	events, err := s.deps.Manifest.ListHistory(r.Context(), token, limit, after)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]historyEventDTO, 0, len(events))
	for _, e := range events {
		out = append(out, historyToDTO(e))
	}
	var next int64
	if len(events) > 0 {
		next = events[len(events)-1].ID // ListHistory is DESC by id
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out, "nextCursor": next})
}

// apiHistoryExport handles GET /api/history/export?device=&format=json|csv
// — a downloadable dump of history events (global, or scoped to a device).
// Capped at a generous bound so a huge table doesn't blow up the response.
func (s *Server) apiHistoryExport(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", errMsgManifestNotWired)
		return
	}
	q := r.URL.Query()
	// Validate the format BEFORE the expensive paged scan so a bad value
	// returns 400 without hitting the DB (CodeRabbit on PR #341).
	format := strings.ToLower(q.Get("format"))
	if format != "json" && format != "csv" {
		writeError(w, http.StatusBadRequest, "bad-format", "format must be one of json, csv")
		return
	}
	token := ""
	suffix := "all"
	if prefix := q.Get("device"); prefix != "" {
		t, ok := s.resolveDeviceTokenByPrefix(r, prefix)
		if !ok {
			writeError(w, http.StatusNotFound, errCodeNotFound, "no matching device")
			return
		}
		token = t
		suffix = strings.TrimSuffix(prefix, "…")
	}
	events, err := s.collectHistoryForExport(r, token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	base := "history-" + safeFilename(suffix)
	if format == "json" {
		out := make([]historyEventDTO, 0, len(events))
		for _, e := range events {
			out = append(out, historyToDTO(e))
		}
		setDownloadHeaders(w, "application/json", base+".json")
		_ = json.NewEncoder(w).Encode(map[string]any{"events": out})
		return
	}
	setDownloadHeaders(w, "text/csv; charset=utf-8", base+".csv")
	writeHistoryCSV(w, events)
}

// collectHistoryForExport pages through ListHistory with the cursor.
// ListHistory caps a single call at 1000 rows (passing the total cap
// directly would be silently clamped back to the 200 default and
// truncate the export — Gemini on PR #341); exportCap bounds the grand
// total so a huge table can't blow up the response.
func (s *Server) collectHistoryForExport(r *http.Request, token string) ([]manifest.HistoryEventOut, error) {
	const (
		pageSize  = 1000
		exportCap = 100000
	)
	var events []manifest.HistoryEventOut
	var after int64
	for len(events) < exportCap {
		page, err := s.deps.Manifest.ListHistory(r.Context(), token, pageSize, after)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		events = append(events, page...)
		after = page[len(page)-1].ID // DESC by id → next page is older
		if len(page) < pageSize {
			break
		}
	}
	if len(events) > exportCap {
		events = events[:exportCap]
	}
	return events, nil
}

// writeHistoryCSV streams the history events as CSV (header + one row
// per event). Caller has already set the download headers.
func writeHistoryCSV(w http.ResponseWriter, events []manifest.HistoryEventOut) {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"started_at", "path", "codec", "iface_type", "output_rate", "duration_used", "is_dop", "variant_id", "device_name"})
	for _, e := range events {
		_ = cw.Write([]string{
			nsToRFC3339(e.StartedAt), e.Path, e.Codec, e.IfaceType,
			strconv.Itoa(e.OutputRate), strconv.FormatFloat(e.DurationUsed, 'f', -1, 64),
			strconv.FormatBool(e.IsDoP), e.VariantID, e.DeviceName,
		})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		// The client likely sees a truncated CSV; surface it so a
		// disconnected-client / full-disk export failure isn't silent.
		logging.Component("admin.history").Warn("history CSV export write error", "err", err)
	}
}

func historyToDTO(e manifest.HistoryEventOut) historyEventDTO {
	return historyEventDTO{
		ID:           e.ID,
		Path:         e.Path,
		StartedAt:    nsToRFC3339(e.StartedAt),
		DurationUsed: e.DurationUsed,
		Codec:        e.Codec,
		Route:        e.IfaceType,
		DeviceName:   e.DeviceName,
		VariantID:    e.VariantID,
		OutputRate:   e.OutputRate,
		IsDoP:        e.IsDoP,
	}
}

// --- helpers ---

// nsToRFC3339 formats a UnixNano timestamp as RFC3339 UTC; 0 → "".
func nsToRFC3339(ns int64) string {
	if ns == 0 {
		return ""
	}
	return time.Unix(0, ns).UTC().Format(time.RFC3339)
}

// safeFilename reduces an arbitrary playlist/device label to a token safe
// for a Content-Disposition filename. Keeps alnum + dash/underscore;
// everything else becomes a dash; empty → "export".
func safeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "export"
	}
	// Guard against a pathological all-symbol name producing a huge run.
	out = path.Base(out)
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

func setDownloadHeaders(w http.ResponseWriter, contentType, filename string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Cache-Control", "no-store")
}
