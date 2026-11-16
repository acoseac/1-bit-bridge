package atomicwrite

import (
	"context"
	"errors"
	"fmt"
)

// ErrReadBusy marks a read whose bounded Windows sharing/access retry was
// exhausted. The returned error also wraps the last read error. It says
// nothing about the file's contents; callers may suggest retrying the read.
var ErrReadBusy = errors.New("file was busy")

// ReadWithRetryCtx retries a read blocked by Windows sharing or access errors
// while an atomic replacement or a scan-on-close handle is in flight. It uses
// RenameWithRetry's five attempts and 750 ms total backoff, cancellable between
// attempts. Other errors (including absence and malformed data) return at once;
// on other platforms every read runs once. Exhausted retries wrap ErrReadBusy
// and the last read error; cancellation returns the context error alone.
// The callback must close any handles on failure and must not mutate files.
func ReadWithRetryCtx(ctx context.Context, read func() error) error {
	err := retryIO(ctx, read, readRetryable)
	if err != nil && readRetryable(err) {
		return fmt.Errorf("%w: %w", ErrReadBusy, err)
	}
	return err
}

// readRetryable is what ReadWithRetryCtx asks again on: the platform's
// retryableReadError (Windows' ERROR_SHARING_VIOLATION and
// ERROR_ACCESS_DENIED, nothing elsewhere). SetReadRetryableForTest adds to it.
var readRetryable = retryableReadError

// SetReadRetryableForTest makes ReadWithRetryCtx ask again on the errors fn
// answers true for as well as on those it asked again on before (the
// platform's, and an earlier call's), and returns what puts that back: for a
// test, on every platform, of a reader that must ask again where Windows
// refuses a read for a moment, whose real reads still wait out Windows' own
// refusals (backlog B898). Process-global, like SetSyncDirForTest: a test that
// sets it does not call t.Parallel, and puts it back once every reader it
// started has returned.
func SetReadRetryableForTest(fn func(error) bool) (restore func()) {
	prev := readRetryable
	readRetryable = func(err error) bool { return fn(err) || prev(err) }
	return func() { readRetryable = prev }
}
