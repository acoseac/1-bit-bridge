package backup

import "context"

// SnapshotBusyPatience is snapshotBusyPatience, for the external tests: a
// snapshot that waited a lock out in SQLite's busy handler cannot answer
// sooner.
const SnapshotBusyPatience = snapshotBusyPatience

// WithBusyObserver returns ctx carrying observe, which a snapshot run on it
// calls each time an attempt is refused a lock.
func WithBusyObserver(ctx context.Context, observe func()) context.Context {
	return context.WithValue(ctx, busyObserverKey{}, observe)
}

// ResetPlayerAfterStep exposes the interruption seam to external tests.
func ResetPlayerAfterStep(dir string, after func(string) error) (bool, error) {
	return resetPlayer(dir, after)
}

// SnapshotAfterCapture exposes the per-call capture seam to external tests.
func SnapshotAfterCapture(ctx context.Context, src Sources, after func(string)) (string, error) {
	return snapshot(ctx, src, after)
}
