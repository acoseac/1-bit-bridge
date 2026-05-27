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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
	hcmdns "github.com/hashicorp/mdns"
)

var logger = logging.Component("mdns")

// Service is the Bonjour service type the bridge registers under.
// Underscore prefix is mandatory for DNS-SD service types.
const Service = "_onebit-bridge._tcp"

// defaultRebindInterval bounds how long a stale IP set can keep
// silencing the advertisement before the re-advertise loop
// notices and rebuilds. 60 s is the sweet spot — short enough
// that an operator switching networks (Wi-Fi roam, Ethernet plug,
// docking station) sees discovery work again within a minute,
// long enough that the rebuild cost (tear down + restart of the
// hashicorp/mdns server) doesn't fire on every spurious interface
// flap.
//
// The hazard this defends against is real: hashicorp/mdns
// snapshots the IP set at NewServer() time and never re-binds.
// After 5 days uptime on a typical macOS dev machine the bridge
// was logging "advertising" while `dns-sd -B _onebit-bridge._tcp`
// found nothing — the cached sockets were tied to a now-gone IP.
const defaultRebindInterval = 60 * time.Second

// Advertiser wraps a running mDNS server. Close to stop advertising;
// safe for concurrent callers.
//
// Internally manages a re-advertise goroutine that watches for
// network-interface changes and rebuilds the underlying mDNS
// server when the IP set drifts. The goroutine's lifetime is tied
// to Close().
type Advertiser struct {
	cfg       Config
	rebindMu  sync.Mutex // guards server + cachedIPs + closed against the rebind goroutine
	server    *hcmdns.Server
	cachedIPs []net.IP // last IP set advertised; compared against new snapshots in rebindLoop
	closed    bool

	// ipSource returns the current advertise-eligible interface
	// IPs. Pluggable so tests can drive the rebind loop
	// deterministically without touching the host's network
	// state. Defaults to ipsForAdvertise() at Advertise() time.
	ipSource func() []net.IP

	// rebindInterval is how often the loop polls the IP source
	// for changes. Tests override to a small value; production
	// uses defaultRebindInterval.
	rebindInterval time.Duration

	done      chan struct{}
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

	// Interface is the LAN-eligible network interface to bind the
	// mDNS multicast listener to. When nil, hashicorp/mdns uses
	// `net.ListenMulticastUDP` with a nil interface — which makes
	// the OS pick the multicast interface by default-route / metric.
	// On multi-homed Windows hosts with Tailscale (default tunnel
	// interface metric 5 < Wi-Fi 30), the OS picks the Tailscale
	// tunnel — which doesn't carry multicast — and the bridge's
	// mDNS advertisement never reaches the LAN. The iPhone's
	// Bonjour browser then never finds the bridge.
	//
	// Caller should resolve via `internal/dlna.PickLANEligibleInterface`
	// (the same picker used by the SSDP advertiser at
	// `internal/dlna/ssdp.go` and the SSDP discovery client at
	// `internal/dlna/discovery/client.go`). When resolution fails,
	// caller can pass nil here — production falls back to the
	// OS-default selection (which works fine on single-NIC hosts).
	Interface *net.Interface
}

// Advertise starts advertising Service with the given config. Returns
// an error if the underlying UDP sockets can't be opened (typically a
// permissions issue on Linux without cap_net_bind).
//
// On success, spawns a background goroutine that polls the
// advertise-eligible interface set every defaultRebindInterval (60 s)
// and rebuilds the underlying mDNS server when the IP set drifts —
// hashicorp/mdns snapshots IPs at construction time and never re-
// binds, so without this loop a Wi-Fi roam / Ethernet plug / docking-
// station handoff silently kills discovery until process restart.
// Goroutine stops on Close().
func Advertise(cfg Config) (*Advertiser, error) {
	return advertiseInternal(cfg, ipsForAdvertise, defaultRebindInterval, true /* spawn loop */)
}

// advertiseInternal is the shared core for Advertise() (production)
// and the package-internal test constructor. Splitting the seams out
// here lets tests inject a custom ipSource and a small
// rebindInterval without ever writing to Advertiser.ipSource after
// the rebind goroutine has started — the race detector caught that
// pattern, and a lock-on-every-read fix would have introduced
// contention against the (production) 60 s tick.
//
// `spawnLoop=false` is the test escape hatch: returns an Advertiser
// whose maybeRebind() the test drives manually, with no background
// goroutine. Production always passes true.
func advertiseInternal(cfg Config, ipSource func() []net.IP, interval time.Duration, spawnLoop bool) (*Advertiser, error) {
	if cfg.Port <= 0 || cfg.Port > 65535 {
		// Reject out-of-TCP-range ports up-front. The TXT record now
		// publishes `port=<int>` to clients, so an invalid value would
		// land in the Bonjour announcement and have iOS construct
		// unusable URLs from it.
		return nil, errors.New("mdns: Port must be in 1-65535")
	}
	if cfg.ProtocolVersion <= 0 {
		cfg.ProtocolVersion = 1
	}

	a := &Advertiser{
		cfg:            cfg,
		ipSource:       ipSource,
		rebindInterval: interval,
		done:           make(chan struct{}),
	}
	// Initial rebuild runs WITHOUT taking a.rebindMu. Safe because
	// `a` was just allocated and no goroutine has a reference to it
	// yet — `rebindLoop()` is the only other writer of
	// `a.server` / `a.cachedIPs` / `a.closed`, and we don't spawn it
	// until below (after this returns successfully). `Close()` is
	// the only other lock-acquiring caller, and it can't race a
	// pre-publication Advertiser either. The rebindMu contract from
	// `rebuildLocked`'s doc applies to every subsequent invocation
	// from `maybeRebind`, where contention with `Close` is real
	// (CodeRabbit on PR #112).
	if err := a.rebuildLocked(a.ipSource()); err != nil {
		return nil, err
	}
	if spawnLoop {
		go a.rebindLoop()
	}
	return a, nil
}

// rebuildLocked tears down the existing hashicorp/mdns server (if
// any) and stands a fresh one up bound to ips. The caller MUST
// hold a.rebindMu so concurrent rebinds and Close calls don't
// race the server pointer. Returns the error from NewServer when
// the new server fails to start; in that case the cached server
// is left as-nil so the next tick retries.
func (a *Advertiser) rebuildLocked(ips []net.IP) error {
	instance := sanitizeInstance(a.cfg.InstanceName)
	if instance == "" {
		instance = "1-bit Bridge"
	}
	// SRV target needs the trailing dot — `cfg.advertisedHost()`
	// returns the bare ".local" form (matching what iOS reads from
	// the TXT record), so we re-append it here.
	host := a.cfg.advertisedHost() + "."
	info := buildTXTRecords(a.cfg)

	svc, err := hcmdns.NewMDNSService(instance, Service, "", host, a.cfg.Port, ips, info)
	if err != nil {
		return fmt.Errorf("mdns: NewMDNSService: %w", err)
	}
	// Pin the multicast listener to the operator-chosen interface
	// when available. Without `Iface`, hashicorp/mdns falls through
	// to `net.ListenMulticastUDP(..., nil, ...)` which lets the OS
	// pick a default multicast interface — broken on multi-homed
	// Windows hosts with Tailscale (see Config.Interface docblock).
	// `Iface == nil` is also a valid input to hashicorp/mdns (its
	// own fallback to OS-default), so callers that can't resolve
	// an interface here still get a working-enough advertiser on
	// single-NIC hosts.
	srv, err := hcmdns.NewServer(&hcmdns.Config{
		Zone:  svc,
		Iface: a.cfg.Interface,
	})
	if err != nil {
		return fmt.Errorf("mdns: NewServer: %w", err)
	}
	if a.server != nil {
		// Best-effort shutdown of the previous server; we already
		// have the replacement built so a Shutdown error here is
		// logged but not fatal — the new server is the source of
		// truth for the next tick.
		if shutdownErr := a.server.Shutdown(); shutdownErr != nil {
			logger.Warn("mdns: shutdown previous server", "err", shutdownErr)
		}
	}
	a.server = srv
	a.cachedIPs = append([]net.IP(nil), ips...)
	return nil
}

// rebindLoop runs in a background goroutine for the lifetime of
// the Advertiser. Each tick it diffs the current IP source against
// the cached set and rebuilds the underlying mDNS server when they
// disagree. Logs the rebuild so operators can correlate "discovery
// stopped working" reports against actual server-side action.
//
// Cheap when nothing changes: a single net.Interfaces() call + a
// sorted-string compare. Expensive only on the (rare) network
// transition tick: tears down and rebuilds the hashicorp/mdns
// listener pair, which costs a couple of UDP sockets.
func (a *Advertiser) rebindLoop() {
	t := time.NewTicker(a.rebindInterval)
	defer t.Stop()
	for {
		select {
		case <-a.done:
			return
		case <-t.C:
			a.maybeRebind()
		}
	}
}

func (a *Advertiser) maybeRebind() {
	fresh := a.ipSource()
	a.rebindMu.Lock()
	defer a.rebindMu.Unlock()
	if a.closed {
		return
	}
	if ipSetEqual(a.cachedIPs, fresh) {
		return
	}
	if err := a.rebuildLocked(fresh); err != nil {
		// Don't blank cachedIPs — the previous server is still
		// running (we only swap in rebuildLocked on success), so
		// a transient NewMDNSService failure leaves us no worse
		// off than before. Next tick retries.
		//
		// Same `ips` key + `ipsForLog` shaping the success log
		// uses so log-greppers can correlate rebind-attempt
		// sequences across success/failure ticks without learning
		// a per-callsite vocabulary (Qodo on PR #112).
		logger.Error("mdns: rebind failed; keeping previous advertisement",
			"err", err, "ips", ipsForLog(fresh))
		return
	}
	logger.Info("mdns: re-advertising on new interface set",
		"ips", ipsForLog(a.cachedIPs))
}

// ipSetEqual returns true when a and b cover the same IPs, ignoring
// order. Both inputs are typically small (<10 entries on a dev
// machine), so the sorted-string compare is fine — no need for a
// per-entry hash map.
func ipSetEqual(a, b []net.IP) bool {
	if len(a) != len(b) {
		return false
	}
	as := make([]string, len(a))
	bs := make([]string, len(b))
	for i, ip := range a {
		as[i] = ip.String()
	}
	for i, ip := range b {
		bs[i] = ip.String()
	}
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// ipsForLog formats an IP slice for the `ips` log attribute —
// keeps the slog output readable without dumping the raw []net.IP.
func ipsForLog(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

// Close stops the advertisement and the rebind loop. Safe to call
// multiple times and from concurrent goroutines. hashicorp/mdns's
// Shutdown() tears down the UDP listeners but does NOT send TTL-0
// "goodbye" packets, so clients may see a stale entry until the
// record's TTL expires.
func (a *Advertiser) Close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		// Stop the rebind goroutine first so it can't race the
		// server pointer below.
		close(a.done)
		a.rebindMu.Lock()
		defer a.rebindMu.Unlock()
		a.closed = true
		if a.server != nil {
			a.closeErr = a.server.Shutdown()
			a.server = nil
		}
	})
	return a.closeErr
}

// buildTXTRecords assembles the TXT records that Bonjour clients see.
// Each entry is "key=value". iOS parses pv (protocol version) first —
// if it doesn't match a supported range, the picker greys the service
// out before any TLS handshake.
//
// `host` and `port` are advertised explicitly so iOS can build the
// `https://<host>:<port>` URL directly from the TXT record without
// having to NWConnection-resolve the Bonjour service to its hostport
// form. iOS 26.4's `currentPath?.remoteEndpoint` doesn't reliably
// surface the resolved IP for Bonjour-bound connections (it stays in
// `.service(...)` form even at state `.ready` time on some
// configurations), which left the Add Bridge sheet's URL field
// blank. Putting `host` and `port` directly in TXT sidesteps the
// problem — DNS-SD has already resolved the SRV record to host+port
// by the time the browser hands us a result, so we're just exposing
// what's already known.
func buildTXTRecords(cfg Config) []string {
	hostBare := strings.TrimSuffix(cfg.advertisedHost(), ".")
	out := []string{
		fmt.Sprintf("pv=%d", cfg.ProtocolVersion),
		fmt.Sprintf("host=%s", hostBare),
		fmt.Sprintf("port=%d", cfg.Port),
	}
	if cfg.LibraryName != "" {
		out = append(out, "library="+cfg.LibraryName)
	}
	return out
}

// advertisedHost returns the hostname that the SRV record will use,
// re-deriving it from `cfg.Hostname` (or os.Hostname when blank) and
// applying the same first-label + ".local." normalization Advertise
// uses internally. Kept as a method on Config so the TXT-record
// builder doesn't have to duplicate the logic.
//
// Always returns a non-empty hostname. Falls back to "localhost" when
// every other source is blank — `os.Hostname()` returning ("", nil) is
// rare but documented as possible on minimally-configured Linux
// containers, and a bare ".local" target would have made clients
// build URLs like `https://.local:7788` which are invalid.
func (cfg Config) advertisedHost() string {
	host := strings.TrimSuffix(cfg.Hostname, ".")
	if host == "" {
		if h, err := os.Hostname(); err == nil {
			host = strings.TrimSuffix(h, ".")
		}
	}
	if host == "" {
		host = "localhost"
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	return host + ".local"
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
