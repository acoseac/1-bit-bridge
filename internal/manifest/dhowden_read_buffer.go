package manifest

import "io"

// dhowdenReadBufferSize is how much a dhowdenReadBuffer reads from its stream
// at a time: a page, which a NAS client fetches for the smallest read anyway.
const dhowdenReadBufferSize = 4096

// dhowdenReadBuffer is a buffered io.ReadSeeker in front of a stream dhowden
// reads (backlog B117). dhowden reads a tag in small pieces (a frame's id, its
// size and its flags are a read each), and under an ID3v2 tag's
// unsynchronisation flag its filter (unsynchroniser.Read, id3v2.go) reads the
// stream ONE BYTE per Read call, so on a file every byte of such a tag was a
// read(2): a 10 MB tag took seconds to extract, and the 512 MiB the size field
// admits held a scan worker for minutes, while Scan holds the scanner's mutex.
// Through this buffer those reads come from memory, a page at a time.
//
// It is the stream, read ahead: it holds exactly the stream's bytes from where
// the stream was when it was made, and answers a Seek as the stream would. A
// Seek landing within the bytes it holds moves nothing (tag.ReadFrom's
// Seek(-11, io.SeekCurrent) after the 11 bytes it picks a parser by, dhowden's
// skips over short blocks and atoms); any other Seek is the stream's own, with
// its result and its error. A Read no smaller than the buffer, once the buffer
// is drained, goes straight to the stream, so a cover is not copied twice, and
// an empty Read is the stream's own answer too. release leaves the stream
// where a reader that had read it unbuffered would have left it.
type dhowdenReadBuffer struct {
	rs   io.ReadSeeker
	buf  []byte
	base int64 // the stream offset of buf[0]; the stream itself is at base+w
	r, w int   // how much of buf has been read, and how much it holds
	err  error // what the stream answered with the last bytes buf holds
}

// newDhowdenReadBuffer buffers rs from where it is, and returns the release
// that puts rs where its reader got to. A stream that cannot say where it is is
// handed back as it is, with a release that does nothing.
func newDhowdenReadBuffer(rs io.ReadSeeker) (io.ReadSeeker, func()) {
	base, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return rs, func() {}
	}
	b := &dhowdenReadBuffer{rs: rs, buf: make([]byte, dhowdenReadBufferSize), base: base}
	return b, b.release
}

func (b *dhowdenReadBuffer) Read(p []byte) (int, error) {
	if b.r == b.w {
		if len(p) == 0 {
			return b.rs.Read(p)
		}
		if b.err != nil {
			err := b.err
			b.err = nil
			return 0, err
		}
		b.base += int64(b.w)
		b.r, b.w = 0, 0
		if len(p) >= len(b.buf) {
			n, err := b.rs.Read(p)
			b.base += int64(n)
			return n, err
		}
		n, err := b.rs.Read(b.buf)
		if n == 0 {
			return 0, err
		}
		b.w, b.err = n, err
	}
	n := copy(p, b.buf[b.r:b.w])
	b.r += n
	return n, nil
}

func (b *dhowdenReadBuffer) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekCurrent:
		target = b.base + int64(b.r) + offset
	case io.SeekStart:
		target = offset
	default:
		return b.seekStream(offset, whence)
	}
	if target >= b.base && target <= b.base+int64(b.w) {
		b.r, b.err = int(target-b.base), nil
		return target, nil
	}
	return b.seekStream(target, io.SeekStart)
}

// seekStream is a Seek the buffer cannot answer: the stream's, after which the
// buffer holds nothing. A failed one leaves the stream, and so the buffer, as
// they were.
func (b *dhowdenReadBuffer) seekStream(offset int64, whence int) (int64, error) {
	abs, err := b.rs.Seek(offset, whence)
	if err != nil {
		return abs, err
	}
	b.base, b.r, b.w, b.err = abs, 0, 0, nil
	return abs, nil
}

// release seeks the stream to where its reader is.
func (b *dhowdenReadBuffer) release() {
	_, _ = b.rs.Seek(b.base+int64(b.r), io.SeekStart)
}
