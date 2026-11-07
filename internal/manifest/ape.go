package manifest

// Monkey's Audio (.ape), read from its headers (backlog B489). Nothing is
// decoded.
//
// A file is "MAC " and the file version (the version number times 1000, so
// 3.99 is 3990), at the start or after junk an ID3v2 tag left there. From
// 3.98 (3980) on, an APE_DESCRIPTOR follows (52 bytes, its own length and the
// APE_HEADER's among them), and the APE_HEADER (24 bytes) where the
// descriptor's length says: the compression level, the format flags, the
// blocks per frame, the final frame's blocks, the frame count, the bits per
// sample, the channels and the sample rate. Before 3.98 the APE_HEADER_OLD
// (32 bytes from "MAC ") holds the level, the flags, the channels, the rate,
// the WAV header's and the terminating data's lengths, the frame count and
// the final frame's blocks; its bits are the flags' (8, 24, else 16), and
// its blocks per frame follow from the version and the level as MACLib
// derives them (9216; 73728 from 3.90, or from 3.80 at Extra High; 73728 x 4
// from 3.95). The file's length in samples is (frames - 1) x blocks per
// frame + the final frame's blocks.
//
// Versions 3800 to 3990 are the ones read here, the range FFmpeg's reader
// takes; any other is refused (the scanner indexes it by name, backlog B145),
// never read by a guess about a layout this reader does not know. So is a
// header that cannot be read: no frames, a rate of 0 (both before any
// division), no channels, a bit depth or a compression level Monkey's Audio
// does not write, and lengths that do not fit.
//
// And so is a file that does not hold what its header says lies ahead of its
// frames, which the decoders read before the first (round 2 of backlog B489,
// Codex's review: a 76-byte file whose header claimed 4 GiB was indexed with
// a duration): the descriptor, the header, the seek table and the WAV header
// from 3.98 on, and before, the old header with its peak level and seek
// table count where its flags say, the WAV header unless the flags say the
// decoder makes one, the seek table and, before 3.81, a byte a frame. Those
// must end inside the file with frame data after them, and the seek table
// must name every frame, as FFmpeg's reader refuses a file with none, with
// fewer entries than frames, or cut inside it.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The compression levels and format flags the reader takes, as MACLib's
// All.h names them.
const (
	apeLevelFast      = 1000
	apeLevelExtraHigh = 4000
	apeLevelInsane    = 5000

	apeFlag8Bit         = 1
	apeFlagPeakLevel    = 4
	apeFlag24Bit        = 8
	apeFlagSeekElements = 16
	apeFlagCreateWAVH   = 32

	apeMinVersion = 3800
	apeMaxVersion = 3990
	apeNewLayout  = 3980 // the first version with a descriptor
	apeBitTable   = 3810 // the first version without a byte a frame after the seek table

	apeDescriptorBytes = 52
	apeHeaderBytes     = 24
	apeOldHeaderBytes  = 32

	// apeSearchLimit is how far the reader looks for "MAC " past where it
	// starts looking, as MACLib's FindDescriptor does (1 MB).
	apeSearchLimit = 1 << 20
	// apeMaxDescriptorBytes bounds the descriptor's own length, which a
	// later version may grow: 64 KiB is more than any version writes, and
	// the header is read where it ends.
	apeMaxDescriptorBytes = 64 << 10
	// apeMaxChannels is the most channels Monkey's Audio writes (32 since
	// version 10; 2 before).
	apeMaxChannels = 32
)

// errAPENoDescriptor is a file in which "MAC " is not found where MACLib
// looks for it: not a Monkey's Audio file.
var errAPENoDescriptor = errors.New("ape: no \"MAC \" within the search limit")

// apeStream is what a Monkey's Audio file's headers state.
type apeStream struct {
	version     uint16
	compression uint16
	formatFlags uint16
	channels    uint16
	rate        uint32
	bits        uint16
	totalBlocks uint64
}

// duration is the file's length in seconds: its blocks (one sample per
// channel each) over its rate, which readAPEHeader refuses at 0.
func (s apeStream) duration() float64 { return float64(s.totalBlocks) / float64(s.rate) }

// readAPEHeader finds and reads the headers of a Monkey's Audio file of size
// bytes in r. A read that fails comes back as the reader's error; a file that
// ends before its headers do, or holds no header a reader takes, is refused.
func readAPEHeader(r io.ReaderAt, size int64) (apeStream, error) {
	start, err := apeJunkLength(r)
	if err != nil {
		return apeStream{}, err
	}
	at, err := findAPEDescriptor(r, start)
	if err != nil {
		return apeStream{}, err
	}
	var head [8]byte
	if err := readAtExactly(r, head[:], at); err != nil {
		return apeStream{}, err
	}
	s := apeStream{version: binary.LittleEndian.Uint16(head[4:6])}
	if s.version < apeMinVersion || s.version > apeMaxVersion {
		return apeStream{}, fmt.Errorf("ape: file version %d is not one this reader takes (%d to %d)",
			s.version, apeMinVersion, apeMaxVersion)
	}
	var fr apeFrames
	if s.version >= apeNewLayout {
		fr, err = s.readNewHeader(r, at)
	} else {
		fr, err = s.readOldHeader(r, at)
	}
	if err != nil {
		return apeStream{}, err
	}
	if err := s.check(fr); err != nil {
		return apeStream{}, err
	}
	if err := fr.fits(at, size); err != nil {
		return apeStream{}, err
	}
	s.totalBlocks = uint64(fr.totalFrames-1)*uint64(fr.blocksPerFrame) + uint64(fr.finalFrameBlocks)
	return s, nil
}

// apeFrames is what a header states of the frames: the blocks per frame, the
// final frame's blocks, the frame count, the seek table's entries, and the
// bytes from "MAC " to the first frame, which the descriptor, the headers,
// the seek table and the WAV header fill.
type apeFrames struct {
	blocksPerFrame, finalFrameBlocks, totalFrames uint32
	seekEntries                                   uint64
	ahead                                         uint64
}

// fits refuses frames a file of size bytes, its "MAC " at at, does not hold:
// a seek table naming fewer entries than there are frames (none at all
// among them), and what lies ahead of the first frame ending at or past the
// file's end, so no frame data follows it, or cut short.
func (fr apeFrames) fits(at, size int64) error {
	switch {
	case fr.seekEntries < uint64(fr.totalFrames):
		return fmt.Errorf("ape: a seek table of %d entries beside %d frames", fr.seekEntries, fr.totalFrames)
	case uint64(at)+fr.ahead >= uint64(size):
		return fmt.Errorf("ape: %d bytes of headers and seek table, beyond the file's %d", fr.ahead, size-at)
	}
	return nil
}

// readNewHeader reads the descriptor at at and the APE_HEADER its length
// places.
func (s *apeStream) readNewHeader(r io.ReaderAt, at int64) (apeFrames, error) {
	var d [apeDescriptorBytes]byte
	if err := readAtExactly(r, d[:], at); err != nil {
		return apeFrames{}, err
	}
	descriptorBytes := binary.LittleEndian.Uint32(d[8:12])
	headerBytes := binary.LittleEndian.Uint32(d[12:16])
	seekTableBytes := binary.LittleEndian.Uint32(d[16:20])
	wavHeaderBytes := binary.LittleEndian.Uint32(d[20:24])
	if descriptorBytes < apeDescriptorBytes || descriptorBytes > apeMaxDescriptorBytes {
		return apeFrames{}, fmt.Errorf("ape: a descriptor of %d bytes", descriptorBytes)
	}
	if headerBytes < apeHeaderBytes {
		return apeFrames{}, fmt.Errorf("ape: a header of %d bytes", headerBytes)
	}
	var h [apeHeaderBytes]byte
	if err := readAtExactly(r, h[:], at+int64(descriptorBytes)); err != nil {
		return apeFrames{}, err
	}
	s.compression = binary.LittleEndian.Uint16(h[0:2])
	s.formatFlags = binary.LittleEndian.Uint16(h[2:4])
	s.bits = binary.LittleEndian.Uint16(h[16:18])
	s.channels = binary.LittleEndian.Uint16(h[18:20])
	s.rate = binary.LittleEndian.Uint32(h[20:24])
	return apeFrames{
		blocksPerFrame:   binary.LittleEndian.Uint32(h[4:8]),
		finalFrameBlocks: binary.LittleEndian.Uint32(h[8:12]),
		totalFrames:      binary.LittleEndian.Uint32(h[12:16]),
		seekEntries:      uint64(seekTableBytes) / 4,
		ahead:            uint64(descriptorBytes) + uint64(headerBytes) + uint64(seekTableBytes) + uint64(wavHeaderBytes),
	}, nil
}

// readOldHeader reads the APE_HEADER_OLD at at, with the peak level and the
// seek table's count its flags place after it, and derives what the old
// layout does not hold: the bits from the flags, and the blocks per frame
// from the version and the level.
func (s *apeStream) readOldHeader(r io.ReaderAt, at int64) (apeFrames, error) {
	var h [apeOldHeaderBytes]byte
	if err := readAtExactly(r, h[:], at); err != nil {
		return apeFrames{}, err
	}
	s.compression = binary.LittleEndian.Uint16(h[6:8])
	s.formatFlags = binary.LittleEndian.Uint16(h[8:10])
	s.channels = binary.LittleEndian.Uint16(h[10:12])
	s.rate = binary.LittleEndian.Uint32(h[12:16])
	wavHeaderBytes := binary.LittleEndian.Uint32(h[16:20])
	fr := apeFrames{
		totalFrames:      binary.LittleEndian.Uint32(h[24:28]),
		finalFrameBlocks: binary.LittleEndian.Uint32(h[28:32]),
		ahead:            apeOldHeaderBytes,
	}
	if s.formatFlags&apeFlagPeakLevel != 0 {
		fr.ahead += 4
	}
	fr.seekEntries = uint64(fr.totalFrames)
	if s.formatFlags&apeFlagSeekElements != 0 {
		var n [4]byte
		if err := readAtExactly(r, n[:], at+int64(fr.ahead)); err != nil {
			return apeFrames{}, err
		}
		fr.seekEntries = uint64(binary.LittleEndian.Uint32(n[:]))
		fr.ahead += 4
	}
	if s.formatFlags&apeFlagCreateWAVH == 0 {
		fr.ahead += uint64(wavHeaderBytes)
	}
	fr.ahead += fr.seekEntries * 4
	if s.version < apeBitTable {
		fr.ahead += uint64(fr.totalFrames)
	}
	switch {
	case s.formatFlags&apeFlag8Bit != 0:
		s.bits = 8
	case s.formatFlags&apeFlag24Bit != 0:
		s.bits = 24
	default:
		s.bits = 16
	}
	switch {
	case s.version >= 3950:
		fr.blocksPerFrame = 73728 * 4
	case s.version >= 3900 || (s.version >= 3800 && s.compression == apeLevelExtraHigh):
		fr.blocksPerFrame = 73728
	default:
		fr.blocksPerFrame = 9216
	}
	return fr, nil
}

// check refuses a header no reader takes: no frames and a rate of 0, before
// any division by either; no channels, or more than Monkey's Audio writes; a
// bit depth it does not write; a compression level that is not one of its
// five; and frame counts that do not fit together.
func (s apeStream) check(fr apeFrames) error {
	switch {
	case fr.totalFrames == 0:
		return errors.New("ape: no frames")
	case s.rate == 0:
		return errors.New("ape: a sample rate of 0")
	case s.channels == 0 || s.channels > apeMaxChannels:
		return fmt.Errorf("ape: %d channels", s.channels)
	case s.bits != 8 && s.bits != 16 && s.bits != 24 && s.bits != 32:
		return fmt.Errorf("ape: %d bits per sample", s.bits)
	case s.compression < apeLevelFast || s.compression > apeLevelInsane || s.compression%1000 != 0:
		return fmt.Errorf("ape: compression level %d", s.compression)
	case fr.blocksPerFrame == 0:
		return errors.New("ape: no blocks per frame")
	case fr.finalFrameBlocks == 0 || fr.finalFrameBlocks > fr.blocksPerFrame:
		return fmt.Errorf("ape: a final frame of %d blocks beside %d per frame", fr.finalFrameBlocks, fr.blocksPerFrame)
	}
	return nil
}

// apeJunkLength is where MACLib starts looking for "MAC ": past an ID3v2 tag
// at the file's start, and the NUL padding after one with no footer.
func apeJunkLength(r io.ReaderAt) (int64, error) {
	var h [10]byte
	n, err := r.ReadAt(h[:], 0)
	if err != nil && !endOfFile(err) {
		return 0, err
	}
	if n < 10 || string(h[0:3]) != "ID3" {
		return 0, nil
	}
	length := int64(unsyncsafe(h[6:10]))
	footer := h[5]&0x10 != 0
	if footer {
		return length + 20, nil
	}
	start := length + 10
	// The padding after the tag, which MACLib reads past a byte at a time;
	// bounded here by the search limit, a window at a time.
	buf := make([]byte, 4096)
	for limit := start + apeSearchLimit; start < limit; {
		n, err := r.ReadAt(buf, start)
		if err != nil && !endOfFile(err) {
			return 0, err
		}
		for i := 0; i < n; i++ {
			if buf[i] != 0 {
				return start + int64(i), nil
			}
		}
		if n < len(buf) {
			return start + int64(n), nil
		}
		start += int64(n)
	}
	return start, nil
}

// findAPEDescriptor finds "MAC " at or after start, at most apeSearchLimit
// bytes past it, a window at a time.
func findAPEDescriptor(r io.ReaderAt, start int64) (int64, error) {
	buf := make([]byte, 64<<10)
	for pos := start; pos <= start+apeSearchLimit; {
		n, err := r.ReadAt(buf, pos)
		if err != nil && !endOfFile(err) {
			return 0, err
		}
		for i := 0; i+4 <= n; i++ {
			if pos+int64(i) > start+apeSearchLimit {
				return 0, errAPENoDescriptor
			}
			if buf[i] == 'M' && string(buf[i:i+4]) == "MAC " {
				return pos + int64(i), nil
			}
		}
		if n < len(buf) {
			return 0, errAPENoDescriptor
		}
		pos += int64(n - 3)
	}
	return 0, errAPENoDescriptor
}

// readAtExactly reads len(b) bytes at off. A file that ends before them is
// errTruncatedHeader; any other failure is the reader's.
func readAtExactly(r io.ReaderAt, b []byte, off int64) error {
	n, err := r.ReadAt(b, off)
	if n == len(b) {
		return nil
	}
	if err != nil && !endOfFile(err) {
		return err
	}
	return errTruncatedHeader
}

// errTruncatedHeader is a file that ends inside a header this reader needs.
var errTruncatedHeader = errors.New("the file ends inside its header")
