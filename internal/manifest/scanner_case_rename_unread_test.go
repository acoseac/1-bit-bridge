package manifest

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

const (
	caseRenameOld  = "Album/01.flac"
	caseRenameNew  = "album/01.flac"
	caseRenameReap = "reaped stale rows from case-only rename"
)

// An unread case rename keeps the stored row (backlog B175). The walk
// records the new spelling, and the deletion pass used to reap the old
// spelling at once, on the premise that the new spelling was written this
// pass. A worker that cannot read the file writes no row, so both
// spellings were gone while the unread line said the rows were kept. The
// old row is counted missing instead, and the new spelling has no row
// until a scan reads it.
func TestAnUnreadCaseRenameKeepsTheOldRow(t *testing.T) {
	t.Run("scan", func(t *testing.T) { assertCaseRenameUnread(t, false) })
	t.Run("subtree", func(t *testing.T) { assertCaseRenameUnread(t, true) })
}

// A readable case rename still reaps the old row at once. The new
// spelling is written this pass, so the stored spelling must not shadow
// it for the missing-count grace.
func TestAReadableCaseRenameReapsTheOldRowAtOnce(t *testing.T) {
	t.Run("scan", func(t *testing.T) { assertCaseRenameRead(t, false) })
	t.Run("subtree", func(t *testing.T) { assertCaseRenameRead(t, true) })
}

func assertCaseRenameUnread(t *testing.T, subtree bool) {
	t.Helper()
	root, store, sc := caseRenameFixture(t)
	injectAudioFault(t, sc, filepath.Join(root, "album", "01.flac"), "", audioFault{open: syscall.EIO})
	rec := rescanCaseRename(t, sc, root, subtree)
	requireTrack(t, store, caseRenameOld, true)
	requireTrack(t, store, caseRenameNew, false)
	row, ok := storedRowAt(t, store, caseRenameOld)
	if !ok || row.missingCount != 1 {
		t.Fatalf("old row missing_count = %d present=%v, want 1", row.missingCount, ok)
	}
	requireOneUnreadLine(t, rec, 1, caseRenameNew, "open")
	if lines := rec.Lines(caseRenameReap); len(lines) != 0 {
		t.Fatalf("reaped an unread rename: %v", lines)
	}
}

func assertCaseRenameRead(t *testing.T, subtree bool) {
	t.Helper()
	root, store, sc := caseRenameFixture(t)
	rec := rescanCaseRename(t, sc, root, subtree)
	requireTrack(t, store, caseRenameOld, false)
	requireTrack(t, store, caseRenameNew, true)
	if lines := rec.Lines(caseRenameReap); len(lines) != 1 {
		t.Fatalf("reap lines %d, want 1", len(lines))
	}
}

func caseRenameFixture(t *testing.T) (string, *Store, *Scanner) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "Album")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalFLAC(t, filepath.Join(dir, "01.flac"), 44100, 16, map[string]string{"TITLE": "One"})
	store, sc := newScanFixture(t, root)
	sc.SetDeleteThreshold(config.DefaultDeleteAfterMissingScans)
	scanOnce(t, sc, "index")
	mustIndexed(t, store, caseRenameOld)
	if err := os.Rename(dir, filepath.Join(root, "album")); err != nil {
		t.Fatal(err)
	}
	if got := listedName(t, root, "album"); got != "album" {
		t.Skipf("this volume stored %q, not the renamed case", got)
	}
	return root, store, sc
}

func rescanCaseRename(t *testing.T, sc *Scanner, root string, subtree bool) *loggingtest.Recorder {
	t.Helper()
	rec := loggingtest.Record(t)
	var err error
	if subtree {
		_, err = sc.ScanSubtree(context.Background(), root)
	} else {
		_, err = sc.Scan(context.Background())
	}
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func requireTrack(t *testing.T, store *Store, path string, present bool) {
	t.Helper()
	got, err := store.GetTrack(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if present && got == nil {
		t.Fatalf("GetTrack %s: no row", path)
	}
	if !present && got != nil {
		t.Fatalf("GetTrack %s: row present", path)
	}
}
