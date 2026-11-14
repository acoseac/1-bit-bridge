//go:build windows

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/acoseac/1-bit-bridge/internal/packaging"
	"github.com/acoseac/1-bit-bridge/internal/updater"
)

// The Restart Manager (rstrtmgr.dll), which x/sys/windows does not wrap.
var (
	modRstrtmgr             = windows.NewLazySystemDLL("rstrtmgr.dll")
	procRmStartSession      = modRstrtmgr.NewProc("RmStartSession")
	procRmRegisterResources = modRstrtmgr.NewProc("RmRegisterResources")
	procRmGetList           = modRstrtmgr.NewProc("RmGetList")
	procRmEndSession        = modRstrtmgr.NewProc("RmEndSession")
)

const (
	// cchRmSessionKey is CCH_RM_SESSION_KEY, a GUID's hex digits: the key
	// RmStartSession writes, NUL-terminated.
	cchRmSessionKey = 32
	// errorMoreData is ERROR_MORE_DATA, RmGetList's answer when the list it
	// has is longer than the room it was given.
	errorMoreData = 234
	// rmGetListTries bounds RmGetList's asks for more room: the list can
	// grow between one ask and the next.
	rmGetListTries = 8
)

// rmUniqueProcess is RM_UNIQUE_PROCESS.
type rmUniqueProcess struct {
	ProcessID        uint32
	ProcessStartTime windows.Filetime
}

// rmProcessInfo is RM_PROCESS_INFO: CCH_RM_MAX_APP_NAME (255) and
// CCH_RM_MAX_SVC_NAME (63) characters and a NUL each, the RM_APP_TYPE enum,
// AppStatus, TSSessionId and bRestartable.
type rmProcessInfo struct {
	Process          rmUniqueProcess
	AppName          [256]uint16
	ServiceShortName [64]uint16
	ApplicationType  uint32
	AppStatus        uint32
	TSSessionID      uint32
	Restartable      int32
}

// RM_PROCESS_INFO is 668 bytes on every Windows: 12, then 512 and 128 of
// names, then four 4-byte fields. A layout that differs fails the build.
var (
	_ [unsafe.Sizeof(rmProcessInfo{}) - 668]byte
	_ [668 - unsafe.Sizeof(rmProcessInfo{})]byte
)

// windowsErrorCode is the Windows error code a refused rename carries, and
// whether it says a file is in use: a sharing violation (32, "being used by
// another process", which the reds on windows-latest said), a lock
// violation (33), or Access is denied (5), which Windows answers a rename
// onto an image another process runs (backlog B202) and an open of a file a
// deletion is pending on, as well as a permission it lacks.
func windowsErrorCode(err error) (uint32, bool) {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return 0, false
	}
	switch errno {
	case windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION, windows.ERROR_ACCESS_DENIED:
		return uint32(errno), true
	}
	return uint32(errno), false
}

// candidateUsers is every process the Restart Manager lists as using the
// file at path, asked in a session of its own, which holds that file alone
// (RmGetList answers for all of a session's files together), with each
// process's image and the pid it was started by.
func candidateUsers(path string) ([]candidateUser, error) {
	for _, p := range []*windows.LazyProc{procRmStartSession, procRmRegisterResources, procRmGetList, procRmEndSession} {
		if err := p.Find(); err != nil {
			return nil, fmt.Errorf("rstrtmgr.dll: %w", err)
		}
	}
	var session uint32
	var key [cchRmSessionKey + 1]uint16
	if r, _, _ := syscall.SyscallN(procRmStartSession.Addr(), uintptr(unsafe.Pointer(&session)), 0, uintptr(unsafe.Pointer(&key[0]))); r != 0 {
		return nil, fmt.Errorf("RmStartSession: %w", syscall.Errno(r))
	}
	defer func() { _, _, _ = syscall.SyscallN(procRmEndSession.Addr(), uintptr(session)) }()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	names := []*uint16{name}
	if r, _, _ := syscall.SyscallN(procRmRegisterResources.Addr(), uintptr(session),
		1, uintptr(unsafe.Pointer(&names[0])), 0, 0, 0, 0); r != 0 {
		return nil, fmt.Errorf("RmRegisterResources: %w", syscall.Errno(r))
	}
	infos, err := rmList(session)
	if err != nil {
		return nil, err
	}
	parents, perr := parentPIDs()
	users := make([]candidateUser, 0, len(infos))
	for i := range infos {
		info := &infos[i]
		pid := info.Process.ProcessID
		u := candidateUser{
			pid: int(pid), parent: -1,
			app:     windows.UTF16ToString(info.AppName[:]),
			service: windows.UTF16ToString(info.ServiceShortName[:]),
		}
		// Zero, or none a process can have started at, leaves it zero.
		u.started, _ = filetimeStart(info.Process.ProcessStartTime.HighDateTime, info.Process.ProcessStartTime.LowDateTime)
		if img, err := processImage(pid); err != nil {
			u.image = fmt.Sprintf("not read (%v)", err)
		} else {
			u.image = img
		}
		if parent, ok := parents[pid]; ok && perr == nil {
			u.parent = int(parent)
		}
		users = append(users, u)
	}
	return users, nil
}

// rmList is RmGetList's list for session, asked again with the room it says
// it needs while it answers ERROR_MORE_DATA, at most rmGetListTries times.
func rmList(session uint32) ([]rmProcessInfo, error) {
	var infos []rmProcessInfo
	for range rmGetListTries {
		var needed, reasons uint32
		count := uint32(len(infos))
		var first *rmProcessInfo
		if len(infos) > 0 {
			first = &infos[0]
		}
		r, _, _ := syscall.SyscallN(procRmGetList.Addr(), uintptr(session), uintptr(unsafe.Pointer(&needed)),
			uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(first)), uintptr(unsafe.Pointer(&reasons)))
		switch r {
		case 0:
			return infos[:count], nil
		case errorMoreData:
			// A little room past what it needs: the list can grow meanwhile.
			infos = make([]rmProcessInfo, needed+4)
		default:
			return nil, fmt.Errorf("RmGetList: %w", syscall.Errno(r))
		}
	}
	return nil, fmt.Errorf("RmGetList still wanted more room after %d asks", rmGetListTries)
}

// processImage is the image of the process pid, as QueryFullProcessImageNameW
// gives it.
func processImage(pid uint32) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:size]), nil
}

// parentPIDs is each running process's parent pid, from a snapshot of the
// process list (CreateToolhelp32Snapshot).
func parentPIDs() (map[uint32]uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	parents := map[uint32]uint32{}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		parents[e.ProcessID] = e.ParentProcessID
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return parents, err
	}
	return parents, nil
}

// skipIfABridgeServiceIsRegistered skips unless the service manager says no
// bridge service exists: RollbackBinary stops a running one and starts it
// again (stopServiceIfRunning), and a test must not. It asks with the least
// rights that answer, which any user has (SC_MANAGER_CONNECT, then
// SERVICE_QUERY_STATUS), so an unprivileged runner is answered too, and runs
// only on ERROR_SERVICE_DOES_NOT_EXIST: a lookup that fails otherwise is no
// answer, and skips, failing closed. internal/updater's
// skipIfBridgeServiceRegistered is the same check.
func skipIfABridgeServiceIsRegistered(t *testing.T) {
	t.Helper()
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		t.Skipf("the service manager could not be asked whether a bridge service is registered (%v); RollbackBinary would stop one", err)
	}
	defer func() { _ = windows.CloseServiceHandle(scm) }()
	name, err := windows.UTF16PtrFromString(packaging.ServiceLabel)
	if err != nil {
		t.Fatal(err)
	}
	s, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS)
	switch {
	case err == nil:
		_ = windows.CloseServiceHandle(s)
		t.Skip("a bridge service is registered on this machine; RollbackBinary would stop it")
	case !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
		t.Skipf("the service manager could not say whether a bridge service is registered (%v); RollbackBinary would stop one", err)
	}
}

// holderChildEnv names the file this test binary, run again as the positive
// control's child, opens without sharing its deletion and holds until its
// stdin closes (TestTheHolderDiagnosticNamesTheChildThatHoldsTheBackup).
const holderChildEnv = "BRIDGE_B872_HOLD_BACKUP"

// holdUntilStdinCloses is the child: it opens path for reading, sharing
// reading and writing and not deletion, so a rename of the file is refused
// while it holds it, says "holding" on its stdout, and holds the file until
// its stdin closes, which the test does to let it go, and which a test
// binary that dies before its cleanup does too. Nothing ends it on a timer.
func holdUntilStdinCloses(path string) int {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open the backup:", err)
		return 2
	}
	defer windows.CloseHandle(h)
	fmt.Println("holding")
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

// TestTheHolderDiagnosticNamesTheChildThatHoldsTheBackup is the diagnostic's
// positive control (backlog B872 part 2): a child of this test, this binary
// run again, opens the backup without sharing its deletion, so the real
// RollbackBinary's rename of the backup over the binary is refused, a
// sharing violation, and the binary is put back from where it was set aside
// ("(restored)", its bytes as they were). The diagnostic, asked at that
// refusal, lists the child among the backup's candidate users, by its pid,
// its image (this test binary) and its parent (this process), and the
// binary, set aside then, as absent. The child holds the backup until the
// test closes its stdin, which ends it.
func TestTheHolderDiagnosticNamesTheChildThatHoldsTheBackup(t *testing.T) {
	if path := os.Getenv(holderChildEnv); path != "" {
		os.Exit(holdUntilStdinCloses(path))
	}
	skipIfABridgeServiceIsRegistered(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge.exe")
	bak := dst + ".bak"
	if err := os.WriteFile(dst, []byte("RUNNING"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, []byte("KNOWN-GOOD"), 0o755); err != nil {
		t.Fatal(err)
	}

	child := exec.Command(exe, "-test.run=^"+t.Name()+"$")
	child.Env = append(os.Environ(), holderChildEnv+"="+bak)
	said, sayer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Both ends, so a Start that fails leaks neither; the write end is
	// closed again below once the child has its copy.
	t.Cleanup(func() { _ = said.Close(); _ = sayer.Close() })
	child.Stdout = sayer
	var childErr bytes.Buffer
	child.Stderr = &childErr
	letGo, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Closed here too where Start fails, which leaves it to no Wait.
	t.Cleanup(func() { _ = letGo.Close() })
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	_ = sayer.Close()
	t.Cleanup(func() { _ = child.Process.Kill() })
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = child.Wait()
	}()
	drainLoopOnCleanup(t, func() { _ = letGo.Close() }, exited, "the child holding the backup")
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(said).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if strings.TrimSpace(line) != "holding" {
			// Its stderr is read once Wait has copied all of it.
			_ = child.Process.Kill()
			select {
			case <-exited:
				t.Fatalf("the child did not hold the backup (it said %q); its stderr: %s", line, childErr.String())
			case <-serveGiveUp(t):
				t.Fatalf("the child did not hold the backup (it said %q), and did not end once killed", line)
			}
		}
	case <-serveGiveUp(t):
		t.Fatal("the child never said it holds the backup")
	}

	h := watchRollbackHolders(t, dst)
	err = updater.RollbackBinary(dst, ".bak")
	if err == nil || !strings.HasSuffix(err.Error(), "(restored)") {
		t.Fatalf("RollbackBinary with the backup held = %v, want its rename refused and the binary put back (restored)", err)
	}
	if got, rerr := os.ReadFile(dst); rerr != nil || string(got) != "RUNNING" {
		t.Errorf("the binary after the refused rollback = %q (%v), want its own bytes, put back", got, rerr)
	}
	if code, inUse := windowsErrorCode(err); code != uint32(windows.ERROR_SHARING_VIOLATION) || !inUse {
		t.Errorf("the refusal carries Windows error %d (in use %v), want a sharing violation (32)", code, inUse)
	}
	report := strings.Join(h.lines(), "\n")
	users, asked := h.usersOf(updater.RollbackStageBackup, bak)
	if !asked {
		t.Fatalf("the diagnostic did not ask who uses the backup at its rename's refusal:\n%s", report)
	}
	pid := child.Process.Pid
	var held *candidateUser
	for i := range users {
		if users[i].pid == pid {
			held = &users[i]
		}
	}
	if held == nil {
		t.Fatalf("the diagnostic does not list the child (pid %d) among the backup's candidate users:\n%s", pid, report)
	}
	if held.parent != os.Getpid() {
		t.Errorf("the child's parent pid is %d, want this test's, %d:\n%s", held.parent, os.Getpid(), report)
	}
	img, ierr := os.Stat(held.image)
	self, serr := os.Stat(exe)
	if ierr != nil || serr != nil || !os.SameFile(img, self) {
		t.Errorf("the child's image is %q (%v), want this test binary, %s:\n%s", held.image, ierr, exe, report)
	}
	if !strings.Contains(report, dst+": absent") {
		t.Errorf("the diagnostic does not say the binary, set aside at the refusal, is absent:\n%s", report)
	}

	// The test lets the child go through its stdin, and it ends.
	_ = letGo.Close()
	select {
	case <-exited:
		if code := child.ProcessState.ExitCode(); code != 0 {
			t.Errorf("the child exited %d once its stdin closed, want 0; its stderr: %s", code, childErr.String())
		}
	case <-serveGiveUp(t):
		t.Error("the child did not end once its stdin closed")
	}
}
