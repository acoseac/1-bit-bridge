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

// SetSeamsForTest gives b the seams s, before anything asks them: a test of
// the backend's external package holds a goroutine at one of them.
func SetSeamsForTest(b *Backend, s *Seams) { b.cfg.Seams = s }

// ChoiceHeldForTest says whether a choice's write holds the configuration
// lease, and whether one holds the writes' turn, now: a write that has
// returned holds neither (B728 B9 PR 2 round 2), and a test asks before a
// call that would wait for either, so a leak fails it at once. The backend's
// lock, which the lease is read under, is taken within a bound
// (lockWithin), since the backend's goroutines (a probe's) may hold it a
// moment: locked is false where it stayed held, a leak of the write's, and
// lease is then unread (backlog B853, round 4: it took the lock
// unconditionally, so that leak hung the case).
func ChoiceHeldForTest(b *Backend) (lease, turn, locked bool) {
	turn = len(b.choiceTurn) != 0
	if !lockWithin(&b.mu, handMadeBound) {
		return false, turn, false
	}
	lease = b.lease != nil
	b.mu.Unlock()
	return lease, turn, true
}
