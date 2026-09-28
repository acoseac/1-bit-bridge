package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// `bridge artwork --gc` met a directory it could not list and stopped
// there: `walk artwork dir: …: permission denied`, exit 1 and no summary,
// with the orphans it had walked before that directory already removed,
// the ones after it never looked at, and no flag past it. On a cache that
// is an ext4 volume's mount root, that directory is the volume's own
// lost+found, so every run ended that way, and a fresh install's first
// run, with an empty store, answered "cannot inspect" (2026-09-28).
//
// Its verdict about a file is the file's name against the referenced keys,
// and nothing else: no count over the tree, no ratio, nothing a missing
// directory could change about a file it can see. So it steps over a
// directory it cannot list, names it, and goes on; and it steps over the
// filesystem's lost+found (integrity.IsFilesystemLostFound) without a
// word, as the sidecar inventory does.

// The artwork keys the tests reference and orphan: an MBID a track names,
// and one no track names.
const (
	keptArtworkKey   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	orphanArtworkKey = "11111111-1111-4111-8111-111111111111"
)

// artworkCacheFixture opens a store whose tracks reference keys, and
// returns it with its artwork cache directory, created and empty.
func artworkCacheFixture(t *testing.T, keys ...string) (*manifest.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for i, key := range keys {
		if err := store.UpsertTrack(context.Background(), &manifest.Track{
			Path: fmt.Sprintf("A/%02d.flac", i), Size: 1, ModTime: time.Now(), ArtworkMBID: key,
		}); err != nil {
			t.Fatal(err)
		}
	}
	artworkDir := filepath.Join(dir, "artwork")
	if err := os.MkdirAll(artworkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return store, artworkDir
}

// runArtworkGCOver runs the GC over artworkDir and returns its exit code,
// stdout and stderr.
func runArtworkGCOver(store *manifest.Store, artworkDir string, dryRun bool) (int, string, string) {
	var stdout, stderr bytes.Buffer
	rc := runArtworkGC(context.Background(), &stdout, &stderr, store, artworkDir, dryRun, false)
	return rc, stdout.String(), stderr.String()
}

// TestArtworkGCStepsOverADirectoryItCannotList — an orphan beside a
// derived-tier directory this user cannot list (thumbs/, left root-owned by
// a run under sudo) is removed, the run exits 0 after its summary, and it
// names the directory it could not list, whose files it neither examined
// nor removed. The dry run reads the tree the same way.
func TestArtworkGCStepsOverADirectoryItCannotList(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root lists any directory, so there would be nothing to step over")
	store, dir := artworkCacheFixture(t, keptArtworkKey)
	kept := filepath.Join(dir, keptArtworkKey+"-500.jpg")
	orphan := filepath.Join(dir, orphanArtworkKey+"-500.jpg")
	thumbs := filepath.Join(dir, manifest.ThumbsDirName)
	unexamined := filepath.Join(thumbs, orphanArtworkKey+"-250.jpg")
	for _, p := range []string{kept, orphan, unexamined} {
		writeFixtureFile(t, p, 10)
	}
	lockDir(t, thumbs)

	for _, dryRun := range []bool{true, false} {
		rc, stdout, stderr := runArtworkGCOver(store, dir, dryRun)
		if rc != 0 {
			t.Fatalf("dry run %v: rc=%d over a directory it could not list\nstdout: %s\nstderr: %s", dryRun, rc, stdout, stderr)
		}
		if !strings.Contains(stderr, "could not list") || !strings.Contains(stderr, thumbs) {
			t.Errorf("dry run %v: stderr does not name the directory it could not list:\n%s", dryRun, stderr)
		}
		if !strings.Contains(stdout, "1 director(y/ies) it could not list") {
			t.Errorf("dry run %v: the summary does not count the directory it could not list:\n%s", dryRun, stdout)
		}
	}
	mustExist(t, kept)
	mustGone(t, orphan)
	if err := os.Chmod(thumbs, 0o755); err != nil {
		t.Fatal(err)
	}
	mustExist(t, unexamined)
}

// TestArtworkGCStepsOverTheFilesystemsLostFoundWithoutAWord — a cache that
// is an ext4 volume's mount root holds the volume's root-owned lost+found.
// It is the filesystem's: nothing the bridge caches is in it, so it is
// stepped over and not reported, and the walk goes on to thumbs/, which
// sorts after it and which the old code never reached.
func TestArtworkGCStepsOverTheFilesystemsLostFoundWithoutAWord(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root lists lost+found, so there would be nothing to step over")
	store, dir := artworkCacheFixture(t, keptArtworkKey)
	kept := filepath.Join(dir, keptArtworkKey+"-500.jpg")
	orphans := []string{
		filepath.Join(dir, orphanArtworkKey+"-500.jpg"),
		filepath.Join(dir, manifest.ThumbsDirName, orphanArtworkKey+"-250.jpg"),
	}
	for _, p := range append([]string{kept}, orphans...) {
		writeFixtureFile(t, p, 10)
	}
	lostFound := filepath.Join(dir, "lost+found")
	writeFixtureFile(t, filepath.Join(lostFound, "#12345"), 10)
	lockDir(t, lostFound)

	rc, stdout, stderr := runArtworkGCOver(store, dir, false)
	if rc != 0 {
		t.Fatalf("rc=%d over the filesystem's lost+found\nstdout: %s\nstderr: %s", rc, stdout, stderr)
	}
	mustExist(t, kept)
	for _, p := range orphans {
		mustGone(t, p)
	}
	if strings.Contains(stderr, "lost+found") || strings.Contains(stdout, "could not list") {
		t.Errorf("the filesystem's lost+found was reported:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
}

// TestArtworkGCEmptyStoreGuardReadsTheCacheAsTheWalkDoes — the refusal over
// an empty referenced set asks whether the cache holds a file the GC would
// remove, and it now reads the cache as the walk does. A fresh install
// whose cache is a volume's mount root (an empty store, nothing cached,
// the volume's locked lost+found) is clean, and so is a cache whose only
// other content is a directory the GC cannot list, which it names; a cache
// file it CAN see still refuses, wherever the locked directory sorts. The
// old guard answered "cannot inspect" and exit 1 for all three.
func TestArtworkGCEmptyStoreGuardReadsTheCacheAsTheWalkDoes(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root lists any directory")
	for _, c := range []struct {
		name       string
		locked     string
		cached     bool
		wantRC     int
		wantStderr string
	}{
		{"a fresh install on a volume's mount root", "lost+found", false, 0, ""},
		{"only a directory it cannot list", "0-locked", false, 0, "could not list"},
		{"a cache file it can see", "0-locked", true, 1, "refusing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, dir := artworkCacheFixture(t)
			if c.cached {
				writeFixtureFile(t, filepath.Join(dir, "local-"+strings.Repeat("a", 64)+"-500.jpg"), 10)
			}
			lockDir(t, filepath.Join(dir, c.locked))
			rc, stdout, stderr := runArtworkGCOver(store, dir, false)
			if rc != c.wantRC {
				t.Errorf("rc=%d, want %d\nstdout: %s\nstderr: %s", rc, c.wantRC, stdout, stderr)
			}
			if c.wantStderr != "" && !strings.Contains(stderr, c.wantStderr) {
				t.Errorf("stderr does not say %q:\n%s", c.wantStderr, stderr)
			}
			if strings.Contains(stderr, "cannot inspect") {
				t.Errorf("the guard failed on a directory the walk steps over:\n%s", stderr)
			}
		})
	}
}

// TestArtworkGCStillFailsOnACacheItCannotList — the cache directory itself
// is not a directory inside the cache: a GC that cannot list it can do
// nothing, and says so with exit 1, as it always has.
func TestArtworkGCStillFailsOnACacheItCannotList(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root lists any directory")
	store, dir := artworkCacheFixture(t, keptArtworkKey)
	writeFixtureFile(t, filepath.Join(dir, orphanArtworkKey+"-500.jpg"), 10)
	lockDir(t, dir)
	if rc, stdout, stderr := runArtworkGCOver(store, dir, false); rc == 0 {
		t.Errorf("rc=0 over a cache it could not list\nstdout: %s\nstderr: %s", stdout, stderr)
	}
}
