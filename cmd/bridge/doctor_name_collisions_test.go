package main

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The name-collisions check (backlog B420). The app folds case and Unicode
// composition on a bridge share, so two names in one folder that fold to one
// name are one folder or one track there. These tests drive buildDoctorDeps
// and doctor.Run over a seeded manifest, because the check reads the store
// and nothing else: no file exists at any seeded path, and none is needed.

// nameCollisionInstall writes a bridge.yaml whose library roots are roots
// (directories made under a temp dir) and seeds its manifest with a track at
// each of paths, in the form the scanner stores them: library-relative, and
// with more than one root prefixed by the root's folder name.
func nameCollisionInstall(t *testing.T, roots, paths []string) string {
	t.Helper()
	dir := t.TempDir()
	var body strings.Builder
	body.WriteString("libraryRoots:\n")
	for _, r := range roots {
		abs := filepath.Join(dir, filepath.FromSlash(r))
		if err := os.MkdirAll(abs, 0o755); err != nil {
			t.Fatal(err)
		}
		body.WriteString("  - " + abs + "\n")
	}
	dataDir := filepath.Join(dir, "data")
	body.WriteString("dataDir: " + dataDir + "\nadminAddress: 127.0.0.1:" + closedLoopbackPort(t) + "\n")
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	for _, p := range paths {
		if err := store.UpsertTrack(context.Background(), &manifest.Track{
			Path: p, Size: 1, ModTime: time.Unix(0, 0).UTC(), Title: path.Base(p),
		}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", p, err)
		}
	}
	return cfgPath
}

// nameCollisionCheck runs the whole report for cfgPath and returns the
// name-collisions line.
func nameCollisionCheck(t *testing.T, cfgPath string) doctor.Check {
	t.Helper()
	return findCheck(t, doctor.Run(context.Background(), buildDoctorDeps(cfgPath)), "name-collisions")
}

// wantGroup is how the hint names a group: each path quoted, in byte order.
func wantGroup(paths ...string) string {
	q := make([]string, len(paths))
	for i, p := range paths {
		q[i] = strconv.Quote(p)
	}
	if len(q) == 1 {
		return q[0]
	}
	return strings.Join(q[:len(q)-1], ", ") + " and " + q[len(q)-1]
}

// TestDoctorNameCollisionsNamesAComposedAndADecomposedSpelling: an album
// folder stored once composed (NFC "Café") and once decomposed (NFD, "e" and
// a combining acute) is one folder to the app. The two print alike, so the
// hint says which is which.
func TestDoctorNameCollisionsNamesAComposedAndADecomposedSpelling(t *testing.T) {
	const nfc, nfd = "Artist/Caf\u00e9", "Artist/Cafe\u0301"
	cfgPath := nameCollisionInstall(t, []string{"Music"}, []string{
		nfc + "/01 Intro.flac",
		nfd + "/01 Intro.flac",
		"Artist/Other/01 Intro.flac",
	})
	c := nameCollisionCheck(t, cfgPath)
	if c.Status != doctor.Warn {
		t.Fatalf("status=%v, want warn\nsummary: %s", c.Status, c.Summary)
	}
	if !strings.HasPrefix(c.Summary, "1 group(s) ") {
		t.Errorf("summary does not count one group: %q", c.Summary)
	}
	// NFD sorts first: "e" (0x65) is below the composed letter's lead byte.
	want := strconv.Quote(nfd) + " (decomposed) and " + strconv.Quote(nfc) + " (composed)"
	if !strings.Contains(c.Hint, want) {
		t.Errorf("hint does not name the pair as %s:\n%s", want, c.Hint)
	}
	if strings.Contains(c.Hint, "01 Intro.flac") {
		t.Errorf("hint names the tracks below the folders, not the folders: %s", c.Hint)
	}
}

// TestDoctorNameCollisionsNamesTwoSpellingsThatDifferOnlyInCase: two files in
// one folder whose names differ in case alone are one track to the app.
func TestDoctorNameCollisionsNamesTwoSpellingsThatDifferOnlyInCase(t *testing.T) {
	const upper, lower = "The Beatles/Abbey Road/01 Come Together.flac", "The Beatles/Abbey Road/01 come together.flac"
	cfgPath := nameCollisionInstall(t, []string{"Music"}, []string{
		upper, lower,
		"The Beatles/Abbey Road/02 Something.flac",
	})
	c := nameCollisionCheck(t, cfgPath)
	if c.Status != doctor.Warn {
		t.Fatalf("status=%v, want warn\nsummary: %s", c.Status, c.Summary)
	}
	if !strings.HasPrefix(c.Summary, "1 group(s) ") {
		t.Errorf("summary does not count one group: %q", c.Summary)
	}
	if want := wantGroup(upper, lower); !strings.Contains(c.Hint, want) {
		t.Errorf("hint does not name the pair as %s:\n%s", want, c.Hint)
	}
	// Both spellings are ASCII: there is no composition to tell apart.
	if strings.Contains(c.Hint, "composed)") {
		t.Errorf("hint marks a case pair with a composition note: %s", c.Hint)
	}
}

// TestDoctorNameCollisionsReportsAStrayFolderOnceNotPerTrack: the folders
// "Artist" and "artist" hold the same album. The paths first differ at the
// artist folder, so that is the group, once, and no track below it is a group
// of its own.
func TestDoctorNameCollisionsReportsAStrayFolderOnceNotPerTrack(t *testing.T) {
	cfgPath := nameCollisionInstall(t, []string{"Music"}, []string{
		"Artist/Album/01 One.flac",
		"Artist/Album/02 Two.flac",
		"Artist/Album/03 Three.flac",
		"artist/Album/01 One.flac",
		"artist/Album/02 Two.flac",
		"artist/Album/03 Three.flac",
		"artist/Album/04 Four.flac",
	})
	c := nameCollisionCheck(t, cfgPath)
	if c.Status != doctor.Warn {
		t.Fatalf("status=%v, want warn\nsummary: %s", c.Status, c.Summary)
	}
	if !strings.HasPrefix(c.Summary, "1 group(s) ") {
		t.Errorf("summary does not count exactly one group: %q", c.Summary)
	}
	if want := wantGroup("Artist", "artist"); !strings.Contains(c.Hint, want) {
		t.Errorf("hint does not name the folder pair as %s:\n%s", want, c.Hint)
	}
	if strings.Contains(c.Hint, "Album/") || strings.Contains(c.Hint, "more)") {
		t.Errorf("hint names paths below the stray folder: %s", c.Hint)
	}
}

// TestDoctorNameCollisionsComparesAPathOnlyWithinItsOwnRoot: with two roots
// every stored path begins with its root's folder name. "Miles Davis" in one
// root and "miles davis" in the other are two folders in the app too, so they
// are not a group; two spellings of an album inside one root are, named with
// the root's folder name in front.
func TestDoctorNameCollisionsComparesAPathOnlyWithinItsOwnRoot(t *testing.T) {
	cfgPath := nameCollisionInstall(t, []string{"Music", "Archive"}, []string{
		"Music/Miles Davis/Kind of Blue/01 So What.flac",
		"Music/Miles Davis/Kind Of Blue/01 So What.flac",
		"Archive/miles davis/Kind of Blue/01 So What.flac",
		"Archive/John Coltrane/Blue Train/01 Blue Train.flac",
	})
	c := nameCollisionCheck(t, cfgPath)
	if c.Status != doctor.Warn {
		t.Fatalf("status=%v, want warn\nsummary: %s", c.Status, c.Summary)
	}
	if !strings.HasPrefix(c.Summary, "1 group(s) ") {
		t.Errorf("summary does not count exactly one group (the cross-root pair is not one): %q", c.Summary)
	}
	if want := wantGroup("Music/Miles Davis/Kind Of Blue", "Music/Miles Davis/Kind of Blue"); !strings.Contains(c.Hint, want) {
		t.Errorf("hint does not name the in-root pair as %s:\n%s", want, c.Hint)
	}
	if strings.Contains(c.Hint, "Archive") {
		t.Errorf("hint pairs a path with one in another root: %s", c.Hint)
	}
}

// TestDoctorNameCollisionsComparesTheRootsFolderNames: the roots' own folder
// names are the first component of every stored path, and the app folds the
// whole path, so two roots named "Café", one composed and one decomposed, are
// one folder there. fs.ValidateRoots refuses two root names that differ only
// in case and accepts this pair.
func TestDoctorNameCollisionsComparesTheRootsFolderNames(t *testing.T) {
	const nfc, nfd = "Caf\u00e9", "Cafe\u0301"
	cfgPath := nameCollisionInstall(t, []string{"one/" + nfc, "two/" + nfd}, []string{
		nfc + "/Artist/Album/01 Intro.flac",
		nfd + "/Artist/Album/01 Intro.flac",
	})
	c := nameCollisionCheck(t, cfgPath)
	if c.Status != doctor.Warn {
		t.Fatalf("status=%v, want warn\nsummary: %s", c.Status, c.Summary)
	}
	if !strings.HasPrefix(c.Summary, "1 group(s) ") {
		t.Errorf("summary does not count exactly one group: %q", c.Summary)
	}
	want := strconv.Quote(nfd) + " (decomposed) and " + strconv.Quote(nfc) + " (composed)"
	if !strings.Contains(c.Hint, want) {
		t.Errorf("hint does not name the two roots' folders as %s:\n%s", want, c.Hint)
	}
}

// TestDoctorNameCollisionsNamesTheFirstFiveGroupsAndCountsTheRest: seven
// groups, five named, the rest counted, the bound the variants-index hint
// puts on its sample. The first five are the first in PATH order: the
// top-level pair is found last (its folder's list is complete only at the
// end) and sorts first, and the six in "Beta/Album" are one folder's list.
func TestDoctorNameCollisionsNamesTheFirstFiveGroupsAndCountsTheRest(t *testing.T) {
	paths := []string{"Alpha/01 Intro.flac", "alpha/01 Intro.flac"}
	for _, n := range []string{"P", "Q", "R", "S", "T", "U"} {
		paths = append(paths, "Beta/Album/01 "+n+".flac", "Beta/Album/01 "+strings.ToLower(n)+".flac")
	}
	cfgPath := nameCollisionInstall(t, []string{"Music"}, paths)
	c := nameCollisionCheck(t, cfgPath)
	if c.Status != doctor.Warn {
		t.Fatalf("status=%v, want warn\nsummary: %s", c.Status, c.Summary)
	}
	if !strings.HasPrefix(c.Summary, "7 group(s) ") {
		t.Errorf("summary does not count all seven groups: %q", c.Summary)
	}
	if doctorNameCollisionGroups != 5 {
		t.Fatalf("the hint names %d group(s), and this test names five", doctorNameCollisionGroups)
	}
	named := []string{wantGroup("Alpha", "alpha")}
	for _, n := range []string{"P", "Q", "R", "S"} {
		named = append(named, wantGroup("Beta/Album/01 "+n+".flac", "Beta/Album/01 "+strings.ToLower(n)+".flac"))
	}
	if want := strings.Join(named, "; ") + " (+2 more)"; !strings.Contains(c.Hint, want) {
		t.Errorf("hint does not name the first five groups in path order and count the rest:\nwant %s\nhint %s", want, c.Hint)
	}
	for _, n := range []string{"T", "U"} {
		if strings.Contains(c.Hint, "01 "+n+".flac") {
			t.Errorf("hint names a group past the first five (%s): %s", n, c.Hint)
		}
	}
}

// TestDoctorNameCollisionsPassesALibraryWithNoCollisions: names that are
// alike and not one name to the app pass. A capital I with a dot is not a
// plain "i" (the fold keeps its dot), "ß" is not "ss" (the app lowercases, it
// does not case-fold), and the same name in two folders is two names.
func TestDoctorNameCollisionsPassesALibraryWithNoCollisions(t *testing.T) {
	cfgPath := nameCollisionInstall(t, []string{"Music"}, []string{
		"Artist/Album/01 Song.flac",
		"Artist/Album 2/01 Song.flac",
		"\u0130stanbul/Album/01 Song.flac",
		"istanbul/Album/01 Song.flac",
		"Stra\u00dfe/Album/01 Song.flac",
		"STRASSE/Album/01 Song.flac",
		"Artist A/Greatest Hits/01 Song.flac",
		"Artist B/greatest hits/01 Song.flac",
	})
	c := nameCollisionCheck(t, cfgPath)
	if c.Status != doctor.OK {
		t.Fatalf("status=%v, want ok\nsummary: %s\nhint: %s", c.Status, c.Summary, c.Hint)
	}
	if !strings.HasPrefix(c.Summary, "8 track path(s) under the library roots; ") {
		t.Errorf("summary does not say what it compared: %q", c.Summary)
	}
}

// TestDoctorNameCollisionsLeavesUPnPRoutedRowsOut: a routed row's path is
// made from another server's metadata, and no copy of it is on this host to
// fix, so routed rows are not compared and not counted.
func TestDoctorNameCollisionsLeavesUPnPRoutedRowsOut(t *testing.T) {
	routed := []string{"2go/Artist/Album/01 Song.flac", "2go/artist/Album/01 Song.flac"}
	cfgPath := nameCollisionInstall(t, []string{"Music"}, append([]string{"Local/Album/01 Song.flac"}, routed...))
	dataDir := filepath.Join(filepath.Dir(cfgPath), "data")
	store, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range routed {
		if err := store.UpsertUPnPRouting(context.Background(), &manifest.UPnPRouting{
			SourcePath: p, ServerUDN: "uuid:00000000-0000-0000-0000-00000000b420",
			ObjectID: "64$0$" + strconv.Itoa(i), ResURL: "http://192.0.2.10:8200/MediaItems/" + strconv.Itoa(i) + ".flac",
			LastSeenAt: time.Now(),
		}); err != nil {
			t.Fatalf("UpsertUPnPRouting %q: %v", p, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	c := nameCollisionCheck(t, cfgPath)
	if c.Status != doctor.OK {
		t.Fatalf("status=%v, want ok (routed rows are not compared)\nsummary: %s\nhint: %s", c.Status, c.Summary, c.Hint)
	}
	if !strings.HasPrefix(c.Summary, "1 track path(s) under the library roots; ") {
		t.Errorf("summary counts the routed rows: %q", c.Summary)
	}
}
