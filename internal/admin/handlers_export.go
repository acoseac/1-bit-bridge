package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Everything a person made, in one file.
//
// The bridge already exports playlists and history separately, and the app can
// read all of it over /v1. What was missing is a SINGLE artifact someone can
// ask for once — which is what data portability actually means, and what the
// hosted service owes an account it is about to let someone delete. Favourites
// and device registrations had no export at all.
//
// It carries what the USER MADE, not what the bridge derived. The track
// catalogue is deliberately absent: it is a description of their own files,
// which they already have and which /v1/manifest serves in full, and including
// 50,000 rows would bury the part that cannot be reconstructed.
//
// ⚠️ NO CREDENTIALS. `DeviceRegistration.DeviceToken` is the iOS Keychain
// recovery token and `TokenID` names a live bearer token; `HistoryEventOut`
// carries the device token on every row. An export that leaked either would
// hand a working credential to anyone the file was forwarded to — which in a
// PRIVACY feature is the worst possible defect. Devices are identified here by
// name and first/last seen only, and the history rows carry the source device's
// NAME.

// exportBundle is the wire shape of the download. Declared here, in the API
// package, so a schema change to a manifest row cannot silently alter what
// leaves the building.
type exportBundle struct {
	Format        string           `json:"format"`
	ExportedAt    time.Time        `json:"exportedAt"`
	LibraryName   string           `json:"libraryName,omitempty"`
	ServerVersion string           `json:"serverVersion"`
	Note          string           `json:"note"`
	Playlists     []exportPlaylist `json:"playlists"`
	Favorites     exportFavorites  `json:"favorites"`
	History       []exportPlay     `json:"playbackHistory"`
	Devices       []exportDevice   `json:"devices"`
	Truncated     *exportTruncated `json:"truncated,omitempty"`
}

// exportFormat versions the bundle. A reader that does not recognise it should
// say so rather than guess; a writer that changes a field's meaning bumps it.
const exportFormat = "1-bit-bridge-export/1"

// exportHistoryCap bounds the one unbounded table in here. A heavy listener
// generates roughly 18k plays a year, so this is several years of listening and
// still a file a browser will open. When it bites, the bundle says so in
// `truncated` rather than quietly handing over a partial history and calling it
// an export.
const exportHistoryCap = 100000

type exportPlaylist struct {
	ID             string               `json:"id"`
	Name           string               `json:"name"`
	TrackCount     int                  `json:"trackCount"`
	LastModifiedAt time.Time            `json:"lastModifiedAt"`
	Items          []exportPlaylistItem `json:"items"`
}

type exportPlaylistItem struct {
	Position int    `json:"position"`
	Path     string `json:"path,omitempty"`
	Title    string `json:"title,omitempty"`
	Artist   string `json:"artist,omitempty"`
	// OriginPath is set for an item that lives on another bridge, so a
	// playlist that spans sources exports intact rather than losing rows.
	OriginPath string `json:"originPath,omitempty"`
}

type exportFavorites struct {
	Tracks []exportFavoriteTrack `json:"tracks"`
	Albums []exportFavoriteAlbum `json:"albums"`
}

type exportFavoriteTrack struct {
	Path        string     `json:"path,omitempty"`
	OriginPath  string     `json:"originPath,omitempty"`
	Title       string     `json:"title,omitempty"`
	Artist      string     `json:"artist,omitempty"`
	Album       string     `json:"album,omitempty"`
	FavoritedAt *time.Time `json:"favoritedAt,omitempty"`
}

type exportFavoriteAlbum struct {
	AlbumArtist string     `json:"albumArtist,omitempty"`
	Album       string     `json:"album,omitempty"`
	Year        int        `json:"year,omitempty"`
	FavoritedAt *time.Time `json:"favoritedAt,omitempty"`
}

type exportPlay struct {
	Path         string    `json:"path"`
	StartedAt    time.Time `json:"startedAt"`
	DurationUsed float64   `json:"secondsPlayed"`
	Codec        string    `json:"codec,omitempty"`
	DeviceName   string    `json:"deviceName,omitempty"`
}

type exportDevice struct {
	Name        string     `json:"name,omitempty"`
	FirstSeenAt *time.Time `json:"firstSeenAt,omitempty"`
	LastSeenAt  *time.Time `json:"lastSeenAt,omitempty"`
}

// exportTruncated names what did not fit, so a partial export never passes for
// a complete one.
type exportTruncated struct {
	PlaybackHistory bool `json:"playbackHistory"`
	Limit           int  `json:"limit"`
}

// apiExport streams the bundle as a download.
//
// GET, and a read — so csrfGuard passes it like every other read on this
// listener, the same reasoning the log and playlist exports already carry.
func (s *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "no_store", "the library store is not available")
		return
	}
	bundle, err := s.buildExport(r.Context())
	if err != nil {
		logger.Error("admin export", "err", err)
		writeError(w, http.StatusInternalServerError, "export_failed", "could not assemble the export")
		return
	}
	name := fmt.Sprintf("1-bit-export-%s.json", time.Now().UTC().Format("2006-01-02"))
	setDownloadHeaders(w, "application/json; charset=utf-8", name)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(bundle); err != nil {
		// Headers are already out; nothing useful left to say to the client.
		logger.Warn("admin export: write", "err", err)
	}
}

// buildExport assembles the bundle. Separate from the handler so the suite can
// assert on the CONTENT rather than on a decoded HTTP body, which is where the
// credential-leak check belongs.
func (s *Server) buildExport(ctx context.Context) (*exportBundle, error) {
	st := s.deps.Manifest
	out := &exportBundle{
		Format:        exportFormat,
		ExportedAt:    time.Now().UTC(),
		ServerVersion: version.ServerVersion,
		Note: "Everything you created on this bridge. Your music files are not in here — " +
			"they are your own files, and the library page lists them all.",
		Playlists: []exportPlaylist{},
		Favorites: exportFavorites{Tracks: []exportFavoriteTrack{}, Albums: []exportFavoriteAlbum{}},
		History:   []exportPlay{},
		Devices:   []exportDevice{},
	}
	if s.deps.CfgHolder != nil {
		out.LibraryName = s.deps.CfgHolder.Load().LibraryName
	}

	summaries, err := st.ListAllPlaylistsForAdmin(ctx)
	if err != nil {
		return nil, fmt.Errorf("playlists: %w", err)
	}
	for _, p := range summaries {
		row := exportPlaylist{
			ID:             p.ID,
			Name:           p.Name,
			TrackCount:     p.TrackCount,
			LastModifiedAt: time.Unix(0, p.LastModifiedAt).UTC(),
			Items:          []exportPlaylistItem{},
		}
		// The items are a second read per playlist. That is fine here and
		// would not be on a hot path: an export runs when a person asks for
		// one, and a playlist's items are the part that cannot be
		// reconstructed from anything else.
		_, items, err := st.GetPlaylist(ctx, p.ID)
		if err != nil {
			return nil, fmt.Errorf("playlist %s: %w", p.ID, err)
		}
		for _, it := range items {
			row.Items = append(row.Items, exportPlaylistItem{
				Position:   it.Position,
				Path:       it.Path,
				Title:      it.Title,
				Artist:     it.Artist,
				OriginPath: it.OriginPath,
			})
		}
		out.Playlists = append(out.Playlists, row)
	}

	_, favTracks, favAlbums, err := st.ListFavoritesForAdmin(ctx)
	if err != nil {
		return nil, fmt.Errorf("favorites: %w", err)
	}
	for _, f := range favTracks {
		out.Favorites.Tracks = append(out.Favorites.Tracks, exportFavoriteTrack{
			Path: f.Path, OriginPath: f.OriginPath, Title: f.Title,
			Artist: f.Artist, Album: f.Album, FavoritedAt: nsTime(f.FavoritedAt),
		})
	}
	for _, a := range favAlbums {
		out.Favorites.Albums = append(out.Favorites.Albums, exportFavoriteAlbum{
			AlbumArtist: a.AlbumArtist, Album: a.Album, Year: a.Year,
			FavoritedAt: nsTime(a.FavoritedAt),
		})
	}

	// The whole history, paged. ListHistory caps a single call at 1000, so one
	// call would silently export the most recent page and nothing else.
	var after int64
	for len(out.History) < exportHistoryCap {
		page, err := st.ListHistory(ctx, "", 1000, after)
		if err != nil {
			return nil, fmt.Errorf("history: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, h := range page {
			out.History = append(out.History, exportPlay{
				Path:         h.Path,
				StartedAt:    time.Unix(0, h.StartedAt).UTC(),
				DurationUsed: h.DurationUsed,
				Codec:        h.Codec,
				// The NAME, never SourceDeviceToken — that is a credential.
				DeviceName: firstNonEmpty(h.SourceDeviceName, h.DeviceName),
			})
		}
		after = page[len(page)-1].ID
	}
	if len(out.History) >= exportHistoryCap {
		out.Truncated = &exportTruncated{PlaybackHistory: true, Limit: exportHistoryCap}
	}

	devices, err := st.ListDeviceRegistrations(ctx)
	if err != nil {
		return nil, fmt.Errorf("devices: %w", err)
	}
	for _, d := range devices {
		out.Devices = append(out.Devices, exportDevice{
			Name:        d.DeviceName,
			FirstSeenAt: zeroTime(d.FirstSeenAt),
			LastSeenAt:  zeroTime(d.LastSeenAt),
		})
	}
	return out, nil
}

// nsTime renders a UnixNano stamp, or nil for "never". A pointer rather than a
// zero time.Time: Go does NOT drop a zero time under omitempty, so the
// non-pointer form ships "0001-01-01T00:00:00Z" and a reader parses a real,
// very old date — the trap this repo already records for wire fields.
func nsTime(ns int64) *time.Time {
	if ns == 0 {
		return nil
	}
	t := time.Unix(0, ns).UTC()
	return &t
}

func zeroTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
