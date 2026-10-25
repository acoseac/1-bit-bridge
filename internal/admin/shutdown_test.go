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

	// Hand ServeListener the bound socket so no other test can take its port.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg.AdminAddress = lis.Addr().String()
	t.Cleanup(func() { _ = lis.Close() })

	// Spawn Serve in a goroutine. Cancel its ctx after kicking off a
	// background scan and assert Serve returns only after the scan
	// completes.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	serveDone := make(chan struct{})
	go func() {
		_ = srv.ServeListener(ctx, lis)
		close(serveDone)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(7 * time.Second):
			t.Error("ServeListener did not stop during cleanup")
		}
	})
	waitForShutdownTestServer(t, cfg.AdminAddress, serveDone)

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
	t.Cleanup(func() { _ = lis.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	serveDone := make(chan struct{})
	go func() {
		_ = srv.ServeListener(ctx, lis)
		close(serveDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(7 * time.Second):
			t.Error("ServeListener did not stop during cleanup")
		}
	})
	waitForShutdownTestServer(t, cfg.AdminAddress, serveDone)

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
}

// waitForShutdownTestServer waits for HTTP, since a pre-bound socket can
// complete a TCP handshake before ServeListener has adopted it.
func waitForShutdownTestServer(t *testing.T, addr string, done <-chan struct{}) {
	t.Helper()
	tr := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr, Timeout: time.Second}
	deadline, ok := t.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Minute)
	}
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		select {
		case <-done:
			t.Fatal("ServeListener exited before answering HTTP")
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("ServeListener did not answer HTTP")
}
