package backup

import (
	"context"
	"errors"
	"fmt"
)

// established is what a verification's worker has established (verifyHeld),
// published to its hold before each of its steps that can block (know,
// knowFound, knowRead), so its caller answers it when it stops waiting for the
// worker (expired) and a give-up of the hold keeps it (givenUp): what its check
// found, and the errors of its own its steps answered (a read of the record
// that failed, a recheck refused or not recorded, a veto or a write that
// failed or is not known durable), each of which a reader is to hear whatever
// the stop (backlog B895).
type established struct {
	found   Verification
	legacy  bool
	errs    []error
	readErr error
}

// know publishes err, an error of the worker's own, before its next step.
func (h *hold) know(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.established.errs = append(h.established.errs, err)
}

// knowFound publishes what the check found, legacy saying the snapshot was
// written before verification, before the steps after the check.
func (h *hold) knowFound(found Verification, legacy bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.established.found, h.established.legacy = found, legacy
}

// knowRead publishes why the read of the record after the check read nothing,
// where that read answered a failure of its own (never ErrReadBackGivenUp: a
// read-back given up is the hold's give-up, which its cause says).
func (h *hold) knowRead(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.established.readErr = err
}

// knownErrs is the errors of its own the worker has published, in order.
func (h *hold) knownErrs() []error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]error(nil), h.established.errs...)
}

// expired is what a caller that stopped waiting for its worker answers
// (verifyAndThen's expiry), the hold given up by it: what the worker had
// published as the caller gave up (know), its check's finding and the
// errors of its own, with WriteUnknown, since a write may still land (backlog
// B653's eighth round); and an error saying the verification was left to end
// on its own (verifyExpiryError), joined to the first give-up a step's own
// bound had made (givenUpError) and to those errors, so a caller that takes
// its stop for no news still hears each, whichever of the two bounds the stop
// arms fired first (backlog B895).
func (h *hold) expired(ctx context.Context) (VerifyResult, error) {
	h.mu.Lock()
	k := h.established
	k.errs = append([]error(nil), k.errs...)
	other, otherStep := h.other, h.otherStep
	h.mu.Unlock()
	res := VerifyResult{Found: k.found, Legacy: k.legacy, Write: WriteUnknown, ReadErr: k.readErr}
	errs := []error{verifyExpiryError(ctx.Err())}
	if other != nil {
		errs = append(errs, givenUpError(otherStep, other))
	}
	errs = append(errs, k.errs...)
	if k.readErr != nil {
		errs = append(errs, fmt.Errorf("%w: %w", ErrRecordNotReadBack, k.readErr))
	}
	if len(errs) == 1 {
		return res, errs[0]
	}
	return res, errors.Join(errs...)
}

// givenUpError is ErrCheckGivenUp naming the step given up and its cause.
func givenUpError(step string, why error) error {
	if step == "" {
		step = "a step"
	}
	return fmt.Errorf("%w: %s did not end within its bound (%w)", ErrCheckGivenUp, step, why)
}
