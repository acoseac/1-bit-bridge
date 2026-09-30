package dlna

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// hostGuardServer is a Server whose configuration names a LOCATION host, a
// ServerURL host and a pinned-looking listen address, and whose interfaces
// are a stand-in list: 10.1.2.3 and fe80::1, neither of which the
// configuration names. calls counts the times the guard asked for the
// interface list. It is served through handler(), the tree Start serves,
// without the SSDP half Start needs multicast for.
func hostGuardServer(t *testing.T) (*Server, *httptest.Server, *atomic.Int32) {
	t.Helper()
	s, err := NewServer(ServerConfig{
		Library:       newTestLib(testTrack("t1", "Test Track")),
		UDN:           "uuid:test-host-guard",
		FriendlyName:  "Test Bridge",
		ListenAddress: ":7790",
		ServerURL:     "http://192.0.2.10:7790",
		AdvertiseEndpoints: []AdvertiseEndpoint{
			{ServerURL: "http://192.0.2.10:7790"},
			{ServerURL: "http://198.51.100.7:7790"},
			{ServerURL: "http://Bridge-Box.lan.:7790"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.interfaceAddrs = func() ([]net.Addr, error) {
		calls.Add(1)
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("10.1.2.3"), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
		}, nil
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return s, ts, &calls
}

// sendAs sends one request to the test server under the given Host.
func sendAs(t *testing.T, ts *httptest.Server, req *http.Request, host string) (int, string) {
	t.Helper()
	req.Host = host
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func descriptionRequest(t *testing.T, ts *httptest.Server) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/dlna/description.xml", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func browseRequest(t *testing.T, ts *httptest.Server) *http.Request {
	t.Helper()
	tmpl := buildBrowseRequest(t, allTracksObjectID, "BrowseDirectChildren", 0, 10)
	body, _ := io.ReadAll(tmpl.Body)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/dlna/cds/control", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = tmpl.Header.Clone()
	return req
}

// Test_ownHostOnly_RefusesAHostThatIsNotThisHosts is backlog B170 on the
// DLNA listener: a request that names a host other than this one is what a
// page on the LAN sends once it has pointed its own name at this host's
// address, and the listener has no other lock. It must be 421, and a Browse
// must not come back with the library, nor with <res> URLs built on the
// page's name.
func Test_ownHostOnly_RefusesAHostThatIsNotThisHosts(t *testing.T) {
	_, ts, _ := hostGuardServer(t)
	for _, host := range []string{
		"evil.example:7790",
		"evil.example",
		"192.0.2.10.nip.io:7790",   // a public name that resolves to the LOCATION address
		"bridge-box.lan.evil:7790", // the advertised name as a label
		"203.0.113.5:7790",         // an address that is not this host's
		"0.0.0.0:7790",
		"[::]:7790",
	} {
		if code, body := sendAs(t, ts, descriptionRequest(t, ts), host); code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q, description.xml: status %d, want 421", host, code)
		} else if strings.Contains(body, "uuid:test-host-guard") {
			t.Errorf("Host %q: the refusal carries the device description", host)
		}
		code, body := sendAs(t, ts, browseRequest(t, ts), host)
		if code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q, Browse: status %d, want 421", host, code)
		}
		for _, served := range []string{"Test Track", "/dlna/file/"} {
			if strings.Contains(body, served) {
				t.Errorf("Host %q, Browse: the refusal carries %q, which only a Browse serves", host, served)
			}
		}
	}
}

// Test_ownHostOnly_AnswersThisHostsAddresses is the other half: every host
// a renderer or control point can have been given is answered, and a Browse
// under it builds its <res> URLs on that same host, as before.
func Test_ownHostOnly_AnswersThisHostsAddresses(t *testing.T) {
	_, ts, _ := hostGuardServer(t)
	for _, host := range []string{
		"192.0.2.10:7790",   // ServerURL and the first LOCATION
		"192.0.2.10",        // no port
		"198.51.100.7:7790", // a second interface's LOCATION
		"bridge-box.lan:7790",
		"BRIDGE-BOX.LAN.:7790", // the advertised name, folded
		"10.1.2.3:7790",        // an address of this host no LOCATION names
		"[fe80::1]:7790",
		"[::ffff:10.1.2.3]:7790",
		"127.0.0.1:7790",
		"[::1]:7790",
		"localhost:7790",
	} {
		if code, _ := sendAs(t, ts, descriptionRequest(t, ts), host); code != http.StatusOK {
			t.Errorf("Host %q, description.xml: status %d, want 200", host, code)
		}
		code, body := sendAs(t, ts, browseRequest(t, ts), host)
		if code != http.StatusOK {
			t.Errorf("Host %q, Browse: status %d, want 200", host, code)
			continue
		}
		if want := "http://" + host + "/dlna/file/"; !strings.Contains(body, want) {
			t.Errorf("Host %q, Browse: no <res> built on %q", host, want)
		}
	}
}

// Test_ownHostOnly_AnswersARequestWithNoHost pins the HTTP/1.0 renderer:
// it may send no Host, and a Browse then builds its <res> URLs on the
// ServerURL, as it always has. No browser sends a request without one.
func Test_ownHostOnly_AnswersARequestWithNoHost(t *testing.T) {
	_, ts, _ := hostGuardServer(t)
	conn, err := net.DialTimeout("tcp", ts.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	tmpl := buildBrowseRequest(t, allTracksObjectID, "BrowseDirectChildren", 0, 10)
	body, _ := io.ReadAll(tmpl.Body)
	req := fmt.Sprintf("POST /dlna/cds/control HTTP/1.0\r\nSOAPAction: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n%s",
		tmpl.Header.Get("SOAPAction"), tmpl.Header.Get("Content-Type"), len(body), body)
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an HTTP/1.0 Browse with no Host: status %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(got), "http://192.0.2.10:7790/dlna/file/") {
		t.Errorf("an HTTP/1.0 Browse with no Host: <res> not built on the ServerURL")
	}
}

// Test_ownHostOnly_AsksTheInterfacesOnlyForAnUnnamedLiteral pins the cost:
// a renderer on a host the configuration names (its LOCATION, the
// ServerURL, loopback) and a name are judged without listing the
// interfaces; only a literal the configuration does not name is.
func Test_ownHostOnly_AsksTheInterfacesOnlyForAnUnnamedLiteral(t *testing.T) {
	_, ts, calls := hostGuardServer(t)
	for _, host := range []string{"192.0.2.10:7790", "198.51.100.7:7790", "bridge-box.lan:7790", "127.0.0.1:7790", "evil.example:7790", ""} {
		sendAs(t, ts, descriptionRequest(t, ts), host)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the interfaces were listed %d times for hosts the configuration names or names it does not, want 0", n)
	}
	sendAs(t, ts, descriptionRequest(t, ts), "10.1.2.3:7790")
	if n := calls.Load(); n != 1 {
		t.Errorf("the interfaces were listed %d times for one unnamed literal, want 1", n)
	}
}

// Test_ownHostOnly_LogsARefusedNameOnce pins noteForeignHost: one Warn per
// refused name, bounded by hostRefusedSeenCap.
func Test_ownHostOnly_LogsARefusedNameOnce(t *testing.T) {
	rec := loggingtest.Record(t)
	_, ts, _ := hostGuardServer(t)
	const msg = "DLNA refused a request that names another host"
	for i := 0; i < 3; i++ {
		sendAs(t, ts, descriptionRequest(t, ts), "evil.example:7790")
	}
	if n := len(rec.Failures(msg)); n != 1 {
		t.Fatalf("got %d refusal lines for one name, want 1", n)
	}
	for i := 0; i < 3*hostRefusedSeenCap; i++ {
		sendAs(t, ts, descriptionRequest(t, ts), fmt.Sprintf("rebind-%d.example:7790", i))
	}
	if n := len(rec.Failures(msg)); n != hostRefusedSeenCap {
		t.Errorf("got %d refusal lines, want the cap %d", n, hostRefusedSeenCap)
	}
}

// Test_Server_Start_ServesTheHostCheck pins the wiring: Start serves the
// tree handler() builds, so the listener it binds refuses a foreign Host.
// Its advertiser is bound to the loopback interface, as the lifecycle
// test's is, and it skips where multicast cannot be pinned there.
func Test_Server_Start_ServesTheHostCheck(t *testing.T) {
	addr := findFreePort(t)
	s, err := NewServer(ServerConfig{
		Library:       newTestLib(testTrack("t1", "Test Track")),
		UDN:           "uuid:test-host-guard-start",
		FriendlyName:  "Test Bridge",
		ListenAddress: addr,
		ServerURL:     "http://" + addr,
		AdvertiseEndpoints: []AdvertiseEndpoint{{
			Interface: loopbackInterface(t),
			ServerURL: "http://" + addr,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Skipf("Start failed (SSDP on this host): %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = s.Stop(stopCtx)
	}()
	get := func(host string) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/dlna/description.xml", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("evil.example"); code != http.StatusMisdirectedRequest {
		t.Errorf("Start's listener, Host evil.example: status %d, want 421", code)
	}
	if code := get(addr); code != http.StatusOK {
		t.Errorf("Start's listener, Host %s: status %d, want 200", addr, code)
	}
}
