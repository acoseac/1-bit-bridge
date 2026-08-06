package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
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
	cfgPath   string
	cache     *upnp.ServerCache
	ingester  *upnpingest.Ingester
	store     *manifest.Store
	state     *upnpAdminState
	// bgCtx is the lifecycle's run context. ForceRescan dispatches its
	// Ingester.Run on it (NOT the admin handler's request ctx) so a
	// client disconnect can't abort a multi-second walk. Cancelled by
	// the lifecycle's Stop during graceful shutdown.
	bgCtx context.Context //nolint:containedctx // intentional: ctx is the run-scope for spawned scans

	// ingestWg is the lifecycle's ingest WaitGroup (&l.ingestWg), shared
	// with the periodic ingest loop. ForceRescan's spawned goroutine
	// registers on it so upnpUpstreamLifecycle.Stop's ingestWg.Wait()
	// joins an operator-triggered rescan that's still writing to the
	// manifest — otherwise Stop could return (and shutdown proceed to
	// close the store) mid-walk. Nil-guarded: the direct-construction
	// unit tests leave it unset.
	ingestWg *sync.WaitGroup
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
		// Key by the CONFIGURED server's stable key — the same value
		// ConfiguredServers looks up with (upnpingest.StableServerKey).
		// Keying by ServerUDN missed for every manually-configured server
		// (StableKey is "manual:<hash>", ServerUDN is the discovered
		// device UDN) and for any server whose device reports its UDN in
		// different case than the config (StableKey lowercases; ServerUDN
		// is raw), so last-walk telemetry never rendered for those.
		key := pr.StableKey
		if key == "" {
			key = pr.ServerUDN // pre-StableKey results / hand-built test fixtures
		}
		st.lastByUDN[key] = pr
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
// track count into the admin DTO.
//
// The per-server track count is a DB read (one COUNT(*) per configured
// upstream), so the caller's ctx is threaded all the way through to
// lookupUPnPServerRuntime. It used to pass context.Background() on the
// reasoning that "admin polling is operator-driven" — true of the page
// handlers, but this is also on the SSE `sources` slow tick, where the
// connection ctx is exactly the signal that matters.
func (a *upnpAdminAdapter) ConfiguredServers(ctx context.Context) []admin.UPnPUpstreamServerState {
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
			// The three editable YAML fields, echoed verbatim so the
			// admin edit modal prefills what is actually stored and a
			// save round-trips. Emitting the STORED value (not the
			// EffectiveRootObjectID() / normalizePrefix() fallback) is
			// deliberate: a blank here means "unset, let the loader
			// default it", and PATCHing back a materialised default
			// would freeze today's fallback into bridge.yaml.
			// Cloned, not aliased: the slice belongs to the live
			// config snapshot and this DTO escapes to handlers.
			PathPrefix:             srv.PathPrefix,
			RootObjectID:           srv.RootObjectID,
			SkipTopLevelContainers: slices.Clone(srv.SkipTopLevelContainers),
		}
		// Per-server runtime state (FriendlyName + RoutedTracks)
		// routes through the shared helper so the admin row and the
		// public /v1/health row can't diverge on the cache lookup
		// gate (UDN-only — manual entries don't get into the cache)
		// AND the StableServerKey count keying (UDN OR manual hash —
		// both routed through one column). Both paths now propagate a
		// real ctx so a client disconnect cancels the COUNT(*).
		// The admin DTO surfaces liveness via its own `Discovered` field
		// (cache.Get below), so the shared helper's `online` is discarded
		// here — only the public /v1/health DTO carries `online`.
		row.FriendlyName, row.RoutedTracks, _ = lookupUPnPServerRuntime(ctx, srv, a.cache, a.store)
		// The admin DTO carries operator-internal fields the public
		// DTO deliberately omits (Discovered / ResolvedUDN /
		// Manufacturer / ControlURL / LastSeenAt). FriendlyName is
		// already populated by the shared helper above — don't
		// overwrite it here.
		// Trim before the cache lookup so a config UDN with surrounding
		// whitespace (" uuid:123 ") doesn't miss the SSDP-clean cache key
		// and false-report Discovered=false. Mirrors the same trim
		// lookupUPnPServerRuntime already applies for the public
		// /v1/health row (Gemini on PR #362) — the admin path's parallel
		// cache.Get was missed by that fix.
		if trimmedUDN := strings.TrimSpace(srv.UDN); trimmedUDN != "" && a.cache != nil {
			if info, ok := a.cache.Get(trimmedUDN); ok {
				row.Discovered = true
				row.ResolvedUDN = info.UDN
				row.Manufacturer = info.Manufacturer
				row.ControlURL = info.ContentDirectoryControlURL
				row.LastSeenAt = info.LastSeenAt
			}
		}
		// Last-walk telemetry stays admin-only — the public DTO
		// deliberately excludes operator-internal walk error strings
		// + timing. Keyed on the SAME StableServerKey the routed-
		// track lookup inside the shared helper uses.
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
	// Trim first, matching RemoveServer/UpdateServer — findConfiguredIdx
	// compares against trimmed server fields, so an untrimmed identity
	// with surrounding whitespace would spuriously 404 (Gemini on PR #358).
	udn = strings.TrimSpace(udn)
	if udn != "" {
		// Match a configured server before we accept the request;
		// otherwise return the typed 404 the handler maps to. Use the
		// same UDN-or-ManualDescriptionURL identity lookup as
		// RemoveServer/UpdateServer — a srv.UDN-only loop 404s a
		// manual-URL-only server (empty UDN) the operator can otherwise
		// remove/update (Gemini HIGH on PR #353 fixed the sibling gates;
		// ForceRescan was the residual miss).
		if findConfiguredIdx(cfg, udn) < 0 {
			return admin.ErrUPnPNoSuchServer
		}
	}
	// Resolve the lifecycle run scope and refuse if graceful shutdown has
	// already begun. bgCtx is the PARENT of the periodic ingest loop's
	// tickCtx (context.WithCancel(ctx) in startUPnPUpstreamIfEnabled), so
	// once it's cancelled the periodic loop exits and drops its startup +1
	// on ingestWg toward 0 — a concurrent ingestWg.Add(1) below could then
	// race Stop's ingestWg.Wait() at the zero transition and panic
	// ("WaitGroup is reused before previous Wait has returned"). Refusing
	// the instant bgCtx.Err() is non-nil — before both the inFlight latch
	// and the Add — keeps the Add under the periodic loop's live +1.
	// Gemini medium on PR #524.
	bgCtx := a.bgCtx
	if bgCtx == nil {
		bgCtx = context.Background()
	} else if bgCtx.Err() != nil {
		return fmt.Errorf("cannot force-rescan: bridge is shutting down: %w", bgCtx.Err())
	}
	// Race-free acquire of the in-flight slot.
	a.state.mu.Lock()
	if a.state.inFlight {
		a.state.mu.Unlock()
		return admin.ErrUPnPRescanInFlight
	}
	a.state.inFlight = true
	a.state.mu.Unlock()

	// bgCtx was resolved above (before the shutdown gate). It's the
	// lifecycle run scope, NOT the request ctx, so a client disconnect /
	// reverse-proxy timeout can't abort the scan mid-tree; the lifecycle's
	// Stop cancels it during graceful shutdown so this goroutine drains
	// cleanly.
	//
	// Register the spawned walk on the lifecycle's ingest WaitGroup so
	// graceful shutdown (upnpUpstreamLifecycle.Stop → ingestWg.Wait)
	// joins it before the process tears down the manifest store. The
	// bgCtx.Err() gate above guarantees the periodic ingest loop still
	// holds its startup +1 here (it drops that only after bgCtx cancels),
	// so this Add lifts the counter from ≥1, never from zero — the same
	// bounded-goroutine contract as admin's bgScans WaitGroup. Nil-guard
	// for the direct-construction unit tests. Add(1) is paired with the
	// deferred Done() inside the goroutine below.
	if a.ingestWg != nil {
		a.ingestWg.Add(1)
	}
	go func() {
		if a.ingestWg != nil {
			defer a.ingestWg.Done()
		}
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
//
// `cfgPath` is the bridge.yaml path the CRUD methods (Add / Remove /
// Update) persist to via `Config.Save`. Empty path is non-fatal —
// the CRUD methods surface a typed save error rather than crashing.
func (l *upnpUpstreamLifecycle) installAdminAdapter(ctx context.Context, cfgHolder *config.RuntimeConfig, cfgPath string, store *manifest.Store, ingester *upnpingest.Ingester) admin.UPnPUpstreamProvider {
	if l == nil || l.cache == nil || ingester == nil {
		return nil
	}
	if l.adminState == nil {
		l.adminState = newUPnPAdminState()
	}
	return &upnpAdminAdapter{
		cfgHolder: cfgHolder,
		cfgPath:   cfgPath,
		cache:     l.cache,
		ingester:  ingester,
		store:     store,
		state:     l.adminState,
		bgCtx:     ctx,
		ingestWg:  &l.ingestWg,
	}
}

// DiscoveredServers returns SSDP-cached MediaServers NOT in the
// operator-configured list — candidates the admin UI can prefill into
// the add form. Reads cfg + the SSDP cache (both lock-protected),
// builds a configured-UDN set for fast exclusion, then sorts by
// friendlyName ASC for stable UI rendering.
//
// Manual-only entries (configured via ManualDescriptionURL, no UDN)
// don't appear in the SSDP cache by definition — they're not subject
// to this exclusion path; the discovery list just doesn't include
// them. The configured list is the authoritative surface for them.
func (a *upnpAdminAdapter) DiscoveredServers() []admin.UPnPDiscoveredServer {
	cfg := a.cfgHolder.Load()
	if cfg == nil || a.cache == nil {
		return nil
	}
	configuredUDNs := make(map[string]struct{}, len(cfg.UPnPUpstream.Servers))
	for _, srv := range cfg.UPnPUpstream.Servers {
		if udn := strings.TrimSpace(srv.UDN); udn != "" {
			configuredUDNs[udn] = struct{}{}
		}
	}
	cache := a.cache.Snapshot()
	out := make([]admin.UPnPDiscoveredServer, 0, len(cache))
	for _, info := range cache {
		if _, configured := configuredUDNs[info.UDN]; configured {
			continue
		}
		out = append(out, admin.UPnPDiscoveredServer{
			UDN:              info.UDN,
			FriendlyName:     info.FriendlyName,
			Manufacturer:     info.Manufacturer,
			ModelName:        info.ModelName,
			ModelDescription: info.ModelDescription,
			ControlURL:       info.ContentDirectoryControlURL,
			LastSeenAt:       info.LastSeenAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FriendlyName != out[j].FriendlyName {
			return out[i].FriendlyName < out[j].FriendlyName
		}
		return out[i].UDN < out[j].UDN
	})
	return out
}

// AddServer validates the request, then atomically (under the
// holder's single write lock via CfgHolder.Update) clones the live
// config, duplicate-checks + appends the new server entry, runs
// NormalizeAndValidate (catches the upnpUpstream-specific shape rules
// + the cross-section ones), persists via `Config.Save`, and swaps
// the live snapshot. On any failure before Save, the clone is
// discarded.
//
// Restart semantics: the upnpUpstreamLifecycle constructs its
// Ingester at startup from the configured server list, so a runtime
// add doesn't begin ingesting tracks until the next restart. The
// admin handler surfaces this via `restartRequired: true` in the
// response — same precedent as the Sox / DLNA / Tailscale-mode
// switches.
func (a *upnpAdminAdapter) AddServer(_ context.Context, req admin.UPnPServerAddRequest) error {
	if a.cfgHolder.Load() == nil {
		return fmt.Errorf("%w: config not loaded", admin.ErrUPnPValidation)
	}
	name := strings.TrimSpace(req.Name)
	udn := strings.TrimSpace(req.UDN)
	manualURL := strings.TrimSpace(req.ManualDescriptionURL)
	if name == "" {
		return fmt.Errorf("%w: name is required", admin.ErrUPnPValidation)
	}
	if udn == "" && manualURL == "" {
		return fmt.Errorf("%w: either udn or manualDescriptionURL is required", admin.ErrUPnPValidation)
	}
	return a.updateCfg(func(next *config.Config) error {
		// Duplicate-identity check against existing rows. UDN match is
		// the strong identity; ManualDescriptionURL match is the weak
		// identity for SSDP-unreachable servers. A new row colliding on
		// either is rejected with 409. Runs on the locked clone so a
		// concurrent add can't slip the same identity past the check.
		for _, existing := range next.UPnPUpstream.Servers {
			if udn != "" && strings.TrimSpace(existing.UDN) == udn {
				return fmt.Errorf("%w: %q is already configured", admin.ErrUPnPDuplicateUDN, udn)
			}
			if manualURL != "" && strings.TrimSpace(existing.ManualDescriptionURL) == manualURL {
				return fmt.Errorf("%w: %q is already configured", admin.ErrUPnPDuplicateUDN, manualURL)
			}
		}
		next.UPnPUpstream.Servers = append(next.UPnPUpstream.Servers, config.UPnPUpstreamServerConfig{
			Name:                   name,
			UDN:                    udn,
			ManualDescriptionURL:   manualURL,
			PathPrefix:             strings.TrimSpace(req.PathPrefix),
			RootObjectID:           strings.TrimSpace(req.RootObjectID),
			SkipTopLevelContainers: sanitizeSkipList(req.SkipTopLevelContainers),
		})
		return nil
	})
}

// RemoveServer drops the configured entry matching the UDN. Match is
// strict (trimmed-exact); manual-only entries (no UDN configured) can
// be removed by passing their ManualDescriptionURL as the udn arg —
// admin layer treats UDN as opaque identity, the adapter just needs
// to find the matching row.
//
// **Manifest tracks the removed server contributed are NOT swept**
// here. The ingester's orphan sweep (`reapOrphanServers`, run at the
// top of every ingest tick — hourly plus on startup) reaps every
// routing row whose server_udn no longer matches a configured server.
// Doing the sweep here would tie up the admin goroutine on a manifest
// write that may be holding other work.
func (a *upnpAdminAdapter) RemoveServer(_ context.Context, udn string) error {
	udn = strings.TrimSpace(udn)
	if a.cfgHolder.Load() == nil || udn == "" {
		return admin.ErrUPnPNoSuchServer
	}
	return a.updateCfg(func(next *config.Config) error {
		idx := findConfiguredIdx(next, udn)
		if idx < 0 {
			return admin.ErrUPnPNoSuchServer
		}
		next.UPnPUpstream.Servers = append(
			next.UPnPUpstream.Servers[:idx],
			next.UPnPUpstream.Servers[idx+1:]...,
		)
		return nil
	})
}

// UpdateServer edits the operator-visible fields of an existing row.
// Pointer-discriminator partial update: nil pointers preserve the
// current value (matches the apiSettingsPatch convention). UDN +
// ManualDescriptionURL are NOT editable (they're identity); to
// "rename" a server the operator removes + re-adds.
//
// Empty-string is a valid "clear this field" value for PathPrefix /
// RootObjectID — the YAML loader fills the defaults. An empty Name
// is a validation error (the operator-visible label can't be blank).
func (a *upnpAdminAdapter) UpdateServer(_ context.Context, udn string, req admin.UPnPServerUpdateRequest) error {
	udn = strings.TrimSpace(udn)
	if a.cfgHolder.Load() == nil || udn == "" {
		return admin.ErrUPnPNoSuchServer
	}
	return a.updateCfg(func(next *config.Config) error {
		idx := findConfiguredIdx(next, udn)
		if idx < 0 {
			return admin.ErrUPnPNoSuchServer
		}
		row := &next.UPnPUpstream.Servers[idx]
		if req.Name != nil {
			name := strings.TrimSpace(*req.Name)
			if name == "" {
				return fmt.Errorf("%w: name must not be empty", admin.ErrUPnPValidation)
			}
			row.Name = name
		}
		if req.PathPrefix != nil {
			row.PathPrefix = strings.TrimSpace(*req.PathPrefix)
		}
		if req.RootObjectID != nil {
			row.RootObjectID = strings.TrimSpace(*req.RootObjectID)
		}
		if req.SkipTopLevelContainers != nil {
			row.SkipTopLevelContainers = sanitizeSkipList(*req.SkipTopLevelContainers)
		}
		return nil
	})
}

// updateCfg is the shared mutate→validate→persist tail used by every
// CRUD path (AddServer / RemoveServer / UpdateServer). fn runs under
// the holder's single write lock (CfgHolder.Update): it receives the
// fresh clone of the live snapshot, mutates it, and — after fn
// succeeds — the helper validates the result against the cross-section
// rules in `Config.NormalizeAndValidate`, and Update persists via
// `Config.Save` + atomically swaps the live snapshot. Because the
// clone→validate→Save→Store sequence is serialized with every OTHER
// config writer in the process (admin settings/roots/variants-dir
// handlers), a concurrent mutation can't be silently dropped by the
// last Save (2026-07-21 review finding M13).
//
// fn rejections and validation failures carry the admin sentinel
// wrapping (ErrUPnPValidation / ErrUPnPDuplicateUDN /
// ErrUPnPNoSuchServer); anything else out of Update is the Save
// failure and gets the "save bridge.yaml" wrapping. Extracted to drop
// SonarCloud duplication on PR #357 round-2 (as persistCfg; reworked
// to the Update contract for M13).
func (a *upnpAdminAdapter) updateCfg(fn func(*config.Config) error) error {
	if a.cfgPath == "" {
		return fmt.Errorf("save bridge.yaml: cfgPath not wired")
	}
	err := a.cfgHolder.Update(a.cfgPath, func(next *config.Config) error {
		if err := fn(next); err != nil {
			return err
		}
		if err := next.NormalizeAndValidate(); err != nil {
			return fmt.Errorf("%w: %v", admin.ErrUPnPValidation, err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, admin.ErrUPnPValidation) ||
			errors.Is(err, admin.ErrUPnPDuplicateUDN) ||
			errors.Is(err, admin.ErrUPnPNoSuchServer) {
			return err
		}
		return fmt.Errorf("save bridge.yaml: %w", err)
	}
	return nil
}

// findConfiguredIdx scans `cfg.UPnPUpstream.Servers` for a row whose
// UDN OR ManualDescriptionURL matches the supplied identity (trimmed
// exact). Returns -1 when no row matches. Extracted to drop
// duplication between RemoveServer + UpdateServer.
func findConfiguredIdx(cfg *config.Config, identity string) int {
	for i, srv := range cfg.UPnPUpstream.Servers {
		if strings.TrimSpace(srv.UDN) == identity || strings.TrimSpace(srv.ManualDescriptionURL) == identity {
			return i
		}
	}
	return -1
}

// sanitizeSkipList trims whitespace + drops empty entries from a
// SkipTopLevelContainers payload. Returns nil when the resulting slice
// would be empty so the YAML round-trip emits the field as omitted
// (cleaner output for the common case).
func sanitizeSkipList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Keep linter happy about the imported time package if the adapter
// stops using it later.
var _ = time.Time{}
