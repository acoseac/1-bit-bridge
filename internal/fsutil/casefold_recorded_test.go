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
// looked up in was recorded as folding case, or as unknown, or where the
// folding recorded is not one of this path's folders (none, or one of a path
// of another depth), and two where it was recorded as keeping case, the
// case-sensitive sibling; a difference that is not of case alone, a path of
// another length, and a relative path compared but as written are never one.
func TestARecordedFoldingDecidesACaseOnlyDifference(t *testing.T) {
	root := volumeRoot()
	mnt := filepath.Join(root, "mnt", "usb")
	recorded := filepath.Join(mnt, "Bridge")
	// The folding of recorded's three folders, root first: root, root/mnt
	// and root/mnt/usb, where Bridge is looked up.
	of := func(root, mntDir, usb CaseFolding) []CaseFolding { return []CaseFolding{root, mntDir, usb} }
	for _, tc := range []struct {
		name    string
		now     string
		folding []CaseFolding
		same    bool
	}{
		{"the same spelling", recorded, of(CaseSensitive, CaseSensitive, CaseSensitive), true},
		{"another case in a folder recorded as folding case", filepath.Join(mnt, "bridge"),
			of(CaseSensitive, CaseSensitive, CaseInsensitive), true},
		{"another case in a folder recorded as keeping case", filepath.Join(mnt, "bridge"),
			of(CaseInsensitive, CaseInsensitive, CaseSensitive), false},
		{"another case in a folder recorded as unknown", filepath.Join(mnt, "bridge"),
			of(CaseSensitive, CaseSensitive, CaseUnknown), true},
		{"another case where nothing is recorded", filepath.Join(mnt, "bridge"), nil, true},
		{"another case by the folding of a shorter path", filepath.Join(mnt, "bridge"),
			[]CaseFolding{CaseSensitive, CaseSensitive}, true},
		{"another case by the folding of a longer path", filepath.Join(mnt, "bridge"),
			[]CaseFolding{CaseSensitive, CaseSensitive, CaseSensitive, CaseSensitive}, true},
		{"another case above, in a folder recorded as keeping case", filepath.Join(root, "MNT", "usb", "Bridge"),
			of(CaseSensitive, CaseSensitive, CaseInsensitive), false},
		{"another case above, in a folder recorded as folding case", filepath.Join(root, "MNT", "usb", "Bridge"),
			of(CaseInsensitive, CaseSensitive, CaseSensitive), true},
		{"another name", filepath.Join(mnt, "Bridges"), of(CaseInsensitive, CaseInsensitive, CaseInsensitive), false},
		{"a path below it", filepath.Join(mnt, "Bridge", "x"), of(CaseInsensitive, CaseInsensitive, CaseInsensitive), false},
		{"a path above it", mnt, of(CaseInsensitive, CaseInsensitive, CaseInsensitive), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SameAsRecorded(recorded, tc.now, tc.folding); got != tc.same {
				t.Errorf("SameAsRecorded(%q, %q, %v) = %v, want %v", recorded, tc.now, tc.folding, got, tc.same)
			}
		})
	}
	everyFolds := []CaseFolding{CaseInsensitive, CaseInsensitive}
	if SameAsRecorded(filepath.Join("usb", "Bridge"), filepath.Join("usb", "bridge"), everyFolds) {
		t.Error("two relative spellings in another case read as one path")
	}
	if !SameAsRecorded(filepath.Join("usb", "bridge"), filepath.Join("usb", ".", "bridge"), nil) {
		t.Error("a relative path read as another than itself")
	}
	if runtime.GOOS == "windows" {
		lower := strings.ToLower(root[:1]) + root[1:]
		if !SameAsRecorded(filepath.Join(root, "x"), filepath.Join(lower, "x"), []CaseFolding{CaseSensitive}) {
			t.Errorf("a drive spelled in another case read as another volume")
		}
	}
}

// TestFoldingOnTheWayAsksEachFolderOnce: FoldingOnTheWay asks the probe of
// each folder from the path's own up to its volume's root, keyed as
// filepath.Dir spells it, and none it was asked of already; it answers one
// folding a folder, the root's first, which is what SameAsRecorded compares
// by; and a path 2,000 folders deep is asked of each folder once and kept in
// a letter a folder (backlog B741's round 2).
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
	memo := map[string]CaseFolding{}
	recorded := filepath.Join(mnt, "Bridge")
	folding := FoldingOnTheWay(recorded, probe, memo)
	if want := []string{mnt, filepath.Join(root, "mnt"), root}; !slices.Equal(asked, want) {
		t.Errorf("asked %q, want %q", asked, want)
	}
	if want := []CaseFolding{CaseSensitive, CaseSensitive, CaseInsensitive}; !slices.Equal(folding, want) {
		t.Errorf("FoldingOnTheWay(%q) = %v, want %v, the root's first", recorded, folding, want)
	}
	asked = nil
	other := FoldingOnTheWay(filepath.Join(mnt, "other", "x"), probe, memo)
	if want := []string{filepath.Join(mnt, "other")}; !slices.Equal(asked, want) {
		t.Errorf("a second path asked %q, want %q, the folder not asked yet", asked, want)
	}
	if want := []CaseFolding{CaseSensitive, CaseSensitive, CaseInsensitive, CaseSensitive}; !slices.Equal(other, want) {
		t.Errorf("the second path's folding is %v, want %v", other, want)
	}
	if !SameAsRecorded(recorded, filepath.Join(mnt, "BRIDGE"), folding) {
		t.Error("two spellings in a folder recorded as folding case read as two paths")
	}
	if SameAsRecorded(recorded, filepath.Join(root, "MNT", "usb", "Bridge"), folding) {
		t.Error("two spellings in a folder recorded as keeping case read as one path")
	}
	if got := FoldingOnTheWay(filepath.Join("usb", "Bridge"), probe, memo); got != nil {
		t.Errorf("a relative path's folding is %v, want none", got)
	}

	deep := root
	for i := 0; i < 2000; i++ {
		deep = filepath.Join(deep, "d")
	}
	asked = nil
	letters := FormatFolding(FoldingOnTheWay(deep, func(dir string) CaseFolding {
		asked = append(asked, dir)
		return CaseInsensitive
	}, map[string]CaseFolding{}))
	if len(asked) != 2000 || len(letters) != 2000 || strings.Trim(letters, "i") != "" {
		t.Errorf("a path 2,000 folders deep asked %d folders and kept %d letters (%.20q…), want each folder once and a letter each",
			len(asked), len(letters), letters)
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
	folding := FoldingOnTheWay(recorded, func(dir string) CaseFolding {
		if dir == mnt {
			return CaseInsensitive
		}
		return CaseSensitive
	}, map[string]CaseFolding{})
	if !SamePathAsRecorded(recorded, now, folding) {
		t.Errorf("SamePathAsRecorded(%q, %q) = false where the record says its folder folded case", recorded, now)
	}
	t.Run("through a link to the mount point", func(t *testing.T) {
		link := filepath.Join(base, "link")
		if err := os.Symlink(mnt, link); err != nil {
			t.Skipf("this host makes no link here: %v", err)
		}
		if through := filepath.Join(link, "bridge"); !SamePathAsRecorded(recorded, through, folding) {
			t.Errorf("SamePathAsRecorded(%q, %q) = false: a link to the mount point resolves now", recorded, through)
		}
	})
	folding[len(folding)-1] = CaseSensitive
	if SamePathAsRecorded(recorded, now, folding) {
		t.Errorf("SamePathAsRecorded(%q, %q) = true where the record says its folder kept case", recorded, now)
	}
	if !SamePathAsRecorded(recorded, recorded, folding) {
		t.Errorf("SamePathAsRecorded(%q, itself) = false", recorded)
	}
}

// TestACaseOnlyDifferenceInAFolderThatIsThere states the answer for a folder
// on the way that is there, named in another case (B741's round 4): on
// Windows, where the folder takes both spellings, one folder whatever the
// letter of the folder its name is looked up in, since a resolution there
// names a folder that is there by its name on disk, so both spellings resolve
// to it and the letters judge only what lies below it; on Linux and macOS,
// whose resolution keeps the spelling it is given, one folder where that
// letter is folding case and two where it is keeping case, the record
// deciding as it does for a folder that is not there. A difference in the
// leaf, which is not there, is judged by its folder's letter everywhere.
func TestACaseOnlyDifferenceInAFolderThatIsThere(t *testing.T) {
	base := t.TempDir()
	music := filepath.Join(base, "Music")
	if err := os.Mkdir(music, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(base, "MUSIC"))
	oneFolder := runtime.GOOS == "windows" && err == nil
	recorded := filepath.Join(music, "Bridge")
	const (
		I = CaseInsensitive
		S = CaseSensitive
	)
	for _, tc := range []struct {
		name    string
		now     string
		letters map[string]CaseFolding
		same    bool
	}{
		{"its folder recorded as folding case", filepath.Join(base, "MUSIC", "Bridge"),
			map[string]CaseFolding{base: I, music: S}, true},
		{"its folder recorded as keeping case", filepath.Join(base, "MUSIC", "Bridge"),
			map[string]CaseFolding{base: S, music: S}, oneFolder},
		{"its folder recorded as keeping case, the leaf in another case in a folder folding case", filepath.Join(base, "MUSIC", "bridge"),
			map[string]CaseFolding{base: S, music: I}, oneFolder},
		{"its folder recorded as folding case, the leaf in another case in a folder keeping case", filepath.Join(base, "MUSIC", "bridge"),
			map[string]CaseFolding{base: I, music: S}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folding := FoldingOnTheWay(recorded, lettersOf(tc.letters), map[string]CaseFolding{})
			if got := SamePathAsRecorded(recorded, tc.now, folding); got != tc.same {
				t.Errorf("SamePathAsRecorded(%q, %q, %s) = %v, want %v (%s)", recorded, tc.now, FormatFolding(folding), got, tc.same, runtime.GOOS)
			}
		})
	}
}

// TestParseFoldingReadsWhatFormatFoldingWrites: a folding reads back from
// its letters, each CaseFolding its own letter, and a letter ParseFolding
// does not know reads as CaseUnknown.
func TestParseFoldingReadsWhatFormatFoldingWrites(t *testing.T) {
	folding := []CaseFolding{CaseSensitive, CaseInsensitive, CaseUnknown, CaseSensitive}
	letters := FormatFolding(folding)
	if letters != "sius" {
		t.Errorf("FormatFolding(%v) = %q, want %q", folding, letters, "sius")
	}
	if got := ParseFolding(letters); !slices.Equal(got, folding) {
		t.Errorf("ParseFolding(%q) = %v, want %v", letters, got, folding)
	}
	if got := ParseFolding("sx"); !slices.Equal(got, []CaseFolding{CaseSensitive, CaseUnknown}) {
		t.Errorf("ParseFolding of a letter it does not know = %v, want it unknown", got)
	}
	if got := ParseFolding(""); got != nil {
		t.Errorf("ParseFolding of nothing = %v, want none", got)
	}
}
