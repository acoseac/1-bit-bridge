package manifest

import (
	"context"
	"io"
	"sync/atomic"
	"time"
)

// VariantLookup is the minimum metadata downstream needs to (a)
// freshness-check a sidecar against its source on disk and (b)
// open the cached file. The api package's VariantRecord mirrors
// this shape (separately defined to keep the manifest package
// from importing the api package — would create an upward cycle).
// The cmd/bridge wiring translates between the two.
//
// **Freshness check belongs to the caller, not to this lookup.**
// Pre-fix, Provider walked scanner.Roots() and re-resolved
// `(library-relative path) → (abs path)` by stripping the root
// basename, which (a) failed in single-root mode where the
// scanner emits paths WITHOUT a root-basename prefix (Gemini bot
// review on PR #108), and (b) tripped a CodeQL "uncontrolled
// data used in path expression" alert. Both go away when this
// method just returns the recorded provenance and the caller
// (which already has a validated `os.FileInfo` from
// `bridgefs.Resolver.ResolveChecked`) compares.
type VariantLookup struct {
	// SourcePath / VariantID are the CANONICAL row values (case-
	// preserved) — LookupVariant matches case-insensitively via
	// `unicode_lower`, but downstream consumers (reactive
	// open-on-serve cleanup in api/files.go) need the canonical
	// form to drive DeleteVariant + the upscale.deleted SSE
	// emission so the wire path matches `Track.path` byte-
	// identical for iOS reverse-index resolution. CodeRabbit
	// Major on PR #209.
	SourcePath    string
	VariantID     string
	SidecarPath   string
	SourceMTimeNS int64
	SourceSize    int64
}

// Provider adapts the Scanner + Store pair to the api.ManifestProvider
// interface. Owned by cmd/bridge's serveCmd; api.Server holds a pointer to
// it.
//
// The root list is sourced from the scanner (which holds the hot-reloadable
// authoritative copy) so a runtime SetRoots lands in the next manifest
// without a separate wire-up here.
//
// `upscaleEnabled` mirrors the runtime config flag. The store always
// reads variants from `track_variants` (it's a single correlated
// subquery per page, cheap), but the provider strips them before
// serialization when the flag is off — so an operator who toggles
// the feature off after running `bridge upscale` sees the variants
// disappear from the wire without the sidecars being deleted from
// disk. Round-trippable: re-enable and the variants reappear.
type Provider struct {
	store   *Store
	scanner *Scanner
	// upscaleEnabled is written by SetUpscaleEnabled and read on every
	// manifest fetch (WriteManifest / BuildManifestPage run on HTTP-handler
	// goroutines) — atomic.Bool so those concurrent read/write accesses are
	// race-free, honouring the "safe to call at any point" contract on the
	// setter. Provider is always used by pointer (see NewProvider), so the
	// non-copyable atomic is never copied (go vet copylocks stays clean).
	upscaleEnabled atomic.Bool
	// upscaleEnabledFn, when set, SUPERSEDES the atomic and is consulted
	// per manifest fetch. It exists because the flag became hot: the
	// atomic is written once at startup, so after a settings PATCH it
	// would keep stripping (or keep emitting) variants against a value
	// the rest of the bridge had already moved past — a client's manifest
	// disagreeing with /v1/health about whether the feature is on.
	//
	// atomic.Pointer rather than a bare field: manifest fetches run on
	// HTTP-handler goroutines, so an unsynchronised write would race them.
	upscaleEnabledFn atomic.Pointer[func() bool]

	// last{Count,Pending}ErrLogNS carry the unix-nano timestamp of the most
	// recent health-probe DB-error log for TracksIndexed / PendingDeletions.
	// /v1/health is polled frequently, so a persistently-broken DB would
	// otherwise flood the log — throttledHealthErr rate-limits each site to
	// ~once/min. atomic.Int64 (non-copyable, like upscaleEnabled above)
	// keeps the concurrent HTTP-handler reads/writes race-free.
	lastCountErrLogNS   atomic.Int64
	lastPendingErrLogNS atomic.Int64
}

// throttledHealthErr logs a health-probe DB error at most once/min per call
// site so a persistently-broken DB polled on every /v1/health request doesn't
// flood the log. Lock-free AND race-free: the CompareAndSwap lets exactly one
// goroutine per window win the timestamp update and log — no mutex on the
// health hot path, no double-log under simultaneous probes.
func throttledHealthErr(last *atomic.Int64, msg string, err error) {
	now := time.Now().UnixNano()
	prev := last.Load()
	if now-prev > int64(time.Minute) {
		if last.CompareAndSwap(prev, now) {
			logger.Error(msg, "err", err)
		}
	}
}

// NewProvider ties together the store and scanner so the api package can
// fetch manifests without importing either directly. UpscaleEnabled
// defaults to false; callers in production wire it from config via
// SetUpscaleEnabled.
func NewProvider(store *Store, scanner *Scanner) *Provider {
	return &Provider{store: store, scanner: scanner}
}

// SetUpscaleEnabled flips the gate that decides whether the manifest
// emits each Track's `Variants` slice. Plumbed from `cfg.Upscale.Enabled`
// at serve startup. Safe to call at any point — the field is an
// atomic.Bool that provider methods Load on each call, so this setter and
// the concurrent per-request reads stay race-free even if a future caller
// wires it to a live bridge.yaml hot-reload; the change takes effect on
// the next /v1/manifest fetch.
func (p *Provider) SetUpscaleEnabled(v bool) { p.upscaleEnabled.Store(v) }

// SetUpscaleEnabledSource attaches a LIVE gate, consulted per manifest
// fetch instead of the stored boolean. Nil clears it, restoring the
// stored value — which is what every caller that only ever calls
// SetUpscaleEnabled (tests, the CLI) keeps getting.
func (p *Provider) SetUpscaleEnabledSource(f func() bool) {
	if f == nil {
		p.upscaleEnabledFn.Store(nil)
		return
	}
	p.upscaleEnabledFn.Store(&f)
}

// upscaleGate resolves the effective gate, preferring the live source.
func (p *Provider) upscaleGate() bool {
	if fn := p.upscaleEnabledFn.Load(); fn != nil {
		return (*fn)()
	}
	return p.upscaleEnabled.Load()
}

// WriteManifest satisfies api.ManifestProvider for the legacy
// non-paginated /v1/manifest endpoint. Streams JSON straight to w
// instead of materialising a full []Track in memory — the previous
// shape OOM-killed Pi-class hosts on a 50k-track library because the
// in-memory []Track materialisation alone could push past 200 MB.
// See WriteManifest in scanner.go for the streaming format details.
//
// `ctx` is checked inside the per-row stream loop so a client
// disconnect mid-response (slow network, iOS app backgrounded mid-sync,
// slow-read DOS) terminates the SQLite scan instead of running to EOF.
func (p *Provider) WriteManifest(ctx context.Context, w io.Writer, since time.Time) error {
	return writeManifestGated(ctx, w, p.store, p.scanner.Roots(), since, p.upscaleGate())
}

// BuildManifestPage satisfies api.ManifestProvider for the paginated
// full-manifest path introduced in v1.1. See `BuildManifestPage` in
// scanner.go for the cursor semantics.
func (p *Provider) BuildManifestPage(ctx context.Context, cursor string, limit int) (*Manifest, error) {
	return buildManifestPageGated(ctx, p.store, p.scanner.Roots(), cursor, limit, p.upscaleGate())
}

// IsScanning satisfies api.ManifestProvider.
func (p *Provider) IsScanning() bool { return p.scanner.IsScanning() }

// LastFullScan satisfies api.ManifestProvider.
func (p *Provider) LastFullScan() time.Time { return p.scanner.LastFullScan() }

// TracksIndexed reports the number of SERVED tracks (duplicate-
// suppressed rows excluded) — the served-population rule: /v1/health's
// tracksIndexed must agree with the manifest a client can fetch, not
// with the store's internal row count (which the admin dashboard's
// RollupByPrefix keeps reporting in full). Backed by a `SELECT COUNT(*)`
// so /v1/health doesn't allocate O(n) strings per poll.
func (p *Provider) TracksIndexed(ctx context.Context) int {
	n, err := p.store.CountServedTracks(ctx)
	if err != nil {
		// Surface the DB fault for operators without changing the wire
		// value — returning e.g. -1 would break the versioned /v1/health
		// contract + the iOS decoder. Throttled (see helper).
		throttledHealthErr(&p.lastCountErrLogNS, "health probe: count tracks failed", err)
		return 0
	}
	return n
}

// PendingDeletions satisfies api.ManifestProvider. Reports the number of
// tracks + folders rows the scanner has marked as missing this pass but
// hasn't yet reached the configured delete threshold for. Cheap query —
// two indexed counts.
func (p *Provider) PendingDeletions(ctx context.Context) int64 {
	n, err := p.store.PendingDeletions(ctx)
	if err != nil {
		throttledHealthErr(&p.lastPendingErrLogNS, "health probe: pending deletions failed", err)
		return 0
	}
	return n
}

// HasTrackWithArtworkMBID satisfies api.MBIDProbe. Delegates straight
// to the Store so the api package doesn't need a direct dependency on
// internal/manifest.
func (p *Provider) HasTrackWithArtworkMBID(ctx context.Context, mbid string) (bool, error) {
	return p.store.HasTrackWithArtworkMBID(ctx, mbid)
}

// HasTrackWithArtistMBID satisfies api.MBIDProbe.
func (p *Provider) HasTrackWithArtistMBID(ctx context.Context, mbid string) (bool, error) {
	return p.store.HasTrackWithArtistMBID(ctx, mbid)
}

// ArtworkMBIDEnrichmentPending satisfies api.MBIDProbe.
func (p *Provider) ArtworkMBIDEnrichmentPending(ctx context.Context, mbid string) (bool, error) {
	return p.store.ArtworkMBIDEnrichmentPending(ctx, mbid)
}

// ArtistMBIDEnrichmentPending satisfies api.MBIDProbe.
func (p *Provider) ArtistMBIDEnrichmentPending(ctx context.Context, mbid string) (bool, error) {
	return p.store.ArtistMBIDEnrichmentPending(ctx, mbid)
}

// ResolveArtworkVersionMBID satisfies api.MBIDProbe — the /v1/artwork
// 16-hex alias resolver (see Store.ResolveArtworkVersionMBID).
func (p *Provider) ResolveArtworkVersionMBID(ctx context.Context, version string) (string, error) {
	return p.store.ResolveArtworkVersionMBID(ctx, version)
}

// LookupVariant returns the cached metadata for one (sourcePath,
// variantID) pair, or `nil` if no such row exists. The api-side
// caller does the freshness check against its already-validated
// `os.FileInfo` from `bridgefs.Resolver.ResolveChecked`.
//
// The cmd/bridge serve wiring wraps this into the api.VariantStore
// interface so the api package doesn't import the manifest package
// directly (mirrors the MBIDProbe / ManifestProvider pattern).
//
// Routes through `Store.LookupVariant` (NOT the exact-match
// `Store.GetVariant`) so iOS-shaped lowercase paths from
// `/v1/download?path=…&variant=…` resolve against the case-preserved
// `track_variants.source_path` rows. Pre-fix this called `GetVariant`,
// matching exact only — every variant download from iOS returned 404
// because iOS's `share.normalize()` lowercases paths while the
// bridge's manifest writer preserves filesystem case. PR #126 split
// the case-folded `LookupVariant` out of `GetVariant` for the
// upscale-enqueue path; the `/v1/download` wrapper here was missed
// in that split — same logical bug class as PR #126 itself, just on
// a different caller. (CodeRabbit / Qodo on PR #126 second-pass
// missed this site.)
func (p *Provider) LookupVariant(ctx context.Context, sourcePath, variantID string) (*VariantLookup, error) {
	v, err := p.store.LookupVariant(ctx, sourcePath, variantID)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	return &VariantLookup{
		// Carry forward the row's canonical-case values — NOT
		// the request inputs the case-insensitive lookup
		// resolved from.
		SourcePath:    v.SourcePath,
		VariantID:     v.VariantID,
		SidecarPath:   v.SidecarPath,
		SourceMTimeNS: v.SourceMTimeNS,
		SourceSize:    v.SourceSize,
	}, nil
}

// AnalysisLookup is the manifest-package-local projection of one
// track_analysis row that the /v1/waveform handler needs: where the
// sidecar lives, its content tag (served as the ETag / used by iOS as
// the immutable-cache key), and the source freshness fields for the
// drift check. The cmd/bridge serve wiring wraps this into the
// api.AnalysisStore interface so the api package doesn't import the
// manifest package (mirrors VariantLookup / api.VariantStore).
type AnalysisLookup struct {
	// SourcePath is the CANONICAL row value (case-preserved);
	// LookupAnalysis matches case-insensitively but downstream
	// consumers want the canonical form. Mirrors VariantLookup.
	SourcePath    string
	WaveformPath  string
	WaveformTag   string
	SourceMTimeNS int64
	SourceSize    int64
	// Spectrum is the `1BSP` file-provenance curve, or nil when the row
	// carries none (analysis predating wf6, or a track too short to
	// average).
	Spectrum []byte
}

// LookupAnalysis returns the cached waveform metadata for one source
// path, or nil if no analysis row exists. Routes through
// Store.LookupAnalysis (case-insensitive) so iOS-shaped lowercased
// paths from /v1/waveform resolve against the case-preserved rows —
// same shape + rationale as LookupVariant.
func (p *Provider) LookupAnalysis(ctx context.Context, sourcePath string) (*AnalysisLookup, error) {
	a, err := p.store.LookupAnalysis(ctx, sourcePath)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, nil
	}
	return &AnalysisLookup{
		SourcePath:    a.SourcePath,
		WaveformPath:  a.WaveformPath,
		WaveformTag:   a.WaveformTag,
		SourceMTimeNS: a.SourceMTimeNS,
		SourceSize:    a.SourceSize,
		Spectrum:      a.Spectrum,
	}, nil
}
