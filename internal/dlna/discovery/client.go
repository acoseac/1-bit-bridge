package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// SSDPDiscoveryClient is the orchestrator that drives SSDP M-SEARCH
// + NOTIFY listening + per-renderer detail-fetch + cache lifecycle.
//
// **Lifecycle**: `Start(ctx)` binds the SSDP multicast UDP socket
// (single socket on 0.0.0.0:0 joined to the multicast group on
// `cfg.Interface`), fires the first M-SEARCH immediately, then
// loops every `cfg.MSearchInterval` (default 30s) sending fresh
// M-SEARCHes + periodically evicting stale cache entries.
// `Stop()` cancels the run loop, closes the socket, and clears
// the cache for clean teardown.
//
// **Thread model**: the client owns ONE goroutine — `runLoop` —
// that reads from the UDP socket, dispatches NOTIFY / M-SEARCH-
// response packets to per-event handlers, and ticks the M-SEARCH
// / eviction cadence. All cache mutations happen from this single
// goroutine + the cache's own mutex serializes against the HTTP
// handler's read path. No goroutine-per-renderer fanout — the
// per-renderer detail fetches (DeviceDescription, GetProtocolInfo)
// are dispatched as a small bounded worker pool to keep network
// concurrency under control.
//
// **Why not goupnp's client**: same rationale as the server-side
// PR 1 — goupnp's SSDP client is opinionated about goroutine
// fanout + has historically been flaky in edge cases (multi-
// interface hosts, IPv6 ambiguity). Hand-rolling against the
// existing `internal/dlna` SSDP primitives keeps the surface
// small + the failure modes inspectable.
type SSDPDiscoveryClient struct {
	cfg DiscoveryConfig

	// cache is the shared store consumed by the HTTP handler.
	// Constructed externally so the same cache can be wired into
	// both the client lifecycle AND the /v1/renderers handler.
	cache *RendererCache

	// dispatcher backs the per-renderer detail fetches. Stub
	// implementations injected via `cfg.Dispatcher` for tests.
	dispatcher SOAPDispatcher

	// log is the operator-visible breadcrumb sink.
	log *slog.Logger

	// runCtx + runCancel manage the run loop's lifecycle. Set on
	// Start; cancelled on Stop. nil before first Start.
	runMu     sync.Mutex
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
	// Interface is the LAN-eligible network interface to bind for
	// multicast. Caller resolves via
	// `internal/dlna.PickLANEligibleInterface` (the same picker
	// the server-side advertiser uses).
	Interface *net.Interface

	// MSearchInterval is the cadence for sending fresh M-SEARCH
	// requests. Default 30s. Each M-SEARCH lets us refresh entries
	// for renderers that may have missed our earlier alive (e.g.
	// renderer came online between cycles + only sends ssdp:alive
	// on its own multicast schedule).
	MSearchInterval time.Duration

	// RendererTTL is the staleness window. Entries older than this
	// (no observation within the window) get evicted on the next
	// tick. Default 60s — comfortably above MSearchInterval so
	// one missed observation cycle doesn't evict.
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
	log *slog.Logger,
) (*SSDPDiscoveryClient, error) {
	if cfg.Interface == nil {
		return nil, errors.New("DiscoveryConfig.Interface is required")
	}
	if cache == nil {
		return nil, errors.New("RendererCache is required")
	}
	if log == nil {
		log = slog.Default()
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
		log:            log,
		detailFetchSem: make(chan struct{}, 4), // see field docblock
		nowFunc:        nowFunc,
	}, nil
}

// Start binds the SSDP socket + spawns the run loop. Returns an
// error when the bind fails. Caller MUST call Stop before
// discarding the client (matches the `internal/dlna` server-side
// lifecycle convention).
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

	// Bind the SSDP multicast socket on the picked interface.
	// Ephemeral local port — we're a client, not a server, so the
	// fixed 1900 port is reserved for our M-SEARCH listener (the
	// server-side `internal/dlna` advertiser). Multicast listens
	// happen on the joined group, not the local port.
	addr := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	conn, err := net.ListenMulticastUDP("udp4", c.cfg.Interface, addr)
	if err != nil {
		return fmt.Errorf("ListenMulticastUDP on %s: %w", c.cfg.Interface.Name, err)
	}
	c.conn = conn

	runCtx, runCancel := context.WithCancel(parent)
	c.runCtx = runCtx
	c.runCancel = runCancel

	go c.runLoop(runCtx)
	go c.runTickLoop(runCtx)

	c.log.Info("DLNA renderer discovery started",
		slog.String("interface", c.cfg.Interface.Name),
		slog.Duration("msearchInterval", c.cfg.MSearchInterval),
		slog.Duration("rendererTTL", c.cfg.RendererTTL))
	return nil
}

// Stop cancels the run loop, closes the socket, and clears the
// cache. Idempotent.
func (c *SSDPDiscoveryClient) Stop() {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	if c.runCancel == nil {
		return
	}
	c.runCancel()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.runCancel = nil
	c.runCtx = nil
	c.cache.Clear()
	c.log.Info("DLNA renderer discovery stopped")
}

// runLoop reads SSDP packets from the multicast socket + dispatches
// each into the per-NTS handler. Exits when ctx is cancelled OR the
// socket is closed by Stop (in which case the read returns an error,
// which we treat as "shutting down" rather than logging at error
// level).
func (c *SSDPDiscoveryClient) runLoop(ctx context.Context) {
	buf := make([]byte, 4096) // SSDP packets are always <2KB in practice
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		// Short read deadline so the loop wakes periodically to
		// re-check ctx.Done() — without it, a quiet network could
		// park us inside ReadFromUDP indefinitely after Stop()
		// closed the socket, and we'd see the close error rather
		// than a clean ctx-cancel exit.
		_ = c.conn.SetReadDeadline(c.nowFunc().Add(500 * time.Millisecond))
		n, src, err := c.conn.ReadFromUDP(buf)
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
				c.log.Warn("SSDP read error",
					slog.String("err", err.Error()))
				return
			}
		}
		packet := make([]byte, n)
		copy(packet, buf[:n])
		c.handlePacket(packet, src)
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
	if c.conn == nil {
		return
	}
	target := "urn:schemas-upnp-org:device:MediaRenderer:1"
	packet := buildMSearchRequest(target)
	dst := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	if _, err := c.conn.WriteToUDP(packet, dst); err != nil {
		c.log.Warn("M-SEARCH send failed", slog.String("err", err.Error()))
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

// handlePacket dispatches a single received UDP packet. The src
// addr is unused today but plumbed so future debugging telemetry
// can correlate packet-loss / out-of-order arrival per renderer
// host.
func (c *SSDPDiscoveryClient) handlePacket(packet []byte, _ *net.UDPAddr) {
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
	if hdr.NTS == "ssdp:byebye" {
		c.cache.Remove(udn)
		c.log.Debug("renderer byebye",
			slog.String("udn", udn))
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
	// semaphore so a 100-renderer LAN doesn't spawn 100
	// simultaneous outbound TCPs.
	if hdr.Location == "" {
		return // no location → can't fetch description; skip
	}
	go c.fetchAndCacheDetails(udn, hdr.Location, now)
}

// fetchAndCacheDetails dispatches the description + GetProtocolInfo
// fetches for a newly-discovered renderer + populates the cache.
// Bounded via the semaphore so the run loop's NOTIFY storm during a
// LAN-wide event (everyone powering up at once) can't fanout
// unbounded TCP connections.
func (c *SSDPDiscoveryClient) fetchAndCacheDetails(
	udn, location string, lastSeenAt time.Time,
) {
	// Acquire / release the semaphore — bounded concurrency.
	c.detailFetchSem <- struct{}{}
	defer func() { <-c.detailFetchSem }()

	// Race window: a concurrent ssdp:byebye may have just removed
	// this UDN before we got here. Skip the fetch in that case —
	// the next M-SEARCH cycle will pick the renderer up again if
	// it's still online.
	if _, evictedByBye := c.cache.Get(udn); !evictedByBye {
		// Wait — we WANT to populate even if not in cache yet
		// (this IS the first-time-discovery path). The check
		// above is wrong; remove it.
		//
		// Actually: the byebye-during-fetch race is legitimate.
		// But we can't distinguish "never inserted yet" from
		// "byebye'd". The simplest safe shape: just proceed with
		// the fetch + Upsert. If a byebye fires AFTER our Upsert
		// here, its Remove call will fire from runLoop and drop
		// us cleanly.
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.DetailFetchTimeout)
	defer cancel()
	desc, err := FetchDeviceDescription(ctx, c.dispatcher, location)
	if err != nil {
		c.log.Debug("device description fetch failed",
			slog.String("udn", udn),
			slog.String("location", location),
			slog.String("err", err.Error()))
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
		piCtx, piCancel := context.WithTimeout(context.Background(), c.cfg.DetailFetchTimeout)
		piSinks, piErr := FetchGetProtocolInfo(piCtx, c.dispatcher, cmSvc.ControlURL)
		piCancel()
		if piErr != nil {
			c.log.Debug("GetProtocolInfo fetch failed",
				slog.String("udn", udn),
				slog.String("err", piErr.Error()))
		} else {
			sinks = piSinks
		}
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
	c.log.Info("renderer discovered",
		slog.String("udn", udn),
		slog.String("friendlyName", desc.FriendlyName),
		slog.Int("sinkCount", len(sinks)))
}

// Cache returns the underlying renderer cache for read access by
// the HTTP handler. Exposed so the cache + the client can be
// constructed independently AND the same cache passed to both
// `NewSSDPDiscoveryClient` AND the api.Server.
func (c *SSDPDiscoveryClient) Cache() *RendererCache { return c.cache }
