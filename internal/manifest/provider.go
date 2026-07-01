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
	return writeManifestGated(ctx, w, p.store, p.scanner.Roots(), since, p.upscaleEnabled.Load())
}

// BuildManifestPage satisfies api.ManifestProvider for the paginated
// full-manifest path introduced in v1.1. See `BuildManifestPage` in
// scanner.go for the cursor semantics.
func (p *Provider) BuildManifestPage(ctx context.Context, cursor string, limit int) (*Manifest, error) {
	return buildManifestPageGated(ctx, p.store, p.scanner.Roots(), cursor, limit, p.upscaleEnabled.Load())
}

// IsScanning satisfies api.ManifestProvider.
func (p *Provider) IsScanning() bool { return p.scanner.IsScanning() }

// LastFullScan satisfies api.ManifestProvider.
func (p *Provider) LastFullScan() time.Time { return p.scanner.LastFullScan() }

// TracksIndexed reports the total number of tracks currently in the
// manifest store (not the count for a single scan). Backed by a
// `SELECT COUNT(*)` so /v1/health doesn't allocate O(n) strings per poll.
func (p *Provider) TracksIndexed(ctx context.Context) int {
	n, err := p.store.CountTracks(ctx)
	if err != nil {
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
		return 0
	}
	return n
}

// HasTrackWithArtworkMBID satisfies api.MBIDProbe. Delegates straight
// to the Store so the api package doesn't need a direct dependency on
// internal/manifest.
func (p *Provider) HasTrackWithArtworkMBID(ctx context.Context, mbid string) bool {
	return p.store.HasTrackWithArtworkMBID(ctx, mbid)
}

// HasTrackWithArtistMBID satisfies api.MBIDProbe.
func (p *Provider) HasTrackWithArtistMBID(ctx context.Context, mbid string) bool {
	return p.store.HasTrackWithArtistMBID(ctx, mbid)
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
	}, nil
}
