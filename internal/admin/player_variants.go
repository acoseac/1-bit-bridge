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
func (s *Server) variantSummaryFor(r *http.Request, paths []string, sourceBytes int64) *playerVariantSummaryDTO {
	if s.deps.Manifest == nil {
		return nil
	}
	cfg := s.deps.CfgHolder.Load()
	out := &playerVariantSummaryDTO{
		SourceBytes:  sourceBytes,
		Enabled:      upscaleFeatureEnabled(cfg),
		SoxAvailable: s.deps.UpscalePrecheck != nil && s.deps.UpscalePrecheck() == nil,
	}

	// Coverage comes from SQL rather than from hydrated rows, and that
	// is what lets an ARTIST have a summary at all: its discography can
	// run to thousands of tracks, which the page has no other reason to
	// materialise. Album detail takes the same route so the two views
	// cannot drift on what "covered" means.
	cov, err := s.deps.Manifest.VariantPresenceForPaths(r.Context(), paths)
	if err != nil {
		logger.Warn("player: variant presence for variant summary", "err", err)
		return out
	}
	for _, p := range paths {
		c, ok := cov[p]
		if !ok {
			continue
		}
		out.VariantBytes += c.Bytes
		foldPresence(&out.Upscale, c.Upscaled, c.UpscaledFresh)
		foldPresence(&out.Optimize, c.Optimized, c.OptimizedFresh)
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
	out.Upscale.Exempt = exemptCount(len(paths), counts.Upscale)
	out.Optimize.Exempt = exemptCount(len(paths), counts.Optimize)
	return out
}

// foldPresence records one track against one kind's counters.
//
// A track is STALE only when it has no fresh sidecar of the kind: a
// current copy sitting beside a superseded one is served correctly, and
// flagging it would send an operator hunting for a problem that is
// already solved.
func foldPresence(c *playerVariantCoverageDTO, present, fresh bool) {
	if !present {
		return
	}
	c.Covered++
	if !fresh {
		c.Stale++
	}
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
