// Package consolelan holds the console's home-network socket to the
// interface its address is on (backlog B989), so that the kernel, and not
// only the console's source check, keeps a connection that arrives by
// another interface out: on Linux SO_BINDTOIFINDEX, which the listener
// lookup compares with the interface a SYN arrived on; on macOS IP_BOUND_IF,
// which scopes what the socket sends, its SYN-ACK included, to that
// interface; on Windows the stack's strong host model, read from every row
// of the address family's interface table. Where a platform holds nothing
// the bridge can rely on, or the kernel refuses the hold, the socket stays
// closed: never unrestricted (the plan's §3.3, D11).
//
// It imports no player package, so internal/admin, which imports none, can
// read its types; cmd/bridge wires its functions into the console's plan.
package consolelan

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"
)

// The holds, as the console's state and its log name them.
const (
	HoldBindToIfindex = "SO_BINDTOIFINDEX"
	HoldBoundIf       = "IP_BOUND_IF"
	HoldStrongHost    = "strong_host"
)

// The reasons a hold, or the reading it rests on, refuses the socket.
const (
	// ReasonNotSupportedHere is a platform, or an address family of one,
	// that holds no interface the bridge can rely on: a ULA on macOS, and
	// any GOOS but linux, darwin and windows.
	ReasonNotSupportedHere = "not_supported_here"
	// ReasonInterfaceUnknown is an interface index of 0 or less.
	ReasonInterfaceUnknown = "interface_unknown"
	// ReasonRestrictionRefused is a hold the kernel refused (EPERM before
	// Linux 5.7 without CAP_NET_RAW, ENOPROTOOPT before 5.0).
	ReasonRestrictionRefused = "restriction_refused"
	// ReasonRestrictionUnconfirmed is a hold whose read-back does not name
	// the interface it was set to.
	ReasonRestrictionUnconfirmed = "restriction_unconfirmed"
	// ReasonInterfaceNotStrongHost is a Windows interface table one of
	// whose rows departs from the strong host model: weak host send, weak
	// host receive or forwarding on.
	ReasonInterfaceNotStrongHost = "interface_not_strong_host"
	// ReasonHostModelUnreadable is a Windows interface table that did not
	// read.
	ReasonHostModelUnreadable = "host_model_unreadable"
	// ReasonInterfacesUnreadable is a reading of the box's interfaces that
	// is not whole: the listing, one interface's addresses, or a Windows
	// table that names no row for an interface that holds an address of the
	// family.
	ReasonInterfacesUnreadable = "interfaces_unreadable"
)

// RefusalError is a hold's refusal: its reason, the interface it names, if
// any, and the error under it. Its text names no address.
type RefusalError struct {
	Reason    string
	Interface string
	Err       error
}

func (e *RefusalError) Error() string {
	var b strings.Builder
	b.WriteString(e.Reason)
	if e.Interface != "" {
		b.WriteString(" (" + e.Interface + ")")
	}
	if e.Err != nil {
		b.WriteString(": " + e.Err.Error())
	}
	return b.String()
}

func (e *RefusalError) Unwrap() error { return e.Err }

// Decide is the hold goos gives an address of the family ipv6 says, or
// ReasonNotSupportedHere: SO_BINDTOIFINDEX on Linux for both families,
// IP_BOUND_IF on macOS for IPv4 alone (IPV6_BOUND_IF has never been
// measured), the strong host model on Windows for both, and nothing
// elsewhere (the plan's §3.3, D13). goos is a parameter so every platform's
// row is tested on every platform.
func Decide(goos string, ipv6 bool) (string, error) {
	switch goos {
	case "linux":
		return HoldBindToIfindex, nil
	case "darwin":
		if ipv6 {
			return "", &RefusalError{Reason: ReasonNotSupportedHere}
		}
		return HoldBoundIf, nil
	case "windows":
		return HoldStrongHost, nil
	default:
		return "", &RefusalError{Reason: ReasonNotSupportedHere}
	}
}

// OptionCalls are the two calls a hold makes on the socket's descriptor: Set
// holds it to an index, and Get reads back the index it is held to. Name is
// the option, as a refusal names it.
type OptionCalls struct {
	Name string
	Set  func(fd uintptr, index int) error
	Get  func(fd uintptr) (int, error)
}

// HoldUsing is the hold over calls, which Linux's Hold makes with
// SO_BINDTOIFINDEX and macOS's with IP_BOUND_IF; a test hands calls of its
// own to stand in for a kernel that refuses or misreports the option. An
// index of 0 or less is ReasonInterfaceUnknown before any call; a Set that
// fails (EPERM before Linux 5.7 without CAP_NET_RAW, ENOPROTOOPT before
// 5.0) ReasonRestrictionRefused; a Get that fails, or names another index,
// ReasonRestrictionUnconfirmed: each a *RefusalError, so Listen closes the
// socket and nothing listens.
func HoldUsing(calls OptionCalls) func(index int, ipv6 bool) func(network, address string, c syscall.RawConn) error {
	return func(index int, _ bool) func(network, address string, c syscall.RawConn) error {
		return func(_, _ string, c syscall.RawConn) error {
			if index <= 0 {
				return &RefusalError{Reason: ReasonInterfaceUnknown}
			}
			var serr, rerr error
			var got int
			if err := c.Control(func(fd uintptr) {
				serr = calls.Set(fd, index)
				if serr == nil {
					got, rerr = calls.Get(fd)
				}
			}); err != nil {
				return &RefusalError{Reason: ReasonRestrictionRefused, Err: err}
			}
			if serr != nil {
				return &RefusalError{Reason: ReasonRestrictionRefused, Err: fmt.Errorf("%s: %w", calls.Name, serr)}
			}
			if rerr != nil || got != index {
				if rerr == nil {
					rerr = errors.New("the socket names another interface")
				}
				return &RefusalError{Reason: ReasonRestrictionUnconfirmed, Err: rerr}
			}
			return nil
		}
	}
}

// Interface is one interface of a reading, as internal/player/lan reads it:
// its name, its index, its flags and its addresses with their on-link
// prefixes (an address with no network is the address alone).
type Interface struct {
	Name  string
	Index int
	Flags net.Flags
	Addrs []netip.Prefix
}

// Row is one row of a Windows interface table of one address family
// (MIB_IPINTERFACE_ROW): the interface's index and the three flags the
// strong host model needs off.
type Row struct {
	Index           int
	WeakHostSend    bool
	WeakHostReceive bool
	Forwarding      bool
}

// Table is a Windows interface table of one address family.
type Table []Row

// JudgeStrongHost judges a Windows interface table of A's family (the plan's
// §3.3, F3, F11 and the brief's I5 as §9.3 decides it): every row must read
// weak host send, weak host receive and forwarding off, whatever interface
// it is of, and every interface the listing shows holding an address of the
// family, A's own above all, must have a row. A row whose interface the
// listing does not show with such an address (an adapter disconnected or
// unnumbered) is no mismatch: its flags are judged and it closes nothing
// else. A missing row is a reading that is not whole
// (ReasonInterfacesUnreadable), which the next reading asks again; a row
// that departs from the model is ReasonInterfaceNotStrongHost, naming the
// interface. names gives each index its name for the refusal.
func JudgeStrongHost(t Table, addressed []int, aIndex int, names map[int]string) error {
	rows := make(map[int]bool, len(t))
	for _, r := range t {
		rows[r.Index] = true
		var bad []string
		if r.WeakHostSend {
			bad = append(bad, "weak host send")
		}
		if r.WeakHostReceive {
			bad = append(bad, "weak host receive")
		}
		if r.Forwarding {
			bad = append(bad, "forwarding")
		}
		if len(bad) > 0 {
			return &RefusalError{Reason: ReasonInterfaceNotStrongHost, Interface: nameOf(names, r.Index),
				Err: fmt.Errorf("%s on", strings.Join(bad, ", "))}
		}
	}
	if !rows[aIndex] {
		return &RefusalError{Reason: ReasonInterfacesUnreadable, Interface: nameOf(names, aIndex),
			Err: fmt.Errorf("the interface table has no row for the interface that holds the address")}
	}
	for _, idx := range addressed {
		if !rows[idx] {
			return &RefusalError{Reason: ReasonInterfacesUnreadable, Interface: nameOf(names, idx),
				Err: fmt.Errorf("the interface table has no row for an interface that holds an address of the family")}
		}
	}
	return nil
}

func nameOf(names map[int]string, idx int) string {
	if n := names[idx]; n != "" {
		return n
	}
	return fmt.Sprintf("interface %d", idx)
}
