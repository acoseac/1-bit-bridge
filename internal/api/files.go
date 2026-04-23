package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
)

// Entry is the JSON shape of a single directory entry returned by /v1/list.
// The path field is library-root-relative, not the server's absolute path.
type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// StatResponse is the JSON shape returned by /v1/stat.
type StatResponse struct {
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// list handles GET /v1/list?path=<rel>. Returns the entries of the resolved
// directory. Entries are sorted by name (case-insensitive) for stable client
// rendering.
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	clientPath := r.URL.Query().Get("path")
	abs, info, err := s.resolver.ResolveChecked(clientPath)
	if ok := writeResolveError(w, err); ok {
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, "bad_request", "path is a file, not a directory")
		return
	}

	dir, err := os.Open(abs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer dir.Close()
	raw, err := dir.Readdir(-1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	entries := make([]Entry, 0, len(raw))
	for _, ri := range raw {
		// Skip hidden files — macOS/Windows both drop noise into every
		// directory, none of it is music.
		if len(ri.Name()) > 0 && ri.Name()[0] == '.' {
			continue
		}
		entries = append(entries, Entry{
			Name:    ri.Name(),
			Path:    childPath(clientPath, ri.Name()),
			IsDir:   ri.IsDir(),
			Size:    ri.Size(),
			ModTime: ri.ModTime().UTC(),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return lessCaseFold(entries[i].Name, entries[j].Name)
	})
	writeJSON(w, http.StatusOK, entries)
}

// stat handles GET /v1/stat?path=<rel>. Returns a single-entry StatResponse.
// Works on both files and directories.
func (s *Server) stat(w http.ResponseWriter, r *http.Request) {
	clientPath := r.URL.Query().Get("path")
	_, info, err := s.resolver.ResolveChecked(clientPath)
	if ok := writeResolveError(w, err); ok {
		return
	}
	writeJSON(w, http.StatusOK, StatResponse{
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		ModTime: info.ModTime().UTC(),
	})
}

// read handles GET /v1/read?path=<rel>. Range header is REQUIRED per
// PROTOCOL.md — unranged reads are rejected with 416. This endpoint is
// intended for tag-header windows (64–128 KB) and similar sub-file queries
// from the iOS scanner fast-path fallback. Whole-file reads should use
// /v1/download.
func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Range") == "" {
		writeError(w, http.StatusRequestedRangeNotSatisfiable,
			"range_required",
			"use /v1/download for unranged reads; /v1/read requires a Range header")
		return
	}
	s.serveFile(w, r, true)
}

// download handles GET /v1/download?path=<rel>. Supports Range; returns the
// whole file when unranged.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, false)
}

// serveFile is the shared file-body path for /v1/read and /v1/download. The
// rangeRequired flag is decorative here — /v1/read pre-checks and bails
// before calling in — but kept for documentation.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, rangeRequired bool) {
	clientPath := r.URL.Query().Get("path")
	abs, info, err := s.resolver.ResolveChecked(clientPath)
	if ok := writeResolveError(w, err); ok {
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "bad_request", "path is a directory")
		return
	}

	f, err := os.Open(abs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer f.Close()

	// Pre-set content-type: we treat every library file as opaque bytes —
	// the iOS side already knows what format it asked for (the manifest
	// told it). Standard binary content type keeps intermediaries from
	// transcoding anything.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")

	// http.ServeContent handles Range, If-Modified-Since, and the 206
	// partial-content bookkeeping for us. It also skips the body on HEAD
	// requests automatically.
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
	_ = rangeRequired // silence unused-arg linter
}

// childPath returns the library-relative path of a child given its parent's
// library-relative path and its own name. Uses forward slashes regardless
// of the server's OS.
func childPath(parent, name string) string {
	if parent == "" || parent == "/" {
		return name
	}
	// Use filepath.ToSlash on the join so Windows backslashes don't leak.
	return filepath.ToSlash(filepath.Clean(parent)) + "/" + name
}

// writeResolveError maps an fs resolver error to the right JSON error
// response. Returns true if an error was written (caller should bail).
func writeResolveError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, bridgefs.ErrBadPath):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, bridgefs.ErrUnknownRoot):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, bridgefs.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
	return true
}

// lessCaseFold compares two strings case-insensitively. Matches the
// alphabetical-but-CI ordering iOS users expect in a file browser.
func lessCaseFold(a, b string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		ca, cb := foldByte(a[i]), foldByte(b[i])
		if ca != cb {
			return ca < cb
		}
	}
	return len(a) < len(b)
}

func foldByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// Copy is exposed for test shims; wraps io.Copy so tests can swap it out.
// (Currently unused — kept as a seam for benchmarks.)
var Copy = io.Copy
