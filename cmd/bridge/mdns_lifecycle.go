package main

import (
	"fmt"
	"io"
	"sync"

	bridgemdns "github.com/acoseac/1-bit-bridge/internal/mdns"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// mdnsLifecycle owns the per-process Bonjour advertiser and
// supports hot-reload from the admin settings PATCH path. The
// pre-existing wiring spun up an advertiser once at boot and
// closed it on exit — PR 4 promotes that into a struct so the
// operator can toggle mDNS off/on without restarting the bridge.
//
// Thread safety: `mu` guards `advertiser` so a Set() called from
// the admin handler goroutine doesn't race the close-on-shutdown
// defer in main. The mutex window is small (one Close / one
// Advertise call), so contention is negligible.
//
// Idempotence: Set(true) on an already-running advertiser is a
// no-op; Set(false) on a not-running advertiser is also a no-op.
// The operator can hammer the checkbox without spawning leaked
// advertiser instances.
type mdnsLifecycle struct {
	cfg    bridgemdns.Config
	stderr io.Writer
	stdout io.Writer

	mu         sync.Mutex
	advertiser *bridgemdns.Advertiser
}

// newMDNSLifecycle constructs the lifecycle helper with the
// boot-time config (instance name, port, library name). The
// config is captured once — changing the library name at
// runtime would normally take effect on the next mDNS start; in
// practice nothing else exercises that path today, so we let
// the snapshot stand and revisit if needed.
func newMDNSLifecycle(cfg bridgemdns.Config, stdout, stderr io.Writer) *mdnsLifecycle {
	return &mdnsLifecycle{cfg: cfg, stdout: stdout, stderr: stderr}
}

// Set is the hot-reload entry point. `enabled=true` starts the
// advertiser if it isn't already running; `enabled=false` stops
// it. Errors are logged to stderr (non-fatal — mDNS is a nice-
// to-have, and the operator can keep using the bridge with it
// off).
func (m *mdnsLifecycle) Set(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if enabled {
		if m.advertiser != nil {
			return // already running
		}
		a, err := bridgemdns.Advertise(m.cfg)
		if err != nil {
			fmt.Fprintf(m.stderr, "mDNS advertise failed (non-fatal): %v\n", err)
			return
		}
		m.advertiser = a
		fmt.Fprintf(m.stdout, "mDNS: advertising as %q (protocol v%d)\n",
			m.cfg.LibraryName, version.ProtocolVersion)
		return
	}
	if m.advertiser == nil {
		return // already off
	}
	_ = m.advertiser.Close()
	m.advertiser = nil
	fmt.Fprintf(m.stdout, "mDNS: advertisement stopped\n")
}

// Close shuts down any active advertiser. Safe to call on a
// never-Set lifecycle (no-op). Used by the runServe deferred
// shutdown.
func (m *mdnsLifecycle) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.advertiser == nil {
		return
	}
	_ = m.advertiser.Close()
	m.advertiser = nil
}
