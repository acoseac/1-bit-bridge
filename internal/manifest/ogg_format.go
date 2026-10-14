package manifest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

// An Ogg page is at most a 27-byte header, a 255-byte segment table and
// 255 segments of 255 bytes. The duration read keeps to one such page at
// the end of the file.
const (
	oggHeaderLen      = 27
	oggMaxSegments    = 255
	oggLastPageWindow = oggHeaderLen + oggMaxSegments + oggMaxSegments*oggMaxSegments
	oggIdentBytes     = 32
	oggKindVorbis     = 1
	oggKindOpus       = 2
	opusClock         = 48000
	maxOggSampleRate  = 768000
)

// oggLossy is the identification header of a Vorbis or Opus stream.
// preSkip is Opus only. rate for Opus is the 48 kHz granule clock.
type oggLossy struct {
	rate     float64
	channels int
	preSkip  int
	opus     bool
}

// extractOggWithContext reads an .ogg or .oga file. The codec starts as
// OGG. An Ogg FLAC stream takes the FLAC facts from its header packets.
// Vorbis and Opus take rate, channels and a duration from the last page,
// and stay OGG.
func extractOggWithContext(absPath string, t *Track, ec *ExtractContext) error {
	f, err := openAudioFile(absPath, ec)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := applyOggLossyFacts(f, t); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return extractViaDhowdenFromReader(f, absPath, t, ec)
}

// applyJoinedFLACFacts reads STREAMINFO from the native-FLAC join
// oggFLACMetadata built, including the channel count that reader does not
// store, then puts the join back at the start for the tag read.
func applyJoinedFLACFacts(join io.ReadSeeker, absPath string, t *Track) {
	if err := extractFLACFormatFromReader(join, absPath, t); err == nil {
		if ch, ok := flacJoinedChannels(join); ok {
			t.Channels = &ch
		}
	}
	_, _ = join.Seek(0, io.SeekStart)
}

// flacJoinedChannels reads the channel field of the STREAMINFO body. The
// join starts at fLaC, so that byte sits at offset 20.
func flacJoinedChannels(join io.ReadSeeker) (int, bool) {
	if _, err := join.Seek(20, io.SeekStart); err != nil {
		return 0, false
	}
	var b [1]byte
	if _, err := io.ReadFull(join, b[:]); err != nil {
		return 0, false
	}
	ch := int((b[0]>>1)&7) + 1
	if ch < 1 || ch > 8 {
		return 0, false
	}
	return ch, true
}

// applyOggLossyFacts stamps Vorbis or Opus facts when the identification
// header parses and a page of that stream ends at EOF. Anything else
// leaves the track as the caller set it.
func applyOggLossyFacts(f io.ReadSeeker, t *Track) error {
	prefix, serial, kind, err := oggOpeningPacket(f)
	if err != nil || kind == 0 {
		return err
	}
	header, ok := oggLossyHeader(kind, prefix)
	if !ok {
		return nil
	}
	granule, ok := oggGranuleAtEOF(f, serial)
	if !ok {
		return nil
	}
	rate := header.rate
	ch := header.channels
	t.SampleRate = &rate
	t.Channels = &ch
	setOggDuration(t, header, granule)
	return nil
}

func oggLossyHeader(kind int, prefix []byte) (oggLossy, bool) {
	switch kind {
	case oggKindVorbis:
		return vorbisIdentFields(prefix)
	case oggKindOpus:
		return opusHeadFields(prefix)
	default:
		return oggLossy{}, false
	}
}

func setOggDuration(t *Track, h oggLossy, granule uint64) {
	samples := float64(granule)
	rate := h.rate
	if h.opus {
		samples -= float64(h.preSkip)
		rate = opusClock
	}
	if samples <= 0 || rate <= 0 {
		return
	}
	d := samples / rate
	if plausibleDuration(d) {
		t.Duration = &d
	}
}

func oggOpeningPacket(f io.ReadSeeker) ([]byte, uint32, int, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, 0, err
	}
	hdr, err := readFullOrNone(f, oggHeaderLen)
	if err != nil || hdr == nil || !oggBOSHeader(hdr) {
		return nil, 0, 0, err
	}
	prefix, err := oggPacketPrefix(f, int(hdr[26]))
	if err != nil || prefix == nil {
		return nil, 0, 0, err
	}
	return prefix, binary.LittleEndian.Uint32(hdr[14:18]), oggPacketKind(prefix), nil
}

func oggBOSHeader(hdr []byte) bool {
	return bytes.Equal(hdr[:4], []byte("OggS")) && hdr[4] == 0 && hdr[5]&oggBOS != 0
}

func oggPacketKind(p []byte) int {
	switch {
	case bytes.HasPrefix(p, []byte("\x01vorbis")):
		return oggKindVorbis
	case bytes.HasPrefix(p, []byte("OpusHead")):
		return oggKindOpus
	default:
		return 0
	}
}

func oggPacketPrefix(f io.Reader, nseg int) ([]byte, error) {
	if nseg < 1 || nseg > oggMaxSegments {
		return nil, nil
	}
	table, err := readFullOrNone(f, nseg)
	if err != nil || table == nil {
		return nil, err
	}
	need := 0
	for _, s := range table {
		need += int(s)
		if s < 255 || need >= oggIdentBytes {
			break
		}
	}
	if need < 1 {
		return nil, nil
	}
	if need > oggIdentBytes {
		need = oggIdentBytes
	}
	return readFullOrNone(f, need)
}

func vorbisIdentFields(p []byte) (oggLossy, bool) {
	if len(p) < 16 || !bytes.HasPrefix(p, []byte("\x01vorbis")) {
		return oggLossy{}, false
	}
	if binary.LittleEndian.Uint32(p[7:11]) != 0 {
		return oggLossy{}, false
	}
	ch := int(p[11])
	rate := binary.LittleEndian.Uint32(p[12:16])
	if ch < 1 || rate == 0 || rate > maxOggSampleRate {
		return oggLossy{}, false
	}
	return oggLossy{rate: float64(rate), channels: ch}, true
}

func opusHeadFields(p []byte) (oggLossy, bool) {
	if len(p) < 19 || !bytes.HasPrefix(p, []byte("OpusHead")) || p[8] != 1 {
		return oggLossy{}, false
	}
	ch := int(p[9])
	if ch < 1 {
		return oggLossy{}, false
	}
	skip := int(binary.LittleEndian.Uint16(p[10:12]))
	return oggLossy{rate: opusClock, channels: ch, preSkip: skip, opus: true}, true
}

// oggGranuleAtEOF reads one maximum page from the end of f and returns
// the granule of the page of serial that ends on the last byte.
func oggGranuleAtEOF(f io.ReadSeeker, serial uint32) (uint64, bool) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil || size < oggHeaderLen {
		return 0, false
	}
	window := int64(oggLastPageWindow)
	if window > size {
		window = size
	}
	if _, err = f.Seek(size-window, io.SeekStart); err != nil {
		return 0, false
	}
	buf, err := readFullOrNone(f, int(window))
	if err != nil || buf == nil {
		return 0, false
	}
	return granuleEndingAt(buf, serial)
}

func granuleEndingAt(buf []byte, serial uint32) (uint64, bool) {
	for i := len(buf) - 4; i >= 0; i-- {
		if buf[i] != 'O' || buf[i+1] != 'g' || buf[i+2] != 'g' || buf[i+3] != 'S' {
			continue
		}
		if g, ok := granuleIfPageEndsHere(buf[i:], serial); ok {
			return g, true
		}
	}
	return 0, false
}

func granuleIfPageEndsHere(page []byte, serial uint32) (uint64, bool) {
	if len(page) < oggHeaderLen || page[4] != 0 {
		return 0, false
	}
	if binary.LittleEndian.Uint32(page[14:18]) != serial {
		return 0, false
	}
	granule := binary.LittleEndian.Uint64(page[6:14])
	if int64(granule) < 0 {
		return 0, false
	}
	nseg := int(page[26])
	if nseg > oggMaxSegments || len(page) < oggHeaderLen+nseg {
		return 0, false
	}
	body := 0
	for _, s := range page[27 : 27+nseg] {
		body += int(s)
	}
	if oggHeaderLen+nseg+body != len(page) {
		return 0, false
	}
	if binary.LittleEndian.Uint32(page[22:26]) != oggPageCRC(page) {
		return 0, false
	}
	return granule, true
}

// oggPageCRC is the Ogg page checksum: CRC-32, polynomial 0x04c11db7,
// no reflection and no final XOR. Bytes 22 through 25 are taken as zero.
func oggPageCRC(page []byte) uint32 {
	var crc uint32
	for i, v := range page {
		if i >= 22 && i < 26 {
			v = 0
		}
		crc ^= uint32(v) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func readFullOrNone(f io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	_, err := io.ReadFull(f, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return buf, nil
}
