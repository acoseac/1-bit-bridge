package admin

import (
	"context"
	"net/http"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/librarycat"
)

// Per-album variant coverage for the album GRID.
//
// The detail views answer this per request with two bounded queries.
// The grid cannot: its FILTER ("albums that still need CarPlay copies")
// has to be applied across the whole library before paging, or page 1
// of the filtered list is drawn from page 1 of the unfiltered one and
// the total is wrong. So the answer is precomputed for every album and
// read O(1) per tile.
//
// What is NOT precomputed is as deliberate. Eligibility could live in
// the catalog — it changes only on a rescan — but it also depends on the
// UPSCALE TARGET, which is a runtime setting the catalog knows nothing
// about; baking it there would leave every bar wrong after a target
// change until the next scan. Keying this snapshot on (epoch, rate,
// bits) makes a target change invalidate it by construction.
//
// Coverage genuinely cannot be cached in the catalog at all: the
// auto-optimize sweeper writes variants continuously and does NOT bump
// the catalog epoch, so a baked mask would tell the operator an album
// still needs work it finished minutes ago.
// It is a wire DTO as well as an internal one — the grid reads it
// straight off the album row — so it carries json tags matching the
// detail views' summary, and a client can use the same rendering for
// both.
type albumCoverage struct {
	Upscale  playerVariantCoverageDTO `json:"upscale"`
	Optimize playerVariantCoverageDTO `json:"optimize"`
}

type coverageSnapshot struct {
	epoch      uint64
	rate, bits int
	builtAt    time.Time
	byAlbum    map[string]albumCoverage
}

// coverageTTL is short because the underlying facts move on their own —
// a background sweep writes variants with nothing to nudge us. The
// DETAIL views are refreshed exactly, off the pool's own progress; this
// is the browse-surface badge, where being half a minute behind is
// invisible and rebuilding on every completed job is not worth two
// table scans.
const coverageTTL = 30 * time.Second

// albumCoverageFor returns the snapshot, rebuilding when the catalog
// epoch moved, the upscale target changed, or the TTL lapsed.
//
// A failure degrades to nil rather than to an error: the grid's job is
// to show albums, and losing a badge is not a reason to lose the page.
func (s *Server) albumCoverageFor(r *http.Request, cat *librarycat.Catalog) map[string]albumCoverage {
	if s.deps.Manifest == nil {
		return nil
	}
	cfg := s.deps.CfgHolder.Load()
	rate, bits, err := s.resolveUpscaleTarget(r.Context(), cfg)
	if err != nil {
		logger.Warn("player: resolve upscale target for album coverage", "err", err)
		return nil
	}
	epoch := s.catalogEpoch.Load()
	if c := s.coverage.Load(); c != nil && c.epoch == epoch &&
		c.rate == rate && c.bits == bits && time.Since(c.builtAt) < coverageTTL {
		return c.byAlbum
	}

	// Same singleflight discipline as the catalog: N tabs hitting an
	// expired snapshot at once collapse to one rebuild.
	v, err, _ := s.coverageSF.Do("coverage", func() (any, error) {
		if c := s.coverage.Load(); c != nil && c.epoch == epoch &&
			c.rate == rate && c.bits == bits && time.Since(c.builtAt) < coverageTTL {
			return c.byAlbum, nil
		}
		// Detached from the request: the result is shared by every
		// joined caller, so one client hanging up must not cancel a
		// build the others are waiting on. Bounded by its own timeout.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), catalogBuildTimeout)
		defer cancel()
		built, err := s.buildAlbumCoverage(ctx, cat, rate, bits)
		if err != nil {
			return nil, err
		}
		s.coverage.Store(&coverageSnapshot{
			epoch: epoch, rate: rate, bits: bits,
			builtAt: time.Now(), byAlbum: built,
		})
		return built, nil
	})
	if err != nil {
		logger.Warn("player: build album coverage", "err", err)
		return nil
	}
	out, _ := v.(map[string]albumCoverage)
	return out
}

// buildAlbumCoverage folds two whole-library reads into a per-album
// answer.
//
// The two big maps are TRANSIENT on purpose: a fully covered 24k-track
// library makes them tens of thousands of entries, while the result is
// one small struct per album — under a thousand entries on the same
// library. Holding the per-path maps would multiply the retained size
// by an order of magnitude for data no reader wants again.
func (s *Server) buildAlbumCoverage(ctx context.Context, cat *librarycat.Catalog, rate, bits int) (map[string]albumCoverage, error) {
	present, err := s.deps.Manifest.AllVariantPresence(ctx)
	if err != nil {
		return nil, err
	}
	eligible, err := s.deps.Manifest.AllEligibleKinds(ctx, rate, bits)
	if err != nil {
		return nil, err
	}
	out := make(map[string]albumCoverage, len(cat.Albums))
	for i := range cat.Albums {
		a := &cat.Albums[i]
		var cov albumCoverage
		for _, p := range a.TrackPaths {
			e := eligible[p]
			if e.Upscale {
				cov.Upscale.Eligible++
			}
			if e.Optimize {
				cov.Optimize.Eligible++
			}
			c, ok := present[p]
			if !ok {
				continue
			}
			foldPresence(&cov.Upscale, c.Upscaled, c.UpscaledFresh)
			foldPresence(&cov.Optimize, c.Optimized, c.OptimizedFresh)
		}
		cov.Upscale.Exempt = exemptCount(len(a.TrackPaths), cov.Upscale.Eligible)
		cov.Optimize.Exempt = exemptCount(len(a.TrackPaths), cov.Optimize.Eligible)
		out[a.ID] = cov
	}
	return out, nil
}

// InvalidateAlbumCoverage drops the snapshot outright.
//
// Wired to the upscale-target write, which is the one input that can
// change without the TTL being the right answer: an operator who has
// just moved the target is about to look at the bars they moved it for.
func (s *Server) InvalidateAlbumCoverage() {
	s.coverage.Store(nil)
}
