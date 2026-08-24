package admin

import (
	"net/http"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// variantSummaryFor builds the album/artist-level variant readout for a
// set of tracks.
//
// It is nil-safe in the direction that matters: a bridge with the
// upscale feature off, or with no sox, still gets a summary — it just
// reports `enabled:false` / `soxAvailable:false` alongside whatever
// coverage already exists. A UI that only learned "unavailable" could
// not tell an operator whether to change a setting or install a
// toolchain, and could not show the variants a previously-enabled
// bridge already built.
//
// Errors are logged and degrade to a nil summary rather than failing
// the album: the page's job is to play music, and losing the variant
// panel is not a reason to lose the track list.
func (s *Server) variantSummaryFor(r *http.Request, paths []string, tracks []playerTrackDTO) *playerVariantSummaryDTO {
	if s.deps.Manifest == nil {
		return nil
	}
	cfg := s.deps.CfgHolder.Load()
	out := &playerVariantSummaryDTO{
		Enabled:      upscaleFeatureEnabled(cfg),
		SoxAvailable: s.deps.UpscalePrecheck != nil && s.deps.UpscalePrecheck() == nil,
	}

	// Covered / sizes come from the rows already hydrated — no second
	// pass over the store for numbers the caller is holding.
	for _, t := range tracks {
		out.SourceBytes += t.SizeBytes
		// Per TRACK, not per variant row: a track with two sidecars of
		// one kind (an old schema version beside the current one) is
		// still one covered track, and counting rows would push the
		// numerator past the denominator.
		var up, opt variantTally
		for _, v := range t.Variants {
			out.VariantBytes += v.SizeBytes
			switch v.Kind {
			case variantKindUpscale:
				up.note(v.Fresh)
			case variantKindOptimize:
				opt.note(v.Fresh)
			}
		}
		up.foldInto(&out.Upscale)
		opt.foldInto(&out.Optimize)
	}

	rate, bits, err := s.resolveUpscaleTarget(r.Context(), cfg)
	if err != nil {
		logger.Warn("player: resolve upscale target for variant summary", "err", err)
		return out
	}
	counts, err := s.deps.Manifest.EligibleCountsForPaths(r.Context(), paths, rate, bits)
	if err != nil {
		logger.Warn("player: eligible counts for variant summary", "err", err)
		return out
	}
	out.Upscale.Eligible = counts.Upscale
	out.Optimize.Eligible = counts.Optimize
	// Exempt is what is left over: tracks this kind can never apply to
	// (DSD, lossy, unknown geometry, or already at the target). It is a
	// muted footnote, not missing work — the distinction the
	// eligible-denominator bars exist to make.
	out.Upscale.Exempt = exemptCount(len(tracks), counts.Upscale)
	out.Optimize.Exempt = exemptCount(len(tracks), counts.Optimize)
	return out
}

func exemptCount(total, eligible int) int {
	if n := total - eligible; n > 0 {
		return n
	}
	return 0
}

// upscaleFeatureEnabled reports the CONFIGURED state, which is
// deliberately not the same question as whether the pool is running.
// The runtime answer (`/api/upscale/stats.enabled`) is nil when sox
// failed its boot precheck; here the two are reported separately so the
// UI can say "enabled, but sox is missing" rather than collapsing both
// into "off".
func upscaleFeatureEnabled(cfg *config.Config) bool {
	return cfg != nil && cfg.Upscale.Enabled
}

// variantTally accumulates one track's sidecars of a single kind.
//
// A track counts as STALE only when it has no fresh sidecar of the
// kind: a current copy sitting beside a superseded one is served
// correctly, and flagging it would send an operator hunting for a
// problem that is already solved.
type variantTally struct {
	any   bool
	fresh bool
}

func (t *variantTally) note(fresh bool) {
	t.any = true
	if fresh {
		t.fresh = true
	}
}

func (t variantTally) foldInto(c *playerVariantCoverageDTO) {
	if !t.any {
		return
	}
	c.Covered++
	if !t.fresh {
		c.Stale++
	}
}
