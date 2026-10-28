package alsapcm

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
	"unsafe"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

func dopConfig() DSDConfig {
	c := nativeConfig()
	c.Format = format.S32LE
	c.Kind = format.KindDoP
	return c
}
func dopKernel(t *testing.T) *fakeKernel {
	k := nativeKernel(t)
	k.hw.formats = append(k.hw.formats, formatS32LE)
	return k
}

// dopOddKernel commits a 1001-frame period and a 7007-frame buffer, both
// odd, so every period's write, the buffer filling and a pause there end on
// an odd frame index: a phase that restarted at any of them would repeat a
// marker the oracle does not.
func dopOddKernel(t *testing.T) *fakeKernel {
	k := dopKernel(t)
	k.hw.perMin, k.hw.perMax = 1001, 1001
	k.hw.bufMin, k.hw.bufMax = 7007, 7007
	return k
}

// dopPattern is n bytes per plane, the channels different at every index,
// with no 0x69 byte, so a frame of silence is never taken for content.
func dopPattern(n int) (l, r []byte) {
	l, r = make([]byte, n), make([]byte, n)
	for i := range n {
		l[i], r[i] = byte(i*37+11), byte(i*101+0xa5)
		if l[i] == 0x69 {
			l[i] = 0x96
		}
		if r[i] == 0x69 {
			r[i] = 0x97
		}
	}
	return l, r
}

// dopStream is the independent oracle: what a DoP sink must give the
// kernel, assembled one bit at a time from the DoP definition, never through
// PackDoP. A channel's word is its next 16 canonical bits, the earliest at
// bit 15; left-aligned in S32_LE it is 00, the low byte, the high byte, the
// marker. Both channels of a frame carry one marker, which starts each
// hardware run at 05 and alternates once per stereo frame through content,
// padding and silence alike. Content is padded only at a final end, by the
// 0x69 pattern's bits at their own positions.
type dopStream struct {
	out         []byte
	marker      byte
	left, right []byte // content bits not yet in a frame, one per byte
}

func newDoPStream() *dopStream { return &dopStream{marker: 0x05} }

// run starts a new hardware run.
func (o *dopStream) run() *dopStream { o.marker = 0x05; return o }

// bits appends n bits of each plane from bit from on.
func (o *dopStream) bits(l, r []byte, from, n int) *dopStream {
	for i := from; i < from+n; i++ {
		o.left = append(o.left, (l[i/8]>>(7-i%8))&1)
		o.right = append(o.right, (r[i/8]>>(7-i%8))&1)
	}
	return o.frames(false)
}

// pad completes the last partial word, as a final end does.
func (o *dopStream) pad() *dopStream { return o.frames(true) }

// silence appends frames of 0x69 silence, which only ever follow whole
// words.
func (o *dopStream) silence(frames int) *dopStream {
	if len(o.left) != 0 {
		panic("dopStream: silence inside a partial word")
	}
	for range frames {
		o.emit(0x6969, 0x6969)
	}
	return o
}

func (o *dopStream) frames(pad bool) *dopStream {
	for len(o.left) >= 16 || (pad && len(o.left) > 0) {
		var w [2]uint16
		for ch, bits := range [2][]byte{o.left, o.right} {
			for j := range 16 {
				b := (byte(0x69) >> (7 - j%8)) & 1
				if j < len(bits) {
					b = bits[j]
				}
				w[ch] |= uint16(b) << (15 - j)
			}
		}
		n := min(16, len(o.left))
		o.left, o.right = o.left[n:], o.right[n:]
		o.emit(w[0], w[1])
	}
	return o
}

func (o *dopStream) emit(l, r uint16) {
	for _, v := range [2]uint16{l, r} {
		o.out = append(o.out, 0, byte(v), byte(v>>8), o.marker)
	}
	if o.marker == 0x05 {
		o.marker = 0xfa
	} else {
		o.marker = 0x05
	}
}

// sameDoP fails the test unless got is want, naming the first frame that
// differs.
func sameDoP(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	i := 0
	for i < min(len(got), len(want)) && got[i] == want[i] {
		i++
	}
	f := i / 8 * 8
	t.Fatalf("%s: %d bytes, want %d; frame %d is % x, want % x", what, len(got), len(want), f/8, got[f:min(f+8, len(got))], want[f:min(f+8, len(want))])
}

func TestDoPExactRate(t *testing.T) {
	for i, name := range []string{"DSD64", "DSD128", "DSD256"} {
		t.Run(name, func(t *testing.T) {
			c := dopConfig()
			c.BitRate <<= i
			k := dopKernel(t)
			s := nativeSink(t, k, c)
			p := s.Params()
			tr := s.Transport()
			if p.RateHz != 176400<<i || p.Format != format.S32LE || p.Channels != 2 || tr.Kind != format.KindDoP || tr.LogicalRate != c.BitRate || s.word != 16 || s.ring.frameBytes != 4 {
				t.Fatalf("params=%+v transport=%+v word=%d", p, tr, s.word)
			}
		})
	}
}
func TestDoPFinalBits(t *testing.T) {
	for _, tc := range []struct {
		name string
		bits int
	}{{"one_bit", 1}, {"one_frame", 16}, {"797", 797}} {
		t.Run(tc.name, func(t *testing.T) { testDoPBits(t, tc.bits) })
	}
	t.Run("remainders", func(t *testing.T) {
		for n := 1; n < 16; n++ {
			testDoPBits(t, 32+n)
		}
	})
}
func testDoPBits(t *testing.T, n int) {
	t.Helper()
	k := dopKernel(t)
	k.free()
	s := nativeSink(t, k, dopConfig())
	l, r := bytes.Repeat([]byte{0x81}, (n+7)/8), bytes.Repeat([]byte{0x23}, (n+7)/8)
	if n == 1 {
		l[0] = 0x80
		r[0] = 0
	}
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	if got, e := s.WriteDSD(context.Background(), 1, l, r, 0, n); e != nil || got != n {
		t.Fatalf("accepted %d want %d: %v", got, n, e)
	}
	if e := s.Finish(context.Background()); e != nil {
		t.Fatal(e)
	}
	p := s.Position()
	if p.Submitted != int64(n) || p.Played != int64(n) {
		t.Fatal(p)
	}
	// Independent bit-position oracle: container bits assembled one bit at a time.
	want := make([]byte, ((n+15)/16)*8)
	for f := 0; f < len(want)/8; f++ {
		for ch, src := range [][]byte{l, r} {
			var v uint16
			for j := 0; j < 16; j++ {
				at := f*16 + j
				bit := (byte(0x69) >> uint(7-j%8)) & 1
				if at < n {
					bit = (src[at/8] >> uint(7-at%8)) & 1
				}
				v |= uint16(bit) << uint(15-j)
			}
			base := f*8 + ch*4
			want[base+1] = byte(v)
			want[base+2] = byte(v >> 8)
			want[base+3] = 5
			if f%2 == 1 {
				want[base+3] = 0xfa
			}
		}
	}
	if !bytes.Equal(given(k), want) {
		t.Fatalf("bits=%d got %x want %x", n, given(k), want)
	}
}
func TestDoPPrecision(t *testing.T) {
	for _, path := range []string{"initial", "reopen"} {
		t.Run(path, func(t *testing.T) {
			for _, bits := range []uint32{16, 20, 24, 32, 0} {
				t.Run(fmt.Sprint(bits), func(t *testing.T) {
					k := dopKernel(t)
					c := dopConfig()
					c.Prefill = time.Microsecond
					set := func() { k.inspect(func(k *fakeKernel) { k.commit = func(p *hwParams) { p.msbits = bits } }) }
					open := func() (*DSDSink, error) {
						return openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(k, k.now, ep, c) }, now: k.now, at: k.nativeAt})
					}
					var s *DSDSink
					var e error
					before := 0
					if path == "initial" {
						set()
						s, e = open()
					} else {
						s, e = open()
						if e != nil {
							t.Fatal(e)
						}
						defer s.Release(context.Background(), Immediate)
						if e = s.Activate(context.Background(), 1); e != nil {
							t.Fatal(e)
						}
						if e = s.Pause(context.Background()); e != nil {
							t.Fatal(e)
						}
						k.grant(31 * time.Second)
						nativeWait(t, s, "released", func() bool { return s.released })
						k.revoke()
						before = len(given(k))
						set()
						e = s.Resume(context.Background())
					}
					if path == "initial" && s != nil {
						defer s.Release(context.Background(), Immediate)
						e = s.Activate(context.Background(), 1)
					}
					good := bits >= 24 && bits <= 32
					if (e == nil) != good {
						t.Fatalf("precision %d: %v", bits, e)
					}
					if !good && len(given(k)) != before {
						t.Fatal("insufficient precision wrote")
					}
					if !good && path == "reopen" && !s.Position().Paused {
						t.Fatal("refused reopen lost pause")
					}
				})
			}
		})
	}
}

type dopSWSys struct {
	sys
	corrupt   bool
	requested swParams
}

func (s *dopSWSys) ioctl(fd int, req uint32, p unsafe.Pointer) error {
	if req == ioctlSWParams {
		s.requested = *(*swParams)(p)
	}
	e := s.sys.ioctl(fd, req, p)
	if req == ioctlSWParams && s.corrupt {
		(*swParams)(p).silenceSize = 1
	}
	return e
}
func TestDoPSoftwareParams(t *testing.T) {
	for _, name := range []string{"zero_fill", "readback"} {
		t.Run(name, func(t *testing.T) {
			k := dopKernel(t)
			hw := &dopSWSys{sys: k, corrupt: name == "readback"}
			c := dopConfig()
			s, e := openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(hw, k.now, ep, c) }, now: k.now, at: k.nativeAt})
			if s != nil {
				defer s.Release(context.Background(), Immediate)
			}
			if hw.corrupt {
				if e == nil {
					t.Fatal("accepted SW silence fill readback")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			sw := hw.requested
			if sw.silenceSize != 0 || sw.silenceThreshold != 0 || sw.stopThreshold != uint64(s.Params().BufferFrames) {
				t.Fatal("requested unsafe SW parameters")
			}
		})
	}
}

func assertDoPMarkers(t *testing.T, b []byte, phase byte) {
	t.Helper()
	if len(b)%8 != 0 {
		t.Fatal("partial stereo frame")
	}
	for i := 0; i < len(b); i += 8 {
		if b[i] != 0 || b[i+4] != 0 || b[i+3] != phase || b[i+7] != phase {
			t.Fatalf("marker at frame %d: %x want %x", i/8, b[i:i+8], phase)
		}
		phase ^= 0xff
	}
}

// TestDoPPhase pins the writer-owned marker phase at every kind of boundary,
// each leaf's boundary on an odd frame index, against the independent
// oracle of the uninterrupted stream: what the kernel was given must be
// exactly that, byte for byte. partial1 and partial3 accept one and three
// frames of a sixteen-frame request, so a phase advanced by the request
// rather than by the frames accepted repeats a marker.
func TestDoPPhase(t *testing.T) {
	for _, name := range []string{"buffer", "period", "ring_wrap", "occurrence", "gapless", "partial1", "partial3", "eintr", "eagain"} {
		t.Run(name, func(t *testing.T) { testDoPStreamPhase(t, name) })
	}
	for _, name := range []string{"kernel_pause", "silence_pause", "reopen", "prefill", "tail", "cut", "cancel", "track_change"} {
		t.Run(name, func(t *testing.T) { testDoPRunPhase(t, name) })
	}
}

func testDoPStreamPhase(t *testing.T, name string) {
	ctx := context.Background()
	k := dopKernel(t)
	c := dopConfig()
	frames := 16
	switch name {
	case "buffer":
		k = dopOddKernel(t)
		frames = 7007 + 1001 + 3
	case "period":
		k = dopOddKernel(t)
		frames = 3*1001 + 5
	case "ring_wrap":
		k = dopOddKernel(t)
		c.RingBytes = 4 * 7509
		frames = 3*7509 + 7
	case "partial1", "partial3":
		take := 1
		if name == "partial3" {
			take = 3
		}
		k.writes = []fakeWrite{{take: take}, {err: errnoEAGAIN}}
		k.polls = []fakePoll{{block: true}}
	case "eintr":
		k.writes = []fakeWrite{{err: errnoEINTR}, {take: 1}}
	case "eagain":
		k.writes = []fakeWrite{{err: errnoEAGAIN}, {take: 3}}
	}
	s := nativeSink(t, k, c)
	l, r := dopPattern(frames * 2)
	want := newDoPStream()
	write := func(id uint64, from, n int) {
		t.Helper()
		if got, e := s.WriteDSD(ctx, id, l, r, from, n); e != nil || got != n {
			t.Fatalf("accepted %d of %d bits: %v", got, n, e)
		}
		want.bits(l, r, from, n)
	}
	activate := func() {
		t.Helper()
		if e := s.Activate(ctx, 1); e != nil {
			t.Fatal(e)
		}
	}
	switch name {
	case "ring_wrap":
		// The ring holds 7509 frames, odd, and keeps the buffer's 7007
		// behind its writer, so the content wraps it at odd frame indexes in
		// writes the ring's end cuts short.
		activate()
		k.free()
		write(1, 0, frames*16)
	case "occurrence":
		// Compatible occurrences whose joins fall inside words share the
		// carry: their bits follow each other with nothing between them.
		at := 0
		for i, n := range []int{33, 7, 15, 1, 40} {
			write(uint64(i+1), at, n)
			at += n
		}
		activate()
		k.free()
		want.pad()
	case "gapless":
		// Occurrence 1 is three whole frames, all written; occurrence 2's
		// first frame is the next write's, at frame 3.
		write(1, 0, 48)
		activate()
		await(t, k.when(filledAndIdle(3)), "occurrence 1's three frames written")
		write(2, 48, 80)
		k.free()
	case "buffer":
		write(1, 0, frames*16)
		activate()
		await(t, k.when(filledAndIdle(7007)), "the 7007-frame buffer full and the writer waiting for room")
		k.free()
	default:
		write(1, 0, frames*16)
		activate()
		if name == "partial1" || name == "partial3" {
			// The writer waits for room with part of its request accepted;
			// a pause ends that write and a resume writes the rest.
			await(t, k.entered, "the accepted prefix and a wait for room")
			if e := s.Pause(ctx); e != nil {
				t.Fatal(e)
			}
			if e := s.Resume(ctx); e != nil {
				t.Fatal(e)
			}
		}
		k.free()
	}
	if e := s.Finish(ctx); e != nil {
		t.Fatal(e)
	}
	sameDoP(t, name, given(k), want.out)
	if p := s.Position(); p.Played != p.Submitted {
		t.Fatalf("played %d of %d submitted bits", p.Played, p.Submitted)
	}
}

func testDoPRunPhase(t *testing.T, name string) {
	ctx := context.Background()
	k := dopKernel(t)
	c := dopConfig()
	frames := 16
	switch name {
	case "kernel_pause", "silence_pause":
		k = dopOddKernel(t)
		frames = 7007 + 1001 + 3
		if name == "silence_pause" {
			c.Pause = PauseZeros
		}
	case "reopen", "prefill":
		c.Prefill = time.Microsecond // one frame
	case "tail":
		c.Tail = time.Microsecond // one frame
		frames = 7
	case "track_change":
		c.Tail = time.Microsecond
	}
	if name == "cancel" || name == "track_change" {
		k.writes = []fakeWrite{{take: 1}, {err: errnoEAGAIN}}
		k.polls = []fakePoll{{block: true}}
	}
	s := nativeSink(t, k, c)
	l, r := dopPattern(frames * 2)
	want := newDoPStream()
	if name == "reopen" {
		// One frame of prefill leaves the run's phase at FA; the run ends with
		// the pause's release, and the reopened run's prefill starts at 05.
		if e := s.Activate(ctx, 1); e != nil {
			t.Fatal(e)
		}
		if e := s.Pause(ctx); e != nil {
			t.Fatal(e)
		}
		k.grant(31 * time.Second)
		nativeWait(t, s, "released pause", func() bool { return s.released })
		k.revoke()
		if n := len(given(k)) / 8; n%2 != 1 {
			t.Fatalf("the released run gave %d frames; the reopen needs an odd count", n)
		}
		if e := s.Resume(ctx); e != nil {
			t.Fatal(e)
		}
		if e := s.Release(ctx, Immediate); e != nil {
			t.Fatal(e)
		}
		sameDoP(t, name, given(k), want.silence(1).run().silence(1).out)
		return
	}
	if got, e := s.WriteDSD(ctx, 1, l, r, 0, frames*16); e != nil || got != frames*16 {
		t.Fatalf("accepted %d: %v", got, e)
	}
	if name == "cut" {
		// The cut keeps 17 bits: a frame and one bit, padded at the end.
		if ok, e := s.Cut(17); e != nil || !ok {
			t.Fatal(ok, e)
		}
	}
	if e := s.Activate(ctx, 1); e != nil {
		t.Fatal(e)
	}
	switch name {
	case "cancel", "track_change":
		// One frame of the request is accepted and the writer waits for
		// room: the release ends that write, keeps the frame and drops the
		// rest; a track change then appends its tail in the same run.
		await(t, k.entered, "the accepted prefix and a wait for room")
		k.free()
		mode := Immediate
		if name == "track_change" {
			mode = TrackChange
		}
		if e := s.Release(ctx, mode); e != nil {
			t.Fatal(e)
		}
		want.bits(l, r, 0, 16)
		if name == "track_change" {
			want.silence(1)
		}
		sameDoP(t, name, given(k), want.out)
		return
	case "kernel_pause", "silence_pause":
		// The pause comes once the odd 7007-frame buffer is full and running.
		await(t, k.when(func(k *fakeKernel) bool { return k.appl == 7007 && k.state == stateRunning && starved(k) }), "a full running buffer")
		if e := s.Pause(ctx); e != nil {
			t.Fatal(e)
		}
		written := s.Position().Written
		if name == "silence_pause" {
			k.grant(2 * s.Params().PeriodTime())
			await(t, k.when(func(k *fakeKernel) bool { return k.total > 7007 && starved(k) }), "pause silence accepted")
			if s.Position().Written != written {
				t.Fatal("pause silence counted as content")
			}
		} else {
			var state int32
			k.inspect(func(k *fakeKernel) { state = k.state })
			if state != statePaused {
				t.Fatalf("the kernel did not pause: state %d", state)
			}
		}
		if e := s.Resume(ctx); e != nil {
			t.Fatal(e)
		}
	}
	k.free()
	if e := s.Finish(ctx); e != nil {
		t.Fatal(e)
	}
	got := given(k)
	switch name {
	case "kernel_pause":
		want.bits(l, r, 0, frames*16)
	case "silence_pause":
		// The pause's silence lies between frame 7007 and the rest of the
		// content; it is counted from what was given, and must continue the
		// markers there.
		silent := 0
		for f := 7007; (f+1)*8 <= len(got) && bytes.Equal([]byte{got[f*8+1], got[f*8+2], got[f*8+5], got[f*8+6]}, []byte{0x69, 0x69, 0x69, 0x69}); f++ {
			silent++
		}
		if silent == 0 {
			t.Fatal("no pause silence given")
		}
		want.bits(l, r, 0, 7007*16).silence(silent).bits(l, r, 7007*16, (frames-7007)*16)
	case "prefill":
		want.silence(1).bits(l, r, 0, frames*16)
	case "tail":
		want.bits(l, r, 0, frames*16).silence(1)
	case "cut":
		want.bits(l, r, 0, 17).pad()
	}
	sameDoP(t, name, got, want.out)
	if p := s.Position(); p.Played != p.Submitted {
		t.Fatalf("played %d of %d submitted bits", p.Played, p.Submitted)
	}
}

// TestDoPSilenceVectors checks prefill and tail silence, each leaf for one
// property alone: no frame of zeros, the 0x69 payload with its zero pad
// byte, and markers that alternate from 05 through both.
func TestDoPSilenceVectors(t *testing.T) {
	for _, name := range []string{"zeros", "payload", "marker"} {
		t.Run(name, func(t *testing.T) {
			k := dopKernel(t)
			k.free()
			c := dopConfig()
			c.Prefill = time.Millisecond
			c.Tail = time.Millisecond
			s := nativeSink(t, k, c)
			if e := s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			if e := s.Release(context.Background(), TrackChange); e != nil {
				t.Fatal(e)
			}
			got := given(k)
			if len(got) != 177*8*2 {
				t.Fatal("silence duration", len(got))
			}
			for f := 0; f < len(got)/8; f++ {
				frame := got[f*8 : f*8+8]
				switch name {
				case "zeros":
					if bytes.Equal(frame, make([]byte, 8)) {
						t.Fatalf("frame %d is zeros, which a DAC plays as PCM", f)
					}
				case "payload":
					for ch := range 2 {
						if w := frame[ch*4 : ch*4+3]; !bytes.Equal(w, []byte{0, 0x69, 0x69}) {
							t.Fatalf("frame %d channel %d payload % x, want 00 69 69", f, ch, w)
						}
					}
				case "marker":
					m := byte(0x05)
					if f%2 == 1 {
						m = 0xfa
					}
					if frame[3] != m || frame[7] != m {
						t.Fatalf("frame %d markers %02x %02x, want %02x", f, frame[3], frame[7], m)
					}
				}
			}
		})
	}
}
func TestDoPTrackChangeTail(t *testing.T) {
	k := dopKernel(t)
	k.free()
	c := dopConfig()
	c.Tail = time.Millisecond
	s := nativeSink(t, k, c)
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	if e := s.Release(context.Background(), TrackChange); e != nil {
		t.Fatal(e)
	}
	if len(given(k)) != 177*8 || s.Position().Played != 0 {
		t.Fatal("tail missing or counted as content")
	}
	assertDoPMarkers(t, given(k), 5)
	k.checkClosedEach(t)
}
func TestDoPInertActivation(t *testing.T) {
	k := dopKernel(t)
	c := dopConfig()
	c.Prefill = time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	c.Check = func(NodeInfo) error {
		calls++
		if calls == 2 {
			close(entered)
			<-release
		}
		return nil
	}
	s := nativeSink(t, k, c)
	answer := make(chan error, 1)
	go func() { answer <- s.Activate(context.Background(), 1) }()
	<-entered
	if e := s.Pause(context.Background()); e != nil {
		t.Fatal(e)
	}
	close(release)
	if e := <-answer; e == nil {
		t.Fatal("activation crossed pause fence")
	}
	if len(given(k)) != 0 {
		t.Fatal("paused inert sink wrote")
	}
}

// TestDoPTransportIdentity: kind isolates the representation, every other
// transport field equal; policy holds a sink to the lifecycle policy it was
// admitted with: a reopened run whose device no longer pauses in the kernel
// is refused before any write, its content kept and the sink paused. Which
// policy's sink an occurrence may join is the producer's (playback,
// TestNativeIntegrationDoPTransitions/policy).
func TestDoPTransportIdentity(t *testing.T) {
	t.Run("kind", func(t *testing.T) {
		a := Transport{Kind: format.KindDoP, RateHz: 176400, Channels: 2, Format: format.S32LE, ValidBits: 1, LogicalRate: 2822400, PackingVersion: 1}
		b := a
		b.Kind = format.KindPCM
		if a.Join(b).Gapless {
			t.Fatal("kind ignored")
		}
	})
	t.Run("policy", func(t *testing.T) {
		ctx := context.Background()
		k := dopKernel(t)
		c := dopConfig()
		c.Prefill = time.Microsecond
		s := nativeSink(t, k, c)
		if e := s.Activate(ctx, 1); e != nil {
			t.Fatal(e)
		}
		if e := s.Pause(ctx); e != nil {
			t.Fatal(e)
		}
		k.grant(31 * time.Second)
		nativeWait(t, s, "released pause", func() bool { return s.released })
		k.revoke()
		before := len(given(k))
		k.inspect(func(k *fakeKernel) { k.hw.info &^= infoPause })
		if e := s.Resume(ctx); e == nil {
			t.Fatal("a reopened run without the kernel's pause continued a kernel-pause sink")
		}
		if len(given(k)) != before {
			t.Fatal("the refused reopen wrote")
		}
		if !s.Position().Paused {
			t.Fatal("the refused reopen lost the pause")
		}
	})
}
func TestDoPStorageBudget(t *testing.T) {
	k := dopKernel(t)
	c := dopConfig()
	c.BitRate = 11289600
	c.BufferTime = 500 * time.Millisecond
	probe := nativeSink(t, k, c)
	period := probe.Params().PeriodFrames
	// Four canonical bytes a frame in the ring and its scratch, eight
	// hardware bytes a frame in the packed scratch.
	if probe.ring.frameBytes != 4 || len(probe.scratch) != period*4 || len(probe.dopScratch) != period*8 {
		t.Fatalf("ring frame %d bytes, scratch %d and %d bytes for a %d-frame period", probe.ring.frameBytes, len(probe.scratch), len(probe.dopScratch), period)
	}
	if e := probe.Release(context.Background(), Immediate); e != nil {
		t.Fatal(e)
	}
	c.RingBytes = MaxRingBytes - (period*4 + nativeLedgerMax*16 + 8 + 4096)
	s, e := openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(k, k.now, ep, c) }, now: k.now, at: k.nativeAt})
	if s != nil {
		s.Release(context.Background(), Immediate)
	}
	if e == nil {
		t.Fatal("packed scratch omitted from aggregate budget")
	}
}

// TestDoPBitTransparent pins that nothing between the canonical bits and
// the kernel changes a DoP sample. values: every byte value on both
// channels, written in 13-bit pieces through the carry, reaches the kernel
// as exactly their DoP, so no gain, dither, mixing or conversion touched
// them. raw_node: the open is the endpoint's own device node, never a
// plugin. format, rate, rational and channels: a committed setup other
// than the one asked for is refused before any write.
func TestDoPBitTransparent(t *testing.T) {
	t.Run("values", func(t *testing.T) {
		k := dopKernel(t)
		k.free()
		s := nativeSink(t, k, dopConfig())
		l, r := make([]byte, 512), make([]byte, 512)
		for i := range l {
			l[i], r[i] = byte(i), byte(255-i)
		}
		if e := s.Activate(context.Background(), 1); e != nil {
			t.Fatal(e)
		}
		for at := 0; at < len(l)*8; at += 13 {
			n := min(13, len(l)*8-at)
			if got, e := s.WriteDSD(context.Background(), 1, l, r, at, n); e != nil || got != n {
				t.Fatalf("accepted %d of %d: %v", got, n, e)
			}
		}
		if e := s.Finish(context.Background()); e != nil {
			t.Fatal(e)
		}
		sameDoP(t, "values", given(k), newDoPStream().bits(l, r, 0, len(l)*8).out)
	})
	t.Run("raw_node", func(t *testing.T) {
		k := dopKernel(t)
		s := nativeSink(t, k, dopConfig())
		if e := s.Release(context.Background(), Immediate); e != nil {
			t.Fatal(e)
		}
		var opened []string
		k.inspect(func(k *fakeKernel) { opened = append(opened, k.opened...) })
		if len(opened) != 1 || opened[0] != "/dev/snd/pcmC5D0p" {
			t.Fatalf("opened %q, want the endpoint's own node alone", opened)
		}
	})
	for _, tc := range []struct {
		name   string
		commit func(*hwParams)
	}{
		{"format", func(p *hwParams) { p.setMask(hwParamFormat, formatS24LE) }},
		{"rate", func(p *hwParams) { p.setInteger(hwParamRate, 192000) }},
		{"rational", func(p *hwParams) { p.rateNum, p.rateDen = 176401, 1 }},
		{"channels", func(p *hwParams) { p.setInteger(hwParamChannels, 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := dopKernel(t)
			k.commit = tc.commit
			s, e := openDSDSink(context.Background(), dopConfig(), nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(k, k.now, ep, c) }, now: k.now, at: k.nativeAt})
			if s != nil {
				s.Release(context.Background(), Immediate)
				t.Fatal("a converted setup was taken")
			}
			if ReasonOf(e) != ReasonMismatch {
				t.Fatalf("refusal %v, want %s", e, ReasonMismatch)
			}
			if len(given(k)) != 0 {
				t.Fatal("a refused setup was written")
			}
			k.checkClosedEach(t)
		})
	}
}

// TestDoPUnderrun: the buffer running dry stops the DoP stream and ends the
// sink with the xrun. The bridge neither fills silence, zeros or 0x69, nor
// recovers: the kernel was given exactly the content's prefix, and one
// PREPARE, the open's.
func TestDoPUnderrun(t *testing.T) {
	k := dopOddKernel(t)
	s := nativeSink(t, k, dopConfig())
	l, r := dopPattern(2 * 9009)
	if got, e := s.WriteDSD(context.Background(), 1, l, r, 0, 9009*16); e != nil || got != 9009*16 {
		t.Fatalf("accepted %d: %v", got, e)
	}
	k.inspect(func(k *fakeKernel) {
		k.at = append(k.at, fakeAt{frames: 8008, do: func(k *fakeKernel) { k.state = stateXrun }})
	})
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	k.free()
	await(t, s.Done(), "the sink to end at the underrun")
	if ReasonOf(s.Result()) != ReasonXrun {
		t.Fatalf("result %v, want an xrun", s.Result())
	}
	if n := k.callsOf(ioctlPrepare); n != 1 {
		t.Fatalf("%d PREPAREs: the underrun was recovered", n)
	}
	sameDoP(t, "underrun", given(k), newDoPStream().bits(l, r, 0, 8008*16).out)
	k.checkClosedEach(t)
}

// These admission regression pins reuse the native rate gate; decoders still support DSD512.
func TestDoPUnsupportedAdmission(t *testing.T) {
	for _, name := range []string{"DSD512", "48k_family", "S24_LE", "S24_3LE"} {
		t.Run(name, func(t *testing.T) {
			c := dopConfig()
			switch name {
			case "DSD512":
				c.BitRate = 22579200
			case "48k_family":
				c.BitRate = 3072000
			default:
				c.Format = format.ALSAFormat(name)
			}
			k := dopKernel(t)
			opens := 0
			_, err := openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { opens++; return openNative(k, k.now, ep, c) }, now: k.now, at: k.nativeAt})
			if err == nil || opens != 0 {
				t.Fatalf("unsupported admission err=%v opens=%d", err, opens)
			}
		})
	}
}

func TestDoPReleaseEscalation(t *testing.T) {
	k := dopKernel(t)
	g := &nativePollGate{sys: k, entered: make(chan struct{}), gate: make(chan struct{})}
	s, e := openDSDSink(context.Background(), dopConfig(), nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(g, k.now, ep, c) }, now: k.now, at: k.nativeAt})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Release(context.Background(), Immediate)
	if e = s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	_, _ = s.WriteDSD(context.Background(), 1, []byte{1, 2, 3, 4}, []byte{5, 6, 7, 8}, 0, 32)
	await(t, k.when(filledAndIdle(2)), "queued word")
	g.armed.Store(true)
	answer := make(chan error, 1)
	go func() { answer <- s.Release(context.Background(), TrackChange) }()
	await(t, g.entered, "cancellable drain seam")
	start := time.Now()
	immediate := make(chan error, 1)
	go func() { immediate <- s.Release(context.Background(), Immediate) }()
	select {
	case e = <-immediate:
		if e != nil {
			t.Errorf("escalation failed: %v", e)
		}
	case <-time.After(time.Second):
		t.Errorf("escalation did not cancel drain promptly after %v", time.Since(start))
		// Let a defective escalation end through the independently tested
		// release clock, so the mutation produces an assertion, not a hang.
		k.advanceNow(2 * time.Second)
		<-immediate
	}
	close(g.gate)
	await(t, s.Done(), "Immediate cleanup")
	<-answer
	k.checkClosedEach(t)
}
