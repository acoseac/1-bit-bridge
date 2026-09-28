//go:build unix

package manifest

// An audio-named entry that is not a file is not a track: a named pipe, a
// socket, a device, or a link to one.
//
// A scan worker opens what the walk hands it, and opening a named pipe waits
// for a writer, with nothing to cancel the wait. Until 2026-09-28 a FIFO
// named 01.flac therefore held a worker, and with it the scan, forever: Scan
// holds the scanner's mutex for its whole run, and IsScanning stayed true. A
// link to a device or a socket was indexed from its path alone. The walk now
// stats what an entry names and hands the workers only what opens as a file
// (walkedFileInfo). These tests make the real entries, which only a POSIX
// host can: syscall.Mkfifo, and a Unix socket bound by name.
//
// A row at such a path is reaped like a deleted file's, after the same
// missing-count grace: the walk stat'ed the entry and knows what is there.
// "We could not see this path" is the other case, a stat that FAILED, whose
// row is kept (TestScanner_ALinkWhoseTargetWentAwayKeepsItsRow).

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// scanBound is how long a scan of these fixtures may take: a few files, well
// under a second, with room for a loaded race runner.
const scanBound = 10 * time.Second

// mkfifo makes a named pipe at p.
func mkfifo(t *testing.T, p string) {
	t.Helper()
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Fatalf("mkfifo %s: %v", p, err)
	}
}

// bindSocket makes a Unix socket at p. A socket's address holds at most 104
// bytes on macOS, which a test's temp dir overruns, so it is bound by its
// name from inside its directory.
func bindSocket(t *testing.T, p string) {
	t.Helper()
	t.Chdir(filepath.Dir(p))
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Base(p), Net: "unix"})
	if err != nil {
		t.Fatalf("listen on %s: %v", p, err)
	}
	// Closing a listener removes its socket unless told not to, and the
	// entry has to outlive it.
	l.SetUnlinkOnClose(false)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

// scanPastFIFOs runs scan and waits at most scanBound for it. A scan still
// running then has a worker blocked opening one of fifos, which is the
// defect: the helper reports it, then plays the writer (opens each named pipe
// for writing, which lets a blocked open return, and closes it at once) until
// the scan returns, so the failure neither hangs the suite nor leaves a scan
// running under the test's cleanups.
func scanPastFIFOs(t *testing.T, label string, fifos []string, scan func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- scan() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		return
	case <-time.After(scanBound):
	}
	t.Errorf("%s: still running after %v, a worker blocked opening a named pipe", label, scanBound)
	giveUp := time.After(30 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s: %v", label, err)
			}
			return
		case <-giveUp:
			t.Fatalf("%s: still running 30 s after its named pipes were first written to", label)
		case <-tick.C:
			for _, p := range fifos {
				// Nonblocking, so that with no reader waiting it fails
				// at once (ENXIO) rather than waiting for one.
				if w, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = w.Close()
				}
			}
		}
	}
}

// requireNoRowsAt asserts that nothing is indexed at rel, nor under it, where
// an SACD container's virtual rows would sit.
func requireNoRowsAt(t *testing.T, store *Store, label, rel string) {
	t.Helper()
	if tr, err := store.GetTrack(context.Background(), rel); err != nil || tr != nil {
		if tr != nil {
			t.Errorf("%s: a row at %s, title %q, %d bytes", label, rel, tr.Title, tr.Size)
		} else {
			t.Errorf("%s: GetTrack(%s): %v", label, rel, err)
		}
	}
	if under := sacdTrackPathsUnder(t, store, rel); len(under) != 0 {
		t.Errorf("%s: rows under %s: %v", label, rel, under)
	}
}

// TestScanner_AnEntryThatIsNotAFileIsNotATrack: each kind of entry that is
// not a file, named like a track, mints no row in a full scan or a subtree
// scan, holds neither scan, and each scan says so once. The file beside it is
// indexed, so the scan did walk the album.
func TestScanner_AnEntryThatIsNotAFileIsNotATrack(t *testing.T) {
	for _, tc := range []struct {
		name, entry, kind string
		// create makes the entry at p and returns the named pipe a blocked
		// open would be waiting on, when there is one.
		create func(t *testing.T, f linkedFixture, p string) string
	}{
		{"a named pipe", "01.flac", "named pipe", func(t *testing.T, _ linkedFixture, p string) string {
			mkfifo(t, p)
			return p
		}},
		{"a link to a named pipe", "01.flac", "named pipe", func(t *testing.T, f linkedFixture, p string) string {
			pipe := filepath.Join(f.parked, "pipe")
			mkfifo(t, pipe)
			linkOrSkip(t, pipe, p)
			return pipe
		}},
		{"an SACD container that is a named pipe", "01.iso", "named pipe", func(t *testing.T, _ linkedFixture, p string) string {
			mkfifo(t, p)
			return p
		}},
		{"a link to a device", "01.flac", "character device", func(t *testing.T, _ linkedFixture, p string) string {
			linkOrSkip(t, os.DevNull, p)
			return ""
		}},
		{"a socket", "01.flac", "socket", func(t *testing.T, _ linkedFixture, p string) string {
			bindSocket(t, p)
			return ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinkedFixture(t)
			ctx := context.Background()
			writeMinimalFLAC(t, filepath.Join(f.album, "02.flac"), 44100, 16, map[string]string{"TITLE": "A file"})
			var fifos []string
			if fifo := tc.create(t, f, filepath.Join(f.album, tc.entry)); fifo != "" {
				fifos = append(fifos, fifo)
			}
			rel := "Music/Album/" + tc.entry
			rec := loggingtest.Record(t)

			scanPastFIFOs(t, "full scan", fifos, func() error { _, err := f.sc.Scan(ctx); return err })
			requireNoRowsAt(t, f.store, "full scan", rel)
			scanPastFIFOs(t, "subtree scan", fifos, func() error { _, err := f.sc.ScanSubtree(ctx, f.album); return err })
			requireNoRowsAt(t, f.store, "subtree scan", rel)

			if got := titleOf(t, f.store, "Music/Album/02.flac"); got != "A file" {
				t.Errorf("the file beside it: title %q", got)
			}
			requireNotFilesLines(t, rec.Lines(msgNotFiles), 2, 1, rel, tc.kind, f.root, f.parked)
		})
	}
}

// TestScanner_AFileReplacedByANamedPipeLosesItsRow pins what an entry that is
// not a file does to a row at its path. The walk stat'ed the entry and knows
// what is there, so the row is reaped like a deleted file's (the test scanner
// reaps a missing row at the first scan; a bridge waits
// deleteAfterMissingScans scans first). The full walk meets a named pipe where
// a file was, the subtree walk a link to one, and the file beside them is
// left as it was.
func TestScanner_AFileReplacedByANamedPipeLosesItsRow(t *testing.T) {
	f := newLinkedFixture(t)
	ctx := context.Background()
	for _, name := range []string{"01.flac", "02.flac", "03.flac"} {
		writeMinimalFLAC(t, filepath.Join(f.album, name), 44100, 16, map[string]string{"TITLE": name})
	}
	scanOnce(t, f.sc, "initial")
	kept := indexedAt(t, f.store, "Music/Album/03.flac")

	first := filepath.Join(f.album, "01.flac")
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	mkfifo(t, first)
	scanPastFIFOs(t, "full scan", []string{first}, func() error { _, err := f.sc.Scan(ctx); return err })
	requireNoRowsAt(t, f.store, "full scan", "Music/Album/01.flac")

	second := filepath.Join(f.album, "02.flac")
	pipe := filepath.Join(f.parked, "pipe")
	mkfifo(t, pipe)
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	linkOrSkip(t, pipe, second)
	scanPastFIFOs(t, "subtree scan", []string{pipe}, func() error { _, err := f.sc.ScanSubtree(ctx, f.album); return err })
	requireNoRowsAt(t, f.store, "subtree scan", "Music/Album/02.flac")

	if got := indexedAt(t, f.store, "Music/Album/03.flac"); got != kept {
		t.Errorf("the file beside them was rewritten: indexed_at %d, was %d", got, kept)
	}
}
