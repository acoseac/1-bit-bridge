//go:build windows

package updater

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

// scmStopWait, how long the swap waits for the SCM to stop the service, is
// in swapbound.go, where its bound against the install marker's lock's wait
// is checked on every platform.

// swapBinary atomically replaces the running bridge.exe on Windows.
//
// **Why this works at all**: Windows allows `MoveFile` (which is what
// `os.Rename` calls under the hood with MOVEFILE_REPLACE_EXISTING)
// against a running executable. The running process holds the file
// open via the image loader's section object, but the directory entry
// is what `MoveFile` updates — the inode equivalent (file kernel
// object) is preserved across the rename, the running process keeps
// reading from the old bytes, and the next exec loads the new ones.
//
// **SCM coordination**: when bridge.exe was installed via
// `bridge init` as a Windows Service, the SCM holds a handle to the
// path that prevents the rename in some configurations
// (specifically: ImagePathName is canonicalised at service start, so
// a swap-then-restart works, but if the service is currently running
// we want to stop it first to flush any in-flight HTTP responses
// cleanly). When SCM access is available AND the service is in the
// Running state, we stop it before the rename and restart after.
//
// **Without admin rights**: `mgr.Connect` fails with "access is
// denied" — we treat that as "service-management isn't available
// here" and fall through to the rename-only path. That path still
// works for non-service installs (Startup-folder shortcut from
// `installWindowsStartup`, which doesn't take an SCM file lock) and
// for any future user-mode install layouts.
//
// **markSwapStarted** (nil in tests) is invoked immediately before the
// first rename — deliberately AFTER the SCM stop, which is where this
// platform's marker-armed-but-nothing-mutated window lived: the stop can
// block for scmStopWait (15 s), and a kill in there used to leave the
// next boot renaming a two-versions-old .bak over a healthy binary. See
// State.SwapStarted. And after the staging and the probes, whose waits
// for another process's hold come before anything is armed (backlog B898;
// swapByVacate, which makes the renames). A non-nil error aborts before
// any rename; the deferred svc.Start still brings the service back.
func swapBinary(dst, newBinary, backupExt string, markSwapStarted func() error) error {

	// SCM coordination: try to stop the service first. If we can't
	// reach SCM (no admin), or no service registered, or service
	// already stopped — those are all "skip the stop step" cases,
	// not failures. The post-rename restart symmetry only fires
	// when we successfully stopped, so a fall-through path doesn't
	// leave a service flapping.
	stoppedHandle, stoppedErr := stopServiceIfRunning()
	if stoppedHandle != nil {
		defer func() {
			// Re-start the service after the swap. The service is
			// configured StartAutomatic by InstallWindowsService, so
			// the next boot would start it anyway — but operators
			// running `bridge update` expect to come back to a live
			// bridge without rebooting.
			if err := stoppedHandle.svc.Start(); err != nil {
				// Log; don't escalate. The rename succeeded and SCM
				// auto-start will pick the new binary up on next
				// boot. Better to surface a clear "Install completed
				// but service didn't restart cleanly" hint than fail
				// the whole install on a transient SCM hiccup.
				logger.Warn("post-swap service restart failed; the new binary will load on next boot via SCM auto-start", "err", err)
			}
			stoppedHandle.svc.Close()
			stoppedHandle.scm.Disconnect()
		}()
	} else if stoppedErr != nil && !errors.Is(stoppedErr, errSCMUnavailable) && !errors.Is(stoppedErr, errServiceNotRegistered) {
		// Genuine SCM error (service registered + running but stop
		// failed). The rename below would go through but leave a
		// running old service holding the file open in some edge
		// cases. Surface as a clear error rather than letting the
		// swap proceed against a likely-locked file.
		return fmt.Errorf("stop SCM service: %w", stoppedErr)
	}

	// Rename trick (swapByVacate). os.Rename on Windows uses MoveFileEx
	// with MOVEFILE_REPLACE_EXISTING, so a stale .bak is silently
	// overwritten.
	//
	// Note on B35 (the swap_unix.go "hardlink dst→bak, then rename over
	// dst" fix that keeps dst present throughout): it deliberately does
	// NOT apply here. Windows lets you rename a running/locked .exe out of
	// the way (the trick above) but refuses to REPLACE it in place —
	// MoveFileEx over a mapped, running image fails with a sharing
	// violation. So the running dst MUST be vacated to bak first, then the
	// new binary placed at the now-empty dst.
	//
	// The new bytes are beside dst before anything is vacated. This is not
	// a fallback here — the hardlink trick cannot apply on Windows at all,
	// so EVERY swap on this OS goes through the two-rename shape, and
	// until the staging moved ahead of the vacate every one of them called
	// placeNewBinaryWindows with dst already gone. That function's own
	// docblock named the host where its cross-volume branch fires:
	// "bridge.exe on D: and the data dir under %LOCALAPPDATA% on C: — a
	// small-SSD media PC — EVERY update failed". On exactly that host, every
	// update then spent the duration of a ~30 MiB copy plus a flush with no
	// bridge.exe on disk.
	//
	// No directory fsync on Windows: FlushFileBuffers rejects directory
	// handles (ERROR_ACCESS_DENIED / ERROR_INVALID_HANDLE) regardless of
	// the access right requested, so the old CreateFile + FlushFileBuffers
	// block was a dead no-op. Durability of the rename is already
	// guaranteed by os.Rename (MoveFileEx) + NTFS metadata journaling
	// ($LogFile). (Gemini r2 review; supersedes the PR #48 GENERIC_WRITE
	// note.)
	return swapByVacate(dst, newBinary, backupExt, markSwapStarted)
}

// installedMode is the permission bits a staged binary takes: none, since
// NTFS has no executable bit to set (stageIntoDir's mode 0).
func installedMode(string) os.FileMode { return 0 }

// scmStopHandle bundles the SCM connection + service handle the
// caller needs to keep alive across the rename + restart pair.
// The defer in swapBinary closes both — operators don't see this
// type.
type scmStopHandle struct {
	scm *mgr.Mgr
	svc *mgr.Service
}

var (
	errSCMUnavailable       = errors.New("SCM access denied (need admin)")
	errServiceNotRegistered = errors.New("bridge service not registered with SCM")
)

// isCrossDeviceErr reports whether a rename failed because the two paths
// live on different volumes — the one condition that makes staging fall
// back to a copy.
//
// Go's Windows os.Rename is MoveFileEx WITHOUT MOVEFILE_COPY_ALLOWED
// (see internal/syscall/windows), so a cross-volume move returns
// ERROR_NOT_SAME_DEVICE rather than doing the copy itself. The staged
// binary lives under <DataDir>/updates, which need not share a volume
// with the install path — that is the D:-and-C: host this branch exists
// for (#522 on the Unix side; Windows had no counterpart and no test,
// F0b).
func isCrossDeviceErr(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE)
}

func stopServiceIfRunning() (*scmStopHandle, error) {
	if inService, err := svc.IsWindowsService(); err == nil && inService {
		logger.Info("skipping SCM stop: this process IS the service (a self-stop would kill the install mid-swap)")
		return nil, nil
	}
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSCMUnavailable, err)
	}
	s, err := m.OpenService(packaging.ServiceLabel)
	if err != nil {
		m.Disconnect()
		return nil, fmt.Errorf("%w: %v", errServiceNotRegistered, err)
	}
	state, err := s.Query()
	if err != nil {
		s.Close()
		m.Disconnect()
		return nil, fmt.Errorf("query service: %w", err)
	}
	switch state.State {
	case svc.Stopped:
		s.Close()
		m.Disconnect()
		return nil, nil // nothing to do
	case svc.StopPending:
		// Service is already stopping (operator hit Stop manually,
		// or a prior Control(Stop) is still settling). Sending
		// another Stop here returns ERROR_SERVICE_CANNOT_ACCEPT_CTRL
		// — skip it, just wait for the existing stop to finish
		// (Gemini flagged on PR #48).
		if werr := waitServiceStopped(s, scmStopWait); werr != nil {
			s.Close()
			m.Disconnect()
			return nil, werr
		}
		return &scmStopHandle{scm: m, svc: s}, nil
	default:
		if _, err := s.Control(svc.Stop); err != nil {
			s.Close()
			m.Disconnect()
			return nil, fmt.Errorf("send stop: %w", err)
		}
		if werr := waitServiceStopped(s, scmStopWait); werr != nil {
			// We successfully SENT the stop but the service didn't reach
			// Stopped within the budget. swapBinary treats this returned
			// error as fatal and aborts the install BEFORE the rename —
			// but the stop is already in flight, so returning without a
			// restart would leave the bridge offline until a manual
			// `sc start` / reboot (B34). WE initiated the stop, so we own
			// bringing it back: best-effort Start before surfacing werr.
			// A service still in StopPending may refuse Start; that's the
			// best we can do, and werr (the real failure) still surfaces.
			if serr := s.Start(); serr != nil {
				logger.Error("service stop timed out and the compensating restart also failed; a manual `sc start` may be needed", "err", serr)
			}
			s.Close()
			m.Disconnect()
			return nil, werr
		}
		return &scmStopHandle{scm: m, svc: s}, nil
	}
}

// waitServiceStopped polls Service.Query until the state reaches
// Stopped or the timeout expires. Mirror of the helper in
// internal/packaging/service_windows.go — duplicated here rather
// than imported so the updater's swap path doesn't pull in the
// packaging package's full surface.
func waitServiceStopped(s *mgr.Service, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("query service: %w", err)
		}
		if st.State == svc.Stopped {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("service did not stop within %s", timeout)
}

// RollbackBinary restores dst.bak → dst. The restore itself is not the
// Unix rename: Windows refuses to replace a mapped running image, and
// every caller runs inside the process that maps dst.
// replaceRunningBinary moves that image aside first.
//
// It stops no SCM service (backlog B898; until then it made swapBinary's
// stop, up to scmStopWait, under the install marker's lock). Its callers
// all run in serve: the console's Roll back, the early, crash and boot
// rollbacks. A rename of the mapped image needs no stop, the vacate
// being a rename of it, and the stop was skipped already where serve is
// the service. In a foreground serve that holds its data dir's lock, a
// bridge service the stop could reach serves another data dir, another
// install the rollback has no business stopping. A serve that could not
// take its data dir's lock (a lock file it cannot open or lock, which it
// starts without; a filesystem that keeps no locks refuses the install
// marker's lock too, so no rollback runs there) may have a bridge
// service beside it on the same data dir, which it says it cannot rule
// out: that service runs its image until it restarts, and its next start
// runs the binary put back.
func RollbackBinary(dst, backupExt string) error {
	bak := dst + backupExt
	if _, err := os.Stat(bak); err != nil {
		return fmt.Errorf("backup %s missing: %w", bak, err)
	}
	return replaceRunningBinary(dst, bak)
}

// RemoveBackup deletes dst.bak. Same semantics as the Unix
// implementation: missing file is not an error, post-successful-
// install housekeeping calls this on the next boot.
func RemoveBackup(dst, backupExt string) error {
	bak := dst + backupExt
	err := os.Remove(bak)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
