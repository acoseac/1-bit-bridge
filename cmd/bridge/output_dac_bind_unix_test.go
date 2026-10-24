//go:build unix

package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

// dacUUIDPipe makes the data dir the config at cfgPath names (dacConfig
// names it and does not make it) with a named pipe at its player-uuid, and
// returns the pipe's path.
func dacUUIDPipe(t *testing.T, cfgPath string) string {
	t.Helper()
	dataDir := filepath.Join(filepath.Dir(cfgPath), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(dataDir, config.PlayerUUIDFile)
	fsutiltest.MakeFIFO(t, pipe)
	return pipe
}

// runDACWithin is runDAC on a goroutine of its own, bounded by
// fsutiltest.ServeBound with the writer played on pipe past it
// (fsutiltest.AwaitPastFIFOs), so a command that waits on the pipe fails
// the test rather than hang it.
func runDACWithin(t *testing.T, pipe string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var so, se bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		code = runOutputDACBinding(context.Background(), args, dacEnv(), &so, &se)
	}()
	fsutiltest.AwaitPastFIFOs(t, "bridge output dac "+args[0], fsutiltest.ServeBound, done, pipe)
	return so.String(), se.String(), code
}

// TestOutputDACBindRefusesANamedPipeAtThePlayersUUIDAtOnce: bind on an
// enabled player reads the player's UUID once it has written the binding,
// to make it the first time. A named pipe at <dataDir>/player-uuid is no
// identity: bind refuses it at once and names it, where the read waited for
// a writer and held bind, and makes no UUID in its place.
func TestOutputDACBindRefusesANamedPipeAtThePlayersUUIDAtOnce(t *testing.T) {
	root := dacTree(t)
	cfgPath := dacConfig(t, "  player:\n    enabled: true\n")
	pipe := dacUUIDPipe(t, cfgPath)
	out, errOut, code := runDACWithin(t, pipe, append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...)
	if code != 1 || !strings.Contains(errOut, "the player's UUID: ") || !strings.Contains(errOut, "named pipe is not a file") {
		t.Errorf("bind with a named pipe at player-uuid: exit %d, stderr %q; want it refused as a named pipe", code, errOut)
	}
	if strings.Contains(out, "player uuid: ") {
		t.Errorf("bind gave the player a UUID past a named pipe at player-uuid: %q", out)
	}
	if info, err := os.Lstat(pipe); err != nil || info.Mode()&fs.ModeNamedPipe == 0 {
		t.Errorf("player-uuid is %v (%v) after bind, want the named pipe left as it was", info, err)
	}
}

// TestOutputDACInspectSaysANamedPipeAtThePlayersUUIDIsNotAFile: inspect
// shows the player's UUID, or why there is none. A named pipe at
// <dataDir>/player-uuid is read at once for what it is, never as a UUID not
// made yet, where the read waited for a writer and held inspect.
func TestOutputDACInspectSaysANamedPipeAtThePlayersUUIDIsNotAFile(t *testing.T) {
	root := dacTree(t)
	cfgPath := dacConfig(t, "  player:\n    enabled: true\n")
	pipe := dacUUIDPipe(t, cfgPath)
	out, errOut, code := runDACWithin(t, pipe, append([]string{"inspect", "--config", cfgPath}, treeFlags(root)...)...)
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "player uuid: ") {
			line = l
		}
	}
	if code != 0 || !strings.HasSuffix(line, ": named pipe is not a file") {
		t.Errorf("inspect with a named pipe at player-uuid: exit %d, its UUID line %q, stderr %q; want the pipe named as no file", code, line, errOut)
	}
}
