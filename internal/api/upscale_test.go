package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// stubEnqueuer is a test double for api.UpscaleEnqueuer. Records
// every call so tests can assert which library-relative paths
// the handler dispatched, and is configurable to simulate the
// queue-full / source-missing / ineligible / generic-error
// branches.
type stubEnqueuer struct {
	calls       []string
	resultByRel map[string]error // optional override per-rel; default nil = success
	defaultErr  error            // applied to any rel not in resultByRel
}

func newStubEnqueuer() *stubEnqueuer {
	return &stubEnqueuer{resultByRel: map[string]error{}}
}

func (s *stubEnqueuer) EnqueueOne(libraryRelativePath string) error {
	s.calls = append(s.calls, libraryRelativePath)
	if err, ok := s.resultByRel[libraryRelativePath]; ok {
		return err
	}
	return s.defaultErr
}

// upscaleFixture stands up a small library tree + the api server
// wired with a stub enqueuer. Returns the live test server, a
// valid bearer token, the library root, and the stub for per-test
// configuration.
func upscaleFixture(t *testing.T, withEnqueuer bool) (*httptest.Server, string, string, *stubEnqueuer) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(filepath.Join(root, "Artist/Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"Artist/Album/01.flac", "Artist/Album/02.flac", "Artist/Single.flac"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), bytes.Repeat([]byte{0xAA}, 64), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{
		LibraryRoots:  []string{root},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("test")

	stub := newStubEnqueuer()
	srv := New(cfg, store, nil, "fp")
	if withEnqueuer {
		srv = srv.WithUpscaleEnqueuer(stub)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, root, stub
}

func postJSON(t *testing.T, hs *httptest.Server, path, token string, body any) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, _ := http.NewRequest("POST", hs.URL+path, bytes.NewReader(buf))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func decodeUpscaleResponse(t *testing.T, resp *http.Response) UpscaleResponse {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var ur UpscaleResponse
	if err := json.Unmarshal(body, &ur); err != nil {
		t.Fatalf("decode response: %v (raw: %s)", err, string(body))
	}
	return ur
}

// TestUpscaleDisabledReturns503 — no enqueuer wired (feature off
// or sox precheck failed) → 503 `upscale_disabled`. iOS treats
// this as "no variant features", same as a pre-v1.2 bridge.
func TestUpscaleDisabledReturns503(t *testing.T) {
	hs, tok, _, _ := upscaleFixture(t, false /* no enqueuer */)
	resp := postJSON(t, hs, "/v1/upscale", tok, UpscaleRequest{Path: "Artist/Album/01.flac"})
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("status: got %d, want 503", resp.StatusCode)
	}
	assertWireErrorCode(t, resp, "upscale_disabled")
}

// TestUpscaleRequiresAuth — endpoint is bearer-token gated.
func TestUpscaleRequiresAuth(t *testing.T) {
	hs, _, _, _ := upscaleFixture(t, true)
	resp := postJSON(t, hs, "/v1/upscale", "" /* no token */, UpscaleRequest{Path: "Artist/Album/01.flac"})
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status: got %d, want 401", resp.StatusCode)
	}
}

// TestUpscaleSingleTrackEnqueues — happy path: one file → one
// EnqueueOne call → 202 with enqueued: 1.
func TestUpscaleSingleTrackEnqueues(t *testing.T) {
	hs, tok, _, stub := upscaleFixture(t, true)
	resp := postJSON(t, hs, "/v1/upscale", tok, UpscaleRequest{Path: "Artist/Album/01.flac"})
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("status: got %d, want 202", resp.StatusCode)
	}
	body := decodeUpscaleResponse(t, resp)
	if body.Enqueued != 1 || body.Rejected != 0 {
		t.Errorf("response: got %+v, want {Enqueued: 1}", body)
	}
	if len(stub.calls) != 1 || stub.calls[0] != "Artist/Album/01.flac" {
		t.Errorf("enqueuer calls: got %v, want [Artist/Album/01.flac]", stub.calls)
	}
}

// TestUpscaleFolderRecursivelyWalks — folder request walks all
// regular files and dispatches them by library-relative path.
func TestUpscaleFolderRecursivelyWalks(t *testing.T) {
	hs, tok, _, stub := upscaleFixture(t, true)
	resp := postJSON(t, hs, "/v1/upscale", tok, UpscaleRequest{Path: "Artist/Album"})
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("status: got %d, want 202", resp.StatusCode)
	}
	body := decodeUpscaleResponse(t, resp)
	if body.Enqueued != 2 {
		t.Errorf("Enqueued: got %d, want 2 (the two files in Artist/Album)", body.Enqueued)
	}
	if len(stub.calls) != 2 {
		t.Errorf("enqueuer calls: got %d, want 2", len(stub.calls))
	}
	expected := map[string]bool{"Artist/Album/01.flac": false, "Artist/Album/02.flac": false}
	for _, c := range stub.calls {
		if _, ok := expected[c]; !ok {
			t.Errorf("unexpected enqueue %q", c)
			continue
		}
		expected[c] = true
	}
	for path, seen := range expected {
		if !seen {
			t.Errorf("missing enqueue for %q", path)
		}
	}
}

// TestUpscaleQueueFullPartial — some candidates accepted, some
// rejected with ErrUpscaleQueueFull. Response keeps 202 + reports
// queueFull: true so iOS can toast.
func TestUpscaleQueueFullPartial(t *testing.T) {
	hs, tok, _, stub := upscaleFixture(t, true)
	stub.resultByRel["Artist/Album/02.flac"] = ErrUpscaleQueueFull

	resp := postJSON(t, hs, "/v1/upscale", tok, UpscaleRequest{Path: "Artist/Album"})
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("status: got %d, want 202 (partial success)", resp.StatusCode)
	}
	body := decodeUpscaleResponse(t, resp)
	if body.Enqueued != 1 || body.Rejected != 1 || !body.QueueFull {
		t.Errorf("response: got %+v, want {Enqueued: 1, Rejected: 1, QueueFull: true}", body)
	}
}

// TestUpscaleQueueFullEverythingReturns503 — every candidate
// bounces queue-full → 503 with `queue_full` code, not a
// misleading 202+enqueued:0.
func TestUpscaleQueueFullEverythingReturns503(t *testing.T) {
	hs, tok, _, stub := upscaleFixture(t, true)
	stub.defaultErr = ErrUpscaleQueueFull

	resp := postJSON(t, hs, "/v1/upscale", tok, UpscaleRequest{Path: "Artist/Album"})
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("status: got %d, want 503", resp.StatusCode)
	}
	assertWireErrorCode(t, resp, "queue_full")
}

// TestUpscaleIneligibleSilentlyRejects — DSD / already-at-target
// tracks return ErrUpscaleIneligible from the enqueuer; the
// handler counts them in `rejected` but doesn't flip queueFull
// (those are different conditions).
func TestUpscaleIneligibleSilentlyRejects(t *testing.T) {
	hs, tok, _, stub := upscaleFixture(t, true)
	stub.defaultErr = ErrUpscaleIneligible

	resp := postJSON(t, hs, "/v1/upscale", tok, UpscaleRequest{Path: "Artist/Single.flac"})
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("status: got %d, want 202 (ineligible isn't a server error)", resp.StatusCode)
	}
	body := decodeUpscaleResponse(t, resp)
	if body.Enqueued != 0 || body.Rejected != 1 || body.QueueFull {
		t.Errorf("response: got %+v, want {Enqueued: 0, Rejected: 1, QueueFull: false}", body)
	}
}

// TestUpscaleBadJSONReturns400 — body that isn't valid JSON or
// has no path field → 400 bad_request.
func TestUpscaleBadJSONReturns400(t *testing.T) {
	hs, tok, _, _ := upscaleFixture(t, true)

	// Empty path.
	resp1 := postJSON(t, hs, "/v1/upscale", tok, UpscaleRequest{Path: ""})
	defer resp1.Body.Close()
	if resp1.StatusCode != 400 {
		t.Errorf("empty path status: got %d, want 400", resp1.StatusCode)
	}

	// Malformed JSON.
	req, _ := http.NewRequest("POST", hs.URL+"/v1/upscale", bytes.NewReader([]byte("not json")))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Errorf("malformed body status: got %d, want 400", resp2.StatusCode)
	}
}

// TestUpscaleNotFoundPathReturns404 — path that doesn't resolve
// under any library root → 404 not_found.
func TestUpscaleNotFoundPathReturns404(t *testing.T) {
	hs, tok, _, _ := upscaleFixture(t, true)
	resp := postJSON(t, hs, "/v1/upscale", tok, UpscaleRequest{Path: "Nope/missing.flac"})
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestUpscaleTraversalRejected — `..`-segments must hit the
// resolver's traversal guard (400 bad_request).
func TestUpscaleTraversalRejected(t *testing.T) {
	hs, tok, _, _ := upscaleFixture(t, true)
	resp := postJSON(t, hs, "/v1/upscale", tok, UpscaleRequest{Path: "../../etc/passwd"})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestUpscaleErrorsIsCheckOnSentinels — locks the
// errors.Is/errors.Is contract on the public sentinels so a
// future refactor can't accidentally break the typed-error
// wrapping.
func TestUpscaleErrorsIsCheckOnSentinels(t *testing.T) {
	wrapped := errors.New("inner")
	for _, sentinel := range []error{ErrUpscaleQueueFull, ErrUpscaleSourceMissing, ErrUpscaleIneligible} {
		wrappedErr := errors.Join(sentinel, wrapped)
		if !errors.Is(wrappedErr, sentinel) {
			t.Errorf("errors.Is failed for sentinel %v", sentinel)
		}
	}
}
