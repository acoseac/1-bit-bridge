package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// stubBatchCoordinator stands in for what actually sits behind this
// interface in production: cmd/bridge's `upscaleBatchCoordinatorAdapter`,
// which substitutes the stored admin Settings for a target of EXACTLY 0
// and then calls `transcode.Coordinator.Submit`. Both halves are
// modelled — the zero-substitution (so the documented "omit to use the
// bridge default" path stays exercised) and the coordinator's plain
// errors (which are what surfaced as a 500).
type stubBatchCoordinator struct {
	submits   int
	optimizes int
}

const (
	stubDefaultRate = 96000
	stubDefaultBits = 24
)

func (s *stubBatchCoordinator) Submit(ctx context.Context, p string, rate, bits int) (BatchSubmitResult, error) {
	s.submits++
	if rate == 0 {
		rate = stubDefaultRate
	}
	if bits == 0 {
		bits = stubDefaultBits
	}
	if rate <= 0 {
		return BatchSubmitResult{}, fmt.Errorf("submit: target rate %d Hz: must be positive", rate)
	}
	switch bits {
	case 16, 24, 32:
	default:
		return BatchSubmitResult{}, fmt.Errorf("submit: target bits %d: must be 16/24/32", bits)
	}
	return BatchSubmitResult{BatchID: "b", Path: p, TargetRate: rate, TargetBits: bits}, nil
}

func (s *stubBatchCoordinator) SubmitOptimize(ctx context.Context, p string) (BatchSubmitResult, error) {
	s.optimizes++
	return BatchSubmitResult{BatchID: "b", Path: p}, nil
}

func (s *stubBatchCoordinator) Cancel(id uuid.UUID) error           { return nil }
func (s *stubBatchCoordinator) ListBatches(int) ([]BatchRow, error) { return nil, nil }
func (s *stubBatchCoordinator) Throughput() BatchThroughput         { return BatchThroughput{} }

func batchFixture(t *testing.T) (*httptest.Server, string, *stubBatchCoordinator) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{tmp}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("batch")
	stub := &stubBatchCoordinator{}
	srv := New(cfg, store, nil, "fp").WithBatchCoordinator(stub)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, stub
}

// `targetRate` / `targetBits` come straight off the request body, and
// the cmd/bridge adapter substitutes the stored defaults only for
// EXACTLY 0 — so `8` or `-1` reached the coordinator, which rejects it
// with a plain error. The handler's only typed arm is disk space, so
// control fell to an unconditional `500 internal` logged at Error: any
// authed client could drive ERROR lines into the operator's log with a
// value it could not diagnose from the sanitized 500, and could not
// self-correct. Sibling handlers (upscaleRequest's kind, the variant
// delete query) 400 their own bad input.
func TestUpscaleBatchRejectsInvalidTargetWith400(t *testing.T) {
	tests := []struct {
		name        string
		body        BatchRequest
		wantStatus  int
		wantSubmits int
	}{
		{
			name:       "bit depth not in 16/24/32",
			body:       BatchRequest{TargetRate: 96000, TargetBits: 8},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "negative bit depth",
			body:       BatchRequest{TargetRate: 96000, TargetBits: -1},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "negative sample rate",
			body:       BatchRequest{TargetRate: -1, TargetBits: 24},
			wantStatus: http.StatusBadRequest,
		},
		{
			// 0 is the documented "use the bridge default" sentinel the
			// adapter substitutes for — it must keep reaching the
			// coordinator, not become a 400.
			name:        "omitted targets fall through to the defaults",
			body:        BatchRequest{},
			wantStatus:  http.StatusAccepted,
			wantSubmits: 1,
		},
		// All three accepted depths, so the client contract is pinned and
		// a narrowed switch can't silently start 400-ing 16 or 32.
		{
			name:        "valid explicit target 16-bit",
			body:        BatchRequest{TargetRate: 96000, TargetBits: 16},
			wantStatus:  http.StatusAccepted,
			wantSubmits: 1,
		},
		{
			name:        "valid explicit target 24-bit",
			body:        BatchRequest{TargetRate: 96000, TargetBits: 24},
			wantStatus:  http.StatusAccepted,
			wantSubmits: 1,
		},
		{
			name:        "valid explicit target 32-bit",
			body:        BatchRequest{TargetRate: 96000, TargetBits: 32},
			wantStatus:  http.StatusAccepted,
			wantSubmits: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hs, tok, stub := batchFixture(t)
			resp := postJSON(t, hs, "/v1/upscale/batch", tok, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if stub.submits != tc.wantSubmits {
				t.Errorf("coordinator submits = %d, want %d (a rejected "+
					"target must be caught before dispatch)",
					stub.submits, tc.wantSubmits)
			}
		})
	}
}

// `kind: "optimize"` documents both target fields as ignored — the
// coordinator derives a family-preserving target per track and never
// reads them. Validating them there would reject a request that
// succeeds today, so the guard is deliberately scoped to the upscale
// arm.
func TestUpscaleBatchOptimizeIgnoresTargetFields(t *testing.T) {
	hs, tok, stub := batchFixture(t)
	resp := postJSON(t, hs, "/v1/upscale/batch", tok,
		BatchRequest{Kind: "optimize", TargetRate: -1, TargetBits: 8})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202 (targets are documented as ignored for optimize)",
			resp.StatusCode)
	}
	if stub.optimizes != 1 {
		t.Errorf("optimize calls = %d, want 1", stub.optimizes)
	}
}
