//go:build !windows

package main

import (
	"log/slog"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// TestADSDRequestThatCannotCompareItsVolumesNamesNoDirectory: a DSD
// rendition's pre-flight compares the variants volume with the render
// scratch's before it adds their needs (SameVolume), and a comparison that
// cannot stat either directory is logged with both roles and the stat's
// errno, and never the directory: SameVolume answered with os.Stat's
// *fs.PathError, which names it. Not on Windows, where a directory below a
// file resolves to the file, which the volume probe opens.
func TestADSDRequestThatCannotCompareItsVolumesNamesNoDirectory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block func(b *stampBridge, blocked string)
	}{
		{"the variants directory", func(b *stampBridge, blocked string) {
			b.adapter.outputDir = func() string { return blocked }
		}},
		{"the render scratch", func(b *stampBridge, blocked string) {
			b.adapter.tempDir = func() string { return blocked }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := loggingtest.Record(t)
			b := newStampBridge(t)
			root := filepath.Join(t.TempDir(), "Private Volume")
			tc.block(b, blockedUnder(t, root))
			b.requestRefused(t, stampDSD, "optimize")
			requireLine(t, logs, "upscale: enqueue failed", slog.LevelError, "check free space for the rendition",
				transcode.ProbeRoleVariants, transcode.ProbeRoleScratch, "stat", syscall.ENOTDIR.Error())
			if leaked := logs.Carrying(root); len(leaked) != 0 {
				t.Errorf("a line names the directory:\n%s", strings.Join(leaked, "\n"))
			}
		})
	}
}
