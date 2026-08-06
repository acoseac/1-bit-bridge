package dlna

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// Test_Server_ServeGoroutineUsesCapturedServerNotTheField pins the capture in
// Start's HTTP serve goroutine.
//
// The production sequence it reproduces: Start binds the listener, spawns the
// serve goroutine, then fails because no SSDP advertiser could bind (no
// multicast permission on the NIC). cmd/bridge/dlna_wiring.go answers that
// error with a defensive srv.Stop(...), which sets s.httpServer = nil — and
// the serve goroutine may not have been scheduled even once by then. A
// goroutine that read the FIELD instead of a captured local would call
// (*http.Server)(nil).Serve(...), which nil-derefs inside
// shouldConfigureHTTP2ForServe: a panic in a bare goroutine, so the whole
// bridge dies rather than DLNA degrading.
//
// The interleave is pinned with the serveGoroutineHookForTests seam rather
// than left to the scheduler — without it the goroutine almost always wins
// the race and the test would assert nothing. Reverting the capture (reading
// s.httpServer inside the goroutine) makes this test panic the test binary.
func Test_Server_ServeGoroutineUsesCapturedServerNotTheField(t *testing.T) {
	parked := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	rel := func() { releaseOnce.Do(func() { close(release) }) }

	// Written by the serve goroutine inside the hook, read by the test body
	// after <-parked — close(parked) is the happens-before edge.
	var exited <-chan struct{}

	serveGoroutineHookForTests = func(done <-chan struct{}) {
		exited = done
		close(parked)
		<-release
	}
	t.Cleanup(func() { serveGoroutineHookForTests = nil })
	// Registered last so it runs FIRST on teardown (t.Cleanup is LIFO): an
	// early t.Fatal must never leave the serve goroutine parked forever.
	t.Cleanup(rel)

	addr := findFreePort(t)
	s, err := NewServer(ServerConfig{
		Library:       newTestLib(),
		UDN:           "uuid:test-serve-goroutine-capture",
		ListenAddress: addr,
		ServerURL:     "http://" + addr,
		// A nonexistent interface makes every SSDP advertiser fail to bind,
		// which is the production trigger for "Start returns an error after
		// the serve goroutine was already spawned". The window under test is
		// created by the park + Stop below either way, so a host where the
		// bind unexpectedly succeeds still exercises it.
		AdvertiseEndpoints: []AdvertiseEndpoint{{
			Interface: &net.Interface{Index: 1 << 20, Name: "bridge-test-nonexistent0"},
			ServerURL: "http://" + addr,
		}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if startErr := s.Start(ctx); startErr != nil {
		t.Logf("Start failed on the SSDP path (the expected trigger): %v", startErr)
	}

	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("serve goroutine never reached the hook")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := s.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if s.httpServer != nil {
		t.Fatal("precondition: Stop must clear s.httpServer for this test to mean anything")
	}

	// The goroutine now proceeds to use whatever it holds. With the capture
	// that is the live *http.Server (Serve returns ErrServerClosed, since
	// Shutdown already ran); with a field read it is nil and the process dies.
	rel()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("serve goroutine never returned")
	}
}
