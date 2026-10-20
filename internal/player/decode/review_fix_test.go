package decode_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"runtime"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
)

// Red-first coverage for B470 PR 1a round-2 review findings (Argon + Codex).

func TestReviewExtensibleAlignment(t *testing.T) {
	for _, c := range []struct{ container, valid int }{{32, 24}, {32, 16}, {24, 16}} {
		want := []int32{-1, 1, -123, 123}
		stored := make([]int32, len(want))
		shift := c.container - c.valid
		for i, s := range want {
			stored[i] = s << uint(shift)
		}
		raw := writeWAV(t, 44100, 2, c.container, c.valid, stored, true, nil)
		d, err := decode.Open(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		got := readAll(t, d)
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%d-in-%d: got %v want %v", c.valid, c.container, got, want)
			}
		}
	}
}

func TestReviewAIFFChunkConfinement(t *testing.T) {
	raw := writeAIFF(t, 8000, 2, 16, []int32{1, -1, 2, -2}, false)
	ssnd := bytes.Index(raw, []byte("SSND"))
	for _, which := range []string{"short-ssnd", "offset-outside"} {
		b := bytes.Clone(raw)
		if which == "short-ssnd" {
			binary.BigEndian.PutUint32(b[ssnd+4:], 8)
		} else {
			binary.BigEndian.PutUint32(b[ssnd+8:], 8)
			b = append(b, []byte("JUNK\x00\x00\x00\x00")...)
		}
		d, err := decode.Open(bytes.NewReader(b))
		if err != nil {
			continue // refusal at open is success
		}
		buf := make([]int32, 4)
		n, err := d.ReadFrames(context.Background(), buf)
		if n > 0 {
			t.Errorf("%s returned %v n=%d err=%v from outside declared audio", which, buf[:n], n, err)
		}
	}
}

func TestReviewWAVRIFFBodyBound(t *testing.T) {
	wav := writeWAV(t, 44100, 2, 16, 16, []int32{1, 2}, false, nil)
	binary.LittleEndian.PutUint32(wav[4:], 4)
	if d, err := decode.Open(bytes.NewReader(wav)); err == nil {
		t.Errorf("RIFF body of 4 still accepted: %+v", d.Format())
	}
}

func TestReviewFLACPictureAllocation(t *testing.T) {
	raw := encodeFLACPCM(t, 44100, 16, constantStereo(32, 1))
	// Truncate to STREAMINFO-only metadata header region and append a PICTURE
	// that declares 128 MiB of image data.
	raw = raw[:42]
	raw[4] &= 0x7f // STREAMINFO not last
	picture := make([]byte, 32)
	binary.BigEndian.PutUint32(picture[28:], 128<<20)
	raw = append(raw, 0x86, 0, 0, 32)
	raw = append(raw, picture...)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := decode.Open(bytes.NewReader(raw))
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("%d-byte input allocated %d bytes; err=%v", len(raw), alloc, err)
	if alloc > 16<<20 {
		t.Fatalf("metadata allocation %d exceeds bound", alloc)
	}
}

func TestReviewFLACMismatch(t *testing.T) {
	for _, which := range []string{"bits", "rate", "total-small"} {
		raw := encodeFLACPCM(t, 44100, 24, constantStereo(32, 65536))
		switch which {
		case "bits":
			raw[20] = raw[20] & 0xfe
			raw[21] = (raw[21] & 0x0f) | 0xf0
		case "rate":
			v := binary.BigEndian.Uint64(raw[18:26])
			v = (v & ((1 << 44) - 1)) | (uint64(48000) << 44)
			binary.BigEndian.PutUint64(raw[18:26], v)
		case "total-small":
			raw[25] = 1
		}
		d, err := decode.Open(bytes.NewReader(raw))
		if err != nil {
			continue // open-time refusal is fine
		}
		buf := make([]int32, 64)
		n, err := d.ReadFrames(context.Background(), buf)
		if n > 0 {
			t.Errorf("%s: accepted stream/frame mismatch (n=%d err=%v sample0=%d pcm=%+v)", which, n, err, buf[0], d.Format())
		}
	}
}

func TestReviewFLACBoundarySeeks(t *testing.T) {
	const frames = 9000
	samples := make([]int32, frames*2)
	for i := 0; i < frames; i++ {
		samples[2*i] = int32(i)
		samples[2*i+1] = -int32(i)
	}
	raw := encodeFLACPCM(t, 44100, 16, samples)
	d, err := decode.Open(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, f := range []int64{0, 1, 4095, 4096, 4097, 8191, 8192, 8193, 8999, 9000, 0} {
		if err := d.Seek(context.Background(), f); err != nil {
			t.Errorf("seek %d: %v", f, err)
			continue
		}
		b := make([]int32, 2)
		n, err := d.ReadFrames(context.Background(), b)
		if f == frames {
			if n != 0 || !errors.Is(err, io.EOF) {
				t.Errorf("end: n=%d err=%v", n, err)
			}
			continue
		}
		if n != 2 || b[0] != int32(f) || b[1] != -int32(f) {
			t.Errorf("seek %d: got %v n=%d e=%v", f, b, n, err)
		}
	}
}

func TestReviewFLACUnknownLengthRestart(t *testing.T) {
	samples := make([]int32, 9000*2)
	for i := range samples {
		samples[i] = int32(i / 2)
	}
	raw := encodeFLACPCM(t, 44100, 16, samples)
	// Clear NSamples in STREAMINFO (bytes 10-17 hold the packed fields).
	raw[21] &= 0xf0
	for i := 22; i < 26; i++ {
		raw[i] = 0
	}
	d, err := decode.Open(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	b := make([]int32, 2)
	if _, err := d.ReadFrames(context.Background(), b); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if err := d.Seek(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	n, err := d.ReadFrames(context.Background(), b)
	if n != 2 || b[0] != 0 {
		t.Errorf("unknown total seek 0 returned %v n=%d e=%v", b, n, err)
	}
}

func TestReviewFLACInheritedBits(t *testing.T) {
	raw := encodeFLACPCM(t, 44100, 16, constantStereo(32, 37))
	// Clear sample-size code to 000 (inherit from STREAMINFO); repair CRCs.
	raw[45] &= 0xf1
	raw[48] = crc8FLAC(raw[42:48])
	binary.BigEndian.PutUint16(raw[len(raw)-2:], crc16FLAC(raw[42:len(raw)-2]))
	d, err := decode.Open(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	b := make([]int32, 64)
	n, err := d.ReadFrames(context.Background(), b)
	if !errors.Is(err, io.EOF) || n != 64 || b[0] != 37 {
		t.Errorf("valid inherited sample size: n=%d err=%v samples=%v", n, err, b[:4])
	}
}

func TestReviewTruncation(t *testing.T) {
	for _, kind := range []string{"wav", "aiff", "flac"} {
		var raw []byte
		switch kind {
		case "wav":
			raw = writeWAV(t, 44100, 2, 16, 16, []int32{1, -1, 2, -2, 3, -3, 4, -4}, false, nil)
		case "aiff":
			raw = writeAIFF(t, 44100, 2, 16, []int32{1, -1, 2, -2, 3, -3, 4, -4}, false)
		case "flac":
			// Cut inside the frame payload after STREAMINFO so Open still
			// succeeds (metadata-only) and ReadFrames must refuse.
			raw = encodeFLACPCM(t, 44100, 16, constantStereo(8192, 1))
		}
		cut := 4
		if kind == "flac" {
			cut = (len(raw) - 42) / 2
		}
		b := raw[:len(raw)-cut]
		d, err := decode.Open(bytes.NewReader(b))
		if err != nil {
			t.Errorf("%s: Open on truncated stream: %v", kind, err)
			continue
		}
		buf := make([]int32, 20000)
		sawTrunc := false
		for j := 0; j < 8; j++ {
			n, err := d.ReadFrames(context.Background(), buf)
			if errors.Is(err, io.EOF) {
				t.Errorf("%s missing %d bytes: clean EOF declared=%d", kind, cut, d.Format().Frames)
				break
			}
			if decode.IsRefusal(err) {
				// Short mid-stream reads may return n>0 with the Refusal
				// on the same call (Decoder contract).
				_ = n
				sawTrunc = true
				break
			}
			if err != nil {
				t.Errorf("%s: unexpected err=%v n=%d", kind, err, n)
				break
			}
		}
		if !sawTrunc {
			t.Errorf("%s: expected truncated refusal before clean EOF", kind)
		}
	}
}

func TestReviewWAVPartialDeclaredFrame(t *testing.T) {
	b := writeWAV(t, 44100, 2, 16, 16, []int32{1, 2}, false, nil)
	b = append(b, 3)
	binary.LittleEndian.PutUint32(b[40:], 5)
	_, err := decode.Open(bytes.NewReader(b))
	if err == nil {
		t.Fatal("5-byte stereo s16 data accepted at open")
	}
	if !decode.IsRefusal(err) {
		t.Fatalf("want refusal, got %v", err)
	}
}

func TestReviewRates(t *testing.T) {
	for _, kind := range []string{"wav", "aiff"} {
		var b []byte
		if kind == "wav" {
			b = writeWAV(t, 0, 2, 16, 16, []int32{1, 2}, false, nil)
		} else {
			b = writeAIFF(t, 0, 2, 16, []int32{1, 2}, false)
		}
		if d, err := decode.Open(bytes.NewReader(b)); err == nil {
			t.Errorf("%s accepts zero rate: %+v", kind, d.Format())
		}
	}
}

func TestReviewAIFFNonIntegerRate(t *testing.T) {
	for _, r := range []float64{44100.25, -8000, math.Inf(1)} {
		b := writeAIFF(t, 44100, 2, 16, []int32{1, 2}, false)
		copy(b[28:38], floatToExtended80(r))
		if d, err := decode.Open(bytes.NewReader(b)); err == nil {
			t.Errorf("AIFF rate %v accepted as %d", r, d.Format().RateHz)
		}
	}
}

func TestReviewID3FLAC(t *testing.T) {
	raw := encodeFLACPCM(t, 44100, 16, constantStereo(32, 37))
	b := append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0}, raw...)
	d, err := decode.Open(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("ID3-tagged FLAC: %v", err)
	}
	if len(readAll(t, d)) != 64 {
		t.Fatal("length")
	}
}

func TestReviewCancelDuringSeek(t *testing.T) {
	// Seek near the end of a multi-block stream so a scan that ignores ctx
	// keeps reading frames. Seeking into the first block alone would still
	// pass even if the loop stopped checking cancellation.
	const blocks = 40
	const block = 4096
	raw := encodeFLACPCM(t, 44100, 16, constantStereo(block*blocks, 1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &cancelReader{Reader: bytes.NewReader(raw), cancel: cancel}
	d, err := decode.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	r.armed = true
	start := time.Now()
	target := int64(block*(blocks-1) + 10) // inside the final full block
	err = d.Seek(ctx, target)
	t.Logf("Seek err=%v reads after arm=%d time=%v", err, r.reads, time.Since(start))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
	// One cancelled frame parse is a handful of reads; a full uncancellable
	// scan of ~40 verbatim frames is thousands.
	if r.reads > 200 {
		t.Errorf("seek continues full scan after cancellation (%d reads)", r.reads)
	}
}

func TestReviewCancelDuringPCMRead(t *testing.T) {
	for _, kind := range []string{"wav", "aiff"} {
		var raw []byte
		if kind == "wav" {
			raw = writeWAV(t, 44100, 2, 16, 16, constantStereo(4000, 1), false, nil)
		} else {
			raw = writeAIFF(t, 44100, 2, 16, constantStereo(4000, 1), false)
		}
		ctx, cancel := context.WithCancel(context.Background())
		r := &cancelReader{Reader: bytes.NewReader(raw), cancel: cancel}
		d, err := decode.Open(r)
		if err != nil {
			t.Fatal(err)
		}
		r.armed = true
		_, err = d.ReadFrames(ctx, make([]int32, 8000))
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s canceled during Read: e=%v", kind, err)
		}
	}
}

func TestReviewSniffMP4(t *testing.T) {
	ftyp := []byte("\x00\x00\x00\x10ftypM4A \x00\x00\x00\x00")
	// AAC sample entry fourcc only appears as metadata text "alac", not as stsd.
	b := append(ftyp, []byte("\x00\x00\x00\x20moovudta\x00\x00\x00\x14metaartist-alac")...)
	_, err := decode.Open(bytes.NewReader(b))
	r, ok := decode.AsRefusal(err)
	if !ok {
		t.Fatalf("want refusal, got %v", err)
	}
	if r.Reason == decode.ReasonNotYetSupported {
		t.Fatalf("metadata text alac misclassified as ALAC: %v", err)
	}
	if r.Reason == decode.ReasonLossy {
		t.Fatalf("unknown MP4 guessed as AAC: %v", err)
	}
}

func TestReviewFmtChunkSizeBound(t *testing.T) {
	// fmt chunk claiming 1 MiB body after a valid minimal prefix.
	b := []byte("RIFF\x00\x00\x10\x00WAVEfmt ")
	b = binary.LittleEndian.AppendUint32(b, 1<<20)
	b = append(b, make([]byte, 16)...)
	_, err := decode.Open(bytes.NewReader(b))
	if err == nil || !decode.IsRefusal(err) {
		t.Fatalf("want fmt size refusal, got %v", err)
	}
}

type cancelReader struct {
	*bytes.Reader
	cancel context.CancelFunc
	armed  bool
	reads  int
}

func (r *cancelReader) Read(p []byte) (int, error) {
	if r.armed {
		r.reads++
		if r.reads == 1 {
			r.cancel()
		}
	}
	return r.Reader.Read(p)
}

func crc8FLAC(b []byte) byte {
	var c byte
	for _, v := range b {
		c ^= v
		for j := 0; j < 8; j++ {
			if c&0x80 != 0 {
				c = c<<1 ^ 7
			} else {
				c <<= 1
			}
		}
	}
	return c
}

func crc16FLAC(b []byte) uint16 {
	var c uint16
	for _, v := range b {
		c ^= uint16(v) << 8
		for j := 0; j < 8; j++ {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x8005
			} else {
				c <<= 1
			}
		}
	}
	return c
}
