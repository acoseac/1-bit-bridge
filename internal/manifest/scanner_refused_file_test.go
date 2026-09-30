package manifest

// A file its extractor refuses (read whole, and not its format: the DSF, DFF,
// AIFF or WAV walk's answer for a file whose header is not one) is indexed by
// its name, as it always was. The skip gate asked such a row the questions it
// asks every unchanged file (is its version current, has its lyrics sidecar
// changed, is its local-art cache file missing, has its folder's cover
// changed), and re-read the file to answer any that said yes, but the
// extraction of a refused file never reaches the lyrics or the artwork, and
// the version-stale leg (reExtractUnchanged) stamped nothing for a refusal.
// So after an ExtractorVersion bump, or beside a lyrics sidecar with no bump
// at all, the file was re-read, and logged at ERROR, on every scan, forever
// (backlog B145).
//
// These tests drive the real scanner over a DSF whose header is not one
// (writeJunkDSF), counting the audio opens of each scan (artFixture).

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/lyrics"
)

// refusedRel is the refused file's library-relative path in these tests.
const refusedRel = "Artist/Album/bad.dsf"

// refusalLines returns the lines the scans recorded by rec logged about a
// file its extractor refused, on either leg, whose path names base.
func refusalLines(rec *loggingtest.Recorder, base string) []string {
	var out []string
	for _, msg := range []string{"extract", "re-extract (version-stale)"} {
		for _, l := range rec.Lines(msg) {
			if strings.Contains(l, base) {
				out = append(out, l)
			}
		}
	}
	return out
}

// indexRefused writes the refused file into f's library and indexes it, and
// returns what the scan logged from then on.
func indexRefused(t *testing.T, f *artFixture) *loggingtest.Recorder {
	t.Helper()
	f.dir(t, filepath.Dir(refusedRel))
	writeJunkDSF(t, f.path(refusedRel))
	setMTime(t, f.path(refusedRel), t0)
	rec := loggingtest.Record(t)
	if n := f.scan(t, "index"); n != 1 {
		t.Fatalf("fixture: the index read %d audio files, want 1", n)
	}
	mustIndexed(t, f.store, refusedRel)
	if lines := refusalLines(rec, "bad.dsf"); len(lines) != 1 {
		t.Fatalf("fixture: the index logged %q, want the one line a refusal has always had", lines)
	}
	return rec
}

// bumpVersion makes the row at rel read as one an older ExtractorVersion
// wrote, and sets its missing count and enriched_at to values a stamp must
// leave (the count reset, the enrichment kept).
func bumpVersion(t *testing.T, store *Store, rel string) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE tracks SET extractor_version = ?, missing_count = 2, enriched_at = 777 WHERE path = ?`,
		ExtractorVersion-1, rel); err != nil {
		t.Fatal(err)
	}
}

// requireStamped asserts that the row at rel is before, stamped current:
// the same tags, stat and indexed_at (nothing a paired device sees moved),
// its version current and its missing count reset, its enrichment kept.
func requireStamped(t *testing.T, store *Store, rel string, before storedRow) {
	t.Helper()
	after, ok := storedRowAt(t, store, rel)
	if !ok {
		t.Fatal("the row was deleted")
	}
	want := before
	want.extractorVersion = ExtractorVersion
	want.missingCount = 0
	if after != want {
		t.Errorf("the row is\n %+v\nwant it stamped current and otherwise as it was\n %+v", after, want)
	}
	if got := trackColumn(t, store, rel, "enriched_at"); got != 777 {
		t.Errorf("enriched_at = %d, want 777 kept: a refusal re-queues no enrichment", got)
	}
}

// TestScanner_ARefusedFileIsReadOnceAfterAVersionBump is the entry's
// measurement: after an ExtractorVersion bump the refused file is read once,
// its row stamped current and otherwise left as it was, and the scans after
// read it no more. Nothing more is logged: the refusal of this file, at this
// size and mtime, was logged when it was indexed.
func TestScanner_ARefusedFileIsReadOnceAfterAVersionBump(t *testing.T) {
	f := newArtFixture(t)
	rec := indexRefused(t, f)
	bumpVersion(t, f.store, refusedRel)
	before, _ := storedRowAt(t, f.store, refusedRel)

	if n := f.scan(t, "the first scan after the bump"); n != 1 {
		t.Errorf("the refused file was read %d times, want once", n)
	}
	requireStamped(t, f.store, refusedRel, before)
	for _, label := range []string{"the second scan", "the third scan"} {
		if n := f.scan(t, label); n != 0 {
			t.Errorf("%s re-read the refused file %d times, want never: nothing stamps it current", label, n)
		}
	}
	if lines := refusalLines(rec, "bad.dsf"); len(lines) != 1 {
		t.Errorf("refusal lines %q, want only the index's: a refusal is logged once per path, size and mtime", lines)
	}
}

// TestScanner_ASubtreeScanStampsARefusedFileToo: the watcher's subtree scan
// runs the same worker, and converges the same way.
func TestScanner_ASubtreeScanStampsARefusedFileToo(t *testing.T) {
	f := newArtFixture(t)
	indexRefused(t, f)
	bumpVersion(t, f.store, refusedRel)
	before, _ := storedRowAt(t, f.store, refusedRel)

	if n := f.subtreeScan(t, filepath.Dir(refusedRel)); n != 1 {
		t.Errorf("the subtree scan read the refused file %d times, want once", n)
	}
	requireStamped(t, f.store, refusedRel, before)
	if n := f.subtreeScan(t, filepath.Dir(refusedRel)); n != 0 {
		t.Errorf("the next subtree scan re-read the refused file %d times, want never", n)
	}
}

// TestScanner_ARefusedFileIsReadAgainWhenItChanges: the stamp holds only
// while the file is the one refused. Changed, it is read again, and logged
// again, once, for its new size and mtime; readable, it is written with its
// own tags and goes back to the gate every other file takes, so a lyrics
// sidecar added beside it then reaches it.
func TestScanner_ARefusedFileIsReadAgainWhenItChanges(t *testing.T) {
	f := newArtFixture(t)
	rec := indexRefused(t, f)
	p := f.path(refusedRel)

	writeFixtureBytes(t, p, bytes.Repeat([]byte("still not a DSD stream "), 9))
	setMTime(t, p, t0.Add(time.Hour))
	if n := f.scan(t, "the file changed, still refused"); n != 1 {
		t.Errorf("the changed file was read %d times, want once", n)
	}
	if st, err := f.store.GetTrackStat(context.Background(), refusedRel); err != nil || st == nil || st.Size != int64(9*len("still not a DSD stream ")) {
		t.Errorf("the row's stat is %+v (%v), want the changed file's", st, err)
	}
	if n := f.scan(t, "unchanged since"); n != 0 {
		t.Errorf("the scan after re-read the refused file %d times, want never", n)
	}
	if lines := refusalLines(rec, "bad.dsf"); len(lines) != 2 {
		t.Errorf("refusal lines %q, want two: the index's and the changed file's", lines)
	}

	writeMinimalDSF(t, p, 2822400, map[string]string{"title": "Readable"})
	setMTime(t, p, t0.Add(2*time.Hour))
	if n := f.scan(t, "the file readable"); n != 1 {
		t.Errorf("the readable file was read %d times, want once", n)
	}
	if title, _ := rowTitle(t, f.store, refusedRel); title != "Readable" {
		t.Errorf("title %q, want the readable file's own", title)
	}
	writeFixtureBytes(t, filepath.Join(filepath.Dir(p), "bad.lrc"), []byte(syncedLRC))
	if n := f.scan(t, "a lyrics sidecar added"); n != 1 {
		t.Errorf("the readable file was read %d times for its new lyrics sidecar, want once", n)
	}
	if l, err := f.store.GetLyrics(context.Background(), refusedRel); err != nil || l == nil || l.Source != string(lyrics.SourceSidecarLRC) {
		t.Errorf("lyrics %+v (%v), want the sidecar's", l, err)
	}
}

// syncedLRC is a lyrics sidecar the extraction accepts as a synced document.
const syncedLRC = "[00:01.00]The first line\n[00:02.00]The second line\n[00:03.00]The third line\n"

// TestScanner_ARefusedFileWhoseReadDidNotCompleteIsReadAgain keeps B134's
// line: a version-stale refused file whose open fails (EIO from a NAS) is
// not stamped, since what it holds was not read, and the next scan that can
// read it does, stamps it, and is the last.
func TestScanner_ARefusedFileWhoseReadDidNotCompleteIsReadAgain(t *testing.T) {
	f := newArtFixture(t)
	indexRefused(t, f)
	bumpVersion(t, f.store, refusedRel)
	before, _ := storedRowAt(t, f.store, refusedRel)

	counting := f.sc.openAudio
	f.sc.openAudio = func(abs string) (extractSource, error) {
		if filepath.Base(abs) == "bad.dsf" {
			return nil, &fs.PathError{Op: "open", Path: abs, Err: syscall.EIO}
		}
		return counting(abs)
	}
	rec := loggingtest.Record(t)
	scanOnce(t, f.sc, "the open failing")
	after, _ := storedRowAt(t, f.store, refusedRel)
	if after.extractorVersion != ExtractorVersion-1 {
		t.Errorf("extractor_version = %d, want %d kept: a read that did not complete stamps nothing", after.extractorVersion, ExtractorVersion-1)
	}
	requireOneUnreadLine(t, rec, 1, refusedRel, "open")

	f.sc.openAudio = counting
	if n := f.scan(t, "readable again"); n != 1 {
		t.Errorf("the refused file was read %d times, want once", n)
	}
	requireStamped(t, f.store, refusedRel, before)
	if n := f.scan(t, "the scan after"); n != 0 {
		t.Errorf("the scan after re-read the refused file %d times, want never", n)
	}
}

// TestScanner_ARefusedFileIsNotReadForWhatItsExtractionNeverReaches: the
// gate's other questions, asked of a refused row, said yes forever, since
// the extraction of a refused file never reaches the lyrics or the artwork:
// a lyrics sidecar beside it (no version bump needed), and a local-art cache
// file its row names that is missing. The file is read at most once more,
// and never logged again.
func TestScanner_ARefusedFileIsNotReadForWhatItsExtractionNeverReaches(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *artFixture)
	}{
		{"a lyrics sidecar beside it", func(t *testing.T, f *artFixture) {
			writeFixtureBytes(t, filepath.Join(filepath.Dir(f.path(refusedRel)), "bad.lrc"), []byte(syncedLRC))
		}},
		{"a local-art cache file its row names is missing", func(t *testing.T, f *artFixture) {
			if _, err := f.store.db.Exec(`UPDATE tracks SET tags_json = json_set(tags_json, '$.artworkMBID', ?) WHERE path = ?`,
				localArtworkFilePrefix+strings.Repeat("ab", 32), refusedRel); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArtFixture(t)
			rec := indexRefused(t, f)
			tc.setup(t, f)
			before, _ := storedRowAt(t, f.store, refusedRel)

			reads := 0
			for i := 0; i < 3; i++ {
				reads += f.scan(t, "a scan")
			}
			if reads > 1 {
				t.Errorf("three scans read the refused file %d times, want at most once: the gate keeps asking what its extraction never answers", reads)
			}
			if after, _ := storedRowAt(t, f.store, refusedRel); after.tags != before.tags || after.indexedAt != before.indexedAt {
				t.Errorf("the row is %+v, want it as it was (%+v)", after, before)
			}
			if lines := refusalLines(rec, "bad.dsf"); len(lines) != 1 {
				t.Errorf("refusal lines %q, want only the index's", lines)
			}
		})
	}
}

// TestScanner_ARefusalKeepsTheRowAnOlderExtractorWrote: a stricter
// extractor that refuses a file an older one read keeps the row the older
// one wrote (its tags, its lyrics row, its enrichment, its indexed_at) and
// stamps it current, so the file is read once. The version-stale stamp's
// lyrics write would have deleted the lyrics row, since a refusal finds none.
// This file, at this size and mtime, was never refused before, so the
// refusal is logged, once.
func TestScanner_ARefusalKeepsTheRowAnOlderExtractorWrote(t *testing.T) {
	f := newArtFixture(t)
	ctx := context.Background()
	f.dir(t, filepath.Dir(refusedRel))
	p := f.path(refusedRel)
	writeJunkDSF(t, p)
	setMTime(t, p, t0)
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	const body = "[00:01.00]Read by an older extractor\n[00:02.00]Kept\n"
	doc := lyrics.Doc{Format: "lrc", Synced: true, Body: body}
	old := &Track{Path: refusedRel, Size: info.Size(), ModTime: info.ModTime().UTC(),
		Title: "Read Before", Artist: "An Artist", Album: "An Album",
		lyrics: &extractedLyrics{Format: doc.Format, Synced: true, Body: body,
			Source: string(lyrics.SourceSYLT), Tag: lyrics.Tag(doc), SourceMTimeNS: info.ModTime().UnixNano(), SourceSize: info.Size()}}
	if err := f.store.UpsertTrack(ctx, old); err != nil {
		t.Fatal(err)
	}
	bumpVersion(t, f.store, refusedRel)
	before, _ := storedRowAt(t, f.store, refusedRel)
	lyricsBefore, err := f.store.GetLyrics(ctx, refusedRel)
	if err != nil || lyricsBefore == nil {
		t.Fatalf("fixture: no lyrics row (%v)", err)
	}

	rec := loggingtest.Record(t)
	if n := f.scan(t, "the first scan after the bump"); n != 1 {
		t.Errorf("the refused file was read %d times, want once", n)
	}
	requireStamped(t, f.store, refusedRel, before)
	if l, err := f.store.GetLyrics(ctx, refusedRel); err != nil || l == nil || *l != *lyricsBefore {
		t.Errorf("lyrics %+v (%v), want the older extractor's row kept\n %+v", l, err, lyricsBefore)
	}
	if n := f.scan(t, "the scan after"); n != 0 {
		t.Errorf("the scan after re-read the refused file %d times, want never", n)
	}
	if lines := refusalLines(rec, "bad.dsf"); len(lines) != 1 {
		t.Errorf("refusal lines %q, want one: this refusal is new", lines)
	}
}
