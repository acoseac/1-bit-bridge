//go:build windows

package updater

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

// scmStopWait is how long we wait for the SCM service to reach the
// Stopped state after we send svc.Stop. Long enough for an in-flight
// `/v1/download` to flush (the active-sessions gate above us already
// refused if any are in flight, but transient TCP teardown can still
// take a beat); short enough that an operator running `bridge update`
// gets feedback within a typical patience window.
const scmStopWait = 15 * time.Second

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
func swapBinary(dst, newBinary, backupExt string) error {
	bak := dst + backupExt

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
				fmt.Fprintf(os.Stderr,
					"updater: SCM service started fine on next boot, but immediate restart failed: %v\n",
					err)
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

	// Rename trick. os.Rename on Windows uses MoveFileEx with
	// MOVEFILE_REPLACE_EXISTING, so a stale .bak is silently
	// overwritten. Wrap each step's error so the operator can tell
	// which side of the swap failed.
	//
	// Note on B35 (the swap_unix.go "hardlink dst→bak, then rename over
	// dst" fix that keeps dst present throughout): it deliberately does
	// NOT apply here. Windows lets you rename a running/locked .exe out of
	// the way (the trick above) but refuses to REPLACE it in place —
	// MoveFileEx over a mapped, running image fails with a sharing
	// violation. So the running dst MUST be vacated to bak first, then the
	// new binary placed at the now-empty dst; the tiny no-file window
	// between the two renames is structurally unavoidable on this OS.
	// NTFS metadata journaling ($LogFile) keeps each individual rename
	// crash-consistent, and the boot-time rollback marker + SCM restart
	// recover the rare crash-in-window case.
	if err := os.Rename(dst, bak); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", dst, bak, err)
	}
	if err := os.Rename(newBinary, dst); err != nil {
		// Restore .bak so we don't leave the operator with no
		// executable.
		if rerr := os.Rename(bak, dst); rerr != nil {
			return fmt.Errorf("install %s -> %s failed (%v); rollback also failed (%v); manual recovery needed",
				newBinary, dst, err, rerr)
		}
		return fmt.Errorf("install %s -> %s: %w (rolled back)", newBinary, dst, err)
	}

	// No directory fsync on Windows: FlushFileBuffers rejects directory
	// handles (ERROR_ACCESS_DENIED / ERROR_INVALID_HANDLE) regardless of
	// the access right requested, so the old CreateFile + FlushFileBuffers
	// block was a dead no-op. Durability of the rename is already
	// guaranteed by os.Rename (MoveFileEx) + NTFS metadata journaling
	// ($LogFile). (Gemini r2 review; supersedes the PR #48 GENERIC_WRITE
	// note.)
	return nil
}

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

// stopServiceIfRunning attempts to stop the bridge SCM service so
// the swap doesn't race a running process. Returns:
//
//   - (handle, nil) when the service was running and is now stopped.
//     Caller MUST close the handle (via the deferred path) and is
//     responsible for restarting after the swap.
//   - (nil, errSCMUnavailable) when SCM access is denied (no admin).
//     Caller treats as "skip stop, proceed with rename".
//   - (nil, errServiceNotRegistered) when no service exists. Same
//     treatment.
//   - (nil, otherErr) on any other SCM-side failure.
//
// Already-stopped services return (nil, nil) — no handle needed,
// no restart action needed.
func stopServiceIfRunning() (*scmStopHandle, error) {
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
				fmt.Fprintf(os.Stderr,
					"updater: service did not stop within budget and the compensating restart also failed (a manual `sc start` may be needed): %v\n",
					serr)
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

// RollbackBinary restores dst.bak → dst. Mirror of swap_unix.go's
// implementation, with the same SCM-stop coordination as
// swapBinary.
func RollbackBinary(dst, backupExt string) error {
	bak := dst + backupExt
	if _, err := os.Stat(bak); err != nil {
		return fmt.Errorf("backup %s missing: %w", bak, err)
	}

	stoppedHandle, stoppedErr := stopServiceIfRunning()
	if stoppedHandle != nil {
		defer func() {
			if err := stoppedHandle.svc.Start(); err != nil {
				fmt.Fprintf(os.Stderr,
					"updater: post-rollback service restart failed (will start on next boot): %v\n",
					err)
			}
			stoppedHandle.svc.Close()
			stoppedHandle.scm.Disconnect()
		}()
	} else if stoppedErr != nil && !errors.Is(stoppedErr, errSCMUnavailable) && !errors.Is(stoppedErr, errServiceNotRegistered) {
		return fmt.Errorf("stop SCM service: %w", stoppedErr)
	}

	if err := os.Rename(bak, dst); err != nil {
		return fmt.Errorf("rollback rename %s -> %s: %w", bak, dst, err)
	}
	return nil
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
