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
	err := retryIO(ctx, read, retryableReadError)
	if err != nil && retryableReadError(err) {
		return fmt.Errorf("%w: %w", ErrReadBusy, err)
	}
	return err
}
