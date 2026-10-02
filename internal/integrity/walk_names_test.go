package integrity

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// What the sidecar walks compare and what they look inside (2026-10-02,
// backlog B206 and B207). The known set is compared in one Unicode
// normalization, so a file an HFS+ volume hands back decomposed is the file
// its row names composed; and the walks pass over the directories a library
// walk skips (manifest.ShouldSkipDir), so a NAS recycle bin or snapshot under
// the variants directory is neither counted nor unlinked.

// skippedDirNames are the names manifest.ShouldSkipDir answers beyond the dot
// rule, except lost+found, whose rule in these walks is its own
// (IsFilesystemLostFound). Listed by hand because ShouldSkipDir is a switch;
// TestTheSkippedDirNamesAreTheScannersOwn holds each to it.
var skippedDirNames = []string{
	"$RECYCLE.BIN", "$Recycle.Bin", "System Volume Information",
	"@eaDir", "#recycle", "#snapshot",
	"@Recycle", "@Recently-Snapshot",
	"~snapshot",
}

// walkedNeighbourNames are directory names a library may hold that sit beside
// the skipped ones, each one character or one case away: a walk that
// matched loosely would take an album for a recycle bin.
var walkedNeighbourNames = []string{"#1 Hits", "Recycler", "eaDir", "snapshot", "Lost+Found Sessions"}

// TestTheSkippedDirNamesAreTheScannersOwn — each name the tests below plant as
// a skipped directory is one the scanner skips, and each neighbour is one it
// walks.
func TestTheSkippedDirNamesAreTheScannersOwn(t *testing.T) {
	for _, name := range skippedDirNames {
		if !manifest.ShouldSkipDir(name) {
			t.Errorf("manifest.ShouldSkipDir(%q) = false: the fixture plants a directory the scanner walks", name)
		}
	}
	for _, name := range walkedNeighbourNames {
		if manifest.ShouldSkipDir(name) {
			t.Errorf("manifest.ShouldSkipDir(%q) = true: the neighbour is not one the scanner walks", name)
		}
	}
}

// renditionName is a rendition basename in the source-mirrored layout.
const renditionName = "01.flac.upscaled-v2-176400-24.flac"

// TestSidecarInventoryPassesOverTheDirectoriesALibraryWalkSkips — a NAS
// share mounted as the variants directory holds its recycle bin and its
// snapshots at the top (Synology's #recycle and #snapshot, QNAP's @Recycle
// and @Recently-Snapshot, NetApp's ~snapshot, Windows' $RECYCLE.BIN on a
// drive root) and Synology's @eaDir beside every folder it indexes. The
// walk counted their files as orphans, so `upscale --gc` unlinked a
// recycle bin's renditions (files an operator deleted to get back) and a
// snapshot's copies, and a visible snapshot, every rendition again, made the
// tree read as one that lost its index. They are passed over now, at any
// depth, as a dot-directory always was, and a directory one character
// away from such a name is walked.
func TestSidecarInventoryPassesOverTheDirectoriesALibraryWalkSkips(t *testing.T) {
	root := t.TempDir()
	live := seedTree(t, root,
		"Artist/Album/"+renditionName,
		"Artist/Album/02.flac.upscaled-v2-176400-24.flac",
	)
	var walked []string
	for _, name := range walkedNeighbourNames {
		walked = append(walked, seedTree(t, root, name+"/Album/"+renditionName)...)
	}
	for _, name := range skippedDirNames {
		seedTree(t, root,
			name+"/Artist/Album/"+renditionName,
			"Artist/Album/"+name+"/"+renditionName+"/SYNOINDEX_MEDIA_INFO",
		)
	}

	inv, err := TakeSidecarInventory(context.Background(), root, knownOf(live...), SidecarInventoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Files != len(live)+len(walked) || inv.Known != len(live) || inv.Orphans != len(walked) {
		t.Errorf("files=%d known=%d orphans=%d, want %d, %d and %d: the live renditions and the albums beside "+
			"the skipped directories, nothing inside them", inv.Files, inv.Known, inv.Orphans,
			len(live)+len(walked), len(live), len(walked))
	}
	got := append([]string(nil), inv.OrphanPaths...)
	sort.Strings(got)
	sort.Strings(walked)
	if strings.Join(got, "\n") != strings.Join(walked, "\n") {
		t.Errorf("orphan paths:\n%s\nwant the albums beside the skipped directories:\n%s",
			strings.Join(got, "\n"), strings.Join(walked, "\n"))
	}
}

// TestTheRenditionProbeCountsNoRenditionInADirectoryALibraryWalkSkips — the
// probe behind the mount-loss check and the relocation guard
// (scanForRenditions) asks whether the tree still holds a rendition. A
// recycle bin's are renditions an operator deleted and a snapshot's are
// copies, so a tree holding renditions only there holds none of its own: the
// mount-loss probe reads it as the B223 state (holds no rendition), and the
// relocation guard does not take deleted files for moved ones. A link to a
// directory under such a name is no rendition it cannot see behind, as a
// dot-named link was not. Each name is planted at the top and further down.
func TestTheRenditionProbeCountsNoRenditionInADirectoryALibraryWalkSkips(t *testing.T) {
	for _, name := range skippedDirNames {
		for _, rel := range []string{name + "/Artist/Album/" + renditionName, "Artist/Album/" + name + "/" + renditionName} {
			dir := t.TempDir()
			seedTree(t, dir, rel)
			if got, err := TreeHoldsVariantSidecars(dir); got || err != nil {
				t.Errorf("%s: TreeHoldsVariantSidecars = %v, %v; want false, nil", rel, got, err)
			}
			if b := VariantsDirSweepBlock(dir); !b.Empty || b.Reason != "variants directory holds no rendition" {
				t.Errorf("%s: the probe answered %+v, want the directory read as holding no rendition", rel, b)
			}
		}
	}
	for _, name := range walkedNeighbourNames {
		dir := t.TempDir()
		seedTree(t, dir, name+"/Album/"+renditionName)
		if got, err := TreeHoldsVariantSidecars(dir); !got || err != nil {
			t.Errorf("%s: TreeHoldsVariantSidecars = %v, %v; want true, nil (a directory the scanner walks)", name, got, err)
		}
	}
	if runtime.GOOS == "windows" {
		return // a symlink needs a privilege the CI user may not hold
	}
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	seedTree(t, target, "Album/"+renditionName)
	if err := os.Symlink(target, filepath.Join(dir, "#snapshot")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if b := VariantsDirSweepBlock(dir); !b.Empty {
		t.Errorf("a link named #snapshot kept the directory healthy: %+v", b)
	}
}

// TestOrphanSidecarSweeperLeavesARecycleBinAndASnapshotAlone — the
// background sweep over a NAS share. With a visible snapshot holding a copy
// of the tree, every tick refused as a lost index ("the catalog is far
// smaller than the tree it describes") and reclaimed nothing, forever; with
// a recycle bin under the floor of ten, it emptied the recycle bin. It
// reclaims the tree's own orphans now and leaves both alone.
func TestOrphanSidecarSweeperLeavesARecycleBinAndASnapshotAlone(t *testing.T) {
	t.Run("a visible snapshot no longer reads as a lost index", func(t *testing.T) {
		dir := t.TempDir()
		live := seedTestSidecarTree(t, dir, "live-", 20)
		seedTestSidecarTree(t, dir, "orphan-", 5)
		snapshot := filepath.Join(dir, "#snapshot", "GMT+01_2026-09-01-0300")
		seedTestSidecarTree(t, snapshot, "live-", 20)
		seedTestSidecarTree(t, snapshot, "orphan-", 5)
		ageFixtures(t, dir)
		s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: pathSet(live)}, staticDir(dir), time.Hour, sweepPercent)
		rec := loggingtest.Record(t)

		if n := s.tick(context.Background()); n != 5 {
			t.Errorf("unlinked %d, want the tree's 5 orphans", n)
		}
		requireLinesSay(t, rec.Lines(msgOrphanRefusal), 0, "no lost-index refusal over a snapshot")
		if got := countFiles(t, filepath.Join(dir, "#snapshot")); got != 25 {
			t.Errorf("%d of the snapshot's 25 files survive", got)
		}
	})
	t.Run("a recycle bin is not emptied", func(t *testing.T) {
		dir := t.TempDir()
		live := seedTestSidecarTree(t, dir, "live-", 20)
		recycled := filepath.Join(dir, "#recycle", "Artist", "Album")
		seedTestSidecarTree(t, recycled, "deleted-", 3)
		ageFixtures(t, dir)
		s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: pathSet(live)}, staticDir(dir), time.Hour, sweepPercent)

		if n := s.tick(context.Background()); n != 0 {
			t.Errorf("unlinked %d, want nothing: the recycle bin is not the tree's", n)
		}
		if got := countFiles(t, recycled); got != 3 {
			t.Errorf("%d of the recycle bin's 3 files survive", got)
		}
	})
}

// TestTheKnownSetMatchesASidecarInEitherNormalization — HFS+ stores a name
// decomposed (NFD) whatever spelling created it, and a walk hands it back
// that way, while the row records the spelling the file was written by:
// the library-relative path the scanner read, composed (NFC) on most
// filesystems. The lookup found the decomposed name in no row, while
// LocateSidecar's stat of the composed one reached the file, so a live
// rendition was an orphan to the forward sweeps and present to the reverse
// one: unlinked, its row reaped, rendered again, and so on. Planted here
// in both spellings on any filesystem; the third case writes the file by
// the row's own spelling, as the pool does, which only an HFS+ volume
// decomposes (TMPDIR on an hdiutil HFS+ image reproduces it there). The
// last, a row recorded decomposed (a library scanned from HFS+) over a file
// written by it, every build kept: it fails if the known set is keyed
// without composing, since the walk's lookup composes what it walks.
func TestTheKnownSetMatchesASidecarInEitherNormalization(t *testing.T) {
	const source = "Beyoncé/Café Tacvba/01 Révolución.flac"
	const id = "upscaled-v2-176400-24"
	for _, tc := range []struct {
		name             string
		recorded, onDisk norm.Form
		writtenAsRecord  bool
	}{
		{name: "the row composed, the walk decomposed (an HFS+ volume)", recorded: norm.NFC, onDisk: norm.NFD},
		{name: "the row decomposed, the walk composed", recorded: norm.NFD, onDisk: norm.NFC},
		{name: "written by the row's own spelling, as the pool writes it", recorded: norm.NFC, writtenAsRecord: true},
		{name: "recorded decomposed and written by it", recorded: norm.NFD, writtenAsRecord: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			row := VariantSnapshot{SourcePath: tc.recorded.String(source), VariantID: id}
			row.SidecarPath = CanonicalSidecarPath(dir, row)
			onDisk := row.SidecarPath
			if !tc.writtenAsRecord {
				onDisk = CanonicalSidecarPath(dir, VariantSnapshot{SourcePath: tc.onDisk.String(source), VariantID: id})
			}
			seedTree(t, dir, mustRel(t, dir, onDisk))
			if tc.recorded != tc.onDisk && !tc.writtenAsRecord && listedName(t, onDisk) == filepath.Base(row.SidecarPath) {
				t.Skip("this filesystem hands both spellings back alike, so the fixture cannot plant the other one")
			}

			inv, err := TakeSidecarInventory(context.Background(), dir, KnownSidecarSet(dir, []VariantSnapshot{row}), SidecarInventoryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if inv.Known != 1 || inv.Orphans != 0 {
				t.Errorf("known=%d orphans=%d, want the rendition known: its row spells %+q, the walk %+q",
					inv.Known, inv.Orphans, filepath.Base(row.SidecarPath), listedName(t, onDisk))
			}

			ageFixtures(t, dir)
			s := NewOrphanSidecarSweeper(&fakeSidecarLister{rows: []VariantSnapshot{row}}, staticDir(dir), time.Hour, sweepPercent)
			if n := s.tick(context.Background()); n != 0 {
				t.Errorf("the background sweep unlinked %d, want the live rendition kept", n)
			}
			if got := countFiles(t, dir); got != 1 {
				t.Errorf("%d file(s) left, want the rendition", got)
			}
		})
	}
}

// listedName is the name the directory holding p lists for it: the
// spelling a walk hands back, which on a normalizing filesystem is not the
// one the file was created by.
func listedName(t *testing.T, p string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("%s holds %d entries, want the one rendition", filepath.Dir(p), len(entries))
	}
	return entries[0].Name()
}

// TestSidecarWalksListNoDirectoryALibraryWalkSkips — the walks pass over
// such a directory by its NAME, before listing it, so one this user cannot
// list (a recycle bin root owns, or on Windows System Volume Information,
// which only SYSTEM may list) is no directory the walk could not list. On
// main the inventory counted it as one, so the background sweep refused
// every tick as a partial walk, and the probe failed on it.
func TestSidecarWalksListNoDirectoryALibraryWalkSkips(t *testing.T) {
	skipWhereModesDenyNothing(t)
	dir := t.TempDir()
	live := seedTestSidecarTree(t, dir, "live-", 3)
	recycle := filepath.Join(dir, "#recycle")
	seedTestSidecarTree(t, recycle, "deleted-", 2)
	lockDir(t, recycle)

	inv, err := TakeSidecarInventory(context.Background(), dir, knownOf(live...), SidecarInventoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Unreadable != 0 || inv.UnlistedDirs != 0 {
		t.Errorf("unreadable=%d unlistedDirs=%d, want 0 and 0: the recycle bin is passed over by its name",
			inv.Unreadable, inv.UnlistedDirs)
	}
	if reason := PartialWalkRefusal(inv, len(live), sweepPercent); reason != "" {
		t.Errorf("the walk reads as partial: %s", reason)
	}

	only := t.TempDir()
	locked := filepath.Join(only, "#recycle")
	seedTestSidecarTree(t, locked, "deleted-", 2)
	lockDir(t, locked)
	if got, err := TreeHoldsVariantSidecars(only); got || err != nil {
		t.Errorf("TreeHoldsVariantSidecars over a locked recycle bin alone = %v, %v; want false, nil", got, err)
	}
}

// TestKnownSidecarKeyIsOneKeyPerName — two spellings of one name, in either
// Unicode normalization and either case, are one key, and two names are
// two. The dotted capital I pins the order: lowercased before it is
// composed, its decomposed spelling (I and U+0307) lowers to i and U+0307,
// which compose to nothing else, while its composed spelling (U+0130)
// lowers to i, so the two would be two keys.
func TestKnownSidecarKeyIsOneKeyPerName(t *testing.T) {
	same := [][2]string{
		{norm.NFC.String("/v/Café/01.flac"), norm.NFD.String("/v/Café/01.flac")},
		{norm.NFC.String("/v/CAFÉ/01.flac"), norm.NFD.String("/v/café/01.flac")},
		{"/v/İstanbul/01.flac", "/v/İstanbul/01.flac"},
		{"/v/Album/../Album/01.flac", "/v/album/01.flac"},
	}
	for _, p := range same {
		if a, b := KnownSidecarKey(p[0]), KnownSidecarKey(p[1]); a != b {
			t.Errorf("KnownSidecarKey(%+q) = %+q and KnownSidecarKey(%+q) = %+q: one name keyed two ways", p[0], a, p[1], b)
		}
	}
	different := [][2]string{
		{"/v/Cafe/01.flac", "/v/Café/01.flac"},
		{"/v/Album/01.flac", "/v/Album/02.flac"},
	}
	for _, p := range different {
		if KnownSidecarKey(p[0]) == KnownSidecarKey(p[1]) {
			t.Errorf("KnownSidecarKey keys %+q and %+q alike: two names", p[0], p[1])
		}
	}
}
