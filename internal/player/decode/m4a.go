package decode

import (
	"encoding/binary"
	"io"
	"math"
)

// M4A (ISO/IEC 14496-12) for ALAC: the box walk and the sample tables of the
// one audio track. Boxes are read from the file by seeking, never by loading
// moov whole, so cover art in moov costs a seek. Every size, count and offset
// is compared as a uint64 with what holds it before anything is allocated or
// added, and every bound subtracts from what remains rather than adding to a
// position (PR 1b round 3).

// m4aBox is a box's type and its payload's extent in the file: [start, end).
type m4aBox struct {
	typ        string
	start, end int64
}

func (b m4aBox) size() int64 { return b.end - b.start }

// m4aWalk reads box headers from rs under a budget of boxes.
type m4aWalk struct {
	rs    io.ReadSeeker
	size  int64 // the file's size
	boxes int
}

// readAt reads exactly len(p) bytes at off. A read that runs out of file is
// a truncated refusal naming what was being read.
func (w *m4aWalk) readAt(off int64, p []byte, what string) error {
	if _, err := w.rs.Seek(off, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.ReadFull(w.rs, p); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return refuse(ReasonTruncated, "M4A ends inside %s", what)
		}
		return err
	}
	return nil
}

// children calls fn for each box in [start, end). top is the file's top
// level, where a size of 0 runs to the end of the file, mdat may run past it
// (a file cut inside its audio still opens, and reading past the cut is
// refused then), and fewer than eight bytes left after the last box end the
// walk.
func (w *m4aWalk) children(parent string, start, end int64, top bool, fn func(b m4aBox) error) error {
	pos := start
	for n := 0; pos < end; n++ {
		if end-pos < 8 {
			if top {
				return nil
			}
			return refuse(ReasonUnsupportedLayout, "M4A %s has %d bytes after its last box", parent, end-pos)
		}
		if n >= maxM4AChildren {
			return refuse(ReasonUnsupportedLayout, "M4A %s holds more than %d boxes", parent, maxM4AChildren)
		}
		if w.boxes++; w.boxes > maxM4ABoxes {
			return refuse(ReasonUnsupportedLayout, "M4A has more than %d boxes", maxM4ABoxes)
		}
		b, err := w.header(pos, end, top)
		if err != nil {
			return err
		}
		if err := fn(b); err != nil {
			return err
		}
		pos = b.end
	}
	return nil
}

// header reads the box header at pos, inside a parent that ends at end.
func (w *m4aWalk) header(pos, end int64, top bool) (m4aBox, error) {
	var h [16]byte
	if err := w.readAt(pos, h[:8], "a box header"); err != nil {
		return m4aBox{}, err
	}
	size := uint64(binary.BigEndian.Uint32(h[0:4]))
	typ := string(h[4:8])
	headerLen := int64(8)
	switch size {
	case 1:
		if end-pos < 16 {
			if top {
				return m4aBox{}, refuse(ReasonTruncated, "M4A ends inside box %q's 64-bit size", typ)
			}
			return m4aBox{}, refuse(ReasonUnsupportedLayout, "M4A box %q's 64-bit size runs past its parent", typ)
		}
		if err := w.readAt(pos+8, h[8:16], "a box header"); err != nil {
			return m4aBox{}, err
		}
		size, headerLen = binary.BigEndian.Uint64(h[8:16]), 16
	case 0:
		if !top {
			return m4aBox{}, refuse(ReasonUnsupportedLayout, "M4A box %q of size 0 inside a container", typ)
		}
		size = uint64(end - pos)
	}
	if size < uint64(headerLen) {
		return m4aBox{}, refuse(ReasonUnsupportedLayout, "M4A box %q size %d is smaller than its header", typ, size)
	}
	if size > uint64(math.MaxInt64-pos) {
		return m4aBox{}, refuse(ReasonUnsupportedLayout, "M4A box %q size %d exceeds addressable length", typ, size)
	}
	if size > uint64(end-pos) {
		if !top {
			return m4aBox{}, refuse(ReasonUnsupportedLayout, "M4A box %q of %d bytes runs past its parent", typ, size)
		}
		if typ != "mdat" {
			return m4aBox{}, refuse(ReasonTruncated, "M4A box %q of %d bytes runs past the end of the file", typ, size)
		}
	}
	return m4aBox{typ: typ, start: pos + headerLen, end: pos + int64(size)}, nil
}

// collect walks parent's children and returns the box of each type in want,
// refusing a second box of any of them.
func (w *m4aWalk) collect(parent m4aBox, want ...string) (map[string]m4aBox, error) {
	found := make(map[string]m4aBox, len(want))
	err := w.children(parent.typ, parent.start, parent.end, false, func(b m4aBox) error {
		for _, typ := range want {
			if b.typ != typ {
				continue
			}
			if _, dup := found[typ]; dup {
				return refuse(ReasonUnsupportedLayout, "M4A %s holds two %s boxes", parent.typ, typ)
			}
			found[typ] = b
		}
		return nil
	})
	return found, err
}

// payload reads a small box's payload whole, refusing one larger than max.
func (w *m4aWalk) payload(b m4aBox, max int64) ([]byte, error) {
	if b.size() > max {
		return nil, refuse(ReasonUnsupportedLayout, "M4A %s box of %d bytes exceeds %d", b.typ, b.size(), max)
	}
	p := make([]byte, b.size())
	if err := w.readAt(b.start, p, "box "+b.typ); err != nil {
		return nil, err
	}
	return p, nil
}

// fullBox reads a FullBox's version and the uint32 that follows its flags,
// which every table here starts with: an entry count or, for stsz, the
// constant sample size.
func (w *m4aWalk) fullBox(b m4aBox, need int64) ([]byte, error) {
	if b.size() < need {
		return nil, refuse(ReasonTruncated, "M4A %s box of %d bytes is shorter than its header", b.typ, b.size())
	}
	h := make([]byte, need)
	if err := w.readAt(b.start, h, "box "+b.typ); err != nil {
		return nil, err
	}
	if h[0] != 0 {
		return nil, refuse(ReasonUnsupportedLayout, "M4A %s version %d; version 0 is read", b.typ, h[0])
	}
	return h, nil
}

// table checks that count entries of width bytes fit in b after its header
// of headerLen bytes, then hands them to fn a bounded read at a time. The
// count is compared with what the box holds before anything is allocated.
func (w *m4aWalk) table(b m4aBox, headerLen int64, count uint64, width int, fn func(i int, e []byte) error) error {
	if room := uint64(b.size() - headerLen); count > room/uint64(width) {
		return refuse(ReasonTruncated, "M4A %s box of %d bytes holds fewer than its %d entries", b.typ, b.size(), count)
	}
	buf := make([]byte, width*int(min(count, m4aTableRead)))
	off := b.start + headerLen
	for done := uint64(0); done < count; {
		n := min(count-done, m4aTableRead)
		chunk := buf[:width*int(n)]
		if err := w.readAt(off, chunk, "box "+b.typ); err != nil {
			return err
		}
		for k := 0; k < int(n); k++ {
			if err := fn(int(done)+k, chunk[width*k:width*(k+1)]); err != nil {
				return err
			}
		}
		done += n
		off += int64(len(chunk))
	}
	return nil
}
