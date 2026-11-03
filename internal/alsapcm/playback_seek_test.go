package alsapcm_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/bits"
	"path/filepath"
	"slices"
	"testing"
	"time"

	flac "github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// The seek within a track (B674), through the backend alone and through the
// engine: a session started at a frame (engine.StartRequest.From) gives the
// DAC the content from that frame and nothing before it, bit for bit, and
// counts its position from there.

// aiff is an integer AIFF of stereo samples of depth valid bits at rate.
func aiff(rate, depth int, samples []int32) []byte {
	width := depth / 8
	var data bytes.Buffer
	for _, s := range samples {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(s)<<(32-depth))
		data.Write(b[:width])
	}
	var w bytes.Buffer
	w.WriteString("FORM")
	_ = binary.Write(&w, binary.BigEndian, uint32(4+8+18+8+8+data.Len()))
	w.WriteString("AIFFCOMM")
	_ = binary.Write(&w, binary.BigEndian, uint32(18))
	_ = binary.Write(&w, binary.BigEndian, uint16(2))
	_ = binary.Write(&w, binary.BigEndian, uint32(len(samples)/2))
	_ = binary.Write(&w, binary.BigEndian, uint16(depth))
	// The rate as an 80-bit extended float: the exponent, biased, and the
	// mantissa with its integer bit set.
	exp := 63 - bits.LeadingZeros64(uint64(rate))
	_ = binary.Write(&w, binary.BigEndian, uint16(16383+exp))
	_ = binary.Write(&w, binary.BigEndian, uint64(rate)<<(63-exp))
	w.WriteString("SSND")
	_ = binary.Write(&w, binary.BigEndian, uint32(8+data.Len()))
	_ = binary.Write(&w, binary.BigEndian, [2]uint32{})
	w.Write(data.Bytes())
	return w.Bytes()
}

// seekableBuffer is a buffer that also seeks, so mewkiz's encoder can write
// STREAMINFO again as it closes.
type seekableBuffer struct {
	buf []byte
	off int
}

func (s *seekableBuffer) Write(p []byte) (int, error) {
	if end := s.off + len(p); end > len(s.buf) {
		s.buf = append(s.buf, make([]byte, end-len(s.buf))...)
	}
	copy(s.buf[s.off:], p)
	s.off += len(p)
	return len(p), nil
}

func (s *seekableBuffer) Seek(offset int64, whence int) (int64, error) {
	n := offset
	switch whence {
	case io.SeekCurrent:
		n += int64(s.off)
	case io.SeekEnd:
		n += int64(len(s.buf))
	}
	if n < 0 {
		return 0, io.ErrUnexpectedEOF
	}
	s.off = int(n)
	return n, nil
}

// flacOf is a FLAC of stereo samples of depth valid bits at rate, verbatim
// subframes in blocks of 4,096 and no SEEKTABLE, by mewkiz's encoder, so a
// seek in it is the decoder's forward scan.
func flacOf(t *testing.T, rate, depth int, samples []int32) []byte {
	t.Helper()
	frames := len(samples) / 2
	b := &seekableBuffer{}
	enc, err := flac.NewEncoder(b, &meta.StreamInfo{BlockSizeMin: 16, BlockSizeMax: 4096, SampleRate: uint32(rate), NChannels: 2,
		BitsPerSample: uint8(depth), NSamples: uint64(frames)})
	if err != nil {
		t.Fatal(err)
	}
	enc.EnablePredictionAnalysis(false)
	for first := 0; first < frames; first += 4096 {
		n := min(4096, frames-first)
		left, right := make([]int32, n), make([]int32, n)
		for i := range n {
			left[i], right[i] = samples[2*(first+i)], samples[2*(first+i)+1]
		}
		if err := enc.WriteFrame(&frame.Frame{
			Header: frame.Header{HasFixedBlockSize: true, BlockSize: uint16(n), SampleRate: uint32(rate), Channels: frame.ChannelsLR, BitsPerSample: uint8(depth)},
			Subframes: []*frame.Subframe{
				{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: left, NSamples: n},
				{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: right, NSamples: n},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.Clone(b.buf)
}

// startFrom starts session s with ref as its first occurrence (id 1, as
// start's), from content frame from.
func (d *direct) startFrom(s engine.SessionID, ref string, from int64) error {
	return d.be.Start(bg, engine.StartRequest{Session: s, First: occurrence(engine.OccurrenceID(s)*100+1, 1, ref), From: from,
		Options: engine.SessionOptions{PauseRelease: time.Second, DSD: "off"}, Events: &recEvents{r: d.rec, next: nopEvents{}}})
}

// playFrom plays g through the backend alone, from frame from to its end on
// a DAC that takes f, and returns what the DAC heard and the last position
// the session reported.
func playFrom(t *testing.T, g golden, f format.ALSAFormat, from int64) ([]int32, engine.Position) {
	t.Helper()
	d := newDirect(t, nil)
	d.k.Formats(f)
	d.k.Free()
	d.files.add(g.ref, g.data)
	if err := d.startFrom(1, g.ref, from); err != nil {
		t.Fatalf("a start from frame %d: %v", from, err)
	}
	// Nothing follows it, as the engine says once its first frames are in
	// the sink: the session then plays out and finishes.
	if err := d.be.Prepare(bg, 1, 101, nil); err != nil {
		t.Fatalf("the end named after the occurrence: %v", err)
	}
	waitOn(t, "the session to play out", d.rec.ch, func() bool { return count(d.rec.events(), "finished", 0) == 1 })
	d.release(1)
	var last engine.Position
	for _, e := range d.rec.events() {
		if e.kind == "position" {
			last = e.pos
		}
	}
	return unpack(t, d.k.Heard(), f, g.bits), last
}

// TestPlaybackStartsAtItsFromBitExactly pins the backend's half of a seek on
// every PR 1 golden that plays, FLAC, WAV, AIFF, AIFC and ALAC, an edited one
// among them: a session started from a frame gives the DAC the content from
// that frame and nothing before it, every sample as the reference tools
// decoded it (the whole decode, pinned to their hash first), and counts the
// occurrence's position from that frame, the last one the content's length.
func TestPlaybackStartsAtItsFromBitExactly(t *testing.T) {
	for _, g := range goldens(t) {
		if g.refusal != "" {
			continue
		}
		t.Run(filepath.Base(g.file), func(t *testing.T) {
			f := map[int]format.ALSAFormat{16: format.S16LE, 24: format.S243LE, 32: format.S32LE}[g.bits]
			whole, _ := playFrom(t, g, f, 0)
			if int64(len(whole)) != 2*g.frames || sumOf(whole) != g.sha {
				t.Fatalf("from the start the DAC heard %d samples hashing %s; want %d, the reference tools' %s", len(whole), sumOf(whole), 2*g.frames, g.sha)
			}
			for _, from := range []int64{1, g.frames / 2, g.frames - 1} {
				got, last := playFrom(t, g, f, from)
				if want := whole[2*from:]; !slices.Equal(got, want) {
					t.Errorf("from frame %d the DAC heard %d samples, not the %d of the content from there", from, len(got), len(want))
				}
				if last.Frames != g.frames {
					t.Errorf("from frame %d the last position was %d frames, want the content's length, %d", from, last.Frames, g.frames)
				}
			}
		})
	}
}

// seekContainers make a track of the containers the engine seeks in by the
// second, longer than the goldens: WAV, AIFF and FLAC.
var seekContainers = []struct {
	name string
	of   func(t *testing.T, rate int, samples []int32) []byte
}{
	{"wav", func(_ *testing.T, rate int, s []int32) []byte { return wav(rate, 16, s) }},
	{"aiff", func(_ *testing.T, rate int, s []int32) []byte { return aiff(rate, 16, s) }},
	{"flac", func(t *testing.T, rate int, s []int32) []byte { return flacOf(t, rate, 16, s) }},
}

// TestPlaybackSeeksWithinATrackThroughTheEngine pins a seek through the
// engine's Run, the backend and the sink: a track of 4 s at 44.1 kHz playing,
// 100 ms of it heard and the DAC holding the rest, sought to 2 s. The session
// that played it is released, its device dropped with the frames the DAC had
// not played, and the session started for the seek continues the occurrence
// heard from frame 88,200: its open gives the DAC the track from there and
// nothing else, the decoder positioned before a frame, so the DAC hears the
// first 100 ms and then everything from 2 s, each frame once; the position is
// the target plus what the DAC has played since, and the track and its count
// are the ones it continues.
func TestPlaybackSeeksWithinATrackThroughTheEngine(t *testing.T) {
	const rate = 44100
	for _, c := range seekContainers {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			a := pattern(1, 4*rate, 16)
			r.files.add("a", c.of(t, rate, a))
			r.insert("a")
			r.play()
			r.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
			r.k.Play(t, 100*time.Millisecond)
			r.tick()
			s := r.waitSnap("a heard", func(s *engine.Snapshot) bool { _, ok := s.Track(); return ok })
			heard, _ := s.Track()
			if err := r.eng.SeekAbsolute(bg, 2); err != nil {
				t.Fatalf("a seek to 2 s: %v", err)
			}
			first := r.waitCall("the first start", func(c call) bool { return c.kind == "start" })
			start := r.waitCall("the session started for the seek", func(c call) bool { return c.kind == "start" && c.s != first.s })
			if start.first != heard.Occurrence || start.at != 2*rate {
				t.Fatalf("the seek started occurrence %d from frame %d; want the one heard, %d, from %d", start.first, start.at, heard.Occurrence, 2*rate)
			}
			r.k.Await(t, "the session at the target to fill the hardware buffer", func(v alsapcm.KernelView) bool { return v.Opens == 2 && runningAndWaiting(v) })
			r.k.Play(t, 100*time.Millisecond)
			r.tick()
			played := int64(r.k.View().Consumed)
			s = r.waitSnap("the position past the target", func(s *engine.Snapshot) bool { return s.Position().Frames == 2*rate+played })
			if tr, ok := s.Track(); !ok || tr.Occurrence != heard.Occurrence || s.TrackCount() != 1 {
				t.Errorf("after the seek the track is %+v (%v), %d heard; want occurrence %d, the one track", tr, ok, s.TrackCount(), heard.Occurrence)
			}
			r.k.Free()
			r.waitStopped(engine.ReasonEndOfQueue)
			v := r.k.View()
			if v.Opens != 2 || len(v.OpenAt) != 2 {
				t.Fatalf("%d opens; want the first session's and the seek's", v.Opens)
			}
			tail := a[2*2*rate:]
			if got := unpack(t, r.k.Given()[v.OpenAt[1]:], format.S16LE, 16); !slices.Equal(got, tail) {
				t.Errorf("the seek's open was given %d samples, not exactly the %d of the track from 2 s", len(got), len(tail))
			}
			got := unpack(t, r.k.Heard(), format.S16LE, 16)
			before := len(got) - len(tail)
			if before <= 0 || !slices.Equal(got[:before], a[:before]) || !slices.Equal(got[before:], tail) {
				t.Errorf("the DAC heard %d samples; want the track's start, then the track from 2 s and nothing else", len(got))
			}
		})
	}
}
