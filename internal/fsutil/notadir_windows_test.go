//go:build windows

package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNotADirectoryTakesAJunctionToADirectory: a directory junction, which
// os.Lstat calls an irregular file since Go 1.23, is no entry in the way when
// it leads to a directory, as os.MkdirAll and bridge serve take it, so a data
// directory that is one, or is below one, passes bridge doctor's data-dir
// line; a junction whose target is gone is in the way, named as os.Lstat
// sees it.
func TestNotADirectoryTakesAJunctionToADirectory(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(base, "junction")
	makeJunction(t, target, junction)
	if in, err := NotADirectory(junction); in != nil || err != nil {
		t.Errorf("NotADirectory(a junction to a directory) = %+v, %v; want nothing in the way", in, err)
	}
	if nearest, blocker, err := BlockingEntry(filepath.Join(junction, "data")); err != nil || nearest != junction || blocker != nil {
		t.Errorf("BlockingEntry(below a junction to a directory) = %q, %+v, %v; want %s and nothing in the way", nearest, blocker, err, junction)
	}
	gone := filepath.Join(base, "gone")
	if err := os.Mkdir(gone, 0o700); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(base, "dangling")
	makeJunction(t, gone, dangling)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if in, err := NotADirectory(dangling); err != nil || in == nil || *in != (EntryInTheWay{Entry: dangling, Kind: "an irregular file"}) {
		t.Errorf("NotADirectory(a junction whose target is gone) = %+v, %v; want it in the way, an irregular file", in, err)
	}
}
