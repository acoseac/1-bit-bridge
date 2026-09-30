//go:build unix

package manifest

// What the scanner reads after its walk is opened as fsutil.OpenAsFile opens
// it, so a path that is no longer a file is refused at once.
//
// The walk judges what it hands the workers (walkedFileInfo, #1070), and a
// worker opens the path later, sometimes much later on a large library: the
// walk runs ahead of the workers. A file replaced in between by a named pipe
// (a rename over it, a link repointed) reached os.Open, which waits for a
// writer with nothing that can cancel the wait, so the worker, and with it
// the scan and the scanner's mutex, stayed until something wrote to the
// pipe. The folder-art lookup needed no swap at all: it read cover.jpg with
// os.ReadFile, and the walk judges only audio-named entries, so a named pipe
// called cover.jpg held every scan that extracted a track of its album.
// These tests make the real entries, which only a POSIX host can.

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// msgNoLongerAFile is the line a scan worker logs for a path the walk handed
// it as a file that is no longer one when the worker opens it.
const msgNoLongerAFile = "audio file replaced after the walk by something that is not a file; nothing is written for it"

// within runs fn and waits at most bound for it, playing the writer on fifos
// if it is still running then (fsutiltest.AwaitPastFIFOs), so a failure
// neither hangs the suite nor leaves fn running under the test's cleanups.
// An open is refused in microseconds; fsutiltest.ServeBound leaves room for
// a loaded race runner, and a scan gets scanBound.
func within(t *testing.T, label string, bound time.Duration, fifos []string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	fsutiltest.AwaitPastFIFOs(t, label, bound, done, fifos...)
}

// TestEveryExtractorRefusesANamedPipeWithoutWaiting: every extension the
// scanner extracts, handed a named pipe where the walk saw a file, answers at
// once with fsutil's refusal, naming the kind. An .iso is expanded by the
// worker, not the dispatcher (TestTheSACDOpenersRefuseANamedPipeWithoutWaiting).
func TestEveryExtractorRefusesANamedPipeWithoutWaiting(t *testing.T) {
	exts := make([]string, 0, len(Ext))
	for ext := range Ext {
		if ext != ".iso" {
			exts = append(exts, ext)
		}
	}
	sort.Strings(exts)
	// Fourteen when this was written; a floor so a table that lost its
	// extensions cannot pass by testing none.
	if len(exts) < 10 {
		t.Fatalf("only %d extensions to extract: %v", len(exts), exts)
	}
	dir := t.TempDir()
	for _, ext := range exts {
		t.Run(ext, func(t *testing.T) {
			p := filepath.Join(dir, "01"+ext)
			fsutiltest.MakeFIFO(t, p)
			var err error
			within(t, "ExtractWithContext("+filepath.Base(p)+")", fsutiltest.ServeBound, []string{p}, func() {
				err = ExtractWithContext(p, &Track{Path: "Music/Album/01" + ext}, nil)
			})
			if got := fsutil.NotAFileKind(err); got != "named pipe" {
				t.Errorf("err %v, refused as %q; want the named pipe refused", err, got)
			}
		})
	}
}

// TestTheSACDOpenersRefuseANamedPipeWithoutWaiting: the scan worker's opener
// for an .iso container, and ExpandSACDISO, answer a named pipe at once.
func TestTheSACDOpenersRefuseANamedPipeWithoutWaiting(t *testing.T) {
	p := filepath.Join(t.TempDir(), "01.iso")
	fsutiltest.MakeFIFO(t, p)
	var (
		sc                 Scanner
		openErr, expandErr error
	)
	within(t, "the .iso openers", fsutiltest.ServeBound, []string{p}, func() {
		if c, err := sc.openSACDContainer(p); err != nil {
			openErr = err
		} else {
			_ = c.Close()
		}
		_, expandErr = ExpandSACDISO(p, "Music/01.iso", 0, time.Time{})
	})
	for label, err := range map[string]error{"openSACDContainer": openErr, "ExpandSACDISO": expandErr} {
		if got := fsutil.NotAFileKind(err); got != "named pipe" {
			t.Errorf("%s: err %v, refused as %q; want the named pipe refused", label, err, got)
		}
	}
}

// TestReadSidecarCandidateRefusesANamedPipeWithoutWaiting: a lyrics sidecar
// replaced by a named pipe after the stat its callers take (applySidecarLyrics
// and the skip gate refuse one that is not a regular file there) yields no
// document, at once.
func TestReadSidecarCandidateRefusesANamedPipeWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	stated := filepath.Join(dir, "stated.lrc")
	if err := os.WriteFile(stated, []byte("[00:01.00]A line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info := statOf(t, stated)
	p := filepath.Join(dir, "01.lrc")
	fsutiltest.MakeFIFO(t, p)
	var ok bool
	within(t, "readSidecarCandidate", fsutiltest.ServeBound, []string{p}, func() {
		_, ok = readSidecarCandidate(p, "01.lrc", info)
	})
	if ok {
		t.Error("a named pipe yielded a lyrics document")
	}
}

// runWorkerOn drives one scan worker over the single path pi, the way the walk
// hands it one, and returns what the worker sent to be written. The worker
// runs within scanBound (within).
func runWorkerOn(t *testing.T, sc *Scanner, root string, pi pathInfo, fifos []string) []*Track {
	t.Helper()
	paths := make(chan pathInfo, 1)
	paths <- pi
	close(paths)
	writes := make(chan *Track, 8)
	var wg sync.WaitGroup
	wg.Add(1)
	within(t, "the scan worker on "+pi.rel, scanBound, fifos, func() {
		sc.runScanWorker(context.Background(), paths, writes, false, map[string]struct{}{filepath.Clean(root): {}}, &wg)
	})
	close(writes)
	var out []*Track
	for tw := range writes {
		out = append(out, tw)
	}
	return out
}

// TestScanWorkerWritesNoRowForAPathThatIsNoLongerAFile drives the scan worker
// with what the walk hands it, the path and the stat of a file, after the file
// was replaced by something that is not one. The worker must answer at once
// and write nothing: the stat it holds describes a file that is gone, and the
// tags it would write are the path's (fillFromPath), which replaced the
// file's own when the worker wrote on (#1070 measured that for a FIFO a writer
// had opened). The next walk sees what is there, and its row goes as a deleted
// file's does.
//
// It drives the worker, not a whole scan: the window is between the walk's
// stat and a worker's open, which the NumCPU workers of a real scan leave no
// way to hold a path in.
func TestScanWorkerWritesNoRowForAPathThatIsNoLongerAFile(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		// row: "" writes no row first; "changed" indexes an older version of
		// the file, so the worker takes the full path; "stale" indexes this
		// version under an older ExtractorVersion, so it takes the
		// version-stale path (reExtractUnchanged).
		row string
		// replace puts something that is not a file at p and returns the
		// named pipe an open that waits would be waiting on, and the kind
		// the worker must name.
		replace func(t *testing.T, f linkedFixture, p string) (fifo, kind string)
	}{
		{"a new file replaced by a named pipe", "01.flac", "", replaceWithFIFO},
		{"a changed file replaced by a named pipe", "01.flac", "changed", replaceWithFIFO},
		{"a version-stale file replaced by a named pipe", "01.flac", "stale", replaceWithFIFO},
		{"a file replaced by a link to a named pipe", "01.flac", "changed", func(t *testing.T, f linkedFixture, p string) (string, string) {
			pipe := filepath.Join(f.parked, "pipe")
			fsutiltest.MakeFIFO(t, pipe)
			removeOrFatal(t, p)
			linkOrSkip(t, pipe, p)
			return pipe, "named pipe"
		}},
		{"a DSF replaced by a named pipe", "02.dsf", "changed", replaceWithFIFO},
		{"an SACD container replaced by a named pipe", "03.iso", "", replaceWithFIFO},
		{"a file replaced by a socket", "01.flac", "changed", func(t *testing.T, _ linkedFixture, p string) (string, string) {
			removeOrFatal(t, p)
			fsutiltest.BindSocket(t, p)
			return "", "socket"
		}},
		{"a file replaced by a directory", "01.flac", "changed", func(t *testing.T, _ linkedFixture, p string) (string, string) {
			removeOrFatal(t, p)
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
			return "", "directory"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinkedFixture(t)
			p := filepath.Join(f.album, tc.file)
			rel := "Music/Album/" + tc.file
			writeWorkerFixture(t, f, p, tc.row)
			pi := pathInfo{abs: p, rel: rel, info: statOf(t, p)}
			fifo, kind := tc.replace(t, f, p)
			var fifos []string
			if fifo != "" {
				fifos = append(fifos, fifo)
			}
			rec := loggingtest.Record(t)

			for _, tw := range runWorkerOn(t, f.sc, f.root, pi, fifos) {
				t.Errorf("the worker wrote a row for %s: title %q, %d bytes", tw.Path, tw.Title, tw.Size)
			}
			lines := rec.Lines(msgNoLongerAFile)
			if len(lines) != 1 || !strings.Contains(lines[0], "path="+rel) || !strings.Contains(lines[0], "kind="+kind) {
				t.Errorf("lines %q, want one naming path=%s kind=%s", lines, rel, kind)
			}
		})
	}
}

// TestScanWorkerStillWritesAFileLeftAlone is the positive control for the
// test above: the same harness, with nothing replaced, writes the file's row.
func TestScanWorkerStillWritesAFileLeftAlone(t *testing.T) {
	for _, row := range []string{"", "changed"} {
		t.Run("row="+row, func(t *testing.T) {
			f := newLinkedFixture(t)
			p := filepath.Join(f.album, "01.flac")
			writeWorkerFixture(t, f, p, row)
			got := runWorkerOn(t, f.sc, f.root, pathInfo{abs: p, rel: "Music/Album/01.flac", info: statOf(t, p)}, nil)
			if len(got) != 1 || got[0].Title != "After" {
				var titles []string
				for _, tw := range got {
					titles = append(titles, tw.Title)
				}
				t.Errorf("wrote %d rows, titles %q; want the file's one row, title \"After\"", len(got), titles)
			}
		})
	}
}

// writeWorkerFixture writes the file at p, titled "After", and the row the
// case asks for (TestScanWorkerWritesNoRowForAPathThatIsNoLongerAFile).
func writeWorkerFixture(t *testing.T, f linkedFixture, p, row string) {
	t.Helper()
	write := func(title string) {
		switch filepath.Ext(p) {
		case ".dsf":
			writeMinimalDSF(t, p, 2822400, map[string]string{"title": title})
		case ".iso":
			if err := os.WriteFile(p, []byte("an image the worker never gets to read"), 0o644); err != nil {
				t.Fatal(err)
			}
		default:
			writeMinimalFLAC(t, p, 44100, 16, map[string]string{"TITLE": title})
		}
	}
	switch row {
	case "":
	case "changed":
		write("Before")
		setMTime(t, p, time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC))
		scanOnce(t, f.sc, "the older version")
	case "stale":
		write("After")
		scanOnce(t, f.sc, "this version")
		rel, err := filepath.Rel(f.root, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.Exec(`UPDATE tracks SET extractor_version = ? WHERE path = ?`,
			ExtractorVersion-1, filepath.ToSlash(rel)); err != nil {
			t.Fatal(err)
		}
		return
	default:
		t.Fatalf("unknown row %q", row)
	}
	write("After")
}

// replaceWithFIFO puts a named pipe where p was.
func replaceWithFIFO(t *testing.T, _ linkedFixture, p string) (string, string) {
	removeOrFatal(t, p)
	fsutiltest.MakeFIFO(t, p)
	return p, "named pipe"
}

func removeOrFatal(t *testing.T, p string) {
	t.Helper()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
}

// TestScanner_AFolderArtCandidateThatIsNotAFileIsSkipped: a cover.jpg,
// folder.jpg, cover.png or folder.png that is not a file (a named pipe, a
// link to one, a link to a device, a socket) holds no scan and becomes no
// cover, in a track's own folder and in the album folder a disc folder's
// track looks in (extractLocalArtwork's parent fallback), and each is refused
// by what it is, the socket by its stat, since no open reaches one. A real
// cover beside another album is still stamped, so the lookup did run.
//
// Until 2026-09-30 the lookup read a candidate with os.ReadFile after a stat
// that judged only its size, so a named pipe called cover.jpg held the scan
// until something wrote to it, and a link to /dev/zero called cover.jpg was
// read until the process ran out of memory (measured, ops/engineering-log.md).
func TestScanner_AFolderArtCandidateThatIsNotAFileIsSkipped(t *testing.T) {
	root := t.TempDir()
	parked := t.TempDir()
	store, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sc := NewScanner([]string{root}, store, t.TempDir())

	album := filepath.Join(root, "Music", "Album")
	disc := filepath.Join(root, "Music", "Boxed", "Disc 1")
	covered := filepath.Join(root, "Music", "Covered")
	for _, d := range []string{album, disc, covered} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeMinimalFLAC(t, filepath.Join(album, "01.flac"), 44100, 16, map[string]string{"TITLE": "Album track"})
	writeMinimalFLAC(t, filepath.Join(disc, "01.flac"), 44100, 16, map[string]string{"TITLE": "Disc track"})
	writeMinimalFLAC(t, filepath.Join(covered, "01.flac"), 44100, 16, map[string]string{"TITLE": "Covered track"})
	if err := os.WriteFile(filepath.Join(covered, "cover.jpg"), encodeSolidImage(t, 64, 64, 80), 0o644); err != nil {
		t.Fatal(err)
	}

	coverPipe := filepath.Join(album, "cover.jpg")
	fsutiltest.MakeFIFO(t, coverPipe)
	parkedPipe := filepath.Join(parked, "pipe")
	fsutiltest.MakeFIFO(t, parkedPipe)
	linkOrSkip(t, parkedPipe, filepath.Join(album, "folder.jpg"))
	linkOrSkip(t, os.DevNull, filepath.Join(album, "cover.png"))
	boxedPipe := filepath.Join(root, "Music", "Boxed", "cover.jpg")
	fsutiltest.MakeFIFO(t, boxedPipe)
	// Last: BindSocket changes the working directory.
	fsutiltest.BindSocket(t, filepath.Join(album, "folder.png"))
	rec := loggingtest.Record(t)

	scanPastFIFOs(t, "full scan", []string{coverPipe, parkedPipe, boxedPipe},
		func() error { _, err := sc.Scan(context.Background()); return err })

	// Each candidate is refused as what it is, and named: three named
	// pipes (one reached through a link), a link to a character device,
	// and a socket, which only a stat names (no open reaches one).
	counts := map[string]int{}
	for _, line := range rec.Lines("folder-art read") {
		for _, kind := range []string{"named pipe", "character device", "socket"} {
			if strings.Contains(line, kind+" is not a file") {
				counts[kind]++
			}
		}
	}
	if counts["named pipe"] != 3 || counts["character device"] != 1 || counts["socket"] != 1 {
		t.Errorf("folder-art refusals by kind %v, want 3 named pipes, 1 character device and 1 socket; lines %q",
			counts, rec.Lines("folder-art read"))
	}

	for rel, title := range map[string]string{
		"Music/Album/01.flac":        "Album track",
		"Music/Boxed/Disc 1/01.flac": "Disc track",
	} {
		tr, err := store.GetTrack(context.Background(), rel)
		if err != nil || tr == nil {
			t.Errorf("%s: no row (%v)", rel, err)
			continue
		}
		if tr.Title != title || tr.ArtworkMBID != "" {
			t.Errorf("%s: title %q, artwork %q; want %q and no cover", rel, tr.Title, tr.ArtworkMBID, title)
		}
	}
	if tr, err := store.GetTrack(context.Background(), "Music/Covered/01.flac"); err != nil || tr == nil ||
		!strings.HasPrefix(tr.ArtworkMBID, "local-") {
		t.Errorf("the album with a real cover: %+v (%v); want its cover stamped", tr, err)
	}
}
