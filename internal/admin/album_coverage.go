package admin

import (
	"context"
	"net/http"
	"strconv"
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

// coverageKey names the snapshot identity for the singleflight. Every
// field of coverageSnapshot that the cache-hit check compares appears
// here, and that correspondence is the point: two callers may share a
// build exactly when they would accept each other's stored result.
func coverageKey(epoch uint64, rate, bits int) string {
	return strconv.FormatUint(epoch, 10) + ":" +
		strconv.Itoa(rate) + ":" + strconv.Itoa(bits)
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
// Staleness has TWO causes here and — exactly as in libraryCatalog —
// they are not the same question, so they do not get the same answer:
//
//   - KNOWN-WRONG — the catalog epoch moved (a scan happened, so the
//     album set itself may have changed), the upscale target moved (the
//     operator is about to look at the bars they moved it for), or there
//     is no snapshot at all. Rebuild SYNCHRONOUSLY; there is either
//     nothing to serve or nothing worth serving.
//   - TTL LAPSED, everything else unchanged — nothing told us anything
//     moved. The TTL is a GUESS that the auto-optimize sweeper might
//     have written a variant behind our back. Serve the snapshot we have
//     and refresh behind the request.
//
// That second case is the whole point. This is the ONE whole-library
// rebuild on an interactive navigation path — every other TTL snapshot
// in this package (jobs, diagnostics, enrichment) sits on an endpoint
// the page POLLS, where a blocking rebuild is amortised across a request
// that was going to happen anyway and nobody is watching a grid redraw.
// Blocking here instead put two whole-library scans in front of a person
// opening the Albums page, and because 30 s is shorter than any
// realistic gap between visits it landed on essentially EVERY visit
// rather than on an unlucky one. Measured on the 21,431-track VPS
// library: 252 ms cold against 19 ms warm on an idle bridge, 853 ms
// cold under a concurrent sweep, versus 48 ms for /api/player/artists,
// which reads the same catalog and no coverage. catalog.go's own
// docblock had already written the rule down — "blocking a page load on
// a guess is what made an occasional visitor pay a full fold on
// essentially every visit" — for the snapshot beside this one.
//
// The staleness this admits is bounded by one request cycle, not by a
// second TTL: the refresh is kicked off as we answer, so the NEXT reader
// gets fresh numbers. The badge being a few hundred milliseconds further
// behind is invisible for the reason coverageTTL already gives.
//
// A failure degrades to nil rather than to an error: the grid's job is
// to show albums, and losing a badge is not a reason to lose the page.
func (s *Server) albumCoverageFor(r *http.Request, cat *librarycat.Catalog, needsFilter bool) map[string]albumCoverage {
	if s.deps.Manifest == nil {
		return nil
	}
	// Nothing to say, so do not pay to say it.
	//
	// The grid's badge is drawn from PRESENCE — `covered` and `stale`.
	// With no `track_variants` row anywhere, `AllVariantPresence` comes
	// back empty, `foldPresence` is never reached, and every album's
	// coverage is a denominator and nothing else: covered 0, stale 0 for
	// all of them, which renders no badge on any tile. The expensive
	// half — `AllEligibleKinds`, a full scan of `tracks` carrying three
	// EXISTS subqueries — would be computed entirely to be discarded.
	//
	// The `needs=` filter is the ONE consumer that reads the denominator
	// alone ("albums that still need CarPlay copies" is a real question
	// with zero variants — the answer is "all the eligible ones"), so it
	// forces the build. That is not an optimisation detail: `filterAlbums`
	// treats a nil snapshot as "drop the filter", so skipping the build
	// under a `needs=` query would quietly answer with the UNFILTERED
	// library rather than with nothing.
	//
	// This is the cloud tenant's case and every fresh bridge's — the
	// hosted template ships with upscale and optimize off, so those
	// libraries never hold a variant at all and paid two whole-library
	// reads per page load for a badge that could not appear.
	//
	// Checked BEFORE the snapshot is consulted, so a skipped answer is
	// never stored and cannot be read back by a later `needs=` request.
	if !needsFilter && !s.libraryHasAnyVariant(r.Context()) {
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
		c.rate == rate && c.bits == bits {
		if time.Since(c.builtAt) < coverageTTL {
			return c.byAlbum
		}
		// Stale by the clock only. Hand back what we have and refresh
		// out of band.
		s.refreshCoverageAsync(r.Context(), cat, epoch, rate, bits)
		return c.byAlbum
	}
	return s.rebuildCoverage(r.Context(), cat, epoch, rate, bits)
}

// libraryHasAnyVariant answers the cheap half of "is coverage worth
// building", and fails OPEN.
//
// An error here means we do not know, and the honest response to not
// knowing is to do what the bridge did before this gate existed: build
// the snapshot. Failing closed would drop every badge in the library on
// a transient database error, silently and for a full page — much worse
// than paying for a scan.
func (s *Server) libraryHasAnyVariant(ctx context.Context) bool {
	any, err := s.deps.Manifest.AnyVariantExists(ctx)
	if err != nil {
		logger.Warn("player: probe for any variant", "err", err)
		return true
	}
	return any
}

// rebuildCoverage is the single owner of the coverage singleflight.
//
// Both the synchronous path and the background refresher enter HERE
// rather than through albumCoverageFor, for the two reasons rebuildCatalog
// records: routing through the flight is what makes a request landing
// mid-refresh attach to the running build instead of starting a second
// pair of whole-library scans, and NOT routing back through
// albumCoverageFor is what stops the refresher taking that function's own
// serve-the-stale-copy shortcut and rebuilding nothing at all.
//
// The key is the SNAPSHOT IDENTITY, not a constant — and this is the one
// place where coverage must not copy the catalog, whose flight is keyed
// on `"catalog"` on purpose. A joiner there receives a catalog built for
// a different epoch, and its own docblock accepts that: it is "a
// consistent snapshot, merely not the newest", which is a true statement
// about a library view. Coverage is parameterised by the upscale TARGET
// as well, and a snapshot built for a different (rate, bits) is not an
// older answer to the same question — it is an answer to a different
// one. A joiner would render bars computed against a target nobody asked
// for, and `filterAlbums` would answer `needs=` from them. Identity in
// the key is what makes joining safe.
//
// Two identities can now build concurrently where they would once have
// serialised. That is the trade and it is the right way round: it costs a
// second whole-library pass in the rare window where the target changed
// mid-flight, and it buys never showing a number computed for something
// else.
//
// Returns nil on failure — the caller's "lose a badge, not the page"
// contract.
func (s *Server) rebuildCoverage(ctx context.Context, cat *librarycat.Catalog, epoch uint64, rate, bits int) map[string]albumCoverage {
	key := coverageKey(epoch, rate, bits)
	v, err, _ := s.coverageSF.Do(key, func() (any, error) {
		// Re-check inside the flight: a queued caller must not rebuild
		// what the leader just built.
		if c := s.coverage.Load(); c != nil && c.epoch == epoch &&
			c.rate == rate && c.bits == bits && time.Since(c.builtAt) < coverageTTL {
			return c.byAlbum, nil
		}
		// Detached from the request: the result is shared by every
		// joined caller, so one client hanging up must not cancel a
		// build the others are waiting on. Bounded by its own timeout.
		buildCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), catalogBuildTimeout)
		defer cancel()
		built, err := s.buildAlbumCoverage(buildCtx, cat, rate, bits)
		if err != nil {
			return nil, err // errors are never cached
		}
		if h := coverageBuiltHookForTests; h != nil {
			h()
		}
		// Stored under the epoch captured BEFORE the build, so a scan
		// landing mid-build parks a snapshot the next reader will see as
		// mismatched and rebuild — self-correcting, and the joined
		// callers still get a consistent answer for the catalog they
		// asked about.
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

// refreshCoverageAsync rebuilds the snapshot behind a request that was
// already answered from a clock-stale copy.
//
// What bounds the actual WORK is the singleflight inside rebuildCoverage;
// the `refreshing` flag bounds GOROUTINE SPAWNS, so a burst of stale
// reads spawns one rather than one each that then merely joins the
// running flight. It is deliberately NOT a claim that the flag prevents
// duplicate rebuilds — refreshCatalogAsync's docblock makes the same
// distinction for the same reason.
//
// ONE flag rather than one per target, and that is not the oversight it
// looks like. This function is reachable ONLY from the branch in
// albumCoverageFor where the cached snapshot's epoch, rate and bits all
// EQUAL the request's — a read whose identity differs is a cache miss,
// which takes the synchronous path and never consults this flag. Since
// `s.coverage` holds one snapshot, at most one identity can be
// clock-stale at any moment, so a per-target set would have at most one
// member. A second target cannot be starved here because it was never
// eligible to be refreshed in the background in the first place.
//
// Joined to the same bgRefresh WaitGroup as the catalog refresher, so
// shutdown waits for it and a rebuild cannot still be reading the store
// after Store.Close.
func (s *Server) refreshCoverageAsync(ctx context.Context, cat *librarycat.Catalog, epoch uint64, rate, bits int) {
	if !s.coverageRefreshing.CompareAndSwap(false, true) {
		return
	}
	s.bgRefresh.Add(1)
	go func() {
		defer s.bgRefresh.Done()
		defer s.coverageRefreshing.Store(false)
		// WithoutCancel over the CALLER's ctx rather than a fresh
		// Background, for refreshCatalogAsync's reason: this refresh
		// outlives the request that triggered it by design, but the
		// request-scoped logger and request id should still reach it.
		s.rebuildCoverage(context.WithoutCancel(ctx), cat, epoch, rate, bits)
	}()
}

// coverageBuiltHookForTests fires after each successful coverage fold.
// Production code MUST NOT set it; only tests, restoring via t.Cleanup.
// It exists because "how many rebuilds happened" is otherwise
// unobservable from outside, and a test that cannot count them cannot
// tell a served-stale answer from a blocking one.
var coverageBuiltHookForTests func()

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
	eligible, err := s.deps.Manifest.AllEligibleKinds(ctx, rate, bits, s.eligibilityOpts())
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
