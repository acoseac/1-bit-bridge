package upnpingest

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
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
}

// ServerIngestResult is the per-server outcome.
type ServerIngestResult struct {
	Name            string // operator-visible label from config
	ServerUDN       string // resolved at run time (empty when unresolved)
	Skipped         bool   // true when GetSystemUpdateID matched + within backstop window
	SkipReason      string // human-readable hint when Skipped is true
	Walked          int    // tracks yielded by the walker (= upserted)
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
	for idx := range i.cfg.Servers {
		srv := i.cfg.Servers[idx]
		res := ServerIngestResult{Name: srv.Name}
		i.ingestOne(ctx, srv, opts.ForceWalk, backstop, now, &res)
		out.PerServer = append(out.PerServer, res)
	}
	return out, nil
}

// ingestOne handles a single server: resolve controlURL, gate on
// GetSystemUpdateID + time backstop, walk, reconcile.
func (i *Ingester) ingestOne(ctx context.Context, srv config.UPnPUpstreamServerConfig,
	forceWalk bool, backstop time.Duration, now func() time.Time, res *ServerIngestResult,
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
	stored, _ := i.idStore.Get(srv.UDN)
	lastWalkedAt, _ := i.idStore.LastWalkedAt(srv.UDN)
	if decideSkipWalk(currentID, stored, lastWalkedAt, now(), backstop, forceWalk) {
		res.Skipped = true
		res.SkipReason = "SystemUpdateID matched stored value AND within backstop"
		return
	}

	// Walk.
	res.WalkStartedAt = now()
	udn := strings.TrimSpace(srv.UDN)
	if udn == "" {
		// Manual-URL server without a configured UDN — use the prefix
		// as a stable key so the reconcile sweep can still target only
		// this server's rows. (A bare description URL doesn't yield a
		// UDN without the additional device-description fetch the
		// discovery path does; the prefix is a safe per-server proxy.)
		udn = "manual:" + normalizePrefix(srv)
	}
	res.ServerUDN = udn
	prefix := normalizePrefix(srv)

	var pendingTracks []*manifest.Track
	var pendingRouting []*manifest.UPnPRouting
	const flushEvery = 200

	flush := func() error {
		if len(pendingTracks) == 0 {
			return nil
		}
		// Track rows MUST go in BEFORE the routing rows because the
		// routing table's FK references tracks(path).
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
	_, walkErr := upnp.BrowseFoldersWalk(ctx, i.cdsClient, controlURL,
		upnp.WalkOptions{
			RootObjectID:        srv.EffectiveRootObjectID(),
			PathPrefix:          prefix,
			SkipContainerTitles: srv.SkipTopLevelContainers,
		},
		func(w upnp.Walked) error {
			tr, rt := buildTrackAndRouting(w, udn, walkStart)
			pendingTracks = append(pendingTracks, tr)
			pendingRouting = append(pendingRouting, rt)
			res.Walked++
			if len(pendingTracks) >= flushEvery {
				return flush()
			}
			return nil
		})
	if flushErr := flush(); flushErr != nil && walkErr == nil {
		walkErr = flushErr
	}
	res.WalkCompletedAt = now()
	if walkErr != nil && !errors.Is(walkErr, upnp.ErrWalkTruncated) {
		res.Err = walkErr
		// Do NOT reap on a failed walk — a transient error mid-tree
		// would otherwise delete legitimate rows on the server side
		// (the same class of bug as the filesystem scanner's
		// errorSubtrees sentinel).
		return
	}

	// Reconcile: anything not refreshed in this walk generation goes.
	// last_seen_at < walk-start is the cutoff: every row touched THIS
	// pass has last_seen_at == walk-start.
	stale, err := i.store.ListUPnPSourcePathsOlderThan(ctx, udn, walkStart)
	if err != nil {
		res.Err = fmt.Errorf("ListUPnPSourcePathsOlderThan: %w", err)
		return
	}
	for _, p := range stale {
		if dErr := i.store.DeleteTrack(ctx, p); dErr != nil {
			// Log via res.Err but continue reaping siblings.
			res.Err = fmt.Errorf("DeleteTrack %q: %w", p, dErr)
			continue
		}
		res.Reaped++
	}

	// Stash the SystemUpdateID + walk-time for next tick's skip gate.
	i.idStore.Set(srv.UDN, currentID, walkStart)
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
// to seconds. Returns 0 on malformed input.
func parseDurationSeconds(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0
	}
	h := atoiOr(parts[0], -1)
	m := atoiOr(parts[1], -1)
	if h < 0 || m < 0 {
		return 0
	}
	sec := parseFloatOr(parts[2], -1)
	if sec < 0 {
		return 0
	}
	return float64(h*3600+m*60) + sec
}

// yearFromDate extracts the 4-digit leading year from a DLNA date
// string (e.g. "2019-01-01" → 2019). Returns 0 on malformed input.
func yearFromDate(s string) int {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return 0
	}
	return atoiOr(s[:4], 0)
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

func atoiOr(s string, def int) int {
	n := 0
	if s == "" {
		return def
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func parseFloatOr(s string, def float64) float64 {
	// Tiny inline float parser to avoid pulling in strconv just for
	// this — DLNA duration's fractional part is well-behaved.
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return float64(atoiOr(s, int(def)))
	}
	whole := atoiOr(s[:dot], -1)
	frac := atoiOr(s[dot+1:], -1)
	if whole < 0 || frac < 0 {
		return def
	}
	denom := 1.0
	for range s[dot+1:] {
		denom *= 10
	}
	return float64(whole) + float64(frac)/denom
}
