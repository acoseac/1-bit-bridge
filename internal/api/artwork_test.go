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
	// 64-char lowercase hex, used as the "valid" baseline that the
	// negative variants are derived from.
	const validHex64 = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	for _, bad := range []string{
		"not-a-uuid",
		"../../etc/passwd",
		"12345678-1234-1234-1234",
		// local- branch negatives: the regex's local- arm requires
		// exactly 64 lowercase hex chars, nothing more, nothing less.
		"local-",                         // empty hash
		"local-" + validHex64[:63],       // 63 chars (one short)
		"local-" + validHex64 + "0",      // 65 chars (one too many)
		"local-" + validHex64[:63] + "Z", // non-hex char in hash
		"LOCAL-" + validHex64,            // uppercase prefix is rejected
		"local-" + "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789AB"[:64], // uppercase hex
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

// TestArtworkAcceptsLocalHashMBID verifies the relaxed regex accepts
// the local-<sha256> sentinel and serves the corresponding cache file.
// Pre-stages <artDir>/local-<hash>-500.jpg exactly the way the scanner
// would have written it; asserts 200 + bytes round-trip + image/jpeg.
func TestArtworkAcceptsLocalHashMBID(t *testing.T) {
	dir := t.TempDir()
	artDir := filepath.Join(dir, "artwork")
	const hash = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	mbid := "local-" + hash
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}
	if err := os.WriteFile(filepath.Join(artDir, mbid+"-500.jpg"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := store.Mint("probe")
	srv := New(cfg, store, nil, "fp").WithArtworkDirs(fakeArtworkDirs{dir: artDir})
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	resp := authedGET(t, hs.URL+"/v1/artwork/"+mbid, raw)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", ct)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(want) {
		t.Errorf("body bytes mismatch: got %x, want %x", got, want)
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

// --- 202 + Retry-After semantics (v1.1 §7) ---

// fakeMBIDProbe stubs the optional MBIDProbe interface so the handler
// can pretend a given MBID is known (or unknown) without wiring a
// real manifest store. `known` is a closed set — anything not in it
// returns false so the 404 branch stays exercised.
type fakeMBIDProbe struct{ known map[string]bool }

func (f fakeMBIDProbe) HasTrackWithArtworkMBID(m string) bool { return f.known[m] }
func (f fakeMBIDProbe) HasTrackWithArtistMBID(m string) bool  { return f.known[m] }

// artworkFixtureWithProbe layers an MBIDProbe onto the base artwork
// fixture. `present` is still about whether the cache file exists on
// disk; `probeKnown` is about whether the MBIDProbe says the server
// has seen the MBID in a track.
func artworkFixtureWithProbe(t *testing.T, present, probeKnown bool) (*httptest.Server, string, string) {
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

	probe := fakeMBIDProbe{known: map[string]bool{}}
	if probeKnown {
		probe.known[mbid] = true
	}

	srv := New(cfg, store, nil, "fp").
		WithArtworkDirs(fakeArtworkDirs{dir: artDir}).
		WithMBIDProbe(probe)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, mbid
}

// Cache miss + probe says "known": 202 + Retry-After. iOS uses this
// to drive its backoff retry loop instead of giving up on first call.
func TestArtworkReturns202WhenProbeKnowsMBID(t *testing.T) {
	hs, tok, mbid := artworkFixtureWithProbe(t, false, true)
	resp := authedGET(t, hs.URL+"/v1/artwork/"+mbid, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Errorf("Retry-After header missing")
	}
}

// Cache miss + probe says "unknown": 404. Preserves v1.0 behaviour
// for MBIDs nobody's ever referenced — iOS can stop asking.
func TestArtworkReturns404WhenProbeDoesNotKnow(t *testing.T) {
	hs, tok, mbid := artworkFixtureWithProbe(t, false, false)
	resp := authedGET(t, hs.URL+"/v1/artwork/"+mbid, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// No probe wired at all → legacy 404-on-miss. Keeps tests that use
// the classic fixture passing without a probe.
func TestArtworkReturns404WhenNoProbeAttached(t *testing.T) {
	hs, tok, mbid, _ := artworkFixture(t, false)
	resp := authedGET(t, hs.URL+"/v1/artwork/"+mbid, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// Cache hit wins regardless of probe state — fast path is not touched.
func TestArtworkCacheHitIgnoresProbe(t *testing.T) {
	hs, tok, mbid := artworkFixtureWithProbe(t, true, true)
	resp := authedGET(t, hs.URL+"/v1/artwork/"+mbid, tok)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (cache hit should not be 202)", resp.StatusCode)
	}
}

// Mirror of the above for /v1/artist-image — same 202/404 contract.
func TestArtistImageReturns202WhenProbeKnows(t *testing.T) {
	dir := t.TempDir()
	artDir := filepath.Join(dir, "artwork")
	mbid := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	cfg := &config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := store.Mint("probe")
	probe := fakeMBIDProbe{known: map[string]bool{mbid: true}}
	srv := New(cfg, store, nil, "fp").
		WithArtworkDirs(fakeArtworkDirs{dir: artDir}).
		WithMBIDProbe(probe)
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	resp := authedGET(t, hs.URL+"/v1/artist-image/"+mbid, raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Errorf("Retry-After header missing on artist-image 202")
	}
}

// Cache miss + probe says "unknown" on /v1/artist-image → 404.
// Mirrors TestArtworkReturns404WhenProbeDoesNotKnow so both handlers'
// 404 branches are explicitly exercised when a probe IS attached.
func TestArtistImageReturns404WhenProbeDoesNotKnow(t *testing.T) {
	dir := t.TempDir()
	artDir := filepath.Join(dir, "artwork")
	mbid := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	cfg := &config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := store.Mint("probe")
	probe := fakeMBIDProbe{known: map[string]bool{}} // empty — nothing known
	srv := New(cfg, store, nil, "fp").
		WithArtworkDirs(fakeArtworkDirs{dir: artDir}).
		WithMBIDProbe(probe)
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	resp := authedGET(t, hs.URL+"/v1/artist-image/"+mbid, raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
