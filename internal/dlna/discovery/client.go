package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
	"golang.org/x/net/ipv4"
)

// packageLogger follows the repo convention — internal/* packages
// declare a single package-scoped `logging.Component(...)` logger.
// Per CodeRabbit Major round-1 on PR #305.
var packageLogger = logging.Component("dlna-discovery")

// ssdpReadErrBackoff paces the read loop after an unexpected (non-timeout,
// non-shutdown) UDP read error so a persistently-broken socket can't hot-spin
// a CPU core. On Windows an unconnected UDP socket can surface WSAECONNRESET
// on the read that follows a send whose datagram drew an ICMP port-unreachable
// (SIO_UDP_CONNRESET) — a transient one-shot we recover from rather than
// letting it kill discovery for the process lifetime.
const ssdpReadErrBackoff = 250 * time.Millisecond

// ssdpReadErrEscalateAt is the consecutive-read-error count at which the loop
// logs once at Error instead of Warn — a distinct "discovery is sustained-
// degraded" signal (e.g. the interface was removed) vs a one-off transient
// blip. ~20 × ssdpReadErrBackoff ≈ 5s of back-to-back failures.
const ssdpReadErrEscalateAt = 20

// HandleReadErr drives the shared read-loop resilience policy so BOTH SSDP
// discovery read loops — this package's renderer client AND
// internal/upnp's MediaServer client — stay byte-identical (they mirror each
// other by design; keeping the policy in one place is what stops them drifting):
//   - a read-deadline timeout is the normal idle tick → reset the streak, the
//     caller continues;
//   - a shutdown signal (ctx cancelled OR socket closed by Stop) → the caller
//     returns;
//   - any other (transient) error, e.g. a Windows WSAECONNRESET after an ICMP
//     port-unreachable → log + ctx-aware backoff (bounding a persistently-
//     broken socket at ~4 reads/sec instead of hot-spinning a core), bump the
//     streak, and escalate to Error once failures are sustained; caller
//     continues.
//
// Returns stop=true when the caller should exit its loop. `streak` is the
// caller's consecutive-error counter (reset here on a timeout, bumped on a
// transient error; the caller also resets it on a successful read).
func HandleReadErr(ctx context.Context, err error, streak *int, log *slog.Logger) (stop bool) {
	var nErr net.Error
	if errors.As(err, &nErr) && nErr.Timeout() {
		*streak = 0
		return false
	}
	if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
		return true
	}
	*streak++
	if *streak == ssdpReadErrEscalateAt {
		log.Error("SSDP read errors sustained; discovery degraded",
			"consecutive", *streak, "err", err.Error())
	} else {
		log.Warn("SSDP read error; backing off", "err", err.Error())
	}
	select {
	case <-ctx.Done():
		return true
	case <-time.After(ssdpReadErrBackoff):
	}
	return false
}

// structuralStubLastSeen is the sentinel LastSeenAt stamped on a
// STRUCTURALLY-failed renderer stub (4xx / unparseable description / no
// AVTransport — see errStructuralDescription). Because EvictStale treats
// a future timestamp as never-stale (IsStaleRenderer's interval<0 branch),
// the stub persists indefinitely; combined with the exists-branch's
// "incomplete stub → don't refresh, don't re-fetch" rule, that suppresses
// the retry storm for a permanently-broken renderer until it sends
// ssdp:byebye (Remove) or the bridge restarts. A TRANSIENT-failure stub
// instead keeps its real fail-time LastSeenAt so it ages out + retries.
// Both are hidden from /v1/renderers by Snapshot's ControlURL=="" gate.
// (Gemini consult — bridge-12.)
var structuralStubLastSeen = time.Date(2999, time.January, 1, 0, 0, 0, 0, time.UTC)

// maxTrackedLocations caps how many distinct SSDP Locations a single UDN may
// hold in lastLocations. A real renderer has one address per interface it is
// attached to, so 4 covers dual-homed hardware with room to spare while
// keeping a buggy or spoofed source that announces a fresh host every packet
// from growing the map without bound. On overflow the LEAST-recently-observed
// record is dropped — the one least likely to still be live.
const maxTrackedLocations = 4

// locationRecord is one SSDP Location observed for a UDN, plus when it was
// last seen. The timestamp is what separates "this renderer answers from two
// addresses" (both keep being announced, so both stay live and neither reads
// as a move) from "this renderer moved away and later came back" (the old
// address stops being announced, ages out after RendererTTL, and IS treated
// as a move when it reappears).
type locationRecord struct {
	url  string
	seen time.Time
}

// SSDPDiscoveryClient is the orchestrator that drives SSDP M-SEARCH
// + per-renderer detail-fetch + cache lifecycle.
//
// **Lifecycle**: `Start(ctx)` binds an ephemeral-port UDP socket
// (wildcard 0.0.0.0:0) for sending M-SEARCH requests + receiving
// unicast responses. Loops every `cfg.MSearchInterval` (default 30s)
// sending fresh M-SEARCHes + periodically evicting stale cache
// entries. `Stop()` cancels the run loop, closes the socket, and
// clears the cache for clean teardown.
//
// **v1: M-SEARCH cycle only, no NOTIFY listening.** SSDP supports
// renderers spontaneously announcing themselves via
// `NOTIFY ssdp:alive` / `ssdp:byebye` packets to the well-known
// multicast group + port (239.255.255.250:1900). Listening for
// NOTIFY would require a SECOND socket bound to that well-known
// port — and the bridge's own DLNA server-side advertiser
// (`internal/dlna/ssdp.go`) is already bound there. Cross-process
// SO_REUSEPORT semantics differ between Linux and macOS and even
// when supported deliver packets to exactly one socket per packet,
// which would split NOTIFY observations between the two sockets
// unpredictably. v1 ships M-SEARCH-only — renderers that come +
// go between cycles surface within `MSearchInterval` (30s) on
// the next cycle; departures surface via `RendererTTL` (60s)
// eviction. A future PR can add cross-package coupling with the
// server-side advertiser to multiplex NOTIFY observations.
//
// **Linux/BSD socket correctness**: binding to wildcard
// `0.0.0.0:0` (NOT `239.255.255.250:1900` via
// `ListenMulticastUDP`) is load-bearing. A multicast-bound socket
// on Linux delivers ONLY packets whose destination IP matches the
// bound multicast address — unicast M-SEARCH responses from
// renderers (sent to the host IP + the source port of our
// M-SEARCH packet) would NOT land on the multicast-bound socket
// and discovery would silently fail. macOS is more permissive
// but the correct portable shape is wildcard + explicit
// multicast-write outgoing. Per Gemini HIGH round-1 on PR #305.
//
// **Thread model**: the client owns ONE goroutine — `runLoop` —
// that reads from the UDP socket + dispatches packets to per-event
// handlers, AND a sibling `runTickLoop` that ticks the M-SEARCH /
// eviction cadence. All cache mutations happen from these two
// goroutines + the cache's own mutex serializes against the HTTP
// handler's read path. Per-renderer detail fetches are dispatched
// via a bounded worker semaphore (4) to keep network concurrency
// under control.
type SSDPDiscoveryClient struct {
	cfg DiscoveryConfig

	// cache is the shared store consumed by the HTTP handler.
	// Constructed externally so the same cache can be wired into
	// both the client lifecycle AND the /v1/renderers handler.
	cache *RendererCache

	// dispatcher backs the per-renderer detail fetches. Stub
	// implementations injected via `cfg.Dispatcher` for tests.
	dispatcher SOAPDispatcher

	// runMu guards conn / runCtx / runCancel lifecycle. Writers
	// (Start / Stop) take Lock; readers (runLoop / sendMSearch)
	// take RLock to snapshot conn before each access. Per Gemini
	// CRITICAL round-1 on PR #305 — the prior shape had runLoop
	// dereferencing `c.conn` after Stop had already cleared it,
	// risking a nil-pointer panic that would crash the bridge.
	runMu     sync.RWMutex
	runCtx    context.Context
	runCancel context.CancelFunc
	conn      *net.UDPConn

	// detailFetchSem bounds concurrent detail fetches so a 100-
	// renderer LAN doesn't spawn 100 simultaneous outbound TCP
	// connections from a single M-SEARCH burst. 4 is comfortable
	// for any real home LAN.
	detailFetchSem chan struct{}

	// nowFunc returns the current time. Injected via cfg.NowFunc
	// for tests. Default: time.Now.
	nowFunc func() time.Time

	// wg tracks the two run-loop goroutines (runLoop, runTickLoop)
	// AND every in-flight per-renderer detail fetch, so Stop() can
	// block until the cache can no longer be mutated BEFORE it calls
	// cache.Clear(). Without it, a fetch that already passed its
	// runCtx.Err() guard could Upsert into the freshly-cleared cache
	// (the Stop()/Upsert race, external review r3). Mirrors the
	// documented bgScans WaitGroup invariant ("graceful shutdown
	// triggers full cleanup").
	wg sync.WaitGroup

	// locMu guards lastLocations + inFlight. A DEDICATED mutex, not
	// runMu: runMu's scope is the conn / runCtx lifecycle and Stop()
	// holds it across cache.Clear(), so borrowing it for the per-packet
	// bookkeeping below would widen a lock whose ordering contract is
	// already load-bearing.
	locMu sync.Mutex

	// lastLocations maps UDN → the SSDP `Location`s this client currently
	// believes are live for that renderer, most recently observed at the
	// recorded time. It is the host-change reference, and the reason it is a
	// SET rather than a single value is that one UDN legitimately announces
	// from more than one address: a dual-homed renderer (Wi-Fi + Ethernet)
	// answers an M-SEARCH on both, and duplicate responses within a single
	// cycle are expected (see fetchAndCacheDetails' stub rationale). Against
	// a single remembered Location, an A/B/A/B sequence read as a move on
	// EVERY packet and re-fetched forever at the M-SEARCH cadence.
	//
	// It also covers entries whose cached ControlURL is empty — a
	// failed-fetch stub carries no URL to compare a fresh announcement
	// against, so without this map a renderer that failed at address A and
	// moved to address B would never be re-fetched (a structural stub
	// carries the year-2999 sentinel and never ages out).
	//
	// Bounded three ways: at most maxTrackedLocations records per UDN,
	// records unseen within RendererTTL are dropped on the next touch, and
	// pruneLocations reaps whole UDNs that are neither cached nor mid-fetch.
	//
	// Deliberately NOT a field on RendererInfo: that struct IS the
	// `/v1/renderers` wire shape (see renderer_dto.go) and this is
	// client-side bookkeeping the protocol has no business carrying.
	lastLocations map[string][]locationRecord

	// inFlight holds the UDNs with a detail fetch currently running, so
	// a burst of announcements for the same renderer dispatches exactly
	// one fetch. Two paths depend on it, both because a fetch publishes
	// nothing until it finishes: a NEW renderer sending a burst of packets
	// has no cache entry yet, so every one of them lands in the
	// first-time-UDN branch; and a MOVED renderer's new Location isn't
	// recorded until its fetch completes, so every further packet from that
	// address still reads as a move.
	//
	// Self-cleaning: every claim is released by the spawned fetch's
	// defer, so the map can't grow past the number of concurrent
	// fetches. NOT a substitute for a time-based cooldown — after a
	// FAILED fetch the stub's empty ControlURL makes the exists-branch
	// early-return, which suppresses re-fetching until EvictStale.
	inFlight map[string]struct{}
}

// DiscoveryConfig captures the SSDPDiscoveryClient's tunables.
// All fields except `Interface` have sensible defaults; the zero-
// value config + a nil interface is rejected by `NewSSDPDiscoveryClient`.
type DiscoveryConfig struct {
	// Interface is the LAN-eligible network interface to write
	// M-SEARCH packets out on. Caller resolves via
	// `internal/dlna.PickLANEligibleInterface` (the same picker
	// the server-side advertiser uses).
	Interface *net.Interface

	// MSearchInterval is the cadence for sending fresh M-SEARCH
	// requests. Default 30s. Tunable for operators whose
	// renderers are slow to respond / whose LAN is saturated.
	MSearchInterval time.Duration

	// RendererTTL is the staleness window. Entries older than this
	// (no M-SEARCH response observed within the window) get
	// evicted on the next tick. Default 60s — must be greater
	// than MSearchInterval so one missed cycle doesn't evict.
	// Enforced in `internal/config.DLNAConfig.Validate`.
	RendererTTL time.Duration

	// DetailFetchTimeout caps a single per-renderer detail fetch
	// (DeviceDescription OR GetProtocolInfo). Default 5s — well
	// under the operator's patience and short enough that a hung
	// fetch doesn't block subsequent fetches via the worker
	// semaphore.
	DetailFetchTimeout time.Duration

	// Dispatcher backs the detail fetches. Defaults to
	// HTTPClientDispatcher wrapping http.DefaultClient with the
	// per-call timeout above. Tests inject stubs.
	Dispatcher SOAPDispatcher

	// NowFunc returns the current time. Defaults to time.Now.
	// Tests inject a fixed clock so eviction timing is
	// deterministic.
	NowFunc func() time.Time
}

// DefaultDiscoveryConfig returns a config seeded with the doc'd
// defaults. Caller MUST set Interface before passing to
// NewSSDPDiscoveryClient.
func DefaultDiscoveryConfig() DiscoveryConfig {
	return DiscoveryConfig{
		MSearchInterval:    30 * time.Second,
		RendererTTL:        60 * time.Second,
		DetailFetchTimeout: 5 * time.Second,
	}
}

// NewSSDPDiscoveryClient constructs a discovery client. Returns an
// error when the config is invalid (nil Interface, etc.). The
// returned client is in the stopped state; call Start to begin
// discovery.
func NewSSDPDiscoveryClient(
	cfg DiscoveryConfig,
	cache *RendererCache,
) (*SSDPDiscoveryClient, error) {
	if cfg.Interface == nil {
		return nil, errors.New("DiscoveryConfig.Interface is required")
	}
	if cache == nil {
		return nil, errors.New("RendererCache is required")
	}
	// Apply defaults for unset durations / dispatcher.
	if cfg.MSearchInterval <= 0 {
		cfg.MSearchInterval = 30 * time.Second
	}
	if cfg.RendererTTL <= 0 {
		cfg.RendererTTL = 60 * time.Second
	}
	if cfg.DetailFetchTimeout <= 0 {
		cfg.DetailFetchTimeout = 5 * time.Second
	}
	if cfg.Dispatcher == nil {
		cfg.Dispatcher = &HTTPClientDispatcher{
			Client: &http.Client{
				Timeout: cfg.DetailFetchTimeout,
				// Relay 3xx verbatim instead of following it. The
				// description / control URLs come from SSDP-advertised
				// Location headers (a LAN device, possibly rogue or
				// spoofed), so auto-following a redirect to loopback
				// or a link-local metadata address would turn the
				// bridge into an SSRF probe against its own no-auth
				// admin API. Mirrors internal/upnpproxy's
				// CheckRedirect guard.
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			},
		}
	}
	nowFunc := cfg.NowFunc
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &SSDPDiscoveryClient{
		cfg:            cfg,
		cache:          cache,
		dispatcher:     cfg.Dispatcher,
		detailFetchSem: make(chan struct{}, 4), // see field docblock
		nowFunc:        nowFunc,
		lastLocations:  make(map[string][]locationRecord),
		inFlight:       make(map[string]struct{}),
	}, nil
}

// Start binds the SSDP socket + spawns the run loop. Returns an
// error when the bind fails. Caller MUST call Stop before
// discarding the client.
//
// `parent` is the operator-supplied lifecycle context (e.g. from
// `cmd/bridge`'s signal handler). The client derives its own
// cancellable child so Stop can tear down independently of
// parent cancellation.
func (c *SSDPDiscoveryClient) Start(parent context.Context) error {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	if c.runCancel != nil {
		return errors.New("SSDPDiscoveryClient already started")
	}

	// Bind to wildcard 0.0.0.0:0 (OS picks ephemeral port). This
	// socket:
	//   - SENDS M-SEARCH requests via WriteToUDP to the SSDP
	//     multicast address.
	//   - RECEIVES unicast M-SEARCH responses from renderers
	//     (sent back to our source IP + ephemeral port).
	//
	// We deliberately do NOT join the multicast group nor bind to
	// the multicast IP — see the lifecycle docblock above for the
	// rationale (Linux unicast-receive semantics + same-process
	// port conflict with the server-side advertiser).
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return fmt.Errorf("ListenUDP wildcard: %w", err)
	}
	c.conn = conn

	// Pin outgoing M-SEARCH multicast to the operator-chosen
	// interface. Without this, the kernel picks an outbound
	// interface for `239.255.255.250` based on default routes /
	// interface metrics — and on a multi-homed Windows host
	// (LAN + Tailscale) Tailscale's default metric of 5 wins over
	// Wi-Fi's 30. M-SEARCH then egresses via the Tailscale tunnel
	// (which doesn't carry multicast); the LAN renderer never sees
	// it; `/v1/renderers` stays empty forever. Confirmed
	// 2026-05-27 on `home-pc` (Windows bridge + Tailscale): same-LAN
	// 2go on `192.168.0.62` was reachable via direct HTTP but
	// invisible to SSDP discovery until a wakeup window happened
	// to hit the correct interface by chance.
	//
	// Mirrors the pattern already in `internal/dlna/ssdp.go` (the
	// SSDP advertiser, PR #303). Soft-fail on bind error per that
	// site's rationale: a failed bind degrades to OS-default
	// multicast egress (works on single-NIC hosts where the OS
	// default IS the LAN), but refusing to start would punish the
	// happy path.
	if c.cfg.Interface != nil {
		if err := ipv4.NewPacketConn(conn).SetMulticastInterface(c.cfg.Interface); err != nil {
			packageLogger.Warn("SSDP discovery multicast interface bind failed (falling back to OS default)",
				"interface", c.cfg.Interface.Name,
				"err", err.Error())
		}
	}

	runCtx, runCancel := context.WithCancel(parent)
	c.runCtx = runCtx
	c.runCancel = runCancel

	// Add(2) here (under runMu, before the spawns) pairs with the
	// deferred Done() at the top of each loop. Start + Stop serialize
	// on runMu and Stop's Wait() runs AFTER it releases runMu, so this
	// Add never races a concurrent Wait.
	c.wg.Add(2)
	go c.runLoop(runCtx)
	go c.runTickLoop(runCtx)

	packageLogger.Info("DLNA renderer discovery started",
		"interface", c.cfg.Interface.Name,
		"msearchInterval", c.cfg.MSearchInterval,
		"rendererTTL", c.cfg.RendererTTL)
	return nil
}

// Stop cancels the run loop, closes the socket, and clears the
// cache. Idempotent.
func (c *SSDPDiscoveryClient) Stop() {
	c.runMu.Lock()
	if c.runCancel == nil {
		c.runMu.Unlock()
		return
	}
	c.runCancel()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.runMu.Unlock()
	// Wait for runLoop, runTickLoop, and any in-flight detail fetches to
	// exit BEFORE clearing the cache — otherwise a fetch that already
	// passed its runCtx.Err() guard could Upsert into the cleared cache.
	// runCancel() above cancels every fetch's derived context, so this is
	// bounded by in-flight HTTP cancellation latency, not DetailFetchTimeout.
	// Wait() runs OUTSIDE runMu (load-bearing): runLoop/sendMSearch take
	// runMu.RLock via snapshotConn on their way out, so holding the lock
	// here would deadlock.
	c.wg.Wait()
	// Clear runCancel/runCtx only AFTER Wait() returns. Keeping runCancel
	// non-nil across the Wait window is load-bearing: it makes a concurrent
	// Start() fail its `runCancel != nil` guard and refuse — rather than
	// slipping past it to call wg.Add(2) while this Wait is in progress,
	// which panics ("WaitGroup misuse: Add called concurrently with Wait").
	// Re-acquire runMu for the writes (snapshotConn readers take RLock during
	// the Wait). (Gemini HIGH + CodeRabbit CRITICAL, r3 round 1.)
	c.runMu.Lock()
	c.runCancel = nil
	c.runCtx = nil
	// Clear the cache UNDER runMu (not after the unlock): otherwise a concurrent
	// Start() could pass its `runCancel != nil` guard in the gap, spin up fresh
	// loops that Upsert, and this stale Clear() would wipe the new client's
	// cache. Clear takes cache.mu (not runMu) so there's no lock-order hazard,
	// and all loops have already exited (wg.Wait above). (Twin of the Gemini
	// HIGH fix on PR #469's MediaServerDiscoveryClient — kept identical.)
	c.cache.Clear()
	c.runMu.Unlock()
	packageLogger.Info("DLNA renderer discovery stopped")
}

// snapshotConn returns the live UDP connection under RLock. Returns
// nil when the client is stopped. Per Gemini CRITICAL round-1 on
// PR #305 — the prior shape read `c.conn` without synchronization
// + risked a nil deref after Stop cleared it.
func (c *SSDPDiscoveryClient) snapshotConn() *net.UDPConn {
	c.runMu.RLock()
	defer c.runMu.RUnlock()
	return c.conn
}

// runLoop reads SSDP packets from the UDP socket + dispatches each
// into the per-NTS handler. Exits when ctx is cancelled OR the
// socket is closed by Stop.
func (c *SSDPDiscoveryClient) runLoop(ctx context.Context) {
	defer c.wg.Done()
	buf := make([]byte, 4096) // SSDP packets are always <2KB in practice
	consecutiveReadErrs := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn := c.snapshotConn()
		if conn == nil {
			return // Stop closed the socket
		}
		// Short read deadline so the loop wakes periodically to
		// re-check ctx.Done() — without it, a parent-ctx cancel that
		// doesn't go through Stop() (which closes the socket) could
		// park us inside ReadFromUDP indefinitely on a quiet network.
		//
		// Wall-clock (time.Now), NOT c.nowFunc(): net.Conn deadlines are
		// always evaluated against the real OS clock, so feeding the
		// injectable logical clock here would instant-timeout every read
		// under a test that pins nowFunc to a past date. nowFunc stays
		// for the TTL/staleness domain (EvictStale), where logical time
		// is the right reference.
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if HandleReadErr(ctx, err, &consecutiveReadErrs, packageLogger) {
				return
			}
			continue
		}
		consecutiveReadErrs = 0
		packet := make([]byte, n)
		copy(packet, buf[:n])
		c.handlePacket(ctx, packet, src)
	}
}

// runTickLoop drives the M-SEARCH cadence + the staleness eviction
// pass. Fires immediately on entry so a fresh start doesn't wait
// the full interval before the first M-SEARCH.
func (c *SSDPDiscoveryClient) runTickLoop(ctx context.Context) {
	defer c.wg.Done()
	// Initial M-SEARCH + eviction pass.
	c.sendMSearch()
	c.evictStaleEntries()
	ticker := time.NewTicker(c.cfg.MSearchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sendMSearch()
			c.evictStaleEntries()
		}
	}
}

// evictStaleEntries runs the periodic cache eviction pass AND prunes the
// client-side bookkeeping that shadows it. Both tick sites call this so the
// two can't drift apart — a cache entry that ages out must not leave its
// recorded Location behind.
func (c *SSDPDiscoveryClient) evictStaleEntries() {
	c.cache.EvictStale(c.nowFunc(), c.cfg.RendererTTL)
	c.pruneLocations()
}

// pruneLocations drops lastLocations entries for UDNs that are neither cached
// nor mid-fetch. Without it the map retains one entry per distinct UDN ever
// observed: ssdp:byebye is the only other removal path, and this client is
// M-SEARCH-only in production (see the lifecycle docblock) so byebye is
// rarely received. A buggy or spoofed LAN source announcing many distinct
// UDNs would otherwise grow it without bound. (Per-UDN growth is bounded
// separately by maxTrackedLocations + the RendererTTL freshness window.)
//
// The in-flight half of the predicate is load-bearing: a fetch records its
// Location just BEFORE it writes the cache entry, so between those two calls
// a UDN legitimately has a recorded Location and no cache entry. A tick
// landing in that window would otherwise drop the reference the very next
// announcement needs.
//
// Lock order is locMu → cache.mu (via Get). Safe: RendererCache never calls
// back into the client, and handlePacket's cache.Get / locationMoved calls
// are sequential, never nested — so the inverse order doesn't exist
// anywhere. Holding locMu across the lookups is also what makes the decision
// race-free: a concurrent fetch can neither record nor release mid-prune.
func (c *SSDPDiscoveryClient) pruneLocations() {
	c.locMu.Lock()
	defer c.locMu.Unlock()
	for udn := range c.lastLocations {
		if _, busy := c.inFlight[udn]; busy {
			continue
		}
		if _, cached := c.cache.Get(udn); cached {
			continue
		}
		delete(c.lastLocations, udn)
	}
}

// sendMSearch broadcasts an M-SEARCH request for MediaRenderer
// devices. Renderer responses come back on the same socket as
// unicast HTTP responses + are handled in runLoop.
func (c *SSDPDiscoveryClient) sendMSearch() {
	conn := c.snapshotConn()
	if conn == nil {
		return
	}
	target := "urn:schemas-upnp-org:device:MediaRenderer:1"
	packet := buildMSearchRequest(target)
	dst := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	if _, err := conn.WriteToUDP(packet, dst); err != nil {
		packageLogger.Warn("M-SEARCH send failed", "err", err.Error())
	}
}

// buildMSearchRequest assembles the SSDP M-SEARCH packet. MX=3
// gives renderers 3s to spread their response (per the spec — they
// add a random delay 0..MX to avoid response storms).
func buildMSearchRequest(searchTarget string) []byte {
	return []byte("M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		`MAN: "ssdp:discover"` + "\r\n" +
		"MX: 3\r\n" +
		"ST: " + searchTarget + "\r\n" +
		"USER-AGENT: 1-bit-bridge/discovery UPnP/1.0\r\n" +
		"\r\n")
}

// handlePacket dispatches a single received UDP packet. `ctx` is
// the run-loop's context, plumbed through so background detail
// fetches get cancelled cleanly on Stop. Per Gemini HIGH round-1
// on PR #305 — the prior shape used `context.Background()` for
// the detail fetches, leaving in-flight fetches alive past Stop
// and re-populating the cache after it was cleared.
func (c *SSDPDiscoveryClient) handlePacket(
	ctx context.Context,
	packet []byte,
	_ *net.UDPAddr,
) {
	hdr, err := ParseSSDPHeaders(packet)
	if err != nil {
		// Malformed packet on a multicast socket — likely a
		// non-SSDP broadcast we incidentally received. Quiet drop.
		return
	}

	// We only care about MediaRenderer announcements. Filter by
	// the ST (M-SEARCH response) or NT (NOTIFY) header.
	target := hdr.ST
	if target == "" {
		target = hdr.NT
	}
	if target != "urn:schemas-upnp-org:device:MediaRenderer:1" {
		return
	}

	udn := UDNFromUSN(hdr.USN)
	if udn == "" {
		return
	}

	// NTS=ssdp:byebye → explicit departure; remove from cache.
	// (v1 doesn't listen for spontaneous NOTIFY broadcasts on the
	// multicast port — see lifecycle docblock — but a renderer
	// COULD theoretically include NTS on an M-SEARCH response in
	// some odd firmware, so we still handle the case defensively.)
	if hdr.NTS == "ssdp:byebye" {
		c.cache.Remove(udn)
		c.forgetLocation(udn)
		packageLogger.Debug("renderer byebye", "udn", udn)
		return
	}

	// Everything else (M-SEARCH response without NTS, OR NOTIFY
	// ssdp:alive) refreshes the cache. New UDNs trigger a detail
	// fetch; known UDNs just refresh lastSeenAt.
	now := c.nowFunc()
	if existing, exists := c.cache.Get(udn); exists {
		// Known UDN announcing from a host we do NOT believe is live for it
		// (DHCP lease renew, Wi-Fi ↔ Ethernet move). The cached ControlURL
		// points at the old address, and because LastSeenAt keeps advancing
		// on every packet EvictStale never ages the entry out — iOS would
		// dispatch SetAVTransportURI to a dead address until the bridge
		// restarts. So re-fetch the description at the new address.
		//
		// The re-fetch happens IN PLACE: the cached entry keeps serving
		// /v1/renderers while it runs. The old shape Removed the entry
		// first, which left the renderer missing from the output picker
		// between the Remove and the fetch's write — and Snapshot hides
		// ControlURL-less stubs, so a failed fetch widened that hole to a
		// whole M-SEARCH cycle. Combined with a single remembered Location,
		// a UDN that answers from two addresses re-triggered on every packet
		// and blinked out of the picker at the M-SEARCH cadence forever.
		//
		// What made the pre-emptive Remove load-bearing was
		// mergeRendererInfo's non-empty-wins merge: a failed re-fetch
		// upserting a ControlURL-less stub merged into the live entry,
		// KEEPING the dead ControlURL while refreshing LastSeenAt — pinning
		// the bad entry forever (immortally, with the year-2999 structural
		// sentinel). fetchAndCacheDetails now REPLACES rather than merges,
		// which enforces that invariant at the write instead of by
		// pre-deleting, so a failed re-fetch still leaves a genuine stub:
		// hidden from Snapshot, aged out + retried by EvictStale, and with a
		// structural sentinel earned at the NEW address rather than the old.
		//
		// A same-UDN fetch already in flight blocks the dispatch below; the
		// NEXT announcement re-detects the change with the slot free —
		// self-healing within one M-SEARCH cycle.
		if c.locationMoved(udn, hdr.Location, existing.ControlURL, now) {
			packageLogger.Debug("renderer moved; re-fetching description",
				"udn", udn, "from", existing.ControlURL, "to", hdr.Location)
			c.spawnDetailFetch(ctx, udn, hdr.Location, now)
			return
		}
		// Incomplete stub (no AVTransport ControlURL) = residue of a
		// failed detail fetch. Do NOT refresh its LastSeenAt and do NOT
		// re-fetch here: a transient-failure stub then ages out via
		// EvictStale and is re-discovered as new on a later cycle (which
		// retries the fetch); a structural-failure stub carries the
		// far-future LastSeenAt sentinel so it never ages out (no retry
		// storm). Both stay hidden from /v1/renderers via Snapshot's
		// ControlURL gate. (Gemini consult — bridge-12: pre-fix this branch
		// refreshed LastSeenAt forever, so a stub never aged out and never
		// retried → the renderer was stuck nameless until restart.)
		if existing.ControlURL == "" {
			return
		}
		c.cache.Upsert(RendererInfo{
			UDN:        udn,
			LastSeenAt: now,
		})
		return
	}

	// First-time UDN: kick a detail fetch. Bounded via the
	// semaphore so the run loop's NOTIFY storm during a LAN-wide
	// event can't fanout unbounded TCP connections.
	if hdr.Location == "" {
		return // no location → can't fetch description; skip
	}
	c.spawnDetailFetch(ctx, udn, hdr.Location, now)
}

// sameURLHost reports whether two URLs share the same host:port.
// Unparseable input compares as "same" so a malformed SSDP Location
// can't trigger description re-fetch storms against a healthy entry.
//
// Deliberate lockstep COPY of the identically-named helper in
// internal/upnp/discovery.go (the MediaServer twin). Importing across the
// two discovery subsystems for an 8-line helper would couple packages that
// are otherwise independent by design — the same call already made for
// tls.ParseHostFromURL / advertise.parseHostFromURL. Keep the two in sync;
// the unparseable→true semantic is the load-bearing half.
func sameURLHost(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return true
	}
	return ua.Host == ub.Host
}

// locationMoved reports whether loc announces a host this client does NOT
// currently believe is live for udn — the signal that the renderer actually
// moved, rather than simply answering from a second address it has been
// answering from all along.
//
// A match REFRESHES that record's freshness, and that is what makes a
// dual-homed renderer settle: both of its addresses stay live for as long as
// both keep announcing, so neither reads as a move and no re-fetch is
// dispatched. An address that stops announcing ages out after RendererTTL, so
// a renderer that really does move away and later returns IS re-fetched.
//
// Returns false when there is nothing to compare against — no live record and
// no cached ControlURL to fall back on (same host as the description Location
// in every real device description). Callers MUST treat that as "no change"
// rather than guessing: a false positive here dispatches a re-fetch storm.
func (c *SSDPDiscoveryClient) locationMoved(udn, loc, cachedControlURL string, now time.Time) bool {
	if loc == "" {
		return false
	}
	c.locMu.Lock()
	defer c.locMu.Unlock()
	recs, anyFresh := c.dropStaleLocationsLocked(udn, now)
	// Nothing observed within RendererTTL: prefer the cached ControlURL — the
	// address we are actually DRIVING — over the last-known record, which
	// dropStaleLocationsLocked retains only so a ControlURL-less stub keeps
	// some reference at all. Without this preference a renderer that had
	// announced from two addresses, went quiet, and came back at the one we
	// are NOT driving would read as "already known" and keep the entry
	// pointing at an address it has left.
	if !anyFresh && cachedControlURL != "" {
		return !sameURLHost(loc, cachedControlURL)
	}
	if len(recs) == 0 {
		return cachedControlURL != "" && !sameURLHost(loc, cachedControlURL)
	}
	for i := range recs {
		if sameURLHost(loc, recs[i].url) {
			// recs shares its backing array with the map's slice, so this
			// writes through — the record is live, keep it live.
			recs[i].seen = now
			return false
		}
	}
	return true
}

// recordLocation stamps the Location a fetch resolved against, so the next
// announcement for udn has a reference to compare hosts with even when the
// fetch produced a ControlURL-less stub.
func (c *SSDPDiscoveryClient) recordLocation(udn, location string) {
	c.noteLocation(udn, location, c.nowFunc())
}

// noteLocation records (or refreshes) location among udn's live addresses.
// A location whose HOST already appears is refreshed in place rather than
// appended, so the set holds one record per distinct address.
func (c *SSDPDiscoveryClient) noteLocation(udn, location string, now time.Time) {
	if udn == "" || location == "" {
		return
	}
	c.locMu.Lock()
	defer c.locMu.Unlock()
	recs, _ := c.dropStaleLocationsLocked(udn, now)
	for i := range recs {
		if sameURLHost(location, recs[i].url) {
			recs[i].url = location
			recs[i].seen = now
			c.lastLocations[udn] = recs
			return
		}
	}
	recs = append(recs, locationRecord{url: location, seen: now})
	if len(recs) > maxTrackedLocations {
		// One record was just added, so dropping one restores the cap.
		i := oldestLocationIndex(recs)
		recs = append(recs[:i], recs[i+1:]...)
	}
	c.lastLocations[udn] = recs
}

// dropStaleLocationsLocked removes udn's records that haven't been observed
// within RendererTTL, returning what survives and whether ANY record was
// fresh. Caller MUST hold locMu.
//
// It never empties a known UDN's set: when every record is stale the single
// most-recently-observed one is retained (and anyFresh is false, so callers
// can tell a live set from that floor). The floor restores the old "remember
// one Location, forever" behaviour and is load-bearing for a ControlURL-less
// stub — the recorded Location is then the ONLY reference a later
// announcement can be compared against, and a STRUCTURAL stub never ages out
// of the cache on its own (year-2999 sentinel), so dropping the reference
// would make it immortal. That is the case
// TestHandlePacket_StructuralStubRecoversAfterHostChange guards, extended
// into the time domain: a renderer that failed structurally, went quiet for
// longer than RendererTTL, and came back at a new address must still be
// recognised as moved.
//
// Never creates a map entry for an unknown udn — pruneLocations is the sole
// whole-UDN reaper and this must not resurrect one behind its back.
func (c *SSDPDiscoveryClient) dropStaleLocationsLocked(udn string, now time.Time) (kept []locationRecord, anyFresh bool) {
	recs, ok := c.lastLocations[udn]
	if !ok || len(recs) == 0 {
		return nil, false
	}
	// Captured by VALUE before the in-place filter below overwrites elements.
	newest := recs[0]
	for _, r := range recs[1:] {
		if r.seen.After(newest.seen) {
			newest = r
		}
	}
	kept = recs[:0] // filter in place; writes only ever land at or behind the read index
	for _, r := range recs {
		if IsStaleRenderer(r.seen, now, c.cfg.RendererTTL) {
			continue
		}
		kept = append(kept, r)
	}
	anyFresh = len(kept) > 0
	if !anyFresh {
		kept = append(kept, newest)
	}
	c.lastLocations[udn] = kept
	return kept, anyFresh
}

// oldestLocationIndex returns the index of the least-recently-observed
// record. Callers guarantee a non-empty slice.
func oldestLocationIndex(recs []locationRecord) int {
	oldest := 0
	for i := 1; i < len(recs); i++ {
		if recs[i].seen.Before(recs[oldest].seen) {
			oldest = i
		}
	}
	return oldest
}

// forgetLocation drops udn's recorded Location — called when ssdp:byebye
// removes the renderer, so the map tracks live devices only.
//
// Deliberately does NOT drop an inFlight claim: that claim is owned by the
// running fetch's defer (so it can't leak), and clearing it here would let
// a packet arriving mid-fetch spawn a duplicate.
func (c *SSDPDiscoveryClient) forgetLocation(udn string) {
	c.locMu.Lock()
	delete(c.lastLocations, udn)
	c.locMu.Unlock()
}

// claimFetch reserves the single in-flight fetch slot for udn. Returns
// false when a fetch is already running, in which case the caller MUST NOT
// spawn (and MUST NOT release).
func (c *SSDPDiscoveryClient) claimFetch(udn string) bool {
	c.locMu.Lock()
	defer c.locMu.Unlock()
	if _, busy := c.inFlight[udn]; busy {
		return false
	}
	c.inFlight[udn] = struct{}{}
	return true
}

// releaseFetch frees udn's in-flight slot. Called from the spawned fetch's
// defer — see fetchAndCacheDetails for the ordering contract.
func (c *SSDPDiscoveryClient) releaseFetch(udn string) {
	c.locMu.Lock()
	delete(c.inFlight, udn)
	c.locMu.Unlock()
}

// spawnDetailFetch launches a tracked detail fetch for udn, unless one is
// already in flight for that UDN (see the inFlight field docblock).
//
// wg.Add(1) here (not inside fetchAndCacheDetails) is safe to run
// concurrently with Stop()'s wg.Wait(): in production handlePacket runs ON
// the runLoop goroutine, which holds its OWN wg slot for its entire
// lifetime, so the counter is always ≥1 here — this Add takes it ≥1→≥2,
// never 0→1 (the only shape that panics under a concurrent Wait). And
// Stop()'s Wait can't return until runLoop returns, by which time no
// further fetch Adds are issued. Mirrors the Add-under-live-parent pattern
// in internal/dlna/ssdp.go's runMSearchListener.
func (c *SSDPDiscoveryClient) spawnDetailFetch(
	ctx context.Context,
	udn, location string,
	now time.Time,
) {
	if !c.claimFetch(udn) {
		return
	}
	c.wg.Add(1)
	go c.fetchAndCacheDetails(ctx, udn, location, now)
}

// fetchAndCacheDetails dispatches the description + GetProtocolInfo
// fetches for a newly-discovered renderer + populates the cache.
// Bounded via the semaphore so the run loop's NOTIFY storm during a
// LAN-wide event (everyone powering up at once) can't fanout
// unbounded TCP connections.
//
// `runCtx` is the discovery client's run-loop context — when Stop
// cancels it, in-flight detail fetches return early without
// touching the cache. Per Gemini HIGH round-1 on PR #305.
func (c *SSDPDiscoveryClient) fetchAndCacheDetails(
	runCtx context.Context,
	udn, location string, lastSeenAt time.Time,
) {
	// Paired with the wg.Add(1) in spawnDetailFetch. Deferred at the very
	// top so it fires on EVERY return path (including the semaphore-acquire
	// ctx.Done bail below), letting Stop()'s Wait() observe completion.
	defer c.wg.Done()
	// Paired with the claimFetch in spawnDetailFetch. Registered AFTER the
	// wg.Done defer so LIFO runs it FIRST: once Stop()'s wg.Wait() returns,
	// every claim is guaranteed released (a lingering one would make a
	// restarted client skip that UDN's first fetch).
	defer c.releaseFetch(udn)
	// Acquire / release the semaphore — bounded concurrency.
	select {
	case c.detailFetchSem <- struct{}{}:
	case <-runCtx.Done():
		return
	}
	defer func() { <-c.detailFetchSem }()

	// Re-check cancellation post-acquire — a long queue ahead of
	// us could mean ctx was cancelled while we waited.
	if runCtx.Err() != nil {
		return
	}

	ctx, cancel := context.WithTimeout(runCtx, c.cfg.DetailFetchTimeout)
	defer cancel()
	desc, err := FetchDeviceDescription(ctx, c.dispatcher, location)
	if err != nil {
		// Stop cancelled us — drop the stub-write too so the
		// cache stays clean.
		if runCtx.Err() != nil {
			return
		}
		// Classify the failure. A STRUCTURAL failure (4xx / unparseable
		// description / no AVTransport — see errStructuralDescription)
		// can't be fixed by re-fetching, so stamp the stub with the
		// far-future sentinel → EvictStale never ages it out → the
		// exists-branch never retries it (no storm). A TRANSIENT failure
		// (timeout / dial / 5xx) keeps the real fail-time LastSeenAt → the
		// stub ages out + is re-discovered + retried on a later cycle.
		// Both stubs stay hidden from /v1/renderers (no ControlURL).
		// (Gemini consult — bridge-12.)
		structural := errors.Is(err, errStructuralDescription)
		stubLastSeen := lastSeenAt
		if structural {
			stubLastSeen = structuralStubLastSeen
		}
		packageLogger.Debug("device description fetch failed",
			"udn", udn,
			"location", location,
			"structural", structural,
			"err", err.Error())
		// Cache a stub (UDN + lastSeenAt only) so duplicate packets in
		// THIS M-SEARCH cycle hit the exists-branch and don't re-fetch.
		// Record the attempted Location too — the stub carries no
		// ControlURL, so this map is the ONLY way a later announcement
		// from a new address can be recognised as a move (which matters
		// most for a structural stub: it never ages out on its own).
		//
		// Replace, NOT Upsert: on a re-fetch of an entry that is still
		// cached with a live ControlURL, merging this stub would keep that
		// (now unreachable) URL and refresh LastSeenAt, pinning an
		// undrivable renderer forever. See RendererCache.Replace.
		c.recordLocation(udn, location)
		c.cache.Replace(RendererInfo{UDN: udn, LastSeenAt: stubLastSeen})
		return
	}

	avSvc := desc.Services[ServiceAVTransport]
	cmSvc := desc.Services[ServiceConnectionManager]

	// GetProtocolInfo is optional — a renderer without
	// ConnectionManager skips this step (its Sink list is unknown;
	// iOS falls back to RendererProfile defaults).
	var sinks []string
	if cmSvc.ControlURL != "" {
		piCtx, piCancel := context.WithTimeout(runCtx, c.cfg.DetailFetchTimeout)
		piSinks, piErr := FetchGetProtocolInfo(piCtx, c.dispatcher, cmSvc.ControlURL)
		piCancel()
		if piErr != nil {
			packageLogger.Debug("GetProtocolInfo fetch failed",
				"udn", udn,
				"err", piErr.Error())
		} else {
			sinks = piSinks
		}
	}

	// Final cancellation check before mutating the cache — Stop
	// landing mid-fetch must NOT re-populate the cleared cache.
	if runCtx.Err() != nil {
		return
	}

	// Replace, NOT Upsert — a completed fetch is the whole truth about this
	// renderer, so nothing from the pre-move entry (a stale
	// RenderingControlURL at the old address, say) may survive by merge.
	c.recordLocation(udn, location)
	c.cache.Replace(RendererInfo{
		UDN:                 udn,
		FriendlyName:        desc.FriendlyName,
		Manufacturer:        desc.Manufacturer,
		ModelDescription:    desc.ModelDescription,
		ModelName:           desc.ModelName,
		ControlURL:          avSvc.ControlURL,
		EventURL:            avSvc.EventSubURL,
		RenderingControlURL: desc.Services[ServiceRenderingControl].ControlURL,
		SinkProtocolInfos:   sinks,
		LastSeenAt:          lastSeenAt,
	})
	packageLogger.Info("renderer discovered",
		"udn", udn,
		"friendlyName", desc.FriendlyName,
		"sinkCount", len(sinks))
}

// Cache returns the underlying renderer cache for read access by
// the HTTP handler. Exposed so the cache + the client can be
// constructed independently AND the same cache passed to both
// `NewSSDPDiscoveryClient` AND the api.Server.
func (c *SSDPDiscoveryClient) Cache() *RendererCache { return c.cache }
