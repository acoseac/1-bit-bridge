package backup_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// quotedName is name as a quoted case names the link it gives its snapshot
// through, so that %q writes the path given otherwise than as it is:
// elsewhere than on Windows with a backslash in it, which is no separator
// there; on Windows as it is, every path's separators being written so
// already.
func quotedName(name string) string {
	if runtime.GOOS == "windows" {
		return name
	}
	return `le\` + name
}

// TestARestoreSpellsItsSourcesWordsAsGivenAndLeavesItsDestinationsAsTheyAre
// is backlog B871's items 11 and 13 through RestoreWithOptions, both ways a
// snapshot is given through a link. A read of the snapshot that fails is said
// as the restore was given the snapshot, at the operation that read it, a path
// its words quote (%q) included, below the path given with one separator
// between the two; and a write of the destination that fails is said as it
// is, even where the destination's paths begin with the snapshot's own
// spelling where it resolves. A snapshot at a volume root (E:\) and a
// destination on that volume is that case for every destination path, and no
// test can make a volume root: here the destination lies in the snapshot's
// own folder, which gives its paths the same beginning. Until then the restore
// spelled every error it answered (sourceHold.asGiven, deferred over all of
// RestoreWithOptions), so the destination's path was named through the path
// given, the quoted source path kept where it resolves, and a snapshot at a
// root was spelled not at all.
func TestARestoreSpellsItsSourcesWordsAsGivenAndLeavesItsDestinationsAsTheyAre(t *testing.T) {
	for _, sp := range spellings {
		t.Run(sp.name+", a source's quoted path", func(t *testing.T) {
			snap := snapshotOf(t, storeWithTracks(t, 5))
			name := "linked-backups"
			if sp.name != spellings[0].name {
				name = "latest"
			}
			given := sp.named(t, snap, quotedName(name))
			b := newDestinationBox(t)
			db := filepath.Join(snap, backup.ManifestDBFileName)
			t.Cleanup(backup.SetFailSourceOpenForTest(func(p string) error {
				if p == db {
					return fmt.Errorf("open %q: %w", p, fs.ErrPermission)
				}
				return nil
			}))
			err := backup.RestoreWithOptions(given, b.targets, backup.RestoreOptions{})
			if !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("the restore whose snapshot's database cannot be opened answered %v, want the open's error", err)
			}
			want, resolved := strconv.Quote(filepath.Join(given, backup.ManifestDBFileName)), strconv.Quote(db)
			if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), resolved) {
				t.Errorf("the restore says %v, want the quoted path given, %s, never where it resolves, %s", err, want, resolved)
			}
		})
		t.Run(sp.name+", a destination below the snapshot's own spelling", func(t *testing.T) {
			snap := snapshotOf(t, storeWithTracks(t, 5))
			given := sp.through(t, snap)
			dest := filepath.Join(snap, "destination")
			wal := filepath.Join(dest, backup.ManifestDBFileName+"-wal")
			if err := os.MkdirAll(wal, 0o700); err != nil {
				t.Fatal(err)
			}
			// A sidecar no removal takes: the restore's first write fails.
			if err := os.WriteFile(filepath.Join(wal, "kept"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := backup.RestoreWithOptions(given, backup.Targets{ManifestDB: filepath.Join(dest, backup.ManifestDBFileName)}, backup.RestoreOptions{})
			if err == nil || !strings.Contains(err.Error(), "remove stale sidecar") {
				t.Fatalf("the restore into a destination whose sidecar no removal takes answered %v, want its sidecar's refusal", err)
			}
			if !says(err.Error(), wal) || says(err.Error(), filepath.Join(given, "destination")) {
				t.Errorf("the restore says %v, want the destination's sidecar as it is, %s, never respelled through %s", err, wal, given)
			}
		})
	}
}
