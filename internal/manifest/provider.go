package manifest

import (
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

// BuildManifest satisfies api.ManifestProvider.
func (p *Provider) BuildManifest(since time.Time) (any, error) {
	return BuildManifest(p.store, p.scanner.Roots(), since)
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
