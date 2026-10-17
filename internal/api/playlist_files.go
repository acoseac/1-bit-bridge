package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// PlaylistFileStore is the backing store for GET /v1/playlist-files
// (backlog B431): the `.m3u` / `.m3u8` files the scanner read from the
// library, with their entries resolved against the tracks it holds now.
// *manifest.Store satisfies it. Read-only: the scanner writes the index,
// and nothing writes a playlist file.
type PlaylistFileStore interface {
	PlaylistFiles(ctx context.Context) (manifest.PlaylistFileFeed, error)
}

// WithPlaylistFileStore wires the playlist-file feed and advertises the
// "playlistFiles" health-feature flag. Returns the receiver.
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

// playlistFilesResponse is the GET /v1/playlist-files document. Its JSON
// is what the ETag hashes (playlistFilesETag). Complete is always sent:
// false is the answer a client must not reap on.
type playlistFilesResponse struct {
	Complete  bool              `json:"complete"`
	Playlists []playlistFileDTO `json:"playlists"`
}

// nonNilStrings is s, or an empty list for nil, so the wire never carries
// null for a list.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// playlistFiles handles GET /v1/playlist-files: every playlist file the
// index holds, in manifest-path order, and whether the index is complete (a
// full scan has brought it up to date since it was created or emptied).
// Bearer-authenticated, user-wide (no X-Device-Token), read-only. The answer
// carries a strong ETag over its document, and an If-None-Match that matches
// it is a 304 with no body, so a client that polls after each sync pays a
// hash, not a transfer.
//
// Errors go through the shared writers with fixed messages (B425): a store
// failure is a 500 `internal` logged at Error, and a client that went away
// mid-read is the Info line and 499 writeErrorLog records (B364).
func (s *Server) playlistFiles(w http.ResponseWriter, r *http.Request) {
	if s.playlistFileStore == nil {
		writeError(w, http.StatusNotFound, "playlist_files_not_supported",
			"this bridge does not index playlist files")
		return
	}
	feed, err := s.playlistFileStore.PlaylistFiles(r.Context())
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't read its playlist files", err)
		return
	}
	resp := playlistFilesResponse{Complete: feed.Complete, Playlists: make([]playlistFileDTO, 0, len(feed.Files))}
	for _, f := range feed.Files {
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
