package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// ServeArtwork is the read path the DLNA listener mounts as
// `/dlna/artwork/{key}`, and the contract it inherits is "the SAME bytes,
// headers and miss shapes as `GET /v1/artwork/{key}`". This test drives the
// v1 route through the real router and ServeArtwork directly with the same
// key, and compares status, body and the headers that carry the contract —
// on a cache hit, on each of the three miss shapes, on a bad key, and on
// the alias form. If someone re-inlines the v1 handler or forks a branch,
// the two answers stop matching here.
func TestServeArtworkAnswersExactlyLikeTheV1Route(t *testing.T) {
	const (
		mbid    = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		known   = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" // known, enrichment pending → 202
		noImage = "cccccccc-cccc-4ccc-8ccc-cccccccccccc" // known, enrichment done → 404 no_image
		unknown = "dddddddd-dddd-4ddd-8ddd-dddddddddddd" // nobody references it → 404 not_found
		alias   = "0123456789abcdef"                     // 16-hex artworkVersion → resolves to mbid
	)
	dir := t.TempDir()
	artDir := filepath.Join(dir, "artwork")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, mbid+"-500.jpg"), []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", LibraryName: "T"}
	store, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := store.Mint("probe")
	if err != nil {
		t.Fatal(err)
	}
	probe := fakeMBIDProbe{
		known:          map[string]bool{known: true, noImage: true},
		pending:        map[string]bool{known: true},
		versionAliases: map[string]string{alias: mbid},
	}
	srv := New(cfg, store, nil, "fp").
		WithArtworkDirs(fakeArtworkDirs{dir: artDir}).
		WithMBIDProbe(probe)
	router := srv.Handler()

	contractHeaders := []string{"Content-Type", "Cache-Control", "ETag", "Retry-After"}
	for _, key := range []string{mbid, known, noImage, unknown, alias, "not-a-key", mbid + "?size=250"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			viaRoute := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/v1/artwork/"+key, nil)
			req.Header.Set("Authorization", "Bearer "+raw)
			router.ServeHTTP(viaRoute, req)

			direct := httptest.NewRecorder()
			dreq := httptest.NewRequest(method, "/anything/"+key, nil)
			// The DLNA wrapper hands over the bare key; a query string on
			// the request still selects the size on both paths.
			bare := key
			if i := strings.IndexByte(key, '?'); i >= 0 {
				bare = key[:i]
			}
			srv.ServeArtwork(direct, dreq, bare)

			if viaRoute.Code != direct.Code {
				t.Errorf("%s %s: status via route %d, direct %d", method, key, viaRoute.Code, direct.Code)
			}
			rb, _ := io.ReadAll(viaRoute.Body)
			db, _ := io.ReadAll(direct.Body)
			if string(rb) != string(db) {
				t.Errorf("%s %s: body via route %q, direct %q", method, key, rb, db)
			}
			for _, h := range contractHeaders {
				if a, b := viaRoute.Header().Get(h), direct.Header().Get(h); a != b {
					t.Errorf("%s %s: header %s via route %q, direct %q", method, key, h, a, b)
				}
			}
		}
	}
	// And the hit really is a hit — the equality above must not be two
	// matching failures.
	rec := httptest.NewRecorder()
	srv.ServeArtwork(rec, httptest.NewRequest(http.MethodGet, "/x/"+mbid, nil), mbid)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" || rec.Body.Len() != 7 {
		t.Fatalf("direct hit: status %d ct %q len %d", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
	rec = httptest.NewRecorder()
	srv.ServeArtwork(rec, httptest.NewRequest(http.MethodGet, "/x/"+known, nil), known)
	if rec.Code != http.StatusAccepted || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("direct pending miss: status %d Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}
