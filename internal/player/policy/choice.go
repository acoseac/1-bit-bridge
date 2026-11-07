package policy

import (
	"errors"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// ReasonChoiceUncertain is the refusal of a rate while a choice write that
// reached the state's file is not known to be durable (B728 B9): the state in
// memory and the one the file may hold at the next start play it differently.
const ReasonChoiceUncertain engine.Reason = "dsd_choice_uncertain"

// ChoiceReason is a word a choice write is refused with for what the state
// holds (the design's section 7.2, checks 4 to 7).
type ChoiceReason string

const (
	ChoiceStale                  ChoiceReason = "stale"
	ChoiceModeNotOffered         ChoiceReason = "mode_not_offered"
	ChoiceConfirmationRequired   ChoiceReason = "dop_confirmation_required"
	ChoiceFirstUseUnacknowledged ChoiceReason = "first_use_unacknowledged"
)

// ChoiceRefusal is a choice write refused, its word and its sentence.
type ChoiceRefusal struct {
	Reason ChoiceReason
	Detail string
}

func (r *ChoiceRefusal) Error() string { return string(r.Reason) + ": " + r.Detail }

// Choice is a write of a DAC's DSD choice (B728 B9).
type Choice struct {
	BaseRevision         uint64
	Mode                 string
	MaxBitRate           int
	ConfirmDoP           bool
	FirstUseAcknowledged bool
	By                   string
}

// StoredRevision is the revision a choice write compares its base with.
func StoredRevision(st dsdstate.State, key dacprofile.Key) uint64 { return 0 }

// Choose is what a choice write does to in.State.
func Choose(in Inputs, c Choice, now time.Time) (dsdstate.State, error) {
	return in.State, errors.New("policy: a choice write is not made yet")
}

// Intersect is cur where landed plays what it plays.
func Intersect(cur, landed Set) Set { return cur }
