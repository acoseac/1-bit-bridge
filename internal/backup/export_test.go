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
