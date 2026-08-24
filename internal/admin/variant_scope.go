package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/librarycat"
)

// Variant work is scoped one of two ways, and the difference is not
// cosmetic.
//
// A FOLDER scope is a path prefix: the store walks everything under it.
// That is the right shape for the Folders view, a library root, and the
// whole library (`""`).
//
// An IDENTITY scope — an album, an artist, a hand-picked track — is a
// SET of tracks, and it cannot be expressed as a prefix. An album's
// directory is `commonDir()` of its tracks, and on the reference
// library 69 of 880 albums share that directory with another album; one
// artist folder holds 18 albums flat. Submitting such an album by
// prefix enqueues all 18, and deleting by prefix reclaims all 18 albums'
// sidecars. A single track is the mirror image: the prefix query builds
// `<base>/%`, which matches strict descendants, so a file path matches
// nothing at all.
//
// So identity scopes travel as IDS and are expanded here, server-side,
// against the same catalog snapshot that produced them. That also keeps
// the wire small — an artist with 3,000 tracks costs one 16-hex id, not
// 3,000 paths. There is deliberately no `album_id` column to expand
// against in SQL: album identity is `dupes.AlbumIDOf(dupes.Resolve(row))`,
// folded at catalog-build time and never stored (see librarycat/doc.go).
type variantScope struct {
	// Label is display only. It lands in `upscale_batches.path` and on
	// the Jobs page row; nothing derives scope from it.
	Label string

	// Prefix is the folder form. Valid only when ByIdentity is false;
	// empty then means the whole library.
	Prefix string

	// Paths is the identity form: the exact tracks to act on.
	Paths []string

	ByIdentity bool
}

// Caps. albumIds is sized for a grid multi-select; trackPaths is
// deliberately much smaller because it is the only form that carries
// paths on the wire — anything larger has an id form to use instead.
const (
	maxScopeAlbumIDs   = 500
	maxScopeTrackPaths = 64
)

// scopeRequest is the subset of a request body that names a scope.
// Embedded by the concrete request types so the submit and delete
// endpoints cannot drift on what a scope means.
type scopeRequest struct {
	Path       string   `json:"path,omitempty"`
	AlbumIDs   []string `json:"albumIds,omitempty"`
	ArtistID   string   `json:"artistId,omitempty"`
	TrackPaths []string `json:"trackPaths,omitempty"`
}

// scopeError carries an HTTP status + error code so callers can reply
// without re-deriving the classification.
type scopeError struct {
	Status  int
	Code    string
	Message string
}

func (e *scopeError) Error() string { return e.Message }

func badScope(code, msg string) *scopeError {
	return &scopeError{Status: http.StatusBadRequest, Code: code, Message: msg}
}

// resolveVariantScope turns a request body into a scope, expanding any
// identity form against the live catalog.
//
// A body with no identity field is the folder form, which is what keeps
// every pre-existing caller working unchanged.
func (s *Server) resolveVariantScope(r *http.Request, req scopeRequest) (variantScope, *scopeError) {
	// PRESENCE, not emptiness. A field that arrived as `[]` is an
	// identity scope that names nothing — a client that serialised an
	// empty selection — and it must not be read as the absence of an
	// identity scope, because the absence of one means the FOLDER form
	// and an empty folder path means THE WHOLE LIBRARY. Read wrongly,
	// `{"albumIds": []}` upscales every track the bridge has.
	//
	// encoding/json leaves an absent field nil and a present `[]`
	// non-nil, which is exactly the distinction needed. (`null` decodes
	// to nil and reads as absent; that is the right call — a client
	// sending null is saying "no value", not "an empty selection".)
	forms := 0
	if req.AlbumIDs != nil {
		forms++
	}
	if req.ArtistID != "" {
		forms++
	}
	if req.TrackPaths != nil {
		forms++
	}
	if forms > 1 {
		return variantScope{}, badScope("ambiguous-scope",
			"give exactly one of albumIds, artistId or trackPaths")
	}
	if forms == 1 && len(req.AlbumIDs) == 0 && len(req.TrackPaths) == 0 && req.ArtistID == "" {
		return variantScope{}, badScope("empty-scope",
			"albumIds / trackPaths must not be empty; omit the field for the whole library")
	}
	if forms == 0 {
		// Folder form. normaliseBrowsePath is the same helper the
		// read-side endpoints use: it strips ALL leading slashes (not
		// just one) and maps path.Clean's "." back to "". Hand-rolling
		// that is how the `Clean("") == "."` trap gets reintroduced,
		// and forwarding verbatim is how `{"path": "//"}` once enqueued
		// the entire library.
		normalised, ok := normaliseBrowsePath(req.Path)
		if !ok {
			return variantScope{}, badScope("bad-path",
				"path must be a clean library-relative path (no traversal, no backslashes)")
		}
		return variantScope{Label: normalised, Prefix: normalised}, nil
	}
	if req.Path != "" {
		return variantScope{}, badScope("ambiguous-scope",
			"path is the folder form; it cannot be combined with albumIds, artistId or trackPaths")
	}

	if req.TrackPaths != nil {
		return resolveTrackPathScope(req.TrackPaths)
	}

	cat, err := s.libraryCatalog(r.Context())
	if err != nil {
		if errors.Is(err, errCatalogTooLarge) {
			return variantScope{}, &scopeError{
				Status:  http.StatusServiceUnavailable,
				Code:    "catalog_too_large",
				Message: "this library is too large for the in-memory catalog album and artist scopes use",
			}
		}
		// A browser that navigated away mid-request cancels the
		// context, which surfaces here as a catalog build failure. That
		// is not an operator-actionable fault, and logging it at Error
		// trains people to ignore the level that matters.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			logger.Debug("resolve variant scope: request cancelled", "err", err)
		} else {
			logger.Error("resolve variant scope: build catalog", "err", err)
		}
		return variantScope{}, &scopeError{
			Status:  http.StatusInternalServerError,
			Code:    "internal",
			Message: "could not build the library catalog",
		}
	}
	if req.AlbumIDs != nil {
		return albumScope(cat, req.AlbumIDs)
	}
	return artistScope(cat, req.ArtistID)
}

func resolveTrackPathScope(raw []string) (variantScope, *scopeError) {
	if len(raw) > maxScopeTrackPaths {
		return variantScope{}, badScope("too-many-tracks",
			fmt.Sprintf("at most %d trackPaths (use albumIds or artistId for larger scopes)", maxScopeTrackPaths))
	}
	paths := make([]string, 0, len(raw))
	for _, p := range raw {
		// Every expanded path goes through the same normalisation the
		// folder form gets. These arrive from a client, so they are the
		// one identity form that is not catalog-derived.
		normalised, ok := normaliseBrowsePath(p)
		if !ok || normalised == "" {
			return variantScope{}, badScope("bad-path",
				"trackPaths entries must be clean library-relative paths")
		}
		paths = append(paths, normalised)
	}
	paths = dedupePaths(paths)
	label := paths[0]
	if len(paths) > 1 {
		label = fmt.Sprintf("%d tracks", len(paths))
	}
	return variantScope{Label: label, Paths: paths, ByIdentity: true}, nil
}

func albumScope(cat *librarycat.Catalog, ids []string) (variantScope, *scopeError) {
	if len(ids) > maxScopeAlbumIDs {
		return variantScope{}, badScope("too-many-albums",
			fmt.Sprintf("at most %d albumIds per request", maxScopeAlbumIDs))
	}
	var (
		paths []string
		first librarycat.Album
	)
	for i, id := range ids {
		if !playerIDPattern.MatchString(id) {
			return variantScope{}, badScope("bad-request", "invalid album id")
		}
		album, found := cat.AlbumByID(id)
		if !found {
			return variantScope{}, &scopeError{
				Status: http.StatusNotFound, Code: "not_found",
				Message: "no such album: " + id,
			}
		}
		if i == 0 {
			first = album
		}
		paths = append(paths, album.TrackPaths...)
	}
	label := albumLabel(first)
	if len(ids) > 1 {
		label = fmt.Sprintf("%d albums", len(ids))
	}
	return variantScope{Label: label, Paths: dedupePaths(paths), ByIdentity: true}, nil
}

func artistScope(cat *librarycat.Catalog, id string) (variantScope, *scopeError) {
	if !playerIDPattern.MatchString(id) {
		return variantScope{}, badScope("bad-request", "invalid artist id")
	}
	artist, found := cat.ArtistByID(id)
	if !found {
		return variantScope{}, &scopeError{
			Status: http.StatusNotFound, Code: "not_found",
			Message: "no such artist: " + id,
		}
	}
	var paths []string
	for _, albumID := range artist.AlbumIDs {
		// A missing album is skipped rather than fatal: AlbumIDs and
		// Albums come from the same immutable snapshot, so a miss means
		// a contract violation in Build, and refusing the whole request
		// over one would be worse than acting on the rest.
		if album, ok := cat.AlbumByID(albumID); ok {
			paths = append(paths, album.TrackPaths...)
		}
	}
	return variantScope{
		Label:      artist.Name,
		Paths:      dedupePaths(paths),
		ByIdentity: true,
	}, nil
}

// albumLabel is what an operator sees on the Jobs page. FolderPath is
// preferred because it is the only value that is both meaningful and a
// real location; it is empty exactly when the album's tracks share no
// common directory, and then the title is all there is.
func albumLabel(a librarycat.Album) string {
	if a.FolderPath != "" {
		return a.FolderPath
	}
	if a.AlbumArtist != "" {
		return strings.TrimSpace(a.AlbumArtist + " — " + a.Title)
	}
	return a.Title
}

// dedupePaths keeps first-seen order. An artist scope unions its albums
// and a multi-album scope can overlap, and while the store's `IN (…)`
// would dedupe on its own, the batch row's TotalFiles and the pool's
// dedup key are both easier to reason about from a set.
func dedupePaths(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
