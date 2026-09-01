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

	// DescriptionURL is the SSDP LOCATION the description was fetched
	// from (e.g. http://192.168.0.62:8200/rootDesc.xml). Kept because
	// it is the ONE string a client needs to add this server on its
	// own, and it is not derivable from anything else here: the path
	// is vendor-specific (MiniDLNA /rootDesc.xml, others
	// /description.xml or /dd.xml), so it cannot be reconstructed from
	// the control URL's host:port. /v1/health advertises it on LAN
	// bridges so a phone whose own SSDP failed can still add the
	// upstream directly.
	DescriptionURL string

	// DeviceUDN is the device's OWN UDN as its description reports it.
	//
	// It is usually the same as UDN above, but NOT for a manual-URL
	// server: there, UDN carries the ingest's StableServerKey
	// (`manual:<sha256(url)>`) because routing rows, telemetry, LiveHost
	// and the online chip all key on that string, while this field keeps
	// the real identity for display and for the duplicate-configuration
	// check. Empty when the description carried none.
	DeviceUDN string

	LastSeenAt time.Time
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
		if info.DescriptionURL == "" {
			info.DescriptionURL = existing.DescriptionURL
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

	// locMu guards lastLocation. A DEDICATED mutex, not runMu: runMu's
	// scope is the conn / runCtx lifecycle and Stop() holds it across
	// cache.Clear(), so borrowing it for per-packet bookkeeping would
	// widen a lock whose ordering contract is already load-bearing.
	// Lock order is runMu → locMu → cache.mu; nothing takes them the
	// other way round.
	locMu sync.Mutex

	// lastLocation maps UDN → the SSDP `Location` the last SUCCESSFUL
	// detail fetch resolved against. It is the host-change reference for
	// handlePacket.
	//
	// The cached ContentDirectoryControlURL is NOT a substitute:
	// ParseDeviceDescription resolves <controlURL> through
	// base.ResolveReference, which returns an ABSOLUTE control URL
	// verbatim — so a device that advertises its control endpoint on a
	// different host:port than its description endpoint compared as
	// "moved" on EVERY announcement. That branch re-fetches and returns,
	// so the entry's LastSeenAt advanced only when the description fetch
	// succeeded: a description endpoint flaky for longer than ServerTTL
	// evicted a live, M-SEARCH-answering server, after which
	// ResolveControlURL returns "" and every play of its tracks 503s.
	// Steady-state it also meant one description GET per announcement,
	// forever.
	//
	// Deliberately a client-side map rather than a ServerInfo field: that
	// struct is the shape the admin/API surfaces render, and this is
	// discovery bookkeeping they have no business carrying. Mirrors the
	// sibling renderer client's lastLocation — the previous-Location
	// tracking ONLY; that client's Remove-then-refetch + stub semantics
	// are deliberately NOT copied (the server twin re-fetches in place,
	// and a stub-clobber there would pin a dead controlURL forever).
	lastLocation map[string]string
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
			Client: &http.Client{
				Timeout: cfg.DetailFetchTimeout,
				// Relay 3xx verbatim instead of following it. The fetch
				// URL comes from an SSDP-advertised Location header (a
				// LAN device, possibly rogue or spoofed), so
				// auto-following a redirect to loopback or a link-local
				// metadata address would turn the bridge into an SSRF
				// probe against its own no-auth admin API. Mirrors
				// internal/upnpproxy's CheckRedirect guard.
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
	return &MediaServerDiscoveryClient{
		cfg:            cfg,
		cache:          cache,
		dispatcher:     cfg.Dispatcher,
		nowFunc:        nowFunc,
		detailFetchSem: make(chan struct{}, 2),
		lastLocation:   make(map[string]string),
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
	// The location map shadows the cache, so it must not outlive it: a
	// restarted client comparing a fresh announcement against a pre-Stop
	// address would mis-classify it. Lock order runMu → locMu holds.
	c.locMu.Lock()
	c.lastLocation = make(map[string]string)
	c.locMu.Unlock()
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
	consecutiveReadErrs := 0
	for {
		if ctx.Err() != nil {
			return
		}
		conn := c.snapshotConn()
		if conn == nil {
			return
		}
		// Short read deadline so an idle socket still hears ctx cancel.
		// Wall-clock (time.Now), NOT c.nowFunc(): net.Conn deadlines are
		// always evaluated against the real OS clock, so feeding the
		// injectable logical clock here would instant-timeout every read
		// under a test that pins nowFunc to a past date — HandleReadErr
		// treats a timeout as retry-with-no-backoff, so the loop would
		// busy-spin a core and read no packets (discovery silently dead).
		// nowFunc stays for the TTL/staleness domain (EvictStale below),
		// where logical time is the right reference. Mirrors the sibling
		// renderer client at internal/dlna/discovery/client.go.
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			// Shared policy (timeout→retry / shutdown→return / transient→
			// log+backoff+retry, escalating on a sustained streak) — kept in
			// one place so this client and the renderer client can't drift.
			if discovery.HandleReadErr(ctx, err, &consecutiveReadErrs, logger) {
				return
			}
			continue
		}
		consecutiveReadErrs = 0
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
			// Prune the shadow bookkeeping in the SAME step as the cache
			// eviction so the two can't drift — an entry that ages out
			// must not leave its recorded Location behind.
			c.cache.EvictStale(c.nowFunc(), c.cfg.ServerTTL)
			c.pruneLocations()
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
		c.forgetLocation(udn)
		return
	}
	now := c.nowFunc()
	if existing, exists := c.cache.Get(udn); exists {
		// The device just announced itself, so it is alive: refresh
		// LastSeenAt on EVERY announcement, INCLUDING the moved-host
		// branch below. Pre-fix that branch returned without refreshing,
		// so LastSeenAt advanced only when a description fetch succeeded
		// and a flaky description endpoint got a live server evicted
		// (ResolveControlURL then returns "" → 503 on every play).
		// Upsert merges: only UDN + LastSeenAt are set here, so the
		// cached descriptive fields and controlURL survive untouched.
		c.cache.Upsert(ServerInfo{UDN: udn, LastSeenAt: now})
		// Known UDN announcing from a NEW host:port (DHCP renew, Wi-Fi ↔
		// Ethernet move): the cached controlURL points at the old address,
		// so without a re-fetch it would stay dead forever (TTL eviction
		// never fires while the server keeps answering M-SEARCH). The
		// fetch upserts the fresh controlURL on success; on failure the
		// old entry stays and the NEXT announcement re-detects the move
		// and retries, because lastLocation is only stamped by a
		// SUCCESSFUL fetch.
		//
		// Compare Location-to-Location, NOT Location-to-controlURL: see
		// the lastLocation field docblock. prev == "" means we have no
		// recorded reference and the cached controlURL is absent too —
		// treat as "no change" rather than guessing, since a false
		// positive here is a re-fetch storm.
		if hdr.Location != "" {
			if prev := c.previousLocation(udn, existing.ContentDirectoryControlURL); prev != "" &&
				!sameURLHost(hdr.Location, prev) {
				logger.Debug("upstream server moved; re-fetching description",
					"udn", udn, "from", prev, "to", hdr.Location)
				c.spawnDetailFetch(ctx, udn, hdr.Location, now)
			}
		}
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

// previousLocation returns the SSDP Location this client last fetched for
// udn, falling back to the cached controlURL when no fetch has been
// recorded — e.g. an entry seeded by a caller other than the fetch path, or
// one carried over from before this process recorded anything. That
// fallback is the pre-fix approximation and is only ever reached when
// there is nothing better; see the lastLocation docblock for why it is not
// good enough on its own. Returns "" when neither is known, which callers
// MUST treat as "no change" rather than guessing.
func (c *MediaServerDiscoveryClient) previousLocation(udn, cachedControlURL string) string {
	c.locMu.Lock()
	loc := c.lastLocation[udn]
	c.locMu.Unlock()
	if loc != "" {
		return loc
	}
	return cachedControlURL
}

// recordLocation stamps the Location a fetch resolved against, so the next
// announcement for udn has a same-kind reference to compare hosts with.
//
// Called ONLY after a fetch has successfully upserted the entry. Recording
// a FAILED (or merely attempted) fetch would silence the move detector for
// a server that genuinely moved but whose description endpoint is down: the
// next announcement would compare equal, no retry would be dispatched, and
// the cache would keep its dead controlURL indefinitely.
func (c *MediaServerDiscoveryClient) recordLocation(udn, location string) {
	if udn == "" || location == "" {
		return
	}
	c.locMu.Lock()
	c.lastLocation[udn] = location
	c.locMu.Unlock()
}

// forgetLocation drops udn's recorded Location — called when ssdp:byebye
// removes the server, so the map tracks live devices only.
func (c *MediaServerDiscoveryClient) forgetLocation(udn string) {
	c.locMu.Lock()
	delete(c.lastLocation, udn)
	c.locMu.Unlock()
}

// pruneLocations drops recorded Locations for UDNs no longer in the cache.
// Without it the map retains one entry per distinct UDN ever fetched:
// ssdp:byebye is the only other removal path and this client listens on an
// ephemeral unicast port (M-SEARCH responses only), so byebye is rarely
// received. A buggy or spoofed LAN source announcing many distinct UDNs
// would otherwise grow it without bound.
//
// Safe against an in-flight first fetch because recordLocation runs AFTER
// that fetch's cache.Upsert — so a recorded UDN is always a cached one
// unless it has since been evicted, and the move path re-fetches IN PLACE
// (the entry is never removed for the duration of a fetch).
//
// Lock order is locMu → cache.mu (via Get); ServerCache never calls back
// into the client, so the inverse order does not exist.
func (c *MediaServerDiscoveryClient) pruneLocations() {
	c.locMu.Lock()
	defer c.locMu.Unlock()
	for udn := range c.lastLocation {
		if _, cached := c.cache.Get(udn); !cached {
			delete(c.lastLocation, udn)
		}
	}
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
		DescriptionURL:             location,
		LastSeenAt:                 lastSeenAt,
	})
	// Stamp AFTER the Upsert, so a recorded UDN is always a cached one and
	// pruneLocations can use "not in cache" as its sole predicate.
	c.recordLocation(udn, location)
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
