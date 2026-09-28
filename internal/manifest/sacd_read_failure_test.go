package manifest

// A read that did not complete is not an answer about an SACD container.
//
// processSACDISO retires every virtual row of a container that expands to
// nothing, at threshold 1, with a tombstone to every paired device, so the
// reader has to keep two outcomes apart that a dropped error merges: a read
// that stopped at the end of the image (structural truth: the bytes are not
// there) and a read that failed (EIO, ETIMEDOUT or ESTALE from a NAS that is
// still serving the file). The iOS reader makes the same split
// (SACDISOFormat.swift's ByteReader contract: past-EOF reads return short
// data, transport failures throw). The scanner half is in
// scanner_sacd_read_failure_test.go.

import (
	"bytes"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

// sacdFault fails every ReadAt that touches [from, to): it hands back n real
// bytes of the image (a short read) and err.
type sacdFault struct {
	from, to int64
	n        int
	err      error
}

// sacdFaultAt returns the fault a read of n bytes at off touches, if any.
func sacdFaultAt(faults []sacdFault, off int64, n int) (sacdFault, bool) {
	for _, f := range faults {
		if off < f.to && off+int64(n) > f.from {
			return f, true
		}
	}
	return sacdFault{}, false
}

// sacdFaultyImage reads an image held in memory and fails the reads its
// faults cover. It counts them, because a fault no read reached proves
// nothing, and every test here asserts the count.
type sacdFaultyImage struct {
	img    []byte
	faults []sacdFault
	failed int
}

func (r *sacdFaultyImage) ReadAt(p []byte, off int64) (int, error) {
	if f, ok := sacdFaultAt(r.faults, off, len(p)); ok {
		r.failed++
		n := 0
		if off >= 0 && off < int64(len(r.img)) {
			n = copy(p[:min(f.n, len(p))], r.img[off:])
		}
		return n, f.err
	}
	return bytes.NewReader(r.img).ReadAt(p, off)
}

// sacdProbeFaults covers the eight bytes of every probe for the master
// signature, under both geometries: the reads whose failures the old
// geometry detection dropped.
func sacdProbeFaults(n int, err error) []sacdFault {
	var out []sacdFault
	for _, g := range []sacdGeometry{sacdPlain2048, sacdRaw2064} {
		for _, lsn := range sacdMasterTOCSectors {
			off := lsn*g.stride + g.payloadOffset
			out = append(out, sacdFault{from: off, to: off + int64(len(sacdMasterSignature)), n: n, err: err})
		}
	}
	return out
}

// sacdSectorFault covers count plain-geometry sectors from first.
func sacdSectorFault(first, count int64, err error) sacdFault {
	return sacdFault{from: first * sacdSectorPayload, to: (first + count) * sacdSectorPayload, err: err}
}

// sacdReadEIO is what a NAS read failure looks like to the expansion: an
// *os.File's read error, naming the file.
var sacdReadEIO = &os.PathError{Op: "read", Path: "/srv/music/Music/Album.iso", Err: syscall.EIO}

// TestSACDExpand_AReadThatDidNotCompleteIsAnError pins each of the three
// reads whose failures used to become "not an SACD": the geometry probe, the
// area TOC, and the DST probe. Each must come back as an error wrapping what
// failed, never as (nil, nil), which the scanner would act on by retiring
// the album's rows. A short read with NO error breaks the io.ReaderAt
// contract and is a failure too.
func TestSACDExpand_AReadThatDidNotCompleteIsAnError(t *testing.T) {
	img := buildSACDImage(t, twoFixtureTracks(), sacdFixtureOptions{})
	for _, tc := range []struct {
		name   string
		faults []sacdFault
		want   error
	}{
		{"every master-signature probe fails", sacdProbeFaults(0, sacdReadEIO), syscall.EIO},
		{"every probe returns three bytes and an error", sacdProbeFaults(3, sacdReadEIO), syscall.EIO},
		{"every probe returns three bytes and no error", sacdProbeFaults(3, nil), errSACDShortRead},
		{"both copies of the area TOC fail", []sacdFault{
			sacdSectorFault(fixAreaStart, fixTOCSectors, sacdReadEIO),
			sacdSectorFault(fixAreaEnd, fixTOCSectors, sacdReadEIO),
		}, syscall.EIO},
		{"the DST probe fails", []sacdFault{
			{from: fixAudioStart * sacdSectorPayload, to: fixAudioStart*sacdSectorPayload + 1, err: sacdReadEIO},
		}, syscall.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &sacdFaultyImage{img: img, faults: tc.faults}
			tracks, err := expandSACD(r, "Music/Album.iso", int64(len(img)), time.Unix(1, 0))
			if r.failed == 0 {
				t.Fatal("no read reached a fault, so the case tests nothing")
			}
			if err == nil {
				t.Fatalf("a read that did not complete answered %d tracks and no error; "+
					"the scanner retires every row of a container that expands to nothing", len(tracks))
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want one wrapping %v", err, tc.want)
			}
			if tracks != nil {
				t.Fatalf("tracks beside an error: %v", tracks)
			}
		})
	}
}

// TestSACDExpand_AFailedCopyFallsBackToTheNext is the other half of the
// rule. A failed read is passed over like a damaged copy, since the disc
// carries more than one (the doubled-TOC mechanism), and its error is
// reported only when no copy answers. Returning the first failure at once
// would keep the rows safe and lose every disc with one bad sector.
func TestSACDExpand_AFailedCopyFallsBackToTheNext(t *testing.T) {
	img := buildSACDImage(t, twoFixtureTracks(), sacdFixtureOptions{})
	for _, tc := range []struct {
		name  string
		fault sacdFault
	}{
		// The probe at 510 fails, so geometry detection answers from 520,
		// and the master TOC read at 510 fails, so it is adopted from 520.
		{"the first master TOC copy is unreadable", sacdSectorFault(sacdMasterTOCSectors[0], 1, sacdReadEIO)},
		{"the area TOC's first copy is unreadable", sacdSectorFault(fixAreaStart, fixTOCSectors, sacdReadEIO)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &sacdFaultyImage{img: img, faults: []sacdFault{tc.fault}}
			tracks, err := expandSACD(r, "Music/Album.iso", int64(len(img)), time.Unix(1, 0))
			if r.failed == 0 {
				t.Fatal("no read reached the fault, so the case tests nothing")
			}
			if err != nil || len(tracks) != 2 {
				t.Fatalf("the next copy must answer: tracks=%d err=%v", len(tracks), err)
			}
		})
	}
}

// sacdEOFEveryRead reports io.EOF beside every read, full ones included, as
// io.ReaderAt permits at the end of a source.
type sacdEOFEveryRead struct{ r io.ReaderAt }

func (e sacdEOFEveryRead) ReadAt(p []byte, off int64) (int, error) {
	n, err := e.r.ReadAt(p, off)
	if err == nil {
		err = io.EOF
	}
	return n, err
}

// sacdUnexpectedEOF reports the end of the source as io.ErrUnexpectedEOF.
type sacdUnexpectedEOF struct{ r io.ReaderAt }

func (u sacdUnexpectedEOF) ReadAt(p []byte, off int64) (int, error) {
	n, err := u.r.ReadAt(p, off)
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// TestSACDExpand_AReadThatStopsAtTheEndIsStillAnAnswer pins what must NOT
// become an error. A truncated image is structural truth, and (nil, nil) is
// the right answer for it: a re-rip that stopped being an SACD still retires
// its rows. And a read is judged by the bytes it returned before the error
// beside them, since a full read may carry io.EOF.
func TestSACDExpand_AReadThatStopsAtTheEndIsStillAnAnswer(t *testing.T) {
	img := buildSACDImage(t, twoFixtureTracks(), sacdFixtureOptions{})
	cutBeforeArea := img[:fixAreaStart*sacdSectorPayload]
	cutBeforeAudio := img[:fixAudioStart*sacdSectorPayload]

	for _, tc := range []struct {
		name string
		r    io.ReaderAt
	}{
		{"cut before the area TOC", bytes.NewReader(cutBeforeArea)},
		{"cut before the first audio sector", bytes.NewReader(cutBeforeAudio)},
		{"cut before the area TOC, ending in ErrUnexpectedEOF", sacdUnexpectedEOF{bytes.NewReader(cutBeforeArea)}},
		{"cut before the first audio sector, ending in ErrUnexpectedEOF", sacdUnexpectedEOF{bytes.NewReader(cutBeforeAudio)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracks, err := expandSACD(tc.r, "Music/Album.iso", 1, time.Unix(1, 0))
			if err != nil || tracks != nil {
				t.Fatalf("a truncated image must answer (nil, nil): tracks=%v err=%v", tracks, err)
			}
		})
	}

	t.Run("every read carries io.EOF beside its bytes", func(t *testing.T) {
		tracks, err := expandSACD(sacdEOFEveryRead{bytes.NewReader(img)}, "Music/Album.iso", 1, time.Unix(1, 0))
		if err != nil || len(tracks) != 2 {
			t.Fatalf("a full read with io.EOF is complete: tracks=%d err=%v", len(tracks), err)
		}
	})
}
