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

// WithBeforeConfigForTest returns opts with fn as what a restore runs just
// before it writes the snapshot's bridge.yaml, in its turn of the file's
// lock.
func WithBeforeConfigForTest(opts RestoreOptions, fn func()) RestoreOptions {
	opts.beforeConfig = fn
	return opts
}

// WithBeforeWritesForTest returns opts with fn as what a restore runs once it
// holds its locks and has read the snapshot, just before its first write.
func WithBeforeWritesForTest(opts RestoreOptions, fn func()) RestoreOptions {
	opts.beforeWrites = fn
	return opts
}
