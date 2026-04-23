package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

type fakeArtworkDirs struct{ dir string }

func (f fakeArtworkDirs) ArtworkCacheDir() string { return f.dir }

// artworkFixture lays down a dummy JPEG in the expected cache location
// (<artDir>/<mbid>-<size>.jpg) and returns server + token + mbid.
func artworkFixture(t *testing.T, present bool) (*httptest.Server, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	artDir := filepath.Join(dir, "artwork")
	mbid := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if present {
		os.MkdirAll(artDir, 0o755)
		os.WriteFile(filepath.Join(artDir, mbid+"-500.jpg"), []byte{0xFF, 0xD8, 0xFF, 0xE0}, 0o644)
	}
	cfg := &config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := store.Mint("probe")

	srv := New(cfg, store, nil, "fp").WithArtworkDirs(fakeArtworkDirs{dir: artDir})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, mbid, artDir
}

func authedGET(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestArtworkReturnsCachedJPEG(t *testing.T) {
	hs, tok, mbid, _ := artworkFixture(t, true)
	resp := authedGET(t, hs.URL+"/v1/artwork/"+mbid+"?size=500", tok)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content-type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 || body[0] != 0xFF {
		t.Errorf("body looks wrong: %x", body[:min(len(body), 8)])
	}
}

func TestArtworkDefaultSizeIs500(t *testing.T) {
	hs, tok, mbid, _ := artworkFixture(t, true)
	resp := authedGET(t, hs.URL+"/v1/artwork/"+mbid, tok) // no size param
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d (default size should be 500)", resp.StatusCode)
	}
}

func TestArtworkReturns404IfNotCached(t *testing.T) {
	hs, tok, mbid, _ := artworkFixture(t, false)
	resp := authedGET(t, hs.URL+"/v1/artwork/"+mbid, tok)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestArtworkRejectsBadMBID(t *testing.T) {
	hs, tok, _, _ := artworkFixture(t, true)
	for _, bad := range []string{
		"not-a-uuid",
		"../../etc/passwd",
		"12345678-1234-1234-1234",
	} {
		resp := authedGET(t, hs.URL+"/v1/artwork/"+url.PathEscape(bad), tok)
		if resp.StatusCode != 400 && resp.StatusCode != 404 {
			// 404 is also acceptable if the router doesn't dispatch to
			// our handler for paths it considers malformed; 400 is the
			// expected path when our own validator runs.
			t.Errorf("bad mbid %q: status = %d, want 400 or 404", bad, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestArtworkRejectsUnsupportedSize(t *testing.T) {
	hs, tok, mbid, _ := artworkFixture(t, true)
	resp := authedGET(t, hs.URL+"/v1/artwork/"+mbid+"?size=42", tok)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 for bad size", resp.StatusCode)
	}
}

func TestArtworkRequiresAuth(t *testing.T) {
	hs, _, mbid, _ := artworkFixture(t, true)
	resp, err := http.Get(hs.URL + "/v1/artwork/" + mbid)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestArtwork503WhenNoProvider(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := store.Mint("probe")
	// No WithArtworkDirs.
	srv := New(cfg, store, nil, "fp")
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	resp := authedGET(t, hs.URL+"/v1/artwork/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", raw)
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---- /v1/artist-image/{mbid} ----

func artistImageFixture(t *testing.T, present bool) (*httptest.Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	artDir := filepath.Join(dir, "artwork")
	mbid := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if present {
		os.MkdirAll(artDir, 0o755)
		os.WriteFile(filepath.Join(artDir, "artist-"+mbid+".jpg"),
			[]byte{0xFF, 0xD8, 0xFF, 0xE1}, 0o644)
	}
	cfg := &config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := store.Mint("probe")
	srv := New(cfg, store, nil, "fp").WithArtworkDirs(fakeArtworkDirs{dir: artDir})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, mbid
}

func TestArtistImageReturnsCachedJPEG(t *testing.T) {
	hs, tok, mbid := artistImageFixture(t, true)
	resp := authedGET(t, hs.URL+"/v1/artist-image/"+mbid, tok)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content-type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 || body[0] != 0xFF {
		t.Errorf("body wrong: %x", body[:min(len(body), 8)])
	}
}

func TestArtistImage404IfNotCached(t *testing.T) {
	hs, tok, mbid := artistImageFixture(t, false)
	resp := authedGET(t, hs.URL+"/v1/artist-image/"+mbid, tok)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestArtistImageRejectsBadMBID(t *testing.T) {
	hs, tok, _ := artistImageFixture(t, true)
	resp := authedGET(t, hs.URL+"/v1/artist-image/not-a-uuid", tok)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestArtistImageRequiresAuth(t *testing.T) {
	hs, _, mbid := artistImageFixture(t, true)
	resp, err := http.Get(hs.URL + "/v1/artist-image/" + mbid)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}
