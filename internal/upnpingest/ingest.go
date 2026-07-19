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
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
)

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
	Name            string // operator-visible label from config
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
		res := ServerIngestResult{Name: srv.Name}
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
	controlURL, err := i.resolver.ResolveControlURL(ctx, srv)
	if err != nil {
		res.Err = fmt.Errorf("resolve controlURL: %w", err)
		return
	}
	if controlURL == "" {
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
	udn := StableServerKey(srv)
	// Stamp ServerUDN BEFORE the skip early-return. The admin adapter keys its
	// per-server telemetry map on res.ServerUDN, and a SKIPPED result is the
	// steady state — the SystemUpdateID gate exists to skip most ticks. Leaving
	// it "" on the skip path collided every skipped server under the empty key,
	// so a correctly-functioning (=skipped) upstream showed NO recent-walk
	// telemetry on the admin "Sources" dashboard.
	res.ServerUDN = udn
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
		ModTime:     walkStart,
		Title:       w.Title,
		Artist:      w.Artist,
		Album:       w.Album,
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
// Genre / DiscNumber / ReplayGain* are excluded for the same reason in
// the other direction: the walker never sets them, so any stored value
// (from a future enricher addition) must not read as a walk change.
func walkFieldsEqual(existing, fresh *manifest.Track) bool {
	if existing == nil || fresh == nil {
		return false
	}
	return existing.Size == fresh.Size &&
		existing.Title == fresh.Title &&
		existing.Artist == fresh.Artist &&
		existing.AlbumArtist == fresh.AlbumArtist &&
		existing.Album == fresh.Album &&
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
