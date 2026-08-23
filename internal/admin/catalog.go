package admin

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/acoseac/1-bit-bridge/internal/librarycat"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The library catalog cache — one whole-library snapshot backing every
// /api/player/* read.
//
// Deliberately NOT libMetaCache[T]: that type is path-keyed, bounded at
// 64 entries and 60 s, which is right for "one folder's Atlas refs" and
// wrong for a single library-wide snapshot. The CEREMONY is reused
// though (TTL → singleflight → re-check inside the flight → compute
// under context.WithoutCancel → store), because that ordering is what
// stops a handler skipping a step.
//
// ONE snapshot, not a keyed map: every player endpoint reads slices of
// the same object, and a keyed cache would let two of them disagree
// about the library mid-navigation. Filters and sorts narrow the cached
// snapshot in the HANDLER and never enter the cache key — the
// libMetaMisses precedent, where facet and limit narrow a cached
// snapshot so clicking through facets can't re-walk the library once
// per facet.

const (
	// catalogCacheTTL is deliberately longer than the 60 s
	// composition/enrichment family. This snapshot changes on a SCAN or
	// an enrichment write, and the post-scan nudge makes it
	// correct-on-change; a 60 s TTL would rebuild ~10 times during a
	// ten-minute browsing session for data that hasn't moved. Nothing
	// on the SSE tick touches it.
	catalogCacheTTL = 5 * time.Minute

	// catalogBuildTimeout bounds a rebuild. Measured: a 15k-track
	// library is a fraction of a second end to end. A minute is
	// pathological-host headroom, not an expectation.
	catalogBuildTimeout = 60 * time.Second

	// catalogMaxTracks is a refuse-rather-than-OOM guard. The fold's
	// peak is roughly twice the snapshot (per-group vote maps), and the
	// snapshot is ~3.5 MB at 15k tracks — so this is ~100 MB peak on a
	// host that may be a Pi. Past it, say so instead of dying.
	catalogMaxTracks = 500_000
)

type cachedCatalog struct {
	cat     *librarycat.Catalog
	epoch   uint64
	builtAt time.Time
}

// InvalidateLibraryCatalog marks the snapshot stale. Cheap and
// non-blocking: it bumps a counter, and the NEXT reader rebuilds.
//
// That laziness IS the debounce. postScanNudges fires after every
// successful scan including watcher-driven ScanSubtree, so a bulk
// import or a noisy inotify burst would re-fold the whole library
// dozens of times under an eager rebuild. Here a burst just bumps the
// epoch repeatedly and costs one rebuild — and if nobody is looking at
// the player, it costs none at all.
func (s *Server) InvalidateLibraryCatalog() {
	s.catalogEpoch.Add(1)
}

// libraryCatalog returns the current snapshot, rebuilding if it is cold
// or stale.
//
// The rebuild is SYNCHRONOUS and single-flighted. An async
// swap-when-ready would avoid the one caller's wait, but it also means
// "I scanned, I refreshed, my new album isn't there" — and at a
// fraction of a second on a loopback console that trade is not worth
// the reasoning cost. Revisit if a library ever pushes the build past
// a second or two; the singleflight already bounds the damage to one
// waiter rather than N.
func (s *Server) libraryCatalog(ctx context.Context) (*librarycat.Catalog, error) {
	epoch := s.catalogEpoch.Load()
	if c := s.catalog.Load(); c != nil && c.epoch == epoch &&
		time.Since(c.builtAt) < catalogCacheTTL {
		return c.cat, nil
	}
	v, err, _ := s.catalogSF.Do("catalog", func() (any, error) {
		// Re-check inside the flight: a queued caller must not rebuild
		// what the leader just built.
		cur := s.catalogEpoch.Load()
		if c := s.catalog.Load(); c != nil && c.epoch == cur &&
			time.Since(c.builtAt) < catalogCacheTTL {
			return c.cat, nil
		}
		buildCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), catalogBuildTimeout)
		defer cancel()
		cat, err := s.buildLibraryCatalog(buildCtx)
		if err != nil {
			return nil, err // errors are never cached
		}
		// Fence on the epoch captured BEFORE the build. libMetaCache
		// documents why IT doesn't fence — its retry writes nothing the
		// walk reads — and that reasoning does not carry here: a
		// post-scan nudge fires exactly when `tracks` has just changed,
		// so a build that started pre-scan could otherwise re-park
		// stale data for a full TTL. A superseded result is still
		// RETURNED to the callers that joined this flight (it is a
		// consistent snapshot, merely not the newest); it just isn't
		// stored.
		if s.catalogEpoch.Load() == cur {
			s.catalog.Store(&cachedCatalog{cat: cat, epoch: cur, builtAt: time.Now()})
		}
		return cat, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*librarycat.Catalog), nil
}

// buildLibraryCatalog streams the whole served library through the fold.
//
// No nil check on s.deps.Manifest, deliberately: New() refuses to
// construct a Server without it ("admin: Auth, Manifest, Scanner,
// Resolver are required"), so the field cannot be nil on any Server
// that exists. A defensive check here would be dead code that IMPLIES
// the dependency is optional — which is worse than nothing, because
// the next person to read it may make it optional and never notice the
// constructor guard that says otherwise.
func (s *Server) buildLibraryCatalog(ctx context.Context) (*librarycat.Catalog, error) {
	if n, err := s.deps.Manifest.CountServedTracks(ctx); err == nil && n > catalogMaxTracks {
		return nil, errCatalogTooLarge
	}
	b := librarycat.New()
	err := s.deps.Manifest.StreamCatalogRefs(ctx, func(r manifest.CatalogRef) error {
		b.Add(librarycat.Row{
			Path: r.Path, Title: r.Title, Artist: r.Artist,
			AlbumArtist: r.AlbumArtist, Album: r.Album, Year: r.Year,
			Disc: r.Disc, DiscTagged: r.DiscTagged,
			Track: r.Track, TrackTagged: r.TrackTagged,
			Size: r.Size, Duration: r.Duration,
			SampleRate: r.SampleRate, BitsPerSample: r.BitsPerSample,
			IsDSD: r.IsDSD, Codec: r.Codec,
			Genre: r.Genre, Composer: r.Composer,
			ArtworkMBID: r.ArtworkMBID, ArtworkVersion: r.ArtworkVersion,
			ReleaseMBID: r.ReleaseMBID, ArtistMBID: r.ArtistMBID,
			IndexedAt: r.IndexedAt, RoutedUDN: r.RoutedUDN,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return b.Build(time.Now()), nil
}

// catalogState is the Server-side cache state, embedded by value.
type catalogState struct {
	catalog      atomic.Pointer[cachedCatalog]
	catalogEpoch atomic.Uint64
	catalogSF    singleflight.Group
}

var errCatalogTooLarge = errors.New("library too large for the in-memory catalog")
