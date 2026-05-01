package manifest

import (
	"context"
	"io"
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
	store          *Store
	scanner        *Scanner
	upscaleEnabled bool
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
// at serve startup. Safe to call at any point — provider methods read
// the field on each call so a hot-reload of bridge.yaml takes effect
// on the next /v1/manifest fetch.
func (p *Provider) SetUpscaleEnabled(v bool) { p.upscaleEnabled = v }

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
	return writeManifestGated(ctx, w, p.store, p.scanner.Roots(), since, p.upscaleEnabled)
}

// BuildManifestPage satisfies api.ManifestProvider for the paginated
// full-manifest path introduced in v1.1. See `BuildManifestPage` in
// scanner.go for the cursor semantics.
func (p *Provider) BuildManifestPage(cursor string, limit int) (any, error) {
	return buildManifestPageGated(p.store, p.scanner.Roots(), cursor, limit, p.upscaleEnabled)
}

// IsScanning satisfies api.ManifestProvider.
func (p *Provider) IsScanning() bool { return p.scanner.IsScanning() }

// LastFullScan satisfies api.ManifestProvider.
func (p *Provider) LastFullScan() time.Time { return p.scanner.LastFullScan() }

// TracksIndexed reports the total number of tracks currently in the
// manifest store (not the count for a single scan). Backed by a
// `SELECT COUNT(*)` so /v1/health doesn't allocate O(n) strings per poll.
func (p *Provider) TracksIndexed() int {
	n, err := p.store.CountTracks()
	if err != nil {
		return 0
	}
	return n
}

// HasTrackWithArtworkMBID satisfies api.MBIDProbe. Delegates straight
// to the Store so the api package doesn't need a direct dependency on
// internal/manifest.
func (p *Provider) HasTrackWithArtworkMBID(mbid string) bool {
	return p.store.HasTrackWithArtworkMBID(mbid)
}

// HasTrackWithArtistMBID satisfies api.MBIDProbe.
func (p *Provider) HasTrackWithArtistMBID(mbid string) bool {
	return p.store.HasTrackWithArtistMBID(mbid)
}

// LookupVariant returns the cached metadata for one (sourcePath,
// variantID) pair, or `nil` if no such row exists. The api-side
// caller does the freshness check against its already-validated
// `os.FileInfo` from `bridgefs.Resolver.ResolveChecked`.
//
// The cmd/bridge serve wiring wraps this into the api.VariantStore
// interface so the api package doesn't import the manifest package
// directly (mirrors the MBIDProbe / ManifestProvider pattern).
func (p *Provider) LookupVariant(sourcePath, variantID string) (*VariantLookup, error) {
	v, err := p.store.GetVariant(sourcePath, variantID)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	return &VariantLookup{
		SidecarPath:   v.SidecarPath,
		SourceMTimeNS: v.SourceMTimeNS,
		SourceSize:    v.SourceSize,
	}, nil
}
