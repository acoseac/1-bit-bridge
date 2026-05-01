//go:build windows

package supervision

import "golang.org/x/sys/windows/svc"

// isSupervisedForOS checks whether the current process is running
// under the Windows Service Control Manager. `svc.IsWindowsService`
// is the documented detection — it inspects the parent process and
// returns true only for SCM-launched services.
//
// A bridge installed via `bridge init` runs as an SCM service with
// recovery actions configured to restart on failure (mirrors
// launchd KeepAlive / systemd Restart=always). A bridge started
// from PowerShell or via the v0.1.1-hardened Startup-folder path
// is NOT under SCM and won't be relaunched after os.Exit; this
// helper returns false for those cases so the admin UI doesn't
// lie about auto-relaunch.
//
// On error from the Win32 API we fall back to "unsupervised" — the
// conservative answer (UI gets the manual-restart hint) is better
// than promising auto-relaunch on a process we can't classify.
func isSupervisedForOS() bool {
	inService, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return inService
}
