package upnpingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
)

var logger = logging.Component("upnpingest")

// ServerResolver looks up a configured server's live ContentDirectory
// controlURL. The production implementation consults the
// upnp.MediaServerDiscoveryClient's ServerCache; tests pass a stub.
type ServerResolver interface {
	// ResolveControlURL returns the live controlURL for the configured
	// server entry. Returns "" + nil when the server is not currently
	// discoverable (the caller logs + skips that server for this tick;
	// the next tick re-tries).
	ResolveControlURL(ctx context.Context, server config.UPnPUpstreamServerConfig) (string, error)
}

// UpdateIDStore stores + retrieves the last-known SystemUpdateID per
// server. Pluggable so the production wiring (a small SQLite table or
// even a JSON file in dataDir) is decoupled from the ingest loop; tests
// pass an in-memory map.
type UpdateIDStore interface {
	Get(udnOrKey string) (string, bool)
	Set(udnOrKey, id string, lastWalkedAt time.Time)
	LastWalkedAt(udnOrKey string) (time.Time, bool)
}

// IngestResult summarizes a single Run call (one pass over all configured
// servers). Counts are per-server in the per-server fields and aggregated
// in the top-level fields.
type IngestResult struct {
	PerServer []ServerIngestResult

	// OrphanServersReaped / OrphanTracksReaped count the removed-server
	// sweep: routing rows whose server_udn no longer matches any
	// configured server (the operator removed the upstream). Routed
	// rows are invisible to the fs scanner's missing pass (PR #370),
	// so without this sweep a removed server's tracks would stay in
	// the manifest forever, syncing to iOS and 503ing on every play.
	OrphanServersReaped int
	OrphanTracksReaped  int
	// OrphanSweepErr carries a sweep failure without failing the whole
	// Run — the per-server ingest still proceeds; the next tick retries.
	OrphanSweepErr error
}

// ServerIngestResult is the per-server outcome.
type ServerIngestResult struct {
	Name string // operator-visible label from config
	// StableKey is StableServerKey(srv) for the CONFIGURED server this
	// result belongs to — the same key the admin surface looks results up
	// by. It must not be confused with ServerUDN: for a manually
	// configured server (no `udn:`, only `manualDescriptionUrl:`)
	// StableKey is "manual:<sha256(url)>" while ServerUDN is whatever UDN
	// the device reported at walk time, and for a UDN-configured server
	// StableKey is LOWERCASED while ServerUDN is raw. Keying telemetry by
	// ServerUDN therefore missed the lookup in both cases and the admin
	// console silently showed no last-walk info.
	StableKey       string
	ServerUDN       string // resolved at run time (empty when unresolved)
	Skipped         bool   // true when GetSystemUpdateID matched + within backstop window
	SkipReason      string // human-readable hint when Skipped is true
	Walked          int    // tracks yielded by the walker
	Unchanged       int    // of Walked: skip-if-unchanged hits (track upsert skipped; routing still refreshed)
	Reaped          int    // tracks deleted by the reconcile sweep
	Err             error  // non-nil aborts JUST this server; siblings still run
	WalkStartedAt   time.Time
	WalkCompletedAt time.Time
}

// Options gates ingest behaviour. The defaults match the
// CLAUDE.md-documented design: a ~24 h time backstop ensures the walker
// runs at least once a day even if GetSystemUpdateID never moves.
type Options struct {
	// ForceWalk skips the GetSystemUpdateID-equality short circuit and
	// always walks. Used by an operator's "rescan now" admin button.
	ForceWalk bool

	// TimeBackstop is the maximum interval between walks for any one
	// server regardless of GetSystemUpdateID. Defaults to 24 h. A
	// zero value falls back to the default; pass a negative to
	// disable the backstop entirely (tests).
	TimeBackstop time.Duration

	// Now overrides the wall clock (tests).
	Now func() time.Time

	// MaxItems caps the total tracks the walker yields per server before
	// it returns ErrWalkTruncated. Zero falls back to the walker's
	// built-in default (50k). This is currently a TEST SEAM ONLY — it
	// exercises the truncation path cheaply. It is NOT yet surfaced in
	// config (UPnPUpstreamConfig), so production Run() calls always pass
	// 0 (= the 50k default); wiring it to config is a separate follow-up.
	MaxItems int
}

const defaultTimeBackstop = 24 * time.Hour

// minPlausibleWalkFraction is the share of the stored baseline a walk must
// reach before its result is trusted as authoritative for the reconcile
// sweep. A walk that yielded LESS than this is treated as a partial view of
// the upstream and MUST NOT drive deletions — see reapAuthorized.
//
// Deliberately loose (half the library): the guard exists to catch the
// catastrophic shapes (an empty root, a whole subtree that browsed empty),
// not to second-guess ordinary churn. Any legitimate bulk deletion larger
// than this still reaps once implausibleWalkGrace has elapsed, so a
// too-loose threshold costs stale rows for a while and a too-tight one
// would let the treadmill through.
const minPlausibleWalkFraction = 0.5

// implausibleWalkGrace bounds how long a server may keep reporting an
// implausibly small library before the reconcile sweep is allowed to act on
// it anyway. Without it a genuinely-emptied (or bulk-pruned) upstream would
// keep its rows FOREVER: the baseline is re-read from the store on every
// walk, so the same suspicious ratio would recur on every tick and the
// operator's only recourse would be removing the server from config.
//
// This is elapsed WALL TIME, not a tick count, on purpose: the admin
// "Rescan now" button calls Run(ForceWalk) directly, so a count-based
// debounce could be burned through with three clicks during exactly the
// upstream DB rebuild it is meant to survive. It matches the default
// ScanIntervalSec (6h), so in the default configuration this reads as "at
// least one extra tick"; the first observation never reaps regardless of
// the clock, so a fast-ticking bridge still gets a real time window.
const implausibleWalkGrace = 6 * time.Hour

// Ingester orchestrates one pass per scan tick. Construct once + reuse
// for the lifetime of the bridge.
type Ingester struct {
	cfg       config.UPnPUpstreamConfig
	cdsClient *upnp.ContentDirectoryClient
	resolver  ServerResolver
	store     *manifest.Store
	idStore   UpdateIDStore

	// runMu serialises Run: the periodic lifecycle tick and the admin
	// ForceRescan goroutine share one Ingester, and two overlapping
	// walks of the same server race their reconcile sweeps — the walk
	// with the later walkStart can reap rows the earlier walk wrote
	// after the later walk passed that path (last_seen_at < the later
	// cutoff). A queued second run is cheap: the SystemUpdateID skip
	// gate short-circuits it unless the upstream actually changed.
	runMu sync.Mutex

	// implausibleSince records, per StableServerKey, when the CURRENT run
	// of implausible walks began (see reapAuthorized). Cleared as soon as a
	// walk looks plausible again, so it only holds entries for servers
	// actively reporting a suspect library shape.
	//
	// Guarded by runMu: every access is inside ingestOne, which is only
	// ever reached from Run — and Run holds runMu for its whole body.
	// In-memory ONLY: a restart resets the grace window, which is the safe
	// direction (it delays a reap, never authorises one early). Bounded by
	// the number of distinct servers configured in this process lifetime.
	implausibleSince map[string]time.Time

	// walkKey / walkItems are the LIVE progress of the walk currently in
	// flight: the StableServerKey being walked, and how many items its
	// callback has seen. Both are atomics read from outside runMu, because
	// the whole point is to be readable WHILE the walk holds it.
	//
	// A walk of a 15,000-track upstream took minutes with nothing on
	// screen — no counter, no in-flight marker, and the admin page does
	// not even re-fetch after load. The filesystem scanner has had exactly
	// this (Scanner.progress) since it was written; this is its twin.
	//
	// walkKey is "" when nothing is walking, which is also the signal the
	// SSE publisher gates on. Cleared in the same defer that stops the
	// walk, so a panicking or erroring walk cannot leave the UI showing a
	// walk that has finished.
	walkKey   atomic.Value // string
	walkItems atomic.Int64
}

// WalkStatus is one upstream's live walk state.
type WalkStatus struct {
	// Key is upnpingest.StableServerKey for the server being walked —
	// what upnp_track_routing.server_udn holds, NOT the device UDN.
	Key     string
	Walking bool
	// Items is how many entries the walk callback has seen so far. It
	// counts WALKED items, not written rows: the skip-if-unchanged path
	// writes nothing for most of them, and a counter that only moved on
	// writes would sit still through an unchanged re-walk — the exact
	// case that most looks like a hang.
	Items int64
}

// WalkProgress reports the walk in flight, if any. Atomic reads only —
// no lock, no DB — so it is safe on an SSE tick.
func (i *Ingester) WalkProgress() WalkStatus {
	if i == nil {
		return WalkStatus{}
	}
	key, _ := i.walkKey.Load().(string)
	if key == "" {
		return WalkStatus{}
	}
	return WalkStatus{Key: key, Walking: true, Items: i.walkItems.Load()}
}

// beginWalk marks a walk in flight and returns the closer that clears it.
func (i *Ingester) beginWalk(key string) func() {
	i.walkItems.Store(0)
	i.walkKey.Store(key)
	return func() { i.walkKey.Store("") }
}

// NewIngester wires the orchestrator. None of the args may be nil
// except idStore (an in-memory default is used when nil — the
// SystemUpdateID skip degenerates to "never skip", which is correct
// for first-launch / no-state-store deployments).
func NewIngester(
	cfg config.UPnPUpstreamConfig,
	cdsClient *upnp.ContentDirectoryClient,
	resolver ServerResolver,
	store *manifest.Store,
	idStore UpdateIDStore,
) (*Ingester, error) {
	if cdsClient == nil {
		return nil, errors.New("upnpingest: nil ContentDirectoryClient")
	}
	if resolver == nil {
		return nil, errors.New("upnpingest: nil ServerResolver")
	}
	if store == nil {
		return nil, errors.New("upnpingest: nil manifest.Store")
	}
	if idStore == nil {
		idStore = newMemoryUpdateIDStore()
	}
	return &Ingester{
		cfg: cfg, cdsClient: cdsClient, resolver: resolver,
		store: store, idStore: idStore,
		implausibleSince: make(map[string]time.Time),
	}, nil
}

// Run sweeps every configured server once. Per-server errors are
// captured in the per-server result; Run only returns an error for
// fatal misconfiguration (e.g. ctx already cancelled). Designed to be
// called from a scan-tick loop in cmd/bridge — wiring is the file
// proxy PR's job, not this one.
func (i *Ingester) Run(ctx context.Context, opts Options) (IngestResult, error) {
	i.runMu.Lock()
	defer i.runMu.Unlock()
	if err := ctx.Err(); err != nil {
		return IngestResult{}, err
	}
	if !i.cfg.Enabled {
		return IngestResult{}, nil
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	backstop := opts.TimeBackstop
	if backstop == 0 {
		backstop = defaultTimeBackstop
	}

	var out IngestResult
	// Orphan sweep BEFORE the per-server pass: reap rows belonging to
	// servers the operator removed from config. Runs only while the
	// feature is enabled (a temporary feature-off toggle must not wipe
	// routed state) and never aborts the per-server ingest on failure.
	i.reapOrphanServers(ctx, &out)
	for idx := range i.cfg.Servers {
		srv := i.cfg.Servers[idx]
		res := ServerIngestResult{Name: srv.Name, StableKey: StableServerKey(srv)}
		i.ingestOne(ctx, srv, opts.ForceWalk, backstop, opts.MaxItems, now, &res)
		out.PerServer = append(out.PerServer, res)
	}
	return out, nil
}

// reapOrphanServers deletes the manifest tracks (and, via FK CASCADE,
// the routing rows) of every server_udn present in upnp_track_routing
// but absent from the configured server set. This is the ONLY lifecycle
// path for a removed server's rows: the per-server reconcile in
// ingestOne never sees an unconfigured UDN, and the fs scanner's
// missing pass deliberately spares routed rows (PR #370). Keyed on
// StableServerKey to match what ingestOne stamps into server_udn.
func (i *Ingester) reapOrphanServers(ctx context.Context, out *IngestResult) {
	routed, err := i.store.ListUPnPRoutedServerUDNs(ctx)
	if err != nil {
		out.OrphanSweepErr = fmt.Errorf("ListUPnPRoutedServerUDNs: %w", err)
		return
	}
	if len(routed) == 0 {
		return
	}
	configured := make(map[string]struct{}, len(i.cfg.Servers))
	for idx := range i.cfg.Servers {
		configured[StableServerKey(i.cfg.Servers[idx])] = struct{}{}
	}
	// Per-orphan failures accumulate (errors.Join) instead of aborting
	// the sweep — one failing server must not block the cleanup of its
	// siblings; the next tick retries whatever failed.
	for _, udn := range routed {
		if _, ok := configured[udn]; ok {
			continue
		}
		paths, err := i.store.ListUPnPSourcePathsByServer(ctx, udn)
		if err != nil {
			out.OrphanSweepErr = errors.Join(out.OrphanSweepErr,
				fmt.Errorf("ListUPnPSourcePathsByServer(%s): %w", udn, err))
			continue
		}
		if len(paths) == 0 {
			continue
		}
		if err := i.store.DeleteTracksBatch(ctx, paths); err != nil {
			out.OrphanSweepErr = errors.Join(out.OrphanSweepErr,
				fmt.Errorf("DeleteTracksBatch(orphan %s): %w", udn, err))
			continue
		}
		out.OrphanServersReaped++
		out.OrphanTracksReaped += len(paths)
	}
}

// ingestOne handles a single server: resolve controlURL, gate on
// GetSystemUpdateID + time backstop, walk, reconcile.
func (i *Ingester) ingestOne(ctx context.Context, srv config.UPnPUpstreamServerConfig,
	forceWalk bool, backstop time.Duration, maxItems int, now func() time.Time, res *ServerIngestResult,
) {
	// Stamp ServerUDN FIRST, before any early return. StableServerKey is a pure
	// function of the configured server, so it's known at entry — and the admin
	// adapter keys its per-server telemetry map on res.ServerUDN, so ANY path
	// that returns with it empty collides under the "" key. That includes the
	// resolve-failure and not-discoverable returns just below, i.e. exactly the
	// offline/unreachable servers whose telemetry an operator most wants to see
	// (Gemini, post-merge review of #527).
	udn := StableServerKey(srv)
	res.ServerUDN = udn

	controlURL, err := i.resolver.ResolveControlURL(ctx, srv)
	if err != nil {
		res.Err = fmt.Errorf("resolve controlURL: %w", err)
		return
	}
	if controlURL == "" {
		// Honest split: a UDN-less manual-URL entry can NEVER resolve —
		// the discovery-cache resolver only looks up by UDN, and the
		// manual-URL fetch path is unimplemented (see the TODO in
		// cmd/bridge/upnp_upstream_wiring.go's ResolveControlURL).
		// Reporting it as "not discoverable this tick" implied SSDP
		// might find it next tick, which sent operators debugging a
		// discovery problem that doesn't exist. Feature-review P2-29
		// (2026-08-14).
		if strings.TrimSpace(srv.UDN) == "" && strings.TrimSpace(srv.ManualDescriptionURL) != "" {
			res.Err = errors.New("manualDescriptionURL is not yet supported — the bridge resolves servers via SSDP only; configure the server's UDN")
			return
		}
		res.Err = errors.New("server not discoverable this tick")
		return
	}

	// GetSystemUpdateID + time-backstop skip gate. The decision is a
	// pure function so it's unit-testable without standing up a real
	// SOAP server.
	currentID, idErr := i.cdsClient.GetSystemUpdateID(ctx, controlURL)
	if idErr != nil {
		// Don't treat this as a hard failure — some servers temporarily
		// reject GetSystemUpdateID under load. Fall through to a walk.
		currentID = ""
	}
	// Key the idStore skip-gate on the StableServerKey, NOT raw srv.UDN:
	// srv.UDN is "" for every UDN-less manual server, so a raw-UDN key
	// collides all manual servers' SystemUpdateID + lastWalkedAt into one
	// shared slot — one manual server's update-ID match then suppresses
	// the others' walks. The walk/reap below already use StableServerKey
	// (lines that pass `udn`); this aligns the skip-gate with them. Same
	// class as PR #353's admin-gate fix; this was the residual ingest miss.
	// udn / res.ServerUDN are stamped at function entry (see the top of
	// ingestOne) so EVERY early return — including the skip path below, which
	// is the steady state since the SystemUpdateID gate skips most ticks —
	// carries the key the admin telemetry map is keyed on.
	stored, _ := i.idStore.Get(udn)
	lastWalkedAt, _ := i.idStore.LastWalkedAt(udn)
	if decideSkipWalk(currentID, stored, lastWalkedAt, now(), backstop, forceWalk) {
		res.Skipped = true
		res.SkipReason = "SystemUpdateID matched stored value AND within backstop"
		return
	}

	// Walk.
	res.WalkStartedAt = now()
	prefix := normalizePrefix(srv)

	// Skip-if-unchanged baseline: the stored manifest rows currently
	// routed via this server. A walked track whose walk-authoritative
	// fields match its baseline row skips the Track upsert entirely —
	// preserving `indexed_at` (iOS delta-sync stops re-receiving the
	// whole routed library after every walk) and `enriched_at` (the
	// enricher stops re-processing 15k unchanged tracks per walk; the
	// `enriched_at = 0` reset in UpsertTrack is contractually "on
	// track change"). This is the UPnP analog of the filesystem
	// scanner's size+mtime early-skip (scanner.go) — the walker has no
	// upstream mtime to compare, so it compares the walked metadata
	// fields instead (walkFieldsEqual).
	//
	// A baseline load failure degrades to nil (= no skips, the legacy
	// rewrite-everything behaviour) rather than failing the server —
	// upserts are ON CONFLICT keyed so correctness is unaffected.
	baseline, baselineErr := i.store.ListUPnPTracksByServer(ctx, udn)
	if baselineErr != nil {
		baseline = nil
	}

	var pendingTracks []*manifest.Track
	var pendingRouting []*manifest.UPnPRouting
	const flushEvery = 200

	flush := func() error {
		if len(pendingTracks) == 0 && len(pendingRouting) == 0 {
			return nil
		}
		// Track rows MUST go in BEFORE the routing rows because the
		// routing table's FK references tracks(path). Unchanged-skipped
		// tracks have no pending Track row — their parent row already
		// exists in the DB, so the routing upsert's FK is satisfied.
		// UpsertTrackBatch no-ops on an empty slice.
		if err := i.store.UpsertTrackBatch(ctx, pendingTracks); err != nil {
			return fmt.Errorf("UpsertTrackBatch: %w", err)
		}
		if err := i.store.UpsertUPnPRoutingBatch(ctx, pendingRouting); err != nil {
			return fmt.Errorf("UpsertUPnPRoutingBatch: %w", err)
		}
		pendingTracks = pendingTracks[:0]
		pendingRouting = pendingRouting[:0]
		return nil
	}

	walkStart := res.WalkStartedAt
	// Scoped to the browse itself, not to ingestOne: the reconcile sweep
	// and the orphan pass after it are DB work with their own duration,
	// and reporting them as "walking 15,000" would be a counter that had
	// stopped moving for a reason the reader cannot see.
	endWalk := i.beginWalk(res.StableKey)
	defer endWalk()
	walkStats, walkErr := upnp.BrowseFoldersWalk(ctx, i.cdsClient, controlURL,
		upnp.WalkOptions{
			RootObjectID:        srv.EffectiveRootObjectID(),
			PathPrefix:          prefix,
			SkipContainerTitles: srv.SkipTopLevelContainers,
			MaxItems:            maxItems, // 0 → walker's built-in 50k default
		},
		func(w upnp.Walked) error {
			tr, rt := buildTrackAndRouting(w, udn, walkStart)
			// The routing row is ALWAYS refreshed — its last_seen_at
			// drives the reconcile sweep below, and res_url / object_id
			// legitimately float across upstream restarts (DHCP, port
			// changes) without the track content changing.
			if existing, ok := baseline[tr.Path]; ok && walkFieldsEqual(existing, tr) {
				res.Unchanged++
			} else {
				pendingTracks = append(pendingTracks, tr)
			}
			pendingRouting = append(pendingRouting, rt)
			res.Walked++
			i.walkItems.Store(int64(res.Walked))
			// Flush keyed on the routing slice — it grows on EVERY
			// walked item (tracks only on changed ones), so it alone
			// bounds the batch size.
			if len(pendingRouting) >= flushEvery {
				return flush()
			}
			return nil
		})
	if flushErr := flush(); flushErr != nil && walkErr == nil {
		walkErr = flushErr
	}
	walkErr = effectiveWalkErr(walkStats, walkErr)
	res.WalkCompletedAt = now()
	if walkErr != nil {
		res.Err = walkErr
		// Do NOT reap on a failed OR TRUNCATED walk — partial results
		// are not authoritative. A transient error mid-tree would
		// otherwise delete legitimate rows on the server side (the same
		// class of bug as the filesystem scanner's errorSubtrees
		// sentinel). A MaxItems-TRUNCATED walk (ErrWalkTruncated) only
		// visited a PREFIX of the library, so reaping would delete every
		// track past the ceiling that the walk never reached — exactly
		// the data loss the walker's ErrWalkTruncated contract warns
		// against ("partial results MUST NOT be treated as
		// authoritative"). ErrWalkTruncated was previously (incorrectly)
		// excluded from this guard, so a truncated walk fell through to
		// the reconcile sweep below. Skipping idStore.Set here too is
		// correct: a truncated tick must re-walk next time, not settle.
		return
	}

	// An error-free walk is NOT the same as an authoritative one. A
	// container that Browses EMPTY without an error terminates BrowseAll
	// (client.go's `pageLen == 0` break) and the walker just yields
	// nothing for that subtree — no error, no stats.Truncated — so the
	// sweep below would delete every row underneath it with none of the
	// debounce the filesystem scanner's missing_count pass provides. The
	// upstream family this ingests is documented as doing exactly that
	// while its DB rebuilds (see pager.go on MiniDLNA's inaccurate
	// TotalMatches), and the SystemUpdateID gate FORCES a walk during a
	// rebuild precisely because the ID moved. Same class for a NAS share
	// unmounting under a still-answering server: the root browses empty.
	//
	// Skipping the idStore.Set below is deliberate and mirrors the
	// truncated-walk branch above: a tick we refused to trust must re-walk
	// next tick rather than settle on this SystemUpdateID.
	if allowed, suspectFor := i.reapAuthorized(udn, res.Walked, len(baseline), baselineErr == nil, now()); !allowed {
		logger.Warn("upstream walk yielded implausibly little; skipping reconcile sweep",
			"server", udn, "walked", res.Walked, "baseline", len(baseline),
			"baselineKnown", baselineErr == nil, "suspectFor", suspectFor.String())
		return
	}

	// Reconcile: anything not refreshed in this walk generation goes.
	// last_seen_at < walk-start is the cutoff: every row touched THIS
	// pass has last_seen_at == walk-start. DeleteTracksBatch keeps the
	// reap to one transaction + one lock acquisition regardless of the
	// stale-row count (Gemini HIGH on PR #351: per-row deletes would
	// be an N+1 transaction storm under a large reorganisation).
	stale, err := i.store.ListUPnPSourcePathsOlderThan(ctx, udn, walkStart)
	if err != nil {
		res.Err = fmt.Errorf("ListUPnPSourcePathsOlderThan: %w", err)
		return
	}
	if len(stale) > 0 {
		if dErr := i.store.DeleteTracksBatch(ctx, stale); dErr != nil {
			res.Err = fmt.Errorf("DeleteTracksBatch: %w", dErr)
			return
		}
		res.Reaped = len(stale)
	}

	// Stash the SystemUpdateID + walk-time for next tick's skip gate.
	// Same StableServerKey (`udn`) the Get/LastWalkedAt above use.
	i.idStore.Set(udn, currentID, walkStart)
}

// walkLooksImplausible reports whether a SUCCESSFUL walk yielded so much
// less than the stored baseline that its result must not be treated as
// authoritative for the reconcile sweep. Pure — the grace-window
// bookkeeping lives in reapAuthorized.
//
//   - baseline unknown (the ListUPnPTracksByServer load failed): ALWAYS
//     implausible. The guard is otherwise disarmed by exactly the query
//     failure that hides how much we are about to delete, and skipping one
//     reconcile tick costs nothing.
//   - baseline empty: never implausible. Nothing is stored, so there is
//     nothing to protect and nothing for the sweep to delete — this is the
//     normal first-ingest shape (walked=N, baseline=0).
//   - walked zero against a non-empty baseline: implausible. A server that
//     answered SSDP, answered GetSystemUpdateID, and browsed without error
//     but holds none of the N tracks we have recorded for it is reporting
//     an empty root, not a library the operator deleted between ticks.
//   - otherwise: implausible below minPlausibleWalkFraction of the
//     baseline. This is the partial case — an empty page MID-TREE truncates
//     one subtree and leaves the rest of the walk looking clean.
func walkLooksImplausible(walked, baseline int, baselineKnown bool) bool {
	if !baselineKnown {
		return true
	}
	if baseline <= 0 {
		return false
	}
	if walked <= 0 {
		return true
	}
	return float64(walked) < minPlausibleWalkFraction*float64(baseline)
}

// reapAuthorized decides whether this walk's result may drive the reconcile
// sweep, and updates the per-server grace bookkeeping. Returns how long the
// server has been reporting an implausible shape, for the caller's log.
//
// A plausible walk clears the window immediately. The FIRST implausible walk
// always refuses (so a transient rebuild costs at most stale rows for one
// tick); further ones refuse until implausibleWalkGrace of wall time has
// elapsed, after which the shape is accepted as the real library and the
// window resets. A clock that jumps backwards only ever delays the reap.
//
// Caller MUST hold runMu (see Ingester.implausibleSince).
func (i *Ingester) reapAuthorized(udn string, walked, baseline int, baselineKnown bool, now time.Time) (bool, time.Duration) {
	if !walkLooksImplausible(walked, baseline, baselineKnown) {
		delete(i.implausibleSince, udn)
		return true, 0
	}
	since, seen := i.implausibleSince[udn]
	if !seen {
		i.implausibleSince[udn] = now
		return false, 0
	}
	elapsed := now.Sub(since)
	if elapsed >= implausibleWalkGrace {
		delete(i.implausibleSince, udn)
		return true, elapsed
	}
	return false, elapsed
}

// effectiveWalkErr folds the walker's stats into the walk error. A
// per-container ErrBrowseLimit is non-fatal to the walker (it keeps
// walking siblings, sets stats.Truncated, and returns nil) — but the
// result is still a PARTIAL view of the library, so it MUST route
// through the same no-reap + no-idStore.Set guard as ErrWalkTruncated.
// Pre-fix the stats were discarded and this truncation flavour fell
// straight through to the reconcile sweep, deleting every row the
// truncated walk never reached. Pure helper — table-tested.
func effectiveWalkErr(stats upnp.WalkStats, walkErr error) error {
	if walkErr == nil && stats.Truncated {
		return fmt.Errorf("upnpingest: walk truncated by per-container browse limit: %w", upnp.ErrWalkTruncated)
	}
	return walkErr
}

// buildTrackAndRouting converts one Walked record into the matched
// (Track, UPnPRouting) pair the ingest writes. Split out as a pure
// helper so the field mapping is independently testable + obvious.
func buildTrackAndRouting(w upnp.Walked, serverUDN string, walkStart time.Time) (*manifest.Track, *manifest.UPnPRouting) {
	enriched := true
	tr := &manifest.Track{
		Path: w.Path,
		Size: w.Size,
		// The bridge cannot stat the upstream's filesystem mtime; the
		// walk-start time is a stable mtime that ALSO doubles as the
		// "this row is fresh in the current walk generation" stamp the
		// existing scanner uses missing_count for. Per-track mtime
		// from the upstream's DIDL (date attr) is metadata, not a file
		// mtime — surfaced separately if/when we extend the wire.
		ModTime: walkStart,
		Title:   w.Title,
		Artist:  w.Artist,
		Album:   w.Album,
		// The walker parses <upnp:genre> (didl.go) and carried it all the
		// way here — but this constructor (the ONLY manifest.Track
		// producer outside the scanner) never assigned it, so every
		// UPnP-proxied library lost its genre axis end-to-end: the iOS
		// genre browse/search/smart-playlist predicates were empty for
		// proxied content AND the bridge's own DLNA re-serve dropped it
		// for third-party renderers. Existing rows heal on the next walk
		// (the field diff below now sees the change). P1-8, 2026-08-14
		// feature review. NOTE: DiscNumber stays absent — DIDL-Lite has
		// no standard disc element and `Walked` carries none.
		Genre:       w.Genre,
		Codec:       codecFromExtension(w.Path),
		Enriched:    &enriched,
		AlbumArtist: w.Artist, // best-effort fallback; the DIDL rarely separates
	}
	if w.TrackNumber > 0 {
		tn := w.TrackNumber
		tr.TrackNumber = &tn
	}
	if dur := parseDurationSeconds(w.Duration); dur > 0 {
		d := dur
		tr.Duration = &d
	}
	if y := yearFromDate(w.Date); y > 0 {
		yy := y
		tr.Year = &yy
	}
	if w.SampleRate > 0 {
		sr := float64(w.SampleRate)
		tr.SampleRate = &sr
	}
	if w.BitsPerSample > 0 {
		bps := w.BitsPerSample
		tr.BitsPerSample = &bps
	}
	// isDSD heuristic: protocolInfo MIME or extension. Lets iOS render
	// the DoP signal-path chip without an enrichment pass.
	if isDSDFromProtocolOrExt(w.ProtocolInfo, w.Path) {
		isDSD := true
		tr.IsDSD = &isDSD
	}
	rt := &manifest.UPnPRouting{
		SourcePath:     w.Path,
		ServerUDN:      serverUDN,
		ObjectID:       w.ObjectID,
		ParentObjectID: w.ParentObjectID,
		ResURL:         w.Res,
		ProtocolInfo:   w.ProtocolInfo,
		LastSeenAt:     walkStart,
	}
	return tr, rt
}

// walkFieldsEqual reports whether the walk-derived fields of `fresh`
// match the stored baseline row — the skip-if-unchanged decision.
//
// ONLY fields buildTrackAndRouting actually sets participate. Two
// deliberate exclusions, both load-bearing:
//   - ModTime: buildTrackAndRouting stamps it with walkStart, so it
//     differs on every walk by construction — including it would
//     defeat the skip entirely.
//   - Enricher-owned fields (MusicBrainzTrackID / MusicBrainzAlbumID /
//     ArtworkMBID / ArtistMBID): the enricher adds these to the stored
//     row via MarkEnriched, and a fresh walk row never carries them —
//     including them would mark every ENRICHED row as changed forever,
//     re-running the UpsertTrack that resets enriched_at = 0: an
//     enrich → walk → wipe → re-enrich loop (the exact churn this
//     helper exists to stop).
//
// DiscNumber / ReplayGain* are excluded for the same reason in the
// other direction: the walker never sets them, so any stored value
// (from a future enricher addition) must not read as a walk change.
// Genre USED to sit in that list under the claim "the walker never
// sets them" — false since the walker always parsed <upnp:genre>; the
// stale comment is what hid the buildTrackAndRouting drop (P1-8,
// 2026-08-14 review). Genre now participates: buildTrackAndRouting
// sets it, no enricher path writes it (verified — the only other
// writers are the file extractors, which never touch UPnP-routed
// rows), so the diff can't treadmill.
func walkFieldsEqual(existing, fresh *manifest.Track) bool {
	if existing == nil || fresh == nil {
		return false
	}
	return existing.Size == fresh.Size &&
		existing.Title == fresh.Title &&
		existing.Artist == fresh.Artist &&
		existing.AlbumArtist == fresh.AlbumArtist &&
		existing.Album == fresh.Album &&
		existing.Genre == fresh.Genre &&
		existing.Codec == fresh.Codec &&
		ptrEqual(existing.TrackNumber, fresh.TrackNumber) &&
		ptrEqual(existing.Year, fresh.Year) &&
		ptrEqual(existing.Duration, fresh.Duration) &&
		ptrEqual(existing.SampleRate, fresh.SampleRate) &&
		ptrEqual(existing.BitsPerSample, fresh.BitsPerSample) &&
		ptrEqual(existing.IsDSD, fresh.IsDSD)
}

// ptrEqual compares two optional values: both nil, or both non-nil and
// pointee-equal.
func ptrEqual[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// decideSkipWalk is the pure skip-gate decision. It returns true ONLY
// when:
//   - !forceWalk
//   - currentID and stored are non-empty AND equal (the SystemUpdateID
//     hasn't moved); MiniDLNA's "0" verbatim is treated as untrusted on
//     purpose so a real "no rebuild" signal must come from a server
//     that actually maintains the counter
//   - the time backstop hasn't elapsed (or is disabled when negative)
func decideSkipWalk(currentID, stored string, lastWalkedAt, now time.Time, backstop time.Duration, forceWalk bool) bool {
	if forceWalk {
		return false
	}
	current := strings.TrimSpace(currentID)
	if current == "" || current == "0" {
		return false
	}
	if current != strings.TrimSpace(stored) {
		return false
	}
	if backstop < 0 {
		// Negative = disable backstop (tests).
		return true
	}
	if lastWalkedAt.IsZero() {
		return false
	}
	return now.Sub(lastWalkedAt) < backstop
}

// normalizePrefix matches BrowseFoldersWalk's prefix normalisation
// (Trim space + slashes) so collisions between config validate + walk
// stay in lockstep.
func normalizePrefix(s config.UPnPUpstreamServerConfig) string {
	prefix := strings.Trim(strings.TrimSpace(s.PathPrefix), "/")
	if prefix == "" {
		prefix = strings.Trim(strings.TrimSpace(s.Name), "/")
	}
	return prefix
}

// StableServerKey returns the per-server key used for the routing
// table's `server_udn` column and for the SystemUpdateID store.
//
// When the operator supplied a real UDN we use it (lowercased + trimmed
// to mirror DLNARenderer.canonicalUDN on iOS). When the UDN is empty
// (a manual-URL server before its description has been fetched) we
// derive a fallback key from the SHA-256 of the ManualDescriptionURL,
// prefixed with "manual:" so it's visibly distinguishable. The hash
// makes silent collisions structurally impossible across operator
// configurations — two manual servers can share a Name or PathPrefix
// without one's reconcile sweep eating the other's tracks (Gemini HIGH
// on PR #351).
//
// Exported so the cmd/bridge admin adapter can compute the same key
// when surfacing manual-URL servers in /api/upnp/servers (Gemini HIGH
// on PR #353 — the prior srv.UDN-only gate silently excluded them).
func StableServerKey(s config.UPnPUpstreamServerConfig) string {
	if udn := strings.TrimSpace(s.UDN); udn != "" {
		return strings.ToLower(udn)
	}
	url := strings.TrimSpace(s.ManualDescriptionURL)
	if url == "" {
		// Last resort: trimmed name. config.Validate rejects an empty
		// (UDN AND ManualDescriptionURL) pair, so this branch is only
		// reachable from a test that bypassed validation.
		return "manual:no-url:" + strings.ToLower(strings.TrimSpace(s.Name))
	}
	sum := sha256.Sum256([]byte(url))
	return "manual:" + hex.EncodeToString(sum[:])
}

// codecFromExtension maps a file path's extension to the bridge's
// canonical codec name (matching the values internal/manifest's
// filesystem extractors set on Track.Codec). The iOS signal-path chip
// and the MIME-resolution logic both read this field.
func codecFromExtension(p string) string {
	switch strings.ToLower(strings.TrimPrefix(path.Ext(p), ".")) {
	case "flac":
		return "FLAC"
	case "dsf":
		return "DSF"
	case "dff":
		return "DFF"
	case "wav":
		return "WAV"
	case "aiff", "aif":
		return "AIFF"
	case "m4a", "mp4":
		// We can't disambiguate ALAC vs AAC without reading the file.
		// Leave Codec empty so the iOS side falls back to its existing
		// extension-based path (which is the same we'd do).
		return ""
	case "mp3":
		return "MP3"
	case "ogg":
		return "OGG"
	case "opus":
		return "OPUS"
	}
	return ""
}

// parseDurationSeconds parses an "H:MM:SS" / "H:MM:SS.mmm" DLNA duration
// to seconds. Returns 0 on malformed input. Uses strconv so unusual
// inputs (negatives, multiple dots, exponent notation, leading whitespace
// on a segment) fail predictably the way the standard library defines.
func parseDurationSeconds(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// Allocation-free split on ':' — strings.Split allocated a 3-element
	// slice (+ 3 substring headers) per track across a full UPnP sync.
	// Equivalent to the prior `len(parts) != 3` gate: a third colon lands
	// in the seconds segment and fails ParseFloat the same way. Gemini r4.
	i1 := strings.IndexByte(s, ':')
	if i1 < 0 {
		return 0
	}
	i2rel := strings.IndexByte(s[i1+1:], ':')
	if i2rel < 0 {
		return 0
	}
	i2 := i1 + 1 + i2rel
	h, err := strconv.Atoi(s[:i1])
	if err != nil || h < 0 {
		return 0
	}
	m, err := strconv.Atoi(s[i1+1 : i2])
	if err != nil || m < 0 {
		return 0
	}
	sec, err := strconv.ParseFloat(s[i2+1:], 64)
	if err != nil || sec < 0 {
		return 0
	}
	return float64(h*3600+m*60) + sec
}

// yearFromDate extracts the 4-digit leading year from a DLNA date
// string (e.g. "2019-01-01" → 2019). Returns 0 on malformed input.
// Per DLNA spec the date is digit-prefixed; we explicitly reject signed
// values ('+'/'-') that strconv.Atoi otherwise accepts as numeric.
func yearFromDate(s string) int {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return 0
	}
	head := s[:4]
	for _, r := range head {
		if r < '0' || r > '9' {
			return 0
		}
	}
	y, err := strconv.Atoi(head)
	if err != nil || y < 0 {
		return 0
	}
	return y
}

// isDSDFromProtocolOrExt decides isDSD without reading the file. DSF +
// DFF are unambiguous via either signal. ALAC/PCM are NOT DSD; leave
// the field unset for those (the wire treats absent IsDSD as "unknown",
// which the iOS side correctly handles).
func isDSDFromProtocolOrExt(proto, path string) bool {
	pi := strings.ToLower(proto)
	if strings.Contains(pi, "audio/x-dsf") || strings.Contains(pi, "audio/dsf") ||
		strings.Contains(pi, "audio/x-dff") || strings.Contains(pi, "audio/dsdiff") ||
		strings.Contains(pi, "audio/dsd") {
		return true
	}
	switch strings.ToLower(strings.TrimPrefix(extOf(path), ".")) {
	case "dsf", "dff":
		return true
	}
	return false
}

func extOf(p string) string { return path.Ext(p) }
