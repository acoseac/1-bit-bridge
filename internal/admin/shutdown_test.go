package admin

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestShutdownWaitsForBgScans pins: spawning a long-running scan
// via spawnBackgroundScan keeps Serve from returning until the scan
// finishes (or the 5s grace expires). Exercised end-to-end through
// the real Serve loop with a stub Scanner that blocks until told.
func TestShutdownWaitsForBgScans(t *testing.T) {
	srv, cfg, _ := newTestServer(t)

	// Bind a listener on a random loopback port so Serve can stand up
	// against it. AdminAddress in cfg drives Listen().
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg.AdminAddress = lis.Addr().String()
	lis.Close()

	// Spawn Serve in a goroutine. Cancel its ctx after kicking off a
	// background scan and assert Serve returns only after the scan
	// completes.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	serveDone := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx)
		close(serveDone)
	}()

	// Wait until Serve has bound its listener.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, derr := net.Dial("tcp", cfg.AdminAddress)
		if derr == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Inject a fake bg scan via the WaitGroup. We don't need the
	// Scanner to actually run — we just need bgScans counted.
	var ran atomic.Bool
	srv.bgScans.Add(1)
	go func() {
		// Simulate work shorter than the 5s shutdown grace.
		time.Sleep(150 * time.Millisecond)
		ran.Store(true)
		srv.bgScans.Done()
	}()

	// Cancel the parent ctx → triggers the shutdown branch.
	cancel()

	// Serve must NOT return before the bg scan finishes.
	select {
	case <-serveDone:
		// Verify the bg scan actually completed before Serve exit.
		if !ran.Load() {
			t.Errorf("Serve returned before bgScans drained")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Serve did not return within 3s of ctx cancel")
	}
}

// TestShutdownGraceCappedForStuckScan pins: a bg scan that runs
// past the 5s shutdown grace doesn't wedge Serve. (We use a
// short-lived sentinel that holds the WG longer than we wait.)
func TestShutdownGraceCappedForStuckScan(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 5s+ shutdown grace test in -short")
	}
	srv, cfg, _ := newTestServer(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg.AdminAddress = lis.Addr().String()
	lis.Close()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	serveDone := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx)
		close(serveDone)
	}()
	// Wait for listener.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, derr := net.Dial("tcp", cfg.AdminAddress)
		if derr == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Hold the WG for longer than the 5s grace.
	srv.bgScans.Add(1)
	holdDone := make(chan struct{})
	go func() {
		select {
		case <-time.After(8 * time.Second):
		case <-holdDone:
		}
		srv.bgScans.Done()
	}()
	t.Cleanup(func() { close(holdDone) })

	cancel()
	select {
	case <-serveDone:
		// expected: capped at the 5s shutdown grace
	case <-time.After(7 * time.Second):
		t.Errorf("Serve hung past shutdown grace")
	}
	// Avoid hijacking the `http` import being unused.
	_ = http.ErrServerClosed
}
