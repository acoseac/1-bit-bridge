package manifest

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	meta "github.com/mewkiz/flac/meta"
)

// Ogg format facts (backlog B118). An .ogg or .oga row used to carry the
// codec alone: no sample rate, bit depth, channel count or duration, and an
// Ogg FLAC stream was classed lossy because that codec was OGG.

func streamInfoBody(rate, channels, bits int, samples uint64, sum [16]byte) []byte {
	var buf bytes.Buffer
	writeStreamInfo(&buf, &meta.StreamInfo{
		BlockSizeMin:  16,
		BlockSizeMax:  16,
		SampleRate:    uint32(rate),
		NChannels:     uint8(channels),
		BitsPerSample: uint8(bits),
		NSamples:      samples,
		MD5sum:        sum,
	})
	return flacBlock(false, 0, buf.Len(), buf.Bytes())
}

func oggFLACMapped(info []byte, declared uint16) []byte {
	p := []byte("\x7fFLAC\x01\x00")
	p = binary.BigEndian.AppendUint16(p, declared)
	return append(append(p, "fLaC"...), info...)
}

func stampGranule(pages [][]byte, granule uint64) []byte {
	page := pages[len(pages)-1]
	binary.LittleEndian.PutUint64(page[6:14], granule)
	binary.LittleEndian.PutUint32(page[22:26], 0)
	binary.LittleEndian.PutUint32(page[22:26], oggCRC(page))
	return joinPages(pages...)
}

func vorbisFile(channels byte, rate uint32, granule uint64) []byte {
	ident := make([]byte, 30)
	copy(ident, "\x01vorbis")
	ident[11] = channels
	binary.LittleEndian.PutUint32(ident[12:16], rate)
	ident[29] = 1
	pages := oggLogicalStream(0x0b15, 255, ident, vorbisCommentPacket("TITLE=Vorbis"), []byte{1, 2, 3})
	return stampGranule(pages, granule)
}

func opusFile(channels byte, preSkip uint16, granule uint64) []byte {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8] = 1
	head[9] = channels
	binary.LittleEndian.PutUint16(head[10:12], preSkip)
	binary.LittleEndian.PutUint32(head[12:16], 48000)
	pages := oggLogicalStream(0x09a5, 255, head, opusCommentPacket("TITLE=Opus"), []byte{1, 2, 3})
	return stampGranule(pages, granule)
}

func oggFLACWithFacts(rate, channels, bits int, samples uint64, sum [16]byte) []byte {
	info := streamInfoBody(rate, channels, bits, samples, sum)
	packets := [][]byte{oggFLACMapped(info, 0), commentBlock(false, "TITLE=Ogg FLAC"), oggFLACAudioPacket()}
	return joinPages(oggLogicalStream(0x0f1ac, 255, packets...)...)
}

func floatIs(p *float64, want float64) bool {
	if p == nil {
		return false
	}
	d := *p - want
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}

func wantFacts(t *testing.T, tr Track, codec string, lossy bool, rate float64, bits, channels int, duration float64, md5 string) {
	t.Helper()
	if tr.Codec != codec || IsLossyCodec(tr.Codec) != lossy {
		t.Errorf("codec = %q lossy=%v, want %q lossy=%v", tr.Codec, IsLossyCodec(tr.Codec), codec, lossy)
	}
	if !floatIs(tr.SampleRate, rate) {
		t.Errorf("sample rate = %v, want %v", tr.SampleRate, rate)
	}
	if !intIs(tr.BitsPerSample, bits) {
		t.Errorf("bit depth = %v, want %d", tr.BitsPerSample, bits)
	}
	if !intIs(tr.Channels, channels) {
		t.Errorf("channels = %v, want %d", tr.Channels, channels)
	}
	if duration == 0 {
		if tr.Duration != nil {
			t.Errorf("duration = %v, want none", *tr.Duration)
		}
	} else if !floatIs(tr.Duration, duration) {
		t.Errorf("duration = %v, want %v", tr.Duration, duration)
	}
	if tr.audioMD5 != md5 {
		t.Errorf("audio md5 = %q, want %q", tr.audioMD5, md5)
	}
}

func wantCodecOnly(t *testing.T, tr Track) {
	t.Helper()
	if tr.Codec != "OGG" || tr.SampleRate != nil || tr.BitsPerSample != nil || tr.Channels != nil || tr.Duration != nil {
		t.Errorf("got codec %q rate %v bits %v channels %v duration %v, want the codec alone",
			tr.Codec, tr.SampleRate, tr.BitsPerSample, tr.Channels, tr.Duration)
	}
}

// TestAnOggFLACStreamCarriesAFlacFilesFacts is red while the .oga branch
// stamps OGG and nothing else.
func TestAnOggFLACStreamCarriesAFlacFilesFacts(t *testing.T) {
	var sum [16]byte
	for i := range sum {
		sum[i] = byte(i + 1)
	}
	tr := extractBytesAs(t, "facts.oga", oggFLACWithFacts(96000, 2, 24, 96000, sum))
	wantFacts(t, tr, "FLAC", false, 96000, 24, 2, 1, "0102030405060708090a0b0c0d0e0f10")

	ffmpeg := extractOggFixture(t, "ffmpeg_pages.oga", "ffmpeg_pages.oga")
	wantFacts(t, ffmpeg, "FLAC", false, 8000, 16, 1, 0, "")

	lib := extractOggFixture(t, "libflac_picture.oga", "libflac_picture.oga")
	wantFacts(t, lib, "FLAC", false, 8000, 16, 1, 0.2, "da69a4bafde920299fb862b04fc1e3fd")
}

// TestVorbisAndOpusCarryRateChannelsAndDuration is red while those rows
// carry the codec alone.
func TestVorbisAndOpusCarryRateChannelsAndDuration(t *testing.T) {
	vorbis := extractBytesAs(t, "tone.ogg", vorbisFile(2, 44100, 44100))
	if vorbis.Codec != "OGG" || IsLossyCodec(vorbis.Codec) != true {
		t.Errorf("vorbis codec = %q lossy=%v, want OGG and lossy", vorbis.Codec, IsLossyCodec(vorbis.Codec))
	}
	if vorbis.BitsPerSample != nil {
		t.Errorf("vorbis bit depth = %v, want none", *vorbis.BitsPerSample)
	}
	if !floatIs(vorbis.SampleRate, 44100) || !intIs(vorbis.Channels, 2) || !floatIs(vorbis.Duration, 1) {
		t.Errorf("vorbis rate/channels/duration = %v / %v / %v, want 44100 / 2 / 1",
			vorbis.SampleRate, vorbis.Channels, vorbis.Duration)
	}

	opus := extractBytesAs(t, "tone.opus.ogg", opusFile(1, 312, 48312))
	if opus.Codec != "OGG" || !IsLossyCodec(opus.Codec) {
		t.Errorf("opus codec = %q lossy=%v, want OGG and lossy", opus.Codec, IsLossyCodec(opus.Codec))
	}
	if opus.BitsPerSample != nil {
		t.Errorf("opus bit depth = %v, want none", *opus.BitsPerSample)
	}
	if !floatIs(opus.SampleRate, 48000) || !intIs(opus.Channels, 1) || !floatIs(opus.Duration, 1) {
		t.Errorf("opus rate/channels/duration = %v / %v / %v, want 48000 / 1 / 1",
			opus.SampleRate, opus.Channels, opus.Duration)
	}
}

// TestACutOrGarbageOggKeepsTheCodecOnly is the malformed control: a file
// that does not end on a page, and one that is not Ogg, keep the codec and
// invent no duration.
func TestACutOrGarbageOggKeepsTheCodecOnly(t *testing.T) {
	whole := vorbisFile(2, 44100, 88200)
	cut := whole[:len(whole)-10]
	wantCodecOnly(t, extractBytesAs(t, "cut.ogg", cut))
	wantCodecOnly(t, extractBytesAs(t, "junk.ogg", []byte("not ogg at all, just bytes")))
	wantCodecOnly(t, extractBytesAs(t, "neg.ogg", vorbisFile(2, 44100, 1<<63)))
	// A granule past a week is not a duration. The rate still belongs to
	// a page that ends the file.
	huge := extractBytesAs(t, "huge.ogg", vorbisFile(2, 44100, 44100*8*24*3600))
	if !floatIs(huge.SampleRate, 44100) || huge.Duration != nil {
		t.Errorf("rate / duration = %v / %v, want 44100 and no duration for a granule past a week", huge.SampleRate, huge.Duration)
	}
	zero := extractBytesAs(t, "zero.ogg", vorbisFile(2, 44100, 0))
	if !floatIs(zero.SampleRate, 44100) || !intIs(zero.Channels, 2) || zero.Duration != nil {
		t.Errorf("zero granule rate/channels/duration = %v / %v / %v, want 44100 / 2 / none",
			zero.SampleRate, zero.Channels, zero.Duration)
	}
}

// TestOggFormatLeavesFLACMP3AndM4AAlone is the positive control: a native
// FLAC, an MP3 and an M4A extract the same fields they extracted before
// an Ogg row gained format facts.
func TestOggFormatLeavesFLACMP3AndM4AAlone(t *testing.T) {
	dir := t.TempDir()
	flacPath := filepath.Join(dir, "a.flac")
	writeMinimalFLAC(t, flacPath, 44100, 16, map[string]string{"TITLE": "Native"})
	mp3Path := filepath.Join(dir, "a.mp3")
	writeMinimalMP3(t, mp3Path, map[string]string{"title": "Plain"})
	raw, err := os.ReadFile(filepath.Join("testdata", "m4a", "itunes_alac.m4a"))
	if err != nil {
		t.Fatal(err)
	}
	m4aPath := filepath.Join(dir, "a.m4a")
	if err := os.WriteFile(m4aPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	flac := extractPath(t, flacPath)
	if flac.Codec != "FLAC" || IsLossyCodec(flac.Codec) || !floatIs(flac.SampleRate, 44100) || !intIs(flac.BitsPerSample, 16) || flac.Channels != nil || !floatIs(flac.Duration, 5) || flac.audioMD5 != "" || flac.IsDSD == nil || *flac.IsDSD {
		t.Errorf("flac = codec %s lossy %v rate %v bits %v ch %v dur %v dsd %v md5 %q",
			flac.Codec, IsLossyCodec(flac.Codec), flac.SampleRate, flac.BitsPerSample, flac.Channels, flac.Duration, flac.IsDSD, flac.audioMD5)
	}
	mp3 := extractPath(t, mp3Path)
	if mp3.Codec != "MP3" || !floatIs(mp3.SampleRate, 44100) || mp3.BitsPerSample != nil || mp3.Channels != nil || mp3.Duration != nil || mp3.IsDSD != nil || mp3.audioMD5 != "" {
		t.Errorf("mp3 = codec %s rate %v bits %v ch %v dur %v dsd %v md5 %q",
			mp3.Codec, mp3.SampleRate, mp3.BitsPerSample, mp3.Channels, mp3.Duration, mp3.IsDSD, mp3.audioMD5)
	}
	m4a := extractPath(t, m4aPath)
	if m4a.Codec != "ALAC" || !floatIs(m4a.SampleRate, 44100) || !intIs(m4a.BitsPerSample, 16) || m4a.Channels != nil || m4a.Duration != nil || m4a.IsDSD != nil || m4a.audioMD5 != "" {
		t.Errorf("m4a = codec %s rate %v bits %v ch %v dur %v dsd %v md5 %q",
			m4a.Codec, m4a.SampleRate, m4a.BitsPerSample, m4a.Channels, m4a.Duration, m4a.IsDSD, m4a.audioMD5)
	}
}

func extractPath(t *testing.T, path string) Track {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	tr := Track{Path: filepath.Base(path), Size: info.Size(), ModTime: info.ModTime()}
	if err := ExtractWithContext(path, &tr, &ExtractContext{}); err != nil {
		t.Fatal(err)
	}
	return tr
}

// TestVorbisDurationIsReadFromTheTail pins the granule read to the first
// page and one maximum page at EOF. The bytes between them are the
// comment packet, and a read of them fails the test.
func TestVorbisDurationIsReadFromTheTail(t *testing.T) {
	ident := make([]byte, 30)
	copy(ident, "\x01vorbis")
	ident[11] = 2
	binary.LittleEndian.PutUint32(ident[12:16], 44100)
	ident[29] = 1
	comment := make([]byte, 200000)
	pages := oggLogicalStream(0x0b15, 255, ident, comment, []byte{1, 2, 3})
	data := stampGranule(pages, 44100)
	if len(data) <= oggLastPageWindow+4096 {
		t.Fatalf("fixture is %d bytes, want a middle past the tail window", len(data))
	}
	span := &spanRead{data: data, head: 4096, tail: oggLastPageWindow}
	var tr Track
	if err := applyOggLossyFacts(span, &tr); err != nil {
		t.Fatal(err)
	}
	if span.hit {
		t.Fatal("the duration read touched the middle of the file")
	}
	if !floatIs(tr.SampleRate, 44100) || !intIs(tr.Channels, 2) || !floatIs(tr.Duration, 1) {
		t.Errorf("rate/channels/duration = %v / %v / %v, want 44100 / 2 / 1", tr.SampleRate, tr.Channels, tr.Duration)
	}
}

// spanRead is a ReadSeeker that records a read overlapping the middle.
type spanRead struct {
	data []byte
	off  int64
	head int
	tail int
	hit  bool
}

func (s *spanRead) Read(p []byte) (int, error) {
	if s.off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[s.off:])
	s.note(s.off, s.off+int64(n))
	s.off += int64(n)
	return n, nil
}

func (s *spanRead) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += s.off
	case io.SeekEnd:
		off += int64(len(s.data))
	default:
		return 0, errors.New("bad whence")
	}
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	s.off = off
	return off, nil
}

func (s *spanRead) note(from, to int64) {
	tailAt := int64(len(s.data) - s.tail)
	if to > int64(s.head) && from < tailAt {
		s.hit = true
	}
}

// TestScanner_V25_AnOggRowJoinsTheDelta_OtherRowsOnlyStamp is the diff
// guard. Ogg rows whose facts were missing join the delta. A FLAC, an
// MP3 and an M4A only stamp.
func TestScanner_V25_AnOggRowJoinsTheDelta_OtherRowsOnlyStamp(t *testing.T) {
	root := t.TempDir()
	var sum [16]byte
	for i := range sum {
		sum[i] = byte(i + 1)
	}
	files := map[string][]byte{
		"facts.oga":     oggFLACWithFacts(96000, 2, 24, 96000, sum),
		"tone.ogg":      vorbisFile(2, 44100, 44100),
		"tone.opus.ogg": opusFile(1, 312, 48312),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeMinimalFLAC(t, filepath.Join(root, "native.flac"), 44100, 16, map[string]string{"TITLE": "Native"})
	writeMinimalMP3(t, filepath.Join(root, "plain.mp3"), map[string]string{"title": "Plain"})
	raw, err := os.ReadFile(filepath.Join("testdata", "m4a", "itunes_alac.m4a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plain.m4a"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	store, sc := newScanFixture(t, root)
	ctx := context.Background()
	scanOnce(t, sc, "initial")

	ogg := []string{"facts.oga", "tone.ogg", "tone.opus.ogg"}
	for _, rel := range ogg {
		got, err := store.GetTrack(ctx, rel)
		if err != nil || got == nil {
			t.Fatalf("GetTrack(%s): err=%v nil=%v", rel, err, got == nil)
		}
		got.Codec = "OGG"
		got.SampleRate = nil
		got.BitsPerSample = nil
		got.Channels = nil
		got.Duration = nil
		got.IsDSD = nil
		body, err := marshalForStorage(got)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec("UPDATE tracks SET tags_json = ?, audio_md5 = '' WHERE path = ?", string(body), rel); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec("UPDATE tracks SET extractor_version = ?", ExtractorVersion-1); err != nil {
		t.Fatal(err)
	}
	rows := append(append([]string{}, ogg...), "native.flac", "plain.mp3", "plain.m4a")
	before := map[string]int64{}
	for _, rel := range rows {
		before[rel] = trackIndexedAt(t, store, rel)
	}
	scanOnce(t, sc, "v25")
	for _, rel := range ogg {
		if trackIndexedAt(t, store, rel) <= before[rel] {
			t.Errorf("%s indexed_at stayed %d, want it to join the delta", rel, before[rel])
		}
	}
	for _, rel := range []string{"native.flac", "plain.mp3", "plain.m4a"} {
		if trackIndexedAt(t, store, rel) != before[rel] {
			t.Errorf("%s indexed_at moved from %d to %d, want a stamp", rel, before[rel], trackIndexedAt(t, store, rel))
		}
	}
}
