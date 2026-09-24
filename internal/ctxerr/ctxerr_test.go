package ctxerr

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestWithoutCancellationKeepsOnlyWhatFailed pins the classification behind
// every quiet shutdown, one row per claim in WithoutCancellation's docblock.
// The joined rows use the exact shape PruneContext's delete loop and
// reapOrphans return when a cancel stops them after a genuine failure,
// errors.Join(errors.Join(errs...), ctx.Err()).
//
// Each row names how its pass's context ENDED rather than holding the
// context: nil for a pass that is still live.
func TestWithoutCancellationKeepsOnlyWhatFailed(t *testing.T) {
	removeA := errors.New("remove backups/a: permission denied")
	removeB := errors.New("remove backups/b: permission denied")
	copyFailed := errors.New("copy tokens.json: input/output error")

	for _, tc := range []struct {
		name       string
		ended      error // how the pass's ctx ended: nil, Canceled or DeadlineExceeded
		err        error
		want       string // "" = nil
		wantCancel bool   // what is reported still carries a cancellation
	}{
		{"nil error", context.Canceled, nil, "", false},
		{"the snapshot's cancelled vacuum", context.Canceled,
			fmt.Errorf("vacuum manifest db: %w", context.Canceled), "", false},
		{"a cancellation wrapped twice", context.Canceled,
			fmt.Errorf("snapshot: %w", fmt.Errorf("vacuum manifest db: %w", context.Canceled)), "", false},
		{"a prune stopped before any failure", context.Canceled,
			errors.Join(errors.Join(), context.Canceled), "", false},
		{"a prune stopped after two failures", context.Canceled,
			errors.Join(errors.Join(removeA, removeB), context.Canceled),
			removeA.Error() + "\n" + removeB.Error(), false},
		// A join under a wrapper cannot be rebuilt around what is left
		// without dropping the wrapper's own context, so it is reported
		// whole when it holds a genuine failure, and is quiet only when the
		// cancellation is all it holds.
		{"a wrapped join holding a failure", context.Canceled,
			fmt.Errorf("prune: %w", errors.Join(removeA, context.Canceled)),
			"prune: " + removeA.Error() + "\n" + context.Canceled.Error(), true},
		{"a wrapped join holding only the cancellation", context.Canceled,
			fmt.Errorf("prune: %w", errors.Join(errors.Join(), context.Canceled)), "", false},
		{"a failure with no cancellation in it, in a cancelled pass", context.Canceled,
			copyFailed, copyFailed.Error(), false},
		{"a deadline", context.DeadlineExceeded,
			fmt.Errorf("vacuum manifest db: %w", context.DeadlineExceeded),
			"vacuum manifest db: " + context.DeadlineExceeded.Error(), false},
		// The row above is rejected by the error alone, since a deadline is
		// not context.Canceled. This one reaches the context: a pass whose
		// ctx ran out of time failed, even when the error it carries is a
		// cancellation from somewhere else. Only a CANCELLED ctx is quiet,
		// not one that is merely done.
		{"a deadline, with another context's cancellation in the error", context.DeadlineExceeded,
			fmt.Errorf("vacuum manifest db: %w", context.Canceled),
			"vacuum manifest db: " + context.Canceled.Error(), true},
		{"another context's cancellation while ctx is live", nil,
			fmt.Errorf("vacuum manifest db: %w", context.Canceled),
			"vacuum manifest db: " + context.Canceled.Error(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := WithoutCancellation(contextThatEnded(t, tc.ended), tc.err)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("WithoutCancellation = %q, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("WithoutCancellation = nil, want %q", tc.want)
			}
			if got.Error() != tc.want {
				t.Errorf("WithoutCancellation = %q, want %q", got, tc.want)
			}
			if errors.Is(got, context.Canceled) != tc.wantCancel {
				t.Errorf("errors.Is(result, context.Canceled) = %v, want %v", !tc.wantCancel, tc.wantCancel)
			}
		})
	}
	cancelled := contextThatEnded(t, context.Canceled)
	// The kept half of a joined error is the SAME errors, not copies of
	// their text.
	got := WithoutCancellation(cancelled, errors.Join(errors.Join(removeA, removeB), context.Canceled))
	if !errors.Is(got, removeA) || !errors.Is(got, removeB) {
		t.Errorf("the genuine failures lost their identity: %v", got)
	}
	// And an error with no cancellation in it comes back unchanged.
	if got := WithoutCancellation(cancelled, copyFailed); got != copyFailed {
		t.Errorf("WithoutCancellation rebuilt an error it had nothing to take out of: %v", got)
	}
	// A lone survivor comes back as itself, not as a join of one, so its
	// own type still answers a type switch. For PruneContext's shape that
	// is the very join of failures it collected.
	errs := errors.Join(removeA, removeB)
	if got := WithoutCancellation(cancelled, errors.Join(errs, context.Canceled)); got != errs {
		t.Errorf("the one surviving child came back re-wrapped: %#v", got)
	}
	if got := WithoutCancellation(cancelled, errors.Join(copyFailed, context.Canceled)); got != copyFailed {
		t.Errorf("the one surviving error came back re-wrapped: %#v", got)
	}
}

// contextThatEnded returns a context that is live when ended is nil,
// cancelled when it is context.Canceled, and past its deadline when it is
// context.DeadlineExceeded.
func contextThatEnded(t *testing.T, ended error) context.Context {
	t.Helper()
	switch {
	case ended == nil:
		return context.Background()
	case errors.Is(ended, context.Canceled):
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	case errors.Is(ended, context.DeadlineExceeded):
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		t.Cleanup(cancel)
		return ctx
	}
	t.Fatalf("contextThatEnded: no context ends with %v", ended)
	return nil
}
