package doctor

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// listeningChildEnv makes this test binary, run again by one of the tests
// that use startListeningChild, the process they record as a live bridge
// (runListeningChild). Its value is the child's mode: "listen" (on
// 127.0.0.1), "listen6" (on [::1]) or "idle".
const listeningChildEnv = "DOCTOR_TEST_LISTENING_CHILD"

// listeningChildReady opens the line the child prints once it is ready,
// before the port it listens on (0 for none).
const listeningChildReady = "listening child ready, port "

// startListeningChild runs this test binary again as a process of this user
// that listens on a loopback port of its own, as a running bridge does on
// the ports of the config it started with: on 127.0.0.1 in mode "listen",
// on [::1] in mode "listen6", and on nothing in mode "idle". It returns the
// child's pid and that port (0 when it listens on none). The test that
// calls it must hand the child to runListeningChild first thing (the child
// runs that test alone).
//
// The child holds until its stdin reaches EOF: the test's cleanup kills it
// first, and if this binary dies before that, the pipe closes and the child
// exits by itself. The same shape as the Linux tests' startUndumpable,
// without making the child non-dumpable, so it runs on every platform.
func startListeningChild(t *testing.T, mode string) (pid, port int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), listeningChildEnv+"="+mode)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// The writer is dropped here and still stays open: cmd keeps it and
	// closes it only in Wait, once the child has exited, or on a failed
	// Start (startUndumpable says the same).
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), listeningChildReady); ok {
			port, err := strconv.Atoi(p)
			if err != nil {
				t.Fatalf("the child's ready line names no port: %q", sc.Text())
			}
			return cmd.Process.Pid, port
		}
	}
	t.Fatalf("the listening child exited before it was ready: %s", stderr.String())
	return 0, 0
}

// runListeningChild is the child's side of startListeningChild. It never
// returns.
//
// The listener is kept alive past the wait: net closes a listener nothing
// references from a finalizer, so a collection while the child waits would
// otherwise free the port the test records as the bridge's.
func runListeningChild(mode string) {
	addr := map[string]string{"listen": "127.0.0.1:0", "listen6": "[::1]:0"}[mode]
	var l net.Listener
	port := 0
	if addr != "" {
		var err error
		if l, err = net.Listen("tcp", addr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		port = l.Addr().(*net.TCPAddr).Port
	}
	fmt.Printf("%s%d\n", listeningChildReady, port)
	_, _ = io.Copy(io.Discard, os.Stdin)
	runtime.KeepAlive(l)
	os.Exit(0)
}
