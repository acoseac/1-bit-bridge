package manifest

import (
	"bytes"
	"encoding/binary"
	"io"
	"sort"
)

// FLAC in Ogg: an .oga, or any file whose first bytes are an Ogg page of a
// FLAC stream (tag.ReadFrom picks its reader by the first bytes, never by the
// extension).
//
// dhowden's ReadOGGTags reads the pages in order until a packet opens with
// "\x03vorbis" (a Vorbis comment header) or "OpusTags", and an Ogg FLAC stream
// has neither: its comments are a FLAC VORBIS_COMMENT metadata block, one of
// the stream's header packets. So dhowden read every page of such a file,
// checking each CRC, and failed at its end: the file had no tags, and was read
// whole on every extraction (backlog B102).
//
// The Ogg FLAC mapping (xiph.org/flac/ogg_mapping.html) keeps a native FLAC
// stream's metadata as it is: the stream's first packet is 0x7F "FLAC", a
// major and a minor version, a big-endian count of the header packets that
// follow (0 when not declared), then "fLaC" and the STREAMINFO block, as a
// .flac file begins; each header packet after it is one metadata block, its
// header included, the last one flagged last; the packets after them are FLAC
// frames. Joined, the first packet from its "fLaC" on and the header packets
// are the metadata of a .flac file. oggFLACMetadata finds them, reading page
// headers and segment tables and nothing past the last header packet, and
// hands back that join, read in place from the file: dhowden's FLAC reader,
// both of its guards and the FLAC multi-value pass then read an Ogg FLAC
// file's tags exactly as they read a .flac file's.

// maxOggFLACHeaderPackets bounds the header packets oggFLACMetadata collects:
// the most the mapping's count can declare, and far past what a file carries
// (libFLAC writes a VORBIS_COMMENT, a PICTURE per picture and a PADDING block;
// ffmpeg a VORBIS_COMMENT alone). A stream that declares no count and flags no
// block last would otherwise hold a packetSource per packet for as long as it
// went on.
const maxOggFLACHeaderPackets = 1<<16 - 1

// oggFLACTerminator ends the join: an empty PADDING block flagged last, so a
// reader of the join stops where the header packets did, even when the last
// of them is not flagged last. In a .flac file the first audio frame ends such
// a run for dhowden: its first byte, 0xFF, reads as a header flagged last.
var oggFLACTerminator = [4]byte{0x81, 0, 0, 0}

// oggFLACMetadata returns the metadata an Ogg FLAC stream carries, laid out as
// a .flac file holds it, when rs, from its current offset, is one: the first
// packet from its "fLaC" on, every header packet after it, and
// oggFLACTerminator. It declines a stream that is not Ogg, one whose streams'
// first pages hold no FLAC stream's, and one that carries a Vorbis or Opus
// stream, which dhowden reads (it returns the first comment packet it meets).
// rs is left at the offset it came in at; the join reads it by offset.
func oggFLACMetadata(rs io.ReadSeeker) (*io.SectionReader, bool) {
	st, ok := openDhowdenStream(rs)
	if !ok {
		return nil, false
	}
	defer st.restore()
	head, ok := st.head()
	if !ok || dhowdenReaderFor(head) != dhowdenReadsOgg {
		return nil, false
	}
	parts, ok := oggFLACHeaderPackets(st.from())
	if !ok {
		return nil, false
	}
	joined := newConcatSource(append(parts, bytes.NewReader(oggFLACTerminator[:])))
	return io.NewSectionReader(joined, 0, joined.Size()), true
}

// oggFLACHeaderPackets walks src's pages as dhowden's demuxer joins them into
// packets (oggDemux), and returns the FLAC stream's metadata: its first packet
// from "fLaC" on, then its header packets. An Ogg file begins with its
// streams' first pages, each flagged BOS and holding its stream's first
// packet. The header packets are the FLAC stream's packets after its first,
// up to the count the first declares, the one flagged last or
// maxOggFLACHeaderPackets, whichever comes first; an empty packet, one that
// opens as a FLAC frame does (0xFF, the first byte of its sync code) or the
// stream's last page ends them sooner, and so does a page the walk cannot
// read, as it ends dhowden's. It reads no segment of a page but the first
// bytes of a packet, and no page after the one that ends the header packets.
// It reports false when no FLAC stream's first packet is among the leading BOS
// pages, or a Vorbis or Opus stream's is.
func oggFLACHeaderPackets(src byteSource) ([]byteSource, bool) {
	d := &oggDemux{src: src, open: map[uint32]*packetSource{}}
	var (
		parts    []byteSource // the first packet from "fLaC" on, then the header packets
		serial   uint32       // the FLAC stream's
		declared int
		bos      = true // still among the leading first pages
	)
	for {
		done, ok := d.page()
		if !ok {
			return parts, parts != nil
		}
		if bos && d.flags&oggBOS == 0 {
			bos = false
			if parts == nil {
				return nil, false
			}
		}
		for i, p := range done {
			if bos && i == 0 {
				// A stream's first page opens with its first packet.
				if p.hasPrefix("\x01vorbis") || p.hasPrefix("OpusHead") {
					return nil, false
				}
				if n, ok := oggFLACMappingCount(p); ok && parts == nil {
					parts = []byteSource{io.NewSectionReader(p, 9, p.Size()-9)}
					serial, declared = d.serial, n
				}
				continue
			}
			if parts == nil || d.serial != serial {
				continue // another stream's packet
			}
			var first [1]byte
			if !readFullAt(p, 0, first[:]) || first[0] == 0xFF {
				return parts, true // an empty packet, or the first audio frame
			}
			parts = append(parts, p)
			headers := len(parts) - 1
			if first[0]&0x80 != 0 || headers == declared || headers == maxOggFLACHeaderPackets {
				return parts, true
			}
		}
		if parts != nil && d.serial == serial && d.flags&oggEOS != 0 {
			return parts, true
		}
	}
}

// oggFLACMappingCount reads a stream's first packet as the Ogg FLAC mapping's:
// 0x7F "FLAC", major version 1 (the one there is, and the one libFLAC's and
// ffmpeg's readers accept), a minor version, the header packet count, then
// "fLaC", as a .flac file begins. It returns the count, and false for a packet
// of any other shape.
func oggFLACMappingCount(p *packetSource) (int, bool) {
	var b [13]byte
	if !readFullAt(p, 0, b[:]) || string(b[:5]) != "\x7fFLAC" || b[5] != 1 || string(b[9:]) != "fLaC" {
		return 0, false
	}
	return int(binary.BigEndian.Uint16(b[7:9])), true
}

// concatSource reads its parts one after another, as one source.
type concatSource struct {
	parts  []byteSource
	starts []int64 // the offset each part starts at
	size   int64
}

func newConcatSource(parts []byteSource) *concatSource {
	c := &concatSource{parts: parts, starts: make([]int64, len(parts))}
	for i, p := range parts {
		c.starts[i] = c.size
		c.size += p.Size()
	}
	return c
}

// Size returns the length of the parts together.
func (c *concatSource) Size() int64 { return c.size }

// ReadAt reads the bytes at off, across the parts that hold them.
func (c *concatSource) ReadAt(b []byte, off int64) (int, error) {
	if off < 0 || off >= c.size {
		return 0, io.EOF
	}
	i := sort.Search(len(c.starts), func(i int) bool { return c.starts[i] > off }) - 1
	read := 0
	for read < len(b) && i < len(c.parts) {
		within := off + int64(read) - c.starts[i]
		want := min(int64(len(b)-read), c.parts[i].Size()-within)
		n, err := c.parts[i].ReadAt(b[read:read+int(want)], within)
		read += n
		if int64(n) < want {
			if err == nil || err == io.EOF {
				err = io.ErrUnexpectedEOF // a part shorter than its Size
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
