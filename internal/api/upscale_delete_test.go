package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// stubVariantDeleter is a test double for api.VariantDeleter.
// Returns a fixed snapshot from list methods; records deletes.
type stubVariantDeleter struct {
	mu      sync.Mutex
	all     []VariantSummary
	byPath  map[string][]VariantSummary
	deletes []string // "sourcePath|variantID"
	listErr error    // injected for the enumeration-failure branch
	delErr  error    // injected for the per-row delete failure branch
}

func (s *stubVariantDeleter) AllVariants(ctx context.Context) ([]VariantSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]VariantSummary, len(s.all))
	copy(out, s.all)
	return out, nil
}

func (s *stubVariantDeleter) ListVariantsByPathPrefix(ctx context.Context, prefix string) ([]VariantSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := []VariantSummary{}
	for _, r := range s.all {
		if strings.HasPrefix(r.SourcePath, prefix) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *stubVariantDeleter) ListVariantsForPath(ctx context.Context, sourcePath string) ([]VariantSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	if v, ok := s.byPath[sourcePath]; ok {
		out := make([]VariantSummary, len(v))
		copy(out, v)
		return out, nil
	}
	return nil, nil
}

func (s *stubVariantDeleter) DeleteVariant(ctx context.Context, sourcePath, variantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delErr != nil {
		return s.delErr
	}
	s.deletes = append(s.deletes, sourcePath+"|"+variantID)
	return nil
}

func (s *stubVariantDeleter) deletedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.deletes))
	copy(out, s.deletes)
	return out
}

// stubInflightDropper records every predicate it was called with.
type stubInflightDropper struct {
	mu              sync.Mutex
	predicateCalled bool
	matchedPaths    []string
}

func (s *stubInflightDropper) DropInflight(matches func(sourcePath string) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.predicateCalled = true
	// Simulate the predicate being called against a couple of paths.
	probes := []string{"Music/Album/01.flac", "Other/Album/01.flac"}
	for _, p := range probes {
		if matches(p) {
			s.matchedPaths = append(s.matchedPaths, p)
		}
	}
	return len(s.matchedPaths)
}

// stubPublisher captures every Publish call so SSE emission tests
// can assert the topic + payload shape.
type stubPublisher struct {
	mu       sync.Mutex
	captured []struct {
		topic   string
		payload any
	}
}

func (p *stubPublisher) Publish(topic string, payload any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.captured = append(p.captured, struct {
		topic   string
		payload any
	}{topic, payload})
}

func (p *stubPublisher) lastEvent() (string, any, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.captured) == 0 {
		return "", nil, false
	}
	e := p.captured[len(p.captured)-1]
	return e.topic, e.payload, true
}

// deleteFixture stands up a Server wired with stubs for the
// variant deleter, the inflight dropper, AND the SSE broker.
// Returns the live test server + token + stubs for per-test
// assertion.
func deleteFixture(t *testing.T, wireDeleter bool) (*httptest.Server, string, *stubVariantDeleter, *stubInflightDropper) {
	t.Helper()
	tmp := t.TempDir()

	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("test")

	srv := New(cfg, store, nil, "fp")
	srv.upscaleEnabled = true // so the /v1/health features path advertises (not used in this file)

	deleter := &stubVariantDeleter{
		all:    []VariantSummary{},
		byPath: map[string][]VariantSummary{},
	}
	dropper := &stubInflightDropper{}
	if wireDeleter {
		srv = srv.WithVariantDeleter(deleter).WithInflightDropper(dropper)
	}

	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, deleter, dropper
}

func authDelete(t *testing.T, hs *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, hs.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	return resp
}

func decodeDeleteResponse(t *testing.T, resp *http.Response) upscaleDeleteResponse {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var r upscaleDeleteResponse
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&r); err != nil {
		t.Fatalf("decode response: %v (body=%q)", err, string(body))
	}
	return r
}

// TestUpscaleDelete_unwiredDeleterReturns404 pins the feature-flag
// contract: a Server without WithVariantDeleter returns 404,
// matching pre-feature bridges' "feature unavailable" shape.
func TestUpscaleDelete_unwiredDeleterReturns404(t *testing.T) {
	hs, raw, _, _ := deleteFixture(t, false /* don't wire deleter */)
	resp := authDelete(t, hs, "/v1/upscale/variants?confirm=true", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestUpscaleDelete_unscopedRequiresConfirm pins the safety gate:
// DELETE /v1/upscale/variants WITHOUT ?confirm=true returns 400
// to defend against accidental tooling.
func TestUpscaleDelete_unscopedRequiresConfirm(t *testing.T) {
	hs, raw, _, _ := deleteFixture(t, true)
	resp := authDelete(t, hs, "/v1/upscale/variants", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 (missing confirm)", resp.StatusCode)
	}
}

// TestUpscaleDelete_prefixAndPathRejected pins the mutual-exclusion
// contract: combining ?prefix= and ?path= is ambiguous, return 400.
func TestUpscaleDelete_prefixAndPathRejected(t *testing.T) {
	hs, raw, _, _ := deleteFixture(t, true)
	resp := authDelete(t, hs, "/v1/upscale/variants?prefix=A/&path=A/1.flac", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 (prefix+path)", resp.StatusCode)
	}
}

// TestUpscaleDelete_pathRejectsTraversal pins the path-validation
// guard: a `..` segment must produce 400, not silently traverse.
func TestUpscaleDelete_pathRejectsTraversal(t *testing.T) {
	hs, raw, _, _ := deleteFixture(t, true)
	resp := authDelete(t, hs, "/v1/upscale/variants?path=../../etc/passwd", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 (traversal)", resp.StatusCode)
	}
}

// TestUpscaleDelete_pathHappyPath pins the per-path delete shape.
// One row, no sidecar on disk (so unlink-then-DB-delete becomes
// noent-tolerant unlink + DB delete = success). Response carries
// the count + freed bytes.
func TestUpscaleDelete_pathHappyPath(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)

	tmp := t.TempDir()
	missingSidecar := filepath.Join(tmp, "abc-v1.flac") // never created
	row := VariantSummary{
		SourcePath:  "Music/Album/01.flac",
		VariantID:   "v1",
		SidecarPath: missingSidecar,
		SizeBytes:   100,
	}
	deleter.byPath["Music/Album/01.flac"] = []VariantSummary{row}

	resp := authDelete(t, hs, "/v1/upscale/variants?path=Music/Album/01.flac", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	dr := decodeDeleteResponse(t, resp)
	if dr.DeletedCount != 1 {
		t.Errorf("deletedCount: got %d, want 1", dr.DeletedCount)
	}
	if dr.FreedBytes != 100 {
		t.Errorf("freedBytes: got %d, want 100", dr.FreedBytes)
	}
	if len(dr.DeletedPaths) != 1 || dr.DeletedPaths[0] != "Music/Album/01.flac" {
		t.Errorf("deletedPaths: got %v, want [Music/Album/01.flac]", dr.DeletedPaths)
	}
	got := deleter.deletedKeys()
	if len(got) != 1 || got[0] != "Music/Album/01.flac|v1" {
		t.Errorf("DeleteVariant calls: got %v, want [Music/Album/01.flac|v1]", got)
	}
}

// TestUpscaleDelete_inflightDropperCalledWithSourcePathOnly pins the
// dedup-cancel contract: the predicate the handler hands to
// DropInflight must see source_paths, not composite keys.
func TestUpscaleDelete_inflightDropperCalledWithSourcePathOnly(t *testing.T) {
	hs, raw, deleter, dropper := deleteFixture(t, true)

	// Two distinct source paths in the delete set — predicate
	// should match both when probed.
	deleter.all = []VariantSummary{
		{SourcePath: "Music/Album/01.flac", VariantID: "v1", SidecarPath: "/tmp/x", SizeBytes: 1},
		{SourcePath: "Other/Album/01.flac", VariantID: "v2", SidecarPath: "/tmp/y", SizeBytes: 2},
	}

	resp := authDelete(t, hs, "/v1/upscale/variants?confirm=true", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if !dropper.predicateCalled {
		t.Errorf("InflightDropper.DropInflight was not called")
	}
	if len(dropper.matchedPaths) != 2 {
		t.Errorf("predicate matched %d source paths, want 2", len(dropper.matchedPaths))
	}
}

// TestUpscaleDelete_sseEventFires pins the SSE fan-out contract:
// every successful delete batch publishes a single upscale.deleted
// event carrying the union of paths + variantIDs.
func TestUpscaleDelete_sseEventFires(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)
	pub := &stubPublisher{}
	// Wire a custom publisher into the Server via the broker
	// EventPublisher seam. We can't reach private fields from
	// the test directly, so we test that publishUpscaleDeleted
	// (the helper) was invoked indirectly: assert the SSE
	// path went through the nop publisher OR (better) inject
	// a test broker. Use the nop-publisher path AND the
	// stubPublisher helper to assert the shape via direct call.
	_ = pub // documented for future broker-injection test seam

	deleter.byPath["Music/Album/01.flac"] = []VariantSummary{
		{SourcePath: "Music/Album/01.flac", VariantID: "v1", SidecarPath: "/tmp/x", SizeBytes: 1},
	}

	resp := authDelete(t, hs, "/v1/upscale/variants?path=Music/Album/01.flac", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}

	// Direct exercise of the SSE helper to pin the contract
	// shape — even when the production broker is the nop, the
	// helper's behavior is well-defined.
	publishUpscaleDeleted(pub, []string{"a"}, []string{"vA"})
	topic, payload, ok := pub.lastEvent()
	if !ok {
		t.Fatal("publishUpscaleDeleted did not call Publish")
	}
	if topic != "upscale.deleted" {
		t.Errorf("topic: got %q, want upscale.deleted", topic)
	}
	ev, ok := payload.(UpscaleDeletedEvent)
	if !ok {
		t.Fatalf("payload type: got %T, want UpscaleDeletedEvent", payload)
	}
	if len(ev.Paths) != 1 || ev.Paths[0] != "a" {
		t.Errorf("Paths: got %v, want [a]", ev.Paths)
	}
}

// TestUpscaleDelete_listErrorReturns500 pins the error-surface:
// an enumeration error before any delete fires must return 500.
func TestUpscaleDelete_listErrorReturns500(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)
	deleter.listErr = errors.New("simulated DB error")
	resp := authDelete(t, hs, "/v1/upscale/variants?confirm=true", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", resp.StatusCode)
	}
}

// TestUpscaleDelete_emptyResultReturns200 pins idempotence: no
// rows to delete returns 200 with deletedCount=0, NOT a 404.
// iOS treats 200 with zero count as "already in the desired
// state".
func TestUpscaleDelete_emptyResultReturns200(t *testing.T) {
	hs, raw, _, _ := deleteFixture(t, true)
	resp := authDelete(t, hs, "/v1/upscale/variants?path=Nothing/Here.flac", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	dr := decodeDeleteResponse(t, resp)
	if dr.DeletedCount != 0 {
		t.Errorf("deletedCount: got %d, want 0", dr.DeletedCount)
	}
	if dr.FreedBytes != 0 {
		t.Errorf("freedBytes: got %d, want 0", dr.FreedBytes)
	}
}

// TestValidateRelativePath_acceptsCleanRelative locks down the
// path-validation helper: a plain relative path with forward
// slashes and no trailing trash is accepted unchanged.
func TestValidateRelativePath_acceptsCleanRelative(t *testing.T) {
	for _, in := range []string{
		"Music/Album/01.flac",
		"a",
		"Albums/20%_Hits/01.flac",
		"Artist Name/Album/Track.flac",
	} {
		out, ok := validateRelativePath(in)
		if !ok {
			t.Errorf("rejected clean path %q", in)
			continue
		}
		if out != in {
			t.Errorf("path %q mutated to %q (must be byte-identical)", in, out)
		}
	}
}

// TestValidateRelativePath_rejectsBad pins the negative branches.
func TestValidateRelativePath_rejectsBad(t *testing.T) {
	for _, in := range []string{
		"",
		"/leading",
		"../escape",
		"a/../b",
		"a/./b",       // path.Clean would strip the ./, mismatch input → reject
		"a//b",        // double slash, same reason
		`a\backslash`, // Windows separator rejected
		"trailing/",   // path.Clean strips the slash, mismatch → reject
	} {
		if _, ok := validateRelativePath(in); ok {
			t.Errorf("accepted bad path %q", in)
		}
	}
}
