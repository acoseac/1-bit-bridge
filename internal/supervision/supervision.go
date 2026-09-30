// Package supervision answers a single runtime question: "if this
// process exits to be restarted (RestartExitCode), will something
// relaunch us?"
//
// The admin UI's "Restart now" button delegates the actual relaunch
// to launchd / systemd / Windows SCM, expecting one of them to be
// supervising the bridge process. When the bridge is running
// unsupervised — a dev fixture started with `./bin/bridge serve`,
// a Windows install that didn't reach SCM, a foreground manual run
// — that assumption is false and "Restart now" reduces to "Stop
// now". Pre-fix the admin's confirm dialog even claimed "the page
// will become unreachable until the service manager relaunches it"
// when there was no service manager at all.
//
// `IsSupervised()` lets the admin UI tailor the button label and
// confirm wording so operators see honest text. The detection is a
// best-effort heuristic — false negatives (a custom systemd unit
// that strips INVOCATION_ID, an exotic supervision tree) make the
// UI more conservative than necessary, never less. We never claim
// supervised when we shouldn't.
//
// Detection by platform:
//
//   - macOS (launchd):   `XPC_SERVICE_NAME` env var, set by launchd
//     for jobs it manages. Present for any plist-installed daemon
//     or agent.
//   - Linux (systemd):   `INVOCATION_ID` env var, set by systemd
//     for services it spawns. The `JOURNAL_STREAM` companion var
//     could also be checked but `INVOCATION_ID` is the documented
//     "is this a systemd service" signal.
//   - Windows (SCM):     `svc.IsWindowsService()` from
//     `golang.org/x/sys/windows/svc` — already imported elsewhere
//     in the bridge for the SCM lifecycle commands.
//
// All three checks run on the appropriate platform; a `_test.go`
// pins the env-var path so a future refactor that drops one of
// the checks fails the build.
package supervision

// RestartExitCode is the status `bridge serve` exits with when it stops
// because a restart was requested (the console's Restart, "Install &
// restart", the auto-installer), where a stop (SIGINT, SIGTERM, the SCM's
// Stop) exits 0. Each supervisor the bridge installs under relaunches it:
//
//   - launchd: the LaunchAgent `bridge init` writes keeps the job alive on
//     an unsuccessful exit only (KeepAlive {SuccessfulExit: false}), so the
//     exit 0 a restart request made until backlog B201 left the bridge down
//     (measured: "restarting": true, then `last exit code = 0`, not
//     running);
//   - the Windows SCM: the service reports this as its service-specific
//     exit code, a failure, which its recovery actions answer by starting
//     it again (FailureActionsOnNonCrashFailures; packaging sets both);
//   - systemd: the unit's Restart=always restarts any exit, and the journal
//     names this one 75/TEMPFAIL.
//
// 75 is sysexits.h's EX_TEMPFAIL, "a temporary failure; the user is invited
// to retry", which is what a supervisor does with it.
const RestartExitCode = 75

// IsSupervised returns true when the current process is running
// under a supervisor that will relaunch it after it exits with
// RestartExitCode.
// Platform implementations live in `supervision_unix.go` and
// `supervision_windows.go` behind build tags.
func IsSupervised() bool {
	return isSupervisedForOS()
}
