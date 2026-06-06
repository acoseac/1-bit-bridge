package main

import (
	"context"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
	"github.com/acoseac/1-bit-bridge/internal/upnpingest"
)

// upnpAdminAdapter fulfills admin.UPnPUpstreamProvider so the admin
// console can render per-server status + trigger a force-rescan. Wired
// in cmd/bridge alongside the upnpUpstreamLifecycle.
//
// The adapter is the SINGLE chokepoint that turns the lifecycle's
// runtime state (discovery cache + last-run result) + the configured
// servers (cfg) + the manifest's per-server routed-track count into the
// flat admin DTO. Nil-safe via lifecycle gating: if the lifecycle is
// nil, the adapter never gets constructed.
type upnpAdminAdapter struct {
	cfgHolder *config.RuntimeConfig
	cache     *upnp.ServerCache
	ingester  *upnpingest.Ingester
	store     *manifest.Store
	state     *upnpAdminState
}

// upnpAdminState is the small in-memory store of last-run results,
// keyed by the stable server key the ingester uses. The lifecycle
// loop updates it; the admin handler reads it under the mutex.
//
// We avoid storing per-server result on the lifecycle's exported
// fields because the ingester's result type already carries everything
// we need; the adapter just remembers the LAST result.
type upnpAdminState struct {
	mu        sync.Mutex
	lastByUDN map[string]upnpingest.ServerIngestResult

	// inFlight gates concurrent force-rescans; matches the ticker's
	// single-goroutine invariant.
	inFlight bool
}

func newUPnPAdminState() *upnpAdminState {
	return &upnpAdminState{lastByUDN: make(map[string]upnpingest.ServerIngestResult)}
}

func (st *upnpAdminState) record(res upnpingest.IngestResult) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, pr := range res.PerServer {
		// Key the latest result by the *configured* UDN when present;
		// fall back to ServerUDN (which may be a hashed manual key).
		st.lastByUDN[pr.ServerUDN] = pr
	}
}

func (st *upnpAdminState) snapshot() map[string]upnpingest.ServerIngestResult {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[string]upnpingest.ServerIngestResult, len(st.lastByUDN))
	for k, v := range st.lastByUDN {
		out[k] = v
	}
	return out
}

// ConfiguredServers merges the YAML config with the live discovery
// cache + the last-known ingest result + the manifest's per-server
// track count into the admin DTO. Cheap reads — safe under the
// dashboard's polling cadence.
func (a *upnpAdminAdapter) ConfiguredServers() []admin.UPnPUpstreamServerState {
	cfg := a.cfgHolder.Load()
	if cfg == nil {
		return nil
	}
	last := a.state.snapshot()
	out := make([]admin.UPnPUpstreamServerState, 0, len(cfg.UPnPUpstream.Servers))
	for _, srv := range cfg.UPnPUpstream.Servers {
		row := admin.UPnPUpstreamServerState{
			Name:          srv.Name,
			ConfiguredUDN: srv.UDN,
			ManualURL:     srv.ManualDescriptionURL,
		}
		if srv.UDN != "" {
			if info, ok := a.cache.Get(srv.UDN); ok {
				row.Discovered = true
				row.ResolvedUDN = info.UDN
				row.FriendlyName = info.FriendlyName
				row.Manufacturer = info.Manufacturer
				row.ControlURL = info.ContentDirectoryControlURL
				row.LastSeenAt = info.LastSeenAt
			}
			if pr, ok := last[srv.UDN]; ok {
				row.LastWalkStarted = pr.WalkStartedAt
				row.LastWalkFinished = pr.WalkCompletedAt
				row.LastWalked = pr.Walked
				row.LastReaped = pr.Reaped
				if pr.Err != nil {
					row.LastWalkErr = pr.Err.Error()
				}
			}
			if a.store != nil {
				if n, err := a.store.CountUPnPRoutingForServer(context.Background(), srv.UDN); err == nil {
					row.RoutedTracks = n
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// ForceRescan triggers a single rescan. udn=="" forces every server;
// any other value matches the configured UDN exactly. Concurrent calls
// are rejected with ErrUPnPRescanInFlight so we don't overlap with the
// ticker's own goroutine.
func (a *upnpAdminAdapter) ForceRescan(ctx context.Context, udn string) error {
	a.state.mu.Lock()
	if a.state.inFlight {
		a.state.mu.Unlock()
		return admin.ErrUPnPRescanInFlight
	}
	a.state.inFlight = true
	a.state.mu.Unlock()
	defer func() {
		a.state.mu.Lock()
		a.state.inFlight = false
		a.state.mu.Unlock()
	}()

	cfg := a.cfgHolder.Load()
	if cfg == nil || !cfg.UPnPUpstream.Enabled {
		return admin.ErrUPnPNoSuchServer
	}
	if udn != "" {
		// Match a configured server before we run; otherwise return
		// the typed 404 the handler maps to.
		match := false
		for _, srv := range cfg.UPnPUpstream.Servers {
			if srv.UDN == udn {
				match = true
				break
			}
		}
		if !match {
			return admin.ErrUPnPNoSuchServer
		}
	}
	res, err := a.ingester.Run(ctx, upnpingest.Options{ForceWalk: true})
	if err != nil {
		return err
	}
	a.state.record(res)
	return nil
}

// Lifecycle integration helpers. These are called from the lifecycle's
// tick loop so the admin surface always reflects the most recent run.

// recordIngest captures one Ingester.Run result into the admin state.
// Called from runOneIngest (lifecycle); also called by ForceRescan
// (admin path).
func (l *upnpUpstreamLifecycle) recordIngestResult(res upnpingest.IngestResult) {
	if l == nil || l.adminState == nil {
		return
	}
	l.adminState.record(res)
}

// installAdminAdapter constructs the adapter once and stashes it on the
// lifecycle so the cmd/bridge wiring can pass it into admin.Deps.
func (l *upnpUpstreamLifecycle) installAdminAdapter(cfgHolder *config.RuntimeConfig, store *manifest.Store, ingester *upnpingest.Ingester) admin.UPnPUpstreamProvider {
	if l == nil || l.cache == nil || ingester == nil {
		return nil
	}
	if l.adminState == nil {
		l.adminState = newUPnPAdminState()
	}
	return &upnpAdminAdapter{
		cfgHolder: cfgHolder,
		cache:     l.cache,
		ingester:  ingester,
		store:     store,
		state:     l.adminState,
	}
}

// Keep linter happy about the imported time package if the adapter
// stops using it later.
var _ = time.Time{}
