package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// healthCmd is a container liveness probe (the Docker HEALTHCHECK). It GETs
// the UNAUTHENTICATED /v1/health on the API listen address and exits 0 iff
// the bridge answers 200.
//
// Why /v1/health and not the admin API (`bridge status`): in public mode the
// admin console is wrapped in session auth, so a `bridge status` probe from
// inside the container would get 401/403 and Docker would mark a perfectly
// healthy public-mode container `unhealthy` forever. /v1/health is the iOS
// pairing-probe endpoint — unauthenticated in every deployment mode.
//
// The API cert is self-signed on loopback and, in public mode, the SNI
// switcher serves self-signed for this non-domain-SNI loopback connection —
// so the probe skips chain verification (it only needs liveness, not trust).
func healthCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, configFlagUsage)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "health: config load: %v\n", err)
		return 1
	}

	host, port, err := net.SplitHostPort(cfg.ListenAddress)
	if err != nil {
		fmt.Fprintf(stderr, "health: bad listenAddress %q: %v\n", cfg.ListenAddress, err)
		return 1
	}
	// A wildcard bind doesn't listen on a routable host — dial loopback.
	// A specific bound IP (e.g. an internal interface) is dialed as-is so
	// the probe follows the operator's networking intent.
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	url := "https://" + net.JoinHostPort(host, port) + "/v1/health"

	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(stderr, "health: %v\n", err)
		return 1
	}
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			// Loopback liveness probe against a self-signed listener; chain
			// trust is not the point (and can't succeed on 127.0.0.1 anyway).
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // loopback self-signed liveness probe
			DisableKeepAlives: true,                                  // probe runs every 30s for months — don't pool sockets
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "health: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "health: HTTP %d from %s\n", resp.StatusCode, url)
		return 1
	}
	fmt.Fprintln(stdout, "ok")
	return 0
}
