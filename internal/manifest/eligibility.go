// Transcode-eligibility aggregation for the admin Library Inspector's
// coverage bars (eligible-denominator semantics).
//
// The tile bars answer "did my generate finish?" — so their
// denominator counts tracks that HAVE a variant of the kind OR are
// currently ELIGIBLE to get one. Tracks that need nothing (already at
// the CarPlay floor / at the upscale target / DSD / unknown geometry)
// drop out of the denominator instead of pinning mixed folders below
// 100% forever (field case: ABBA 62/136 "optimized" where the other
// 74 tracks were already 16/44.1 — natively CarPlay-ready).
//
// The predicates are SQL MIRRORS of the Go gates:
//
//   - optimizeEligibleSQL  ⇄ transcode.OptimizeEligible
//   - upscaleEligibleSQL   ⇄ Coordinator.Submit's candidate walk
//     (internal/transcode/batch.go)
//   - dsdRenderEligibleSQL ⇄ transcode.DSDRenderEligible (composed into
//     the optimize denominator for the DSD compact tier; the faithful
//     `pcm` tier is it alone), gated by EligibilityOpts — the SQL form
//     of transcode.DSDRenderCaps
//
// The duplication is deliberate (the rollups must stay plain-column
// SQL — no json_extract on the browse hot path, which is what the v25
// columns exist for) and is pinned by the admin package's lockstep
// truth-table tests (TestEligibilitySQLAgreesWithOptimizeEligible /
// ...WithUpscaleSubmitGate — admin is the only package importing both
// manifest and transcode). CHANGE THE GO GATE AND THE SQL TOGETHER.
package manifest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// optimizeEligibleSQL mirrors transcode.OptimizeEligible exactly:
// lossless-PCM allowlist (with the legacy extension fallback for
// codec-empty rows) AND above the CarPlay floor (rate > 48000 OR
// bits > 16). DSD needs no explicit arm — DSF/DFF codecs fail the
// allowlist, and codec-empty DSD files don't carry PCM extensions.
// SQLite LIKE is ASCII-case-insensitive, matching the Go ToLower ext
// compare.
//
// NOTE: SQLite's TRIM is NOT strings.TrimSpace — it strips only U+0020,
// where Go also strips \t \n \v \f \r and Unicode spaces. Harmless
// today because every Track.Codec writer stamps a hardcoded literal
// (extractors.go / mp4codec.go), so no whitespace-bearing codec can
// reach the column. If a future extractor ever stamps a codec straight
// from a tag, this mirror stops agreeing — normalise on the Go side
// before it lands in the column.
//
// References a `tracks` row aliased as `t`. No binds.
const optimizeEligibleSQL = `(
	( UPPER(TRIM(COALESCE(t.codec,''))) IN ('FLAC','ALAC','WAV','AIFF','PCM')
	  OR ( TRIM(COALESCE(t.codec,'')) = ''
	       AND ( t.path LIKE '%.flac' OR t.path LIKE '%.wav'
	             OR t.path LIKE '%.aif' OR t.path LIKE '%.aiff'
	             OR t.path LIKE '%.m4a' ) ) )
	AND (COALESCE(t.sample_rate,0) > 48000 OR COALESCE(t.bits_per_sample,0) > 16)
)`

// upscaleEligibleSQL mirrors Coordinator.Submit's candidate gate at a
// bound (targetRate, targetBits): NOT a lossy encode (the IsLossyCodec
// set — upscaling decoded lossy audio adds no fidelity, and
// PROTOCOL.md documents the /v1/upscale gate as "PCM"), known
// geometry, never downsample on either axis, skip the exact-at-target
// no-op. DSD falls out via rate > target (2.8 MHz DSD rates dwarf any
// PCM target); the explicit is_dsd arm is belt-and-braces for exotic
// rows. A NULL/empty codec is NOT lossy (legacy pre-codec rows with
// valid geometry stay eligible — IsLossyCodec's documented fail-open).
//
// The NOT IN set is the SQL mirror of manifest.IsLossyCodec — change
// the two together; TestEligibilitySQLAgreesWithUpscaleSubmitGate
// pins the agreement.
//
// BINDS (textual order): targetRate, targetBits, targetRate,
// targetBits. Zero/negative binds (target unresolved) collapse the
// predicate to false for every known-geometry row — the denominator
// degrades to covered-only, which is the safe rendering.
const upscaleEligibleSQL = `(
	COALESCE(t.is_dsd,0) != 1
	AND UPPER(TRIM(COALESCE(t.codec,''))) NOT IN ('MP3','AAC','OGG','OPUS','WMA')
	AND COALESCE(t.sample_rate,0) > 0 AND COALESCE(t.bits_per_sample,0) > 0
	AND NOT (t.sample_rate > ? OR t.bits_per_sample > ?)
	AND NOT (t.sample_rate = ? AND t.bits_per_sample = ?)
)`

// EligibilityOpts carries the DSD-render capability into the SQL
// mirrors. The Go gate's input is transcode.DSDRenderCaps; manifest
// cannot import transcode (cycle), so the caller folds it:
// DSDRender = caps.Active(), DST = caps.Active() && caps.DecodeDST. The
// zero value is "PCM only", under which every predicate below is
// byte-for-byte the pre-v43 one — a caller that has not been wired to
// the caps yet (the admin, until the serve wiring threads them) simply
// keeps its old numbers.
type EligibilityOpts struct {
	DSDRender bool
	DST       bool
}

// binds is the two-bind textual order dsdRenderEligibleSQL takes:
// dsdRender, then dst.
func (o EligibilityOpts) binds() []any {
	return []any{boolToInt(o.DSDRender), boolToInt(o.DST)}
}

// eligibilityBinds is the ONE bind list for the three coverage
// composites in their textual order — upscale (targetRate, targetBits,
// targetRate, targetBits), then optimize (dsdRender, dst), then pcm
// (dsdRender, dst) — followed by the caller's own trailing binds. Every
// helper in this file composes its query in that order and binds
// through here, so a new bind lands in all of them or none.
func eligibilityBinds(targetRate, targetBits int, opts EligibilityOpts, trailing ...any) []any {
	args := []any{targetRate, targetBits, targetRate, targetBits}
	args = append(args, opts.binds()...)
	args = append(args, opts.binds()...)
	return append(args, trailing...)
}

// sacdVirtualPathSQL is the GLOB form of SACDVirtualContainer's shape —
// `<container>.iso/<st|mc>/<NN>.dff` — so the DSD-render mirror can
// refuse a virtual SACD track (the bridge has no demuxer; handing the
// container to ffmpeg renders the whole disc image). Case-insensitive on
// the `.iso` extension like the Go side; `.dff` lower-case like the Go
// side (strings.CutSuffix). It admits a few index strings the Go parser
// rejects (00, 000–099, 256–999), which no minter ever produces — the
// Go gate stays authoritative on every write path.
const sacdVirtualPathSQL = `(
	   t.path GLOB '*.[iI][sS][oO]/st/[0-9][0-9].dff'
	OR t.path GLOB '*.[iI][sS][oO]/st/[0-9][0-9][0-9].dff'
	OR t.path GLOB '*.[iI][sS][oO]/mc/[0-9][0-9].dff'
	OR t.path GLOB '*.[iI][sS][oO]/mc/[0-9][0-9][0-9].dff'
)`

// dsdRenderEligibleSQL mirrors transcode.DSDRenderEligible term for
// term: caps active (bind 1); the row's own is_dsd flag AND a DSD codec
// — both, so a forged isDSD on an MP3 row cannot reach the decoder;
// codec-empty legacy rows fall back to the extension exactly as
// IsDSDSource does; a rate in the 44.1k or 48k family
// (TargetRateForPCMRender's rule — an off-family header is refused
// outright); DST-compressed only when the dst decoder is present
// (bind 2, against the v43 `tracks.compression` accelerator); never an
// SACD virtual track.
//
// References a `tracks` row aliased as `t`. BINDS (textual order):
// dsdRender, dst — EligibilityOpts.binds.
const dsdRenderEligibleSQL = `(
	? = 1
	AND COALESCE(t.is_dsd,0) = 1
	AND ( UPPER(TRIM(COALESCE(t.codec,''))) IN ('DSF','DFF')
	      OR ( TRIM(COALESCE(t.codec,'')) = ''
	           AND ( t.path LIKE '%.dsf' OR t.path LIKE '%.dff' ) ) )
	AND COALESCE(t.sample_rate,0) > 0
	AND ( COALESCE(t.sample_rate,0) % 44100 = 0 OR COALESCE(t.sample_rate,0) % 48000 = 0 )
	AND ( ? = 1 OR UPPER(TRIM(COALESCE(t.compression,''))) != 'DST' )
	AND NOT ` + sacdVirtualPathSQL + `
)`

// <kind>CoveredOrEligibleSQL is the coverage DENOMINATOR predicate:
// this track already HAS a variant of the kind, OR is currently
// eligible to get one.
//
// Hoisted because EligibleCountsForFolders (the tile bar) and
// EligibleRollupByPrefix (the action panel's header) render the SAME
// folder's numbers from separate queries. Inlined, the two composites
// were byte-identical copies pinned by separate tests with separate
// fixtures — so a one-sided edit would make the two surfaces
// contradict each other with nothing failing. Sharing the const makes
// the agreement structural, exactly as <kind>EligibleSQL already does
// one level down.
//
// Binds: whatever the composed <kind>EligibleSQL takes — the four
// upscale target binds; the two DSD-render binds for optimize AND for
// pcm (eligibilityBinds lays them out in textual order).
const (
	upscaleCoveredOrEligibleSQL = `(
		EXISTS(SELECT 1 FROM track_variants tv
		        WHERE tv.source_path = t.path
		          AND tv.variant_id LIKE 'upscaled-%')
		OR ` + upscaleEligibleSQL + `
	)`

	optimizeCoveredOrEligibleSQL = `(
		EXISTS(SELECT 1 FROM track_variants tv
		        WHERE tv.source_path = t.path
		          AND tv.variant_id LIKE 'optimized-%')
		OR ` + optimizeEligibleSQL + `
		OR ` + dsdRenderEligibleSQL + `
	)`

	// pcmCoveredOrEligibleSQL is the faithful DSD tier's denominator: a
	// `pcm-` variant exists, or the row could get one. A DSD source that
	// is eligible for the compact tier is eligible for this one — the
	// two share dsdRenderEligibleSQL, exactly as the Go gates share
	// DSDRenderEligible.
	pcmCoveredOrEligibleSQL = `(
		EXISTS(SELECT 1 FROM track_variants tv
		        WHERE tv.source_path = t.path
		          AND tv.variant_id LIKE 'pcm-%')
		OR ` + dsdRenderEligibleSQL + `
	)`
)

// EligibleCounts carries the per-kind coverage DENOMINATORS for a
// scope: tracks with a variant of the kind plus tracks currently
// eligible to get one. The numerators are the existing covered
// counts (ChildFolderRollup / FolderRollup).
type EligibleCounts struct {
	Upscale  int
	Optimize int
	// PCM is the faithful DSD tier's denominator (`pcm-` variants plus
	// DSD sources eligible to get one). Always 0 under a PCM-only
	// EligibilityOpts.
	PCM int
}

// EligibleCountsForFolders returns, for each folder path in `paths`,
// the per-kind coverage denominator over that folder's subtree.
//
// Paths travel as ONE bound JSON array consumed via json_each (the
// ResetEnrichedByArtistMBIDs pattern — no placeholder concatenation,
// no bind-ceiling chunking). Subtree scoping uses the same
// `path >= p || '/' AND path < p || '0'` range trick as
// childFolderRollupSelect ('0' is the ASCII successor of '/'), so
// the per-folder probes ride the tracks PK index.
//
// BIND ORDER IS LOAD-BEARING: the upscale correlated subquery's four
// eligibility binds (targetRate, targetBits, targetRate, targetBits),
// then the optimize subquery's two DSD-render binds, then the pcm
// subquery's two, precede the json_each blob in textual order
// (eligibilityBinds) — pinned by TestEligibleCountsForFolders_bindingOrder.
//
// Read-only; no s.mu (WAL handles concurrent readers).
func (s *Store) EligibleCountsForFolders(ctx context.Context, paths []string, targetRate, targetBits int, opts EligibilityOpts) (map[string]EligibleCounts, error) {
	if len(paths) == 0 {
		return map[string]EligibleCounts{}, nil
	}
	blob, err := json.Marshal(paths)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT je.value,
		  (SELECT COUNT(*) FROM tracks t
		     WHERE t.path >= je.value || '/' AND t.path < je.value || '0'
		       AND `+upscaleCoveredOrEligibleSQL+`),
		  (SELECT COUNT(*) FROM tracks t
		     WHERE t.path >= je.value || '/' AND t.path < je.value || '0'
		       AND `+optimizeCoveredOrEligibleSQL+`),
		  (SELECT COUNT(*) FROM tracks t
		     WHERE t.path >= je.value || '/' AND t.path < je.value || '0'
		       AND `+pcmCoveredOrEligibleSQL+`)
		FROM json_each(?) je
	`, eligibilityBinds(targetRate, targetBits, opts, string(blob))...)
	if err != nil {
		return nil, fmt.Errorf("eligible counts: %w", err)
	}
	defer rows.Close()
	out := make(map[string]EligibleCounts, len(paths))
	for rows.Next() {
		var p string
		var ec EligibleCounts
		if err := rows.Scan(&p, &ec.Upscale, &ec.Optimize, &ec.PCM); err != nil {
			return nil, err
		}
		out[p] = ec
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// EligibleRollupByPrefix is the whole-subtree twin of
// EligibleCountsForFolders for the action panel's current-node
// header. Empty prefix means the whole library. Same bind-order
// contract for the four upscale-eligibility binds; the two optional
// prefix binds trail them.
//
// Read-only; no s.mu.
func (s *Store) EligibleRollupByPrefix(ctx context.Context, prefix string, targetRate, targetBits int, opts EligibilityOpts) (EligibleCounts, error) {
	q := `
		SELECT
		  COALESCE(SUM(CASE WHEN ` + upscaleCoveredOrEligibleSQL + `
		    THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN ` + optimizeCoveredOrEligibleSQL + `
		    THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN ` + pcmCoveredOrEligibleSQL + `
		    THEN 1 ELSE 0 END), 0)
		FROM tracks t`
	args := eligibilityBinds(targetRate, targetBits, opts)
	if base := strings.TrimRight(prefix, "/"); base != "" {
		// The range appends its own '/', so a caller-supplied trailing
		// slash (or several) would build `path >= 'Album//'` — and since the byte
		// after "Album/" in a real row ('T' = 0x54) is above '0' (0x30),
		// the upper bound `< 'Album/0'` excludes EVERY row. The result is
		// a silently-empty count, not an error: the Inspector renders
		// "0 eligible" for a folder full of work. Same guard, same
		// reasoning, as RollupByPrefix in store.go — these two must stay
		// in lockstep.
		q += `
		WHERE t.path >= ? || '/' AND t.path < ? || '0'`
		args = append(args, base, base)
	}
	var ec EligibleCounts
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&ec.Upscale, &ec.Optimize, &ec.PCM); err != nil {
		return EligibleCounts{}, fmt.Errorf("eligible rollup %q: %w", prefix, err)
	}
	return ec, nil
}

// eligibleCountsForPathsSQL is a named const rather than a call-site
// concatenation: every operand is already a constant and Go folds them
// identically, but SonarCloud's go:S2077 reads a binary expression in
// the query argument as an assembled query.
const eligibleCountsForPathsSQL = `
	SELECT
	  COALESCE(SUM(CASE WHEN ` + upscaleCoveredOrEligibleSQL + `
	    THEN 1 ELSE 0 END), 0),
	  COALESCE(SUM(CASE WHEN ` + optimizeCoveredOrEligibleSQL + `
	    THEN 1 ELSE 0 END), 0),
	  COALESCE(SUM(CASE WHEN ` + pcmCoveredOrEligibleSQL + `
	    THEN 1 ELSE 0 END), 0)
	FROM tracks t
	WHERE t.path IN (SELECT value FROM json_each(?))`

// EligibleCountsForPaths is the identity-scoped twin of
// EligibleRollupByPrefix: the per-kind coverage denominator over an
// EXPLICIT set of tracks rather than a subtree.
//
// It exists for the same reason TrackProjectionsForPaths does. An album
// is a set, not a subtree — its directory is the common ancestor of its
// tracks and is routinely shared with other albums — so a subtree
// rollup would count the neighbours' tracks into this album's
// denominator and the coverage bar would never reach full.
//
// Shares upscaleCoveredOrEligibleSQL / optimizeCoveredOrEligibleSQL
// with both subtree helpers, so all three scopes agree on what
// "eligible" means by construction rather than by review.
//
// BIND ORDER IS LOAD-BEARING, and it is the same contract as the
// sibling helpers: the upscale predicate's four target binds
// (targetRate, targetBits, targetRate, targetBits) appear in textual
// order BEFORE this query's own `?`, the json_each path array. Pinned
// by TestEligibleCountsForPaths_bindingOrder.
//
// Read-only; no s.mu (WAL handles concurrent readers).
func (s *Store) EligibleCountsForPaths(ctx context.Context, paths []string, targetRate, targetBits int, opts EligibilityOpts) (EligibleCounts, error) {
	if len(paths) == 0 {
		return EligibleCounts{}, nil
	}
	var ec EligibleCounts
	// Chunked for the same reason TrackProjectionsForPaths is: an
	// artist scope can carry thousands of paths, and the counts sum
	// cleanly across chunks because every track appears in exactly one.
	for start := 0; start < len(paths); start += eligibleCountsChunk {
		end := start + eligibleCountsChunk
		if end > len(paths) {
			end = len(paths)
		}
		blob, err := json.Marshal(paths[start:end])
		if err != nil {
			return EligibleCounts{}, err
		}
		var chunk EligibleCounts
		if err := s.db.QueryRowContext(ctx, eligibleCountsForPathsSQL,
			eligibilityBinds(targetRate, targetBits, opts, string(blob))...).
			Scan(&chunk.Upscale, &chunk.Optimize, &chunk.PCM); err != nil {
			return EligibleCounts{}, fmt.Errorf("eligible counts for paths: %w", err)
		}
		ec.Upscale += chunk.Upscale
		ec.Optimize += chunk.Optimize
		ec.PCM += chunk.PCM
	}
	return ec, nil
}

// eligibleCountsChunk matches TrackProjectionsForPaths' chunk size.
const eligibleCountsChunk = 400

// EligibleKinds is one track's per-kind coverage-denominator
// membership: whether it already has a variant of the kind OR could
// still get one. Same predicate as the count helpers, per row instead
// of summed.
type EligibleKinds struct {
	Upscale  bool
	Optimize bool
	PCM      bool
}

// allEligibleKindsSQL is a named const for the same reason
// trackProjectionsForPathsSQL is: every operand is already a constant,
// but go:S2077 reads a concatenation in the query argument as an
// assembled query.
const allEligibleKindsSQL = `
	SELECT t.path,
	  CASE WHEN ` + upscaleCoveredOrEligibleSQL + ` THEN 1 ELSE 0 END,
	  CASE WHEN ` + optimizeCoveredOrEligibleSQL + ` THEN 1 ELSE 0 END,
	  CASE WHEN ` + pcmCoveredOrEligibleSQL + ` THEN 1 ELSE 0 END
	FROM tracks t`

// AllEligibleKinds returns the denominator membership for EVERY track.
//
// Whole-library because its caller is a FILTER: "albums that still need
// CarPlay copies" has to be answered across the library before paging,
// or page 1 of the filtered list is drawn from page 1 of the unfiltered
// one. A per-page query cannot produce a correct total either.
//
// Cheap enough to be a cached snapshot rather than a per-request cost:
// this is plain-column SQL over the v25 accelerator columns, with no
// json_extract and no correlated subquery beyond the variant EXISTS the
// shared predicates already carry.
//
// Read-only; no s.mu.
func (s *Store) AllEligibleKinds(ctx context.Context, targetRate, targetBits int, opts EligibilityOpts) (map[string]EligibleKinds, error) {
	rows, err := s.db.QueryContext(ctx, allEligibleKindsSQL,
		eligibilityBinds(targetRate, targetBits, opts)...)
	if err != nil {
		return nil, fmt.Errorf("eligible kinds: %w", err)
	}
	defer rows.Close()
	out := make(map[string]EligibleKinds)
	for rows.Next() {
		var (
			p            string
			up, opt, pcm int
		)
		if err := rows.Scan(&p, &up, &opt, &pcm); err != nil {
			return nil, err
		}
		out[p] = EligibleKinds{Upscale: up != 0, Optimize: opt != 0, PCM: pcm != 0}
	}
	return out, rows.Err()
}
