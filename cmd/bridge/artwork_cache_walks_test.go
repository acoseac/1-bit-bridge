package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The walks of the artwork cache (2026-09-29, backlog B64): the size cap
// goes on over a directory it cannot list and says so once per streak, the
// GC keeps the thumbnails of an artist a track row names, and every walk
// starts where a linked cache directory resolves.

// keptArtistMBID is the artist a track row names in these tests;
// orphanArtistMBID is one no track names.
const (
	keptArtistMBID   = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	orphanArtistMBID = "22222222-2222-4222-8222-222222222222"
)

// sweepCapCounts runs one pass of the size cap and returns its counts.
func sweepCapCounts(ctx context.Context, dir string, capBytes int64) (int, int64, error) {
	res, err := sweepArtworkCache(ctx, dir, capBytes)
	return res.Evicted, res.Freed, err
}

// cacheFilesUnder lists the regular files under dir, relative to it and
// slash-separated, sorted.
func cacheFilesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// linkCacheDirOrSkip makes link a link to the directory target: a symbolic
// link, and on Windows a directory junction (`mklink /J`), the ordinary way
// to point a folder at another volume there, which needs no privilege.
// Skips on a host that can make neither.
func linkCacheDirOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("this host cannot create a junction: %v: %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this host cannot create a symlink: %v", err)
	}
}

// linkedArtworkCache replaces the fixture's cache directory with a link to
// a directory elsewhere and returns that directory, where the files go.
func linkedArtworkCache(t *testing.T, artworkDir string) string {
	t.Helper()
	if err := os.Remove(artworkDir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "artwork-volume")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	linkCacheDirOrSkip(t, target, artworkDir)
	return target
}

// TestSweepArtworkCacheStepsOverTheFilesystemsLostFound — a cache that is an
// ext4 volume's mount root holds the volume's root-owned lost+found. The
// cap steps over it without a word and evicts down to its low-water mark.
// It returned the directory's permission error before evicting anything,
// on every pass, so on such a host the cap was never enforced.
func TestSweepArtworkCacheStepsOverTheFilesystemsLostFound(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root lists lost+found, so there would be nothing to step over")
	dir := t.TempDir()
	base := time.Now().Add(-10 * time.Hour)
	// 6 × 30 B = 180 B against a cap of 100: the three oldest go, leaving 90.
	var files []string
	for i := 0; i < 6; i++ {
		files = append(files, writeArtFile(t, dir, fmt.Sprintf("local-%d-500.jpg", i), 30, base.Add(time.Duration(i)*time.Hour)))
	}
	lostFound := filepath.Join(dir, "lost+found")
	writeFixtureFile(t, filepath.Join(lostFound, "#12345"), 1000)
	lockDir(t, lostFound)

	res, err := sweepArtworkCache(context.Background(), dir, 100)
	if err != nil {
		t.Fatalf("the sweep failed over the filesystem's lost+found: %v", err)
	}
	if res.Evicted != 3 || res.Freed != 90 {
		t.Errorf("evicted %d files, %d bytes; want 3 and 90", res.Evicted, res.Freed)
	}
	if !res.sawAll() {
		t.Errorf("the filesystem's lost+found was reported as unseen: %+v", res)
	}
	for _, f := range files[:3] {
		mustGone(t, f)
	}
	for _, f := range files[3:] {
		mustExist(t, f)
	}
}

// TestSweepArtworkCacheGoesOnOverADirectoryItCannotList — any other
// directory the cap cannot list is stepped over and named: the files in it
// are neither counted nor evicted, the ones it can see are evicted
// oldest first down to the low-water mark, and the result says what it
// could not see.
func TestSweepArtworkCacheGoesOnOverADirectoryItCannotList(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root lists any directory, so there would be nothing to step over")
	dir := t.TempDir()
	base := time.Now().Add(-10 * time.Hour)
	var files []string
	for i := 0; i < 6; i++ {
		files = append(files, writeArtFile(t, dir, fmt.Sprintf("local-%d-500.jpg", i), 30, base.Add(time.Duration(i)*time.Hour)))
	}
	thumbs := filepath.Join(dir, manifest.ThumbsDirName)
	hidden := filepath.Join(thumbs, "local-0-250.jpg")
	writeFixtureFile(t, hidden, 1000)
	lockDir(t, thumbs)

	res, err := sweepArtworkCache(context.Background(), dir, 100)
	if err != nil {
		t.Fatalf("the sweep stopped at a directory it could not list: %v", err)
	}
	if res.Evicted != 3 || res.Freed != 90 || res.Seen != 180 {
		t.Errorf("evicted %d files, %d bytes, of %d seen; want 3, 90 and 180", res.Evicted, res.Freed, res.Seen)
	}
	if len(res.Unlisted) != 1 || res.Unlisted[0] != thumbs {
		t.Errorf("unlisted = %q, want [%q]", res.Unlisted, thumbs)
	}
	for _, f := range files[:3] {
		mustGone(t, f)
	}
	for _, f := range files[3:] {
		mustExist(t, f)
	}
	if err := os.Chmod(thumbs, 0o755); err != nil {
		t.Fatal(err)
	}
	mustExist(t, hidden)
}

// TestSweepArtworkCacheEvictsOnlyWhatAWholeCachePassWould pins the reason
// the cap may go on over a directory it cannot list: a pass that cannot
// see some files evicts a subset of what a pass over the whole cache
// evicts, never a file that pass would keep, whether the unseen files are
// the oldest in the cache or the newest. Each case builds the same cache
// twice, locks thumbs/ in one, and compares what the two passes removed.
func TestSweepArtworkCacheEvictsOnlyWhatAWholeCachePassWould(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root lists any directory")
	for _, c := range []struct {
		name string
		// hiddenAge places the files in thumbs/ relative to the visible
		// ones, which are 1 to 5 hours old.
		hiddenAge time.Duration
		capBytes  int64
	}{
		{"the unseen files are the oldest", -48 * time.Hour, 140},
		{"the unseen files are the newest", 48 * time.Hour, 140},
		{"the unseen files are the oldest, a tighter cap", -48 * time.Hour, 60},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := time.Now().Add(-100 * time.Hour)
			build := func() string {
				dir := t.TempDir()
				for i := 0; i < 5; i++ {
					writeArtFile(t, dir, fmt.Sprintf("local-%d-500.jpg", i), 30, base.Add(time.Duration(i+1)*time.Hour))
				}
				thumbs := filepath.Join(dir, manifest.ThumbsDirName)
				if err := os.MkdirAll(thumbs, 0o755); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 3; i++ {
					writeArtFile(t, thumbs, fmt.Sprintf("local-%d-250.jpg", i), 30, base.Add(c.hiddenAge+time.Duration(i)*time.Minute))
				}
				return dir
			}
			whole, partial := build(), build()
			before := cacheFilesUnder(t, whole)
			locked := lockDir(t, filepath.Join(partial, manifest.ThumbsDirName))

			if _, err := sweepArtworkCache(context.Background(), whole, c.capBytes); err != nil {
				t.Fatal(err)
			}
			res, err := sweepArtworkCache(context.Background(), partial, c.capBytes)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(locked, 0o755); err != nil {
				t.Fatal(err)
			}
			removed := func(dir string) map[string]bool {
				left := map[string]bool{}
				for _, f := range cacheFilesUnder(t, dir) {
					left[f] = true
				}
				gone := map[string]bool{}
				for _, f := range before {
					if !left[f] {
						gone[f] = true
					}
				}
				return gone
			}
			goneWhole, gonePartial := removed(whole), removed(partial)
			if len(gonePartial) == 0 || res.Evicted != len(gonePartial) {
				t.Fatalf("the partial pass evicted %v (reported %d); it must go on over what it can see", gonePartial, res.Evicted)
			}
			for f := range gonePartial {
				if !goneWhole[f] {
					t.Errorf("the partial pass evicted %s, which the whole-cache pass kept (whole: %v, partial: %v)", f, goneWhole, gonePartial)
				}
				if strings.HasPrefix(f, manifest.ThumbsDirName+"/") {
					t.Errorf("the partial pass evicted %s, in the directory it could not list", f)
				}
			}
		})
	}
}

// TestSweepArtworkCacheCountsAFileItCannotStat — a directory its user may
// list but not search lists its files and cannot stat them. The cap counts
// such a file as one it could not see and goes on; it ended the pass
// before, so a cache holding such a directory was never capped.
func TestSweepArtworkCacheCountsAFileItCannotStat(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root stats any file")
	dir := t.TempDir()
	base := time.Now().Add(-10 * time.Hour)
	var files []string
	for i := 0; i < 6; i++ {
		files = append(files, writeArtFile(t, dir, fmt.Sprintf("local-%d-500.jpg", i), 30, base.Add(time.Duration(i)*time.Hour)))
	}
	thumbs := filepath.Join(dir, manifest.ThumbsDirName)
	writeFixtureFile(t, filepath.Join(thumbs, "local-0-250.jpg"), 10)
	writeFixtureFile(t, filepath.Join(thumbs, "local-1-250.jpg"), 10)
	if err := os.Chmod(thumbs, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(thumbs, 0o755) })

	res, err := sweepArtworkCache(context.Background(), dir, 100)
	if err != nil {
		t.Fatalf("the sweep stopped at a file it could not stat: %v", err)
	}
	if res.Unstatted != 2 || len(res.Unlisted) != 0 {
		t.Errorf("unstatted %d, unlisted %q; want 2 and none", res.Unstatted, res.Unlisted)
	}
	if res.Evicted != 3 {
		t.Errorf("evicted %d files; want the 3 oldest it could see", res.Evicted)
	}
	for _, f := range files[:3] {
		mustGone(t, f)
	}
}

// TestArtworkCapSweeperReportsWhatItCannotSeeOncePerStreak — a pass that
// could not see part of the cache WARNs once when the streak begins and at
// most once a day while it lasts, and the first pass that sees everything
// again says so at Info. A directory the bridge's user cannot list stays
// that way until someone changes it, so a line per pass (every 15 minutes)
// was the same line 96 times a day; and the pass failed with it.
func TestArtworkCapSweeperReportsWhatItCannotSeeOncePerStreak(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root lists any directory")
	dir := t.TempDir()
	writeArtFile(t, dir, "local-a-500.jpg", 30, time.Now().Add(-time.Hour))
	thumbs := filepath.Join(dir, manifest.ThumbsDirName)
	writeFixtureFile(t, filepath.Join(thumbs, "local-a-250.jpg"), 10)
	lockDir(t, thumbs)
	rec := loggingtest.Record(t)
	const (
		unseen = "artwork cache sweep could not see part of the cache"
		again  = "artwork cache sweep sees the whole cache again"
	)
	s := &artworkCapSweeper{dir: dir, capBytes: 1 << 20}
	ctx := context.Background()
	base := time.Now()

	s.pass(ctx, base)
	s.pass(ctx, base.Add(15*time.Minute))
	s.pass(ctx, base.Add(23*time.Hour))
	if got := rec.Failures(unseen); len(got) != 1 {
		t.Fatalf("%d WARN lines in the first day of a streak, want 1: %q", len(got), got)
	}
	if line := rec.Failures(unseen)[0]; !strings.Contains(line, "unlisted=[thumbs]") || !strings.Contains(line, "dir="+dir) {
		t.Errorf("the WARN does not name the directory it could not list: %s", line)
	}
	if got := rec.Failures("artwork cache sweep"); len(got) != 0 {
		t.Errorf("the pass failed over a directory it could not list: %q", got)
	}
	s.pass(ctx, base.Add(24*time.Hour))
	if got := rec.Failures(unseen); len(got) != 2 {
		t.Errorf("%d WARN lines after a day of the streak, want 2: %q", len(got), got)
	}

	if err := os.Chmod(thumbs, 0o755); err != nil {
		t.Fatal(err)
	}
	s.pass(ctx, base.Add(25*time.Hour))
	s.pass(ctx, base.Add(26*time.Hour))
	if got := rec.Lines(again); len(got) != 1 {
		t.Errorf("%d lines saying it sees the whole cache again, want 1: %q", len(got), got)
	}
	if err := os.Chmod(thumbs, 0o000); err != nil {
		t.Fatal(err)
	}
	s.pass(ctx, base.Add(27*time.Hour))
	if got := rec.Failures(unseen); len(got) != 3 {
		t.Errorf("a new streak logged %d WARN lines in all, want 3: %q", len(got), got)
	}
}

// TestArtworkCapSweeperSaysNothingAboutTheFilesystemsLostFound — the
// filesystem's lost+found is not part of the cache that went unseen: a pass
// over it evicts and logs nothing but its eviction.
func TestArtworkCapSweeperSaysNothingAboutTheFilesystemsLostFound(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root lists lost+found")
	dir := t.TempDir()
	base := time.Now().Add(-10 * time.Hour)
	for i := 0; i < 6; i++ {
		writeArtFile(t, dir, fmt.Sprintf("local-%d-500.jpg", i), 30, base.Add(time.Duration(i)*time.Hour))
	}
	lockDir(t, filepath.Join(dir, "lost+found"))
	rec := loggingtest.Record(t)
	s := &artworkCapSweeper{dir: dir, capBytes: 100}
	s.pass(context.Background(), time.Now())
	if got := rec.Failures(); len(got) != 0 {
		t.Errorf("a pass over the filesystem's lost+found warned: %q", got)
	}
	if got := rec.Lines("artwork cache LRU eviction"); len(got) != 1 || !strings.Contains(got[0], "evicted=3") {
		t.Errorf("the pass did not evict the three oldest covers: %q", got)
	}
}

// TestArtworkGCKeepsTheThumbnailsOfAnArtistATrackNames — the console files a
// portrait's derived tiers under manifest.ArtistThumbKey of the artist MBID
// the enricher stamped on the track and fetched the portrait under. The GC
// keeps them while a track row names that artist, and removes those of an
// artist no row names and those filed under an artworkVersion alias, which
// nothing reads (the console resolves an alias before it derives,
// TestAnArtworkAliasFilesItsThumbUnderTheResolvedKey). It kept artwork keys
// alone, and removed every artist thumbnail. The portraits themselves carry
// no size suffix and are out of its scope.
func TestArtworkGCKeepsTheThumbnailsOfAnArtistATrackNames(t *testing.T) {
	store, dir := artworkCacheFixture(t)
	ctx := context.Background()
	if err := store.UpsertTrack(ctx, &manifest.Track{
		Path: "A/01.flac", Size: 1, ModTime: time.Now(), ArtworkMBID: keptArtworkKey, ArtistMBID: keptArtistMBID,
	}); err != nil {
		t.Fatal(err)
	}
	const alias = "0123456789abcdef"
	if n, err := store.SetArtworkVersionAndBumpIndex(ctx, keptArtworkKey, alias); err != nil || n != 1 {
		t.Fatalf("stamp the alias: %d rows, %v", n, err)
	}
	thumb := func(key string, size int) string {
		return filepath.Join(manifest.ThumbsDirName, fmt.Sprintf("%s-%d.jpg", key, size))
	}
	keep := []string{
		keptArtworkKey + "-500.jpg",
		thumb(keptArtworkKey, 250),
		thumb(manifest.ArtistThumbKey(keptArtistMBID), 250),
		thumb(manifest.ArtistThumbKey(keptArtistMBID), 500),
		"artist-" + keptArtistMBID + ".jpg",
		"artist-" + orphanArtistMBID + ".jpg",
		"artist-name-" + strings.Repeat("c", 64) + ".jpg",
	}
	remove := []string{
		thumb(manifest.ArtistThumbKey(orphanArtistMBID), 250),
		thumb(alias, 250),
	}
	for _, p := range append(append([]string{}, keep...), remove...) {
		writeFixtureFile(t, filepath.Join(dir, p), 10)
	}

	rc, stdout, stderr := runArtworkGCOver(store, dir, false)
	if rc != 0 {
		t.Fatalf("rc=%d\nstdout: %s\nstderr: %s", rc, stdout, stderr)
	}
	for _, p := range keep {
		mustExist(t, filepath.Join(dir, p))
	}
	for _, p := range remove {
		mustGone(t, filepath.Join(dir, p))
	}
	if !strings.Contains(stdout, "removed 2 orphan(s), kept 4 known cache file(s), 3 skipped") {
		t.Errorf("summary: %s", stdout)
	}
}

// TestArtworkGCEmptyStoreGuardIsAboutTheCovers — the refusal over an empty
// referenced set is keyed on the ARTWORK keys, whatever artists the rows
// name: the covers are what it protects, and a scanner-extracted one does
// not come back. And it asks whether the walk would remove a file, so a
// cache holding only thumbnails of an artist a row names is nothing to
// refuse over.
func TestArtworkGCEmptyStoreGuardIsAboutTheCovers(t *testing.T) {
	cover := "local-" + strings.Repeat("a", 64) + "-500.jpg"
	for _, c := range []struct {
		name   string
		files  []string
		wantRC int
	}{
		{"a cover, with artists named", []string{cover}, 1},
		{"only the thumbnails of a named artist", []string{
			filepath.Join(manifest.ThumbsDirName, manifest.ArtistThumbKey(keptArtistMBID)+"-250.jpg"),
		}, 0},
		{"the thumbnail of an artist no row names", []string{
			filepath.Join(manifest.ThumbsDirName, manifest.ArtistThumbKey(orphanArtistMBID)+"-250.jpg"),
		}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, dir := artworkCacheFixture(t)
			if err := store.UpsertTrack(context.Background(), &manifest.Track{
				Path: "A/01.flac", Size: 1, ModTime: time.Now(), ArtistMBID: keptArtistMBID,
			}); err != nil {
				t.Fatal(err)
			}
			for _, p := range c.files {
				writeFixtureFile(t, filepath.Join(dir, p), 10)
			}
			rc, stdout, stderr := runArtworkGCOver(store, dir, false)
			if rc != c.wantRC {
				t.Errorf("rc=%d, want %d\nstdout: %s\nstderr: %s", rc, c.wantRC, stdout, stderr)
			}
			if c.wantRC == 1 && !strings.Contains(stderr, "refusing") {
				t.Errorf("stderr does not refuse:\n%s", stderr)
			}
			for _, p := range c.files {
				mustExist(t, filepath.Join(dir, p))
			}
		})
	}
}

// TestArtworkGCWalksALinkedCacheWhereItResolves — a cache directory that is
// a link (a junction on Windows) to a directory on another volume is walked
// there: an orphan in it and one in its thumbs/ are removed, the known
// cover is kept, and the output names them under the configured directory.
// The walk used to see one entry, the link itself, and skip it: "removed 0
// orphan(s), kept 0 known cache file(s), 1 skipped".
func TestArtworkGCWalksALinkedCacheWhereItResolves(t *testing.T) {
	store, dir := artworkCacheFixture(t, keptArtworkKey)
	target := linkedArtworkCache(t, dir)
	kept := filepath.Join(target, keptArtworkKey+"-500.jpg")
	orphans := []string{
		filepath.Join(target, orphanArtworkKey+"-500.jpg"),
		filepath.Join(target, manifest.ThumbsDirName, orphanArtworkKey+"-250.jpg"),
	}
	for _, p := range append([]string{kept}, orphans...) {
		writeFixtureFile(t, p, 10)
	}

	rc, stdout, stderr := runArtworkGCOver(store, dir, true)
	if rc != 0 {
		t.Fatalf("dry run rc=%d\nstdout: %s\nstderr: %s", rc, stdout, stderr)
	}
	for _, want := range []string{
		"would remove: " + filepath.Join(dir, orphanArtworkKey+"-500.jpg"),
		"would remove: " + filepath.Join(dir, manifest.ThumbsDirName, orphanArtworkKey+"-250.jpg"),
		"2 orphan(s) would be removed, 1 kept",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry run does not say %q:\n%s", want, stdout)
		}
	}

	if rc, stdout, stderr := runArtworkGCOver(store, dir, false); rc != 0 {
		t.Fatalf("rc=%d\nstdout: %s\nstderr: %s", rc, stdout, stderr)
	}
	mustExist(t, kept)
	for _, p := range orphans {
		mustGone(t, p)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Errorf("the link itself went: %v", err)
	}
}

// TestArtworkGCEmptyStoreGuardResolvesALinkedCache — the empty-store guard
// walks the cache where the GC does. An empty store over a linked cache
// that holds a cover refuses, and the cover stays; a guard that read the
// link as one entry, beside a GC that walks its target, would wave the
// whole cache through.
func TestArtworkGCEmptyStoreGuardResolvesALinkedCache(t *testing.T) {
	store, dir := artworkCacheFixture(t)
	target := linkedArtworkCache(t, dir)
	cover := filepath.Join(target, "local-"+strings.Repeat("a", 64)+"-500.jpg")
	writeFixtureFile(t, cover, 10)

	rc, stdout, stderr := runArtworkGCOver(store, dir, false)
	if rc != 1 || !strings.Contains(stderr, "refusing") {
		t.Errorf("rc=%d over an empty store and a linked cache holding a cover, want a refusal\nstdout: %s\nstderr: %s", rc, stdout, stderr)
	}
	mustExist(t, cover)
}

// TestSweepArtworkCacheWalksALinkedCache — the size cap over a linked cache
// evicts the oldest covers where the link resolves. It saw the link as one
// entry that is not a cover, counted nothing, and never evicted.
func TestSweepArtworkCacheWalksALinkedCache(t *testing.T) {
	_, dir := artworkCacheFixture(t)
	target := linkedArtworkCache(t, dir)
	base := time.Now().Add(-10 * time.Hour)
	var files []string
	for i := 0; i < 5; i++ {
		files = append(files, writeArtFile(t, target, fmt.Sprintf("local-%d-500.jpg", i), 30, base.Add(time.Duration(i)*time.Hour)))
	}
	res, err := sweepArtworkCache(context.Background(), dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Evicted != 2 || res.Freed != 60 {
		t.Errorf("evicted %d files, %d bytes; want 2 and 60", res.Evicted, res.Freed)
	}
	mustGone(t, files[0])
	mustGone(t, files[1])
	for _, f := range files[2:] {
		mustExist(t, f)
	}
}

// TestArtworkWalksRefuseACacheThatLinksToAFile — a cache directory that is a
// link to a FILE is not a cache. Resolved, a walk would judge the target by
// its own name, and the GC and the cap could remove it; both refuse, and it
// stays.
func TestArtworkWalksRefuseACacheThatLinksToAFile(t *testing.T) {
	store, dir := artworkCacheFixture(t, keptArtworkKey)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), orphanArtworkKey+"-500.jpg")
	writeFixtureFile(t, file, 10)
	if err := os.Symlink(file, dir); err != nil {
		t.Skipf("this host cannot create a symlink to a file: %v", err)
	}
	if rc, stdout, stderr := runArtworkGCOver(store, dir, false); rc != 1 || !strings.Contains(stderr, "not a directory") {
		t.Errorf("rc=%d over a cache that links to a file, want 1 and \"not a directory\"\nstdout: %s\nstderr: %s", rc, stdout, stderr)
	}
	if _, err := sweepArtworkCache(context.Background(), dir, 1); err == nil {
		t.Error("the size cap took a cache that links to a file")
	}
	mustExist(t, file)
}

// TestArtworkWalksRefuseAnEmptyCacheDirectory — "" names no cache. It is
// refused before anything resolves it: filepath.EvalSymlinks("") answers
// ".", and a walk of the working directory would take the cache-shaped
// file planted there.
func TestArtworkWalksRefuseAnEmptyCacheDirectory(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	planted := filepath.Join(cwd, orphanArtworkKey+"-500.jpg")
	writeFixtureFile(t, planted, 10)
	store, _ := artworkCacheFixture(t, keptArtworkKey)

	if rc, stdout, stderr := runArtworkGCOver(store, "", false); rc != 1 {
		t.Errorf("rc=%d over an empty cache directory, want 1\nstdout: %s\nstderr: %s", rc, stdout, stderr)
	}
	if _, err := sweepArtworkCache(context.Background(), "", 1); err == nil {
		t.Error("the size cap took an empty cache directory")
	}
	mustExist(t, planted)
}
