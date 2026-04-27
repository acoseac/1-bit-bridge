package manifest

import (
	"io"
	"time"
)

// Provider adapts the Scanner + Store pair to the api.ManifestProvider
// interface. Owned by cmd/bridge's serveCmd; api.Server holds a pointer to
// it.
//
// The root list is sourced from the scanner (which holds the hot-reloadable
// authoritative copy) so a runtime SetRoots lands in the next manifest
// without a separate wire-up here.
type Provider struct {
	store   *Store
	scanner *Scanner
}

// NewProvider ties together the store and scanner so the api package can
// fetch manifests without importing either directly.
func NewProvider(store *Store, scanner *Scanner) *Provider {
	return &Provider{store: store, scanner: scanner}
}

// WriteManifest satisfies api.ManifestProvider for the legacy
// non-paginated /v1/manifest endpoint. Streams JSON straight to w
// instead of materialising a full []Track in memory — the previous
// shape OOM-killed Pi-class hosts on a 50k-track library because the
// in-memory []Track materialisation alone could push past 200 MB.
// See WriteManifest in scanner.go for the streaming format details.
func (p *Provider) WriteManifest(w io.Writer, since time.Time) error {
	return WriteManifest(w, p.store, p.scanner.Roots(), since)
}

// BuildManifestPage satisfies api.ManifestProvider for the paginated
// full-manifest path introduced in v1.1. See `BuildManifestPage` in
// scanner.go for the cursor semantics.
func (p *Provider) BuildManifestPage(cursor string, limit int) (any, error) {
	return BuildManifestPage(p.store, p.scanner.Roots(), cursor, limit)
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
