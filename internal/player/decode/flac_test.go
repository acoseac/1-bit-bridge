package decode_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

func TestFLACConstantStereo16(t *testing.T) {
	const (
		rate   = 44100
		bits   = 16
		frames = 5000
		value  = 1234
	)
	raw := encodeFLACPCM(t, rate, bits, constantStereo(frames, value))
	d, err := openPCM(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	pcm := d.Format()
	if pcm.RateHz != rate || pcm.Channels != 2 || pcm.ValidBits != bits || pcm.Container != format.ContainerFLAC {
		t.Fatalf("format %+v", pcm)
	}
	if pcm.Frames != frames {
		t.Fatalf("frames %d want %d", pcm.Frames, frames)
	}
	got := readAll(t, d)
	if len(got) != frames*2 {
		t.Fatalf("got %d samples, want %d", len(got), frames*2)
	}
	for i, s := range got {
		if s != value {
			t.Fatalf("sample %d: got %d want %d", i, s, value)
		}
	}
}

func TestFLACSeekExact(t *testing.T) {
	const frames = 9000
	// Distinct per-frame values so a wrong seek is obvious.
	interleaved := make([]int32, frames*2)
	for i := 0; i < frames; i++ {
		interleaved[i*2] = int32(i)
		interleaved[i*2+1] = -int32(i)
	}
	raw := encodeFLACPCM(t, 48000, 24, interleaved)
	d, err := openPCM(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	target := int64(5000) // mid-block (block=4096)
	if err := d.Seek(ctx, target); err != nil {
		t.Fatal(err)
	}
	buf := make([]int32, 4)
	n, err := d.ReadFrames(ctx, buf)
	if n != 4 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if buf[0] != 5000 || buf[1] != -5000 || buf[2] != 5001 || buf[3] != -5001 {
		t.Fatalf("got %v", buf)
	}
	if err := d.Seek(ctx, 0); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, d)
	if len(got) != frames*2 {
		t.Fatalf("after seek-0 got %d samples, want %d", len(got), frames*2)
	}
}

func TestFLACMonoRefused(t *testing.T) {
	// Build a mono FLAC via mewkiz directly.
	raw := encodeFLACMono(t, 44100, 16, 100, 0)
	_, err := decode.Open(bytes.NewReader(raw))
	r, ok := decode.AsRefusal(err)
	if !ok || r.Reason != decode.ReasonUnsupportedChannels {
		t.Fatalf("got %v, want channel refusal", err)
	}
}

func TestFLACCancelRead(t *testing.T) {
	raw := encodeFLACPCM(t, 44100, 16, constantStereo(20000, 1))
	d, err := openPCM(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	buf := make([]int32, 1024)
	_, err = d.ReadFrames(ctx, buf)
	if err == nil {
		t.Fatal("expected canceled context error")
	}
}

func TestFLACEOF(t *testing.T) {
	raw := encodeFLACPCM(t, 44100, 16, constantStereo(32, 7))
	d, err := openPCM(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_ = readAll(t, d)
	buf := make([]int32, 4)
	n, err := d.ReadFrames(context.Background(), buf)
	if n != 0 || err != io.EOF {
		t.Fatalf("n=%d err=%v, want 0, EOF", n, err)
	}
}
