package main

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
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
		logs := watchLog(t)
		box := newOpenHomeBox(t)
		ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    room: Study\n    openhome: true\n", standIn: newPlayerStandIn(),
			with: func(o *serveOpts) { o.playerOpenHome = box.wiring() }})
		ps.runningEngine(t)
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
		req, err := http.NewRequest(http.MethodPost, "http://"+at.String()+"/openhome/playlist/control",
			strings.NewReader(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:Play xmlns:u="urn:av-openhome-org:service:Playlist:1"/></s:Body></s:Envelope>`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
		req.Header.Set("SOAPAction", `"urn:av-openhome-org:service:Playlist:1#Play"`)
		res, err = client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ = io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != 500 || !strings.Contains(string(raw), "<errorCode>602</errorCode>") {
			t.Errorf("Play: %d %s; want fault 602", res.StatusCode, raw)
		}
		if st := ps.runningEngine(t).Snapshot(); st.Len() != 0 || st.State().String() != "stopped" {
			t.Errorf("a refused Play reached the engine: %d entries, %s", st.Len(), st.State())
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
		logs.await(t, "player: shut down")
		all := logs.all()
		surfaceStopped := strings.Index(all, "player: the OpenHome surface stopped")
		engineDown := strings.Index(all, "player: shut down")
		if surfaceStopped < 0 || surfaceStopped > engineDown {
			t.Errorf("the surface stopped at %d and the player shut down at %d: want the surface first\n%s", surfaceStopped, engineDown, all)
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
