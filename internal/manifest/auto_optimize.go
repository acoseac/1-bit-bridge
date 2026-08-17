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
	// StaleVariantID is non-empty when the candidate already has an
	// `optimized-*` variant whose recorded source mtime/size no longer
	// match the track row — i.e. this is a REGENERATION, not a first
	// generation. Logging-only; the write path needs no special case
	// because `UpsertVariant` is keyed on (source_path, variant_id) and
	// a family-preserving target rate is stable for a given source, so
	// the replacement lands on the same row and the same sidecar path.
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
// The LEFT JOIN is scoped to `optimized-%`: an upscaled variant must
// not read as coverage here (the same kind-scoping mistake
// `ListTrackProjectionsUnderPrefix`'s docblock records).
//
// **Staleness compares the variant against the TRACK ROW, not against
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
// BINDS (textual order): limit.
const autoOptimizeCandidateSQL = `
	SELECT t.path, t.size, t.mtime_ns,
	       COALESCE(t.sample_rate, 0), COALESCE(t.bits_per_sample, 0),
	       COALESCE(t.codec, ''),
	       COALESCE(v.variant_id, '') AS stale_variant_id
	  FROM tracks t
	  LEFT JOIN track_variants v
	         ON v.source_path = t.path
	        AND v.variant_id LIKE 'optimized-%'
	 WHERE t.size > 0
	   AND COALESCE(t.dupe_suppressed, 0) = 0
	   AND NOT EXISTS (SELECT 1 FROM upnp_track_routing u
	                    WHERE u.source_path = t.path)
	   AND ` + optimizeEligibleSQL + `
	   AND ( v.source_path IS NULL
	         OR v.source_mtime_ns != t.mtime_ns
	         OR v.source_size     != t.size )
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
	rows, err := s.db.QueryContext(ctx, autoOptimizeCandidateSQL, limit)
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
	err := s.db.QueryRowContext(ctx, autoOptimizeCandidateCountSQL, -1).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count auto-optimize candidates: %w", err)
	}
	return n, nil
}
