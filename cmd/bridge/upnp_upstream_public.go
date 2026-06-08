package main

import (
	"context"
	"strings"

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
// type-level split enforces that distinction at compile time. The
// shared per-server lookup helper (`lookupUPnPServerRuntime`) means
// the cache + store keying stays consistent across both adapters even
// though the DTOs differ.
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
// The `ctx` is the inbound HTTP request's context — propagated through
// to `CountUPnPRoutingForServer` so a client disconnect mid-/v1/health
// cancels the SQLite query rather than letting it run to completion
// against a now-dead connection. Mirrors how the per-root reachability
// probe receives `r.Context()` from the `health()` handler.
//
// Order matches the YAML config order so the iOS UI is deterministic
// across health probes (mirrors the admin adapter's invariant).
//
// Defensive nil checks: a torn-down `cfgHolder` / `cache` (shouldn't
// happen in production, but a future test seam could land here)
// degrades to "empty list" rather than panicking the /v1/health
// handler.
func (a *upnpPublicAdapter) PublicServers(ctx context.Context) []api.UPnPUpstreamPublicServer {
	if a.cfgHolder == nil {
		return nil
	}
	cfg := a.cfgHolder.Load()
	if cfg == nil {
		return nil
	}
	out := make([]api.UPnPUpstreamPublicServer, 0, len(cfg.UPnPUpstream.Servers))
	for _, srv := range cfg.UPnPUpstream.Servers {
		friendly, routed, online := lookupUPnPServerRuntime(ctx, srv, a.cache, a.store)
		out = append(out, api.UPnPUpstreamPublicServer{
			Name:          srv.Name,
			ConfiguredUDN: srv.UDN,
			PathPrefix:    srv.PathPrefix,
			FriendlyName:  friendly,
			RoutedTracks:  routed,
			Online:        online,
		})
	}
	return out
}

// lookupUPnPServerRuntime is the single chokepoint that resolves the
// per-server runtime state (SSDP friendly-name + manifest-routed-track
// count) for ONE configured upstream. Shared between the admin and
// public adapters so the cache lookup gate (UDN-keyed, manual entries
// surface empty) AND the routed-track count keying
// (`upnpingest.StableServerKey` — covers both UDN and hashed-URL keys
// via one column) stay in lockstep across both surfaces.
//
// `ctx` propagates through to the SQLite COUNT(*) so a client
// disconnect cancels the query. `cache` / `store` are nil-tolerant —
// returns ("", 0) on either nil, matching the surface-level "no
// runtime info" semantic both adapters expect.
//
// Returns ("" string, 0 int) for any of: nil cache, nil store, manual
// entry with no UDN to look up, fresh SSDP cache miss, store error.
// The signature deliberately avoids surfacing the error — callers
// treat absence as "warm-up window", not "operational failure".
func lookupUPnPServerRuntime(
	ctx context.Context,
	srv config.UPnPUpstreamServerConfig,
	cache *upnp.ServerCache,
	store *manifest.Store,
) (friendlyName string, routedTracks int, online bool) {
	// SSDP-keyed friendly-name + liveness lookup applies only to servers
	// configured with a UDN — manual-URL entries don't get into the
	// discovery cache (the M-SEARCH responder matches by UDN, and a
	// manual entry pre-discovery has none). Manual entries surface
	// FriendlyName == "" and online == true (their real reachability
	// surfaces as a 503 upnp_server_offline on fetch). A UDN entry is
	// online iff it's currently in the discovery cache — refreshed every
	// M-SEARCH, evicted at ServerTTL (≈180s) — so powering the upstream
	// OFF flips online false within the TTL window even while the bridge
	// stays reachable. The nil-cache guard matches PublicAdapter's
	// defensive posture above.
	// Trim before the empty-check + cache lookup so a config UDN with
	// surrounding whitespace (" uuid:123 ") doesn't miss the cache and
	// false-report offline — matches the trim StableServerKey already
	// does for the routed-track key. (Gemini on PR #362.)
	udn := strings.TrimSpace(srv.UDN)
	online = udn == ""
	if udn != "" && cache != nil {
		if info, ok := cache.Get(udn); ok {
			friendlyName = info.FriendlyName
			online = true
		}
	}
	// Routed-track count keyed on the SAME stable key the ingester
	// uses — `upnpingest.StableServerKey` returns the lowercased UDN
	// OR the SHA-256-hashed ManualDescriptionURL for manual entries.
	// Both paths route through one column so manual servers DO get
	// counted (the original admin adapter shipped a bug here that
	// PR #353 fixed — the public adapter inherits the fix by
	// construction).
	if store != nil {
		key := upnpingest.StableServerKey(srv)
		if n, err := store.CountUPnPRoutingForServer(ctx, key); err == nil {
			routedTracks = n
		}
	}
	return friendlyName, routedTracks, online
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
