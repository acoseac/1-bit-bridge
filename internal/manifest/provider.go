package manifest

import (
	"time"
)

// Provider adapts the Scanner + Store pair to the api.ManifestProvider
// interface. Owned by cmd/bridge's serveCmd; api.Server holds a pointer to
// it.
type Provider struct {
	roots   []string
	store   *Store
	scanner *Scanner
}

// NewProvider ties together the store and scanner so the api package can
// fetch manifests without importing either directly.
func NewProvider(roots []string, store *Store, scanner *Scanner) *Provider {
	return &Provider{roots: roots, store: store, scanner: scanner}
}

// BuildManifest satisfies api.ManifestProvider.
func (p *Provider) BuildManifest(since time.Time) (any, error) {
	return BuildManifest(p.store, p.roots, since)
}

// IsScanning satisfies api.ManifestProvider.
func (p *Provider) IsScanning() bool { return p.scanner.IsScanning() }

// LastFullScan satisfies api.ManifestProvider.
func (p *Provider) LastFullScan() time.Time { return p.scanner.LastFullScan() }

// TracksIndexed reports the total number of tracks currently in the
// manifest store (not the count for a single scan).
func (p *Provider) TracksIndexed() int {
	paths, err := p.store.TrackPaths()
	if err != nil {
		return 0
	}
	return len(paths)
}
