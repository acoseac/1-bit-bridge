package manifest

import (
	"io"
	"os"
	"time"
)

// VariantResolutionStatus is the outcome enum returned by
// Provider.ResolveVariant. Mirrors api.VariantStatus value-for-value
// (we re-export here as a separate enum so the manifest package
// doesn't import the api package — would create an upward cycle).
// The api.VariantStore interface adapter (in cmd/bridge wiring)
// translates between the two enums.
type VariantResolutionStatus int

const (
	VariantOK VariantResolutionStatus = iota
	VariantNotFound
	VariantStale
)

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
func (p *Provider) WriteManifest(w io.Writer, since time.Time) error {
	return writeManifestGated(w, p.store, p.scanner.Roots(), since, p.upscaleEnabled)
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

// ResolveVariant looks up the (sourcePath, variantID) pair in
// `track_variants`, then performs a freshness check against the
// current source file on disk. Returns:
//
//   - sidecarPath, VariantOK, nil — variant exists, source mtime
//     and size match what was captured at conversion time, sidecar
//     is safe to serve.
//   - "", VariantNotFound, nil — no row in the table for that
//     (sourcePath, variantID) pair.
//   - "", VariantStale, nil — row exists but the source has been
//     modified since conversion. Caller should respond 410 Gone
//     and surface the staleness to the user. The sidecar stays on
//     disk; `bridge upscale --force` is the explicit re-conversion
//     path. We don't auto-delete the stale sidecar here because
//     the freshness drift could be transient (e.g. a touch(1) on
//     the source) and re-conversion is the operator's call.
//   - "", VariantOK, err — DB error during lookup. Caller should
//     surface a 500.
//
// The cmd/bridge serve wiring wraps this into the api.VariantStore
// interface so the api package doesn't import the manifest package
// directly (mirrors the MBIDProbe / ManifestProvider pattern).
func (p *Provider) ResolveVariant(sourcePath, variantID string) (string, VariantResolutionStatus, error) {
	v, err := p.store.GetVariant(sourcePath, variantID)
	if err != nil {
		return "", VariantOK, err
	}
	if v == nil {
		return "", VariantNotFound, nil
	}
	// Freshness probe: stat the source on disk and compare. The
	// source path on the wire is library-relative; we need to
	// resolve through the scanner's roots to get the absolute
	// path. Operators with multi-root setups: the resolver is
	// authoritative.
	resolved := p.resolveSourceAbsolute(sourcePath)
	if resolved == "" {
		// Source path doesn't resolve under any current root. The
		// most likely cause is a root being removed at runtime
		// (admin Settings → remove root). Treat as stale: the
		// sidecar's source no longer exists in the library
		// surface.
		return "", VariantStale, nil
	}
	info, err := os.Stat(resolved)
	if err != nil {
		// Source missing on disk — same logical state as a removed
		// root. The DB row will be cleaned up when the scanner
		// next runs DeleteTrack on the missing source.
		if os.IsNotExist(err) {
			return "", VariantStale, nil
		}
		return "", VariantOK, err
	}
	if info.ModTime().UnixNano() != v.SourceMTimeNS || info.Size() != v.SourceSize {
		return "", VariantStale, nil
	}
	return v.SidecarPath, VariantOK, nil
}

// resolveSourceAbsolute maps a library-relative source path to its
// current absolute filesystem location by checking each scanner
// root in order. Returns "" if no root prefixes the path or the
// candidate file is unreadable.
//
// Cheap to call repeatedly — scanner.Roots() returns a snapshot
// that already lives in memory. No filesystem walk; just a few
// path-prefix checks plus a final Stat.
func (p *Provider) resolveSourceAbsolute(libraryRelative string) string {
	for _, root := range p.scanner.Roots() {
		// The library-relative form starts with the basename of
		// the root (matches the manifest serialization in
		// scanner.go via `filepath.Base(r)`). Strip the basename
		// prefix and join with the root's absolute path to
		// reconstruct.
		base := basenameOf(root) + "/"
		if len(libraryRelative) >= len(base) && libraryRelative[:len(base)] == base {
			rel := libraryRelative[len(base):]
			candidate := root + "/" + rel
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return ""
}

// basenameOf is a tiny helper that mirrors filepath.Base for the
// path-style strings the scanner emits — extracted so the call
// site reads at the same level of abstraction as the prefix
// stripping above.
func basenameOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}
