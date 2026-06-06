package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
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
	adminState       *upnpAdminState // populated lazily on installAdminAdapter
	log              *slog.Logger
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
		Client: &http.Client{Timeout: 10 * time.Second},
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
		return &upnpUpstreamLifecycle{
			discoveryClients: clients,
			cache:            cache,
			log:              log,
		}
	}

	// Tick loop. The first tick runs after a short warm-up so SSDP has
	// time to populate the cache.
	tickCtx, cancel := context.WithCancel(ctx)
	scanEvery := time.Duration(cfg.ScanIntervalSec) * time.Second
	if scanEvery <= 0 {
		scanEvery = 6 * time.Hour
	}
	life := &upnpUpstreamLifecycle{
		discoveryClients: clients,
		cache:            cache,
		ingester:         ingester,
		tickerCancel:     cancel,
		log:              log,
	}
	life.ingestWg.Add(1)
	go life.runIngestLoop(tickCtx, ingester, scanEvery)

	log.Info("UPnP upstream started",
		slog.Int("interfaces", len(clients)),
		slog.Int("servers_configured", len(cfg.UPnPUpstream.Servers)),
		slog.Duration("scanInterval", scanEvery))
	return life
}

// Stop tears down the ingest loop + all discovery clients. Idempotent.
func (l *upnpUpstreamLifecycle) Stop() {
	if l == nil {
		return
	}
	if l.tickerCancel != nil {
		l.tickerCancel()
	}
	l.ingestWg.Wait()
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
				slog.Int("walked", pr.Walked), slog.Int("reaped", pr.Reaped))
		}
	}
}

// discoveryServerResolver implements upnpingest.ServerResolver against
// the SSDP discovery cache. Returns "" + nil when the configured server
// hasn't been seen yet — the ingester logs + skips that server for this
// tick.
type discoveryServerResolver struct{ cache *upnp.ServerCache }

func (r *discoveryServerResolver) ResolveControlURL(_ context.Context, srv config.UPnPUpstreamServerConfig) (string, error) {
	if udn := srv.UDN; udn != "" {
		if info, ok := r.cache.Get(udn); ok {
			return info.ContentDirectoryControlURL, nil
		}
	}
	// TODO (Bridge PR-D follow-up): support srv.ManualDescriptionURL —
	// fetch + parse the description here and cache its controlURL. v1
	// is SSDP-only.
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
