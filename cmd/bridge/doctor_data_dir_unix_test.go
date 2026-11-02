//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// TestTheDataDirLineIsAWarnWhereThePathCannotBeInspected: an Lstat on the
// way up the data directory's path that fails for another reason than an
// entry's absence leaves the data-dir line "not checked", a warn in both
// doctors naming the data directory and the error, never ok and never a
// fail (the brief's constraint: the service's user may read what this one
// cannot, and serve's refusal of such a path is no shape it names): a link
// cycle above the data directory, with the path's fix, and a directory this
// user cannot search, with the service user's; and a config this user cannot
// read is not checked either, as config-file warns of it.
func TestTheDataDirLineIsAWarnWhereThePathCannotBeInspected(t *testing.T) {
	const pathFix = "mend what the error names on the way to the data directory, or set dataDir in bridge.yaml to a directory serve can make or use"
	t.Run("a link cycle above the data directory", func(t *testing.T) {
		base := t.TempDir()
		loop1, loop2 := filepath.Join(base, "loop1"), filepath.Join(base, "loop2")
		linkTo(t, loop2, loop1)
		linkTo(t, loop1, loop2)
		dataDir := filepath.Join(loop1, "data")
		said := askBothDoctors(t, writeDataDirConfig(t, base, dataDir, false))
		c := said.requireLine(t, dataDirCheckSlug, doctor.Warn)
		if want := "not checked: the data directory " + dataDir + " cannot be inspected: lstat " + dataDir + ": " + syscall.ELOOP.Error(); c.Summary != want || c.Hint != pathFix {
			t.Errorf("the data-dir line says %q, hint %q; want %q, hint %q", c.Summary, c.Hint, want, pathFix)
		}
		said.requirePrinted(t, c)
	})
	t.Run("below a directory this user cannot search", func(t *testing.T) {
		skipUnlessModeBitsDeny(t, "root searches a directory of mode 0")
		base := t.TempDir()
		locked := filepath.Join(base, "locked")
		if err := os.Mkdir(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		denyDirForTest(t, locked, 0)
		dataDir := filepath.Join(locked, "data")
		said := askBothDoctors(t, writeDataDirConfig(t, base, dataDir, false))
		c := said.requireLine(t, dataDirCheckSlug, doctor.Warn)
		want := "not checked: the data directory " + dataDir + " cannot be inspected: lstat " + dataDir + ": " + syscall.EACCES.Error()
		if hint := "run bridge doctor as the user the service runs as, who can read the data directory"; c.Summary != want || c.Hint != hint {
			t.Errorf("the data-dir line says %q, hint %q; want %q, hint %q", c.Summary, c.Hint, want, hint)
		}
		said.requirePrinted(t, c)
	})
	t.Run("a config this user cannot read", func(t *testing.T) {
		skipUnlessModeBitsDeny(t, "root reads a config of mode 0")
		base := t.TempDir()
		path := writeDataDirConfig(t, base, filepath.Join(base, "data"), false)
		chmodForTest(t, path, 0)
		said := askBothDoctors(t, path)
		c := said.requireLine(t, dataDirCheckSlug, doctor.Warn)
		if want := "not checked: the config that sets the data directory is not readable by this user"; c.Summary != want || c.Hint != notLoadedHint {
			t.Errorf("the data-dir line says %q, hint %q; want %q, hint %q", c.Summary, c.Hint, want, notLoadedHint)
		}
	})
}

// TestThePlayersStorageLineSaysWhatTheDataDirLineSays runs both doctors over a
// config that asks for the player, on a stand-in Linux box with the real
// inspector, for each shape of a data directory serve refuses
// (unusableDataDirs): the player-storage line fails as the data-dir line does,
// in the same words and with the same fix, since both judge the path's
// shapes from one walk up it (fsutil's, which serve's refusal reads too). An
// ancestor that is a regular file, or a link to one, which the medium check
// answers with a bare ENOTDIR that names no entry, is a fail with them: the
// player-storage line called it "not checked: … cannot be inspected: not a
// directory", a warn, until B659 (the owner's decision: serve refuses it).
func TestThePlayersStorageLineSaysWhatTheDataDirLineSays(t *testing.T) {
	prevGOOS := playerStorageGOOS
	playerStorageGOOS = "linux"
	t.Cleanup(func() { playerStorageGOOS = prevGOOS })
	for _, shape := range unusableDataDirs {
		t.Run(shape.name, func(t *testing.T) {
			base := t.TempDir()
			dataDir, why := shape.lay(t, base)
			said := askBothDoctors(t, writeDataDirConfig(t, base, dataDir, true))
			storage := said.requireLine(t, playerStorageCheckName, doctor.Fail)
			dataDirLine := said.requireLine(t, dataDirCheckSlug, doctor.Fail)
			switch {
			case storage.Summary != dataDirLine.Summary || storage.Hint != dataDirLine.Hint:
				t.Errorf("the player-storage line says %q, hint %q; the data-dir line %q, hint %q", storage.Summary, storage.Hint, dataDirLine.Summary, dataDirLine.Hint)
			case !strings.Contains(storage.Summary, "bridge serve will not start: ") || !strings.Contains(storage.Summary, why):
				t.Errorf("the player-storage line says %q, want bridge serve will not start, and %q", storage.Summary, why)
			}
		})
	}
}
