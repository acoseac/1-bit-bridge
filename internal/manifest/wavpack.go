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
	wvFlagInt32        = 0x100 // INT32_DATA: 32-bit integers, extended
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
	wvIDFloatInfo    = 0x8
	wvIDInt32Info    = 0x9
	wvIDWVBitstream  = 0xa
	wvIDWVCBitstream = 0xb
	wvIDWVXBitstream = 0xc
	wvIDChannelInfo  = 0xd
	wvIDDSDBlock     = 0xe
	wvIDSampleRate   = 0x27
	// wvIDWVXNewBitstream is the extended bitstream in its optional form
	// (ID_OPTIONAL_DATA | ID_WVX_BITSTREAM), which a decoder that does not
	// know it passes over.
	wvIDWVXNewBitstream = 0x2c

	// The float flags (ID_FLOAT_INFO's first byte) that send bits the main
	// bitstream does not carry: FLOAT_SHIFT_SAME, FLOAT_SHIFT_SENT,
	// FLOAT_ZEROS_SENT and FLOAT_EXCEPTIONS, as the reference decoder's
	// unpack_init counts a block lossy without its extended bitstream.
	wvFloatSendsExtended = 0x02 | 0x04 | 0x08 | 0x20

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

// errWVBlockCut is a file that ends inside its first audio block, which the
// reference decoder reads whole before it decodes anything.
var errWVBlockCut = errors.New("wavpack: the file ends inside its first audio block")

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
	// needsExtended says the block's samples send bits the main bitstream
	// does not carry: a 32-bit integer stream's sent bits (ID_INT32_INFO)
	// or a float stream's exceptions, zeros or shifts (ID_FLOAT_INFO), which
	// an extended bitstream (ID_WVX_BITSTREAM) carries; extendedBits says the
	// block holds one. A stream that needs one and holds none decodes lossy,
	// as the reference decoder's unpack_init counts it (lossy_blocks).
	needsExtended, extendedBits bool
	// final is the index one past the samples of the block that ends the
	// stream's audio, and finalFound whether one does (wvFinalIndex, over the
	// file's bytes before its tags): read only for a hybrid stream whose
	// header states no length from its start, whose correction file must end
	// where it does (wvStreamFinal).
	final      uint64
	finalFound bool
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

// readWavPackStream finds the stream's first audio block in r, a file of size
// bytes, and reads what it states. A read that fails comes back as the
// reader's error, which the extractor's source has already noted when it did
// not complete; the end of the file is an answer, and a file that ends before
// what this needs is refused: the reference decoder reads its first audio
// block whole, so a file that ends inside it is refused too. So is a block
// the reference decoder's unpack_init refuses: a PCM block with no bitstream
// (a correction file's, which "can't unpack ... alone", among them), and one
// flagged both mono and false stereo.
func readWavPackStream(r io.ReaderAt, size int64) (wvStream, error) {
	off, h, err := firstWVAudioBlock(r)
	if err != nil {
		return wvStream{}, err
	}
	if off+h.size() > size {
		return wvStream{}, errWVBlockCut
	}
	f, err := readWVBlock(r, off, h)
	if err != nil {
		return wvStream{}, err
	}
	s := wvStream{header: h, hybrid: h.flags&wvFlagHybrid != 0, dsd: h.flags&wvFlagDSD != 0,
		correctionBits: f.correctionBits, extendedBits: f.wvxBits,
		needsExtended: (h.flags&wvFlagInt32 != 0 && f.int32SentBits != 0) ||
			(h.flags&wvFlagFloat != 0 && f.floatFlags&wvFloatSendsExtended != 0)}
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
	// extendedBits says the first block holds an extended bitstream
	// (ID_WVX_BITSTREAM), which a hybrid stream's correction file carries for
	// samples that need one.
	extendedBits bool
	// final is the index one past the samples of the block that ends the
	// file, and finalFound whether a block ends it: a file cut short ends
	// inside a block, and none does.
	final      uint64
	finalFound bool
}

// readWavPackCorrection reads a correction file of size bytes: its first
// audio block's header, whether that block holds a correction bitstream and
// an extended one, and the block that ends the file. Its format is the .wv's
// to state (the encoder writes a non-standard rate into the .wv's block
// alone), so nothing here is settled. A file that ends inside its first audio
// block is refused, as a .wv is.
func readWavPackCorrection(r io.ReaderAt, size int64) (wvCorrection, error) {
	off, h, err := firstWVAudioBlock(r)
	if err != nil {
		return wvCorrection{}, err
	}
	if off+h.size() > size {
		return wvCorrection{}, errWVBlockCut
	}
	f, err := readWVBlock(r, off, h)
	if err != nil {
		return wvCorrection{}, err
	}
	c := wvCorrection{first: h, correctionBits: f.correctionBits, extendedBits: f.wvxBits}
	c.final, c.finalFound, err = wvFinalIndex(r, size)
	if err != nil {
		return wvCorrection{}, err
	}
	return c, nil
}

// wvMaxTrailingBlocks bounds the blocks with no samples the look for a
// stream's end passes over after its last block with samples. An encoder
// writes one or two there (its MD5 sum, a RIFF trailer); the reference
// decoder's seek_final_index walks every block of its last wvSearchLimit
// bytes, and the bound is this reader's, against a file of many tiny blocks,
// each a look back of its own.
const wvMaxTrailingBlocks = 16

// wvFinalIndex finds where the audio of a file of size bytes ends and returns
// the index one past its last sample, as the reference decoder's
// seek_final_index finds a stream's length: in the file's last wvSearchLimit
// bytes, the last block with samples, which must be a final block (the last
// of its frame). Blocks with no samples after it are passed over, as
// seek_final_index passes over them: the reference encoder writes its MD5 sum
// and a RIFF trailer after the audio, in a block of metadata alone whose
// header is cleared but for its id, version, size and total
// (write_metadata_block), so it is no final block, and its index is 0. The
// look starts at the block that ends exactly where the file does and walks
// back over at most wvMaxTrailingBlocks such blocks, each found as the block
// that ends where the one after it begins (round 5 of backlog B489,
// CodeRabbit: it took only a final block ending the file, so a .wv whose
// header states no length and that ends with its MD5 sum matched no
// correction file). false when there is none: a file cut short ends inside a
// block, and a file with anything after its last block (a correction file
// holds no tag) is not one an encoder wrote. Nor is one found where the blocks
// with no samples fill the whole of the file's last wvSearchLimit bytes (a
// RIFF trailer past 1 MiB): no block with samples starts there, as
// seek_final_index finds no final block in its last 1 MiB either, so the
// stream's end is not known, and no correction file matches it
// (correctionMatches): its row is WAVPACK-LOSSY.
func wvFinalIndex(r io.ReaderAt, size int64) (uint64, bool, error) {
	floor := max(size-wvSearchLimit, 0)
	buf := make([]byte, wvSearchWindow+wvHeaderBytes)
	end := size
	for passed := 0; ; passed++ {
		at, h, found, err := wvBlockEndingAt(r, buf, floor, end)
		if err != nil || !found {
			return 0, false, err
		}
		if h.blockSamples != 0 {
			if h.flags&wvFlagFinalBlock == 0 {
				return 0, false, nil
			}
			return h.blockIndex + uint64(h.blockSamples), true, nil
		}
		if passed == wvMaxTrailingBlocks {
			return 0, false, nil
		}
		end = at
	}
}

// wvBlockEndingAt finds a block header starting at or after floor whose block
// ends exactly at end, looking back from end a window at a time through buf:
// in a window, the first such header from its start.
func wvBlockEndingAt(r io.ReaderAt, buf []byte, floor, end int64) (int64, wvHeader, bool, error) {
	const window = wvSearchWindow
	for hi := end; hi > floor; hi -= window {
		start := max(hi-window, floor)
		n, err := r.ReadAt(buf[:min(int64(len(buf)), end-start)], start)
		if err != nil && !endOfFile(err) {
			return 0, wvHeader{}, false, err
		}
		for i := 0; i+wvHeaderBytes <= n && start+int64(i) < hi; i++ {
			if buf[i] != 'w' {
				continue
			}
			if h, ok := parseWVHeader(buf[i : i+wvHeaderBytes]); ok && start+int64(i)+h.size() == end {
				return start + int64(i), h, true, nil
			}
		}
	}
	return 0, wvHeader{}, false, nil
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
	// int32SentBits is ID_INT32_INFO's sent bits and floatFlags
	// ID_FLOAT_INFO's flags, each 0 where the block holds none; wvxBits says
	// it holds an extended bitstream, one with bits past its 4-byte CRC.
	int32SentBits, floatFlags byte
	wvxBits                   bool
}

// wvBlockWindow is how much of a block a walk of its sub-blocks reads at a
// time.
const wvBlockWindow = 64 << 10

// readWVBlock reads the sub-blocks of the block at off, whose header is h and
// which its caller found whole inside the file: the rate, the channel layout,
// DSD's multiplier, the int32 and float information, and which bitstreams are
// there, skipping every other by its length. It reads the block a window at a
// time (wvWindow), so a block of many small sub-blocks costs a read a window,
// never one a sub-block. A sub-block that runs past its block is refused, as
// the reference decoder refuses it, and so is the file ending inside the
// block, which is the file changing while it is read.
func readWVBlock(r io.ReaderAt, off int64, h wvHeader) (wvBlockFacts, error) {
	f := wvBlockFacts{dsdPower: -1}
	end := off + h.size()
	w := wvWindow{r: r, end: end, buf: make([]byte, min(int64(wvBlockWindow), h.size()))}
	for pos := off + wvHeaderBytes; pos < end; {
		hdr, err := w.bytes(pos, 2)
		if err != nil {
			return wvBlockFacts{}, err
		}
		id := hdr[0]
		words, hdrLen := int64(hdr[1]), int64(2)
		if id&wvIDLarge != 0 {
			if end-pos < 4 {
				break // as the reference decoder's read_metadata_buff stops
			}
			large, err := w.bytes(pos, 4)
			if err != nil {
				return wvBlockFacts{}, err
			}
			words, hdrLen = int64(large[1])|int64(large[2])<<8|int64(large[3])<<16, 4
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
		if err := f.read(&w, id&wvIDUnique, dataAt, dataLen); err != nil {
			return wvBlockFacts{}, err
		}
		pos = dataAt + words*2
	}
	return f, nil
}

// wvWindow reads a block a window at a time: bytes answers the k bytes at pos
// from the window it holds, or reads a new window from pos, never past the
// block's end. Its buffer is the window's, whatever the block claims.
type wvWindow struct {
	r     io.ReaderAt
	end   int64 // the block's end
	buf   []byte
	start int64 // the offset of buf's first byte
	n     int   // the bytes buf holds
}

// bytes returns the k bytes at pos, k at most the window. The block is the
// caller's to have found inside the file by the size its stat gave, so a read
// that ends inside it is the file shrinking while it was read (errFileShrank),
// a read that did not complete, never a block cut as the file was written
// (errWVBlockCut, which the caller answers before it reads a byte of the
// block; round 5 of backlog B489, CodeRabbit). Through statBoundReader, as the
// extractor reads, that read's own answer says so first, and is noted.
func (w *wvWindow) bytes(pos int64, k int) ([]byte, error) {
	if pos < w.start || pos+int64(k) > w.start+int64(w.n) {
		want := min(int64(len(w.buf)), w.end-pos)
		if int64(k) > want {
			return nil, errors.New("wavpack: a metadata sub-block runs past its block")
		}
		n, err := w.r.ReadAt(w.buf[:want], pos)
		if err != nil && !endOfFile(err) {
			return nil, err
		}
		w.start, w.n = pos, n
		if int64(n) < want {
			return nil, errFileShrank
		}
	}
	return w.buf[pos-w.start : pos-w.start+int64(k)], nil
}

// read takes what one sub-block of function id, dataLen bytes at at, says,
// refusing what the reference decoder's readers refuse: ID_INT32_INFO and
// ID_FLOAT_INFO of any length but 4 (read_int32_info, read_float_info), a
// main or correction bitstream that is empty or of odd length
// (init_wv_bitstream, init_wvc_bitstream), and an extended bitstream of its
// 4-byte CRC alone or of odd length (init_wvx_bitstream). Its optional form
// refuses nothing, since a decoder that does not know it passes over it, and
// carries extended bits only past its CRC.
func (f *wvBlockFacts) read(w *wvWindow, id byte, at, dataLen int64) error {
	switch id {
	case wvIDSampleRate:
		// A length the reference decoder ignores (neither 3 nor 4) says
		// nothing.
		if dataLen == 3 || dataLen == 4 {
			b, err := w.bytes(at, int(dataLen))
			if err != nil {
				return err
			}
			f.customRate, f.haveRate = wvSampleRateOf(b), true
		}
	case wvIDChannelInfo:
		if dataLen < 1 || dataLen > 7 {
			return fmt.Errorf("wavpack: a channel layout of %d bytes", dataLen)
		}
		b, err := w.bytes(at, int(dataLen))
		if err != nil {
			return err
		}
		c, err := wvChannelCount(b)
		if err != nil {
			return err
		}
		f.channels = c
	case wvIDDSDBlock:
		if dataLen >= 2 {
			b, err := w.bytes(at, 1)
			if err != nil {
				return err
			}
			f.dsdPower = int(b[0])
		}
	case wvIDInt32Info, wvIDFloatInfo:
		if dataLen != 4 {
			return fmt.Errorf("wavpack: an int32 or float information of %d bytes", dataLen)
		}
		b, err := w.bytes(at, 4)
		if err != nil {
			return err
		}
		if id == wvIDInt32Info {
			f.int32SentBits = b[0]
		} else {
			f.floatFlags = b[0]
		}
	case wvIDWVBitstream, wvIDWVCBitstream:
		if dataLen == 0 || dataLen%2 != 0 {
			return fmt.Errorf("wavpack: a bitstream of %d bytes", dataLen)
		}
		if id == wvIDWVBitstream {
			f.wvBits = true
		} else {
			f.correctionBits = true
		}
	case wvIDWVXBitstream:
		if dataLen <= 4 || dataLen%2 != 0 {
			return fmt.Errorf("wavpack: an extended bitstream of %d bytes", dataLen)
		}
		f.wvxBits = true
	case wvIDWVXNewBitstream:
		f.wvxBits = f.wvxBits || dataLen > 4
	}
	return nil
}

// wvSampleRateOf reads an ID_SAMPLE_RATE sub-block's 3 or 4 bytes: 3 bytes of
// rate, and a fourth whose low 7 bits are the rate's top byte.
func wvSampleRateOf(b []byte) uint32 {
	rate := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
	if len(b) == 4 {
		rate |= uint32(b[3]&0x7f) << 24
	}
	return rate
}

// wvChannelCount reads an ID_CHANNEL_INFO sub-block's channel count from its
// 1 to 7 bytes, as the reference decoder's read_channel_info does: 1 to 5
// bytes, the count and then a channel mask; 6 or 7, a 12-bit count less one,
// the streams and a wider mask. A count of 0 and a mask naming more channels
// than the count are an error: the decoder refuses such a file.
func wvChannelCount(b []byte) (int, error) {
	n := len(b)
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
		for i := 1; i < n; i++ {
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
// samples and flags as the .wv's first audio block (the encoder writes both
// from one header, and the reference decoder's match_wvc_header takes a
// correction block only with the stream's own flags) and a total that agrees
// with the .wv's (correctionTotalAgrees), and a block ends the file at the
// stream's last sample: where the .wv's header states its length from its
// start, that length; otherwise where the .wv's own last block ends
// (wv.final, wvStreamFinal), and a .wv whose end that look did not find
// matches nothing. A file cut short, or of another stream, is not.
func correctionMatches(wv wvStream, wvc wvCorrection) bool {
	h := wvc.first
	if !wvc.correctionBits || !wvc.finalFound ||
		h.blockIndex != wv.header.blockIndex || h.blockSamples != wv.header.blockSamples ||
		h.flags != wv.header.flags || !correctionTotalAgrees(wv.header, h) {
		return false
	}
	if total, ok := wv.header.totalSamples(); ok && wv.header.blockIndex == 0 {
		return wvc.final == total
	}
	return wv.finalFound && wvc.final == wv.final
}

// correctionTotalAgrees reports whether a correction file's first audio block
// states the stream's total as the .wv's first audio block does: the same
// total, both unknown, or unknown where the .wv's is known (round 5 of
// backlog B489, CodeRabbit). The last is the pair an encode leaves when it
// learned the stream's length only at its end (a stream piped in): every
// header is written with the unknown total, and the reference encoder then
// rewrites the first block of the .wv with the length, and of the .wvc after
// it (cli/wavpack.c), so a rewrite of the .wvc that fails leaves this pair.
// The reference decoder takes it: it reads the stream's length from the .wv
// alone (WavpackOpenFileInputEx64: the .wv's first audio block's total, or
// seek_final_index over the .wv), and takes a correction block by its index,
// samples and flags (read_wvc_block, match_wvc_header), never its total. It
// would take the reverse too, a total in the .wvc alone, and two totals that
// differ, which no reference encode writes, so this reader refuses both, and
// keeps every other check correctionMatches makes, the correction file's end
// at the stream's last sample among them.
func correctionTotalAgrees(wv, wvc wvHeader) bool {
	wvTotal, wvKnown := wv.totalSamples()
	wvcTotal, wvcKnown := wvc.totalSamples()
	switch {
	case wvKnown && wvcKnown:
		return wvTotal == wvcTotal
	case wvKnown:
		return true
	default:
		return !wvcKnown
	}
}

// statesItsLength reports whether s's header states the stream's length
// from its first sample: a total the encoder wrote, in a first audio block
// at index 0.
func (s wvStream) statesItsLength() bool {
	_, ok := s.header.totalSamples()
	return ok && s.header.blockIndex == 0
}

// wvStreamFinal finds where the stream's audio ends in a .wv of size bytes:
// the block that ends the file's bytes before its tags (an ID3v1 tag in its
// last 128 bytes, and an APEv2 tag before that, header and all), as
// wvFinalIndex finds a correction file's. It reads only for a stream whose
// header does not state its length (statesItsLength), which is all
// correctionMatches asks it for.
func wvStreamFinal(r io.ReaderAt, size int64) (uint64, bool, error) {
	end := size
	if end >= apeID3v1Bytes {
		var id3 [3]byte
		if err := readAtExactly(r, id3[:], end-apeID3v1Bytes); err != nil {
			return 0, false, tagReadErr(err)
		}
		if string(id3[:]) == "TAG" {
			end -= apeID3v1Bytes
		}
	}
	if end >= apeTagFrameBytes {
		var footer [apeTagFrameBytes]byte
		if err := readAtExactly(r, footer[:], end-apeTagFrameBytes); err != nil {
			return 0, false, tagReadErr(err)
		}
		if string(footer[0:8]) == "APETAGEX" {
			tagSize := int64(binary.LittleEndian.Uint32(footer[12:16]))
			if binary.LittleEndian.Uint32(footer[20:24])&apeTagFlagHasHeader != 0 {
				tagSize += apeTagFrameBytes
			}
			if tagSize > end {
				return 0, false, nil
			}
			end -= tagSize
		}
	}
	return wvFinalIndex(r, end)
}
