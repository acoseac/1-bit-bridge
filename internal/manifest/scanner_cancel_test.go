package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/sqlitetest"
)

// A scan runs on runServe's scanCtx, and a shutdown that cancels it while a
// pass is writing is a pass that STOPPED, not one that failed
// (ctxerr.WithoutCancellation). Each test below cancels a real scan INSIDE
// one of its writes: it creates an index under sqlitetest's collation over
// the column that write moves, so the statement calls back into the test
// part-way, and the cancel lands while it runs. Every write the pass makes
// after it then runs on the cancelled context too, which is what a
// shutdown does to the rest of the pass.
//
// Each has a twin that makes the same writes FAIL on a live context, with a
// trigger that raises, so a genuine failure at the same site is still
// reported.

// Messages the scanner logs when a write fails.
const (
	msgTracksPass        = "missing-count tracks pass"
	msgRenameReap        = "case-only rename reap"
	msgFoldersPass       = "missing-count folders pass"
	msgUpsertBatch       = "upsert batch"
	msgStampBatch        = "stamp extractor-version batch"
	msgUpsertFolder      = "upsert folder"
	msgResetMissingCount = "reset missing_count on skip"
	msgSACDRetire        = "sacd stale-row retire"
	msgSubtreeTracks     = "subtree missing-count tracks pass"
	msgSubtreeRename     = "subtree case-only rename reap"
	msgSubtreeFolders    = "subtree missing-count folders pass"
	msgSubtreeFolder     = "subtree upsert folder"
	msgDupeStamping      = "duplicate stamping"
	msgDupeSummary       = "save dupe summary"
	msgListWaveforms     = "list waveform sidecars"
	msgIterWaveforms     = "iter waveform sidecars"
	msgEmptyRootCount    = "count tracks under root; conservatively sparing deletion for root"
	msgSubtreeAudit      = "subtree absent but owning root audit failed"
)

// reconciliationMessages are the five post-scan passes, in the order Scan
// runs them.
var reconciliationMessages = []string{
	"album-title reconciliation",
	"album-artist reconciliation",
	"year reconciliation",
	"year reconciliation (mbid)",
	"track-number reconciliation",
}

// TestAScanStoppedInItsDeletionPassReportsNothing cancels a scan inside the
// missing-count UPDATE of its deletion pass. The case-only rename reap and
// the folders pass then run on the cancelled context. None of the three is
// reported, and the cancelled pass changed nothing: its transaction rolled
// back, the renamed row is still there, and nothing counts as missing yet.
func TestAScanStoppedInItsDeletionPassReportsNothing(t *testing.T) {
	root := t.TempDir()
	s, sc := newScanFixture(t, root)
	stageDeletionPass(t, s, sc, root, root)
	parkIndex(t, s, `tracks ((CAST(missing_count AS TEXT)) COLLATE `+sqlitetest.Collation+`) WHERE missing_count > 0`)

	rec := loggingtest.Record(t)
	_ = cancelInsideAWrite(t, func(ctx context.Context) error { _, err := sc.Scan(ctx); return err })

	mustNotReport(t, rec, msgTracksPass, msgRenameReap, msgFoldersPass)
	mustHaveMissingCount(t, s, "tracks", "Album/b.flac", 0)
	mustHaveMissingCount(t, s, "tracks", "Gone/c.flac", 0)
	mustHaveMissingCount(t, s, "folders", "Gone", 0)
	mustIndexed(t, s, "Album/Old.flac")
}

// TestAScanWhoseDeletionPassFailsStillReportsIt is the twin: the same three
// writes fail on a live context, and each is reported once.
func TestAScanWhoseDeletionPassFailsStillReportsIt(t *testing.T) {
	root := t.TempDir()
	s, sc := newScanFixture(t, root)
	stageDeletionPass(t, s, sc, root, root)
	abortOn(t, s, `BEFORE UPDATE OF missing_count ON tracks WHEN NEW.missing_count > 0`)
	abortOn(t, s, `BEFORE DELETE ON tracks`)
	abortOn(t, s, `BEFORE UPDATE OF missing_count ON folders WHEN NEW.missing_count > 0`)

	rec := loggingtest.Record(t)
	scanOnce(t, sc, "failing deletion pass")

	mustReportOnce(t, rec, msgTracksPass, msgRenameReap, msgFoldersPass)
}

// TestAScanStoppedInItsReconciliationReportsNothing cancels a scan inside
// the album-title pass's UPDATE, the first reconciliation write. The four
// passes after it then run on the cancelled context. None of the five is
// reported, and the album the first pass was rewriting is unchanged.
func TestAScanStoppedInItsReconciliationReportsNothing(t *testing.T) {
	root := t.TempDir()
	s, sc := newScanFixture(t, root)
	stageAlbumTitleFix(t, s, sc, root)
	// Nothing else a rescan of unchanged files does moves indexed_at, so the
	// first statement to reach this index is the reconciliation's UPDATE.
	parkIndex(t, s, `tracks ((CAST(indexed_at AS TEXT)) COLLATE `+sqlitetest.Collation+`)`)

	rec := loggingtest.Record(t)
	_ = cancelInsideAWrite(t, func(ctx context.Context) error { _, err := sc.Scan(ctx); return err })

	mustNotReport(t, rec, reconciliationMessages...)
	if got := albumOf(t, s, "Album/b.flac"); got != "Album" {
		t.Errorf("album of the row the stopped pass was rewriting = %q, want it unchanged (\"Album\")", got)
	}
}

// TestAScanWhoseReconciliationFailsStillReportsIt is the twin: the
// album-title rewrite fails on a live context, and is reported once.
func TestAScanWhoseReconciliationFailsStillReportsIt(t *testing.T) {
	root := t.TempDir()
	s, sc := newScanFixture(t, root)
	stageAlbumTitleFix(t, s, sc, root)
	abortOn(t, s, `BEFORE UPDATE OF tags_json ON tracks`)

	rec := loggingtest.Record(t)
	scanOnce(t, sc, "failing reconciliation")

	mustReportOnce(t, rec, reconciliationMessages[0])
}

// TestAScanStoppedInItsFinalWriteLosesNothingTheNextScanCannotRedo cancels a
// scan inside its writer's final flush: the upsert of a changed file, with
// the extractor-version stamp of a version-stale one queued behind it.
// Neither is reported. The rows the cancel dropped were extracted and never
// written, which is deliberate: the writer's drain already drops every batch
// after a cancel, and the next scan re-extracts them, because its skip gate
// compares the file against the stored row, which the dropped write never
// touched. The last half pins exactly that.
func TestAScanStoppedInItsFinalWriteLosesNothingTheNextScanCannotRedo(t *testing.T) {
	root := t.TempDir()
	s, sc := newScanFixture(t, root)
	stageFinalFlush(t, s, sc, root)
	parkIndex(t, s, `tracks ((CAST(mtime_ns AS TEXT)) COLLATE `+sqlitetest.Collation+`)`)
	indexedBefore := indexedAtOf(t, s, "Album/a.flac")

	rec := loggingtest.Record(t)
	err := cancelInsideAWrite(t, func(ctx context.Context) error { _, err := sc.Scan(ctx); return err })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a scan stopped in its final flush returned %v, want context.Canceled", err)
	}
	mustNotReport(t, rec, msgUpsertBatch, msgStampBatch)
	if got := titleOf(t, s, "Album/b.flac"); got != "Before" {
		t.Fatalf("the stopped flush wrote the changed file anyway: title %q", got)
	}
	if got := extractorVersionOf(t, s, "Album/a.flac"); got != 0 {
		t.Fatalf("the stopped flush stamped the version-stale file anyway: extractor_version %d", got)
	}

	scanOnce(t, sc, "the scan after")
	if got := titleOf(t, s, "Album/b.flac"); got != "After" {
		t.Errorf("the next scan did not re-extract the file the stopped flush dropped: title %q", got)
	}
	if got := extractorVersionOf(t, s, "Album/a.flac"); got != ExtractorVersion {
		t.Errorf("the next scan did not stamp the version-stale file: extractor_version %d, want %d", got, ExtractorVersion)
	}
	// The stamp leg, not the upsert: an upsert would have moved indexed_at.
	// This is what shows the fixture reached StampExtractorVersionBatch.
	if got := indexedAtOf(t, s, "Album/a.flac"); got != indexedBefore {
		t.Errorf("the version-stale file took the upsert (indexed_at %d → %d), so the stamp site was never exercised",
			indexedBefore, got)
	}
}

// TestAScanWhoseFinalWriteFailsStillReportsIt is the twin: both halves of
// the final flush fail on a live context, and each is reported once.
func TestAScanWhoseFinalWriteFailsStillReportsIt(t *testing.T) {
	root := t.TempDir()
	s, sc := newScanFixture(t, root)
	stageFinalFlush(t, s, sc, root)
	abortOn(t, s, `BEFORE UPDATE ON tracks`)

	rec := loggingtest.Record(t)
	scanOnce(t, sc, "failing final flush")

	mustReportOnce(t, rec, msgUpsertBatch, msgStampBatch)
}

// TestAScanStoppedInAFolderUpsertReportsNothing cancels a scan inside the
// walker's upsert of the library root's folder row.
func TestAScanStoppedInAFolderUpsertReportsNothing(t *testing.T) {
	root := t.TempDir()
	s, sc := newScanFixture(t, root)
	stageFolderUpsert(t, sc, root, root)
	parkIndex(t, s, `folders ((CAST(mtime_ns AS TEXT)) COLLATE `+sqlitetest.Collation+`)`)

	rec := loggingtest.Record(t)
	err := cancelInsideAWrite(t, func(ctx context.Context) error { _, err := sc.Scan(ctx); return err })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a scan stopped in its walk returned %v, want context.Canceled", err)
	}
	mustNotReport(t, rec, msgUpsertFolder)
}

// TestAScanWhoseFolderUpsertFailsStillReportsIt is the twin.
func TestAScanWhoseFolderUpsertFailsStillReportsIt(t *testing.T) {
	root := t.TempDir()
	s, sc := newScanFixture(t, root)
	stageFolderUpsert(t, sc, root, root)
	abortOn(t, s, `BEFORE UPDATE ON folders`)

	rec := loggingtest.Record(t)
	scanOnce(t, sc, "failing folder upsert")

	if got := rec.Failures(msgUpsertFolder); len(got) == 0 {
		t.Errorf("a folder upsert that failed on a live context was not reported")
	}
}

// TestAScanStoppedWhileResettingAMissingCountReportsNothing cancels a scan
// inside a worker's reset of an unchanged file's missing_count, the one
// write the skip gate makes.
//
// It asserts nothing about the row. The reset is one autocommit statement,
// and modernc answers ctx.Err() from any statement its interrupt goroutine
// fired on, including one that got past SQLite's last interrupt check and
// committed: this test's first draft asserted the count was untouched and
// found it reset. The passes in a transaction roll back, and their tests do
// assert it.
func TestAScanStoppedWhileResettingAMissingCountReportsNothing(t *testing.T) {
	root := t.TempDir()
	s, sc := newScanFixture(t, root)
	stageMissingCountReset(t, s, sc, root)
	parkIndex(t, s, `tracks ((CAST(missing_count AS TEXT)) COLLATE `+sqlitetest.Collation+`) WHERE missing_count > 0`)

	rec := loggingtest.Record(t)
	_ = cancelInsideAWrite(t, func(ctx context.Context) error { _, err := sc.Scan(ctx); return err })

	mustNotReport(t, rec, msgResetMissingCount)
}

// TestAScanWhoseMissingCountResetFailsStillReportsIt is the twin.
func TestAScanWhoseMissingCountResetFailsStillReportsIt(t *testing.T) {
	root := t.TempDir()
	s, sc := newScanFixture(t, root)
	stageMissingCountReset(t, s, sc, root)
	abortOn(t, s, `BEFORE UPDATE OF missing_count ON tracks WHEN NEW.missing_count = 0`)

	rec := loggingtest.Record(t)
	scanOnce(t, sc, "failing missing_count reset")

	mustReportOnce(t, rec, msgResetMissingCount)
}

// TestASACDRetireStoppedByShutdownReportsNothing cancels a scan inside the
// journaled retire of a re-ripped SACD image's trailing virtual row.
func TestASACDRetireStoppedByShutdownReportsNothing(t *testing.T) {
	root, s, sc := sacdScanFixture(t)
	stageSACDShrink(t, s, sc, root)
	parkIndex(t, s, `tracks ((CAST(missing_count AS TEXT)) COLLATE `+sqlitetest.Collation+`) WHERE missing_count > 0`)

	rec := loggingtest.Record(t)
	_ = cancelInsideAWrite(t, func(ctx context.Context) error { _, err := sc.Scan(ctx); return err })

	mustNotReport(t, rec, msgSACDRetire)
	mustIndexed(t, s, "Music/Album.iso/st/02.dff")
}

// TestASACDRetireThatFailsIsStillReported is the twin.
func TestASACDRetireThatFailsIsStillReported(t *testing.T) {
	root, s, sc := sacdScanFixture(t)
	stageSACDShrink(t, s, sc, root)
	abortOn(t, s, `BEFORE UPDATE OF missing_count ON tracks WHEN NEW.missing_count > 0`)

	rec := loggingtest.Record(t)
	scanOnce(t, sc, "failing SACD retire")

	mustReportOnce(t, rec, msgSACDRetire)
}

// TestASubtreeScanStoppedInItsDeletionPassReportsNothing is the deletion-pass
// test through ScanSubtree. Its restamp gate opens on a failed tracks pass,
// a stopped one included, so the duplicate-stamping pass then runs on the
// cancelled context too, and is not reported either.
func TestASubtreeScanStoppedInItsDeletionPassReportsNothing(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	s, sc := newScanFixture(t, root)
	stageDeletionPass(t, s, sc, root, album)
	parkIndex(t, s, `tracks ((CAST(missing_count AS TEXT)) COLLATE `+sqlitetest.Collation+`) WHERE missing_count > 0`)

	rec := loggingtest.Record(t)
	_ = cancelInsideAWrite(t, func(ctx context.Context) error { _, err := sc.ScanSubtree(ctx, album); return err })

	mustNotReport(t, rec, msgSubtreeTracks, msgSubtreeRename, msgSubtreeFolders, msgDupeStamping)
	mustHaveMissingCount(t, s, "tracks", "Album/b.flac", 0)
	mustHaveMissingCount(t, s, "folders", "Album/Gone", 0)
	mustIndexed(t, s, "Album/Old.flac")
}

// TestASubtreeScanWhoseDeletionPassFailsStillReportsIt is the twin.
func TestASubtreeScanWhoseDeletionPassFailsStillReportsIt(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	s, sc := newScanFixture(t, root)
	stageDeletionPass(t, s, sc, root, album)
	abortOn(t, s, `BEFORE UPDATE OF missing_count ON tracks WHEN NEW.missing_count > 0`)
	abortOn(t, s, `BEFORE DELETE ON tracks`)
	abortOn(t, s, `BEFORE UPDATE OF missing_count ON folders WHEN NEW.missing_count > 0`)

	rec := loggingtest.Record(t)
	if _, err := sc.ScanSubtree(context.Background(), album); err != nil {
		t.Fatalf("ScanSubtree: %v", err)
	}

	mustReportOnce(t, rec, msgSubtreeTracks, msgSubtreeRename, msgSubtreeFolders)
}

// TestASubtreeScanStoppedInAFolderUpsertReportsNothing cancels a subtree
// scan inside the upsert of its own folder row.
func TestASubtreeScanStoppedInAFolderUpsertReportsNothing(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	s, sc := newScanFixture(t, root)
	stageFolderUpsert(t, sc, root, album)
	parkIndex(t, s, `folders ((CAST(mtime_ns AS TEXT)) COLLATE `+sqlitetest.Collation+`)`)

	rec := loggingtest.Record(t)
	err := cancelInsideAWrite(t, func(ctx context.Context) error { _, err := sc.ScanSubtree(ctx, album); return err })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a subtree scan stopped in its walk returned %v, want context.Canceled", err)
	}
	mustNotReport(t, rec, msgSubtreeFolder)
}

// TestASubtreeScanWhoseFolderUpsertFailsStillReportsIt is the twin.
func TestASubtreeScanWhoseFolderUpsertFailsStillReportsIt(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	s, sc := newScanFixture(t, root)
	stageFolderUpsert(t, sc, root, album)
	abortOn(t, s, `BEFORE UPDATE ON folders`)

	rec := loggingtest.Record(t)
	if _, err := sc.ScanSubtree(context.Background(), album); err != nil {
		t.Fatalf("ScanSubtree: %v", err)
	}
	if got := rec.Failures(msgSubtreeFolder); len(got) == 0 {
		t.Errorf("a subtree folder upsert that failed on a live context was not reported")
	}
}

// TestDuplicateStampingStoppedByShutdownReportsNothing drives the tail both
// scans share, restampDuplicatesNonFatal, and cancels it at the hook between
// the election and the commit. With stamps to write, the commit fails with
// the cancellation; with none (a library already stamped), the summary write
// behind it does. Neither is reported.
func TestDuplicateStampingStoppedByShutdownReportsNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stamped bool // a pass has already written this library's stamps
		msg     string
	}{
		{"stamps to write", false, msgDupeStamping},
		{"nothing to write, so the summary is next", true, msgDupeSummary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			mode := dupes.FilterHighestQuality
			sc := dupeScanner(t, s, &mode)
			seedDupePair(t, s)
			if tc.stamped {
				if _, err := sc.RestampDuplicates(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			beforeApplyDupeStampsHookForTests = cancel
			t.Cleanup(func() { beforeApplyDupeStampsHookForTests = nil })

			rec := loggingtest.Record(t)
			sc.restampDuplicatesNonFatal(ctx)

			mustNotReport(t, rec, msgDupeStamping, msgDupeSummary)
			if !tc.stamped {
				if st := stampOf(t, s, "CopyA/Album/01 Song.flac"); st.GroupID != "" {
					t.Errorf("the stopped pass wrote stamps anyway: %+v", st)
				}
			}
		})
	}
}

// TestDuplicateStampingThatFailsIsStillReported is the twin, one failing
// write per case.
func TestDuplicateStampingThatFailsIsStillReported(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stamped bool
		abort   string
		msg     string
	}{
		{"the stamps", false, `BEFORE UPDATE OF dupe_group_id ON tracks`, msgDupeStamping},
		{"the summary", true, `BEFORE UPDATE ON scan_state`, msgDupeSummary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			mode := dupes.FilterHighestQuality
			sc := dupeScanner(t, s, &mode)
			seedDupePair(t, s)
			if tc.stamped {
				if _, err := sc.RestampDuplicates(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			abortOn(t, s, tc.abort)

			rec := loggingtest.Record(t)
			sc.restampDuplicatesNonFatal(context.Background())

			mustReportOnce(t, rec, tc.msg)
		})
	}
}

// TestTheWaveformListingStoppedByShutdownReportsNothing covers the one store
// log a scan reaches on its own: the best-effort waveform listing inside the
// deletion passes. A listing whose query is cancelled, and one whose rows are
// closed by the cancel part-way, report nothing; a genuine failure on a live
// context is still reported.
func TestTheWaveformListingStoppedByShutdownReportsNothing(t *testing.T) {
	s := openTestStore(t)
	seedWaveformRow(t, s, "Album/a.flac")

	t.Run("query", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec := loggingtest.Record(t)
		s.listWaveformSidecars(ctx, "source_path = ?", "Album/a.flac")
		mustNotReport(t, rec, msgListWaveforms, msgIterWaveforms)
	})
	t.Run("rows", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rec := loggingtest.Record(t)
		got := listWaveformSidecarsQ(ctx, closedByCancel{q: s.db, cancel: cancel}, "source_path = ?", "Album/a.flac")
		if len(got) != 0 {
			t.Fatalf("the listing read %v through rows the cancel had closed", got)
		}
		mustNotReport(t, rec, msgListWaveforms, msgIterWaveforms)
	})
	t.Run("a genuine failure", func(t *testing.T) {
		closed := openTestStore(t)
		_ = closed.Close()
		rec := loggingtest.Record(t)
		closed.listWaveformSidecars(context.Background(), "source_path = ?", "Album/a.flac")
		mustReportOnce(t, rec, msgListWaveforms)
	})
}

// TestAnEmptyRootAuditStoppedByShutdownReportsNothing: the full scan audits
// a root whose walk saw nothing with a COUNT, and a count the cancel stopped
// is not reported. It still spares the root: a scan that could not audit
// must not delete (the scan then stops before its deletion pass anyway).
func TestAnEmptyRootAuditStoppedByShutdownReportsNothing(t *testing.T) {
	root := t.TempDir()
	s, sc := newScanFixture(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := loggingtest.Record(t)
	if !sc.emptyRootMustBeSpared(ctx, root, false) {
		t.Error("a root whose audit was stopped was not spared")
	}
	mustNotReport(t, rec, msgEmptyRootCount)

	_ = s.Close()
	if !sc.emptyRootMustBeSpared(context.Background(), root, false) {
		t.Error("a root whose audit failed was not spared")
	}
	mustReportOnce(t, rec, msgEmptyRootCount)
}

// TestASubtreeMissAuditStoppedByShutdownReportsNothing: ScanSubtree audits
// the owning root of a subtree that is not there, and when that root is
// empty on disk the audit COUNTs its rows. An audit the cancel stopped is
// not reported, and still aborts the walk (an error, never nil). The
// control is the audit's genuine verdict: an empty root the DB has rows
// for is a suspected mount drop, and is reported.
func TestASubtreeMissAuditStoppedByShutdownReportsNothing(t *testing.T) {
	root := t.TempDir()
	s, sc := newScanFixture(t, root)
	if err := s.UpsertTrack(context.Background(), &Track{Path: "Album/a.flac", Size: 1, ModTime: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(root, "Album")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := loggingtest.Record(t)
	if err := sc.auditSubtreeMiss(ctx, gone, root, false); err == nil {
		t.Error("a stopped audit returned nil, which would let the deletion pass run on an unaudited root")
	}
	mustNotReport(t, rec, msgSubtreeAudit)

	if err := sc.auditSubtreeMiss(context.Background(), gone, root, false); err == nil {
		t.Error("an empty root with rows in the DB passed the audit")
	}
	mustReportOnce(t, rec, msgSubtreeAudit)
}

// cancelInsideAWrite runs run on a context it cancels while one of run's
// writes is parked inside its statement, lets the statement go, and returns
// what run returned. A test calls it after creating its park index.
func cancelInsideAWrite(t *testing.T, run func(ctx context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// Registered before Arm, so it runs AFTER the park is disarmed: a run a
	// failed assertion left parked is let go first, then joined, and only
	// then does the store's cleanup close it.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the parked run did not finish within 10s of its cancel")
		}
	})
	park := sqlitetest.Arm(t)
	var err error
	go func() {
		defer close(done)
		err = run(ctx)
	}()
	park.Wait(t)
	cancel()
	park.ReleaseUntil(t, done)
	<-done
	// So whatever the test runs next, such as a second scan, writes
	// straight through.
	park.Disarm()
	return err
}

// parkIndex creates an index under sqlitetest's collation. spec is
// everything after ON: the table, the ordered expression and any WHERE.
func parkIndex(t *testing.T, s *Store, spec string) {
	t.Helper()
	if _, err := s.db.Exec(`CREATE INDEX test_park ON ` + spec); err != nil {
		t.Fatalf("create park index: %v", err)
	}
}

// abortOn makes every write matching spec fail, on a live context, with a
// trigger that raises. spec is the trigger's timing and event.
func abortOn(t *testing.T, s *Store, spec string) {
	t.Helper()
	name := fmt.Sprintf("test_abort_%d", time.Now().UnixNano())
	if _, err := s.db.Exec(`CREATE TRIGGER ` + name + ` ` + spec + ` BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatalf("create trigger %q: %v", spec, err)
	}
}

// stageDeletionPass leaves the library in the shape a deletion pass has
// work in: under scope (the root, or a subtree of it), a track that is gone,
// a track renamed in case only, and a folder that is gone with its track.
func stageDeletionPass(t *testing.T, s *Store, sc *Scanner, root, scope string) {
	t.Helper()
	album := filepath.Join(root, "Album")
	gone := filepath.Join(scope, "Gone")
	for _, p := range []string{
		filepath.Join(album, "a.flac"), filepath.Join(album, "b.flac"),
		filepath.Join(album, "Old.flac"), filepath.Join(gone, "c.flac"),
	} {
		writeCancelFixtureFLAC(t, p, "Title")
	}
	scanOnce(t, sc, "the first scan")
	mustIndexed(t, s, "Album/b.flac", "Album/Old.flac")

	if err := os.Remove(filepath.Join(album, "b.flac")); err != nil {
		t.Fatal(err)
	}
	// Through a temporary name, because a case-insensitive filesystem
	// refuses nothing and a case-sensitive one needs the second step.
	renameCaseOnly(t, filepath.Join(album, "Old.flac"), filepath.Join(album, "OLD.flac"))
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
}

// stageAlbumTitleFix scans an album whose two tracks share a title, then
// rewrites one row's album to the folder's name, which is what the
// album-title pass repairs. The file does not change, so the next scan's
// only write is that repair.
func stageAlbumTitleFix(t *testing.T, s *Store, sc *Scanner, root string) {
	t.Helper()
	album := filepath.Join(root, "Album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.flac", "b.flac"} {
		writeMinimalFLAC(t, filepath.Join(album, name), 44100, 16, map[string]string{"ALBUM": "Real Title", "TITLE": name})
	}
	scanOnce(t, sc, "the first scan")
	if _, err := s.db.Exec(`UPDATE tracks SET tags_json = json_set(tags_json, '$.album', 'Album') WHERE path = 'Album/b.flac'`); err != nil {
		t.Fatal(err)
	}
}

// stageFinalFlush scans two files, then changes one on disk and marks the
// other's row version-stale, so the next scan's writer flushes one upsert
// and one extractor-version stamp.
func stageFinalFlush(t *testing.T, s *Store, sc *Scanner, root string) {
	t.Helper()
	album := filepath.Join(root, "Album")
	writeCancelFixtureFLAC(t, filepath.Join(album, "a.flac"), "Stable")
	writeCancelFixtureFLAC(t, filepath.Join(album, "b.flac"), "Before")
	scanOnce(t, sc, "the first scan")

	writeCancelFixtureFLAC(t, filepath.Join(album, "b.flac"), "After")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(album, "b.flac"), later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE tracks SET extractor_version = 0 WHERE path = 'Album/a.flac'`); err != nil {
		t.Fatal(err)
	}
}

// stageFolderUpsert scans a library with scope's folder in it, then changes
// scope's mtime, so the next scan's first write is scope's folder upsert.
func stageFolderUpsert(t *testing.T, sc *Scanner, root, scope string) {
	t.Helper()
	writeCancelFixtureFLAC(t, filepath.Join(root, "Album", "a.flac"), "Title")
	scanOnce(t, sc, "the first scan")
	if err := os.WriteFile(filepath.Join(scope, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(scope, later, later); err != nil {
		t.Fatal(err)
	}
}

// stageMissingCountReset scans one file, then marks its row as missed by an
// earlier scan, beside a second row with no file, so the next scan's first
// write is the skip gate's reset of the first.
func stageMissingCountReset(t *testing.T, s *Store, sc *Scanner, root string) {
	t.Helper()
	writeCancelFixtureFLAC(t, filepath.Join(root, "Album", "a.flac"), "Title")
	scanOnce(t, sc, "the first scan")
	if err := s.UpsertTrack(context.Background(), &Track{Path: "Elsewhere/ghost.flac", Size: 1, ModTime: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE tracks SET missing_count = 1 WHERE path IN ('Album/a.flac', 'Elsewhere/ghost.flac')`); err != nil {
		t.Fatal(err)
	}
}

// stageSACDShrink scans a two-track SACD image, then re-rips it with one
// track and a newer mtime, beside a row with no file already missed once,
// so the next scan's first write is the retire of the image's trailing row.
func stageSACDShrink(t *testing.T, s *Store, sc *Scanner, root string) {
	t.Helper()
	writeSACDFixture(t, filepath.Join(root, "Music"), "Album.iso", twoFixtureTracks(), sacdFixtureOptions{})
	scanOnce(t, sc, "the first scan")
	if err := s.UpsertTrack(context.Background(), &Track{Path: "Elsewhere/ghost.flac", Size: 1, ModTime: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE tracks SET missing_count = 1 WHERE path = 'Elsewhere/ghost.flac'`); err != nil {
		t.Fatal(err)
	}
	iso := filepath.Join(root, "Music", "Album.iso")
	if err := os.WriteFile(iso, buildSACDImage(t,
		[]sacdFixtureTrack{{startFrame: 0, duration: 150, title: "Only One"}},
		sacdFixtureOptions{}), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(iso, later, later); err != nil {
		t.Fatal(err)
	}
}

// writeCancelFixtureFLAC writes a real FLAC, so extraction succeeds and the
// scan writes what it read.
func writeCancelFixtureFLAC(t *testing.T, path, title string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalFLAC(t, path, 44100, 16, map[string]string{"TITLE": title, "ALBUM": "Album", "ARTIST": "Artist"})
}

// renameCaseOnly renames from to to, which differ in case only.
func renameCaseOnly(t *testing.T, from, to string) {
	t.Helper()
	tmp := from + ".renaming"
	if err := os.Rename(from, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, to); err != nil {
		t.Fatal(err)
	}
}

// seedWaveformRow gives path a track row and an analysis row naming a
// waveform sidecar.
func seedWaveformRow(t *testing.T, s *Store, path string) {
	t.Helper()
	if err := s.UpsertTrack(context.Background(), &Track{Path: path, Size: 1, ModTime: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO track_analysis (source_path, waveform_path, source_mtime_ns, source_size, created_at)
		VALUES (?, ?, 1, 1, 1)`, path, "/tmp/"+path+".1bwf"); err != nil {
		t.Fatal(err)
	}
}

// closedByCancel runs a query, then cancels its context and waits until
// database/sql has closed the rows in response, so the caller iterates rows
// the cancel closed part-way. Columns fails once the rows are closed.
type closedByCancel struct {
	q      rowQueryer
	cancel context.CancelFunc
}

// QueryContext runs the query, cancels, and returns the rows once they are
// closed.
func (c closedByCancel) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := c.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	c.cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, cerr := rows.Columns(); cerr != nil {
			return rows, nil
		}
		if time.Now().After(deadline) {
			return rows, errors.New("closedByCancel: the cancel never closed the rows")
		}
		runtime.Gosched()
	}
}

// mustNotReport fails the test for each msg that was logged.
func mustNotReport(t *testing.T, rec *loggingtest.Recorder, msgs ...string) {
	t.Helper()
	for _, m := range msgs {
		if got := rec.Failures(m); len(got) != 0 {
			t.Errorf("a pass that shutdown stopped reported %q:\n%s", m, strings.Join(got, "\n"))
		}
	}
}

// mustReportOnce fails the test for each msg that was not logged exactly
// once.
func mustReportOnce(t *testing.T, rec *loggingtest.Recorder, msgs ...string) {
	t.Helper()
	for _, m := range msgs {
		if got := rec.Failures(m); len(got) != 1 {
			t.Errorf("a failure on a live context logged %q %d times, want 1:\n%s", m, len(got), strings.Join(got, "\n"))
		}
	}
}

// mustHaveMissingCount fails the test unless table's row at path has the
// given missing_count.
func mustHaveMissingCount(t *testing.T, s *Store, table, path string, want int) {
	t.Helper()
	var got int
	if err := s.db.QueryRow(`SELECT missing_count FROM `+table+` WHERE path = ?`, path).Scan(&got); err != nil {
		t.Fatalf("missing_count of %s %q: %v", table, path, err)
	}
	if got != want {
		t.Errorf("missing_count of %s %q = %d, want %d", table, path, got, want)
	}
}

// albumOf, titleOf and extractorVersionOf read one field of a stored row
// (indexedAtOf, in dupe_stamps_test.go, reads a fourth).
func albumOf(t *testing.T, s *Store, path string) string {
	t.Helper()
	tr, err := s.GetTrack(context.Background(), path)
	if err != nil || tr == nil {
		t.Fatalf("GetTrack(%q): %v", path, err)
	}
	return tr.Album
}

func titleOf(t *testing.T, s *Store, path string) string {
	t.Helper()
	tr, err := s.GetTrack(context.Background(), path)
	if err != nil || tr == nil {
		t.Fatalf("GetTrack(%q): %v", path, err)
	}
	return tr.Title
}

func extractorVersionOf(t *testing.T, s *Store, path string) int {
	t.Helper()
	var v int
	if err := s.db.QueryRow(`SELECT extractor_version FROM tracks WHERE path = ?`, path).Scan(&v); err != nil {
		t.Fatalf("extractor_version of %q: %v", path, err)
	}
	return v
}
