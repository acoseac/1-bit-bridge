package main

import (
	"context"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
	"github.com/acoseac/1-bit-bridge/internal/upnpingest"
)

// upnpPublicAdapter fulfills api.UPnPUpstreamPublicProvider so the
// bridge advertises operator-configured upstream MediaServers on
// `/v1/health` for iOS UI consumption.
//
// Companion to `upnpAdminAdapter` (admin console / operator surface) —
// the data sources are the same (cfg, discovery cache, manifest count)
// but the consumer + the wire shape differ. The admin surface carries
// operator-internal fields (control URL, last-walk error, manufacturer,
// raw manualDescriptionURL); the public surface advertises only the
// minimal subset iOS needs for sub-source filtering (Name + UDN +
// PathPrefix membership key + FriendlyName display label + RoutedTracks
// chip subtitle).
//
// Why a SEPARATE adapter and not a method on upnpAdminAdapter: keeping
// the public DTO independent of the admin DTO means a future admin-side
// addition (e.g. a debug-only telemetry counter) can't accidentally
// leak through `/v1/health` to every paired iOS device on the LAN. The
// two surfaces have different security postures (admin = loopback-only
// + operator-authed; public = LAN-exposed once paired) and the
// type-level split enforces that distinction at compile time.
type upnpPublicAdapter struct {
	cfgHolder *config.RuntimeConfig
	cache     *upnp.ServerCache
	store     *manifest.Store
}

// PublicServers merges the YAML config with the live discovery cache +
// the manifest's per-server routed-track count into the public DTO. Run
// once per /v1/health request — cheap (single-digit server count in
// practice, plus one COUNT(*) per server gated by an index on
// upnp_track_routing.server_udn).
//
// Order matches the YAML config order so the iOS UI is deterministic
// across health probes (mirrors the admin adapter's invariant).
func (a *upnpPublicAdapter) PublicServers() []api.UPnPUpstreamPublicServer {
	cfg := a.cfgHolder.Load()
	if cfg == nil {
		return nil
	}
	out := make([]api.UPnPUpstreamPublicServer, 0, len(cfg.UPnPUpstream.Servers))
	for _, srv := range cfg.UPnPUpstream.Servers {
		row := api.UPnPUpstreamPublicServer{
			Name:          srv.Name,
			ConfiguredUDN: srv.UDN,
			PathPrefix:    srv.PathPrefix,
		}
		// SSDP-keyed friendly-name lookup applies only to servers
		// configured with a UDN — manual-URL entries don't get into the
		// discovery cache (the M-SEARCH responder matches by UDN, and a
		// manual entry pre-discovery has none). Manual entries surface
		// FriendlyName == "" on the wire; iOS falls back to Name.
		if srv.UDN != "" {
			if info, ok := a.cache.Get(srv.UDN); ok {
				row.FriendlyName = info.FriendlyName
			}
		}
		// Routed-track count keyed on the SAME stable key the ingester
		// uses (`upnpingest.StableServerKey` returns the lowercased UDN
		// OR the SHA-256-hashed ManualDescriptionURL for manual entries
		// — both paths route through one column). Keeps the public
		// count consistent with the admin row's `routedTracks`.
		key := upnpingest.StableServerKey(srv)
		if a.store != nil {
			if n, err := a.store.CountUPnPRoutingForServer(context.Background(), key); err == nil {
				row.RoutedTracks = n
			}
		}
		out = append(out, row)
	}
	return out
}

// installPublicProvider builds the public-surface adapter for the
// lifecycle. Returns nil when the lifecycle is nil OR when the cache
// hasn't been initialised (feature disabled at startup) so the
// `WithUPnPUpstreamPublicProvider` call in cmd/bridge/main.go cleanly
// passes nil and `/v1/health` omits the upnpUpstreamServers field.
func (l *upnpUpstreamLifecycle) installPublicProvider(cfgHolder *config.RuntimeConfig, store *manifest.Store) api.UPnPUpstreamPublicProvider {
	if l == nil || l.cache == nil {
		return nil
	}
	return &upnpPublicAdapter{
		cfgHolder: cfgHolder,
		cache:     l.cache,
		store:     store,
	}
}
