package api

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
)

func TestReachabilityCache_HealthyRootReachable(t *testing.T) {
	dir := t.TempDir()
	c := newReachabilityCache()
	got := c.probe(context.Background(), dir)
	if !got.Reachable {
		t.Errorf("Reachable = false for a real tempdir, want true")
	}
	if got.Reason != "" {
		t.Errorf("Reason = %q, want empty for healthy root", got.Reason)
	}
}

func TestReachabilityCache_MissingRootReportsNotMounted(t *testing.T) {
	c := newReachabilityCache()
	got := c.probe(context.Background(), filepath.Join(os.TempDir(), "definitely-not-a-real-mount-xyzzy-12345"))
	if got.Reachable {
		t.Fatal("Reachable = true for missing path, want false")
	}
	if got.Reason != "not_mounted" {
		t.Errorf("Reason = %q, want not_mounted", got.Reason)
	}
}

func TestReachabilityCache_HonorsTTL(t *testing.T) {
	dir := t.TempDir()
	c := newReachabilityCache()
	first := c.probe(context.Background(), dir)
	// Within TTL: result is reused even if the underlying state changes.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("rm tempdir: %v", err)
	}
	second := c.probe(context.Background(), dir)
	if !second.Reachable {
		t.Error("expected cached Reachable=true within TTL, got false")
	}
	if first.checkedAt != second.checkedAt {
		t.Error("cache should return the same entry within TTL")
	}
}

func TestMatchesRoot_SingleRootMode(t *testing.T) {
	root := t.TempDir()
	s := &Server{resolver: bridgefs.New([]string{root})}
	cases := []struct {
		clientPath string
		want       string
	}{
		{"", root},
		{"/", root},
		{".", root},
		{"some/file.flac", ""},
		{"/some/file.flac", ""},
	}
	for _, tc := range cases {
		got := s.matchesRoot(tc.clientPath)
		if got != tc.want {
			t.Errorf("matchesRoot(%q) = %q, want %q", tc.clientPath, got, tc.want)
		}
	}
}

func TestMatchesRoot_MultiRootMode(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	s := &Server{resolver: bridgefs.New([]string{a, b})}
	cases := []struct {
		clientPath string
		want       string
	}{
		{"", ""},                          // ambiguous, no match
		{"/", ""},                         //
		{filepath.Base(a), a},             // root basename → that root
		{filepath.Base(b), b},             // sibling root
		{filepath.Base(a) + "/sub", ""},   // descendant, not a root
		{"/" + filepath.Base(a) + "/", a}, // leading + trailing slashes normalize
		{"non-existent-root", ""},         // unknown root → no match
	}
	for _, tc := range cases {
		got := s.matchesRoot(tc.clientPath)
		if got != tc.want {
			t.Errorf("matchesRoot(%q) = %q, want %q", tc.clientPath, got, tc.want)
		}
	}
}

func TestMatchesRoot_NoRoots(t *testing.T) {
	s := &Server{resolver: bridgefs.New(nil)}
	if got := s.matchesRoot(""); got != "" {
		t.Errorf("matchesRoot on empty resolver = %q, want empty", got)
	}
}

func TestProbeAllRoots_MixedReachability(t *testing.T) {
	good := t.TempDir()
	missing := filepath.Join(os.TempDir(), "missing-root-zzz-9876")
	s := &Server{
		resolver:     bridgefs.New([]string{good, missing}),
		reachability: newReachabilityCache(),
	}
	roots := s.probeAllRoots(context.Background())
	if len(roots) != 2 {
		t.Fatalf("len(roots) = %d, want 2", len(roots))
	}
	if !roots[0].Reachable {
		t.Errorf("first root should be reachable, got %+v", roots[0])
	}
	if roots[0].Name != filepath.Base(good) {
		t.Errorf("first root name = %q, want %q", roots[0].Name, filepath.Base(good))
	}
	if roots[1].Reachable {
		t.Errorf("second root should NOT be reachable, got %+v", roots[1])
	}
	if roots[1].Reason != "not_mounted" {
		t.Errorf("second root reason = %q, want not_mounted", roots[1].Reason)
	}
}

// TestReachabilityProbe_ACancelledCallerCachesTheRealVerdict pins that a
// caller's cancellation never reaches the verdict every caller shares.
// probeLocked runs its stat under context.WithoutCancel(ctx), so a caller
// whose context is already cancelled still waits for the real stat, and the
// cache stores what that stat found. The second probe answers from that
// entry inside the TTL, so a cancellation that got through (the Done
// branch's "offline", cached) fails the assertion.
//
// Its premise changed with #373. It was written for #198, when the probe ran
// on the caller's context: the cancellation took the Done branch, and the
// test pinned that the "offline" it produced was returned but not cached.
// #373 detached the probe and dropped that exception, and the comments here
// went on describing it until 2026-09-28, under the name TimeoutRespected,
// though no timeout fires in this test.
func TestReachabilityProbe_ACancelledCallerCachesTheRealVerdict(t *testing.T) {
	c := newReachabilityCache()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller hung up before the probe started

	// A real directory, so the stat the probe runs regardless finds it.
	dir := t.TempDir()
	_ = c.probe(ctx, dir)

	got := c.probe(context.Background(), dir)
	if !got.Reachable {
		t.Errorf("a probe whose caller had already cancelled cached Reachable=false "+
			"for a reachable root; the caller's cancellation reached the shared verdict (%+v)", got)
	}
}

func TestReachabilityCache_NilReceiverIsSafe(t *testing.T) {
	// Test harnesses that construct &Server{...} without going through
	// New() must not panic when the reachability cache is nil — fall
	// open with Reachable=true. Production callers always go through
	// New() which initialises the cache.
	var c *reachabilityCache
	got := c.probe(context.Background(), "/anywhere")
	if !got.Reachable {
		t.Errorf("nil receiver should fall open, got Reachable=false")
	}
}

func TestReachabilityCache_SingleflightCollapsesConcurrentProbes(t *testing.T) {
	// 50 concurrent probers against the same missing path. Without the
	// singleflight collapse, each one would spawn its own probe
	// goroutine. After the storm, exactly one cache entry should be
	// present — the singleflight winner's classified result.
	missing := filepath.Join(os.TempDir(), "definitely-not-real-singleflight-test-zzz")
	c := newReachabilityCache()
	done := make(chan reachabilityStatus, 50)
	for i := 0; i < 50; i++ {
		go func() { done <- c.probe(context.Background(), missing) }()
	}
	for i := 0; i < 50; i++ {
		got := <-done
		if got.Reachable {
			t.Errorf("iteration %d: expected Reachable=false for missing path, got %+v", i, got)
		}
		if got.Reason != "not_mounted" {
			t.Errorf("iteration %d: reason = %q, want not_mounted", i, got.Reason)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) != 1 {
		t.Errorf("entries = %d, want 1 (singleflight should produce one shared cache entry)", len(c.entries))
	}
}

func TestProbeAllRoots_RunsInParallel(t *testing.T) {
	// Two unreachable roots. Sequential probing on a flaky NAS would
	// run O(N × probe_timeout); parallel is bounded by the slowest
	// single probe. ENOENT returns immediately so we can't time-assert
	// against the timeout floor in a stable way — instead just confirm
	// the call returns the structural shape (one entry per root).
	missing1 := filepath.Join(os.TempDir(), "definitely-not-real-parallel-test-1")
	missing2 := filepath.Join(os.TempDir(), "definitely-not-real-parallel-test-2")
	s := &Server{
		resolver:     bridgefs.New([]string{missing1, missing2}),
		reachability: newReachabilityCache(),
	}
	out := s.probeAllRoots(context.Background())
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
	for i, r := range out {
		if r.Reachable {
			t.Errorf("out[%d] should be unreachable: %+v", i, r)
		}
	}
}

func TestClassifyProbeResult(t *testing.T) {
	if got := classifyProbeResult(nil, os.ErrNotExist); got.Reachable || got.Reason != "not_mounted" {
		t.Errorf("ErrNotExist -> %+v, want not_mounted", got)
	}
	if got := classifyProbeResult(nil, fs.ErrPermission); got.Reachable || got.Reason != "permission_denied" {
		t.Errorf("ErrPermission -> %+v, want permission_denied", got)
	}
	if got := classifyProbeResult(nil, errors.New("nfs: stale handle")); got.Reachable || got.Reason != "offline" {
		t.Errorf("generic err -> %+v, want offline", got)
	}
}

func TestReachabilityCache_ConcurrentSafe(t *testing.T) {
	dir := t.TempDir()
	c := newReachabilityCache()
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				_ = c.probe(context.Background(), dir)
			}
		}()
	}
	deadline := time.After(5 * time.Second)
	for i := 0; i < 20; i++ {
		select {
		case <-done:
		case <-deadline:
			t.Fatal("timed out waiting for concurrent probers")
		}
	}
}
