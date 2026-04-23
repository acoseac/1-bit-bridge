package mdns

import (
	"runtime"
	"strings"
	"testing"
)

func TestBuildTXTRecordsIncludesProtocolAndLibrary(t *testing.T) {
	got := buildTXTRecords(Config{ProtocolVersion: 1, LibraryName: "My Music"})
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "pv=1") {
		t.Errorf("missing pv: %v", got)
	}
	if !strings.Contains(joined, "library=My Music") {
		t.Errorf("missing library: %v", got)
	}
}

func TestBuildTXTRecordsOmitsEmptyLibrary(t *testing.T) {
	got := buildTXTRecords(Config{ProtocolVersion: 1})
	for _, r := range got {
		if strings.HasPrefix(r, "library=") {
			t.Errorf("library should not be present when empty: %q", r)
		}
	}
}

func TestSanitizeInstanceStripsDotsAndControlChars(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Arsenie's Bridge", "Arsenie's Bridge"},
		{"With.Dots", "WithDots"},
		{"Control\x01Chars\x7F", "ControlChars"},
		{"  padded  ", "padded"},
	}
	for _, c := range cases {
		got := sanitizeInstance(c.in)
		if got != c.want {
			t.Errorf("sanitizeInstance(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAdvertiseRejectsZeroPort(t *testing.T) {
	_, err := Advertise(Config{Port: 0})
	if err == nil {
		t.Error("expected error for Port=0")
	}
}

// TestAdvertiseStartsAndStops spins up a real mDNS server on a high
// port and immediately shuts it down. On CI runners without multicast
// access this might error — in that case we skip rather than flake.
func TestAdvertiseStartsAndStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows CI / sandboxes often restrict multicast sockets;
		// skip the live path rather than fail on permissions.
		t.Skip("mdns live test skipped on windows")
	}
	a, err := Advertise(Config{
		InstanceName:    "test",
		Port:            62999,
		ProtocolVersion: 1,
		LibraryName:     "Test Library",
	})
	if err != nil {
		t.Skipf("mdns unavailable in this env: %v", err)
	}
	if a == nil {
		t.Fatal("advertiser is nil")
	}
	if err := a.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Double-close is a no-op.
	if err := a.Close(); err != nil {
		t.Errorf("double Close: %v", err)
	}
}

func TestAdvertiseClosingNilAdvertiserIsSafe(t *testing.T) {
	var a *Advertiser
	if err := a.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
}

func TestIpsForAdvertiseExcludesLoopback(t *testing.T) {
	// On any reasonable dev machine this returns at least one address;
	// on a locked-down CI machine it may be empty. Either is valid.
	for _, ip := range ipsForAdvertise() {
		if ip.IsLoopback() {
			t.Errorf("loopback IP leaked into advertised set: %v", ip)
		}
		if ip.IsLinkLocalUnicast() {
			t.Errorf("link-local IP leaked: %v", ip)
		}
	}
}
