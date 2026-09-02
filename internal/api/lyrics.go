package api

import (
	"context"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// LyricsStore answers GET /v1/lyrics — wired by WithLyrics.
type LyricsStore interface {
	LookupLyrics(ctx context.Context, sourcePath string) (*LyricsRecord, error)
}

// LyricsRecord is one track's resolved lyrics document plus the provenance
// the staleness check needs.
type LyricsRecord struct {
	SourcePath    string
	Format        string
	Synced        bool
	Body          string
	Language      string
	Source        string
	SidecarName   string
	Tag           string
	SourceMTimeNS int64
	SourceSize    int64
}

// lyricsDocument is the wire body — the shape the iOS `BridgeLyricsPayload`
// decodes and re-parses with the same parsers it uses for local files.
type lyricsDocument struct {
	Format   string `json:"format"`
	Synced   bool   `json:"synced"`
	Body     string `json:"body"`
	Language string `json:"language,omitempty"`
}

// lyricsSourceInfo is the stat of the LYRICS SOURCE: the sidecar file when
// the row came from one (an edited .lrc under an unchanged FLAC must read
// stale), else the audio file. The sidecar is resolved through the SAME
// traversal-checked resolver the audio path went through — a bare file
// name beside the track, never a stored path joined blindly.
func (s *Server) lyricsSourceInfo(rec *LyricsRecord, clientPath string, audio os.FileInfo) (os.FileInfo, bool) {
	if !strings.HasPrefix(rec.Source, "sidecar") || rec.SidecarName == "" {
		return audio, true
	}
	name := rec.SidecarName
	if name != filepath.Base(name) || name == "." || name == ".." {
		return nil, false
	}
	_, info, err := s.resolver.ResolveChecked(path.Join(path.Dir(clientPath), name))
	if err != nil || info.IsDir() {
		return nil, false
	}
	return info, true
}

// lyricsSourceDrifted mirrors analysisSourceDrifted against the lyrics
// source's stat.
func lyricsSourceDrifted(rec *LyricsRecord, source os.FileInfo) bool {
	delta := rec.SourceMTimeNS - source.ModTime().UnixNano()
	if delta < 0 {
		delta = -delta
	}
	return delta > analysisSourceMTimeToleranceNS || rec.SourceSize != source.Size()
}

// etagMatches implements If-None-Match's weak comparison (RFC 9110 §13.1.2):
// `*` matches any current representation and a `W/` prefix is ignored.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		c := strings.TrimSpace(candidate)
		if c == "*" || strings.TrimPrefix(c, "W/") == etag {
			return true
		}
	}
	return false
}

// GET /v1/lyrics?path=<rel> — the track's lyrics document, or:
//   - 404 lyrics_not_found: feature not wired, or no row for the track;
//   - 410 lyrics_stale: the lyrics source drifted since extraction;
//   - 304: the client's If-None-Match matches the row's content tag.
func (s *Server) lyrics(w http.ResponseWriter, r *http.Request) {
	if s.lyricsStore == nil {
		writeError(w, http.StatusNotFound, "lyrics_not_found", "lyrics are not enabled on this bridge")
		return
	}
	clientPath := safeQuery(r).Get("path")
	if clientPath == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing path parameter")
		return
	}
	_, info, err := s.resolver.ResolveChecked(clientPath)
	if ok := writeResolveError(w, r, err); ok {
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "bad_request", "path is a directory")
		return
	}
	rec, err := s.lyricsStore.LookupLyrics(r.Context(), clientPath)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't look up these lyrics", err)
		return
	}
	if rec == nil || rec.Body == "" {
		writeError(w, http.StatusNotFound, "lyrics_not_found", "no lyrics for this track")
		return
	}
	source, ok := s.lyricsSourceInfo(rec, clientPath, info)
	if !ok || lyricsSourceDrifted(rec, source) {
		writeError(w, http.StatusGone, "lyrics_stale", "lyrics are out of date relative to their source")
		return
	}
	etag := `"` + rec.Tag + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, lyricsDocument{
		Format: rec.Format, Synced: rec.Synced, Body: rec.Body, Language: rec.Language,
	})
}
