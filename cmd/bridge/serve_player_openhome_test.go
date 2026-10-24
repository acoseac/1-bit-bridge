package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/player/lan/lantest"
	"github.com/acoseac/1-bit-bridge/internal/player/openhome"
)

// openhomeBox is a boot test's LAN for the player's OpenHome surface: one
// address on eth0, and a channel told the surface once the player starts
// it.
type openhomeBox struct {
	*lantest.Box
	addr    lan.Address
	started chan *openhome.Surface
}

func newOpenHomeBox(t *testing.T) *openhomeBox {
	t.Helper()
	b := &openhomeBox{Box: lantest.New(t), addr: lantest.Address("192.168.1.20/24", "eth0", 2), started: make(chan *openhome.Surface, 1)}
	b.Set([]lan.Address{b.addr})
	return b
}

// wiring is the box as serve's playerOpenHome.
func (b *openhomeBox) wiring() openhomeWiring {
	return openhomeWiring{Addresses: b.Addresses, Listen: b.Listen, Started: func(s *openhome.Surface) { b.started <- s }}
}

// surface waits for the player to start its surface and for the surface to
// listen on the box's address, and returns it and where it listens.
func (b *openhomeBox) surface(t *testing.T) (*openhome.Surface, netip.AddrPort) {
	t.Helper()
	var s *openhome.Surface
	select {
	case s = <-b.started:
	case <-serveGiveUp(t):
		t.Fatal("the player never started its OpenHome surface")
	}
	for {
		ch := s.Changed()
		for _, l := range s.Status().Listening {
			if l.Address == b.addr {
				return s, l.Bound
			}
		}
		select {
		case <-ch:
		case <-serveGiveUp(t):
			t.Fatalf("the surface never listened on %s: %+v", b.addr.Prefix, s.Status())
		}
	}
}

// TestServeRunsTheOpenHomeSurfaceOnlyWithItsOptInAndThePlayer: with
// output.player.openhome off, the player runs and nothing of the surface
// does: no listener asked for, no surface built. With it on, the player
// starts it once its engine runs, on each LAN address on 7792, the device
// description naming the player's identity and room, and the SOAP layer
// answering 602; serve's stop stops it first, before the engine, and closes
// its sockets. With it on and the player off, nothing of it runs, and boot
// says so once.
func TestServeRunsTheOpenHomeSurfaceOnlyWithItsOptInAndThePlayer(t *testing.T) {
	needsDirectorySync(t)
	t.Run("off", func(t *testing.T) {
		box := newOpenHomeBox(t)
		ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn(),
			with: func(o *serveOpts) { o.playerOpenHome = box.wiring() }})
		ps.runningEngine(t)
		if ps.player.surface != nil || len(box.Listens()) != 0 {
			t.Errorf("with the opt-in off, a surface %v and listens %v", ps.player.surface != nil, box.Listens())
		}
		if code := ps.stopServe(t); code != 0 {
			t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
		}
	})
	t.Run("on", func(t *testing.T) {
		box := newOpenHomeBox(t)
		var run *playerRun
		stoppedWhileRunning := make(chan bool, 1)
		ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    room: Study\n    openhome: true\n", standIn: newPlayerStandIn(),
			with: func(o *serveOpts) {
				o.playerOpenHome = box.wiring()
				o.playerOpenHome.Seams = &openhome.Seams{Stopping: func() {
					select {
					case <-run.done:
						stoppedWhileRunning <- false
					default:
						stoppedWhileRunning <- true
					}
				}}
			}})
		eng := ps.runningEngine(t)
		run = ps.player.run
		s, at := box.surface(t)
		if listens := box.Listens(); !slices.Equal(listens, []netip.AddrPort{netip.MustParseAddrPort("192.168.1.20:7792")}) {
			t.Errorf("the surface asked for %v, want 192.168.1.20:7792 alone", listens)
		}
		uuid, err := config.ReadPlayerUUID(ps.dataDir)
		if err != nil {
			t.Fatal(err)
		}
		client := box.Client(netip.MustParseAddr("192.168.1.50"))
		res, err := client.Get("http://" + at.String() + "/openhome/description.xml")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		var doc struct {
			Device struct {
				DeviceType   string `xml:"deviceType"`
				FriendlyName string `xml:"friendlyName"`
				UDN          string `xml:"UDN"`
			} `xml:"device"`
		}
		if err := xml.Unmarshal(raw, &doc); err != nil || res.StatusCode != 200 {
			t.Fatalf("the description: %d, %v\n%s", res.StatusCode, err, raw)
		}
		if doc.Device.UDN != "uuid:"+uuid || doc.Device.FriendlyName != "Study" || doc.Device.DeviceType != openhome.DeviceType {
			t.Errorf("the description names %+v, want the player's uuid:%s, Study, a Source", doc.Device, uuid)
		}
		// Positive control: the same Insert forwarded to the engine changes
		// the queue. Restore it before checking the surface's refusal.
		ref, err := engine.NewReference("library-track")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if _, err := eng.InsertAfter(ctx, 0, ref, []byte("metadata")); err != nil {
			t.Fatal(err)
		}
		if eng.Snapshot().Len() != 1 {
			t.Fatal("a forwarded Insert was not observable")
		}
		if err := eng.DeleteAll(ctx); err != nil {
			t.Fatal(err)
		}
		if eng.Snapshot().Len() != 0 {
			t.Fatal("positive control did not restore an empty queue")
		}
		req, err := http.NewRequest(http.MethodPost, "http://"+at.String()+"/openhome/playlist/control",
			strings.NewReader(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:Insert xmlns:u="urn:av-openhome-org:service:Playlist:1"><AfterId>0</AfterId><Uri>library-track</Uri><Metadata>metadata</Metadata></u:Insert></s:Body></s:Envelope>`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
		req.Header.Set("SOAPAction", `"urn:av-openhome-org:service:Playlist:1#Insert"`)
		res, err = client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ = io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != 500 || !strings.Contains(string(raw), "<errorCode>602</errorCode>") {
			t.Errorf("Insert: %d %s; want fault 602", res.StatusCode, raw)
		}
		if st := ps.runningEngine(t).Snapshot(); st.Len() != 0 || st.State().String() != "stopped" {
			t.Errorf("a refused Insert reached the engine: %d entries, %s", st.Len(), st.State())
		}
		if code := ps.stopServe(t); code != 0 {
			t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
		}
		if st := s.Status(); !st.Stopped {
			t.Errorf("the surface runs after serve's stop: %+v", st)
		}
		if open := box.Open(); len(open) != 0 {
			t.Errorf("sockets open after serve's stop: %v", open)
		}
		select {
		case live := <-stoppedWhileRunning:
			if !live {
				t.Error("the engine's Run ended before the surface began stopping")
			}
		default:
			t.Error("the surface's stopping hook never ran")
		}
	})
	t.Run("on, with no box of the test's", func(t *testing.T) {
		// The production address source and listener, over hostLAN, which
		// this suite's TestMain makes a box with no interface: the surface
		// lists it, finds nothing, and binds nothing of the host.
		started := make(chan *openhome.Surface, 1)
		ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    openhome: true\n", standIn: newPlayerStandIn(),
			with: func(o *serveOpts) {
				o.playerOpenHome = openhomeWiring{Started: func(s *openhome.Surface) { started <- s }}
			}})
		ps.runningEngine(t)
		var s *openhome.Surface
		select {
		case s = <-started:
		case <-serveGiveUp(t):
			t.Fatal("the player never started its OpenHome surface")
		}
		for {
			ch := s.Changed()
			if st := s.Status(); st.Reconciles > 0 {
				if len(st.Listening) != 0 || len(st.Failed) != 0 {
					t.Errorf("the suite's surface found the host's addresses: %+v", st)
				}
				break
			}
			select {
			case <-ch:
			case <-serveGiveUp(t):
				t.Fatal("the surface never listed its interfaces")
			}
		}
		if code := ps.stopServe(t); code != 0 {
			t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
		}
	})
	t.Run("on, with the player off", func(t *testing.T) {
		rec := loggingtest.Record(t)
		box := newOpenHomeBox(t)
		ps := bootPlayerServe(t, playerServeCase{player: "    enabled: false\n    openhome: true\n", standIn: newPlayerStandIn(),
			with: func(o *serveOpts) { o.playerOpenHome = box.wiring() }})
		if ps.player.plan.start || ps.player.surface != nil {
			t.Fatalf("the player started (%v) or a surface was built (%v)", ps.player.plan.start, ps.player.surface != nil)
		}
		if code := ps.stopServe(t); code != 0 {
			t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
		}
		if lines := rec.Lines("player: the OpenHome surface does not start"); len(lines) != 1 || !strings.Contains(lines[0], "output.player.enabled is not true") {
			t.Errorf("boot said %q, want one line saying the player is off", lines)
		}
		if len(box.Listens()) != 0 {
			t.Errorf("a listener was asked for: %v", box.Listens())
		}
	})
}

// TestServeRefusesANonnumericAddressOverrideBeforePlanning: the override
// follows file validation, before the data-dir lock or any listener opens.
func TestServeRefusesANonnumericAddressOverrideBeforePlanning(t *testing.T) {
	for _, addr := range []string{":postgresql", ":http", ":", ":65536", ":-1"} {
		t.Run(addr, func(t *testing.T) {
			cfgPath := writeValidConfig(t)
			raw, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			dataDir := filepath.Join(filepath.Dir(cfgPath), "data")
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			raw = append(raw, []byte("dataDir: "+dataDir+"\noutput:\n  player:\n"+goBarPlayer+"    openhome: true\n    openhomePort: 5432\n")...)
			if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			// A bad override must be refused before this lock is even tried;
			// it also keeps the negative control from binding a service port.
			unlock, err := lockServeDataDir(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			_, stderr, code := runCapture(t, "serve", "--config", cfgPath, "--addr", addr)
			if code != 2 || !strings.Contains(stderr, "invalid --addr") {
				t.Errorf("override was not refused before the lock: exit %d, %s", code, stderr)
			}
			if len(addr) > 1 && strings.Contains(stderr, addr) {
				t.Errorf("refusal quoted the override: %s", stderr)
			}
		})
	}
}

// TestServeSaysOnceWhenAnAddressOverrideClashesWithOpenHome: file settings
// pass, but the effective API port conflicts; the player stays off and says
// why once without printing the port, while serve still starts.
func TestServeSaysOnceWhenAnAddressOverrideClashesWithOpenHome(t *testing.T) {
	needsDirectorySync(t)
	rec := loggingtest.Record(t)
	var port int
	box := newOpenHomeBox(t)
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    openhome: true\n", standIn: newPlayerStandIn(),
		with: func(o *serveOpts) {
			o.playerOpenHome = box.wiring()
			at := netip.MustParseAddrPort(o.addrOverride)
			port = int(at.Port())
			raw, err := os.ReadFile(o.configPath)
			if err != nil {
				t.Fatal(err)
			}
			raw = append(raw, []byte(fmt.Sprintf("    openhomePort: %d\n", port))...)
			raw = append([]byte("listenAddress: :8080\n"), raw...)
			if err := os.WriteFile(o.configPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(o.configPath)
			if err != nil {
				t.Fatal(err)
			}
			if on, why := cfg.PlayerEnabled("linux"); !on || why != nil {
				t.Fatalf("file settings must enable the player before --addr: %v", why)
			}
		}})
	if ps.player.plan.start || ps.engine(t) != nil || ps.player.surface != nil || len(box.Listens()) != 0 {
		t.Error("the conflicting player or its surface started")
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d: %s", code, ps.stderr.String())
	}
	lines := rec.Lines("player: not started")
	const want = "WARN player: not started reason=output.player.openhomePort: bridge serve binds that port already, for listenAddress, set by --addr: give the OpenHome surface a port of its own"
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("override refusal: %q; want exactly %q", lines, want)
	}
	for _, line := range lines {
		if strings.Contains(line, fmt.Sprint(port)) {
			t.Errorf("refusal quoted the port: %s", line)
		}
	}
}

// TestServeDoesNotRepeatAnUnchangedRefusalUnderAnAddressOverride: Load
// already reports an invalid player block. A valid, non-clashing numeric
// override must leave that refusal told, so only Validate's line is logged.
func TestServeDoesNotRepeatAnUnchangedRefusalUnderAnAddressOverride(t *testing.T) {
	needsDirectorySync(t)
	// Validate remembers the last refusal process-wide. A valid config with
	// the player off clears it now and at cleanup, also under -count.
	clear, err := config.Load(writeValidConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := clear.Validate(); err != nil {
			t.Error(err)
		}
	})
	rec := loggingtest.Record(t)
	box := newOpenHomeBox(t)
	ps := bootPlayerServe(t, playerServeCase{
		// This refusal precedes the platform gate, so Load reports the same
		// reason on macOS and Linux without changing the host's platform.
		player:  goBarPlayer + "    openhome: true\n    openhomePort: 0\n    pauseReleaseSeconds: 3601\n",
		standIn: newPlayerStandIn(),
		with:    func(o *serveOpts) { o.playerOpenHome = box.wiring() },
	})
	if ps.player.plan.start || ps.engine(t) != nil || ps.player.surface != nil || len(box.Listens()) != 0 {
		t.Error("the refused player or its surface started")
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d: %s", code, ps.stderr.String())
	}
	lines := rec.Lines("output.player: the player is off")
	if len(lines) != 1 || !strings.Contains(lines[0], "output.player.pauseReleaseSeconds") {
		t.Errorf("Load refusal: %q; want one Validate line naming output.player.pauseReleaseSeconds", lines)
	}
	if lines := rec.Lines("player: not started"); len(lines) != 0 {
		t.Errorf("serve repeated Load's unchanged refusal: %q", lines)
	}
}
