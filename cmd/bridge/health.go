package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"time"
)

// healthCmd is a container liveness probe (the Docker HEALTHCHECK). It checks
// that the bridge's API listener is accepting TCP connections on the
// configured listen address, and exits 0 iff it is.
//
// Why a TCP dial and not an /v1/health GET: the API speaks TLS with a
// self-signed cert on loopback, so an HTTP probe would have to disable
// certificate verification — a real (if benign-here) finding for security
// scanners. A TCP connect is the standard container liveness signal (cf.
// Kubernetes TCPSocketAction): the listener accepts once serve is up and a
// crashed process refuses the connection.
//
// Why not `bridge status`: status probes the ADMIN API, which is wrapped in
// session auth in public mode — a healthy public-mode container would get
// 401/403 and Docker would mark it unhealthy forever. Reading the API listen
// address from the config keeps this probe correct in loopback (:7788) and
// public (:443/:8443) alike, with no auth gate.
func healthCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to config file (default: ./bridge.yaml, else the platform config dir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, _, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "health: config load: %v\n", err)
		return 1
	}

	host, port, err := net.SplitHostPort(cfg.ListenAddress)
	if err != nil {
		fmt.Fprintf(stderr, "health: bad listenAddress %q: %v\n", cfg.ListenAddress, err)
		return 1
	}
	// A wildcard bind doesn't listen on a routable host — dial loopback. A
	// specific bound IP is dialed as-is so the probe follows the operator's
	// networking intent.
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, port)

	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		fmt.Fprintf(stderr, "health: API listener %s not accepting connections: %v\n", addr, err)
		return 1
	}
	_ = conn.Close()
	fmt.Fprintln(stdout, "ok")
	return 0
}
