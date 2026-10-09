package main

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// consoleBridge is a `bridge serve` a test stood up with its console on a
// loopback port of its own, and what the test reaches it with. Serve's own
// context stays inside launchServe, whose drain cancels it; a request the
// test makes takes the test's, t.Context().
type consoleBridge struct {
	servedBridge
	// adminBase is the console, over plain HTTP; apiBase is the v1 API,
	// over TLS, as a paired device reaches it.
	adminBase, apiBase string
	console            *http.Client
	// phone skips certificate verification: the bridge presents the
	// self-signed pair it minted at boot, which this test has no pin for.
	phone *http.Client
	// lib is the library root and dataDir the data directory the config
	// names, so a test can put files where serve reads them.
	lib, dataDir string
	// autoOptimizeSweeps counts the pre-generation sweeps that have
	// finished and been recorded on the Jobs card, through
	// serveOpts.autoOptimizeSwept: a count a test can wait on where a
	// finish time cannot be compared (waitForAutoOptimizeSweep).
	autoOptimizeSweeps atomic.Int64
}

// startConsoleBridge stands up `bridge serve` on a config it writes for a
// library in a directory of the test's own, with yamlTail appended to that
// config as written, and returns once the v1 API and the console both
// answer. fill, when not nil, puts files in the library first, so the
// startup scan finds them.
//
// It launches through launchServe, with runServe and the options `serve
// --config --addr` builds plus the sweep counter, which no flag carries,
// and then each of with, for a seam no flag carries either
// (serveOpts.soxProbe). launchServe registers the drain after this
// function's t.TempDir, so the drain runs first, and a cleanup the caller
// registered before this call runs after serve has returned.
func startConsoleBridge(t *testing.T, yamlTail string, fill func(lib string), with ...func(*serveOpts)) *consoleBridge {
	t.Helper()
	root := t.TempDir()
	lib := filepath.Join(root, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if fill != nil {
		fill(lib)
	}
	// Both listeners stay open and are handed to serve. A callback that
	// replaces addrOverride replaces lanListener and lanPacket too: the
	// address serve adopts has to be the one the override names, and the
	// listener this holds otherwise stays open until the cleanup.
	lan := holdLoopback(t)
	admin := holdLoopback(t)
	listenAddr := lan.addr
	consoleAddr := admin.addr
	configPath := filepath.Join(root, "bridge.yaml")
	dataDir := filepath.Join(root, "data")
	yamlText := "libraryRoots:\n  - " + lib + "\ndataDir: " + dataDir +
		"\nadminAddress: " + consoleAddr + "\n" + yamlTail
	if err := os.WriteFile(configPath, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}

	b := &consoleBridge{
		lib:       lib,
		dataDir:   dataDir,
		adminBase: "http://" + consoleAddr,
		console:   &http.Client{Timeout: 30 * time.Second},
		phone: &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
	}
	opts := serveOpts{
		configPath:        configPath,
		addrOverride:      listenAddr,
		lanListener:       lan.ln,
		adminListener:     admin.ln,
		autoOptimizeSwept: func() { b.autoOptimizeSweeps.Add(1) },
	}
	for _, set := range with {
		set(&opts)
	}
	b.servedBridge = launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) int {
		return runServe(ctx, opts, stdout, stderr)
	})
	waitForAdminReady(t, consoleAddr, b.done, b.stderr)
	b.apiBase = "https://" + b.addr
	return b
}
