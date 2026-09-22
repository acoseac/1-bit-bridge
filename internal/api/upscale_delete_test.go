package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
	// locate, when set, answers LocateVariantSidecar. nil means every
	// row is at its recorded path INSIDE the current variants directory,
	// which is what the bridge reported before #937's rule reached this
	// handler and is what keeps the pre-existing cases in this file
	// describing the same bridge. WithinStore false is the relocation
	// shape — a recorded path on some OTHER tree — and a case that wants
	// it says so.
	locate func(VariantSummary) VariantSidecarLocation
	// storeUnavailable makes SidecarStoreState answer unavailable — the
	// unmounted-volume state. Default false (available), which is the
	// bridge every pre-existing case in this file describes.
	storeUnavailable bool
	// storeEmpty is the one unavailable reason a caller can explain
	// away, so it is separable here: an unmounted mountpoint reverts to
	// an empty local directory, and so does a flat variants directory
	// whose last file this very request unlinked. Only meaningful
	// alongside storeUnavailable.
	storeEmpty bool
	// storeID names the directory INSTANCE the probe reports. Changing
	// it mid-request is how a test spells "the mount dropped and a
	// different directory is at that path now". Empty means no identity
	// at all (a probe that could not stat), which the handler must read
	// as "cannot claim I emptied it".
	storeID string
	// storeProbes counts SidecarStoreState calls. The probe answers "is
	// a missing sidecar evidence about the FILE or about the VOLUME",
	// which is a question about a file that is missing — asking it of a
	// row whose file was just located and unlinked is the defect, and a
	// status assertion cannot see it.
	storeProbes int
}

func (s *stubVariantDeleter) SidecarStoreState() VariantSidecarStoreState {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storeProbes++
	st := VariantSidecarStoreState{Available: !s.storeUnavailable, Empty: s.storeEmpty}
	if s.storeID != "" {
		st.Store = stubStoreIdentity(s.storeID)
	}
	return st
}

// stubStoreIdentity stands in for the adapter's os.SameFile wrapper.
type stubStoreIdentity string

func (a stubStoreIdentity) Same(other SidecarStoreIdentity) bool {
	b, ok := other.(stubStoreIdentity)
	return ok && a == b
}

func (s *stubVariantDeleter) LocateVariantSidecar(v VariantSummary) VariantSidecarLocation {
	s.mu.Lock()
	fn := s.locate
	s.mu.Unlock()
	if fn == nil {
		return VariantSidecarLocation{Placement: VariantSidecarRecorded, Path: v.SidecarPath, WithinStore: true}
	}
	return fn(v)
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
	// Live feature predicate. It advertises the /v1/health feature flag AND —
	// since the delete handler gained the gate the nil-deleter check stopped
	// providing — decides whether this endpoint answers at all.
	srv.upscaleEnabled = func() bool { return true }

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

// TestUpscaleDeleteUnlinksARelocatedSidecar.
//
// `sidecar_path` is absolute, so a variants directory moved to another
// host leaves every row naming the old one while every file sits,
// byte-identical, at its canonical place under the current one. The
// unlink used only the recorded path, and its ENOENT flowed through the
// already-gone guard as success — so the operator's "delete all
// renditions" answered `deletedCount: N, freedBytes: 0`, left the bytes
// on disk with nothing referencing them, and handed `--gc` a tree it
// then refuses to reclaim (orphans > rows).
//
// #937 made "a recorded path is a claim, never proof" the rule and
// enumerated three reapers. This path deletes rows too and was not one
// of them.
//
// The assertion that matters is on the FILE, not on the response: a
// count alone is satisfied by the bug, which deletes every row happily.
func TestUpscaleDeleteUnlinksARelocatedSidecar(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)

	tmp := t.TempDir()
	recorded := filepath.Join(tmp, "old-host", "abc-v1.flac") // never created
	canonical := filepath.Join(tmp, "new-host", "abc-v1.flac")
	if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonical, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	row := VariantSummary{
		SourcePath:  "Music/Album/01.flac",
		VariantID:   "v1",
		SidecarPath: recorded,
		SizeBytes:   10,
	}
	deleter.byPath["Music/Album/01.flac"] = []VariantSummary{row}
	deleter.locate = func(v VariantSummary) VariantSidecarLocation {
		return VariantSidecarLocation{Placement: VariantSidecarRelocated, Path: canonical}
	}

	resp := authDelete(t, hs, "/v1/upscale/variants?path=Music/Album/01.flac", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if _, err := os.Stat(canonical); !os.IsNotExist(err) {
		t.Fatalf("the relocated sidecar is still on disk (stat err %v) — "+
			"the delete reported success and reclaimed nothing", err)
	}
	dr := decodeDeleteResponse(t, resp)
	if dr.DeletedCount != 1 {
		t.Errorf("deletedCount: got %d, want 1", dr.DeletedCount)
	}
	if dr.FreedBytes != 10 {
		t.Errorf("freedBytes: got %d, want 10 — the bytes really were reclaimed", dr.FreedBytes)
	}
}

// TestUpscaleDeleteLeavesACopyInFlightAlone — the other half of the
// locator's verdict, and the one where doing nothing is the answer.
//
// A file at the canonical path whose size disagrees with the row is not
// this row's sidecar. Unlinking it would take a file the job pool may
// still be writing; deleting the ROW would strand it as an orphan.
// Neither — the same reading LookupVariant takes when it answers 410
// rather than reaping.
func TestUpscaleDeleteLeavesACopyInFlightAlone(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)

	tmp := t.TempDir()
	inFlight := filepath.Join(tmp, "abc-v1.flac")
	if err := os.WriteFile(inFlight, []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	deleter.byPath["Music/Album/01.flac"] = []VariantSummary{{
		SourcePath: "Music/Album/01.flac", VariantID: "v1",
		SidecarPath: filepath.Join(tmp, "elsewhere", "abc-v1.flac"), SizeBytes: 999,
	}}
	deleter.locate = func(v VariantSummary) VariantSidecarLocation {
		return VariantSidecarLocation{Placement: VariantSidecarCopyInFlight}
	}

	resp := authDelete(t, hs, "/v1/upscale/variants?path=Music/Album/01.flac", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Errorf("the in-flight copy was unlinked: %v", err)
	}
	if got := deleter.deletedKeys(); len(got) != 0 {
		t.Errorf("DeleteVariant calls: got %v, want none — the row must outlive a copy in flight", got)
	}
	dr := decodeDeleteResponse(t, resp)
	if dr.DeletedCount != 0 || dr.FreedBytes != 0 {
		t.Errorf("deletedCount/freedBytes = %d/%d, want 0/0", dr.DeletedCount, dr.FreedBytes)
	}
}

// TestUpscaleDeleteKeepsRowsWhileTheVariantsDirIsUnavailable.
//
// The round-1 fix taught this handler to ask WHERE the file is, and
// left open the case where the answer cannot be trusted: with the
// variants directory unmounted, LocateSidecar stats both the recorded
// and the canonical path under the same dead mountpoint, answers
// "absent at both", and the ENOENT that follows flows through the
// already-gone guard as success — deleting the row while its sidecar
// sits intact on the volume that will come back.
//
// That is the same stranding the relocation case produces, reached by a
// different cause, so it takes the answer the two sweeps already give:
// keep the row. (CodeRabbit Major on #959.)
func TestUpscaleDeleteKeepsRowsWhileTheVariantsDirIsUnavailable(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)
	deleter.byPath["Music/Album/01.flac"] = []VariantSummary{{
		SourcePath: "Music/Album/01.flac", VariantID: "v1",
		SidecarPath: filepath.Join(t.TempDir(), "unmounted", "abc-v1.flac"), SizeBytes: 10,
	}}
	deleter.mu.Lock()
	deleter.storeUnavailable = true
	deleter.mu.Unlock()

	resp := authDelete(t, hs, "/v1/upscale/variants?path=Music/Album/01.flac", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if got := deleter.deletedKeys(); len(got) != 0 {
		t.Fatalf("DeleteVariant calls: got %v, want none — the row must outlive an unmounted volume", got)
	}
	dr := decodeDeleteResponse(t, resp)
	if dr.DeletedCount != 0 || dr.FreedBytes != 0 {
		t.Errorf("deletedCount/freedBytes = %d/%d, want 0/0", dr.DeletedCount, dr.FreedBytes)
	}
}

// TestUpscaleDeleteUnlinksAFileItLocatedThoughTheDirIsEmpty is the
// other half of the rule above, and the one the round-2 shape had
// backwards.
//
// The probe answers "is a MISSING sidecar evidence about the file or
// about the volume". Asked before the locate, of every row, it also
// answered for rows whose file the locate had just found — and an EMPTY
// variants directory is one of its unavailable reasons. So the ordinary
// shape of a moved variants directory (config repointed at a fresh
// folder, every rendition still at the absolute path its row records)
// made "delete all renditions" unlink nothing and leave the bytes:
// exactly the stranding #959's lookup was added to end, reached one
// commit later through the guard added beside it.
func TestUpscaleDeleteUnlinksAFileItLocatedThoughTheDirIsEmpty(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)
	// The file is where the row says — on the OLD tree, which is why the
	// newly-configured variants directory has nothing in it.
	old := filepath.Join(t.TempDir(), "abc-v1.flac")
	if err := os.WriteFile(old, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	deleter.byPath["Music/Album/01.flac"] = []VariantSummary{{
		SourcePath: "Music/Album/01.flac", VariantID: "v1",
		SidecarPath: old, SizeBytes: 10,
	}}
	deleter.mu.Lock()
	deleter.storeUnavailable, deleter.storeEmpty = true, true
	// The file is on the OLD tree — that is what makes the newly
	// configured directory empty — so the locate says so rather than
	// leaning on the default.
	deleter.locate = func(v VariantSummary) VariantSidecarLocation {
		return VariantSidecarLocation{Placement: VariantSidecarRecorded, Path: v.SidecarPath, WithinStore: false}
	}
	deleter.mu.Unlock()

	resp := authDelete(t, hs, "/v1/upscale/variants?path=Music/Album/01.flac", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the located sidecar is still on disk (%v) — the volume probe answered "+
			"about a file that had just been found", err)
	}
	if got := deleter.deletedKeys(); len(got) != 1 {
		t.Errorf("DeleteVariant calls: got %v, want the one row", got)
	}
	dr := decodeDeleteResponse(t, resp)
	if dr.DeletedCount != 1 || dr.FreedBytes != 10 {
		t.Errorf("deletedCount/freedBytes = %d/%d, want 1/10", dr.DeletedCount, dr.FreedBytes)
	}
	// The probe was not consulted to DECIDE anything here: nothing was
	// missing. It is called at most once, to record which directory
	// instance the first in-store unlink happened in — and this row's
	// unlink was NOT in-store (the file is on the old tree), so not even
	// that.
	deleter.mu.Lock()
	probes := deleter.storeProbes
	deleter.mu.Unlock()
	if probes != 0 {
		t.Errorf("SidecarStoreState was probed %d time(s) for a row whose file was present and "+
			"outside the store — the question is about a file that is missing", probes)
	}
}

// TestUpscaleDeleteFinishesAFlatTreeItEmptiedItself.
//
// On the legacy hash-flat layout (`<dir>/<hash>-<variantID>.flac`, no
// subdirectories) this request's own unlinks are what empty the
// directory. A probe after that refuses the remaining rows on a state
// the request just created, and no re-run clears it: the directory is
// still empty, so it refuses again having removed nothing. That is
// gcCheckOutputDirBeforeReverseSweep's defect (#941) one layer over, and
// it takes the same answer — EMPTY alone is explicable once files have
// been removed; missing, unreadable and not-a-directory are not, because
// this loop unlinks files and never directories.
func TestUpscaleDeleteFinishesAFlatTreeItEmptiedItself(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)
	dir := t.TempDir()
	first := filepath.Join(dir, "abc-v1.flac")
	if err := os.WriteFile(first, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Row two's sidecar is already gone — a prior --gc, a manual wipe.
	// Reconciling it is the whole point of an idempotent delete.
	deleter.all = []VariantSummary{
		{SourcePath: "Music/Album/01.flac", VariantID: "v1", SidecarPath: first, SizeBytes: 10},
		{SourcePath: "Music/Album/02.flac", VariantID: "v1", SidecarPath: filepath.Join(dir, "def-v1.flac"), SizeBytes: 20},
	}
	// The directory reads EMPTY from the second row onwards, because the
	// first row's unlink emptied it. The stub cannot watch the disk, so
	// it is set up front — which is the stricter fixture: it is empty for
	// the first row too, and that row's file is still found and removed.
	//
	// One stable identity throughout: the same directory all along, which
	// is what "this request emptied it" means.
	deleter.mu.Lock()
	deleter.storeUnavailable, deleter.storeEmpty = true, true
	deleter.storeID = "vol-A"
	deleter.mu.Unlock()

	resp := authDelete(t, hs, "/v1/upscale/variants?confirm=true", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if got := deleter.deletedKeys(); len(got) != 2 {
		t.Fatalf("DeleteVariant calls: got %v, want both rows — the second was refused on a state "+
			"the first row's unlink created", got)
	}
	dr := decodeDeleteResponse(t, resp)
	// One byte total: the already-gone row frees nothing.
	if dr.DeletedCount != 2 || dr.FreedBytes != 10 {
		t.Errorf("deletedCount/freedBytes = %d/%d, want 2/10", dr.DeletedCount, dr.FreedBytes)
	}
}

// TestUpscaleDeleteKeepsRowsWhenAnEmptyDirIsNotItsOwnDoing is the
// negative control for the exception above: EMPTY only explains itself
// once this request has unlinked something. With nothing removed, an
// empty directory is the cleanly-unmounted mountpoint it has always
// been, and the row stays.
func TestUpscaleDeleteKeepsRowsWhenAnEmptyDirIsNotItsOwnDoing(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)
	deleter.byPath["Music/Album/01.flac"] = []VariantSummary{{
		SourcePath: "Music/Album/01.flac", VariantID: "v1",
		SidecarPath: filepath.Join(t.TempDir(), "unmounted", "abc-v1.flac"), SizeBytes: 10,
	}}
	deleter.mu.Lock()
	deleter.storeUnavailable, deleter.storeEmpty = true, true
	deleter.mu.Unlock()

	resp := authDelete(t, hs, "/v1/upscale/variants?path=Music/Album/01.flac", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if got := deleter.deletedKeys(); len(got) != 0 {
		t.Fatalf("DeleteVariant calls: got %v, want none — nothing was unlinked, so an empty "+
			"directory is still the unmount it has always been", got)
	}
}

// TestUpscaleDeleteWillNotExplainAnEmptyStoreWithAnUnlinkOutsideIt.
//
// The empty-store exception exists because on the legacy hash-flat
// layout THIS request's own unlinks empty the directory. Counting every
// removed file made that argument from evidence it does not have: a
// recorded sidecar_path is absolute and need not be under the current
// variants directory at all — that is the relocation the whole lookup
// exists for — so unlinking from the OLD tree said nothing about the
// CURRENT one.
//
// The shape: rows recorded on a still-mounted old tree, the current
// variants directory unmounted (a clean unmount reverts a mountpoint to
// an empty local directory). One old file is removed, and the next row —
// whose sidecar sits intact on the volume that will come back — was then
// deleted under the exception and orphaned. (CodeRabbit on #968.)
func TestUpscaleDeleteWillNotExplainAnEmptyStoreWithAnUnlinkOutsideIt(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)
	oldTree := t.TempDir()
	present := filepath.Join(oldTree, "abc-v1.flac")
	if err := os.WriteFile(present, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	deleter.all = []VariantSummary{
		// On the old tree, and still there: unlinked, but from a
		// directory the probe is not describing.
		{SourcePath: "Music/Album/01.flac", VariantID: "v1", SidecarPath: present, SizeBytes: 10},
		// Absent at both locations — which is what an unmounted current
		// volume looks like from a stat.
		{SourcePath: "Music/Album/02.flac", VariantID: "v1", SidecarPath: filepath.Join(oldTree, "def-v1.flac"), SizeBytes: 20},
	}
	deleter.mu.Lock()
	deleter.locate = func(v VariantSummary) VariantSidecarLocation {
		// Recorded paths on the OLD tree: outside the probed store.
		return VariantSidecarLocation{Placement: VariantSidecarRecorded, Path: v.SidecarPath, WithinStore: false}
	}
	deleter.storeUnavailable, deleter.storeEmpty = true, true
	deleter.mu.Unlock()

	resp := authDelete(t, hs, "/v1/upscale/variants?confirm=true", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	got := deleter.deletedKeys()
	for _, k := range got {
		if strings.Contains(k, "02.flac") {
			t.Fatalf("the second row was deleted (%v) — an unlink from the OLD tree was taken as "+
				"proof this request emptied the CURRENT one, which is unmounted", got)
		}
	}
	// The first row is still reconciled: its file really was removed.
	if len(got) != 1 || !strings.Contains(got[0], "01.flac") {
		t.Errorf("DeleteVariant calls: got %v, want only the row whose file was unlinked", got)
	}
}

// TestUpscaleDeleteWillNotExplainAnEmptyStoreAfterTheMountChanged.
//
// The empty-store exception argued from "this request unlinked
// something here", and a clean unmount reverts a mountpoint to an empty
// LOCAL directory — which exists, is a directory, and holds no entries.
// So an unlink at row k proved only that the volume was mounted at row
// k, and said nothing about row k+1.
//
// That matters because the probe is per row precisely BECAUSE a
// whole-library delete runs long enough for a mount to drop underneath
// it. Without an identity compare, the drop let every row after it be
// deleted while its sidecar sat intact on the volume that will come
// back — the stranding this handler is being fixed for, re-entered
// through its own exception. (CodeRabbit on #968.)
func TestUpscaleDeleteWillNotExplainAnEmptyStoreAfterTheMountChanged(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)
	dir := t.TempDir()
	first := filepath.Join(dir, "abc-v1.flac")
	if err := os.WriteFile(first, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	deleter.all = []VariantSummary{
		{SourcePath: "Music/Album/01.flac", VariantID: "v1", SidecarPath: first, SizeBytes: 10},
		{SourcePath: "Music/Album/02.flac", VariantID: "v1", SidecarPath: filepath.Join(dir, "def-v1.flac"), SizeBytes: 20},
	}
	// The mount drops after the first row: the path is still an empty
	// directory, but it is a DIFFERENT directory — the local mountpoint,
	// not the volume that was on it.
	deleter.mu.Lock()
	deleter.storeUnavailable, deleter.storeEmpty = true, true
	deleter.storeID = "the-mounted-volume"
	deleter.locate = func(v VariantSummary) VariantSidecarLocation {
		if v.SourcePath == "Music/Album/02.flac" {
			// Row two runs after the drop: both stats land under a dead
			// mountpoint and answer ENOENT, which is "absent at both".
			deleter.storeID = "the-bare-mountpoint"
			return VariantSidecarLocation{Placement: VariantSidecarAbsent}
		}
		return VariantSidecarLocation{Placement: VariantSidecarRecorded, Path: v.SidecarPath, WithinStore: true}
	}
	deleter.mu.Unlock()

	resp := authDelete(t, hs, "/v1/upscale/variants?confirm=true", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	got := deleter.deletedKeys()
	for _, k := range got {
		if strings.Contains(k, "02.flac") {
			t.Fatalf("the row after the mount dropped was deleted (%v) — an unlink from the volume "+
				"that WAS mounted was taken as proof this request emptied the directory that is "+
				"there now, and its sidecar comes back with the volume", got)
		}
	}
	if len(got) != 1 {
		t.Errorf("DeleteVariant calls: got %v, want only the row unlinked before the drop", got)
	}
}

// TestUpscaleDeleteWillNotExplainAnEmptyStoreWithNoIdentity is the
// fail-closed half: a probe that could not stat the directory reports no
// identity, and "I cannot tell you which directory this is" must not
// satisfy "it is the one I emptied".
func TestUpscaleDeleteWillNotExplainAnEmptyStoreWithNoIdentity(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)
	dir := t.TempDir()
	first := filepath.Join(dir, "abc-v1.flac")
	if err := os.WriteFile(first, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	deleter.all = []VariantSummary{
		{SourcePath: "Music/Album/01.flac", VariantID: "v1", SidecarPath: first, SizeBytes: 10},
		{SourcePath: "Music/Album/02.flac", VariantID: "v1", SidecarPath: filepath.Join(dir, "def-v1.flac"), SizeBytes: 20},
	}
	deleter.mu.Lock()
	deleter.storeUnavailable, deleter.storeEmpty = true, true
	deleter.storeID = "" // no identity at all
	deleter.mu.Unlock()

	resp := authDelete(t, hs, "/v1/upscale/variants?confirm=true", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	for _, k := range deleter.deletedKeys() {
		if strings.Contains(k, "02.flac") {
			t.Fatalf("a probe with no identity satisfied the empty-store exception (%v)",
				deleter.deletedKeys())
		}
	}
}
