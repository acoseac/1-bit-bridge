package lan

import (
	"net"
	"testing"
)

// TestReadInterfacesAgreesWithHostInterfacesWhereNothingFails: where every
// read answers and every address is a *net.IPNet, ReadInterfacesFrom reads
// each address as HostInterfaces does, through prefixOf, prefixes and all,
// over synthetic masks (I3: the injected reader keeps this hermetic, and
// prefixOf's own cases stay the baseline): a 16-byte IPv4 address and mask,
// a mask that is no prefix, a mask of no ones, and ordinary ones.
func TestReadInterfacesAgreesWithHostInterfacesWhereNothingFails(t *testing.T) {
	addrs := []*net.IPNet{
		ipNet("192.168.1.20/24"),
		{IP: net.ParseIP("192.168.1.21").To16(), Mask: net.CIDRMask(120, 128)},
		{IP: net.ParseIP("10.0.0.5").To4(), Mask: net.IPv4Mask(255, 0, 255, 0)},
		{IP: net.ParseIP("10.0.0.6").To4(), Mask: net.IPv4Mask(0, 0, 0, 0)},
		ipNet("fd12:3456::7/64"),
		{IP: net.ParseIP("fd12:3456::8"), Mask: net.CIDRMask(0, 128)},
	}
	var as []net.Addr
	for _, a := range addrs {
		as = append(as, a)
	}
	eth := net.Interface{Index: 2, Name: "eth0", Flags: net.FlagUp | net.FlagRunning}
	f := fakeListing{ifs: []net.Interface{eth}, addrs: map[string][]net.Addr{"eth0": as}}
	got, err := ReadInterfacesFrom(f.list, f.addrsOf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Addrs) != len(addrs) {
		t.Fatalf("ReadInterfacesFrom = %+v", got)
	}
	for i, a := range addrs {
		want, ok := prefixOf(a)
		if !ok {
			t.Fatalf("prefixOf(%v) refused it", a)
		}
		if got[0].Addrs[i] != want {
			t.Errorf("address %v read as %s, HostInterfaces' prefixOf gives %s", a, got[0].Addrs[i], want)
		}
	}
}
