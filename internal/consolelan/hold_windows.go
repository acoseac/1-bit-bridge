//go:build windows

package consolelan

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Hold sets nothing on Windows: the hold is the stack's strong host model,
// which the console's reading judges from every row of the family's
// interface table (HostModel, JudgeStrongHost) before it binds and at every
// reading after. An index of 0 or less is still refused.
func Hold(index int, ipv6 bool) func(network, address string, c syscall.RawConn) error {
	_ = ipv6
	return func(_, _ string, _ syscall.RawConn) error {
		if index <= 0 {
			return &RefusalError{Reason: ReasonInterfaceUnknown}
		}
		return nil
	}
}

// HostModel reads the interface table of IPv4, or of IPv6 where ipv6, in one
// GetIpInterfaceTable call: each row's interface index and its weak host
// send, weak host receive and forwarding flags, read from the table's bytes
// at the native row size (parseInterfaceTable), never by indexing x/sys's row
// type, which is 8 bytes longer. A table that does not read is
// ReasonHostModelUnreadable.
func HostModel(ipv6 bool) (Table, error) {
	family := uint16(windows.AF_INET)
	if ipv6 {
		family = windows.AF_INET6
	}
	var tab *windows.MibIpInterfaceTable
	if err := windows.GetIpInterfaceTable(family, &tab); err != nil {
		return nil, &RefusalError{Reason: ReasonHostModelUnreadable, Err: fmt.Errorf("GetIpInterfaceTable: %w", err)}
	}
	defer windows.FreeMibTable(unsafe.Pointer(tab))
	return parseInterfaceTable(unsafe.Slice((*byte)(unsafe.Pointer(tab)),
		interfaceTableRowsAt+interfaceRowSize*int(tab.NumEntries)))
}

// hostModelEntry reads one interface's row with GetIpInterfaceEntry, the
// second API §9.5's qualification compares the table's rows with. It keeps
// x/sys's row type: GetIpInterfaceEntry writes the native row's 168 bytes into
// its 176, and the four fields read here lie below offset 164, where the two
// layouts agree (they part at TransmitOffload).
func hostModelEntry(index int, ipv6 bool) (Row, error) {
	family := uint16(windows.AF_INET)
	if ipv6 {
		family = windows.AF_INET6
	}
	row := windows.MibIpInterfaceRow{Family: family, InterfaceIndex: uint32(index)}
	if err := windows.GetIpInterfaceEntry(&row); err != nil {
		return Row{}, err
	}
	return Row{
		Index:           int(row.InterfaceIndex),
		WeakHostSend:    row.WeakHostSend != 0,
		WeakHostReceive: row.WeakHostReceive != 0,
		Forwarding:      row.ForwardingEnabled != 0,
	}, nil
}
