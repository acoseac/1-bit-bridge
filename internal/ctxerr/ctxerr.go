// Package ctxerr tells a pass that shutdown STOPPED from one that FAILED.
//
// A background loop runs on a context that shutdown cancels (runServe's
// scanCtx, or the serve ctx itself). Every call the loop makes on that
// context then fails with the cancellation, and a log site that reports
// whatever error arrives puts a failure in the journal for a pass that
// failed at nothing. The rule, which CLAUDE.md records under "The CLI and
// the serve wiring", is that a cancelled pass is not a failed one.
//
// passCancelled in cmd/bridge (#997) is the context-only form, for a call
// whose error on a cancel IS the cancel. WithoutCancellation is the general
// form, and the one every other site uses: it asks the context AND the error.
package ctxerr

import (
	"context"
	"errors"
)

// WithoutCancellation returns err with ctx's cancellation taken out of it,
// or nil when the cancellation is all there was. A pass reports only what
// is left, which is how it tells a pass that shutdown STOPPED from one that
// FAILED.
//
// Both of its conditions are load-bearing. ctx must be CANCELLED: a pass
// whose context ran out of time failed, so a deadline is reported. And the
// error must BE that cancellation, not merely arrive after one: a snapshot's
// file copies do not watch ctx, so one that fails as shutdown begins is a
// failure and is reported. Only ctx's own cancellation is quiet, which also
// keeps a context.Canceled from somewhere else reported while ctx is live.
//
// Pass the context the PASS runs on, the loop's own, never a per-request
// child of it. A child that its own cancel func stopped is cancelled while
// the loop is live, and would silence a request that was aborted for a
// reason of its own. A shutdown reaches every child anyway, as a
// cancellation, so asking the loop's context loses nothing.
//
// An error can carry both at once. A loop that keeps going past a directory
// it cannot remove or read, and is then stopped by a cancel, joins what it
// collected with ctx.Err() (PruneContext, reapOrphans, the UPnP orphan
// sweep). Asking errors.Is of the whole error would silence those genuine
// failures along with the cancel, so a multi-error (errors.Join) is filtered
// child by child, and a lone survivor comes back as itself rather than as a
// join of one.
//
// A wrapper is judged by what it wraps. `vacuum manifest db: %w` around the
// cancellation IS the cancellation. A wrapper around a join that holds a
// genuine failure as well is reported WHOLE, cancellation text included,
// because it cannot be rebuilt around what is left without dropping its own
// context. (Gemini, #998, proposed returning the filtered inner error, which
// drops the wrapper's context and compares errors with ==, a runtime panic on
// an error type that is not comparable.) An error with no cancellation in it
// comes back unchanged, the same value, message and all.
//
// One residual, measured rather than handled. database/sql rolls a cancelled
// transaction back from a goroutine of its own, and when that goroutine wins
// the race, the caller's next call on the transaction answers
// `sql: statement is closed` (a bare errors.New) or sql.ErrTxDone, and
// neither carries the cancellation. Cancelling a 500-row UpsertTrackBatch or
// IncrementMissingTracksAndDeleteAtThreshold at a random point, 3,000 times,
// returned context.Canceled 1,574 times, `sql: statement is closed` once and
// sql.ErrTxDone never; the other 1,425 had finished. That one line is
// reported. Recognising it would mean matching error text, the trap the
// enricher's structural status parser exists to avoid: a 4xx whose body
// mentions "HTTP 503".
func WithoutCancellation(ctx context.Context, err error) error {
	if !errors.Is(err, context.Canceled) || !errors.Is(ctx.Err(), context.Canceled) {
		return err
	}
	// The cases in errors.Is's own order, so this walks the chain that
	// errors.Is matched above.
	switch e := err.(type) {
	case interface{ Unwrap() error }:
		if WithoutCancellation(ctx, e.Unwrap()) == nil {
			return nil
		}
		return err
	case interface{ Unwrap() []error }:
		var kept []error
		for _, child := range e.Unwrap() {
			if child = WithoutCancellation(ctx, child); child != nil {
				kept = append(kept, child)
			}
		}
		if len(kept) == 1 {
			return kept[0] // the survivor itself, not a join of one (Gemini, #998)
		}
		return errors.Join(kept...) // nil when nothing survived
	default:
		return nil // errors.Is matched a leaf: the cancellation itself
	}
}
