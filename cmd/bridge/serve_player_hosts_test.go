package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna/ssdptest"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/player/lan/lantest"
	"github.com/acoseac/1-bit-bridge/internal/player/trackref"
)

// acceptsOnly is a DLNA listener that takes connections on one address.
type acceptsOnly netip.Addr

func (a acceptsOnly) AcceptsOn(b netip.Addr) bool { return netip.Addr(a) == b }

// TestThePlayersHostsAreHealthsAndTheDLNAListeners pins the inventory serve
// confines an OpenHome Insert's Uri to (B470 PR 5a4), its own expected set
// at each step: the hosts /v1/health advertises (here 192.168.1.5), on 7790
// whatever dlna.listenAddress says and on dlna.listenAddress's port; an
// address only the DLNA listener binds (10.0.0.5) is the bridge's once serve
// hands the inventory that listener, and not before; and a port 0 or one
// that does not read names no other port.
func TestThePlayersHostsAreHealthsAndTheDLNAListeners(t *testing.T) {
	uri := func(host string, port int) trackref.URI {
		t.Helper()
		u, err := trackref.Parse(fmt.Sprintf("http://%s:%d/dlna/file/b5b4c8dc9764772e", host, port))
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	cfg := &config.Config{DLNA: config.DLNAConfig{ListenAddress: ":7800"}}
	inv, setDLNA := newPlayerHosts(func() []string { return []string{"https://192.168.1.5:7788"} }, cfg)
	for _, tc := range []struct {
		host string
		port int
		want bool
	}{
		{"192.168.1.5", 7790, true}, {"192.168.1.5", 7800, true}, {"192.168.1.5", 7801, false},
		{"192.168.1.5", 7788, false}, {"10.0.0.5", 7790, false},
	} {
		if got := inv.Names(uri(tc.host, tc.port)); got != tc.want {
			t.Errorf("before DLNA started, %s:%d: %v, want %v", tc.host, tc.port, got, tc.want)
		}
	}
	setDLNA(acceptsOnly(netip.MustParseAddr("10.0.0.5")))
	if !inv.Names(uri("10.0.0.5", 7790)) || !inv.Names(uri("10.0.0.5", 7800)) || inv.Names(uri("10.0.0.6", 7790)) {
		t.Error("once DLNA started, the inventory does not take the address its listener binds, and that alone")
	}
	for _, tc := range []struct {
		listen string
		want   []uint16
	}{
		{"", []uint16{7790}}, {":7790", []uint16{7790}}, {"192.168.1.5:7800", []uint16{7790, 7800}},
		{"[fd00::5]:8200", []uint16{7790, 8200}}, {"127.0.0.1:0", []uint16{7790}}, {"no port", []uint16{7790}},
	} {
		got := playerTrackPorts(&config.Config{DLNA: config.DLNAConfig{ListenAddress: tc.listen}})
		if !slices.Equal(got, tc.want) {
			t.Errorf("dlna.listenAddress %q: ports %v, want %v", tc.listen, got, tc.want)
		}
	}
}

// TestServeConfinesAnInsertToTheBridgesOwnHosts pins serve's wiring of the
// confinement through the real serve, its library and its engine, on the
// discovery rig's LAN: an Insert of the golden track the library holds is
// taken on the host /v1/health advertises (its walk here eth9's 192.168.7.7,
// none of the LAN's), with DLNA on and off; on the DLNA listener's own
// address (192.168.1.20, which health does not advertise here) it is taken
// while DLNA runs, the inventory hearing of the listener once DLNA starts,
// after the player, and refused 600 on a bridge without DLNA; on an address
// neither names, 600; and a track id the library does not hold, 600, on any
// host. A taken Insert's Read gives its Uri back byte for byte.
func TestServeConfinesAnInsertToTheBridgesOwnHosts(t *testing.T) {
	needsDirectorySync(t)
	const rel, id = "Artist/Album (2020)/01 Track One.flac", "b5b4c8dc9764772e"
	for _, media := range []bool{true, false} {
		t.Run(fmt.Sprintf("DLNA %v", media), func(t *testing.T) {
			t.Cleanup(advertise.SetInterfacesForTest([]advertise.InterfaceForTest{{Name: "eth9",
				Flags: net.FlagUp | net.FlagBroadcast | net.FlagMulticast | net.FlagRunning, Addrs: []string{"192.168.7.7"}}}))
			root := t.TempDir()
			file := filepath.Join(root, "Music", filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("one"), 0o600); err != nil {
				t.Fatal(err)
			}
			seedTrack(t, filepath.Join(root, "data"), rel, 3)
			box := lantest.NewMemory(t)
			addr := lantest.Address("192.168.1.20/24", "eth0", 2)
			box.Set([]lan.Address{addr})
			ps, started := bootDiscoveryServe(t, root, media, goBarPlayer+"    openhome: true\n    openhomePort: 0\n", "linux", box, ssdptest.New(), nil)
			s := discoverySurface(t, started)
			var at netip.AddrPort
			for _, l := range s.Status().Listening {
				if l.Address == addr {
					at = l.Bound
				}
			}
			if !at.IsValid() {
				t.Fatalf("the surface does not listen on %s: %+v", addr.Prefix, s.Status())
			}
			client := box.Client(netip.MustParseAddr("192.168.1.50"))
			soap := func(action, args string) (int, string) {
				t.Helper()
				req, err := http.NewRequest(http.MethodPost, "http://"+at.String()+"/openhome/playlist/control",
					strings.NewReader(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:`+action+
						` xmlns:u="urn:av-openhome-org:service:Playlist:1">`+args+`</u:`+action+`></s:Body></s:Envelope>`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
				req.Header.Set("SOAPAction", `"urn:av-openhome-org:service:Playlist:1#`+action+`"`)
				res, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := io.ReadAll(res.Body)
				_ = res.Body.Close()
				return res.StatusCode, string(raw)
			}
			for _, tc := range []struct {
				what, uri string
				taken     bool
			}{
				{"the host /v1/health advertises", "http://192.168.7.7:7790/dlna/file/" + id, true},
				{"the DLNA listener's address", "http://192.168.1.20:7790/dlna/file/" + id, media},
				{"an address neither names", "http://192.168.1.99:7790/dlna/file/" + id, false},
				{"a track the library does not hold", "http://192.168.7.7:7790/dlna/file/1111111111111111", false},
			} {
				code, body := soap("Insert", "<AfterId>0</AfterId><Uri>"+tc.uri+"</Uri><Metadata></Metadata>")
				switch {
				case tc.taken && (code != http.StatusOK || !strings.Contains(body, "<NewId>")):
					t.Errorf("%s: %d %s; want it taken", tc.what, code, body)
				case !tc.taken && (code != http.StatusInternalServerError || !strings.Contains(body, "<errorCode>600</errorCode>")):
					t.Errorf("%s: %d %s; want fault 600", tc.what, code, body)
				case tc.taken:
					newID := body[strings.Index(body, "<NewId>")+len("<NewId>") : strings.Index(body, "</NewId>")]
					if code, read := soap("Read", "<Id>"+newID+"</Id>"); code != http.StatusOK || !strings.Contains(read, "<Uri>"+tc.uri+"</Uri>") {
						t.Errorf("%s: Read %d %s; want the Uri back", tc.what, code, read)
					}
				}
			}
			stopDiscoveryServe(t, ps)
		})
	}
}
