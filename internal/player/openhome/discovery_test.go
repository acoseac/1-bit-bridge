package openhome

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/dlna/ssdptest"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/player/lan/lantest"
)

func discoveryRig(t *testing.T, addresses ...lan.Address) (*Surface, *lantest.Box, *ssdptest.Network) {
	t.Helper()
	box := lantest.NewMemory(t)
	box.Set(addresses)
	network := ssdptest.New()
	subsystem := dlna.NewSSDPSubsystem(context.Background(), dlna.SSDPSubsystemOptions{Network: network.Transport()})
	t.Cleanup(func() { _ = subsystem.Close(context.Background()) })
	s, err := Start(Config{Device: Device{UUID: testUUID, ModelNumber: "test"}, Addresses: box.Addresses, Listen: box.Listen, SSDP: subsystem, Limits: Limits{Rescan: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop() })
	awaitStatus(t, s, 5*time.Second, "first discovery transaction", func(st Status) bool { return st.Reconciles > 0 })
	return s, box, network
}

func discoveryScan(t *testing.T, s *Surface) {
	t.Helper()
	before := s.Status().Reconciles
	s.Rescan()
	awaitStatus(t, s, 5*time.Second, "discovery transaction", func(st Status) bool { return st.Reconciles > before })
}

func packetHeader(packet, key string) string {
	for _, line := range strings.Split(packet, "\r\n") {
		if k, v, ok := strings.Cut(line, ": "); ok && strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}
func packets(network *ssdptest.Network, key, value string) []ssdptest.Write {
	var out []ssdptest.Write
	for _, w := range network.Snapshot() {
		if packetHeader(w.Pkt, key) == value {
			out = append(out, w)
		}
	}
	return out
}
func awaitPackets(t *testing.T, n *ssdptest.Network, key, value string, count int) []ssdptest.Write {
	t.Helper()
	give := time.After(5 * time.Second)
	for {
		changed := n.Changed()
		out := packets(n, key, value)
		if len(out) >= count {
			return out
		}
		select {
		case <-changed:
		case <-give:
			t.Fatalf("wanted %d %s=%s packets, got %d", count, key, value, len(out))
		}
	}
}
func searchSource(n *ssdptest.Network, t *testing.T, st, src string, meta dlna.SSDPDatagram) {
	n.OpenListener(t, "eth0").Deliver("M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\nST: "+st+"\r\n\r\n", netip.MustParseAddrPort(src), meta)
}
func sourceGroup() dlna.SSDPDatagram {
	return dlna.SSDPDatagram{Destination: netip.MustParseAddr("239.255.255.250"), DestinationValid: true, ArrivalIndex: 2, ArrivalValid: true}
}

// TestSourceDiscoveryIdentityAndSearches reaches the real registration and HTTP
// surface through the LOCATION in its packets, with every action still 602.
func TestSourceDiscoveryIdentityAndSearches(t *testing.T) {
	s, box, n := discoveryRig(t, eth0, eth0b)
	alive := packets(n, "NTS", "ssdp:alive")
	expected := []string{"upnp:rootdevice", "uuid:" + testUUID, DeviceType,
		"urn:av-openhome-org:service:Product:1", "urn:av-openhome-org:service:Playlist:1", "urn:av-openhome-org:service:Info:1", "urn:av-openhome-org:service:Time:1"}
	if len(alive) != 14 {
		t.Fatalf("alive targets: %d", len(alive))
	}
	for i, w := range alive {
		nt := expected[i%7]
		usn := "uuid:" + testUUID
		if nt != usn {
			usn += "::" + nt
		}
		if packetHeader(w.Pkt, "NT") != nt || packetHeader(w.Pkt, "USN") != usn {
			t.Fatalf("target %d: %s", i, w.Pkt)
		}
		if packetHeader(w.Pkt, "SERVER") != "Darwin/UPnP/1.0 1-bit-bridge/test" {
			t.Fatalf("SERVER: %s", w.Pkt)
		}
	}
	if sourceServerToken("") != "Darwin/UPnP/1.0 1-bit-bridge/dev" {
		t.Fatal("empty version must be dev")
	}
	st := s.Status()
	for i, l := range st.Listening {
		location := packetHeader(alive[i*7].Pkt, "LOCATION")
		if location != "http://"+l.Bound.String()+"/openhome/description.xml" {
			t.Fatalf("LOCATION %s != bound %s", location, l.Bound)
		}
		src := l.Address.Prefix.Addr().Next()
		response, err := box.Client(src).Get(location)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 || !strings.Contains(string(body), "uuid:"+testUUID) || !strings.Contains(string(body), "http://"+l.Bound.String()+"/openhome/product/control") {
			t.Fatalf("description: %s", body)
		}
	}
	if st.Listening[0].Bound.Port() == st.Listening[1].Bound.Port() {
		t.Fatal("fixture must prove distinct ephemeral ports")
	}
	client := box.Client(phone)
	req, _ := http.NewRequest("POST", "http://"+st.Listening[0].Bound.String()+"/openhome/product/control", strings.NewReader(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:SourceIndex xmlns:u="urn:av-openhome-org:service:Product:1"/></s:Body></s:Envelope>`))
	req.Header.Set("Content-Type", "text/xml")
	req.Header.Set("SOAPAction", `"urn:av-openhome-org:service:Product:1#SourceIndex"`)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 500 || !strings.Contains(string(body), "<errorCode>602</errorCode>") {
		t.Fatalf("SOAP: %d %s", response.StatusCode, body)
	}
	for _, target := range expected {
		searchSource(n, t, target, "192.168.1.50:50000", sourceGroup())
		awaitPackets(t, n, "ST", target, 1)
	}
	searchSource(n, t, "ssdp:all", "10.0.0.50:50000", sourceGroup())
	awaitPackets(t, n, "ST", "upnp:rootdevice", 2)
	// Withdraw joins every answer, including the remainder of ssdp:all.
	// Wait for its last target before cancellation, which intentionally cuts sends.
	awaitPackets(t, n, "ST", expected[6], 2)
	searchSource(n, t, "uuid:foreign", "192.168.1.50:50000", sourceGroup())
	searchSource(n, t, "urn:schemas-upnp-org:device:MediaRenderer:1", "192.168.1.50:50000", sourceGroup())
	s.Stop()
	if got := len(packets(n, "NTS", "ssdp:byebye")); got != 14 {
		t.Fatalf("byebye targets: %d", got)
	}
	replies := 0
	for _, w := range n.Snapshot() {
		if w.Kind == "reply" {
			replies++
			if strings.Contains(w.To, "10.0.0.50") && packetHeader(w.Pkt, "LOCATION") != "http://"+st.Listening[1].Bound.String()+"/openhome/description.xml" {
				t.Fatalf("wrong alias: %s", w.Pkt)
			}
		}
	}
	if replies != 14 {
		t.Fatalf("searches wrote %d replies", replies)
	}
	for _, from := range n.ReplySources() {
		if from.Interface.Index != 2 || (from.Local != eth0.Prefix && from.Local != eth0b.Prefix) {
			t.Fatalf("reply lost source/interface: %+v", from)
		}
	}
	if a, b, c := n.OpenSockets(); a+b+c != 0 {
		t.Fatalf("owned sockets after stop: %d/%d/%d", a, b, c)
	}
}

// TestSourceAdmissionUsesTheCurrentBoundInventory pins each source proof alone,
// including an Own-only change without a listener replacement.
func TestSourceAdmissionUsesTheCurrentBoundInventory(t *testing.T) {
	s, box, _ := discoveryRig(t, eth0, eth0b)
	ep := s.discovery.view.Load().endpoints[0]
	for _, tc := range []struct {
		name, source string
		want         bool
	}{
		{"on-link", "192.168.1.50", true}, {"IPv6", "::1", false}, {"loopback", "127.0.0.1", false},
		{"own", "192.168.1.20", false}, {"other-prefix", "10.0.0.50", false}, {"off-link", "172.16.1.2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := s.discovery.admits(dlna.SSDPSearch{Endpoint: ep, Datagram: dlna.SSDPDatagram{Source: netip.AddrPortFrom(netip.MustParseAddr(tc.source), 50000)}})
			if got != tc.want {
				t.Fatalf("admitted=%v", got)
			}
		})
	}
	// A broad prefix must not turn public space into a LAN source.
	broad := eth0
	broad.Prefix = netip.MustParsePrefix("192.168.1.20/0")
	box.Set([]lan.Address{broad})
	discoveryScan(t, s)
	ep = s.discovery.view.Load().endpoints[0]
	if s.discovery.admits(dlna.SSDPSearch{Endpoint: ep, Datagram: dlna.SSDPDatagram{Source: netip.MustParseAddrPort("8.8.8.8:50000")}}) {
		t.Fatal("non-LAN source admitted")
	}
	before := s.Status().Listening[0].Bound
	box.Set([]lan.Address{broad}, phone)
	discoveryScan(t, s)
	if s.Status().Listening[0].Bound != before {
		t.Fatal("Own-only change rebound HTTP")
	}
	if s.discovery.admits(dlna.SSDPSearch{Endpoint: ep, Datagram: dlna.SSDPDatagram{Source: netip.AddrPortFrom(phone, 50000)}}) {
		t.Fatal("Own-only change left stale admission")
	}
	box.Set(nil)
	discoveryScan(t, s)
	if s.discovery.admits(dlna.SSDPSearch{Endpoint: ep, Datagram: dlna.SSDPDatagram{Source: netip.AddrPortFrom(tablet, 50000)}}) {
		t.Fatal("unbound endpoint admitted")
	}
}

// TestSourceDiscoveryFollowsAddressTransactions pins late/lost/returned addresses,
// survivor re-alive, interface lives, and listing failures keeping the last view.
func TestSourceDiscoveryFollowsAddressTransactions(t *testing.T) {
	s, box, n := discoveryRig(t)
	select {
	case <-s.Ready():
		t.Fatal("empty surface ready")
	default:
	}
	if a, b, c := n.OpenSockets(); a+b+c != 0 {
		t.Fatal("empty set opened SSDP")
	}
	box.Set([]lan.Address{eth0, eth1})
	discoveryScan(t, s)
	select {
	case <-s.Ready():
	default:
		t.Fatal("successful start not signalled")
	}
	view := s.discovery.view.Load()
	if len(view.endpoints) != 2 || view.endpoints[0].Interface.Incarnation != 0 {
		t.Fatalf("initial view: %+v", view)
	}
	box.FailListing(errors.New("listing failed"))
	discoveryScan(t, s)
	if !slices.Equal(s.discovery.view.Load().endpoints, view.endpoints) {
		t.Fatal("failed listing changed discovery")
	}
	start := len(n.Snapshot())
	aliveBefore := len(packets(n, "NTS", "ssdp:alive"))
	box.Set([]lan.Address{eth1})
	discoveryScan(t, s)
	awaitPackets(t, n, "NTS", "ssdp:alive", aliveBefore+7)
	writes := n.Snapshot()[start:]
	if len(writes) < 14 {
		t.Fatalf("missing withdrawal/survivor alive: %v", writes)
	}
	for i, w := range writes[:14] {
		want := "ssdp:byebye"
		if i >= 7 {
			want = "ssdp:alive"
		}
		if packetHeader(w.Pkt, "NTS") != want {
			t.Fatal("alive before byebye")
		}
	}
	box.Set([]lan.Address{eth0, eth1})
	discoveryScan(t, s)
	for _, ep := range s.discovery.view.Load().endpoints {
		if ep.Interface.Name == "eth0" && ep.Interface.Incarnation != 1 {
			t.Fatalf("recreated interface reused life: %+v", ep)
		}
	}
	box.Set([]lan.Address{eth0, eth0b, eth1})
	discoveryScan(t, s)
	for _, ep := range s.discovery.view.Load().endpoints {
		if ep.Interface.Name == "eth0" && ep.Interface.Incarnation != 1 {
			t.Fatal("alias edit advanced life")
		}
	}
	box.Set(nil)
	discoveryScan(t, s)
	if a, b, c := n.OpenSockets(); a+b+c != 0 {
		t.Fatal("empty set did not withdraw")
	}
	box.Set([]lan.Address{eth0})
	discoveryScan(t, s)
	if a, b, _ := n.OpenSockets(); a != 1 || b != 1 {
		t.Fatal("return reused a withdrawn handle")
	}
}

// TestSourceDiscoveryRetriesHTTPAndSSDPWithoutAddressChanges pins partial and
// total failures: only active endpoints advertise, unchanged snapshots retry.
func TestSourceDiscoveryRetriesHTTPAndSSDPWithoutAddressChanges(t *testing.T) {
	for _, all := range []bool{false, true} {
		name := "partial"
		if all {
			name = "all"
		}
		t.Run(name, func(t *testing.T) {
			s, box, n := discoveryRig(t)
			box.FailListen(eth0.Prefix.Addr(), errors.New("bind refused"))
			n.Configure(func(n *ssdptest.Network) { n.PinErr["eth1"] = errors.New("pin refused") })
			addresses := []lan.Address{eth0, eth1}
			if !all {
				addresses = append(addresses, cable)
			}
			box.Set(addresses)
			discoveryScan(t, s)
			for _, w := range packets(n, "NTS", "ssdp:alive") {
				if w.Iface != "eth2" {
					t.Fatalf("advertised failed endpoint: %s", w.Pkt)
				}
			}
			n.Configure(func(n *ssdptest.Network) { delete(n.PinErr, "eth1") })
			// First recover SSDP alone, with exactly the same HTTP set.
			discoveryScan(t, s)
			recovered := false
			for _, w := range packets(n, "NTS", "ssdp:alive") {
				if w.Iface == "eth1" {
					recovered = true
				}
			}
			if !recovered {
				t.Fatal("unchanged bound set did not retry SSDP")
			}
			box.FailListen(eth0.Prefix.Addr(), nil)
			discoveryScan(t, s)
			if len(s.Status().Listening) != len(addresses) {
				t.Fatal("HTTP bind was not retried")
			}
			for _, iface := range []string{"eth0", "eth1"} {
				found := false
				for _, w := range packets(n, "NTS", "ssdp:alive") {
					if w.Iface == iface {
						found = true
					}
				}
				if !found {
					t.Fatalf("SSDP did not recover %s", iface)
				}
			}
		})
	}
}

// TestSourceWithdrawsBeforeHTTPCloseAndRefusesWhileParked orders the production
// phase boundary with a barrier inside the real subsystem's byebye write.
func TestSourceWithdrawsBeforeHTTPCloseAndRefusesWhileParked(t *testing.T) {
	s, box, n := discoveryRig(t, eth0)
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var once sync.Once
	n.Configure(func(n *ssdptest.Network) {
		n.OnSenderWrite = func(_ *ssdptest.Sender, pkt string) {
			if packetHeader(pkt, "NTS") == "ssdp:byebye" {
				once.Do(func() { close(entered); <-release })
			}
		}
	})
	done := make(chan StopReport, 1)
	go func() { done <- s.Stop() }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no byebye")
	}
	if len(box.Open()) != 1 {
		t.Fatal("HTTP closed before byebye")
	}
	response, err := box.Client(phone).Get("http://" + box.Open()[0].String() + "/openhome/description.xml")
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 503 || !response.Close {
		close(release)
		t.Fatalf("withdrawal window: %d close=%v", response.StatusCode, response.Close)
	}
	if _, ok := response.Header["Ext"]; !ok {
		close(release)
		t.Fatal("503 lost EXT")
	}
	close(release)
	select {
	case rep := <-done:
		if rep.Abandoned != 0 {
			t.Fatalf("abandoned: %+v", rep)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not join")
	}
	if len(box.Open()) != 0 {
		t.Fatal("HTTP remained open")
	}
	if a, b, c := n.OpenSockets(); a+b+c != 0 {
		t.Fatal("SSDP remained open")
	}
}

// TestSourceRemovalWithdrawsBeforeRebinding pins same-address prefix replacement.
func TestSourceRemovalWithdrawsBeforeRebinding(t *testing.T) {
	s, box, n := discoveryRig(t, eth0)
	old := box.Open()[0]
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var once sync.Once
	n.Configure(func(n *ssdptest.Network) {
		n.OnSenderWrite = func(_ *ssdptest.Sender, pkt string) {
			if packetHeader(pkt, "NTS") == "ssdp:byebye" {
				once.Do(func() { close(entered); <-release })
			}
		}
	})
	changed := eth0
	changed.Prefix = netip.MustParsePrefix("192.168.1.20/25")
	box.Set([]lan.Address{changed})
	s.Rescan()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no withdrawal")
	}
	if !slices.Equal(box.Open(), []netip.AddrPort{old}) || len(box.Listens()) != 1 {
		close(release)
		t.Fatal("old HTTP closed or replacement bound before withdrawal")
	}
	close(release)
	awaitStatus(t, s, 5*time.Second, "prefix replacement", func(st Status) bool {
		return st.Reconciles >= 2 && len(st.Listening) == 1 && st.Listening[0].Address == changed
	})
	if len(box.Listens()) != 2 || box.Open()[0] == old {
		t.Fatal("replacement was not rebound")
	}
}

// TestSourceStopWinsAStalledBind proves a late HTTP success cannot advertise.
func TestSourceStopWinsAStalledBind(t *testing.T) {
	box := lantest.NewMemory(t)
	box.Set([]lan.Address{eth0})
	n := ssdptest.New()
	sub := dlna.NewSSDPSubsystem(context.Background(), dlna.SSDPSubsystemOptions{Network: n.Transport()})
	defer sub.Close(context.Background())
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	s, err := Start(Config{Device: Device{UUID: testUUID}, Addresses: box.Addresses, SSDP: sub, Limits: Limits{Join: 20 * time.Millisecond}, Listen: func(ctx context.Context, at netip.AddrPort, a lan.Address) (net.Listener, string, error) {
		close(entered)
		<-release
		return box.Listen(ctx, at, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	<-entered
	rep := s.Stop()
	if rep.Abandoned != 1 {
		t.Fatalf("stalled listing accounting: %+v", rep)
	}
	close(release)
	select {
	case <-s.loopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("late bind did not end")
	}
	if len(box.Open()) != 0 || len(n.Snapshot()) != 0 {
		t.Fatal("late bind revived discovery or kept HTTP")
	}
	select {
	case <-s.Ready():
		t.Fatal("cancelled start signalled ready")
	default:
	}
}

func TestSourceStopWinsInFlightDiscovery(t *testing.T) {
	for _, phase := range []string{"Register", "Update"} {
		t.Run(phase, func(t *testing.T) {
			s, box, n := discoveryRig(t)
			if phase == "Update" {
				box.Set([]lan.Address{eth0})
				discoveryScan(t, s)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			var once sync.Once
			n.Configure(func(n *ssdptest.Network) { n.OnListen = func(string) { once.Do(func() { close(entered); <-release }) } })
			box.Set([]lan.Address{eth1})
			s.Rescan()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("registration did not park")
			}
			before := len(packets(n, "NTS", "ssdp:alive"))
			s.Refuse(context.Background())
			done := make(chan struct{})
			go func() { s.Stop(); close(done) }()
			close(release)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("stop did not join late operation")
			}
			if len(packets(n, "NTS", "ssdp:alive")) != before {
				t.Fatal("late discovery completion advertised")
			}
			if a, b, c := n.OpenSockets(); a+b+c != 0 {
				t.Fatal("late completion retained sockets")
			}
			if len(box.Open()) != 0 {
				t.Fatal("late completion retained HTTP")
			}
		})
	}
}

func TestSourceDiscoveryEndsWithItsSurfaceGeneration(t *testing.T) {
	for _, reason := range []string{"HTTP-exit", "listing-panic", "restart"} {
		t.Run(reason, func(t *testing.T) {
			box := lantest.NewMemory(t)
			box.Set([]lan.Address{eth0})
			n := ssdptest.New()
			sub := dlna.NewSSDPSubsystem(context.Background(), dlna.SSDPSubsystemOptions{Network: n.Transport()})
			defer sub.Close(context.Background())
			var panicListing bool
			var mu sync.Mutex
			bound := make(chan net.Listener, 2)
			cfg := Config{Device: Device{UUID: testUUID}, SSDP: sub, Limits: Limits{Rescan: time.Hour},
				Addresses: func() (lan.Inventory, error) {
					mu.Lock()
					p := panicListing
					mu.Unlock()
					if p {
						panic("test")
					}
					return box.Addresses()
				},
				Listen: func(ctx context.Context, at netip.AddrPort, a lan.Address) (net.Listener, string, error) {
					ln, r, e := box.Listen(ctx, at, a)
					if e == nil {
						bound <- ln
					}
					return ln, r, e
				},
			}
			s, err := Start(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Stop()
			awaitStatus(t, s, 5*time.Second, "initial generation", func(st Status) bool { return st.Reconciles > 0 })
			first := <-bound
			switch reason {
			case "HTTP-exit":
				box.FailListen(eth0.Prefix.Addr(), errors.New("remains down"))
				_ = first.Close()
			case "listing-panic":
				mu.Lock()
				panicListing = true
				mu.Unlock()
				s.Rescan()
			case "restart":
				s.Stop()
			}
			awaitPackets(t, n, "NTS", "ssdp:byebye", 7)
			if reason == "HTTP-exit" {
				awaitStatus(t, s, 5*time.Second, "HTTP exit", func(st Status) bool { return st.Reconciles > 1 && len(st.Listening) == 0 })
			}
			s.Stop()
			if a, b, c := n.OpenSockets(); a+b+c != 0 {
				t.Fatal("ended generation retained SSDP")
			}
			if reason == "restart" {
				next, err := Start(cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer next.Stop()
				awaitStatus(t, next, 5*time.Second, "next generation", func(st Status) bool { return st.Reconciles > 0 })
				select {
				case <-next.Ready():
				default:
					t.Fatal("later generation has no success signal")
				}
				if got := len(packets(n, "NTS", "ssdp:alive")); got != 14 {
					t.Fatalf("later generation did not register fresh: %d", got)
				}
			}
		})
	}
}

func TestSourceRefusalsAcquireNoReplySocket(t *testing.T) {
	s, _, n := discoveryRig(t, eth0, eth0b)
	for _, tc := range []struct {
		name, src string
		meta      dlna.SSDPDatagram
	}{
		{"missing-destination", "192.168.1.50:50000", dlna.SSDPDatagram{ArrivalValid: true, ArrivalIndex: 2}},
		{"missing-arrival", "192.168.1.50:50000", dlna.SSDPDatagram{DestinationValid: true, Destination: netip.MustParseAddr("239.255.255.250")}},
		{"wrong-arrival", "192.168.1.50:50000", dlna.SSDPDatagram{DestinationValid: true, Destination: netip.MustParseAddr("239.255.255.250"), ArrivalValid: true, ArrivalIndex: 3}},
		{"wrong-destination", "192.168.1.50:50000", dlna.SSDPDatagram{DestinationValid: true, Destination: netip.MustParseAddr("192.168.1.99"), ArrivalValid: true, ArrivalIndex: 2}},
		{"unicast-no-alias-fallback", "10.0.0.50:50000", dlna.SSDPDatagram{DestinationValid: true, Destination: eth0.Prefix.Addr(), ArrivalValid: true, ArrivalIndex: 2}},
		{"loopback", "127.0.0.1:50000", sourceGroup()},
		{"own", "10.0.0.2:50000", sourceGroup()},
		{"off-link", "172.16.0.2:50000", sourceGroup()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A positive search queued behind the refusal fences the reader. Joining
			// the resulting burst then verifies the negative made no reply socket.
			before := len(n.ReplySources())
			count := len(packets(n, "ST", "upnp:rootdevice"))
			searchSource(n, t, "upnp:rootdevice", tc.src, tc.meta)
			searchSource(n, t, "upnp:rootdevice", "192.168.1.50:50000", sourceGroup())
			awaitPackets(t, n, "ST", "upnp:rootdevice", count+1)
			if len(n.ReplySources()) != before+1 {
				t.Fatal("refusal created a reply socket")
			}
		})
	}
	s.Stop()
}

func TestSourceWaitsForBoundHTTPAndRepeatsItsTargets(t *testing.T) {
	box := lantest.NewMemory(t)
	box.Set([]lan.Address{eth0})
	n := ssdptest.New()
	sub := dlna.NewSSDPSubsystem(context.Background(), dlna.SSDPSubsystemOptions{Network: n.Transport()})
	defer sub.Close(context.Background())
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	s, err := Start(Config{Device: Device{UUID: testUUID}, Addresses: box.Addresses, SSDP: sub, Seams: &Seams{AdvertiseInterval: 10 * time.Millisecond},
		Listen: func(ctx context.Context, at netip.AddrPort, a lan.Address) (net.Listener, string, error) {
			close(entered)
			<-release
			return box.Listen(ctx, at, a)
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	<-entered
	if len(n.Snapshot()) != 0 {
		close(release)
		t.Fatal("Start return advertised an unbound address")
	}
	select {
	case <-s.Ready():
		close(release)
		t.Fatal("Start return signalled readiness")
	default:
	}
	close(release)
	alive := awaitPackets(t, n, "NTS", "ssdp:alive", 14)
	for i := 0; i < 7; i++ {
		if alive[i].Pkt != alive[i+7].Pkt {
			t.Fatal("repeat changed targets or identity")
		}
	}
	s.Stop()
	if len(packets(n, "NTS", "ssdp:byebye")) != 7 {
		t.Fatal("repeat lifetime lost withdrawal")
	}
	before := len(n.Snapshot())
	s.Rescan()
	s.Stop()
	if len(n.Snapshot()) != before {
		t.Fatal("stopped generation revived")
	}
}

func TestSourceAdmissionProofOrder(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, source, want string
		own                        bool
	}{
		{"IPv4", "::/0", "::1", refusedNotIPv4, false},
		{"loopback", "127.0.0.1/8", "127.0.0.2", refusedLoopback, false},
		{"Own", "192.168.1.20/24", "192.168.1.50", refusedOwnAddress, true},
		{"prefix", "192.168.1.20/24", "10.0.0.50", refusedOutsidePrefix, false},
		{"LAN", "192.168.1.20/0", "8.8.8.8", refusedNotLAN, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := netip.MustParseAddr(tc.source)
			got := sourceRefusal(netip.MustParsePrefix(tc.prefix), src, true, tc.own)
			if got != tc.want {
				t.Fatalf("proof gave %q want %q", got, tc.want)
			}
		})
	}
}

func TestSourceInterfaceReplacementKeepsOnlyTheCurrentIdentity(t *testing.T) {
	s, box, n := discoveryRig(t, eth0)
	for _, replacement := range []lan.Address{
		lantest.Address("192.168.1.20/24", "renamed", 2),
		lantest.Address("192.168.1.20/24", "renamed", 8),
	} {
		old := s.discovery.view.Load().endpoints[0]
		box.Set([]lan.Address{replacement})
		discoveryScan(t, s)
		ep := s.discovery.view.Load().endpoints[0]
		if ep.Interface.Name != replacement.Interface || ep.Interface.Index != replacement.Index || ep.Location == old.Location {
			t.Fatalf("replacement kept stale endpoint: %+v", ep)
		}
		if s.discovery.admits(dlna.SSDPSearch{Endpoint: old, Datagram: dlna.SSDPDatagram{Source: netip.AddrPortFrom(phone, 50000)}}) {
			t.Fatal("old interface still admitted")
		}
		if a, b, c := n.OpenSockets(); a != 1 || b != 1 || c != 0 {
			t.Fatalf("replacement retained sockets: %d/%d/%d", a, b, c)
		}
	}
}
