package mdns

import (
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestAdvertiseRejectsOutOfRangePort(t *testing.T) {
	// `Port` is `int`, so values above 65535 are representable. The
	// TXT record publishes the value to clients verbatim, so accepting
	// invalid ports would have iOS construct unusable URLs.
	for _, p := range []int{-1, 65536, 70000, 1 << 20} {
		_, err := Advertise(Config{Port: p})
		if err == nil {
			t.Errorf("expected error for Port=%d", p)
		}
	}
}

func TestAdvertisedHostNeverBareLocal(t *testing.T) {
	// `os.Hostname` returning ("", nil) on a minimally-configured
	// container would have produced just ".local" before the
	// fallback-to-localhost guard. Hard to simulate without forking
	// the test, but we can check that the empty-Hostname path always
	// produces a non-bare result and that the FQDN trimming still
	// fires.
	got := Config{Hostname: ""}.advertisedHost()
	if got == ".local" {
		t.Errorf("advertisedHost() returned bare .local, would build invalid URLs")
	}
	if !strings.HasSuffix(got, ".local") {
		t.Errorf("advertisedHost() should always end in .local, got %q", got)
	}
	// FQDN reduces to first label.
	if got := (Config{Hostname: "mac.corp.example.com"}).advertisedHost(); got != "mac.local" {
		t.Errorf("advertisedHost(mac.corp.example.com) = %q, want mac.local", got)
	}
	// Trailing dot stripped.
	if got := (Config{Hostname: "host."}).advertisedHost(); got != "host.local" {
		t.Errorf("advertisedHost(host.) = %q, want host.local", got)
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

// TestIpSetEqualOrderInvariant — the rebind detector compares IP
// snapshots from net.Interfaces() across ticks. The OS may return
// addresses in different orders between calls (interface renames,
// hot-plug, kernel scheduler), so the equality must be order-
// invariant. False positives here would trigger spurious rebuilds
// every 60 s and tear down working hashicorp/mdns sockets for no
// reason.
func TestIpSetEqualOrderInvariant(t *testing.T) {
	a := []net.IP{net.ParseIP("192.168.1.10"), net.ParseIP("fe80::1"), net.ParseIP("10.0.0.5")}
	b := []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("192.168.1.10"), net.ParseIP("fe80::1")}
	if !ipSetEqual(a, b) {
		t.Errorf("equal sets in different order should compare equal")
	}
}

func TestIpSetEqualDetectsAdditionAndRemoval(t *testing.T) {
	a := []net.IP{net.ParseIP("192.168.1.10")}
	b := []net.IP{net.ParseIP("192.168.1.10"), net.ParseIP("10.0.0.5")}
	if ipSetEqual(a, b) {
		t.Errorf("set with extra element should NOT compare equal")
	}
	c := []net.IP{net.ParseIP("192.168.1.11")}
	if ipSetEqual(a, c) {
		t.Errorf("set with replaced element should NOT compare equal")
	}
}

// TestRebindFiresOnIPChange — drives the loop with an injected
// ipSource that flips between two IP sets. Verifies that the
// underlying mDNS server is rebuilt (new pointer) when the IP
// set changes. Uses advertiseInternal(spawnLoop: false) so the
// test drives maybeRebind() directly rather than racing a real
// background goroutine — the race detector caught the
// alternative (write `a.ipSource` after Advertise) and a lock-
// per-read fix would add cost to the production hot path.
func TestRebindFiresOnIPChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mdns live test skipped on windows")
	}
	// Two IP sets the injected source flips between. Use only the
	// loopback range so the underlying hashicorp/mdns server can
	// successfully bind even on locked-down CI runners.
	setA := []net.IP{net.ParseIP("127.0.0.1")}
	setB := []net.IP{net.ParseIP("127.0.0.2")}
	var phase atomic.Int32 // 0 → setA, 1 → setB

	a, err := advertiseInternal(Config{
		InstanceName:    "rebind-test",
		Port:            62998,
		ProtocolVersion: 1,
		LibraryName:     "Rebind Test",
	}, func() []net.IP {
		if phase.Load() == 0 {
			return setA
		}
		return setB
	}, time.Hour /* loop never fires; we drive manually */, false /* don't spawn the goroutine */)
	if err != nil {
		t.Skipf("mdns unavailable in this env: %v", err)
	}
	defer a.Close()

	a.rebindMu.Lock()
	firstSrv := a.server
	a.rebindMu.Unlock()
	if firstSrv == nil {
		t.Fatal("initial server is nil")
	}
	phase.Store(1)
	a.maybeRebind()

	a.rebindMu.Lock()
	defer a.rebindMu.Unlock()
	if a.server == nil {
		t.Fatal("server is nil after rebind")
	}
	if a.server == firstSrv {
		t.Errorf("expected server pointer to change after IP set flip; still %p", firstSrv)
	}
	if !ipSetEqual(a.cachedIPs, setB) {
		t.Errorf("cachedIPs not updated after rebind: got %v, want %v", a.cachedIPs, setB)
	}
}

// TestNoRebindOnUnchangedIPSet — back-to-back ticks with the same
// IP set must NOT rebuild. Otherwise the loop would tear down
// working sockets every 60 s for no reason.
func TestNoRebindOnUnchangedIPSet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mdns live test skipped on windows")
	}
	stable := []net.IP{net.ParseIP("127.0.0.1")}
	a, err := advertiseInternal(Config{
		InstanceName:    "stable-test",
		Port:            62997,
		ProtocolVersion: 1,
		LibraryName:     "Stable Test",
	}, func() []net.IP { return stable }, time.Hour, false)
	if err != nil {
		t.Skipf("mdns unavailable in this env: %v", err)
	}
	defer a.Close()

	a.rebindMu.Lock()
	firstSrv := a.server
	a.rebindMu.Unlock()

	a.maybeRebind()
	a.maybeRebind()

	a.rebindMu.Lock()
	defer a.rebindMu.Unlock()
	if a.server != firstSrv {
		t.Errorf("server pointer changed without IP set change: was %p, now %p", firstSrv, a.server)
	}
}

// TestRebindAfterCloseIsNoop — once Close() has fired, a stray
// goroutine reaching maybeRebind must NOT touch the (now-nil)
// server. Belt-and-braces: the goroutine is told to stop via
// `done`, but if the close-channel signal races a tick the
// safety net is the `closed` flag check inside maybeRebind.
func TestRebindAfterCloseIsNoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mdns live test skipped on windows")
	}
	stable := []net.IP{net.ParseIP("127.0.0.1")}
	flipped := []net.IP{net.ParseIP("127.0.0.2")}
	var phase atomic.Int32

	a, err := advertiseInternal(Config{
		InstanceName:    "close-test",
		Port:            62996,
		ProtocolVersion: 1,
		LibraryName:     "Close Test",
	}, func() []net.IP {
		if phase.Load() == 0 {
			return stable
		}
		return flipped
	}, time.Hour, false)
	if err != nil {
		t.Skipf("mdns unavailable in this env: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	phase.Store(1)
	// Should be a silent no-op — closed flag short-circuits.
	a.maybeRebind()
	a.rebindMu.Lock()
	defer a.rebindMu.Unlock()
	if a.server != nil {
		t.Errorf("server should be nil after Close; got %p", a.server)
	}
}

func TestFilterIPsToInterface_NilInterfacePassthrough(t *testing.T) {
	ips := []net.IP{net.ParseIP("192.168.0.208"), net.ParseIP("10.0.0.1")}
	out := filterIPsToInterface(ips, nil)
	if len(out) != 2 {
		t.Fatalf("nil iface should be passthrough; got %d ips", len(out))
	}
}

func TestFilterIPsToInterface_KeepsOnlyMatchingIPs(t *testing.T) {
	// Resolve loopback (always present, predictable IP: 127.0.0.1).
	// Use it as the "pinned" interface and pass a mixed-IP list.
	// Result should keep 127.0.0.1 only.
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("net.Interfaces unavailable: %v", err)
	}
	var loopback *net.Interface
	for i := range ifaces {
		if ifaces[i].Flags&net.FlagLoopback != 0 {
			loopback = &ifaces[i]
			break
		}
	}
	if loopback == nil {
		t.Skip("no loopback interface available in this environment")
	}
	ips := []net.IP{
		net.ParseIP("127.0.0.1"),
		net.ParseIP("192.168.0.208"),
		net.ParseIP("10.0.0.1"),
	}
	out := filterIPsToInterface(ips, loopback)
	// loopback should contain 127.0.0.1; the other two LAN IPs
	// should NOT be on the loopback interface.
	if len(out) == 0 {
		t.Fatalf("expected at least 127.0.0.1 to survive the filter, got empty")
	}
	for _, ip := range out {
		s := ip.String()
		if s == "192.168.0.208" || s == "10.0.0.1" {
			t.Errorf("non-loopback IP %s passed filter for loopback interface", s)
		}
	}
}
