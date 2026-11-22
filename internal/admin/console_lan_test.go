package admin

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/consolelan"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The console on the home network (backlog B989), through Handler(): a
// request stamped with the home-network socket's description, as its
// server's BaseContext stamps every request it carries, and the RemoteAddr
// the test names. These prove the guards and nothing about interfaces,
// which internal/consolelan's tests qualify on real sockets (D13).

const (
	testLANAddr = "192.168.77.20"
	testLANNet  = "192.168.77.0/24"
	testLANHost = "192.168.77.20:7789"
	testLANPeer = "192.168.77.5:51000"
)

// testLANPolicy is a home-network socket's policy, as a reading of A on
// its interface would make it: A, its network, P and the .local name it
// answers to ("" for none).
func testLANPolicy(t *testing.T, addr, prefix string, port int, localName string) *lanPolicy {
	t.Helper()
	a := netip.MustParseAddr(addr)
	class, _ := config.ConsoleLANClass(a)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &lanPolicy{
		addr: a, port: port, class: class, hold: consolelan.HoldBindToIfindex, ctx: ctx, cancel: cancel,
		parts: lanPolicyParts{Index: 7, Name: "en-test", Flags: net.FlagUp | net.FlagRunning | net.FlagBroadcast | net.FlagMulticast,
			Prefix: netip.MustParsePrefix(prefix).Masked(), LocalName: localName},
	}
}

// onLAN stamps r as a request the home-network socket p carries.
func onLAN(r *http.Request, p *lanPolicy) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), socketDescKey{}, socketDesc{kind: socketLAN, policy: p}))
}

// lanRequest is a request on p's socket from remote with Host host.
func lanRequest(p *lanPolicy, method, target, remote, host string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.RemoteAddr, r.Host = remote, host
	return onLAN(r, p)
}

// loopbackRequest is a request on the loopback socket from 127.0.0.1.
func loopbackRequest(method, target, host string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.RemoteAddr, r.Host = "127.0.0.1:51000", host
	return r
}

func serveTo(srv *Server, r *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, r)
	return rr
}

// TestTheLANSocketAdmitsOnlyItsOwnNetwork holds the source policy (the
// plan's §4.1, R1, I9): a peer inside both A's on-link network and A's
// private class, unmapped and without a zone, the box's own address among
// them; nothing outside either, nothing in Tailscale's ranges whatever the
// prefix (a broad ULA prefix that holds Tailscale's), and nothing whose
// address does not read.
func TestTheLANSocketAdmitsOnlyItsOwnNetwork(t *testing.T) {
	srv, _, _ := newTestServer(t)
	v4 := testLANPolicy(t, testLANAddr, testLANNet, 7789, "")
	ula := testLANPolicy(t, "fd12::20", "fd00::/8", 7789, "")
	wide := testLANPolicy(t, "fd12::20", "fc00::/7", 7789, "")
	for _, c := range []struct {
		name   string
		p      *lanPolicy
		remote string
		host   string
		admit  bool
	}{
		{"a neighbour", v4, "192.168.77.5:51000", testLANHost, true},
		{"the box itself", v4, "192.168.77.20:51000", testLANHost, true},
		{"a neighbour in its IPv4-mapped spelling", v4, "[::ffff:192.168.77.5]:51000", testLANHost, true},
		{"another network of the class", v4, "192.168.78.5:51000", testLANHost, false},
		{"another private class", v4, "10.0.0.5:51000", testLANHost, false},
		{"loopback", v4, "127.0.0.1:51000", testLANHost, false},
		{"Tailscale's IPv4 range", v4, "100.64.0.5:51000", testLANHost, false},
		{"a public address", v4, "8.8.8.8:51000", testLANHost, false},
		{"an address that does not read", v4, "garbage", testLANHost, false},
		{"a ULA neighbour", ula, "[fd12::99]:51000", "[fd12::20]:7789", true},
		{"a ULA elsewhere in the broad prefix", ula, "[fd34::1]:51000", "[fd12::20]:7789", true},
		{"Tailscale's ULA inside the broad prefix", ula, "[fd7a:115c:a1e0::5]:51000", "[fd12::20]:7789", false},
		{"Tailscale's ULA inside fc00::/7 itself", wide, "[fd7a:115c:a1e0::5]:51000", "[fd12::20]:7789", false},
		{"a zoned peer", ula, "[fe80::1%25eth0]:51000", "[fd12::20]:7789", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rr := serveTo(srv, lanRequest(c.p, http.MethodGet, "/api/console", c.remote, c.host, nil))
			if c.admit && rr.Code != http.StatusOK {
				t.Fatalf("admitted source answered %d: %s", rr.Code, rr.Body)
			}
			if !c.admit && rr.Code != http.StatusForbidden {
				t.Fatalf("refused source answered %d: %s", rr.Code, rr.Body)
			}
		})
	}
}

// hostRows are §4.2's Host table: the answer on the loopback socket, and on
// a LAN socket at P = 7789 and at P = 80, whose .local name is box.local.
var hostRows = []struct {
	host            string
	loop, lan, at80 int
}{
	{"", 200, 421, 421},
	{"localhost", 200, 421, 421},
	{"localhost.", 200, 421, 421},
	{"127.0.0.1", 200, 421, 421},
	{"[::1]", 200, 421, 421},
	{"localhost:7789", 200, 421, 421},
	{"127.0.0.1:1234", 200, 421, 421},
	{"[::ffff:127.0.0.1]:7789", 200, 421, 421},
	{"192.168.77.20:7789", 421, 200, 421},
	{"192.168.77.20", 421, 421, 200},
	{"192.168.77.20:", 421, 421, 200},
	{"192.168.77.20:80", 421, 421, 200},
	{"192.168.77.20:7790", 421, 421, 421},
	{"192.168.77.20:07789", 421, 421, 421},
	{"box.local:7789", 421, 200, 421},
	{"BOX.Local:7789", 421, 200, 421},
	{"box.local.:7789", 421, 200, 421},
	{"box.local..:7789", 421, 421, 421},
	{"box.local", 421, 421, 200},
	{"box.local:80", 421, 421, 200},
	{"box.local:7790", 421, 421, 421},
	{"192.168.77.21:7789", 421, 421, 421},
	{"x.example:7789", 421, 421, 421},
	{"[fd12::20]:7789", 421, 421, 421},
	{"[fe80::1%25eth0]:7789", 421, 421, 421},
	{"[::ffff:192.168.77.20]:7789", 421, 421, 421},
	{"3232255252:7789", 421, 421, 421},
	{"192.168.077.20:7789", 421, 421, 421},
}

// hostProbe is the route the Host and spelling tables ask through: one main
// serves, so the loopback column answers at main as it does after.
const hostProbe = "/api/stats"

// TestEachHostIsAnsweredAsItsSocketsRuleSays holds both columns of §4.2's
// table (R6): the loopback socket's rule as it was, and the LAN socket's,
// which takes exactly its authority, A with P or the .local name with P.
func TestEachHostIsAnsweredAsItsSocketsRuleSays(t *testing.T) {
	srv, _, _ := newTestServer(t)
	lan := testLANPolicy(t, testLANAddr, testLANNet, 7789, "box.local")
	at80 := testLANPolicy(t, testLANAddr, testLANNet, 80, "box.local")
	for _, c := range hostRows {
		if got := serveTo(srv, loopbackRequest(http.MethodGet, hostProbe, c.host, nil)).Code; got != c.loop {
			t.Errorf("loopback socket, Host %q: %d, want %d", c.host, got, c.loop)
		}
		if got := serveTo(srv, lanRequest(lan, http.MethodGet, hostProbe, testLANPeer, c.host, nil)).Code; got != c.lan {
			t.Errorf("LAN socket at 7789, Host %q: %d, want %d", c.host, got, c.lan)
		}
		if got := serveTo(srv, lanRequest(at80, http.MethodGet, hostProbe, testLANPeer, c.host, nil)).Code; got != c.at80 {
			t.Errorf("LAN socket at 80, Host %q: %d, want %d", c.host, got, c.at80)
		}
	}
	ula := testLANPolicy(t, "fd12::20", "fd12::/64", 7789, "")
	for host, want := range map[string]int{
		"[fd12::20]:7789":          200,
		"[fd12:0:0::20]:7789":      200,
		"[FD12::20]:7789":          200,
		"fd12::20:7789":            421,
		"[fd12::20%25eth0]:7789":   421,
		"192.168.77.20:7789":       421,
		"[fd12::21]:7789":          421,
		"[::ffff:192.168.77.20]:0": 421,
	} {
		if got := serveTo(srv, lanRequest(ula, http.MethodGet, hostProbe, "[fd12::99]:51000", host, nil)).Code; got != want {
			t.Errorf("ULA socket, Host %q: %d, want %d", host, got, want)
		}
	}
	// I10: a socket that answers to no .local name, its responder not
	// running, refuses the name.
	noName := testLANPolicy(t, testLANAddr, testLANNet, 7789, "")
	if got := serveTo(srv, lanRequest(noName, http.MethodGet, hostProbe, testLANPeer, "box.local:7789", nil)).Code; got != 421 {
		t.Errorf("a socket with no .local name answered Host box.local:7789 with %d, want 421", got)
	}
}

// originRows are §4.3's table: the answer on the loopback socket and on the
// LAN socket at P = 7789 (a pass, or 403 cross_origin), and at P = 80.
var originRows = []struct {
	origin          string
	loop, lan, at80 bool
}{
	{"", true, true, true},
	{"null", false, false, false},
	{"http://127.0.0.1:7789", true, false, false},
	{"http://localhost:7789", true, false, false},
	{"http://[::1]:7789", true, false, false},
	{"http://[::ffff:127.0.0.1]:7789", true, false, false},
	{"http://localhost:17789", false, false, false},
	{"http://192.168.77.20:7789", false, true, false},
	{"http://192.168.77.20", false, false, true},
	{"http://192.168.77.20:80", false, false, true},
	{"https://192.168.77.20:7789", false, false, false},
	{"http://192.168.77.20:7790", false, false, false},
	{"http://box.local:7789", false, true, false},
	{"http://BOX.local.:7789", false, true, false},
	{"http://box.local", false, false, true},
	{"http://box.local:80", false, false, true},
	{"http://[::ffff:192.168.77.20]:7789", false, false, false},
	{"http://u@192.168.77.20:7789", false, false, false},
	{"http://192.168.77.20:7789/", false, false, false},
	{"http://192.168.77.20:7789?x", false, false, false},
	{"http://192.168.77.20:7789#f", false, false, false},
	{"http://192.168.77.21:7789", false, false, false},
	{"https://x.example", false, false, false},
}

// originKinds are the requests §4.3 asks the Origin of: a PATCH with a
// body, a bodyless POST, an upload chunk's PUT and the event stream.
var originKinds = []struct {
	name, method, target, ctype, body string
}{
	{"a PATCH", http.MethodPatch, "/api/settings", "application/json", "{}"},
	{"a bodyless POST", http.MethodPost, "/logout", "", ""},
	{"an upload chunk", http.MethodPut, "/api/upload/sessions/s/files/f?offset=0", "application/octet-stream", "x"},
	{"the event stream", http.MethodGet, "/api/events", "", ""},
}

// originPassed reports whether rr passed the console's boundary, which
// refuses in three shapes: the Host's 421, the source's 403 ("admin refused:
// ..."), and the Origin's 403 cross_origin. A route's own answer past it, an
// upload chunk's 403 among them, is a pass. A row's socket and Host pass the
// boundary, so after the change a row it refuses is refused for its Origin;
// TestEachRefusalSaysWhereToOpenTheConsole holds the words.
func originPassed(rr *httptest.ResponseRecorder) bool {
	if rr.Code == http.StatusMisdirectedRequest {
		return false
	}
	if rr.Code != http.StatusForbidden {
		return true
	}
	if strings.HasPrefix(rr.Body.String(), "admin refused:") {
		return false
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return body.Error != errCodeCrossOrigin
}

// originRequest is kind's request with origin on the socket given.
func originRequest(kind int, p *lanPolicy, host, origin string) *http.Request {
	k := originKinds[kind]
	var body io.Reader
	if k.body != "" {
		body = strings.NewReader(k.body)
	}
	var r *http.Request
	if p != nil {
		r = lanRequest(p, k.method, k.target, testLANPeer, host, body)
	} else {
		r = loopbackRequest(k.method, k.target, host, body)
	}
	if k.ctype != "" {
		r.Header.Set("Content-Type", k.ctype)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if k.target == "/api/events" {
		// The stream ends at its context: one already ended returns after
		// the headers, or the gate's refusal.
		ctx, cancel := context.WithCancel(r.Context())
		cancel()
		r = r.WithContext(ctx)
	}
	return r
}

// TestEachOriginIsAnsweredAsItsSocketsRuleSays holds §4.3 for each request
// kind, on both sockets (R6, R7): absent passes, null and every other origin
// is refused, and each socket takes its own authority alone.
func TestEachOriginIsAnsweredAsItsSocketsRuleSays(t *testing.T) {
	srv, _, _ := newTestServer(t)
	lan := testLANPolicy(t, testLANAddr, testLANNet, 7789, "box.local")
	at80 := testLANPolicy(t, testLANAddr, testLANNet, 80, "box.local")
	for k := range originKinds {
		for _, c := range originRows {
			if got := originPassed(serveTo(srv, originRequest(k, nil, testConsoleHost, c.origin))); got != c.loop {
				t.Errorf("%s on the loopback socket, Origin %q: passed %v, want %v", originKinds[k].name, c.origin, got, c.loop)
			}
			if got := originPassed(serveTo(srv, originRequest(k, lan, testLANHost, c.origin))); got != c.lan {
				t.Errorf("%s on the LAN socket, Origin %q: passed %v, want %v", originKinds[k].name, c.origin, got, c.lan)
			}
			if got := originPassed(serveTo(srv, originRequest(k, at80, "192.168.77.20", c.origin))); got != c.at80 {
				t.Errorf("%s on the LAN socket at 80, Origin %q: passed %v, want %v", originKinds[k].name, c.origin, got, c.at80)
			}
		}
	}
}

// spellingRows are the spellings §4.2's table asks alike as a Host and as an
// Origin, on a LAN socket at P (7789) and at 80, for an IPv4 A and a ULA A.
var spellingRows = []struct {
	spelling        string
	v4, v4at80, ula bool
	note            string
}{
	{"192.168.77.20:7789", true, false, false, "A:P"},
	{"192.168.77.20", false, true, false, "no port"},
	{"192.168.77.20:", false, true, false, "an empty port"},
	{"192.168.77.20:80", false, true, false, "the default port"},
	{"192.168.77.20:07789", false, false, false, "a leading zero"},
	{"192.168.77.20:+7789", false, false, false, "a sign"},
	{"u@192.168.77.20:7789", false, false, false, "a user"},
	{"192.168.77.20:7789/", false, false, false, "a path"},
	{"192.168.77.20:7789?x", false, false, false, "a query"},
	{"192.168.77.20:7789#f", false, false, false, "a fragment"},
	{"192.168.77.20.:7789", false, false, false, "a trailing dot on a literal"},
	{"[::ffff:192.168.77.20]:7789", false, false, false, "mapped"},
	{"box.local:7789", true, false, true, ".local"},
	{"BOX.LOCAL:7789", true, false, true, ".local in upper case"},
	{"box.local.:7789", true, false, true, ".local with one trailing dot"},
	{"box.local..:7789", false, false, false, ".local with two"},
	{"box.local", false, true, false, ".local with no port"},
	{"box.local:80", false, true, false, ".local at 80"},
	{"[fd12::20]:7789", false, false, true, "a ULA"},
	{"[fd12:0::20]:7789", false, false, true, "a ULA in another spelling"},
	{"fd12::20:7789", false, false, false, "a ULA without brackets"},
	{"[fd12::20%25eth0]:7789", false, false, false, "a zone, escaped"},
	{"[fd12::20%eth0]:7789", false, false, false, "a zone"},
}

// TestHostAndOriginTakeEachSpellingAlike asks each spelling as a Host and as
// an Origin (http:// and the spelling) on the same socket: both answers
// agree, and agree with the table (D3, D4, D5, F5).
func TestHostAndOriginTakeEachSpellingAlike(t *testing.T) {
	srv, _, _ := newTestServer(t)
	v4 := testLANPolicy(t, testLANAddr, testLANNet, 7789, "box.local")
	v4at80 := testLANPolicy(t, testLANAddr, testLANNet, 80, "box.local")
	ula := testLANPolicy(t, "fd12::20", "fd12::/64", 7789, "box.local")
	sockets := []struct {
		p              *lanPolicy
		peer, wantHost string
		want           func(int) bool
	}{
		{v4, testLANPeer, testLANHost, func(i int) bool { return spellingRows[i].v4 }},
		{v4at80, testLANPeer, "192.168.77.20", func(i int) bool { return spellingRows[i].v4at80 }},
		{ula, "[fd12::99]:51000", "[fd12::20]:7789", func(i int) bool { return spellingRows[i].ula }},
	}
	for _, s := range sockets {
		for i, row := range spellingRows {
			// Host: is the request answered past the boundary?
			hostTaken := serveTo(srv, lanRequest(s.p, http.MethodGet, hostProbe, s.peer, row.spelling, nil)).Code == http.StatusOK
			// Origin: a bodyless POST on the socket's own Host.
			r := lanRequest(s.p, http.MethodPost, "/logout", s.peer, s.wantHost, nil)
			r.Header.Set("Origin", "http://"+row.spelling)
			originTaken := originPassed(serveTo(srv, r))
			if hostTaken != originTaken {
				t.Errorf("socket %s:%d, %s %q: Host taken %v, Origin taken %v", s.p.addr, s.p.port, row.note, row.spelling, hostTaken, originTaken)
			}
			if want := s.want(i); hostTaken != want {
				t.Errorf("socket %s:%d, %s %q: taken %v, want %v", s.p.addr, s.p.port, row.note, row.spelling, hostTaken, want)
			}
		}
	}
}

// TestTheLANAuthorityParserIsOneGrammar holds parseLANAuthority to §4.2's
// grammar at its every edge, and its answer to a canonical form a second
// parse gives again (F5).
func TestTheLANAuthorityParserIsOneGrammar(t *testing.T) {
	for _, raw := range []string{
		"", "a b", "a\tb", "a@b", "a/b", "a?b", "a#b", "a%b", "a\\b", "a\x7fb", "é.local", "a\x00b",
		"[fd12::1%eth0]", "[fd12::1%25eth0]", "[::ffff:192.168.1.1]", "[192.168.1.1]", "[fd12::1", "[fd12::1]x",
		"1.2.3", "01.2.3.4", "1.2.3.4.5", "256.1.1.1", "1.2.3.4.",
		strings.Repeat("a", 64) + ".local", strings.Repeat("a.", 127) + "aa",
		"a..b", ".local", "a:0", "a:65536", "a:07789", "a:+1", "a:1a", "a:1:2", "fd12::1",
	} {
		if got, ok := parseLANAuthority(raw); ok {
			t.Errorf("parseLANAuthority(%q) took it: %+v", raw, got)
		}
	}
	for _, c := range []struct {
		raw  string
		want lanAuthority
	}{
		{"192.168.1.20:7789", lanAuthority{addr: netip.MustParseAddr("192.168.1.20"), port: 7789}},
		{"192.168.1.20", lanAuthority{addr: netip.MustParseAddr("192.168.1.20"), port: 80}},
		{"192.168.1.20:", lanAuthority{addr: netip.MustParseAddr("192.168.1.20"), port: 80}},
		{"[FD12::0:1]:65535", lanAuthority{addr: netip.MustParseAddr("fd12::1"), port: 65535}},
		{"[fd12::1]", lanAuthority{addr: netip.MustParseAddr("fd12::1"), port: 80}},
		{"[fd12::1]:", lanAuthority{addr: netip.MustParseAddr("fd12::1"), port: 80}},
		{"Box.Local.:1", lanAuthority{name: "box.local", port: 1}},
		{"my_box-2.local", lanAuthority{name: "my_box-2.local", port: 80}},
		{strings.Repeat("a", 63) + ".local:7789", lanAuthority{name: strings.Repeat("a", 63) + ".local", port: 7789}},
	} {
		got, ok := parseLANAuthority(c.raw)
		if !ok || got != c.want {
			t.Errorf("parseLANAuthority(%q) = %+v, %v; want %+v", c.raw, got, ok, c.want)
			continue
		}
		// Canonical: its own spelling parses to itself.
		var again string
		if got.addr.IsValid() {
			again = net.JoinHostPort(got.addr.String(), itoaPort(got.port))
		} else {
			again = got.name + ":" + itoaPort(got.port)
		}
		if back, ok := parseLANAuthority(again); !ok || back != got {
			t.Errorf("the canonical %q of %q parses to %+v, %v", again, c.raw, back, ok)
		}
	}
	for _, o := range []string{"null", "https://192.168.1.20", "HTTP://192.168.1.20", " http://192.168.1.20", "http:/192.168.1.20", "file://x"} {
		if _, ok := lanOriginAuthority(o); ok {
			t.Errorf("lanOriginAuthority(%q) took it", o)
		}
	}
}

func itoaPort(p int) string {
	b, _ := json.Marshal(p)
	return string(b)
}

// TestTheEventStreamAndTheCSRFGuardShareOneOriginRule: both gates call
// originMatches and neither the loopback matcher itself, so the two change
// together or not at all (§4.3, structural).
func TestTheEventStreamAndTheCSRFGuardShareOneOriginRule(t *testing.T) {
	for file, fn := range map[string]string{"admin.go": "csrfGuard", "handlers_events.go": "apiEvents"} {
		calls := callsIn(t, file, fn)
		if !calls["originMatches"] {
			t.Errorf("%s in %s does not call originMatches", fn, file)
		}
		if calls["originMatchesAdmin"] {
			t.Errorf("%s in %s calls originMatchesAdmin itself", fn, file)
		}
	}
	if calls := callsIn(t, "console_lan.go", "originMatches"); !calls["lanOriginOK"] || !calls["originMatchesAdmin"] {
		t.Errorf("originMatches calls %v, want lanOriginOK and originMatchesAdmin", calls)
	}
}

// TestHostAndOriginShareOneAuthorityParser: the Host check and the Origin
// check call the one parser, and no code of the LAN socket's reads an
// authority through url.Parse or url.URL's Hostname or Port (F5,
// structural).
func TestHostAndOriginShareOneAuthorityParser(t *testing.T) {
	if !callsIn(t, "console_lan.go", "lanHostOK")["parseLANAuthority"] {
		t.Error("lanHostOK does not call parseLANAuthority")
	}
	if !callsIn(t, "console_lan.go", "lanOriginOK")["lanOriginAuthority"] {
		t.Error("lanOriginOK does not call lanOriginAuthority")
	}
	if !callsIn(t, "console_lan_authority.go", "lanOriginAuthority")["parseLANAuthority"] {
		t.Error("lanOriginAuthority does not call parseLANAuthority")
	}
	for _, file := range []string{"console_lan.go", "console_lan_authority.go"} {
		for _, fn := range funcsIn(t, file) {
			calls := callsIn(t, file, fn)
			for _, bad := range []string{"Parse", "Hostname", "Port", "ParseRequestURI"} {
				if calls[bad] && (bad != "Parse" || calls["url.Parse"]) {
					t.Errorf("%s in %s calls %s", fn, file, bad)
				}
			}
		}
	}
}

// callsIn is the names fn's body calls, as name and as package.name.
func callsIn(t *testing.T, file, fn string) map[string]bool {
	t.Helper()
	f := parseFile(t, file)
	out := map[string]bool{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := call.Fun.(type) {
			case *ast.Ident:
				out[f.Name] = true
			case *ast.SelectorExpr:
				out[f.Sel.Name] = true
				if x, ok := f.X.(*ast.Ident); ok {
					out[x.Name+"."+f.Sel.Name] = true
				}
			}
			return true
		})
	}
	return out
}

// funcsIn is the functions file declares.
func funcsIn(t *testing.T, file string) []string {
	t.Helper()
	var out []string
	for _, d := range parseFile(t, file).Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			out = append(out, fd.Name.Name)
		}
	}
	return out
}

func parseFile(t *testing.T, file string) *ast.File {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), file, strings.ReplaceAll(string(src), "\r\n", "\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// listeningState stores a listening state on srv's plan, as a bind leaves
// it, without a socket: the surfaces read the state alone.
func listeningState(t *testing.T, srv *Server, source string, key config.LANKey) {
	t.Helper()
	plan := ConsoleLANPlan{Address: netip.MustParseAddr(testLANAddr), Port: 7789, Source: source, FileKey: key}
	c := &consoleLAN{s: srv, plan: plan}
	st := c.base(ConsoleLANListening, "")
	st.URL, st.NameURL = "http://"+testLANHost, "http://box.local:7789"
	st.Network, st.Interface, st.Hold = testLANNet, "en-test", consolelan.HoldBindToIfindex
	c.state.Store(&st)
	srv.consoleLAN = c
}

// waitingState stores a waiting state with reason on srv's plan.
func waitingState(t *testing.T, srv *Server, reason, source string, key config.LANKey) {
	t.Helper()
	plan := ConsoleLANPlan{Address: netip.MustParseAddr(testLANAddr), Port: 7789, Source: source, FileKey: key}
	c := &consoleLAN{s: srv, plan: plan}
	st := c.base(ConsoleLANWaiting, reason)
	c.state.Store(&st)
	srv.consoleLAN = c
}

// TestEachRefusalSaysWhereToOpenTheConsole holds §4.4's words per socket
// (R7): the LAN 421 names the socket's URL and .local name, the LAN 403 no
// address, the LAN cross-origin refusal the socket's URL and no tunnel; the
// loopback socket's words as they were, and one sentence more while the LAN
// socket listens.
func TestEachRefusalSaysWhereToOpenTheConsole(t *testing.T) {
	srv, _, _ := newTestServer(t)
	lan := testLANPolicy(t, testLANAddr, testLANNet, 7789, "box.local")
	rr := serveTo(srv, lanRequest(lan, http.MethodGet, "/api/console", testLANPeer, "other.example:7789", nil))
	if rr.Code != 421 || !strings.Contains(rr.Body.String(), "answers only as http://192.168.77.20:7789 or http://box.local:7789") {
		t.Errorf("LAN 421: %d %q", rr.Code, rr.Body)
	}
	rr = serveTo(srv, lanRequest(lan, http.MethodGet, "/api/console", "10.9.9.9:5000", testLANHost, nil))
	if rr.Code != 403 || !strings.Contains(rr.Body.String(), "answers only to devices on its own network") || strings.Contains(rr.Body.String(), "10.9.9.9") {
		t.Errorf("LAN 403: %d %q", rr.Code, rr.Body)
	}
	r := lanRequest(lan, http.MethodPost, "/logout", testLANPeer, testLANHost, nil)
	r.Header.Set("Origin", "https://evil.example")
	rr = serveTo(srv, r)
	if !strings.Contains(rr.Body.String(), "This console must be opened at http://192.168.77.20:7789.") || strings.Contains(rr.Body.String(), "ssh") {
		t.Errorf("LAN cross-origin: %q", rr.Body)
	}
	// Loopback, with no LAN socket: today's words alone.
	lr := loopbackRequest(http.MethodPost, "/logout", testConsoleHost, nil)
	lr.Header.Set("Origin", "https://evil.example")
	rr = serveTo(srv, lr)
	if !strings.Contains(rr.Body.String(), "ssh -L 7789:127.0.0.1:7789") || strings.Contains(rr.Body.String(), "home network") {
		t.Errorf("loopback cross-origin, no LAN socket: %q", rr.Body)
	}
	if got := serveTo(srv, loopbackRequest(http.MethodGet, "/api/console", "other.example", nil)).Body.String(); !strings.Contains(got, errMsgForeignHost) {
		t.Errorf("loopback 421 changed: %q", got)
	}
	listeningState(t, srv, consolelan.SourceFile, config.LANKey{State: config.LANKeyAddress, Address: testLANAddr})
	rr = serveTo(srv, lr)
	if !strings.Contains(rr.Body.String(), "ssh -L 7789:127.0.0.1:7789") ||
		!strings.Contains(rr.Body.String(), "It also answers on your home network at http://192.168.77.20:7789.") {
		t.Errorf("loopback cross-origin, LAN socket listening: %q", rr.Body)
	}
}

// TestTheLANSocketsRefusalsNameNoPeerAndSpareLoopbacksLog holds §4.4's log
// lines: one Warn per kind of source refusal, naming A's network and never
// the peer; and the LAN socket's refused Hosts keep a set of their own, so
// they cannot use up loopback's.
func TestTheLANSocketsRefusalsNameNoPeerAndSpareLoopbacksLog(t *testing.T) {
	logs := loggingtest.Record(t)
	srv, _, _ := newTestServer(t)
	lan := testLANPolicy(t, testLANAddr, testLANNet, 7789, "")
	for _, peer := range []string{"10.9.9.9:5000", "10.9.9.10:5000", "192.168.78.1:5000", "100.64.0.9:5000"} {
		serveTo(srv, lanRequest(lan, http.MethodGet, "/api/console", peer, testLANHost, nil))
	}
	lines := logs.Lines("console refused a home-network connection from outside its network")
	if len(lines) != 3 {
		t.Fatalf("source refusal lines %d, want one per kind (3): %v", len(lines), lines)
	}
	for _, l := range lines {
		if !strings.Contains(l, testLANNet) {
			t.Errorf("the line %q does not name the network", l)
		}
	}
	if got := logs.Carrying("10.9.9.9", "10.9.9.10", "192.168.78.1", "100.64.0.9"); len(got) != 0 {
		t.Errorf("a line names a peer: %v", got)
	}
	for i := 0; i < foreignHostSeenCap+4; i++ {
		serveTo(srv, lanRequest(lan, http.MethodGet, "/api/console", testLANPeer, "n"+itoaPort(i)+".example:7789", nil))
	}
	serveTo(srv, loopbackRequest(http.MethodGet, "/api/console", "loopback-proxy.example", nil))
	if got := logs.Carrying("loopback-proxy.example"); len(got) != 1 {
		t.Errorf("after the LAN socket's refusals, loopback's refused Host logged %d lines, want 1", len(got))
	}
}

// TestTheMenuSaysWhereTheConsoleAnswers holds §5.1's three states (R9): the
// line is read from the socket's state at the page load, never the config.
func TestTheMenuSaysWhereTheConsoleAnswers(t *testing.T) {
	srv, _, _ := newTestServer(t)
	page := func() string {
		return serveTo(srv, loopbackRequest(http.MethodGet, "/devices", testConsoleHost, nil)).Body.String()
	}
	if strings.Contains(page(), `id="console-lan-line"`) {
		t.Fatal("a bridge with no posture has a menu line")
	}
	key := config.LANKey{State: config.LANKeyAddress, Address: testLANAddr}
	listeningState(t, srv, consolelan.SourceFile, key)
	if got := page(); !strings.Contains(got, "Open on your home network at http://192.168.77.20:7789, with no password: anyone on that network can use this console.") {
		t.Errorf("listening: no line in the page")
	}
	waitingState(t, srv, consolelan.ReasonAddressNotOnBox, consolelan.SourceFile, key)
	if got := page(); !strings.Contains(got, "Not open on your home network yet: 192.168.77.20 is not on this box (tried again every 5 s). Until then this console answers on this machine only.") {
		t.Errorf("waiting: no line in the page")
	}
	waitingState(t, srv, consolelan.ReasonNotSupportedHere, consolelan.SourceFile, key)
	if got := page(); !strings.Contains(got, "this platform cannot hold the console") || strings.Contains(got, "tried again") {
		t.Errorf("not supported here: the line is wrong")
	}
}

// TestTheMenuLineSaysWhereTheAddressCameFromAndWhatItCostsTheRollback: the
// source's words per source, the rollback sentence where the file held the
// key at the start, the variable's case apart, and an empty key's line while
// off (D7, D8, D16).
func TestTheMenuLineSaysWhereTheAddressCameFromAndWhatItCostsTheRollback(t *testing.T) {
	srv, _, _ := newTestServer(t)
	line := func() string { return srv.consoleLANLine() }
	listeningState(t, srv, consolelan.SourceFile, config.LANKey{State: config.LANKeyAddress, Address: testLANAddr})
	if l := line(); !strings.Contains(l, "set in bridge.yaml (adminLANAddress)") || !strings.Contains(l, "remove the adminLANAddress line") ||
		!strings.Contains(l, "Bridge 0.2.3 and earlier refuse this bridge.yaml while it holds the adminLANAddress line, even empty") {
		t.Errorf("file: %q", l)
	}
	listeningState(t, srv, consolelan.SourceEnvironment, config.LANKey{State: config.LANKeyAbsent})
	if l := line(); !strings.Contains(l, "set by BRIDGE_ADMIN_LAN_ADDRESS in the bridge's environment") ||
		!strings.Contains(l, "ignore that variable, so a rollback to one of them starts with the console on this machine only") ||
		strings.Contains(l, "refuse this bridge.yaml") {
		t.Errorf("environment: %q", l)
	}
	listeningState(t, srv, consolelan.SourceBoth, config.LANKey{State: config.LANKeyEmpty})
	if l := line(); !strings.Contains(l, "over bridge.yaml's adminLANAddress") || !strings.Contains(l, "turns the console off the home network") ||
		!strings.Contains(l, "refuse this bridge.yaml") {
		t.Errorf("both over an empty key: %q", l)
	}
	// Off, with an empty key at the start: the rollback sentence alone.
	c := &consoleLAN{s: srv, plan: ConsoleLANPlan{Port: 7789, FileKey: config.LANKey{State: config.LANKeyEmpty}}}
	st := c.base(ConsoleLANOff, "")
	c.state.Store(&st)
	srv.consoleLAN = c
	if l := line(); !strings.HasPrefix(l, "Bridge 0.2.3 and earlier refuse this bridge.yaml") || strings.Contains(l, "home network at") {
		t.Errorf("off with an empty key: %q", l)
	}
}

// TestTheConsoleRouteReportsTheSocketsState holds GET /api/console to §5.2,
// on both sockets.
func TestTheConsoleRouteReportsTheSocketsState(t *testing.T) {
	srv, _, _ := newTestServer(t)
	read := func(r *http.Request) map[string]any {
		t.Helper()
		rr := serveTo(srv, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /api/console: %d %s", rr.Code, rr.Body)
		}
		var got map[string]map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got["lan"]
	}
	off := read(loopbackRequest(http.MethodGet, "/api/console", testConsoleHost, nil))
	if off["state"] != "off" || off["fileKeyAtBoot"] != "absent" || off["source"] != "" {
		t.Errorf("off: %v", off)
	}
	listeningState(t, srv, consolelan.SourceBoth, config.LANKey{State: config.LANKeyAddress, Address: "192.168.77.30"})
	for _, r := range []*http.Request{
		loopbackRequest(http.MethodGet, "/api/console", testConsoleHost, nil),
		lanRequest(testLANPolicy(t, testLANAddr, testLANNet, 7789, ""), http.MethodGet, "/api/console", testLANPeer, testLANHost, nil),
	} {
		got := read(r)
		for k, want := range map[string]string{
			"state": "listening", "address": testLANAddr, "url": "http://" + testLANHost, "nameUrl": "http://box.local:7789",
			"source": "both", "fileKeyAtBoot": "address", "fileAddressAtBoot": "192.168.77.30",
			"network": testLANNet, "interface": "en-test", "hold": "SO_BINDTOIFINDEX",
		} {
			if got[k] != want {
				t.Errorf("%s = %v, want %q", k, got[k], want)
			}
		}
	}
}
