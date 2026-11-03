package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// TestCertRotateNamesAPairingLinkTheAppOpens reads the preamble `bridge
// cert rotate` prints before its prompt. Its re-pair line names the
// pairing link by the scheme the console's QR carries, onebit://: iOS 27
// hands a bridge:// link to Apple's Watch app (com.apple.Bridge), so a
// device sent to one does not reach 1-bit (B627).
func TestCertRotateNamesAPairingLinkTheAppOpens(t *testing.T) {
	dir := t.TempDir()
	cfgPath := certFixtureConfig(t, dir)
	certPath, keyPath := servertls.DefaultPaths(filepath.Join(dir, "data"))
	certFixtureWindow(t, certPath, keyPath, time.Now().Add(-time.Hour), time.Now().Add(300*24*time.Hour))

	// Decline at the prompt: the preamble is what the operator reads
	// before deciding, and a rotation would replace the fixture.
	var stdout, stderr bytes.Buffer
	if code := certRotateCmd([]string{"--config", cfgPath}, strings.NewReader("no\n"), &stdout, &stderr); code != 1 {
		t.Fatalf("declined rotate: code=%d, want 1\nstdout=%s", code, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "re-pair") || !strings.Contains(out, "onebit://") {
		t.Errorf("the preamble does not send devices to an onebit:// link to re-pair:\n%s", out)
	}
	if strings.Contains(out, "bridge://") {
		t.Errorf("the preamble names a bridge:// link, which iOS 27 opens in Apple's Watch app:\n%s", out)
	}
}
