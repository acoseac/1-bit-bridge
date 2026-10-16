package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// Backlog B84: the serve size cap (sweepArtworkCache, behind
// artwork.cacheMaxBytes) evicted the oldest `.jpg` files whatever they were,
// covers and portraits that track rows still name among them. Their tracks
// are enriched, so /v1/artwork and /v1/artist-image answered the terminal
// `no_image` for good, and a `local-` cover answered 202 until the next scan
// wrote it again (measured end to end on 67154340: the engineering log's
// B84 entry). The cap keeps what `artwork --gc` keeps, but for the
// console's thumbnails, and never a file written within its grace; it
// evicts the rest, oldest first.

// thirdArtworkKey is a second cover key no track names.
const thirdArtworkKey = "33333333-3333-4333-8333-333333333333"

// writeCapFile writes a cache file of size bytes at path, its directory
// included, modified at mod.
func writeCapFile(t *testing.T, path string, size int, mod time.Time) string {
	t.Helper()
	writeFixtureFile(t, path, size)
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTheSizeCapNeverEvictsACoverATrackRowNames is the defect. One pass
// under a cap far below the cache evicts the cover no row names, and keeps
// an enriched track's MBID cover and a scanner-extracted `local-` cover,
// though they are the oldest files and the cache stays over the cap. On
// main both went in that pass.
func TestTheSizeCapNeverEvictsACoverATrackRowNames(t *testing.T) {
	local := "local-" + strings.Repeat("a", 64)
	store, dir := artworkCacheFixture(t, keptArtworkKey, local)
	old := time.Now().Add(-48 * time.Hour)
	mbidCover := writeArtFile(t, dir, keptArtworkKey+"-1200.jpg", 600, old)
	localCover := writeArtFile(t, dir, local+"-500.jpg", 600, old.Add(time.Minute))
	orphan := writeArtFile(t, dir, orphanArtworkKey+"-1200.jpg", 600, old.Add(2*time.Minute))

	res, err := sweepArtworkCache(context.Background(), store, dir, 100, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	mustExist(t, mbidCover)
	mustExist(t, localCover)
	mustGone(t, orphan)
	if res.Evicted != 1 || res.Referenced != 1200 || res.Left != 1200 {
		t.Errorf("evicted %d, referenced %d bytes, left %d; want 1, 1200 and 1200", res.Evicted, res.Referenced, res.Left)
	}
}

// TestTheSizeCapNeverEvictsAPortraitTheGCKeeps — the cap decides a portrait
// as `artwork --gc` does (portraitVerdicts): a named artist's portrait and
// the canonical it is a hard link to stay; an unnamed artist's portrait
// goes, with the canonical it was the last link to, and so does a canonical
// nothing links to. On main the named one went too.
func TestTheSizeCapNeverEvictsAPortraitTheGCKeeps(t *testing.T) {
	store, dir := artworkCacheFixture(t)
	storeNamingArtist(t, store, keptArtistMBID)
	portraitFixture(t, dir, canonicalName("a"), keptArtistMBID)
	portraitFixture(t, dir, canonicalName("b"), orphanArtistMBID)
	writeFixtureFile(t, filepath.Join(dir, canonicalName("c")), 64)
	// The pass runs an hour on, as the GC's portrait tests do (artworkGCNow),
	// rather than ageing the files: on Windows a hard link's directory entry
	// keeps the times it had when the link was made, so ageing a canonical
	// leaves its portrait link looking freshly written, inside the grace.
	res, err := sweepArtworkCache(context.Background(), store, dir, 10, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	mustExist(t, filepath.Join(dir, "artist-"+keptArtistMBID+".jpg"))
	mustExist(t, filepath.Join(dir, canonicalName("a")))
	mustGone(t, filepath.Join(dir, "artist-"+orphanArtistMBID+".jpg"))
	mustGone(t, filepath.Join(dir, canonicalName("b")))
	mustGone(t, filepath.Join(dir, canonicalName("c")))
	if res.Evicted != 3 || res.Referenced != 128 {
		t.Errorf("evicted %d, referenced %d bytes; want 3 and 128 (a named portrait and its canonical)", res.Evicted, res.Referenced)
	}
}

// TestTheSizeCapEvictsUnreferencedFilesAndThumbnailsOldestFirst — what the
// cap may evict still goes oldest first, down to the low-water mark: a
// thumbnail (even a named cover's, which the console derives again), a
// cover no row names, and a `.jpg` of no shape the bridge writes. The named
// covers stay though they are older, and the newest files it may evict stay
// because the mark is reached without them. On main the oldest named cover
// went first.
func TestTheSizeCapEvictsUnreferencedFilesAndThumbnailsOldestFirst(t *testing.T) {
	local := "local-" + strings.Repeat("b", 64)
	store, dir := artworkCacheFixture(t, keptArtworkKey, local)
	base := time.Now().Add(-10 * time.Hour)
	at := func(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }
	thumbs := filepath.Join(dir, manifest.ThumbsDirName)
	namedCover := writeCapFile(t, filepath.Join(dir, keptArtworkKey+"-1200.jpg"), 100, at(0))
	namedThumb := writeCapFile(t, filepath.Join(thumbs, keptArtworkKey+"-250.jpg"), 100, at(1))
	orphan := writeCapFile(t, filepath.Join(dir, orphanArtworkKey+"-1200.jpg"), 100, at(2))
	namedLocal := writeCapFile(t, filepath.Join(dir, local+"-500.jpg"), 100, at(3))
	stray := writeCapFile(t, filepath.Join(dir, "folder.jpg"), 100, at(4))
	newerOrphan := writeCapFile(t, filepath.Join(dir, thirdArtworkKey+"-500.jpg"), 100, at(5))
	artistThumb := writeCapFile(t, filepath.Join(thumbs, manifest.ArtistThumbKey(keptArtistMBID)+"-250.jpg"), 100, at(6))

	// 700 bytes against a cap of 450 (low water 405): the three oldest it
	// may evict take it to 400.
	res, err := sweepArtworkCache(context.Background(), store, dir, 450, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{namedThumb, orphan, stray} {
		mustGone(t, p)
	}
	for _, p := range []string{namedCover, namedLocal, newerOrphan, artistThumb} {
		mustExist(t, p)
	}
	if res.Evicted != 3 || res.Freed != 300 || res.Referenced != 200 || res.Left != 400 {
		t.Errorf("evicted %d (%d bytes), referenced %d, left %d; want 3 (300), 200 and 400", res.Evicted, res.Freed, res.Referenced, res.Left)
	}
}

// TestTheSizeCapEvictsNothingWhenItCannotReadTheStore — a store read that
// fails decides nothing: the pass evicts no file, not even the ones no row
// names, and says why. An empty keep set in its place would make every
// cover evictable, which is the defect. A pass with no store at all is the
// same refusal, and the sweeper's pass logs it as a WARN with no eviction
// line.
func TestTheSizeCapEvictsNothingWhenItCannotReadTheStore(t *testing.T) {
	store, dir := artworkCacheFixture(t, keptArtworkKey)
	old := time.Now().Add(-48 * time.Hour)
	files := []string{
		writeCapFile(t, filepath.Join(dir, keptArtworkKey+"-1200.jpg"), 100, old),
		writeCapFile(t, filepath.Join(dir, orphanArtworkKey+"-1200.jpg"), 100, old.Add(time.Minute)),
		writeCapFile(t, filepath.Join(dir, manifest.ThumbsDirName, keptArtworkKey+"-250.jpg"), 100, old.Add(2*time.Minute)),
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name  string
		store *manifest.Store
	}{{"a store that fails", store}, {"no store", nil}} {
		res, err := sweepArtworkCache(context.Background(), c.store, dir, 10, time.Now())
		if err == nil || !strings.Contains(err.Error(), "evicts nothing") {
			t.Errorf("%s: err = %v, want a refusal that says the pass evicts nothing", c.name, err)
		}
		if res.Evicted != 0 {
			t.Errorf("%s: evicted %d files", c.name, res.Evicted)
		}
	}
	for _, p := range files {
		mustExist(t, p)
	}

	rec := loggingtest.Record(t)
	s := &artworkCapSweeper{store: store, dir: dir, capBytes: 10}
	s.pass(context.Background(), time.Now())
	if got := rec.Failures("artwork cache sweep"); len(got) != 1 || !strings.Contains(got[0], "evicts nothing") {
		t.Errorf("the pass logged %q, want one WARN saying it evicts nothing", got)
	}
	if got := rec.Lines("artwork cache LRU eviction"); len(got) != 0 {
		t.Errorf("the pass logged an eviction: %q", got)
	}
	for _, p := range files {
		mustExist(t, p)
	}
}

// TestTheSizeCapKeepsAFileWrittenWithinTheGrace — the race: a writer puts
// its file in the cache before the row that names it commits, so a pass can
// read the keep set in between. No file modified within artworkCapGrace of
// the pass goes, whatever the store says: here an enricher's cover whose
// row has not committed, a thumbnail just derived, and one exactly at the
// edge. One a second past it goes. Once the row commits, a pass after the
// grace keeps the cover as named and evicts the rest.
func TestTheSizeCapKeepsAFileWrittenWithinTheGrace(t *testing.T) {
	store, dir := artworkCacheFixture(t)
	// Whole seconds, so every filesystem stores the edge exactly.
	now := time.Now().Truncate(time.Second)
	justWritten := writeCapFile(t, filepath.Join(dir, keptArtworkKey+"-1200.jpg"), 100, now.Add(-time.Minute))
	freshThumb := writeCapFile(t, filepath.Join(dir, manifest.ThumbsDirName, orphanArtworkKey+"-250.jpg"), 100, now.Add(-time.Minute))
	atTheEdge := writeCapFile(t, filepath.Join(dir, orphanArtworkKey+"-1200.jpg"), 100, now.Add(-artworkCapGrace))
	pastTheEdge := writeCapFile(t, filepath.Join(dir, thirdArtworkKey+"-1200.jpg"), 100, now.Add(-artworkCapGrace-time.Second))

	if _, err := sweepArtworkCache(context.Background(), store, dir, 10, now); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{justWritten, freshThumb, atTheEdge} {
		mustExist(t, p)
	}
	mustGone(t, pastTheEdge)

	if err := store.UpsertTrack(context.Background(), &manifest.Track{
		Path: "B/01.flac", Size: 1, ModTime: time.Now(), ArtworkMBID: keptArtworkKey,
	}); err != nil {
		t.Fatal(err)
	}
	res, err := sweepArtworkCache(context.Background(), store, dir, 10, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	mustExist(t, justWritten)
	mustGone(t, freshThumb)
	mustGone(t, atTheEdge)
	if res.Referenced != 100 || res.Left != 100 {
		t.Errorf("referenced %d, left %d; want 100 and 100", res.Referenced, res.Left)
	}
}

// TestTheSizeCapKeepsEveryCanonicalWhenAPortraitWentUnseen — a canonical is
// kept while a kept portrait is the same file, so a pass that listed a
// portrait at the top and could not stat it cannot tell that the canonical
// is unlinked: it keeps every canonical, as a pass over the whole cache
// might have. With every portrait seen, the unlinked canonical is evictable.
func TestTheSizeCapKeepsEveryCanonicalWhenAPortraitWentUnseen(t *testing.T) {
	dir := t.TempDir()
	canonical := writeArtFile(t, dir, canonicalName("a"), 64, time.Now().Add(-48*time.Hour))
	info, err := os.Lstat(canonical)
	if err != nil {
		t.Fatal(err)
	}
	files := []artworkCapFile{{path: canonical, kind: artworkCapPortrait, size: 64, mod: info.ModTime(), info: info}}
	keep := artworkCapKeep{known: map[string]bool{}, artists: map[string]bool{keptArtistMBID: true}}

	if got, _ := artworkCapCandidates(files, keep, false, time.Now()); len(got) != 1 || got[0].path != canonical {
		t.Errorf("with every portrait seen, candidates = %v; want the unlinked canonical", got)
	}
	if got, _ := artworkCapCandidates(files, keep, true, time.Now()); len(got) != 0 {
		t.Errorf("with a portrait unseen, candidates = %v; want none", got)
	}
}

// TestArtworkCapSweeperWarnsOncePerStreakWhenTrackRowsNameMoreThanTheCap —
// when the covers and portraits rows name are more than the cap, a pass
// evicts what it may and the cache stays over. That WARNs once when the
// streak begins and at most once a day while it lasts, naming
// artwork.cacheMaxBytes, and the first pass after which what the rows name
// fits says so at Info. A streak that begins again WARNs again.
func TestArtworkCapSweeperWarnsOncePerStreakWhenTrackRowsNameMoreThanTheCap(t *testing.T) {
	store, dir := artworkCacheFixture(t, keptArtworkKey)
	old := time.Now().Add(-48 * time.Hour)
	named := writeArtFile(t, dir, keptArtworkKey+"-1200.jpg", 100, old)
	rec := loggingtest.Record(t)
	s := &artworkCapSweeper{store: store, dir: dir, capBytes: 50}
	ctx := context.Background()
	base := time.Now()

	s.pass(ctx, base)
	s.pass(ctx, base.Add(15*time.Minute))
	s.pass(ctx, base.Add(23*time.Hour))
	got := rec.Failures(msgArtworkCapOver)
	if len(got) != 1 {
		t.Fatalf("%d WARN lines in the first day of a streak, want 1: %q", len(got), got)
	}
	for _, want := range []string{"referencedBytes=100", "capBytes=50", "artwork.cacheMaxBytes"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the WARN does not say %q: %s", want, got[0])
		}
	}
	mustExist(t, named)
	s.pass(ctx, base.Add(24*time.Hour))
	if got := rec.Failures(msgArtworkCapOver); len(got) != 2 {
		t.Errorf("%d WARN lines after a day of the streak, want 2: %q", len(got), got)
	}

	// The track goes: its cover is evictable, the cache gets under the cap
	// and the streak ends, once.
	if err := store.DeleteTrack(ctx, "A/00.flac"); err != nil {
		t.Fatal(err)
	}
	s.pass(ctx, base.Add(25*time.Hour))
	s.pass(ctx, base.Add(26*time.Hour))
	if got := rec.Lines(msgArtworkCapFits); len(got) != 1 {
		t.Errorf("%d lines saying the cap holds what rows name again, want 1: %q", len(got), got)
	}
	mustGone(t, named)

	if err := store.UpsertTrack(ctx, &manifest.Track{
		Path: "A/00.flac", Size: 1, ModTime: time.Now(), ArtworkMBID: keptArtworkKey,
	}); err != nil {
		t.Fatal(err)
	}
	writeArtFile(t, dir, keptArtworkKey+"-1200.jpg", 100, old)
	s.pass(ctx, base.Add(27*time.Hour))
	if got := rec.Failures(msgArtworkCapOver); len(got) != 3 {
		t.Errorf("a new streak logged %d WARN lines in all, want 3: %q", len(got), got)
	}
	if got := rec.Failures("artwork cache sweep"); len(got) != 0 {
		t.Errorf("a pass failed: %q", got)
	}
}
