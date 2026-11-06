package manifest

// WavPack (.wv), read from its first audio block's 32-byte header and its
// metadata sub-blocks (backlog B489). Nothing is decoded: the bridge serves
// the file as it is, and reads what the header states and nothing more.
//
// The layout is WavPack 5's (WavPack5FileFormat.pdf; wavpack.h's
// WavpackHeader, its flags and its ID_ values), which WavPack 4 shares: a
// stream of blocks, each "wvpk", ckSize (the block's bytes less 8), the
// stream version (0x402 to 0x410 are the ones a decoder reads), the upper
// bytes of a 40-bit block index and total sample count, their lower 32
// bits, the block's samples, the flags and a CRC; then metadata sub-blocks,
// each an id byte (ID_ODD_SIZE: the last byte is a pad; ID_LARGE: a 3-byte
// length follows), a length in 16-bit words, and the data.
//
// What a header can say, it says here: the sample rate (an index into
// WavPack's table, or 15 and an ID_SAMPLE_RATE sub-block), the bytes stored
// per sample and the bits shifted out of them, mono or stereo (and the
// channel count of ID_CHANNEL_INFO beyond two), 32-bit float, the hybrid
// mode, and WavPack 5's DSD: its samples count bytes of eight DSD bits, its
// rate is the byte rate (the table's rate shifted left by the power ID_DSD's
// first byte holds), and the bit rate a client reports is eight times that.
//
// Every length a file holds is checked against what the file and the block
// hold before anything is read on its account, and nothing is allocated by
// it: the search for a block reads a fixed window at a time, and a
// sub-block is either skipped by its length or read into a buffer of a size
// fixed here. A file that holds no block the reference decoder would take
// is refused (an error the scanner indexes by name, backlog B145), never
// read by a guess.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// The header flags and metadata ids the reader takes, as wavpack.h names
// them.
const (
	wvFlagBytesStored  = 0x3 // bytes stored per sample, less one
	wvFlagMono         = 0x4
	wvFlagHybrid       = 0x8
	wvFlagFloat        = 0x80
	wvFlagShiftLSB     = 13
	wvFlagFinalBlock   = 0x1000                 // the last block of a frame
	wvFlagShiftMask    = 0x1f << wvFlagShiftLSB // low bits a sample does not use
	wvFlagSrateLSB     = 23
	wvFlagSrateMask    = 0xf << wvFlagSrateLSB
	wvFlagFalseStereo  = 0x40000000
	wvFlagDSD          = 0x80000000
	wvSrateCustomIndex = 15 // the rate is in ID_SAMPLE_RATE

	wvIDUnique       = 0x3f
	wvIDOddSize      = 0x40
	wvIDLarge        = 0x80
	wvIDWVBitstream  = 0xa
	wvIDWVCBitstream = 0xb
	wvIDChannelInfo  = 0xd
	wvIDDSDBlock     = 0xe
	wvIDSampleRate   = 0x27

	wvMinStreamVersion = 0x402
	wvMaxStreamVersion = 0x410
	wvHeaderBytes      = 32
)

// wvSampleRates is the header's rate table, by index (15 is a rate the
// table lacks).
var wvSampleRates = [15]uint32{6000, 8000, 9600, 11025, 12000, 16000, 22050,
	24000, 32000, 44100, 48000, 64000, 88200, 96000, 192000}

// wvSearchLimit is how far the reader looks for a block's header past where
// it starts looking: the reference decoder's read_next_header gives up past
// 1 MiB, and so does this.
const wvSearchLimit = 1 << 20

// wvSearchWindow is the buffer the search reads the file through.
const wvSearchWindow = 64 << 10

// wvMaxLeadingBlocks bounds the blocks with no samples (a block holding a
// RIFF header alone, say) passed over before the first block with audio.
const wvMaxLeadingBlocks = 16

// wvMaxDSDPower bounds the DSD multiplier's power. The byte rate is the
// table's rate shifted left by it, and DSD1024 at 48 kHz is 48000 << 7.
const wvMaxDSDPower = 10

// errWVNoBlock is a file in which no WavPack block's header is found where
// the reference decoder would look: not a WavPack file, or one this reader
// cannot take.
var errWVNoBlock = errors.New("wavpack: no block header within the search limit")

// wvHeader is one block's header.
type wvHeader struct {
	ckSize       uint32
	version      uint16
	blockIndex   uint64 // 40 bits
	totalLow     uint32
	totalHigh    uint8
	blockSamples uint32
	flags        uint32
}

// size is the whole block's bytes, its header included.
func (h wvHeader) size() int64 { return int64(h.ckSize) + 8 }

// totalSamples is wavpack.h's GET_TOTAL_SAMPLES: the 40-bit total, which
// SET_TOTAL_SAMPLES encodes so that a known count never puts 0xFFFFFFFF in
// the low 32 bits, and false for the unknown length (-1) a streaming
// encoder writes.
func (h wvHeader) totalSamples() (uint64, bool) {
	if h.totalLow == math.MaxUint32 {
		return 0, false
	}
	return uint64(h.totalLow) + uint64(h.totalHigh)<<32 - uint64(h.totalHigh), true
}

// parseWVHeader reads a block header from 32 bytes, or false where the
// reference decoder's read_next_header would not take them as one: the
// "wvpk" id, an even ckSize of at least 24 and under 16 MiB, a stream version
// it decodes (0x402 to 0x410), and a block of fewer than 196,608 samples.
func parseWVHeader(b []byte) (wvHeader, bool) {
	if len(b) < wvHeaderBytes || string(b[0:4]) != "wvpk" {
		return wvHeader{}, false
	}
	h := wvHeader{
		ckSize:       binary.LittleEndian.Uint32(b[4:8]),
		version:      binary.LittleEndian.Uint16(b[8:10]),
		blockIndex:   uint64(binary.LittleEndian.Uint32(b[16:20])) | uint64(b[10])<<32,
		totalLow:     binary.LittleEndian.Uint32(b[12:16]),
		totalHigh:    b[11],
		blockSamples: binary.LittleEndian.Uint32(b[20:24]),
		flags:        binary.LittleEndian.Uint32(b[24:28]),
	}
	if h.ckSize&1 != 0 || h.ckSize < 24 || h.ckSize >= 16<<20 {
		return wvHeader{}, false
	}
	if h.version < wvMinStreamVersion || h.version > wvMaxStreamVersion {
		return wvHeader{}, false
	}
	if h.blockSamples >= 3<<16 {
		return wvHeader{}, false
	}
	return h, true
}

// wvStream is what a WavPack stream's first audio block states.
type wvStream struct {
	header wvHeader
	// rate is the PCM sample rate, or a DSD stream's byte rate (its bit
	// rate over eight), which its sample counts count in.
	rate     uint32
	channels int
	// bits is a PCM sample's significant bits: 32 for float, otherwise the
	// bytes stored less the bits shifted out. 0 for DSD.
	bits   int
	float  bool
	hybrid bool
	dsd    bool
	// correctionBits says the block holds an ID_WVC_BITSTREAM: the
	// correction file's block, which a .wv's never holds.
	correctionBits bool
}

// duration is the stream's length in seconds from its header, and false when
// the header does not say it: a total the encoder could not write (a
// streamed file), or a first audio block whose index is not 0, about which
// the format says the total is not the file's.
func (s wvStream) duration() (float64, bool) {
	total, ok := s.header.totalSamples()
	if !ok || s.header.blockIndex != 0 || s.rate == 0 {
		return 0, false
	}
	return float64(total) / float64(s.rate), true
}

// readWavPackStream finds the stream's first audio block in r and reads what
// it states. A read that fails comes back as the reader's error, which the
// extractor's source has already noted when it did not complete; the end of
// the file is an answer, and a file that ends before what this needs is
// refused. So is a block the reference decoder's unpack_init refuses: a PCM
// block with no bitstream (a correction file's, which "can't unpack ...
// alone", among them), and one flagged both mono and false stereo.
func readWavPackStream(r io.ReaderAt) (wvStream, error) {
	off, h, err := firstWVAudioBlock(r)
	if err != nil {
		return wvStream{}, err
	}
	f, err := readWVBlock(r, off, h)
	if err != nil {
		return wvStream{}, err
	}
	s := wvStream{header: h, hybrid: h.flags&wvFlagHybrid != 0, dsd: h.flags&wvFlagDSD != 0,
		correctionBits: f.correctionBits}
	switch {
	case h.flags&(wvFlagMono|wvFlagFalseStereo) == wvFlagMono|wvFlagFalseStereo:
		return wvStream{}, errors.New("wavpack: a block flagged both mono and false stereo")
	case !s.dsd && !f.wvBits && f.correctionBits:
		return wvStream{}, errors.New("wavpack: a correction file's block, which no decoder unpacks alone")
	case !s.dsd && !f.wvBits:
		return wvStream{}, errors.New("wavpack: an audio block without its bitstream")
	}
	if err := s.settle(h, f); err != nil {
		return wvStream{}, err
	}
	return s, nil
}

// wvCorrection is what a bounded look at a correction file finds: its first
// audio block's header and whether that block holds a correction bitstream,
// and the block that ends the file (wvFinalIndex).
type wvCorrection struct {
	first          wvHeader
	correctionBits bool
	// final is the index one past the samples of the block that ends the
	// file, and finalFound whether a block ends it: a file cut short ends
	// inside a block, and none does.
	final      uint64
	finalFound bool
}

// readWavPackCorrection reads a correction file of size bytes: its first
// audio block's header, whether that block holds a correction bitstream, and
// the block that ends the file. Its format is the .wv's to state (the
// encoder writes a non-standard rate into the .wv's block alone), so nothing
// here is settled.
func readWavPackCorrection(r io.ReaderAt, size int64) (wvCorrection, error) {
	off, h, err := firstWVAudioBlock(r)
	if err != nil {
		return wvCorrection{}, err
	}
	f, err := readWVBlock(r, off, h)
	if err != nil {
		return wvCorrection{}, err
	}
	c := wvCorrection{first: h, correctionBits: f.correctionBits}
	c.final, c.finalFound, err = wvFinalIndex(r, size)
	if err != nil {
		return wvCorrection{}, err
	}
	return c, nil
}

// wvFinalIndex finds the block that ends a file of size bytes and returns
// the index one past its samples, as the reference decoder's
// seek_final_index finds a stream's length: it looks at the file's last
// wvSearchLimit bytes, a window at a time from the end, for a block header
// whose block ends exactly where the file does and is a final block (the
// last of its frame). false when there is none: a file cut short ends inside
// a block, and a file with anything after its last block (a correction file
// holds no tag) is not one an encoder wrote.
func wvFinalIndex(r io.ReaderAt, size int64) (uint64, bool, error) {
	const window = wvSearchWindow
	buf := make([]byte, window+wvHeaderBytes)
	floor := max(size-wvSearchLimit, 0)
	for end := size; end > floor; end -= window {
		start := max(end-window, floor)
		n, err := r.ReadAt(buf[:min(int64(len(buf)), size-start)], start)
		if err != nil && !endOfFile(err) {
			return 0, false, err
		}
		for i := 0; i+wvHeaderBytes <= n && start+int64(i) < end; i++ {
			if buf[i] != 'w' {
				continue
			}
			h, ok := parseWVHeader(buf[i : i+wvHeaderBytes])
			if !ok || start+int64(i)+h.size() != size || h.flags&wvFlagFinalBlock == 0 {
				continue
			}
			return h.blockIndex + uint64(h.blockSamples), true, nil
		}
	}
	return 0, false, nil
}

// firstWVAudioBlock finds the stream's first block with samples, passing
// over at most wvMaxLeadingBlocks without.
func firstWVAudioBlock(r io.ReaderAt) (int64, wvHeader, error) {
	off, h, err := findWVBlock(r, 0)
	if err != nil {
		return 0, wvHeader{}, err
	}
	for i := 0; h.blockSamples == 0; i++ {
		if i == wvMaxLeadingBlocks {
			return 0, wvHeader{}, errors.New("wavpack: no audio block among the first blocks")
		}
		if off, h, err = findWVBlock(r, off+h.size()); err != nil {
			return 0, wvHeader{}, err
		}
	}
	return off, h, nil
}

// findWVBlock finds the first block header at or after start, looking at
// most wvSearchLimit bytes past it, as the reference decoder does: a header
// it would not take is passed over, and the search goes on from the next
// "w". It reads a window at a time, so it allocates the window's bytes
// whatever the file holds.
func findWVBlock(r io.ReaderAt, start int64) (int64, wvHeader, error) {
	buf := make([]byte, wvSearchWindow+wvHeaderBytes)
	for pos := start; pos <= start+wvSearchLimit; {
		n, err := r.ReadAt(buf, pos)
		if err != nil && !endOfFile(err) {
			return 0, wvHeader{}, err
		}
		if n < wvHeaderBytes {
			return 0, wvHeader{}, errWVNoBlock
		}
		for i := 0; i+wvHeaderBytes <= n; i++ {
			if pos+int64(i) > start+wvSearchLimit {
				return 0, wvHeader{}, errWVNoBlock
			}
			if buf[i] != 'w' {
				continue
			}
			if h, ok := parseWVHeader(buf[i : i+wvHeaderBytes]); ok {
				return pos + int64(i), h, nil
			}
		}
		if n < len(buf) {
			return 0, wvHeader{}, errWVNoBlock
		}
		pos += int64(n - wvHeaderBytes + 1)
	}
	return 0, wvHeader{}, errWVNoBlock
}

// wvBlockFacts is what a block's sub-blocks say beside its header.
type wvBlockFacts struct {
	customRate     uint32
	haveRate       bool
	dsdPower       int // -1: no DSD sub-block, or one that ended before it
	channels       int // 0: no channel layout
	wvBits         bool
	correctionBits bool
}

// readWVBlock reads the sub-blocks of the block at off, whose header is h:
// the rate, the channel layout and DSD's multiplier, and whether a
// correction bitstream is there, skipping every other by its length. A
// sub-block that runs past its block is refused, as the reference decoder
// refuses it; the file ending inside the block ends the walk with what was
// read.
func readWVBlock(r io.ReaderAt, off int64, h wvHeader) (wvBlockFacts, error) {
	f := wvBlockFacts{dsdPower: -1}
	end := off + h.size()
	for pos := off + wvHeaderBytes; pos < end; {
		var hdr [4]byte
		n, err := r.ReadAt(hdr[:], pos)
		if err != nil && !endOfFile(err) {
			return wvBlockFacts{}, err
		}
		if n < 2 {
			break
		}
		id := hdr[0]
		words, hdrLen := int64(hdr[1]), int64(2)
		if id&wvIDLarge != 0 {
			if n < 4 {
				break
			}
			words, hdrLen = int64(hdr[1])|int64(hdr[2])<<8|int64(hdr[3])<<16, 4
		}
		dataLen := words * 2
		if id&wvIDOddSize != 0 {
			if dataLen == 0 {
				return wvBlockFacts{}, errors.New("wavpack: a metadata sub-block of odd size holds nothing")
			}
			dataLen--
		}
		dataAt := pos + hdrLen
		if dataAt+words*2 > end {
			return wvBlockFacts{}, errors.New("wavpack: a metadata sub-block runs past its block")
		}
		if err := f.read(r, id&wvIDUnique, dataAt, dataLen); err != nil {
			return wvBlockFacts{}, err
		}
		pos = dataAt + words*2
	}
	return f, nil
}

// read takes what one sub-block of function id, dataLen bytes at at, says.
func (f *wvBlockFacts) read(r io.ReaderAt, id byte, at, dataLen int64) error {
	switch id {
	case wvIDSampleRate:
		rate, ok, err := readWVSampleRate(r, at, dataLen)
		if err != nil {
			return err
		}
		if ok {
			f.customRate, f.haveRate = rate, true
		}
	case wvIDChannelInfo:
		c, err := readWVChannelInfo(r, at, dataLen)
		if err != nil {
			return err
		}
		f.channels = c
	case wvIDDSDBlock:
		if dataLen >= 2 {
			var p [1]byte
			n, err := r.ReadAt(p[:], at)
			if err != nil && !endOfFile(err) {
				return err
			}
			if n == 1 {
				f.dsdPower = int(p[0])
			}
		}
	case wvIDWVBitstream:
		f.wvBits = true
	case wvIDWVCBitstream:
		f.correctionBits = true
	}
	return nil
}

// readWVSampleRate reads an ID_SAMPLE_RATE sub-block: 3 bytes of rate, and a
// fourth whose low 7 bits are the rate's top byte. A length the reference
// decoder ignores (neither 3 nor 4) says nothing, and false.
func readWVSampleRate(r io.ReaderAt, at, n int64) (uint32, bool, error) {
	if n != 3 && n != 4 {
		return 0, false, nil
	}
	var b [4]byte
	got, err := r.ReadAt(b[:n], at)
	if err != nil && !endOfFile(err) {
		return 0, false, err
	}
	if int64(got) < n {
		return 0, false, nil
	}
	rate := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
	if n == 4 {
		rate |= uint32(b[3]&0x7f) << 24
	}
	return rate, true, nil
}

// readWVChannelInfo reads an ID_CHANNEL_INFO sub-block's channel count, as
// the reference decoder's read_channel_info does: 1 to 5 bytes, the count
// and then a channel mask; 6 or 7, a 12-bit count less one, the streams and
// a wider mask. A count of 0, a length it refuses and a mask naming more
// channels than the count are an error: the decoder refuses such a file.
func readWVChannelInfo(r io.ReaderAt, at, n int64) (int, error) {
	if n < 1 || n > 7 {
		return 0, fmt.Errorf("wavpack: a channel layout of %d bytes", n)
	}
	var b [7]byte
	got, err := r.ReadAt(b[:n], at)
	if err != nil && !endOfFile(err) {
		return 0, err
	}
	if int64(got) < n {
		return 0, errors.New("wavpack: the file ends inside its channel layout")
	}
	var count int
	var mask uint32
	if n >= 6 {
		count = (int(b[0]) | int(b[2]&0xf)<<8) + 1
		streams := (int(b[1]) | int(b[2]&0xf0)<<4) + 1
		if count < streams {
			return 0, errors.New("wavpack: a channel layout with fewer channels than streams")
		}
		mask = uint32(b[3]) | uint32(b[4])<<8 | uint32(b[5])<<16
		if n == 7 {
			mask |= uint32(b[6]) << 24
		}
	} else {
		count = int(b[0])
		for i := int64(1); i < n; i++ {
			mask |= uint32(b[i]) << (8 * uint(i-1))
		}
	}
	if count == 0 {
		return 0, errors.New("wavpack: a channel layout of no channels")
	}
	bits := 0
	for m := mask; m != 0; m >>= 1 {
		bits += int(m & 1)
	}
	if bits > count {
		return 0, errors.New("wavpack: a channel mask naming more channels than the layout holds")
	}
	return count, nil
}

// settle decides the stream's format from its header and the sub-blocks
// read, or refuses it: a rate of 0 or none, a DSD stream with no multiplier,
// a hybrid DSD stream (DSD has no hybrid mode), a float stream not stored in
// four bytes, or a shift past the bytes stored. No division happens before
// these checks.
func (s *wvStream) settle(h wvHeader, f wvBlockFacts) error {
	base, dsdPower, channels := f.customRate, f.dsdPower, f.channels
	if !f.haveRate {
		idx := (h.flags & wvFlagSrateMask) >> wvFlagSrateLSB
		if idx == wvSrateCustomIndex {
			return errors.New("wavpack: the header names a rate its block does not carry")
		}
		base = wvSampleRates[idx]
	}
	if base == 0 {
		return errors.New("wavpack: a sample rate of 0")
	}
	bytesStored := int(h.flags&wvFlagBytesStored) + 1
	switch {
	case s.dsd:
		if s.hybrid {
			return errors.New("wavpack: a hybrid DSD stream, which the format does not define")
		}
		if dsdPower < 0 || dsdPower > wvMaxDSDPower {
			return errors.New("wavpack: a DSD stream without its multiplier")
		}
		rate := uint64(base) << uint(dsdPower)
		if rate*8 > math.MaxInt32 {
			return errors.New("wavpack: a DSD rate past what a client can be told")
		}
		s.rate = uint32(rate)
	case h.flags&wvFlagFloat != 0:
		if bytesStored != 4 {
			return errors.New("wavpack: float samples not stored in four bytes")
		}
		s.float, s.bits, s.rate = true, 32, base
	default:
		shift := int((h.flags & wvFlagShiftMask) >> wvFlagShiftLSB)
		if shift >= bytesStored*8 {
			return errors.New("wavpack: more bits shifted out than are stored")
		}
		s.bits, s.rate = bytesStored*8-shift, base
	}
	s.channels = channels
	if s.channels == 0 {
		s.channels = 2
		if h.flags&wvFlagMono != 0 {
			s.channels = 1
		}
	}
	return nil
}

// correctionMatches reports whether wvc, a bounded look at a correction file,
// is the one the stream wv's lossless reconstruction needs: its first audio
// block holds a correction bitstream, with the same block index, block
// samples and total samples as the .wv's first audio block (the encoder
// writes both from one header), and a block ends the file, at the stream's
// last sample where the .wv's header states its length from its start. A
// file cut short, or of another stream, is not.
func correctionMatches(wv wvStream, wvc wvCorrection) bool {
	h := wvc.first
	if !wvc.correctionBits || !wvc.finalFound ||
		h.blockIndex != wv.header.blockIndex || h.blockSamples != wv.header.blockSamples ||
		h.totalLow != wv.header.totalLow || h.totalHigh != wv.header.totalHigh {
		return false
	}
	if total, ok := wv.header.totalSamples(); ok && wv.header.blockIndex == 0 {
		return wvc.final == total
	}
	return true
}
