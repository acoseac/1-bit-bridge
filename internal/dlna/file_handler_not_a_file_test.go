//go:build unix

package dlna

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

// Test_FileHandler_RefusesWhatIsNotAFile: a track whose manifest path now
// holds a named pipe, a link to one, a socket or a link to a character
// device answers 404 at once, as a source that cannot be opened always has,
// and a rendition whose sidecar is a named pipe answers 410; the track
// beside them still plays.
//
// The route serves the manifest's path, and a manifest row outlives the
// file it was minted for until the scan that reaps it, so a track replaced
// by a named pipe is served until then. Until 2026-09-28 it opened that path
// with os.Open, which waits for a writer, and a renderer's request, like
// the HTTP handler serving it, waited with it; a link to a character device
// was served as an empty 200.
func Test_FileHandler_RefusesWhatIsNotAFile(t *testing.T) {
	dir := t.TempDir()
	kinds, pipe := fsutiltest.PlantNotAFiles(t, dir)
	sidecar := filepath.Join(t.TempDir(), "variant.flac")
	fsutiltest.MakeFIFO(t, sidecar)
	src := createTempFile(t, ".flac", "SOURCE FLAC")
	tracks := []TrackInfo{{
		TrackID: "trk", AbsolutePath: src, FileExtension: ".flac", Size: 11,
		Variants: []VariantInfo{{
			VariantID: "upscaled-v2-176400-24", AbsolutePath: sidecar, FileExtension: ".flac",
		}},
	}}
	for name := range kinds {
		tracks = append(tracks, TrackInfo{
			TrackID: name, AbsolutePath: filepath.Join(dir, name), FileExtension: ".flac",
		})
	}
	h := FileHandler(newTestLib(tracks...), nil, nil, nil)
	serve := func(method, target string) *httptest.ResponseRecorder {
		return fsutiltest.ServeWithin(t, h, httptest.NewRequest(method, target, nil), pipe, sidecar)
	}

	for name, kind := range kinds {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			if rec := serve(method, "/dlna/file/"+name); rec.Code != http.StatusNotFound {
				t.Errorf("%s of a %s: status %d, want 404", method, kind, rec.Code)
			}
		}
	}
	if rec := serve(http.MethodGet, "/dlna/file/trk/variant-upscaled-v2-176400-24.flac"); rec.Code != http.StatusGone {
		t.Errorf("a rendition whose sidecar is a named pipe: status %d, want 410", rec.Code)
	}
	if rec := serve(http.MethodGet, "/dlna/file/trk"); rec.Code != http.StatusOK || rec.Body.String() != "SOURCE FLAC" {
		t.Errorf("the track beside them: status %d, %q; want 200 with its bytes", rec.Code, rec.Body.String())
	}
}
