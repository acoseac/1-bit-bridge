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
// lease, whether one holds the writes' turn, and whether one holds its reading
// (B728 B9 PR 3), now: a write that has returned holds none of them (B728 B9
// PR 2 round 2), and a test asks before a call that would wait for one, so
// one left held fails it at once. The backend's lock and the refreshes',
// which the lease and the reading are read under, are taken within a bound
// (lockWithin), since the backend's goroutines (a probe's, a refresh's) may
// hold them a moment: locked is false where one stayed held, a write's left
// held, and what it guards is then unread (backlog B853, round 4: it took
// the lock unconditionally, so a lock left held hung the case).
func ChoiceHeldForTest(b *Backend) (lease, turn, reading, locked bool) {
	turn = len(b.choiceTurn) != 0
	if !lockWithin(&b.dsdMu, handMadeBound) {
		return false, turn, false, false
	}
	reading = b.choiceReading
	b.dsdMu.Unlock()
	if !lockWithin(&b.mu, handMadeBound) {
		return false, turn, reading, false
	}
	lease = b.lease != nil
	b.mu.Unlock()
	return lease, turn, reading, true
}

// DACTokenForTest is the token the backend's key mints for id (dacToken):
// another unit's, a test's own, as a reading of that unit would mint it.
func (b *Backend) DACTokenForTest(id policy.Identity) DACToken { return b.dacToken(id) }
