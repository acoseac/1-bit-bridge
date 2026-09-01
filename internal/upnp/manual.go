package upnp

// Manual-URL MediaServer resolution.
//
// The config has accepted `upnpUpstream.servers[].manualDescriptionURL`
// since PR #351 — validated at load, hashed into a StableServerKey — and
// the ingest refused it at runtime with "not yet supported". It is the
// only escape hatch for a network where the bridge's SSDP cannot reach
// the server (multicast filtered by the AP, the server on another
// subnet, iOS Local Network permission irrelevant because it is the
// BRIDGE that cannot see it), and it looked supported while doing
// nothing.
//
// **The design falls out of one observation: all three surfaces that need
// a manual server read the same ServerCache.** ResolveControlURL looks the
// cache up, api's LiveHost derives its host:port from the cached control
// URL, and the admin/health online chip reads the cache. So ONE insertion
// point makes all three work, rather than three parallel implementations
// that can disagree.
//
// **The cache entry is keyed under the ingest's StableServerKey** — the
// `manual:<sha256(url)>` form — NOT the device's real UDN. Routing rows,
// per-server telemetry, LiveHost and the status chip all key on that
// string; PR #807 already had to carry both spellings for exactly this
// reason. The real UDN is kept as a display field.

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
)

// ManualServer is one configured manual-URL upstream.
type ManualServer struct {
	// Key is the ingest's StableServerKey for this server — the string
	// every other subsystem uses to refer to it. The cache entry is
	// stored under this, not under the device's own UDN.
	Key string
	// DescriptionURL is the operator-configured device-description URL.
	DescriptionURL string
	// Name is the operator's label, used only when the description
	// carries no friendlyName.
	Name string
}

// ManualPoller refreshes cache entries for manual-URL servers.
//
// It must run on the SAME cadence as the SSDP TTL, because EvictStale
// reaps entries the poller stops refreshing — and that eviction is
// CORRECT: a manual URL that stops answering should show as offline, and
// getting that for free is why the poller writes into the shared cache
// rather than a parallel map.
type ManualPoller struct {
	cache      *ServerCache
	dispatcher discovery.SOAPDispatcher
	servers    func() []ManualServer
	// knownUDNs reports the UDNs of SSDP-configured servers, so a device
	// configured BOTH ways is not walked twice under two routing keys.
	knownUDNs func() map[string]struct{}
	interval  time.Duration
	timeout   time.Duration
	log       *slog.Logger

	mu      sync.Mutex
	warned  map[string]struct{} // dedup for the duplicate-config warning
	nowFunc func() time.Time
}

// ManualPollerConfig configures a ManualPoller. Servers and KnownUDNs are
// closures so a config reload is picked up without reconstructing.
type ManualPollerConfig struct {
	Cache      *ServerCache
	Dispatcher discovery.SOAPDispatcher
	Servers    func() []ManualServer
	KnownUDNs  func() map[string]struct{}
	Interval   time.Duration
	Timeout    time.Duration
	Logger     *slog.Logger
}

// NewManualPoller constructs a poller. Returns nil when there is nothing
// to poll, so the caller can skip spawning a goroutine entirely.
func NewManualPoller(cfg ManualPollerConfig) *ManualPoller {
	if cfg.Cache == nil || cfg.Servers == nil {
		return nil
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultMediaServerDetailTimeout
	}
	if cfg.Dispatcher == nil {
		// Same guarded client the SSDP detail fetch uses, built here so
		// the SSRF guard lives in one place rather than at each wiring
		// site. The URL is operator-configured and therefore more
		// trusted than an SSDP Location header — but "more trusted" is
		// not "trusted", and an operator can paste a URL that redirects.
		cfg.Dispatcher = &discovery.HTTPClientDispatcher{
			Client: &http.Client{
				Timeout: cfg.Timeout,
				// Relay 3xx verbatim rather than following it: an
				// auto-followed redirect to loopback or a link-local
				// metadata address would turn the bridge into an SSRF
				// probe against its own no-auth admin API. Mirrors
				// NewMediaServerDiscoveryClient and internal/upnpproxy.
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			},
		}
	}
	if cfg.Logger == nil {
		cfg.Logger = logger
	}
	if cfg.KnownUDNs == nil {
		cfg.KnownUDNs = func() map[string]struct{} { return nil }
	}
	return &ManualPoller{
		cache: cfg.Cache, dispatcher: cfg.Dispatcher,
		servers: cfg.Servers, knownUDNs: cfg.KnownUDNs,
		interval: cfg.Interval, timeout: cfg.Timeout, log: cfg.Logger,
		warned: make(map[string]struct{}), nowFunc: time.Now,
	}
}

// Run polls until ctx is done. One immediate pass, then on the interval.
func (p *ManualPoller) Run(ctx context.Context) {
	if p == nil {
		return
	}
	p.PollOnce(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.PollOnce(ctx)
		}
	}
}

// PollOnce refreshes every configured manual server once. Failures are
// logged at Debug and leave the cache entry to age out — which is how an
// unreachable manual server comes to report offline.
func (p *ManualPoller) PollOnce(ctx context.Context) {
	if p == nil {
		return
	}
	known := p.knownUDNs()
	for _, srv := range p.servers() {
		if ctx.Err() != nil {
			return
		}
		p.pollServer(ctx, srv, known)
	}
}

func (p *ManualPoller) pollServer(ctx context.Context, srv ManualServer, knownUDNs map[string]struct{}) {
	url := strings.TrimSpace(srv.DescriptionURL)
	if url == "" || srv.Key == "" {
		return
	}
	fetchCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	desc, err := discovery.FetchDeviceDescription(fetchCtx, p.dispatcher, url)
	// FetchDeviceDescription returns a "no AVTransport service" error for
	// any non-renderer device — which every MediaServer is — while still
	// populating desc.Services. Tolerate that specific shape and let the
	// ContentDirectory lookup below be the real verdict. Same allowance
	// the SSDP path makes.
	if err != nil && len(desc.Services) == 0 {
		p.log.Debug("UPnP manual server: description fetch failed",
			slog.String("server", srv.Name), slog.String("url", url),
			slog.String("err", err.Error()))
		return
	}
	ctrlURL := lookupContentDirectoryControlURL(desc.Services)
	if ctrlURL == "" {
		p.log.Debug("UPnP manual server: description carries no ContentDirectory service",
			slog.String("server", srv.Name), slog.String("url", url))
		return
	}

	// A device reachable BOTH by SSDP and by manual URL would otherwise
	// land in the cache twice — under its real UDN and under manual:<sha>
	// — and the ingest would walk it twice under two routing prefixes,
	// producing duplicate rows for one upstream. Refuse the manual entry
	// and say so once.
	//
	// UNLESS the UDN is this entry's OWN. A server configured with both a
	// UDN and a manual URL has StableServerKey == its lowercased UDN, so
	// the description it returns necessarily "matches a configured UDN" —
	// itself. Rejecting there would make the manual URL useless to the
	// operator who supplied both as a belt-and-braces, which is precisely
	// the case where SSDP is unreliable and the fallback is wanted. There
	// is no double-walk risk: the ingest walks per CONFIGURED SERVER,
	// keyed on StableServerKey, so one entry is walked once however many
	// ways its description was obtained.
	if realUDN := strings.ToLower(strings.TrimSpace(desc.UDN)); realUDN != "" && realUDN != strings.ToLower(srv.Key) {
		if _, dup := knownUDNs[realUDN]; dup {
			p.warnOnce(srv.Key, func() {
				p.log.Warn("UPnP manual server: this device is already configured by UDN — "+
					"ignoring the manual URL so it is not walked twice",
					slog.String("server", srv.Name),
					slog.String("url", url),
					slog.String("udn", realUDN))
			})
			return
		}
	}

	name := desc.FriendlyName
	if strings.TrimSpace(name) == "" {
		name = srv.Name
	}
	p.cache.Upsert(ServerInfo{
		// The StableServerKey, deliberately — see the file docblock.
		UDN:                        srv.Key,
		FriendlyName:               name,
		Manufacturer:               desc.Manufacturer,
		ModelDescription:           desc.ModelDescription,
		ModelName:                  desc.ModelName,
		ContentDirectoryControlURL: ctrlURL,
		DescriptionURL:             url,
		DeviceUDN:                  strings.TrimSpace(desc.UDN),
		LastSeenAt:                 p.nowFunc(),
	})
}

// warnOnce emits fn the first time it is called for key. The
// duplicate-config warning describes a static misconfiguration, so
// repeating it on every poll tick would be noise.
func (p *ManualPoller) warnOnce(key string, fn func()) {
	p.mu.Lock()
	_, seen := p.warned[key]
	if !seen {
		p.warned[key] = struct{}{}
	}
	p.mu.Unlock()
	if !seen {
		fn()
	}
}
