package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/dlna/ssdptest"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/player/lan/lantest"
	"github.com/acoseac/1-bit-bridge/internal/player/openhome"
)

// bootDiscoveryServe runs the real serve/engine with in-memory HTTP and SSDP.
// The observer returns an empty inventory without reading audio device trees.
func bootDiscoveryServe(t *testing.T, root string, media bool, playerBlock, goos string, box *lantest.Box, n *ssdptest.Network, with func(*serveOpts)) (*playerServe, <-chan *openhome.Surface) {
	t.Helper()
	music, data := filepath.Join(root, "Music"), filepath.Join(root, "data")
	for _, p := range []string{music, data} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	httpBox := lantest.NewMemory(t)
	bind := func(at string) net.Listener {
		ln, _, err := httpBox.Listen(context.Background(), netip.MustParseAddrPort(at), lan.Address{})
		if err != nil {
			t.Fatal(err)
		}
		return ln
	}
	api, admin := bind("127.0.0.1:0"), bind("127.0.0.1:0")
	cfg := filepath.Join(root, "bridge.yaml")
	body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\nlistenAddress: %s\nadminAddress: %s\ndisableHttp3: true\nmdns:\n  enabled: false\ndlna:\n  enabled: %t\n  listenAddress: 0.0.0.0:7790\noutput:\n  mpd: off\n  player:\n%s", music, data, api.Addr(), admin.Addr(), media, playerBlock)
	if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	ps := &playerServe{music: music, dataDir: data, configPath: cfg, standIn: newPlayerStandIn()}
	started := make(chan *openhome.Surface, 1)
	pboot := make(chan *bridgePlayer, 1)
	iface := net.Interface{Index: 2, Name: "eth0", Flags: net.FlagUp | net.FlagMulticast}
	_, prefix, _ := net.ParseCIDR("192.168.1.20/24")
	prefix.IP = net.ParseIP("192.168.1.20")
	opts := serveOpts{configPath: cfg, lanListener: api, adminListener: admin, playerGOOS: goos,
		playerBackend: ps.standIn.build, playerBoot: func(p *bridgePlayer) { pboot <- p },
		hotplugSource: func(context.Context) hotplug.Source { return hotplug.PollingSource(hearsNothing) },
		hotplugObserve: func(func(context.Context) (binding.Inventory, error)) func(context.Context) (binding.Inventory, error) {
			return func(context.Context) (binding.Inventory, error) {
				return binding.Inventory{Sysfs: true, CardsComplete: true}, nil
			}
		},
		ssdpNetwork:    n.Transport(),
		playerOpenHome: openhomeWiring{Addresses: box.Addresses, Listen: box.Listen, Limits: openhome.Limits{Rescan: time.Hour}, Started: func(s *openhome.Surface) { started <- s }},
		dlnaNetwork: &dlnaBootNetwork{
			Pick: func() (*net.Interface, error) { return &iface, nil },
			Gather: func(bool, int, *slog.Logger) []dlna.AdvertiseEndpoint {
				return []dlna.AdvertiseEndpoint{{Interface: &iface, ServerURL: "http://192.168.1.20:7790"}}
			},
			Interfaces: func() ([]net.Interface, error) { return []net.Interface{iface}, nil },
			Addrs:      func(*net.Interface) ([]net.Addr, error) { return []net.Addr{prefix}, nil },
		},
	}
	if media {
		opts.dlnaNetwork.Listener = bind("192.168.1.20:7790")
	}
	if with != nil {
		with(&opts)
	}
	ps.servedBridge = launchServe(t, func(ctx context.Context, out, errout io.Writer) int { return runServe(ctx, opts, out, errout) })
	select {
	case ps.player = <-pboot:
	case <-serveGiveUp(t):
		t.Fatal("no player decision")
	}
	ps.engine(t)
	return ps, started
}
func discoverySurface(t *testing.T, ch <-chan *openhome.Surface) *openhome.Surface {
	t.Helper()
	select {
	case s := <-ch:
		for {
			changed := s.Changed()
			if s.Status().Reconciles > 0 {
				return s
			}
			select {
			case <-changed:
			case <-serveGiveUp(t):
				t.Fatal("surface did not reconcile")
			}
		}
	case <-serveGiveUp(t):
		t.Fatal("surface did not start")
	}
	return nil
}
func discoveryPacketHeader(pkt, key string) string {
	for _, line := range strings.Split(pkt, "\r\n") {
		k, v, ok := strings.Cut(line, ": ")
		if ok && k == key {
			return v
		}
	}
	return ""
}
func stopDiscoveryServe(t *testing.T, ps *playerServe) {
	t.Helper()
	ps.stop()
	select {
	case code := <-ps.done:
		if code != 0 {
			t.Fatalf("serve exit %d: %s", code, ps.stderr.String())
		}
	case <-serveGiveUp(t):
		t.Fatal("serve did not stop")
	}
}

func TestServeSharesDiscoveryAcrossItsEligibleDevices(t *testing.T) {
	needsDirectorySync(t)
	for _, tc := range []struct {
		name                                                           string
		media, source, disabled, refused, failed, disjoint, unconfined bool
	}{
		{name: "off"}, {name: "Source-only", source: true}, {name: "MediaServer-only", media: true},
		{name: "both", media: true, source: true}, {name: "disjoint", media: true, source: true, disjoint: true},
		{name: "disabled-player", media: true, source: true, disabled: true}, {name: "refused-player", media: true, source: true, refused: true},
		{name: "failed-player", media: true, source: true, failed: true}, {name: "strict-failure", media: true, source: true, unconfined: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := lantest.NewMemory(t)
			a := lantest.Address("192.168.1.20/24", "eth0", 2)
			if tc.disjoint {
				a = lantest.Address("10.0.0.20/24", "eth1", 3)
			}
			box.Set([]lan.Address{a})
			n := ssdptest.New()
			if tc.unconfined {
				n.Setups["eth0"] = dlna.SSDPListenerCaps{DestinationReporting: true, ArrivalReporting: true}
			}
			block := goBarPlayer + fmt.Sprintf("    openhome: %t\n    openhomePort: 0\n", tc.source)
			if tc.disabled {
				block = strings.Replace(block, "enabled: true", "enabled: false", 1)
			}
			goos := "linux"
			if tc.refused {
				goos = "darwin"
			}
			ps, started := bootDiscoveryServe(t, t.TempDir(), tc.media, block, goos, box, n, func(o *serveOpts) {
				if tc.failed {
					o.playerSeams = &servePlayerSeams{engineConfig: func(c *engine.Config) { c.ReservePath = string([]byte{0}) }}
				}
			})
			wantsSurface := tc.source && !tc.disabled && !tc.refused && !tc.failed
			if wantsSurface {
				discoverySurface(t, started)
			}
			sourceCount, mediaCount := 0, 0
			var sourceUDN, mediaUDN string
			for _, w := range n.Snapshot() {
				if discoveryPacketHeader(w.Pkt, "NTS") != "ssdp:alive" {
					continue
				}
				usn := discoveryPacketHeader(w.Pkt, "USN")
				if strings.Contains(w.Pkt, "/openhome/description.xml") {
					sourceCount++
					sourceUDN = strings.Split(usn, "::")[0]
				} else {
					mediaCount++
					mediaUDN = strings.Split(usn, "::")[0]
				}
			}
			wantSource := 0
			if wantsSurface && !tc.unconfined {
				wantSource = 7
			}
			wantMedia := 0
			if tc.media {
				wantMedia = len(dlna.NotifyTargetsFor("uuid:any"))
			}
			if sourceCount != wantSource || mediaCount != wantMedia {
				t.Fatalf("alive Source/MediaServer %d/%d want %d/%d", sourceCount, mediaCount, wantSource, wantMedia)
			}
			if wantSource > 0 && wantMedia > 0 && sourceUDN == mediaUDN {
				t.Fatal("devices share identity")
			}
			listeners, _, _ := n.OpenSockets()
			wantListeners := 0
			if wantSource+wantMedia > 0 {
				wantListeners = 1
			}
			if tc.disjoint {
				wantListeners = 2
			}
			if listeners != wantListeners {
				t.Fatalf("memberships %d want %d", listeners, wantListeners)
			}
			stopDiscoveryServe(t, ps)
			if !wantsSurface {
				select {
				case <-started:
					t.Error("ineligible surface started before serve settled")
				default:
				}
				if len(box.Listens()) != 0 {
					t.Errorf("ineligible surface bound HTTP: %v", box.Listens())
				}
			} else if len(box.Listens()) == 0 {
				t.Error("eligible surface start was not observed")
			}

			if a, b, c := n.OpenSockets(); a+b+c != 0 {
				t.Fatalf("serve left SSDP sockets %d/%d/%d", a, b, c)
			}
			if len(box.Open()) != 0 {
				t.Fatal("serve left Source HTTP")
			}
		})
	}
}

func TestServeDiscoveryRestartsWithStableIdentity(t *testing.T) {
	needsDirectorySync(t)
	root := t.TempDir()
	var identity string
	for i := 0; i < 2; i++ {
		box := lantest.NewMemory(t)
		box.Set([]lan.Address{lantest.Address("192.168.1.20/24", "eth0", 2)})
		n := ssdptest.New()
		ps, ch := bootDiscoveryServe(t, root, false, goBarPlayer+"    openhome: true\n    openhomePort: 0\n", "linux", box, n, nil)
		discoverySurface(t, ch)
		alive := awaitPackets(t, n, "NTS", "ssdp:alive", 7)
		var generationIdentity string
		for _, w := range alive {
			if discoveryPacketHeader(w.Pkt, "NT") == openhome.DeviceType {
				udn := strings.Split(discoveryPacketHeader(w.Pkt, "USN"), "::")[0]
				generationIdentity = udn
				if i == 0 {
					identity = udn
				} else if udn != identity {
					t.Fatal("restart changed Source identity")
				}
			}
		}
		if generationIdentity == "" || generationIdentity == "uuid:" {
			t.Fatalf("generation %d advertised no Source identity", i)
		}

		stopDiscoveryServe(t, ps)
		byebye := 0
		for _, w := range n.Snapshot() {
			if discoveryPacketHeader(w.Pkt, "NTS") == "ssdp:byebye" {
				byebye++
				nt := discoveryPacketHeader(w.Pkt, "NT")
				want := identity + "::" + nt
				if nt == identity {
					want = identity
				}
				if got := discoveryPacketHeader(w.Pkt, "USN"); got != want {
					t.Errorf("generation %d byebye %s USN %q want %q", i, nt, got, want)
				}
			}
		}
		if byebye != 7 {
			t.Fatalf("generation %d byebye=%d", i, byebye)
		}
	}
}

func TestServeWithdrawsSourceBeforeStoppingItsEngine(t *testing.T) {
	needsDirectorySync(t)
	for _, reason := range []string{"cancel", "supervisor-exit"} {
		t.Run(reason, func(t *testing.T) {
			box := lantest.NewMemory(t)
			box.Set([]lan.Address{lantest.Address("192.168.1.20/24", "eth0", 2)})
			n := ssdptest.New()
			entered, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			var once sync.Once
			n.OnSenderWrite = func(_ *ssdptest.Sender, pkt string) {
				if discoveryPacketHeader(pkt, "NTS") == "ssdp:byebye" {
					once.Do(func() { close(entered); <-release })
				}
			}
			panicNow := make(chan struct{})
			ps, ch := bootDiscoveryServe(t, t.TempDir(), false, goBarPlayer+"    openhome: true\n    openhomePort: 0\n", "linux", box, n, func(o *serveOpts) {
				if reason == "supervisor-exit" {
					o.playerSeams = &servePlayerSeams{reports: func(engine.OutputReport) { <-panicNow; panic("test") }}
				}
			})
			surface := discoverySurface(t, ch)
			if reason == "cancel" {
				ps.stop()
			} else {
				close(panicNow)
			}
			select {
			case <-entered:
			case <-serveGiveUp(t):
				t.Fatal("no withdrawal")
			}
			select {
			case <-ps.player.run.done:
				close(release)
				t.Fatal("engine stopped before withdrawal")
			default:
			}
			if len(box.Open()) != 1 {
				close(release)
				t.Fatal("HTTP closed before withdrawal")
			}
			resp, err := box.Client(netip.MustParseAddr("192.168.1.50")).Get("http://" + surface.Status().Listening[0].Bound.String() + "/openhome/description.xml")
			if err != nil {
				close(release)
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != 503 || !resp.Close {
				close(release)
				t.Fatal("serve did not refuse while withdrawing")
			}
			close(release)
			stopDiscoveryServe(t, ps)
			select {
			case <-ps.player.run.done:
			default:
				t.Fatal("engine not joined")
			}
			if a, b, c := n.OpenSockets(); a+b+c != 0 {
				t.Fatal("serve retained sockets")
			}
		})
	}
}

func TestServeDiscoverySharesOneShortShutdownDeadline(t *testing.T) {
	t.Run("blocked-writes", func(t *testing.T) {
		needsDirectorySync(t)
		box := lantest.NewMemory(t)
		box.Set([]lan.Address{lantest.Address("192.168.1.20/24", "eth0", 2)})
		n := ssdptest.New()
		ps, ch := bootDiscoveryServe(t, t.TempDir(), true, goBarPlayer+"    openhome: true\n    openhomePort: 0\n", "linux", box, n, func(o *serveOpts) { o.ssdpStopTimeout = 50 * time.Millisecond })
		discoverySurface(t, ch)
		n.Configure(func(n *ssdptest.Network) { n.BlockSends["eth0"] = true })
		began := time.Now()
		stopDiscoveryServe(t, ps)
		if elapsed := time.Since(began); elapsed > 500*time.Millisecond {
			t.Fatalf("shutdown reset its shared 50ms budget: %s", elapsed)
		}
		if a, b, c := n.OpenSockets(); a+b+c != 0 {
			t.Fatalf("expired retirement retained %d/%d/%d sockets", a, b, c)
		}
		if len(box.Open()) != 0 {
			t.Fatal("expired retirement retained HTTP")
		}
	})
	t.Run("same-absolute-deadline", func(t *testing.T) {
		needsDirectorySync(t)
		box := lantest.NewMemory(t)
		box.Set([]lan.Address{lantest.Address("192.168.1.20/24", "eth0", 2)})
		n := ssdptest.New()
		var mu sync.Mutex
		type write struct {
			packet   string
			deadline time.Time
		}
		var writes []write
		n.OnSenderWrite = func(s *ssdptest.Sender, pkt string) {
			if discoveryPacketHeader(pkt, "NTS") == "ssdp:byebye" {
				mu.Lock()
				writes = append(writes, write{pkt, s.WriteDeadline()})
				mu.Unlock()
			}
		}
		ps, ch := bootDiscoveryServe(t, t.TempDir(), true, goBarPlayer+"    openhome: true\n    openhomePort: 0\n", "linux", box, n, func(o *serveOpts) { o.ssdpStopTimeout = time.Second })
		discoverySurface(t, ch)
		awaitPackets(t, n, "NTS", "ssdp:alive", 7+len(dlna.NotifyTargetsFor("uuid:any")))
		stopDiscoveryServe(t, ps)
		mu.Lock()
		defer mu.Unlock()
		if len(writes) != 7+len(dlna.NotifyTargetsFor("uuid:any")) {
			t.Fatalf("byebye writes=%d", len(writes))
		}
		var source, media bool
		deadline := writes[0].deadline
		if deadline.IsZero() {
			t.Fatal("no write deadline")
		}
		for _, w := range writes {
			if !w.deadline.Equal(deadline) {
				t.Errorf("byebye deadline %s differs from shared %s", w.deadline, deadline)
			}
			nt := discoveryPacketHeader(w.packet, "NT")
			source = source || nt == openhome.DeviceType
			media = media || strings.Contains(nt, "device:MediaServer:")
		}
		if !source || !media {
			t.Fatalf("byebye owners Source/MediaServer=%v/%v", source, media)
		}
	})
}

// discoveryClosingListener records the real DLNA HTTP listener's close.
type discoveryClosingListener struct {
	net.Listener
	closed func()
}

func (l discoveryClosingListener) Close() error {
	err := l.Listener.Close()
	l.closed()
	return err
}

func TestServeStopsDLNABeforeItsUPnPResolver(t *testing.T) {
	needsDirectorySync(t)
	box := lantest.NewMemory(t)
	box.Set([]lan.Address{lantest.Address("192.168.1.20/24", "eth0", 2)})
	n := ssdptest.New()
	var mu sync.Mutex
	var order []string
	record := func(event string) { mu.Lock(); defer mu.Unlock(); order = append(order, event) }
	n.OnSenderWrite = func(_ *ssdptest.Sender, pkt string) {
		if discoveryPacketHeader(pkt, "NTS") == "ssdp:byebye" && strings.Contains(discoveryPacketHeader(pkt, "NT"), "device:MediaServer:") {
			record("MediaServer byebye")
		}
	}
	ps, ch := bootDiscoveryServe(t, t.TempDir(), true, goBarPlayer+"    openhome: true\n    openhomePort: 0\n", "linux", box, n, func(o *serveOpts) {
		o.dlnaNetwork.Listener = discoveryClosingListener{o.dlnaNetwork.Listener, func() { record("DLNA HTTP close") }}
		o.upnpStopping = func() { record("UPnP stop") }
	})
	discoverySurface(t, ch)
	stopDiscoveryServe(t, ps)
	mu.Lock()
	defer mu.Unlock()
	index := func(event string) int {
		for i, got := range order {
			if got == event {
				return i
			}
		}
		t.Fatalf("missing %s in %v", event, order)
		return -1
	}
	bye, http, upstream := index("MediaServer byebye"), index("DLNA HTTP close"), index("UPnP stop")
	if bye >= http || http >= upstream {
		t.Fatalf("want MediaServer byebye < DLNA HTTP close < UPnP stop, got %v", order)
	}
}

// awaitPackets waits for actual sends; a surface reconciliation is not a
// barrier for the MediaServer's asynchronous announcement.
func awaitPackets(t *testing.T, n *ssdptest.Network, key, value string, count int) []ssdptest.Write {
	t.Helper()
	give := time.After(5 * time.Second)
	for {
		changed := n.Changed()
		var out []ssdptest.Write
		for _, w := range n.Snapshot() {
			if discoveryPacketHeader(w.Pkt, key) == value {
				out = append(out, w)
			}
		}
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
