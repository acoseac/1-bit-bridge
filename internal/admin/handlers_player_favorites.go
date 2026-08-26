package admin

import (
	"net/http"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
	"github.com/acoseac/1-bit-bridge/internal/librarycat"
)

// The player's favorites surface.
//
// /api/favorites already exists and stays: it is the OPERATOR's view of
// the stored backup document — raw display strings exactly as a device
// pushed them, with no artwork and no playability, which is the right
// answer for "what did this device back up".
//
// The player is asking a different question. A hearted album should be
// an album — a cover you can open — and a hearted track should be
// something you can press play on. Neither is derivable from the backup
// document alone, so this joins it against the catalog rather than
// widening the operator endpoint into something that serves neither
// well.
//
// Both halves resolve through the SAME machinery the rest of the player
// uses, deliberately: albums through the catalog's own album identity,
// tracks through hydrateTracks. So a favorite album is byte-identical to
// the tile in the grid, and Play on the tracks tab behaves exactly like
// Play on an album.

type playerFavoritesResponse struct {
	// Stored distinguishes "no device has ever pushed favorites" from
	// "a device pushed an empty set". The empty states differ: the
	// first is a setup hint, the second is not a problem at all.
	Stored bool             `json:"stored"`
	Albums []playerAlbumDTO `json:"albums"`
	Tracks []playerTrackDTO `json:"tracks"`

	// Unresolved* count hearts this bridge cannot turn into rows:
	// another bridge's tracks (or SMB / on-device files), and albums
	// and tracks removed from this library since they were hearted.
	//
	// Reported rather than hidden, for the reason the collection detail
	// reports its own: the operator's Favorites panel counts every
	// entry, and a page that silently shows fewer disagrees with it in
	// a way that reads as a bug rather than as a fact about the data.
	UnresolvedAlbums int    `json:"unresolvedAlbums,omitempty"`
	UnresolvedTracks int    `json:"unresolvedTracks,omitempty"`
	SnapshotAt       string `json:"snapshotAt"`

	// Provenance: which device last pushed the document and when the
	// bridge received it. Came off the operator page when favorites
	// consolidated here — it is the one thing that view said which this
	// one could not, and "hearts from a device that stopped syncing
	// three months ago" is only visible if the date is.
	DeviceName        string `json:"deviceName,omitempty"`
	DeviceTokenPrefix string `json:"deviceTokenPrefix,omitempty"`
	UpdatedAt         string `json:"updatedAt,omitempty"`
}

// apiPlayerFavorites handles GET /api/player/favorites.
//
// Order is newest heart first in both tabs — the store's own ordering,
// carried through rather than re-sorted. hydrateTracks preserves its
// input order, so the tracks tab needs nothing extra to keep it.
func (s *Server) apiPlayerFavorites(w http.ResponseWriter, r *http.Request) {
	cat, ok := s.playerCatalog(w, r)
	if !ok {
		return
	}
	meta, favTracks, favAlbums, err := s.deps.Manifest.ListFavoritesForAdmin(r.Context())
	if err != nil {
		logger.Error("player favorites", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	resp := playerFavoritesResponse{
		Stored:     meta != nil,
		Albums:     []playerAlbumDTO{},
		Tracks:     []playerTrackDTO{},
		SnapshotAt: snapshotStamp(cat.BuiltAt),
	}
	if meta == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.DeviceTokenPrefix = redactDeviceToken(meta.DeviceToken)
	resp.DeviceName = s.deviceNamesByToken(r)[meta.DeviceToken]
	resp.UpdatedAt = nsToRFC3339(meta.UpdatedAt)

	for _, fa := range favAlbums {
		album, found := cat.AlbumByID(favoriteAlbumID(fa.AlbumArtist, fa.Album, fa.Year))
		if !found {
			resp.UnresolvedAlbums++
			continue
		}
		resp.Albums = append(resp.Albums, albumDTO(album, s.routedOnline))
	}

	// Foreign entries are opaque references the bridge never resolves —
	// they carry no local path by construction, so they can only ever be
	// counted. Local entries whose file has since gone are dropped by
	// hydrateTracks and land in the same count.
	paths := make([]string, 0, len(favTracks))
	for _, ft := range favTracks {
		if !ft.Foreign && ft.Path != "" {
			paths = append(paths, ft.Path)
		}
	}
	tracks, err := s.hydrateTracks(r, cat, paths)
	if err != nil {
		logger.Error("player favorites tracks", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if tracks != nil {
		resp.Tracks = tracks
	}
	resp.UnresolvedTracks = len(favTracks) - len(resp.Tracks)
	writeJSON(w, http.StatusOK, resp)
}

// favoriteAlbumID maps a stored favorite album's display triple onto the
// catalog's album id.
//
// The wire stores album favorites as (albumArtist, album, year) — which
// is exactly the input to the client's album identity, and
// internal/dupes is a verbatim mirror of that identity. So this is a
// lookup by the SAME key the catalog grouped on, not a resemblance
// test: dupes.AlbumIDOf does its own normalization of all three
// components, and librarycat.HashID is the same digest the builder
// stamped onto Album.ID.
//
// The partially-filled Resolved is exact rather than lucky — album
// identity reads those three fields and nothing else, which is not a
// local assumption but the shape of the client rule dupes mirrors.
// TestFavoriteAlbumIDMatchesTheCatalogsOwnIdentity compares this
// against the catalog's own ids rather than a hard-coded digest, so the
// two derivations cannot drift apart unnoticed.
//
// A miss therefore means the album genuinely is not in this library —
// hearted on another bridge, or removed since — and is reported as
// unresolved. Deliberately no looser fallback (year-insensitive, say):
// a second, fuzzier match would attribute a heart to the wrong album
// while looking like it worked, and the whole point of the mirror is
// that there is ONE definition of album identity on both sides.
func favoriteAlbumID(albumArtist, album string, year int) string {
	return librarycat.HashID(dupes.AlbumIDOf(dupes.Resolved{
		AlbumArtist: albumArtist, Album: album, Year: year,
	}))
}
