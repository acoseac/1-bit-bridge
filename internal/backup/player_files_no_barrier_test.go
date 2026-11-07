package backup_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// TestARestoreWhereNoDirectoryCanBeSyncedRefusesThePlayersFilesNamingTheWay
// is backlog B745. A snapshot a Linux box takes once its player has run holds
// the player's identity (player-uuid and its reservation, player-ids.json)
// and its DSD state (player-dsd.json), each restored durably or not at all.
// Restored on Windows, where no directory can be synced (the existing seam,
// atomicwrite.SetSyncDirForTest(nil), so this runs natively), it refuses: the
// DSD state as the identity does (the owner's decision, kept). The refusal
// comes before anything is written and names every one of those files the
// snapshot holds and the way through, removing them from a copy of the
// snapshot, which then restores the rest and leaves the box's own player
// files as they are.
func TestARestoreWhereNoDirectoryCanBeSyncedRefusesThePlayersFilesNamingTheWay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		uuid     bool
		dsd      bool
		theFiles []string
	}{
		{"the identity", true, false, []string{backup.PlayerUUIDFileName, "player-ids.json"}},
		{"the DSD state", false, true, []string{backup.PlayerDSDFileName}},
		{"the identity and the DSD state", true, true, []string{backup.PlayerUUIDFileName, "player-ids.json", backup.PlayerDSDFileName}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The snapshot is a Linux box's: taken where directories sync.
			restoreBarrier := atomicwrite.SetSyncDirForTest(func(string) error { return nil })
			t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
			live := t.TempDir()
			src := primeLiveState(t, live)
			if tc.uuid {
				withPlayerUUID(t, &src, snapshotPlayerUUID+"\n", 0o600)
			}
			if tc.dsd {
				src.PlayerDSD = filepath.Join(live, backup.PlayerDSDFileName)
				if err := os.WriteFile(src.PlayerDSD, []byte(aDSDState), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			snap, err := backup.Snapshot(t.Context(), src)
			if err != nil {
				t.Fatal(err)
			}
			restoreBarrier()

			// Restored where no directory can be synced, as on Windows.
			t.Cleanup(atomicwrite.SetSyncDirForTest(nil))
			box := t.TempDir()
			targets := backup.Targets{
				TokensJSON: filepath.Join(box, "tokens.json"),
				PlayerUUID: filepath.Join(box, backup.PlayerUUIDFileName),
				PlayerDSD:  filepath.Join(box, backup.PlayerDSDFileName),
			}
			err = backup.Restore(snap, targets)
			if !errors.Is(err, atomicwrite.ErrNoDirectoryBarrier) {
				t.Fatalf("the restore = %v, want its refusal where no directory can be synced", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "nothing was restored") {
				t.Errorf("the refusal does not say nothing was restored: %q", msg)
			}
			_, way, ok := strings.Cut(msg, "remove ")
			named, _, inCopy := strings.Cut(way, " from a copy of the snapshot")
			if !ok || !inCopy {
				t.Fatalf("the refusal names no way through, removing the files from a copy of the snapshot: %q", msg)
			}
			for _, name := range tc.theFiles {
				if !strings.Contains(named, name) {
					t.Errorf("the way through does not name %s: %q", name, msg)
				}
			}
			if entries, _ := os.ReadDir(box); len(entries) != 0 {
				t.Fatalf("the refused restore wrote %v", entryNames(entries))
			}

			// The way through: a copy of the snapshot without those files
			// restores the rest, and the box keeps its own player files.
			copied := copySnapshotWithout(t, snap, tc.theFiles)
			if err := backup.Restore(copied, targets); err != nil {
				t.Fatalf("the copy without %v = %v, want the rest restored", tc.theFiles, err)
			}
			if !pathExists(t, targets.TokensJSON) {
				t.Error("the copy's restore did not restore the rest")
			}
			for _, p := range []string{targets.PlayerUUID, targets.PlayerDSD} {
				if pathExists(t, p) {
					t.Errorf("the copy's restore wrote %s", filepath.Base(p))
				}
			}
		})
	}
}

// copySnapshotWithout copies the snapshot at snap into a directory of the
// test's, leaving out the files named, as an operator does who removes them
// from a copy; the manifest is copied as it is.
func copySnapshotWithout(t *testing.T, snap string, without []string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), filepath.Base(snap))
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || slices.Contains(without, e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(snap, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}
