package integrity

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The watcher judges rows as it LISTED them, and its delete removes a row
// only while it is still that row (backlog B204). A row another writer
// changed in between, `bridge variants move` above all, comes back from the
// delete as manifest.ErrVariantChanged; the store half is pinned in
// internal/manifest, and the real move against the real store in
// cmd/bridge (TestAVariantSweepDuringAMoveKeepsTheRowsTheMoveRelocated).
// These pin what the tick makes of that answer.

// changingRows is a fakeDeleter whose delete answers as the store adapter
// does for a row that changed since the tick listed it, for the source
// paths it names: an error wrapping manifest.ErrVariantChanged, nothing
// deleted.
type changingRows struct {
	fakeDeleter
	changed map[string]bool
}

// DeleteVariantIfUnchanged refuses a row it names as changed, and deletes
// any other.
func (c *changingRows) DeleteVariantIfUnchanged(r VariantSnapshot) error {
	if c.changed[r.SourcePath] {
		return fmt.Errorf("manifest: delete variant %s: %w", r.SourcePath, manifest.ErrVariantChanged)
	}
	return c.fakeDeleter.DeleteVariantIfUnchanged(r)
}

// rowsWithFiveGone is forty rows with their sidecars at their canonical
// place under dir, and rows 5, 15, 25, 30 and 35 with their sidecars
// removed: five rows gone at both locations, under the mass-delete floor.
func rowsWithFiveGone(t *testing.T, dir string) []VariantSnapshot {
	t.Helper()
	var rows []VariantSnapshot
	for i := 0; i < 40; i++ {
		rows = append(rows, presentRow(t, dir, i))
	}
	for _, i := range []int{5, 15, 25, 30, 35} {
		if err := os.Remove(rows[i].SidecarPath); err != nil {
			t.Fatal(err)
		}
	}
	return rows
}

// TestVariantWatcherKeepsARowThatChangedSinceItsListing — five rows read as
// gone, and three of them changed since the listing: those three are kept
// and counted as changed, with no event and nothing at Warn but the
// summary of a tick that deleted the other two.
func TestVariantWatcherKeepsARowThatChangedSinceItsListing(t *testing.T) {
	dir := mountedVariantsDir(t)
	rows := rowsWithFiveGone(t, dir)
	store := &changingRows{changed: map[string]bool{
		mountSource(5): true, mountSource(25): true, mountSource(35): true,
	}}
	pub := &fakePublisher{}
	w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, store, pub.publish, staticDir(dir), time.Hour, 20)
	rec := loggingtest.Record(t)

	r := w.tick(context.Background())
	if r.Deleted != 2 || r.Changed != 3 || r.Present != 35 || r.Failed != 0 || r.Refused != 0 {
		t.Errorf("report %+v, want 35 present, 2 deleted and 3 changed", r)
	}
	if got := store.deleted(); len(got) != 2 {
		t.Errorf("deleted %v, want the two rows that did not change", got)
	}
	paths, _ := pub.lastEvent()
	if pub.eventCount() != 1 || len(paths) != 2 {
		t.Errorf("published %d event(s), the last naming %v, want one naming the two deleted rows' sources", pub.eventCount(), paths)
	}
	for _, p := range paths {
		if store.changed[p] {
			t.Errorf("published %s as deleted, a row the delete kept", p)
		}
	}
	requireLinesSay(t, rec.Failures(), 1, "one WARN, the summary of a tick that deleted", msgVariantSweepSummary, "deleted=2", "changed=3")
	requireLinesSay(t, rec.Lines(msgVariantRowChanged), 3, "a line per changed row", "listed=")
}

// TestVariantWatcherSaysNothingAtWarnWhenEveryMissingRowChanged — the move's
// shape while it runs: every row the tick found gone had been moved, so the
// tick deleted nothing, published nothing, and its summary is at Info.
func TestVariantWatcherSaysNothingAtWarnWhenEveryMissingRowChanged(t *testing.T) {
	dir := mountedVariantsDir(t)
	rows := rowsWithFiveGone(t, dir)
	changed := map[string]bool{}
	for _, i := range []int{5, 15, 25, 30, 35} {
		changed[mountSource(i)] = true
	}
	store := &changingRows{changed: changed}
	pub := &fakePublisher{}
	w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, store, pub.publish, staticDir(dir), time.Hour, 20)
	rec := loggingtest.Record(t)

	r := w.tick(context.Background())
	if r.Deleted != 0 || r.Changed != 5 || r.Present != 35 {
		t.Errorf("report %+v, want 35 present and 5 changed", r)
	}
	if pub.eventCount() != 0 {
		t.Errorf("published %d upscale.deleted event(s) for a tick that deleted nothing", pub.eventCount())
	}
	if got := rec.Failures(); len(got) != 0 {
		t.Errorf("a tick that deleted nothing logged at Warn: %v", got)
	}
	requireLinesSay(t, rec.Lines(msgVariantSweepSummary), 1, "the summary, at Info", "INFO", "deleted=0", "changed=5")
}
