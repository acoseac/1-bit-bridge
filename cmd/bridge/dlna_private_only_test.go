package main

import (
	"context"
	"encoding/xml"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/dlna/ssdptest"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/player/lan/lantest"
)

// mediaServerLANFlags are the flags a wired or Wi-Fi adapter reports (eth0 on
// Linux: UP,BROADCAST,RUNNING,MULTICAST).
const mediaServerLANFlags = net.FlagUp | net.FlagBroadcast | net.FlagMulticast | net.FlagRunning

// hostIface is one interface of a host a test stands in for the
// MediaServer's wiring: its name, its flags and its addresses with their
// prefixes, in the order the OS lists them.
type hostIface struct {
	name  string
	flags net.Flags
	cidrs []string
}

// standInHost turns interfaces, in the order the OS would list them, into
// the listing and the address reader dlnaBootNetwork takes. Each interface's
// index is its place in the list plus two (1 is the loopback's on Linux).
func standInHost(t *testing.T, ifs ...hostIface) (func() ([]net.Interface, error), func(*net.Interface) ([]net.Addr, error)) {
	t.Helper()
	ifaces := make([]net.Interface, len(ifs))
	byIndex := map[int][]net.Addr{}
	for i, f := range ifs {
		ifaces[i] = net.Interface{Index: i + 2, Name: f.name, Flags: f.flags}
		for _, c := range f.cidrs {
			ip, n, err := net.ParseCIDR(c)
			if err != nil {
				t.Fatalf("parse %s: %v", c, err)
			}
			n.IP = ip
			byIndex[i+2] = append(byIndex[i+2], n)
		}
	}
	return func() ([]net.Interface, error) { return slices.Clone(ifaces), nil },
		func(ifi *net.Interface) ([]net.Addr, error) { return slices.Clone(byIndex[ifi.Index]), nil }
}

// standInListener is the MediaServer's listener as a test holds it: it
// accepts on lantest listeners, each standing for one address of the host,
// and reports the address the real bind would (the unspecified address of a
// wildcard, or the address a listener pinned to one binds).
type standInListener struct {
	addr  net.Addr
	inner []net.Listener
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newStandInListener(t *testing.T, addr net.Addr, inner ...net.Listener) *standInListener {
	t.Helper()
	l := &standInListener{addr: addr, inner: inner, conns: make(chan net.Conn), done: make(chan struct{})}
	for _, in := range inner {
		go func() {
			for {
				c, err := in.Accept()
				if err != nil {
					return
				}
				select {
				case l.conns <- c:
				case <-l.done:
					_ = c.Close()
					return
				}
			}
		}()
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func (l *standInListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *standInListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		for _, in := range l.inner {
			_ = in.Close()
		}
	})
	return nil
}

func (l *standInListener) Addr() net.Addr { return l.addr }

// mediaServerRig is the MediaServer startDLNAIfEnabled started on a host of a
// test's own, and what the test reaches it through.
type mediaServerRig struct {
	lc      *dlnaLifecycle
	enabled bool
	box     *lantest.Box
	ssdp    *ssdptest.Network
	log     *loggingtest.Recorder
	port    uint16
	// id and body are the library's one track: its DLNA id and its bytes.
	id   string
	body []byte
}

// startMediaServerOn runs startDLNAIfEnabled, the MediaServer's real wiring,
// on a host of the test's own: ifs, listed in the order given, which the
// wiring's selection and the server's served addresses read
// (dlnaBootNetwork); a listener that stands for dlna.listenAddress
// (listenAddress), accepting connections that arrive on each address of at;
// and an SSDP network that records what is sent. The library holds one
// track.
func startMediaServerOn(t *testing.T, listenAddress string, at []string, ifs ...hostIface) *mediaServerRig {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Music")
	if err := os.MkdirAll(filepath.Join(root, "Glasswing", "Harbour"), 0o755); err != nil {
		t.Fatal(err)
	}
	const rel = "Glasswing/Harbour/01 Harbour Light.flac"
	body := []byte("fLaC-the-one-track-of-this-library")
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), body, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertTrack(context.Background(), &manifest.Track{Path: rel, Size: int64(len(body)), ModTime: time.Now(),
		Title: "Harbour Light", Artist: "Glasswing", Album: "Harbour", Codec: "FLAC"}); err != nil {
		t.Fatal(err)
	}

	host, portText, err := net.SplitHostPort(listenAddress)
	if err != nil {
		t.Fatalf("listen address %q: %v", listenAddress, err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		t.Fatalf("listen address %q: %v", listenAddress, err)
	}
	// What the real bind reports: the unspecified address of the family
	// the wildcard names, the address a pin names, unmapped, or for a pinned
	// name the address it resolved to, at's first.
	bound := netip.IPv6Unspecified()
	if host != "" {
		if a, err := netip.ParseAddr(host); err == nil {
			bound = a.Unmap()
		} else {
			bound = netip.MustParseAddr(at[0])
		}
	}
	box := lantest.NewMemory(t)
	var inner []net.Listener
	for _, a := range at {
		l, _, err := box.Listen(context.Background(), netip.AddrPortFrom(netip.MustParseAddr(a), uint16(port)), lan.Address{})
		if err != nil {
			t.Fatal(err)
		}
		inner = append(inner, l)
	}
	listener := newStandInListener(t, net.TCPAddrFromAddrPort(netip.AddrPortFrom(bound, uint16(port))), inner...)

	list, addrs := standInHost(t, ifs...)
	n := ssdptest.New()
	rec := loggingtest.NewRecorder()
	logger := rec.Logger()
	ctx, cancel := context.WithCancel(context.Background())
	subsystem := newServeSSDP(ctx, logger, n.Transport())
	cfg := &config.Config{LibraryRoots: []string{root}, DataDir: t.TempDir()}
	cfg.DLNA.Enabled = true
	cfg.DLNA.ListenAddress = listenAddress
	lc, enabled := startDLNAIfEnabled(ctx, dlnaWiring{
		Cfg:      cfg,
		Store:    store,
		Resolver: bridgefs.New([]string{root}),
		Logger:   logger,
		SSDP:     subsystem,
		Network:  &dlnaBootNetwork{Interfaces: list, Addrs: addrs, Listener: listener},
	})
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		lc.Stop(stopCtx)
		_ = subsystem.Close(stopCtx)
		cancel()
	})
	return &mediaServerRig{lc: lc, enabled: enabled, box: box, ssdp: n, log: rec, port: uint16(port),
		id: dlna.TrackID("", rel), body: body}
}

// at is the stand-in address a with the listener's port.
func (r *mediaServerRig) at(a string) netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr(a), r.port)
}

// request sends one request to the MediaServer from a client at from that
// reaches it on the stand-in address to, naming host in Host, and returns
// the status and the body.
func (r *mediaServerRig) request(t *testing.T, from, to, host, method, path, soapAction, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+r.at(to).String()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if soapAction != "" {
		req.Header.Set("SOAPAction", soapAction)
		req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	}
	resp, err := r.box.Client(netip.MustParseAddr(from)).Do(req)
	if err != nil {
		t.Fatalf("%s %s on %s (Host %s): %v", method, path, to, host, err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(got)
}

// browse lists the All Tracks container from a client at from that reaches
// the server on to, naming host in Host, and returns the status and every
// <res> URL the answer carries.
func (r *mediaServerRig) browse(t *testing.T, from, to, host string) (int, []string) {
	t.Helper()
	const envelope = `<?xml version="1.0" encoding="UTF-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">` +
		`<ObjectID>1</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><Filter>*</Filter>` +
		`<StartingIndex>0</StartingIndex><RequestedCount>0</RequestedCount><SortCriteria></SortCriteria>` +
		`</u:Browse></s:Body></s:Envelope>`
	status, body := r.request(t, from, to, host, http.MethodPost, "/dlna/cds/control",
		`"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`, envelope)
	if status != http.StatusOK {
		return status, nil
	}
	var env struct {
		Result string `xml:"Body>BrowseResponse>Result"`
	}
	if err := xml.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("an undecodable Browse answer: %v\n%s", err, body)
	}
	var didl struct {
		Items []struct {
			Res []string `xml:"res"`
		} `xml:"item"`
	}
	if err := xml.Unmarshal([]byte(env.Result), &didl); err != nil {
		t.Fatalf("an undecodable DIDL-Lite: %v\n%s", err, env.Result)
	}
	var res []string
	for _, it := range didl.Items {
		res = append(res, it.Res...)
	}
	return status, res
}

// bytesBack is what a client at from gets back on a connection that arrives
// on the stand-in address to and asks for the device description, naming
// host in Host: nothing at all once the listener has closed it unread.
func (r *mediaServerRig) bytesBack(t *testing.T, from, to, host string) int {
	t.Helper()
	c, err := r.box.Dial(r.at(to), netip.MustParseAddr(from))
	if err != nil {
		t.Fatalf("dial %s: %v", to, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	// A write the listener's close beat is the same refusal; the read says.
	_, _ = io.WriteString(c, "GET /dlna/description.xml HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n")
	got, _ := io.ReadAll(c)
	return len(got)
}

// ssdpHeader is a header of an SSDP packet, its name in any case.
func ssdpHeader(pkt, key string) string {
	for _, line := range strings.Split(pkt, "\r\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// aliveLocations waits for the MediaServer's first alive burst (one NOTIFY
// per target it advertises) and returns the LOCATION of every alive NOTIFY
// sent by then.
func (r *mediaServerRig) aliveLocations(t *testing.T) []string {
	t.Helper()
	var locations []string
	for _, w := range r.aliveNotifies(t, 1) {
		locations = append(locations, ssdpHeader(w.Pkt, "LOCATION"))
	}
	return locations
}

// aliveNotifies waits for the first alive burst of each of endpoints
// advertise endpoints (one NOTIFY per target each) and returns every alive
// NOTIFY sent by then.
func (r *mediaServerRig) aliveNotifies(t *testing.T, endpoints int) []ssdptest.Write {
	t.Helper()
	want := endpoints * len(dlna.NotifyTargetsFor("uuid:any"))
	give := serveGiveUp(t)
	for {
		changed := r.ssdp.Changed()
		var alive []ssdptest.Write
		for _, w := range r.ssdp.Snapshot() {
			if w.Kind == "notify" && ssdpHeader(w.Pkt, "NTS") == "ssdp:alive" {
				alive = append(alive, w)
			}
		}
		if len(alive) >= want {
			return alive
		}
		select {
		case <-changed:
		case <-give:
			t.Fatalf("%d alive NOTIFYs, want a burst of %d", len(alive), want)
		}
	}
}

// search delivers one unicast M-SEARCH for the root device, from src, to the
// listener on the interface named iface (index), as sent to the address dst.
func (r *mediaServerRig) search(t *testing.T, iface string, index int, src, dst string) {
	t.Helper()
	r.ssdp.OpenListener(t, iface).Deliver("M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\nST: upnp:rootdevice\r\n\r\n",
		netip.MustParseAddrPort(src), dlna.SSDPDatagram{Destination: netip.MustParseAddr(dst), DestinationValid: true, ArrivalIndex: index, ArrivalValid: true})
}

// awaitReplyTo waits for the answer to a search from src.
func (r *mediaServerRig) awaitReplyTo(t *testing.T, src string) ssdptest.Write {
	t.Helper()
	give := serveGiveUp(t)
	for {
		changed := r.ssdp.Changed()
		for _, w := range r.ssdp.Snapshot() {
			if w.Kind == "reply" && w.To == src {
				return w
			}
		}
		select {
		case <-changed:
		case <-give:
			t.Fatalf("no answer to the search from %s", src)
		}
	}
}

// refusedAt returns the address of every refusal of what ("a connection",
// "an M-SEARCH") the MediaServer logged.
func (r *mediaServerRig) refusedAt(what string) []string {
	var out []string
	for _, rec := range r.log.Records() {
		if !strings.HasPrefix(rec.Message, "DLNA refused "+what+" to an address it does not serve") {
			continue
		}
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "address" {
				out = append(out, a.Value.String())
			}
			return true
		})
	}
	return out
}

// TestTheMediaServerServesAMixedInterfaceOnItsPrivateAddressAlone is B687
// through the MediaServer's real wiring. One LAN interface carries a private
// IPv4 address and a public one, listed in either order, and the wiring's
// own selection runs over it (dlnaBootNetwork stands in the host, never the
// selection). Whatever the order, DLNA announces the private address, serves
// the library on it, closes a connection that arrives on the public address
// before a byte of an answer whatever Host it names, and answers no M-SEARCH
// sent to the public address. Until B687 the public-first host announced and
// served the public address: whoever could route to it browsed and streamed
// the library, unauthenticated. The private-first host was already right,
// and is here so that it stays so.
func TestTheMediaServerServesAMixedInterfaceOnItsPrivateAddressAlone(t *testing.T) {
	const (
		private   = "192.168.1.20"
		public    = "203.0.113.5"
		lanPhone  = "192.168.1.50"
		outsider  = "198.51.100.9"
		wantWhere = "http://192.168.1.20:7790/dlna/description.xml"
	)
	for _, tc := range []struct {
		name  string
		cidrs []string
	}{
		{"public_listed_first", []string{public + "/24", private + "/24", "fe80::20/64"}},
		{"private_listed_first", []string{private + "/24", public + "/24", "fe80::20/64"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := startMediaServerOn(t, "0.0.0.0:7790", []string{private, public},
				hostIface{"eth0", mediaServerLANFlags, tc.cidrs})
			if !rig.enabled {
				t.Fatalf("DLNA did not start on a host with a private address:\n%s", strings.Join(rig.log.All(), "\n"))
			}
			srv := rig.lc.server

			for _, location := range rig.aliveLocations(t) {
				if location != wantWhere {
					t.Errorf("an alive NOTIFY announces %s, want %s", location, wantWhere)
				}
			}
			if !srv.AcceptsOn(netip.MustParseAddr(private)) {
				t.Errorf("the listener does not take a connection on %s", private)
			}
			if srv.AcceptsOn(netip.MustParseAddr(public)) {
				t.Errorf("the listener takes a connection on the public address %s", public)
			}

			// The private address serves the library, its URLs on that address.
			status, res := rig.browse(t, lanPhone, private, private+":7790")
			wantRes := "http://" + private + ":7790/dlna/file/" + rig.id
			if status != http.StatusOK || !slices.Contains(res, wantRes) {
				t.Fatalf("Browse on %s: status %d, <res> %v, want 200 and %s", private, status, res, wantRes)
			}
			if status, got := rig.request(t, lanPhone, private, private+":7790", http.MethodGet, "/dlna/file/"+rig.id, "", ""); status != http.StatusOK || got != string(rig.body) {
				t.Errorf("GET of the track on %s: status %d, %d bytes, want 200 and its %d bytes", private, status, len(got), len(rig.body))
			}

			// The public address: closed unread, whatever Host it names.
			for _, host := range []string{private + ":7790", public + ":7790", ""} {
				if n := rig.bytesBack(t, outsider, public, host); n != 0 {
					t.Errorf("a connection on the public address naming Host %q got %d bytes back, want none", host, n)
				}
			}
			if got := rig.refusedAt("a connection"); !slices.Contains(got, public) {
				t.Errorf("no refused connection logged for %s: %v", public, got)
			}

			// SSDP: the search sent to the public address is read first, and
			// refused as it is read; the one sent to the private address is
			// answered, naming the private address.
			rig.search(t, "eth0", 2, outsider+":50000", public)
			rig.search(t, "eth0", 2, lanPhone+":50000", private)
			reply := rig.awaitReplyTo(t, lanPhone+":50000")
			if got := ssdpHeader(reply.Pkt, "LOCATION"); got != wantWhere {
				t.Errorf("the answer to a search of %s names %s, want %s", private, got, wantWhere)
			}
			if got := rig.refusedAt("an M-SEARCH"); !slices.Equal(got, []string{public}) {
				t.Errorf("M-SEARCH refusals logged for %v, want one, for %s", got, public)
			}
		})
	}
}

// TestTheMediaServerAnswersNoRequestNamingItsPublicAddress is B687's Host
// half through the real wiring: over a connection to the private address,
// which the listener serves, a request whose Host names the host's own
// public address is refused (421), and no Browse builds a <res> on it. Until
// B687 the Host check took every address of the host's interfaces, in either
// order, so a Browse naming the public address handed back the library's
// URLs on it.
func TestTheMediaServerAnswersNoRequestNamingItsPublicAddress(t *testing.T) {
	const private, public, lanPhone = "192.168.1.20", "203.0.113.5", "192.168.1.50"
	for _, tc := range []struct {
		name  string
		cidrs []string
	}{
		{"public_listed_first", []string{public + "/24", private + "/24"}},
		{"private_listed_first", []string{private + "/24", public + "/24"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := startMediaServerOn(t, "0.0.0.0:7790", []string{private},
				hostIface{"eth0", mediaServerLANFlags, tc.cidrs})
			if !rig.enabled {
				t.Fatalf("DLNA did not start:\n%s", strings.Join(rig.log.All(), "\n"))
			}
			for _, host := range []string{public + ":7790", public, "[::ffff:" + public + "]:7790"} {
				status, res := rig.browse(t, lanPhone, private, host)
				if status != http.StatusMisdirectedRequest {
					t.Errorf("Browse over %s naming Host %q: status %d, want 421 (<res> %v)", private, host, status, res)
				}
			}
			if status, _ := rig.browse(t, lanPhone, private, private+":7790"); status != http.StatusOK {
				t.Errorf("Browse over %s naming it: status %d, want 200", private, status)
			}
		})
	}
}

// TestTheMediaServerFallsBackToNoPublicAddress is the wiring's fallback: an
// interface eligible through a ULA whose one IPv4 address is public has no
// address the MediaServer may announce (SSDP is IPv4), so DLNA does not
// start, and nothing is announced. Until B687 its public IPv4 address was
// the LOCATION, and the listener served it.
func TestTheMediaServerFallsBackToNoPublicAddress(t *testing.T) {
	rig := startMediaServerOn(t, "0.0.0.0:7790", []string{"203.0.113.5"},
		hostIface{"eth0", mediaServerLANFlags, []string{"203.0.113.5/24", "fd12:3456::5/64", "fe80::5/64"}})
	if rig.enabled {
		t.Errorf("DLNA started on a host whose only IPv4 address is public")
	}
	for _, w := range rig.ssdp.Snapshot() {
		t.Errorf("sent %s to %s: %q", w.Kind, w.To, ssdpHeader(w.Pkt, "LOCATION"))
	}
}
