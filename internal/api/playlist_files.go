package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// PlaylistFileStore is the backing store for GET /v1/playlist-files
// (backlog B431, paged since B442): the `.m3u` / `.m3u8` files the scanner
// read from the library, a page at a time, with their entries resolved
// against the tracks it holds now. *manifest.Store satisfies it. Read-only:
// the scanner writes the index, and nothing writes a playlist file.
type PlaylistFileStore interface {
	PlaylistFilesPage(ctx context.Context, version, after string) (manifest.PlaylistFilePage, error)
}

// WithPlaylistFileStore wires the playlist-file feed and advertises the
// "playlistFiles" and "playlistFilesPaged" health-feature flags. Returns
// the receiver.
func (s *Server) WithPlaylistFileStore(st PlaylistFileStore) *Server {
	s.playlistFileStore = st
	return s
}

// --- wire DTOs (additive; PROTOCOL.md "Playlist files") ---

// playlistFileDTO is one playlist file on the wire. Members and Unresolved
// are always arrays, never null.
type playlistFileDTO struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	MTime      time.Time `json:"mtime"`
	Members    []string  `json:"members"`
	Unresolved []string  `json:"unresolved"`
	TooLarge   bool      `json:"tooLarge,omitempty"`
}

// playlistFilesResponse is one page of GET /v1/playlist-files. Its JSON is
// what the page's ETag hashes (playlistFilesETag). Version and Complete are
// always sent (false is the answer a client must not reap on); NextCursor
// is absent on the last page.
type playlistFilesResponse struct {
	Version    string            `json:"version"`
	Complete   bool              `json:"complete"`
	Playlists  []playlistFileDTO `json:"playlists"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

// errPlaylistFilesCursor is an `after` this bridge did not make.
var errPlaylistFilesCursor = errors.New("api: not a playlist-files cursor")

// encodePlaylistFilesCursor is the opaque `nextCursor` for the page after
// the one that ends at path, read at version: both, NUL-separated (no
// manifest path holds a NUL, and a path's bytes need not be UTF-8), in
// unpadded base64url so it travels in a query string as it is.
func encodePlaylistFilesCursor(version, path string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(version + "\x00" + path))
}

// decodePlaylistFilesCursor is encodePlaylistFilesCursor's inverse: the
// version the cursor was made at and the path the next page starts after.
func decodePlaylistFilesCursor(cursor string) (version, path string, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", errPlaylistFilesCursor
	}
	v, p, ok := bytes.Cut(raw, []byte{0})
	if !ok || len(v) == 0 || len(p) == 0 {
		return "", "", errPlaylistFilesCursor
	}
	return string(v), string(p), nil
}

// nonNilStrings is s, or an empty list for nil, so the wire never carries
// null for a list.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// playlistFiles handles GET /v1/playlist-files: one page of the playlist
// files the index holds, in manifest-path order (backlog B442). The first
// page is the request with no `after`; each page but the last carries the
// `nextCursor` the next request passes as `after`. Every page carries the
// feed's `version`, and a cursor made at another version is a 410
// `playlist_files_stale`: the feed changed under the client's read, which
// starts again from the first page, so a read that reaches the last page
// is one consistent state. Bearer-authenticated, user-wide (no
// X-Device-Token), read-only. Each page carries a strong ETag over its
// document, and an If-None-Match that matches it is a 304 with no body.
//
// Errors go through the shared writers with fixed messages (B425): an
// `after` this bridge did not make is a 400 `bad_request`, a store failure
// is a 500 `internal` logged at Error, and a client that went away mid-read
// is the Info line and 499 writeErrorLog records (B364).
func (s *Server) playlistFiles(w http.ResponseWriter, r *http.Request) {
	if s.playlistFileStore == nil {
		writeError(w, http.StatusNotFound, "playlist_files_not_supported",
			"this bridge does not index playlist files")
		return
	}
	var want, after string
	if cursor := r.URL.Query().Get("after"); cursor != "" {
		var err error
		if want, after, err = decodePlaylistFilesCursor(cursor); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request",
				"after is not a cursor this bridge gave; start from the first page")
			return
		}
	}
	page, err := s.playlistFileStore.PlaylistFilesPage(r.Context(), want, after)
	if errors.Is(err, manifest.ErrPlaylistFilesChanged) {
		writeError(w, http.StatusGone, "playlist_files_stale",
			"the playlist files changed since this cursor; start again from the first page")
		return
	}
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't read its playlist files", err)
		return
	}
	resp := playlistFilesResponse{Version: page.Version, Complete: page.Complete,
		Playlists: make([]playlistFileDTO, 0, len(page.Files))}
	if page.Next != "" {
		resp.NextCursor = encodePlaylistFilesCursor(page.Version, page.Next)
	}
	for _, f := range page.Files {
		resp.Playlists = append(resp.Playlists, playlistFileDTO{
			Path:       f.Path,
			Name:       f.Name,
			MTime:      f.ModTime.UTC(),
			Members:    nonNilStrings(f.Members),
			Unresolved: nonNilStrings(f.Unresolved),
			TooLarge:   f.TooLarge,
		})
	}
	etag, err := playlistFilesETag(resp)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't read its playlist files", err)
		return
	}
	if ifNoneMatchHits(strings.Join(r.Header.Values("If-None-Match"), ","), etag, true) {
		writeNotModified(w, etag)
		return
	}
	setETag(w, etag)
	writeJSONGzip(w, r, http.StatusOK, resp)
}
