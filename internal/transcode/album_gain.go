package transcode

import (
	"context"
	"fmt"
	"math"
	"time"
)

// The album-level gain for DSD renditions (ops/plan-2026-09-27-dsd-album-gain.md).
//
// Stage B's clip guard is decided PER TRACK, so the tracks of one album get
// different boosts and a rendered album loses the balance it was mastered
// with: measured on the operator's library, 57 % of DSD albums shift their
// tracks' relative levels by more than 1 dB (median 1.2, max 4.7), and a
// segued album steps in level at its track boundaries. The album-level gain
// gives every track the boost its album's hottest track allows. It reuses
// Stage B's own measurement: the render measures its track, an AlbumGainer
// supplies the album's other peaks, and Stage C applies the shared figure.

// DSDPeakRecipe versions Stage A of the DSD chain — the decode, the
// decimation and the faithful tier's low-pass — which is everything a
// recorded true peak depends on. It is NOT DSDRenditionSchemaVersion: the
// gain policy lives in Stage C, so changing the policy keeps every recorded
// peak valid. Bump this, and only this, when Stage A changes what it
// produces; peaks recorded under the old recipe then match no profile and
// are measured again.
const DSDPeakRecipe = "a1"

// DSDPeakProfile names the intermediate a DSD job's true peak is measured
// on: the Stage A recipe, the tier, the target rate and sox's rate quality.
// Two jobs with the same profile decode to the same intermediate, so a peak
// measured by one is the peak the other would measure. Empty for a job that
// is not a DSD render.
func (j JobSpec) DSDPeakProfile() string {
	if !j.SourceIsDSD || j.TargetSampleRate <= 0 {
		return ""
	}
	return DSDPeakProfileFor(j.Kind, j.TargetSampleRate, j.rateFlag())
}

// DSDPeakProfileFor is DSDPeakProfile's formula for callers that hold the
// parts rather than a spec. manifest's v47 seed spells the same string in
// SQL (it cannot import this package); both sides pin the literal
// "a1|compact|44100|-v", which is what keeps them in step.
func DSDPeakProfileFor(kind JobKind, targetRate int, rateFlag string) string {
	tier := "compact"
	if kind == JobKindPCMRender {
		tier = "faithful"
	}
	return fmt.Sprintf("%s|%s|%d|%s", DSDPeakRecipe, tier, targetRate, rateFlag)
}

// AlbumClipGuardedGainDB is the album-level boost: the lowest clip-guarded
// gain among the album's tracks, which is ClipGuardedGainDB of the album's
// highest true peak at unity. A nil peak (a digitally silent track) does not
// constrain it, and neither does a non-finite one; an album with no
// measurable peak at all gets the nominal +6, as a silent track does alone.
func AlbumClipGuardedGainDB(peaks []*float64) float64 {
	highest := math.Inf(-1)
	for _, p := range peaks {
		if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) {
			continue
		}
		highest = math.Max(highest, *p)
	}
	if math.IsInf(highest, -1) {
		return dsdNominalGainDB
	}
	return ClipGuardedGainDB(highest)
}

// AlbumGainer decides the album-level boost for a DSD render. The render
// asks it between Stage B, which measured the track's own true peak, and
// Stage C, which applies the boost. An interface rather than a func field so
// JobSpec stays comparable.
type AlbumGainer interface {
	// SurveyBudget is how much longer than its own decode a render of j will
	// run because it must first measure album-mates whose peak is not on
	// record — 0 when every one is. The pool widens the job's deadline by it,
	// so it must be cheap and have no side effects.
	SurveyBudget(ctx context.Context, j JobSpec) time.Duration
	// Claim marks j's own track as being measured by this render, so an
	// album-mate's survey waits for its peak instead of decoding the file a
	// second time. The render calls the returned func exactly once: with the
	// measured peak after Stage B (nil for a digitally silent source, err
	// nil), or with a non-nil err on any exit before that.
	Claim(ctx context.Context, j JobSpec) (resolve func(truePeakUnity *float64, err error))
	// AlbumGainDB returns the album's boost for j given j's own true peak at
	// unity. ok=false keeps the per-track guard: j is in no album of two or
	// more tracks that render to the same profile.
	AlbumGainDB(ctx context.Context, j JobSpec, ownTruePeakUnity *float64) (gainDB float64, ok bool, err error)
}

// Gain scopes recorded in a DSD rendition's settings blob: which rule
// decided its applied gain.
const (
	GainScopeTrack = "track"
	GainScopeAlbum = "album"
)

// albumBoundedGain is the gain Stage C applies when an album boost was
// decided: the album's figure, but never more than the track's own guard
// allows, so a stale or wrong album figure can never make a file clip. A
// non-finite album figure is ignored, and the figure is clamped to the
// guard's own [0, 6] range.
func albumBoundedGain(trackGain, albumGain float64) float64 {
	if math.IsNaN(albumGain) || math.IsInf(albumGain, 0) {
		return trackGain
	}
	albumGain = math.Max(0, math.Min(dsdNominalGainDB, albumGain))
	return math.Min(trackGain, albumGain)
}
