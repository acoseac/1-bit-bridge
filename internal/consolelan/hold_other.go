//go:build !linux && !darwin && !windows

package consolelan

import "syscall"

// Hold refuses on every platform but Linux, macOS and Windows: none of them
// holds the console's socket to an interface the bridge can rely on.
func Hold(index int, ipv6 bool) func(network, address string, c syscall.RawConn) error {
	_, _ = index, ipv6
	return func(_, _ string, _ syscall.RawConn) error {
		return &RefusalError{Reason: ReasonNotSupportedHere}
	}
}

// HostModel has no table here.
func HostModel(ipv6 bool) (Table, error) {
	_ = ipv6
	return nil, &RefusalError{Reason: ReasonNotSupportedHere}
}
