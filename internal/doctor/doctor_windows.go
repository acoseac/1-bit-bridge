//go:build windows

package doctor

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// portProbeAvailable is always true on Windows — the native
// GetExtendedTcpTable probe needs no external binary (unlike lsof on
// unix). A per-call mechanism failure (e.g. an antivirus blocking the
// iphlpapi.dll load) is reported through isPIDListeningOnPort's error
// return, which checkPort turns into a Warn rather than a hard Fail.
// Package var so tests can stub it.
var portProbeAvailable = func() bool { return true }

// tcpTableOwnerPIDListener is the TCP_TABLE_CLASS for GetExtendedTcpTable
// that returns LISTENING sockets with their owning PID. The value is 3 —
// 4 is TCP_TABLE_OWNER_PID_CONNECTIONS (established sockets), which would
// miss every listener and make the probe useless.
const tcpTableOwnerPIDListener = 3

// Win32 return codes GetExtendedTcpTable branches on.
const (
	errSuccess            = 0
	errInsufficientBuffer = 122 // ERROR_INSUFFICIENT_BUFFER
)

// mibTCPRowOwnerPID mirrors MIB_TCPROW_OWNER_PID (IPv4); every field is a
// DWORD (uint32).
type mibTCPRowOwnerPID struct {
	State      uint32
	LocalAddr  uint32
	LocalPort  uint32
	RemoteAddr uint32
	RemotePort uint32
	OwningPID  uint32
}

// mibTCP6RowOwnerPID mirrors MIB_TCP6ROW_OWNER_PID (IPv6). The row layout
// differs from the v4 row (16-byte addresses + scope IDs); tcpTableEntries
// is instantiated separately per family, so each family gets its own typed,
// correctly-strided view of the buffer (the table-header shape lives in
// tcpTableEntries' local type).
type mibTCP6RowOwnerPID struct {
	LocalAddr     [16]byte
	LocalScopeID  uint32
	LocalPort     uint32
	RemoteAddr    [16]byte
	RemoteScopeID uint32
	RemotePort    uint32
	State         uint32
	OwningPID     uint32
}

var (
	iphlpapi           = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcp = iphlpapi.NewProc("GetExtendedTcpTable")
)

// isPIDListeningOnPort reports whether targetPID owns a listening socket on
// the given local TCP port, querying BOTH the IPv4 and IPv6 owner-PID
// listener tables natively via iphlpapi.dll (no shell-out). The error
// return signals a probe-MECHANISM failure (DLL/proc can't load, or the
// API errors) so checkPort degrades to Warn rather than a hard Fail.
func isPIDListeningOnPort(port, targetPID int) (bool, error) {
	// Resolve the proc explicitly: LazyProc.Call panics if Find fails, so
	// a missing/blocked iphlpapi.dll must be turned into an error here.
	if err := procGetExtendedTcp.Find(); err != nil {
		return false, fmt.Errorf("iphlpapi GetExtendedTcpTable unavailable: %w", err)
	}
	// The two families share one scan (pidOwnsPortFamily); only the row type
	// and the field-access closure differ. The closure captures port/targetPID.
	v4, err := pidOwnsPortFamily(windows.AF_INET, "INET", func(r mibTCPRowOwnerPID) bool {
		return ntohsPort(r.LocalPort) == port && int(r.OwningPID) == targetPID
	})
	if err != nil || v4 {
		return v4, err
	}
	return pidOwnsPortFamily(windows.AF_INET6, "INET6", func(r mibTCP6RowOwnerPID) bool {
		return ntohsPort(r.LocalPort) == port && int(r.OwningPID) == targetPID
	})
}

// pidOwnsPortFamily fetches the owner-PID LISTENER table for one address
// family and reports whether any row satisfies match. Generic over the row
// type T so the IPv4 and IPv6 scans share one implementation — the row
// layouts differ, but the fetch + bounds-check + iterate shape is identical.
func pidOwnsPortFamily[T any](family int, af string, match func(T) bool) (bool, error) {
	buf, err := extendedTCPTable(family)
	if err != nil {
		return false, err
	}
	rows, err := tcpTableEntries[T](buf, af)
	if err != nil {
		return false, err
	}
	for i := range rows {
		if match(rows[i]) {
			return true, nil
		}
	}
	return false, nil
}

// tcpTableEntries reinterprets a GetExtendedTcpTable buffer as the typed
// MIB_*TABLE_OWNER_PID for row type T — { DWORD NumEntries; T rows[…] } — and
// returns its rows. It bounds-checks the buffer against the entry count it
// declares BEFORE unsafe.Slice, so a short/corrupt buffer can't produce an
// out-of-bounds view (Offsetof/Sizeof are compile-time, no deref). The local
// table type carries the exact header shape per instantiation, so the v4 and
// v6 strides are honoured without a per-family wrapper struct.
func tcpTableEntries[T any](buf []byte, af string) ([]T, error) {
	if len(buf) == 0 {
		return nil, nil
	}
	type table struct {
		numEntries uint32
		rows       [1]T
	}
	t := (*table)(unsafe.Pointer(&buf[0]))
	hdr := int(unsafe.Offsetof(t.rows))
	if len(buf) < hdr || len(buf) < hdr+int(t.numEntries)*int(unsafe.Sizeof(t.rows[0])) {
		return nil, fmt.Errorf("GetExtendedTcpTable(af=%s): buffer too small for %d entries (%d bytes)", af, t.numEntries, len(buf))
	}
	return unsafe.Slice(&t.rows[0], t.numEntries), nil
}

// extendedTCPTable returns the raw GetExtendedTcpTable buffer for the given
// address family (windows.AF_INET / AF_INET6), TCP_TABLE_OWNER_PID_LISTENER
// class. It uses the canonical two-call sizing pattern wrapped in a bounded
// retry loop: the required size can GROW between the size query and the
// fetch as other processes open sockets (a TOCTOU race), so a repeated
// ERROR_INSUFFICIENT_BUFFER just means "grow and retry".
func extendedTCPTable(family int) ([]byte, error) {
	var buf []byte
	var size uint32
	for attempt := 0; attempt < 5; attempt++ {
		var ptr uintptr // 0 (NULL) for the size-query call; &buf[0] once allocated
		if len(buf) > 0 {
			ptr = uintptr(unsafe.Pointer(&buf[0]))
		}
		ret, _, _ := procGetExtendedTcp.Call(
			ptr,
			uintptr(unsafe.Pointer(&size)),
			0, // bOrder = FALSE — sort order is irrelevant to a membership test
			uintptr(family),
			uintptr(tcpTableOwnerPIDListener),
			0, // Reserved
		)
		switch ret {
		case errSuccess:
			return buf, nil
		case errInsufficientBuffer:
			buf = make([]byte, size)
		default:
			return nil, fmt.Errorf("GetExtendedTcpTable(af=%d): win32 error %d", family, ret)
		}
	}
	return nil, fmt.Errorf("GetExtendedTcpTable(af=%d): buffer kept growing after 5 attempts", family)
}

// ntohsPort converts a MIB row's LocalPort (the port in network byte order
// stored in the low 16 bits of a DWORD) to a host-order port number, via the
// stdlib-provided windows.Ntohs rather than a hand-rolled byteswap.
func ntohsPort(localPort uint32) int {
	return int(windows.Ntohs(uint16(localPort)))
}
