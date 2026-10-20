package decode_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
)

// Round-3 Argon findings (.review-argon-r2.md): red-first probes.

func TestReviewR3SeekTableIgnoresPointsAfterTarget(t *testing.T) {
	raw := encodeFLACPCM(t, 44100, 16, func() []int32 {
		const frames = 9000
		s := make([]int32, frames*2)
		for i := 0; i < frames; i++ {
			s[2*i], s[2*i+1] = int32(i), -int32(i)
		}
		return s
	}())
	// Single SEEKTABLE point after the seek target: old seekPointBefore
	// returned it anyway and Seek(100) failed.
	raw = injectFLACSeekTable(t, raw, []seekPoint{{sample: 5000, offset: 0}})
	d, err := openPCM(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Seek(context.Background(), 100); err != nil {
		t.Fatalf("seek 100 with point-at-5000: %v", err)
	}
	b := make([]int32, 2)
	n, err := d.ReadFrames(context.Background(), b)
	if n != 2 || b[0] != 100 || b[1] != -100 {
		t.Fatalf("got %v n=%d err=%v", b, n, err)
	}
}

func TestReviewR3SeekTableIgnoresOverflowOffsets(t *testing.T) {
	raw := encodeFLACPCM(t, 44100, 16, constantStereo(128, 7))
	raw = injectFLACSeekTable(t, raw, []seekPoint{
		{sample: 0, offset: 1 << 63}, // would wrap int64(dataStart)+int64(offset)
		{sample: 64, offset: math.MaxUint64},
	})
	d, err := openPCM(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Seek(context.Background(), 10); err != nil {
		t.Fatalf("seek with overflow seek points: %v", err)
	}
	b := make([]int32, 2)
	n, err := d.ReadFrames(context.Background(), b)
	if n != 2 || b[0] != 7 {
		t.Fatalf("got %v n=%d err=%v", b, n, err)
	}
}

func TestReviewR3SeekTableUnsortedUsesBestAtOrBefore(t *testing.T) {
	raw := encodeFLACPCM(t, 44100, 16, func() []int32 {
		const frames = 9000
		s := make([]int32, frames*2)
		for i := 0; i < frames; i++ {
			s[2*i], s[2*i+1] = int32(i), -int32(i)
		}
		return s
	}())
	// Unsorted: a late sample first, then sample 0. Must still pick sample 0
	// when seeking early — not blindly break on the first larger SampleNum.
	raw = injectFLACSeekTable(t, raw, []seekPoint{
		{sample: 8000, offset: 0},
		{sample: 0, offset: 0},
	})
	d, err := openPCM(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Seek(context.Background(), 50); err != nil {
		t.Fatalf("seek 50: %v", err)
	}
	b := make([]int32, 2)
	n, err := d.ReadFrames(context.Background(), b)
	if n != 2 || b[0] != 50 {
		t.Fatalf("got %v n=%d err=%v", b, n, err)
	}
}

func TestReviewR3ID3FooterOffset(t *testing.T) {
	flac := encodeFLACPCM(t, 44100, 16, constantStereo(32, 9))
	// ID3v2.4, footer flag 0x10, synchsafe size 0 → 10-byte header + 10-byte footer.
	tag := []byte{'I', 'D', '3', 4, 0, 0x10, 0, 0, 0, 0}
	footer := []byte{'3', 'D', 'I', 4, 0, 0x10, 0, 0, 0, 0}
	b := append(append(tag, footer...), flac...)
	d, err := openPCM(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("ID3v2.4 footer before FLAC: %v", err)
	}
	got := readAll(t, d)
	if len(got) != 64 || got[0] != 9 {
		t.Fatalf("samples %v", got[:min(4, len(got))])
	}
}

func TestReviewR3ID3PrefixedFullSniff(t *testing.T) {
	tag := []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0} // size 0, no footer
	for _, c := range []struct {
		name    string
		payload []byte
		reason  decode.Reason
	}{
		// Since PR 1b these open the DSF and DFF demuxers, which refuse an
		// 8-byte header as truncated.
		{"dsf", []byte("DSD \x00\x00\x00\x00"), decode.ReasonTruncated},
		{"dff", []byte("FRM8\x00\x00\x00\x00"), decode.ReasonTruncated},
		{"rf64", []byte("RF64\x00\x00\x00\x00WAVE"), decode.ReasonUnsupportedLayout},
		{"alac", id3ALACPayload(t), decode.ReasonNotYetSupported},
	} {
		b := append(append([]byte{}, tag...), c.payload...)
		_, err := decode.Open(bytes.NewReader(b))
		r, ok := decode.AsRefusal(err)
		if !ok {
			t.Errorf("%s: want refusal, got %v", c.name, err)
			continue
		}
		if r.Reason != c.reason {
			t.Errorf("%s: got reason %q want %q (%v)", c.name, r.Reason, c.reason, err)
		}
	}
}

func TestReviewR3MP4MetaFullBoxAndDepth(t *testing.T) {
	// moov/meta (FullBox) / … / stsd with alac. Without skipping meta's
	// version/flags, the walk misreads children and misses alac.
	moov := mp4Box("moov",
		mp4FullBox("meta", 0,
			mp4Box("ilst", nil), // decoy
			mp4Box("trak",
				mp4Box("mdia",
					mp4Box("minf",
						mp4Box("stbl",
							mp4Box("stsd", stsdALAC()),
						),
					),
				),
			),
		),
	)
	b := append(mp4Box("ftyp", []byte("M4A \x00\x00\x00\x00")), moov...)
	_, err := decode.Open(bytes.NewReader(b))
	r, ok := decode.AsRefusal(err)
	if !ok || r.Reason != decode.ReasonNotYetSupported {
		t.Fatalf("meta FullBox alac: want not_yet_supported, got %v", err)
	}

	// Deep nesting of unknown 8-byte boxes must not overflow the stack.
	deep := make([]byte, 0, 4096)
	for i := 0; i < 64; i++ {
		deep = append(mp4Box("trak", deep), deep...) // grow nesting via trak
		if len(deep) > 2000 {
			break
		}
	}
	b = append(mp4Box("ftyp", []byte("isom\x00\x00\x00\x00")), mp4Box("moov", deep)...)
	_, err = decode.Open(bytes.NewReader(b))
	if err == nil {
		t.Fatal("expected refusal on deep empty moov")
	}
}

func TestReviewR3AIFFUnnormalisedRate(t *testing.T) {
	b := writeAIFF(t, 44100, 2, 16, []int32{1, 2}, false)
	// Non-zero exponent with explicit integer bit clear (unnormalised).
	rate := make([]byte, 10)
	binary.BigEndian.PutUint16(rate[0:2], 0x4000) // exp=0x4000, sign=0
	// mantissa hi without 0x80000000
	binary.BigEndian.PutUint32(rate[2:6], 0x40000000)
	copy(b[28:38], rate)
	if d, err := openPCM(bytes.NewReader(b)); err == nil {
		t.Fatalf("unnormalised 80-bit rate accepted as %d", d.Format().RateHz)
	}
}

func TestReviewR3ShortReadReturnsSamplesAndError(t *testing.T) {
	// Contract: a short mid-stream read may return n>0 together with a
	// truncated Refusal on the same call (documented on Decoder).
	raw := writeWAV(t, 44100, 2, 16, 16, constantStereo(100, 3), false, nil)
	// Keep header + first 40 sample frames (160 bytes of PCM), then drop rest
	// but leave declared data size claiming 100 frames.
	dataOff := bytes.Index(raw, []byte("data"))
	if dataOff < 0 {
		t.Fatal("no data chunk")
	}
	keep := dataOff + 8 + 40*4
	b := raw[:keep]
	d, err := openPCM(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]int32, 200)
	n, err := d.ReadFrames(context.Background(), buf)
	if n == 0 {
		t.Fatalf("expected some samples before truncation, err=%v", err)
	}
	if !decode.IsRefusal(err) {
		t.Fatalf("want truncated refusal with n=%d, got err=%v", n, err)
	}
	if buf[0] != 3 {
		t.Fatalf("decoded sample %d want 3", buf[0])
	}
}

type seekPoint struct {
	sample uint64
	offset uint64
}

func injectFLACSeekTable(t *testing.T, flac []byte, points []seekPoint) []byte {
	t.Helper()
	if len(flac) < 42 || string(flac[:4]) != "fLaC" {
		t.Fatalf("not a FLAC fixture")
	}
	// STREAMINFO must be first; clear is-last and insert SEEKTABLE after it.
	out := make([]byte, 0, len(flac)+4+18*len(points))
	out = append(out, flac[:4]...)
	hdr := flac[4]
	out = append(out, hdr&0x7f, flac[5], flac[6], flac[7])
	out = append(out, flac[8:42]...)
	body := make([]byte, 18*len(points))
	for i, p := range points {
		off := i * 18
		binary.BigEndian.PutUint64(body[off:], p.sample)
		binary.BigEndian.PutUint64(body[off+8:], p.offset)
		binary.BigEndian.PutUint16(body[off+16:], 4096)
	}
	out = append(out, 0x80|0x03) // last + SEEKTABLE
	out = append(out, byte(len(body)>>16), byte(len(body)>>8), byte(len(body)))
	out = append(out, body...)
	out = append(out, flac[42:]...)
	return out
}

func id3ALACPayload(t *testing.T) []byte {
	t.Helper()
	moov := mp4Box("moov",
		mp4Box("trak",
			mp4Box("mdia",
				mp4Box("minf",
					mp4Box("stbl",
						mp4Box("stsd", stsdALAC()),
					),
				),
			),
		),
	)
	return append(mp4Box("ftyp", []byte("M4A \x00\x00\x00\x00")), moov...)
}

func stsdALAC() []byte {
	// version/flags + entry_count=1 + one alac sample entry (16 bytes min).
	entry := make([]byte, 16)
	binary.BigEndian.PutUint32(entry[0:4], 16)
	copy(entry[4:8], "alac")
	body := make([]byte, 8+len(entry))
	binary.BigEndian.PutUint32(body[4:8], 1)
	copy(body[8:], entry)
	return body
}

func mp4Box(typ string, payload []byte) []byte {
	if len(typ) != 4 {
		panic(typ)
	}
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(8+len(payload)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func mp4FullBox(typ string, flags uint32, children ...[]byte) []byte {
	var payload []byte
	vf := make([]byte, 4)
	binary.BigEndian.PutUint32(vf, flags) // version in top byte when needed
	payload = append(payload, vf...)
	for _, c := range children {
		payload = append(payload, c...)
	}
	return mp4Box(typ, payload)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
