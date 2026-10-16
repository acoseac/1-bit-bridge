package dlna

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// judgedAs is what a test's loopback connections and datagrams are judged as
// (servedAddrs.judgeAs): the address it holds, or the real one while it holds
// none.
type judgedAs struct{ p atomic.Pointer[netip.Addr] }

// set judges what arrives from now on as the address as, or as itself for "".
func (j *judgedAs) set(as string) {
	if as == "" {
		j.p.Store(nil)
		return
	}
	a := netip.MustParseAddr(as)
	j.p.Store(&a)
}

// judge is the servedAddrs.judgeAs seam.
func (j *judgedAs) judge(a netip.Addr) netip.Addr {
	if as := j.p.Load(); as != nil {
		return *as
	}
	return a
}

// macServedJudgedBy returns what the Mac B48 was measured on serves, with
// every address judged by j.
func macServedJudgedBy(j *judgedAs) *servedAddrs {
	s := newTestHost(macWithTailscale()...).served(nil)
	s.judgeAs = j.judge
	return s
}

// describeOverTCP sends one GET of the device description on a fresh
// connection and reads until the server closes it. It returns the status the
// response carries (0 for none), how many bytes came back, and the read's
// error: a refused connection may end in EOF, a reset or an abort,
// depending on the platform and on whether the request reached the server
// before its close, and what proves the refusal is that no byte came back.
func describeOverTCP(t *testing.T, addr string) (status, n int, readErr error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// A write the server's close beat is the same refusal; the read says.
	_, _ = fmt.Fprintf(conn, "GET /dlna/description.xml HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", addr)
	body, readErr := io.ReadAll(conn)
	if line, _, found := strings.Cut(string(body), "\r\n"); found {
		_, _ = fmt.Sscanf(line, "HTTP/1.1 %d", &status)
	}
	return status, len(body), readErr
}

// TestTheDLNAListenerRefusesAConnectionToAnAddressItDoesNotServe pins the
// listener half of backlog B48 through the real Start. The listener binds
// dlna.listenAddress, ":7790" by default, and so took a connection on every
// address of the host: measured from a tailnet peer, a GET of the
// description on the host's tailnet address answered 200, with 1,440 bytes.
// Each case judges one loopback connection as one of the Mac's addresses
// (servedAddrs.judgeAs). A served one is answered; a refused one is closed
// before a byte of the answer is written.
func TestTheDLNAListenerRefusesAConnectionToAnAddressItDoesNotServe(t *testing.T) {
	var judged judgedAs
	s := startLoopbackDLNA(t, ServerConfig{}, func(s *Server) { s.served = macServedJudgedBy(&judged) })
	cases := []struct {
		name   string
		as     string
		served bool
	}{
		{"loopback", "", true},
		{"lan_ipv4", "192.168.1.20", true},
		{"lan_link_local_ipv6", "fe80::e0%en0", true},
		{"zero_config_link", "169.254.7.8", true},
		{"tailnet_ipv4", "100.64.0.7", false},
		{"tailnet_ula", "fd7a:115c:a1e0::7", false},
		{"tailnet_link_local", "fe80::d7%utun7", false},
		{"awdl_link_local", "fe80::a0%awdl0", false},
		{"lan_slaac_address", "2001:db8::20", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			judged.set(tc.as)
			defer judged.set("")
			status, n, err := describeOverTCP(t, s.cfg.ListenAddress)
			switch {
			case tc.served && (status != 200 || err != nil):
				t.Errorf("a connection judged as %q was answered %d (%d bytes, read error %v), want 200", tc.as, status, n, err)
			case !tc.served && n != 0:
				t.Errorf("a connection judged as %s was answered %d with %d bytes, want it closed before any", tc.as, status, n)
			}
		})
	}
}

// TestTheDLNAListenerLogsARefusedConnection pins that the listener's
// refusal reaches the server's log, naming the address and its interface.
func TestTheDLNAListenerLogsARefusedConnection(t *testing.T) {
	var judged judgedAs
	sink := newLogSink()
	s := startLoopbackDLNA(t, ServerConfig{Logger: sink.logger()}, func(s *Server) { s.served = macServedJudgedBy(&judged) })
	judged.set("100.64.0.7")
	if _, n, _ := describeOverTCP(t, s.cfg.ListenAddress); n != 0 {
		t.Errorf("a connection judged as the tailnet address got %d bytes back, want none", n)
	}
	if want := "refused a connection to an address it does not serve"; !sink.waitFor(want, 5*time.Second) {
		t.Fatalf("no refusal in the log:\n%s", sink.String())
	}
	if got := sink.String(); !strings.Contains(got, "address=100.64.0.7 interface=utun7 from=127.0.0.1") {
		t.Errorf("the refusal does not name the address, its interface and the peer:\n%s", got)
	}
}

// pinnedOffTheLANLine is the Warn Start logs for a listener pinned to an
// address the server does not serve.
const pinnedOffTheLANLine = "dlna.listenAddress pins the listener to an address that is not a " +
	"LAN-eligible interface's: with it, DLNA serves no one"

// TestAListenerPinnedToAnAddressItDoesNotServeWarnsOnceAtStart pins the
// startup check (backlog B48): a dlna.listenAddress pinned to an address
// the server does not serve, the tailnet's say, binds only that address and
// refuses every connection on it, so DLNA serves no one. Start says so once,
// naming the pinned address, and each refused connection is still logged, once
// under its own cap. The listener is pinned to 127.0.0.1 and judged, from
// before Start, as the tailnet address.
func TestAListenerPinnedToAnAddressItDoesNotServeWarnsOnceAtStart(t *testing.T) {
	var judged judgedAs
	judged.set("100.64.0.7")
	sink := newLogSink()
	s := startLoopbackDLNA(t, ServerConfig{Logger: sink.logger()}, func(s *Server) { s.served = macServedJudgedBy(&judged) })
	got := sink.String()
	if n := strings.Count(got, pinnedOffTheLANLine); n != 1 {
		t.Fatalf("Start logged the pinned-address warning %d times, want once:\n%s", n, got)
	}
	if want := "listenAddress=" + s.cfg.ListenAddress + " address=100.64.0.7 interface=utun7"; !strings.Contains(got, want) {
		t.Errorf("the warning does not name the pinned address (%s):\n%s", want, got)
	}
	for range 2 {
		if _, n, _ := describeOverTCP(t, s.cfg.ListenAddress); n != 0 {
			t.Errorf("a connection to the pinned address got %d bytes back, want none", n)
		}
	}
	const refused = "refused a connection to an address it does not serve"
	if !sink.waitFor(refused, 5*time.Second) {
		t.Fatalf("no per-connection refusal in the log:\n%s", sink.String())
	}
	got = sink.String()
	if n := strings.Count(got, refused); n != 1 {
		t.Errorf("two refused connections logged %d refusals, want 1 (the per-address cap):\n%s", n, got)
	}
	if n := strings.Count(got, pinnedOffTheLANLine); n != 1 {
		t.Errorf("the pinned-address warning appears %d times after the connections, want once:\n%s", n, got)
	}
}

// TestTheStartupCheckWarnsOnlyForAPinnedAddressItDoesNotServe pins which
// bound addresses warnIfPinnedOffTheLAN warns about: a wildcard is no pin;
// loopback, a LAN address, a zero-config link's and an address the server
// advertises on a member are served; the tailnet's addresses and a member's
// SLAAC address are not, and the warning names the one bound.
func TestTheStartupCheckWarnsOnlyForAPinnedAddressItDoesNotServe(t *testing.T) {
	publicMember := []fakeIface{lan("eth0", "203.0.113.5", "fd12:3456::5")}
	cases := []struct {
		name       string
		host       []fakeIface
		advertised []string
		bound      string
		warn       bool
	}{
		{"ipv4_wildcard", macWithTailscale(), nil, "0.0.0.0", false},
		{"ipv6_wildcard", macWithTailscale(), nil, "::", false},
		{"loopback", macWithTailscale(), nil, "127.0.0.1", false},
		{"lan_ipv4", macWithTailscale(), nil, "192.168.1.20", false},
		{"zero_config_link", macWithTailscale(), nil, "169.254.7.8", false},
		{"tailnet_ipv4", macWithTailscale(), nil, "100.64.0.7", true},
		{"tailnet_ula", macWithTailscale(), nil, "fd7a:115c:a1e0::7", true},
		{"lan_slaac_address", macWithTailscale(), nil, "2001:db8::20", true},
		{"a_public_address_the_server_advertises", publicMember, []string{"203.0.113.5"}, "203.0.113.5", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := &Server{
				cfg:    ServerConfig{ListenAddress: net.JoinHostPort(tc.bound, "7790")},
				log:    slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
				served: newTestHost(tc.host...).served(nil, tc.advertised...),
			}
			s.warnIfPinnedOffTheLAN(&net.TCPAddr{IP: net.ParseIP(tc.bound), Port: 7790})
			got := buf.String()
			switch n := strings.Count(got, pinnedOffTheLANLine); {
			case tc.warn && n != 1:
				t.Errorf("bound to %s: %d warnings, want 1:\n%s", tc.bound, n, got)
			case !tc.warn && n != 0:
				t.Errorf("bound to %s: %d warnings, want none:\n%s", tc.bound, n, got)
			case tc.warn && !strings.Contains(got, "address="+tc.bound+" "):
				t.Errorf("the warning does not name the bound address %s:\n%s", tc.bound, got)
			}
		})
	}
}

// logSink is a log a test can wait on.
type logSink struct {
	mu    sync.Mutex
	buf   strings.Builder
	wrote chan struct{}
}

func newLogSink() *logSink { return &logSink{wrote: make(chan struct{}, 1)} }

// logger returns a Warn-level text logger that writes to the sink.
func (l *logSink) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	n, err := l.buf.Write(p)
	l.mu.Unlock()
	select {
	case l.wrote <- struct{}{}:
	default:
	}
	return n, err
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// waitFor reports whether the log holds want, waiting up to d for it.
func (l *logSink) waitFor(want string, d time.Duration) bool {
	timeout := time.NewTimer(d)
	defer timeout.Stop()
	for !strings.Contains(l.String(), want) {
		select {
		case <-l.wrote:
		case <-timeout.C:
			return strings.Contains(l.String(), want)
		}
	}
	return true
}
