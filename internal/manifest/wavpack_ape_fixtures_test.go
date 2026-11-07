package manifest

// Hand-built WavPack, Monkey's Audio and APEv2 fixtures (backlog B489). No
// reference encoder runs on the hosts this suite runs on (wavpack, wvunpack
// and mac are not installed there), so each fixture is the header a reference
// encoder writes, laid out field by field from the formats' own definitions:
// WavPack 5's block header and metadata sub-blocks (WavPack5FileFormat.pdf,
// and wavpack.h's WavpackHeader, the flags and the ID_ values), Monkey's
// Audio's APE_DESCRIPTOR and APE_HEADER (3.98 and later) and APE_HEADER_OLD
// (before), and the APEv2 tag's header, footer and items. The audio itself is
// filler: the bridge decodes nothing, and reads only these headers.
//
// Untagged, like scanner_fixture_test.go, so every test file can use them on
// every platform.

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// The WavPack flags and metadata ids the fixtures set, as wavpack.h names
// them. The parser keeps its own copies (wavpack.go); a fixture spelling one
// differently would make a test of the parser test the fixture.
const (
	fxWVBytesStored  = 3
	fxWVMono         = 0x4
	fxWVHybrid       = 0x8
	fxWVJointStereo  = 0x10
	fxWVFloat        = 0x80
	fxWVInt32        = 0x100
	fxWVInitialBlock = 0x800
	fxWVFinalBlock   = 0x1000
	fxWVShiftLSB     = 13
	fxWVSrateLSB     = 23
	fxWVDSD          = 0x80000000

	fxIDDecorrTerms  = 0x2
	fxIDHybridProf   = 0x6
	fxIDFloatInfo    = 0x8
	fxIDInt32Info    = 0x9
	fxIDWVBitstream  = 0xa
	fxIDWVCBitstream = 0xb
	fxIDWVXBitstream = 0xc
	fxIDChannelInfo  = 0xd
	fxIDDSDBlock     = 0xe
	fxIDRIFFHeader   = 0x21
	fxIDMD5Checksum  = 0x26
	fxIDSampleRate   = 0x27
	// fxIDWVXNew is the optional extended bitstream (ID_OPTIONAL_DATA |
	// ID_WVX_BITSTREAM).
	fxIDWVXNew = 0x2c
)

// fxWVRateIndex is the header's sample-rate index of rate (WavPack's table),
// or 15, the non-standard rate a metadata sub-block carries.
func fxWVRateIndex(rate uint32) uint32 {
	for i, r := range []uint32{6000, 8000, 9600, 11025, 12000, 16000, 22050, 24000, 32000, 44100, 48000, 64000, 88200, 96000, 192000} {
		if r == rate {
			return uint32(i)
		}
	}
	return 15
}

// wvSub encodes one metadata sub-block: the id, its size in 16-bit words (a
// byte, or three after ID_LARGE), the data and the pad byte an odd length
// takes (ID_ODD_SIZE).
func wvSub(id byte, data []byte) []byte {
	words := (len(data) + 1) / 2
	if len(data)%2 == 1 {
		id |= 0x40
	}
	var out []byte
	if words > 255 {
		out = append(out, id|0x80, byte(words), byte(words>>8), byte(words>>16))
	} else {
		out = append(out, id, byte(words))
	}
	out = append(out, data...)
	if len(data)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

// wvBlock is one WavPack block: its header's fields and its sub-blocks.
type wvBlock struct {
	version      uint16
	blockIndex   uint64
	totalSamples int64 // -1 is the unknown length a streaming encoder writes
	blockSamples uint32
	flags        uint32
	crc          uint32
	subs         [][]byte
}

// bytes lays the block out: "wvpk", ckSize (the block less 8), the version,
// the upper bytes of the 40-bit index and total, their lower 32 bits, the
// block's samples, the flags and the CRC, then the sub-blocks. The total is
// encoded as wavpack.h's SET_TOTAL_SAMPLES encodes it.
func (b wvBlock) bytes() []byte {
	var body []byte
	for _, s := range b.subs {
		body = append(body, s...)
	}
	h := make([]byte, 32)
	copy(h, "wvpk")
	binary.LittleEndian.PutUint32(h[4:8], uint32(24+len(body)))
	v := b.version
	if v == 0 {
		v = 0x410
	}
	binary.LittleEndian.PutUint16(h[8:10], v)
	h[10] = byte(b.blockIndex >> 32)
	tmp := b.totalSamples
	if tmp >= 0 {
		tmp += tmp / 0xffffffff
	}
	h[11] = byte(tmp >> 32)
	binary.LittleEndian.PutUint32(h[12:16], uint32(tmp))
	binary.LittleEndian.PutUint32(h[16:20], uint32(b.blockIndex))
	binary.LittleEndian.PutUint32(h[20:24], b.blockSamples)
	binary.LittleEndian.PutUint32(h[24:28], b.flags)
	binary.LittleEndian.PutUint32(h[28:32], b.crc)
	return append(h, body...)
}

// wvSpec describes a WavPack file the way an encoder's options do.
type wvSpec struct {
	rate     uint32 // the PCM rate, or the DSD BYTE rate's table base (see dsdPower)
	bytes    int    // bytes per sample stored, 1 to 4
	shift    int    // the low bits a sample does not use (a 20-bit file in 3 bytes: 4)
	channels int
	float    bool
	hybrid   bool
	dsd      bool
	dsdPower int // the DSD block's multiplier power: the byte rate is rate << dsdPower
	total    int64
	samples  uint32 // the first block's samples; total when 0
	// customRate forces the rate into an ID_SAMPLE_RATE sub-block (index
	// 15), as an encoder does for a rate its table lacks.
	customRate bool
	// noChannelInfo leaves out ID_CHANNEL_INFO, which an encoder writes for
	// more than two channels.
	noChannelInfo bool
}

// wvFirstBlock is the first block an encoder writes for spec: the RIFF
// header's sub-block, the rate and channel sub-blocks it needs, then the
// bitstream, wv's or (for a correction file) wvc's.
func wvFirstBlock(spec wvSpec, correction bool) wvBlock {
	flags := uint32(fxWVInitialBlock | fxWVFinalBlock)
	flags |= uint32(spec.bytes-1) & fxWVBytesStored
	flags |= uint32(spec.shift) << fxWVShiftLSB
	if spec.channels == 1 {
		flags |= fxWVMono
	} else {
		flags |= fxWVJointStereo
	}
	if spec.float {
		flags |= fxWVFloat
	}
	if spec.hybrid {
		flags |= fxWVHybrid
	}
	if spec.dsd {
		flags |= fxWVDSD
	}
	idx := fxWVRateIndex(spec.rate)
	if spec.customRate {
		idx = 15
	}
	flags |= idx << fxWVSrateLSB
	samples := spec.samples
	if samples == 0 {
		if spec.total > 0 && spec.total < 1<<17 {
			samples = uint32(spec.total)
		} else {
			samples = 4096
		}
	}
	b := wvBlock{totalSamples: spec.total, blockSamples: samples, flags: flags, crc: 0xC0FFEE}
	if correction {
		b.crc = 0xBADC0DE
		b.subs = append(b.subs, wvSub(fxIDWVCBitstream, bytes.Repeat([]byte{0x5A}, 64)))
		return b
	}
	b.subs = append(b.subs, wvSub(fxIDRIFFHeader, []byte("RIFF\x00\x00\x00\x00WAVEfmt ")))
	if idx == 15 {
		r := spec.rate
		b.subs = append(b.subs, wvSub(fxIDSampleRate, []byte{byte(r), byte(r >> 8), byte(r >> 16)}))
	}
	if spec.channels > 2 && !spec.noChannelInfo {
		mask := uint32(1)<<uint(spec.channels) - 1
		b.subs = append(b.subs, wvSub(fxIDChannelInfo, []byte{byte(spec.channels), byte(mask), byte(mask >> 8), byte(mask >> 16)}))
	}
	b.subs = append(b.subs, wvSub(fxIDDecorrTerms, []byte{0x12, 0x34}))
	if spec.hybrid {
		b.subs = append(b.subs, wvSub(fxIDHybridProf, []byte{0x01, 0x02, 0x03, 0x04}))
	}
	if spec.float {
		b.subs = append(b.subs, wvSub(fxIDFloatInfo, []byte{0, 0, 0x7f, 0x7f}))
	}
	if spec.dsd {
		b.subs = append(b.subs, wvSub(fxIDDSDBlock, append([]byte{byte(spec.dsdPower), 0}, bytes.Repeat([]byte{0x69}, 30)...)))
		return b
	}
	b.subs = append(b.subs, wvSub(fxIDWVBitstream, bytes.Repeat([]byte{0xA5}, 96)))
	return b
}

// wvFile is a WavPack file of spec (wvBlocks).
func wvFile(spec wvSpec) []byte { return wvBlocks(spec, false) }

// wvcFile is the correction file a hybrid encode of spec writes beside it
// (wvBlocks).
func wvcFile(spec wvSpec) []byte { return wvBlocks(spec, true) }

// wvBlocks lays out the blocks an encoder writes for spec, each carrying the
// stream's total: the first, and where the stream is longer than one block,
// the block that ends it at its last sample. The blocks between are left
// out, since nothing here decodes them: a reader of a header looks at the
// first audio block, and at the block that ends the file. A stream whose
// length the encoder could not write gets a second block after the first.
func wvBlocks(spec wvSpec, correction bool) []byte {
	return wvBlocksStating(spec, correction, spec.total)
}

// wvBlocksStating is wvBlocks with every header stating total (-1: the
// unknown length) in place of spec's: the blocks an encoder writes for a
// stream it learned the length of only once the stream was written, the
// headers it did not go back to rewrite.
func wvBlocksStating(spec wvSpec, correction bool, total int64) []byte {
	first := wvFirstBlock(spec, correction)
	first.totalSamples = total
	out := first.bytes()
	n := int64(first.blockSamples)
	switch {
	case spec.total < 0:
		next := first
		next.blockIndex = uint64(n)
		out = append(out, next.bytes()...)
	case spec.total > n:
		last := first
		s := min(spec.total-n, n)
		last.blockIndex = uint64(spec.total - s)
		last.blockSamples = uint32(s)
		out = append(out, last.bytes()...)
	}
	return out
}

// wvMetadataBlock is a block of metadata alone, with no samples, as the
// reference encoder's write_metadata_block writes one after a stream's audio
// (its MD5 sum, a RIFF trailer): a header cleared but for its id, version,
// size and total, so its flags are 0 and its index 0, holding one sub-block
// of payload bytes.
func wvMetadataBlock(total int64, payload int) wvBlock {
	return wvBlock{totalSamples: total, crc: 0xFEED,
		subs: [][]byte{wvSub(fxIDMD5Checksum, bytes.Repeat([]byte{0x3C}, payload))}}
}

// The APE compression levels and the format flags the fixtures set, as
// MACLib's All.h names them.
const (
	fxAPEFast      = 1000
	fxAPENormal    = 2000
	fxAPEHigh      = 3000
	fxAPEExtraHigh = 4000
	fxAPEInsane    = 5000

	fxAPEFlag8Bit       = 1
	fxAPEFlagPeakLevel  = 4
	fxAPEFlag24Bit      = 8
	fxAPEFlagSeekElems  = 16
	fxAPEFlagCreateWAVH = 32
)

// apeSpec describes a Monkey's Audio file's headers. A version from 3980 on
// takes the descriptor and header; below, the old header.
type apeSpec struct {
	version          uint16
	compression      uint16
	formatFlags      uint16
	channels         uint16
	rate             uint32
	bits             uint16 // the new layout's; the old one says it in its flags
	blocksPerFrame   uint32 // the new layout's; the old one derives it
	finalFrameBlocks uint32
	totalFrames      uint32
	// junk is what precedes "MAC ": an ID3v2 tag, say.
	junk []byte
	// descriptorExtra and headerExtra lengthen the two structures past
	// their sizes, as the descriptor's own lengths allow a later version.
	descriptorExtra int
	headerExtra     int
}

// apeFile lays a file of spec out: the headers, then filler for the seek
// table, the WAV header and the frames.
func apeFile(spec apeSpec) []byte {
	out := append([]byte(nil), spec.junk...)
	if spec.version >= 3980 {
		d := make([]byte, 52+spec.descriptorExtra)
		copy(d, "MAC ")
		binary.LittleEndian.PutUint16(d[4:6], spec.version)
		binary.LittleEndian.PutUint32(d[8:12], uint32(52+spec.descriptorExtra))
		binary.LittleEndian.PutUint32(d[12:16], uint32(24+spec.headerExtra))
		binary.LittleEndian.PutUint32(d[16:20], spec.totalFrames*4)
		binary.LittleEndian.PutUint32(d[20:24], 44)
		binary.LittleEndian.PutUint32(d[24:28], 4096)
		h := make([]byte, 24+spec.headerExtra)
		binary.LittleEndian.PutUint16(h[0:2], spec.compression)
		binary.LittleEndian.PutUint16(h[2:4], spec.formatFlags)
		binary.LittleEndian.PutUint32(h[4:8], spec.blocksPerFrame)
		binary.LittleEndian.PutUint32(h[8:12], spec.finalFrameBlocks)
		binary.LittleEndian.PutUint32(h[12:16], spec.totalFrames)
		binary.LittleEndian.PutUint16(h[16:18], spec.bits)
		binary.LittleEndian.PutUint16(h[18:20], spec.channels)
		binary.LittleEndian.PutUint32(h[20:24], spec.rate)
		out = append(out, d...)
		out = append(out, h...)
	} else {
		h := make([]byte, 32)
		copy(h, "MAC ")
		binary.LittleEndian.PutUint16(h[4:6], spec.version)
		binary.LittleEndian.PutUint16(h[6:8], spec.compression)
		binary.LittleEndian.PutUint16(h[8:10], spec.formatFlags)
		binary.LittleEndian.PutUint16(h[10:12], spec.channels)
		binary.LittleEndian.PutUint32(h[12:16], spec.rate)
		binary.LittleEndian.PutUint32(h[16:20], 44)
		binary.LittleEndian.PutUint32(h[20:24], 0)
		binary.LittleEndian.PutUint32(h[24:28], spec.totalFrames)
		binary.LittleEndian.PutUint32(h[28:32], spec.finalFrameBlocks)
		out = append(out, h...)
		if spec.formatFlags&fxAPEFlagPeakLevel != 0 {
			out = append(out, 0, 0, 0, 0)
		}
		if spec.formatFlags&fxAPEFlagSeekElems != 0 {
			var n [4]byte
			binary.LittleEndian.PutUint32(n[:], spec.totalFrames)
			out = append(out, n[:]...)
		}
	}
	return append(out, bytes.Repeat([]byte{0x3C}, 512)...)
}

// apeItem is one APEv2 item: its key, value and flags (bits 1 and 2 the
// item's type: 0 text, 1 binary, 2 an external locator).
type apeItem struct {
	key   string
	value []byte
	flags uint32
}

func apeText(key, value string) apeItem { return apeItem{key: key, value: []byte(value)} }

// apeTag is an APEv2 tag holding items, footer last, with its header in
// front when header is set.
func apeTag(items []apeItem, header bool) []byte {
	var body []byte
	for _, it := range items {
		var h [8]byte
		binary.LittleEndian.PutUint32(h[0:4], uint32(len(it.value)))
		binary.LittleEndian.PutUint32(h[4:8], it.flags)
		body = append(body, h[:]...)
		body = append(body, it.key...)
		body = append(body, 0)
		body = append(body, it.value...)
	}
	flags := uint32(0)
	if header {
		flags |= 1 << 31
	}
	frame := func(isHeader bool) []byte {
		f := make([]byte, 32)
		copy(f, "APETAGEX")
		binary.LittleEndian.PutUint32(f[8:12], 2000)
		binary.LittleEndian.PutUint32(f[12:16], uint32(len(body)+32))
		binary.LittleEndian.PutUint32(f[16:20], uint32(len(items)))
		fl := flags
		if isHeader {
			fl |= 1 << 29
		}
		binary.LittleEndian.PutUint32(f[20:24], fl)
		return f
	}
	var out []byte
	if header {
		out = append(out, frame(true)...)
	}
	out = append(out, body...)
	return append(out, frame(false)...)
}

// id3v1Tag is a 128-byte ID3v1 tag naming a title and an artist.
func id3v1Tag(title, artist string) []byte {
	t := make([]byte, 128)
	copy(t, "TAG")
	copy(t[3:33], title)
	copy(t[33:63], artist)
	t[127] = 255
	return t
}

// stdWavPack is 16-bit stereo at 44.1 kHz, a second long.
func stdWavPack() wvSpec {
	return wvSpec{rate: 44100, bytes: 2, channels: 2, total: 44100}
}

// stdAPE is a 3.99 file of 16-bit stereo at 44.1 kHz, ten frames.
func stdAPE() apeSpec {
	return apeSpec{version: 3990, compression: fxAPENormal, channels: 2, rate: 44100, bits: 16,
		blocksPerFrame: 73728, finalFrameBlocks: 1000, totalFrames: 10}
}

// writeWavPack writes a WavPack file of spec at p, making its folders, an
// APEv2 tag holding items after it when items is not nil.
func writeWavPack(t *testing.T, p string, spec wvSpec, items []apeItem) {
	t.Helper()
	data := wvFile(spec)
	if items != nil {
		data = append(data, apeTag(items, true)...)
	}
	writeFixtureFile(t, p, data)
}

// writeAPE writes a Monkey's Audio file of spec at p, making its folders, an
// APEv2 tag holding items after it when items is not nil.
func writeAPE(t *testing.T, p string, spec apeSpec, items []apeItem) {
	t.Helper()
	data := apeFile(spec)
	if items != nil {
		data = append(data, apeTag(items, true)...)
	}
	writeFixtureFile(t, p, data)
}

// writeFixtureFile writes data at p, making its folders.
func writeFixtureFile(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureBytes(t, p, data)
}
