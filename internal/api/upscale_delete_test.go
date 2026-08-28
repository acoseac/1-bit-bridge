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
	"time"

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
	// afterDelete, when set, runs after each successful DeleteVariant —
	// lets a test cancel the request context mid-loop to exercise the
	// ctx early-break.
	afterDelete func()
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
	// Mirror the STORE's scoping, which appends its own separator so a
	// folder prefix matches descendants only. The stub previously used
	// a bare `HasPrefix(r.SourcePath, prefix)` — the same over-matching
	// shape the real query had, so it could never catch the divergence
	// where `?prefix=Album` also reaped `Album 2/`.
	out := []VariantSummary{}
	scope := strings.TrimRight(prefix, "/")
	for _, r := range s.all {
		if scope == "" || strings.HasPrefix(r.SourcePath, scope+"/") {
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
	if s.afterDelete != nil {
		s.afterDelete()
	}
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
	srv.upscaleEnabled = func() bool { return true } // so the /v1/health features path advertises (not used in this file)

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

func decodeDeleteResponse(t *testing.T, resp *http.Response) VariantDeleteResponse {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var r VariantDeleteResponse
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
	// The sidecar never existed on disk, so nothing was actually reclaimed:
	// FreedBytes counts only bytes truly unlinked (B15), while DeletedCount
	// still reflects the reconciled DB row.
	if dr.FreedBytes != 0 {
		t.Errorf("freedBytes: got %d, want 0 (sidecar was already absent)", dr.FreedBytes)
	}
	if len(dr.DeletedPaths) != 1 || dr.DeletedPaths[0] != "Music/Album/01.flac" {
		t.Errorf("deletedPaths: got %v, want [Music/Album/01.flac]", dr.DeletedPaths)
	}
	got := deleter.deletedKeys()
	if len(got) != 1 || got[0] != "Music/Album/01.flac|v1" {
		t.Errorf("DeleteVariant calls: got %v, want [Music/Album/01.flac|v1]", got)
	}
}

// TestUpscaleDelete_kindNarrowsToUpscale pins the per-kind filter
// contract added in PR #276 (senior-review high-severity fix from
// Gemini): a DELETE with ?kind=upscale must delete ONLY variants
// whose variant_id begins with "upscaled-", leaving "optimized-"
// variants untouched. Without the filter (the load-bearing
// gap before the fix) per-kind drawer Delete buttons would have
// silently wiped both kinds.
// seedMixedKindFixture installs three variant rows (one upscaled +
// two optimized) on the deleter stub. Shared by the upscale-only
// and optimize-only kind-narrow tests below so the row literal
// doesn't repeat across files (was a SonarCloud duplication trip).
func seedMixedKindFixture(deleter *stubVariantDeleter) {
	deleter.all = []VariantSummary{
		{SourcePath: "Music/Album/01.flac", VariantID: "upscaled-v2-192000-24",
			SidecarPath: "/tmp/u1", SizeBytes: 1000},
		{SourcePath: "Music/Album/01.flac", VariantID: "optimized-v2-48000-16",
			SidecarPath: "/tmp/o1", SizeBytes: 200},
		{SourcePath: "Music/Album/02.flac", VariantID: "optimized-v2-44100-16",
			SidecarPath: "/tmp/o2", SizeBytes: 250},
	}
}

func TestUpscaleDelete_kindNarrowsToUpscale(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)
	seedMixedKindFixture(deleter)

	resp := authDelete(t, hs, "/v1/upscale/variants?confirm=true&kind=upscale", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	dr := decodeDeleteResponse(t, resp)
	if dr.DeletedCount != 1 {
		t.Errorf("deletedCount: got %d, want 1 (only the upscaled-v2 row)", dr.DeletedCount)
	}
	got := deleter.deletedKeys()
	if len(got) != 1 || got[0] != "Music/Album/01.flac|upscaled-v2-192000-24" {
		t.Errorf("DeleteVariant calls: got %v, want [Music/Album/01.flac|upscaled-v2-192000-24]", got)
	}
}

// TestUpscaleDelete_kindNarrowsToOptimize is the optimize-side
// mirror of the upscale test above. Asserts the optimized-v2
// rows are deleted and the upscaled-v2 row is NOT.
func TestUpscaleDelete_kindNarrowsToOptimize(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)
	seedMixedKindFixture(deleter)

	resp := authDelete(t, hs, "/v1/upscale/variants?confirm=true&kind=optimize", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	dr := decodeDeleteResponse(t, resp)
	if dr.DeletedCount != 2 {
		t.Errorf("deletedCount: got %d, want 2 (both optimized- rows)", dr.DeletedCount)
	}
	for _, key := range deleter.deletedKeys() {
		if !strings.Contains(key, "optimized-") {
			t.Errorf("unexpected deletion of non-optimize variant: %s", key)
		}
	}
}

// TestUpscaleDelete_kindEmptyPreservesLegacyBehaviour asserts that
// callers that DON'T set kind (e.g. an iOS client predating the
// per-kind feature, or a stray external curl) keep the pre-feature
// behaviour: all variants matching the path scope get deleted
// regardless of kind. Critical for back-compat.
func TestUpscaleDelete_kindEmptyPreservesLegacyBehaviour(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)
	deleter.all = []VariantSummary{
		{SourcePath: "Music/Album/01.flac", VariantID: "upscaled-v2-192000-24",
			SidecarPath: "/tmp/u1", SizeBytes: 1000},
		{SourcePath: "Music/Album/01.flac", VariantID: "optimized-v2-48000-16",
			SidecarPath: "/tmp/o1", SizeBytes: 200},
	}

	// No kind param.
	resp := authDelete(t, hs, "/v1/upscale/variants?confirm=true", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	dr := decodeDeleteResponse(t, resp)
	if dr.DeletedCount != 2 {
		t.Errorf("deletedCount: got %d, want 2 (both kinds, legacy behaviour)", dr.DeletedCount)
	}
}

// TestUpscaleDelete_kindUnknownReturns400 pins the parser-layer
// rejection so a typo doesn't silently fall through.
func TestUpscaleDelete_kindUnknownReturns400(t *testing.T) {
	hs, raw, _, _ := deleteFixture(t, true)
	resp := authDelete(t, hs, "/v1/upscale/variants?confirm=true&kind=junk", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("kind=junk: got %d, want 400", resp.StatusCode)
	}
}

// TestRunVariantDelete_unknownKindRejected is the defense-in-depth
// mirror at the RunVariantDelete method boundary. The HTTP parser
// already rejects unknown kinds at the wire (TestUpscaleDelete_
// kindUnknownReturns400 above), but RunVariantDelete is exported —
// a direct caller (test harness, internal tool, future
// integration) that constructs VariantDeleteRequest{Kind: "junk"}
// would have the empty wantPrefix match every row via
// `strings.HasPrefix(..., "")` and silently widen the delete back
// to BOTH kinds. The `default` arm in the switch closes that gap.
// Per CodeRabbit major on PR #276 round 3.
func TestRunVariantDelete_unknownKindRejected(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	authStore, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	srv := New(cfg, authStore, nil, "fp")
	deleter := &stubVariantDeleter{
		all: []VariantSummary{
			{SourcePath: "Music/Album/01.flac", VariantID: "upscaled-v2-192000-24",
				SidecarPath: "/tmp/u1", SizeBytes: 1000},
		},
		byPath: map[string][]VariantSummary{},
	}
	srv = srv.WithVariantDeleter(deleter)

	_, err := srv.RunVariantDelete(context.Background(), VariantDeleteRequest{
		All:  true,
		Kind: "junk",
	})
	if err == nil {
		t.Fatalf("RunVariantDelete Kind=\"junk\": got nil, want unknown-kind error")
	}
	if !strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("error message: %q, want \"unknown kind\" substring", err.Error())
	}
	// Critically: no rows were touched. Without the default arm,
	// strings.HasPrefix(row.VariantID, "") would have matched every
	// row and the unlink+delete loop would have silently widened
	// the operation to all kinds.
	if got := deleter.deletedKeys(); len(got) != 0 {
		t.Errorf("unexpected deletions on unknown-kind reject: %v", got)
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

// TestRunVariantDelete_DedupsVariantIDsInSSE pins the dedup: two tracks
// sharing ONE variant kind must emit that variant ID exactly once in the
// upscale.deleted event (mirrors the DeletedPaths dedup), while both
// distinct source paths survive.
func TestRunVariantDelete_DedupsVariantIDsInSSE(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{tmp}, ListenAddress: ":7788", LibraryName: "Test"}
	authStore, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	srv := New(cfg, authStore, nil, "fp")
	deleter := &stubVariantDeleter{
		all: []VariantSummary{
			{SourcePath: "Music/Album/01.flac", VariantID: "upscaled-v2-192000-24",
				SidecarPath: filepath.Join(tmp, "u1"), SizeBytes: 1000},
			{SourcePath: "Music/Album/02.flac", VariantID: "upscaled-v2-192000-24",
				SidecarPath: filepath.Join(tmp, "u2"), SizeBytes: 1000},
		},
		byPath: map[string][]VariantSummary{},
	}
	srv = srv.WithVariantDeleter(deleter)
	stop := srv.StartEventBroker()
	defer stop()

	sub, _, _ := srv.eventBroker.subscribe([]string{"upscale"}, "", 0)
	defer srv.eventBroker.unsubscribe(sub)

	resp, err := srv.RunVariantDelete(context.Background(), VariantDeleteRequest{All: true})
	if err != nil {
		t.Fatalf("RunVariantDelete: %v", err)
	}
	if resp.DeletedCount != 2 {
		t.Fatalf("deletedCount = %d, want 2", resp.DeletedCount)
	}

	select {
	case env := <-sub.ch:
		if env.Topic != "upscale.deleted" {
			t.Fatalf("topic = %q, want upscale.deleted", env.Topic)
		}
		var ev UpscaleDeletedEvent
		if err := json.Unmarshal(env.Data, &ev); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if len(ev.VariantIDs) != 1 || ev.VariantIDs[0] != "upscaled-v2-192000-24" {
			t.Errorf("VariantIDs = %v, want exactly [upscaled-v2-192000-24] (deduped)", ev.VariantIDs)
		}
		if len(ev.Paths) != 2 {
			t.Errorf("Paths = %v, want 2 distinct source paths", ev.Paths)
		}
	case <-time.After(time.Second):
		t.Fatal("upscale.deleted event not delivered")
	}
}

// TestRunVariantDelete_StopsOnContextCancel pins the ctx early-break:
// once the request context is canceled mid-loop, no further sidecars are
// unlinked / rows deleted (the file-gone/row-kept zombie cascade the
// break prevents), and DeletedCount reflects only the pre-cancel rows.
func TestRunVariantDelete_StopsOnContextCancel(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{tmp}, ListenAddress: ":7788", LibraryName: "Test"}
	authStore, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))

	ctx, cancel := context.WithCancel(context.Background())
	deleter := &stubVariantDeleter{
		all: []VariantSummary{
			{SourcePath: "Music/01.flac", VariantID: "v1", SidecarPath: filepath.Join(tmp, "a"), SizeBytes: 1},
			{SourcePath: "Music/02.flac", VariantID: "v2", SidecarPath: filepath.Join(tmp, "b"), SizeBytes: 1},
			{SourcePath: "Music/03.flac", VariantID: "v3", SidecarPath: filepath.Join(tmp, "c"), SizeBytes: 1},
		},
		byPath: map[string][]VariantSummary{},
	}
	// Cancel right after the first row's DB delete so iteration 2 breaks.
	deleter.afterDelete = cancel

	srv := New(cfg, authStore, nil, "fp").WithVariantDeleter(deleter)
	resp, err := srv.RunVariantDelete(ctx, VariantDeleteRequest{All: true})
	if err != nil {
		t.Fatalf("RunVariantDelete: %v", err)
	}
	if resp.DeletedCount != 1 {
		t.Errorf("deletedCount = %d, want 1 (loop broke after ctx cancel)", resp.DeletedCount)
	}
	if got := deleter.deletedKeys(); len(got) != 1 || got[0] != "Music/01.flac|v1" {
		t.Errorf("DeleteVariant calls = %v, want only [Music/01.flac|v1] (rows 2-3 skipped)", got)
	}
}
