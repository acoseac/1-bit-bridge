//go:build !windows

package packaging

import (
	"fmt"
	"runtime"
)

// SpawnDetached is Windows-only. On macOS/Linux, init hands off to
// launchd / systemctl which start the daemon as part of the unit
// install — the shell doesn't need a detached child. This stub exists
// only so init.go compiles without a build-tag dance around the call
// site.
func SpawnDetached(binary, configPath, logPath string) error {
	return fmt.Errorf("SpawnDetached: not supported on %s (launchd/systemctl handles start-on-init)", runtime.GOOS)
}
