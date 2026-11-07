package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/formatgate"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/smartplaylist/season"
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

// WithSmartPlaylistEnabled attaches a LIVE on/off predicate, so the
// feature toggle applies without a restart.
//
// The store stays the "is this wired at all" signal; this is the
// "is it on right now" one, and both have to agree before the endpoint
// answers or the health flag is advertised. Splitting them is what lets
// cmd/bridge wire the store unconditionally — which it must, because a
// store wired only when the flag was on at boot makes the flag
// restart-bound no matter how live the rest of the path is.
//
// Nil keeps the pre-existing behaviour (wired == enabled), which is what
// every caller other than cmd/bridge wants.
func (s *Server) WithSmartPlaylistEnabled(f func() bool) *Server {
	s.smartPlaylistsEnabled = f
	return s
}

// smartPlaylistsActive reports whether the feed should answer: wired AND
// currently enabled.
func (s *Server) smartPlaylistsActive() bool {
	if s.smartPlaylistStore == nil {
		return false
	}
	return s.smartPlaylistsEnabled == nil || s.smartPlaylistsEnabled()
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
//
// The Christmas family is served from 1 December to 5 January, UTC, and
// left out of every other response (B750). The window is read here, from
// the clock at the request, never from when the cache was built: the
// regenerator builds the family all year, so a cache built in November is
// ready the moment the window opens, and one built on 5 January cannot keep
// it served on the 6th. A family left out counts toward nothing, the
// top-level refreshedAt included.
//
// Each family's items, the Christmas family's in its window among them, are
// the view the request declared (formats.go): an item of a format the view
// does not declare is left out and the positions after it close up, and a
// family the gate leaves with no item is not sent. The Christmas family is
// sent only while what the view leaves of it clears its floor
// (season.MeetsFloor, the predicate the engine asks of the whole pool), so a
// client is never sent a mix below the owner's floor because the gate
// thinned it.
func (s *Server) smartPlaylists(w http.ResponseWriter, r *http.Request) {
	if !s.smartPlaylistsActive() {
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

	now := s.now()
	localHour, hasLocal := parseLocalHour(r.URL.Query().Get("local_hour"))
	nowUTCHour := now.UTC().Hour()
	inSeason := season.InWindow(now)
	view := requestFormats(r)
	varyOnFormats(w.Header())

	// Operator-uploaded custom covers, keyed by family slug (best-effort).
	covers := s.coverHashesForScope(r.Context(), manifest.CoverScopeSmartMix)

	resp := smartPlaylistsResponse{Playlists: make([]smartPlaylistDTO, 0, len(rows))}
	for _, row := range rows {
		if row.Kind == christmasKind && !inSeason {
			continue // out of its window: not served, not counted
		}
		dto, ok := buildSmartPlaylistDTO(row, nowUTCHour, localHour, hasLocal, view)
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

// buildSmartPlaylistDTO decodes one cached row into its wire shape, in view.
// The time-of-day family is special-cased: its blob is per-UTC-hour pools,
// from which we union the window around nowUTCHour and retitle by the local
// hour. ok=false drops the family from this response (an empty time-of-day
// window, a family whose every item is of a format view does not declare,
// or a Christmas family view leaves below its floor).
func buildSmartPlaylistDTO(row manifest.StoredSmartPlaylist, nowUTCHour, localHour int, hasLocal bool, view formatgate.Set) (smartPlaylistDTO, bool) {
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
		items := resolveTimeOfDayItems(blob.Hourly, nowUTCHour, timeOfDayWindow, smartPlaylistResponseMaxItems, view)
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
	inView := make([]manifest.SmartPlaylistItem, 0, len(stored))
	for _, it := range stored {
		if view.Takes(it.Path) {
			inView = append(inView, it)
		}
	}
	if len(stored) > 0 && len(inView) == 0 {
		return smartPlaylistDTO{}, false
	}
	// The Christmas family's floor holds for what this request is sent, by
	// the one predicate generation asks of the whole pool (round 4 of
	// backlog B489): the gate can leave a view fourteen tracks, or two
	// artists on two albums, of a family the whole pool built.
	if row.Kind == christmasKind && !season.MeetsFloor(inView, storedItemAlbum, storedItemArtist) {
		return smartPlaylistDTO{}, false
	}
	dto.Items = make([]smartPlaylistItemDTO, 0, len(inView))
	for _, it := range inView {
		dto.Items = append(dto.Items, smartPlaylistItemDTO{
			Position: len(dto.Items), Path: it.Path, Title: it.Title, Artist: it.Artist,
		})
	}
	return dto, true
}

// storedItemAlbum and storedItemArtist are what season.MeetsFloor reads of a
// cached item. A blob cached before items stored their album has none, and
// its family is judged on its artists alone until the next build.
func storedItemAlbum(it manifest.SmartPlaylistItem) string  { return it.Album }
func storedItemArtist(it manifest.SmartPlaylistItem) string { return it.Artist }

// timeOfDayKind mirrors smartplaylist.KindTimeOfDay without importing the
// engine package into the API layer (the string is the stable wire contract).
const timeOfDayKind = "timeOfDay"

// christmasKind mirrors smartplaylist.KindChristmas the same way. The window
// it is served in is season.InWindow, a leaf the engine side imports too.
const christmasKind = "christmas"

// now is the clock the smart-playlist feed reads: the test's instant when
// one is set (clockNow), else time.Now.
func (s *Server) now() time.Time {
	if s.clockNow != nil {
		return s.clockNow()
	}
	return time.Now()
}

// resolveTimeOfDayItems unions the hour pools in [utcHour-window, utcHour+window]
// (mod 24), center first then outward, deduping by path and capping at max,
// leaving out an item of a format view does not declare.
func resolveTimeOfDayItems(hourly map[int][]manifest.SmartPlaylistItem, utcHour, window, maxItems int, view formatgate.Set) []smartPlaylistItemDTO {
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
			if it.Path == "" || seen[it.Path] || !view.Takes(it.Path) {
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
