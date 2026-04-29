// SAN-gather helpers for the TLS cert template. Walk the same set of
// network interfaces `Endpoints()` advertises, plus the Tailscale-
// supplied IPs/DNSName, plus the parsed hosts of operator-supplied
// `cfg.CustomEndpoints` — feed all of them into the cert's SAN slot
// at first-mint or `bridge cert rotate` time, so iOS clients can
// handshake against any URL the bridge advertises in /v1/health.
//
// Loopback IPs (127.0.0.1, ::1, 0.0.0.0) are NOT included here — the
// `internal/tls.mergeIPs` helper unconditionally adds them. We only
// emit the operator-relevant extras.
package advertise

import (
	"net"
	"net/url"
	"strings"
)

// CertSANConfig is the minimal slice of *config.Config we need to
// gather SAN inputs. Defined here (rather than imported from
// `internal/config`) so the advertise package stays config-package-
// independent — matches the pattern `Endpoints()` uses with `Params`.
type CertSANConfig struct {
	// CustomEndpoints is `cfg.CustomEndpoints` — operator-supplied
	// URLs whose hostnames need cert SAN coverage. Already validated
	// by `cfg.Validate` (HTTPS-only, parseable).
	CustomEndpoints []string
}

// GatherCertSANIPs returns the operator-relevant IP set the TLS cert
// SAN list should cover, on top of the loopback defaults the cert
// template adds unconditionally.
//
// Sources:
//
//  1. Up, non-loopback, non-virtual-switch interface IPs — the same
//     filter `Endpoints()` uses, so the cert covers every URL the
//     bridge advertises in /v1/health.
//  2. `tailscale status --json` Self.TailscaleIPs (silent on failure).
//  3. IP-literal hostnames from CustomEndpoints (e.g.
//     `https://192.168.50.10:7788` → `192.168.50.10`).
//
// Link-local IPs are dropped (not useful across-device; same rule as
// Endpoints()). Returns deduped — same physical IP appearing twice
// across sources is collapsed.
func GatherCertSANIPs(cfg CertSANConfig) []net.IP {
	var out []net.IP
	seen := make(map[string]bool)
	add := func(ip net.IP) {
		if ip == nil {
			return
		}
		key := string(ip.To16())
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, ip)
	}

	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			if isVirtualSwitchInterface(iface.Name) {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				ip, ok := ipFromAddr(addr)
				if !ok || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
					continue
				}
				add(ip)
			}
		}
	}

	for _, ip := range GetTailscaleIPs() {
		add(ip)
	}

	for _, raw := range cfg.CustomEndpoints {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u == nil {
			continue
		}
		host := u.Hostname()
		if ip := net.ParseIP(host); ip != nil {
			add(ip)
		}
	}

	return out
}

// GatherCertSANDNS returns the operator-relevant DNS hostname set the
// cert SAN list should cover, on top of `localhost` + `os.Hostname`
// which the cert template adds unconditionally.
//
// Sources:
//
//  1. `tailscale status --json` Self.DNSName (silent on failure).
//  2. Hostname-shaped CustomEndpoints (`https://my-bridge.example.com`
//     → `my-bridge.example.com`). IP-literal entries route to
//     `GatherCertSANIPs` via `net.ParseIP` instead.
//
// Hostnames are deduped case-insensitively.
func GatherCertSANDNS(cfg CertSANConfig) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, name)
	}

	if magic := GetTailscaleDNSName(); magic != "" {
		add(magic)
	}

	for _, raw := range cfg.CustomEndpoints {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u == nil {
			continue
		}
		host := u.Hostname()
		if host == "" {
			continue
		}
		if ip := net.ParseIP(host); ip != nil {
			continue // IP literal — handled by GatherCertSANIPs
		}
		add(host)
	}

	return out
}
