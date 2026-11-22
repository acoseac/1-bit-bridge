// Package consolelan holds the console's home-network socket to the
// interface its address is on (backlog B989).
//
// Names-only shim (the tests-first commit): every name the tests read, each
// body inert until the change gives it its own.
package consolelan

import (
	"net"
	"net/netip"
	"syscall"
)

const (
	HoldBindToIfindex = "SO_BINDTOIFINDEX"
	HoldBoundIf       = "IP_BOUND_IF"
	HoldStrongHost    = "strong_host"
)

const (
	ReasonNotSupportedHere       = "not_supported_here"
	ReasonInterfaceUnknown       = "interface_unknown"
	ReasonRestrictionRefused     = "restriction_refused"
	ReasonRestrictionUnconfirmed = "restriction_unconfirmed"
	ReasonInterfaceNotStrongHost = "interface_not_strong_host"
	ReasonHostModelUnreadable    = "host_model_unreadable"
	ReasonInterfacesUnreadable   = "interfaces_unreadable"
)

type RefusalError struct {
	Reason    string
	Interface string
	Err       error
}

func (e *RefusalError) Error() string { return e.Reason }

func (e *RefusalError) Unwrap() error { return e.Err }

// Decide decides no hold for any platform.
func Decide(goos string, ipv6 bool) (string, error) {
	return "", &RefusalError{Reason: ReasonNotSupportedHere}
}

type OptionCalls struct {
	Name string
	Set  func(fd uintptr, index int) error
	Get  func(fd uintptr) (int, error)
}

// HoldUsing holds nothing and reads nothing back.
func HoldUsing(calls OptionCalls) func(index int, ipv6 bool) func(network, address string, c syscall.RawConn) error {
	return func(int, bool) func(string, string, syscall.RawConn) error {
		return func(string, string, syscall.RawConn) error { return nil }
	}
}

type Interface struct {
	Name  string
	Index int
	Flags net.Flags
	Addrs []netip.Prefix
}

type Row struct {
	Index           int
	WeakHostSend    bool
	WeakHostReceive bool
	Forwarding      bool
}

type Table []Row

// JudgeStrongHost holds every table.
func JudgeStrongHost(t Table, addressed []int, aIndex int, names map[int]string) error { return nil }
