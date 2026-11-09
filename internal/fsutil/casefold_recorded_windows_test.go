//go:build windows

package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// shortPathOf is the short (8.3) spelling Windows gives the folder at long,
// skipping the case, saying why, where it gives none: a volume with short
// names turned off answers long itself.
func shortPathOf(t *testing.T, long string) string {
	t.Helper()
	p, err := windows.UTF16PtrFromString(long)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil {
		t.Skipf("GetShortPathName(%s): %v", long, err)
	}
	short := windows.UTF16ToString(buf[:n])
	if strings.EqualFold(short, long) {
		t.Skipf("this volume gives %s no short name (8.3 names are off): the case would test nothing", long)
	}
	return short
}

// TestARecordedPathComparesThroughAShortName: on Windows a folder has a short
// (8.3) spelling beside its long one, and a resolution names it by the long
// one, so a path recorded by the short spelling and compared by the long one
// (and the reverse) is one path where the folder the leaf is looked up in was
// recorded as folding case and two where it was recorded as keeping case,
// the short name and the long one never two folders (B741's round 4: the
// comparison set the recorded spelling, short name and all, against the
// resolved one, element by element, and read the two names as two folders,
// as it did wherever a temporary directory is spelled with a short name).
func TestARecordedPathComparesThroughAShortName(t *testing.T) {
	long := filepath.Join(t.TempDir(), "A Folder With A Long Name")
	mnt := filepath.Join(long, "usb")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	short := shortPathOf(t, long)
	shortMnt := filepath.Join(short, "usb")
	for _, tc := range []struct {
		name          string
		recorded, now string
		folds         string // the folder whose letter decides, as recorded spells it
	}{
		{"recorded by the short name, compared by the long", filepath.Join(shortMnt, "Bridge"), filepath.Join(mnt, "bridge"), shortMnt},
		{"recorded by the long name, compared by the short", filepath.Join(mnt, "Bridge"), filepath.Join(shortMnt, "bridge"), mnt},
	} {
		for _, letter := range []CaseFolding{CaseInsensitive, CaseSensitive} {
			t.Run(tc.name+", "+letter.String(), func(t *testing.T) {
				folding := FoldingOnTheWay(tc.recorded, lettersOf(map[string]CaseFolding{tc.folds: letter}), map[string]CaseFolding{})
				if got, want := SamePathAsRecorded(tc.recorded, tc.now, folding), letter == CaseInsensitive; got != want {
					t.Errorf("SamePathAsRecorded(%q, %q, %s) = %v, want %v", tc.recorded, tc.now, FormatFolding(folding), got, want)
				}
			})
		}
	}
}
