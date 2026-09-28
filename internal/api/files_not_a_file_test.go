//go:build unix

package api

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
)

// countingSessions is a SessionTracker that counts the byte-route requests
// begun and the ones still in flight, which is the count the updater reads
// before it swaps the binary.
type countingSessions struct{ begun, active atomic.Int64 }

func (c *countingSessions) Begin() { c.begun.Add(1); c.active.Add(1) }
func (c *countingSessions) End()   { c.active.Add(-1) }

// requireRefusal asserts that rec answered status with the error code, and
// with a message naming want when one is given. A HEAD answer is judged by
// its status alone, since it has no body.
func requireRefusal(t *testing.T, label string, rec *httptest.ResponseRecorder, method string, status int, code, want string) {
	t.Helper()
	if rec.Code != status {
		t.Errorf("%s: status %d (%s), want %d", label, rec.Code, strings.TrimSpace(rec.Body.String()), status)
		return
	}
	if method == http.MethodHead {
		return
	}
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Errorf("%s: body %q: %v", label, rec.Body.String(), err)
		return
	}
	if body.Error != code || !strings.Contains(body.Message, want) {
		t.Errorf("%s: %+v, want error %q with a message naming %q", label, body, code, want)
	}
}

// TestByteRoutesRefuseWhatIsNotAFile: /v1/download and /v1/read refuse a
// named pipe, a link to one, a socket and a link to a character device,
// each named like a track in a library root, at once, with 400 bad_request
// naming the kind; a rendition whose sidecar is a named pipe gets 410
// variant_missing_on_disk, so the client falls back to the source. Every
// request ends the updater session it began, and the track beside them is
// still served.
//
// Until 2026-09-28 both routes opened such a path with os.Open, and opening
// a named pipe waits for a writer: measured over the real api.Server, a
// client gave up at 2 s while both handlers, and the two updater sessions
// they had begun, stayed until a writer came, 7 s in, and a pinned session
// keeps auto-install deferring on every poll. A link to a character device
// was served as an empty 200, a socket as a 500.
func TestByteRoutesRefuseWhatIsNotAFile(t *testing.T) {
	srv, tok, root := fileFixtureServer(t)
	sessions := &countingSessions{}
	srv.WithSessionTracker(sessions)
	source := filepath.Join(root, "Artist", "Album", "01 Track.flac")
	srcInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(t.TempDir(), "variant.flac")
	fsutiltest.MakeFIFO(t, sidecar)
	const variantID = "upscaled-v1-176400-24"
	srv.WithUpscale(func() bool { return true }, &fakeVariantStore{
		wantSourcePath: "Artist/Album/01 Track.flac",
		wantVariantID:  variantID,
		rec: &VariantRecord{
			SidecarPath:   sidecar,
			SourceMTimeNS: srcInfo.ModTime().UnixNano(),
			SourceSize:    srcInfo.Size(),
		},
	})
	kinds, pipe := fsutiltest.PlantNotAFiles(t, filepath.Join(root, "Artist", "Album"))
	h := srv.Handler()
	requests := 0
	serve := func(method, target, rng string) *httptest.ResponseRecorder {
		requests++
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		return fsutiltest.ServeWithin(t, h, req, pipe, sidecar)
	}

	for name, kind := range kinds {
		rel := "Artist/Album/" + name
		for _, rt := range []struct{ method, route, rng string }{
			{http.MethodGet, "/v1/download", ""},
			{http.MethodHead, "/v1/download", ""},
			{http.MethodGet, "/v1/read", "bytes=0-1023"},
		} {
			rec := serve(rt.method, rt.route+"?path="+rel, rt.rng)
			requireRefusal(t, rt.method+" "+rt.route+" "+rel, rec, rt.method,
				http.StatusBadRequest, "bad_request", kind)
		}
	}
	rec := serve(http.MethodGet, "/v1/download?path=Artist/Album/01%20Track.flac&variant="+variantID, "")
	requireRefusal(t, "a rendition whose sidecar is a named pipe", rec, http.MethodGet,
		http.StatusGone, "variant_missing_on_disk", "")

	rec = serve(http.MethodGet, "/v1/download?path=Artist/Album/01%20Track.flac", "")
	if rec.Code != http.StatusOK || rec.Body.Len() != int(srcInfo.Size()) {
		t.Errorf("the track beside them: status %d, %d bytes; want 200 with its %d bytes",
			rec.Code, rec.Body.Len(), srcInfo.Size())
	}
	if begun, active := sessions.begun.Load(), sessions.active.Load(); begun != int64(requests) || active != 0 {
		t.Errorf("updater sessions: %d begun, %d still in flight; want %d begun and none left", begun, active, requests)
	}
}
