package atomicwrite

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestReadRetryWindowsErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		code error
	}{
		{"sharing_violation", windows.ERROR_SHARING_VIOLATION},
		{"access_denied", windows.ERROR_ACCESS_DENIED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocked := &os.PathError{Op: "open", Path: "player-ids.json", Err: tc.code}
			calls := 0
			err := ReadWithRetryCtx(t.Context(), func() error {
				calls++
				if calls == 1 {
					return blocked
				}
				return nil
			})
			if err != nil || calls != 2 {
				t.Fatalf("read = %v after %d calls", err, calls)
			}
			calls = 0
			err = ReadWithRetryCtx(t.Context(), func() error { calls++; return blocked })
			if !errors.Is(err, tc.code) || calls != 5 {
				t.Fatalf("persistent read = %v after %d calls", err, calls)
			}
		})
	}
}
