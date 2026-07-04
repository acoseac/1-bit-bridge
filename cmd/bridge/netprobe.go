package main

import "net"

// probeLoopbackAddr rewrites a wildcard / unspecified bind address to a
// loopback address so a LOCAL liveness probe can actually connect to it.
//
// A direct TCP dial to 0.0.0.0 (or ::) succeeds on Linux/macOS — the OS
// routes it to loopback — but fails on Windows with WSAEADDRNOTAVAIL. In
// public mode the admin listener may legitimately bind a wildcard
// address (config.validateLoopbackAddress is skipped for public
// deployments), so a probe that dials cfg.AdminAddress verbatim would
// spuriously report the bridge "down" on Windows. One such false negative
// (bridge tsnet logout's running-instance guard) would wipe the tsnet
// state dir out from under a running bridge; another (the restart
// health-probe) would falsely report "service didn't respond".
//
// Non-wildcard hosts — a concrete IP, a loopback literal, a hostname —
// are returned unchanged. net.ParseIP returns nil for a hostname or an
// empty host, so the IsUnspecified() check is nil-guarded to avoid a
// panic.
func probeLoopbackAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" {
		// ":7789" — empty host binds all interfaces; probe loopback.
		return net.JoinHostPort("127.0.0.1", port)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			return net.JoinHostPort("127.0.0.1", port)
		}
		return net.JoinHostPort("::1", port) // JoinHostPort brackets IPv6 → [::1]:port
	}
	return addr
}
