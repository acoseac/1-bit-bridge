package decode_test

import (
	"bytes"
	"io"
	"testing"

	flac "github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// seekBuf is a bytes.Buffer that also seeks, so flac.Encoder.Close can rewrite
// STREAMINFO.
type seekBuf struct {
	buf []byte
	off int
}

func (s *seekBuf) Write(p []byte) (int, error) {
	for len(s.buf) < s.off+len(p) {
		s.buf = append(s.buf, 0)
	}
	copy(s.buf[s.off:], p)
	s.off += len(p)
	return len(p), nil
}

func (s *seekBuf) Seek(offset int64, whence int) (int64, error) {
	var n int64
	switch whence {
	case io.SeekStart:
		n = offset
	case io.SeekCurrent:
		n = int64(s.off) + offset
	case io.SeekEnd:
		n = int64(len(s.buf)) + offset
	default:
		return 0, io.ErrUnexpectedEOF
	}
	if n < 0 {
		return 0, io.ErrUnexpectedEOF
	}
	s.off = int(n)
	return n, nil
}

func (s *seekBuf) Bytes() []byte { return s.buf }

// encodeFLACPCM writes a stereo FLAC of interleaved int32 samples (sign-
// extended valid bits) using mewkiz's encoder. Ordinary tests use this so they
// need no external flac(1). Committed goldens are produced by the reference
// tools instead.
func encodeFLACPCM(t *testing.T, rate, bits int, interleaved []int32) []byte {
	t.Helper()
	const channels = 2
	if len(interleaved)%channels != 0 {
		t.Fatalf("interleaved length %d not divisible by %d", len(interleaved), channels)
	}
	frames := len(interleaved) / channels
	info := &meta.StreamInfo{
		BlockSizeMin:  16,
		BlockSizeMax:  4096,
		SampleRate:    uint32(rate),
		NChannels:     channels,
		BitsPerSample: uint8(bits),
		NSamples:      uint64(frames),
	}
	sb := &seekBuf{}
	enc, err := flac.NewEncoder(sb, info)
	if err != nil {
		t.Fatal(err)
	}
	enc.EnablePredictionAnalysis(false)
	const block = 4096
	for first := 0; first < frames; first += block {
		n := block
		if first+n > frames {
			n = frames - first
		}
		left := make([]int32, n)
		right := make([]int32, n)
		for i := 0; i < n; i++ {
			left[i] = interleaved[(first+i)*2]
			right[i] = interleaved[(first+i)*2+1]
		}
		f := &frame.Frame{
			Header: frame.Header{
				HasFixedBlockSize: true,
				BlockSize:         uint16(n),
				SampleRate:        uint32(rate),
				Channels:          frame.ChannelsLR,
				BitsPerSample:     uint8(bits),
			},
			Subframes: []*frame.Subframe{
				{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: left, NSamples: n},
				{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: right, NSamples: n},
			},
		}
		if err := enc.WriteFrame(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.Clone(sb.Bytes())
}

func encodeFLACMono(t *testing.T, rate, bits, frames int, value int32) []byte {
	t.Helper()
	info := &meta.StreamInfo{
		BlockSizeMin:  16,
		BlockSizeMax:  4096,
		SampleRate:    uint32(rate),
		NChannels:     1,
		BitsPerSample: uint8(bits),
		NSamples:      uint64(frames),
	}
	sb := &seekBuf{}
	enc, err := flac.NewEncoder(sb, info)
	if err != nil {
		t.Fatal(err)
	}
	enc.EnablePredictionAnalysis(false)
	samples := make([]int32, frames)
	for i := range samples {
		samples[i] = value
	}
	f := &frame.Frame{
		Header: frame.Header{
			HasFixedBlockSize: true,
			BlockSize:         uint16(frames),
			SampleRate:        uint32(rate),
			Channels:          frame.ChannelsMono,
			BitsPerSample:     uint8(bits),
		},
		Subframes: []*frame.Subframe{
			{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: samples, NSamples: frames},
		},
	}
	if err := enc.WriteFrame(f); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.Clone(sb.Bytes())
}

func constantStereo(frames int, value int32) []int32 {
	out := make([]int32, frames*2)
	for i := range out {
		out[i] = value
	}
	return out
}
