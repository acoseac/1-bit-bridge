//go:build !windows

package main

import (
	"context"
	"io"
)

// isWindowsService is the non-Windows stub. Always false — non-Windows
// processes are never launched by the SCM.
func isWindowsService() bool { return false }

// runAsWindowsService is the non-Windows stub. Called only from code
// paths gated by isWindowsService(), so in practice it never runs on
// non-Windows, but the signature has to match so main.go stays
// platform-agnostic.
func runAsWindowsService(_ context.Context, _ string, _ func(context.Context) error, _ io.Writer) error {
	return nil
}
