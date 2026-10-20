package decode_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
)

// DSF field offsets (DSF 1.01): the DSD chunk is 28 bytes, fmt 52, then the
// data chunk header (12) and the data.
const (
	dsfDSDSize    = 4
	dsfMetaPtr    = 20
	dsfFmtSize    = 32
	dsfFmtVersion = 40
	dsfFmtID      = 44
	dsfChanType   = 48
	dsfChanNum    = 52
	dsfRate       = 56
	dsfBits       = 60
	dsfSamples    = 64
	dsfBlockSize  = 72
	dsfDataSize   = 84
)

func putLE64(b []byte, off int, v uint64) []byte {
	out := append([]byte{}, b...)
	binary.LittleEndian.PutUint64(out[off:], v)
	return out
}

func putLE32(b []byte, off int, v uint32) []byte {
	out := append([]byte{}, b...)
	binary.LittleEndian.PutUint32(out[off:], v)
	return out
}

// dffField returns the offset of the first chunk id after from.
func dffField(t testing.TB, raw []byte, id string, from int) int {
	t.Helper()
	i := bytes.Index(raw[from:], []byte(id))
	if i < 0 {
		t.Fatalf("no %q chunk", id)
	}
	return from + i
}

func putBE64(b []byte, off int, v uint64) []byte {
	out := append([]byte{}, b...)
	binary.BigEndian.PutUint64(out[off:], v)
	return out
}

// Every file-supplied size or count that cannot be honoured is a typed
// refusal, never a panic, an allocation of the declared size, or audio read
// from outside the declared region.
func TestDSDAbsurdSizesRefused(t *testing.T) {
	g := loadDSDGoldens(t)
	_, dsf := g.fixture(t, "lsb_dsd64_partial_block.dsf")
	_, dff := g.fixture(t, "dsd64.dff")
	const huge = ^uint64(0)
	prop := dffField(t, dff, "PROP", 12)
	chnl := dffField(t, dff, "CHNL", prop)
	fs := dffField(t, dff, "FS  ", prop)
	cmpr := dffField(t, dff, "CMPR", prop)
	comt := dffField(t, dff, "COMT", 12)
	sound := dffSoundDataStart(t, dff) - 12

	cases := []struct {
		name   string
		raw    []byte
		reason decode.Reason
	}{
		{"dsf DSD chunk size huge", putLE64(dsf, dsfDSDSize, huge), decode.ReasonUnsupportedLayout},
		{"dsf fmt size 2^63", putLE64(dsf, dsfFmtSize, 1<<63), decode.ReasonUnsupportedLayout},
		{"dsf fmt size huge", putLE64(dsf, dsfFmtSize, huge), decode.ReasonUnsupportedLayout},
		{"dsf data size huge", putLE64(dsf, dsfDataSize, huge), decode.ReasonUnsupportedLayout},
		{"dsf data size below its header", putLE64(dsf, dsfDataSize, 11), decode.ReasonUnsupportedLayout},
		{"dsf data shorter than the sample count", putLE64(dsf, dsfDataSize, 12+8192), decode.ReasonTruncated},
		{"dsf sample count huge", putLE64(dsf, dsfSamples, huge), decode.ReasonTruncated},
		{"dsf sample count 2^63", putLE64(dsf, dsfSamples, 1<<63), decode.ReasonTruncated},
		{"dsf channel count 2^31", putLE32(dsf, dsfChanNum, 1<<31), decode.ReasonUnsupportedChannels},
		{"dsf channel count 0", putLE32(dsf, dsfChanNum, 0), decode.ReasonUnsupportedChannels},
		{"dsf stereo count with a 5.1 type", putLE32(dsf, dsfChanType, 7), decode.ReasonUnsupportedLayout},
		{"dsf block size 0", putLE32(dsf, dsfBlockSize, 0), decode.ReasonUnsupportedLayout},
		{"dsf block size huge", putLE32(dsf, dsfBlockSize, ^uint32(0)), decode.ReasonUnsupportedLayout},
		{"dsf 48k-family rate", putLE32(dsf, dsfRate, 3_072_000), decode.ReasonUnsupportedLayout},
		{"dsf rate 0", putLE32(dsf, dsfRate, 0), decode.ReasonUnsupportedLayout},
		{"dsf DSD1024", putLE32(dsf, dsfRate, 16*2_822_400), decode.ReasonUnsupportedLayout},
		{"dsf 4 bits per sample", putLE32(dsf, dsfBits, 4), decode.ReasonUnsupportedBits},
		{"dsf format version 2", putLE32(dsf, dsfFmtVersion, 2), decode.ReasonUnsupportedLayout},
		{"dsf format id 1", putLE32(dsf, dsfFmtID, 1), decode.ReasonUnsupportedComp},
		{"dsf header only", dsf[:20], decode.ReasonTruncated},
		{"dsf no data chunk", dsf[:80], decode.ReasonTruncated},

		{"dff FRM8 size huge", putBE64(dff, 4, huge), decode.ReasonUnsupportedLayout},
		{"dff FRM8 size 0", putBE64(dff, 4, 0), decode.ReasonTruncated},
		{"dff COMT size huge", putBE64(dff, comt+4, huge), decode.ReasonUnsupportedLayout},
		{"dff PROP size 2^63", putBE64(dff, prop+4, 1<<63), decode.ReasonUnsupportedLayout},
		{"dff CHNL count 0xFFFF", putBE16(dff, chnl+12, 0xFFFF), decode.ReasonUnsupportedLayout},
		{"dff CHNL size huge", putBE64(dff, chnl+4, huge), decode.ReasonUnsupportedLayout},
		{"dff FS 0", putBE32(dff, fs+12, 0), decode.ReasonUnsupportedLayout},
		{"dff FS 44100", putBE32(dff, fs+12, 44100), decode.ReasonUnsupportedLayout},
		{"dff CMPR unknown", putTag(dff, cmpr+12, "ABCD"), decode.ReasonUnsupportedComp},
		{"dff sound size huge", putBE64(dff, sound+4, huge), decode.ReasonUnsupportedLayout},
		{"dff sound size odd", putBE64(dff, sound+4, 2*13288-1), decode.ReasonTruncated},
		{"dff no PROP before sound", putTag(dff, prop, "XXXX"), decode.ReasonUnsupportedLayout},
		{"dff not DSD form", putTag(dff, 12, "AIFF"), decode.ReasonUnsupportedLayout},
		{"dff header only", dff[:14], decode.ReasonTruncated},
		{"dff chunk bomb", dffChunkBomb(), decode.ReasonUnsupportedLayout},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			s, err := decode.Open(bytes.NewReader(c.raw))
			if err == nil {
				s.Close()
				t.Fatalf("opened; want refusal %s", c.reason)
			}
			r, ok := decode.AsRefusal(err)
			if !ok || r.Reason != c.reason {
				t.Fatalf("got %v, want refusal %s", err, c.reason)
			}
		})
	}
}

// The DSF metadata pointer is never followed: a pointer into the audio or
// past the file changes nothing about the decoded bytes.
func TestDSFMetadataPointerIgnored(t *testing.T) {
	g := loadDSDGoldens(t)
	f, dsf := g.fixture(t, "lsb_dsd64_partial_block.dsf")
	for _, ptr := range []uint64{^uint64(0), 92, 1} {
		d, err := openDSD(bytes.NewReader(putLE64(dsf, dsfMetaPtr, ptr)))
		if err != nil {
			t.Fatalf("pointer %d: %v", ptr, err)
		}
		requireSameDSD(t, readAllDSD(t, d, 4096), g.want(f))
		d.Close()
	}
}

func putBE16(b []byte, off int, v uint16) []byte {
	out := append([]byte{}, b...)
	binary.BigEndian.PutUint16(out[off:], v)
	return out
}

func putBE32(b []byte, off int, v uint32) []byte {
	out := append([]byte{}, b...)
	binary.BigEndian.PutUint32(out[off:], v)
	return out
}

func putTag(b []byte, off int, tag string) []byte {
	out := append([]byte{}, b...)
	copy(out[off:], tag)
	return out
}

// dffChunkBomb is a FRM8 of 100,000 empty unknown chunks: the walk stops at
// its chunk cap instead of seeking through all of them.
func dffChunkBomb() []byte {
	var body bytes.Buffer
	body.WriteString("DSD ")
	for i := 0; i < 100000; i++ {
		body.WriteString("FOO ")
		body.Write(make([]byte, 8))
	}
	head := make([]byte, 12)
	copy(head, "FRM8")
	binary.BigEndian.PutUint64(head[4:], uint64(body.Len()))
	return append(head, body.Bytes()...)
}

// A cancel during a read, or an I/O error after it, leaves the decoder where
// it was: the next read continues with the next byte, and nothing is skipped
// or repeated (PR 1a round 6, here for DSD).
func TestDSDCancelThenRetryNoGap(t *testing.T) {
	g := loadDSDGoldens(t)
	for _, name := range []string{"lsb_dsd64_partial_block.dsf", "dsd64.dff"} {
		t.Run(name, func(t *testing.T) {
			f, raw := g.fixture(t, name)
			ctx, cancel := context.WithCancel(context.Background())
			r := &cancelReader{Reader: bytes.NewReader(raw), cancel: cancel}
			d, err := openDSD(r)
			if err != nil {
				t.Fatal(err)
			}
			first := dsdBufs(2, 3000)
			n, err := d.ReadDSD(context.Background(), first)
			if err != nil || n != 3000 {
				t.Fatalf("first read: n=%d err=%v", n, err)
			}
			r.armed = true
			dst := dsdBufs(2, 6000)
			n1, err1 := d.ReadDSD(ctx, dst)
			if !errors.Is(err1, context.Canceled) {
				t.Fatalf("read under cancel: n=%d err=%v, want Canceled", n1, err1)
			}
			r.armed = false
			got := [][]byte{append(first[0][:n:n], dst[0][:n1]...), append(first[1][:n:n], dst[1][:n1]...)}
			rest := readAllDSD(t, d, 4096)
			got[0] = append(got[0], rest[0]...)
			got[1] = append(got[1], rest[1]...)
			requireSameDSD(t, got, g.want(f))
		})
	}
}

func TestDSDIOErrorThenRetryNoGap(t *testing.T) {
	g := loadDSDGoldens(t)
	for _, name := range []string{"msb_dsd128_partial_block.dsf", "dsd256_right_first.dff"} {
		t.Run(name, func(t *testing.T) {
			f, raw := g.fixture(t, name)
			r := &ioErrorReader{Reader: bytes.NewReader(raw), err: errShortRead}
			d, err := openDSD(r)
			if err != nil {
				t.Fatal(err)
			}
			first := dsdBufs(2, 3000)
			n, err := d.ReadDSD(context.Background(), first)
			if err != nil || n != 3000 {
				t.Fatalf("first read: n=%d err=%v", n, err)
			}
			r.armed, r.reads, r.failAfter = true, 0, 1
			dst := dsdBufs(2, 6000)
			n1, err1 := d.ReadDSD(context.Background(), dst)
			if !errors.Is(err1, errShortRead) {
				t.Fatalf("read with an I/O error: n=%d err=%v", n1, err1)
			}
			r.armed = false
			got := [][]byte{append(first[0][:n:n], dst[0][:n1]...), append(first[1][:n:n], dst[1][:n1]...)}
			rest := readAllDSD(t, d, 4096)
			got[0] = append(got[0], rest[0]...)
			got[1] = append(got[1], rest[1]...)
			requireSameDSD(t, got, g.want(f))
		})
	}
}

// ReadDSD refuses a dst that is not one equal-length slice per channel
// rather than writing a partial frame.
func TestDSDReadBufferShape(t *testing.T) {
	g := loadDSDGoldens(t)
	_, raw := g.fixture(t, "dsd64.dff")
	d, err := openDSD(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, dst := range [][][]byte{
		{make([]byte, 4)},
		{make([]byte, 4), make([]byte, 5)},
		{make([]byte, 4), make([]byte, 4), make([]byte, 4)},
	} {
		if n, err := d.ReadDSD(context.Background(), dst); err == nil || n != 0 {
			t.Fatalf("dst shape %d channels: n=%d err=%v, want an error", len(dst), n, err)
		}
	}
	if err := d.Seek(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.ReadDSD(ctx, dsdBufs(2, 4)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
	if err := d.Seek(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled seek: %v", err)
	}
}
