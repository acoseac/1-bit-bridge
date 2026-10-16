package dlna

import (
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
// response carries (0 for none) and how many bytes came back.
func describeOverTCP(t *testing.T, addr string) (status, n int) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// A write the server's close beat is the same refusal; the read says.
	_, _ = fmt.Fprintf(conn, "GET /dlna/description.xml HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", addr)
	body, err := io.ReadAll(conn)
	if err != nil && !isConnReset(err) {
		t.Fatalf("read the answer from %s: %v", addr, err)
	}
	if line, _, found := strings.Cut(string(body), "\r\n"); found {
		_, _ = fmt.Sscanf(line, "HTTP/1.1 %d", &status)
	}
	return status, len(body)
}

// isConnReset reports whether err is the reset a close with an unread
// request in its buffer sends, which is how a refused connection can end.
func isConnReset(err error) bool {
	s := err.Error()
	return strings.Contains(s, "reset") || strings.Contains(s, "forcibly closed") || strings.Contains(s, "broken pipe")
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
			status, n := describeOverTCP(t, s.cfg.ListenAddress)
			switch {
			case tc.served && status != 200:
				t.Errorf("a connection judged as %q was answered %d (%d bytes), want 200", tc.as, status, n)
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
	if _, n := describeOverTCP(t, s.cfg.ListenAddress); n != 0 {
		t.Errorf("a connection judged as the tailnet address got %d bytes back, want none", n)
	}
	if want := "refused a connection to an address it does not serve"; !sink.waitFor(want, 5*time.Second) {
		t.Fatalf("no refusal in the log:\n%s", sink.String())
	}
	if got := sink.String(); !strings.Contains(got, "address=100.64.0.7 interface=utun7 from=127.0.0.1") {
		t.Errorf("the refusal does not name the address, its interface and the peer:\n%s", got)
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
