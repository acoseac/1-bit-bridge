package admin

import (
	"context"
	"errors"
	"sync"
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
// Staleness has TWO causes and they are not the same question, so they
// no longer get the same answer:
//
//   - EPOCH CHANGED — a scan actually happened, and the snapshot is
//     known-wrong. Rebuild SYNCHRONOUSLY. This is the case the original
//     design protects: "I scanned, I refreshed, my new album isn't
//     there" is a real complaint, and making the one unlucky caller wait
//     a fraction of a second is the right trade.
//   - TTL EXPIRED, epoch unchanged — nothing told us anything moved;
//     the TTL is a GUESS that an unnudged writer (enrichment, a dupe
//     restamp) might have. Serve the snapshot we have and refresh
//     behind the request. Blocking a page load on a guess is what made
//     an occasional visitor pay a full fold on essentially every visit.
//
// Cold (no snapshot at all) stays synchronous — there is nothing to
// serve. `WarmLibraryCatalog` exists so that case is normally paid at
// boot rather than by a person waiting on a page.
//
// EVERY path into a build goes through `catalogSF.Do("catalog", …)` —
// boot warm, cold request, and background refresh alike. That is what
// makes a request arriving mid-warm ATTACH to the running flight
// instead of starting a second fold of the same library, and it is the
// entire reason the singleflight is keyed on a constant.
func (s *Server) libraryCatalog(ctx context.Context) (*librarycat.Catalog, error) {
	epoch := s.catalogEpoch.Load()
	if c := s.catalog.Load(); c != nil && c.epoch == epoch {
		if time.Since(c.builtAt) < catalogCacheTTL {
			return c.cat, nil
		}
		// Stale by the clock only. Hand back what we have and refresh
		// out of band.
		s.refreshCatalogAsync()
		return c.cat, nil
	}
	return s.rebuildCatalog(ctx)
}

// rebuildCatalog is the single owner of the catalog singleflight. Both
// the cold-request path and the background refresher enter HERE rather
// than through libraryCatalog, and that is load-bearing in each
// direction: routing through the flight is what makes a request landing
// mid-warm attach to the running build instead of starting a second fold
// of the same library, and NOT routing back through libraryCatalog is
// what stops the background refresher taking libraryCatalog's own
// serve-the-stale-copy shortcut and rebuilding nothing at all.
func (s *Server) rebuildCatalog(ctx context.Context) (*librarycat.Catalog, error) {
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
		// Test seam (nil in production — one nil-check on a path that
		// just folded the whole library, so the cost is genuinely
		// nothing). It exists because "how many rebuilds happened" is
		// otherwise unobservable from outside, and a test that cannot
		// count them cannot tell real coalescing from luck.
		if h := catalogBuiltHookForTests; h != nil {
			h()
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

// refreshCatalogAsync rebuilds the snapshot behind a request that was
// already answered from a clock-stale copy.
//
// What bounds the actual WORK is the singleflight inside rebuildCatalog:
// a burst of stale reads coalesces into one fold no matter how many
// callers arrive. The `refreshing` flag bounds something narrower and
// worth being precise about — GOROUTINE SPAWNS. Without it, every stale
// read spawns a goroutine that then merely joins the running flight, and
// each of those is a bgRefresh.Add that shutdown has to wait out. With
// it, a burst spawns one.
//
// It is deliberately NOT a claim that the flag prevents duplicate
// rebuilds; it does not, and a test asserting so passes with the flag
// removed.
func (s *Server) refreshCatalogAsync() {
	if !s.catalogRefreshing.CompareAndSwap(false, true) {
		return
	}
	s.bgRefresh.Add(1)
	go func() {
		defer s.bgRefresh.Done()
		defer s.catalogRefreshing.Store(false)
		// Detached ctx: this refresh outlives the request that triggered
		// it by design — that is the point of answering from the stale
		// copy — and it is bounded by catalogBuildTimeout inside the
		// flight. The PR #373 shared-result precedent: a singleflight
		// result belongs to every joined caller, so it must not be
		// cancellable by whichever one happened to start it.
		if _, err := s.rebuildCatalog(context.WithoutCancel(context.Background())); err != nil {
			logger.Warn("background catalog refresh", "err", err)
		}
	}()
}

// WarmLibraryCatalog builds the snapshot ahead of the first request.
//
// Without it the first person to open the player after a restart pays
// the whole fold synchronously. It routes through libraryCatalog, so a
// request that lands mid-warm joins this flight rather than starting a
// second one.
//
// Deliberately NOT re-run on the post-scan nudge: catalog.go's
// invalidation docblock is right that laziness is the debounce, and a
// bulk import or a noisy inotify burst must not re-fold the library
// once per event. This is a one-shot at boot.
func (s *Server) WarmLibraryCatalog(ctx context.Context) {
	start := time.Now()
	cat, err := s.libraryCatalog(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logger.Warn("warm library catalog", "err", err)
		}
		return
	}
	logger.Info("library catalog warm",
		"albums", len(cat.Albums), "artists", len(cat.Artists),
		"took", time.Since(start).Round(time.Millisecond))
}

// WaitForCatalogRefresh drains any in-flight background refresh. Called
// on shutdown so a rebuild cannot still be reading the store after
// Store.Close — the same discipline the bgWriters WaitGroup applies to
// every other background reader.
func (s *Server) WaitForCatalogRefresh() { s.bgRefresh.Wait() }

// catalogState is the Server-side cache state, embedded by value.
type catalogState struct {
	catalog      atomic.Pointer[cachedCatalog]
	catalogEpoch atomic.Uint64
	catalogSF    singleflight.Group

	// catalogRefreshing admits one background refresher at a time; see
	// refreshCatalogAsync for why the singleflight alone is not enough.
	catalogRefreshing atomic.Bool
	// bgRefresh lets shutdown wait for that refresher.
	bgRefresh sync.WaitGroup
}

// catalogBuiltHookForTests fires after each successful fold. Production
// code MUST NOT set it; only tests, restoring via t.Cleanup.
var catalogBuiltHookForTests func()

var errCatalogTooLarge = errors.New("library too large for the in-memory catalog")
