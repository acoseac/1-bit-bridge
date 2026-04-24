//go:build windows

package main

import (
	"os"
	"path/filepath"
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
// yet), so the log path is a fixed constant. Packaging's Install sets
// up %PROGRAMDATA%\1-bit-bridge\ with the right permissions, so this
// path exists by the time the service starts.
func redirectServiceIO() {
	programData := os.Getenv("PROGRAMDATA")
	if programData == "" {
		// Fallback: a well-known absolute path. Shouldn't happen on a
		// real Windows box, but belt-and-braces keeps the service from
		// running blind if some automation stripped the env var.
		programData = `C:\ProgramData`
	}
	logDir := filepath.Join(programData, "1-bit-bridge")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return // best-effort: if we can't create the dir, leave stdio as-is
	}
	logPath := filepath.Join(logDir, "bridge.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	os.Stdout = f
	os.Stderr = f
}
