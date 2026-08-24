package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/librarycat"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The player's playlist and smart-mix surface.
//
// Both existed as inventory listings only: flat, unclickable rows with a
// count, no artwork, and no way to hear anything. The operator-facing
// endpoints they read are summaries-only by design — /api/playlists
// backs the Data page's table, and /api/smart-playlists deliberately
// carries no items because the operator page server-renders its own.
//
// So the player gets its own read surface rather than widening theirs:
// tiles that carry cover refs, and detail responses hydrated through the
// SAME hydrateTracks the album page uses, so Play and Shuffle behave
// identically to an album and playability is resolved the same way.

// mosaicCovers is how many covers a tile mosaic uses. Four is the 2x2
// most players show; fewer collapses to a single full-bleed cover.
const mosaicCovers = 4

// mosaicScanDepth is how far into a collection to look for those four
// DISTINCT covers. A playlist often opens with several tracks from one
// album, so scanning only the first four would routinely yield one
// cover for a varied playlist. Bounded because this runs per tile.
const mosaicScanDepth = 40

type playerCollectionDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Count    int    `json:"count"`
	Kind     string `json:"kind,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	HasCover bool   `json:"hasCover,omitempty"`
	// Covers are up to mosaicCovers artwork refs, most-representative
	// first, for the tile mosaic. Empty when nothing in the collection
	// resolves to a local album with artwork.
	Covers []playerCoverRefDTO `json:"covers,omitempty"`
}

// playerCoverRefDTO is the pair coverURL() needs, named to match
// playerAlbumDTO so the client helper works on it unchanged.
type playerCoverRefDTO struct {
	ArtworkMBID    string `json:"artworkMBID"`
	ArtworkVersion string `json:"artworkVersion,omitempty"`
}

type playerCollectionsResponse struct {
	Collections []playerCollectionDTO `json:"collections"`
	SnapshotAt  string                `json:"snapshotAt"`
}

type playerCollectionDetailResponse struct {
	Collection playerCollectionDTO `json:"collection"`
	Tracks     []playerTrackDTO    `json:"tracks"`
	// Unresolved counts members that could not be turned into playable
	// rows — foreign refs from another bridge, or tracks deleted since
	// the backup. Reported rather than hidden: silently dropping them
	// makes the detail page disagree with the count on the tile, and the
	// operator's own Data page.
	Unresolved int    `json:"unresolved,omitempty"`
	SnapshotAt string `json:"snapshotAt"`
}

// apiPlayerPlaylists handles GET /api/player/playlists.
func (s *Server) apiPlayerPlaylists(w http.ResponseWriter, r *http.Request) {
	cat, ok := s.playerCatalog(w, r)
	if !ok {
		return
	}
	rows, err := s.deps.Manifest.ListAllPlaylistsForAdmin(r.Context())
	if err != nil {
		logger.Error("player playlists", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	// One query for every tile's leading paths, not one per playlist.
	heads, err := s.deps.Manifest.PlaylistHeadPaths(r.Context(), mosaicScanDepth)
	if err != nil {
		// Mosaics are decoration; a failure here costs artwork, not the
		// listing.
		logger.Warn("player playlists: head paths", "err", err)
		heads = nil
	}
	covers := s.coverSet(r, manifest.CoverScopePlaylist)

	out := make([]playerCollectionDTO, 0, len(rows))
	for _, p := range rows {
		_, hasCover := covers[strings.ToLower(p.ID)]
		out = append(out, playerCollectionDTO{
			ID: p.ID, Name: p.Name, Count: p.TrackCount,
			HasCover: hasCover,
			Covers:   mosaicFor(cat, heads[p.ID]),
		})
	}
	writeJSON(w, http.StatusOK, playerCollectionsResponse{
		Collections: out, SnapshotAt: snapshotStamp(cat.BuiltAt),
	})
}

// apiPlayerPlaylistDetail handles GET /api/player/playlists/{id}.
func (s *Server) apiPlayerPlaylistDetail(w http.ResponseWriter, r *http.Request) {
	cat, ok := s.playerCatalog(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if id == "" || len(id) > maxPlayerCollectionID {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid playlist id")
		return
	}
	// Id-scoped, no device token: playlists stopped being per-device in
	// v1.7, so the player needs none — unlike the older
	// /api/playlists/detail?device=&id=.
	row, items, err := s.deps.Manifest.GetPlaylist(r.Context(), id)
	if err != nil {
		logger.Error("player playlist detail", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if row == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such playlist")
		return
	}
	paths := make([]string, 0, len(items))
	for _, it := range items {
		if it.Path != "" {
			paths = append(paths, it.Path)
		}
	}
	tracks, err := s.hydrateTracks(r, cat, paths)
	if err != nil {
		logger.Error("player playlist tracks", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	covers := s.coverSet(r, manifest.CoverScopePlaylist)
	_, hasCover := covers[strings.ToLower(row.ID)]
	writeJSON(w, http.StatusOK, playerCollectionDetailResponse{
		Collection: playerCollectionDTO{
			ID: row.ID, Name: row.Name, Count: len(items), HasCover: hasCover,
			Covers: mosaicFor(cat, paths),
		},
		Tracks:     tracks,
		Unresolved: len(items) - len(tracks),
		SnapshotAt: snapshotStamp(cat.BuiltAt),
	})
}

// apiPlayerMixes handles GET /api/player/mixes.
func (s *Server) apiPlayerMixes(w http.ResponseWriter, r *http.Request) {
	cat, ok := s.playerCatalog(w, r)
	if !ok {
		return
	}
	rows, err := s.deps.Manifest.LoadSmartPlaylists(r.Context())
	if err != nil {
		logger.Error("player mixes", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	covers := s.coverSet(r, manifest.CoverScopeSmartMix)
	out := make([]playerCollectionDTO, 0, len(rows))
	for _, m := range rows {
		_, hasCover := covers[strings.ToLower(m.Slug)]
		out = append(out, playerCollectionDTO{
			ID: m.Slug, Name: m.Title, Kind: m.Kind, Subtitle: m.Subtitle,
			Count:    smartPlaylistItemCount(m),
			HasCover: hasCover,
			Covers:   mosaicFor(cat, smartMixHeadPaths(m, mosaicScanDepth)),
		})
	}
	writeJSON(w, http.StatusOK, playerCollectionsResponse{
		Collections: out, SnapshotAt: snapshotStamp(cat.BuiltAt),
	})
}

// apiPlayerMixDetail handles GET /api/player/mixes/{slug}.
func (s *Server) apiPlayerMixDetail(w http.ResponseWriter, r *http.Request) {
	cat, ok := s.playerCatalog(w, r)
	if !ok {
		return
	}
	slug := r.PathValue("slug")
	if !smartMixSlugPattern.MatchString(slug) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid mix slug")
		return
	}
	rows, err := s.deps.Manifest.LoadSmartPlaylists(r.Context())
	if err != nil {
		logger.Error("player mix detail", "slug", slug, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	// Linear scan, not a binary search: LoadSmartPlaylists orders by
	// POSITION (the homepage order), not by slug, and the family count
	// is in the low tens.
	var row *manifest.StoredSmartPlaylist
	for j := range rows {
		if rows[j].Slug == slug {
			row = &rows[j]
			break
		}
	}
	if row == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such mix")
		return
	}
	// smartMixTracksForView already flattens the time-of-day family's 24
	// hourly pools into one deduped list — reused rather than re-derived
	// so the player and the operator page agree on what a mix contains.
	views := smartMixTracksForView(*row)
	paths := make([]string, 0, len(views))
	for _, v := range views {
		if v.Path != "" {
			paths = append(paths, v.Path)
		}
	}
	tracks, err := s.hydrateTracks(r, cat, paths)
	if err != nil {
		logger.Error("player mix tracks", "slug", slug, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	covers := s.coverSet(r, manifest.CoverScopeSmartMix)
	_, hasCover := covers[strings.ToLower(row.Slug)]
	writeJSON(w, http.StatusOK, playerCollectionDetailResponse{
		Collection: playerCollectionDTO{
			ID: row.Slug, Name: row.Title, Kind: row.Kind, Subtitle: row.Subtitle,
			Count: len(views), HasCover: hasCover, Covers: mosaicFor(cat, paths),
		},
		Tracks:     tracks,
		Unresolved: len(views) - len(tracks),
		SnapshotAt: snapshotStamp(cat.BuiltAt),
	})
}

// mosaicFor picks up to mosaicCovers DISTINCT album covers from a
// collection's leading paths.
//
// Albums with no artwork are skipped BEFORE the count is reached, not
// after: an empty ArtworkMBID would otherwise consume a quadrant and
// render as a hole in the mosaic. Fewer than four survivors is a normal
// outcome, and the client collapses to a single cover rather than
// drawing a partial grid.
func mosaicFor(cat *librarycat.Catalog, paths []string) []playerCoverRefDTO {
	if len(paths) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, mosaicCovers)
	out := make([]playerCoverRefDTO, 0, mosaicCovers)
	for _, p := range paths {
		albumID, ok := cat.AlbumIDForPath(p)
		if !ok {
			continue
		}
		if _, dup := seen[albumID]; dup {
			continue
		}
		album, ok := cat.AlbumByID(albumID)
		if !ok || album.ArtworkMBID == "" {
			continue
		}
		seen[albumID] = struct{}{}
		out = append(out, playerCoverRefDTO{
			ArtworkMBID: album.ArtworkMBID, ArtworkVersion: album.ArtworkVersion,
		})
		if len(out) == mosaicCovers {
			break
		}
	}
	return out
}

// smartMixHeadPaths returns a mix's leading member paths, bounded.
func smartMixHeadPaths(row manifest.StoredSmartPlaylist, limit int) []string {
	views := smartMixTracksForView(row)
	if len(views) > limit {
		views = views[:limit]
	}
	out := make([]string, 0, len(views))
	for _, v := range views {
		if v.Path != "" {
			out = append(out, v.Path)
		}
	}
	return out
}

// coverSet returns the keys in a cover scope that have an uploaded
// image, lowercased to match the serve route's normalization.
//
// A failure degrades to "no covers": the mosaic is the fallback, so the
// tile still renders.
func (s *Server) coverSet(r *http.Request, scope string) map[string]manifest.PlaylistCover {
	got, err := s.deps.Manifest.PlaylistCoversByScope(r.Context(), scope)
	if err != nil {
		logger.Warn("player collection covers", "scope", scope, "err", err)
		return nil
	}
	out := make(map[string]manifest.PlaylistCover, len(got))
	for k, v := range got {
		out[strings.ToLower(k)] = v
	}
	return out
}

// maxPlayerCollectionID bounds a playlist id before it reaches the
// store. Mirrors the wire's own maxPlaylistIDLen.
const maxPlayerCollectionID = 128

// smartMixSlugPattern bounds a family slug. Slugs are generated by the
// engine from a closed set of family names, so a strict lowercase-and-
// dash alphabet is the whole space.
var smartMixSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// collectionCoverScopes is the closed set of scopes the cover route
// serves. Validated against the manifest constants rather than passed
// through, because the value becomes part of a filename.
var collectionCoverScopes = map[string]bool{
	manifest.CoverScopePlaylist: true,
	manifest.CoverScopeSmartMix: true,
}

// apiPlayerCollectionCover handles
// GET /api/library/collection-cover/{scope}/{key}.
//
// The loopback twin of /v1/{playlist,smart-playlist}-image, which are
// BEARER-authed and therefore unreachable from the cookie-authed
// console. Same files, same normalization; no new auth layer, matching
// the admin posture.
func (s *Server) apiPlayerCollectionCover(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeError(w, http.StatusNotFound, "not_found", "covers not wired")
		return
	}
	cfg := s.deps.CfgHolder.Load()
	if cfg == nil {
		writeError(w, http.StatusInternalServerError, "internal", "config unavailable")
		return
	}
	scope := r.PathValue("scope")
	if !collectionCoverScopes[scope] {
		writeError(w, http.StatusBadRequest, "bad_request", "unknown cover scope")
		return
	}
	// Lowercased + trimmed to match how the covers were stored, then
	// bounded BEFORE any path join — the value ends up in a filename.
	key := strings.ToLower(strings.TrimSpace(r.PathValue("key")))
	if key == "" || len(key) > maxPlayerCollectionID || !collectionKeyPattern.MatchString(key) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid cover key")
		return
	}
	cover, ok, err := s.deps.Manifest.GetPlaylistCover(r.Context(), scope, key)
	if err != nil {
		logger.Error("collection cover lookup", "scope", scope, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no cover")
		return
	}
	name := manifest.PlaylistCoverFilename(scope, key, cover.Ext)
	full := filepath.Join(manifest.PlaylistCoverDir(cfg.DataDir), name)
	if _, err := os.Stat(full); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no cover")
		return
	}
	// The image hash IS a content key, so this response is immutable —
	// the same reasoning artworkCacheControl applies to a versioned
	// cover.
	cc := "private, max-age=86400"
	if r.URL.Query().Get("v") != "" {
		cc = "private, max-age=31536000, immutable"
	}
	serveCacheFile(w, r, full, coverContentType(cover.Ext), cc)
}

// collectionKeyPattern bounds a cover key: a playlist UUID or a family
// slug, both of which live in this alphabet.
var collectionKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)

func coverContentType(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "png":
		return "image/png"
	case "webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}
