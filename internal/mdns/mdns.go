// Package mdns advertises the bridge on the local network as
// _onebit-bridge._tcp.local. so iOS clients can auto-discover on LAN.
//
// mDNS only works on the same broadcast domain (no routing across the
// internet, unlikely to traverse Tailscale reliably). It's a LAN
// convenience — once paired, the iOS app stores the Tailscale IP or
// public hostname for remote access. The TXT records include the
// advertised protocolVersion so the iOS client can refuse incompatible
// versions before even attempting a TLS handshake.
package mdns

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	hcmdns "github.com/hashicorp/mdns"
)

// Service is the Bonjour service type the bridge registers under.
// Underscore prefix is mandatory for DNS-SD service types.
const Service = "_onebit-bridge._tcp"

// Advertiser wraps a running mDNS server. Close to stop advertising;
// safe for concurrent callers.
type Advertiser struct {
	server    *hcmdns.Server
	closeOnce sync.Once
	closeErr  error
}

// Config controls what's advertised. InstanceName and Port are the only
// required fields; the rest are derived if empty.
type Config struct {
	// InstanceName is the human-friendly name shown in the iOS picker
	// (e.g. "Arsenie's 1-bit Bridge"). Spaces are allowed; control chars
	// and dots are stripped.
	InstanceName string

	// Port the bridge's HTTPS listener is bound to.
	Port int

	// Hostname advertised in the SRV record. If empty, os.Hostname is
	// used. Trailing dots stripped; ".local." appended for Bonjour
	// compliance.
	Hostname string

	// ProtocolVersion is advertised in the TXT record as "pv=<N>" so
	// clients can version-gate at discovery time.
	ProtocolVersion int

	// LibraryName is advertised in the TXT record as "library=<name>"
	// so the iOS picker can display it alongside the instance name.
	LibraryName string
}

// Advertise starts advertising Service with the given config. Returns
// an error if the underlying UDP sockets can't be opened (typically a
// permissions issue on Linux without cap_net_bind).
func Advertise(cfg Config) (*Advertiser, error) {
	if cfg.Port <= 0 {
		return nil, errors.New("mdns: Port must be > 0")
	}
	instance := sanitizeInstance(cfg.InstanceName)
	if instance == "" {
		instance = "1-bit Bridge"
	}
	host := cfg.Hostname
	if host == "" {
		if h, err := os.Hostname(); err == nil {
			host = h
		} else {
			host = "localhost"
		}
	}
	host = strings.TrimSuffix(host, ".")
	// Take only the first label of the OS hostname before appending
	// ".local.". On Linux/corp-managed macOS boxes os.Hostname() can
	// return an FQDN (e.g. mymachine.corp.example.com); without this
	// step the SRV target would be "mymachine.corp.example.com.local.",
	// which is not a valid mDNS hostname.
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	host += ".local."

	if cfg.ProtocolVersion <= 0 {
		cfg.ProtocolVersion = 1
	}

	info := buildTXTRecords(cfg)

	svc, err := hcmdns.NewMDNSService(
		instance, Service, "", host,
		cfg.Port, ipsForAdvertise(), info,
	)
	if err != nil {
		return nil, fmt.Errorf("mdns: NewMDNSService: %w", err)
	}

	srv, err := hcmdns.NewServer(&hcmdns.Config{Zone: svc})
	if err != nil {
		return nil, fmt.Errorf("mdns: NewServer: %w", err)
	}
	return &Advertiser{server: srv}, nil
}

// Close stops the advertisement. Safe to call multiple times and from
// concurrent goroutines. hashicorp/mdns's Shutdown() tears down the UDP
// listeners but does NOT send TTL-0 "goodbye" packets, so clients may
// see a stale entry until the record's TTL expires.
func (a *Advertiser) Close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		a.closeErr = a.server.Shutdown()
	})
	return a.closeErr
}

// buildTXTRecords assembles the TXT records that Bonjour clients see.
// Each entry is "key=value". iOS parses pv (protocol version) first —
// if it doesn't match a supported range, the picker greys the service
// out before any TLS handshake.
func buildTXTRecords(cfg Config) []string {
	out := []string{
		fmt.Sprintf("pv=%d", cfg.ProtocolVersion),
	}
	if cfg.LibraryName != "" {
		out = append(out, "library="+cfg.LibraryName)
	}
	return out
}

// sanitizeInstance strips characters Bonjour can't handle in the
// instance name. Dots confuse the label-splitting, control chars cause
// encoding errors.
func sanitizeInstance(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7F || r == '.' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ipsForAdvertise returns the non-loopback IPv4/IPv6 addresses to
// advertise in A/AAAA records. Link-local addresses are included —
// mDNS/Bonjour is explicitly designed to work over link-local
// (fe80::/10 and 169.254.0.0/16) and excluding them would break
// discovery on IPv6-only or zero-config LANs. Returns nil if no usable
// interface is present; hashicorp/mdns will then advertise without A/AAAA
// records, which clients typically resolve via SRV+DNS instead.
func ipsForAdvertise() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			out = append(out, ip)
		}
	}
	return out
}
