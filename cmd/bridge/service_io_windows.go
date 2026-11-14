//go:build windows

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"

	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

// redirectServiceIO points os.Stdout and os.Stderr at a per-machine log
// file before the rest of serveCmd starts. The SCM doesn't attach a
// console to service processes, so any `fmt.Fprintln(stdout, ...)` or
// error log in bridge.yaml-parsing code would silently vanish without
// this redirect.
//
// Log path: %PROGRAMDATA%\1-bit-bridge\bridge.log. Machine-wide (not
// %LOCALAPPDATA%) because the service typically runs as LocalSystem,
// whose %LOCALAPPDATA% points at C:\Windows\System32\config\systemprofile\...
// — unfindable by the admin user. ProgramData is the canonical Windows
// spot for machine-wide app state.
//
// We can't read the config here (`bridge serve` hasn't parsed flags
// yet), so the log path is fixed: packaging's ServiceLog, the one path
// `bridge logs` and the console read the service's log by (backlog B618),
// with C:\ProgramData where some automation stripped %PROGRAMDATA%.
// Packaging's Install sets up %PROGRAMDATA%\1-bit-bridge\ with the right
// permissions, so this path exists by the time the service starts.
func redirectServiceIO() {
	dirs := packaging.HostWindowsLogDirs()
	logDir := dirs.ServiceLogDir()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		// If we can't create the log dir we're stuck with whatever
		// handles the SCM gave us. At least write the failure to
		// stderr before giving up — even if the SCM drops that
		// output, a test harness running under `bridge serve` won't.
		// Previously this was a silent `return`, and any follow-on
		// failure (bad config, missing TLS cert, port in use) would
		// disappear with no trace.
		fmt.Fprintf(os.Stderr, "service: cannot prepare log dir %q: %v\n", logDir, err)
		return
	}
	logPath := dirs.ServiceLog()
	f, err := packaging.OpenServiceLog(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "service: cannot open log file %q: %v\n", logPath, err)
		return
	}
	os.Stdout = f
	os.Stderr = f

	// Reassigning the Go os.Stdout/os.Stderr vars redirects fmt.Fprintln
	// and the slog handlers, but Go-runtime *fatal* output (an unrecovered
	// panic, a concurrent-map-write, an OOM throw) is written by
	// runtime.write1(fd=2), which resolves its target via
	// GetStdHandle(STD_ERROR_HANDLE) fresh on every write — verified in
	// the go1.26 runtime source (runtime/os_windows.go); the handle is NOT
	// cached at init. Pointing the process std-handle table at the log
	// file therefore ALSO lands those fatal crash traces in bridge.log
	// instead of losing them to the console-less SCM. Errors are
	// non-fatal — the Go-var redirect above still covers normal output.
	//
	// Version note: this relies on write1 resolving the handle per-write;
	// re-verify against runtime/os_windows.go if a future Go caches it.
	h := windows.Handle(f.Fd())
	_ = windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, h)
	_ = windows.SetStdHandle(windows.STD_ERROR_HANDLE, h)
}
