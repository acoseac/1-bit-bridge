package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openJournalTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func journalTombstones(t *testing.T, s *Store) map[string]int64 {
	t.Helper()
	rows, err := s.db.Query(`SELECT path, deleted_at FROM manifest_deletions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var p string
		var at int64
		if err := rows.Scan(&p, &at); err != nil {
			t.Fatal(err)
		}
		out[p] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func journalUpsert(t *testing.T, s *Store, path string, size int64) {
	t.Helper()
	tr := &Track{Path: path, Size: size, ModTime: time.Now(), Artist: "A", Album: "B", Title: path}
	if err := s.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
}

// The const-derivation pin: every journal INSERT must embed the exact
// predicate its sibling DELETE uses. Mirrors
// TestThresholdReapPredicatesAreShared — drift here silently tombstones
// a different row set than the one deleted.
func TestDeletionJournalPredicatesAreShared(t *testing.T) {
	squash := func(x string) string { return strings.Join(strings.Fields(x), " ") }
	for _, tc := range []struct{ name, stmt, want string }{
		{"journalThresholdReapBatchSQL", journalThresholdReapBatchSQL, thresholdReapBatchWhereSQL},
		{"journalThresholdReapOneSQL", journalThresholdReapOneSQL, thresholdReapOneWhereSQL},
		{"journalDeleteByPrefixSQL", journalDeleteByPrefixSQL, deleteByPrefixWhereSQL},
		{"deleteTracksByPrefixSQL", deleteTracksByPrefixSQL, deleteByPrefixWhereSQL},
		{"journalClearMissingSQL", journalClearMissingSQL, clearMissingTracksWhereSQL},
		{"clearMissingTracksDeleteSQL", clearMissingTracksDeleteSQL, clearMissingTracksWhereSQL},
		// The two tombstone-clear forms share the served-row guard, so
		// the per-path (UpsertTrack) and batch-sweep (UpsertTrackBatch)
		// variants can't diverge on the `dupe_suppressed = 0` term.
		{"clearTombstoneIfServedSQL", clearTombstoneIfServedSQL, clearTombstoneServedGuardSQL},
		{"clearAllServedTombstonesSQL", clearAllServedTombstonesSQL, clearTombstoneServedGuardSQL},
	} {
		if !strings.Contains(squash(tc.stmt), squash(tc.want)) {
			t.Errorf("%s no longer embeds its shared predicate — the tombstoned set and the deleted set can diverge.\nStatement:\n%s\n\nwant it to contain:\n%s",
				tc.name, tc.stmt, tc.want)
		}
	}
}

func TestJournal_ThresholdReapWritesTombstones(t *testing.T) {
	s := openJournalTestStore(t)
	ctx := context.Background()
	journalUpsert(t, s, "a/one.flac", 1)
	journalUpsert(t, s, "a/two.flac", 2)

	deleted, err := s.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{"a/one.flac"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	ts := journalTombstones(t, s)
	if _, ok := ts["a/one.flac"]; !ok || len(ts) != 1 {
		t.Fatalf("tombstones = %v, want exactly a/one.flac", ts)
	}
}

func TestJournal_DeleteTracksBatchWritesTombstones(t *testing.T) {
	s := openJournalTestStore(t)
	ctx := context.Background()
	// Seed enough survivors that the 2-path delete stays under the
	// mass-op guard's 25% bound (the guard case has its own test below).
	journalUpsert(t, s, "b/one.flac", 1)
	journalUpsert(t, s, "b/two.flac", 2)
	for i := 0; i < 10; i++ {
		journalUpsert(t, s, fmt.Sprintf("b/keep-%02d.flac", i), int64(10+i))
	}

	if err := s.DeleteTracksBatch(ctx, []string{"b/one.flac", "b/two.flac"}); err != nil {
		t.Fatal(err)
	}
	ts := journalTombstones(t, s)
	if len(ts) != 2 {
		t.Fatalf("tombstones = %v, want 2", ts)
	}
	if _, ok := ts["b/keep-00.flac"]; ok {
		t.Fatalf("surviving row must not be tombstoned")
	}
}

func TestJournal_DeleteTracksBatchMassOpResetsCoverage(t *testing.T) {
	s := openJournalTestStore(t)
	ctx := context.Background()
	// 4 tracks; deleting 2 = 50% > 25% → mass-op guard trips.
	paths := []string{"m/1.flac", "m/2.flac", "m/3.flac", "m/4.flac"}
	for i, p := range paths {
		journalUpsert(t, s, p, int64(i+1))
	}
	preWipe := time.Now().Add(-time.Minute)
	if covered, err := s.DeltaSinceCovered(ctx, time.Now()); err != nil || !covered {
		t.Fatalf("precondition: fresh store must cover a now-cursor (covered=%v err=%v)", covered, err)
	}

	if err := s.DeleteTracksBatch(ctx, paths[:2]); err != nil {
		t.Fatal(err)
	}
	if ts := journalTombstones(t, s); len(ts) != 0 {
		t.Fatalf("mass-op must NOT write per-path tombstones, got %v", ts)
	}
	covered, err := s.DeltaSinceCovered(ctx, preWipe)
	if err != nil {
		t.Fatal(err)
	}
	if covered {
		t.Fatalf("mass-op must reset coverage — a pre-reset cursor can no longer be answered")
	}
}

func TestJournal_ByPrefixClearMissingAndDeleteTrack(t *testing.T) {
	s := openJournalTestStore(t)
	ctx := context.Background()
	journalUpsert(t, s, "root/a/one.flac", 1)
	journalUpsert(t, s, "root/a/two.flac", 2)
	journalUpsert(t, s, "elsewhere/three.flac", 3)

	if _, err := s.DeleteTracksByPrefix(ctx, "root/a"); err != nil {
		t.Fatal(err)
	}
	ts := journalTombstones(t, s)
	if len(ts) != 2 {
		t.Fatalf("byPrefix tombstones = %v, want the two root/a rows", ts)
	}

	// DeleteTrack journals its single path.
	if err := s.DeleteTrack(ctx, "elsewhere/three.flac"); err != nil {
		t.Fatal(err)
	}
	if _, ok := journalTombstones(t, s)["elsewhere/three.flac"]; !ok {
		t.Fatalf("DeleteTrack must tombstone its path")
	}

	// ClearMissingCounts journals the rows it reaps.
	journalUpsert(t, s, "miss/four.flac", 4)
	if _, err := s.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{"miss/four.flac"}, 99); err != nil {
		t.Fatal(err) // increments to 1, threshold 99 → no reap yet
	}
	if _, err := s.ClearMissingCounts(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := journalTombstones(t, s)["miss/four.flac"]; !ok {
		t.Fatalf("ClearMissingCounts must tombstone its reaped rows")
	}
}

func TestJournal_ReAddClearsTombstone(t *testing.T) {
	s := openJournalTestStore(t)
	ctx := context.Background()
	journalUpsert(t, s, "re/add.flac", 1)
	if err := s.DeleteTrack(ctx, "re/add.flac"); err != nil {
		t.Fatal(err)
	}
	if len(journalTombstones(t, s)) != 1 {
		t.Fatal("precondition: tombstone written")
	}
	// The file comes back — the upsert must clear its tombstone (both
	// the single and the batch writers carry the clear).
	journalUpsert(t, s, "re/add.flac", 2)
	if ts := journalTombstones(t, s); len(ts) != 0 {
		t.Fatalf("re-added path must clear its tombstone, got %v", ts)
	}

	// Batch-writer variant.
	if err := s.DeleteTrack(ctx, "re/add.flac"); err != nil {
		t.Fatal(err)
	}
	tr := &Track{Path: "re/add.flac", Size: 3, ModTime: time.Now(), Artist: "A", Album: "B"}
	if err := s.UpsertTrackBatch(ctx, []*Track{tr}); err != nil {
		t.Fatal(err)
	}
	if ts := journalTombstones(t, s); len(ts) != 0 {
		t.Fatalf("batch re-add must clear the tombstone too, got %v", ts)
	}
}

func TestJournal_MetadataChangeUpsertEmitsNoTombstone(t *testing.T) {
	// Review insurance: upserts are INSERT … ON CONFLICT UPDATE — the
	// journal attaches only to the enumerated DELETE sites, so a
	// metadata-change re-upsert must write NOTHING to the journal.
	s := openJournalTestStore(t)
	journalUpsert(t, s, "meta/change.flac", 1)
	journalUpsert(t, s, "meta/change.flac", 2) // content changed
	if ts := journalTombstones(t, s); len(ts) != 0 {
		t.Fatalf("metadata-change upsert must not tombstone, got %v", ts)
	}
}

func TestJournal_SuppressionTransitions(t *testing.T) {
	s := openJournalTestStore(t)
	ctx := context.Background()
	journalUpsert(t, s, "dupe/copy.flac", 1)

	// 0→1: served→suppressed journals a tombstone; indexed_at is NOT
	// bumped (the standing rule — the tombstone is the delta signal).
	preIndexed := journalTrackIndexedAt(t, s, "dupe/copy.flac")
	if _, err := s.ApplyDupeStamps(ctx, []DupeStamp{{
		Path: "dupe/copy.flac", GroupID: "g1", Tier: "same-format",
		Suppressed: true, JournalDelete: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := journalTombstones(t, s)["dupe/copy.flac"]; !ok {
		t.Fatalf("suppression must tombstone the path")
	}
	if got := journalTrackIndexedAt(t, s, "dupe/copy.flac"); got != preIndexed {
		t.Fatalf("suppression must NOT bump indexed_at (was %d, now %d)", preIndexed, got)
	}

	// Content-changed upsert while STILL suppressed keeps the tombstone
	// (the dupe_suppressed = 0 EXISTS guard).
	journalUpsert(t, s, "dupe/copy.flac", 2)
	if _, ok := journalTombstones(t, s)["dupe/copy.flac"]; !ok {
		t.Fatalf("suppressed re-upsert must KEEP its tombstone")
	}

	// 1→0: the served transition clears the tombstone (+ the existing
	// indexed_at bump the delta story relies on).
	if _, err := s.ApplyDupeStamps(ctx, []DupeStamp{{
		Path: "dupe/copy.flac", GroupID: "g1", Tier: "same-format",
		Suppressed: false, BumpIndexed: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if ts := journalTombstones(t, s); len(ts) != 0 {
		t.Fatalf("un-suppression must clear the tombstone, got %v", ts)
	}
}

func journalTrackIndexedAt(t *testing.T, s *Store, path string) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRow(`SELECT indexed_at FROM tracks WHERE path = ?`, path).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestJournal_WipeResetsCoverage(t *testing.T) {
	s := openJournalTestStore(t)
	ctx := context.Background()
	journalUpsert(t, s, "w/one.flac", 1)
	if err := s.DeleteTrack(ctx, "w/one.flac"); err != nil {
		t.Fatal(err)
	}
	preWipe := time.Now().Add(-time.Minute)

	if err := s.WipeAllTracks(ctx); err != nil {
		t.Fatal(err)
	}
	if ts := journalTombstones(t, s); len(ts) != 0 {
		t.Fatalf("wipe must clear all tombstones, got %v", ts)
	}
	covered, err := s.DeltaSinceCovered(ctx, preWipe)
	if err != nil {
		t.Fatal(err)
	}
	if covered {
		t.Fatalf("a pre-wipe cursor must read as NOT covered (full resync)")
	}
}

func TestJournal_PruneAdvancesCoverage(t *testing.T) {
	s := openJournalTestStore(t)
	ctx := context.Background()
	base := time.Now()

	// Tombstone written "200 days ago" via the injected clock.
	old := base.Add(-200 * 24 * time.Hour)
	s.now = func() time.Time { return old }
	journalUpsert(t, s, "p/old.flac", 1)
	if err := s.DeleteTrack(ctx, "p/old.flac"); err != nil {
		t.Fatal(err)
	}
	// And a fresh one.
	s.now = func() time.Time { return base }
	journalUpsert(t, s, "p/new.flac", 2)
	if err := s.DeleteTrack(ctx, "p/new.flac"); err != nil {
		t.Fatal(err)
	}

	pruned, err := s.PruneDeletionJournal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1 (the 200-day tombstone)", pruned)
	}
	ts := journalTombstones(t, s)
	if _, ok := ts["p/new.flac"]; !ok || len(ts) != 1 {
		t.Fatalf("fresh tombstone must survive the prune, got %v", ts)
	}
	// A cursor OLDER than the prune horizon can no longer be answered.
	covered, err := s.DeltaSinceCovered(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	if covered {
		t.Fatalf("a cursor older than the prune horizon must read as not covered")
	}
	// A cursor inside the retention window AND after the store's
	// coverage seed (store creation time) stays covered.
	covered, err = s.DeltaSinceCovered(ctx, base.Add(time.Second))
	if err != nil || !covered {
		t.Fatalf("a fresh cursor must stay covered (covered=%v err=%v)", covered, err)
	}
}

// --- manifest-level delta emission ---

func TestManifest_DeltaCarriesDeletedPaths_BothLegs(t *testing.T) {
	s := openJournalTestStore(t)
	ctx := context.Background()
	journalUpsert(t, s, "d/gone.flac", 1)
	journalUpsert(t, s, "d/stays.flac", 2)
	// Ordering: coverage seed (store creation) < since < deleted_at —
	// the sleeps make both inequalities strict regardless of clock
	// granularity.
	time.Sleep(5 * time.Millisecond)
	since := time.Now()
	time.Sleep(5 * time.Millisecond) // deletion strictly after `since`
	if err := s.DeleteTrack(ctx, "d/gone.flac"); err != nil {
		t.Fatal(err)
	}

	// Buffered leg.
	m, err := BuildManifest(ctx, s, []string{"/lib"}, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Deleted) != 1 || m.Deleted[0] != "d/gone.flac" {
		t.Fatalf("BuildManifest delta Deleted = %v, want [d/gone.flac]", m.Deleted)
	}
	if m.DeltaIncomplete {
		t.Fatalf("a covered cursor must not read deltaIncomplete")
	}

	// Streaming leg — decode the wire bytes and require parity.
	var buf bytes.Buffer
	if err := WriteManifest(ctx, &buf, s, []string{"/lib"}, since); err != nil {
		t.Fatal(err)
	}
	var streamed Manifest
	if err := json.Unmarshal(buf.Bytes(), &streamed); err != nil {
		t.Fatalf("decode streamed manifest: %v\n%s", err, buf.String())
	}
	if len(streamed.Deleted) != 1 || streamed.Deleted[0] != "d/gone.flac" {
		t.Fatalf("streamed delta Deleted = %v, want [d/gone.flac]", streamed.Deleted)
	}
	if streamed.DeltaIncomplete {
		t.Fatalf("streamed leg must agree deltaIncomplete=false")
	}

	// Legs isolation: a FULL manifest carries neither field.
	full, err := BuildManifest(ctx, s, []string{"/lib"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if full.Deleted != nil || full.DeltaIncomplete {
		t.Fatalf("full manifest must not carry delta fields (deleted=%v incomplete=%v)",
			full.Deleted, full.DeltaIncomplete)
	}
	buf.Reset()
	if err := WriteManifest(ctx, &buf, s, []string{"/lib"}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"deleted"`)) ||
		bytes.Contains(buf.Bytes(), []byte(`"deltaIncomplete"`)) {
		t.Fatalf("full streamed manifest must not emit delta fields:\n%s", buf.String())
	}
}

func TestManifest_DeltaIncompleteWhenCursorPredatesCoverage(t *testing.T) {
	s := openJournalTestStore(t)
	ctx := context.Background()
	journalUpsert(t, s, "c/one.flac", 1)
	// Coverage started at store creation (v41 post seed) — a cursor a
	// year earlier predates it.
	stale := time.Now().Add(-365 * 24 * time.Hour)

	m, err := BuildManifest(ctx, s, []string{"/lib"}, stale)
	if err != nil {
		t.Fatal(err)
	}
	if !m.DeltaIncomplete {
		t.Fatalf("a pre-coverage cursor must answer deltaIncomplete")
	}
	if len(m.Deleted) != 0 {
		t.Fatalf("an incomplete delta must not carry a partial deleted list, got %v", m.Deleted)
	}

	var buf bytes.Buffer
	if err := WriteManifest(ctx, &buf, s, []string{"/lib"}, stale); err != nil {
		t.Fatal(err)
	}
	var streamed Manifest
	if err := json.Unmarshal(buf.Bytes(), &streamed); err != nil {
		t.Fatal(err)
	}
	if !streamed.DeltaIncomplete {
		t.Fatalf("streamed leg must agree deltaIncomplete=true")
	}
}

func TestManifest_DeltaOmitsPathsWithServedRows(t *testing.T) {
	// A path with both a tombstone and a SERVED row (rare — a journal
	// write whose delete failed) must NOT reach the wire: a delta
	// response never names a path in both `tracks` and `deleted`.
	s := openJournalTestStore(t)
	ctx := context.Background()
	journalUpsert(t, s, "o/live.flac", 1)
	if _, err := s.db.Exec(journalSinglePathSQL, time.Now().UnixNano(), "o/live.flac"); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-time.Hour)
	deleted, overflow, err := s.DeletedSince(ctx, since)
	if err != nil || overflow {
		t.Fatalf("DeletedSince: %v overflow=%v", err, overflow)
	}
	if len(deleted) != 0 {
		t.Fatalf("a served row's path must not be reported deleted, got %v", deleted)
	}
}
