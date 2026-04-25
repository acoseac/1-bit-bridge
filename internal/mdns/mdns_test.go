package mdns

import (
	"runtime"
	"strings"
	"testing"
)

func TestBuildTXTRecordsIncludesProtocolAndLibrary(t *testing.T) {
	got := buildTXTRecords(Config{ProtocolVersion: 1, Port: 7788, LibraryName: "My Music"})
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "pv=1") {
		t.Errorf("missing pv: %v", got)
	}
	if !strings.Contains(joined, "library=My Music") {
		t.Errorf("missing library: %v", got)
	}
}

func TestBuildTXTRecordsOmitsEmptyLibrary(t *testing.T) {
	got := buildTXTRecords(Config{ProtocolVersion: 1, Port: 7788})
	for _, r := range got {
		if strings.HasPrefix(r, "library=") {
			t.Errorf("library should not be present when empty: %q", r)
		}
	}
}

// TestBuildTXTRecordsIncludesHostAndPort pins the host/port keys iOS
// reads to construct `https://<host>:<port>` directly from the TXT
// record. Without these, iOS would have to NWConnection-resolve the
// Bonjour service, which on iOS 26.4 doesn't reliably surface the
// resolved hostport via `currentPath?.remoteEndpoint`.
func TestBuildTXTRecordsIncludesHostAndPort(t *testing.T) {
	got := buildTXTRecords(Config{
		ProtocolVersion: 1,
		Port:            7788,
		Hostname:        "test-mac",
	})
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "host=test-mac.local") {
		t.Errorf("missing or wrong host TXT: %v", got)
	}
	if !strings.Contains(joined, "port=7788") {
		t.Errorf("missing or wrong port TXT: %v", got)
	}
}

// TestBuildTXTRecordsHostFromOSWhenBlank ensures the TXT host follows
// the same first-label + ".local" derivation Advertise uses for the
// SRV record, so iOS lands on a name the bridge actually serves. The
// derived host comes through `os.Hostname` here; the only check we
// can make portably is that the record has the `.local` suffix.
func TestBuildTXTRecordsHostFromOSWhenBlank(t *testing.T) {
	got := buildTXTRecords(Config{ProtocolVersion: 1, Port: 7788})
	for _, r := range got {
		if strings.HasPrefix(r, "host=") {
			if !strings.HasSuffix(r, ".local") {
				t.Errorf("host TXT should end with .local: %q", r)
			}
			return
		}
	}
	t.Error("no host= record in TXT")
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
	// Link-local addresses are intentionally allowed — mDNS/Bonjour
	// discovery on link-local is the primary use case.
	for _, ip := range ipsForAdvertise() {
		if ip.IsLoopback() {
			t.Errorf("loopback IP leaked into advertised set: %v", ip)
		}
	}
}
