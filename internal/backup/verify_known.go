package backup

import (
	"context"
	"errors"
	"fmt"
)

// established is what a verification's worker has established, published to
// its hold as each of its steps answers, on the step's own goroutine (knowIn,
// knowReadIn; the check's answer is the hold's check slot, takeFinding), so
// its caller answers it when it stops waiting for the worker (expired) and a
// give-up of the hold keeps it (givenUp): what its check found, and the errors
// of its own its steps answered (a read of the record that failed, a recheck
// refused or not recorded, a veto or a write that failed or is not known
// durable, a read-back that failed), each of which a reader is to hear
// whatever the stop where it is news (backlog B895).
type established struct {
	// found and legacy are what the worker takes the check to have found
	// where the check's own answer was not taken (findingOf): its INCOMPLETE.
	found  Verification
	legacy bool
	// errs is every error of its own a step answered, as it answered (knowIn)
	// or as its worker took it (keep).
	errs    []knownErr
	readErr error
}

// knownErr is an error a step of a verification answered, and whether it is
// news beside a give-up of its hold (newsOf): the errors a give-up made are
// said in the worker's own answer, never in its caller's expiry, and never
// make a give-up its caller's stop made alone anything but that stop.
type knownErr struct {
	err  error
	news bool
}

// checkAnswer is a verification's check's answer as published (takeFinding):
// what it found, whether the snapshot was written before verification, and
// whether the record cannot take it (no directory can be synced), in which
// case ErrRecheckNotRecorded is published with it.
type checkAnswer struct {
	found       Verification
	legacy      bool
	notRecorded bool
}

// checkClosed fills the hold's check slot once the check's answer can no
// longer be taken: a give-up of the hold, or its caller's of the check, came
// first.
var checkClosed = &checkAnswer{}

// knowIn publishes err, what the step whose give-up is given answered, where
// that step was not given up before it: a completion its worker takes as the
// step's answer, which a give-up after it keeps, and never the late answer of
// a step given up (runStep drops those), whatever step runs then (backlog
// B895's third round). It answers whether it did. runStep calls it on the
// step's own goroutine as the step answers, so an expiry or a give-up that
// takes the hold's lock between the operation's return and this lock answers
// without it: the residual B895's History entry states.
func (h *hold) knowIn(given context.Context, err error) bool {
	if err == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if given.Err() != nil {
		return false
	}
	h.established.errs = append(h.established.errs, knownErr{err: err, news: newsOf(err) != nil})
	return true
}

// sameError is the outcome of a step that answers an error of its own and
// nothing else (stepKnowing).
func sameError(err error) error { return err }

// keep publishes err, what a step answered that knowIn could not publish, its
// signal ended by a give-up of the hold as it answered, and which its worker
// took all the same (runStep): for the worker's own answer, after any expiry.
func (h *hold) keep(err error) {
	if err == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.established.errs = append(h.established.errs, knownErr{err: err, news: newsOf(err) != nil})
}

// knowReadIn publishes why the read of the record after the check read
// nothing, as knowIn publishes an error: its read's own failure, never its
// give-up (ErrReadBackGivenUp, which the hold's give-up says).
func (h *hold) knowReadIn(given context.Context, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if given.Err() != nil || newsOf(err) == nil {
		return
	}
	h.established.readErr = err
}

// knownRead is the read-back's own failure as published (knowReadIn), nil
// for none.
func (h *hold) knownRead() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.established.readErr
}

// takeFinding publishes the check's answer as the check makes it, on its own
// thread: found, with ErrRecheckNotRecorded beside it where the record cannot
// take it (notRecorded), in one compare-and-swap of the hold's check slot,
// which every give-up of the hold (giveUpAs) and its caller's give-up of the
// check (dropCheck) make too: so a check that answered before either is
// published whole, and one that answered after is not published at all, its
// verdict the INCOMPLETE its worker then publishes (findingOf; backlog B895's
// third round). It takes no lock: the check's thread runs at the lowest
// priority, and a lock it held could keep a caller's expiry waiting on it.
func (h *hold) takeFinding(found Verification, legacy, notRecorded bool) bool {
	return h.check.CompareAndSwap(nil, &checkAnswer{found: found, legacy: legacy, notRecorded: notRecorded})
}

// dropCheck closes the hold's check slot as its caller gives the check up
// (runCheck), and answers what the check found where its answer was taken
// first.
func (h *hold) dropCheck() (Verification, bool) {
	if h.check.CompareAndSwap(nil, checkClosed) {
		return Verification{}, false
	}
	if a := h.answered(); a != nil {
		return a.found, true
	}
	return Verification{}, false
}

// findingOf is what the check found as its worker takes it once runCheck has
// answered: the check's answer where it was taken (takeFinding), else
// incomplete, which it publishes in its place.
func (h *hold) findingOf(incomplete Verification, legacy bool) Verification {
	h.check.CompareAndSwap(nil, checkClosed)
	if a := h.answered(); a != nil {
		return a.found
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.established.found, h.established.legacy = incomplete, legacy
	return incomplete
}

// answered is the check's answer as taken (takeFinding), nil while none is.
func (h *hold) answered() *checkAnswer {
	if a := h.check.Load(); a != nil && a != checkClosed {
		return a
	}
	return nil
}

// knownErrs is the errors of its own the worker has published, in order,
// ErrRecheckNotRecorded first where its check's answer was taken with it, and
// whether any of them is news (newsOf); with newsOnly, those alone.
func (h *hold) knownErrs(newsOnly bool) (errs []error, news bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.knownErrsLocked(newsOnly)
}

// knownErrsLocked is knownErrs, its caller holding h.mu.
func (h *hold) knownErrsLocked(newsOnly bool) (errs []error, news bool) {
	if a := h.answered(); a != nil && a.notRecorded {
		errs, news = append(errs, ErrRecheckNotRecorded), true
	}
	for _, e := range h.established.errs {
		if e.news || !newsOnly {
			errs = append(errs, e.err)
		}
		news = news || e.news
	}
	return errs, news
}

// expired is what a caller that stopped waiting for its worker answers
// (verifyAndThen's expiry), the hold given up by it: what the worker had
// published as the caller gave up, its check's finding and the errors of its
// own that are news (newsOf), with WriteUnknown, since a write may still land
// (backlog B653's eighth round); and an error saying the verification was
// left to end on its own (verifyExpiryError), joined to the first give-up a
// step's own bound had made (givenUpError) and to those errors, so a caller
// that takes its stop for no news still hears each, whichever of the two
// bounds the stop arms fired first (backlog B895).
func (h *hold) expired(ctx context.Context) (VerifyResult, error) {
	h.mu.Lock()
	known, _ := h.knownErrsLocked(true)
	k := h.established
	other, otherStep := h.other, h.otherStep
	a := h.answered()
	h.mu.Unlock()
	if a != nil {
		k.found, k.legacy = a.found, a.legacy
	}
	res := VerifyResult{Found: k.found, Legacy: k.legacy, Write: WriteUnknown, ReadErr: k.readErr}
	errs := []error{verifyExpiryError(ctx.Err())}
	if other != nil {
		errs = append(errs, givenUpError(otherStep, other))
	}
	errs = append(errs, known...)
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

// newsOf is err where it is news beside a give-up of its hold, and nil where
// the give-up itself made it: an operation a give-up stopped (a veto attempt
// or a removal refused, ErrCheckGivenUp; a rename refused, its step's context's
// error), which says no more than the give-up does, or a cache stamp a give-up
// skipped and nothing else (cacheStampWarningOnly). A failure the operation
// met on its own is news, whatever a give-up skipped beside it (backlog B895's
// third round).
func newsOf(err error) error {
	if err == nil || errors.Is(err, ErrCheckGivenUp) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) || cacheStampWarningOnly(err) {
		return nil
	}
	return err
}
