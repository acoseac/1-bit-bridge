package dlna

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"testing"
)

// lanFlags are the flags a Wi-Fi or Ethernet adapter reports (en0 on the
// Mac measured 2026-09-27: UP,BROADCAST,RUNNING,MULTICAST).
const lanFlags = net.FlagUp | net.FlagBroadcast | net.FlagMulticast | net.FlagRunning

// tunnelFlags are the flags a macOS utun reports (utun0 on the same Mac:
// flags=8051<UP,POINTOPOINT,RUNNING,MULTICAST>).
const tunnelFlags = net.FlagUp | net.FlagPointToPoint | net.FlagMulticast | net.FlagRunning

// fakeIface is one interface of a synthetic host: what net.Interfaces()
// reports for it and what its Addrs returns.
type fakeIface struct {
	name     string
	flags    net.Flags
	addrs    []string
	addrsErr bool // Addrs fails for this interface
}

// lan is an adapter with lanFlags and the given addresses.
func lan(name string, addrs ...string) fakeIface {
	return fakeIface{name: name, flags: lanFlags, addrs: addrs}
}

// tunnel is a point-to-point interface with tunnelFlags and the given
// addresses.
func tunnel(name string, addrs ...string) fakeIface {
	return fakeIface{name: name, flags: tunnelFlags, addrs: addrs}
}

// fakeHost turns interfaces, in the order net.Interfaces() would report
// them, into the enumeration and the address lookup the pickers take.
func fakeHost(ifs []fakeIface) ([]net.Interface, interfaceAddrs) {
	ifaces := make([]net.Interface, len(ifs))
	byIndex := make(map[int]fakeIface, len(ifs))
	for i, f := range ifs {
		ifaces[i] = net.Interface{Index: i + 1, Name: f.name, Flags: f.flags}
		byIndex[i+1] = f
	}
	return ifaces, func(ifi *net.Interface) ([]net.Addr, error) {
		f := byIndex[ifi.Index]
		if f.addrsErr {
			return nil, errors.New("addresses unreadable")
		}
		out := make([]net.Addr, 0, len(f.addrs))
		for _, a := range f.addrs {
			out = append(out, mkIPNet(a))
		}
		return out, nil
	}
}

// ifaceNames lists the names of the interfaces a picker returned.
func ifaceNames(ifaces []*net.Interface) []string {
	var out []string
	for _, ifi := range ifaces {
		out = append(out, ifi.Name)
	}
	return out
}

// assertPickIsInTheSet requires what the two pickers return over one host to
// agree: the single pick is a member of the multicast set, and the single
// picker errors exactly when that set is empty. The mDNS responder binds the
// single pick, and an interface it binds must not be one the multicast set
// calls unusable.
func assertPickIsInTheSet(t *testing.T, ifaces []net.Interface, addrsOf interfaceAddrs, opts EligibilityOpts) {
	t.Helper()
	one, err := pickLANInterface(ifaces, addrsOf, opts)
	all := ifaceNames(pickAllLANInterfaces(ifaces, addrsOf, opts))
	switch {
	case err != nil && len(all) != 0:
		t.Errorf("single picker errored (%v) but the multicast set is %v", err, all)
	case err == nil && !slices.Contains(all, one.Name):
		t.Errorf("single pick %s is not in the multicast set %v", one.Name, all)
	}
}

// TestLANPreferenceOfRanksWhatTheInterfaceCarries pins the key the pickers
// compare: the point-to-point flag as Go reports it, and the best class among
// the addresses, wherever in the list it sits. A public or link-local address
// beside a private one changes nothing, and the opted-in tsnet interface
// ranks as a usable address whatever it carries.
func TestLANPreferenceOfRanksWhatTheInterfaceCarries(t *testing.T) {
	cases := []struct {
		name  string
		iface fakeIface
		opts  EligibilityOpts
		want  lanPreference
	}{
		{"macos_utun0", tunnel("utun0", "fe80::a1"), EligibilityOpts{},
			lanPreference{pointToPoint: true, class: lanLinkLocalOnly}},
		{"macos_en0", lan("en0", "fe80::e0", "192.168.1.20"), EligibilityOpts{},
			lanPreference{pointToPoint: false, class: lanPrivateIPv4}},
		{"dual_stack_private_v4_last", lan("en0", "2001:4860:4860::8888", "fe80::1", "192.168.1.5"), EligibilityOpts{},
			lanPreference{pointToPoint: false, class: lanPrivateIPv4}},
		{"ula_only", lan("en1", "fd12:3456::1"), EligibilityOpts{},
			lanPreference{pointToPoint: false, class: lanOtherUsable}},
		{"ula_before_a_private_v4", lan("en1", "fd12:3456::1", "10.0.0.5"), EligibilityOpts{},
			lanPreference{pointToPoint: false, class: lanPrivateIPv4}},
		{"private_v4_before_a_ula", lan("en1", "10.0.0.5", "fd12:3456::1"), EligibilityOpts{},
			lanPreference{pointToPoint: false, class: lanPrivateIPv4}},
		{"self_assigned_v4", lan("en12", "fe80::e12", "169.254.20.30"), EligibilityOpts{},
			lanPreference{pointToPoint: false, class: lanLinkLocalOnly}},
		// Tailscale's utun on the Mac: eligible through its fd7a:115c:a1e0::
		// ULA, whatever the CGNAT address beside it.
		{"macos_tailscale_utun", tunnel("utun12", "fe80::d12", "100.64.0.7", "fd7a:115c:a1e0::7"), EligibilityOpts{},
			lanPreference{pointToPoint: true, class: lanOtherUsable}},
		{"wireguard_private_v4", tunnel("wg0", "10.8.0.2"), EligibilityOpts{},
			lanPreference{pointToPoint: true, class: lanPrivateIPv4}},
		{"opted_in_tsnet", tunnel("utun7", "100.64.0.5"), EligibilityOpts{TsnetIfaceName: "utun7"},
			lanPreference{pointToPoint: true, class: lanOtherUsable}},
		{"opted_in_tsnet_link_local_only", tunnel("utun7", "fe80::7"), EligibilityOpts{TsnetIfaceName: "utun7"},
			lanPreference{pointToPoint: true, class: lanOtherUsable}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ifaces, addrsOf := fakeHost([]fakeIface{tc.iface})
			addrs, err := addrsOf(&ifaces[0])
			if err != nil {
				t.Fatal(err)
			}
			if !IsLANEligibleInterface(ifaces[0], addrs, tc.opts) {
				t.Fatal("fixture is not LAN-eligible, and lanPreferenceOf means nothing for one that is not")
			}
			if got := lanPreferenceOf(ifaces[0], addrs, tc.opts); got != tc.want {
				t.Errorf("lanPreferenceOf = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestPickLANInterfacePrefersANonTunnelWithAPrivateIPv4 pins the single
// picker's choice among eligible interfaces: a non-point-to-point interface
// first, then a private IPv4, then any other usable address, then
// link-local only, and the host's enumeration order between equals. The
// first row is the Mac this was measured on, where the first eligible
// interface was utun0 and the mDNS responder bound it.
func TestPickLANInterfacePrefersANonTunnelWithAPrivateIPv4(t *testing.T) {
	cases := []struct {
		name string
		host []fakeIface
		opts EligibilityOpts
		want string // "" means the picker must error
	}{
		{"macos_utun0_before_en0", []fakeIface{
			tunnel("utun0", "fe80::a1"),
			lan("en0", "fe80::e0", "192.168.1.20"),
		}, EligibilityOpts{}, "en0"},
		{"self_assigned_bridge0_before_en0", []fakeIface{
			lan("bridge0", "169.254.10.20"),
			lan("en0", "192.168.1.20"),
		}, EligibilityOpts{}, "en0"},
		{"private_tunnel_before_en0", []fakeIface{
			tunnel("utun4", "10.8.0.2"),
			lan("en0", "192.168.1.20"),
		}, EligibilityOpts{}, "en0"},
		{"zero_config_lan_alone", []fakeIface{
			lan("en0", "169.254.7.8", "fe80::1"),
		}, EligibilityOpts{}, "en0"},
		{"zero_config_lan_after_a_tunnel", []fakeIface{
			tunnel("utun0", "fe80::2"),
			lan("en0", "169.254.7.8", "fe80::1"),
		}, EligibilityOpts{}, "en0"},
		{"dual_stack_home_lan", []fakeIface{
			lan("en0", "192.168.1.5", "2001:4860:4860::8888", "fe80::1"),
		}, EligibilityOpts{}, "en0"},
		{"dual_stack_home_lan_after_a_link_local_adapter", []fakeIface{
			lan("awdl0", "fe80::a0d1"),
			lan("en0", "192.168.1.5", "2001:4860:4860::8888", "fe80::1"),
		}, EligibilityOpts{}, "en0"},
		{"ula_beats_link_local_only", []fakeIface{
			lan("awdl0", "fe80::a0d1"),
			lan("en1", "fd12:3456::1"),
		}, EligibilityOpts{}, "en1"},
		{"private_ipv4_beats_ula", []fakeIface{
			lan("en1", "fd12:3456::1"),
			lan("en0", "192.168.1.20"),
		}, EligibilityOpts{}, "en0"},
		// Go gives Wintun (IF_TYPE_PROP_VIRTUAL) no point-to-point flag,
		// so on Windows it is the address class that puts the LAN first.
		{"windows_tailscale_adapter_before_ethernet", []fakeIface{
			{name: "Tailscale", flags: net.FlagUp | net.FlagRunning, addrs: []string{"fe80::d12", "100.64.0.7", "fd7a:115c:a1e0::7"}},
			lan("Ethernet", "192.168.1.20"),
		}, EligibilityOpts{}, "Ethernet"},
		{"non_tunnel_link_local_beats_private_tunnel", []fakeIface{
			tunnel("wg0", "10.8.0.2"),
			lan("en0", "169.254.7.8"),
		}, EligibilityOpts{}, "en0"},
		{"equals_keep_os_order", []fakeIface{
			lan("en1", "10.0.0.5"),
			lan("en0", "192.168.1.20"),
		}, EligibilityOpts{}, "en1"},
		{"tunnels_alone_keep_os_order", []fakeIface{
			tunnel("utun0", "fe80::2"),
			tunnel("utun1", "fe80::3"),
		}, EligibilityOpts{}, "utun0"},
		{"opted_in_tsnet_tunnel_behind_en0", []fakeIface{
			tunnel("utun7", "100.64.0.5"),
			lan("en0", "192.168.1.20"),
		}, EligibilityOpts{TsnetIfaceName: "utun7"}, "en0"},
		{"opted_in_tsnet_tunnel_alone", []fakeIface{
			tunnel("utun7", "100.64.0.5"),
		}, EligibilityOpts{TsnetIfaceName: "utun7"}, "utun7"},
		{"unreadable_addresses_skipped", []fakeIface{
			{name: "en0", flags: lanFlags, addrsErr: true},
			lan("en1", "192.168.1.9"),
		}, EligibilityOpts{}, "en1"},
		{"nothing_eligible", []fakeIface{
			{name: "lo0", flags: net.FlagUp | net.FlagLoopback, addrs: []string{"127.0.0.1", "::1"}},
			lan("eth0", "8.8.8.8", "fe80::1"),
			{name: "en0", flags: net.FlagBroadcast, addrs: []string{"192.168.1.20"}},
		}, EligibilityOpts{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ifaces, addrsOf := fakeHost(tc.host)
			got, err := pickLANInterface(ifaces, addrsOf, tc.opts)
			switch {
			case tc.want == "" && err == nil:
				t.Errorf("picked %s, want an error", got.Name)
			case tc.want != "" && err != nil:
				t.Errorf("error %v, want %s", err, tc.want)
			case tc.want != "" && got.Name != tc.want:
				t.Errorf("picked %s, want %s", got.Name, tc.want)
			}
			assertPickIsInTheSet(t, ifaces, addrsOf, tc.opts)
		})
	}
}

// TestPickAllLANInterfacesDropsALinkLocalOnlyTunnel pins the multicast set:
// every eligible interface in enumeration order, except a point-to-point one
// whose only addresses are link-local, which is left out whenever anything
// else is eligible. The first row is the Mac this was measured on: six such
// utuns were in the set, and an SSDP client bound to one cannot send IPv4.
func TestPickAllLANInterfacesDropsALinkLocalOnlyTunnel(t *testing.T) {
	cases := []struct {
		name string
		host []fakeIface
		opts EligibilityOpts
		want []string
	}{
		{"macos_host", []fakeIface{
			tunnel("utun0", "fe80::a1"),
			lan("en0", "fe80::e0", "192.168.1.20"),
			lan("awdl0", "fe80::a0d1"),
			tunnel("utun1", "fe80::a2"),
			tunnel("utun12", "fe80::d12", "100.64.0.7", "fd7a:115c:a1e0::7"),
			lan("en12", "fe80::e12", "169.254.20.30"),
		}, EligibilityOpts{}, []string{"en0", "awdl0", "utun12", "en12"}},
		{"tunnels_alone_are_kept", []fakeIface{
			tunnel("utun0", "fe80::2"),
			tunnel("utun1", "fe80::3"),
		}, EligibilityOpts{}, []string{"utun0", "utun1"}},
		{"direct_cable_kept_beside_en0", []fakeIface{
			lan("en0", "192.168.1.20"),
			lan("en5", "169.254.1.2", "fe80::5"),
		}, EligibilityOpts{}, []string{"en0", "en5"}},
		{"tunnel_dropped_beside_a_zero_config_lan", []fakeIface{
			tunnel("utun0", "fe80::2"),
			lan("en0", "169.254.7.8", "fe80::1"),
		}, EligibilityOpts{}, []string{"en0"}},
		{"tunnel_with_a_private_address_kept", []fakeIface{
			tunnel("wg0", "10.8.0.2"),
			lan("en0", "192.168.1.20"),
		}, EligibilityOpts{}, []string{"wg0", "en0"}},
		// An opted-in tsnet tunnel carries no private address at all; the
		// opt-in is what keeps it from reading as a link-local tunnel.
		{"opted_in_tsnet_tunnel_kept", []fakeIface{
			tunnel("utun7", "100.64.0.5"),
			lan("en0", "192.168.1.20"),
		}, EligibilityOpts{TsnetIfaceName: "utun7"}, []string{"utun7", "en0"}},
		{"nothing_eligible", []fakeIface{
			lan("eth0", "8.8.8.8", "fe80::1"),
		}, EligibilityOpts{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ifaces, addrsOf := fakeHost(tc.host)
			got := ifaceNames(pickAllLANInterfaces(ifaces, addrsOf, tc.opts))
			if !slices.Equal(got, tc.want) {
				t.Errorf("multicast set %v, want %v", got, tc.want)
			}
			assertPickIsInTheSet(t, ifaces, addrsOf, tc.opts)
		})
	}
}

// TestTheExportedPickersRunTheSelection requires PickLANEligibleInterface and
// PickAllLANEligibleInterfaces to answer, on this host, what the selection the
// tables above drive answers over the same enumeration. Where the host's first
// eligible interface already ranks best the two cannot differ, so this can
// only catch an exported picker that skips the selection on a host like the
// Mac this was measured on, where utun0 comes first. A result that differs
// once is retried, since an interface can come or go between the calls; one
// that differs five times running is reported.
func TestTheExportedPickersRunTheSelection(t *testing.T) {
	var mismatch string
	for range 5 {
		ifaces, err := net.Interfaces()
		if err != nil {
			t.Skipf("net.Interfaces: %v", err)
		}
		wantOne, wantErr := pickLANInterface(ifaces, hostInterfaceAddrs, EligibilityOpts{})
		wantAll := ifaceNames(pickAllLANInterfaces(ifaces, hostInterfaceAddrs, EligibilityOpts{}))
		gotOne, gotErr := PickLANEligibleInterface(EligibilityOpts{})
		gotAll := ifaceNames(PickAllLANEligibleInterfaces(EligibilityOpts{}))
		switch {
		case (wantErr == nil) != (gotErr == nil):
			mismatch = fmt.Sprintf("single picker returned error %v, the selection %v", gotErr, wantErr)
		case wantErr == nil && gotOne.Name != wantOne.Name:
			mismatch = fmt.Sprintf("single picker chose %s, the selection chooses %s", gotOne.Name, wantOne.Name)
		case !slices.Equal(gotAll, wantAll):
			mismatch = fmt.Sprintf("multicast set %v, the selection's %v", gotAll, wantAll)
		default:
			return
		}
	}
	t.Error(mismatch)
}
