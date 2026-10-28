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
func TestDoPPhase(t *testing.T) {
	for _, name := range []string{"partial1", "partial3", "eintr", "eagain", "buffer", "period", "ring_wrap", "occurrence", "gapless", "kernel_pause", "silence_pause", "reopen", "prefill", "tail", "cut", "cancel", "track_change"} {
		t.Run(name, func(t *testing.T) {
			if name == "kernel_pause" || name == "silence_pause" {
				testDoPPausePhase(t, name)
				return
			}
			k := dopKernel(t)
			c := dopConfig()
			n := 32
			switch name {
			case "buffer", "period", "ring_wrap":
				n = 100000
				c.RingBytes = 40000
			case "prefill", "reopen":
				c.Prefill = time.Microsecond
			case "tail", "track_change":
				c.Tail = time.Microsecond
			case "silence_pause":
				c.Pause = PauseZeros
				n = 40000
			}
			partial := name == "partial1" || name == "partial3" || name == "cancel" || name == "track_change"
			if partial {
				take := 1
				if name == "partial3" {
					take = 3
				}
				k.writes = []fakeWrite{{take: take}, {err: errnoEAGAIN}}
				k.polls = []fakePoll{{block: true}}
			}
			if name == "eintr" {
				k.writes = []fakeWrite{{err: errnoEINTR}, {take: 1}}
			}
			if name == "eagain" {
				k.writes = []fakeWrite{{err: errnoEAGAIN}, {take: 3}}
			}
			s := nativeSink(t, k, c)
			if name == "reopen" {
				if e := s.Activate(context.Background(), 1); e != nil {
					t.Fatal(e)
				}
				if e := s.Pause(context.Background()); e != nil {
					t.Fatal(e)
				}
				k.grant(31 * time.Second)
				nativeWait(t, s, "released pause", func() bool { return s.released })
				k.revoke()
				before := len(given(k))
				if before != 8 {
					t.Fatalf("outgoing run must have odd count: %d", before)
				}
				if e := s.Resume(context.Background()); e != nil {
					t.Fatal(e)
				}
				if e := s.Release(context.Background(), Immediate); e != nil {
					t.Fatal(e)
				}
				assertDoPMarkers(t, given(k)[before:], 5)
				if len(given(k)[before:]) != 8 {
					t.Fatal("reopen prefill missing")
				}
				return
			}
			l, r := bytes.Repeat([]byte{0x81}, n), bytes.Repeat([]byte{0x23}, n)
			if n <= 32 {
				if _, e := s.WriteDSD(context.Background(), 1, l, r, 0, n*8); e != nil {
					t.Fatal(e)
				}
				if name == "cut" {
					ok, e := s.Cut(17)
					if e != nil || !ok {
						t.Fatal(ok, e)
					}
					n = 3
				}
			}
			if e := s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			if partial {
				await(t, k.entered, "accepted prefix")
				if name == "cancel" || name == "track_change" {
					k.free()
					mode := Immediate
					if name == "track_change" {
						mode = TrackChange
					}
					if e := s.Release(context.Background(), mode); e != nil {
						t.Fatal(e)
					}
					want := 8
					if name == "track_change" {
						want += 8
					}
					if len(given(k)) != want {
						t.Fatalf("released suffix: %d want %d", len(given(k)), want)
					}
					assertDoPMarkers(t, given(k), 5)
					return
				}
				if e := s.Pause(context.Background()); e != nil {
					t.Fatal(e)
				}
				if e := s.Resume(context.Background()); e != nil {
					t.Fatal(e)
				}
			}
			if name == "kernel_pause" {
				await(t, k.when(filledAndIdle(16)), "content submitted")
				if e := s.Pause(context.Background()); e != nil {
					t.Fatal(e)
				}
				if e := s.Resume(context.Background()); e != nil {
					t.Fatal(e)
				}
			}
			k.free()
			if n > 32 {
				if _, e := s.WriteDSD(context.Background(), 1, l, r, 0, n*8); e != nil {
					t.Fatal(e)
				}
			}
			if name == "occurrence" || name == "gapless" {
				for i, bits := range []int{1, 7, 15} {
					if _, e := s.WriteDSD(context.Background(), uint64(i+2), l, r, 0, bits); e != nil {
						t.Fatal(e)
					}
				}
			}
			if e := s.Finish(context.Background()); e != nil {
				t.Fatal(e)
			}
			got := given(k)
			assertDoPMarkers(t, got, 5)
			if name == "cut" && (len(got) != 16 || got[10] != 0xe9 || got[14] != 0x69) {
				t.Fatalf("canonical cut lost bits: %x", got)
			}
			if partial {
				want := make([]byte, n*4)
				for i := 0; i < n/2; i++ {
					copy(want[i*8:], []byte{0, 0x81, 0x81, 5, 0, 0x23, 0x23, 5})
					if i%2 == 1 {
						want[i*8+3] = 0xfa
						want[i*8+7] = 0xfa
					}
				}
				if !bytes.Equal(got, want) {
					t.Fatal("resumed suffix changed payload/phase")
				}
			}
		})
	}
}
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
			assertDoPMarkers(t, got, 5)
			for i := 0; i < len(got); i += 4 {
				if got[i+1] != 0x69 || got[i+2] != 0x69 {
					t.Fatalf("silence payload at %d: %x", i, got[i:i+4])
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
func TestDoPTransportIdentity(t *testing.T) {
	t.Run("kind", func(t *testing.T) {
		a := Transport{Kind: format.KindDoP, RateHz: 176400, Channels: 2, Format: format.S32LE, ValidBits: 1, LogicalRate: 2822400, PackingVersion: 1}
		b := a
		b.Kind = format.KindPCM
		if a.Join(b).Gapless {
			t.Fatal("kind ignored")
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

func testDoPPausePhase(t *testing.T, name string) {
	k := dopKernel(t)
	c := dopConfig()
	if name == "silence_pause" {
		c.Pause = PauseZeros
	}
	s := nativeSink(t, k, c)
	l, r := bytes.Repeat([]byte{0x81}, 40000), bytes.Repeat([]byte{0x23}, 40000)
	if _, e := s.WriteDSD(context.Background(), 1, l, r, 0, len(l)*8); e != nil {
		t.Fatal(e)
	}
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	await(t, k.when(func(k *fakeKernel) bool { return k.state == stateRunning && starved(k) }), "full running buffer")
	if e := s.Pause(context.Background()); e != nil {
		t.Fatal(e)
	}
	before := s.Position().Written
	var total uint64
	k.inspect(func(k *fakeKernel) { total = k.total })
	if name == "silence_pause" {
		k.grant(2 * s.Params().PeriodTime())
		await(t, k.when(func(k *fakeKernel) bool { return k.total > total && starved(k) }), "pause silence accepted")
		if s.Position().Written != before {
			t.Fatal("silence counted as content")
		}
	} else {
		k.inspect(func(k *fakeKernel) {
			if k.state != statePaused {
				t.Error("kernel did not pause")
			}
		})
	}
	if e := s.Resume(context.Background()); e != nil {
		t.Fatal(e)
	}
	k.free()
	if e := s.Finish(context.Background()); e != nil {
		t.Fatal(e)
	}
	assertDoPMarkers(t, given(k), 5)
	if s.Position().Played != int64(len(l)*8) {
		t.Fatal("pause changed logical length")
	}
	all := given(k)
	content := 0
	for i := 0; i < len(all); i += 8 {
		b := all[i : i+8]
		if b[1] == 0x69 && b[2] == 0x69 && b[5] == 0x69 && b[6] == 0x69 {
			continue
		}
		if b[1] != 0x81 || b[2] != 0x81 || b[5] != 0x23 || b[6] != 0x23 {
			t.Fatal("pause changed content payload")
		}
		content++
	}
	if content != len(l)/2 {
		t.Fatalf("content frames=%d", content)
	}
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
