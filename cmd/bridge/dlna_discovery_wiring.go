package main

import (
	"context"
	"log/slog"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
)

// dlnaDiscoveryLifecycle owns the SSDP renderer discovery client +
// the api.Server adapter that converts cache entries into the wire
// shape. Nil-safe — disabled / setup-failed paths return a
// lifecycle whose Stop is a no-op and whose `snapshotter` is nil.
type dlnaDiscoveryLifecycle struct {
	client      *discovery.SSDPDiscoveryClient
	snapshotter api.RendererDiscoverySnapshotter
	log         *slog.Logger
}

// startDLNADiscoveryIfEnabled wires the SSDP MediaRenderer
// discovery client when the operator opted in via
// `cfg.DLNA.Discovery.Enabled = true`. Refuses unless:
//
//   - `dlnaEnabled` (sibling MediaServer is up + public-mode gate
//     passed) — the rendererDiscovery feature flag is AND-gated on
//     the MediaServer being up, so an iOS client seeing the flag
//     can trust the UDN namespace is coherent.
//
// Returns a nil-safe lifecycle in all paths (caller defers
// `.Stop()` without nil check). Failure to start (interface pick
// failed, bind error) is logged at WARN + the bridge continues
// without renderer discovery; matches the "additive features fail
// open" convention.
func startDLNADiscoveryIfEnabled(
	ctx context.Context,
	cfg *config.Config,
	dlnaEnabled bool,
	logger *slog.Logger,
) *dlnaDiscoveryLifecycle {
	log := logger.With(slog.String("component", "dlna-discovery"))
	if !cfg.DLNA.Discovery.Enabled {
		// Silent skip — operator didn't ask for it.
		return &dlnaDiscoveryLifecycle{log: log}
	}
	if !dlnaEnabled {
		// Operator enabled discovery but the MediaServer refused
		// (public mode / setup failure). Run-time gate per the
		// `rendererDiscovery` feature-flag contract.
		log.Warn("DLNA renderer discovery refused — MediaServer is not enabled (gated AND-wise)")
		return &dlnaDiscoveryLifecycle{log: log}
	}

	// LAN-eligible interface pick — same picker the server-side
	// SSDP advertiser uses. Reusing it keeps the SSDP socket on a
	// consistent interface (a future PR threading tsnet would
	// extend `EligibilityOpts` for both call sites in lockstep).
	iface, ifaceErr := dlna.PickLANEligibleInterface(dlna.EligibilityOpts{})
	if ifaceErr != nil {
		log.Warn("DLNA renderer discovery disabled — no LAN-eligible interface",
			slog.String("err", ifaceErr.Error()))
		return &dlnaDiscoveryLifecycle{log: log}
	}

	cache := discovery.NewRendererCache()
	discCfg := discovery.DefaultDiscoveryConfig()
	discCfg.Interface = iface
	discCfg.MSearchInterval = cfg.DLNA.Discovery.EffectiveMSearchInterval()
	discCfg.RendererTTL = cfg.DLNA.Discovery.EffectiveRendererTTL()

	client, err := discovery.NewSSDPDiscoveryClient(discCfg, cache)
	if err != nil {
		log.Warn("DLNA renderer discovery disabled — NewSSDPDiscoveryClient failed",
			slog.String("err", err.Error()))
		return &dlnaDiscoveryLifecycle{log: log}
	}
	if startErr := client.Start(ctx); startErr != nil {
		log.Warn("DLNA renderer discovery disabled — Start failed",
			slog.String("err", startErr.Error()))
		return &dlnaDiscoveryLifecycle{log: log}
	}
	log.Info("DLNA renderer discovery started",
		slog.String("interface", iface.Name),
		slog.Duration("msearchInterval", discCfg.MSearchInterval),
		slog.Duration("rendererTTL", discCfg.RendererTTL))
	return &dlnaDiscoveryLifecycle{
		client:      client,
		snapshotter: &rendererCacheAdapter{cache: cache},
		log:         log,
	}
}

// Stop cancels the discovery client. Safe to call on a nil-client
// lifecycle (disabled / failed-to-start paths return a zero-value
// lifecycle).
func (d *dlnaDiscoveryLifecycle) Stop() {
	if d == nil || d.client == nil {
		return
	}
	d.client.Stop()
}

// rendererCacheAdapter satisfies `api.RendererDiscoverySnapshotter`
// by converting `discovery.RendererInfo` → `api.RendererInfo`.
// Trivial value copy — the two structs are structurally identical
// (intentional duplication per the api-package docblock to avoid
// the api package importing the discovery package transitively).
//
// A compile-time assertion below catches structural drift: any
// future change to either struct that breaks the field-by-field
// copy will fail this file to compile.
type rendererCacheAdapter struct {
	cache *discovery.RendererCache
}

// Snapshot copies the discovery cache snapshot into the api-local
// wire shape. Allocates one slice per call (cheap — typical home
// LAN has <20 renderers, copy is O(N)).
func (a *rendererCacheAdapter) Snapshot() []api.RendererInfo {
	src := a.cache.Snapshot()
	if len(src) == 0 {
		return []api.RendererInfo{}
	}
	out := make([]api.RendererInfo, len(src))
	for i, e := range src {
		out[i] = api.RendererInfo{
			UDN:               e.UDN,
			FriendlyName:      e.FriendlyName,
			Manufacturer:      e.Manufacturer,
			ModelDescription:  e.ModelDescription,
			ModelName:         e.ModelName,
			ControlURL:        e.ControlURL,
			EventURL:          e.EventURL,
			SinkProtocolInfos: e.SinkProtocolInfos,
			LastSeenAt:        e.LastSeenAt,
		}
	}
	return out
}

// Compile-time guard: api.RendererInfo and discovery.RendererInfo
// MUST stay structurally assignable. The conversion in Snapshot
// above is the single chokepoint that would fail to compile on
// drift, but this explicit assertion makes the contract loud.
var _ api.RendererDiscoverySnapshotter = (*rendererCacheAdapter)(nil)
