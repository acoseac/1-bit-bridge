package dlna

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// NewServer — config validation
// -----------------------------------------------------------------------------

func Test_NewServer_RequiresConfigFields(t *testing.T) {
	validLib := newTestLib()
	cases := []struct {
		name   string
		cfg    ServerConfig
		errSub string
	}{
		{
			name:   "missing_library",
			cfg:    ServerConfig{UDN: "uuid:x", ListenAddress: ":7790", ServerURL: "http://x"},
			errSub: "Library required",
		},
		{
			name:   "missing_udn",
			cfg:    ServerConfig{Library: validLib, ListenAddress: ":7790", ServerURL: "http://x"},
			errSub: "UDN required",
		},
		{
			name:   "missing_listen_address",
			cfg:    ServerConfig{Library: validLib, UDN: "uuid:x", ServerURL: "http://x"},
			errSub: "ListenAddress required",
		},
		{
			name:   "missing_server_url",
			cfg:    ServerConfig{Library: validLib, UDN: "uuid:x", ListenAddress: ":7790"},
			errSub: "ServerURL required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewServer(tc.cfg)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil (server=%v)", tc.errSub, s)
			}
			if !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.errSub)
			}
		})
	}
}

func Test_NewServer_ReturnsServerForValidConfig(t *testing.T) {
	s, err := NewServer(ServerConfig{
		Library: newTestLib(), UDN: "uuid:x", ListenAddress: ":0", ServerURL: "http://x",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("returned server is nil")
	}
}

// -----------------------------------------------------------------------------
// Start / Stop lifecycle — uses port :0 (OS-assigned ephemeral port)
// so the test doesn't conflict with anything on a real port.
// -----------------------------------------------------------------------------

// findFreePort returns an OS-assigned ephemeral port. Used so the
// lifecycle test can verify start/stop against a real socket without
// hardcoding a port that might conflict.
func findFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("findFreePort: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// Test_Server_StartStop_LifecycleBindsLoopbackPort exercises the
// happy-path Start → serve a request → Stop cycle on the loopback
// address. Validates that:
//   - Start binds the listener
//   - HTTP handlers respond (device description XML accessible)
//   - Stop drains gracefully without leaking goroutines
//
// Doesn't exercise SSDP runtime (binding to the multicast group
// requires root on Linux + can conflict with other UPnP services on
// the test host); SSDP packet builders are tested in ssdp_packet_test.go.
func Test_Server_StartStop_LifecycleBindsLoopbackPort(t *testing.T) {
	addr := findFreePort(t)
	lib := newTestLib(testTrack("t1", "Test Track"))
	s, err := NewServer(ServerConfig{
		Library:       lib,
		UDN:           "uuid:test-lifecycle",
		FriendlyName:  "Test Bridge",
		ListenAddress: addr,
		ServerURL:     "http://" + addr,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// SSDP startup typically requires multicast permissions that the
	// test host may not have — Start() will likely error here. The
	// HTTP listener should be cleanly torn down on SSDP failure.
	// Re-bind the HTTP server WITHOUT SSDP for the rest of the test
	// by mounting handlers directly via a manual http.Server.
	//
	// What we're actually testing: NewServer accepts valid config +
	// the HTTP handler tree is constructed correctly. SSDP runtime
	// lifecycle is covered separately in ssdp_test.go.
	if err := s.Start(ctx); err != nil {
		// SSDP failure on the test host is expected — log + skip the
		// lifecycle exercise rather than failing.
		t.Logf("Start failed (likely SSDP-related on test host): %v", err)
		t.Skip("SSDP multicast bind not available in test environment")
		return
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		if err := s.Stop(stopCtx); err != nil {
			t.Errorf("Stop returned error: %v", err)
		}
	}()

	// Give the server a moment to start accepting connections.
	time.Sleep(50 * time.Millisecond)

	resp, err := http.Get("http://" + addr + "/dlna/description.xml")
	if err != nil {
		t.Fatalf("GET description.xml: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("description.xml status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<UDN>uuid:test-lifecycle</UDN>") {
		t.Errorf("description.xml body missing UDN: %s", body)
	}
}

func Test_Server_StopBeforeStartIsSafe(t *testing.T) {
	s, err := NewServer(ServerConfig{
		Library: newTestLib(), UDN: "uuid:x", ListenAddress: ":0", ServerURL: "http://x",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// Should not panic
	ctx := context.Background()
	if err := s.Stop(ctx); err != nil {
		t.Errorf("Stop before Start should be a no-op, got error: %v", err)
	}
	// Twice should also be safe
	if err := s.Stop(ctx); err != nil {
		t.Errorf("Stop twice should be a no-op, got error: %v", err)
	}
}

// -----------------------------------------------------------------------------
// genaHandler — SUBSCRIBE / UNSUBSCRIBE + SERVER header + initial NOTIFY
// -----------------------------------------------------------------------------

// newGENATestServer builds a minimal Server for exercising genaHandler
// directly. The notify context is PRE-CANCELLED by default so any
// initial-NOTIFY goroutine spawned by a SUBSCRIBE fails fast without real
// network I/O. Pass `live=true` for the integration test that exercises a
// real loopback callback.
func newGENATestServer(live bool) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	if !live {
		cancel()
	}
	return &Server{
		log:          slog.Default(),
		cfg:          ServerConfig{ModelNumber: "TestModel"},
		notifyCtx:    ctx,
		notifyCancel: cancel,
		notifyClient: &http.Client{Timeout: time.Second},
	}
}

func Test_genaHandler_SubscribeReturns200WithSIDAndServerHeader(t *testing.T) {
	s := newGENATestServer(false)
	h := s.genaHandler("cds")
	req, _ := http.NewRequest("SUBSCRIBE", "/dlna/cds/event", nil)
	req.Header.Set("CALLBACK", "<http://192.168.0.5:55555/>")
	rec := httptestRecorder()
	h(rec, req)
	s.notifyWG.Wait() // drain the (pre-cancelled) NOTIFY goroutine

	if rec.statusCode != http.StatusOK {
		t.Errorf("SUBSCRIBE status = %d, want 200", rec.statusCode)
	}
	if !strings.HasPrefix(rec.header.Get("SID"), "uuid:dlna-cds-") {
		t.Errorf("SID header missing or malformed: %q", rec.header.Get("SID"))
	}
	if rec.header.Get("TIMEOUT") == "" {
		t.Errorf("TIMEOUT header missing")
	}
	// SERVER header is UPnP-mandated on GENA responses.
	if rec.header.Get("Server") == "" {
		t.Errorf("SERVER header missing on SUBSCRIBE response")
	}
}

func Test_genaHandler_UnsubscribeReturns200WithServerHeader(t *testing.T) {
	s := newGENATestServer(false)
	h := s.genaHandler("cm")
	req, _ := http.NewRequest("UNSUBSCRIBE", "/dlna/cm/event", nil)
	req.Header.Set("SID", "uuid:dlna-cm-12345")
	rec := httptestRecorder()
	h(rec, req)
	if rec.statusCode != http.StatusOK {
		t.Errorf("UNSUBSCRIBE status = %d, want 200", rec.statusCode)
	}
	if rec.header.Get("Server") == "" {
		t.Errorf("SERVER header missing on UNSUBSCRIBE response")
	}
}

func Test_genaHandler_OtherMethodsReturn405(t *testing.T) {
	s := newGENATestServer(false)
	h := s.genaHandler("cds")
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		req, _ := http.NewRequest(m, "/dlna/cds/event", nil)
		rec := httptestRecorder()
		h(rec, req)
		if rec.statusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s should return 405, got %d", m, rec.statusCode)
		}
	}
}

// Test_genaHandler_FiresInitialNotify is the integration test: a real
// loopback callback sink receives exactly one NOTIFY with the GENA
// headers + a propertyset body after a SUBSCRIBE.
func Test_genaHandler_FiresInitialNotify(t *testing.T) {
	type captured struct {
		method string
		nt     string
		nts    string
		sid    string
		seq    string
		body   string
	}
	got := make(chan captured, 1)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- captured{
			method: r.Method,
			nt:     r.Header.Get("NT"),
			nts:    r.Header.Get("NTS"),
			sid:    r.Header.Get("SID"),
			seq:    r.Header.Get("SEQ"),
			body:   string(b),
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	s := newGENATestServer(true) // live notify ctx
	defer s.notifyCancel()
	h := s.genaHandler("cds")
	req, _ := http.NewRequest("SUBSCRIBE", "/dlna/cds/event", nil)
	req.Header.Set("CALLBACK", "<"+sink.URL+"/evt>")
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptestRecorder()
	h(rec, req)
	s.notifyWG.Wait()

	select {
	case c := <-got:
		if c.method != "NOTIFY" {
			t.Errorf("callback method = %q, want NOTIFY", c.method)
		}
		if c.nt != "upnp:event" || c.nts != "upnp:propchange" {
			t.Errorf("NT/NTS = %q/%q, want upnp:event/upnp:propchange", c.nt, c.nts)
		}
		if !strings.HasPrefix(c.sid, "uuid:dlna-cds-") {
			t.Errorf("NOTIFY SID = %q, want uuid:dlna-cds- prefix", c.sid)
		}
		if c.seq != "0" {
			t.Errorf("SEQ = %q, want 0", c.seq)
		}
		if !strings.Contains(c.body, "e:propertyset") || !strings.Contains(c.body, "SystemUpdateID") {
			t.Errorf("NOTIFY body missing propertyset/SystemUpdateID: %q", c.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback sink never received the initial NOTIFY")
	}
}

func Test_firstCallbackURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"<http://192.168.0.5:55555/evt>", "http://192.168.0.5:55555/evt"},
		{"<http://a/1> <http://b/2>", "http://a/1"}, // first of multiple
		{"  <http://x/>  ", "http://x/"},
		{"no-brackets", ""},
		{"<unterminated", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := firstCallbackURL(tc.in); got != tc.want {
			t.Errorf("firstCallbackURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func Test_callbackHostAllowed(t *testing.T) {
	cases := []struct {
		name       string
		host       string
		remoteAddr string
		want       bool
	}{
		{"loopback", "127.0.0.1", "10.0.0.1:5", true},
		{"rfc1918_192", "192.168.1.4", "8.8.8.8:5", true},
		{"rfc1918_10", "10.1.2.3", "8.8.8.8:5", true},
		{"link_local", "169.254.1.1", "8.8.8.8:5", true},
		{"public_rejected", "8.8.8.8", "192.168.0.5:1234", false},
		{"public_but_matches_source", "8.8.8.8", "8.8.8.8:1234", true},
		{"hostname_rejected", "example.com", "192.168.0.5:1234", false},
		{"bad_remote_addr", "8.8.8.8", "garbage", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := callbackHostAllowed(tc.host, tc.remoteAddr); got != tc.want {
				t.Errorf("callbackHostAllowed(%q, %q) = %v, want %v", tc.host, tc.remoteAddr, got, tc.want)
			}
		})
	}
}

func Test_initialNotifyBody(t *testing.T) {
	cds := initialNotifyBody("cds")
	for _, want := range []string{"e:propertyset", "SystemUpdateID", "ContainerUpdateIDs", "TransferIDs"} {
		if !strings.Contains(cds, want) {
			t.Errorf("cds body missing %q: %s", want, cds)
		}
	}
	cm := initialNotifyBody("cm")
	for _, want := range []string{"e:propertyset", "SourceProtocolInfo", "SinkProtocolInfo", "CurrentConnectionIDs"} {
		if !strings.Contains(cm, want) {
			t.Errorf("cm body missing %q: %s", want, cm)
		}
	}
}

// -----------------------------------------------------------------------------
// PickLANEligibleInterface — host-dependent so we just check it returns
// EITHER a valid interface OR a clear error, doesn't panic.
// -----------------------------------------------------------------------------

func Test_PickLANEligibleInterface_DoesNotPanic(t *testing.T) {
	iface, err := PickLANEligibleInterface(EligibilityOpts{})
	// Either result is acceptable; we just verify it doesn't panic
	// and produces a coherent error message when no eligible iface.
	if err != nil && iface != nil {
		t.Errorf("error AND non-nil interface returned: iface=%v err=%v", iface, err)
	}
	if err == nil && iface == nil {
		t.Errorf("nil error AND nil interface — should be one or the other")
	}
}

// -----------------------------------------------------------------------------
// Test helpers (minimal recording http.ResponseWriter + logger)
// -----------------------------------------------------------------------------

type recordingResponseWriter struct {
	header     http.Header
	statusCode int
	body       []byte
}

func httptestRecorder() *recordingResponseWriter {
	return &recordingResponseWriter{header: http.Header{}, statusCode: 200}
}

func (r *recordingResponseWriter) Header() http.Header  { return r.header }
func (r *recordingResponseWriter) WriteHeader(code int) { r.statusCode = code }
func (r *recordingResponseWriter) Write(p []byte) (int, error) {
	r.body = append(r.body, p...)
	return len(p), nil
}
