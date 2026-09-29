package manifest

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"strings"
)

// dhowden/tag allocates a picture's data buffer from a length it reads out of
// the file, BEFORE the read that would fail (vorbis.go, readPictureBlock):
//
//	data := make([]byte, dataLen)
//
// dataLen is a 32-bit field, so a 40-byte file asks for 4 GiB. It is the one
// allocation in the library sized by a declared length rather than by the
// bytes present: every other length goes through readBytes, which allocates
// at most readBytesMaxUpfront (10 MB) before the bytes arrive and streams the
// rest. The runtime THROWS on the out-of-memory this causes, and a throw does
// not unwind, so runScanWorker's recover() cannot catch it: one file takes
// `bridge serve` down, on every scan that reaches it. Where the allocation
// does succeed (a 64-bit host that overcommits), the buffer is garbage the
// next call zeroes and fills again; that is what killed the nightly
// FuzzExtractFLAC runner (backlog B99).
//
// readPictureBlock is reached two ways, and tag.ReadFrom picks the parser by
// the file's first bytes, never by its extension:
//
//   - a FLAC PICTURE block (type 6), and
//   - a METADATA_BLOCK_PICTURE Vorbis comment: base64 of the same structure,
//     which dhowden decodes at the end of EVERY VORBIS_COMMENT block of a FLAC
//     stream (its comment map outlives the block, so one value is decoded
//     again at each later block), and in the comment packet of an Ogg stream
//     (Vorbis, Opus).
//
// dhowden ignores the declared length of a FLAC VORBIS_COMMENT or PICTURE
// block: it reads their contents field by field and takes the next block
// header from wherever the last field ended. So a guard that walks the blocks
// by their declared lengths (the flacPictureBlocksSane this replaces) judges a
// different set of pictures than dhowden reads: a PICTURE block declaring
// length 0 (whose fields that guard could not read, and so passed), a
// PICTURE header inside the unused tail of a VORBIS_COMMENT or PICTURE block,
// and every METADATA_BLOCK_PICTURE all reached the allocation. This file
// walks the stream the way dhowden will, and sums the picture buffers it
// would allocate.

// dhowdenUpfrontBytes is dhowden's readBytesMaxUpfront (util.go, at the
// version go.mod pins): the most it allocates for a declared length before
// the bytes are there. The picture budget allows it once, which is what a
// truncated picture costs.
const dhowdenUpfrontBytes = 10 << 20

// maxDhowdenPictureType is the last key of dhowden's pictureTypes table
// (0x14, "Publisher/Studio logotype"). readPictureBlock refuses any other
// type byte before it reaches the allocation.
const maxDhowdenPictureType = 0x14

// mbpKeyName is the comment key dhowden decodes a picture from, compared
// after strings.ToLower as dhowden compares it.
const mbpKeyName = "metadata_block_picture"

// mbpKeyScan is how many bytes of a comment the walk reads to find its key.
// strings.ToLower reaches a letter of mbpKeyName from no rune longer than 3
// bytes (U+212A KELVIN SIGN lowers to 'k'), so a key that lowers to the
// 22-byte name is at most 66 bytes long, and its '=' is within the first 67.
// TestNoRuneLongerThanThreeBytesLowersIntoThePictureKey pins that premise.
const mbpKeyScan = 3*len(mbpKeyName) + 1

// pictureBudget is the most dhowden may allocate for the pictures of a file
// of size bytes: twice what the file holds, plus dhowden's own up-front
// allowance. A well-formed file needs at most one and a half times its size:
// each picture's bytes are in the file once, and a METADATA_BLOCK_PICTURE is
// decoded into one buffer (three quarters of its base64) and copied into a
// second.
func pictureBudget(size int64) int64 { return 2*size + dhowdenUpfrontBytes }

// byteSource is what the walk reads: the file, a section of it, or one Ogg
// packet laid out across pages.
type byteSource interface {
	io.ReaderAt
	Size() int64
}

// pictureWalk accumulates the picture buffers dhowden would allocate.
type pictureWalk struct {
	budget int64
	spent  int64
	// refusal names the buffer that took the sum past the budget; it stays
	// zero while the walk is within it.
	refusal pictureRefusal
}

// pictureRefusal names the picture that took dhowden past its budget.
type pictureRefusal struct {
	What     string // "a PICTURE block" or "a METADATA_BLOCK_PICTURE comment"
	Declared int64  // the buffer it asked for
	Budget   int64  // pictureBudget of the file
}

// spend records a buffer of n bytes dhowden would allocate for what, and
// reports whether the walk is still within its budget.
func (w *pictureWalk) spend(n int64, what string) bool {
	w.spent += n
	if w.spent > w.budget {
		w.refusal = pictureRefusal{What: what, Declared: n, Budget: w.budget}
		return false
	}
	return true
}

// dhowdenPicturesWithinBudget reports whether tag.ReadFrom, reading rs from
// its current offset, stays within pictureBudget for the pictures it
// allocates; when it would not, the refusal names the picture that crossed
// it. It walks the stream as dhowden will (ReadFrom's dispatch on the
// first bytes, then ReadFLACTags or ReadOGGTags) and reads only the fields
// that decide where dhowden goes next: never a picture's payload, and of a
// METADATA_BLOCK_PICTURE only the base64 that holds its header, so a cover is
// not read twice (the single-open FLAC path exists because it was).
//
// It fails OPEN on a read it cannot complete: a stream that ends there makes
// dhowden's read fail too, and it stops before any allocation further on. So
// does a genuine I/O error, which would otherwise cost a readable file its
// tags on a flaky mount; dhowden reading the same bytes into a bomb a moment
// later needs the same bytes to have read differently twice.
//
// Where dhowden stops with an error, the walk may read on: past a comment
// with no '=' beyond the bytes it scans for the key, past invalid base64
// beyond the header it decodes, and past an Ogg page whose CRC is wrong,
// which it does not check. That only ever adds pictures to the sum, and a
// stream dhowden fails on yields no tags whether or not it is refused. What
// the walk must never do is stop where dhowden reads on, and every stop
// above is one of dhowden's.
//
// rs is left at the offset it came in at.
func dhowdenPicturesWithinBudget(rs io.ReadSeeker) (bool, pictureRefusal) {
	start, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return true, pictureRefusal{}
	}
	end, err := rs.Seek(0, io.SeekEnd)
	if _, serr := rs.Seek(start, io.SeekStart); err != nil || serr != nil || end < start {
		return true, pictureRefusal{}
	}
	ra, ok := rs.(io.ReaderAt)
	if !ok {
		ra = &seekingReaderAt{rs: rs}
	}
	src := io.NewSectionReader(ra, start, end-start)
	w := &pictureWalk{budget: pictureBudget(src.Size())}
	// tag.ReadFrom reads 11 bytes first and fails without them.
	var head [11]byte
	if !readFullAt(src, 0, head[:]) {
		return true, pictureRefusal{}
	}
	switch {
	case string(head[:4]) == "fLaC":
		w.flac(src)
	case string(head[:4]) == "OggS":
		w.ogg(src)
	}
	if _, err := rs.Seek(start, io.SeekStart); err != nil {
		return true, pictureRefusal{}
	}
	return w.refusal.What == "", w.refusal
}

// flac mirrors tag.ReadFLACTags: after "fLaC", blocks of a 1-byte header
// (last flag, type) and a 3-byte length, where a VORBIS_COMMENT or PICTURE
// block is read by its contents and every other block is skipped by its
// length.
func (w *pictureWalk) flac(src byteSource) {
	pos := int64(4)
	var mbp pictureValue // dhowden's comment map outlives the block
	for {
		var hdr [4]byte
		if !readFullAt(src, pos, hdr[:]) {
			return
		}
		pos += int64(len(hdr))
		last := hdr[0]&0x80 != 0
		length := int64(hdr[1])<<16 | int64(hdr[2])<<8 | int64(hdr[3])
		switch hdr[0] &^ 0x80 {
		case 4: // VORBIS_COMMENT
			end, ok := w.vorbisComments(src, pos, &mbp)
			if !ok {
				return
			}
			if mbp.set && !w.decodedPicture(mbp) {
				return
			}
			pos = end
		case 6: // PICTURE
			end, ok := w.filePicture(src, pos)
			if !ok {
				return
			}
			pos = end
		default:
			pos += length
		}
		if last {
			return
		}
	}
}

// pictureValue locates the base64 of a METADATA_BLOCK_PICTURE comment.
type pictureValue struct {
	set bool
	src byteSource
	off int64
	n   int64
}

// vorbisComments mirrors metadataVorbis.readVorbisComment's reads from pos:
// a vendor string and a count of comments, each a 32-bit little-endian length
// and that many bytes of KEY=value. It returns where they end, and false
// where one of dhowden's reads would fail (it then returns the error and
// reads nothing more). mbp is left on the last METADATA_BLOCK_PICTURE seen,
// which is the one dhowden's map holds.
func (w *pictureWalk) vorbisComments(src byteSource, pos int64, mbp *pictureValue) (int64, bool) {
	size := src.Size()
	vendorLen, ok := u32LEAt(src, pos)
	if !ok {
		return pos, false
	}
	pos += 4
	if int64(vendorLen) > size-pos {
		return pos, false
	}
	pos += int64(vendorLen)
	count, ok := u32LEAt(src, pos)
	if !ok {
		return pos, false
	}
	pos += 4
	// Each comment costs at least its 4-byte length, so the loop ends at the
	// end of the source whatever count claims.
	for i := uint32(0); i < count; i++ {
		l, ok := u32LEAt(src, pos)
		if !ok {
			return pos, false
		}
		pos += 4
		n := int64(l)
		if n > size-pos {
			return pos, false
		}
		head := make([]byte, min(n, int64(mbpKeyScan)))
		if !readFullAt(src, pos, head) {
			return pos, false
		}
		eq := bytes.IndexByte(head, '=')
		switch {
		case eq < 0 && n <= int64(mbpKeyScan):
			// No '=' anywhere in it: parseComment refuses it, and dhowden
			// stops.
			return pos, false
		case eq >= 0 && strings.ToLower(string(head[:eq])) == mbpKeyName:
			*mbp = pictureValue{set: true, src: src, off: pos + int64(eq) + 1, n: n - int64(eq) - 1}
		}
		pos += n
	}
	return pos, true
}

// readStringCost is what dhowden's readString allocates for a declared
// length l with avail bytes left to read: all of l up front when l is within
// readBytesMaxUpfront, whether or not the bytes are there, and otherwise what
// io.CopyN buffers of the bytes that are.
func readStringCost(l, avail int64) int64 {
	if l <= dhowdenUpfrontBytes {
		return l
	}
	return min(l, max(avail, 0))
}

// filePicture mirrors readPictureBlock reading a FLAC PICTURE block from the
// file at pos: type, MIME, description, four dimensions, then the data length
// it allocates before reading. It returns where the block's data ends, and
// false where dhowden stops (any failed read, or the budget exceeded).
func (w *pictureWalk) filePicture(src byteSource, pos int64) (int64, bool) {
	size := src.Size()
	typ, ok := u32BEAt(src, pos)
	if !ok || byte(typ) > maxDhowdenPictureType {
		return pos, false
	}
	pos += 4
	for range 2 { // MIME type, then description: a length and its bytes
		l, ok := u32BEAt(src, pos)
		if !ok {
			return pos, false
		}
		pos += 4
		if !w.spend(readStringCost(int64(l), size-pos), "a PICTURE block") {
			return pos, false
		}
		if int64(l) > size-pos {
			return pos, false
		}
		pos += int64(l)
	}
	if size-pos < 16 { // width, height, colour depth, colours used
		return pos, false
	}
	pos += 16
	dataLen, ok := u32BEAt(src, pos)
	if !ok {
		return pos, false
	}
	pos += 4
	if !w.spend(int64(dataLen), "a PICTURE block") {
		return pos, false
	}
	if int64(dataLen) > size-pos {
		return pos, false // io.ReadFull fails, and dhowden returns the error
	}
	return pos + int64(dataLen), true
}

// decodedPicture mirrors the end of readVorbisComment: DecodeString of the
// METADATA_BLOCK_PICTURE value, then readPictureBlock over the decoded
// bytes, whose error dhowden ignores. It reports whether dhowden reads on.
// Only the header is decoded, through base64's streaming decoder, which
// skips '\r' and '\n' as DecodeString does.
//
// Everything this decode makes dhowden allocate is counted, the MIME and
// description reads included, because the decode repeats: dhowden's map
// outlives the block, so the value is decoded again at every later
// VORBIS_COMMENT block, and a readString whose length is past the decoded
// bytes still allocates up to 10 MB before it fails (an ignored failure).
// Measured on the first version of this guard, which counted only the
// decoded bytes and the picture: a 787-byte file whose value declared a
// 2.9 MB MIME type, decoded 23 times, allocated 68 MB.
func (w *pictureWalk) decodedPicture(v pictureValue) bool {
	decodedMax := int64(base64.StdEncoding.DecodedLen(int(v.n)))
	if !w.spend(decodedMax, "a METADATA_BLOCK_PICTURE comment") {
		return false
	}
	dec := base64.NewDecoder(base64.StdEncoding, io.NewSectionReader(v.src, v.off, v.n))
	var consumed int64
	var b [4]byte
	read32 := func() (uint32, error) {
		if _, err := io.ReadFull(dec, b[:]); err != nil {
			return 0, err
		}
		consumed += 4
		return binary.BigEndian.Uint32(b[:]), nil
	}
	// readsOn classifies a failed read of the decoded picture: invalid base64
	// means DecodeString failed and dhowden stopped; running out of decoded
	// bytes (or a read error) fails readPictureBlock, which it ignores.
	readsOn := func(err error) bool {
		var corrupt base64.CorruptInputError
		return !errors.As(err, &corrupt)
	}
	typ, err := read32()
	if err != nil {
		return readsOn(err)
	}
	if byte(typ) > maxDhowdenPictureType {
		return true
	}
	for range 2 { // MIME type, then description
		l, err := read32()
		if err != nil {
			return readsOn(err)
		}
		avail := decodedMax - consumed
		if !w.spend(readStringCost(int64(l), avail), "a METADATA_BLOCK_PICTURE comment") {
			return false
		}
		if int64(l) > avail {
			return true // more than the value can decode to: readPictureBlock fails
		}
		n, err := io.CopyN(io.Discard, dec, int64(l))
		consumed += n
		if err != nil {
			return readsOn(err)
		}
	}
	for range 4 { // width, height, colour depth, colours used
		if _, err := read32(); err != nil {
			return readsOn(err)
		}
	}
	dataLen, err := read32()
	if err != nil {
		return readsOn(err)
	}
	return w.spend(int64(dataLen), "a METADATA_BLOCK_PICTURE comment")
}

// ogg mirrors tag.ReadOGGTags: Ogg pages read in order and their segments
// joined into packets per stream serial, until the first packet that opens
// with "\x03vorbis" or "OpusTags", whose comments dhowden reads before it
// returns. Page CRCs are not checked (dhowden stops at a bad one, so reading
// on can only add pictures; see dhowdenPicturesWithinBudget), which is what
// lets the walk read page headers and segment tables, never segment data it
// does not need.
func (w *pictureWalk) ogg(src byteSource) {
	d := &oggDemux{src: src, open: map[uint32]*packetSource{}}
	for {
		done, ok := d.page()
		if !ok {
			return
		}
		for _, p := range done {
			body, ok := oggCommentBody(p)
			if !ok {
				continue
			}
			var mbp pictureValue
			if _, ok := w.vorbisComments(body, 0, &mbp); ok && mbp.set {
				w.decodedPicture(mbp)
			}
			return // ReadOGGTags returns after the first comment packet
		}
	}
}

// oggDemux mirrors dhowden's oggDemuxer: the packet each stream serial has in
// progress, and where the next page starts.
type oggDemux struct {
	src  byteSource
	pos  int64
	open map[uint32]*packetSource
}

// page reads the page at d.pos and returns the packets it completes, in
// order; false where dhowden's read of the page fails.
func (d *oggDemux) page() ([]*packetSource, bool) {
	var h [27]byte
	if !readFullAt(d.src, d.pos, h[:]) || string(h[:4]) != "OggS" {
		return nil, false
	}
	pos := d.pos + int64(len(h))
	serial := binary.LittleEndian.Uint32(h[14:18])
	segs := make([]byte, h[26])
	if !readFullAt(d.src, pos, segs) {
		return nil, false
	}
	pos += int64(len(segs))
	var total int64
	for _, s := range segs {
		total += int64(s)
	}
	if total > d.src.Size()-pos {
		return nil, false // dhowden's read of the segment data fails
	}
	cur := &packetSource{ra: d.src}
	if h[5]&0x1 != 0 { // continued: the page carries on its serial's packet
		if cur = d.open[serial]; cur == nil {
			return nil, false // "could not find continued packet"
		}
	}
	var done []*packetSource
	for _, s := range segs {
		cur.add(pos, int64(s))
		pos += int64(s)
		if s < 255 {
			done = append(done, cur)
			cur = &packetSource{ra: d.src}
		}
	}
	d.open[serial] = cur
	d.pos = pos
	return done, true
}

// oggCommentBody returns the comments of a Vorbis or Opus comment packet,
// after its prefix, and false for any other packet.
func oggCommentBody(p *packetSource) (byteSource, bool) {
	for _, prefix := range []string{"\x03vorbis", "OpusTags"} {
		if p.hasPrefix(prefix) {
			return io.NewSectionReader(p, int64(len(prefix)), p.Size()-int64(len(prefix))), true
		}
	}
	return nil, false
}

// packetSource is one Ogg packet: pieces of the file, one per page it spans.
type packetSource struct {
	ra     io.ReaderAt
	starts []int64 // offset of each piece within the packet
	pieces []filePiece
	size   int64
}

type filePiece struct{ off, n int64 }

// add appends n bytes at file offset off to the packet.
func (p *packetSource) add(off, n int64) {
	if n == 0 {
		return
	}
	if k := len(p.pieces); k > 0 && p.pieces[k-1].off+p.pieces[k-1].n == off {
		p.pieces[k-1].n += n
	} else {
		p.starts = append(p.starts, p.size)
		p.pieces = append(p.pieces, filePiece{off: off, n: n})
	}
	p.size += n
}

// Size returns the packet's length in bytes.
func (p *packetSource) Size() int64 { return p.size }

// ReadAt reads the packet's bytes at off, across the pages that hold them.
func (p *packetSource) ReadAt(b []byte, off int64) (int, error) {
	if off < 0 || off >= p.size {
		return 0, io.EOF
	}
	i := sort.Search(len(p.starts), func(i int) bool { return p.starts[i] > off }) - 1
	read := 0
	for read < len(b) && i < len(p.pieces) {
		piece := p.pieces[i]
		within := off + int64(read) - p.starts[i]
		want := min(int64(len(b)-read), piece.n-within)
		n, err := p.ra.ReadAt(b[read:read+int(want)], piece.off+within)
		read += n
		if int64(n) < want {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return read, err
		}
		i++
	}
	if read < len(b) {
		return read, io.EOF
	}
	return read, nil
}

// hasPrefix reports whether the packet opens with prefix.
func (p *packetSource) hasPrefix(prefix string) bool {
	if p.size < int64(len(prefix)) {
		return false
	}
	b := make([]byte, len(prefix))
	return readFullAt(p, 0, b) && string(b) == prefix
}

// readFullAt fills b from src at off, and reports whether all of it was
// there. A read that returned every byte counts whatever its error, since
// io.ReaderAt may answer (len(b), io.EOF) at the end of the source.
func readFullAt(src io.ReaderAt, off int64, b []byte) bool {
	if off < 0 {
		return false
	}
	n, _ := src.ReadAt(b, off)
	return n == len(b)
}

func u32LEAt(src io.ReaderAt, off int64) (uint32, bool) {
	var b [4]byte
	if !readFullAt(src, off, b[:]) {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b[:]), true
}

func u32BEAt(src io.ReaderAt, off int64) (uint32, bool) {
	var b [4]byte
	if !readFullAt(src, off, b[:]) {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[:]), true
}

// seekingReaderAt reads at an offset through Seek and Read, for a stream that
// has no ReadAt of its own. Not safe for concurrent use; the walk makes none.
type seekingReaderAt struct{ rs io.ReadSeeker }

func (s *seekingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if _, err := s.rs.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(s.rs, p)
}
