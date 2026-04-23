package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// safeBuffer is a bytes.Buffer guarded by a mutex, safe for the concurrent
// writer-by-serve / reader-by-test access pattern our live-server tests use.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// writeValidConfig drops a minimal-but-valid bridge.yaml next to a real
// library root. Returns the config path.
func writeValidConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte("libraryRoots:\n  - "+lib+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func runCapture(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var so, se bytes.Buffer
	code = run(context.Background(), args, &so, &se)
	return so.String(), se.String(), code
}

func TestVersion(t *testing.T) {
	stdout, stderr, code := runCapture(t, "version")
	if code != 0 {
		t.Fatalf("version exit code = %d, want 0; stderr=%q", code, stderr)
	}
	want := fmt.Sprintf("1-bit-bridge %s (protocol v%d)", version.ServerVersion, version.ProtocolVersion)
	if !strings.Contains(stdout, want) {
		t.Errorf("version stdout = %q, want to contain %q", stdout, want)
	}
}

func TestHelpVariants(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		stdout, _, code := runCapture(t, arg)
		if code != 0 {
			t.Errorf("%q exit code = %d, want 0", arg, code)
		}
		if !strings.Contains(stdout, "Usage:") || !strings.Contains(stdout, "Subcommands:") {
			t.Errorf("%q stdout missing usage block: %q", arg, stdout)
		}
	}
}

func TestNoArgsShowsUsageAndReturns2(t *testing.T) {
	_, stderr, code := runCapture(t)
	if code != 2 {
		t.Errorf("no-args exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("no-args stderr missing usage block: %q", stderr)
	}
}

func TestUnknownSubcommandReturns2(t *testing.T) {
	_, stderr, code := runCapture(t, "frobnicate")
	if code != 2 {
		t.Errorf("unknown exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "unknown subcommand: frobnicate") {
		t.Errorf("unknown stderr missing error: %q", stderr)
	}
}

// TestServeStartsAndServesHealth is the real end-to-end: spin up `serve` on
// an ephemeral port in a goroutine, poll /v1/health over TLS with a
// fingerprint-capturing client, then cancel the context and verify clean
// shutdown with exit code 0.
func TestServeStartsAndServesHealth(t *testing.T) {
	cfgPath := writeValidConfig(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Use concurrency-safe buffers — the test goroutine reads them while
	// serveCmd runs in its own goroutine writing to the same streams.
	stdout := &safeBuffer{}
	stderr := &safeBuffer{}
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--config", cfgPath, "--addr", "127.0.0.1:0"}, stdout, stderr)
	}()

	addr, fingerprint := waitForListening(t, stdout, 5*time.Second)

	// Hit /v1/health over TLS, pinning the server fingerprint.
	var peerFP string
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
				VerifyConnection: func(s tls.ConnectionState) error {
					if len(s.PeerCertificates) > 0 {
						peerFP = servertls.FingerprintFromDER(s.PeerCertificates[0].Raw)
					}
					return nil
				},
			},
		},
	}
	resp, err := client.Get("https://" + addr + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v; stderr=%s", err, stderr.String())
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	var health api.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if health.ProtocolVersion != version.ProtocolVersion {
		t.Errorf("protocolVersion = %d", health.ProtocolVersion)
	}
	if peerFP != fingerprint {
		t.Errorf("peer fingerprint = %q, want %q", peerFP, fingerprint)
	}

	// Cancel → serve should exit cleanly with code 0.
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("serve exit code = %d, want 0; stderr=%s", code, stderr.String())
		}
	case <-time.After(shutdownGrace + 2*time.Second):
		t.Fatalf("serve did not shut down within grace window; stderr=%s", stderr.String())
	}
}

// waitForListening polls the serve goroutine's stdout for the startup banner
// and extracts both the bound host:port and the TLS fingerprint the server
// prints. Returns within deadline or fails the test.
func waitForListening(t *testing.T, out *safeBuffer, deadline time.Duration) (addr, fingerprint string) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		s := out.String()
		// Example banner:
		//   1-bit-bridge v0.0.1 (protocol v1) — listening on https://127.0.0.1:52431
		//   TLS fingerprint (pin this on the iOS side):
		//     AB:CD:...
		if idx := strings.Index(s, "https://"); idx >= 0 {
			rest := s[idx+len("https://"):]
			if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
				addr = strings.TrimSpace(rest[:nl])
			}
		}
		if idx := strings.Index(s, "TLS fingerprint"); idx >= 0 && addr != "" {
			// Next non-blank line is the fingerprint, 2-space indented.
			for _, line := range strings.Split(s[idx:], "\n") {
				line = strings.TrimSpace(line)
				if strings.Count(line, ":") >= 10 {
					fingerprint = line
					return
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for startup banner; stdout so far:\n%s", out.String())
	return
}

func TestServeMissingConfigReturns2(t *testing.T) {
	_, stderr, code := runCapture(t, "serve", "--config", "/nonexistent/bridge.yaml")
	if code != 2 {
		t.Errorf("serve missing-config exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "config load failed") {
		t.Errorf("serve missing-config stderr: %q", stderr)
	}
}

func TestPairMintsTokenAndReturns0(t *testing.T) {
	cfgPath := writeValidConfig(t)
	stdout, stderr, code := runCapture(t, "pair", "--config", cfgPath, "--name", "iPhone 15 Pro")
	if code != 0 {
		t.Fatalf("pair exit code = %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "Paired successfully.") {
		t.Errorf("pair stdout missing success marker: %q", stdout)
	}
	if !strings.Contains(stdout, "iPhone 15 Pro") {
		t.Errorf("pair stdout missing device name: %q", stdout)
	}
	if !strings.Contains(stdout, "Bearer token") {
		t.Errorf("pair stdout missing token label: %q", stdout)
	}
}

func TestPairRejectsMissingName(t *testing.T) {
	cfgPath := writeValidConfig(t)
	_, stderr, code := runCapture(t, "pair", "--config", cfgPath)
	if code != 2 {
		t.Errorf("pair --name missing exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "--name is required") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestPairMissingConfigReturns2(t *testing.T) {
	_, stderr, code := runCapture(t, "pair", "--name", "foo", "--config", "/nonexistent/bridge.yaml")
	if code != 2 {
		t.Errorf("pair bad-config exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "config load failed") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestScanStubLoadsConfigAndReturns1(t *testing.T) {
	cfgPath := writeValidConfig(t)
	_, stderr, code := runCapture(t, "scan", "--config", cfgPath)
	if code != 1 {
		t.Errorf("scan stub exit code = %d, want 1; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "not yet implemented") {
		t.Errorf("scan stub stderr missing marker: %q", stderr)
	}
}

func TestScanMissingConfigReturns2(t *testing.T) {
	_, stderr, code := runCapture(t, "scan", "--config", "/nonexistent/bridge.yaml")
	if code != 2 {
		t.Errorf("scan missing-config exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "config load failed") {
		t.Errorf("scan missing-config stderr: %q", stderr)
	}
}

func TestProtocolVersionIsOne(t *testing.T) {
	// PROTOCOL.md is the source of truth; the constant must match v1 until a
	// breaking wire change bumps both. If you bump ProtocolVersion, update
	// PROTOCOL.md AND the iOS-repo mirror in the same PR cycle (see
	// CONTRIBUTING.md — Mirror-PR rule).
	if version.ProtocolVersion != 1 {
		t.Errorf("ProtocolVersion = %d, want 1; did you forget the Mirror-PR?", version.ProtocolVersion)
	}
}
