// Candidate selection for the serve-side auto-optimize sweeper — the
// background pre-generation of CarPlay `optimized-*` variants.
//
// Deliberately NOT ListTrackProjectionsUnderPrefix. That query backs
// the admin Library Inspector's pre-flight, where the operator wants
// to see EVERYTHING under a path (operator truth, the served-population
// rule's admin-side exception), and its `HasVariant` is a bare
// existence check. A write path that spends disk and CPU needs three
// things it doesn't offer:
//
//  1. UPnP-routed rows EXCLUDED. A routed row has no local file, so
//     `ResolveChecked` cannot resolve it by construction. Measured on
//     the author's hybrid fixture: 1,559 optimize-eligible rows, 1,525
//     of them routed from a Chord 2Go — a sweeper walking those pays
//     1,525 futile resolve failures per tick and logs a warning for
//     each. Exactly the shape `Store.TrackPathsLocal` fixed for the
//     analysis sweeper.
//  2. Duplicate-suppressed rows EXCLUDED. A suppressed row is never
//     served (`/v1/manifest` filters it), so its variant could never
//     be requested — pure wasted disk.
//  3. STALE variants selected, not just missing ones. `HasVariant`
//     existence means a re-encoded or re-tagged source keeps its old
//     sidecar forever: `serveVariant` answers `410 variant_stale`,
//     iOS silently falls back to the source, and nothing ever
//     regenerates it. The sweeper is the natural place to close that.

package manifest

import (
	"context"
	"fmt"
)

// AutoOptimizeCandidate is one track the auto-optimize sweeper should
// generate (or regenerate) an `optimized-*` variant for. Slim by
// design — the fields are exactly what `transcode.JobSpec` construction
// and the Go-side eligibility re-check need.
type AutoOptimizeCandidate struct {
	Path          string
	Size          int64
	MTimeNS       int64
	SampleRate    int
	BitsPerSample int
	// Codec is the upper-case canonical codec string, empty on legacy
	// pre-codec-column rows. Carried so the caller can re-run
	// `transcode.OptimizeEligible` — the SQL below is a MIRROR of that
	// gate, and on a write path the Go gate stays authoritative.
	Codec string
	// StaleVariantID is non-empty when the candidate already holds SOME
	// `optimized-*` variant — i.e. this is a REGENERATION, not a first
	// generation. A candidate by definition has no FRESH variant (see
	// autoOptimizeCandidateSQL), so any row it does hold is stale or
	// superseded; the subquery picks one deterministically for the log
	// line rather than claiming to identify "the" stale row.
	//
	// Logging-only. The write path needs no special case: `UpsertVariant`
	// is keyed on (source_path, variant_id) and a family-preserving
	// target rate is stable for a given source, so the replacement lands
	// on the same row and the same sidecar path. A SUPERSEDED row (older
	// schema version, or a pre-re-rip target rate) is left alone here and
	// reaped by the orphan-sidecar GC, which is its owner.
	StaleVariantID string
}

// autoOptimizeCandidateSQL selects, newest-indexed-first, the tracks
// that want an `optimized-*` variant.
//
// Plain-column throughout (the migration-v25 accelerator columns) — no
// `json_extract`, so this stays cheap enough to run on every sweep
// tick. Reuses `optimizeEligibleSQL`, which is the lockstep SQL mirror
// of `transcode.OptimizeEligible` pinned by
// `TestEligibilitySQLAgreesWithOptimizeEligible`.
//
// Both variant lookups are scoped to `optimized-%`: an upscaled variant
// must not read as coverage here (the same kind-scoping mistake
// `ListTrackProjectionsUnderPrefix`'s docblock records).
//
// **The coverage test is "NO FRESH variant exists", NOT "some variant
// row is stale" — and it must not be a JOIN.** `track_variants` is keyed
// on (source_path, variant_id) and an optimize id encodes the schema
// version AND the target rate, so one track can hold SEVERAL
// `optimized-%` rows: a `VariantSchemaVersion` bump leaves the old id
// behind (which is exactly why `ListTrackProjectionsUnderPrefix`'s LIKE
// is documented as "version-agnostic to cover both v1 and v2"), and so
// does a re-rip that moves the source between the 44.1k and 48k
// families. The sweeper only ever writes the CURRENT target's id, so a
// superseded row's recorded source facts never advance — it is stale
// forever. Asking "is some row stale" therefore re-selected the track on
// EVERY sweep and regenerated an already-fresh variant, and since
// `UpsertVariant` strict-advances `indexed_at`, every sweep pushed a
// delta row to every paired device: precisely the
// regenerate-every-sweep loop this design exists to avoid. A LEFT JOIN
// also multiplies the row — one track came back once per stale variant,
// double-spending `MaxPerSweep` and over-reporting the backlog — so the
// stale-id lookup is a single-row correlated subquery instead.
// Pinned by TestListAutoOptimizeCandidatesIgnoresSupersededVariantRows.
//
// **Freshness compares the variant against the TRACK ROW, not against
// a fresh stat of the file.** That is deliberate and self-consistent:
// the sweeper stamps `JobSpec.SourceMTimeNS/SourceSize` from these same
// track-row values (matching `Coordinator.buildOptimizeCandidates`), so
// a freshly written variant necessarily satisfies this predicate and
// cannot be re-selected on the next tick. Stamping a live stat instead
// would leave the variant permanently "stale" by this query whenever
// the scanner hadn't caught up yet — a regenerate-every-sweep loop on
// exactly the files that changed most recently. The scanner remains the
// authority on what a file is; a drifted file heals on its next scan,
// which re-stamps `mtime_ns`/`size` and re-selects the row here.
//
// `t.size > 0` skips zero-byte sources (truncated / in-flight uploads):
// sox can't probe them, so they would fail on every sweep forever. The
// same reasoning as `collectAnalysisCandidates`' empty-file skip.
//
// ORDER BY indexed_at DESC puts newly indexed tracks first. That is
// both the literal ask ("automate this for new tracks") and the right
// spend order under a cap or a disk floor: the head of the queue is the
// music most likely to be reached for next.
//
// BINDS (textual order): the transcode-failure suppression cutoff, then
// limit. Both callers — ListAutoOptimizeCandidates and the COUNT wrapper —
// must bind both, in that order.
const autoOptimizeCandidateSQL = `
	SELECT t.path, t.size, t.mtime_ns,
	       COALESCE(t.sample_rate, 0), COALESCE(t.bits_per_sample, 0),
	       COALESCE(t.codec, ''),
	       COALESCE((SELECT sv.variant_id FROM track_variants sv
	                  WHERE sv.source_path = t.path
	                    AND sv.variant_id LIKE 'optimized-%'
	                  ORDER BY sv.variant_id ASC
	                  LIMIT 1), '') AS stale_variant_id
	  FROM tracks t
	 WHERE t.size > 0
	   AND COALESCE(t.dupe_suppressed, 0) = 0
	   AND NOT EXISTS (SELECT 1 FROM upnp_track_routing u
	                    WHERE u.source_path = t.path)
	   AND ` + optimizeEligibleSQL + `
	   AND NOT EXISTS (SELECT 1 FROM track_variants fv
	                    WHERE fv.source_path     = t.path
	                      AND fv.variant_id      LIKE 'optimized-%'
	                      AND fv.source_mtime_ns = t.mtime_ns
	                      AND fv.source_size     = t.size)
	   -- Sources that have failed repeatedly on THIS version of the file are
	   -- suppressed, or the sweeper re-selects them every tick forever: a
	   -- failed job writes no variant row, so nothing else marks them done.
	   AND NOT ` + variantFailureSuppressedSQL + `
	 ORDER BY t.indexed_at DESC, t.path ASC
	 LIMIT ?`

// ListAutoOptimizeCandidates returns up to `limit` tracks needing an
// `optimized-*` variant, newest-indexed first. See
// autoOptimizeCandidateSQL for the selection contract.
//
// A non-positive `limit` returns no rows and no error — the caller's
// cap is a safety property (see config.AutoOptimizeConfig.MaxPerSweep),
// so an unset one must not read as "unbounded".
//
// Read-only, so no `s.mu` (WAL handles concurrent readers).
func (s *Store) ListAutoOptimizeCandidates(ctx context.Context, limit int) ([]AutoOptimizeCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, autoOptimizeCandidateSQL, s.VariantFailureCutoff(), limit)
	if err != nil {
		return nil, fmt.Errorf("list auto-optimize candidates: %w", err)
	}
	defer rows.Close()
	out := []AutoOptimizeCandidate{}
	for rows.Next() {
		var c AutoOptimizeCandidate
		if err := rows.Scan(&c.Path, &c.Size, &c.MTimeNS,
			&c.SampleRate, &c.BitsPerSample, &c.Codec, &c.StaleVariantID); err != nil {
			return nil, fmt.Errorf("scan auto-optimize candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// autoOptimizeCandidateCountSQL wraps the listing statement so ONE copy
// of the predicate serves both. A true `const` (compile-time folded)
// rather than a concatenation at the call site — the shape that keeps
// SonarCloud `go:S2077` quiet, per the `trackFeatureSelect` precedent.
// The inner ORDER BY is redundant for a count and SQLite drops it;
// binding -1 for the inner LIMIT is SQLite's "no limit", which is what
// an uncapped count wants.
//
// TWO binds, and the order is load-bearing: the suppression cutoff comes
// first (its `?` is in the WHERE clause) and the LIMIT second. Sharing the
// statement is what keeps the card's number and the sweeper's selection in
// agreement — but it also means a new bind has to be added at BOTH call
// sites, which is how this broke when the debounce landed.
const autoOptimizeCandidateCountSQL = `SELECT COUNT(*) FROM (` + autoOptimizeCandidateSQL + `)`

// CountAutoOptimizeCandidates returns how many tracks currently want an
// `optimized-*` variant — the same predicate
// `ListAutoOptimizeCandidates` selects on, without the cap. Drives the
// admin sweep card's "N remaining" so the operator can see the backlog
// draining rather than only the current tick's enqueue count.
//
// Shares the predicate with the listing by construction (one const, two
// statements) so the number the card shows and the work the sweeper
// does cannot drift.
func (s *Store) CountAutoOptimizeCandidates(ctx context.Context) (int, error) {
	var n int
	// Same bind order as the listing it wraps: the suppression cutoff (its
	// `?` sits in the WHERE) then the LIMIT. -1 is SQLite's "no limit",
	// which is what an uncapped count wants.
	err := s.db.QueryRowContext(ctx, autoOptimizeCandidateCountSQL,
		s.VariantFailureCutoff(), -1).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count auto-optimize candidates: %w", err)
	}
	return n, nil
}
