//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// TestAShareNothingHandleRefusesARead is a probe, and a temporary one (backlog
// B898FU2): on CI's Windows leg the harvest helper's real-hold case
// (…HarvestHelperReadsPastAHoldOnTheStateFile, which B898's third PR removed)
// found os.ReadFile answered beside a handle on the state file that shares
// nothing. In a file of t.TempDir()'s and in the harvest state file
// pendingCoverState writes, each written afresh for each hold, it holds the
// file three ways: (a) GENERIC_READ sharing nothing, the removed case's hold;
// (b) GENERIC_READ and GENERIC_WRITE sharing nothing; (c) os.OpenFile's
// read-write handle, which shares reading and writing, with LockFileEx's
// exclusive lock over every byte, a byte-range lock and no share mode. Beside
// each hold it records what os.ReadFile answers, and what an open for reading
// without FILE_FLAG_BACKUP_SEMANTICS (which os.Open's read-only open sets), one
// for writing and one for deleting answer; whether the held handle and a handle
// the path opens asking no access, which no hold refuses, name one file; the
// volume's file system; and the handle's final path. It fails every time,
// whatever (a) did, with one message holding every fact: a package that passes
// prints nothing in CI's go test, which is not verbose, so the failure is how
// the facts reach its log. The next round removes it.
func TestAShareNothingHandleRefusesARead(t *testing.T) {
	facts := []string{holdProbeProcess()}
	t.Cleanup(func() {
		t.Errorf("the probe's facts, every case's (backlog B898FU2):\n%s", strings.Join(facts, "\n"))
	})
	files := []struct {
		name  string
		write func() string
	}{
		{"a file of t.TempDir()'s", func() string {
			path := filepath.Join(t.TempDir(), "probe.json")
			if err := os.WriteFile(path, []byte(`{"resultCursor":42}`), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{"the harvest state file", func() string { return harvestStatePath(pendingCoverState(t)) }},
	}
	holds := []struct {
		name string
		open func(path string) (windows.Handle, func() error, error)
	}{
		{"(a) GENERIC_READ, share 0", holdProbeShareNothing(windows.GENERIC_READ)},
		{"(b) GENERIC_READ|GENERIC_WRITE, share 0", holdProbeShareNothing(windows.GENERIC_READ | windows.GENERIC_WRITE)},
		{"(c) os.OpenFile O_RDWR, then LockFileEx exclusive over every byte", holdProbeLockEveryByte},
	}
	for _, f := range files {
		for _, h := range holds {
			facts = append(facts, f.name+", "+h.name+": "+holdProbeCase(t, f.write(), h.open))
		}
	}
}

// holdProbeCase holds path as open does, its open asked again while Windows
// refuses it for a moment, as the removed case's was (a scanner may hold a file
// just written), and says what each step beside the hold answered; it lets the
// hold go before it returns, and the test's end does too.
func holdProbeCase(t *testing.T, path string, open func(string) (windows.Handle, func() error, error)) string {
	var h windows.Handle
	var letGo func() error
	attempts := 0
	if err := atomicwrite.ReadWithRetryCtx(t.Context(), func() (err error) {
		attempts++
		h, letGo, err = open(path)
		return err
	}); err != nil {
		return fmt.Sprintf("the hold did not open in %d attempts: %s", attempts, holdProbeErr(err))
	}
	release := sync.OnceValue(letGo)
	t.Cleanup(func() { _ = release() })
	facts := []string{fmt.Sprintf("held at attempt %d", attempts)}
	if b, err := os.ReadFile(path); err != nil {
		facts = append(facts, "os.ReadFile refused, "+holdProbeErr(err))
	} else {
		facts = append(facts, fmt.Sprintf("os.ReadFile not refused, %d bytes read", len(b)))
	}
	facts = append(facts,
		"beside it, an open for reading without FILE_FLAG_BACKUP_SEMANTICS "+holdProbeOpen(path, windows.GENERIC_READ),
		"for writing "+holdProbeOpen(path, windows.GENERIC_WRITE),
		"for deleting "+holdProbeOpen(path, windows.DELETE),
		"identity: "+holdProbeIdentity(path, h),
		"file system: "+holdProbeVolume(h),
		"final path: "+holdProbeFinalPath(h))
	if err := release(); err != nil {
		facts = append(facts, "letting go: "+holdProbeErr(err))
	}
	return strings.Join(facts, "; ")
}

// holdProbeShareNothing opens a path's hold asking for access and sharing
// nothing, as the removed case's hold did with GENERIC_READ alone; the let-go
// closes the handle.
func holdProbeShareNothing(access uint32) func(string) (windows.Handle, func() error, error) {
	return func(path string) (windows.Handle, func() error, error) {
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return 0, nil, err
		}
		h, err := windows.CreateFile(name, access, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			return 0, nil, err
		}
		return h, func() error { return windows.CloseHandle(h) }, nil
	}
}

// holdProbeLockEveryByte opens a path's hold through os.OpenFile's read-write
// handle, which shares reading and writing, and locks every byte of it with
// LockFileEx's exclusive lock, failing at once where it cannot; the let-go
// unlocks and closes the file.
func holdProbeLockEveryByte(path string) (windows.Handle, func() error, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, nil, err
	}
	h := windows.Handle(f.Fd())
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, ^uint32(0), ^uint32(0), new(windows.Overlapped)); err != nil {
		_ = f.Close()
		return 0, nil, &os.PathError{Op: "LockFileEx", Path: path, Err: err}
	}
	return h, func() error {
		return errors.Join(windows.UnlockFileEx(h, 0, ^uint32(0), ^uint32(0), new(windows.Overlapped)), f.Close())
	}, nil
}

// holdProbeOpen opens path asking for access, without
// FILE_FLAG_BACKUP_SEMANTICS and sharing reading, writing and deleting, so that
// only the hold's share mode can refuse it, closes what it opened, and says
// what came of it.
func holdProbeOpen(path string, access uint32) string {
	name, err := windows.UTF16PtrFromString(path)
	if err == nil {
		var h windows.Handle
		if h, err = windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0); err == nil {
			_ = windows.CloseHandle(h)
			return "opened"
		}
	}
	return "refused, " + holdProbeErr(err)
}

// holdProbeIdentity says whether the held handle and a handle the path opens
// beside it, asking no access and sharing reading, writing and deleting (a
// metadata open, which no hold refuses), name one file, by the volume serial
// number and file index GetFileInformationByHandle answers for each. An
// identity it could not read is unavailable, said apart from one that differs.
func holdProbeIdentity(path string, held windows.Handle) string {
	var hi, mi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(held, &hi); err != nil {
		return "unavailable, the held handle's: " + holdProbeErr(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "unavailable: " + err.Error()
	}
	m, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return "unavailable, the metadata open of the path refused: " + holdProbeErr(err)
	}
	defer func() { _ = windows.CloseHandle(m) }()
	if err := windows.GetFileInformationByHandle(m, &mi); err != nil {
		return "unavailable, the metadata handle's: " + holdProbeErr(err)
	}
	of := func(i windows.ByHandleFileInformation) string {
		return fmt.Sprintf("volume %08x, index %08x%08x, %d bytes, %d links", i.VolumeSerialNumber, i.FileIndexHigh, i.FileIndexLow, uint64(i.FileSizeHigh)<<32|uint64(i.FileSizeLow), i.NumberOfLinks)
	}
	if hi.VolumeSerialNumber == mi.VolumeSerialNumber && hi.FileIndexHigh == mi.FileIndexHigh && hi.FileIndexLow == mi.FileIndexLow {
		return "the same file (" + of(hi) + ")"
	}
	return "another file: the held handle's " + of(hi) + ", the path's " + of(mi)
}

// holdProbeVolume names the file system of the volume the held handle is on,
// with its serial number and flags, from GetVolumeInformationByHandle.
func holdProbeVolume(held windows.Handle) string {
	var serial, maxComponent, flags uint32
	fs := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeInformationByHandle(held, nil, 0, &serial, &maxComponent, &flags, &fs[0], uint32(len(fs))); err != nil {
		return "unavailable: " + holdProbeErr(err)
	}
	return fmt.Sprintf("%s (serial %08x, flags %#08x)", windows.UTF16ToString(fs), serial, flags)
}

// holdProbeFinalPath is the held handle's final path, normalized, with the
// volume's drive letter, as GetFinalPathNameByHandle answers it.
func holdProbeFinalPath(held windows.Handle) string {
	buf := make([]uint16, 1024)
	n, err := windows.GetFinalPathNameByHandle(held, &buf[0], uint32(len(buf)), 0)
	switch {
	case err != nil:
		return "unavailable: " + holdProbeErr(err)
	case n >= uint32(len(buf)):
		return fmt.Sprintf("unavailable: longer than %d characters", len(buf)-1)
	}
	return windows.UTF16ToString(buf[:n])
}

// holdProbeProcess says what this process runs on and as: its Go, the Windows
// build RtlGetVersion answers, whether its token is elevated, and whether it
// holds SeBackupPrivilege, which a read-only open's FILE_FLAG_BACKUP_SEMANTICS
// asks to use, enabled.
func holdProbeProcess() string {
	v := windows.RtlGetVersion()
	token := windows.GetCurrentProcessToken()
	return fmt.Sprintf("this process: %s, Windows %d.%d.%d, elevated %v, SeBackupPrivilege %s",
		runtime.Version(), v.MajorVersion, v.MinorVersion, v.BuildNumber, token.IsElevated(), holdProbeBackupPrivilege(token))
}

// holdProbeBackupPrivilege says whether token holds SeBackupPrivilege, and
// whether it is enabled, from the token's own list of privileges.
func holdProbeBackupPrivilege(token windows.Token) string {
	name, err := windows.UTF16PtrFromString("SeBackupPrivilege")
	if err != nil {
		return "unknown: " + err.Error()
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		return "unknown: " + holdProbeErr(err)
	}
	buf := make([]byte, 4096)
	var n uint32
	if err := windows.GetTokenInformation(token, windows.TokenPrivileges, &buf[0], uint32(len(buf)), &n); err != nil {
		return "unknown: " + holdProbeErr(err)
	}
	for _, p := range (*windows.Tokenprivileges)(unsafe.Pointer(&buf[0])).AllPrivileges() {
		if p.Luid == luid {
			if p.Attributes&windows.SE_PRIVILEGE_ENABLED != 0 {
				return "held, enabled"
			}
			return "held, not enabled"
		}
	}
	return "not held"
}

// holdProbeErr says what an error was: its operation where it names one, and
// its errno by name where it is one of the three the holds can give, else by
// number and the system's words.
func holdProbeErr(err error) string {
	var errno windows.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	op := ""
	var pe *os.PathError
	if errors.As(err, &pe) {
		op = pe.Op + ": "
	}
	switch errno {
	case windows.ERROR_SHARING_VIOLATION:
		return op + "ERROR_SHARING_VIOLATION (32)"
	case windows.ERROR_ACCESS_DENIED:
		return op + "ERROR_ACCESS_DENIED (5)"
	case windows.ERROR_LOCK_VIOLATION:
		return op + "ERROR_LOCK_VIOLATION (33)"
	}
	return fmt.Sprintf("%serrno %d (%v)", op, uint32(errno), errno)
}
