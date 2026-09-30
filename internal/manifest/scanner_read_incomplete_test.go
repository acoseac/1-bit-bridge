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

// msgUnreadAudio is the line a scan logs, once, for the audio files it could
// not read.
const msgUnreadAudio = "audio files the scan could not read; a row one had is kept as it was, and none is made for a new one"

// readFaultFormat writes one audio format with a given title.
type readFaultFormat struct {
	name  string
	file  string
	write func(t *testing.T, path, title string)
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
	{"FLAC", "01.flac", func(t *testing.T, p, title string) {
		writeMinimalFLAC(t, p, 44100, 16, map[string]string{"TITLE": title})
	}},
	{"MP3", "02.mp3", func(t *testing.T, p, title string) {
		writeMinimalMP3(t, p, map[string]string{"title": title})
	}},
	{"DSF", "03.dsf", func(t *testing.T, p, title string) {
		writeMinimalDSF(t, p, 2822400, map[string]string{"title": title})
	}},
	{"M4A", "04.m4a", func(t *testing.T, p, title string) {
		writeFixtureBytes(t, p, buildMP4WithILST(true, ilstText("\xa9nam", title)))
	}},
	{"Ogg FLAC", "05.oga", func(t *testing.T, p, title string) {
		writeFixtureBytes(t, p, oggFLACFile(255, 1, commentBlock(true, "TITLE="+title)))
	}},
	{"AIFF", "06.aiff", func(t *testing.T, p, title string) {
		writeFixtureBytes(t, p, buildAIFFWithID3(t, buildID3v2_3(map[string]string{"title": title})))
	}},
	{"WAV", "07.wav", func(t *testing.T, p, title string) {
		writeFixtureBytes(t, p, buildWAVWithID3(t, buildID3v2_3(map[string]string{"title": title})))
	}},
	{"DFF", "08.dff", func(t *testing.T, p, title string) {
		writeFixtureBytes(t, p, buildDFFWithDIIN(t, 2822400, "DSD ", buildDIINSubChunk("DITI", title)))
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

// TestScanner_AChangedFileWhoseReadDidNotCompleteKeepsItsRow: a file indexed
// once, then changed, whose open, seek or tag read fails during the scan that
// would re-read it, keeps the row it had, byte for byte and at the same
// indexed_at (nothing to send a paired device), with its missing count reset
// as the walk saw the file; the scan says so once. The next scan, reading it
// whole, writes the file's own tags.
func TestScanner_AChangedFileWhoseReadDidNotCompleteKeepsItsRow(t *testing.T) {
	for _, format := range readFaultFormats {
		for _, fault := range audioFaults {
			t.Run(format.name+"/"+fault.name, func(t *testing.T) {
				f := newLinkedFixture(t)
				p := filepath.Join(f.album, format.file)
				rel := "Music/Album/" + format.file
				format.write(t, p, "Original")
				setMTime(t, p, time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC))
				scanOnce(t, f.sc, "the original")
				if _, err := f.store.db.Exec(`UPDATE tracks SET missing_count = 2 WHERE path = ?`, rel); err != nil {
					t.Fatal(err)
				}
				before, ok := storedRowAt(t, f.store, rel)
				if !ok {
					t.Fatal("precondition: the original was not indexed")
				}

				format.write(t, p, "Retagged")
				injectAudioFault(t, f.sc, p, "Retagged", fault)
				rec := loggingtest.Record(t)
				scanOnce(t, f.sc, "the fault")

				after, ok := storedRowAt(t, f.store, rel)
				if !ok {
					t.Fatal("the row was deleted")
				}
				before.missingCount = 0
				if after != before {
					t.Errorf("the row changed:\n got  %+v\n want %+v", after, before)
				}
				requireOneUnreadLine(t, rec, 1, rel, fault.op)

				f.sc.openAudio = nil
				scanOnce(t, f.sc, "readable again")
				if title, _ := rowTitle(t, f.store, rel); title != "Retagged" {
					t.Errorf("read whole, the row's title is %q, want \"Retagged\"", title)
				}
			})
		}
	}
}

// TestScanner_ANewFileWhoseReadDidNotCompleteGetsNoRow: a file the store has
// no row for, whose open, seek or tag read fails, gets none until a scan
// reads it whole. A row by its name would carry its size and mtime, which the
// skip gate trusts, and the path's guess at its tags, which it keeps.
func TestScanner_ANewFileWhoseReadDidNotCompleteGetsNoRow(t *testing.T) {
	for _, format := range readFaultFormats {
		for _, fault := range audioFaults {
			t.Run(format.name+"/"+fault.name, func(t *testing.T) {
				f := newLinkedFixture(t)
				p := filepath.Join(f.album, format.file)
				rel := "Music/Album/" + format.file
				format.write(t, p, "Retagged")
				injectAudioFault(t, f.sc, p, "Retagged", fault)
				rec := loggingtest.Record(t)
				scanOnce(t, f.sc, "the fault")

				if title, ok := rowTitle(t, f.store, rel); ok {
					t.Errorf("a row was made for a file the scan could not read: title %q", title)
				}
				requireOneUnreadLine(t, rec, 1, rel, fault.op)

				f.sc.openAudio = nil
				scanOnce(t, f.sc, "readable again")
				if title, _ := rowTitle(t, f.store, rel); title != "Retagged" {
					t.Errorf("read whole, the row's title is %q, want \"Retagged\"", title)
				}
			})
		}
	}
}

// TestScanner_AVersionStaleFileWhoseReadDidNotCompleteKeepsItsRow: the
// version-stale leg (reExtractUnchanged), which an ExtractorVersion bump sends
// every row down, keeps a row whose re-read did not complete as it was, stale
// version included, so the next scan tries again. A read failure dhowden
// swallowed reached its diff as a changed row and replaced the stored one.
func TestScanner_AVersionStaleFileWhoseReadDidNotCompleteKeepsItsRow(t *testing.T) {
	for _, format := range readFaultFormats {
		for _, fault := range audioFaults {
			t.Run(format.name+"/"+fault.name, func(t *testing.T) {
				f := newLinkedFixture(t)
				p := filepath.Join(f.album, format.file)
				rel := "Music/Album/" + format.file
				format.write(t, p, "Retagged")
				scanOnce(t, f.sc, "this version")
				if _, err := f.store.db.Exec(`UPDATE tracks SET extractor_version = ?, missing_count = 2 WHERE path = ?`,
					ExtractorVersion-1, rel); err != nil {
					t.Fatal(err)
				}
				before, _ := storedRowAt(t, f.store, rel)

				injectAudioFault(t, f.sc, p, "Retagged", fault)
				rec := loggingtest.Record(t)
				scanOnce(t, f.sc, "the fault")

				after, ok := storedRowAt(t, f.store, rel)
				if !ok {
					t.Fatal("the row was deleted")
				}
				before.missingCount = 0
				if after != before {
					t.Errorf("the row changed:\n got  %+v\n want %+v", after, before)
				}
				requireOneUnreadLine(t, rec, 1, rel, fault.op)

				f.sc.openAudio = nil
				scanOnce(t, f.sc, "readable again")
				stamped, _ := storedRowAt(t, f.store, rel)
				if stamped.extractorVersion != ExtractorVersion || stamped.tags != before.tags || stamped.indexedAt != before.indexedAt {
					t.Errorf("read whole, the row is %+v; want it stamped current and otherwise as it was (%+v)", stamped, before)
				}
			})
		}
	}
}
