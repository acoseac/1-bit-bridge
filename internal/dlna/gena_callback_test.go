package dlna

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
)

// genaSink is a callback listener that records every request it receives
// as "METHOD /path". answer, when set, writes the response; nil answers 200.
type genaSink struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newGENASinkOn(t *testing.T, ip string, answer http.HandlerFunc) *genaSink {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skipf("cannot listen on %s: %v", ip, err)
	}
	s := &genaSink{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		if answer != nil {
			answer(w, r)
		}
	}))
	s.Listener = l
	s.Start()
	t.Cleanup(s.Close)
	return s
}

func newGENASink(t *testing.T, answer http.HandlerFunc) *genaSink {
	t.Helper()
	return newGENASinkOn(t, "127.0.0.1", answer)
}

func (s *genaSink) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func (s *genaSink) port(t *testing.T) string {
	t.Helper()
	_, port, err := net.SplitHostPort(s.Listener.Addr().String())
	if err != nil {
		t.Fatalf("sink address: %v", err)
	}
	return port
}

// redirectTo answers every request with code and a Location of target.
func redirectTo(target string, code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target)
		w.WriteHeader(code)
	}
}

// subscribe drives the real GENA handler with a SUBSCRIBE whose TCP source
// is source and whose CALLBACK is callback, and waits for the initial NOTIFY
// it may send to finish.
func subscribe(t *testing.T, s *Server, source, callback string) {
	t.Helper()
	req := httptest.NewRequest("SUBSCRIBE", "/dlna/cds/event", nil)
	req.RemoteAddr = source
	req.Header.Set("CALLBACK", "<"+callback+">")
	req.Header.Set("NT", "upnp:event")
	rec := httptest.NewRecorder()
	s.genaHandler("cds")(rec, req)
	s.notifyWG.Wait()
	if rec.Code != http.StatusOK {
		t.Fatalf("SUBSCRIBE from %s answered %d, want 200", source, rec.Code)
	}
}

func liveGENAServer(t *testing.T) *Server {
	t.Helper()
	s := newGENATestServer(true)
	t.Cleanup(s.notifyCancel)
	return s
}

// TestGENANotifyNeverReachesThisHostForAnotherAddressesSubscribe is backlog
// B39's reproduction. The DLNA listener binds every interface, so any LAN
// peer can SUBSCRIBE, and a CALLBACK on a loopback address made the bridge
// send its initial NOTIFY to its own loopback services: the console on 7789
// is loopback-only and unauthenticated. The sink stands in for it. A
// loopback callback names the host that SENDS the NOTIFY, so only a
// subscriber at that same address can mean itself by it.
func TestGENANotifyNeverReachesThisHostForAnotherAddressesSubscribe(t *testing.T) {
	console := newGENASink(t, nil)
	port := console.port(t)
	s := liveGENAServer(t)
	for _, tc := range []struct{ name, source, callback string }{
		{"a LAN subscriber", "192.168.1.9:49152", "http://127.0.0.1:" + port + "/api/stats"},
		{"a LAN subscriber, the mapped spelling", "192.168.1.9:49152", "http://[::ffff:127.0.0.1]:" + port + "/api/stats"},
		{"a subscriber on a public address", "203.0.113.7:49152", "http://127.0.0.1:" + port + "/api/stats"},
		{"a link-local subscriber", "169.254.10.20:49152", "http://127.0.0.1:" + port + "/api/stats"},
		{"another loopback address", "127.0.0.2:49152", "http://127.0.0.1:" + port + "/api/stats"},
	} {
		before := len(console.requests())
		subscribe(t, s, tc.source, tc.callback)
		if got := console.requests()[before:]; len(got) != 0 {
			t.Errorf("%s (%s) with CALLBACK %s: the loopback listener saw %q", tc.name, tc.source, tc.callback, got)
		}
	}
}

// TestGENAHostLocalRefusalIsReportedOncePerPeer drives the report through
// the real handler: a refused loopback callback is one Warn naming both
// addresses, however often that peer renews, while a callback refused for
// being a name stays the Debug line it always was.
func TestGENAHostLocalRefusalIsReportedOncePerPeer(t *testing.T) {
	var buf bytes.Buffer
	s := liveGENAServer(t)
	s.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	sink := newGENASink(t, nil)
	for i := 0; i < 5; i++ {
		subscribe(t, s, "192.168.1.9:"+itoa(49152+i), sink.URL+"/api/stats")
	}
	subscribe(t, s, "192.168.1.9:49160", "http://example.com/evt")
	out := buf.String()
	if c := strings.Count(out, "GENA callback on this machine or a link-local address refused"); c != 1 {
		t.Errorf("want one refusal line for one peer, got %d:\n%s", c, out)
	}
	if !strings.Contains(out, "callbackHost=127.0.0.1") || !strings.Contains(out, "subscribeSource=192.168.1.9") {
		t.Errorf("the refusal must name the callback and the subscriber; got:\n%s", out)
	}
	if strings.Contains(out, "example.com") {
		t.Errorf("a name's refusal reached the Warn log; got:\n%s", out)
	}
	if got := sink.requests(); len(got) != 0 {
		t.Errorf("the loopback listener saw %q", got)
	}
}

// TestGENANotifyReachesThisHostWhenTheSubscribeCameFromIt is the positive
// twin: a control point on this host that subscribed over loopback gets its
// NOTIFY, by either spelling of its own address.
func TestGENANotifyReachesThisHostWhenTheSubscribeCameFromIt(t *testing.T) {
	sink := newGENASink(t, nil)
	s := liveGENAServer(t)
	subscribe(t, s, "127.0.0.1:49152", sink.URL+"/evt")
	subscribe(t, s, "127.0.0.1:49153", "http://[::ffff:127.0.0.1]:"+sink.port(t)+"/evt")
	want := []string{"NOTIFY /evt", "NOTIFY /evt"}
	if got := sink.requests(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the sink saw %q, want %q", got, want)
	}
}

// TestGENAInitialNotifyFollowsNoRedirect pins what the callback rule never
// looked at: the callback's ANSWER. Go's client follows a redirect by
// default, re-sending a NOTIFY to a 307/308 Location and turning a
// 301/302/303 into a GET, which the console's csrfGuard passes. The
// callback here is the subscriber's own address, which every rule admits.
func TestGENAInitialNotifyFollowsNoRedirect(t *testing.T) {
	s := liveGENAServer(t)
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		if got := redirectedNotify(t, s, code); len(got) != 0 {
			t.Errorf("%d: the redirect target saw %q: the NOTIFY followed the callback's redirect", code, got)
		}
	}
}

// redirectedNotify subscribes from loopback with a loopback callback that
// answers code with a Location on another loopback listener, and returns
// what that listener saw: nothing, unless the NOTIFY followed the redirect.
func redirectedNotify(t *testing.T, s *Server, code int) []string {
	t.Helper()
	target := newGENASink(t, nil)
	redirector := newGENASink(t, redirectTo(target.URL+"/api/stats", code))
	subscribe(t, s, "127.0.0.1:49152", redirector.URL+"/evt")
	if got := redirector.requests(); len(got) != 1 || got[0] != "NOTIFY /evt" {
		t.Fatalf("%d: the callback saw %q, want one NOTIFY /evt", code, got)
	}
	return target.requests()
}

// TestGENASubscriberCannotRedirectTheNotifyOntoThisHost is the redirect as a
// LAN peer uses it: its own address is its CALLBACK (so the callback rule
// holds, and would under #818's narrower predicate too), and it answers the
// NOTIFY with a redirect to the bridge's loopback. The host's own LAN
// address stands in for the peer's.
func TestGENASubscriberCannotRedirectTheNotifyOntoThisHost(t *testing.T) {
	lan := hostLANIPv4(t)
	console := newGENASink(t, nil)
	peer := newGENASinkOn(t, lan, redirectTo(console.URL+"/api/stats", http.StatusFound))
	if c, err := net.Dial("tcp", peer.Listener.Addr().String()); err != nil {
		t.Skipf("this host cannot connect to its own address %s: %v", lan, err)
	} else {
		_ = c.Close()
	}
	s := liveGENAServer(t)
	subscribe(t, s, net.JoinHostPort(lan, "49152"), peer.URL+"/evt")
	if got := peer.requests(); len(got) != 1 {
		t.Fatalf("the peer saw %q, want its one NOTIFY: the fixture did not reach the redirect", got)
	}
	if got := console.requests(); len(got) != 0 {
		t.Errorf("the loopback listener saw %q, on a LAN peer's redirect", got)
	}
}

// TestGENANotifyClientChecksTheConnectAgainstTheSubscriber pins the second
// check on its own: the notify client refuses a connect to this machine
// unless the request's context carries that address as the SUBSCRIBE's
// source. The handler's own check refuses the same callbacks first, so no
// handler-level test can see this one missing.
func TestGENANotifyClientChecksTheConnectAgainstTheSubscriber(t *testing.T) {
	sink := newGENASink(t, nil)
	c := newNotifyClient()
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		reaches bool
	}{
		{"a LAN subscriber", discovery.WithDialApproval(context.Background(), discovery.SubscribedFrom(netip.MustParseAddr("192.168.1.9"))), false},
		{"no subscriber", context.Background(), false},
		{"the loopback subscriber", discovery.WithDialApproval(context.Background(), discovery.SubscribedFrom(netip.MustParseAddr("127.0.0.1"))), true},
	} {
		before := len(sink.requests())
		req, err := http.NewRequestWithContext(tc.ctx, "NOTIFY", sink.URL+"/evt", strings.NewReader("<e:propertyset/>"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		if reached := len(sink.requests()) > before; reached != tc.reaches {
			t.Errorf("%s: the loopback listener reached = %v (err %v), want %v", tc.name, reached, err, tc.reaches)
		}
	}
}

// TestGENASubscribeOnAnUnstartedServerSendsNothing: a handler tree mounted
// without Start, as mountedMux mounts it, has no notify context. A SUBSCRIBE
// through it still answers 200 and sends nothing, as it did when the nil
// context failed NewRequestWithContext; wrapping a nil context in the
// SUBSCRIBE's source would panic instead.
func TestGENASubscribeOnAnUnstartedServerSendsNothing(t *testing.T) {
	sink := newGENASink(t, nil)
	mux := mountedMux(t, ServerConfig{Library: newTestLib()})
	req := httptest.NewRequest("SUBSCRIBE", "/dlna/cds/event", nil)
	req.RemoteAddr = "127.0.0.1:49152"
	req.Header.Set("CALLBACK", "<"+sink.URL+"/evt>")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("SUBSCRIBE answered %d, want 200", rec.Code)
	}
	if got := sink.requests(); len(got) != 0 {
		t.Errorf("an unstarted server sent %q", got)
	}
}

// hostLANIPv4 returns a non-loopback, non-link-local IPv4 address of this
// host, or skips.
func hostLANIPv4(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("interfaces: %v", err)
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			return ip4.String()
		}
	}
	t.Skip("no non-loopback IPv4 address on this host")
	return ""
}

// TestStartSendsTheNotifyThroughTheCheckedClient drives Start's own wiring
// (a Start that fails on its port still builds the notify state first), so
// a Start that went back to a plain client fails here even though every
// other GENA test builds its server by hand.
func TestStartSendsTheNotifyThroughTheCheckedClient(t *testing.T) {
	s := failedStartServer(t, "uuid:test-notify-client")
	// The failed Start cancelled its notify context; hand the handler a live
	// one, and keep the client Start built.
	s.notifyCtx, s.notifyCancel = context.WithCancel(context.Background())
	t.Cleanup(s.notifyCancel)
	if got := redirectedNotify(t, s, http.StatusFound); len(got) != 0 {
		t.Errorf("Start's notify client followed the callback's redirect to %q", got)
	}
}
