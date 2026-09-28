package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// servedBridge is a `bridge serve` a test stood up on two loopback ports,
// and what the test reaches it with. Serve's own context stays inside
// startServedBridge, whose drain cancels it; a request the test makes
// takes the test's, t.Context().
type servedBridge struct {
	stderr *safeBuffer
	// adminBase is the console, over plain HTTP; apiBase is the v1 API,
	// over TLS, as a paired device reaches it.
	adminBase, apiBase string
	console            *http.Client
	// phone skips certificate verification: the bridge presents the
	// self-signed pair it minted at boot, which this test has no pin for.
	phone *http.Client
	// autoOptimizeSweeps counts the pre-generation sweeps that have
	// finished and been recorded on the Jobs card, through
	// serveOpts.autoOptimizeSwept: a count a test can wait on where a
	// finish time cannot be compared (waitForAutoOptimizeSweep).
	autoOptimizeSweeps atomic.Int64
}

// startServedBridge stands up `bridge serve` on a config it writes for a
// library in a directory of the test's own, with yamlTail appended to that
// config as written, and returns once the v1 API and the console both
// answer. fill, when not nil, puts files in the library first, so the
// startup scan finds them.
//
// It launches serve and registers the drain in the same function, which
// is what TestEveryBackgroundGoroutineDrainsOnCleanup asks of every
// function that launches one, and its t.TempDir comes before the drain,
// so the drain runs first. A cleanup the caller registered before this
// call runs after serve has returned.
func startServedBridge(t *testing.T, yamlTail string, fill func(lib string)) *servedBridge {
	t.Helper()
	root := t.TempDir()
	lib := filepath.Join(root, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if fill != nil {
		fill(lib)
	}
	listenAddr := fmt.Sprintf("127.0.0.1:%d", freeLoopbackPort(t))
	consoleAddr := fmt.Sprintf("127.0.0.1:%d", freeLoopbackPort(t))
	configPath := filepath.Join(root, "bridge.yaml")
	yamlText := "libraryRoots:\n  - " + lib + "\ndataDir: " + filepath.Join(root, "data") +
		"\nadminAddress: " + consoleAddr + "\n" + yamlTail
	if err := os.WriteFile(configPath, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}

	b := &servedBridge{
		stderr:    &safeBuffer{},
		adminBase: "http://" + consoleAddr,
		console:   &http.Client{Timeout: 30 * time.Second},
		phone: &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
	}
	serveCtx, stop := context.WithCancel(context.Background())
	stdout := &safeBuffer{}
	exitCode, returned := make(chan int, 1), make(chan struct{})
	// runServe with the options `serve --config --addr` builds, plus the
	// sweep counter, which no flag carries.
	opts := serveOpts{
		configPath:        configPath,
		addrOverride:      listenAddr,
		autoOptimizeSwept: func() { b.autoOptimizeSweeps.Add(1) },
	}
	go func() {
		defer close(returned)
		exitCode <- runServe(serveCtx, opts, stdout, b.stderr)
	}()
	drainServeOnCleanup(t, stop, returned, exitCode, b.stderr)
	apiAddr, _ := waitForListening(t, stdout, 30*time.Second)
	waitForAdminReady(t, consoleAddr, exitCode, b.stderr)
	b.apiBase = "https://" + apiAddr
	return b
}
