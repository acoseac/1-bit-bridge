package dlna

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

// The goldens in this file pin what the MediaServer puts on the wire through
// the real Server, black-box: the datagrams it multicasts when it starts and
// stops, spelled out header by header rather than built with the packet
// builders, so a change to the builders or to what the server hands them
// shows here. They were written against the per-endpoint advertisers, before
// B470 PR 5a1 moved SSDP onto shared registrations, and that change keeps
// every byte. Every server here advertises on the loopback interface
// (loopbackInterface, the B38 rule).

// goldenModelNumber is the ModelNumber the golden servers carry, which the
// SERVER header names.
const goldenModelNumber = "v9.9.9-golden"

// goldenServerToken is the SERVER header of a golden server, written out.
const goldenServerToken = "Darwin/UPnP/1.0 1-bit-bridge/v9.9.9-golden DLNADOC/1.50"

// goldenUDN returns a UDN of the uuid: shape carrying a nonce, so what a
// test reads back is its own server's.
func goldenUDN(t *testing.T) string {
	t.Helper()
	n := testNonce(t) // 16 hex
	return "uuid:5a1e5a1e-" + n[:4] + "-4" + n[4:7] + "-8" + n[7:10] + "-" + n[10:16] + "5a1e5a"
}

// goldenTargets is the MediaServer's (NT, USN) pairs, in the order it sends
// them, written out.
func goldenTargets(udn string) [][2]string {
	return [][2]string{
		{"upnp:rootdevice", udn + "::upnp:rootdevice"},
		{udn, udn},
		{"urn:schemas-upnp-org:device:MediaServer:1", udn + "::urn:schemas-upnp-org:device:MediaServer:1"},
		{"urn:schemas-upnp-org:service:ContentDirectory:1", udn + "::urn:schemas-upnp-org:service:ContentDirectory:1"},
		{"urn:schemas-upnp-org:service:ConnectionManager:1", udn + "::urn:schemas-upnp-org:service:ConnectionManager:1"},
	}
}

// goldenNotify is one NOTIFY as the MediaServer writes it: an alive carries
// CACHE-CONTROL, a byebye does not.
func goldenNotify(nts, location, nt, usn string) string {
	s := "NOTIFY * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n"
	if nts == "ssdp:alive" {
		s += "CACHE-CONTROL: max-age=1800\r\n"
	}
	return s +
		"LOCATION: " + location + "\r\n" +
		"SERVER: " + goldenServerToken + "\r\n" +
		"NT: " + nt + "\r\n" +
		"NTS: " + nts + "\r\n" +
		"USN: " + usn + "\r\n" +
		"\r\n"
}

// goldenBurst is one burst of NOTIFYs of kind nts for one LOCATION: one per
// target, in order.
func goldenBurst(nts, location, udn string) []string {
	var out []string
	for _, tg := range goldenTargets(udn) {
		out = append(out, goldenNotify(nts, location, tg[0], tg[1]))
	}
	return out
}

// goldenDate stands in for the DATE header of an M-SEARCH response, which
// normalizeDate puts in its place once the header has been checked.
const goldenDate = "<DATE>"

// goldenResponse is one M-SEARCH response as the MediaServer writes it, its
// DATE normalized.
func goldenResponse(location, st, usn string) string {
	return "HTTP/1.1 200 OK\r\n" +
		"CACHE-CONTROL: max-age=1800\r\n" +
		"DATE: " + goldenDate + "\r\n" +
		"EXT:\r\n" +
		"LOCATION: " + location + "\r\n" +
		"SERVER: " + goldenServerToken + "\r\n" +
		"ST: " + st + "\r\n" +
		"USN: " + usn + "\r\n" +
		"\r\n"
}

// normalizeDate replaces the DATE header's value with goldenDate, after
// checking it is an IMF-fixdate in GMT within a minute of now.
func normalizeDate(t *testing.T, resp string) string {
	t.Helper()
	const key = "\r\nDATE: "
	i := strings.Index(resp, key)
	if i < 0 {
		t.Errorf("a response carries no DATE header:\n%q", resp)
		return resp
	}
	start := i + len(key)
	end := strings.Index(resp[start:], "\r\n")
	if end < 0 {
		t.Errorf("a response's DATE header is not terminated:\n%q", resp)
		return resp
	}
	value := resp[start : start+end]
	when, err := time.Parse("Mon, 02 Jan 2006 15:04:05 GMT", value)
	if err != nil {
		t.Errorf("DATE %q is not an IMF-fixdate in GMT: %v", value, err)
	} else if d := time.Since(when); d < -time.Minute || d > time.Minute {
		t.Errorf("DATE %q is %v from now", value, d)
	}
	return resp[:start] + goldenDate + resp[start+end:]
}

// ssdpGroupCapture joins the SSDP group on iface with a listener of the
// test's own, closed at the test's end, which hears what an advertiser pinned
// there multicasts. Windows applies IP_MULTICAST_LOOP to what a socket
// receives, and ListenMulticastUDP turns it off, so it is turned back on.
func ssdpGroupCapture(t *testing.T, iface *net.Interface) *net.UDPConn {
	t.Helper()
	group, err := net.ResolveUDPAddr("udp4", SSDPMulticastAddr)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.ListenMulticastUDP("udp4", iface, group)
	if err != nil {
		t.Skipf("cannot join the SSDP group on %s: %v", iface.Name, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if err := ipv4.NewPacketConn(l).SetMulticastLoopback(true); err != nil {
		t.Fatalf("turn loopback on: %v", err)
	}
	return l
}

// readNotifies reads l until it holds want NOTIFYs of kind nts that name udn,
// or until nothing has arrived for wait, and returns them in arrival order.
func readNotifies(l *net.UDPConn, nts, udn string, want int, wait time.Duration) []string {
	var got []string
	buf := make([]byte, 8192)
	for len(got) < want {
		_ = l.SetReadDeadline(time.Now().Add(wait))
		n, _, err := l.ReadFromUDP(buf)
		if err != nil {
			return got
		}
		s := string(buf[:n])
		if strings.HasPrefix(s, "NOTIFY ") && strings.Contains(s, "\r\nNTS: "+nts+"\r\n") && strings.Contains(s, udn) {
			got = append(got, s)
		}
	}
	return got
}

// sameDatagrams reports, as one error line per difference, where got differs
// from want.
func sameDatagrams(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d datagrams, want %d:\n got %q\nwant %q", what, len(got), len(want), got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: datagram %d differs:\n got %q\nwant %q", what, i, got[i], want[i])
		}
	}
}

// goldenServer is a Server built for a golden: pre-bound loopback HTTP,
// the golden ModelNumber, and the endpoints, Interface and ServerURL given
// (a ServerURL of "" is the HTTP listener's own address).
func goldenServer(t *testing.T, udn string, cfg ServerConfig) (*Server, string) {
	t.Helper()
	l := loopbackListener(t)
	addr := l.Addr().String()
	cfg.Library = newTestLib(testTrack("t1", "Test Track"))
	cfg.UDN = udn
	cfg.FriendlyName = "Golden"
	cfg.ModelNumber = goldenModelNumber
	cfg.ListenAddress = addr
	cfg.Listener = l
	if cfg.ServerURL == "" {
		cfg.ServerURL = "http://" + addr
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, addr
}

// TestTheMediaServerAnnouncesAndWithdrawsItsTargetsInOrder pins the start
// and stop bursts of a MediaServer with one advertise endpoint: Start
// multicasts one ssdp:alive per target, rootdevice first, and Stop one
// ssdp:byebye per target, in the same order, every byte as written here.
func TestTheMediaServerAnnouncesAndWithdrawsItsTargetsInOrder(t *testing.T) {
	lo := loopbackInterface(t)
	capture := ssdpGroupCapture(t, lo)
	udn := goldenUDN(t)
	s, addr := goldenServer(t, udn, ServerConfig{})
	location := "http://" + addr + "/dlna/description.xml"
	s.cfg.AdvertiseEndpoints = []AdvertiseEndpoint{{Interface: lo, ServerURL: "http://" + addr}}
	if err := s.Start(context.Background()); err != nil {
		t.Skipf("the server did not start on %s: %v", lo.Name, err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = s.Stop(context.Background())
		}
	})
	sameDatagrams(t, "the start burst", readNotifies(capture, "ssdp:alive", udn, 5, 2*time.Second),
		goldenBurst("ssdp:alive", location, udn))

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stopped = true
	sameDatagrams(t, "the stop burst", readNotifies(capture, "ssdp:byebye", udn, 5, 2*time.Second),
		goldenBurst("ssdp:byebye", location, udn))
}

// TestTheMediaServerAnnouncesEachEndpointInItsOrder pins the bursts of a
// MediaServer with two advertise endpoints: each endpoint's LOCATION gets a
// whole burst, the first endpoint's before the second's, when it starts and
// when it stops. Both endpoints are on the loopback interface here, which no
// wiring produces (one endpoint per interface), and is enough for the order.
func TestTheMediaServerAnnouncesEachEndpointInItsOrder(t *testing.T) {
	lo := loopbackInterface(t)
	capture := ssdpGroupCapture(t, lo)
	udn := goldenUDN(t)
	s, addr := goldenServer(t, udn, ServerConfig{})
	_, port, _ := net.SplitHostPort(addr)
	first, second := "http://"+addr, "http://localhost:"+port
	s.cfg.AdvertiseEndpoints = []AdvertiseEndpoint{{Interface: lo, ServerURL: first}, {Interface: lo, ServerURL: second}}
	if err := s.Start(context.Background()); err != nil {
		t.Skipf("the server did not start on %s: %v", lo.Name, err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = s.Stop(context.Background())
		}
	})
	want := append(goldenBurst("ssdp:alive", first+"/dlna/description.xml", udn),
		goldenBurst("ssdp:alive", second+"/dlna/description.xml", udn)...)
	sameDatagrams(t, "the start bursts", readNotifies(capture, "ssdp:alive", udn, 10, 2*time.Second), want)

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stopped = true
	want = append(goldenBurst("ssdp:byebye", first+"/dlna/description.xml", udn),
		goldenBurst("ssdp:byebye", second+"/dlna/description.xml", udn)...)
	sameDatagrams(t, "the stop bursts", readNotifies(capture, "ssdp:byebye", udn, 10, 2*time.Second), want)
}

// TestTheMediaServerAnnouncesItsServerURLAsWritten pins the single-endpoint
// fallback, which a pinned dlna.listenAddress takes (no AdvertiseEndpoints:
// Interface and ServerURL): the LOCATION is ServerURL and the description's
// path, the host exactly as the wiring wrote it, an IPv4 literal, a
// bracketed IPv6 literal or a name. SSDP itself stays IPv4: the bursts go
// to the IPv4 group whatever the LOCATION names.
func TestTheMediaServerAnnouncesItsServerURLAsWritten(t *testing.T) {
	lo := loopbackInterface(t)
	capture := ssdpGroupCapture(t, lo)
	for _, tc := range []struct{ name, host string }{
		{"ipv4_literal", "192.168.1.42"},
		{"ipv6_literal", "[2001:db8::42]"},
		{"host_name", "nas.local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			udn := goldenUDN(t)
			serverURL := "http://" + tc.host + ":7790"
			s, _ := goldenServer(t, udn, ServerConfig{Interface: lo, ServerURL: serverURL})
			if err := s.Start(context.Background()); err != nil {
				t.Skipf("the server did not start on %s: %v", lo.Name, err)
			}
			location := serverURL + "/dlna/description.xml"
			sameDatagrams(t, "the start burst", readNotifies(capture, "ssdp:alive", udn, 5, 2*time.Second),
				goldenBurst("ssdp:alive", location, udn))
			if err := s.Stop(context.Background()); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			sameDatagrams(t, "the stop burst", readNotifies(capture, "ssdp:byebye", udn, 5, 2*time.Second),
				goldenBurst("ssdp:byebye", location, udn))
		})
	}
}

// TestTheMediaServerRepeatsItsAliveBurstUnchanged pins the periodic burst:
// every repeat is the start burst again, byte for byte, in the same order.
// The cadence is 14 minutes (Test_NewSSDPAdvertiser_DefaultsAdvertiseInterval);
// this advertiser repeats every 50 ms, set white-box past the clamp.
func TestTheMediaServerRepeatsItsAliveBurstUnchanged(t *testing.T) {
	lo := loopbackInterface(t)
	capture := ssdpGroupCapture(t, lo)
	udn := goldenUDN(t)
	location := "http://127.0.0.1:7790/" + testNonce(t) + "/description.xml"
	a := newTestAdvertiser(testAdvertiserConfig{UDN: udn, Location: location, ServerToken: goldenServerToken, Interface: lo,
		AdvertiseInterval: 50 * time.Millisecond})
	if err := a.Start(context.Background()); err != nil {
		t.Skipf("multicast unavailable on the loopback interface: %v", err)
	}
	t.Cleanup(a.Stop)
	var want []string
	for range 3 {
		want = append(want, goldenBurst("ssdp:alive", location, udn)...)
	}
	sameDatagrams(t, "the start burst and two repeats", readNotifies(capture, "ssdp:alive", udn, len(want), 2*time.Second), want)
}

// syncBuffer is a bytes.Buffer a logger and a test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// unboundInterface is an interface no host has, where every SSDP join fails.
func unboundInterface() *net.Interface {
	return &net.Interface{Index: 1 << 20, Name: "bridge-test-nonexistent0"}
}

// closeObservedListener signals only after Close reaches the real socket.
// Waiting for it does not change a deadline shared with HTTP's Accept loop.
type closeObservedListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *closeObservedListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

// TestTheMediaServerStartsOnTheEndpointsThatBind pins what Start makes of an
// endpoint that cannot join the group: it is logged and skipped, and the
// server starts, announces and reports on the others; with none left, Start
// fails with the error the wiring reads as "DLNA disabled" and takes the
// HTTP listener back down.
func TestTheMediaServerStartsOnTheEndpointsThatBind(t *testing.T) {
	t.Run("one_of_two_binds", func(t *testing.T) {
		lo := loopbackInterface(t)
		capture := ssdpGroupCapture(t, lo)
		udn := goldenUDN(t)
		var log syncBuffer
		s, addr := goldenServer(t, udn, ServerConfig{
			Logger: slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelInfo})),
		})
		s.cfg.AdvertiseEndpoints = []AdvertiseEndpoint{
			{Interface: unboundInterface(), ServerURL: "http://192.168.77.1:7790"},
			{Interface: lo, ServerURL: "http://" + addr},
		}
		if err := s.Start(context.Background()); err != nil {
			t.Fatalf("Start with one endpoint of two binding: %v", err)
		}
		t.Cleanup(func() { _ = s.Stop(context.Background()) })
		sameDatagrams(t, "the start burst", readNotifies(capture, "ssdp:alive", udn, 6, 2*time.Second),
			goldenBurst("ssdp:alive", "http://"+addr+"/dlna/description.xml", udn))
		got := log.String()
		for _, want := range []string{
			`msg="DLNA SSDP advertiser failed on interface — skipping" interface=bridge-test-nonexistent0 location=http://192.168.77.1:7790`,
			`msg="DLNA server started"`,
			"ssdpInterfaces=1 ",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("the log has no %q:\n%s", want, got)
			}
		}
	})
	t.Run("none_binds", func(t *testing.T) {
		s, _ := goldenServer(t, goldenUDN(t), ServerConfig{})
		l := s.cfg.Listener.(*net.TCPListener)
		observed := &closeObservedListener{Listener: l, closed: make(chan struct{})}
		s.cfg.Listener = observed
		t.Cleanup(func() { _ = s.Stop(context.Background()) })
		s.cfg.AdvertiseEndpoints = []AdvertiseEndpoint{{Interface: unboundInterface(), ServerURL: "http://192.168.77.1:7790"}}
		err := s.Start(context.Background())
		if err == nil {
			_ = s.Stop(context.Background())
			t.Fatal("Start succeeded with no endpoint that binds")
		}
		if want := "dlna: SSDP start: no advertiser could bind (tried 1 endpoint(s))"; err.Error() != want {
			t.Errorf("Start's error is %q, want %q", err, want)
		}
		// Start owns rollback: callers need not Stop after it fails. Observe
		// this socket before Stop can mask a leak, never its reusable port.
		// Serve may still be entering on its goroutine. Wait for the real
		// Close without expiring its Accept and making Serve close it itself.
		select {
		case <-observed.closed:
			_ = l.SetDeadline(time.Now().Add(5 * time.Second))
			if c, acceptErr := l.Accept(); !errors.Is(acceptErr, net.ErrClosed) {
				if c != nil {
					_ = c.Close()
				}
				t.Errorf("the HTTP listener after the failed Start: Accept = %v, want net.ErrClosed", acceptErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("the HTTP listener was not closed by the failed Start")
		}
		_ = s.Stop(context.Background())
	})
}
