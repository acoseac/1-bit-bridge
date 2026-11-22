//go:build linux

package consolelan

import "syscall"

// Hold sets nothing and reads nothing back.
func Hold(index int, ipv6 bool) func(network, address string, c syscall.RawConn) error {
	return func(string, string, syscall.RawConn) error { return nil }
}

func setBindToIfindex(fd uintptr, index int) error { return nil }

func getBindToIfindex(fd uintptr) (int, error) { return 0, nil }

// HostModel is a strong table, read from nothing.
func HostModel(ipv6 bool) (Table, error) { return Table{}, nil }
