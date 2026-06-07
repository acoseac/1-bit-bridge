package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnpproxy"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// dlnaLifecycle wraps DLNA MediaServer construction, start, and stop so
// `runServe` in main.go can hand off all DLNA orchestration with a
// single call. nil-safe: every method short-circuits when the
// underlying server isn't running (loopback-but-disabled / public-mode
// refusal / setup-failed) — call sites don't need explicit nil checks.
type dlnaLifecycle struct {
	server *dlna.Server
	log    *slog.Logger
}

// startDLNAIfEnabled is the single chokepoint for DLNA wiring. Routes
// the operator's `cfg.DLNA.Enabled` + deployment-mode posture through
// `dlna.ShouldEnableDLNA` (the load-bearing safety gate that refuses
// in public mode), then constructs + starts the server.
//
// Returns a nil-safe *dlnaLifecycle in all paths — caller defers
// `.Stop(ctx)` without a nil check. Failure to start (port in use,
// SSDP bind error) is logged at WARN level and the bridge continues
// without DLNA; this matches the project's "additive features fail
// open" convention (lossy fallback, never crash the bridge over an
// optional capability).
//
// The returned `enabled` bool tells the api.Server wiring whether to
// advertise `dlnaServer` in /v1/health.features.
func startDLNAIfEnabled(
	ctx context.Context,
	cfg *config.Config,
	store *manifest.Store,
	resolver *bridgefs.Resolver,
	upnpLC *upnpUpstreamLifecycle,
	logger *slog.Logger,
) (lc *dlnaLifecycle, enabled bool) {
	// Deployment posture → typed mode.
	var mode dlna.DeploymentMode
	if cfg.IsPublic() {
		mode = dlna.DeploymentPublic
	} else {
		mode = dlna.DeploymentLoopback
	}
	dlnaCfg := dlna.DLNAConfig{Enabled: cfg.DLNA.Enabled}
	ok, reason := dlna.ShouldEnableDLNA(dlnaCfg, mode)
	if !ok {
		// Only log at .info level when the operator explicitly
		// asked for DLNA — silently skipping the unconfigured
		// default is the right default behaviour.
		if cfg.DLNA.Enabled {
			logger.Warn("DLNA refused", slog.String("reason", reason))
		}
		return &dlnaLifecycle{log: logger.With(slog.String("component", "dlna"))}, false
	}

	dlnaLog := logger.With(slog.String("component", "dlna"))

	// Pick a LAN-eligible interface for SSDP multicast. Tsnet
	// binding deferred: `cfg.DLNA.AllowTsnet` is honoured at the
	// admin-config layer but the Eligibility picker needs the live
	// tsnet interface name (resolved from the tsnet.Server's bound
	// listener at startup) which lives outside this helper's scope.
	// v1 ships LAN-only by design — Phase 0 confirmed SSDP doesn't
	// traverse the Tailscale tunnel, so the cross-network path is
	// PR 5+6's bridge-mediated discovery rather than tsnet-bound
	// SSDP. Leaving `TsnetIfaceName` empty here keeps the picker
	// on LAN-only interfaces; a future PR threading the tsnet
	// interface name through will set this field per operator opt-in.
	iface, ifaceErr := dlna.PickLANEligibleInterface(dlna.EligibilityOpts{})
	if ifaceErr != nil {
		dlnaLog.Warn("DLNA disabled — no LAN-eligible interface", slog.String("err", ifaceErr.Error()))
		return &dlnaLifecycle{log: dlnaLog}, false
	}

	listenAddr := cfg.DLNA.EffectiveDLNAListenAddress()
	port := portFromListenAddr(listenAddr)
	rawHost, _, splitErr := net.SplitHostPort(listenAddr)
	bindWildcard := splitErr != nil || rawHost == "" || rawHost == "0.0.0.0" || rawHost == "::"

	// Multi-interface SSDP advertise set (wildcard bind only; extracted
	// to keep this function's cognitive complexity in check — Sonar S3776
	// on PR #328). Gathered BEFORE the fallback ServerURL so an IPv6-only
	// FIRST interface can't false-bail startup when a LATER interface has
	// a usable IPv4 (CodeRabbit on PR #328).
	endpoints := gatherAdvertiseEndpoints(bindWildcard, port, dlnaLog)

	// Resolve the fallback ServerURL renderers dial for /dlna/file/ + SOAP
	// control. Prefer a gathered endpoint's IP (any interface with IPv4),
	// then a pinned bind host, then the picked interface's IPv4 as a last
	// resort. DLNA disables only when NONE yield a dialable host.
	serverURL, urlErr := deriveServerURL(rawHost, bindWildcard, port, endpoints, iface)
	if urlErr != nil {
		dlnaLog.Warn("DLNA disabled — couldn't resolve a dialable server URL",
			slog.String("iface", iface.Name),
			slog.String("err", urlErr.Error()))
		return &dlnaLifecycle{log: dlnaLog}, false
	}

	// UDN derived from a hash of (DataDir, FriendlyName) — stable
	// across restarts so renderers don't re-add us, distinct between
	// two bridges on the same LAN (different DataDir).
	udn := deriveDLNAUDN(cfg.DataDir, cfg.DLNA.EffectiveDLNAFriendlyName())

	// Telemetry ring buffer (1000 entries ≈ 200 KB) unless operator
	// opted out. Optional in production; load-bearing for vendor-
	// profile refinement during early field rollout.
	var telemetry *dlna.TelemetryStore
	if cfg.DLNA.EffectiveDLNATelemetryEnabled() {
		telemetry = dlna.NewTelemetryStore(0) // 0 → DefaultTelemetryCapacity
	}

	library := newManifestLibraryAdapter(store, resolver, cfg.LibraryRoots, dlnaLog)

	// UPnP-routed file-handler fast-path: when the upnpUpstream feature is
	// wired AND its SSDP discovery has a host resolver to hand out, the
	// dlna file handler proxies upstream bytes (e.g. from a Chord 2Go)
	// instead of 404'ing on a non-existent local file. The `manifestStore`
	// implements `upnpproxy.RoutingLookup` via its `GetUPnPRouting`; we
	// pair that with a `*upnpproxy.Proxy` constructed from the upstream
	// lifecycle's host resolver. Both nil → legacy filesystem-only
	// behaviour. Per the post-pair-A operator verification of PR #732.
	var dlnaUPnPRouting upnpproxy.RoutingLookup
	var dlnaUPnPProxy *upnpproxy.Proxy
	if upnpLC != nil {
		if hr := upnpLC.HostResolver(); hr != nil {
			dlnaUPnPRouting = store
			dlnaUPnPProxy = upnpproxy.New(hr, dlnaLog)
		}
	}

	srv, err := dlna.NewServer(dlna.ServerConfig{
		Library:            library,
		UPnPRouting:        dlnaUPnPRouting,
		UPnPProxy:          dlnaUPnPProxy,
		UDN:                udn,
		FriendlyName:       cfg.DLNA.EffectiveDLNAFriendlyName(),
		Manufacturer:       "1-bit",
		ManufacturerURL:    "https://1-bit.app",
		ModelDescription:   "1-bit Bridge DLNA MediaServer",
		ModelName:          "1-bit-bridge",
		ModelNumber:        version.ServerVersion,
		ListenAddress:      listenAddr,
		ServerURL:          serverURL,
		Interface:          iface,
		AdvertiseEndpoints: endpoints,
		TelemetryStore:     telemetry,
		Logger:             dlnaLog,
	})
	if err != nil {
		dlnaLog.Warn("DLNA disabled — NewServer failed", slog.String("err", err.Error()))
		return &dlnaLifecycle{log: dlnaLog}, false
	}

	if err := srv.Start(ctx); err != nil {
		dlnaLog.Warn("DLNA disabled — Start failed", slog.String("err", err.Error()))
		// Defensive teardown — `dlna.Server.Start` cleans up its own
		// partial bind on failure (HTTP listener torn down if SSDP
		// fails mid-init), but a future implementation might miss
		// a step. Calling Stop on a partially-initialized server is
		// a guaranteed no-op (Stop is idempotent + nil-safe), so
		// the cost is zero and the safety net is real. Per
		// CodeRabbit Major on PR #303.
		stopCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		_ = srv.Stop(stopCtx)
		cancel()
		return &dlnaLifecycle{log: dlnaLog}, false
	}

	dlnaLog.Info("DLNA MediaServer started",
		slog.String("listenAddress", listenAddr),
		slog.String("serverURL", serverURL),
		slog.String("interface", iface.Name),
		slog.String("udn", udn),
		slog.String("friendlyName", cfg.DLNA.EffectiveDLNAFriendlyName()))

	return &dlnaLifecycle{server: srv, log: dlnaLog}, true
}

// Stop tears down the DLNA server with the given context deadline.
// Safe to call on a nil-server lifecycle (the disabled / failed-to-
// start paths return a zero-value lifecycle).
func (d *dlnaLifecycle) Stop(ctx context.Context) {
	if d == nil || d.server == nil {
		return
	}
	if err := d.server.Stop(ctx); err != nil {
		d.log.Warn("DLNA shutdown error", slog.String("err", err.Error()))
	}
}

// gatherAdvertiseEndpoints builds the per-interface SSDP advertise set.
// When the HTTP listener binds wildcard (the common ":7790" /
// "0.0.0.0:7790" case) it announces on EVERY LAN-eligible interface with
// a per-interface LOCATION URL so a renderer on a secondary subnet
// (Ethernet + Wi-Fi, bridged hosts) can both discover us AND reach the
// description / file URLs from its own subnet. A pinned bind host targets
// a single interface by definition, so it returns nil and the server
// falls back to its single-advertiser path (Interface + ServerURL).
//
// Interfaces that are LAN-eligible but carry no usable unicast IPv4
// (IPv6-only, or link-local-only at this instant) are skipped rather than
// advertised with an unreachable "http://<nil>:port" LOCATION.
func gatherAdvertiseEndpoints(bindWildcard bool, port int, log *slog.Logger) []dlna.AdvertiseEndpoint {
	if !bindWildcard {
		return nil
	}
	var endpoints []dlna.AdvertiseEndpoint
	for _, ai := range dlna.PickAllLANEligibleInterfaces(dlna.EligibilityOpts{}) {
		ip, ipErr := firstIPv4OnInterface(ai)
		if ipErr != nil {
			log.Debug("DLNA SSDP: skipping interface with no usable IPv4",
				slog.String("iface", ai.Name),
				slog.String("err", ipErr.Error()))
			continue
		}
		endpoints = append(endpoints, dlna.AdvertiseEndpoint{
			Interface: ai,
			ServerURL: fmt.Sprintf("http://%s:%d", ip.String(), port),
		})
	}
	return endpoints
}

// deriveServerURL resolves the fallback ServerURL the bridge advertises
// (LOCATION's host on the single-advertiser path, and the serverURLFunc
// fallback when a renderer's Host header is empty). Priority:
//
//  1. Pinned bind host ("192.168.1.42:7790") → advertise exactly that.
//  2. Wildcard bind with ≥1 gathered endpoint → the first endpoint's URL
//     (its IP is already a usable LAN IPv4; this is what lets an IPv6-only
//     FIRST interface not block startup — CodeRabbit on PR #328).
//  3. Wildcard bind, no endpoints → the picked interface's IPv4 as a last
//     resort; errors only when even that has no usable IPv4.
func deriveServerURL(rawHost string, bindWildcard bool, port int, endpoints []dlna.AdvertiseEndpoint, iface *net.Interface) (string, error) {
	if !bindWildcard {
		return fmt.Sprintf("http://%s:%d", rawHost, port), nil
	}
	if len(endpoints) > 0 {
		return endpoints[0].ServerURL, nil
	}
	ip, err := firstIPv4OnInterface(iface)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://%s:%d", ip.String(), port), nil
}

// firstIPv4OnInterface returns the first IPv4 address bound to iface
// that is NOT loopback / unspecified. Returns an error when no
// suitable address is found — caller treats that as "DLNA can't
// advertise a usable URL, skip startup".
func firstIPv4OnInterface(iface *net.Interface) (net.IP, error) {
	if iface == nil {
		return nil, errors.New("nil interface")
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil {
			continue
		}
		v4 := ip.To4()
		if v4 == nil {
			continue
		}
		if v4.IsLoopback() || v4.IsUnspecified() {
			continue
		}
		return v4, nil
	}
	return nil, fmt.Errorf("no usable IPv4 address on interface %s", iface.Name)
}

// portFromListenAddr extracts the port number from a ":7790" or
// "0.0.0.0:7790"-shaped listen address. Returns 7790 on parse failure
// (matches the default and produces a usable ServerURL even if the
// operator's value is malformed; the listener itself will surface the
// real error).
func portFromListenAddr(addr string) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 7790
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil || port <= 0 {
		return 7790
	}
	return port
}

// deriveDLNAUDN produces a stable UPnP unique device name (UDN) for
// this bridge by hashing the data directory + friendly name. Format:
// "uuid:<8>-<4>-<4>-<4>-<12>". The hash content choice is deliberate:
// DataDir uniquely identifies a bridge install (operators don't run
// two bridges out of the same dir); FriendlyName provides a stable
// secondary tag so renaming a bridge re-registers it as a new device
// on the renderer side (intuitive behaviour for the operator —
// "renamed it, looks like a new device, that's expected").
func deriveDLNAUDN(dataDir, friendlyName string) string {
	h := sha256.Sum256([]byte(dataDir + "\x00" + friendlyName))
	hexStr := hex.EncodeToString(h[:16]) // 32 hex chars = 128 bits, matches UUID width
	return fmt.Sprintf("uuid:%s-%s-%s-%s-%s",
		hexStr[0:8], hexStr[8:12], hexStr[12:16], hexStr[16:20], hexStr[20:32])
}

// -----------------------------------------------------------------------------
// manifest.Store → dlna.LibrarySource adapter
// -----------------------------------------------------------------------------

// manifestLibraryAdapter implements dlna.LibrarySource on top of the
// bridge's existing manifest.Store + bridgefs.Resolver. Caches the
// per-library TrackInfo slice for `cacheTTL` so a back-to-back Browse
// burst from a single renderer doesn't re-walk the entire library on
// every SOAP call.
//
// Cache invalidation: time-based only (TTL). Mid-cache scans that add
// or remove tracks become visible to renderers within TTL — a 30 s
// window is below most renderers' patience for "where's that new
// track" complaints and well above the SOAP burst frequency.
type manifestLibraryAdapter struct {
	store        *manifest.Store
	resolver     *bridgefs.Resolver
	libraryRoots []string
	// Resolver-miss tracks backed by an upstream UPnP MediaServer (e.g.
	// a Chord 2Go's microSD card) are kept in the cache with an empty
	// `AbsolutePath` sentinel — the dlna file handler's upnp fast-path
	// proxies the bytes BEFORE `os.Open(servePath)` runs. Membership
	// resolved per rebuild from a single `AllUPnPRoutingPaths` bulk
	// read (previously a per-track `GetUPnPRouting` point query that
	// N+1'd under the 10 s rebuild context deadline — Gemini HIGH on
	// PR #356).
	log *slog.Logger

	// `mu` guards the cached* slots for reads (Snapshot path) and the
	// cachedAt timestamp. Brief — held only for the slot copy / read.
	mu           sync.Mutex
	cachedList   []dlna.TrackInfo
	cachedByID   map[string]dlna.TrackInfo
	cachedByPath map[string]dlna.TrackInfo // keyed on RelativePath (manifest Track.Path) for Search hit resolution
	cachedAt     time.Time
	// generation bumps on every cache rebuild. The DLNA folder-index
	// cache keys on it (via Generation()) so it rebuilds the folder tree
	// at most once per cache refresh rather than per Browse request.
	generation uint64

	// `rebuildMu` serializes the rebuild path. Without it, a SOAP
	// burst (Chord 2go opens 5+ concurrent SOAP connections on its
	// initial Browse cycle) would have every caller race into
	// `rebuild()` simultaneously and walk the full manifest in
	// parallel — defeating the cache on exactly the workload the
	// cache exists for. With it, exactly ONE goroutine does the
	// walk; the others wait briefly on the mutex then short-circuit
	// via the double-check at the top of rebuild(). Per Gemini
	// Medium + CodeRabbit Nitpick on PR #303.
	rebuildMu sync.Mutex
}

// cacheTTL is the freshness window for the TrackInfo cache. 30 s
// balances renderer freshness expectations vs the cost of re-walking
// `manifest.ListTracks` on every SOAP burst.
const cacheTTL = 30 * time.Second

func newManifestLibraryAdapter(
	store *manifest.Store,
	resolver *bridgefs.Resolver,
	libraryRoots []string,
	log *slog.Logger,
) *manifestLibraryAdapter {
	return &manifestLibraryAdapter{
		store:        store,
		resolver:     resolver,
		libraryRoots: libraryRoots,
		log:          log,
	}
}

// ListTrackInfos returns every track currently in the manifest as
// `dlna.TrackInfo` values. Returns nil on transient store error
// (logged); renderers handle empty Browse responses gracefully.
func (a *manifestLibraryAdapter) ListTrackInfos() []dlna.TrackInfo {
	a.refreshIfStale()
	a.mu.Lock()
	defer a.mu.Unlock()
	// Defensive copy — DLNA handlers run concurrently, mutating the
	// returned slice would race the cache slot.
	out := make([]dlna.TrackInfo, len(a.cachedList))
	copy(out, a.cachedList)
	return out
}

// TrackCount returns the number of tracks in the cached manifest view
// without copying the slice (cf. ListTrackInfos). Refreshes the cache on
// the same TTL as the other readers, then returns the cached slice length
// under the lock — never a DB COUNT(*) per call, never a ListTrackInfos
// round-trip.
func (a *manifestLibraryAdapter) TrackCount() int {
	a.refreshIfStale()
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.cachedList)
}

// GetTrackInfo resolves a TrackID back to its TrackInfo for the file
// handler. Returns (zero-value, false) when the ID isn't known.
func (a *manifestLibraryAdapter) GetTrackInfo(trackID string) (dlna.TrackInfo, bool) {
	a.refreshIfStale()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cachedByID == nil {
		return dlna.TrackInfo{}, false
	}
	ti, ok := a.cachedByID[trackID]
	return ti, ok
}

// refreshIfStale rebuilds the cache when the TTL has expired.
func (a *manifestLibraryAdapter) refreshIfStale() {
	a.mu.Lock()
	stale := time.Since(a.cachedAt) >= cacheTTL || a.cachedList == nil
	a.mu.Unlock()
	if !stale {
		return
	}
	a.rebuild()
}

func (a *manifestLibraryAdapter) rebuild() {
	// Stampede defence: only one goroutine walks the manifest at a
	// time. Concurrent callers serialize on rebuildMu; the
	// double-check after acquiring the mutex detects the case where
	// the previous rebuilder satisfied the freshness check before
	// we got the lock, in which case we skip the redundant walk
	// entirely. Per Gemini Medium + CodeRabbit Nitpick on PR #303.
	a.rebuildMu.Lock()
	defer a.rebuildMu.Unlock()

	// Double-check: another rebuilder may have completed while we
	// were parked on rebuildMu. Cheap read under the cache mutex —
	// the slot is fresh enough, we just return.
	a.mu.Lock()
	fresh := a.cachedList != nil && time.Since(a.cachedAt) < cacheTTL
	a.mu.Unlock()
	if fresh {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tracks, err := a.store.ListTracks(ctx, nil)
	if err != nil {
		a.log.Warn("manifest list failed", slog.String("err", err.Error()))
		return
	}

	// **Empty libraryRoot for DLNA TrackID hashing** — load-bearing
	// agreement with iOS PR 4 follow-up's `DLNATrackIDHasher`.
	// iOS doesn't know the operator's `cfg.LibraryRoots[0]` value
	// and we don't want to plumb it through `/v1/health` just for
	// trackID hashing. Both sides standardise on empty libraryRoot
	// so they agree on the hash without iOS having to query bridge
	// config. The trackID hash becomes `SHA256("\x00" + relativePath)`
	// regardless of how many library roots the operator configures
	// on the bridge side.
	//
	// **Trade-off**: trackID is no longer scoped by libraryRoot.
	// Single-root bridges (the canonical case) are unaffected —
	// every track has a unique relative path within the root.
	// Multi-root bridges would have collision risk for tracks
	// with identical relative paths under different roots — that
	// configuration is rejected from the DLNA path entirely via
	// the resolver: `a.resolver.Resolve(t.Path)` would surface
	// ambiguous paths as resolution errors and those tracks would
	// be skipped from the DLNA cache (existing `continue` on
	// `resolveErr` above).
	libraryRoot := ""

	// One bulk read of every offline variant row (sidecar paths +
	// metadata), grouped by source path. Cheaper than an N+1 LookupVariant
	// per track, and runs at most once per cache TTL. Failure is non-fatal:
	// we log and proceed with source-only DIDL (the feature degrades, the
	// CDS still serves originals).
	variantsBySource := map[string][]manifest.VariantRow{}
	if rows, vErr := a.store.AllVariants(ctx); vErr != nil {
		a.log.Warn("manifest variant list failed — DLNA serves source-only", slog.String("err", vErr.Error()))
	} else {
		for _, vr := range rows {
			variantsBySource[vr.SourcePath] = append(variantsBySource[vr.SourcePath], vr)
		}
	}

	// One bulk read of the upnp_track_routing path set. Pre-fix this was
	// a per-track `GetUPnPRouting` point query INSIDE the rebuild loop
	// (one round-trip per filesystem-miss track) — an N+1 under the
	// 10 s context deadline that reliably tripped the timeout on a 15k+
	// routed library, after which every subsequent lookup returned the
	// `ctx.DeadlineExceeded` error path and silently dropped the
	// remainder of the routed tracks from the cache. Now: one SELECT,
	// one map, O(1) per-track check downstream. Mirrors `AllVariants`
	// above and the variant-by-source map pattern. Failure is
	// non-fatal: we log, leave the set empty, and the rebuild proceeds
	// as if no routing exists (every routed track is then skipped at
	// the resolver-miss branch — same fallback the pre-fix
	// `isUPnPRouted` returning false produced on store error). Per
	// Gemini HIGH on PR #356.
	routedPaths := map[string]struct{}{}
	if paths, rErr := a.store.AllUPnPRoutingPaths(ctx); rErr != nil {
		a.log.Warn("manifest UPnP routing list failed — routed tracks skipped this rebuild",
			slog.String("err", rErr.Error()))
	} else {
		for _, p := range paths {
			routedPaths[p] = struct{}{}
		}
	}

	list := make([]dlna.TrackInfo, 0, len(tracks))
	byID := make(map[string]dlna.TrackInfo, len(tracks))
	byPath := make(map[string]dlna.TrackInfo, len(tracks))
	for _, t := range tracks {
		absPath, resolveErr := a.resolver.Resolve(t.Path)
		if resolveErr != nil {
			// Filesystem resolver miss. If this track is backed by
			// an upstream UPnP MediaServer (upnp_track_routing has a
			// row, looked up via the pre-built routedPaths map),
			// include it with an empty `AbsolutePath` so the dlna
			// file handler's upnp fast-path can take over. `os.Open("")`
			// would never be reached for these tracks because the
			// fast-path returns before the filesystem-serve branch.
			// Without this, casting a 2Go-routed track to any DLNA
			// renderer silently 404'd (bug surfaced by the post-pair-A
			// operator verification of PR #732 — see internal/upnpproxy
			// package docblock).
			if _, routed := routedPaths[t.Path]; !routed {
				continue
			}
			absPath = ""
		}
		ti := manifestTrackToDLNATrackInfo(t, absPath, libraryRoot)
		ti.Variants = dlnaVariantsFromRows(variantsBySource[t.Path])
		list = append(list, ti)
		byID[ti.TrackID] = ti
		// Key on RelativePath (== manifest Track.Path) so Search hits —
		// which the FTS index returns as paths — resolve to full TrackInfo.
		byPath[ti.RelativePath] = ti
	}

	a.mu.Lock()
	a.cachedList = list
	a.cachedByID = byID
	a.cachedByPath = byPath
	a.cachedAt = time.Now()
	a.generation++ // signal the DLNA folder-index cache to rebuild
	a.mu.Unlock()
}

// SearchTrackInfos runs the FTS5-backed library search and resolves each
// ranked path hit to its full cached TrackInfo (so DIDL <res> elements
// carry size / duration / sample-rate). Ranked order from the FTS query
// is preserved. Unresolved hits (a path the cache doesn't carry — e.g. a
// track whose resolver.Resolve failed during the last rebuild) are
// skipped. Returns nil on FTS unavailability / store error (logged) so
// the Search action degrades to "no matches" rather than faulting.
func (a *manifestLibraryAdapter) SearchTrackInfos(ctx context.Context, query string) []dlna.TrackInfo {
	a.refreshIfStale()
	// Honour the caller's deadline / cancellation, but bound the query so
	// a missing request deadline can't hang the rebuild lock indirectly.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// 500 = the store's hard cap; gives Browse-style pagination room to
	// page deep into a large result set rather than the 50-row default
	// (gemini-code-assist on PR #329).
	hits, err := a.store.SearchTracks(ctx, query, 500)
	if err != nil {
		a.log.Warn("DLNA search failed", slog.String("err", err.Error()))
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]dlna.TrackInfo, 0, len(hits))
	for _, h := range hits {
		if ti, ok := a.cachedByPath[h.Path]; ok {
			out = append(out, ti)
		}
	}
	return out
}

// Generation returns a counter that advances on every cache rebuild, so
// the DLNA folder-index cache can detect when the track list has moved.
// Triggers a stale-refresh first so the returned value reflects the
// freshest state (a stale value would let the folder cache serve a
// pre-rescan tree).
func (a *manifestLibraryAdapter) Generation() uint64 {
	a.refreshIfStale()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.generation
}

// manifestTrackToDLNATrackInfo flattens the pointer-typed manifest.Track
// optionals into the value-typed dlna.TrackInfo fields, computes the
// stable TrackID, and derives the file extension.
//
// Pure-helper-style — extracted so the adapter's rebuild path is
// short + easy to unit-test in isolation.
func manifestTrackToDLNATrackInfo(t manifest.Track, absPath, libraryRoot string) dlna.TrackInfo {
	ti := dlna.TrackInfo{
		TrackID:      dlna.TrackID(libraryRoot, t.Path),
		AbsolutePath: absPath,
		// `t.Path` IS the library-root-relative path the bridge
		// manifest stores (e.g. "Artist/Album/track.flac"). Surfaced
		// to the DLNA layer for folder-hierarchy derivation in
		// `BuildFolderIndex`. Distinct from `absPath` (which the
		// resolver computed against the configured library root)
		// because the DLNA layer doesn't know the library-root
		// prefix and shouldn't have to compute LCP to recover it.
		// Per PR #316.
		RelativePath:  t.Path,
		Title:         t.Title,
		Artist:        t.Artist,
		AlbumArtist:   t.AlbumArtist,
		Album:         t.Album,
		Composer:      t.Composer,
		Genre:         t.Genre,
		Codec:         t.Codec,
		FileExtension: strings.ToLower(filepath.Ext(absPath)),
		Size:          t.Size,
	}
	if t.Duration != nil {
		ti.DurationSeconds = *t.Duration
	}
	if t.SampleRate != nil {
		ti.SampleRateHz = int(*t.SampleRate)
	}
	if t.BitsPerSample != nil {
		ti.BitsPerSample = *t.BitsPerSample
	}
	if t.IsDSD != nil {
		ti.IsDSD = *t.IsDSD
	}
	if t.Year != nil {
		ti.Year = *t.Year
	}
	if t.TrackNumber != nil {
		ti.TrackNumber = *t.TrackNumber
	}
	// Channels not surfaced by manifest.Track today — left as 0;
	// DIDLForTrack treats 0 as "unknown" and omits the attribute.
	return ti
}

// dlnaVariantsFromRows converts manifest variant rows into the DLNA-local
// VariantInfo shape, pre-resolving the on-disk sidecar path (SidecarPath
// is authoritative — never reconstructed from a filename pattern) so the
// file handler serves the variant without a manifest DB query. The
// extension is derived from the recorded Format (variants are FLAC today),
// falling back to the sidecar path's extension.
func dlnaVariantsFromRows(rows []manifest.VariantRow) []dlna.VariantInfo {
	if len(rows) == 0 {
		return nil
	}
	out := make([]dlna.VariantInfo, 0, len(rows))
	for _, r := range rows {
		ext := ""
		if r.Format != "" {
			ext = "." + strings.ToLower(r.Format)
		} else {
			ext = strings.ToLower(filepath.Ext(r.SidecarPath))
		}
		out = append(out, dlna.VariantInfo{
			VariantID:     r.VariantID,
			AbsolutePath:  r.SidecarPath,
			FileExtension: ext,
			Size:          r.SizeBytes,
			BitDepth:      r.BitsPerSample,
			SampleRate:    r.SampleRate,
		})
	}
	return out
}

// Compile-time interface assertion.
var _ dlna.LibrarySource = (*manifestLibraryAdapter)(nil)
