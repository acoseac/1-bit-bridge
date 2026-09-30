package manifest

// The scanner half of the rule that a read that did not complete is not a
// read (backlog B134). A worker writes the row of a file from what the
// extractor read of it, over what the path says (fillFromPath: the file name
// as the title, the folders as the album and the artist). When the file could
// not be opened, or a read of it failed with anything but the end of the file
// (an EIO, ESTALE or ETIMEDOUT from a NAS still serving it), what was read is
// the path's guess, or part of the file: written, it replaced the row of a
// changed file, or became a new file's row, under the file's own size and
// mtime, and the skip gate kept it on every later scan, since the file did not
// change again. So such a file's row is kept as it was, a new one gets none,
// and a later scan reads it again; one line per scan names them.
//
// These tests fail opens and reads through the scanner's opener seam
// (Scanner.openAudio), on every platform; scanner_unreadable_file_test.go
// fails a real open with chmod 0 where a host allows it.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// readFaultFormat writes one audio format as a given version, "Original" or
// "Retagged", whose bytes hold that word where its tags are, and says which
// version a row describes.
type readFaultFormat struct {
	name  string
	file  string
	write func(t *testing.T, path, version string)
	// version names the version a row describes: its title, unless set.
	version func(tr *Track) string
}

// versionOf names the version the row tr describes.
func (f readFaultFormat) versionOf(tr *Track) string {
	if f.version != nil {
		return f.version(tr)
	}
	return tr.Title
}

// dffRateOf is the DFF fixture's sample rate for a version: a scanned DFF row
// carries the path's title whatever its DITI says (the path's guess fills the
// title before the DIIN walk, which fills only an empty one), so the rate is
// what tells the versions apart.
func dffRateOf(version string) uint32 {
	if version == "Original" {
		return 2822400
	}
	return 5644800
}

// writeFixtureBytes writes data to path.
func writeFixtureBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// readFaultFormats is every family of extractor, each read by its own code:
// dhowden behind the FLAC, MP3, MP4 and Ogg walks, the DSF walk and its ID3
// read, the AIFF and WAV chunk walks, the DFF chunk walk.
var readFaultFormats = []readFaultFormat{
	{name: "FLAC", file: "01.flac", write: func(t *testing.T, p, version string) {
		writeMinimalFLAC(t, p, 44100, 16, map[string]string{"TITLE": version})
	}},
	{name: "MP3", file: "02.mp3", write: func(t *testing.T, p, version string) {
		writeMinimalMP3(t, p, map[string]string{"title": version})
	}},
	{name: "DSF", file: "03.dsf", write: func(t *testing.T, p, version string) {
		writeMinimalDSF(t, p, 2822400, map[string]string{"title": version})
	}},
	{name: "M4A", file: "04.m4a", write: func(t *testing.T, p, version string) {
		writeFixtureBytes(t, p, buildMP4WithILST(true, ilstText("\xa9nam", version)))
	}},
	{name: "Ogg FLAC", file: "05.oga", write: func(t *testing.T, p, version string) {
		writeFixtureBytes(t, p, oggFLACFile(255, 1, commentBlock(true, "TITLE="+version)))
	}},
	{name: "AIFF", file: "06.aiff", write: func(t *testing.T, p, version string) {
		writeFixtureBytes(t, p, buildAIFFWithID3(t, buildID3v2_3(map[string]string{"title": version})))
	}},
	{name: "WAV", file: "07.wav", write: func(t *testing.T, p, version string) {
		writeFixtureBytes(t, p, buildWAVWithID3(t, buildID3v2_3(map[string]string{"title": version})))
	}},
	{name: "DFF", file: "08.dff", write: func(t *testing.T, p, version string) {
		writeFixtureBytes(t, p, buildDFFWithDIIN(t, dffRateOf(version), "DSD ", buildDIINSubChunk("DITI", version)))
	}, version: func(tr *Track) string {
		for _, v := range []string{"Original", "Retagged"} {
			if tr.SampleRate != nil && *tr.SampleRate == float64(dffRateOf(v)) {
				return v
			}
		}
		return fmt.Sprintf("no known rate (%v)", tr.SampleRate)
	}},
}

// audioFault is a failure the opener seam injects into one file, the way a
// NAS fails: the open, every seek, or a read of the bytes holding its title.
type audioFault struct {
	name string
	op   string // the operation the scan's line names
	open error  // the open fails with it
	seek error  // every seek fails with it
	read error  // a read of the bytes holding the title fails with it
}

var audioFaults = []audioFault{
	{name: "the open fails with EIO", op: "open", open: syscall.EIO},
	{name: "the open fails with ESTALE", op: "open", open: syscall.ESTALE},
	{name: "a seek fails with ESTALE", op: "seek", seek: syscall.ESTALE},
	{name: "the read of its title fails with EIO", op: "read", read: syscall.EIO},
}

// faultyAudio is an opened audio file whose seeks, or reads of the byte at,
// fail with its fault. Stat and Close are the real file's.
type faultyAudio struct {
	*os.File
	fault audioFault
	at    int64 // the offset of the title's first byte
}

// covers reports whether a read of n bytes at off takes in the title's first
// byte.
func (f *faultyAudio) covers(off int64, n int) bool {
	return f.fault.read != nil && n > 0 && off <= f.at && f.at < off+int64(n)
}

func (f *faultyAudio) Read(p []byte) (int, error) {
	if pos, err := f.File.Seek(0, io.SeekCurrent); err == nil && f.covers(pos, len(p)) {
		return 0, &fs.PathError{Op: "read", Path: f.Name(), Err: f.fault.read}
	}
	return f.File.Read(p)
}

func (f *faultyAudio) ReadAt(p []byte, off int64) (int, error) {
	if f.covers(off, len(p)) {
		return 0, &fs.PathError{Op: "read", Path: f.Name(), Err: f.fault.read}
	}
	return f.File.ReadAt(p, off)
}

func (f *faultyAudio) Seek(offset int64, whence int) (int64, error) {
	if f.fault.seek != nil {
		return 0, &fs.PathError{Op: "seek", Path: f.Name(), Err: f.fault.seek}
	}
	return f.File.Seek(offset, whence)
}

// injectAudioFault makes sc's workers open the file at target through fault,
// and every other file as they would. title is what the file's tags hold; a
// read fault fails the reads of its first byte.
func injectAudioFault(t *testing.T, sc *Scanner, target, title string, fault audioFault) {
	t.Helper()
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(data, []byte(title))
	if fault.read != nil && at < 0 {
		t.Fatalf("fixture: %q not found in %s", title, target)
	}
	sc.openAudio = func(abs string) (extractSource, error) {
		if filepath.Clean(abs) == filepath.Clean(target) && fault.open != nil {
			return nil, &fs.PathError{Op: "open", Path: abs, Err: fault.open}
		}
		f, err := os.Open(abs)
		if err != nil {
			return nil, err
		}
		if filepath.Clean(abs) != filepath.Clean(target) {
			return f, nil
		}
		return &faultyAudio{File: f, fault: fault, at: int64(at)}, nil
	}
}

// storedRow is a row as the store holds it: the columns a scan writes, and
// the JSON the manifest serves.
type storedRow struct {
	tags             string
	size, mtimeNS    int64
	indexedAt        int64
	extractorVersion int
	missingCount     int
}

// storedRowAt reads the row at rel, and false when there is none.
func storedRowAt(t *testing.T, store *Store, rel string) (storedRow, bool) {
	t.Helper()
	var r storedRow
	err := store.db.QueryRow(`SELECT tags_json, size, mtime_ns, indexed_at, extractor_version, missing_count
		FROM tracks WHERE path = ?`, rel).Scan(&r.tags, &r.size, &r.mtimeNS, &r.indexedAt, &r.extractorVersion, &r.missingCount)
	if errors.Is(err, sql.ErrNoRows) {
		return storedRow{}, false
	}
	if err != nil {
		t.Fatalf("row %s: %v", rel, err)
	}
	return r, true
}

// requireVersion asserts that the row at rel describes the fixture's version
// want (readFaultFormat.versionOf).
func requireVersion(t *testing.T, store *Store, format readFaultFormat, rel, want string) {
	t.Helper()
	tr, err := store.GetTrack(context.Background(), rel)
	if err != nil || tr == nil {
		t.Fatalf("%s: no row (%v)", rel, err)
	}
	if got := format.versionOf(tr); got != want {
		t.Errorf("%s: the row describes %q, want %q (title %q)", rel, got, want, tr.Title)
	}
}

// requireOneUnreadLine asserts that the scan logged exactly one line for the
// files it could not read, counting n of them, with rel among them as its
// example when rel is one of them alone, and naming op as the failure.
func requireOneUnreadLine(t *testing.T, rec *loggingtest.Recorder, n int, rel, op string) {
	t.Helper()
	lines := rec.Lines(msgUnreadAudio)
	if len(lines) != 1 {
		t.Fatalf("lines %q, want one", lines)
	}
	for _, want := range []string{fmt.Sprintf("count=%d", n), "example=" + rel, "err=" + op} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line %q, want it to hold %q", lines[0], want)
		}
	}
}

// readFaultCase is one extractor family under one fault, over a fresh
// library: the file's path p and its library-relative path rel.
type readFaultCase struct {
	f      linkedFixture
	format readFaultFormat
	fault  audioFault
	p, rel string
}

// forEachReadFault runs body for every extractor family under every fault,
// each in a subtest of its own.
func forEachReadFault(t *testing.T, body func(t *testing.T, c readFaultCase)) {
	for _, format := range readFaultFormats {
		for _, fault := range audioFaults {
			t.Run(format.name+"/"+fault.name, func(t *testing.T) {
				f := newLinkedFixture(t)
				body(t, readFaultCase{f: f, format: format, fault: fault,
					p: filepath.Join(f.album, format.file), rel: "Music/Album/" + format.file})
			})
		}
	}
}

// markMissing sets the row's missing count to n, as scans that missed it
// would have.
func (c readFaultCase) markMissing(t *testing.T, n int) {
	t.Helper()
	if _, err := c.f.store.db.Exec(`UPDATE tracks SET missing_count = ? WHERE path = ?`, n, c.rel); err != nil {
		t.Fatal(err)
	}
}

// faultScan runs a scan with c's fault injected into its file, which holds
// the version "Retagged", and returns what the scan logged.
func (c readFaultCase) faultScan(t *testing.T) *loggingtest.Recorder {
	t.Helper()
	injectAudioFault(t, c.f.sc, c.p, "Retagged", c.fault)
	rec := loggingtest.Record(t)
	scanOnce(t, c.f.sc, "the fault")
	return rec
}

// requireKept asserts that the row is before, byte for byte and at the same
// indexed_at, but for its missing count, which the walk that saw the file
// reset.
func (c readFaultCase) requireKept(t *testing.T, before storedRow) {
	t.Helper()
	after, ok := storedRowAt(t, c.f.store, c.rel)
	if !ok {
		t.Fatal("the row was deleted")
	}
	before.missingCount = 0
	if after != before {
		t.Errorf("the row changed:\n got  %+v\n want %+v", after, before)
	}
}

// readableScan takes the fault away and scans again.
func (c readFaultCase) readableScan(t *testing.T) {
	t.Helper()
	c.f.sc.openAudio = nil
	scanOnce(t, c.f.sc, "readable again")
}

// TestScanner_AChangedFileWhoseReadDidNotCompleteKeepsItsRow: a file indexed
// once, then changed, whose open, seek or tag read fails during the scan that
// would re-read it, keeps the row it had, byte for byte and at the same
// indexed_at (nothing to send a paired device), with its missing count reset
// as the walk saw the file; the scan says so once. The next scan, reading it
// whole, writes the file's own tags, and logs nothing more: each scan counts
// its own unread files.
func TestScanner_AChangedFileWhoseReadDidNotCompleteKeepsItsRow(t *testing.T) {
	forEachReadFault(t, func(t *testing.T, c readFaultCase) {
		c.format.write(t, c.p, "Original")
		setMTime(t, c.p, time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC))
		scanOnce(t, c.f.sc, "the original")
		requireVersion(t, c.f.store, c.format, c.rel, "Original")
		c.markMissing(t, 2)
		before, _ := storedRowAt(t, c.f.store, c.rel)

		c.format.write(t, c.p, "Retagged")
		rec := c.faultScan(t)
		c.requireKept(t, before)
		requireOneUnreadLine(t, rec, 1, c.rel, c.fault.op)

		c.readableScan(t)
		requireVersion(t, c.f.store, c.format, c.rel, "Retagged")
		requireOneUnreadLine(t, rec, 1, c.rel, c.fault.op)
	})
}

// TestScanner_ANewFileWhoseReadDidNotCompleteGetsNoRow: a file the store has
// no row for, whose open, seek or tag read fails, gets none until a scan
// reads it whole. A row by its name would carry its size and mtime, which the
// skip gate trusts, and the path's guess at its tags, which it keeps.
func TestScanner_ANewFileWhoseReadDidNotCompleteGetsNoRow(t *testing.T) {
	forEachReadFault(t, func(t *testing.T, c readFaultCase) {
		c.format.write(t, c.p, "Retagged")
		rec := c.faultScan(t)
		if title, ok := rowTitle(t, c.f.store, c.rel); ok {
			t.Errorf("a row was made for a file the scan could not read: title %q", title)
		}
		requireOneUnreadLine(t, rec, 1, c.rel, c.fault.op)

		c.readableScan(t)
		requireVersion(t, c.f.store, c.format, c.rel, "Retagged")
		requireOneUnreadLine(t, rec, 1, c.rel, c.fault.op)
	})
}

// TestScanner_AVersionStaleFileWhoseReadDidNotCompleteKeepsItsRow: the
// version-stale leg (reExtractUnchanged), which an ExtractorVersion bump sends
// every row down, keeps a row whose re-read did not complete as it was, stale
// version included, so the next scan tries again. A read failure the parser
// dropped reached its diff as a changed row, replaced the stored one and
// stamped it current, which no later scan re-read.
func TestScanner_AVersionStaleFileWhoseReadDidNotCompleteKeepsItsRow(t *testing.T) {
	forEachReadFault(t, func(t *testing.T, c readFaultCase) {
		c.format.write(t, c.p, "Retagged")
		scanOnce(t, c.f.sc, "this version")
		if _, err := c.f.store.db.Exec(`UPDATE tracks SET extractor_version = ? WHERE path = ?`,
			ExtractorVersion-1, c.rel); err != nil {
			t.Fatal(err)
		}
		c.markMissing(t, 2)
		before, _ := storedRowAt(t, c.f.store, c.rel)

		rec := c.faultScan(t)
		c.requireKept(t, before)
		requireOneUnreadLine(t, rec, 1, c.rel, c.fault.op)

		c.readableScan(t)
		stamped, _ := storedRowAt(t, c.f.store, c.rel)
		if stamped.extractorVersion != ExtractorVersion || stamped.tags != before.tags || stamped.indexedAt != before.indexedAt {
			t.Errorf("read whole, the row is %+v; want it stamped current and otherwise as it was (%+v)", stamped, before)
		}
		requireOneUnreadLine(t, rec, 1, c.rel, c.fault.op)
	})
}

// TestScanner_AFileReadWholeIsWrittenAsItAlwaysWas is the positive control:
// a file the scan read whole is written as it always was, though its parser
// failed on what it holds, and the scan logs no unread line. Each case is an
// answer faultNotingSource counts as complete: the end of a truncated file,
// a seek to an offset before the start of the file that a malformed file's
// own bytes asked for (dhowden looks for an ID3v1 tag 128 bytes back from the
// end of a file shorter than that; a DSF's metadata pointer with its top bit
// set is a negative offset), and a walk that refuses a file as not its
// format. Counted as failed reads, these files would never be indexed.
func TestScanner_AFileReadWholeIsWrittenAsItAlwaysWas(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		write      func(t *testing.T, p string)
		// check asserts what the row holds beyond its being there.
		check func(t *testing.T, tr *Track)
	}{
		{"an MP3 too short for dhowden's ID3v1 look", "09.mp3", func(t *testing.T, p string) {
			writeFixtureBytes(t, p, []byte("not an MP3 frame or a tag, and shorter than 128 bytes"))
		}, func(t *testing.T, tr *Track) {
			if tr.Codec != "MP3" || tr.Title != "09" {
				t.Errorf("codec %q, title %q; want \"MP3\", \"09\"", tr.Codec, tr.Title)
			}
		}},
		{"a DSF whose metadata pointer is a negative offset", "10.dsf", func(t *testing.T, p string) {
			writeMinimalDSF(t, p, 2822400, map[string]string{"title": "Tagged"})
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			data[27] |= 0x80 // the pointer's top bit (little-endian, bytes 20 to 27)
			writeFixtureBytes(t, p, data)
		}, func(t *testing.T, tr *Track) {
			if tr.SampleRate == nil || *tr.SampleRate != 2822400 || tr.Title != "10" {
				t.Errorf("rate %v, title %q; want the fmt chunk's 2822400 and the path's \"10\"", tr.SampleRate, tr.Title)
			}
		}},
		{"a DSF the walk refuses as not its format", "11.dsf", func(t *testing.T, p string) {
			writeFixtureBytes(t, p, bytes.Repeat([]byte("not a DSD stream "), 8))
		}, func(t *testing.T, tr *Track) {
			if tr.Title != "11" {
				t.Errorf("title %q, want the path's \"11\"", tr.Title)
			}
		}},
		{"a FLAC cut short inside its comment block", "12.flac", func(t *testing.T, p string) {
			writeMinimalFLAC(t, p, 44100, 16, map[string]string{"TITLE": "Tagged"})
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			at := bytes.Index(data, []byte("TITLE="))
			if at < 0 {
				t.Fatal("fixture: no comment")
			}
			writeFixtureBytes(t, p, data[:at])
		}, func(t *testing.T, tr *Track) {
			if tr.SampleRate == nil || *tr.SampleRate != 44100 || tr.Title != "12" {
				t.Errorf("rate %v, title %q; want STREAMINFO's 44100 and the path's \"12\"", tr.SampleRate, tr.Title)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinkedFixture(t)
			p := filepath.Join(f.album, tc.file)
			rel := "Music/Album/" + tc.file
			tc.write(t, p)
			rec := loggingtest.Record(t)
			scanOnce(t, f.sc, "the scan")

			tr, err := f.store.GetTrack(context.Background(), rel)
			if err != nil || tr == nil {
				t.Fatalf("no row for a file read whole (%v); lines %q", err, rec.Lines(msgUnreadAudio))
			}
			tc.check(t, tr)
			if lines := rec.Lines(msgUnreadAudio); len(lines) != 0 {
				t.Errorf("a file read whole was counted unread: %q", lines)
			}
		})
	}
}

// TestScanner_AScanOverUnreadFilesRewritesOnlyWhatItRead: in one scan of a
// library holding a file left alone, a changed file it reads and two changed
// files it cannot, only the file it read is rewritten (its indexed_at moves,
// the one thing a paired device's delta keys on); the unread rows stay as
// they were, nothing is deleted or journaled, and one line counts the two.
func TestScanner_AScanOverUnreadFilesRewritesOnlyWhatItRead(t *testing.T) {
	f := newLinkedFixture(t)
	old := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	paths := map[string]string{}
	for _, name := range []string{"01.flac", "02.flac", "03.flac", "04.flac"} {
		p := filepath.Join(f.album, name)
		paths[name] = p
		writeMinimalFLAC(t, p, 44100, 16, map[string]string{"TITLE": "Original"})
		setMTime(t, p, old)
	}
	scanOnce(t, f.sc, "the originals")
	before := map[string]storedRow{}
	for name := range paths {
		before[name], _ = storedRowAt(t, f.store, "Music/Album/"+name)
	}

	for _, name := range []string{"02.flac", "03.flac", "04.flac"} {
		writeMinimalFLAC(t, paths[name], 44100, 16, map[string]string{"TITLE": "Retagged"})
	}
	unread := map[string]bool{"03.flac": true, "04.flac": true}
	f.sc.openAudio = func(abs string) (extractSource, error) {
		if unread[filepath.Base(abs)] {
			return nil, &fs.PathError{Op: "open", Path: abs, Err: syscall.EIO}
		}
		return os.Open(abs)
	}
	rec := loggingtest.Record(t)
	scanOnce(t, f.sc, "two files unreadable")

	for name := range paths {
		rel := "Music/Album/" + name
		after, ok := storedRowAt(t, f.store, rel)
		if !ok {
			t.Errorf("%s: the row was deleted", rel)
			continue
		}
		switch {
		case name == "02.flac":
			if after.indexedAt == before[name].indexedAt || !strings.Contains(after.tags, `"title":"Retagged"`) {
				t.Errorf("%s: the file read was not rewritten: %+v", rel, after)
			}
		case after != before[name]:
			t.Errorf("%s: the row changed:\n got  %+v\n want %+v", rel, after, before[name])
		}
	}
	var journaled int
	if err := f.store.db.QueryRow(`SELECT COUNT(*) FROM manifest_deletions`).Scan(&journaled); err != nil {
		t.Fatal(err)
	}
	if journaled != 0 {
		t.Errorf("%d deletions were journaled", journaled)
	}
	lines := rec.Lines(msgUnreadAudio)
	if len(lines) != 1 || !strings.Contains(lines[0], "count=2") {
		t.Errorf("lines %q, want one counting the two files", lines)
	}
}

// TestScanSubtree_AChangedFileWhoseReadDidNotCompleteKeepsItsRow: the
// watcher's subtree scan answers an unread file as the full scan does, and
// logs its own line.
func TestScanSubtree_AChangedFileWhoseReadDidNotCompleteKeepsItsRow(t *testing.T) {
	f := newLinkedFixture(t)
	p := filepath.Join(f.album, "01.flac")
	rel := "Music/Album/01.flac"
	writeMinimalFLAC(t, p, 44100, 16, map[string]string{"TITLE": "Original"})
	setMTime(t, p, time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC))
	scanOnce(t, f.sc, "the original")
	before, _ := storedRowAt(t, f.store, rel)

	writeMinimalFLAC(t, p, 44100, 16, map[string]string{"TITLE": "Retagged"})
	injectAudioFault(t, f.sc, p, "Retagged", audioFault{op: "read", read: syscall.EIO})
	rec := loggingtest.Record(t)
	if _, err := f.sc.ScanSubtree(context.Background(), f.album); err != nil {
		t.Fatal(err)
	}
	if after, _ := storedRowAt(t, f.store, rel); after != before {
		t.Errorf("the row changed:\n got  %+v\n want %+v", after, before)
	}
	requireOneUnreadLine(t, rec, 1, rel, "read")
}
