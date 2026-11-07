package playback

import (
	"context"

	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// ChoiceOutcome is how a write of the bound DAC's DSD choice ended (B728 B9;
// the design's section 7.2): written, or the word it was refused with.
type ChoiceOutcome string

const (
	ChoiceWritten                ChoiceOutcome = "written"
	ChoiceBadRequest             ChoiceOutcome = "bad_request"
	ChoiceDACUnavailable         ChoiceOutcome = "dac_unavailable"
	ChoiceDACChanged             ChoiceOutcome = "dac_changed"
	ChoiceStale                  ChoiceOutcome = "stale"
	ChoiceModeNotOffered         ChoiceOutcome = "mode_not_offered"
	ChoiceConfirmationRequired   ChoiceOutcome = "dop_confirmation_required"
	ChoiceFirstUseUnacknowledged ChoiceOutcome = "first_use_unacknowledged"
	ChoiceWouldInterrupt         ChoiceOutcome = "would_interrupt"
	ChoicePersistFailed          ChoiceOutcome = "persist_failed"
	ChoicePersistUncertain       ChoiceOutcome = "persist_uncertain"
	ChoiceStateUnreadable        ChoiceOutcome = "state_unreadable"
)

// ChoiceRequest is a write of the bound DAC's DSD choice: the DAC's key as
// GET /v1/outputs publishes it, the choice, and whether the write may
// interrupt what plays.
type ChoiceRequest struct {
	DACKey    string
	Choice    policy.Choice
	Interrupt bool
}

// ChoiceResult is a write's outcome and the epoch published once it ended.
type ChoiceResult struct {
	Outcome ChoiceOutcome
	Policy  PlayerPolicy
}

// ChooseDSD writes the bound DAC's DSD choice.
func (b *Backend) ChooseDSD(ctx context.Context, req ChoiceRequest) ChoiceResult {
	return ChoiceResult{Outcome: ChoicePersistFailed, Policy: b.Policy()}
}

// ProbeDSDPending reports whether a probe is owed that only the supervisor's
// next ask makes.
func (b *Backend) ProbeDSDPending() bool { return false }

// pendingChoice is a choice write between its reading and its settle.
type pendingChoice struct {
	set  *dsdSet
	done chan struct{}
}

// lockSettled takes s.mu for the step that ends o's open.
func (s *session) lockSettled(ctx context.Context, o *occ) { s.mu.Lock() }

// writeInterrupts reports whether publishing next would interrupt what plays.
func (b *Backend) writeInterrupts(cur, next *dsdSet) bool { return false }
