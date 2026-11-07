package main

import (
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	bridgemdns "github.com/acoseac/1-bit-bridge/internal/mdns"
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
//
// **Live config resolution** (Gemini medium on PR #294): the
// instance + library name are read from `nameSource` at Set time
// rather than captured at construction so a Settings PATCH that
// renamed the library AND then toggled mDNS off→on picks up the
// new name. Port + ProtocolVersion stay captured because they
// don't change at runtime.
type mdnsLifecycle struct {
	port            int
	protocolVersion int
	// nameSource yields the current library name. Closure
	// indirection (rather than a captured *config.Config)
	// keeps this file from importing internal/config.
	nameSource func() string
	stderr     io.Writer
	stdout     io.Writer
	// pick chooses the LAN interface the responder pins itself to; nil is
	// dlna.PickLANEligibleInterface. A boot test pins it to loopback
	// (serveOpts.mdnsPick), so a responder it starts answers nothing on the
	// LAN.
	pick func() (*net.Interface, error)
	// start starts the responder; nil is bridgemdns.Advertise. A test forces
	// a startup failure with it.
	start func(bridgemdns.Config) (*bridgemdns.Advertiser, error)

	mu         sync.Mutex
	advertiser *bridgemdns.Advertiser
}

// newMDNSLifecycle constructs the lifecycle helper. The
// `nameSource` closure is invoked at every Set(true) so a
// hot-reloaded library name reaches the next Bonjour
// advertisement without requiring a process restart.
func newMDNSLifecycle(port, protocolVersion int, nameSource func() string, stdout, stderr io.Writer) *mdnsLifecycle {
	return &mdnsLifecycle{
		port:            port,
		protocolVersion: protocolVersion,
		nameSource:      nameSource,
		stdout:          stdout,
		stderr:          stderr,
	}
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
		name := "1-bit Bridge"
		if m.nameSource != nil {
			if n := m.nameSource(); n != "" {
				name = n
			}
		}
		// `InterfaceSource` callback resolves the LAN-eligible
		// interface fresh on every rebind tick. A static capture at
		// startup would let a Wi-Fi → Ethernet handoff (interface
		// index changes, original adapter goes down) keep the mDNS
		// listener pinned to a now-dead adapter until the next
		// process restart. The rebind loop in `internal/mdns`
		// compares what it answers with the running responder's
		// interface every minute. Soft-fail: if the picker errors
		// (host with no LAN-eligible interface at all), return nil
		// + log once → mDNS falls back to OS-default selection. Per
		// CodeRabbit on PR #307 round-1.
		// TestMDNSInterfaceSourceIsTheOncePerStreakOne pins that the
		// source is lanInterfaceSource's, written in the literal.
		pick := m.pick
		if pick == nil {
			pick = func() (*net.Interface, error) {
				return dlna.PickLANEligibleInterface(dlna.EligibilityOpts{})
			}
		}
		start := m.start
		if start == nil {
			start = bridgemdns.Advertise
		}
		a, err := start(bridgemdns.Config{
			InstanceName:    name,
			Port:            m.port,
			ProtocolVersion: m.protocolVersion,
			LibraryName:     name,
			InterfaceSource: lanInterfaceSource(pick, m.stderr),
			// The host's name through the probe /v1/health's `.local`
			// endpoint and the certificate read it through (backlog B744),
			// so the responder publishes the name they advertise.
			HostnameSource: advertise.ReportedHostname,
		})
		if err != nil {
			fmt.Fprintf(m.stderr, "mDNS advertise failed (non-fatal): %v\n", err)
			// A converted host name's `.local` endpoint is published from
			// the setting, which says on, whether or not the responder
			// started, and nothing but this responder answers it (backlog
			// B744). Said here, read from the probe the endpoints read,
			// with no state of its own: publication stays as it is.
			if local := advertise.OwnResponderEndpoint(m.port); local != "" {
				fmt.Fprintf(m.stderr, "mDNS: the advertised %s will not resolve while mDNS is down: it is the host's "+
					"name in its IDNA ASCII form, which only the bridge's own responder answers, and /v1/health "+
					"and the pairing QR keep listing it while mdns.enabled is on\n", local)
			}
			return
		}
		m.advertiser = a
		// **Use the captured ProtocolVersion**, not the package-
		// scope `version.ProtocolVersion`, so the log line
		// reflects the value this lifecycle was constructed
		// with (CodeRabbit Minor on PR #294 — the global
		// could drift from the captured config in a future
		// refactor that re-uses this helper across processes).
		fmt.Fprintf(m.stdout, "mDNS: advertising as %q (protocol v%d)\n",
			name, m.protocolVersion)
		return
	}
	if m.advertiser == nil {
		return // already off
	}
	_ = m.advertiser.Close()
	m.advertiser = nil
	fmt.Fprintf(m.stdout, "mDNS: advertisement stopped\n")
}

// lanInterfaceSource returns the advertiser's InterfaceSource: pick's
// interface, or nil when pick fails. The advertiser calls it at every
// rebind tick, once a minute, so a failure is printed when a streak of
// them starts or its text changes, never per call: a host with no
// LAN-eligible interface would otherwise print the same line every
// minute, the M-SEARCH flood's shape. Until 2026-09-28 it was called
// only when a rebuild ran, and printed every failure.
func lanInterfaceSource(pick func() (*net.Interface, error), stderr io.Writer) func() *net.Interface {
	var mu sync.Mutex
	var printed string // the failure last printed; "" once a pick succeeds
	return func() *net.Interface {
		iface, err := pick()
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			if msg := err.Error(); msg != printed {
				printed = msg
				fmt.Fprintf(stderr, "mDNS: LAN interface pick failed (rebind will use OS default): %v\n", err)
			}
			return nil
		}
		printed = ""
		return iface
	}
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
