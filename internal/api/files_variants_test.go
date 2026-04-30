package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// stubVariantStore is a tiny test double that the api files_test
// fixture can wire as the VariantStore implementation. Backed by
// in-memory maps so tests can shape the lookup's response per
// case without touching the real manifest store.
//
// Records carry the source mtime/size that the api's freshness
// check compares against the real `os.FileInfo` from the
// resolver. To force the stale branch, set the record's mtime/
// size to a value that won't match what's on disk.
type stubVariantStore struct {
	records map[string]*VariantRecord // (sourcePath|variantID) → record (nil = simulate "not found")
}

func newStubVariantStore() *stubVariantStore {
	return &stubVariantStore{records: map[string]*VariantRecord{}}
}

func (s *stubVariantStore) LookupVariant(sourcePath, variantID string) (*VariantRecord, error) {
	rec, ok := s.records[sourcePath+"|"+variantID]
	if !ok {
		return nil, nil
	}
	return rec, nil
}

// fileVariantFixture extends the file fixture with a real on-disk
// sidecar file and a stub variant store wired to point at it. Mirrors
// fileFixture's shape but returns the stub so tests can mutate
// resolver behaviour.
func fileVariantFixture(t *testing.T) (*httptest.Server, string, string, *stubVariantStore, string) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(filepath.Join(root, "Artist/Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Artist/Album/01.flac"), bytes.Repeat([]byte{0xAA}, 256), 0o644); err != nil {
		t.Fatal(err)
	}
	// Sidecar lives outside the library root — the bridge's data
	// dir in production. Writes a known byte pattern so we can
	// verify the response body actually came from the sidecar
	// path rather than the source.
	sidecarPath := filepath.Join(tmp, "transcoded", "abc-upscaled-v1-176400-24.flac")
	if err := os.MkdirAll(filepath.Dir(sidecarPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath, bytes.Repeat([]byte{0xCC}, 512), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		LibraryRoots:  []string{root},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("test")

	vs := newStubVariantStore()
	srv := New(cfg, store, nil, "fp").WithUpscale(true, vs)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, root, vs, sidecarPath
}

// TestDownloadSourceServesOriginalWhenNoVariantParam — the
// pre-existing /v1/download path must still serve the source
// untouched when no `?variant=` is present. Otherwise this PR
// regresses the entire library.
func TestDownloadSourceServesOriginalWhenNoVariantParam(t *testing.T) {
	hs, tok, _, _, _ := fileVariantFixture(t)
	resp := authGet(t, hs, "/v1/download?path=Artist/Album/01.flac", tok)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	body := readAllOrFail(t, resp)
	if len(body) != 256 {
		t.Errorf("body length: got %d, want 256 (source bytes)", len(body))
	}
	if body[0] != 0xAA {
		t.Errorf("body[0]: got %#x, want 0xAA (source pattern)", body[0])
	}
}

// TestDownloadVariantServesSidecar — request with valid
// `?variant=<id>` returns the sidecar bytes, NOT the source. The
// freshness check passes because the record's mtime/size match
// the real source on disk. This is the load-bearing assertion
// for the variant-routing feature.
func TestDownloadVariantServesSidecar(t *testing.T) {
	hs, tok, root, vs, sidecar := fileVariantFixture(t)
	srcInfo := statSourceOrFail(t, root, "Artist/Album/01.flac")
	vs.records["Artist/Album/01.flac|upscaled-v1-176400-24"] = &VariantRecord{
		SidecarPath:   sidecar,
		SourceMTimeNS: srcInfo.ModTime().UnixNano(),
		SourceSize:    srcInfo.Size(),
	}

	resp := authGet(t, hs, "/v1/download?path=Artist/Album/01.flac&variant=upscaled-v1-176400-24", tok)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	body := readAllOrFail(t, resp)
	if len(body) != 512 {
		t.Errorf("body length: got %d, want 512 (sidecar bytes)", len(body))
	}
	if body[0] != 0xCC {
		t.Errorf("body[0]: got %#x, want 0xCC (sidecar pattern)", body[0])
	}
}

// TestDownloadVariantNotFoundReturns404 — unknown variantID for a
// known source path returns a clean 404 (NOT a server error)
// with the wire-stable `variant_not_found` code in the body. iOS
// keys friendlyErrorMessage off the code, not the status alone,
// so a regression to a generic `not_found` would silently break
// the picker UX (CodeRabbit second-pass on PR #108).
func TestDownloadVariantNotFoundReturns404(t *testing.T) {
	hs, tok, _, _, _ := fileVariantFixture(t)
	// vs.records is empty — every variantID misses.
	resp := authGet(t, hs, "/v1/download?path=Artist/Album/01.flac&variant=upscaled-v1-176400-24", tok)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
	assertWireErrorCode(t, resp, "variant_not_found")
}

// TestDownloadVariantStaleOnMtimeMismatchReturns410 — record's
// recorded mtime ≠ live source mtime → 410 Gone. We force the
// stale state by stamping a wrong mtime in the record (instead
// of touching the real source — that's brittle across
// filesystems with coarse mtime granularity).
//
// Asserts the wire-stable `variant_stale` code so a swap with
// `variant_missing_on_disk` doesn't pass silently (CodeRabbit
// second-pass on PR #108).
func TestDownloadVariantStaleOnMtimeMismatchReturns410(t *testing.T) {
	hs, tok, root, vs, sidecar := fileVariantFixture(t)
	srcInfo := statSourceOrFail(t, root, "Artist/Album/01.flac")
	vs.records["Artist/Album/01.flac|upscaled-v1-176400-24"] = &VariantRecord{
		SidecarPath:   sidecar,
		SourceMTimeNS: srcInfo.ModTime().UnixNano() - 1_000_000_000,
		SourceSize:    srcInfo.Size(),
	}

	resp := authGet(t, hs, "/v1/download?path=Artist/Album/01.flac&variant=upscaled-v1-176400-24", tok)
	defer resp.Body.Close()
	if resp.StatusCode != 410 {
		t.Errorf("status: got %d, want 410", resp.StatusCode)
	}
	assertWireErrorCode(t, resp, "variant_stale")
}

// TestDownloadVariantStaleOnSizeMismatchReturns410 — symmetric
// to the mtime case: a size delta means the source has been
// substantively modified (touch + edit, restore from backup).
func TestDownloadVariantStaleOnSizeMismatchReturns410(t *testing.T) {
	hs, tok, root, vs, sidecar := fileVariantFixture(t)
	srcInfo := statSourceOrFail(t, root, "Artist/Album/01.flac")
	vs.records["Artist/Album/01.flac|upscaled-v1-176400-24"] = &VariantRecord{
		SidecarPath:   sidecar,
		SourceMTimeNS: srcInfo.ModTime().UnixNano(),
		SourceSize:    srcInfo.Size() + 1,
	}

	resp := authGet(t, hs, "/v1/download?path=Artist/Album/01.flac&variant=upscaled-v1-176400-24", tok)
	defer resp.Body.Close()
	if resp.StatusCode != 410 {
		t.Errorf("status: got %d, want 410 (stale on size mismatch)", resp.StatusCode)
	}
	assertWireErrorCode(t, resp, "variant_stale")
}

// TestDownloadVariantMissingOnDiskReturns410 — record exists,
// freshness matches, but the sidecar file is gone (manual rm).
// 410 Gone with the distinct `variant_missing_on_disk` code so
// `bridge upscale --gc` operators can reconcile.
func TestDownloadVariantMissingOnDiskReturns410(t *testing.T) {
	hs, tok, root, vs, sidecar := fileVariantFixture(t)
	srcInfo := statSourceOrFail(t, root, "Artist/Album/01.flac")
	// Delete the sidecar but keep the freshness-matching record.
	if err := os.Remove(sidecar); err != nil {
		t.Fatalf("rm sidecar: %v", err)
	}
	vs.records["Artist/Album/01.flac|upscaled-v1-176400-24"] = &VariantRecord{
		SidecarPath:   sidecar,
		SourceMTimeNS: srcInfo.ModTime().UnixNano(),
		SourceSize:    srcInfo.Size(),
	}

	resp := authGet(t, hs, "/v1/download?path=Artist/Album/01.flac&variant=upscaled-v1-176400-24", tok)
	defer resp.Body.Close()
	if resp.StatusCode != 410 {
		t.Errorf("status: got %d, want 410", resp.StatusCode)
	}
	assertWireErrorCode(t, resp, "variant_missing_on_disk")
}

// TestDownloadVariantWhenFeatureDisabledReturns404 — `WithUpscale(false, nil)`
// must keep `?variant=` in 404-land. iOS sees an unfamiliar bridge as
// "no variants supported" without surprises.
func TestDownloadVariantWhenFeatureDisabledReturns404(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(filepath.Join(root, "Artist/Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Artist/Album/01.flac"), bytes.Repeat([]byte{0xAA}, 256), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		LibraryRoots:  []string{root},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("test")

	// NO WithUpscale call → variantStore stays nil.
	srv := New(cfg, store, nil, "fp")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	resp := authGet(t, hs, "/v1/download?path=Artist/Album/01.flac&variant=upscaled-v1-176400-24", raw)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status: got %d, want 404 (variant feature off)", resp.StatusCode)
	}
}

// TestHealthAdvertisesUpscaleEnabled — operators flip the flag in
// bridge.yaml and iOS picks the change up via /v1/health on the
// next reachability tick. The wire shape MUST match what iOS is
// decoding (bool field `upscaleEnabled`).
func TestHealthAdvertisesUpscaleEnabled(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp}, // any directory satisfies validation
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))

	for _, enabled := range []bool{true, false} {
		srv := New(cfg, store, nil, "fp").WithUpscale(enabled, nil)
		hs := httptest.NewServer(srv.Handler())

		resp := authGet(t, hs, "/v1/health", "")
		body := readAllOrFail(t, resp)
		resp.Body.Close()
		hs.Close()

		var got HealthResponse
		if err := jsonUnmarshalForTest(body, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.UpscaleEnabled == nil {
			t.Errorf("enabled=%v: UpscaleEnabled field missing on the wire", enabled)
			continue
		}
		if *got.UpscaleEnabled != enabled {
			t.Errorf("UpscaleEnabled: got %v, want %v", *got.UpscaleEnabled, enabled)
		}
	}
}

// readAllOrFail is a tiny io.ReadAll wrapper that fails the test
// on a read error, keeping per-case bodies focused on the
// assertion they care about.
func readAllOrFail(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

// jsonUnmarshalForTest is a thin wrapper around json.Unmarshal so
// the test bodies don't need to reference encoding/json directly
// for one call site.
func jsonUnmarshalForTest(b []byte, v any) error { return json.Unmarshal(b, v) }

// statSourceOrFail returns the os.FileInfo of a library-relative
// path under root. Lets variant tests construct VariantRecords
// whose mtime/size match (or deliberately don't match) the real
// source on disk.
func statSourceOrFail(t *testing.T, root, libraryRelative string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(libraryRelative)))
	if err != nil {
		t.Fatalf("stat source %q: %v", libraryRelative, err)
	}
	return info
}

// assertWireErrorCode decodes the response body as the standard
// `{"error": "...", "message": "..."}` shape and asserts the
// `error` field matches `wantCode`. Lets tests pin the wire-
// stable error code separately from the HTTP status — a
// regression to a generic short-code (e.g. variant_stale →
// not_found) would still pass a status-only check.
func assertWireErrorCode(t *testing.T, resp *http.Response, wantCode string) {
	t.Helper()
	var body ErrorResponse
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read error body: %v", err)
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode error body: %v (raw: %s)", err, string(data))
	}
	if body.Error != wantCode {
		t.Errorf("wire error code: got %q, want %q", body.Error, wantCode)
	}
}
