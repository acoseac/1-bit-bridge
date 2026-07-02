package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// SmartPlaylistStore is the optional backing store for GET /v1/smart-playlists.
// Nil-safe — unwired routes return 404 (feature-off). *manifest.Store
// satisfies it. READ-ONLY on the request path: generation happens in the
// background regenerator + the admin "Regenerate now" button, never inside a
// request (so the homepage fetch is a single fast cache read).
type SmartPlaylistStore interface {
	LoadSmartPlaylists(ctx context.Context) ([]manifest.StoredSmartPlaylist, error)
}

// WithSmartPlaylistStore wires the smart-playlist feed. Advertises the
// "smartPlaylists" health-feature flag when set. Returns the receiver.
func (s *Server) WithSmartPlaylistStore(st SmartPlaylistStore) *Server {
	s.smartPlaylistStore = st
	return s
}

// smartPlaylistResponseMaxItems caps each family on the wire. Generation
// already caps at the engine MaxItems; this also bounds the time-of-day
// window union (which merges several hour pools).
const smartPlaylistResponseMaxItems = 100

// timeOfDayWindow is how many UTC hours on each side of "now" the time-of-day
// family unions (±1 → a 3-hour window around the current hour).
const timeOfDayWindow = 1

// --- wire DTOs (additive; PROTOCOL.md "Smart playlists") ---

type smartPlaylistItemDTO struct {
	Position int    `json:"position"`
	Path     string `json:"path,omitempty"`
	Title    string `json:"title,omitempty"`
	Artist   string `json:"artist,omitempty"`
}

type smartPlaylistDTO struct {
	Slug        string                 `json:"slug"`
	Kind        string                 `json:"kind"`
	Title       string                 `json:"title"`
	Subtitle    string                 `json:"subtitle,omitempty"`
	RefreshedAt int64                  `json:"refreshedAt"` // UnixNano of the generating run
	Items       []smartPlaylistItemDTO `json:"items"`
	// ImageHash is the SHA-256 hex of the operator-uploaded custom cover for
	// this family (scope 'smartmix', key = slug), served at
	// GET /v1/smart-playlist-image/{slug}. Omitted when no custom cover is
	// set — iOS falls back to the auto-mosaic. Additive (no ProtocolVersion
	// bump).
	ImageHash string `json:"imageHash,omitempty"`
	// Energy is the per-mix normalized 0..1 loudness contour (one element per
	// member track, downsampled) the iOS waveform-signed cover renders as its
	// halo spline. Omitted when the family has no analyzed members (iOS falls
	// back to a seeded waveform). Additive (no ProtocolVersion bump).
	Energy []float64 `json:"energy,omitempty"`
	// ModalRateHz is the mix's modal sample rate (tie-break → highest),
	// driving the halo glow color via the iOS Hugo-2 rate-LED palette.
	// Omitted (0) when no member rate is known. Additive.
	ModalRateHz int `json:"modalRateHz,omitempty"`
}

type smartPlaylistsResponse struct {
	RefreshedAt int64              `json:"refreshedAt"` // newest family's RefreshedAt
	Playlists   []smartPlaylistDTO `json:"playlists"`
}

// smartPlaylists handles GET /v1/smart-playlists — the populated generated
// families, served from the cache. User-wide (bearer auth, no X-Device-Token
// required, like GET /v1/history). The optional `?local_hour=H` (0..23) is
// the device's current local hour, used ONLY to title the time-of-day family;
// the bucket itself is the server's current UTC hour (the same instant).
func (s *Server) smartPlaylists(w http.ResponseWriter, r *http.Request) {
	if s.smartPlaylistStore == nil {
		writeError(w, http.StatusNotFound, "smart_playlists_not_supported",
			"this bridge does not generate smart playlists")
		return
	}
	rows, err := s.smartPlaylistStore.LoadSmartPlaylists(r.Context())
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to load smart playlists", err)
		return
	}

	localHour, hasLocal := parseLocalHour(r.URL.Query().Get("local_hour"))
	nowUTCHour := time.Now().UTC().Hour()

	// Operator-uploaded custom covers, keyed by family slug (best-effort).
	covers := s.coverHashesForScope(r.Context(), manifest.CoverScopeSmartMix)

	resp := smartPlaylistsResponse{Playlists: make([]smartPlaylistDTO, 0, len(rows))}
	for _, row := range rows {
		dto, ok := buildSmartPlaylistDTO(row, nowUTCHour, localHour, hasLocal)
		if !ok {
			continue // e.g. time-of-day with no habit at the current hour
		}
		dto.ImageHash = covers[dto.Slug]
		resp.Playlists = append(resp.Playlists, dto)
		if row.RefreshedAt > resp.RefreshedAt {
			resp.RefreshedAt = row.RefreshedAt
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildSmartPlaylistDTO decodes one cached row into its wire shape. The
// time-of-day family is special-cased: its blob is per-UTC-hour pools, from
// which we union the window around nowUTCHour and retitle by the local hour.
// ok=false drops the family from this response (an empty time-of-day window).
func buildSmartPlaylistDTO(row manifest.StoredSmartPlaylist, nowUTCHour, localHour int, hasLocal bool) (smartPlaylistDTO, bool) {
	dto := smartPlaylistDTO{
		Slug: row.Slug, Kind: row.Kind, Title: row.Title,
		Subtitle: row.Subtitle, RefreshedAt: row.RefreshedAt,
		ModalRateHz: row.ModalRateHz,
	}
	// Energy is decorative — a malformed blob must not drop the family, so
	// decode best-effort and leave Energy nil on error (iOS seeded fallback).
	if len(row.EnergyJSON) > 0 {
		_ = json.Unmarshal(row.EnergyJSON, &dto.Energy)
	}

	if row.Kind == timeOfDayKind {
		var blob manifest.SmartPlaylistHourlyBlob
		if err := json.Unmarshal(row.ItemsJSON, &blob); err != nil {
			return smartPlaylistDTO{}, false
		}
		items := resolveTimeOfDayItems(blob.Hourly, nowUTCHour, timeOfDayWindow, smartPlaylistResponseMaxItems)
		if len(items) == 0 {
			return smartPlaylistDTO{}, false
		}
		if hasLocal {
			dto.Title = timeOfDayTitle(localHour)
		}
		dto.Items = items
		return dto, true
	}

	var stored []manifest.SmartPlaylistItem
	if err := json.Unmarshal(row.ItemsJSON, &stored); err != nil {
		return smartPlaylistDTO{}, false
	}
	dto.Items = make([]smartPlaylistItemDTO, 0, len(stored))
	for _, it := range stored {
		dto.Items = append(dto.Items, smartPlaylistItemDTO{
			Position: len(dto.Items), Path: it.Path, Title: it.Title, Artist: it.Artist,
		})
	}
	return dto, true
}

// timeOfDayKind mirrors smartplaylist.KindTimeOfDay without importing the
// engine package into the API layer (the string is the stable wire contract).
const timeOfDayKind = "timeOfDay"

// resolveTimeOfDayItems unions the hour pools in [utcHour-window, utcHour+window]
// (mod 24), center first then outward, deduping by path and capping at max.
func resolveTimeOfDayItems(hourly map[int][]manifest.SmartPlaylistItem, utcHour, window, maxItems int) []smartPlaylistItemDTO {
	if len(hourly) == 0 {
		return nil
	}
	// Visit order: center, then ±1, ±2, … so the most-relevant hour leads.
	order := []int{utcHour}
	for d := 1; d <= window; d++ {
		order = append(order, ((utcHour-d)%24+24)%24, (utcHour+d)%24)
	}
	seen := map[string]bool{}
	var out []smartPlaylistItemDTO
	for _, h := range order {
		for _, it := range hourly[h] {
			if it.Path == "" || seen[it.Path] {
				continue
			}
			seen[it.Path] = true
			out = append(out, smartPlaylistItemDTO{
				Position: len(out), Path: it.Path, Title: it.Title, Artist: it.Artist,
			})
			if len(out) >= maxItems {
				return out
			}
		}
	}
	return out
}

// timeOfDayTitle renders a friendly title for the device's local hour.
func timeOfDayTitle(localHour int) string {
	switch {
	case localHour >= 5 && localHour <= 10:
		return "Good Morning"
	case localHour >= 11 && localHour <= 16:
		return "This Afternoon"
	case localHour >= 17 && localHour <= 21:
		return "This Evening"
	default:
		return "Late Night"
	}
}

// parseLocalHour parses an optional 0..23 local-hour query param.
func parseLocalHour(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	h, err := strconv.Atoi(s)
	if err != nil || h < 0 || h > 23 {
		return 0, false
	}
	return h, true
}
