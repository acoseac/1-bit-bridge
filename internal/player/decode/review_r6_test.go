package decode_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// Round-6: after a cancel or I/O error mid-ReadFrames, the reader must not
// stay ahead of posFrame or the next read skips audio and can decode a
// trailing LIST/ID3 chunk as samples.

func TestReviewR6CancelThenRetryNoGap(t *testing.T) {
	for _, kind := range []string{"wav", "aiff"} {
		t.Run(kind, func(t *testing.T) {
			testPCMCancelThenRetry(t, kind)
		})
	}
}

func TestReviewR6IOErrorThenRetryNoGap(t *testing.T) {
	for _, kind := range []string{"wav", "aiff"} {
		t.Run(kind, func(t *testing.T) {
			testPCMIOErrorThenRetry(t, kind)
		})
	}
}

func testPCMCancelThenRetry(t *testing.T, kind string) {
	t.Helper()
	want, raw := pcmWithTrailingJunk(t, kind, 8000)
	trailMarker := int32(0x7F7F) // s16LE of 0x7F,0x7F — must never appear

	ctx1, cancel := context.WithCancel(context.Background())
	r := &cancelReader{Reader: bytes.NewReader(raw), cancel: cancel}
	d, err := openPCM(r)
	if err != nil {
		t.Fatal(err)
	}
	r.armed = true
	buf := make([]int32, 4096) // one ReadFull chunk under the 64 KiB cap
	n1, err1 := d.ReadFrames(ctx1, buf)
	if !errors.Is(err1, context.Canceled) {
		t.Fatalf("%s: first ReadFrames err=%v want Canceled (n=%d)", kind, err1, n1)
	}

	got := append([]int32{}, buf[:n1]...)
	ctx2 := context.Background()
	for {
		n, err := d.ReadFrames(ctx2, buf)
		got = append(got, buf[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s: retry ReadFrames: n=%d err=%v", kind, n, err)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %d samples want %d (gap or overrun into trailing chunk)", kind, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: sample %d: got %d want %d", kind, i, got[i], want[i])
		}
		if got[i] == trailMarker {
			t.Fatalf("%s: sample %d is trailing-chunk marker", kind, i)
		}
	}
}

func testPCMIOErrorThenRetry(t *testing.T, kind string) {
	t.Helper()
	want, raw := pcmWithTrailingJunk(t, kind, 8000)
	inj := errShortRead
	r := &ioErrorReader{Reader: bytes.NewReader(raw), failAfter: 1, err: inj}
	d, err := openPCM(r)
	if err != nil {
		t.Fatal(err)
	}
	// Open consumed reads; arm failure for the first ReadFrames ReadFull.
	r.armed = true
	r.reads = 0
	r.failAfter = 1

	buf := make([]int32, 4096)
	n1, err1 := d.ReadFrames(context.Background(), buf)
	if !errors.Is(err1, inj) {
		t.Fatalf("%s: first ReadFrames err=%v want %v (n=%d)", kind, err1, inj, n1)
	}

	r.armed = false // subsequent reads succeed
	got := append([]int32{}, buf[:n1]...)
	for {
		n, err := d.ReadFrames(context.Background(), buf)
		got = append(got, buf[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s: retry: n=%d err=%v", kind, n, err)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %d samples want %d", kind, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: sample %d: got %d want %d", kind, i, got[i], want[i])
		}
	}
}

var errShortRead = errors.New("injected short read")

// pcmWithTrailingJunk returns stereo s16 frames 0..frames-1 (L=i, R=-i) and a
// container whose audio region is followed by a LIST/ID3-like chunk of 0x7F.
func pcmWithTrailingJunk(t *testing.T, kind string, frames int) (want []int32, raw []byte) {
	t.Helper()
	want = make([]int32, frames*2)
	for i := 0; i < frames; i++ {
		want[2*i] = int32(i)
		want[2*i+1] = -int32(i)
	}
	junk := bytes.Repeat([]byte{0x7F}, 256)
	switch kind {
	case "wav":
		raw = writeWAV(t, 44100, 2, 16, 16, want, false, nil)
		raw = append(raw, chunkLE("LIST", junk)...)
		binary.LittleEndian.PutUint32(raw[4:], uint32(len(raw)-8))
	case "aiff":
		raw = writeAIFF(t, 44100, 2, 16, want, false)
		raw = append(raw, chunkBE("ID3 ", junk)...)
		binary.BigEndian.PutUint32(raw[4:], uint32(len(raw)-8))
	default:
		t.Fatalf("kind %s", kind)
	}
	return want, raw
}

// ioErrorReader fails once after failAfter successful Read calls while armed,
// returning a short read then the injected error so ReadFull leaves the
// underlying offset ahead of a decoder that does not seek back.
type ioErrorReader struct {
	*bytes.Reader
	armed     bool
	failAfter int
	reads     int
	err       error
	short     bool
}

func (r *ioErrorReader) Read(p []byte) (int, error) {
	if !r.armed {
		return r.Reader.Read(p)
	}
	r.reads++
	if r.reads < r.failAfter {
		return r.Reader.Read(p)
	}
	if r.reads == r.failAfter && !r.short {
		r.short = true
		half := len(p) / 2
		if half < 4 {
			half = len(p)
		}
		if half > len(p) {
			half = len(p)
		}
		n, _ := r.Reader.Read(p[:half])
		return n, nil
	}
	if r.short {
		return 0, r.err
	}
	return r.Reader.Read(p)
}
