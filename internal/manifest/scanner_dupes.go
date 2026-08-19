// The post-scan duplicate stamping pass: group the library by the iOS
// client-key (internal/dupes), elect winners under the configured
// policy, and stamp the v31 dupe columns so the Served* readers know
// what to exclude. Runs as pass #6 of Scan's success tail (after every
// metadata reconciliation, so group keys see post-reconciliation tags)
// and on demand from the duplicates sweeper when the operator flips the
// policy (hot-apply, no restart).
package manifest

import (
	"context"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// SetDupePolicy installs the policy-snapshot source the stamping pass
// reads at the START of each run. cmd/bridge wires a closure over the
// live RuntimeConfig holder, so a settings PATCH is picked up by the
// next pass with no scanner restart. The closure must be cheap and
// side-effect-free. nil / never-set (tests, bare CLI scans without
// config wiring) means groups and tiers are still stamped but NOTHING is
// suppressed — fail-open: an unwired scanner can never hide tracks.
func (s *Scanner) SetDupePolicy(fn func() dupes.Policy) {
	if fn == nil {
		return
	}
	s.dupePolicy.Store(&fn)
}

func (s *Scanner) currentDupePolicy() dupes.Policy {
	if fn := s.dupePolicy.Load(); fn != nil {
		return (*fn)()
	}
	return dupes.Policy{Mode: dupes.FilterOff}
}

// dupeSummaryTierOrder is the summary's stable tier ordering (the CLI's
// presentation order): different-masters first, the MD5-refined tiers
// beside their same-format parent, the filesystem-fact tier last.
var dupeSummaryTierOrder = []dupes.Tier{
	dupes.TierDifferentFormat, dupes.TierDifferentAudio, dupes.TierSameFormat,
	dupes.TierIdenticalAudio, dupes.TierInconclusive, dupes.TierSelfNested,
}

// beforeApplyDupeStampsHookForTests fires between the election and the
// commit-time scan-in-flight re-check, so a test can make a scan appear
// mid-pass and pin that the guard sits AFTER the snapshot rather than at
// the top of the pass (where it would catch nothing the caller's own
// pre-check missed). nil in production — one nil check per pass, against
// two full-library streams. Same file-scope test-seam convention as
// afterExtractHookForTests; production code MUST NOT set it.
var beforeApplyDupeStampsHookForTests func()

// restampDuplicatesNonFatal runs one stamping pass on behalf of a scan
// and logs the outcome instead of failing the scan — one pass failing
// must not turn an otherwise-good scan into an error.
//
// "Best-effort" is NOT symmetric, which is why this is called from every
// successful exit rather than just the happy one. A never-stamped row is
// served (fail-open), but a STALE SUPPRESSION hides a row that should be
// served — fail-CLOSED — and nothing else in the system clears it: the
// upserts leave the v31 columns alone by design, so only another
// stamping pass can. Until this pass runs, the healing horizon is the
// next scan.
//
// insideScan=true: the caller holds s.mu and is itself what makes
// activeScans non-zero, so it must never hit the abandon guard.
func (s *Scanner) restampDuplicatesNonFatal(ctx context.Context) {
	if n, err := s.restampDuplicates(ctx, true); err != nil {
		scanLogger.Error("duplicate stamping", "err", err)
	} else if n > 0 {
		scanLogger.Info("duplicate stamping updated serving stamps", "tracks", n)
	}
}

// RestampDuplicates runs one full stamping pass and returns the number
// of rows whose stamps changed. Shape:
//
//  1. Two streaming passes over StreamTrackDupeRefsUnderPrefix (keys
//     only, then members for keys seen ≥2 — the collector's OOM
//     discipline). UPnP-routed rows are excluded by the stream itself;
//     their lifecycle belongs to the ingest reconcile and they are never
//     stamped or suppressed.
//  2. Winner election per group under the policy snapshot
//     (dupes.PlanSuppression — DSD/PCM never cross-suppressed,
//     inconclusive never suppressed, exactly one served winner per
//     suppression unit).
//  3. Diff desired stamps against each row's CURRENT stamp state and
//     write only the changes (ApplyDupeStamps) — a stable library is
//     zero writes. Rows that fell out of every group (their twin was
//     deleted, or the key changed) are cleared; a row transitioning
//     suppressed→served carries the strict-advance indexed_at bump so
//     delta-syncing clients recover it. Rows BECOMING suppressed are
//     not bumped — they leave the served stream, and already-synced
//     clients keep them (hidden by the client-side dedup) until their
//     next full sync; that asymmetry is the whole delta story.
//  4. Persist the DupeSummary scan-state document for the admin tiles
//     and the CLI (after the stamps commit, so the summary always
//     describes applied state).
//
// This is the EXTERNAL entry point (the duplicates sweeper, the CLI,
// tests): it runs without s.mu, so it re-checks for an in-flight scan
// before committing — see restampDuplicates.
func (s *Scanner) RestampDuplicates(ctx context.Context) (int, error) {
	return s.restampDuplicates(ctx, false)
}

// restampDuplicates is the shared body. `insideScan` is true only for
// the Scan / ScanSubtree success tails, which already hold s.mu and are
// themselves the reason activeScans is non-zero — their commit is the
// authoritative one and must never abandon.
func (s *Scanner) restampDuplicates(ctx context.Context, insideScan bool) (int, error) {
	policy := s.currentDupePolicy()

	c := dupes.NewCollector()
	if err := s.store.StreamTrackDupeRefsUnderPrefix(ctx, "", false, func(r dupes.Row, _ DupeStampState) error {
		c.Note(r)
		return nil
	}); err != nil {
		return 0, err
	}
	c.Seal()
	// Pass 2 also snapshots the CURRENT stamp state, but only for rows
	// that carry one — bounded by the library's duplicate population,
	// not its size. Leftover entries after the group walk below are
	// stale stamps to clear.
	current := map[string]DupeStampState{}
	if err := s.store.StreamTrackDupeRefsUnderPrefix(ctx, "", false, func(r dupes.Row, st DupeStampState) error {
		c.Collect(r)
		if st.GroupID != "" || st.Tier != "" || st.Suppressed {
			current[r.Path] = st
		}
		return nil
	}); err != nil {
		return 0, err
	}

	groups := c.Groups()
	var stamps []DupeStamp
	suppressedTotal := 0
	md5Known, md5Total := 0, 0
	perTier := map[dupes.Tier]*DupeTierSummary{}
	for _, tier := range dupeSummaryTierOrder {
		perTier[tier] = &DupeTierSummary{Tier: string(tier)}
	}
	for _, g := range groups {
		gid := g.Key.ID()
		suppress := map[string]bool{}
		for _, p := range dupes.PlanSuppression(g, policy) {
			suppress[p] = true
		}
		if ts := perTier[g.Tier]; ts != nil {
			ts.Groups++
			ts.RedundantFiles += len(g.Members) - 1
			ts.NonLargestBytes += g.RedundantBytes()
			ts.Suppressed += len(suppress)
		}
		suppressedTotal += len(suppress)
		k, tot := g.MD5Coverage()
		md5Known += k
		md5Total += tot
		for _, m := range g.Members {
			want := DupeStamp{
				Path:       m.Path,
				GroupID:    gid,
				Tier:       string(g.Tier),
				Suppressed: suppress[m.Path],
			}
			cur, had := current[m.Path]
			delete(current, m.Path)
			if had && cur.GroupID == want.GroupID && cur.Tier == want.Tier && cur.Suppressed == want.Suppressed {
				continue // unchanged — the stable-library zero-write case
			}
			want.BumpIndexed = had && cur.Suppressed && !want.Suppressed
			// Served→suppressed (the BumpIndexed mirror): the row leaves
			// the served set, so journal a deletion tombstone for delta
			// clients. `!had` counts — a first-ever stamp that suppresses
			// hides a row clients may have synced while it was unstamped.
			want.JournalDelete = want.Suppressed && !(had && cur.Suppressed)
			stamps = append(stamps, want)
		}
	}
	// Whatever is left in `current` carries a stamp but belongs to no
	// group anymore (twin deleted, retagged out of the key) — clear it,
	// and push previously-suppressed rows back into the delta stream.
	for path, cur := range current {
		stamps = append(stamps, DupeStamp{Path: path, BumpIndexed: cur.Suppressed})
	}

	// Commit-time re-check of the scan-in-flight predicate. Everything
	// above is a SNAPSHOT: two full-library streams plus the election,
	// which on a large library is seconds of wall clock. An external
	// caller (the sweeper) passed its own IsScanning() gate before all
	// of that, so a scan may have started since — and that scan runs its
	// own stamping tail from fresher state. Committing here would write
	// the stale snapshot over it, un-suppressing rows the scan just
	// suppressed (or re-suppressing rows it cleared) with no third pass
	// to heal them until the next scan.
	//
	// Abandoning is the cheap, correct response: stamps are best-effort
	// and this pass had nothing the scan's own tail won't produce. Taking
	// s.mu instead would deadlock the in-scan callers and, for the
	// external ones, block the scan for the length of two library walks.
	//
	// Residual, accepted: a scan that starts AND finishes entirely inside
	// this window is undetectable here. A full scan walks the library, so
	// it is far slower than this pass; and the sweeper is nudge-only
	// (operator policy flip), not a tick.
	if hook := beforeApplyDupeStampsHookForTests; hook != nil {
		hook()
	}
	if !insideScan && s.activeScans.Load() > 0 {
		scanLogger.Info("duplicate stamping abandoned: scan started mid-pass (its tail applies the current policy)",
			"stamps", len(stamps))
		return 0, nil
	}

	n, err := s.store.ApplyDupeStamps(ctx, stamps)
	if err != nil {
		return 0, err
	}

	sum := DupeSummary{
		SchemaVersion: DupeSummarySchemaVersion,
		StampedAt:     time.Now().UTC(),
		Policy:        string(policy.Mode),
		Scanned:       c.Observed(),
		Groups:        len(groups),
		Suppressed:    suppressedTotal,
		Served:        c.Observed() - suppressedTotal,
		MD5Known:      md5Known,
		MD5Total:      md5Total,
	}
	for _, tier := range dupeSummaryTierOrder {
		sum.Tiers = append(sum.Tiers, *perTier[tier])
	}
	if serr := s.store.SaveDupeSummary(ctx, sum); serr != nil {
		// The stamps committed; a summary-write failure must not undo
		// that verdict. Log and carry on — the next pass rewrites it.
		scanLogger.Error("save dupe summary", "err", serr)
	}
	return n, nil
}
