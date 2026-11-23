package consolelan

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

// TestEachPlatformAndFamilyIsDecided holds Decide to the plan's §3.3 table
// for every platform, GOOS a parameter, so every row is asked on every CI leg
// (D13): Linux holds both families by SO_BINDTOIFINDEX, macOS IPv4 alone by
// IP_BOUND_IF (a ULA is refused until IPV6_BOUND_IF is measured), Windows
// both by the strong host model, and nothing else holds anything.
func TestEachPlatformAndFamilyIsDecided(t *testing.T) {
	for _, c := range []struct {
		goos string
		ipv6 bool
		want string
	}{
		{"linux", false, HoldBindToIfindex},
		{"linux", true, HoldBindToIfindex},
		{"darwin", false, HoldBoundIf},
		{"darwin", true, ""},
		{"windows", false, HoldStrongHost},
		{"windows", true, HoldStrongHost},
		{"freebsd", false, ""},
		{"freebsd", true, ""},
		{"openbsd", false, ""},
		{"netbsd", false, ""},
		{"illumos", false, ""},
		{"plan9", false, ""},
		{"js", false, ""},
		{"", false, ""},
	} {
		got, err := Decide(c.goos, c.ipv6)
		if got != c.want {
			t.Errorf("Decide(%q, ipv6=%v) = %q, want %q", c.goos, c.ipv6, got, c.want)
		}
		if c.want == "" {
			var re *RefusalError
			if !errors.As(err, &re) || re.Reason != ReasonNotSupportedHere {
				t.Errorf("Decide(%q, ipv6=%v) refused with %v, want %s", c.goos, c.ipv6, err, ReasonNotSupportedHere)
			}
		} else if err != nil {
			t.Errorf("Decide(%q, ipv6=%v) = %v, want no refusal", c.goos, c.ipv6, err)
		}
	}
}

// TestTheStrongHostTableDecidesTheWindowsHold holds JudgeStrongHost to the
// plan's §3.3 and §9.3, with the brief's I5 as §9.3 decides it (I13): every
// row's three flags are judged, whatever interface the row is of (F3), and
// every interface the listing shows holding an address of the family, A's
// above all, must have a row; a row whose interface holds no such address,
// an adapter unnumbered or disconnected, closes nothing, and a missing row is
// a reading's failure, which the next reading asks again.
func TestTheStrongHostTableDecidesTheWindowsHold(t *testing.T) {
	names := map[int]string{3: "Ethernet", 7: "Wi-Fi", 9: "vEthernet", 11: "Disconnected"}
	strong := Table{{Index: 3}, {Index: 7}, {Index: 9}}
	for _, c := range []struct {
		name      string
		table     Table
		addressed []int
		reason    string
		iface     string
	}{
		{"a strong table is held", strong, []int{3, 7, 9}, "", ""},
		{"weak host send on A's row", Table{{Index: 3, WeakHostSend: true}, {Index: 7}, {Index: 9}}, []int{3, 7, 9},
			ReasonInterfaceNotStrongHost, "Ethernet"},
		{"weak host receive on A's row", Table{{Index: 3, WeakHostReceive: true}, {Index: 7}, {Index: 9}}, []int{3, 7, 9},
			ReasonInterfaceNotStrongHost, "Ethernet"},
		{"forwarding on A's row", Table{{Index: 3, Forwarding: true}, {Index: 7}, {Index: 9}}, []int{3, 7, 9},
			ReasonInterfaceNotStrongHost, "Ethernet"},
		{"weak host send on another's row", Table{{Index: 3}, {Index: 7, WeakHostSend: true}, {Index: 9}}, []int{3, 7, 9},
			ReasonInterfaceNotStrongHost, "Wi-Fi"},
		{"weak host receive on another's row", Table{{Index: 3}, {Index: 7}, {Index: 9, WeakHostReceive: true}}, []int{3, 7, 9},
			ReasonInterfaceNotStrongHost, "vEthernet"},
		{"forwarding on another's row (a NAT)", Table{{Index: 3}, {Index: 7}, {Index: 9, Forwarding: true}}, []int{3, 7, 9},
			ReasonInterfaceNotStrongHost, "vEthernet"},
		{"a row the listing does not hold an address on is still judged",
			Table{{Index: 3}, {Index: 7}, {Index: 9}, {Index: 11, Forwarding: true}}, []int{3, 7, 9},
			ReasonInterfaceNotStrongHost, "Disconnected"},
		{"a strong row of an unnumbered adapter closes nothing",
			Table{{Index: 3}, {Index: 7}, {Index: 9}, {Index: 11}}, []int{3, 7, 9}, "", ""},
		{"an unnumbered adapter with no row closes nothing", Table{{Index: 3}, {Index: 7}}, []int{3, 7}, "", ""},
		{"no row for A's interface", Table{{Index: 7}, {Index: 9}}, []int{3, 7, 9}, ReasonInterfacesUnreadable, "Ethernet"},
		{"no row for an interface that holds an address of the family", Table{{Index: 3}, {Index: 9}}, []int{3, 7, 9},
			ReasonInterfacesUnreadable, "Wi-Fi"},
		{"an empty table", Table{}, []int{3}, ReasonInterfacesUnreadable, "Ethernet"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := JudgeStrongHost(c.table, c.addressed, 3, names)
			if c.reason == "" {
				if err != nil {
					t.Fatalf("JudgeStrongHost = %v, want the table held", err)
				}
				return
			}
			var re *RefusalError
			if !errors.As(err, &re) {
				t.Fatalf("JudgeStrongHost = %v, want a refusal %s", err, c.reason)
			}
			if re.Reason != c.reason || re.Interface != c.iface {
				t.Fatalf("refused %s naming %q, want %s naming %q", re.Reason, re.Interface, c.reason, c.iface)
			}
		})
	}
}

// fakeRawConn is a socket's RawConn whose Control hands a fixed descriptor.
type fakeRawConn struct{}

func (fakeRawConn) Control(f func(uintptr)) error  { f(42); return nil }
func (fakeRawConn) Read(func(uintptr) bool) error  { return nil }
func (fakeRawConn) Write(func(uintptr) bool) error { return nil }

var _ syscall.RawConn = fakeRawConn{}

// TestTheHoldsRefusalsAreTyped holds HoldUsing, the calls Linux's and
// macOS's holds make, to its refusals (D11): an index of 0 asks no call, a
// kernel that refuses the option (EPERM before Linux 5.7 without CAP_NET_RAW,
// ENOPROTOOPT before 5.0) is restriction_refused, and a read-back that fails
// or names another index restriction_unconfirmed; only a confirmed hold
// passes.
func TestTheHoldsRefusalsAreTyped(t *testing.T) {
	for _, c := range []struct {
		name   string
		index  int
		set    error
		got    int
		get    error
		reason string
		calls  bool
	}{
		{"held and read back", 7, nil, 7, nil, "", true},
		{"an index of 0", 0, nil, 0, nil, ReasonInterfaceUnknown, false},
		{"a negative index", -1, nil, 0, nil, ReasonInterfaceUnknown, false},
		{"EPERM", 7, syscall.EPERM, 0, nil, ReasonRestrictionRefused, true},
		{"ENOPROTOOPT", 7, syscall.ENOPROTOOPT, 0, nil, ReasonRestrictionRefused, true},
		{"a read-back of another index", 7, nil, 3, nil, ReasonRestrictionUnconfirmed, true},
		{"a read-back that fails", 7, nil, 0, syscall.EBADF, ReasonRestrictionUnconfirmed, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			called := false
			hold := HoldUsing(OptionCalls{
				Name: "TEST_OPTION",
				Set:  func(fd uintptr, index int) error { called = true; return c.set },
				Get:  func(fd uintptr) (int, error) { return c.got, c.get },
			})
			err := hold(c.index, false)("tcp4", "192.168.1.20:7789", fakeRawConn{})
			if called != c.calls {
				t.Errorf("the option was called %v, want %v", called, c.calls)
			}
			if c.reason == "" {
				if err != nil {
					t.Fatalf("hold = %v, want it held", err)
				}
				return
			}
			var re *RefusalError
			if !errors.As(err, &re) || re.Reason != c.reason {
				t.Fatalf("hold = %v, want %s", err, c.reason)
			}
			if strings.Contains(err.Error(), "192.168.1.20") {
				t.Errorf("the refusal %q names the address", err)
			}
		})
	}
}

// TestARefusalNamesNoAddress holds a refusal's text to its reason, its
// interface and the error under it.
func TestARefusalNamesNoAddress(t *testing.T) {
	err := &RefusalError{Reason: ReasonRestrictionRefused, Interface: "eth0", Err: syscall.EPERM}
	if got := err.Error(); !strings.HasPrefix(got, ReasonRestrictionRefused+" (eth0): ") {
		t.Errorf("Error() = %q", got)
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Error("the refusal does not unwrap to its cause")
	}
}
