package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// The test binary re-execs as the bridge itself, this tree's CLI through
// run: a copy of it is the backup the early rollback asks for its version
// (`bridge version`), or a candidate the updater asks for its settings check
// (`bridge serve --validate-config`), so both are answered by the code that
// ships. TestMain checks runAsBridgeEnv before the suite starts, and
// runAsBridgeVersionEnv names the version the copy says it is.
//
// Neither name starts with BRIDGE_: updater.ConfigDigest reads every BRIDGE_
// variable as part of the settings.
const (
	runAsBridgeEnv        = "CMDBRIDGE_TEST_RUN_AS_BRIDGE"
	runAsBridgeVersionEnv = "CMDBRIDGE_TEST_RUN_AS_BRIDGE_VERSION"
	// runAsBridgeCheckEnv makes the copy answer a settings check (`serve
	// --validate-config`) as a backup of another kind would (backlog B660):
	// "refused" refuses the settings, a reason on its line, exit 78;
	// "predates" answers as a release from before the check, the flag
	// package's own words and exit 2; "fails" exits 1, a check that did not
	// answer. Empty is the code that ships.
	runAsBridgeCheckEnv = "CMDBRIDGE_TEST_RUN_AS_BRIDGE_CHECK"
)

// runAsBridge is the re-exec'd test binary's whole run.
func runAsBridge() int {
	if v := os.Getenv(runAsBridgeVersionEnv); v != "" {
		version.ServerVersion = v
	}
	if mode := os.Getenv(runAsBridgeCheckEnv); mode != "" && slices.Contains(os.Args[1:], "--"+updater.ConfigCheckFlag) {
		switch mode {
		case "refused":
			fmt.Fprintln(os.Stderr, updater.ConfigRefusedPrefix+"line 3: a setting this version does not take")
			return updater.ConfigCheckRefusedExit
		case "predates":
			fmt.Fprintln(os.Stderr, "flag provided but not defined: -"+updater.ConfigCheckFlag)
			return 2
		case "fails":
			fmt.Fprintln(os.Stderr, "the settings could not be read")
			return updater.ConfigCheckFailedExit
		}
	}
	// A stop by SIGINT or SIGTERM, as main gives the bridge one, so that a
	// serve the test runs as a process drains as the bridge's does.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Args[1:], os.Stdout, os.Stderr)
}

// installBridgeCopy puts a copy of the running test binary at path, which
// runs as this tree's bridge of version v for the rest of the test, and
// returns path. The environment that makes it the bridge is set here with
// the copy, so that no copy is ever run as the test binary, which would run
// the whole suite. A hard link where the host makes one; on Windows a copy,
// since a link to the mapped test binary cannot be removed by TempDir's
// cleanup.
func installBridgeCopy(t *testing.T, path, v string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" || os.Link(exe, path) != nil {
		copyExecutable(t, exe, path)
	}
	t.Setenv(runAsBridgeVersionEnv, v)
	t.Setenv(runAsBridgeEnv, "1")
	return path
}

func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
