package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
	"golang.org/x/net/ipv4"
)

// packageLogger follows the repo convention — internal/* packages
// declare a single package-scoped `logging.Component(...)` logger.
// Per CodeRabbit Major round-1 on PR #305.
var packageLogger = logging.Component("dlna-discovery")

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
			Client: &http.Client{Timeout: cfg.DetailFetchTimeout},
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
	c.runCancel = nil
	c.runCtx = nil
	c.runMu.Unlock()
	c.cache.Clear()
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
	buf := make([]byte, 4096) // SSDP packets are always <2KB in practice
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
		// re-check ctx.Done() — without it, a quiet network could
		// park us inside ReadFromUDP indefinitely after Stop()
		// closed the socket, and we'd see the close error rather
		// than a clean ctx-cancel exit.
		_ = conn.SetReadDeadline(c.nowFunc().Add(500 * time.Millisecond))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			// Read timeout is the expected loop tick; anything
			// else after a cancel is a closed-socket signal.
			var nErr net.Error
			if errors.As(err, &nErr) && nErr.Timeout() {
				continue
			}
			// On Stop the read returns "use of closed connection"
			// — quiet exit, not an error log.
			select {
			case <-ctx.Done():
				return
			default:
				packageLogger.Warn("SSDP read error", "err", err.Error())
				return
			}
		}
		packet := make([]byte, n)
		copy(packet, buf[:n])
		c.handlePacket(ctx, packet, src)
	}
}

// runTickLoop drives the M-SEARCH cadence + the staleness eviction
// pass. Fires immediately on entry so a fresh start doesn't wait
// the full interval before the first M-SEARCH.
func (c *SSDPDiscoveryClient) runTickLoop(ctx context.Context) {
	// Initial M-SEARCH + eviction pass.
	c.sendMSearch()
	c.cache.EvictStale(c.nowFunc(), c.cfg.RendererTTL)
	ticker := time.NewTicker(c.cfg.MSearchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sendMSearch()
			c.cache.EvictStale(c.nowFunc(), c.cfg.RendererTTL)
		}
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
		packageLogger.Debug("renderer byebye", "udn", udn)
		return
	}

	// Everything else (M-SEARCH response without NTS, OR NOTIFY
	// ssdp:alive) refreshes the cache. New UDNs trigger a detail
	// fetch; known UDNs just refresh lastSeenAt.
	now := c.nowFunc()
	if _, exists := c.cache.Get(udn); exists {
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
	go c.fetchAndCacheDetails(ctx, udn, hdr.Location, now)
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
		packageLogger.Debug("device description fetch failed",
			"udn", udn,
			"location", location,
			"err", err.Error())
		// Cache a stub entry with just the UDN + lastSeenAt so
		// the next M-SEARCH cycle's "exists?" check fires the
		// refresh path (no second detail-fetch storm).
		c.cache.Upsert(RendererInfo{UDN: udn, LastSeenAt: lastSeenAt})
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

	c.cache.Upsert(RendererInfo{
		UDN:               udn,
		FriendlyName:      desc.FriendlyName,
		Manufacturer:      desc.Manufacturer,
		ModelDescription:  desc.ModelDescription,
		ModelName:         desc.ModelName,
		ControlURL:        avSvc.ControlURL,
		EventURL:          avSvc.EventSubURL,
		SinkProtocolInfos: sinks,
		LastSeenAt:        lastSeenAt,
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
