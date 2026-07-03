package upnp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"golang.org/x/net/ipv4"
)

var logger = logging.Component("upnp")

// MediaServerDeviceType is the SSDP search target for UPnP MediaServers
// (the sibling of internal/dlna/discovery's MediaRenderer:1 target).
const MediaServerDeviceType = "urn:schemas-upnp-org:device:MediaServer:1"

// ssdpReadErrBackoff paces the read loop after an unexpected (non-timeout,
// non-shutdown) UDP read error so a persistently-broken socket can't
// hot-spin a CPU core. On Windows an unconnected UDP socket can surface
// WSAECONNRESET on the read that follows a send whose datagram drew an ICMP
// port-unreachable (SIO_UDP_CONNRESET) — a transient one-shot we recover
// from rather than letting it kill discovery for the process lifetime.
const ssdpReadErrBackoff = 250 * time.Millisecond

// ContentDirectoryServiceTypePrefix is the prefix used to find the CDS
// entry inside a parsed device description's Services map (the version
// suffix differs across MiniDLNA / upmpdcli / MinimServer — match by
// prefix to tolerate both ":1" and any future ":4" / ":5").
const ContentDirectoryServiceTypePrefix = "urn:schemas-upnp-org:service:ContentDirectory"

// ServerInfo describes one discovered upstream MediaServer. Identity is
// the UDN; ContentDirectoryControlURL is what the ContentDirectoryClient
// dials. LastSeenAt drives staleness eviction (matches the renderer side).
type ServerInfo struct {
	UDN                        string
	FriendlyName               string
	Manufacturer               string
	ModelDescription           string
	ModelName                  string
	ContentDirectoryControlURL string
	LastSeenAt                 time.Time
}

// ServerCache is a small thread-safe UDN -> ServerInfo map. Parallel
// shape to internal/dlna/discovery's RendererCache but kept separate to
// avoid intertwining the renderer-output hot path with the source path.
type ServerCache struct {
	mu      sync.RWMutex
	servers map[string]ServerInfo
}

func NewServerCache() *ServerCache {
	return &ServerCache{servers: make(map[string]ServerInfo)}
}

// Upsert inserts or updates. When merging an alive-refresh that carries
// only UDN + LastSeenAt, the cached descriptive fields (FriendlyName,
// controlURL, ...) are preserved.
func (c *ServerCache) Upsert(info ServerInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.servers[info.UDN]; ok {
		if info.FriendlyName == "" {
			info.FriendlyName = existing.FriendlyName
		}
		if info.Manufacturer == "" {
			info.Manufacturer = existing.Manufacturer
		}
		if info.ModelDescription == "" {
			info.ModelDescription = existing.ModelDescription
		}
		if info.ModelName == "" {
			info.ModelName = existing.ModelName
		}
		if info.ContentDirectoryControlURL == "" {
			info.ContentDirectoryControlURL = existing.ContentDirectoryControlURL
		}
		if info.LastSeenAt.IsZero() {
			info.LastSeenAt = existing.LastSeenAt
		}
	}
	c.servers[info.UDN] = info
}

func (c *ServerCache) Remove(udn string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.servers, udn)
}

// Get returns the cached ServerInfo and a found flag.
func (c *ServerCache) Get(udn string) (ServerInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.servers[udn]
	return v, ok
}

// Snapshot returns a copy of the cache for read-only iteration.
func (c *ServerCache) Snapshot() []ServerInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ServerInfo, 0, len(c.servers))
	for _, v := range c.servers {
		out = append(out, v)
	}
	return out
}

// EvictStale removes entries whose LastSeenAt is older than now-ttl.
// Returns the number evicted. Mirrors the renderer cache's contract so
// the run loop can run a single tick handler over both surfaces if a
// future PR consolidates them.
func (c *ServerCache) EvictStale(now time.Time, ttl time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ttl <= 0 {
		return 0
	}
	cutoff := now.Add(-ttl)
	n := 0
	for udn, info := range c.servers {
		if info.LastSeenAt.Before(cutoff) {
			delete(c.servers, udn)
			n++
		}
	}
	return n
}

func (c *ServerCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.servers)
}

func (c *ServerCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.servers = make(map[string]ServerInfo)
}

// DiscoveryConfig configures the upstream MediaServer SSDP client.
// Mirrors the shape of internal/dlna/discovery.DiscoveryConfig — the
// two clients have the same lifecycle structure (M-SEARCH tick +
// receive loop + staleness eviction) but listen for a different ST.
type DiscoveryConfig struct {
	// Interface is the LAN-eligible network interface to write
	// M-SEARCH out on. Required (no default — multi-homed hosts MUST
	// pick deliberately; see the renderer-side rationale around the
	// Windows + Tailscale 2026-05-27 incident).
	Interface *net.Interface

	// MSearchInterval is the M-SEARCH cadence. Default 60s — twice
	// the renderer side's 30s because library servers come and go at
	// a much slower rate (a media server lives on a NAS / always-on
	// box; a renderer is a phone/speaker that powers down).
	MSearchInterval time.Duration

	// ServerTTL is the staleness window. Default 180s — at 60s
	// M-SEARCH cadence that gives ~3 missed cycles of grace before
	// eviction. Stricter than the renderer side: a server that's
	// momentarily unresponsive shouldn't churn the ingest layer.
	ServerTTL time.Duration

	// DetailFetchTimeout caps a single per-server detail fetch.
	// Default 5s.
	DetailFetchTimeout time.Duration

	// Dispatcher backs the description fetch. Defaults to the
	// discovery package's HTTPClientDispatcher.
	Dispatcher discovery.SOAPDispatcher

	// NowFunc returns the current time. Defaults to time.Now (tests
	// inject a fixed clock).
	NowFunc func() time.Time
}

const (
	DefaultMediaServerMSearchInterval = 60 * time.Second
	DefaultMediaServerTTL             = 180 * time.Second
	defaultMediaServerDetailTimeout   = 5 * time.Second
)

// DefaultDiscoveryConfig returns a config seeded with the doc'd
// defaults. Caller MUST set Interface before passing to
// NewMediaServerDiscoveryClient.
func DefaultDiscoveryConfig() DiscoveryConfig {
	return DiscoveryConfig{
		MSearchInterval:    DefaultMediaServerMSearchInterval,
		ServerTTL:          DefaultMediaServerTTL,
		DetailFetchTimeout: defaultMediaServerDetailTimeout,
	}
}

// MediaServerDiscoveryClient drives SSDP M-SEARCH for upstream UPnP
// MediaServers, populates a ServerCache, and evicts stale entries.
// Identical lifecycle shape to internal/dlna/discovery.SSDPDiscoveryClient
// — listen on a wildcard UDP socket, send M-SEARCH on tick, receive
// unicast responses + NOTIFY ssdp:alive/byebye, fetch description.xml
// the first time we see a UDN.
type MediaServerDiscoveryClient struct {
	cfg            DiscoveryConfig
	cache          *ServerCache
	dispatcher     discovery.SOAPDispatcher
	nowFunc        func() time.Time
	detailFetchSem chan struct{}

	runMu     sync.RWMutex
	conn      *net.UDPConn
	runCtx    context.Context //nolint:containedctx // discovery client lifecycle
	runCancel context.CancelFunc

	// wg tracks the two run loops (runLoop, runTickLoop) AND every
	// in-flight detail fetch so Stop() can block until the cache can no
	// longer be mutated BEFORE it calls cache.Clear(). Without it, a fetch
	// that already passed its runCtx.Err() guard could Upsert into the
	// freshly-cleared cache (the Stop()/Upsert race). Mirrors the sibling
	// internal/dlna/discovery.SSDPDiscoveryClient invariant.
	wg sync.WaitGroup
}

// NewMediaServerDiscoveryClient validates the config and returns a
// client in the stopped state.
func NewMediaServerDiscoveryClient(cfg DiscoveryConfig, cache *ServerCache) (*MediaServerDiscoveryClient, error) {
	if cfg.Interface == nil {
		return nil, errors.New("upnp/discovery: Interface is required")
	}
	if cache == nil {
		return nil, errors.New("upnp/discovery: ServerCache is required")
	}
	if cfg.MSearchInterval <= 0 {
		cfg.MSearchInterval = DefaultMediaServerMSearchInterval
	}
	if cfg.ServerTTL <= 0 {
		cfg.ServerTTL = DefaultMediaServerTTL
	}
	if cfg.DetailFetchTimeout <= 0 {
		cfg.DetailFetchTimeout = defaultMediaServerDetailTimeout
	}
	if cfg.Dispatcher == nil {
		cfg.Dispatcher = &discovery.HTTPClientDispatcher{
			Client: &http.Client{Timeout: cfg.DetailFetchTimeout},
		}
	}
	nowFunc := cfg.NowFunc
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &MediaServerDiscoveryClient{
		cfg:            cfg,
		cache:          cache,
		dispatcher:     cfg.Dispatcher,
		nowFunc:        nowFunc,
		detailFetchSem: make(chan struct{}, 2),
	}, nil
}

// Cache exposes the underlying cache (e.g. for /v1/servers or the ingest
// scheduler).
func (c *MediaServerDiscoveryClient) Cache() *ServerCache { return c.cache }

// Start binds the SSDP socket and spawns the read + tick loops. Caller
// MUST call Stop before discarding.
func (c *MediaServerDiscoveryClient) Start(parent context.Context) error {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	if c.runCancel != nil {
		return errors.New("upnp/discovery: already started")
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return fmt.Errorf("upnp/discovery: ListenUDP: %w", err)
	}
	c.conn = conn
	// Pin outgoing M-SEARCH multicast to the operator-chosen interface
	// — same rationale as internal/dlna/discovery (Windows + Tailscale
	// 2026-05-27 incident). Soft-fail: a failure here means multicast goes
	// via the OS default (degraded, not broken), so we log + continue
	// rather than refuse to start. Matches internal/dlna/ssdp.go's Warn.
	if c.cfg.Interface != nil {
		if err := ipv4.NewPacketConn(conn).SetMulticastInterface(c.cfg.Interface); err != nil {
			logger.Warn("SSDP multicast interface bind failed (falling back to OS default)",
				"interface", c.cfg.Interface.Name, "err", err.Error())
		}
	}

	runCtx, runCancel := context.WithCancel(parent)
	c.runCtx = runCtx
	c.runCancel = runCancel

	// Add(2) under runMu, before the spawns, pairs with the deferred Done()
	// at the top of each loop. Start + Stop serialize on runMu and Stop's
	// Wait() runs AFTER it releases runMu, so this Add never races Wait.
	c.wg.Add(2)
	go c.runLoop(runCtx)
	go c.runTickLoop(runCtx)
	return nil
}

// Stop cancels the loops, closes the socket, and clears the cache.
// Idempotent.
func (c *MediaServerDiscoveryClient) Stop() {
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
	// exit BEFORE clearing the cache — otherwise a fetch that already passed
	// its runCtx.Err() guard could Upsert into the cleared cache. runCancel()
	// above cancels every fetch's derived context, so this is bounded by
	// in-flight cancellation latency, not DetailFetchTimeout. Wait() runs
	// OUTSIDE runMu (load-bearing): runLoop/sendMSearch take runMu.RLock via
	// snapshotConn on their way out, so holding the lock here would deadlock.
	c.wg.Wait()
	// Clear runCancel/runCtx only AFTER Wait() returns. Keeping runCancel
	// non-nil across the Wait window is load-bearing: it makes a concurrent
	// Start() fail its `runCancel != nil` guard instead of slipping past to
	// call wg.Add(2) while this Wait is in progress (which panics with
	// "WaitGroup misuse: Add called concurrently with Wait").
	c.runMu.Lock()
	c.runCancel = nil
	c.runCtx = nil
	// Clear the cache UNDER runMu: if it ran after the unlock, a concurrent
	// Start() could pass its `runCancel != nil` guard in the gap, spin up fresh
	// loops that Upsert, and then this stale Clear() would wipe the new
	// client's cache. Clear takes cache.mu (not runMu) so there's no lock-order
	// hazard, and all loops have already exited (wg.Wait above) so no reader is
	// blocked on runMu here. (Gemini HIGH on PR #469.)
	c.cache.Clear()
	c.runMu.Unlock()
}

func (c *MediaServerDiscoveryClient) snapshotConn() *net.UDPConn {
	c.runMu.RLock()
	defer c.runMu.RUnlock()
	return c.conn
}

// runLoop reads SSDP packets until ctx is cancelled / the conn is closed.
func (c *MediaServerDiscoveryClient) runLoop(ctx context.Context) {
	defer c.wg.Done()
	buf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return
		}
		conn := c.snapshotConn()
		if conn == nil {
			return
		}
		// Short read deadline so an idle socket still hears ctx cancel.
		// Route through nowFunc so tests with an injected clock can
		// reason about timeouts deterministically (matches
		// internal/dlna/discovery's convention).
		_ = conn.SetReadDeadline(c.nowFunc().Add(2 * time.Second))
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			// Deadline tick — the normal idle path.
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				continue
			}
			// Shutdown — Stop closed the socket / cancelled ctx.
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			// Unexpected transient (e.g. Windows WSAECONNRESET after an ICMP
			// port-unreachable): don't kill discovery for the process
			// lifetime — log + ctx-aware backoff + retry. The backoff caps a
			// persistently-broken socket at ~4 reads/sec instead of hot-spin.
			logger.Warn("SSDP read error; backing off", "err", err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(ssdpReadErrBackoff):
			}
			continue
		}
		c.handlePacket(ctx, append([]byte(nil), buf[:n]...), addr)
	}
}

// runTickLoop sends M-SEARCH on the configured cadence and evicts stale
// entries each tick.
func (c *MediaServerDiscoveryClient) runTickLoop(ctx context.Context) {
	defer c.wg.Done()
	// Fire one immediately so the cache populates without waiting.
	c.sendMSearch()
	t := time.NewTicker(c.cfg.MSearchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sendMSearch()
			c.cache.EvictStale(c.nowFunc(), c.cfg.ServerTTL)
		}
	}
}

func (c *MediaServerDiscoveryClient) sendMSearch() {
	conn := c.snapshotConn()
	if conn == nil {
		return
	}
	packet := buildMSearchPacket(MediaServerDeviceType)
	dst := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	_, _ = conn.WriteToUDP(packet, dst)
}

// buildMSearchPacket assembles an SSDP M-SEARCH for the given target.
// MX=3 gives servers up to 3s of random spread to avoid response storms.
func buildMSearchPacket(searchTarget string) []byte {
	return []byte("M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		`MAN: "ssdp:discover"` + "\r\n" +
		"MX: 3\r\n" +
		"ST: " + searchTarget + "\r\n" +
		"USER-AGENT: 1-bit-bridge/upnp UPnP/1.0\r\n" +
		"\r\n")
}

// handlePacket dispatches one SSDP packet — filtering by ST/NT to
// MediaServer:1 so the renderer-side multicast traffic doesn't pollute
// the server cache.
func (c *MediaServerDiscoveryClient) handlePacket(ctx context.Context, packet []byte, _ *net.UDPAddr) {
	hdr, err := discovery.ParseSSDPHeaders(packet)
	if err != nil {
		return
	}
	target := hdr.ST
	if target == "" {
		target = hdr.NT
	}
	if target != MediaServerDeviceType {
		return
	}
	udn := discovery.UDNFromUSN(hdr.USN)
	if udn == "" {
		return
	}
	if hdr.NTS == "ssdp:byebye" {
		c.cache.Remove(udn)
		return
	}
	now := c.nowFunc()
	if existing, exists := c.cache.Get(udn); exists {
		// Known UDN announcing from a NEW host:port (DHCP renew, Wi-Fi
		// ↔ Ethernet move): the cached controlURL points at the old
		// address, so the bare LastSeenAt refresh would keep a dead URL
		// alive forever (TTL eviction never fires while the server keeps
		// answering M-SEARCH). Re-fetch the description instead — the
		// fetch upserts the fresh controlURL + LastSeenAt on success;
		// on failure the entry ages out via the staleness window, which
		// is correct for a server that moved somewhere unreachable.
		if hdr.Location != "" &&
			!sameURLHost(hdr.Location, existing.ContentDirectoryControlURL) {
			c.spawnDetailFetch(ctx, udn, hdr.Location, now)
			return
		}
		c.cache.Upsert(ServerInfo{UDN: udn, LastSeenAt: now})
		return
	}
	if hdr.Location == "" {
		return
	}
	c.spawnDetailFetch(ctx, udn, hdr.Location, now)
}

// sameURLHost reports whether two URLs share the same host:port.
// Unparseable input compares as "same" so a malformed SSDP Location
// can't trigger description re-fetch storms against a healthy entry.
func sameURLHost(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return true
	}
	return ua.Host == ub.Host
}

// spawnDetailFetch tracks the fetch in the WaitGroup before launching it.
// wg.Add(1) here (not inside fetchAndCacheDetails) is safe vs Stop()'s
// wg.Wait(): handlePacket runs ON the runLoop goroutine, which holds its own
// wg slot for its entire lifetime, so this Add takes the counter ≥1→≥2, never
// 0→1 (the only shape that panics under a concurrent Wait). Stop()'s Wait
// can't return until runLoop returns, by which time no further fetch Adds are
// issued.
func (c *MediaServerDiscoveryClient) spawnDetailFetch(ctx context.Context, udn, location string, now time.Time) {
	c.wg.Add(1)
	go c.fetchAndCacheDetails(ctx, udn, location, now)
}

// fetchAndCacheDetails downloads + parses the device description for a
// newly-discovered UDN, extracts the ContentDirectory controlURL, and
// caches it. Bounded by a semaphore so a NOTIFY storm can't fan out
// unbounded TCP connections.
func (c *MediaServerDiscoveryClient) fetchAndCacheDetails(runCtx context.Context, udn, location string, lastSeenAt time.Time) {
	// Paired with the wg.Add(1) in spawnDetailFetch. Deferred at the very top
	// so it fires on EVERY return path (including the semaphore-acquire
	// ctx.Done bail below), letting Stop()'s Wait() observe completion.
	defer c.wg.Done()
	select {
	case c.detailFetchSem <- struct{}{}:
	case <-runCtx.Done():
		return
	}
	defer func() { <-c.detailFetchSem }()

	// Re-check cancellation post-acquire — a long queue ahead of us could
	// mean ctx was cancelled while we waited. Stop()'s wg.Wait() is the
	// primary guarantee that no Upsert lands after cache.Clear(); this is
	// defense-in-depth so a cancelled fetch doesn't do pointless work.
	if runCtx.Err() != nil {
		return
	}

	fetchCtx, cancel := context.WithTimeout(runCtx, c.cfg.DetailFetchTimeout)
	defer cancel()
	desc, err := discovery.FetchDeviceDescription(fetchCtx, c.dispatcher, location)
	// FetchDeviceDescription returns a "no AVTransport service" error
	// for non-renderer devices (which is exactly what MediaServers
	// are) — but it ALSO populates desc.Services with everything it
	// did find. So we tolerate that specific error and just verify
	// that ContentDirectory is present.
	if err != nil && len(desc.Services) == 0 {
		return
	}
	ctrlURL := lookupContentDirectoryControlURL(desc.Services)
	if ctrlURL == "" {
		// Device advertises MediaServer:1 in SSDP but its description
		// carries no ContentDirectory — silently skip (the entry will
		// expire via the staleness window).
		return
	}
	c.cache.Upsert(ServerInfo{
		UDN:                        udn,
		FriendlyName:               desc.FriendlyName,
		Manufacturer:               desc.Manufacturer,
		ModelDescription:           desc.ModelDescription,
		ModelName:                  desc.ModelName,
		ContentDirectoryControlURL: ctrlURL,
		LastSeenAt:                 lastSeenAt,
	})
}

// lookupContentDirectoryControlURL walks the parsed service map for the
// first ContentDirectory:N entry. Prefix-match so :1 and any future :2
// both resolve.
func lookupContentDirectoryControlURL(services map[string]discovery.ServiceURLs) string {
	for stype, urls := range services {
		if strings.HasPrefix(stype, ContentDirectoryServiceTypePrefix) {
			return urls.ControlURL
		}
	}
	return ""
}
