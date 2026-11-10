package backup_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

func TestAnIncompleteNoBarrierRecheckLeavesItsRecordTrusted(t *testing.T) {
	dir := verifiedSnapshot(t, storeWithTracks(t, 5))
	before := readBytes(t, filepath.Join(dir, backup.ManifestFile))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(backup.SetSyncsParentDirForTest(false))
	t.Cleanup(backup.SetOnDatabaseOpenForTest(cancel))
	t.Cleanup(func() { awaitClean(t) })
	res, err := backup.VerifySnapshot(ctx, dir, backup.VerifyOptions{})
	awaitClean(t)
	if !errors.Is(err, backup.ErrRecheckNotRecorded) || res.Found.DatabaseState != backup.StateIncomplete {
		t.Fatalf("expected a recheck interrupted at SQLite's open: %+v, %v", res, err)
	}
	if vetoed(t, dir) || !bytes.Equal(before, readBytes(t, filepath.Join(dir, backup.ManifestFile))) {
		t.Error("an incomplete recheck changed the record or made a veto")
	}
	requireVerdict(t, "interrupted recheck", listed(t, dir), backup.StateVerified, "", backup.StateVerified, "")
}
