//go:build windows

package consolelan

import "syscall"

// Hold sets nothing.
func Hold(index int, ipv6 bool) func(network, address string, c syscall.RawConn) error {
	return func(string, string, syscall.RawConn) error { return nil }
}

// HostModel is a strong table, read from nothing.
func HostModel(ipv6 bool) (Table, error) { return Table{}, nil }

func hostModelEntry(index int, ipv6 bool) (Row, error) { return Row{}, nil }
