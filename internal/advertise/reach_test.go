package advertise

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func withIfaces(t *testing.T, list []ifaceAddr) {
	t.Helper()
	prev := enumerateIfaces
	enumerateIfaces = func() ([]ifaceAddr, error) { return list, nil }
	t.Cleanup(func() { enumerateIfaces = prev })
}

func oneAddr(name string, flags net.Flags, ip string) ifaceAddr {
	parsed := net.ParseIP(ip)
	bits := 32
	if parsed.To4() == nil {
		bits = 128
	}
	return ifaceAddr{
		iface: net.Interface{Name: name, Flags: flags},
		addrs: []net.Addr{&net.IPNet{IP: parsed, Mask: net.CIDRMask(bits, bits)}},
	}
}

func hasURL(eps []Endpoint, url string) bool {
	for _, e := range eps {
		if e.URL == url {
			return true
		}
	}
	return false
}

func hasClass(eps []Endpoint, url string, class Class) bool {
	for _, e := range eps {
		if e.URL == url && e.Class == class {
			return true
		}
	}
	return false
}

func hasIP(ips []net.IP, want string) bool {
	w := net.ParseIP(want)
	for _, ip := range ips {
		if ip.Equal(w) {
			return true
		}
	}
	return false
}

func httpsIP(ip string) string {
	parsed := net.ParseIP(ip)
	host := parsed.String()
	if parsed.To4() == nil {
		host = "[" + host + "]"
	}
	return "https://" + host + ":7788"
}

func dockerProbe(t *testing.T, inside bool) {
	t.Helper()
	restore := SetHostProbeForTest(nil, func() bool { return inside })
	t.Cleanup(restore)
}

func TestATwelveHexNameBesideDockerenvIsNotPublished(t *testing.T) {
	dockerProbe(t, true)
	eps := Endpoints(Params{Port: 7788, HostOverride: "0123456789ab"})
	if hasURL(eps, "https://0123456789ab.local:7788") {
		t.Fatalf("container id published: %v", eps)
	}
	if !OmitContainerHostname("0123456789ab", true) {
		t.Fatal("12 hex beside the marker was kept")
	}
	if !OmitContainerHostname("0123456789ab.local", true) {
		t.Fatal("12 hex .local beside the marker was kept")
	}
}

func TestATwelveHexNameWithoutDockerenvIsPublished(t *testing.T) {
	dockerProbe(t, false)
	eps := Endpoints(Params{Port: 7788, HostOverride: "0123456789ab"})
	if !hasURL(eps, "https://0123456789ab.local:7788") {
		t.Fatalf("12 hex without the marker missing: %v", eps)
	}
	if OmitContainerHostname("0123456789ab", false) {
		t.Fatal("12 hex without the marker was omitted")
	}
}

func TestAChosenHostnameInsideDockerIsPublished(t *testing.T) {
	dockerProbe(t, true)
	eps := Endpoints(Params{Port: 7788, HostOverride: "musicbox"})
	if !hasURL(eps, "https://musicbox.local:7788") {
		t.Fatalf("chosen hostname missing: %v", eps)
	}
	if OmitContainerHostname("musicbox", true) {
		t.Fatal("chosen hostname was omitted")
	}
	if OmitContainerHostname("0123456789AB", true) {
		t.Fatal("uppercase spelling was omitted")
	}
}

func TestCertHostnameOmitsAGeneratedContainerName(t *testing.T) {
	cases := []struct {
		name   string
		host   string
		inside bool
		want   string
	}{
		{name: "generated", host: "0123456789ab", inside: true, want: ""},
		{name: "chosen", host: "musicbox", inside: true, want: "musicbox"},
		{name: "outside", host: "0123456789ab", inside: false, want: "0123456789ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := SetHostProbeForTest(
				func() (string, error) { return tc.host, nil },
				func() bool { return tc.inside },
			)
			t.Cleanup(restore)
			if got := CertHostname(); got != tc.want {
				t.Fatalf("CertHostname = %q", got)
			}
		})
	}
}

func TestADefaultDockerBridgeAddressInsideDockerIsNotPublished(t *testing.T) {
	dockerProbe(t, true)
	withIfaces(t, []ifaceAddr{
		oneAddr("eth0", net.FlagUp, "172.17.0.2"),
		oneAddr("eth0", net.FlagUp, "192.168.1.10"),
	})
	eps := Endpoints(Params{Port: 7788, HostOverride: "musicbox"})
	if hasURL(eps, httpsIP("172.17.0.2")) {
		t.Fatalf("172.17 published in endpoints: %v", eps)
	}
	if !hasURL(eps, httpsIP("192.168.1.10")) {
		t.Fatalf("LAN address missing from endpoints: %v", eps)
	}
	ips := GatherCertSANIPs(CertSANConfig{})
	if hasIP(ips, "172.17.0.2") {
		t.Fatalf("172.17 published in the want-set: %v", ips)
	}
	if !hasIP(ips, "192.168.1.10") {
		t.Fatalf("LAN address missing from the want-set: %v", ips)
	}
}

func TestALanAddressInsideDockerIsPublished(t *testing.T) {
	dockerProbe(t, true)
	withIfaces(t, []ifaceAddr{
		oneAddr("eth0", net.FlagUp, "192.168.1.10"),
		oneAddr("eth1", net.FlagUp, "10.1.2.3"),
		oneAddr("eth2", net.FlagUp, "172.18.0.2"),
	})
	eps := Endpoints(Params{Port: 7788, HostOverride: "musicbox"})
	ips := GatherCertSANIPs(CertSANConfig{})
	for _, ip := range []string{"192.168.1.10", "10.1.2.3", "172.18.0.2"} {
		if !hasURL(eps, httpsIP(ip)) {
			t.Errorf("endpoints dropped %s: %v", ip, eps)
		}
		if !hasIP(ips, ip) {
			t.Errorf("want-set dropped %s: %v", ip, ips)
		}
	}

	dockerProbe(t, false)
	withIfaces(t, []ifaceAddr{oneAddr("eth0", net.FlagUp, "172.17.0.2")})
	eps = Endpoints(Params{Port: 7788, HostOverride: "musicbox"})
	if !hasURL(eps, httpsIP("172.17.0.2")) {
		t.Fatalf("172.17 without the marker missing: %v", eps)
	}
	if !hasIP(GatherCertSANIPs(CertSANConfig{}), "172.17.0.2") {
		t.Fatal("172.17 without the marker missing from the want-set")
	}
}

func TestAPointToPointULAIsNotPublished(t *testing.T) {
	const ula = "fdcc:cccf:784b::2"
	withIfaces(t, []ifaceAddr{
		oneAddr("utun8", net.FlagUp|net.FlagPointToPoint, ula),
		oneAddr("utun8", net.FlagUp|net.FlagPointToPoint, "10.1.2.3"),
	})
	eps := Endpoints(Params{Port: 7788, HostOverride: "musicbox"})
	if hasURL(eps, httpsIP(ula)) {
		t.Fatalf("point-to-point ULA published: %v", eps)
	}
	if !hasURL(eps, httpsIP("10.1.2.3")) {
		t.Fatalf("point-to-point IPv4 dropped: %v", eps)
	}
	ips := GatherCertSANIPs(CertSANConfig{})
	if hasIP(ips, ula) {
		t.Fatalf("point-to-point ULA in the want-set: %v", ips)
	}
	if !hasIP(ips, "10.1.2.3") {
		t.Fatalf("point-to-point IPv4 missing from the want-set: %v", ips)
	}
}

func TestATailscaleULAOnAPointToPointInterfaceStaysOnTheTailscalePath(t *testing.T) {
	const ts = "fd7a:115c:a1e0::2"
	withIfaces(t, []ifaceAddr{
		oneAddr("utun5", net.FlagUp|net.FlagPointToPoint, ts),
	})
	eps := Endpoints(Params{Port: 7788, HostOverride: "musicbox"})
	if hasURL(eps, httpsIP(ts)) {
		t.Fatalf("Tailscale ULA emitted by Endpoints: %v", eps)
	}
	if !hasIP(GatherCertSANIPs(CertSANConfig{}), ts) {
		t.Fatal("Tailscale ULA missing from the want-set")
	}
}

func TestAULAOnAPhysicalInterfaceIsPublished(t *testing.T) {
	const ula = "fdcc:cccf:784b::2"
	withIfaces(t, []ifaceAddr{oneAddr("en0", net.FlagUp, ula)})
	eps := Endpoints(Params{Port: 7788, HostOverride: "musicbox"})
	if !hasURL(eps, httpsIP(ula)) {
		t.Fatalf("ULA on en0 missing: %v", eps)
	}
	if !hasIP(GatherCertSANIPs(CertSANConfig{}), ula) {
		t.Fatal("ULA on en0 missing from the want-set")
	}
}

func TestAWintunShapedULAIsPublished(t *testing.T) {
	const ula = "fdcc:cccf:784b::9"
	withIfaces(t, []ifaceAddr{oneAddr("Wintun", net.FlagUp, ula)})
	eps := Endpoints(Params{Port: 7788, HostOverride: "musicbox"})
	if !hasURL(eps, httpsIP(ula)) {
		t.Fatalf("Wintun ULA missing: %v", eps)
	}
	if !hasIP(GatherCertSANIPs(CertSANConfig{}), ula) {
		t.Fatal("Wintun ULA missing from the want-set")
	}
}

func TestACustomEndpointKeepsAnAddressTheWalkSkipped(t *testing.T) {
	dockerProbe(t, true)
	const ula = "fdcc:cccf:784b::2"
	withIfaces(t, []ifaceAddr{
		oneAddr("eth0", net.FlagUp, "172.17.0.2"),
		oneAddr("utun8", net.FlagUp|net.FlagPointToPoint, ula),
	})
	custom := []string{
		"https://172.17.0.2:7788",
		httpsIP(ula),
	}
	eps := Endpoints(Params{Port: 7788, HostOverride: "musicbox", CustomEndpoints: custom})
	for _, url := range custom {
		if !hasClass(eps, url, ClassCustom) {
			t.Errorf("custom %s missing: %v", url, eps)
		}
	}
	ips := GatherCertSANIPs(CertSANConfig{CustomEndpoints: custom})
	if !hasIP(ips, "172.17.0.2") || !hasIP(ips, ula) {
		t.Fatalf("custom addresses missing from the want-set: %v", ips)
	}
}

// TestADockerMarkerThatCannotBeReadKeepsTheContainerRules pins that only a
// missing /.dockerenv means "not in a container". A stat that fails another
// way (here a permission error) reads as a container, so a generated
// hostname cannot reach the pairing QR (CodeRabbit on #1204).
func TestADockerMarkerThatCannotBeReadKeepsTheContainerRules(t *testing.T) {
	dir := t.TempDir()
	if dockerMarkerAt(filepath.Join(dir, "missing")) {
		t.Fatal("a missing marker read as a container")
	}
	present := filepath.Join(dir, "dockerenv")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !dockerMarkerAt(present) {
		t.Fatal("a present marker did not read as a container")
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a directory without search permission refuses a stat only for a non-root Unix user")
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if !dockerMarkerAt(filepath.Join(locked, "dockerenv")) {
		t.Fatal("a marker whose stat failed with a permission error read as no container")
	}
}
