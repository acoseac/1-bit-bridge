package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// Backlog B85: `bridge artwork --gc` judged a file by its size suffix, so the
// artist portraits at the top of the cache (`artist-<mbid>.jpg` and the
// name-hashed canonical `artist-name-<sha>.jpg` it hard-links to) were
// "skipped (non-cache file)" whatever the store said, and a portrait whose
// artist no track row names stayed for ever.

// canonicalName returns an `artist-name-<64 hex>.jpg` name made of c.
func canonicalName(c string) string {
	return "artist-name-" + strings.Repeat(c, 64) + ".jpg"
}

// portraitFixture writes a canonical portrait and hard-links each MBID's
// portrait to it, as the enricher does (linkOrCopy).
func portraitFixture(t *testing.T, dir, canonical string, mbids ...string) {
	t.Helper()
	src := filepath.Join(dir, canonical)
	writeFixtureFile(t, src, 64)
	for _, m := range mbids {
		if err := os.Link(src, filepath.Join(dir, "artist-"+m+".jpg")); err != nil {
			t.Fatalf("hard link: %v", err)
		}
	}
}

// storeNamingArtist adds a track row that names artist (and a cover key, so
// the covers' empty-set refusal is not what a case measures).
func storeNamingArtist(t *testing.T, store *manifest.Store, artist string) {
	t.Helper()
	if err := store.UpsertTrack(context.Background(), &manifest.Track{
		Path: "A/01.flac", Size: 1, ModTime: time.Now(), ArtworkMBID: keptArtworkKey, ArtistMBID: artist,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestArtworkGCRemovesThePortraitsOfAnArtistNoTrackNames is the defect. A
// named artist's portrait and its canonical stay. An unnamed artist's
// portrait goes, and so does the canonical it was the last link to, in the
// same run. A canonical nothing links to goes.
func TestArtworkGCRemovesThePortraitsOfAnArtistNoTrackNames(t *testing.T) {
	store, dir := artworkCacheFixture(t)
	storeNamingArtist(t, store, keptArtistMBID)
	portraitFixture(t, dir, canonicalName("a"), keptArtistMBID)
	portraitFixture(t, dir, canonicalName("b"), orphanArtistMBID)
	writeFixtureFile(t, filepath.Join(dir, canonicalName("c")), 64)

	rc, stdout, stderr := runArtworkGCOver(store, dir, false)
	if rc != 0 {
		t.Fatalf("rc=%d\nstdout: %s\nstderr: %s", rc, stdout, stderr)
	}
	mustExist(t, filepath.Join(dir, "artist-"+keptArtistMBID+".jpg"))
	mustExist(t, filepath.Join(dir, canonicalName("a")))
	mustGone(t, filepath.Join(dir, "artist-"+orphanArtistMBID+".jpg"))
	mustGone(t, filepath.Join(dir, canonicalName("b")))
	mustGone(t, filepath.Join(dir, canonicalName("c")))
	if !strings.Contains(stdout, "removed 3 orphan(s), kept 2 known cache file(s), 0 skipped") {
		t.Errorf("summary: %s", stdout)
	}
}

// TestArtworkGCPortraitsDryRunRemovesNothing — a dry run names exactly the
// portraits a real run removes, the last-link canonical included, and
// touches none.
func TestArtworkGCPortraitsDryRunRemovesNothing(t *testing.T) {
	store, dir := artworkCacheFixture(t)
	storeNamingArtist(t, store, keptArtistMBID)
	portraitFixture(t, dir, canonicalName("a"), keptArtistMBID)
	portraitFixture(t, dir, canonicalName("b"), orphanArtistMBID)

	rc, stdout, stderr := runArtworkGCOver(store, dir, true)
	if rc != 0 {
		t.Fatalf("rc=%d\nstdout: %s\nstderr: %s", rc, stdout, stderr)
	}
	for _, name := range []string{"artist-" + keptArtistMBID + ".jpg", canonicalName("a"), "artist-" + orphanArtistMBID + ".jpg", canonicalName("b")} {
		mustExist(t, filepath.Join(dir, name))
	}
	for _, name := range []string{"artist-" + orphanArtistMBID + ".jpg", canonicalName("b")} {
		if !strings.Contains(stdout, "would remove: "+filepath.Join(dir, name)) {
			t.Errorf("the dry run does not name %s:\n%s", name, stdout)
		}
	}
	if strings.Contains(stdout, "would remove: "+filepath.Join(dir, canonicalName("a"))) {
		t.Errorf("the dry run names the canonical a kept portrait links to:\n%s", stdout)
	}
	if !strings.Contains(stdout, "2 orphan(s) would be removed, 2 kept") {
		t.Errorf("summary: %s", stdout)
	}
}

// TestArtworkGCKeepsEveryPortraitWhenNoTrackNamesAnArtist — an empty artist
// set would make every portrait an orphan: the shape of a wrong --config or
// a store never enriched. The portraits stay, with a line saying how to
// remove them, and the run still collects the covers (exit 0). With
// --allow-empty they go.
func TestArtworkGCKeepsEveryPortraitWhenNoTrackNamesAnArtist(t *testing.T) {
	store, dir := artworkCacheFixture(t, keptArtworkKey)
	portraitFixture(t, dir, canonicalName("a"), keptArtistMBID)
	orphanCover := filepath.Join(dir, "11111111-1111-4111-8111-111111111111-500.jpg")
	writeFixtureFile(t, orphanCover, 10)

	rc, stdout, stderr := runArtworkGCOver(store, dir, false)
	if rc != 0 {
		t.Fatalf("rc=%d\nstdout: %s\nstderr: %s", rc, stdout, stderr)
	}
	mustExist(t, filepath.Join(dir, "artist-"+keptArtistMBID+".jpg"))
	mustExist(t, filepath.Join(dir, canonicalName("a")))
	mustGone(t, orphanCover)
	if !strings.Contains(stderr, "--allow-empty") {
		t.Errorf("stderr does not say how to remove them:\n%s", stderr)
	}

	var out, errOut bytes.Buffer
	if rc := runArtworkGC(context.Background(), &out, &errOut, store, dir, false, true); rc != 0 {
		t.Fatalf("--allow-empty: rc=%d\nstdout: %s\nstderr: %s", rc, out.String(), errOut.String())
	}
	mustGone(t, filepath.Join(dir, "artist-"+keptArtistMBID+".jpg"))
	mustGone(t, filepath.Join(dir, canonicalName("a")))
}

// TestArtworkGCPortraitCopyKeepsTheMBIDFile — on a filesystem that refuses
// hard links the enricher copies the canonical to the MBID path
// (linkOrCopy). The MBID copy of a named artist stays: it is what
// `/v1/artist-image` reads. The canonical shares no file with it and goes,
// which costs one Deezer fetch if a new MBID of that name ever needs it.
func TestArtworkGCPortraitCopyKeepsTheMBIDFile(t *testing.T) {
	store, dir := artworkCacheFixture(t)
	storeNamingArtist(t, store, keptArtistMBID)
	writeFixtureFile(t, filepath.Join(dir, canonicalName("a")), 64)
	writeFixtureFile(t, filepath.Join(dir, "artist-"+keptArtistMBID+".jpg"), 64)

	if rc, stdout, stderr := runArtworkGCOver(store, dir, false); rc != 0 {
		t.Fatalf("rc=%d\nstdout: %s\nstderr: %s", rc, stdout, stderr)
	}
	mustExist(t, filepath.Join(dir, "artist-"+keptArtistMBID+".jpg"))
	mustGone(t, filepath.Join(dir, canonicalName("a")))
}

// TestArtworkGCLeavesWhatIsNoPortrait — only the enricher's names at the top
// of the cache are portraits: a name that is not an MBID or a 64-hex hash,
// and a portrait name below the top, are skipped as before.
func TestArtworkGCLeavesWhatIsNoPortrait(t *testing.T) {
	store, dir := artworkCacheFixture(t)
	storeNamingArtist(t, store, keptArtistMBID)
	stray := []string{
		"artist-not-an-mbid.jpg",
		"artist-name-short.jpg",
		filepath.Join("sub", "artist-"+orphanArtistMBID+".jpg"),
		filepath.Join("sub", canonicalName("d")),
	}
	for _, p := range stray {
		writeFixtureFile(t, filepath.Join(dir, p), 10)
	}

	rc, stdout, stderr := runArtworkGCOver(store, dir, false)
	if rc != 0 {
		t.Fatalf("rc=%d\nstdout: %s\nstderr: %s", rc, stdout, stderr)
	}
	for _, p := range stray {
		mustExist(t, filepath.Join(dir, p))
	}
	if !strings.Contains(stdout, "4 skipped") {
		t.Errorf("summary: %s", stdout)
	}
}
