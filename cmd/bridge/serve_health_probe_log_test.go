package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestServeDoesNotLogItsOwnHealthProbe pins the one line in runServe that
// no package test can see: the API listener is wrapped by
// handshakelog.Wrap and its http.Server logs through the ErrorLog that
// Wrap returns. Without it, every Docker HEALTHCHECK — a TCP connect that
// closes before any ClientHello — became
//
//	http: TLS handshake error from 127.0.0.1:41418: EOF
//
// every 30 seconds (2,880 lines a day), which is the M-SEARCH class of
// log flood.
//
// Booting the real server, the test runs the probe the image runs —
// `bridge health`, not a hand-rolled dial — and requires that nothing is
// logged for it. The positive control is a client that rejects the
// bridge's self-signed cert, and it is load-bearing twice over: it shows
// the capture really does see this listener's handshake errors, so the
// silence cannot be an empty buffer, and it shows the filter still lets a
// genuine failure through. Its line is also the barrier: the probe runs
// first, so once the control's line is in, the probe's would have been.
func TestServeDoesNotLogItsOwnHealthProbe(t *testing.T) {
	// Registered before the drain, so the restore runs AFTER it (cleanups
	// are LIFO) and a line written during shutdown still lands here.
	logs := captureStdLog(t)

	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "data")
	apiPort, adminPort := freeLoopbackPort(t), freeLoopbackPort(t)
	cfgPath := filepath.Join(dir, "bridge.yaml")
	// listenAddress lives in the config rather than on --addr because
	// `bridge health` reads it from there, as the image's HEALTHCHECK does.
	body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\nlistenAddress: 127.0.0.1:%d\nadminAddress: 127.0.0.1:%d\n",
		lib, dataDir, apiPort, adminPort)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- run(ctx, []string{"serve", "--config", cfgPath}, stdout, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	addr, _ := waitForListening(t, stdout, 30*time.Second)

	// The banner prints before ServeTLS starts accepting. A handshake that
	// completes is the proof it has — and a verifying client that finishes
	// its handshake is silent with or without the filter, so this adds no
	// line of its own.
	completeVerifiedHandshake(t, addr, filepath.Join(dataDir, "server.crt"))

	var probeOut, probeErr bytes.Buffer
	if code := run(ctx, []string{"health", "--config", cfgPath}, &probeOut, &probeErr); code != 0 {
		t.Fatalf("bridge health = %d, want 0 (a probe that fails proves nothing about its log line); stderr=%s",
			code, probeErr.String())
	}

	rejecter := rejectTheBridgeCert(t, addr)
	want := "http: TLS handshake error from " + rejecter + ": remote error: tls: bad certificate"
	waitForLogLine(t, logs, want, 10*time.Second)
	// The probe connected first; a little slack for its goroutine anyway.
	time.Sleep(300 * time.Millisecond)

	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "TLS handshake error") && strings.HasSuffix(line, ": EOF") {
			t.Errorf("the health probe was logged as a handshake failure: %q", line)
		}
	}
}

// captureStdLog points the standard library's default logger at a buffer
// for the length of the test. That is where an http.Server with no
// ErrorLog writes, and where handshakelog forwards every line it keeps —
// in production it reaches slog through the bridge that slog.SetDefault
// installs, which logging.Init never runs under `go test`.
//
// Flags are forced to 0 so a line reads exactly as it was formatted, and
// all three settings are restored: slog.SetDefault changes the output
// and the flags, and restoring a previous slog default does NOT put them
// back, so a test cannot assume the defaults it started with.
func captureStdLog(t *testing.T) *safeBuffer {
	t.Helper()
	buf := &safeBuffer{}
	prevOut, prevFlags, prevPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(buf)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})
	return buf
}

// completeVerifiedHandshake finishes a TLS handshake against the bridge,
// trusting exactly the certificate on disk.
func completeVerifiedHandshake(t *testing.T, addr, certPath string) {
	t.Helper()
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read the bridge's cert: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemBytes) {
		t.Fatalf("no certificate in %s", certPath)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr,
		&tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("a handshake verified against the bridge's own cert failed: %v", err)
	}
	_ = conn.Close()
}

// rejectTheBridgeCert runs a real TLS client that does not trust the
// bridge's self-signed certificate — what a browser or `curl` without
// `-k` does — and returns the address the server saw it connect from.
// Go's client answers an unknown authority with a bad_certificate alert,
// so the server's line is deterministic.
func rejectTheBridgeCert(t *testing.T, addr string) string {
	t.Helper()
	raw, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	conn := tls.Client(raw, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err := conn.Handshake(); err == nil {
		t.Fatal("a client with only the system roots trusted the bridge's self-signed cert; " +
			"the positive control needs a rejected handshake")
	}
	return raw.LocalAddr().String()
}

func waitForLogLine(t *testing.T, logs *safeBuffer, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("log never carried %q within %v; the capture holds:\n%s", want, within, logs.String())
}
