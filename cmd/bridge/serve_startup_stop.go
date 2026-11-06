package main

import (
	"context"
	"database/sql"
	"errors"

	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
)

// stoppedBeforeServing reports whether err, the error of a start-up step that
// runs a transaction on serve's own stop context (stopCtx), is what a
// requested stop leaves there: the step then exits as a shutdown does, not as
// a failed start. Both halves are required (backlog B662).
//
// stopCtx was cancelled: errors.Is(stopCtx.Err(), context.Canceled), never
// any ctx.Err(), so serve's context ending at a deadline fails, as
// ctxerr.WithoutCancellation rules for every loop.
//
// And the error holds that cancel's artifacts alone (stopArtifactsOnly):
// the cancellation itself, as ctxerr.WithoutCancellation reads it, or
// sql.ErrTxDone. database/sql rolls a transaction back from a goroutine of
// its own once its context is done (Tx.awaitDone), so a cancel that lands
// after the step's last statement has run at the driver, and before the step
// reaches tx.Commit(), leaves Commit to find the transaction ended and answer
// sql.ErrTxDone from its own state, with no cancellation in it; so does a
// statement whose check of its context passed just before the cancel and
// whose check of the transaction came just after that rollback (Tx.grabConn).
// An error that holds anything else, a disk's refusal arriving after the stop
// among them, is a failed start whatever the context says.
//
// sql.ErrTxDone is a stop's only beside a cancelled stop context: on a live
// one it is a transaction ended for another reason, one used twice, and
// fails. ctxerr.WithoutCancellation still reports it for the loops (its
// doc's measured residual): an exit code is serve's to decide, so the
// decision is here and not there.
func stoppedBeforeServing(stopCtx context.Context, err error) bool {
	return err != nil && errors.Is(stopCtx.Err(), context.Canceled) && stopArtifactsOnly(stopCtx, err)
}

// stopArtifactsOnly reports whether err holds nothing but stopCtx's
// cancellation and sql.ErrTxDone, walked in errors.Is's order. A wrapper of
// one error is judged by what it wraps, and a join or a wrapper of several by
// each of them, so one that holds another error beside them is not, and
// neither is a wrapper of nothing. stopCtx is cancelled:
// ctxerr.WithoutCancellation answers nil exactly for an error that is its
// cancellation alone.
func stopArtifactsOnly(stopCtx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctxerr.WithoutCancellation(stopCtx, err) == nil {
		return true
	}
	if err == sql.ErrTxDone { //nolint:errorlint // the leaf itself; the cases below unwrap
		return true
	}
	switch e := err.(type) {
	case interface{ Unwrap() error }:
		return stopArtifactsOnly(stopCtx, e.Unwrap())
	case interface{ Unwrap() []error }:
		errs := e.Unwrap()
		for _, child := range errs {
			if !stopArtifactsOnly(stopCtx, child) {
				return false
			}
		}
		return len(errs) > 0
	}
	return false
}
