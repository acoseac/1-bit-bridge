package atomicwrite

import "context"

// ReadWithRetryCtx retries a read blocked by Windows sharing or access errors
// while an atomic replacement or a scan-on-close handle is in flight. It uses
// RenameWithRetry's five attempts and 750 ms total backoff, cancellable between
// attempts. Other errors (including absence and malformed data) return at once;
// on other platforms every read runs once. A persistent error is never hidden.
// The callback must close any handles on failure and must not mutate files.
func ReadWithRetryCtx(ctx context.Context, read func() error) error {
	return retryIO(ctx, read, retryableReadError)
}
