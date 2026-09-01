package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// fakeRegistrar records UpsertDeviceRegistration calls for assertions.
type fakeRegistrar struct {
	mu    sync.Mutex
	calls [][3]string                       // {deviceToken, tokenID, name}
	failN int                               // fail the next N calls (transient-error simulation)
	hook  func(deviceToken, tokenID string) // invoked at the top of each call (concurrency tests)
}

func (f *fakeRegistrar) UpsertDeviceRegistration(_ context.Context, deviceToken, tokenID, name string) error {
	if f.hook != nil {
		f.hook(deviceToken, tokenID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [3]string{deviceToken, tokenID, name})
	if f.failN > 0 {
		f.failN--
		return errors.New("transient")
	}
	return nil
}

func (f *fakeRegistrar) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestValidDeviceToken(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"deadbeef", true},
		{"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", true}, // 64-char iOS shape
		{"ABCDEF", false}, // uppercase rejected (hex.EncodeToString is lowercase)
		{"xyz123", false}, // non-hex
		{"dead beef", false},
		{string(make([]byte, maxDeviceTokenLen+1)), false}, // over-length (also non-hex)
	}
	for _, c := range cases {
		if got := validDeviceToken(c.in); got != c.want {
			t.Errorf("validDeviceToken(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestTouchDeviceDebounce verifies the in-memory debounce: a repeated
// (device,token) pair within the TTL hits the store once, while a bind
// change (new token for the same device) writes again immediately.
func TestTouchDeviceDebounce(t *testing.T) {
	fr := &fakeRegistrar{}
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	s := New(cfg, nil, nil, "fp").WithDeviceRegistrar(fr)

	ctx := context.Background()
	s.touchDevice(ctx, "dev1", "tok-a")
	s.touchDevice(ctx, "dev1", "tok-a") // debounced — same pair, within TTL
	s.touchDevice(ctx, "dev1", "tok-a")
	if got := fr.count(); got != 1 {
		t.Fatalf("same (device,token) within TTL: want 1 upsert, got %d", got)
	}

	// Bind change (re-pairing → new auth token for the same device) must
	// write immediately, bypassing the TTL.
	s.touchDevice(ctx, "dev1", "tok-b")
	if got := fr.count(); got != 2 {
		t.Fatalf("bind change: want 2 upserts, got %d", got)
	}

	// A different device is independent.
	s.touchDevice(ctx, "dev2", "tok-a")
	if got := fr.count(); got != 3 {
		t.Fatalf("new device: want 3 upserts, got %d", got)
	}

	// Header path always passes name="" — the store-side CASE guard
	// preserves any pairing-captured name.
	fr.mu.Lock()
	defer fr.mu.Unlock()
	for _, c := range fr.calls {
		if c[2] != "" {
			t.Errorf("header-path upsert sent non-empty name %q", c[2])
		}
	}
}

// TestTouchDeviceRetriesAfterTransientFailure pins the debounce-on-success
// contract: a failed upsert must NOT be cached, so the very next request
// retries (rather than waiting out the 5-minute TTL).
func TestTouchDeviceRetriesAfterTransientFailure(t *testing.T) {
	fr := &fakeRegistrar{failN: 1} // first call fails, second succeeds
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	s := New(cfg, nil, nil, "fp").WithDeviceRegistrar(fr)
	ctx := context.Background()

	s.touchDevice(ctx, "dev1", "tok-a") // fails — must not cache
	s.touchDevice(ctx, "dev1", "tok-a") // retries immediately
	if got := fr.count(); got != 2 {
		t.Fatalf("after transient failure want 2 upserts (retry), got %d", got)
	}
	// Third call (now cached after success) is debounced.
	s.touchDevice(ctx, "dev1", "tok-a")
	if got := fr.count(); got != 2 {
		t.Fatalf("after success want debounce (still 2), got %d", got)
	}
}

// TestTouchDeviceConcurrentStampedeGuard pins that concurrent first-hit
// requests for the same device fire the upsert exactly once: the first
// reserves the in-flight slot, the second observes it and skips.
func TestTouchDeviceConcurrentStampedeGuard(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fr := &fakeRegistrar{hook: func(_, _ string) {
		// Only the first (reserving) call reaches the registrar; signal it
		// has entered the upsert, then block until the test releases it.
		once.Do(func() { close(started) })
		<-release
	}}
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	s := New(cfg, nil, nil, "fp").WithDeviceRegistrar(fr)
	ctx := context.Background()

	done := make(chan struct{})
	go func() { s.touchDevice(ctx, "dev1", "tok-a"); close(done) }() // reserves inflight, blocks in hook
	<-started                                                        // first call is now inside the upsert
	s.touchDevice(ctx, "dev1", "tok-a")                              // sees inflight → returns without calling
	close(release)                                                   // let the first call finish
	<-done                                                           // first call recorded its single upsert

	if got := fr.count(); got != 1 {
		t.Fatalf("stampede guard: want exactly 1 upsert, got %d", got)
	}
}

// TestTouchDeviceConcurrentRebindNotSkipped pins that a concurrent rebind
// (same device, NEW token) is NOT swallowed by the in-flight guard while the
// old token's upsert is in flight — the bind change must still persist.
func TestTouchDeviceConcurrentRebindNotSkipped(t *testing.T) {
	startedA := make(chan struct{})
	releaseA := make(chan struct{})
	var onceA sync.Once
	fr := &fakeRegistrar{hook: func(_, tokenID string) {
		// Block only the first token's call; the rebind token passes through.
		if tokenID == "tok-a" {
			onceA.Do(func() { close(startedA) })
			<-releaseA
		}
	}}
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	s := New(cfg, nil, nil, "fp").WithDeviceRegistrar(fr)
	ctx := context.Background()

	done := make(chan struct{})
	go func() { s.touchDevice(ctx, "dev1", "tok-a"); close(done) }() // (dev1,tok-a) reserved + blocked
	<-startedA
	s.touchDevice(ctx, "dev1", "tok-b") // different key → must proceed, not skip
	close(releaseA)
	<-done

	if got := fr.count(); got != 2 {
		t.Fatalf("concurrent rebind: want 2 upserts (both tokens), got %d", got)
	}
}

// TestTouchDeviceClearsInflightAfterUpsertPanic pins the panic-safe
// cleanup: if UpsertDeviceRegistration panics (SQL driver / ctx panic),
// the deferred delete MUST still release the in-flight reservation during
// unwind — otherwise the (device,token) key stays wedged in deviceInflight
// for the process lifetime and every future registration for it is
// silently dropped by the inflight guard. Runs clean under -race (the
// deferred cleanup re-acquires the mutex).
func TestTouchDeviceClearsInflightAfterUpsertPanic(t *testing.T) {
	var mu sync.Mutex
	invocations := 0
	panicNext := true
	fr := &fakeRegistrar{hook: func(_, _ string) {
		mu.Lock()
		invocations++
		shouldPanic := panicNext
		panicNext = false
		mu.Unlock()
		if shouldPanic {
			panic("simulated SQL driver panic")
		}
	}}
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	s := New(cfg, nil, nil, "fp").WithDeviceRegistrar(fr)
	ctx := context.Background()

	// First call panics inside the upsert. In production the http
	// recoverer middleware catches this; emulate that here so we can
	// assert the aftermath. touchDevice's deferred cleanup runs during
	// the unwind, before the panic reaches this recover.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected touchDevice to propagate the upsert panic")
			}
		}()
		s.touchDevice(ctx, "dev1", "tok-a")
	}()

	// Same (device,token) again: the registrar MUST be reached a second
	// time, proving the in-flight slot was released on the panic path.
	// Pre-fix (delete not deferred) this call short-circuits on the
	// wedged inflight key and never invokes the registrar.
	s.touchDevice(ctx, "dev1", "tok-a")

	mu.Lock()
	got := invocations
	mu.Unlock()
	if got != 2 {
		t.Fatalf("registrar invocations = %d, want 2 (panic + retry); inflight key likely wedged", got)
	}

	// The successful retry cached the debounce entry, so a third call is a no-op.
	s.touchDevice(ctx, "dev1", "tok-a")
	mu.Lock()
	got = invocations
	mu.Unlock()
	if got != 2 {
		t.Fatalf("after success want debounce (invocations still 2), got %d", got)
	}
}

// TestReapDeviceSeen pins the bounded-memory contract of the
// deviceSeen reaper (2026-07-21 review, Low — entries were written
// but never deleted): entries at/past deviceRegistrarTTL are swept,
// fresh ones survive. The boundary case matches touchDevice's
// strict-< freshness check, so reaping never changes the upsert
// cadence — a reaped device simply re-upserts on its next request,
// exactly what a TTL-expired entry would do.
func TestReapDeviceSeen(t *testing.T) {
	fr := &fakeRegistrar{}
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	s := New(cfg, nil, nil, "fp").WithDeviceRegistrar(fr)

	now := time.Now()
	s.deviceSeenMu.Lock()
	s.deviceSeen["aa"] = deviceSeenEntry{tokenID: "tok-a", at: now.Add(-deviceRegistrarTTL - time.Second)} // stale
	s.deviceSeen["bb"] = deviceSeenEntry{tokenID: "tok-a", at: now.Add(-deviceRegistrarTTL)}               // boundary — stale per the strict-< check
	s.deviceSeen["cc"] = deviceSeenEntry{tokenID: "tok-a", at: now.Add(-deviceRegistrarTTL + time.Second)} // fresh
	dropped := s.reapDeviceSeen(now)
	s.deviceSeenMu.Unlock()

	if dropped != 2 {
		t.Errorf("reapDeviceSeen dropped %d entries, want 2 (stale + boundary)", dropped)
	}
	s.deviceSeenMu.Lock()
	_, aaGone := s.deviceSeen["aa"]
	_, bbGone := s.deviceSeen["bb"]
	_, ccKept := s.deviceSeen["cc"]
	s.deviceSeenMu.Unlock()
	if aaGone || bbGone {
		t.Errorf("stale entries survived (aa=%v bb=%v)", aaGone, bbGone)
	}
	if !ccKept {
		t.Error("fresh entry was reaped")
	}

	// Behaviour-preserving: the reaped device's next request re-upserts
	// immediately (same path a TTL-expired entry takes).
	s.touchDevice(context.Background(), "aa", "tok-a")
	if got := fr.count(); got != 1 {
		t.Fatalf("reaped device re-upsert: want 1 call, got %d", got)
	}
}

// TestStartDeviceSeenReaperLifecycle pins the lifecycle contract:
// start+stop exits cleanly with the registrar wired, and an unwired
// Server returns a no-op stopFn that is safe to call (mirrors
// StartTokenRateLimitReapers' nil path).
func TestStartDeviceSeenReaperLifecycle(t *testing.T) {
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}

	// Unwired: no registrar → no goroutine, no-op stop.
	unwired := New(cfg, nil, nil, "fp")
	unwired.StartDeviceSeenReaper()()

	// Wired: start then stop must return promptly (a wedged reaper
	// goroutine would hang the suite on -race via the leaked-goroutine
	// detectors in sibling tests).
	wired := New(cfg, nil, nil, "fp").WithDeviceRegistrar(&fakeRegistrar{})
	stop := wired.StartDeviceSeenReaper()
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reaper stopFn did not return within 2s")
	}
}
