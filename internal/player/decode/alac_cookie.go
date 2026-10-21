package decode

import "encoding/binary"

// The ALAC magic cookie (Apple's ALACMagicCookieDescription.txt) is the
// 24-byte ALACSpecificConfig, which an M4A carries in an alac FullBox inside
// the stsd sample entry of the same name. It is validated here, field by
// field, before saprobe-alac is given a configuration built from it: the
// library checks only the bit depth and the compatible version, and panics
// on a channel count of 0 or 9 or a frame length of 0 (Codex's B470
// evaluation).
const (
	alacCookieBytes = 24
	// alacAlacBox is the alac box in the sample entry: its header, its
	// version and flags, and the cookie.
	alacAlacBox = 8 + 4 + alacCookieBytes
	// alacSampleEntry is an ISO AudioSampleEntry's fields after the box
	// header (ISO/IEC 14496-12 §12.2.3): reserved, data_reference_index,
	// two reserved words, channelcount, samplesize, two more, samplerate.
	alacSampleEntry = 28
	// minALACFrameLength keeps the predictor's warm-up inside its buffers:
	// it reads 31 entries whatever the packet's length, and the library
	// sizes its buffers by the frame length.
	minALACFrameLength = 32
	// maxALACFrameLength bounds the library's per-decoder buffers. Apple's
	// and FFmpeg's encoders write 4096.
	maxALACFrameLength = 1 << 16
	// alacElementCPE is the channel pair element's tag; a stereo packet
	// opens with one.
	alacElementCPE = 1
)

// alacConfig is a validated ALACSpecificConfig.
type alacConfig struct {
	frameLength   uint32
	bitDepth      uint8
	pb, mb, kb    uint8
	channels      uint8
	maxRun        uint16
	maxFrameBytes uint32
	avgBitRate    uint32
	sampleRate    uint32
}

// maxPacketBytes is the largest packet the cookie admits: its max frame
// bytes when it states them, and never more than Apple's encoder allocates
// for one, frameLength × channels × (10 + 32) / 8 + 1, which an escape
// packet of 32-bit samples stays under.
func (c alacConfig) maxPacketBytes() uint32 {
	bound := uint64(c.frameLength)*uint64(c.channels)*5 + 1
	if c.maxFrameBytes != 0 && uint64(c.maxFrameBytes) < bound {
		return c.maxFrameBytes
	}
	return uint32(bound)
}

// parseALACSampleDescription reads an stsd payload that must hold exactly
// one ALAC AudioSampleEntry, version 0, with data reference 1, two channels
// and one alac box of 36 bytes, and returns the validated cookie. The entry's
// samplesize and samplerate are not compared with the cookie: afconvert
// writes 16 and 44100 there whatever the stream, and a channel count of 2
// even for mono, so the cookie's count is the one that refuses a stream
// that is not stereo. FFmpeg writes the stream's own values.
func parseALACSampleDescription(p []byte) (alacConfig, error) {
	if len(p) < 8 {
		return alacConfig{}, refuse(ReasonTruncated, "M4A stsd box of %d bytes", len(p))
	}
	if p[0] != 0 {
		return alacConfig{}, refuse(ReasonUnsupportedLayout, "M4A stsd version %d; version 0 is read", p[0])
	}
	if n := binary.BigEndian.Uint32(p[4:8]); n != 1 {
		return alacConfig{}, refuse(ReasonUnsupportedLayout, "M4A stsd has %d entries; one sample description is read", n)
	}
	rest := p[8:]
	if len(rest) < 8 {
		return alacConfig{}, refuse(ReasonTruncated, "M4A sample entry does not fit stsd")
	}
	size := uint64(binary.BigEndian.Uint32(rest[0:4]))
	if size < 8+alacSampleEntry || size > uint64(len(rest)) {
		return alacConfig{}, refuse(ReasonTruncated, "M4A sample entry of %d bytes does not fit stsd's %d", size, len(rest))
	}
	switch typ := string(rest[4:8]); typ {
	case "alac":
	case "mp4a":
		return alacConfig{}, refuse(ReasonLossy, "AAC is refused; the bridge player is bit-exact lossless only")
	default:
		return alacConfig{}, refuse(ReasonUnsupportedComp, "M4A audio codec %q is not ALAC", typ)
	}
	e := rest[8:size]
	if ref := binary.BigEndian.Uint16(e[6:8]); ref != 1 {
		return alacConfig{}, refuse(ReasonUnsupportedLayout, "M4A sample entry names data reference %d; one is read", ref)
	}
	if v := binary.BigEndian.Uint16(e[8:10]); v != 0 {
		return alacConfig{}, refuse(ReasonUnsupportedLayout, "M4A sound sample description version %d; version 0 is read", v)
	}
	entryChannels := binary.BigEndian.Uint16(e[16:18])
	if entryChannels != 2 {
		return alacConfig{}, refuse(ReasonUnsupportedChannels, "want stereo (2 channels), the sample entry has %d", entryChannels)
	}
	cookie, err := alacCookieBox(e[alacSampleEntry:])
	if err != nil {
		return alacConfig{}, err
	}
	return parseALACCookie(cookie)
}

// alacCookieBox finds the sample entry's one alac box among its child boxes
// and returns the cookie it holds. Other children (chan, btrt) are skipped.
func alacCookieBox(children []byte) ([]byte, error) {
	var cookie []byte
	for n := 0; len(children) > 0; n++ {
		if len(children) < 8 || n >= maxM4AChildren {
			return nil, refuse(ReasonUnsupportedLayout, "M4A sample entry's boxes do not fit it")
		}
		size := uint64(binary.BigEndian.Uint32(children[0:4]))
		typ := string(children[4:8])
		if size < 8 {
			return nil, refuse(ReasonUnsupportedLayout, "M4A box %q in the sample entry has size %d", typ, size)
		}
		if size > uint64(len(children)) {
			if typ == "alac" {
				return nil, refuse(ReasonTruncated, "M4A alac box of %d bytes runs past its sample entry", size)
			}
			return nil, refuse(ReasonTruncated, "M4A box %q of %d bytes runs past its sample entry", typ, size)
		}
		if typ == "alac" {
			switch {
			case cookie != nil:
				return nil, refuse(ReasonUnsupportedLayout, "M4A sample entry holds two alac boxes")
			case size != alacAlacBox:
				return nil, refuse(ReasonUnsupportedLayout, "M4A alac box of %d bytes; the cookie's is %d", size, alacAlacBox)
			case binary.BigEndian.Uint32(children[8:12]) != 0:
				return nil, refuse(ReasonUnsupportedLayout, "M4A alac box version and flags %#x; 0 is read", binary.BigEndian.Uint32(children[8:12]))
			}
			cookie = children[12:size]
		}
		children = children[size:]
	}
	if cookie == nil {
		return nil, refuse(ReasonUnsupportedLayout, "M4A ALAC sample entry has no alac box")
	}
	return cookie, nil
}

// parseALACCookie validates an ALACSpecificConfig. Compatible version 0;
// two channels; 16, 24 or 32 bits (20-bit ALAC is refused, as every depth
// but those three is); a frame length of 32 to 65536; kb at most 31, the
// shifts it drives staying inside 32 bits; a rate the player accepts; and a
// max frame bytes and an average bit rate that are 0 (unknown) or within
// what a packet of that frame length can be. pb and mb may be any byte: the
// decoder's arithmetic holds for each, and FFmpeg writes 0 for all three
// tuning values at compression level 0 where Apple and FFmpeg otherwise
// write 40, 10 and 14. maxRun is not used by the decoder; Apple writes 255
// and FFmpeg 0.
func parseALACCookie(b []byte) (alacConfig, error) {
	if len(b) != alacCookieBytes {
		return alacConfig{}, refuse(ReasonTruncated, "ALAC cookie of %d bytes, want %d", len(b), alacCookieBytes)
	}
	c := alacConfig{
		frameLength:   binary.BigEndian.Uint32(b[0:4]),
		bitDepth:      b[5],
		pb:            b[6],
		mb:            b[7],
		kb:            b[8],
		channels:      b[9],
		maxRun:        binary.BigEndian.Uint16(b[10:12]),
		maxFrameBytes: binary.BigEndian.Uint32(b[12:16]),
		avgBitRate:    binary.BigEndian.Uint32(b[16:20]),
		sampleRate:    binary.BigEndian.Uint32(b[20:24]),
	}
	if v := b[4]; v != 0 {
		return alacConfig{}, refuse(ReasonUnsupportedLayout, "ALAC compatible version %d; version 0 is read", v)
	}
	if c.channels != 2 {
		return alacConfig{}, refuse(ReasonUnsupportedChannels, "want stereo (2 channels), the ALAC cookie has %d", c.channels)
	}
	switch c.bitDepth {
	case 16, 24, 32:
	default:
		return alacConfig{}, refuse(ReasonUnsupportedBits, "%d-bit ALAC is refused; want 16, 24 or 32", c.bitDepth)
	}
	if c.frameLength < minALACFrameLength || c.frameLength > maxALACFrameLength {
		return alacConfig{}, refuse(ReasonUnsupportedLayout, "ALAC frame length %d is outside %d..%d", c.frameLength, minALACFrameLength, maxALACFrameLength)
	}
	if c.kb > 31 {
		return alacConfig{}, refuse(ReasonUnsupportedLayout, "ALAC kb %d is past 31", c.kb)
	}
	if err := acceptRate(int(c.sampleRate)); err != nil {
		return alacConfig{}, err
	}
	bound := uint64(c.frameLength)*uint64(c.channels)*5 + 1
	if uint64(c.maxFrameBytes) > bound {
		return alacConfig{}, refuse(ReasonTruncated, "ALAC max frame bytes %d exceed the %d a packet of %d frames can be", c.maxFrameBytes, bound, c.frameLength)
	}
	if rate := bound * 8 * uint64(c.sampleRate) / uint64(c.frameLength); uint64(c.avgBitRate) > rate {
		return alacConfig{}, refuse(ReasonTruncated, "ALAC average bit rate %d exceeds the %d packets of %d frames can carry", c.avgBitRate, rate, c.frameLength)
	}
	return c, nil
}

// checkALACPacket reads the element header that opens packet p before the
// library decodes it, and refuses what the container, the cookie or the
// library cannot take. The packet must open with a channel pair: the
// library returns a frame it never wrote when an END comes first, and
// writes one channel when a single-channel element does (Apple's and
// FFmpeg's encoders open every stereo packet with the pair). The unused bits
// are zero and the shift is at most two bytes. The packet holds the frames
// stts gives it: the cookie's frame length, or the count a partial packet
// carries. A compressed pair of 32-bit samples with no shifted byte would
// make the entropy decoder read 33-bit residuals, past the 32 its bit reads
// hold. A compressed 16-bit pair shifts no byte out: the library reads the
// shifted bytes and its 16-bit output drops them, returning the residuals
// alone and no error (Codex's review of #1300), and Apple's and FFmpeg's
// encoders shift only above 16 bits. A compressed packet needs a kb of at
// least 1.
func checkALACPacket(p int, pkt []byte, frames int, c alacConfig) error {
	if len(pkt) < 3 {
		return refuse(ReasonTruncated, "ALAC packet %d of %d bytes holds no element header", p, len(pkt))
	}
	var w [8]byte
	copy(w[:], pkt)
	v := binary.BigEndian.Uint64(w[:])
	tag, unused := v>>61, v>>45&0xFFF
	partial, shift, escape := v>>44&1, v>>42&3, v>>41&1
	switch {
	case tag != alacElementCPE:
		return refuse(ReasonTruncated, "ALAC packet %d opens with element %d; a stereo packet opens with a channel pair", p, tag)
	case unused != 0:
		return refuse(ReasonTruncated, "ALAC packet %d sets its element header's unused bits", p)
	case shift == 3:
		return refuse(ReasonTruncated, "ALAC packet %d shifts 3 bytes; at most 2 are", p)
	}
	n := uint64(c.frameLength)
	if partial == 1 {
		if len(pkt) < 7 {
			return refuse(ReasonTruncated, "ALAC packet %d ends inside its frame count", p)
		}
		n = v >> 9 & 0xFFFFFFFF
	}
	if n != uint64(frames) {
		return refuse(ReasonTruncated, "ALAC packet %d holds %d frames; stts gives it %d", p, n, frames)
	}
	if escape == 0 {
		if bits := uint64(c.bitDepth) + 1 - 8*shift; bits > 32 {
			return refuse(ReasonTruncated, "ALAC packet %d is a compressed %d-bit pair with no shifted byte: its residuals would be %d bits", p, c.bitDepth, bits)
		}
		if c.bitDepth == 16 && shift != 0 {
			return refuse(ReasonTruncated, "ALAC packet %d is a compressed 16-bit pair with a shift (%d): the 16-bit output drops shifted bytes", p, shift)
		}
		if c.kb == 0 {
			return refuse(ReasonTruncated, "ALAC packet %d is compressed and the cookie's kb is 0", p)
		}
	}
	return nil
}
