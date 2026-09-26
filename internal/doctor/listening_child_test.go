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
	"runtime/metrics"
	"strconv"
	"strings"
	"testing"
	"time"
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
// child's pid and that port (0 when it listens on none). The top-level test
// that calls it, directly or from a subtest, must hand the child to
// runListeningChild first thing: the child runs that test alone, named by
// its top-level name only, since a subtest's name is matched as a regexp
// and one it cannot parse would stop the child before it starts.
//
// The child holds until its stdin reaches EOF: the test's cleanup kills it
// first, and if this binary dies before that, the pipe closes and the child
// exits by itself. It runs a collection before it says it is ready
// (collectBeforeReady). The same shape as the Linux tests'
// startUndumpable, without making the child non-dumpable, so it runs on
// every platform.
func startListeningChild(t *testing.T, mode string) (pid, port int) {
	t.Helper()
	top, _, _ := strings.Cut(t.Name(), "/")
	cmd := exec.Command(os.Args[0], "-test.run=^"+top+"$")
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

// TestAListeningChildStartsFromASubtest: a child started from a subtest runs
// the top-level test, whose first act hands it over, whatever the subtest is
// called. The subtest's name is one `-test.run` cannot parse as a regexp,
// which is what a child named by the full test name would be handed.
func TestAListeningChildStartsFromASubtest(t *testing.T) {
	if mode := os.Getenv(listeningChildEnv); mode != "" {
		runListeningChild(mode)
	}
	t.Run("a name ( a regexp cannot parse", func(t *testing.T) {
		if pid, port := startListeningChild(t, "idle"); pid <= 0 || port != 0 {
			t.Errorf("got pid %d, port %d; want a child that holds no port", pid, port)
		}
	})
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
	collectBeforeReady()
	fmt.Printf("%s%d\n", listeningChildReady, port)
	_, _ = io.Copy(io.Discard, os.Stdin)
	runtime.KeepAlive(l)
	os.Exit(0)
}

// TestAListeningChildKeepsItsPortThroughACollection: the port the child
// reports is still held once it is ready, after the collection it runs
// first. Only the KeepAlive holds it: the listener's local is not read
// after the port, and the collection closes a listener nothing references
// (TestACollectionClosesAListenerNothingReferences).
func TestAListeningChildKeepsItsPortThroughACollection(t *testing.T) {
	if mode := os.Getenv(listeningChildEnv); mode != "" {
		runListeningChild(mode)
	}
	_, port := startListeningChild(t, "listen")
	requirePortHeld(t, port)
}

// collectBeforeReady is collectAndFinalize, run by a child of these tests
// after it listens and before it says it is ready, so the tests grade a
// listener that has already been through a collection. The runtime forces
// one two minutes after the last (runtime/proc.go's forcegcperiod), but
// only once one has run, and whether a child's startup runs one depends on
// how much it allocates: on go1.26.6 it does on macOS and does not on
// Linux. With the collection here, a child that drops its listener has
// lost the port before any test grades it, on every platform, rather than
// in a test held past two minutes on a platform whose startup collected.
// It ends the child on an error, which the parent reports as a child that
// exited before it was ready.
func collectBeforeReady() {
	if err := collectAndFinalize(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// collectAndFinalize runs a garbage collection and returns once the
// finalizers and cleanups it queued have run. net closes a listener nothing
// references from a finalizer (net/fd_posix.go sets (*netFD).Close as one),
// so a listener a process has dropped is closed by the time this returns,
// and one it keeps reachable is not.
//
// runtime.GC returns once the collection has queued them, not once they
// have run, so this then waits until the runtime has run as many of each as
// it has queued, by its own counts (runtime/metrics). Counts rather than a
// sentinel finalizer of this function's own: one goroutine runs finalizers,
// a batch at a time and each batch newest first (runtime/mfinal.go), so a
// sentinel can run ahead of the listener's, and cleanups, which
// fd_posix.go has a TODO to use instead, run on goroutines of their own.
func collectAndFinalize() error {
	runtime.GC()
	s := []metrics.Sample{
		{Name: "/gc/finalizers/queued:finalizers"},
		{Name: "/gc/finalizers/executed:finalizers"},
		{Name: "/gc/cleanups/queued:cleanups"},
		{Name: "/gc/cleanups/executed:cleanups"},
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		metrics.Read(s)
		for _, m := range s {
			if m.Value.Kind() != metrics.KindUint64 {
				return fmt.Errorf("runtime/metrics has no %s", m.Name)
			}
		}
		if s[1].Value.Uint64() >= s[0].Value.Uint64() && s[3].Value.Uint64() >= s[2].Value.Uint64() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("10 s after a collection, the runtime had run %d of the %d finalizers and %d of the %d cleanups it queued",
				s[1].Value.Uint64(), s[0].Value.Uint64(), s[3].Value.Uint64(), s[2].Value.Uint64())
		}
	}
}

// TestACollectionClosesAListenerNothingReferences is the premise the tests
// that ask for a held port after collectAndFinalize rest on: it closes a
// listener nothing references, so the port binds again. Were that not so,
// a child's port surviving the collection would say nothing about the
// KeepAlive that keeps it. It fails the day net stops closing such a
// listener when it is collected.
func TestACollectionClosesAListenerNothingReferences(t *testing.T) {
	port := listenAndDrop(t)
	if err := collectAndFinalize(); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("port %d is still held after a collection by a listener nothing references (%v): "+
			"collectAndFinalize did not wait for net to close it", port, err)
	}
	_ = l.Close()
}

// listenAndDrop listens on a loopback port and returns the port, keeping
// nothing that references the listener. It is a function of its own, never
// inlined, so no variable of its caller's can hold the listener.
//
//go:noinline
func listenAndDrop(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l.Addr().(*net.TCPAddr).Port
}

// requirePortHeld fails the test unless something still listens on
// 127.0.0.1:port: a bind of it must be refused as in use.
func requirePortHeld(t *testing.T, port int) {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err == nil {
		_ = l.Close()
		t.Fatalf("port %d binds again: the child's listener was closed by the collection it ran before it said it was ready", port)
	}
	if !isAddrInUse(err) {
		t.Fatalf("binding port %d: %v; want it refused as in use", port, err)
	}
}
