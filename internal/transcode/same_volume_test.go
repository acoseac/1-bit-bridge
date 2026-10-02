package transcode

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSameVolumeJudgesEachDirectoryWhereTheProbeDoes: two directories on one
// volume are one volume, and a directory that does not exist yet (a variants
// directory or a render scratch nothing has written) is judged by its
// closest existing ancestor, where AvailableDiskSpaceNearest probes it. A
// pre-flight that budgets a DSD render's scratch and its rendition adds the
// two needs on one volume (backlog B264).
func TestSameVolumeJudgesEachDirectoryWhereTheProbeDoes(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, dir := range []string{a, b} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, x, y string }{
		{"two directories on one volume", a, b},
		{"a directory that does not exist yet", filepath.Join(root, "missing", "deeper"), a},
	} {
		same, err := SameVolume(tc.x, tc.y)
		if err != nil || !same {
			t.Errorf("%s: SameVolume(%q, %q) = %v, %v; want true", tc.name, tc.x, tc.y, same, err)
		}
	}
}
