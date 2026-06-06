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
	// bgCtx is the lifecycle's run context. ForceRescan dispatches its
	// Ingester.Run on it (NOT the admin handler's request ctx) so a
	// client disconnect can't abort a multi-second walk. Cancelled by
	// the lifecycle's Stop during graceful shutdown.
	bgCtx context.Context //nolint:containedctx // intentional: ctx is the run-scope for spawned scans
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
		// SSDP-keyed lookup applies only to servers configured with a
		// UDN — manual-URL servers don't get into the discovery cache
		// (SSDP M-SEARCH responses are matched by UDN, and a manual
		// entry pre-discovery has none).
		if srv.UDN != "" {
			if info, ok := a.cache.Get(srv.UDN); ok {
				row.Discovered = true
				row.ResolvedUDN = info.UDN
				row.FriendlyName = info.FriendlyName
				row.Manufacturer = info.Manufacturer
				row.ControlURL = info.ContentDirectoryControlURL
				row.LastSeenAt = info.LastSeenAt
			}
		}
		// The ingester's keying is the SAME for UDN and manual servers
		// — upnpingest.StableServerKey(srv) returns the lowercased UDN OR the
		// SHA-256-hashed ManualDescriptionURL (the "manual:..." form).
		// Using it here means manual servers DO surface their last-walk
		// result + routed-track count, fixing the gap Gemini caught on
		// PR #353.
		key := upnpingest.StableServerKey(srv)
		if pr, ok := last[key]; ok {
			row.LastWalkStarted = pr.WalkStartedAt
			row.LastWalkFinished = pr.WalkCompletedAt
			row.LastWalked = pr.Walked
			row.LastReaped = pr.Reaped
			if pr.Err != nil {
				row.LastWalkErr = pr.Err.Error()
			}
		}
		if a.store != nil {
			if n, err := a.store.CountUPnPRoutingForServer(context.Background(), key); err == nil {
				row.RoutedTracks = n
			}
		}
		out = append(out, row)
	}
	return out
}

// ForceRescan validates the request synchronously (config gate +
// optional UDN match) then SPAWNS the actual walk in a background
// goroutine. The handler returns 202 Accepted before the walk starts —
// a UPnP walk over a 4 TB library can take tens of seconds, so a
// synchronous call would (a) block the admin HTTP connection past a
// reverse-proxy's idle timeout and (b) abort mid-walk if the operator
// closes their browser (the request ctx would cancel). Gemini HIGH on
// PR #353.
//
// `inFlight` is the debounce: a second click while the first walk is
// still running returns ErrUPnPRescanInFlight (409). Validation errors
// (disabled feature, unknown UDN) are returned synchronously so the
// handler still maps them to the right status code; only the actual
// Ingester.Run is offloaded.
func (a *upnpAdminAdapter) ForceRescan(_ context.Context, udn string) error {
	cfg := a.cfgHolder.Load()
	if cfg == nil || !cfg.UPnPUpstream.Enabled {
		return admin.ErrUPnPNoSuchServer
	}
	if udn != "" {
		// Match a configured server before we accept the request;
		// otherwise return the typed 404 the handler maps to.
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
	// Race-free acquire of the in-flight slot.
	a.state.mu.Lock()
	if a.state.inFlight {
		a.state.mu.Unlock()
		return admin.ErrUPnPRescanInFlight
	}
	a.state.inFlight = true
	a.state.mu.Unlock()

	// Spawn the walk under the lifecycle's bgCtx (NOT the request ctx)
	// so a client disconnect / reverse-proxy timeout can't abort the
	// scan mid-tree. The lifecycle's Stop cancels bgCtx during
	// graceful shutdown, so this goroutine still drains cleanly.
	bgCtx := a.bgCtx
	if bgCtx == nil {
		bgCtx = context.Background()
	}
	go func() {
		defer func() {
			a.state.mu.Lock()
			a.state.inFlight = false
			a.state.mu.Unlock()
		}()
		res, err := a.ingester.Run(bgCtx, upnpingest.Options{ForceWalk: true})
		if err != nil {
			// Per-server errors are inside res; this only catches a
			// truly fatal Run-level error (ctx cancelled = shutdown).
			return
		}
		a.state.record(res)
	}()
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
// lifecycle so the cmd/bridge wiring can pass it into admin.Deps. `ctx`
// is the lifecycle's run context (canceled by Stop); ForceRescan
// dispatches its async Ingester.Run on it.
func (l *upnpUpstreamLifecycle) installAdminAdapter(ctx context.Context, cfgHolder *config.RuntimeConfig, store *manifest.Store, ingester *upnpingest.Ingester) admin.UPnPUpstreamProvider {
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
		bgCtx:     ctx,
	}
}

// Keep linter happy about the imported time package if the adapter
// stops using it later.
var _ = time.Time{}
