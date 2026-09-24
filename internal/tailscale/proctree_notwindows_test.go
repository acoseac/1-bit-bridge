//go:build !windows

package tailscale

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// wrappedCLI is a fake `tailscale` shaped like the standalone macOS
// app's CLI helper: a script that runs the real binary WITHOUT exec. The
// "real binary" records its pid, waits to be released, and then writes
// a marker, so a test can tell a process the cancel stopped from one it
// merely orphaned.
type wrappedCLI struct {
	wrapper string // what resolveBinary finds, and what MintCert is handed
	pidFile string // the inner process's pid, written before it blocks
	release string // created by the test to let a surviving process go on
	wrote   string // created by the inner process once released
}

func newWrappedCLI(t *testing.T) wrappedCLI {
	t.Helper()
	dir := t.TempDir()
	c := wrappedCLI{
		wrapper: filepath.Join(dir, "tailscale"),
		pidFile: filepath.Join(dir, "pid"),
		release: filepath.Join(dir, "release"),
		wrote:   filepath.Join(dir, "wrote"),
	}
	inner := filepath.Join(dir, "tailscale-app")
	// The pid goes through a rename so a reader never sees it half
	// written. `sleep` is a child of this shell, so it shares the group.
	innerScript := "#!/bin/sh\n" +
		"echo $$ > '" + c.pidFile + ".tmp' && mv '" + c.pidFile + ".tmp' '" + c.pidFile + "'\n" +
		"while [ ! -e '" + c.release + "' ]; do sleep 0.02; done\n" +
		": > '" + c.wrote + "'\n"
	if err := os.WriteFile(inner, []byte(innerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	// No exec: the shell stays the direct child and the inner script is
	// its child, which is the whole point.
	if err := os.WriteFile(c.wrapper, []byte("#!/bin/sh\n'"+inner+"' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Whatever happens, never leave the inner process waiting.
	t.Cleanup(c.let)
	return c
}

// let releases the inner process. Idempotent.
func (c wrappedCLI) let() {
	if f, err := os.OpenFile(c.release, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
	}
}

// innerPID waits for the inner process to start and returns its pid.
func (c wrappedCLI) innerPID(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(c.pidFile); err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatalf("pid file %q: %v", b, err)
			}
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the fake CLI's inner process never started")
	return 0
}

// TestCancelStopsTheWholeCLIProcessTree pins stopTreeOnCancel through
// both CLI calls. Cancelling the context must stop the process that does
// the work, not only the wrapper the call exec'd. Two ways to get this
// wrong, and a check for each:
//
//   - The call does not return. CommandContext's default kill reaches
//     the wrapper shell alone, and Wait stays blocked on the pipes the
//     orphaned inner process still holds, for as long as that process
//     runs. Here it would run until released, which the test does only
//     after giving up.
//   - The call returns and the inner process lives on. That is what
//     unblocking Wait by other means (Cmd.WaitDelay closing the pipes)
//     would give: a prompt return with the writer still running. The
//     inner pid must be gone, which a killed process is within
//     milliseconds and a blocked one never is.
func TestCancelStopsTheWholeCLIProcessTree(t *testing.T) {
	cases := []struct {
		name string
		call func(ctx context.Context, c wrappedCLI) error
	}{
		{"Detect", func(ctx context.Context, _ wrappedCLI) error {
			_, err := Detect(ctx)
			return err
		}},
		{"MintCert", func(ctx context.Context, c wrappedCLI) error {
			dir := filepath.Dir(c.wrapper)
			return MintCert(ctx, c.wrapper, "bridge.example.ts.net",
				filepath.Join(dir, "tailscale.crt"), filepath.Join(dir, "tailscale.key"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newWrappedCLI(t)
			// Detect resolves `tailscale` through PATH; MintCert is handed
			// the same wrapper directly.
			t.Setenv("PATH", filepath.Dir(c.wrapper)+string(os.PathListSeparator)+os.Getenv("PATH"))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			returned := make(chan error, 1)
			go func() { returned <- tc.call(ctx, c) }()

			pid := c.innerPID(t)
			cancel()
			select {
			case <-returned:
			case <-time.After(5 * time.Second):
				c.let() // so the orphan exits, its pipes close and the call returns
				<-returned
				t.Fatalf("%s did not return within 5s of its context being cancelled. "+
					"The kill reached the wrapper shell, not the CLI it started, and "+
					"Wait stayed blocked on the pipes the orphan held.", tc.name)
			}

			deadline := time.Now().Add(5 * time.Second)
			for {
				err := syscall.Kill(pid, 0)
				if errors.Is(err, syscall.ESRCH) {
					break
				}
				if time.Now().After(deadline) {
					c.let()
					t.Fatalf("%s returned, but the CLI process it started (pid %d) is still "+
						"alive 5s after the cancel (kill(pid, 0) = %v). It will write "+
						"whenever it finishes, after its caller has moved on.", tc.name, pid, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			// And, belt and braces, the work it was blocked on never happens.
			c.let()
			time.Sleep(100 * time.Millisecond)
			if _, err := os.Stat(c.wrote); err == nil {
				t.Fatalf("the CLI process started by %s wrote after the cancel", tc.name)
			}
		})
	}
}
