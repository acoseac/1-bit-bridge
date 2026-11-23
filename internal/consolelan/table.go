package consolelan

import (
	"encoding/binary"
	"fmt"
)

// The native layout of GetIpInterfaceTable's MIB_IPINTERFACE_TABLE and its
// MIB_IPINTERFACE_ROW rows (netioapi.h) on amd64 and arm64, which HostModel
// reads the table's bytes at (parseInterfaceTable). x/sys's
// windows.MibIpInterfaceRow is not used to step through the rows: it
// declares TransmitOffload and ReceiveOffload, natively NL_INTERFACE_OFFLOAD_ROD
// (one byte of one-bit BOOLEAN fields each), as uint32, so its row is 176
// bytes where the native row is 168, and indexing a table by it reads row k
// 8×k bytes past its start, another row's bytes from the second row on.
const (
	// interfaceTableRowsAt is where the rows begin: NumEntries, a uint32 at
	// 0, then padding to the rows' 8-byte alignment.
	interfaceTableRowsAt = 8
	// interfaceRowSize is one native row's size.
	interfaceRowSize = 168
	// The fields a row is read for, at their native offsets in the row:
	// InterfaceIndex (NET_IFINDEX, a uint32), and the BOOLEAN bytes
	// ForwardingEnabled, WeakHostSend and WeakHostReceive.
	interfaceRowIndexAt       = 16
	interfaceRowForwardingAt  = 41
	interfaceRowWeakSendAt    = 42
	interfaceRowWeakReceiveAt = 43
)

// parseInterfaceTable reads a MIB_IPINTERFACE_TABLE's bytes at the native
// layout: NumEntries (little-endian, at 0) rows of interfaceRowSize bytes from
// interfaceTableRowsAt, each row's interface index and its forwarding, weak
// host send and weak host receive flags, a BOOLEAN byte each, non-zero true.
// A buffer shorter than the header and the rows its NumEntries says is
// ReasonHostModelUnreadable.
func parseInterfaceTable(b []byte) (Table, error) {
	if len(b) < interfaceTableRowsAt {
		return nil, &RefusalError{Reason: ReasonHostModelUnreadable,
			Err: fmt.Errorf("the interface table is %d bytes, short of its header", len(b))}
	}
	n := binary.LittleEndian.Uint32(b)
	need := uint64(interfaceTableRowsAt) + uint64(interfaceRowSize)*uint64(n)
	if uint64(len(b)) < need {
		return nil, &RefusalError{Reason: ReasonHostModelUnreadable,
			Err: fmt.Errorf("the interface table is %d bytes, short of the %d its %d rows take", len(b), need, n)}
	}
	out := make(Table, 0, n)
	for k := 0; k < int(n); k++ {
		row := interfaceTableRowsAt + k*interfaceRowSize
		out = append(out, Row{
			Index:           int(binary.LittleEndian.Uint32(b[row+interfaceRowIndexAt:])),
			WeakHostSend:    b[row+interfaceRowWeakSendAt] != 0,
			WeakHostReceive: b[row+interfaceRowWeakReceiveAt] != 0,
			Forwarding:      b[row+interfaceRowForwardingAt] != 0,
		})
	}
	return out, nil
}
