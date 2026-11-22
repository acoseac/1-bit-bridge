package lan

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

// fakeListing is a lister and an address reader of the test's, so no
// reading here asks the host (I3).
type fakeListing struct {
	ifs     []net.Interface
	addrs   map[string][]net.Addr
	listErr error
	addrErr map[string]error
}

func (f fakeListing) list() ([]net.Interface, error) { return f.ifs, f.listErr }

func (f fakeListing) addrsOf(in net.Interface) ([]net.Addr, error) {
	if err := f.addrErr[in.Name]; err != nil {
		return nil, err
	}
	return f.addrs[in.Name], nil
}

func ipNet(cidr string) *net.IPNet {
	ip, n, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	n.IP = ip
	return n
}

// TestReadInterfacesFailsWholeOnAnyInterfacesAddresses holds
// ReadInterfacesFrom to a reading that is whole or fails (the plan's §3.2,
// D12): the listing failing fails it, one interface's addresses failing
// fails it naming that interface, and an address that is no IP fails it;
// HostInterfaces lists such an interface with no addresses instead, which
// is no reading the console's socket may rest on.
func TestReadInterfacesFailsWholeOnAnyInterfacesAddresses(t *testing.T) {
	eth := net.Interface{Index: 2, Name: "eth0", Flags: net.FlagUp | net.FlagRunning}
	wlan := net.Interface{Index: 3, Name: "wlan0", Flags: net.FlagUp | net.FlagRunning}
	good := map[string][]net.Addr{"eth0": {ipNet("192.168.1.20/24")}, "wlan0": {ipNet("10.0.0.5/24")}}
	for _, c := range []struct {
		name  string
		f     fakeListing
		iface string
	}{
		{"the listing fails", fakeListing{listErr: errors.New("netlink: no buffer space")}, ""},
		{"one interface's addresses fail", fakeListing{ifs: []net.Interface{eth, wlan}, addrs: good,
			addrErr: map[string]error{"wlan0": errors.New("route ip+net: no such network interface")}}, "wlan0"},
		{"an address that is no IP", fakeListing{ifs: []net.Interface{eth}, addrs: map[string][]net.Addr{
			"eth0": {&net.UnixAddr{Name: "@x", Net: "unix"}}}}, "eth0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ReadInterfacesFrom(c.f.list, c.f.addrsOf)
			var ue *UnreadableError
			if !errors.As(err, &ue) {
				t.Fatalf("ReadInterfacesFrom = %v, %v; want an *UnreadableError", got, err)
			}
			if ue.Interface != c.iface {
				t.Fatalf("the failure names %q, want %q", ue.Interface, c.iface)
			}
			if got != nil {
				t.Fatalf("a failed reading answered %v", got)
			}
			if c.iface != "" && !strings.Contains(err.Error(), c.iface) {
				t.Fatalf("the failure %q does not name %s", err, c.iface)
			}
		})
	}
	t.Run("a whole reading", func(t *testing.T) {
		got, err := ReadInterfacesFrom(fakeListing{ifs: []net.Interface{eth, wlan}, addrs: good}.list,
			fakeListing{addrs: good}.addrsOf)
		if err != nil || len(got) != 2 || got[0].Addrs[0] != netip.MustParsePrefix("192.168.1.20/24") {
			t.Fatalf("ReadInterfacesFrom = %v, %v", got, err)
		}
	})
}

// TestReadInterfacesKeepsAnAddressThatIsNoIPNetAsTheAddressAlone: an
// address that is no *net.IPNet, which HostInterfaces skips, is kept as the
// address alone, with no on-link network (F8), so the console says
// address_has_no_network for it rather than address_not_on_this_box.
func TestReadInterfacesKeepsAnAddressThatIsNoIPNetAsTheAddressAlone(t *testing.T) {
	eth := net.Interface{Index: 2, Name: "eth0", Flags: net.FlagUp | net.FlagRunning}
	f := fakeListing{ifs: []net.Interface{eth}, addrs: map[string][]net.Addr{
		"eth0": {&net.IPAddr{IP: net.ParseIP("192.168.1.20")}, &net.IPAddr{IP: net.ParseIP("fd12:3456::7")}},
	}}
	got, err := ReadInterfacesFrom(f.list, f.addrsOf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Addrs) != 2 {
		t.Fatalf("ReadInterfacesFrom = %+v", got)
	}
	for i, want := range []string{"192.168.1.20/32", "fd12:3456::7/128"} {
		if got[0].Addrs[i] != netip.MustParsePrefix(want) {
			t.Errorf("address %d = %s, want %s, the address alone", i, got[0].Addrs[i], want)
		}
	}
}
