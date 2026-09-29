package manifest

import (
	"bufio"
	"encoding/binary"
	"io"
)

// dhowden/tag keeps an ID3v2 tag's frames in one map keyed by frame id, and
// gives every repeat of an id a key of its own by counting up from id_0
// (id3v2.go, readID3v2Frames, at the version go.mod pins):
//
//	rawName := name
//	if _, ok := result[rawName]; ok {
//		for i := 0; ok; i++ {
//			rawName = name + "_" + strconv.Itoa(i)
//			_, ok = result[rawName]
//		}
//	}
//
// So the k-th copy of an id costs k-1 lookups, each of a string built for it,
// and n copies cost n(n-1)/2. Measured through tag.ReadFrom on an ID3v2.4 tag
// of n identical 12-byte TIT2 frames: 1,000 took 26 ms, 4,000 337 ms, and
// 16,000 (192 KB) 6.0 s, allocating 1.9 GB on the way. The header's size
// field admits a tag of about 512 MiB (dhowden does not mask its synchsafe
// bytes), tens of millions of frames: hours of one scan worker, while Scan
// holds the scanner's mutex and every later scan waits on it (backlog B101).
//
// The number of frames costs too, linearly, and with a large constant: every
// frame is a map entry (about 150 bytes of live heap for an 11-byte frame),
// and populateFromTagMetadata's lookups walk the map some fifty times. A tag
// of 1,000,000 distinct frames (12 MB) took 3.7 s to extract and held 160 MB;
// the 512 MiB the header admits would hold gigabytes.
//
// This file walks a tag the way dhowden will read it, counting what dhowden
// would store, and refuses one that passes either bound below before dhowden
// sees it. A refused file is one whose tags dhowden could not read, as for the
// picture guard beside it (dhowden_picture_guard.go): a Warn, the folder-art
// fallback, no tags.

// maxID3v2RenameLookups bounds the lookups dhowden makes renaming repeated
// frames, summed over every id: one id repeated 2,896 times (2,896·2,895/2
// lookups fit, 2,897 copies do not), about 0.2 s of renaming on the dev Mac.
// A bound per id would not do: n ids each repeated up to it cost n times as
// much. Taggers repeat an id a handful of times (COMM, TXXX, APIC, PRIV,
// GEOB), and chapters (CHAP) are the most repeated real frame, hundreds in a
// long audiobook. The strings the renaming builds are what hold it to 2^22:
// about 15 bytes a lookup, so at the bound 64 MB, within the 64 MiB the
// extractor fuzz targets' allocation property allows an extraction whatever
// the file (extractionAllocLimit), which a tag the guard passes has to meet.
const maxID3v2RenameLookups = 1 << 22

// maxID3v2Frames bounds the frames dhowden stores from one tag. A real tag
// holds tens of frames, and a chaptered audiobook a few hundred more.
const maxID3v2Frames = 1 << 16

// The two bounds, as a refusal names them.
const (
	id3v2RepeatsPastBound = "a frame id repeated past the renaming bound"
	id3v2FramesPastBound  = "more frames than the tag bound"
)

// id3v2Refusal names the bound a tag crossed and where the walk was.
type id3v2Refusal struct {
	What    string // id3v2RepeatsPastBound or id3v2FramesPastBound
	Frame   string // the id of the frame that crossed it
	Copies  int    // copies of Frame counted, that one included
	Frames  int    // frames counted
	Lookups uint64 // renaming lookups counted
}

// dhowdenID3v2WithinBudget reports whether tag.ReadFrom, reading rs from its
// current offset, stays within both bounds on the ID3v2 tag it reads, and
// when it would not, where it crossed one. It mirrors ReadFrom's dispatch
// (dhowdenReaderFor): an ID3v2 tag at the start, or the one a "DSD " stream's
// pointer names (ReadDSFTags, which seeks to it from the start of the
// underlying stream). Every other stream it passes: they reach no ID3v2
// reader. rs is left at the offset it came in at.
func dhowdenID3v2WithinBudget(rs io.ReadSeeker) (bool, id3v2Refusal) {
	st, ok := openDhowdenStream(rs)
	if !ok {
		return true, id3v2Refusal{}
	}
	w := newID3v2Walk()
	within := true
	if head, ok := st.head(); ok {
		switch dhowdenReaderFor(head) {
		case dhowdenReadsID3v2:
			within = w.tag(st.from())
		case dhowdenReadsDSF:
			// "DSD ", then 16 bytes ReadDSFTags seeks over, then the pointer.
			var p [8]byte
			if readFullAt(st.from(), 20, p[:]) {
				if at := int64(binary.LittleEndian.Uint64(p[:])); at >= 0 && at < st.end {
					within = w.tag(io.NewSectionReader(st.ra, at, st.end-at))
				}
			}
		}
	}
	if !st.restore() {
		return true, id3v2Refusal{}
	}
	return within, w.refusal
}

// id3v2TagWithinBudget is dhowdenID3v2WithinBudget for a caller that hands
// dhowden's ReadID3v2Tags a reader already at the tag: the DSF extractor at
// its metadata pointer, and the AIFF and WAV walkers with an ID3 chunk.
func id3v2TagWithinBudget(rs io.ReadSeeker) (bool, id3v2Refusal) {
	st, ok := openDhowdenStream(rs)
	if !ok {
		return true, id3v2Refusal{}
	}
	w := newID3v2Walk()
	within := w.tag(st.from())
	if !st.restore() {
		return true, id3v2Refusal{}
	}
	return within, w.refusal
}

// id3v2RefusedMessage is the one line a refused tag costs.
const id3v2RefusedMessage = "ID3v2 tag has more frames than dhowden reads in bounded time; skipping tag read"

func warnID3v2Refused(absPath string, t *Track, r id3v2Refusal) {
	scanLogger.Warn(id3v2RefusedMessage,
		"path", trackLogPath(absPath, t), "bound", r.What, "frame", r.Frame,
		"copies", r.Copies, "frames", r.Frames, "lookups", r.Lookups)
}

// id3v2Walk counts what dhowden stores from one tag.
type id3v2Walk struct {
	maxFrames  int
	maxLookups uint64
	copies     map[string]int // frames stored per id
	frames     int
	lookups    uint64
	// refusal is set once a bound is crossed; its What stays empty while the
	// walk is within both.
	refusal id3v2Refusal
}

func newID3v2Walk() *id3v2Walk {
	return &id3v2Walk{maxFrames: maxID3v2Frames, maxLookups: maxID3v2RenameLookups, copies: map[string]int{}}
}

// store records a frame dhowden stores under id: the lookups its renaming
// costs (one per copy already stored), and the frame. It reports whether the
// walk is still within both bounds. The copies map never holds more than
// maxFrames+1 ids, since the walk stops there.
func (w *id3v2Walk) store(id string) bool {
	n := w.copies[id]
	w.copies[id] = n + 1
	w.frames++
	w.lookups += uint64(n)
	what := ""
	switch {
	case w.lookups > w.maxLookups:
		what = id3v2RepeatsPastBound
	case w.frames > w.maxFrames:
		what = id3v2FramesPastBound
	default:
		return true
	}
	w.refusal = id3v2Refusal{What: what, Frame: id, Copies: n + 1, Frames: w.frames, Lookups: w.lookups}
	return false
}

// tag mirrors tag.ReadID3v2Tags reading src from its start: the header
// (readID3v2Header), the extended header it reads past, then the frames
// (readID3v2Frames), through the unsynchronisation filter when the header
// sets it. It returns false once a bound is crossed.
//
// It stops where dhowden stops: at a read dhowden cannot complete (dhowden
// then returns an error, and the file has no tags, but the lookups made up to
// there were made), and where dhowden's loop ends. It reads on in one place
// dhowden may not: dhowden stops at the frame that takes its offset past the
// tag's size when it does not know the frame's id (validID3Frame), and that
// is the last frame either way, so counting it adds one frame's lookups at
// most. It does not look inside a frame, so it also reads on past a frame
// whose contents dhowden fails to parse (dhowden returns an error there,
// after that frame's renaming), which can only add to the count, and a file
// dhowden fails on has no tags whether or not it is refused.
//
// Without unsynchronisation it reads the headers and skips each payload by
// its length: no payload is read, as the picture guard reads none. With it,
// where a frame ends depends on every 0xFF 0x00 before it, so the frames are
// read through the filter, payloads included (dhowden reads them one byte per
// Read call, so the walk is the small part of that file's cost).
func (w *id3v2Walk) tag(src byteSource) bool {
	var h [10]byte
	if !readFullAt(src, 0, h[:]) || string(h[:3]) != "ID3" {
		return true
	}
	version := h[3]
	if version < 2 || version > 4 {
		return true // dhowden: "ID3 version: …, expected: 2, 3 or 4"
	}
	unsync, extended := h[5]&0x80 != 0, h[5]&0x40 != 0
	size := sevenBitChunked(h[6:10])
	pos, offset := int64(len(h)), uint(len(h))
	if extended && version != 2 {
		// Read raw, before the filter: its length, then the bytes it counts.
		// Version 3 counts the bytes after the length and does not add the 4
		// to offset; version 4 counts the length's own 4, so a length under 4
		// wraps below zero, and readBytes reads nothing for it (readBytesTakes).
		var b [4]byte
		if !readFullAt(src, pos, b[:]) {
			return true
		}
		pos += int64(len(b))
		n := bigEndianUint(b[:])
		if version == 4 {
			n = sevenBitChunked(b[:]) - 4
		}
		took, ok := readBytesTakes(n, src.Size()-pos)
		if !ok {
			return true
		}
		pos += took
		offset += n
	}
	var s id3v2Source = &plainID3v2Source{src: src, pos: pos}
	if unsync {
		s = &unsyncID3v2Source{r: bufio.NewReaderSize(io.NewSectionReader(src, pos, src.Size()-pos), 64<<10)}
	}
	return w.readFrames(s, version, offset, size)
}

// readFrames mirrors readID3v2Frames: while offset (the header's 10 bytes, the
// extended header's, and every frame's header and DECLARED size) is under the
// tag's size, a frame header, then the flag fields dhowden reads, then the
// payload, whose length the flags can change: version 3 compression takes 4
// from it (wrapping below zero for a frame declaring under 4), a version 4
// data length indicator replaces it, and encryption takes 1. So the next
// header is read from where the payload ENDS, which can be well before or
// after where the declared size says, and the walk follows the reads, never
// the declared sizes (B99's first guard walked by declared sizes and judged
// different data than dhowden reads).
func (w *id3v2Walk) readFrames(s id3v2Source, version byte, offset, size uint) bool {
	nameLen, headerLen := 4, 10
	if version == 2 {
		nameLen, headerLen = 3, 6
	}
	var h [10]byte
	for offset < size {
		if !s.read(h[:headerLen]) {
			return true
		}
		var n uint
		var compressed, encrypted, dataLength bool
		switch version {
		case 2:
			n = bigEndianUint(h[3:6])
		case 3:
			n = bigEndianUint(h[4:8])
			compressed, encrypted = h[9]&0x80 != 0, h[9]&0x40 != 0
		default:
			n = sevenBitChunked(h[4:8])
			compressed, encrypted, dataLength = h[9]&0x08 != 0, h[9]&0x04 != 0, h[9]&0x01 != 0
		}
		if n == 0 {
			return true // padding: dhowden stops reading frames here
		}
		offset += uint(headerLen) + n
		var field [4]byte
		if compressed {
			if version == 4 && !dataLength {
				return true // dhowden: "compression without data length indicator"
			}
			if version == 3 {
				if !s.read(field[:]) {
					return true
				}
				n -= 4
			}
		}
		if dataLength {
			if !s.read(field[:]) {
				return true
			}
			n = sevenBitChunked(field[:])
		}
		if encrypted {
			if !s.read(field[:1]) {
				return true
			}
			n--
		}
		if !s.skip(n) {
			return true
		}
		if !w.store(string(h[:nameLen])) {
			return false
		}
	}
	return true
}

// id3v2Source is the stream dhowden reads a tag's frames from, as the walk
// reads it: read takes the next len(b) bytes into b, skip consumes what
// dhowden's readBytes consumes for a payload of n bytes, and each reports
// whether dhowden's read succeeds.
type id3v2Source interface {
	read(b []byte) bool
	skip(n uint) bool
}

// plainID3v2Source reads the frames of a tag without unsynchronisation by
// offset, and skips a payload by its length without reading it.
type plainID3v2Source struct {
	src byteSource
	pos int64
}

func (s *plainID3v2Source) read(b []byte) bool {
	if !readFullAt(s.src, s.pos, b) {
		return false
	}
	s.pos += int64(len(b))
	return true
}

func (s *plainID3v2Source) skip(n uint) bool {
	took, ok := readBytesTakes(n, s.src.Size()-s.pos)
	s.pos += took
	return ok
}

// unsyncID3v2Source reads the frames of a tag through dhowden's
// unsynchroniser: a zero byte that follows an emitted 0xFF is dropped. The
// filter's state runs across frames, and starts clear after the header.
type unsyncID3v2Source struct {
	r  *bufio.Reader
	ff bool
}

func (s *unsyncID3v2Source) next() (byte, bool) {
	for {
		c, err := s.r.ReadByte()
		if err != nil {
			return 0, false
		}
		if s.ff && c == 0 {
			s.ff = false
			continue
		}
		s.ff = c == 0xFF
		return c, true
	}
}

func (s *unsyncID3v2Source) read(b []byte) bool {
	for i := range b {
		c, ok := s.next()
		if !ok {
			return false
		}
		b[i] = c
	}
	return true
}

func (s *unsyncID3v2Source) skip(n uint) bool {
	if n > dhowdenUpfrontBytes && int64(n) < 0 {
		return true // see readBytesTakes
	}
	for ; n > 0; n-- {
		if _, ok := s.next(); !ok {
			return false
		}
	}
	return true
}

// readBytesTakes mirrors what dhowden's readBytes (util.go) consumes when
// asked for n bytes with avail left: all n when they are there, and a failure
// when they are not, except for an n past readBytesMaxUpfront that is
// negative as an int64 (a length that wrapped below zero): io.CopyN reads
// nothing for a negative count and returns no error, so dhowden reads on from
// where it was.
func readBytesTakes(n uint, avail int64) (int64, bool) {
	if n > dhowdenUpfrontBytes && int64(n) < 0 {
		return 0, true
	}
	if int64(n) > avail {
		return 0, false
	}
	return int64(n), true
}

// sevenBitChunked is dhowden's get7BitChunkedInt: seven bits per byte, and
// the high bit NOT masked, so a byte past 0x7F overlaps its neighbour and a
// 4-byte field reaches 0x1FFFFFFF (about 512 MiB).
func sevenBitChunked(b []byte) uint {
	var n int
	for _, x := range b {
		n = n<<7 | int(x)
	}
	return uint(n)
}

// bigEndianUint is dhowden's getInt, as the uint its readers keep.
func bigEndianUint(b []byte) uint {
	var n int
	for _, x := range b {
		n = n<<8 | int(x)
	}
	return uint(n)
}
