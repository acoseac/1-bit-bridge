package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Backlog B260: the three walks of the artwork cache (`artwork --gc`, its
// empty-store guard, and the serve size cap) pruned nothing by name, so a
// NAS recycle bin, snapshot or metadata directory under the cache, or a
// dot-directory, was walked as cache content: the GC removed what such a
// directory held that was named like an orphan, the guard counted it as
// covers to protect, and the cap counted its bytes and evicted it, oldest
// first. The sidecar walks have passed over the same names since B207.

// skippedCacheDirs are directories a library walk skips, one of each kind:
// a NAS recycle bin and snapshot, Synology's metadata, and a dot-directory.
var skippedCacheDirs = []string{"#recycle", "#snapshot", "@eaDir", ".Trashes"}

// TestArtworkGCPassesOverTheDirectoriesALibraryWalkSkips — an orphan cover
// at the top of the cache goes; the same name inside each skipped directory
// stays, in a dry run and a real one, and neither run names it.
func TestArtworkGCPassesOverTheDirectoriesALibraryWalkSkips(t *testing.T) {
	store, dir := artworkCacheFixture(t, keptArtworkKey)
	orphan := orphanArtworkKey + "-500.jpg"
	writeFixtureFile(t, filepath.Join(dir, orphan), 10)
	for _, d := range skippedCacheDirs {
		writeFixtureFile(t, filepath.Join(dir, d, orphan), 10)
	}

	for _, dryRun := range []bool{true, false} {
		rc, stdout, stderr := runArtworkGCOver(store, dir, dryRun)
		if rc != 0 {
			t.Fatalf("dryRun=%v: rc=%d\nstdout: %s\nstderr: %s", dryRun, rc, stdout, stderr)
		}
		for _, d := range skippedCacheDirs {
			if strings.Contains(stdout, d) {
				t.Errorf("dryRun=%v: the run names a file in %s:\n%s", dryRun, d, stdout)
			}
		}
		want := "removed 1 orphan(s)"
		if dryRun {
			want = "1 orphan(s) would be removed"
		}
		if !strings.Contains(stdout, want) {
			t.Errorf("dryRun=%v: summary does not say %q:\n%s", dryRun, want, stdout)
		}
	}
	mustGone(t, filepath.Join(dir, orphan))
	for _, d := range skippedCacheDirs {
		mustExist(t, filepath.Join(dir, d, orphan))
	}
}

// TestArtworkGCEmptyStoreGuardPassesOverTheDirectoriesALibraryWalkSkips —
// with no artwork key referenced, a cache whose only covers sit in skipped
// directories holds nothing the GC would remove, so it is no reason to
// refuse: the run exits 0. A dry run, so the case reads the guard's verdict
// alone (the GC walk's own pruning is the case above).
func TestArtworkGCEmptyStoreGuardPassesOverTheDirectoriesALibraryWalkSkips(t *testing.T) {
	store, dir := artworkCacheFixture(t)
	cover := "local-" + strings.Repeat("a", 64) + "-500.jpg"
	for _, d := range skippedCacheDirs {
		writeFixtureFile(t, filepath.Join(dir, d, cover), 10)
	}

	rc, stdout, stderr := runArtworkGCOver(store, dir, true)
	if rc != 0 || strings.Contains(stderr, "refusing") {
		t.Errorf("rc=%d, want 0 with no refusal\nstdout: %s\nstderr: %s", rc, stdout, stderr)
	}
	for _, d := range skippedCacheDirs {
		mustExist(t, filepath.Join(dir, d, cover))
	}
}

// TestSweepArtworkCachePassesOverTheDirectoriesALibraryWalkSkips — the size
// cap neither counts nor evicts what the skipped directories hold, though
// it is the oldest and largest content under the cache: the one cover is
// under the cap, so the pass evicts nothing.
func TestSweepArtworkCachePassesOverTheDirectoriesALibraryWalkSkips(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	for _, d := range skippedCacheDirs {
		p := filepath.Join(dir, d, "x-500.jpg")
		writeFixtureFile(t, p, 100)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	writeFixtureFile(t, filepath.Join(dir, "cover-500.jpg"), 30)

	res, err := sweepArtworkCache(context.Background(), dir, 50)
	if err != nil {
		t.Fatal(err)
	}
	if res.Seen != 30 || res.Evicted != 0 {
		t.Errorf("seen %d bytes and evicted %d file(s), want 30 and 0", res.Seen, res.Evicted)
	}
	for _, d := range skippedCacheDirs {
		mustExist(t, filepath.Join(dir, d, "x-500.jpg"))
	}
	mustExist(t, filepath.Join(dir, "cover-500.jpg"))
}

// TestArtworkWalksWalkACacheNamedLikeASkippedDirectory — the configured
// cache directory is walked whatever its own name: only directories BELOW
// it are passed over. A cache named `.artwork` holding an orphan still has
// it removed by the GC and counted and evicted by the size cap.
func TestArtworkWalksWalkACacheNamedLikeASkippedDirectory(t *testing.T) {
	store, _ := artworkCacheFixture(t, keptArtworkKey)
	for _, name := range []string{".artwork", "#recycle"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			orphan := filepath.Join(dir, orphanArtworkKey+"-500.jpg")
			writeFixtureFile(t, orphan, 10)
			if rc, stdout, stderr := runArtworkGCOver(store, dir, false); rc != 0 {
				t.Fatalf("rc=%d\nstdout: %s\nstderr: %s", rc, stdout, stderr)
			}
			mustGone(t, orphan)

			cover := filepath.Join(dir, "cover-500.jpg")
			writeFixtureFile(t, cover, 100)
			res, err := sweepArtworkCache(context.Background(), dir, 50)
			if err != nil {
				t.Fatal(err)
			}
			if res.Seen != 100 || res.Evicted != 1 {
				t.Errorf("seen %d bytes and evicted %d file(s), want 100 and 1", res.Seen, res.Evicted)
			}
		})
	}
}
