package policy

import (
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// TestAChangeIsReconciledPerOccurrence: when the set changes, each
// occurrence is judged on its own: one whose plan is still the set's plays
// on, whatever it holds in a sink, so a change of an unused rate's cap asks
// nothing; one whose plan left the set is re-planned while no frame of it is
// the kernel's (a buffered-but-unwritten successor, its frames in a sink, is
// a re-plan the user's write needs its interrupt for; one not yet submitted
// is not) and revoked once it was heard or a frame of it is the kernel's (a
// written-but-unheard successor, a released pause, a retiring tail); and an
// identity change that leaves the same tuple available revokes what its
// node holds, the tuple's membership not the test.
func TestAChangeIsReconciledPerOccurrence(t *testing.T) {
	r := newRig(t).everything(dsdstate.ModeNative).withoutCarrier()
	before := Effective(r.inputs())
	plan := before.At(dsd64).Plan
	if plan.Outcome != OutcomeNative {
		t.Fatalf("the plan the occurrences play under: %s", plan.Outcome)
	}
	heard := Occurrence{Plan: plan, Submitted: 1 << 20, Written: 1 << 19, Played: 1 << 18, Audible: true, Identity: true}
	occurrences := []struct {
		name string
		o    Occurrence
		// left is the verdict once the plan left the set.
		left Verdict
	}{
		{"the occurrence heard", heard, Verdict{Revoke, true}},
		{"a buffered-but-unwritten successor", Occurrence{Plan: plan, Submitted: 4096, Identity: true}, Verdict{Replan, true}},
		{"a successor not yet submitted", Occurrence{Plan: plan, Identity: true}, Verdict{Replan, false}},
		{"a written-but-unheard successor", Occurrence{Plan: plan, Submitted: 8192, Written: 4096, Identity: true}, Verdict{Revoke, true}},
		{"a released pause", Occurrence{Plan: plan, Submitted: 1 << 20, Written: 1 << 18, Played: 1 << 18, Audible: true, Identity: true}, Verdict{Revoke, true}},
		{"a released pause that took back all it wrote", Occurrence{Plan: plan, Submitted: 1 << 20, Audible: true, Identity: true}, Verdict{Revoke, true}},
		{"a retiring tail", Occurrence{Plan: plan, Submitted: 1 << 20, Written: 1 << 20, Played: 1 << 20, Audible: true, Retiring: true, Identity: true}, Verdict{Revoke, true}},
		{"a retiring output whose counts were let go", Occurrence{Plan: plan, Retiring: true, Identity: true}, Verdict{Revoke, true}},
	}
	// An unused rate's cap: DSD256 capped away, DSD64 untouched.
	r.max = dsd128
	capped := Effective(r.inputs())
	// The choice changed: DSD converts.
	r.max, r.mode = 0, dsdstate.ModeConvert
	converts := Effective(r.inputs())
	for _, c := range occurrences {
		if v := Reconcile(capped, c.o); v != (Verdict{Keep, false}) {
			t.Errorf("%s, with an unused rate's cap changed: %+v, want kept, asking nothing", c.name, v)
		}
		if v := Reconcile(converts, c.o); v != c.left {
			t.Errorf("%s, its plan left the set: %+v, want %+v", c.name, v, c.left)
		}
	}
	// An identity change that leaves the same tuple available: the node its
	// output has open is not the set's identity.
	for _, c := range []struct {
		name string
		o    Occurrence
		want Verdict
	}{
		{"the occurrence heard", Occurrence{Plan: plan, Submitted: 1 << 20, Written: 1 << 19, Played: 1 << 18, Audible: true}, Verdict{Revoke, true}},
		{"a buffered successor", Occurrence{Plan: plan, Submitted: 4096}, Verdict{Revoke, true}},
		{"a successor with no output", Occurrence{Plan: plan, Identity: true}, Verdict{Keep, false}},
	} {
		if !before.At(dsd64).Plan.Same(c.o.Plan) {
			t.Fatal("the tuple is not available in the set")
		}
		if v := Reconcile(before, c.o); v != c.want {
			t.Errorf("%s, after an identity change: %+v, want %+v", c.name, v, c.want)
		}
	}
	// The same tuple with other timing left the set: a probe found the
	// kernel's PAUSE gone.
	r.mode = dsdstate.ModeNative
	r.answer(format.DSDU32BE, dsd64/32, alsapcm.ProbeAccepted, 32)
	res := r.probe[alsapcm.ProbeCandidate{Format: format.DSDU32BE, RateHz: dsd64 / 32}]
	res.Pause = false
	r.probe[res.Candidate] = res
	if v := Reconcile(Effective(r.inputs()), heard); v != (Verdict{Revoke, true}) {
		t.Errorf("the occurrence heard, its timing changed: %+v, want revoked", v)
	}
	// Evidence is no part of a plan: a rung recorded since keeps it.
	r.answer(format.DSDU32BE, dsd64/32, alsapcm.ProbeAccepted, 32)
	r.forget(dsdstate.TransportNative, dsd64)
	if v := Reconcile(Effective(r.inputs()), heard); v != (Verdict{Keep, false}) {
		t.Errorf("the occurrence heard, its rung changed: %+v, want kept", v)
	}
	// A converted occurrence is kept while the set converts its rate to the
	// same target, and re-planned once it converts to another.
	conv := converts.At(dsd64).Plan
	if v := Reconcile(converts, Occurrence{Plan: conv, Identity: true}); v != (Verdict{Keep, false}) {
		t.Errorf("a converted successor under the same target: %+v, want kept", v)
	}
	r.mode = dsdstate.ModeConvert
	to88 := s24Altset()
	to88.Rates = []int{44100, 88200}
	r.altsets = []alsa.Altset{to88}
	if v := Reconcile(Effective(r.inputs()), Occurrence{Plan: conv, Identity: true}); v != (Verdict{Replan, false}) {
		t.Errorf("a converted successor whose target changed: %+v, want re-planned", v)
	}
	// A set that refuses the rate: the state no longer reads.
	r.unreadable = true
	if v := Reconcile(Effective(r.inputs()), heard); v != (Verdict{Revoke, true}) {
		t.Errorf("the occurrence heard, the state no longer reading: %+v, want revoked", v)
	}
}
