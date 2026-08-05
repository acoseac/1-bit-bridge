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
func (s *Scanner) RestampDuplicates(ctx context.Context) (int, error) {
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
	perTier := map[dupes.Tier]*DupeTierSummary{}
	for _, tier := range []dupes.Tier{dupes.TierDifferentFormat, dupes.TierSameFormat, dupes.TierInconclusive, dupes.TierSelfNested} {
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
			stamps = append(stamps, want)
		}
	}
	// Whatever is left in `current` carries a stamp but belongs to no
	// group anymore (twin deleted, retagged out of the key) — clear it,
	// and push previously-suppressed rows back into the delta stream.
	for path, cur := range current {
		stamps = append(stamps, DupeStamp{Path: path, BumpIndexed: cur.Suppressed})
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
	}
	for _, tier := range []dupes.Tier{dupes.TierDifferentFormat, dupes.TierSameFormat, dupes.TierInconclusive, dupes.TierSelfNested} {
		sum.Tiers = append(sum.Tiers, *perTier[tier])
	}
	if serr := s.store.SaveDupeSummary(ctx, sum); serr != nil {
		// The stamps committed; a summary-write failure must not undo
		// that verdict. Log and carry on — the next pass rewrites it.
		scanLogger.Error("save dupe summary", "err", serr)
	}
	return n, nil
}
