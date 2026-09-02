package api

import (
	"context"
	"net/http"
	"os"
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

// lyricsSourceDrifted mirrors analysisSourceDrifted, but against the
// LYRICS SOURCE: the sidecar file when the row came from one (an edited
// .lrc under an unchanged FLAC must read stale), else the audio file.
func lyricsSourceDrifted(rec *LyricsRecord, audioAbs string, audio os.FileInfo) bool {
	mtime, size := audio.ModTime().UnixNano(), audio.Size()
	if strings.HasPrefix(rec.Source, "sidecar") && rec.SidecarName != "" {
		info, err := os.Stat(filepath.Join(filepath.Dir(audioAbs), rec.SidecarName))
		if err != nil {
			return true
		}
		mtime, size = info.ModTime().UnixNano(), info.Size()
	}
	delta := rec.SourceMTimeNS - mtime
	if delta < 0 {
		delta = -delta
	}
	return delta > analysisSourceMTimeToleranceNS || rec.SourceSize != size
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
	abs, info, err := s.resolver.ResolveChecked(clientPath)
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
	if lyricsSourceDrifted(rec, abs, info) {
		writeError(w, http.StatusGone, "lyrics_stale", "lyrics are out of date relative to their source")
		return
	}
	etag := `"` + rec.Tag + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if match := r.Header.Get("If-None-Match"); match != "" {
		for _, candidate := range strings.Split(match, ",") {
			if strings.TrimSpace(candidate) == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, lyricsDocument{
		Format: rec.Format, Synced: rec.Synced, Body: rec.Body, Language: rec.Language,
	})
}
