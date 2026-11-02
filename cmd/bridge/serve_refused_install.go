package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// servingBinaryPath is the binary serve runs as: os.Executable, its links
// resolved, or the boot test's stand-in (serveOpts.binaryPath). The updater
// swaps the file at this path and keeps its backup beside it, so this value
// feeds maybeRollbackOnBoot, updateInfoAdapter.binaryPath,
// AutoInstallOpts.BinaryPath and the early rollback of an install whose
// version refuses the settings.
//
// os.Executable may fail in unusual environments (a deleted binary
// running); argv[0] is the fallback, so the failure surfaces later in
// Install's preflight rather than blocking the whole server boot.
//
// Links are resolved so a symlinked install swaps the REAL binary, not the
// link, matching what update.go, init.go and menu.go do at their own
// os.Executable sites (update.go's comment: "Mirrors the binary resolution
// in init.go's service install"). On darwin os.Executable returns the
// INVOCATION path unresolved, so an admin-console install via
// /usr/local/bin/bridge -> /opt/.../bridge replaced the symlink with a
// regular file: the real binary stayed at the old version, launchd kept
// running it, and `bridge version` on PATH reported the new one.
// Best-effort: a resolve failure keeps the unresolved path rather than
// blocking boot (F0c).
func servingBinaryPath(opts serveOpts, stderr io.Writer) string {
	if opts.binaryPath != "" {
		return opts.binaryPath
	}
	binaryPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "updater: os.Executable failed (install path may not work): %v\n", err)
		binaryPath = os.Args[0]
	}
	if resolved, err := filepath.EvalSymlinks(binaryPath); err == nil {
		binaryPath = resolved
	}
	return binaryPath
}
