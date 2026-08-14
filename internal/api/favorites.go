package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// FavoritesStore is the optional backing store for the /v1/favorites
// backup endpoints. Nil-safe — when unwired the routes return 404
// (feature-off). *manifest.Store satisfies it in production.
//
// Favorites are a USER-WIDE SINGLETON document (the playlists convention:
// every paired device belongs to the bridge operator), replaced wholesale
// per PUT under the client-wall-clock LWW guard. The deviceToken on the
// upsert records last-writer provenance only.
type FavoritesStore interface {
	UpsertFavorites(ctx context.Context, deviceToken string, lastModifiedAt int64,
		tracks []manifest.FavoriteTrackRow, albums []manifest.FavoriteAlbumRow) error
	GetFavorites(ctx context.Context) (*manifest.FavoritesMeta, []manifest.FavoriteTrackRow, []manifest.FavoriteAlbumRow, error)
}

// WithFavoritesStore wires the favorites-backup feature. Advertises the
// "favorites" health-feature flag when set. Returns the receiver.
func (s *Server) WithFavoritesStore(fs FavoritesStore) *Server {
	s.favoritesStore = fs
	return s
}

// favoritesMaxBodyBytes caps PUT /v1/favorites. Typical real payloads are
// tens of KB (hundreds of favorites); the cap matches the playlists PUT
// so a power user's multi-thousand set with foreign references still has
// comfortable headroom. Documented follow-ups if field data ever shows
// multi-MB sets: gzip request-encoding and/or a PATCH-delta route —
// deliberately NOT built in v1.
const favoritesMaxBodyBytes = 16 << 20

// Entry-count caps, enforced at decode time via the streaming-cap decoder
// clones below so a crafted body of minimal entries can't balloon into
// hundreds of MB of structs before a post-decode length guard fires (the
// cappedPlaylistItems rationale).
const (
	maxFavoriteTracks = 50000
	maxFavoriteAlbums = 10000
)

var (
	errTooManyFavoriteTracks = errors.New("favorites has too many tracks")
	errTooManyFavoriteAlbums = errors.New("favorites has too many albums")
	errFavoritesNotArray     = errors.New("favorites entries must be a JSON array")
)

// cappedFavoriteTracks is []favoriteTrackDTO with a decode-time count cap —
// the cappedPlaylistItems pattern (see playlists.go for the amplification
// rationale). Marshalling is unaffected (a named slice serialises like its
// underlying slice), so the GET/409 response DTOs reuse the type.
type cappedFavoriteTracks []favoriteTrackDTO

func (c *cappedFavoriteTracks) UnmarshalJSON(data []byte) error {
	items, err := decodeCappedArray[favoriteTrackDTO](data, maxFavoriteTracks, errTooManyFavoriteTracks)
	if err != nil {
		return err
	}
	*c = items
	return nil
}

// cappedFavoriteAlbums is []favoriteAlbumDTO with a decode-time count cap.
type cappedFavoriteAlbums []favoriteAlbumDTO

func (c *cappedFavoriteAlbums) UnmarshalJSON(data []byte) error {
	items, err := decodeCappedArray[favoriteAlbumDTO](data, maxFavoriteAlbums, errTooManyFavoriteAlbums)
	if err != nil {
		return err
	}
	*c = items
	return nil
}

// decodeCappedArray streams a JSON array into []T, aborting with capErr
// BEFORE decoding the (cap+1)th element so the oversized array is never
// materialised. Tolerates JSON null (→ nil), rejects non-array values and
// trailing garbage — the cappedPlaylistItems contract, generic so the two
// favorites entry types share one implementation.
func decodeCappedArray[T any](data []byte, cap int, capErr error) ([]T, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, errFavoritesNotArray
	}
	items := make([]T, 0)
	for dec.More() {
		if len(items) >= cap {
			return nil, capErr
		}
		var it T
		if err := dec.Decode(&it); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	closeTok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := closeTok.(json.Delim); !ok || d != ']' {
		return nil, errFavoritesNotArray
	}
	// Trailing-garbage guard: `dec.More()` only answers "more elements in
	// the CURRENT array/object", so at the top level it misses e.g. a stray
	// `]` — asserting the next token is io.EOF is the robust form (Gemini
	// on PR #695). Defensive here (the outer object decode hands this
	// exactly one JSON value), load-bearing hygiene nonetheless.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errFavoritesNotArray
	}
	return items, nil
}

// --- wire DTOs (the favorites contract; see PROTOCOL.md → Favorites) ---

type favoriteTrackDTO struct {
	Path              string `json:"path,omitempty"`              // local, resolvable on this bridge
	OriginFingerprint string `json:"originFingerprint,omitempty"` // foreign: owning bridge fp / "local" / "smb" / "upnp"
	OriginPath        string `json:"originPath,omitempty"`
	Title             string `json:"title,omitempty"`
	Artist            string `json:"artist,omitempty"`
	FavoritedAt       int64  `json:"favoritedAt"` // UnixNano UTC
}

type favoriteAlbumDTO struct {
	AlbumArtist string `json:"albumArtist,omitempty"`
	Album       string `json:"album"`
	Year        int    `json:"year,omitempty"` // 0 ≡ absent (the dhowden/tag sentinel)
	FavoritedAt int64  `json:"favoritedAt"`    // UnixNano UTC
}

type favoritesDTO struct {
	LastModifiedAt int64                `json:"lastModifiedAt"` // UnixNano UTC (LWW guard key)
	Tracks         cappedFavoriteTracks `json:"tracks"`
	Albums         cappedFavoriteAlbums `json:"albums"`
}

type favoritesStoredResponse struct {
	Stored bool `json:"stored"`
}

// favoritesStaleResponse is the 409 body: the error envelope plus the FULL
// server copy. LOAD-BEARING for the iOS client — it union-merges from this
// body (never accepts-stale-as-done: favorites are a singleton set, and
// accepting would silently strand a device's offline favorites).
type favoritesStaleResponse struct {
	Error   string       `json:"error"`
	Message string       `json:"message"`
	Server  favoritesDTO `json:"server"`
}

func toFavoritesDTO(meta *manifest.FavoritesMeta,
	tracks []manifest.FavoriteTrackRow, albums []manifest.FavoriteAlbumRow) favoritesDTO {
	out := favoritesDTO{
		// Empty slices, not nil — the wire arrays must encode as [] so the
		// never-stored empty doc is shape-identical to a stored-empty one.
		Tracks: make(cappedFavoriteTracks, 0, len(tracks)),
		Albums: make(cappedFavoriteAlbums, 0, len(albums)),
	}
	if meta != nil {
		out.LastModifiedAt = meta.LastModifiedAt
	}
	for _, t := range tracks {
		out.Tracks = append(out.Tracks, favoriteTrackDTO{
			Path:              t.Path,
			OriginFingerprint: t.OriginFingerprint,
			OriginPath:        t.OriginPath,
			Title:             t.Title,
			Artist:            t.Artist,
			FavoritedAt:       t.FavoritedAt,
		})
	}
	for _, a := range albums {
		out.Albums = append(out.Albums, favoriteAlbumDTO{
			AlbumArtist: a.AlbumArtist,
			Album:       a.Album,
			Year:        a.Year,
			FavoritedAt: a.FavoritedAt,
		})
	}
	return out
}

// requireFavoritesFeature returns the device token, or writes the
// appropriate error and returns ("", false). The header stays REQUIRED on
// both routes (the playlists convention): it identifies the writer on PUT
// (last-writer provenance) and keeps the device-registration binding
// fresh on every call.
func (s *Server) requireFavoritesFeature(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.favoritesStore == nil {
		writeError(w, http.StatusNotFound, "favorites_not_supported",
			"this bridge does not store favorites backups")
		return "", false
	}
	dt := deviceTokenFromContext(r.Context())
	if dt == "" {
		writeError(w, http.StatusBadRequest, "device_token_required",
			"favorites backup requires the X-Device-Token header")
		return "", false
	}
	return dt, true
}

// getFavorites handles GET /v1/favorites — the full user-wide document.
// Never-stored serves an EMPTY doc with lastModifiedAt 0 (singleton
// semantics — never a 404-as-missing; 404 is reserved for feature-off).
func (s *Server) getFavorites(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireFavoritesFeature(w, r); !ok {
		return
	}
	meta, tracks, albums, err := s.favoritesStore.GetFavorites(r.Context())
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to read favorites", err)
		return
	}
	writeJSON(w, http.StatusOK, toFavoritesDTO(meta, tracks, albums))
}

// putFavorites handles PUT /v1/favorites — wholesale replace under the
// LWW guard. An empty set is valid ("no favorites"); there is no DELETE
// route.
func (s *Server) putFavorites(w http.ResponseWriter, r *http.Request) {
	dt, ok := s.requireFavoritesFeature(w, r)
	if !ok {
		return
	}

	var body favoritesDTO
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, favoritesMaxBodyBytes))
	if err := dec.Decode(&body); err != nil {
		// Entry-count caps are enforced DURING decode (see the capped
		// decoders above); map the sentinels to clean 400s.
		if errors.Is(err, errTooManyFavoriteTracks) {
			writeError(w, http.StatusBadRequest, "bad_request", "favorites has too many tracks")
			return
		}
		if errors.Is(err, errTooManyFavoriteAlbums) {
			writeError(w, http.StatusBadRequest, "bad_request", "favorites has too many albums")
			return
		}
		writeErrorLog(w, r, http.StatusBadRequest, "bad_request",
			"request body must be a favorites JSON object", err)
		return
	}
	// Reject trailing top-level JSON values (`{...} {}`): Decode reads only
	// the FIRST value, so without this a body carrying extra documents would
	// be silently accepted (CodeRabbit on PR #695).
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request",
			"request body must be a single favorites JSON object")
		return
	}
	if body.LastModifiedAt <= 0 {
		writeError(w, http.StatusBadRequest, "bad_request",
			"lastModifiedAt must be a positive UnixNano value")
		return
	}

	// Validate + normalize tracks, then dedup last-wins. Dedup is KEPT
	// even with the DB's partial UNIQUE indexes — a duplicate-bearing
	// payload must store cleanly (200), never constraint-fail into a 500;
	// the indexes are the integrity backstop for handler regressions.
	// Comparable STRUCT keys, never delimiter-joined strings — JSON permits
	// escaped NULs, so two distinct user-controlled fields could collide a
	// "\x00"-joined key and silently overwrite each other (CodeRabbit on
	// PR #695).
	type trackKey struct{ path, originFingerprint, originPath string }
	trackByKey := make(map[trackKey]int, len(body.Tracks))
	tracks := make([]manifest.FavoriteTrackRow, 0, len(body.Tracks))
	for _, t := range body.Tracks {
		// Strict local-XOR-foreign — the playlist-item predicate.
		isLocal := t.Path != "" && t.OriginFingerprint == "" && t.OriginPath == ""
		isForeign := t.Path == "" && t.OriginFingerprint != "" && t.OriginPath != ""
		if !isLocal && !isForeign {
			writeError(w, http.StatusBadRequest, "bad_request",
				"each favorite track must set either path (local) or both originFingerprint and originPath (foreign), and not mix them")
			return
		}
		if t.FavoritedAt <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request",
				"favorite track favoritedAt must be a positive UnixNano value")
			return
		}
		row := manifest.FavoriteTrackRow{
			Path:              strings.ReplaceAll(t.Path, `\`, "/"),
			OriginFingerprint: t.OriginFingerprint,
			OriginPath:        strings.ReplaceAll(t.OriginPath, `\`, "/"),
			Title:             t.Title,
			Artist:            t.Artist,
			FavoritedAt:       t.FavoritedAt,
		}
		// Strip a leading slash on LOCAL paths only: iOS normalizes
		// bridge-source paths with a leading "/", the scanner stores
		// track paths without one, and favorites join the slashless
		// `tracks` PK for the admin display + the smart-mix family (the
		// history.go precedent). Foreign originPath stays opaque.
		if row.Path != "" {
			row.Path = strings.TrimPrefix(row.Path, "/")
			if row.Path == "" {
				writeError(w, http.StatusBadRequest, "bad_request",
					"favorite track path must not be empty after normalization")
				return
			}
		}
		var key trackKey
		if row.Path != "" {
			key.path = row.Path
		} else {
			key.originFingerprint = row.OriginFingerprint
			key.originPath = row.OriginPath
		}
		if idx, dup := trackByKey[key]; dup {
			tracks[idx] = row // last-wins
			continue
		}
		trackByKey[key] = len(tracks)
		tracks = append(tracks, row)
	}

	type albumKey struct {
		albumArtist, album string
		year               int
	}
	albumByKey := make(map[albumKey]int, len(body.Albums))
	albums := make([]manifest.FavoriteAlbumRow, 0, len(body.Albums))
	for _, a := range body.Albums {
		if a.Album == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "favorite album name is required")
			return
		}
		if a.Year < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "favorite album year must not be negative")
			return
		}
		if a.FavoritedAt <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request",
				"favorite album favoritedAt must be a positive UnixNano value")
			return
		}
		row := manifest.FavoriteAlbumRow{
			AlbumArtist: a.AlbumArtist, Album: a.Album, Year: a.Year, FavoritedAt: a.FavoritedAt,
		}
		key := albumKey{albumArtist: row.AlbumArtist, album: row.Album, year: row.Year}
		if idx, dup := albumByKey[key]; dup {
			albums[idx] = row // last-wins
			continue
		}
		albumByKey[key] = len(albums)
		albums = append(albums, row)
	}

	switch err := s.favoritesStore.UpsertFavorites(r.Context(), dt, body.LastModifiedAt, tracks, albums); {
	case errors.Is(err, manifest.ErrFavoritesStale):
		// Re-read the server copy so iOS can union-merge in one round-trip
		// (the load-bearing half of the 409 contract). A FAILED re-read is a
		// database error, not a conflict — 500 it honestly rather than
		// emitting a 409 whose body violates the full-server-copy contract
		// (Gemini + CodeRabbit on PR #695). meta == nil (stale yet nothing
		// stored — practically unreachable) degrades to a body-less 409;
		// iOS treats the undecodable stale body as a transport failure and
		// stays dirty for the next sweep.
		meta, sTracks, sAlbums, gerr := s.favoritesStore.GetFavorites(r.Context())
		if gerr != nil {
			writeErrorLog(w, r, http.StatusInternalServerError, "internal",
				"failed to read favorites for conflict resolution", gerr)
			return
		}
		if meta == nil {
			writeError(w, http.StatusConflict, "stale", "server copy is newer")
			return
		}
		writeJSON(w, http.StatusConflict, favoritesStaleResponse{
			Error: "stale", Message: "server copy is newer",
			Server: toFavoritesDTO(meta, sTracks, sAlbums),
		})
		return
	case err != nil:
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to store favorites", err)
		return
	}
	writeJSON(w, http.StatusOK, favoritesStoredResponse{Stored: true})
}
