// Package handshaketest is what a test needs to watch a TLS listener's error
// log: the standard logger captured into a buffer, and a client whose
// handshake failure the server is certain to log. Like net/http/httptest it
// is imported only by tests, so none of it reaches the binary.
//
// It exists so there is ONE definition of capturing that log. Three test
// packages need it (internal/handshakelog, the console's Serve, the real
// `serve`), and capturing it correctly depends on a detail that is easy to
// lose in a copy: restoring an earlier slog default does not restore the
// standard logger.
package handshaketest

import (
	"crypto/tls"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// Buffer is a log sink that is safe to read while servers write to it.
type Buffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// CaptureStdLog points the standard library's default logger at a Buffer
// for the rest of the test. That is where an http.Server with a nil
// ErrorLog writes, and where handshakelog forwards every line it keeps. In
// production the same logger reaches slog through the bridge that
// slog.SetDefault installs, which logging.Init never runs under `go test`.
//
// Flags are zeroed so a line reads exactly as it was formatted. Output,
// flags and prefix are all restored afterwards: slog.SetDefault changes the
// output and the flags, and restoring an earlier slog default does NOT put
// them back, so a test cannot assume the settings it started with.
//
// Call it BEFORE the fixture that starts a server. The restore is a
// cleanup, cleanups run last in first out, and this order puts it after
// the server's shutdown, so a line written during shutdown still lands here.
func CaptureStdLog(t *testing.T) *Buffer {
	t.Helper()
	buf := &Buffer{}
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

// RejectTheCert runs a real TLS client that trusts only the system roots,
// so it refuses a self-signed cert, and returns the address the server saw
// it connect from. Go's client answers an unknown authority with a
// bad_certificate alert, so the server's line is deterministic:
//
//	http: TLS handshake error from <addr>: remote error: tls: bad certificate
//
// Not curl: curl 8.22 on OpenSSL 3.5 (measured) FINISHES a TLS 1.3
// handshake and checks the certificate afterwards, so the server logs no
// handshake failure at all — only, some of the time, an HTTP/2 preface
// error.
func RejectTheCert(t *testing.T, addr string) string {
	t.Helper()
	raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := tls.Client(raw, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}).Handshake(); err == nil {
		t.Fatal("a client with only the system roots trusted a self-signed cert; " +
			"a rejected handshake is what this helper exists to produce")
	}
	return raw.LocalAddr().String()
}

// WaitForLine returns once logs holds want, and fails the test if it does
// not within the deadline.
func WaitForLine(t *testing.T, logs *Buffer, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !strings.Contains(logs.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("log never carried %q within %v; the capture holds:\n%s", want, within, logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
