package playback

import (
	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// ProbedUnitForTest is the silent probe's check of the node it opened
// (probedUnit), for the unit m, read with key's model, as ProbeDSD captured it.
func ProbedUnitForTest(box DSDBox, m alsa.UnitModel, key dacprofile.Key) func(alsapcm.NodeInfo) error {
	return probedUnit(box, m, key)
}

// AdmitDSDForTest is the effective policy's admission of DSD content of f
// (admitDSD), for a session's first occurrence when leading: the plan, the
// generation of the set it was admitted under (0 for none), or the refusal.
// The external tests of the policy's selection call it; nothing opens.
func (b *Backend) AdmitDSDForTest(f format.NativeDSD, leading bool) (engine.NativePlan, uint64, error) {
	o := &occ{leading: leading}
	plan, err := b.admitDSD(o, f)
	if o.admitted == nil {
		return plan, 0, err
	}
	return plan, o.admitted.gen, err
}

// AdmitDSDPlanForTest is AdmitDSDForTest with the effective set's plan the
// occurrence was admitted under, and whether it converts (B728 B11): a
// converted rate's admission has no native plan.
func (b *Backend) AdmitDSDPlanForTest(f format.NativeDSD, leading bool) (engine.NativePlan, policy.Plan, bool, error) {
	o := &occ{leading: leading}
	plan, err := b.admitDSD(o, f)
	return plan, o.policyPlan, o.converts(), err
}
