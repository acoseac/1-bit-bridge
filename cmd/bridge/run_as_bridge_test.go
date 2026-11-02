package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

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
)

// runAsBridge is the re-exec'd test binary's whole run.
func runAsBridge() int {
	if v := os.Getenv(runAsBridgeVersionEnv); v != "" {
		version.ServerVersion = v
	}
	return run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)
}

// installBridgeCopy puts a copy of the running test binary at path, which
// runs as this tree's bridge of version v once the caller's environment
// says so (asBridge), and returns path. A hard link where the host makes
// one; on Windows a copy, since a link to the mapped test binary cannot be
// removed by TempDir's cleanup.
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
	return path
}

// asBridge makes a copy installBridgeCopy made run as the bridge for the
// rest of the test.
func asBridge(t *testing.T) {
	t.Helper()
	t.Setenv(runAsBridgeEnv, "1")
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
