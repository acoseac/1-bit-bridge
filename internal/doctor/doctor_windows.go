//go:build windows

package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"slices"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// isAddrInUse reports whether a listen error is "address already in use".
//
// The WSAEADDRINUSE arm is LOAD-BEARING and must not be "simplified" away to
// the POSIX form. On Windows `syscall.EADDRINUSE` is an INVENTED constant —
// zerrors_windows.go defines APPLICATION_ERROR = 1<<29 and derives EADDRINUSE
// from it (0x20000002) under the comment "Invented values to support what
// package os and others expects". The OS returns WSAEADDRINUSE (10048), which
// stdlib `syscall` does not define at all (it lives only in x/sys/windows),
// and neither Errno.Is nor anything in net/ bridges the two. So a bare
// errors.Is(err, syscall.EADDRINUSE) is ALWAYS false here.
//
// It is kept alongside the WSA form only so an error that has already been
// normalised to the POSIX errno by some other layer still matches.
//
// Reviewers (and review bots) periodically flag this as a cross-package
// type mismatch — "net.Listen returns syscall.Errno, windows.WSAEADDRINUSE
// is a windows.Errno, so errors.Is can never match" — and suggest
// syscall.WSAEADDRINUSE instead. Both halves of that are wrong, so before
// re-raising it, check the two source lines:
//
//   - x/sys/windows/aliases.go declares `type Errno = syscall.Errno` — a
//     type ALIAS, not a defined type. There is no distinct type here.
//   - x/sys/windows/zerrors_windows.go declares
//     `WSAEADDRINUSE syscall.Errno = 10048` — already the stdlib type.
//
// So errors.Is compares syscall.Errno(10048) against itself and matches.
// The suggested "fix" would not even compile: stdlib syscall on Windows
// defines only a handful of WSA constants (WSAECONNRESET and friends) and
// WSAEADDRINUSE is not among them.
func isAddrInUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, syscall.EADDRINUSE)
}

// pidAlive reports whether a process with this PID currently exists.
//
// Unlike unix, os.FindProcess DOES return an error for a dead PID here —
// but it is not usable as-is. Go opens the handle with
// STANDARD_RIGHTS_READ|PROCESS_QUERY_INFORMATION|SYNCHRONIZE, and
// PROCESS_QUERY_INFORMATION can be denied ACROSS INTEGRITY LEVELS, so a
// LIVE bridge running as a scheduled task under another account reads as
// dead. That is exactly the home-pc install shape.
// PROCESS_QUERY_LIMITED_INFORMATION exists for this and succeeds across
// integrity levels.
//
// Error discrimination is load-bearing: ERROR_ACCESS_DENIED means ALIVE
// (the process exists, we just can't open it); a dead PID surfaces as
// ERROR_INVALID_PARAMETER. Collapsing "any error means dead" would
// re-introduce the cross-account misread this helper is here to fix.
//
// A terminated-but-not-yet-reaped process can still be opened and so
// reads as alive. Here that costs a hint and never a verdict. Both port
// ladders ask liveness only after the owner probe missed the pid, and
// Windows' listener table rules out every pid it does not name
// (listenerTableSighting), so a held port FAILs either way: liveness picks
// between naming our bridge as running and calling the holder "another
// process". A healthy install never gets here, since the table names its
// listener's pid.
func pidAlive(pid int) bool {
	// Windows PIDs are a DWORD, so anything past uint32 is not a pid at
	// all — and the cast below would SILENTLY TRUNCATE it into one that
	// may well exist (4294967297 wraps to 1). readPID parses with
	// strconv.Atoi into an int, so on a 64-bit host a corrupt or
	// hand-edited pidfile reaches here with exactly that value.
	if pid <= 0 || pid > math.MaxUint32 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	_ = windows.CloseHandle(h)
	return true
}

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
// API errors) so checkPort degrades to Warn rather than a hard Fail. When
// it answers false with no error, the sighting names the pids the tables
// give for the port (listenerTableSighting): this probe sees every
// listener's owner, so a miss here rules the pid out, and the hint says so
// rather than blaming a capability Windows does not have.
//
// The context is accepted for signature parity with the unix twin and goes
// unused: there is no subprocess to bound here, and GetExtendedTcpTable is
// an in-memory kernel table read with no blocking failure mode to guard
// against. Keeping the parameter means the caller has one shape to reason
// about — and means a future Windows probe that DOES block has the context
// already in hand.
func isPIDListeningOnPort(_ context.Context, port, targetPID int) (bool, ownerSighting, error) {
	// Resolve the proc explicitly: LazyProc.Call panics if Find fails, so
	// a missing/blocked iphlpapi.dll must be turned into an error here.
	if err := procGetExtendedTcp.Find(); err != nil {
		return false, ownerSighting{}, fmt.Errorf("iphlpapi GetExtendedTcpTable unavailable: %w", err)
	}
	// The two families share one scan (portOwnersFamily); only the row type
	// and the field-access closure differ. A match in the IPv4 table ends
	// the probe before the IPv6 table is read, as it always has, so an
	// IPv6 read that fails cannot turn a found pid into a probe error.
	v4, err := portOwnersFamily(windows.AF_INET, "INET", port, func(r mibTCPRowOwnerPID) (int, int) {
		return ntohsPort(r.LocalPort), int(r.OwningPID)
	})
	if err != nil {
		return false, ownerSighting{}, err
	}
	if slices.Contains(v4, targetPID) {
		return true, ownerSighting{}, nil
	}
	v6, err := portOwnersFamily(windows.AF_INET6, "INET6", port, func(r mibTCP6RowOwnerPID) (int, int) {
		return ntohsPort(r.LocalPort), int(r.OwningPID)
	})
	if err != nil {
		return false, ownerSighting{}, err
	}
	if slices.Contains(v6, targetPID) {
		return true, ownerSighting{}, nil
	}
	return false, listenerTableSighting(append(v4, v6...)), nil
}

// portOwnersFamily fetches the owner-PID LISTENER table for one address
// family and returns the owning pid of every row on port. Generic over the
// row type T so the IPv4 and IPv6 scans share one implementation — the row
// layouts differ, but the fetch + bounds-check + iterate shape is identical;
// row gives a row's (port, pid).
func portOwnersFamily[T any](family int, af string, port int, row func(T) (int, int)) ([]int, error) {
	buf, err := extendedTCPTable(family)
	if err != nil {
		return nil, err
	}
	rows, err := tcpTableEntries[T](buf, af)
	if err != nil {
		return nil, err
	}
	var owners []int
	for i := range rows {
		if p, pid := row(rows[i]); p == port {
			owners = append(owners, pid)
		}
	}
	return owners, nil
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
		// bufPtr stays an unsafe.Pointer (NOT a uintptr) so the GC keeps buf's
		// backing array alive: nil → 0 (NULL) drives the size-query call,
		// &buf[0] the fetch. We call syscall.Syscall6 DIRECTLY rather than
		// LazyProc.Call because syscall.Syscall6 carries //go:uintptrescapes —
		// the compiler pins each inline uintptr(unsafe.Pointer(...)) argument
		// (buf AND the stack-local &size) across the call, guaranteeing they
		// aren't moved or freed mid-syscall (unsafe.Pointer rule 4). LazyProc.Call
		// is a plain method that launders its args through a []uintptr, so it
		// gets none of that protection.
		var bufPtr unsafe.Pointer
		if len(buf) > 0 {
			bufPtr = unsafe.Pointer(&buf[0])
		}
		ret, _, _ := syscall.Syscall6(
			procGetExtendedTcp.Addr(),
			6, // GetExtendedTcpTable takes 6 args
			uintptr(bufPtr),
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

// inodeOf is the unix helper's Windows twin, for the untagged cgroup
// accounting (cgroupID), which only Linux reaches: a FileInfo here carries
// no inode number, so it answers none.
func inodeOf(fs.FileInfo) (uint64, bool) { return 0, false }
