package dlna

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// fakeServer is a MediaServer on loopback HTTP whose SSDP device registers,
// on en0's synthetic interface, with sub.
func fakeServer(t *testing.T, sub *SSDPSubsystem) (*Server, string) {
	t.Helper()
	addr := findFreePort(t)
	s, err := NewServer(ServerConfig{
		Library: newTestLib(testTrack("t1", "Test Track")), UDN: msUDN, FriendlyName: "Fake",
		ListenAddress: addr, ServerURL: "http://" + addr,
		AdvertiseEndpoints: []AdvertiseEndpoint{{Interface: en0, ServerURL: "http://192.168.1.20:7790"}},
		SSDP:               sub,
		Logger:             discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, addr
}

// TestTheServerRegistersWithTheSubsystemItIsGiven pins the injection
// cmd/bridge relies on (B470 PR 5a1): a Server given a subsystem registers
// its device there and builds none of its own; its Stop withdraws that
// device alone, and leaves the subsystem open for the others.
func TestTheServerRegistersWithTheSubsystemItIsGiven(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	s, _ := fakeServer(t, sub)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.SSDP() != sub || s.ssdpOwned {
		t.Fatalf("the server registered with %p (owned %v), want the subsystem it was given, %p", s.SSDP(), s.ssdpOwned, sub)
	}
	sub.mu.Lock()
	registered := len(sub.handles) == 1 && sub.handles[0] == s.ssdpHandle
	sub.mu.Unlock()
	if !registered {
		t.Fatal("the subsystem does not hold the server's registration")
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	sub.mu.Lock()
	closed, handles := sub.closed, len(sub.handles)
	sub.mu.Unlock()
	if closed || handles != 0 {
		t.Errorf("after Stop the subsystem is closed=%v with %d registrations, want open and empty", closed, handles)
	}
	if _, _, err := sub.Register(context.Background(), strictReg(srcUDN, 4, upEn0)); err != nil {
		t.Errorf("a device registered after the server stopped: %v", err)
	}
}

// TestAServerWithNoSubsystemBuildsAndClosesItsOwn pins the private instance:
// a Server given none builds one at Start and closes it at Stop.
func TestAServerWithNoSubsystemBuildsAndClosesItsOwn(t *testing.T) {
	s := startLoopbackDLNA(t, ServerConfig{}, nil)
	own := s.SSDP()
	if own == nil || !s.ssdpOwned {
		t.Fatalf("a server given no subsystem runs on %p (owned %v), want one of its own", own, s.ssdpOwned)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	_, _, err := own.Register(context.Background(), strictReg(srcUDN, 4, upEn0))
	mustErr(t, "Register on the stopped server's own subsystem", err, ErrSSDPClosed)
}

// TestServerStopWithdrawsSSDPBeforeHTTP pins Stop's order: the byebye goes
// out while the description its LOCATION names is still served, and only
// then does the HTTP listener shut down.
func TestServerStopWithdrawsSSDPBeforeHTTP(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	s, addr := fakeServer(t, sub)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var once sync.Once
	status := make(chan int, 1)
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{}}
	n.mu.Lock()
	n.onSenderWrite = func(_ *fakeSender, pkt string) {
		if header(pkt, "NTS") != "ssdp:byebye" {
			return
		}
		once.Do(func() {
			resp, err := client.Get("http://" + addr + "/dlna/description.xml")
			if err != nil {
				status <- 0
				return
			}
			_ = resp.Body.Close()
			status <- resp.StatusCode
		})
	}
	n.mu.Unlock()
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case got := <-status:
		if got != http.StatusOK {
			t.Errorf("while the byebye was being sent the description answered %d, want 200: HTTP was already down", got)
		}
	default:
		t.Fatal("Stop sent no byebye")
	}
}

// TestAByebyeGoesOutAfterTheParentContextIsCancelled pins the review's
// shutdown finding: serve cancels its context (SIGTERM) before any teardown
// runs, which stops every reader and every registration's work, and the
// MediaServer's Stop that follows still sends its whole byebye, as Close
// does for any registration left.
func TestAByebyeGoesOutAfterTheParentContextIsCancelled(t *testing.T) {
	n := newFakeNet()
	parent, cancel := context.WithCancel(context.Background())
	sub := NewSSDPSubsystem(parent, SSDPSubsystemOptions{Logger: discardLogger()})
	sub.transport = n
	sub.replyDelay = func(time.Duration) time.Duration { return 0 }
	s, _ := fakeServer(t, sub)
	if err := s.Start(parent); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mustRegister(t, sub, strictReg(srcUDN, 4, upEn2))
	cancel()
	if !waitUntil(time.Now().Add(5*time.Second), sub.readers.Wait) {
		t.Fatal("the readers did not stop on the parent's cancellation")
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := s.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := len(notifies(n, 0, "ssdp:byebye")); got != len(NotifyTargetsFor(msUDN)) {
		t.Errorf("the MediaServer's Stop after the cancellation sent %d byebyes, want its burst", got)
	}
	if err := sub.Close(stopCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(notifies(n, 0, "ssdp:byebye")); got != len(NotifyTargetsFor(msUDN))+len(sourceTargets(srcUDN)) {
		t.Errorf("Close after the cancellation sent %d byebyes in all, want the other device's too", got)
	}
}
