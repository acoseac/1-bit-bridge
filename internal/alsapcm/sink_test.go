package alsapcm

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// transport48k is what newModel's node takes: 48 kHz, 24 bits in three
// bytes, which is cfg48k.
var transport48k = Transport{Kind: format.KindPCM, RateHz: 48000, Channels: 2, Format: format.S243LE, ValidBits: 24}

// sinkConfig is a sink for transport48k on testEndpoint, every other value
// left to its default.
func sinkConfig() SinkConfig {
	return SinkConfig{Endpoint: testEndpoint, Transport: transport48k}
}

// openTestSink opens a sink through k on k's clock, and closes it when the
// test ends.
func openTestSink(t *testing.T, k *fakeKernel, cfg SinkConfig) *Sink {
	t.Helper()
	return openTestSinkWith(t, context.Background(), k, cfg, sinkSeams{})
}

// openTestSinkWith is openTestSink with a life context and the producer's
// hooks: hooks.open and hooks.now are k's.
func openTestSinkWith(t *testing.T, life context.Context, k *fakeKernel, cfg SinkConfig, hooks sinkSeams) *Sink {
	t.Helper()
	hooks.open = func(ep Endpoint, c Config) (*Device, error) { return open(k, k.now, ep, c) }
	hooks.now = k.now
	s, err := openSink(life, cfg, hooks)
	if err != nil {
		t.Fatalf("openSink: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// content returns n frames of S24_3LE stereo from frame from of an endless
// pattern. No byte is zero, so silence where music belongs shows; every
// frame differs from every other of the first 255^3, so a frame written
// twice or skipped shows; and track picks one of two patterns that share
// no frame, so a join shows where it is.
func content(track byte, from, n int) []byte {
	b := make([]byte, 0, n*6)
	for f := from; f < from+n; f++ {
		b = append(b, byte(1+f%255), byte(1+f/255%255), byte(1+f/65025%255), byte(1+f*7%255), 0x55+track, 0xaa-track)
	}
	return b
}

// zeros is n frames of S24_3LE stereo silence.
func zeros(n int64) []byte { return make([]byte, n*6) }

// join concatenates byte slices.
func join(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// submit writes b to s in pieces of the given frame counts, in turn, and
// fails the test unless every frame is taken.
func submit(t *testing.T, s *Sink, b []byte, pieces ...int) {
	t.Helper()
	if len(pieces) == 0 {
		pieces = []int{len(b) / 6}
	}
	for i := 0; len(b) > 0; i++ {
		n := min(pieces[i%len(pieces)]*6, len(b))
		got, err := s.Write(context.Background(), b[:n])
		if err != nil || got != n/6 {
			t.Fatalf("Write of %d frames: %d, %v", n/6, got, err)
		}
		b = b[n:]
	}
}

// sameFrames fails the test unless got is want, naming the first frame of
// S24_3LE stereo that differs.
func sameFrames(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	n := min(len(got), len(want)) / 6 * 6
	for i := 0; i < n; i += 6 {
		if !bytes.Equal(got[i:i+6], want[i:i+6]) {
			t.Errorf("%s: frame %d is % x, want % x (%d frames, want %d)", what, i/6, got[i:i+6], want[i:i+6], len(got)/6, len(want)/6)
			return
		}
	}
	t.Errorf("%s: %d frames, want %d; the first %d agree", what, len(got)/6, len(want)/6, n/6)
}

// aroundSilence fails the test unless got is before, then whole frames of
// silence, then after, and returns the frames of silence.
func aroundSilence(t *testing.T, what string, got, before, after []byte) int64 {
	t.Helper()
	if len(got) < len(before)+len(after) || !bytes.HasPrefix(got, before) || !bytes.HasSuffix(got, after) {
		sameFrames(t, what+" (before the silence)", got[:min(len(got), len(before))], before)
		t.Errorf("%s: %d frames are not the %d before the silence and the %d after it", what, len(got)/6, len(before)/6, len(after)/6)
		return 0
	}
	middle := got[len(before) : len(got)-len(after)]
	if len(middle)%6 != 0 || bytes.Count(middle, []byte{0}) != len(middle) {
		t.Errorf("%s: the %d bytes between the content are not whole frames of silence", what, len(middle))
	}
	return int64(len(middle) / 6)
}

// heard is what the fake DAC has consumed.
func heard(k *fakeKernel) []byte {
	var b []byte
	k.inspect(func(k *fakeKernel) { b = slices.Clone(k.heard) })
	return b
}

// given is what the fake kernel has taken from writes, every byte of
// every WRITEI_FRAMES.
func given(k *fakeKernel) []byte {
	var b []byte
	k.inspect(func(k *fakeKernel) { b = slices.Clone(k.played) })
	return b
}

// waitState waits, on the sink's own notifications, until it is in want,
// and returns its position then.
func waitState(t *testing.T, s *Sink, want SinkState) Position {
	t.Helper()
	for {
		ch := s.Changed()
		if p := s.Position(); p.State == want {
			return p
		}
		await(t, ch, "the sink to be "+want.String())
	}
}

// filledAndIdle is a condition on the fake: the writer has written appl
// frames since PREPARE, measured, and waits on the stream for more. The
// wait comes after the writer has used its measurement; a condition on the
// STATUS call itself holds inside the call, before the sink has applied
// the reading, so a test that read the position then could read the one
// before (#1303's Linux race).
func filledAndIdle(appl uint64) func(*fakeKernel) bool {
	return func(k *fakeKernel) bool { return k.appl == appl && starved(k) }
}

// drain drains s and fails the test when the drain does not end cleanly.
func drain(t *testing.T, s *Sink) {
	t.Helper()
	if err := s.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

// TestSinkGivesTheKernelEveryFrameOnceInOrder pins the path from producer
// to DAC under load: a producer writing pieces of every size, the store
// wrapping many times, the kernel taking partial writes, refusing with
// EAGAIN and interrupting with EINTR, and every frame reaching the DAC
// once, in order, with nothing written twice.
func TestSinkGivesTheKernelEveryFrameOnceInOrder(t *testing.T) {
	k := newModel(t)
	k.free()
	for i := range 60 {
		switch i % 4 {
		case 0:
			k.writes = append(k.writes, fakeWrite{take: 1})
		case 1:
			k.writes = append(k.writes, fakeWrite{err: errnoEINTR})
		case 2:
			k.writes = append(k.writes, fakeWrite{take: 37 * i})
		case 3:
			k.writes = append(k.writes, fakeWrite{take: -1})
		}
	}
	cfg := sinkConfig()
	cfg.Lookahead = 100 * time.Millisecond
	s := openTestSink(t, k, cfg)
	in := content(0, 0, 3*48000)
	submit(t, s, in, 1, 7, 1000, 4799, 9601, 14399)
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), in)
	sameFrames(t, "what the kernel was given", given(k), in)
	p := s.Position()
	if p.State != SinkStopped || p.Played != 3*48000 || p.Written != 3*48000 || p.Submitted != 3*48000 {
		t.Errorf("position %+v, want stopped with all %d frames played", p, 3*48000)
	}
	c := s.Stats().Counters
	if c.Partial == 0 || c.Interrupted == 0 || c.Waits == 0 {
		t.Errorf("counters %+v: the load was meant to give partial writes, EINTR and waits for room", c)
	}
	if k.callsOf(ioctlDrain) != 1 || k.callsOf(ioctlPrepare) != 1 || k.opens != 1 {
		t.Errorf("DRAIN %d, PREPARE %d, opens %d; want one of each", k.callsOf(ioctlDrain), k.callsOf(ioctlPrepare), k.opens)
	}
	k.checkClosedEach(t)
}

// TestSinkWriteAllocatesNothing pins the producer's path: frames are copied
// into the store preallocated at open, and a Write allocates nothing.
func TestSinkWriteAllocatesNothing(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	submit(t, s, content(0, 0, 12000))
	await(t, k.when(starved), "the writer to fill the hardware buffer and wait on the DAC")
	frame := content(0, 0, 1)
	ctx := context.Background()
	if allocs := testing.AllocsPerRun(1000, func() {
		if _, err := s.Write(ctx, frame); err != nil {
			t.Fatal(err)
		}
	}); allocs != 0 {
		t.Errorf("a Write allocated %v times", allocs)
	}
}

// TestSinkTakesNoFramesADrainHasFinished pins the producer's Write against
// a Drain that ends while it copies (#1303's review: the frames were
// accepted before the copy and published after it). Frames the producer
// has copied into the store but not yet published when the drain finishes
// are refused, not counted, and the Write says the sink has stopped; the
// drain played exactly what was published before it.
func TestSinkTakesNoFramesADrainHasFinished(t *testing.T) {
	k := newModel(t)
	k.free()
	var park atomic.Bool
	parked, release := make(chan struct{}), make(chan struct{})
	s := openTestSinkWith(t, context.Background(), k, sinkConfig(), sinkSeams{producerPublishes: func() {
		if park.CompareAndSwap(true, false) {
			close(parked)
			<-release
		}
	}})
	in := content(0, 0, 9600)
	submit(t, s, in)
	park.Store(true)
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := s.Write(context.Background(), content(0, 9600, 2400))
		done <- result{n, err}
	}()
	await(t, parked, "the producer to copy its frames and stop before it publishes them")
	drain(t, s)
	close(release)
	r := receive(t, done, "the producer's Write to end")
	if r.n != 0 || ReasonOf(r.err) != ReasonClosed {
		t.Errorf("a Write whose frames were copied as the drain finished: %d, %v; want none taken, the sink stopped", r.n, r.err)
	}
	if p := s.Position(); p.State != SinkStopped || p.Submitted != 9600 || p.Played != 9600 {
		t.Errorf("after the drain: %+v; want stopped, the 9600 frames published before it submitted and played", p)
	}
	sameFrames(t, "what the DAC heard", heard(k), in)
}

// TestSinkStoreIsTwoSecondsAheadAndCapped pins the store's size: two
// seconds of frames ahead of the hardware buffer by default, plus the
// hardware buffer's frames kept for a replay, preallocated at open; a
// longer lookahead at a high rate is cut to MaxRingBytes, never past it,
// and still holds a period beyond the frames kept.
func TestSinkStoreIsTwoSecondsAheadAndCapped(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	st := s.Stats()
	if st.LookaheadFrames != 96000 || st.KeptFrames != 9600 || st.RingFrames != 105600 || st.RingBytes != 105600*6 {
		t.Errorf("store %d frames (%d bytes): %d ahead, %d kept; want 105600 (633600): 96000 ahead, 9600 kept",
			st.RingFrames, st.RingBytes, st.LookaheadFrames, st.KeptFrames)
	}

	k = newModel(t)
	cfg := sinkConfig()
	cfg.Transport = Transport{Kind: format.KindPCM, RateHz: 384000, Channels: 2, Format: format.S32LE, ValidBits: 32}
	cfg.Lookahead = maxLookahead
	s = openTestSink(t, k, cfg)
	st = s.Stats()
	if st.RingBytes > MaxRingBytes || st.RingBytes != int(st.RingFrames)*8 {
		t.Errorf("a 30 s lookahead at 384 kHz: %d bytes for %d frames, want at most %d", st.RingBytes, st.RingFrames, MaxRingBytes)
	}
	if st.RingFrames != MaxRingBytes/8 || st.LookaheadFrames != st.RingFrames-st.KeptFrames {
		t.Errorf("capped store %d frames, %d ahead, %d kept", st.RingFrames, st.LookaheadFrames, st.KeptFrames)
	}
	if st.LookaheadFrames < int64(st.Params.PeriodFrames) {
		t.Errorf("the capped store holds %d frames ahead, less than a period", st.LookaheadFrames)
	}
}

// TestSinkJoinsCompatibleTracksBackToBack pins gapless playback: two tracks
// of one transport written one after the other, through partial writes and
// a store that wraps, reach the DAC as one stream with nothing between
// them: no drain, no reopen, no PREPARE, no silence.
func TestSinkJoinsCompatibleTracksBackToBack(t *testing.T) {
	k := newModel(t)
	k.free()
	k.writes = []fakeWrite{{take: 2399}, {take: 1}, {err: errnoEINTR}, {take: 17}, {take: -1}, {take: 5}}
	cfg := sinkConfig()
	cfg.Lookahead = 100 * time.Millisecond
	s := openTestSink(t, k, cfg)
	a, b := content(0, 0, 30011), content(1, 0, 25013)
	submit(t, s, a, 7, 4801, 13, 9600)
	if j := s.Next(transport48k); !j.Gapless || len(j.Differs) != 0 {
		t.Fatalf("Next(the sink's own transport) = %+v, want gapless", j)
	}
	submit(t, s, b, 4799, 1, 10007)
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), join(a, b))
	sameFrames(t, "what the kernel was given", given(k), join(a, b))
	if k.callsOf(ioctlDrain) != 1 || k.callsOf(ioctlPrepare) != 1 || k.opens != 1 || len(k.pauses) != 0 || s.Stats().Silence != 0 {
		t.Errorf("between two gapless tracks: DRAIN %d, PREPARE %d, opens %d, PAUSE %d, silence %d; want the open's and the end's alone",
			k.callsOf(ioctlDrain), k.callsOf(ioctlPrepare), k.opens, len(k.pauses), s.Stats().Silence)
	}
	k.checkClosedEach(t)
}

// TestTransportJoinNamesWhatDiffers pins the decision the sink exposes:
// the same transport joins gaplessly, and any difference, in the kind, the
// rate, the channels, the format or the valid bits packed, needs a drain,
// a reopen and a refill, and is named.
func TestTransportJoinNamesWhatDiffers(t *testing.T) {
	for _, c := range []struct {
		next Transport
		says string
	}{
		{Transport{Kind: format.KindDoP, RateHz: 48000, Channels: 2, Format: format.S243LE, ValidBits: 24}, "kind pcm → dop"},
		{Transport{Kind: format.KindPCM, RateHz: 96000, Channels: 2, Format: format.S243LE, ValidBits: 24}, "rate 48000 → 96000 Hz"},
		{Transport{Kind: format.KindPCM, RateHz: 48000, Channels: 1, Format: format.S243LE, ValidBits: 24}, "channels 2 → 1"},
		{Transport{Kind: format.KindPCM, RateHz: 48000, Channels: 2, Format: format.S32LE, ValidBits: 24}, "format S24_3LE → S32_LE"},
		{Transport{Kind: format.KindPCM, RateHz: 48000, Channels: 2, Format: format.S243LE, ValidBits: 16}, "valid bits 24 → 16"},
	} {
		j := transport48k.Join(c.next)
		if j.Gapless || len(j.Differs) != 1 || j.Differs[0] != c.says {
			t.Errorf("Join(%v) = %+v, want not gapless, differing in %q", c.next, j, c.says)
		}
	}
	s24in32 := Transport{Kind: format.KindPCM, RateHz: 44100, Channels: 2, Format: format.S32LE, ValidBits: 24}
	if j := s24in32.Join(Transport{Kind: format.KindPCM, RateHz: 96000, Channels: 2, Format: format.S32LE, ValidBits: 32}); j.Gapless || len(j.Differs) != 2 {
		t.Errorf("two differences: %+v", j)
	}
	if j := transport48k.Join(transport48k); !j.Gapless || len(j.Differs) != 0 {
		t.Errorf("the same transport: %+v, want gapless", j)
	}
}

// TestTransportJoinTellsSameCarrierPCMFromDoP: PCM in S32_LE at a DoP
// carrier's rate is never DoP's transport (B470 2d.2). The two differ in
// kind, valid bits, logical rate and packing version, and Join names each,
// so a sink of one is drained, closed and reopened for the other; a logical
// rate or a packing version alone tells two transports apart too.
func TestTransportJoinTellsSameCarrierPCMFromDoP(t *testing.T) {
	pcm := Transport{Kind: format.KindPCM, RateHz: 176400, Channels: 2, Format: format.S32LE, ValidBits: 32}
	dop := Transport{Kind: format.KindDoP, RateHz: 176400, Channels: 2, Format: format.S32LE, ValidBits: 1, LogicalRate: 2822400, PackingVersion: 1}
	want := []string{"kind pcm → dop", "valid bits 32 → 1", "logical rate", "packing version"}
	if j := pcm.Join(dop); j.Gapless || !slices.Equal(j.Differs, want) {
		t.Errorf("same-carrier PCM to DoP: %+v, want not gapless, differing in %q", j, want)
	}
	for _, c := range []struct {
		next Transport
		says string
	}{
		{Transport{Kind: format.KindDoP, RateHz: 176400, Channels: 2, Format: format.S32LE, ValidBits: 1, LogicalRate: 5644800, PackingVersion: 1}, "logical rate"},
		{Transport{Kind: format.KindDoP, RateHz: 176400, Channels: 2, Format: format.S32LE, ValidBits: 1, LogicalRate: 2822400, PackingVersion: 2}, "packing version"},
	} {
		if j := dop.Join(c.next); j.Gapless || len(j.Differs) != 1 || j.Differs[0] != c.says {
			t.Errorf("Join(%v) = %+v, want not gapless, differing in %q", c.next, j, c.says)
		}
	}
}

// TestSinkPositionIsWrittenMinusWhatTheKernelHolds pins the position
// against the kernel's account: before the stream starts nothing has
// played, although STATUS reports a delay of 0 then; while it runs, played
// is written minus STATUS's delay, the driver's own delay included; while
// it is paused, where STATUS's delay is 0 again, the pointers say what is
// held; and the position never goes back.
func TestSinkPositionIsWrittenMinusWhatTheKernelHolds(t *testing.T) {
	k := newModel(t)
	k.extra = 64
	s := openTestSink(t, k, sinkConfig())
	in := content(0, 0, 31000)
	submit(t, s, in[:1000*6])
	await(t, k.when(filledAndIdle(1000)), "the writer to write 1000 frames and measure")
	p := s.Position()
	if p.Stream != State(statePrepared) || p.Played != 0 || p.Written != 1000 || p.Submitted != 1000 || p.Queued != 1000 {
		t.Errorf("before the stream starts: %+v, want nothing played and 1000 frames queued", p)
	}
	submit(t, s, in[1000*6:])
	await(t, k.when(starved), "the writer to fill the hardware buffer")
	if p = s.Position(); p.Played != 0 {
		t.Errorf("a stream that has played nothing: played %d", p.Played)
	}
	k.play(t, 100*time.Millisecond)
	var appl, consumed uint64
	k.inspect(func(k *fakeKernel) { appl, consumed = k.appl, k.consumed })
	p = s.Position()
	// The writer waits inside a write the kernel has taken part of, which
	// it has not counted yet: its last measurement came after the write
	// before, so Written may trail the kernel's pointer, and Queued is
	// what the kernel held then.
	if p.Stream != State(stateRunning) || p.Played != int64(consumed)-64 || p.Queued != p.Written-int64(consumed)+64 || p.Written > int64(appl) {
		t.Errorf("running: %+v; want played %d (consumed less the driver's 64), queued %d", p, int64(consumed)-64, p.Written-int64(consumed)+64)
	}
	if p.RateHz != 48000 || p.Seconds() != float64(p.Played)/48000 {
		t.Errorf("rate %d, seconds %v", p.RateHz, p.Seconds())
	}
	if err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	p = s.Position()
	if p.Stream != State(statePaused) || p.Played != int64(consumed) || p.Queued != int64(appl-consumed) {
		t.Errorf("paused: %+v; want played %d and queued %d from the pointers", p, consumed, appl-consumed)
	}
	if err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	if got := s.Position().Played; got != int64(consumed) {
		t.Errorf("resumed: played %d, want %d: the position never goes back", got, consumed)
	}
	k.play(t, 50*time.Millisecond)
	k.inspect(func(k *fakeKernel) { consumed = k.consumed })
	if got := s.Position().Played; got != int64(consumed)-64 {
		t.Errorf("playing again: played %d, want %d", got, int64(consumed)-64)
	}
}

// TestSinkPositionIgnoresAReadingItCannotUse pins the clamps on the
// kernel's account, with STATUS scripted: a delay larger than what was
// written does not move the position back, and a negative one does not
// move it forward. Each look at the position waits for the writer's wait
// on the stream, which follows its use of the last reading (filledAndIdle).
func TestSinkPositionIgnoresAReadingItCannotUse(t *testing.T) {
	k := newFake(t)
	k.delay = 5000
	s := openTestSink(t, k, sinkConfig())
	submit(t, s, content(0, 0, 20000))
	await(t, k.when(func(k *fakeKernel) bool { return len(k.played) == 20000*6 && starved(k) }), "20000 frames written and measured")
	if p := s.Position(); p.Played != 15000 || p.Written != 20000 {
		t.Fatalf("position %+v, want 15000 played of 20000", p)
	}
	for _, delay := range []int64{50000, -100} {
		var written int
		k.inspect(func(k *fakeKernel) { k.delay, written = delay, len(k.played) })
		submit(t, s, content(0, 0, 2400))
		await(t, k.when(func(k *fakeKernel) bool { return len(k.played) == written+2400*6 && starved(k) }), "2400 more written and measured")
		if p := s.Position(); p.Played != 15000 {
			t.Errorf("a delay of %d: played %d, want 15000 kept", delay, p.Played)
		}
	}
}

// TestSinkRefusesWhatItCannotPlay pins the refusals at open: a transport
// that is not stereo PCM packed the way the format package packs it, a
// lookahead or release time out of bounds, a kernel pause asked of
// hardware that has none, and the device's own refusals, each typed, with
// nothing left open.
func TestSinkRefusesWhatItCannotPlay(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(*fakeKernel, *SinkConfig)
		want Reason
	}{
		{"native DSD", func(_ *fakeKernel, c *SinkConfig) { c.Transport.Kind = format.KindNativeDSD }, ReasonBadConfig},
		{"mono", func(_ *fakeKernel, c *SinkConfig) { c.Transport.Channels = 1 }, ReasonBadConfig},
		{"16 bits in S24_3LE", func(_ *fakeKernel, c *SinkConfig) { c.Transport.ValidBits = 16 }, ReasonBadConfig},
		{"32 bits in S24_LE", func(_ *fakeKernel, c *SinkConfig) { c.Transport.Format, c.Transport.ValidBits = format.S24LE, 32 }, ReasonBadConfig},
		{"lookahead too short", func(_ *fakeKernel, c *SinkConfig) { c.Lookahead = 10 * time.Millisecond }, ReasonBadConfig},
		{"lookahead too long", func(_ *fakeKernel, c *SinkConfig) { c.Lookahead = time.Minute }, ReasonBadConfig},
		{"release too soon", func(_ *fakeKernel, c *SinkConfig) { c.ReleaseAfter = time.Millisecond }, ReasonBadConfig},
		{"release too late", func(_ *fakeKernel, c *SinkConfig) { c.ReleaseAfter = time.Hour }, ReasonBadConfig},
		{"an unknown pause mode", func(_ *fakeKernel, c *SinkConfig) { c.Pause = PauseMode(9) }, ReasonBadConfig},
		{"PAUSE without the hardware's", func(k *fakeKernel, c *SinkConfig) { k.hw.info &^= infoPause; c.Pause = PauseKernel }, ReasonNotSupported},
		{"busy", func(k *fakeKernel, _ *SinkConfig) { k.openErr = errnoEBUSY }, ReasonBusy},
		{"a rate the DAC lacks", func(_ *fakeKernel, c *SinkConfig) { c.Transport.RateHz = 22050 }, ReasonNotSupported},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newModel(t)
			cfg := sinkConfig()
			c.set(k, &cfg)
			s, err := openSink(context.Background(), cfg, sinkSeams{
				open: func(ep Endpoint, c Config) (*Device, error) { return open(k, k.now, ep, c) },
				now:  k.now,
			})
			if s != nil {
				_ = s.Close()
				t.Fatalf("opened a sink for %+v", cfg)
			}
			if ReasonOf(err) != c.want {
				t.Errorf("got %v, want %s", err, c.want)
			}
			k.checkClosedEach(t)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	k := newModel(t)
	if _, err := openSink(ctx, sinkConfig(), sinkSeams{
		open: func(ep Endpoint, c Config) (*Device, error) { return open(k, k.now, ep, c) },
		now:  k.now,
	}); ReasonOf(err) != ReasonCancelled || len(k.opened) != 0 {
		t.Errorf("a life already over: %v, %d opens; want cancelled before the open", err, len(k.opened))
	}
}

// TestSinkStateNames pins the names diagnostics print.
func TestSinkStateNames(t *testing.T) {
	var names []string
	for st := SinkPlaying; st <= SinkEnded; st++ {
		names = append(names, st.String())
	}
	if got := strings.Join(names, " "); got != "playing paused released xrun draining stopped ended" {
		t.Errorf("states %q", got)
	}
	if PauseAuto.String() != "auto" || PauseKernel.String() != "pause" || PauseZeros.String() != "zeros" {
		t.Errorf("pause modes %s %s %s", PauseAuto, PauseKernel, PauseZeros)
	}
}
