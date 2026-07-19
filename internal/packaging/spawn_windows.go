//go:build windows

package packaging

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// SpawnDetached launches `<binary> serve --config <configPath>` as a
// detached, minimized process whose stdout/stderr stream to logPath.
// Matches the behaviour of the `.cmd` launcher installed in the user's
// Startup folder by `installWindowsStartup` — the same `start /min` cmd.exe
// wrapper, so the init-time spawn and the logon-time spawn produce a single
// process-shape the operator can recognise (minimised console window titled
// "1-bit-bridge").
//
// Escaping uses cmdArgEscape, NOT the batch-file CmdEscape: this command is
// passed to `cmd /c`, where cmd.exe does not collapse `%%`→`%`, so the
// batch percent-doubling would corrupt a `%`-containing path here. The two
// forms stay byte-identical for `%`-free paths (the overwhelmingly common
// case) and agree at runtime otherwise — see cmdArgEscape.
//
// cmd.exe's `/c start` returns as soon as the child is handed off to
// the shell; the real server keeps running independently of the init
// process that called us. We Wait() only on the launcher `cmd.exe`,
// not the server it spawned.
func SpawnDetached(binary, configPath, logPath string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("ensure log dir: %w", err)
	}
	script := fmt.Sprintf(
		`start "1-bit-bridge" /min "%s" serve --config "%s" 1>>"%s" 2>&1`,
		cmdArgEscape(binary),
		cmdArgEscape(configPath),
		cmdArgEscape(logPath),
	)
	c := exec.Command("cmd.exe", "/c", script)
	if err := c.Start(); err != nil {
		return fmt.Errorf("spawn cmd.exe: %w", err)
	}
	// `cmd.exe /c start` detaches the real child and exits quickly.
	// Waiting here is cheap and lets us surface a setup-level error
	// (wrong path to cmd.exe, ACL denying exec) without hanging init.
	_ = c.Wait()
	return nil
}
