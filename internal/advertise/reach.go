package advertise

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"regexp"
	"strings"
)

// PairingNeedsEndpoint is the sentence the pairing page, a refused mint,
// and bridge doctor share when a detected container has no address a
// phone can dial.
const PairingNeedsEndpoint = "set BRIDGE_CUSTOM_ENDPOINTS to this host's address"

// containerID is the hostname Docker writes when the operator does not
// set one: the first 12 hex digits of the container id.
var containerID = regexp.MustCompile(`^[0-9a-f]{12}$`)

var (
	// 172.17.0.0/16 is Docker's default bridge. Safe to name: the walk drops it only while /.dockerenv is present.
	dockerBridge = mustCIDR("172.17.0.0/16")
	ulaRange     = mustCIDR("fc00::/7")
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// ifaceAddr is one interface and the addresses a walk will judge.
// Addrs is captured with the interface so a test can hand the walk a
// closed set, and so the production walk does not call Addrs again
// after the test has substituted the list.
type ifaceAddr struct {
	iface net.Interface
	addrs []net.Addr
}

// enumerateIfaces, hostnameFunc and inDocker are the seams a test
// replaces. Production reads the host. A test that needs a chosen
// address or a chosen hostname replaces them and restores on cleanup.
var (
	enumerateIfaces = readInterfaces
	hostnameFunc    = os.Hostname
	inDocker        = dockerMarker
)

func readInterfaces() ([]ifaceAddr, error) {
	raw, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]ifaceAddr, 0, len(raw))
	for _, iface := range raw {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		out = append(out, ifaceAddr{iface: iface, addrs: addrs})
	}
	return out, nil
}

func dockerMarker() bool {
	return dockerMarkerAt("/.dockerenv")
}

// dockerMarkerAt reports whether path marks a container. Only "not there"
// means no container. Any other stat error (a permission or I/O failure)
// keeps the container rules on: their mistake is a missing alternate,
// while the other side's is a pairing QR that names the phone itself.
func dockerMarkerAt(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// SetHostProbeForTest replaces the hostname and container-marker reads
// for the rest of this process until the returned function runs.
// A nil argument leaves that read alone.
func SetHostProbeForTest(host func() (string, error), docker func() bool) func() {
	prevH, prevD := hostnameFunc, inDocker
	if host != nil {
		hostnameFunc = host
	}
	if docker != nil {
		inDocker = docker
	}
	return func() {
		hostnameFunc, inDocker = prevH, prevD
	}
}

// OmitContainerHostname reports whether host is Docker's generated
// container id while the process is inside a container. The short
// label, cut at the first dot, must be exactly 12 lowercase hex digits,
// and inDocker must be true. A name the operator chose, a 12-hex name
// on a host with no container marker, and an uppercase spelling all
// stay.
func OmitContainerHostname(host string, inDocker bool) bool {
	if !inDocker || host == "" {
		return false
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	return containerID.MatchString(host)
}

// omitFromWalk reports whether this address is one a phone cannot dial
// and that the operator did not write into customEndpoints.
//
// 172.17.0.0/16 is the default docker0 bridge. It leaves only while the
// container marker is present, so a machine that really numbers a LAN
// there keeps the address. A Compose network on another 172.16/12
// slice stays.
//
// A point-to-point ULA leaves after Tailscale classification. Tailscale's
// own range, and a ULA on an interface that is not point-to-point
// (Wintun carries no such flag), stay. The classifier runs first so a
// Tailscale address on utun is kept for the certificate even though
// health publishes that address from the provider.
func omitFromWalk(iface net.Interface, ip net.IP, docker bool) bool {
	if docker && dockerBridge.Contains(ip) {
		return true
	}
	if isTailscale(iface, ip) {
		return false
	}
	if iface.Flags&net.FlagPointToPoint != 0 && ulaRange.Contains(ip) {
		return true
	}
	return false
}

// skipInterface reports whether the walk passes over this interface
// before it looks at the addresses.
func skipInterface(iface net.Interface) bool {
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
		return true
	}
	// Windows Hyper-V / WSL / VirtualBox / VMware host
	// virtual switches enumerate as up interfaces with
	// 192.168.x.1 host-only IPs. iOS can't route to them,
	// and shipping them in /v1/health just produces red
	// "request timed out" rows in the device's endpoint
	// list. Drop at the source.
	return isVirtualSwitchInterface(iface.Name)
}

// skipAddress reports whether the walk passes over this address.
// ok is false when the address could not be read as an IP, and that
// case returns before the IP is used.
func skipAddress(iface net.Interface, ip net.IP, ok, docker bool) bool {
	if !ok || ip.IsLoopback() {
		return true
	}
	// Link-local addresses (IPv4 169.254/16, IPv6 fe80::/10)
	// are NOT useful across-device — IPv4 link-local only
	// appears when DHCP failed, and IPv6 fe80:: requires a
	// `%zone-id` the bridge can't emit for a remote client.
	// Shipping them in `/v1/health` just pollutes the iOS
	// selector with URLs that will always fail. mDNS still
	// advertises link-local on purpose for its own transport
	// — that's a different use case.
	if ip.IsLinkLocalUnicast() {
		return true
	}
	return omitFromWalk(iface, ip, docker)
}

// eachUsableAddr visits every address Endpoints and the SAN walk both
// judge: up, not loopback, not a virtual switch, not link-local, and
// not an address omitFromWalk drops. It returns false when the
// interface list itself cannot be read. Endpoints still adds the
// hostname URL in that case.
func eachUsableAddr(fn func(net.Interface, net.IP)) bool {
	ifaces, err := enumerateIfaces()
	if err != nil {
		return false
	}
	docker := inDocker()
	for _, ia := range ifaces {
		if skipInterface(ia.iface) {
			continue
		}
		for _, addr := range ia.addrs {
			ip, ok := ipFromAddr(addr)
			if skipAddress(ia.iface, ip, ok, docker) {
				continue
			}
			fn(ia.iface, ip)
		}
	}
	return true
}

// CertHostname is the hostname a certificate minted now would carry.
// A detected container's generated name is omitted, which drops both
// the bare id and the .local name from the SAN want-set. Any other
// name is returned as the host reported it.
func CertHostname() string {
	host, err := hostnameFunc()
	if err != nil || host == "" || OmitContainerHostname(host, inDocker()) {
		return ""
	}
	return host
}

// PairingHost is the hostname the pairing QR is built from, and whether
// the container marker is present. The hostname is returned as reported,
// including when it is a generated container id. An error yields an
// empty hostname with the marker still answered.
func PairingHost() (string, bool) {
	host, err := hostnameFunc()
	docker := inDocker()
	if err != nil {
		return "", docker
	}
	return host, docker
}
