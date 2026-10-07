package api

import (
	"context"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The three user-data topics. Each goes through eventBroker.Publish, so
// it carries id: and is replayed on Last-Event-ID. Heartbeats and dropped
// stay synthetic and still omit id:.
const (
	topicFavoritesChanged = "favorites.changed"
	topicPlaylistsChanged = "playlists.changed"
	topicLibraryChanged   = "library.changed"

	// libraryChangeDebounce is the trailing quiet period for indexed_at
	// writers outside a full scan. A burst resets it; one event goes out
	// once the writes have been quiet this long.
	libraryChangeDebounce = 30 * time.Second
)

// favoritesChanged is the favorites.changed payload.
type favoritesChanged struct {
	Revision int64 `json:"revision"`
}

// playlistsChanged is the playlists.changed payload. The epoch is what a
// client compares, so a restore is visible before it has stored a hash.
type playlistsChanged struct {
	Epoch string `json:"epoch"`
}

// libraryChanged is the library.changed payload. IndexedAt is the watermark
// GET /v1/manifest?since= already parses (RFC3339, millisecond UTC).
type libraryChanged struct {
	IndexedAt string `json:"indexedAt"`
}

// clock is the time source the library debounce arms against. Production
// uses the wall clock. Tests inject a clock whose Advance fires due timers
// without sleeping.
type clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, fn func()) (stop func())
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) AfterFunc(d time.Duration, fn func()) func() {
	t := time.AfterFunc(d, fn)
	return func() { t.Stop() }
}

// SyncEventPublisher turns committed user-data notes into the three
// topics. It publishes only when enabled, which serve leaves false for
// the demo. library.changed is suppressed while scanning() is true — the
// scan /v1/health reports as scanState.isScanning, Scanner.AdvertisedScanning
// — and emitted once from ScanEnded after that flag has cleared. Every
// other library note shares one trailing debounce.
type SyncEventPublisher struct {
	pub       EventPublisher
	scanning  func() bool
	watermark func(context.Context) (int64, bool, error)
	clk       clock
	debounce  time.Duration
	enabled   bool

	mu         sync.Mutex
	stopTimer  func()
	pendingNS  int64
	generation uint64
}

// NewSyncEventPublisher publishes all three topics. scanning is
// Scanner.AdvertisedScanning, the scan /v1/health reports. watermark is
// Store.LibraryWatermark: the later of indexed_at and a journaled
// deletion. An error from watermark publishes nothing. An empty library
// (no watermark, no error) publishes the publisher clock from ScanEnded.
func NewSyncEventPublisher(pub EventPublisher, scanning func() bool, watermark func(context.Context) (int64, bool, error)) *SyncEventPublisher {
	return newSyncEventPublisher(pub, scanning, watermark, wallClock{}, libraryChangeDebounce, true)
}

func newSyncEventPublisher(pub EventPublisher, scanning func() bool, watermark func(context.Context) (int64, bool, error), clk clock, debounce time.Duration, enabled bool) *SyncEventPublisher {
	if scanning == nil {
		scanning = func() bool { return false }
	}
	if watermark == nil {
		watermark = func(context.Context) (int64, bool, error) { return 0, false, nil }
	}
	return &SyncEventPublisher{
		pub:       pub,
		scanning:  scanning,
		watermark: watermark,
		clk:       clk,
		debounce:  debounce,
		enabled:   enabled,
	}
}

// Hooks is what Store.SetSyncHooks installs. The callbacks run on the
// store's writer goroutine, after commit, while Store.mu is held, so they
// only arm a timer or publish. The broker publish is non-blocking.
func (p *SyncEventPublisher) Hooks() manifest.SyncHooks {
	return manifest.SyncHooks{
		Favorites: func(revision int64) { p.FavoritesChanged(revision) },
		Playlists: func(epoch string) { p.PlaylistsChanged(epoch) },
		Library:   func(ns int64) { p.NoteLibrary(ns) },
	}
}

// FavoritesChanged publishes after a commit that bumped the revision.
func (p *SyncEventPublisher) FavoritesChanged(revision int64) {
	if p == nil || !p.enabled || revision <= 0 {
		return
	}
	p.pub.Publish(topicFavoritesChanged, favoritesChanged{Revision: revision})
}

// PlaylistsChanged publishes after a commit that changed the playlist list.
func (p *SyncEventPublisher) PlaylistsChanged(epoch string) {
	if p == nil || !p.enabled || epoch == "" {
		return
	}
	p.pub.Publish(topicPlaylistsChanged, playlistsChanged{Epoch: epoch})
}

// NoteLibrary arms the shared trailing debounce, or drops the note while
// a full scan is in progress. A note during a scan also cancels a timer
// that was already armed, so it cannot fire mid-scan. The scan-end event
// covers every write the scan suppressed.
func (p *SyncEventPublisher) NoteLibrary(ns int64) {
	if p == nil || !p.enabled || ns <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.scanning() {
		p.cancelTimerLocked()
		return
	}
	p.stopTimerLocked()
	p.pendingNS = ns
	p.generation++
	gen := p.generation
	p.stopTimer = p.clk.AfterFunc(p.debounce, func() { p.fireDebounce(gen) })
}

// ScanEnded publishes one library.changed for the watermark at the end of
// a successful full scan, and cancels a debounce that was waiting. The
// caller invokes it after scanState.isScanning has gone false.
func (p *SyncEventPublisher) ScanEnded(ctx context.Context) {
	if p == nil || !p.enabled {
		return
	}
	p.mu.Lock()
	p.cancelTimerLocked()
	p.mu.Unlock()
	p.publishWatermark(ctx)
}

func (p *SyncEventPublisher) stopTimerLocked() {
	if p.stopTimer != nil {
		p.stopTimer()
		p.stopTimer = nil
	}
}

func (p *SyncEventPublisher) cancelTimerLocked() {
	p.generation++
	p.stopTimerLocked()
	p.pendingNS = 0
}

func (p *SyncEventPublisher) fireDebounce(gen uint64) {
	p.mu.Lock()
	if gen != p.generation {
		p.mu.Unlock()
		return
	}
	ns := p.pendingNS
	p.pendingNS = 0
	p.stopTimer = nil
	scanning := p.scanning()
	p.mu.Unlock()
	if scanning || ns <= 0 {
		return
	}
	p.publishLibrary(ns)
}

func (p *SyncEventPublisher) publishWatermark(ctx context.Context) {
	ns, ok, err := p.watermark(ctx)
	if err != nil {
		return
	}
	if !ok || ns <= 0 {
		ns = p.clk.Now().UnixNano()
	}
	if ns <= 0 {
		return
	}
	p.publishLibrary(ns)
}

func (p *SyncEventPublisher) publishLibrary(ns int64) {
	p.pub.Publish(topicLibraryChanged, libraryChanged{IndexedAt: formatIndexedAt(ns)})
}

// formatIndexedAt is the watermark string GET /v1/manifest?since= parses.
// Millisecond UTC keeps a whole second as ".000Z", which RFC3339Nano accepts.
func formatIndexedAt(ns int64) string {
	return time.Unix(0, ns).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}
