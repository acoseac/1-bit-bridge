package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestAStopEndsAStartUpStepOnlyByItsOwnArtifacts pins stoppedBeforeServing's
// two halves (backlog B662): serve's stop context cancelled, never past a
// deadline or live, and an error made of that stop's artifacts alone,
// context.Canceled and sql.ErrTxDone, however they are wrapped or joined. An
// error that holds anything beside them is a failed start.
func TestAStopEndsAStartUpStepOnlyByItsOwnArtifacts(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	withCause, cancelCause := context.WithCancelCause(context.Background())
	cancelCause(errors.New("a restart was requested"))
	expired, done := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer done()
	live := context.Background()
	disk := errors.New("disk I/O error")

	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"the cancellation", cancelled, context.Canceled, true},
		{"the cancellation wrapped", cancelled, fmt.Errorf("begin: %w", context.Canceled), true},
		{"a transaction its rollback ended", cancelled, sql.ErrTxDone, true},
		{"that wrapped", cancelled, fmt.Errorf("commit: %w", sql.ErrTxDone), true},
		{"the two joined", cancelled, errors.Join(context.Canceled, sql.ErrTxDone), true},
		{"the two in one wrapper", cancelled, fmt.Errorf("%w; %w", context.Canceled, sql.ErrTxDone), true},
		{"a cancel with a cause", withCause, sql.ErrTxDone, true},

		{"another error after the stop", cancelled, disk, false},
		{"another error joined to the ended transaction", cancelled, errors.Join(sql.ErrTxDone, disk), false},
		{"another error joined to the cancellation", cancelled, errors.Join(context.Canceled, disk), false},
		{"that join wrapped", cancelled, fmt.Errorf("seed: %w", errors.Join(sql.ErrTxDone, disk)), false},
		{"another error beside the cancellation in one wrapper", cancelled, fmt.Errorf("%w; %w", context.Canceled, disk), false},
		{"a deadline error after the stop", cancelled, context.DeadlineExceeded, false},
		{"another sentinel of database/sql", cancelled, sql.ErrConnDone, false},
		{"a wrapper of nothing", cancelled, wrapsNothing{}, false},
		{"no error", cancelled, nil, false},

		{"an ended transaction with the stop context live", live, sql.ErrTxDone, false},
		{"a cancellation from elsewhere with it live", live, context.Canceled, false},
		{"an ended transaction past a deadline", expired, sql.ErrTxDone, false},
		{"the deadline's own error", expired, context.DeadlineExceeded, false},
		{"a cancellation past a deadline", expired, context.Canceled, false},
	} {
		if got := stoppedBeforeServing(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: stoppedBeforeServing(%v, %v) = %v, want %v", tc.name, tc.ctx.Err(), tc.err, got, tc.want)
		}
	}
}

// wrapsNothing is an error whose Unwrap answers nil.
type wrapsNothing struct{}

func (wrapsNothing) Error() string { return "wraps nothing" }
func (wrapsNothing) Unwrap() error { return nil }
