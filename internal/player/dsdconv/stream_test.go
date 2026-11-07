package dsdconv

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// TestPaddingPastFramesIsNotRead: a stream's Frames counts one-bit
// samples, and a DSF's last byte carries padding past them, which the
// demuxer reads and the filter must not (B728 §4.2). Sources whose sample
// bits are the same give the same samples, the definition's, which never
// reads past Frames, whether the last byte's padding bits are all zero
// (0x00), all one (0xFF's) or mixed; the converter never asks its source
// for a byte past the one that holds sample Frames-1, which a source with
// bytes after it would give; and so through the real DSF demuxer too.
func TestPaddingPastFramesIsNotRead(t *testing.T) {
	for _, rate := range dsdRates {
		for _, p := range []Plan{{176400, 24}, {44100, 16}} {
			h, _ := p.Window(rate)
			d, _ := p.Decimation(rate)
			n := (3*h+2)*d + 3 // the last byte holds 3 bits of the stream and 5 of padding
			ch := [2][]byte{randomBits(1, n), randomBits(2, n)}
			want, _ := reference(t, ch, rate, p)
			last := (n+7)/8 - 1 // the byte that holds sample n-1
			for _, pad := range []byte{0x00, 0xFF, 0x15} {
				for _, after := range []int{0, 2*chunkBytes + 1} {
					planes := [2][]byte{planeOf(ch[0], false), planeOf(ch[1], false)}
					for c := range planes {
						planes[c][last] |= pad & (0xFF >> (n % 8))
						planes[c] = append(planes[c], bytes.Repeat([]byte{pad ^ 0xA5}, after)...)
					}
					s := newStub(rate, int64(n), planes)
					c, err := New(s, p, Options{})
					if err != nil {
						t.Fatal(err)
					}
					got, err := drain(t, c, 333)
					if !errors.Is(err, io.EOF) || !equal32(got, want) {
						t.Errorf("%s %s: padding %#02x, %d bytes after, gives %d samples (%v), the definition's %d, or other samples",
							rateName(rate), p, pad, after, len(got), err, len(want))
					}
					if s.pos != last+1 {
						t.Errorf("%s %s: padding %#02x, %d bytes after: the source was read to byte %d, want %d, which holds sample %d",
							rateName(rate), p, pad, after, s.pos-1, last, n-1)
					}
				}
			}
			// The real demuxer: a DSF whose last byte's padding bits are set.
			lsb := dsdtone.PackBits(ch[0], true)
			lsb[len(lsb)-1] |= 0xF8 // the padding is the high bits of an LSB-first byte
			var buf bytes.Buffer
			if err := dsdtone.WriteDSF(&buf, rate, 2, lsb, int64(n)); err != nil {
				t.Fatal(err)
			}
			src, err := decode.Open(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			c, err := New(src.(decode.DSDDecoder), p, Options{})
			if err != nil {
				t.Fatal(err)
			}
			mono, _ := reference(t, [2][]byte{ch[0], ch[0]}, rate, p)
			if got, err := drain(t, c, 4096); !errors.Is(err, io.EOF) || !equal32(got, mono) {
				t.Errorf("%s %s: through the DSF demuxer the samples are not the definition's (%v)", rateName(rate), p, err)
			}
		}
	}
}

// TestTheOutputHasFloorFramesOverD: a stream of F bits gives floor(F/D)
// frames, its Format says so, and the flush gives them and no more; a
// zero-length stream and one shorter than the filter both work.
func TestTheOutputHasFloorFramesOverD(t *testing.T) {
	for _, rate := range dsdRates {
		for _, p := range allPlans {
			d, _ := p.Decimation(rate)
			h, _ := p.Window(rate)
			for _, n := range []int{0, 1, 7, 8, d - 1, d, d + 1, 3*d + 5, h * d / 2, 10*h*d + 3} {
				ch := [2][]byte{randomBits(uint64(n)+3, n), randomBits(uint64(n)+4, n)}
				want, _ := reference(t, ch, rate, p)
				c, err := New(stubOf(rate, ch), p, Options{})
				if err != nil {
					t.Fatal(err)
				}
				if f := c.Format().Frames; f != int64(n/d) {
					t.Errorf("%s %s %d bits: Format().Frames = %d, want %d", rateName(rate), p, n, f, n/d)
				}
				got, err := drain(t, c, 100)
				if !errors.Is(err, io.EOF) || len(got) != 2*(n/d) || !equal32(got, want) || c.Emitted() != int64(n/d) {
					t.Errorf("%s %s %d bits: %d frames (%v, Emitted %d), want the definition's %d", rateName(rate), p, n, len(got)/2, err, c.Emitted(), n/d)
				}
				if n2, err := c.ReadFrames(context.Background(), make([]int32, 8)); n2 != 0 || !errors.Is(err, io.EOF) {
					t.Errorf("%s %s %d bits: a read after the end gives %d, %v", rateName(rate), p, n, n2, err)
				}
			}
		}
	}
}

// TestTheOutputIsDeterministic: the samples are a function of the bits and
// the plan alone: whatever the reads' sizes, the converter's or the
// source's, they are the chain's definition, which is integer arithmetic,
// so every architecture computes the same ones. Two runs on one machine
// would not show that, so the test also logs a digest of each output for
// the orchestrator to compare across machines (run it with -test.v); none
// is committed. The input is xorshift bits, the same on every machine.
func TestTheOutputIsDeterministic(t *testing.T) {
	for _, rate := range dsdRates {
		for _, p := range allPlans {
			d, _ := p.Decimation(rate)
			n := 2000*d + 11
			ch := [2][]byte{randomBits(0x5eed, n), randomBits(0xfeed, n)}
			ref := referenceRun(t, ch, rate, p, nil)
			for _, read := range []struct{ frames, source int }{{1, 0}, {3, 1}, {4096, 7}, {7, 4096}, {512, 0}} {
				s := stubOf(rate, ch)
				s.maxRead = read.source
				c, err := New(s, p, Options{})
				if err != nil {
					t.Fatal(err)
				}
				got, err := drain(t, c, read.frames)
				if !errors.Is(err, io.EOF) || !equal32(got, ref.out) || c.Clipped() != ref.count(0, len(ref.out)/2) {
					t.Fatalf("%s %s: reads of %d frames from a source of %d-byte reads differ from the definition (%v)", rateName(rate), p, read.frames, read.source, err)
				}
			}
			raw := make([]byte, 4*len(ref.out))
			for i, v := range ref.out {
				binary.LittleEndian.PutUint32(raw[4*i:], uint32(v))
			}
			sum := sha256.Sum256(raw)
			t.Logf("dsdconv digest %s %s: %s (%d frames, %d clipped)", rateName(rate), p, hex.EncodeToString(sum[:]), len(ref.out)/2, ref.count(0, len(ref.out)/2))
		}
	}
}

// TestReadFramesHonoursItsContext: a read whose context ends returns the
// frames it made with the context's error (n > 0 beside it, as
// decode.Decoder allows), keeps what it read for the next call, and a later
// read goes on with nothing lost; a context done before the call gives
// nothing; and a cancel the source does not see is still seen promptly.
func TestReadFramesHonoursItsContext(t *testing.T) {
	rate, p := format.DSD64RateHz, Plan{176400, 24}
	n := 64 * 4096
	ch := [2][]byte{randomBits(7, n), randomBits(8, n)}
	want, _ := reference(t, ch, rate, p)
	for _, sourceSees := range []bool{true, false} {
		s := stubOf(rate, ch)
		s.maxRead = 1024
		ctx, cancel := context.WithCancel(context.Background())
		s.hook = func(call int) {
			if call == 5 {
				cancel()
			}
		}
		// A blind source reads on whatever its context says.
		c, err := New(&ctxBlind{stubDSD: s, blind: !sourceSees}, p, Options{})
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]int32, 2*(n/16))
		got, err := c.ReadFrames(ctx, buf)
		if !errors.Is(err, context.Canceled) || got == 0 || got%2 != 0 {
			t.Fatalf("source sees ctx %v: the cancelled read gave %d samples, %v", sourceSees, got, err)
		}
		if got > 2*(6*1024*8/16) {
			t.Errorf("source sees ctx %v: the cancelled read went on to %d frames, past the read the cancel came in", sourceSees, got/2)
		}
		all := append([]int32(nil), buf[:got]...)
		rest, err := drain(t, c, 4096)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("source sees ctx %v: the read after the cancel ended with %v", sourceSees, err)
		}
		if all = append(all, rest...); !equal32(all, want) {
			t.Errorf("source sees ctx %v: the frames around the cancel are not the uncancelled stream's", sourceSees)
		}
	}
	c, err := New(stubOf(rate, ch), p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := c.ReadFrames(ctx, make([]int32, 64)); n != 0 || !errors.Is(err, context.Canceled) {
		t.Errorf("a read under a done context gave %d, %v", n, err)
	}
}

// ctxBlind is a source that, when blind, reads on after its context ends.
type ctxBlind struct {
	*stubDSD
	blind bool
}

func (b *ctxBlind) ReadDSD(ctx context.Context, dst [][]byte) (int, error) {
	if b.blind {
		ctx = context.Background()
	}
	return b.stubDSD.ReadDSD(ctx, dst)
}

// TestAShortReadKeepsItsFrames: a source that gives fewer bytes than asked,
// down to one at a time, loses nothing and adds nothing.
func TestAShortReadKeepsItsFrames(t *testing.T) {
	for _, rate := range dsdRates {
		p := Plan{88200, 24}
		d, _ := p.Decimation(rate)
		n := 300*d + 5
		ch := [2][]byte{randomBits(11, n), randomBits(12, n)}
		want, _ := reference(t, ch, rate, p)
		for _, max := range []int{1, 2, 3, 13, 100} {
			s := stubOf(rate, ch)
			s.maxRead = max
			c, err := New(s, p, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := drain(t, c, 64); !errors.Is(err, io.EOF) || !equal32(got, want) {
				t.Errorf("%s: reads of %d bytes give other samples (%v)", rateName(rate), max, err)
			}
		}
	}
}

// TestATruncatedRefusalPassesThroughAfterItsFrames: a source that ends
// early, with a truncated *decode.Refusal beside its last bytes, has the
// frames of what it gave converted, flushed as an end, and then the same
// refusal, on that call or the next, and on every call after; a source that
// says io.EOF before its declared length is a truncated refusal of the
// converter's own.
func TestATruncatedRefusalPassesThroughAfterItsFrames(t *testing.T) {
	for _, rate := range dsdRates {
		p := Plan{176400, 24}
		d, _ := p.Decimation(rate)
		n := 400 * d
		ch := [2][]byte{randomBits(21, n), randomBits(22, n)}
		cut := (n / 8) * 3 / 5 // bytes the source gives
		want, _ := reference(t, [2][]byte{ch[0][:8*cut], ch[1][:8*cut]}, rate, p)
		refusal := &decode.Refusal{Reason: decode.ReasonTruncated, Message: "the stub's data ends"}
		for _, early := range []bool{false, true} {
			s := stubOf(rate, ch)
			s.maxRead = 997
			if early {
				s.endAt = cut
			} else {
				s.failAt, s.failErr = cut, refusal
			}
			c, err := New(s, p, Options{})
			if err != nil {
				t.Fatal(err)
			}
			got, err := drain(t, c, 50)
			if !equal32(got, want) {
				t.Errorf("%s early EOF %v: %d samples before the error, the definition's %d", rateName(rate), early, len(got), len(want))
			}
			r, ok := decode.AsRefusal(err)
			switch {
			case !ok || r.Reason != decode.ReasonTruncated:
				t.Errorf("%s early EOF %v: the read ended with %v, want a truncated refusal", rateName(rate), early, err)
			case !early && r != refusal:
				t.Errorf("%s: the source's refusal was not passed through as it is: %v", rateName(rate), err)
			}
			if n, again := c.ReadFrames(context.Background(), make([]int32, 8)); n != 0 || !errors.Is(again, err) {
				t.Errorf("%s early EOF %v: the next read gave %d, %v", rateName(rate), early, n, again)
			}
		}
	}
}

// TestATruncatedSourceKeepsItsLastByte: a source that declares a partial
// last byte (Frames = 8·1000 + 3) and ends after 600 whole bytes has given
// 4,800 real bits, every one a sample: its last byte is no padding, its end
// is the bytes that came (8 · 600), and at D = 16 that is 300 frames, the
// definition's over those bits. The declared partial byte is never reached,
// so its mask applies to nothing, whether the source refuses or says io.EOF
// early, and whichever read the cut lands in.
func TestATruncatedSourceKeepsItsLastByte(t *testing.T) {
	rate, p := format.DSD64RateHz, Plan{176400, 24}
	d, _ := p.Decimation(rate)
	const declared, cut = 8*1000 + 3, 600
	if 8*cut%d != 0 {
		t.Fatalf("the cut must fall on a frame: 8·%d %% %d", cut, d)
	}
	ch := [2][]byte{randomBits(231, declared), randomBits(232, declared)}
	want, _ := reference(t, [2][]byte{ch[0][:8*cut], ch[1][:8*cut]}, rate, p)
	refusal := &decode.Refusal{Reason: decode.ReasonTruncated, Message: "the stub's data ends"}
	for _, read := range []int{0, 7, 600} {
		for _, early := range []bool{false, true} {
			s := stubOf(rate, ch)
			s.maxRead = read
			if early {
				s.endAt = cut
			} else {
				s.failAt, s.failErr = cut, refusal
			}
			c := mustNew(t, s, p, Options{})
			got, err := drain(t, c, 37)
			if r, ok := decode.AsRefusal(err); !ok || r.Reason != decode.ReasonTruncated {
				t.Errorf("reads of %d, early EOF %v: the read ended with %v, want a truncated refusal", read, early, err)
			}
			if len(got) != 2*(8*cut/d) || !equal32(got, want) {
				t.Errorf("reads of %d, early EOF %v: %d frames, want the definition's %d over the %d bits that came", read, early, len(got)/2, 8*cut/d, 8*cut)
			}
		}
	}
}

// TestSeekIsUnsupported: seeking within converted DSD stays unsupported
// (B728 §4.7), so every Seek is ErrSeek but Seek to frame 0 before the first
// read, which positions nothing and succeeds; the source is never sought.
func TestSeekIsUnsupported(t *testing.T) {
	rate, p := format.DSD64RateHz, Plan{176400, 24}
	ch := [2][]byte{randomBits(31, 8192), randomBits(32, 8192)}
	s := stubOf(rate, ch)
	c, err := New(&seekCounter{stubDSD: s}, p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Seek(context.Background(), 0); err != nil {
		t.Errorf("Seek(0) before the first read: %v", err)
	}
	if err := c.Seek(context.Background(), 1); !errors.Is(err, ErrSeek) {
		t.Errorf("Seek(1): %v, want ErrSeek", err)
	}
	if _, err := c.ReadFrames(context.Background(), make([]int32, 20)); err != nil {
		t.Fatal(err)
	}
	for _, f := range []int64{0, 5, -1} {
		if err := c.Seek(context.Background(), f); !errors.Is(err, ErrSeek) {
			t.Errorf("Seek(%d) after a read: %v, want ErrSeek", f, err)
		}
	}
	if got := c.src.(*seekCounter).seeks; got != 0 {
		t.Errorf("the source was sought %d times", got)
	}
}

type seekCounter struct {
	*stubDSD
	seeks int
}

func (s *seekCounter) Seek(ctx context.Context, pos int64) error {
	s.seeks++
	return s.stubDSD.Seek(ctx, pos)
}

// TestCloseClosesTheSourceOnce: Close closes the source once, however often
// it is called, and every call after it is ErrClosed.
func TestCloseClosesTheSourceOnce(t *testing.T) {
	s := stubOf(format.DSD64RateHz, [2][]byte{randomBits(41, 4096), randomBits(42, 4096)})
	c, err := New(s, Plan{176400, 24}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := c.Close(); err != nil {
			t.Errorf("Close %d: %v", i, err)
		}
	}
	if s.closes != 1 {
		t.Errorf("the source was closed %d times", s.closes)
	}
	if _, err := c.ReadFrames(context.Background(), make([]int32, 2)); !errors.Is(err, ErrClosed) {
		t.Errorf("a read after Close: %v, want ErrClosed", err)
	}
}

// allocsOnce is the allocations f makes the one time it runs, which
// testing.AllocsPerRun cannot say (it runs f once before it counts).
func allocsOnce(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.Mallocs - before.Mallocs
}

// loopDSD is a source that never ends: planes over and over, under a
// Frames too large to reach, its reads allocating nothing.
type loopDSD struct {
	f      format.NativeDSD
	planes [2][]byte
	pos    int
}

func newLoop(rate int, seconds float64) *loopDSD {
	n := int(float64(rate)*seconds) / 8
	return &loopDSD{
		f:      format.NativeDSD{BitRateHz: rate, Channels: 2, Container: format.ContainerDFF, Frames: 1 << 50},
		planes: [2][]byte{planeOf(randomBits(51, 8*n), false), planeOf(randomBits(52, 8*n), false)},
	}
}

func (l *loopDSD) Format() format.NativeDSD { return l.f }
func (l *loopDSD) Seek(context.Context, int64) error {
	return errors.New("loop: no seek")
}
func (l *loopDSD) Close() error { return nil }
func (l *loopDSD) ReadDSD(ctx context.Context, dst [][]byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	n := copy(dst[0], l.planes[0][l.pos:])
	copy(dst[1][:n], l.planes[1][l.pos:])
	if l.pos += n; l.pos == len(l.planes[0]) {
		l.pos = 0
	}
	return n, nil
}

// TestReadFramesDoesNotAllocate: once made, a converter's reads allocate
// nothing, over a warmed source that allocates nothing itself; so do its
// window's steps.
func TestReadFramesDoesNotAllocate(t *testing.T) {
	for _, rate := range dsdRates {
		for _, p := range []Plan{{176400, 24}, {44100, 16}} {
			c, err := New(newLoop(rate, 0.5), p, Options{})
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]int32, 2*1024)
			for i := 0; i < 8; i++ {
				if _, err := c.ReadFrames(context.Background(), buf); err != nil {
					t.Fatal(err)
				}
			}
			if a := testing.AllocsPerRun(50, func() {
				if _, err := c.ReadFrames(context.Background(), buf); err != nil {
					t.Fatal(err)
				}
			}); a != 0 {
				t.Errorf("%s %s: a read allocates %v times", rateName(rate), p, a)
			}
			// The window: held, recomputed, ended.
			d, _ := p.Decimation(rate)
			h, _ := p.Window(rate)
			n := (4*h + 3) * d
			ch := [2][]byte{randomBits(61, n), randomBits(62, n)}
			w, err := New(stubOf(rate, ch), p, Options{Hold: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := drain(t, w, 64); !errors.Is(err, ErrWindow) {
				t.Fatalf("%s %s: no window (%v)", rateName(rate), p, err)
			}
			if a := testing.AllocsPerRun(20, func() {
				if err := w.End(); err != nil {
					t.Fatal(err)
				}
				for {
					if _, err := w.ReadFrames(context.Background(), buf[:64]); err != nil {
						break
					}
				}
				if err := w.Rewind(); err != nil {
					t.Fatal(err)
				}
			}); a != 0 {
				t.Errorf("%s %s: the window's End, reads and Rewind allocate %v times", rateName(rate), p, a)
			}
		}
	}
}

// TestTheFirstReadOfARealSourceAllocatesOnlyInTheDemuxer measures, end to
// end, what the first read allocates over fresh DSF and DFF sources: the
// real demuxers allocate their read buffer lazily on their first read
// (decode/dsf.go's span buffer, decode/pcm_region.go's byte buffer), which
// B10 leaves alone, since a change there moves the qualification guard.
// The converter's own share is none (the warmed test above); this one says
// what the demuxer's is, and that the reads after it allocate nothing.
func TestTheFirstReadOfARealSourceAllocatesOnlyInTheDemuxer(t *testing.T) {
	dir := t.TempDir()
	for _, rate := range dsdRates {
		for _, kind := range []string{"dsf", "dff"} {
			path := filepath.Join(dir, rateName(rate)+"."+kind)
			tone := dsdtone.Tone{RateHz: rate, Seconds: 0.2, AmplitudeDBFS: -20}
			var err error
			if kind == "dsf" {
				_, err = dsdtone.MintDSF(path, tone)
			} else {
				_, err = dsdtone.MintDFF(path, tone)
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			src, err := decode.Open(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			c, err := New(src.(decode.DSDDecoder), Plan{176400, 24}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]int32, 2*1024)
			allocs := allocsOnce(func() {
				if _, err := c.ReadFrames(context.Background(), buf); err != nil {
					t.Fatal(err)
				}
			})
			later := testing.AllocsPerRun(5, func() {
				if _, err := c.ReadFrames(context.Background(), buf); err != nil && !errors.Is(err, io.EOF) {
					t.Fatal(err)
				}
			})
			t.Logf("%s %s: the first read allocates %d times (the demuxer's buffer), each read after it %v", rateName(rate), kind, allocs, later)
			if allocs > 2 || later != 0 {
				t.Errorf("%s %s: the first read allocates %d times and the later ones %v", rateName(rate), kind, allocs, later)
			}
		}
	}
}
