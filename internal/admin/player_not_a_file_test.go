//go:build unix

package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// TestPlayerByteRoutesRefuseWhatIsNotAFile: the web player's audio and
// download routes refuse a named pipe, a link to one, a socket and a link
// to a character device, each named like a track in the library root, at
// once, with 400 bad_path; a rendition whose sidecar is a named pipe gets
// 410 variant_missing_on_disk, "was here, fall back to the source". Every
// request ends the playback session it began, and the track beside them
// still plays.
//
// Until 2026-09-28 the route checked only IsDir before os.Open, so a FIFO
// named like a track held the request, and the playback session that keeps
// the updater from swapping the binary, until something wrote to it.
func TestPlayerByteRoutesRefuseWhatIsNotAFile(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	var begun, active atomic.Int64
	srv.deps.BeginPlaybackSession = func() func() {
		begun.Add(1)
		active.Add(1)
		return func() { active.Add(-1) }
	}
	st := srv.deps.Manifest
	const rel = "Rock/Alpha/01.flac"
	_, info := seedPlayerTrack(t, cfg, st, rel, "One")
	const variantID = "optimized-v2-44100-16"
	sidecar := transcode.VariantSidecarPath(cfg.Upscale.EffectiveVariantsDir(cfg.DataDir), rel, variantID)
	if err := os.MkdirAll(filepath.Dir(sidecar), 0o755); err != nil {
		t.Fatal(err)
	}
	fsutiltest.MakeFIFO(t, sidecar)
	if err := st.UpsertVariant(t.Context(), manifest.VariantRow{
		SourcePath: rel, VariantID: variantID, SidecarPath: sidecar,
		Format: "flac", SampleRate: 44100, BitsPerSample: 16, SizeBytes: 7,
		SourceMTimeNS: info.ModTime().UnixNano(), SourceSize: info.Size(),
	}); err != nil {
		t.Fatal(err)
	}
	kinds, pipe := fsutiltest.PlantNotAFiles(t, filepath.Join(cfg.LibraryRoots[0], "Rock", "Alpha"))
	h := srv.Handler()
	requests := 0
	serve := func(method, target string) *httptest.ResponseRecorder {
		requests++
		req := httptest.NewRequest(method, target, nil)
		req.RemoteAddr = "127.0.0.1:1"
		return fsutiltest.ServeWithin(t, h, req, pipe, sidecar)
	}
	refused := func(label string, rec *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != status || body["error"] != code {
			t.Errorf("%s: status %d (%s), want %d %s", label, rec.Code,
				strings.TrimSpace(rec.Body.String()), status, code)
		}
	}

	for name, kind := range kinds {
		for _, route := range []string{"/api/player/audio", "/api/player/download"} {
			rec := serve(http.MethodGet, route+"?path=Rock/Alpha/"+name)
			refused(route+" of a "+kind, rec, http.StatusBadRequest, "bad_path")
		}
	}
	rec := serve(http.MethodGet, "/api/player/audio?path="+rel+"&variant="+variantID)
	refused("a rendition whose sidecar is a named pipe", rec, http.StatusGone, "variant_missing_on_disk")

	rec = serve(http.MethodGet, "/api/player/audio?path="+rel)
	if rec.Code != http.StatusOK || rec.Body.String() != "source" {
		t.Errorf("the track beside them: status %d, %q; want 200 with its bytes", rec.Code, rec.Body.String())
	}
	if b, a := begun.Load(), active.Load(); b != int64(requests) || a != 0 {
		t.Errorf("playback sessions: %d begun, %d still open; want %d begun and none left", b, a, requests)
	}
}
