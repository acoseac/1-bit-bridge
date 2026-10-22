//go:build !linux

package hotplug

import "context"

// openKernelSource is the event source off Linux: none. The player is
// Linux only (config.PlayerEnabled), so serve never starts the watcher
// here; a test that does reads the mode.
func openKernelSource(context.Context) Source { return newPollingSource(ModeUnsupported, "") }
