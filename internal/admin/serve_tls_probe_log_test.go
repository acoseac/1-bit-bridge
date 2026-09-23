package admin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/handshakelog/handshaketest"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// TestTLSConsoleDoesNotLogALocalProbe pins the console half of the
// handshakelog wiring. In public mode without a TLS-terminating proxy,
// Serve wraps its listener in tls.NewListener, and the CLI's own liveness
// probes of that port — probeAdminRunning on every launcher repaint,
// waitForListen after a restart — are TCP connects that close before any
// ClientHello: the same shape as the image's HEALTHCHECK against the API
// listener, and the same line per probe unless Serve wraps the RAW
// listener and logs through what Wrap returns.
//
// The positive control is a client that rejects the console's cert: its
// line proves the capture sees this listener's handshake errors, so the
// probe's silence cannot be an empty buffer, and that a genuine failure
// still reaches the log. The probe runs first, so once the control's
// line is in, the probe's would have been.
func TestTLSConsoleDoesNotLogALocalProbe(t *testing.T) {
	// Before newTestServer, so it is restored after every other cleanup.
	logs := handshaketest.CaptureStdLog(t)

	srv, cfg, _ := newTestServer(t)
	dir := t.TempDir()
	cert, _, err := servertls.LoadOrGenerate(filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	srv.deps.TLSConfig = &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	cfg.AdminAddress = addr
	lis.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx)
	}()
	// A cleanup, not a tail: registered after newTestServer's store
	// teardown, so it runs BEFORE it and a failing assertion cannot leave
	// Serve running against a closed store.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("Serve did not return within 10s of cancel")
		}
	})

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	waitForVerifiedHandshake(t, addr, roots)

	probe, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Close()

	rejecter := handshaketest.RejectTheCert(t, addr)
	handshaketest.WaitForLine(t, logs, "http: TLS handshake error from "+rejecter+": remote error: tls: bad certificate", 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "TLS handshake error") && strings.HasSuffix(line, ": EOF") {
			t.Errorf("a local liveness probe of the TLS console was logged as a handshake failure: %q", line)
		}
	}
}

// waitForVerifiedHandshake retries until a handshake that trusts exactly
// roots completes — the proof Serve is accepting. It is silent with or
// without the filter, so it adds no line of its own.
func waitForVerifiedHandshake(t *testing.T, addr string, roots *x509.CertPool) {
	t.Helper()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr,
			&tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12})
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the TLS console never completed a verified handshake: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
