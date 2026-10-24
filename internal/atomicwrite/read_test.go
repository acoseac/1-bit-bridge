package atomicwrite

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestReadRetryBoundsAndCancellation(t *testing.T) {
	blocked := errors.New("transient read")
	for _, tc := range []struct {
		name                           string
		succeedAt, cancelAt, wantCalls int
		want                           error
	}{
		{"recovers", 3, 0, 3, nil},
		{"exhausted", 0, 0, 5, blocked},
		{"cancelled", 0, 1, 1, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			err := retryIO(ctx, func() error {
				calls++
				if calls == tc.cancelAt {
					cancel()
				}
				if calls == tc.succeedAt {
					return nil
				}
				return blocked
			}, func(err error) bool { return errors.Is(err, blocked) })
			if calls != tc.wantCalls || !errors.Is(err, tc.want) {
				t.Fatalf("read = %v after %d calls, want %v after %d", err, calls, tc.want, tc.wantCalls)
			}
		})
	}
}

func TestReadRetryKeepsPermanentErrors(t *testing.T) {
	for _, want := range []error{os.ErrNotExist, errors.New("invalid reservation"), nil} {
		calls := 0
		err := ReadWithRetryCtx(t.Context(), func() error { calls++; return want })
		if calls != 1 || !errors.Is(err, want) {
			t.Fatalf("read = %v after %d calls, want %v once", err, calls, want)
		}
	}
}
