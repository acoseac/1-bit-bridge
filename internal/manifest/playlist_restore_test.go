package manifest

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The tombstone is data, not a deletion, and these pin the two things
// that makes possible: lifting it, and knowing who wrote it.

func TestRestorePlaylistBringsBackTheRowAndItsItems(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-1", "High-Res Favorites", 100)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if ok, err := s.TombstonePlaylist(ctx, "pl-1", "devB"); err != nil || !ok {
		t.Fatalf("tombstone: ok=%v err=%v", ok, err)
	}

	ok, err := s.RestorePlaylist(ctx, "pl-1")
	if err != nil || !ok {
		t.Fatalf("restore: ok=%v err=%v", ok, err)
	}

	// Readable again, WITH its items — a DELETE only ever set a flag.
	got, gotItems, err := s.GetPlaylist(ctx, "pl-1")
	if err != nil || got == nil {
		t.Fatalf("get after restore: %v (got=%v)", err, got)
	}
	if got.Name != "High-Res Favorites" {
		t.Errorf("name mangled by the round trip: %+v", got)
	}
	if len(gotItems) != len(items) {
		t.Errorf("items after restore = %d, want %d", len(gotItems), len(items))
	}
	// last_modified_at is the CLIENT's clock and the LWW guard key. An
	// operator undoing a delete has not authored a new version, so moving
	// it would make the bridge outrank every device's copy.
	if got.LastModifiedAt != 100 {
		t.Errorf("lastModifiedAt moved on restore: %d, want 100", got.LastModifiedAt)
	}

	// And it is live on both /v1 legs: in the summaries, out of deletedIds.
	list, err := s.ListPlaylists(ctx)
	if err != nil || len(list) != 1 || list[0].ID != "pl-1" {
		t.Fatalf("live list after restore = %+v (err=%v)", list, err)
	}
	ids, err := s.ListPlaylistTombstoneIDs(ctx)
	if err != nil || len(ids) != 0 {
		t.Errorf("tombstone ids after restore = %v (err=%v), want none", ids, err)
	}
}

func TestRestorePlaylistMissesAreNotErrors(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-1", "live", 100)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// A live playlist: nothing to lift.
	if ok, err := s.RestorePlaylist(ctx, "pl-1"); err != nil || ok {
		t.Errorf("restore of a live playlist: ok=%v err=%v, want false/nil", ok, err)
	}
	// An id nobody has ever seen.
	if ok, err := s.RestorePlaylist(ctx, "pl-nope"); err != nil || ok {
		t.Errorf("restore of an unknown id: ok=%v err=%v, want false/nil", ok, err)
	}
	// An empty id is a caller bug, not a miss — it would otherwise be a
	// silent no-op that looks like "already live".
	if _, err := s.RestorePlaylist(ctx, ""); err == nil {
		t.Error("restore of an empty id returned no error")
	}
}

// TestUpdatedAtOnATombstonedRowIsTheDeleteTime is what migration v45
// leans on when it declines to add a `deleted_at` column: on a
// `deleted = 1` row, updated_at already IS the delete time, because the
// one writer of that flag stamps both in a single statement. A future
// writer that moves updated_at on a tombstoned row without lifting the
// flag breaks the console's "deleted 3m ago", and nothing else would
// notice.
func TestUpdatedAtOnATombstonedRowIsTheDeleteTime(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()

	const wrote = int64(1_000_000_000)
	const deleted = int64(5_000_000_000)

	s.now = func() time.Time { return time.Unix(0, wrote) }
	p, items := samplePlaylist("pl-1", "doomed", 100)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	s.now = func() time.Time { return time.Unix(0, deleted) }
	if ok, err := s.TombstonePlaylist(ctx, "pl-1", "devB"); err != nil || !ok {
		t.Fatalf("tombstone: ok=%v err=%v", ok, err)
	}

	rows, err := s.ListDeletedPlaylistsForAdmin(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("deleted list = %+v (err=%v)", rows, err)
	}
	if rows[0].DeletedAt != deleted {
		t.Errorf("DeletedAt = %d, want the tombstone's clock %d — updated_at "+
			"stopped tracking the delete time, so migration v45's reason for "+
			"having no deleted_at column no longer holds",
			rows[0].DeletedAt, deleted)
	}
	if rows[0].LastModifiedAt != 100 {
		t.Errorf("LastModifiedAt = %d, want the client's 100", rows[0].LastModifiedAt)
	}
}

// The deleter is not the last writer, and conflating them is what left
// the 2026-09-20 incident unattributable: every tombstoned row named the
// device that had MADE the playlists, not the one that deleted them.
func TestDeletedPlaylistsCarryBothDevices(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	if err := s.UpsertDeviceRegistration(ctx, "devA-token", "tok-a", "Studio Mac"); err != nil {
		t.Fatalf("register writer: %v", err)
	}
	if err := s.UpsertDeviceRegistration(ctx, "devB-token", "tok-b", "New iPhone"); err != nil {
		t.Fatalf("register deleter: %v", err)
	}
	p, items := samplePlaylist("pl-1", "Shared", 100)
	if err := s.UpsertPlaylist(ctx, "devA-token", p, items); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if ok, err := s.TombstonePlaylist(ctx, "pl-1", "devB-token"); err != nil || !ok {
		t.Fatalf("tombstone: ok=%v err=%v", ok, err)
	}

	rows, err := s.ListDeletedPlaylistsForAdmin(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("deleted list = %+v (err=%v)", rows, err)
	}
	got := rows[0]
	if got.WroteLastToken != "devA-token" || got.WroteLastName != "Studio Mac" {
		t.Errorf("writer = %q/%q, want devA-token/Studio Mac", got.WroteLastToken, got.WroteLastName)
	}
	if got.DeletedByToken != "devB-token" || got.DeletedByName != "New iPhone" {
		t.Errorf("deleter = %q/%q, want devB-token/New iPhone", got.DeletedByToken, got.DeletedByName)
	}
	if got.DeletedByTokenID != "tok-b" {
		t.Errorf("deleter token id = %q, want tok-b", got.DeletedByTokenID)
	}
	if got.TrackCount != len(items) {
		t.Errorf("TrackCount = %d, want %d", got.TrackCount, len(items))
	}

	// Restoring clears the deleter: it is provenance for THAT tombstone,
	// and a stale value would attribute the next delete to the wrong device.
	if ok, err := s.RestorePlaylist(ctx, "pl-1"); err != nil || !ok {
		t.Fatalf("restore: ok=%v err=%v", ok, err)
	}
	if ok, err := s.TombstonePlaylist(ctx, "pl-1", ""); err != nil || !ok {
		t.Fatalf("second tombstone: ok=%v err=%v", ok, err)
	}
	rows, err = s.ListDeletedPlaylistsForAdmin(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("deleted list after re-delete = %+v (err=%v)", rows, err)
	}
	if rows[0].DeletedByToken != "" || rows[0].DeletedByName != "" {
		t.Errorf("stale deleter survived a restore: %q/%q",
			rows[0].DeletedByToken, rows[0].DeletedByName)
	}
}

// A tombstone written before migration v45 has no deleter, and a row
// whose live copy was never deleted must not appear at all.
func TestDeletedListHoldsOnlyTombstones(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	live, liveItems := samplePlaylist("pl-live", "Live", 100)
	dead, deadItems := samplePlaylist("pl-dead", "Dead", 200)
	if err := s.UpsertPlaylist(ctx, "devA", live, liveItems); err != nil {
		t.Fatalf("upsert live: %v", err)
	}
	if err := s.UpsertPlaylist(ctx, "devA", dead, deadItems); err != nil {
		t.Fatalf("upsert dead: %v", err)
	}
	if ok, err := s.TombstonePlaylist(ctx, "pl-dead", "devA"); err != nil || !ok {
		t.Fatalf("tombstone: ok=%v err=%v", ok, err)
	}
	rows, err := s.ListDeletedPlaylistsForAdmin(ctx)
	if err != nil || len(rows) != 1 || rows[0].ID != "pl-dead" {
		t.Fatalf("deleted list = %+v (err=%v), want exactly pl-dead", rows, err)
	}
	// And the live one is still in the live admin list, unchanged.
	all, err := s.ListAllPlaylistsForAdmin(ctx)
	if err != nil || len(all) != 1 || all[0].ID != "pl-live" {
		t.Fatalf("live admin list = %+v (err=%v), want exactly pl-live", all, err)
	}
}

func TestCountRecentPlaylistTombstonesByCountsOneDeviceInsideTheWindow(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	if err := s.UpsertDeviceRegistration(ctx, "burst-dev", "tok-burst", "New iPhone"); err != nil {
		t.Fatalf("register: %v", err)
	}

	base := time.Unix(1_700_000_000, 0)
	seed := func(id, deleter string, at time.Time) {
		p, items := samplePlaylist(id, id, 100)
		s.now = func() time.Time { return at }
		if err := s.UpsertPlaylist(ctx, "writer", p, items); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
		if ok, err := s.TombstonePlaylist(ctx, id, deleter); err != nil || !ok {
			t.Fatalf("tombstone %s: ok=%v err=%v", id, ok, err)
		}
	}
	// Three inside the window from the burst device, one from another
	// device in the same instant, and one from the burst device long ago.
	seed("in-1", "burst-dev", base)
	seed("in-2", "burst-dev", base.Add(2*time.Second))
	seed("in-3", "burst-dev", base.Add(4*time.Second))
	seed("other", "quiet-dev", base.Add(3*time.Second))
	seed("old", "burst-dev", base.Add(-10*time.Minute))

	since := base.Add(-PlaylistDeleteBurstWindow)
	got, err := s.CountRecentPlaylistTombstonesBy(ctx, "burst-dev", since)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got.Count != 3 {
		t.Errorf("Count = %d, want 3 (the other device's delete and the old "+
			"one must not be pooled in)", got.Count)
	}
	if got.DeviceName != "New iPhone" || got.TokenID != "tok-burst" {
		t.Errorf("identity = %q/%q, want New iPhone/tok-burst", got.DeviceName, got.TokenID)
	}

	// A restored playlist leaves the count: it is no longer deleted.
	if ok, err := s.RestorePlaylist(ctx, "in-1"); err != nil || !ok {
		t.Fatalf("restore: ok=%v err=%v", ok, err)
	}
	if got, err := s.CountRecentPlaylistTombstonesBy(ctx, "burst-dev", since); err != nil || got.Count != 2 {
		t.Errorf("Count after restore = %+v (err=%v), want 2", got, err)
	}

	// An unattributed delete must not pool into one phantom device.
	if got, err := s.CountRecentPlaylistTombstonesBy(ctx, "", since); err != nil || got.Count != 0 {
		t.Errorf("Count for an empty device token = %+v (err=%v), want 0", got, err)
	}
}

// The console's burst grouping. It has to answer the same question the
// live WARN answers, because the panel's sentence and the journal line
// describe one event — and the shape CodeRabbit named on #942 (a second
// device deleting once in the middle of a run) is exactly where the old
// client-side chain-of-gaps reading disagreed with it.

func runRow(id, deleter, name string, atSec int64) AdminDeletedPlaylist {
	return AdminDeletedPlaylist{
		ID: id, Name: id, DeletedByToken: deleter, DeletedByName: name,
		DeletedAt: atSec * int64(time.Second),
	}
}

func TestLargestPlaylistDeleteRunSpansAnInterleavedDevice(t *testing.T) {
	// Device A deletes six, and device B deletes one in the middle. A
	// chain-of-gaps reading stops at B and reports three; the window says
	// six, which is what CountRecentPlaylistTombstonesBy counted live.
	rows := []AdminDeletedPlaylist{
		runRow("a6", "devA", "Studio Mac", 106),
		runRow("a5", "devA", "Studio Mac", 105),
		runRow("a4", "devA", "Studio Mac", 104),
		runRow("b1", "devB", "New iPhone", 103),
		runRow("a3", "devA", "Studio Mac", 102),
		runRow("a2", "devA", "Studio Mac", 101),
		runRow("a1", "devA", "Studio Mac", 100),
	}
	run, ok := LargestPlaylistDeleteRun(rows)
	if !ok {
		t.Fatal("no run found across an interleaved device")
	}
	if run.Count != 6 {
		t.Errorf("Count = %d, want 6 — one delete from another device must not end the run", run.Count)
	}
	if run.DeviceToken != "devA" || run.DeviceName != "Studio Mac" {
		t.Errorf("device = %q/%q, want devA/Studio Mac", run.DeviceToken, run.DeviceName)
	}
	if want := 6 * int64(time.Second); run.SpanNS != want {
		t.Errorf("SpanNS = %d, want %d", run.SpanNS, want)
	}
}

func TestLargestPlaylistDeleteRunIsAWindowNotATotal(t *testing.T) {
	// Twenty this morning, one just now: the run is still twenty, which is
	// the number the WARN reported at the time. An anchor at the device's
	// NEWEST tombstone would report one and say nothing happened.
	var rows []AdminDeletedPlaylist
	for i := 0; i < 20; i++ {
		rows = append(rows, runRow(fmt.Sprintf("old-%d", i), "devA", "Studio Mac", 1000+int64(i)))
	}
	rows = append(rows, runRow("recent", "devA", "Studio Mac", 100000))
	run, ok := LargestPlaylistDeleteRun(rows)
	if !ok || run.Count != 20 {
		t.Errorf("run = %+v ok=%v, want a count of 20", run, ok)
	}

	// Spread the same twenty an hour apart and nothing is a burst.
	var spread []AdminDeletedPlaylist
	for i := 0; i < 20; i++ {
		spread = append(spread, runRow(fmt.Sprintf("slow-%d", i), "devA", "Studio Mac", int64(i)*3600))
	}
	if run, ok := LargestPlaylistDeleteRun(spread); ok {
		t.Errorf("twenty deletes an hour apart reported a burst: %+v", run)
	}
}

func TestLargestPlaylistDeleteRunNeedsAnAttributableDevice(t *testing.T) {
	// Tombstones from before migration v45, or from a DELETE that carried
	// no device token: plenty of them, all unattributable, no claim made.
	var rows []AdminDeletedPlaylist
	for i := 0; i < PlaylistDeleteBurstThreshold+5; i++ {
		rows = append(rows, runRow(fmt.Sprintf("orphan-%d", i), "", "", 100+int64(i)))
	}
	if run, ok := LargestPlaylistDeleteRun(rows); ok {
		t.Errorf("claimed a burst with no deleter recorded: %+v", run)
	}
	// And a sub-threshold run is not one either.
	var few []AdminDeletedPlaylist
	for i := 0; i < PlaylistDeleteBurstThreshold-1; i++ {
		few = append(few, runRow(fmt.Sprintf("few-%d", i), "devA", "Studio Mac", 100+int64(i)))
	}
	if run, ok := LargestPlaylistDeleteRun(few); ok {
		t.Errorf("claimed a burst below the threshold: %+v", run)
	}
	if run, ok := LargestPlaylistDeleteRun(nil); ok {
		t.Errorf("claimed a burst from no rows at all: %+v", run)
	}
}

// The grouping walks a Go map, so a comparator that leaves two devices
// equal would flip the reported device between two renders of the same
// data — the dupe elector's rule, and the reason for the token tie-break.
func TestLargestPlaylistDeleteRunIsStableAcrossRuns(t *testing.T) {
	var rows []AdminDeletedPlaylist
	for _, dev := range []string{"devA", "devB", "devC"} {
		for i := 0; i < PlaylistDeleteBurstThreshold+1; i++ {
			rows = append(rows, runRow(dev+fmt.Sprint(i), dev, dev+" name", 100+int64(i)))
		}
	}
	first, ok := LargestPlaylistDeleteRun(rows)
	if !ok {
		t.Fatal("no run found")
	}
	for i := 0; i < 50; i++ {
		got, ok := LargestPlaylistDeleteRun(rows)
		if !ok || got != first {
			t.Fatalf("run %d = %+v (ok=%v), want the stable %+v", i, got, ok, first)
		}
	}
}
