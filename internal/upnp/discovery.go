package upnp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
	"golang.org/x/net/ipv4"
)

// MediaServerDeviceType is the SSDP search target for UPnP MediaServers
// (the sibling of internal/dlna/discovery's MediaRenderer:1 target).
const MediaServerDeviceType = "urn:schemas-upnp-org:device:MediaServer:1"

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
	// 2026-05-27 incident). Soft-fail.
	if c.cfg.Interface != nil {
		_ = ipv4.NewPacketConn(conn).SetMulticastInterface(c.cfg.Interface)
	}

	runCtx, runCancel := context.WithCancel(parent)
	c.runCtx = runCtx
	c.runCancel = runCancel

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
	c.runCancel = nil
	c.runCtx = nil
	c.runMu.Unlock()
	c.cache.Clear()
}

func (c *MediaServerDiscoveryClient) snapshotConn() *net.UDPConn {
	c.runMu.RLock()
	defer c.runMu.RUnlock()
	return c.conn
}

// runLoop reads SSDP packets until ctx is cancelled / the conn is closed.
func (c *MediaServerDiscoveryClient) runLoop(ctx context.Context) {
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
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			// Read deadline / Stop'd socket — both expected.
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				continue
			}
			return
		}
		c.handlePacket(ctx, append([]byte(nil), buf[:n]...), addr)
	}
}

// runTickLoop sends M-SEARCH on the configured cadence and evicts stale
// entries each tick.
func (c *MediaServerDiscoveryClient) runTickLoop(ctx context.Context) {
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
	if _, exists := c.cache.Get(udn); exists {
		c.cache.Upsert(ServerInfo{UDN: udn, LastSeenAt: now})
		return
	}
	if hdr.Location == "" {
		return
	}
	go c.fetchAndCacheDetails(ctx, udn, hdr.Location, now)
}

// fetchAndCacheDetails downloads + parses the device description for a
// newly-discovered UDN, extracts the ContentDirectory controlURL, and
// caches it. Bounded by a semaphore so a NOTIFY storm can't fan out
// unbounded TCP connections.
func (c *MediaServerDiscoveryClient) fetchAndCacheDetails(runCtx context.Context, udn, location string, lastSeenAt time.Time) {
	select {
	case c.detailFetchSem <- struct{}{}:
	case <-runCtx.Done():
		return
	}
	defer func() { <-c.detailFetchSem }()

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
