package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
	"github.com/acoseac/1-bit-bridge/internal/upnpingest"
)

// upnpUpstreamLifecycle owns the SSDP MediaServer discovery client +
// the periodic ingest ticker. Nil-safe — disabled / setup-failed paths
// return a lifecycle whose Stop is a no-op.
type upnpUpstreamLifecycle struct {
	discoveryClients []*upnp.MediaServerDiscoveryClient
	cache            *upnp.ServerCache
	ingester         *upnpingest.Ingester // exposed so the admin adapter can ForceRescan
	tickerCancel     context.CancelFunc
	ingestWg         sync.WaitGroup
	// manualCancel / manualWg scope the manual-URL poller to this
	// lifecycle rather than to the process. Started on the parent ctx it
	// would outlive Stop(), which is a contract violation even though the
	// app ctx would eventually reap it — and would leave a live goroutine
	// behind any test that stops the lifecycle. (Gemini HIGH, PR #824.)
	manualCancel context.CancelFunc
	manualWg     sync.WaitGroup
	adminState   *upnpAdminState // populated lazily on installAdminAdapter
	log          *slog.Logger
}

// startUPnPUpstreamIfEnabled wires the MediaServer SSDP discovery +
// periodic ingest. Refuses unless `cfg.UPnPUpstream.Enabled = true`.
// Returns a nil-safe lifecycle in all paths; failures are logged at WARN
// and the bridge continues without the feature (additive-features-
// fail-open convention).
//
// `store` is the manifest store used both for ingestion and for the
// api.Server's proxy-routing lookup. `apiSrv` is wired with the proxy
// before this function returns so /v1/download serves UPnP-routed
// tracks via the proxy from the first request.
//
// The ingest tick runs every `cfg.ScanIntervalSec` seconds, piggy-
// backing on the bridge's existing scan cadence. A one-shot run also
// fires shortly after Start so a fresh-deploy operator sees their 2Go's
// tracks within seconds, not hours.
func startUPnPUpstreamIfEnabled(
	ctx context.Context,
	cfg *config.Config,
	store *manifest.Store,
	apiSrv *api.Server,
	logger *slog.Logger,
) *upnpUpstreamLifecycle {
	log := logger.With(slog.String("component", "upnp-upstream"))
	if !cfg.UPnPUpstream.Enabled {
		return &upnpUpstreamLifecycle{log: log}
	}
	// Public-mode runtime guard. Config.Validate already refuses the
	// public+enabled combination — this is defense-in-depth against a
	// future code path that might construct a Config without going
	// through Validate. Log loudly so the operator sees the breadcrumb
	// in the bridge log + admin events. (The admin console's edit path
	// runs clone→mutate→validate→save atomically, so it can't bypass
	// Validate; this guard exists for any FUTURE code path that does.)
	if cfg.IsPublic() {
		log.Warn("UPnP upstream refused — public-mode bridge cannot reach the upstream's LAN-only multicast and RFC1918 HTTP endpoints")
		return &upnpUpstreamLifecycle{log: log}
	}
	if store == nil {
		log.Warn("UPnP upstream refused — nil manifest store")
		return &upnpUpstreamLifecycle{log: log}
	}

	// SSDP MediaServer discovery — one client per LAN-eligible interface,
	// all writing into one shared cache (the same pattern the renderer
	// discovery uses).
	ifaces := dlna.PickAllLANEligibleInterfaces(dlna.EligibilityOpts{})
	if len(ifaces) == 0 {
		log.Warn("UPnP upstream disabled — no LAN-eligible interface")
		return &upnpUpstreamLifecycle{log: log}
	}
	cache := upnp.NewServerCache()
	clients := startUPnPDiscoveryAcrossInterfaces(ctx, cfg.UPnPUpstream, cache, ifaces, log)
	if len(clients) == 0 {
		log.Warn("UPnP upstream disabled — no interface could bind")
		return &upnpUpstreamLifecycle{log: log}
	}

	// Manual-URL servers, refreshed into the SAME cache so the resolver,
	// the api proxy's LiveHost and the online chip all see them without
	// any of the three learning about a second source. Interval matches
	// the SSDP TTL: EvictStale reaps entries the poller stops refreshing,
	// and that is exactly how an unreachable manual URL comes to report
	// offline.
	//
	// Known limitation, deliberately not addressed here: the two early
	// returns above disable the whole upstream feature when no LAN
	// interface is eligible or none can bind, which also disables manual
	// URLs even though those need no multicast. Restructuring the
	// lifecycle to run manual-only is a larger change than this one and
	// would put the SSDP path at risk to serve a rarer case.
	//
	// `cfg` is the BOOT snapshot: UPnP upstream configuration is
	// restart-bound (this whole function runs once, at startup), so the
	// closures below read a stable value rather than a live one. Said
	// plainly because an earlier comment here claimed they picked up a
	// reload, which they do not. (Gemini MEDIUM.)
	manualCtx, manualCancel := context.WithCancel(ctx)
	manualPoller := upnp.NewManualPoller(upnp.ManualPollerConfig{
		Cache:     cache,
		Servers:   func() []upnp.ManualServer { return manualServersFrom(cfg) },
		KnownUDNs: func() map[string]struct{} { return foreignConfiguredUDNs(cfg) },
		Interval:  cfg.UPnPUpstream.EffectiveMSearchInterval(),
		Timeout:   upnp.DefaultDiscoveryConfig().DetailFetchTimeout,
		Logger:    log,
	})
	// Attach the poller to whichever lifecycle we end up returning, so
	// Stop() cancels and JOINS it. Started on the parent ctx it would
	// outlive Stop — the app ctx would reap it eventually, but a test that
	// stops the lifecycle would be left with a live goroutine, and the
	// lifecycle contract says Stop means stopped.
	withManualPoller := func(l *upnpUpstreamLifecycle) *upnpUpstreamLifecycle {
		l.manualCancel = manualCancel
		if manualPoller != nil {
			l.manualWg.Add(1)
			go func() {
				defer l.manualWg.Done()
				manualPoller.Run(manualCtx)
			}()
		}
		return l
	}

	// Wire the proxy on the api.Server — the ContentDirectory client
	// the ingester uses is the same shape we'd pass to the proxy if it
	// needed SOAP (it doesn't today; the proxy is plain HTTP), so we
	// just give the proxy the live-host resolver + the routing lookup.
	apiSrv.
		WithUPnPRouting(store).
		WithUPnPHostResolver(&serverCacheHostResolver{cache: cache})

	// Ingester — share the HTTP client config with the discovery
	// dispatcher (matching timeouts means a stuck server fails the same
	// way at both layers).
	cdsClient := upnp.NewContentDirectoryClient(&discovery.HTTPClientDispatcher{
		Client: upnpUpstreamSOAPHTTPClient(10 * time.Second),
	})
	ingester, err := upnpingest.NewIngester(
		cfg.UPnPUpstream, cdsClient,
		&discoveryServerResolver{cache: cache},
		store, nil)
	if err != nil {
		log.Warn("UPnP upstream ingester construct failed", slog.String("err", err.Error()))
		// Discovery already started — leave it running so /v1/servers
		// (or the operator's debug curl against the cache) still works.
		// The lifecycle's Stop will tear it down.
		return withManualPoller(&upnpUpstreamLifecycle{
			discoveryClients: clients,
			cache:            cache,
			log:              log,
		})
	}

	// Tick loop. The first tick runs after a short warm-up so SSDP has
	// time to populate the cache.
	tickCtx, cancel := context.WithCancel(ctx)
	scanEvery := time.Duration(cfg.ScanIntervalSec) * time.Second
	if scanEvery <= 0 {
		scanEvery = 6 * time.Hour
	}
	life := withManualPoller(&upnpUpstreamLifecycle{
		discoveryClients: clients,
		cache:            cache,
		ingester:         ingester,
		tickerCancel:     cancel,
		log:              log,
	})
	life.ingestWg.Add(1)
	go life.runIngestLoop(tickCtx, ingester, scanEvery)

	log.Info("UPnP upstream started",
		slog.Int("interfaces", len(clients)),
		slog.Int("servers_configured", len(cfg.UPnPUpstream.Servers)),
		slog.Duration("scanInterval", scanEvery))
	return life
}

// upnpUpstreamSOAPHTTPClient builds the HTTP client used for
// ContentDirectory SOAP calls to upstream MediaServers. The control
// URL is advertiser-supplied (SSDP description.xml on a LAN device,
// possibly rogue or spoofed), so redirects are relayed verbatim
// rather than followed — auto-following a 3xx to loopback or a
// link-local metadata address would turn the bridge into an SSRF
// probe against its own no-auth admin API. Mirrors
// internal/upnpproxy's CheckRedirect guard and the discovery
// dispatchers in internal/{upnp,dlna}/discovery.
func upnpUpstreamSOAPHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Stop tears down the ingest loop + all discovery clients. Idempotent.
func (l *upnpUpstreamLifecycle) Stop() {
	if l == nil {
		return
	}
	if l.tickerCancel != nil {
		l.tickerCancel()
	}
	if l.manualCancel != nil {
		l.manualCancel()
	}
	l.ingestWg.Wait()
	l.manualWg.Wait()
	for _, c := range l.discoveryClients {
		c.Stop()
	}
}

// runIngestLoop fires Ingester.Run on the configured interval. First
// tick lands after a 15 s warm-up so SSDP populates the cache.
func (l *upnpUpstreamLifecycle) runIngestLoop(ctx context.Context, ingester *upnpingest.Ingester, interval time.Duration) {
	defer l.ingestWg.Done()
	// Warm-up window for SSDP.
	select {
	case <-time.After(15 * time.Second):
	case <-ctx.Done():
		return
	}
	l.runOneIngest(ctx, ingester)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.runOneIngest(ctx, ingester)
		}
	}
}

func (l *upnpUpstreamLifecycle) runOneIngest(ctx context.Context, ingester *upnpingest.Ingester) {
	res, err := ingester.Run(ctx, upnpingest.Options{})
	if err != nil {
		// Only fatal-misconfig errors come out here (ctx cancelled,
		// disabled config) — per-server failures are inside res.
		if !errors.Is(err, context.Canceled) {
			l.log.Warn("UPnP upstream ingest run failed", slog.String("err", err.Error()))
		}
		return
	}
	// Stash the result on the admin state (no-op when no admin adapter
	// has been installed yet — happens during the very-first warm-up
	// tick before cmd/bridge wires the admin Deps).
	l.recordIngestResult(res)
	if res.OrphanSweepErr != nil {
		l.log.Warn("UPnP upstream: orphan sweep failed (retries next tick)",
			slog.String("err", res.OrphanSweepErr.Error()))
	}
	if res.OrphanServersReaped > 0 {
		l.log.Info("UPnP upstream: reaped removed servers' rows",
			slog.Int("servers", res.OrphanServersReaped),
			slog.Int("tracks", res.OrphanTracksReaped))
	}
	for _, pr := range res.PerServer {
		switch {
		case pr.Skipped:
			l.log.Debug("UPnP upstream: skip",
				slog.String("server", pr.Name), slog.String("udn", pr.ServerUDN))
		case pr.Err != nil:
			l.log.Warn("UPnP upstream: per-server error",
				slog.String("server", pr.Name), slog.String("err", pr.Err.Error()))
		default:
			l.log.Info("UPnP upstream: ingest complete",
				slog.String("server", pr.Name), slog.String("udn", pr.ServerUDN),
				slog.Int("walked", pr.Walked), slog.Int("unchanged", pr.Unchanged),
				slog.Int("reaped", pr.Reaped))
		}
	}
}

// discoveryServerResolver implements upnpingest.ServerResolver against
// the SSDP discovery cache. Returns "" + nil when the configured server
// hasn't been seen yet — the ingester logs + skips that server for this
// tick.
type discoveryServerResolver struct{ cache *upnp.ServerCache }

func (r *discoveryServerResolver) ResolveControlURL(_ context.Context, srv config.UPnPUpstreamServerConfig) (string, error) {
	// Trim mirrors lookupUPnPServerRuntime + ConfiguredServers (Gemini on
	// PR #362) — a hand-edited bridge.yaml UDN with stray whitespace
	// otherwise splits the brain: admin/health report the server online
	// (trimmed lookups hit) while the ingest misses the cache every tick.
	if udn := strings.TrimSpace(srv.UDN); udn != "" {
		if info, ok := r.cache.Get(udn); ok {
			return info.ContentDirectoryControlURL, nil
		}
	}
	// Manual-URL servers live in the SAME cache, under the ingest's
	// StableServerKey (`manual:<sha256(url)>` when there is no UDN, the
	// lowercased UDN when there is) rather than the device's own UDN —
	// see internal/upnp/manual.go for why. So resolving one is the same
	// cache lookup, against the other spelling.
	//
	// Tried for a UDN-configured server TOO, not just a UDN-less one:
	// with both configured, the manual URL is the fallback for when SSDP
	// has not found the device, and only reaching for it here makes that
	// fallback real.
	//
	// The entry is written by the ManualPoller, which refreshes on the
	// SSDP TTL cadence; a miss here means the URL has not answered yet
	// (or has stopped), and the ingest reports that as not-discoverable,
	// which is now the honest answer rather than a euphemism for
	// unimplemented.
	if strings.TrimSpace(srv.ManualDescriptionURL) != "" {
		if info, ok := r.cache.Get(upnpingest.StableServerKey(srv)); ok {
			return info.ContentDirectoryControlURL, nil
		}
	}
	return "", nil
}

// serverCacheHostResolver implements api.UPnPServerHostResolver against
// the SSDP discovery cache. The api proxy uses this to rebuild the
// live host:port for every request, defeating the upstream's volatile
// IP/port.
type serverCacheHostResolver struct{ cache *upnp.ServerCache }

func (r *serverCacheHostResolver) LiveHost(udn string) (string, bool) {
	info, ok := r.cache.Get(udn)
	if !ok || info.ContentDirectoryControlURL == "" {
		return "", false
	}
	// Derive host:port from the controlURL (the cache doesn't store
	// host:port separately; the controlURL is the freshest source).
	// We use the discovery package's URL-parse path to stay consistent.
	hostport, ok := hostPortFromURL(info.ContentDirectoryControlURL)
	if !ok {
		return "", false
	}
	return hostport, true
}

// HostResolver returns the live-host lookup wired into this
// lifecycle's SSDP discovery cache — or nil when the feature is
// disabled (no servers configured / no LAN-eligible interface bound /
// cmd/bridge constructed a no-op lifecycle). Consumers that want to
// proxy upstream bytes (both the api `/v1/download` path AND the
// dlna `/dlna/file/{trackID}` path) call this to construct a
// `*upnpproxy.Proxy`. Sharing the resolver across both surfaces means
// they observe the SAME SSDP cache; constructing distinct proxies
// (per-surface HTTP clients) is acceptable at the operator scale.
//
// Idempotent — safe to call from cmd/bridge wiring during startup
// even before discovery has its first M-SEARCH response (the
// resolver's `LiveHost` returns ("", false) until the cache fills,
// which is the documented "warm-up window" path).
func (l *upnpUpstreamLifecycle) HostResolver() *serverCacheHostResolver {
	if l == nil || l.cache == nil {
		return nil
	}
	return &serverCacheHostResolver{cache: l.cache}
}

// hostPortFromURL extracts the host:port from a control URL. The host
// field of net/url.URL carries the port when one is present (the
// upstream's libmicrohttpd typically advertises :8200), which is
// exactly what the proxy needs.
func hostPortFromURL(s string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", false
	}
	return u.Host, true
}

// startUPnPDiscoveryAcrossInterfaces spins up one
// MediaServerDiscoveryClient per LAN-eligible interface, writing into
// the shared cache. Skip-and-warn on per-interface construct / Start
// failures (additive-features-fail-open). Extracted as its own helper
// to keep the per-interface loop's failure-handling pattern in a single
// site instead of repeating it inline + duplicating
// dlna_discovery_wiring's shape.
func startUPnPDiscoveryAcrossInterfaces(
	ctx context.Context,
	upCfg config.UPnPUpstreamConfig,
	cache *upnp.ServerCache,
	ifaces []*net.Interface,
	log *slog.Logger,
) []*upnp.MediaServerDiscoveryClient {
	clients := make([]*upnp.MediaServerDiscoveryClient, 0, len(ifaces))
	for _, iface := range ifaces {
		discCfg := upnp.DefaultDiscoveryConfig()
		discCfg.Interface = iface
		discCfg.MSearchInterval = upCfg.EffectiveMSearchInterval()
		discCfg.ServerTTL = upCfg.EffectiveServerTTL()
		client, cerr := upnp.NewMediaServerDiscoveryClient(discCfg, cache)
		if cerr == nil {
			cerr = client.Start(ctx)
		}
		if cerr != nil {
			log.Warn("UPnP upstream discovery start skipped on interface",
				slog.String("iface", iface.Name), slog.String("err", cerr.Error()))
			continue
		}
		clients = append(clients, client)
	}
	return clients
}

// manualServersFrom projects the configured manual-URL servers into the
// poller's shape, keyed by the ingest's StableServerKey so the cache
// entry lands under the string every other subsystem uses.
func manualServersFrom(cfg *config.Config) []upnp.ManualServer {
	if cfg == nil {
		return nil
	}
	out := make([]upnp.ManualServer, 0, len(cfg.UPnPUpstream.Servers))
	for _, srv := range cfg.UPnPUpstream.Servers {
		if strings.TrimSpace(srv.ManualDescriptionURL) == "" {
			continue
		}
		out = append(out, upnp.ManualServer{
			Key:            upnpingest.StableServerKey(srv),
			DescriptionURL: strings.TrimSpace(srv.ManualDescriptionURL),
			Name:           srv.Name,
		})
	}
	return out
}

// foreignConfiguredUDNs is the set of UDNs an operator configured
// explicitly on some OTHER server entry. The manual poller refuses to
// cache a device whose description reports one of these: it would then
// sit in the cache under two identities and the ingest would walk one
// upstream twice under two routing prefixes.
//
// "Foreign" is the load-bearing word. A server configured with BOTH a UDN
// and a manual URL reports its own UDN, and rejecting it there would make
// the manual URL useless for the operator who supplied both as a
// belt-and-braces — exactly the case where SSDP is unreliable. The
// poller compares against its own key and admits that one; this set only
// carries the UDNs belonging to entries OTHER than the one being polled.
// (Gemini HIGH + CodeRabbit Major, PR #824.)
func foreignConfiguredUDNs(cfg *config.Config) map[string]struct{} {
	if cfg == nil {
		return nil
	}
	out := make(map[string]struct{}, len(cfg.UPnPUpstream.Servers))
	for _, srv := range cfg.UPnPUpstream.Servers {
		if udn := strings.ToLower(strings.TrimSpace(srv.UDN)); udn != "" {
			out[udn] = struct{}{}
		}
	}
	return out
}
