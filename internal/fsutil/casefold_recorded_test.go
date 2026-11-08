package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Backlog B741's gap 3: a data dir's record and its config can spell one data
// dir in two cases on a volume that folds case, and once that volume is away
// every folder below its mount point is gone with it, so a probe of what is
// there (SamePath's caseInsensitiveFS) answers for the folder left at the
// mount point, on the volume above. The record keeps how each folder compared
// names while it was there (FoldingOnTheWay), and the comparison reads that,
// probing nothing (SameAsRecorded, SamePathAsRecorded).

// volumeRoot is the root of the volume this host's temporary directory is on:
// "/", or a drive's `C:\`, so that a test's paths are absolute everywhere.
func volumeRoot() string {
	return filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
}

// TestARecordedFoldingDecidesACaseOnlyDifference: two absolute spellings that
// differ in case alone are one path where the folder the differing name is
// looked up in was recorded as folding case, or as unknown, or not recorded
// at all, and two where it was recorded as keeping case, the case-sensitive
// sibling; a difference that is not of case alone, a path of another length,
// and a relative path compared but as written are never one.
func TestARecordedFoldingDecidesACaseOnlyDifference(t *testing.T) {
	root := volumeRoot()
	mnt := filepath.Join(root, "mnt", "usb")
	recorded := filepath.Join(mnt, "Bridge")
	for _, tc := range []struct {
		name    string
		now     string
		folding map[string]CaseFolding
		same    bool
	}{
		{"the same spelling", recorded, map[string]CaseFolding{mnt: CaseSensitive}, true},
		{"another case in a folder recorded as folding case", filepath.Join(mnt, "bridge"),
			map[string]CaseFolding{mnt: CaseInsensitive}, true},
		{"another case in a folder recorded as keeping case", filepath.Join(mnt, "bridge"),
			map[string]CaseFolding{mnt: CaseSensitive}, false},
		{"another case in a folder recorded as unknown", filepath.Join(mnt, "bridge"),
			map[string]CaseFolding{mnt: CaseUnknown}, true},
		{"another case in a folder the record says nothing of", filepath.Join(mnt, "bridge"),
			map[string]CaseFolding{filepath.Join(root, "mnt"): CaseSensitive}, true},
		{"another case above, in a folder recorded as keeping case", filepath.Join(root, "MNT", "usb", "Bridge"),
			map[string]CaseFolding{root: CaseSensitive, mnt: CaseInsensitive}, false},
		{"another case above, in a folder recorded as folding case", filepath.Join(root, "MNT", "usb", "Bridge"),
			map[string]CaseFolding{root: CaseInsensitive, mnt: CaseSensitive}, true},
		{"another name", filepath.Join(mnt, "Bridges"), map[string]CaseFolding{mnt: CaseInsensitive}, false},
		{"a path below it", filepath.Join(mnt, "Bridge", "x"), map[string]CaseFolding{mnt: CaseInsensitive}, false},
		{"a path above it", mnt, map[string]CaseFolding{mnt: CaseInsensitive}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SameAsRecorded(recorded, tc.now, tc.folding); got != tc.same {
				t.Errorf("SameAsRecorded(%q, %q, %v) = %v, want %v", recorded, tc.now, tc.folding, got, tc.same)
			}
		})
	}
	everyFolds := map[string]CaseFolding{".": CaseInsensitive, "usb": CaseInsensitive}
	if SameAsRecorded(filepath.Join("usb", "Bridge"), filepath.Join("usb", "bridge"), everyFolds) {
		t.Error("two relative spellings in another case read as one path")
	}
	if !SameAsRecorded(filepath.Join("usb", "bridge"), filepath.Join("usb", ".", "bridge"), nil) {
		t.Error("a relative path read as another than itself")
	}
	if runtime.GOOS == "windows" {
		lower := strings.ToLower(root[:1]) + root[1:]
		if !SameAsRecorded(filepath.Join(root, "x"), filepath.Join(lower, "x"), map[string]CaseFolding{}) {
			t.Errorf("a drive spelled in another case read as another volume")
		}
	}
}

// TestFoldingOnTheWayAsksEachFolderOnce: FoldingOnTheWay asks the probe of
// each folder from the path's own up to its volume's root, keyed as
// filepath.Dir spells it, and none it holds already; what it records is what
// SameAsRecorded compares by.
func TestFoldingOnTheWayAsksEachFolderOnce(t *testing.T) {
	root := volumeRoot()
	mnt := filepath.Join(root, "mnt", "usb")
	var asked []string
	probe := func(dir string) CaseFolding {
		asked = append(asked, dir)
		if dir == mnt {
			return CaseInsensitive
		}
		return CaseSensitive
	}
	folding := map[string]CaseFolding{}
	FoldingOnTheWay(filepath.Join(mnt, "Bridge"), probe, folding)
	if want := []string{mnt, filepath.Join(root, "mnt"), root}; !slices.Equal(asked, want) {
		t.Errorf("asked %q, want %q", asked, want)
	}
	asked = nil
	FoldingOnTheWay(filepath.Join(mnt, "other", "x"), probe, folding)
	if want := []string{filepath.Join(mnt, "other")}; !slices.Equal(asked, want) {
		t.Errorf("a second path asked %q, want %q, the folder not recorded yet", asked, want)
	}
	if !SameAsRecorded(filepath.Join(mnt, "Bridge"), filepath.Join(mnt, "BRIDGE"), folding) {
		t.Error("two spellings in a folder recorded as folding case read as two paths")
	}
	if SameAsRecorded(filepath.Join(mnt, "Bridge"), filepath.Join(root, "MNT", "usb", "Bridge"), folding) {
		t.Error("two spellings in a folder recorded as keeping case read as one path")
	}
}

// TestSamePathAsRecordedProbesNoVolume: a data dir's folder gone, as with its
// volume away, and the folder at the mount point there: a spelling in another
// case below it is one path where the record says the volume folded case,
// and two where it says the volume kept case, whatever a probe of this host's
// folder would answer; a spelling through a link that resolves now is
// compared as it resolves.
func TestSamePathAsRecordedProbesNoVolume(t *testing.T) {
	base := t.TempDir()
	mnt := filepath.Join(base, "usb")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	recorded, now := filepath.Join(mnt, "Bridge"), filepath.Join(mnt, "bridge")
	folding := map[string]CaseFolding{}
	FoldingOnTheWay(recorded, func(dir string) CaseFolding {
		if dir == mnt {
			return CaseInsensitive
		}
		return CaseSensitive
	}, folding)
	if !SamePathAsRecorded(recorded, now, folding) {
		t.Errorf("SamePathAsRecorded(%q, %q) = false where the record says its folder folded case", recorded, now)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(mnt, link); err == nil {
		if through := filepath.Join(link, "bridge"); !SamePathAsRecorded(recorded, through, folding) {
			t.Errorf("SamePathAsRecorded(%q, %q) = false: a link to the mount point resolves now", recorded, through)
		}
	}
	folding[mnt] = CaseSensitive
	if SamePathAsRecorded(recorded, now, folding) {
		t.Errorf("SamePathAsRecorded(%q, %q) = true where the record says its folder kept case", recorded, now)
	}
	if !SamePathAsRecorded(recorded, recorded, folding) {
		t.Errorf("SamePathAsRecorded(%q, itself) = false", recorded)
	}
}

// TestParseCaseFoldingReadsWhatStringWrites: every CaseFolding reads back
// from its name, and any other word reads as CaseUnknown.
func TestParseCaseFoldingReadsWhatStringWrites(t *testing.T) {
	for _, c := range []CaseFolding{CaseUnknown, CaseSensitive, CaseInsensitive} {
		if got := ParseCaseFolding(c.String()); got != c {
			t.Errorf("ParseCaseFolding(%q) = %v, want %v", c.String(), got, c)
		}
	}
	if got := ParseCaseFolding("folds"); got != CaseUnknown {
		t.Errorf("ParseCaseFolding of a word it does not know = %v, want unknown", got)
	}
}
