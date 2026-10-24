package backup

import (
	"context"
	"io"
)

// AdoptPlayerStateWith is AdoptPlayerState drawing the adoption's random
// part from random, as restore's adoption does: a source box adopting at a
// chosen draw (TestLegacyCeilingCoversASourceThatAdoptedAfterItsSnapshot).
func AdoptPlayerStateWith(dir string, random io.Reader) error {
	return adoptPlayerState(dir, random)
}

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
	return resetPlayer(dir, false, after)
}

// DiscardPlayerFloorAfterStep exposes durable interruption points.
func DiscardPlayerFloorAfterStep(dir string, after func(string) error) (bool, error) {
	return resetPlayer(dir, true, after)
}
