package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// decodeErrorCode reads the stable machine-readable `error` field off a
// JSON error body. The status alone doesn't discriminate the two 404s
// (`not_found` vs the terminal `no_image`), and that distinction is the
// whole contract under test.
func decodeErrorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var e ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return e.Error
}

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
// returns false so the 404 branch stays exercised. `pending` mirrors
// the enrichment-pending split: known + pending → 202, known +
// !pending → terminal 404 `no_image`.
// `knownErr` / `pendingErr` populate the database-fault arm of the
// respective probe pair — the arm that must classify as "pending", not
// as "unknown"/"complete".
type fakeMBIDProbe struct {
	known      map[string]bool
	pending    map[string]bool
	knownErr   error
	pendingErr error
}

func (f fakeMBIDProbe) HasTrackWithArtworkMBID(ctx context.Context, m string) (bool, error) {
	return f.known[m], f.knownErr
}
func (f fakeMBIDProbe) HasTrackWithArtistMBID(ctx context.Context, m string) (bool, error) {
	return f.known[m], f.knownErr
}
func (f fakeMBIDProbe) ArtworkMBIDEnrichmentPending(ctx context.Context, m string) (bool, error) {
	return f.pending[m], f.pendingErr
}
func (f fakeMBIDProbe) ArtistMBIDEnrichmentPending(ctx context.Context, m string) (bool, error) {
	return f.pending[m], f.pendingErr
}

// artworkProbeOpts is the general shape of the probe-backed artwork
// fixture: the caller picks the MBID (so the `local-<sha256>` sentinel
// branch is reachable) and every axis of the probe's answers.
type artworkProbeOpts struct {
	mbid       string // defaults to the UUID form when empty
	present    bool   // cache file on disk at the canonical 500 size
	probeKnown bool
	pending    bool
	knownErr   error
	pendingErr error
}

func artworkFixtureOpts(t *testing.T, o artworkProbeOpts) (*httptest.Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	artDir := filepath.Join(dir, "artwork")
	mbid := o.mbid
	if mbid == "" {
		mbid = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	}
	if o.present {
		os.MkdirAll(artDir, 0o755)
		os.WriteFile(filepath.Join(artDir, mbid+"-500.jpg"), []byte{0xFF, 0xD8, 0xFF, 0xE0}, 0o644)
	}
	cfg := &config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := store.Mint("probe")

	probe := fakeMBIDProbe{
		known:      map[string]bool{},
		pending:    map[string]bool{},
		knownErr:   o.knownErr,
		pendingErr: o.pendingErr,
	}
	if o.probeKnown {
		probe.known[mbid] = true
	}
	if o.pending {
		probe.pending[mbid] = true
	}

	srv := New(cfg, store, nil, "fp").
		WithArtworkDirs(fakeArtworkDirs{dir: artDir}).
		WithMBIDProbe(probe)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, mbid
}

// artworkFixtureWithProbe layers an MBIDProbe onto the base artwork
// fixture. `present` is still about whether the cache file exists on
// disk; `probeKnown` is about whether the MBIDProbe says the server
// has seen the MBID in a track; `enrichPending` is whether any track
// carrying it still awaits the enricher (the 202-vs-no_image axis).
func artworkFixtureWithProbe(t *testing.T, present, probeKnown, enrichPending bool) (*httptest.Server, string, string) {
	t.Helper()
	return artworkFixtureOpts(t, artworkProbeOpts{
		present:    present,
		probeKnown: probeKnown,
		pending:    enrichPending,
	})
}

// Cache miss + probe says "known + enrichment pending": 202 +
// Retry-After. iOS uses this to drive its backoff retry loop instead
// of giving up on first call.
func TestArtworkReturns202WhenProbeKnowsMBID(t *testing.T) {
	hs, tok, mbid := artworkFixtureWithProbe(t, false, true, true)
	resp := authedGET(t, hs.URL+"/v1/artwork/"+mbid, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Errorf("Retry-After header missing")
	}
}

// Cache miss + probe says "known, enrichment COMPLETE": terminal 404
// with the `no_image` code — the enricher took its turn and nothing
// exists upstream, so a 202 would have clients ladder-retry forever
// (the 2026-08-06 bridge.ars.md field case: 78 imageless artists ×
// a 4-5 minute iOS retry ladder per sync, for bytes that can never
// arrive).
func TestArtworkReturns404NoImageWhenEnrichmentComplete(t *testing.T) {
	hs, tok, mbid := artworkFixtureWithProbe(t, false, true, false)
	resp := authedGET(t, hs.URL+"/v1/artwork/"+mbid, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q on terminal miss; must be absent so clients don't retry", ra)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"no_image"`) {
		t.Errorf("body = %s, want error code no_image (distinct from not_found for diagnostics)", body)
	}
}

// Cache miss + probe says "unknown": 404. Preserves v1.0 behaviour
// for MBIDs nobody's ever referenced — iOS can stop asking.
func TestArtworkReturns404WhenProbeDoesNotKnow(t *testing.T) {
	hs, tok, mbid := artworkFixtureWithProbe(t, false, false, false)
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
	hs, tok, mbid := artworkFixtureWithProbe(t, true, true, true)
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
	probe := fakeMBIDProbe{
		known:   map[string]bool{mbid: true},
		pending: map[string]bool{mbid: true},
	}
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

// Cache miss + known MBID + enrichment complete on /v1/artist-image →
// terminal 404 `no_image`. The artist twin of
// TestArtworkReturns404NoImageWhenEnrichmentComplete — this is the
// exact state the 78 portrait-less bridge.ars.md artists were stuck
// in, each answering 202 forever.
func TestArtistImageReturns404NoImageWhenEnrichmentComplete(t *testing.T) {
	dir := t.TempDir()
	artDir := filepath.Join(dir, "artwork")
	mbid := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	cfg := &config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := store.Mint("probe")
	probe := fakeMBIDProbe{known: map[string]bool{mbid: true}} // nil pending map — enrichment done
	srv := New(cfg, store, nil, "fp").
		WithArtworkDirs(fakeArtworkDirs{dir: artDir}).
		WithMBIDProbe(probe)
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	resp := authedGET(t, hs.URL+"/v1/artist-image/"+mbid, raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q on terminal miss; must be absent", ra)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"no_image"`) {
		t.Errorf("body = %s, want error code no_image", body)
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

// --- scanner-owned `local-` sentinels are never "no image" ---

// A `local-<sha256>` artworkMBID names bytes the SCANNER extracted from
// an embedded APIC frame or a folder-level cover.jpg. The enricher
// stamps `enriched_at` for those rows without ever fetching anything,
// so "enrichment complete" carries no information about them — yet it
// was the discriminator, so a missing cache file answered a TERMINAL
// 404 `no_image` for a cover the bridge is guaranteed to restore
// (`Scanner.needsLocalArtworkRecovery` re-extracts it on the next
// scan). Reachable through a first-class workflow: `internal/backup`
// snapshots the DB, tokens, certs and bridge.yaml but NOT
// <dataDir>/artwork/, so every `bridge restore` lands in exactly this
// state — and on a well-tagged library nearly every cover is a
// `local-` sentinel.
func TestArtworkLocalSentinelStaysPendingWhenEnrichmentComplete(t *testing.T) {
	const hash = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	hs, tok, mbid := artworkFixtureOpts(t, artworkProbeOpts{
		mbid:       "local-" + hash,
		present:    false, // artwork cache wiped, e.g. post-restore
		probeKnown: true,
		pending:    false, // enricher took its (no-op) turn on this row
	})
	resp := authedGET(t, hs.URL+"/v1/artwork/"+mbid, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (scanner re-extracts local- art; "+
			"a terminal no_image would strand a cover that IS coming back)",
			resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Errorf("Retry-After header missing on local- pending response")
	}
}

// The UUID form is unaffected — enrichment IS the right discriminator
// for MBIDs whose bytes the enricher owns. Guards against "fixing" the
// above by blanket-pending every miss, which would restore the futile-
// ladder bug the three-way split exists to kill.
func TestArtworkUUIDStillTerminalWhenEnrichmentComplete(t *testing.T) {
	hs, tok, mbid := artworkFixtureOpts(t, artworkProbeOpts{
		present: false, probeKnown: true, pending: false,
	})
	resp := authedGET(t, hs.URL+"/v1/artwork/"+mbid, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if code := decodeErrorCode(t, resp); code != "no_image" {
		t.Errorf("error = %q, want no_image", code)
	}
}

// --- probe faults classify as pending, never as terminal ---

// The pending probe documents itself as failing OPEN on a database
// fault ("a wrongly pending answer costs one bounded retry, a wrongly
// complete answer would terminal-404 an image that was about to
// land") — but the known-probe checked FIRST in the same chain folded
// its error into `false`, so a DB fault produced a terminal `404
// not_found` and the fail-open below it never ran. Both arms now
// answer 202.
func TestArtworkProbeFaultAnswersPending(t *testing.T) {
	dbDown := errors.New("database is locked")
	tests := []struct {
		name string
		opts artworkProbeOpts
	}{
		{
			// Pre-fix: Has* swallowed the error → not known → 404 not_found.
			name: "known-probe fault",
			opts: artworkProbeOpts{probeKnown: true, knownErr: dbDown},
		},
		{
			// Pre-existing fail-open, re-pinned at its new home in the api.
			name: "pending-probe fault",
			opts: artworkProbeOpts{probeKnown: true, pendingErr: dbDown},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hs, tok, mbid := artworkFixtureOpts(t, tc.opts)
			resp := authedGET(t, hs.URL+"/v1/artwork/"+mbid, tok)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 — an unanswerable probe is "+
					"not an answer, and a terminal 404 is unrecoverable",
					resp.StatusCode)
			}
		})
	}
}

// Same contract on the artist-image twin, which shares the classifier.
func TestArtistImageProbeFaultAnswersPending(t *testing.T) {
	dir := t.TempDir()
	artDir := filepath.Join(dir, "artwork")
	mbid := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	cfg := &config.Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := store.Mint("probe")
	probe := fakeMBIDProbe{
		known:    map[string]bool{mbid: true},
		pending:  map[string]bool{},
		knownErr: errors.New("database is locked"),
	}
	srv := New(cfg, store, nil, "fp").
		WithArtworkDirs(fakeArtworkDirs{dir: artDir}).
		WithMBIDProbe(probe)
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	resp := authedGET(t, hs.URL+"/v1/artist-image/"+mbid, raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202 on a probe fault", resp.StatusCode)
	}
}

// --- off-canonical `size` is a size-specific miss, not "no image" ---

// `enrich.ParseSize` admits 250 / 500 / 1200, but every writer in the
// bridge caches at 500 only — so `?size=1200` builds a path that never
// exists even for a release whose cover IS cached, and answering
// `no_image` ("enrichment complete; no image exists upstream —
// terminal") told the client to stop asking for an image sitting on
// disk one size away. `not_found` keeps the answer inside the
// documented vocabulary and scoped to what it means: not cached under
// this key.
func TestArtworkOffCanonicalSizeMissIsNotFound(t *testing.T) {
	tests := []struct {
		name     string
		size     string
		present  bool // 500 variant on disk
		wantCode string
	}{
		{
			name: "1200 with 500 cached", size: "1200", present: true,
			wantCode: "not_found",
		},
		{
			name: "250 with 500 cached", size: "250", present: true,
			wantCode: "not_found",
		},
		{
			// Nothing cached at any size + enrichment complete: the
			// terminal answer is still correct and must not be widened
			// away by the size branch.
			name: "1200 with nothing cached", size: "1200", present: false,
			wantCode: "no_image",
		},
		{
			// The canonical size keeps the full three-way split.
			name: "500 with nothing cached", size: "500", present: false,
			wantCode: "no_image",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hs, tok, mbid := artworkFixtureOpts(t, artworkProbeOpts{
				present: tc.present, probeKnown: true,
			})
			resp := authedGET(t, hs.URL+"/v1/artwork/"+mbid+"?size="+tc.size, tok)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", resp.StatusCode)
			}
			if code := decodeErrorCode(t, resp); code != tc.wantCode {
				t.Errorf("error = %q, want %q", code, tc.wantCode)
			}
		})
	}
}
