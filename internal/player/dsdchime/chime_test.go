package dsdchime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// chimeRates are the rates the chime is made at.
var chimeRates = []int{format.DSD64RateHz, 2 * format.DSD64RateHz, 4 * format.DSD64RateHz}

func rateName(r int) string { return fmt.Sprintf("DSD%d", r/44100) }

// goldens are each rate's chime, its left plane then its right, hashed by
// SHA-256 on linux/amd64 on 2026-11-21; TestTheChimeIsTheSameOnEveryArchitecture
// holds every other architecture to them.
var goldens = map[int]string{
	format.DSD64RateHz:     "b8147b70c856a8d89390cff4769afef63fea95f233c673f74454c554345a4736",
	2 * format.DSD64RateHz: "4f185fcabc79fb287baddecc047dcc0c76bdf4e21fe012543240b63b723e807e",
	4 * format.DSD64RateHz: "306d45e3967e47e51c71f6da579d76977f89a428f69bac3c1cb8bbf4d0a1c314",
}

func digest(c Chime) string {
	h := sha256.New()
	h.Write(c.planes[0])
	h.Write(c.planes[1])
	return hex.EncodeToString(h.Sum(nil))
}

// unpack is p's bits, one a byte, 0 or 1, bit 7 earliest.
func unpack(p []byte) []byte {
	bits := make([]byte, 8*len(p))
	for i := range bits {
		bits[i] = p[i>>3] >> (7 - i&7) & 1
	}
	return bits
}

// TestTheChimeLoopIsTheGenerators holds the chime's loop to the generator
// it ports, modulate() of internal/transcode/testdata/gen/dsd_fixtures.py,
// through an input with no libm in it: the generator's own digest of one
// second at DSD64 of its rotation (cos and sin of 2π·1000/2822400), which
// internal/player/dsdconv's TestTheModulatorIsThePythonGenerators also
// holds its test-side copy to. The generator's input is the rotation before
// its FULL_SCALE, 0.5; the loop's is in the one-bit range, so the adapter is
// a tone of peak 0.5 with no edges, and the product is exact either way.
// The loop's packed bytes are unpacked to the generator's one byte a
// sample before they are hashed.
func TestTheChimeLoopIsTheGenerators(t *testing.T) {
	const pythonDigest = "c164f2ef37c9eb01d8c07404b4e81f3297f80e3009f0e7e13e137dbd4f804154"
	packed := make([]byte, format.DSD64RateHz/8)
	rotation := tone{amp: 0.5, c: 0x1.ffffacda93a18p-1, s: 0x1.23ca52113ba91p-9, limit: bound, n: format.DSD64RateHz}
	if err := rotation.modulate(context.Background(), packed); err != nil {
		t.Fatal(err)
	}
	bits := unpack(packed)
	ones := bytes.Count(bits, []byte{1})
	sum := sha256.Sum256(bits)
	if got := hex.EncodeToString(sum[:]); got != pythonDigest {
		t.Fatalf("the loop's one-bit samples hash to %s, the Python generator's to %s (%d ones of %d)", got, pythonDigest, ones, len(bits))
	}
	if ones != 1411200 {
		t.Errorf("%d ones, the Python run had 1411200", ones)
	}
}

// pads are where a channel's chime is 0x69, in frames of 1/44,100 s: the
// left before its tone and after it, the right before its tone and after
// it.
var pads = [2][2][2]int{
	{{0, padFrames}, {padFrames + sectionFrames, chimeFrames}},
	{{0, padFrames + sectionFrames}, {padFrames + 2*sectionFrames, chimeFrames}},
}

// TestTheChimeHasItsShapeAtEveryRate: at each rate Get's chime is 2.4 s of
// stereo native DSD at that rate, in whole bytes; its pads, and each tone's
// other channel, are 0x69 and nothing else; each tone's section is
// modulated, its 10 ms before the tone the loop's own silence from rest;
// and a rate the chime is not made at is refused.
func TestTheChimeHasItsShapeAtEveryRate(t *testing.T) {
	for _, rate := range chimeRates {
		c, err := Get(context.Background(), rate)
		if err != nil {
			t.Fatalf("%s: %v", rateName(rate), err)
		}
		f := c.Format()
		want := format.NativeDSD{BitRateHz: rate, Channels: 2, Frames: int64(rate) * 24 / 10}
		if f != want {
			t.Errorf("%s: Format is %+v, want %+v", rateName(rate), f, want)
		}
		if err := f.Validate(); err != nil {
			t.Errorf("%s: %v", rateName(rate), err)
		}
		bpf := rate / 44100 / 8
		for ch := range c.planes {
			p := c.planes[ch]
			if len(p) != chimeFrames*bpf {
				t.Fatalf("%s channel %d: %d bytes, want %d", rateName(rate), ch, len(p), chimeFrames*bpf)
			}
			for _, pad := range pads[ch] {
				for i := pad[0] * bpf; i < pad[1]*bpf; i++ {
					if p[i] != 0x69 {
						t.Fatalf("%s channel %d: pad byte %d is %#02x, not 0x69", rateName(rate), ch, i, p[i])
					}
				}
			}
			at := (padFrames + ch*sectionFrames) * bpf
			section := p[at : at+sectionFrames*bpf]
			if n := bytes.Count(section, []byte{0x69}); n > len(section)/8 {
				t.Errorf("%s channel %d: %d of the section's %d bytes are 0x69: not modulated", rateName(rate), ch, n, len(section))
			}
			// From rest, the loop with no input makes 96 69 69 96 over and
			// over, and the 10 ms before each tone are that.
			lead := section[:leadFrames*bpf]
			if !bytes.Equal(lead, bytes.Repeat([]byte{0x96, 0x69, 0x69, 0x96}, len(lead)/4)) {
				t.Errorf("%s channel %d: the lead-in is not the loop's silence from rest; it begins % x", rateName(rate), ch, lead[:16])
			}
		}
	}
	for _, rate := range []int{8 * format.DSD64RateHz, 3072000, 0, -format.DSD64RateHz} {
		if _, err := Get(context.Background(), rate); !errors.Is(err, errRate) {
			t.Errorf("Get(%d): %v, want errRate", rate, err)
		}
	}
}

// bigPi is π to prec bits, by Machin's formula, 16·atan(1/5) − 4·atan(1/239).
func bigPi(prec uint) *big.Float {
	atanInv := func(n int64) *big.Float {
		x := new(big.Float).SetPrec(prec).Quo(new(big.Float).SetPrec(prec).SetInt64(1), new(big.Float).SetPrec(prec).SetInt64(n))
		x2 := new(big.Float).SetPrec(prec).Mul(x, x)
		sum := new(big.Float).SetPrec(prec).Set(x)
		term := new(big.Float).SetPrec(prec).Set(x)
		for k := int64(1); k < 2*int64(prec); k++ {
			term.Mul(term, x2)
			q := new(big.Float).SetPrec(prec).Quo(term, new(big.Float).SetPrec(prec).SetInt64(2*k+1))
			if k%2 == 1 {
				sum.Sub(sum, q)
			} else {
				sum.Add(sum, q)
			}
		}
		return sum
	}
	a := new(big.Float).SetPrec(prec).Mul(new(big.Float).SetPrec(prec).SetInt64(16), atanInv(5))
	return a.Sub(a, new(big.Float).SetPrec(prec).Mul(new(big.Float).SetPrec(prec).SetInt64(4), atanInv(239)))
}

// bigCosSin is the cosine and sine of x, a small angle, by their series.
func bigCosSin(x *big.Float) (float64, float64) {
	prec := x.Prec()
	c := new(big.Float).SetPrec(prec).SetInt64(1)
	s := new(big.Float).SetPrec(prec).Set(x)
	x2 := new(big.Float).SetPrec(prec).Mul(x, x)
	tc := new(big.Float).SetPrec(prec).SetInt64(1)
	ts := new(big.Float).SetPrec(prec).Set(x)
	for k := int64(1); k < 60; k++ {
		tc.Mul(tc, x2)
		tc.Quo(tc, new(big.Float).SetPrec(prec).SetInt64((2*k-1)*(2*k)))
		ts.Mul(ts, x2)
		ts.Quo(ts, new(big.Float).SetPrec(prec).SetInt64((2*k)*(2*k+1)))
		if k%2 == 1 {
			c.Sub(c, tc)
			s.Sub(s, ts)
		} else {
			c.Add(c, tc)
			s.Add(s, ts)
		}
	}
	cf, _ := c.Float64()
	sf, _ := s.Float64()
	return cf, sf
}

// TestTheChimeIsTheSameOnEveryArchitecture: every bit of the chime is the
// same wherever it is built. Its constants are the float64s nearest their
// values, computed here in 256-bit arithmetic with no libm (big.Float's
// Float64 rounds to nearest), so none depends on a platform's sin or cos;
// its arithmetic rounds each product alone; and each rate's chime hashes to
// its golden, which every leg of CI builds. It also measures the rotations'
// drift: the radius of DSD256's rotations over a section's 7,902,720 steps.
func TestTheChimeIsTheSameOnEveryArchitecture(t *testing.T) {
	const prec = 256
	pi := bigPi(prec)
	ten := new(big.Float).SetPrec(prec).SetInt64(10)
	tenSqrtTen := new(big.Float).SetPrec(prec).Mul(ten, new(big.Float).SetPrec(prec).Sqrt(ten))
	lev, _ := new(big.Float).SetPrec(prec).Quo(new(big.Float).SetPrec(prec).SetFloat64(0.5), tenSqrtTen).Float64()
	if level != lev {
		t.Errorf("level is %s, and 0.5 × 10^(−30/20) is nearest %s", strconv.FormatFloat(level, 'x', -1, 64), strconv.FormatFloat(lev, 'x', -1, 64))
	}
	for i, rate := range chimeRates {
		angle := func(num, den int64) *big.Float {
			x := new(big.Float).SetPrec(prec).Mul(pi, new(big.Float).SetPrec(prec).SetInt64(num))
			return x.Quo(x, new(big.Float).SetPrec(prec).SetInt64(den))
		}
		for ch, f := range []int64{880, 1320} {
			c, s := bigCosSin(angle(2*f, int64(rate)))
			if got := rates[i].tone[ch]; got != [2]float64{c, s} {
				t.Errorf("%s %d Hz: the rotation is (%x, %x), and the nearest is (%x, %x)", rateName(rate), f, got[0], got[1], c, s)
			}
		}
		c, s := bigCosSin(angle(1, int64(rampFrames*(rate/44100))))
		if got := rates[i].ramp; got != [2]float64{c, s} {
			t.Errorf("%s: the edges' rotation is (%x, %x), and the nearest is (%x, %x)", rateName(rate), got[0], got[1], c, s)
		}
	}
	for _, rate := range chimeRates {
		c, err := Get(context.Background(), rate)
		if err != nil {
			t.Fatalf("%s: %v", rateName(rate), err)
		}
		if got := digest(c); got != goldens[rate] {
			t.Errorf("%s: the chime hashes to %s, its golden is %s (%s/%s)", rateName(rate), got, goldens[rate], runtime.GOOS, runtime.GOARCH)
		}
	}
	// The drift, by the loop's own recurrence, over a whole section's
	// samples at DSD256, more than either tone takes.
	for ch, f := range toneHz {
		c, s := rates[2].tone[ch][0], rates[2].tone[ch][1]
		y, x := 0.0, 1.0
		var worst float64
		steps := sectionFrames * 4 * format.DSD64RateHz / 44100
		for range steps {
			y, x = float64(y*c)+float64(x*s), float64(x*c)-float64(y*s)
			worst = math.Max(worst, math.Abs(math.Sqrt(x*x+y*y)-1))
		}
		t.Logf("DSD256 %.0f Hz: over %d steps the rotation's radius strays from 1 by %.3g at most, %.3g dB", f, steps, worst, 20*math.Log10(1+worst))
		if worst > 1e-9 {
			t.Errorf("DSD256 %.0f Hz: the rotation's radius strays by %.3g", f, worst)
		}
	}
}

// askCounter is a context whose Err counts its asks and answers
// context.Canceled from its after'th on (never when after is 0).
type askCounter struct {
	context.Context
	asks, after int
}

func (c *askCounter) Err() error {
	c.asks++
	if c.after > 0 && c.asks >= c.after {
		return context.Canceled
	}
	return nil
}

// TestTheChimeStopsWithItsContext: a build asks its context every 65,536
// samples of each tone's section, from its first, and a build whose
// context ends stops at its next ask with the context's error; through
// Get, it publishes nothing, and the next caller builds again.
func TestTheChimeStopsWithItsContext(t *testing.T) {
	for _, rate := range chimeRates {
		ctx := &askCounter{Context: context.Background()}
		if _, err := build(ctx, rate); err != nil {
			t.Fatalf("%s: %v", rateName(rate), err)
		}
		samples := 8 * sectionFrames * (rate / 44100 / 8)
		if want := 2 * ((samples + 0xFFFF) / 0x10000); ctx.asks != want {
			t.Errorf("%s: a build asked its context %d times, every 65,536 samples is %d", rateName(rate), ctx.asks, want)
		}
		ctx = &askCounter{Context: context.Background(), after: 5}
		if c, err := build(ctx, rate); !errors.Is(err, context.Canceled) || c != nil {
			t.Errorf("%s: a build whose context ended at its fifth ask answered %v, %v", rateName(rate), c, err)
		}
		if ctx.asks != 5 {
			t.Errorf("%s: a build whose context ended at its fifth ask asked %d times", rateName(rate), ctx.asks)
		}
	}
	builds := 0
	cc := &cache{build: func(ctx context.Context, rate int) (*Chime, error) {
		builds++
		return build(ctx, rate)
	}}
	if _, err := cc.get(&askCounter{Context: context.Background(), after: 3}, format.DSD64RateHz); !errors.Is(err, context.Canceled) {
		t.Fatalf("a build whose context ended answered %v", err)
	}
	if cc.slots[0].chime != nil {
		t.Fatal("a build whose context ended published")
	}
	c, err := cc.get(context.Background(), format.DSD64RateHz)
	if err != nil || builds != 2 || c == nil || digest(*c) != goldens[format.DSD64RateHz] {
		t.Fatalf("after a build its context ended, the next caller has %v and %d builds", err, builds)
	}
}

// TestTheLoopRefusesToLeaveItsBounds: an input the loop cannot hold, a sine
// of 0.9 of the one-bit range, ends the modulation with errBounds as soon as
// an integrator passes the bound; a sine of 0.8 is held with every
// integrator under 0.7, as bound's comment says; and so is the chime, at
// its level, at every rate (the state bounded at the chime's level).
func TestTheLoopRefusesToLeaveItsBounds(t *testing.T) {
	dst := make([]byte, format.DSD64RateHz/8)
	sine := func(amp, limit float64) tone {
		return tone{amp: amp, c: 0x1.ffffacda93a18p-1, s: 0x1.23ca52113ba91p-9, limit: limit, n: format.DSD64RateHz}
	}
	if err := sine(0.9, bound).modulate(context.Background(), dst); !errors.Is(err, errBounds) {
		t.Errorf("a sine of 0.9 answered %v, want errBounds", err)
	}
	if err := sine(0.8, 0.7).modulate(context.Background(), dst); err != nil {
		t.Errorf("a sine of 0.8, its integrators held under 0.7: %v", err)
	}
	for i, rate := range chimeRates {
		for ch := range 2 {
			tn := toneOf(i, ch, rates[i].tail[ch])
			tn.limit = 0.7
			if err := tn.modulate(context.Background(), make([]byte, sectionFrames*(rate/44100/8))); err != nil {
				t.Errorf("%s channel %d, its integrators held under 0.7: %v", rateName(rate), ch, err)
			}
		}
	}
}

// TestTheChimeIsPacked: a build allocates its two planes, eight samples a
// byte, and little else: one uncached build's bytes are at most the planes'
// size and 64 KiB.
func TestTheChimeIsPacked(t *testing.T) {
	for _, rate := range chimeRates {
		planes := uint64(2 * chimeFrames * (rate / 44100 / 8))
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		c, err := build(context.Background(), rate)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatalf("%s: %v", rateName(rate), err)
		}
		got := after.TotalAlloc - before.TotalAlloc
		t.Logf("%s: a build allocated %d bytes, its planes %d", rateName(rate), got, planes)
		if got > planes+64<<10 {
			t.Errorf("%s: a build allocated %d bytes, past its planes' %d and 64 KiB", rateName(rate), got, planes)
		}
		if n := uint64(len(c.planes[0]) + len(c.planes[1])); n != planes {
			t.Errorf("%s: the planes hold %d bytes, 2.4 s of stereo is %d", rateName(rate), n, planes)
		}
	}
}

// stubChime is a chime a stand-in build hands out, cheap to make.
func stubChime(rate int) *Chime { return &Chime{bitRate: rate, planes: [2][]byte{{0x69}, {0x69}}} }

// gated is a stand-in build that counts its calls and holds each until the
// test lets it go, answering then what the test says; the test's cleanup
// lets every build still held go.
type gated struct {
	mu      sync.Mutex
	builds  int
	entered chan struct{}
	release chan error
}

func newGated(t *testing.T) *gated {
	g := &gated{entered: make(chan struct{}, 8), release: make(chan error, 8)}
	t.Cleanup(func() { close(g.release) })
	return g
}

func (g *gated) build(ctx context.Context, rate int) (*Chime, error) {
	g.mu.Lock()
	g.builds++
	g.mu.Unlock()
	g.entered <- struct{}{}
	if err := <-g.release; err != nil {
		return nil, err
	}
	return stubChime(rate), nil
}

func (g *gated) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.builds
}

// within waits for ch, failing the test after a few seconds.
func within(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s within 5 s", what)
	}
}

// result is one Get's answer.
type result struct {
	c   *Chime
	err error
}

// TestConcurrentFirstCallersShareOneBuild: callers of one rate that come
// while its first build is under way wait for it, and every one of them
// has the chime that build made: one build, one publication, and the next
// caller is answered from the cache.
func TestConcurrentFirstCallersShareOneBuild(t *testing.T) {
	g := newGated(t)
	waiting := make(chan struct{}, 8)
	cc := &cache{build: g.build, waiting: func() { waiting <- struct{}{} }}
	const callers = 5
	answers := make(chan result, callers)
	call := func() {
		c, err := cc.get(context.Background(), format.DSD64RateHz)
		answers <- result{c, err}
	}
	go call()
	within(t, g.entered, "build")
	for range callers - 1 {
		go call()
	}
	for range callers - 1 {
		within(t, waiting, "caller waiting for the build")
	}
	g.release <- nil
	var first *Chime
	for range callers {
		var a result
		select {
		case a = <-answers:
		case <-time.After(5 * time.Second):
			t.Fatal("a caller had no answer within 5 s")
		}
		if a.err != nil || a.c == nil {
			t.Fatalf("a caller had %v, %v", a.c, a.err)
		}
		if first == nil {
			first = a.c
		} else if a.c != first {
			t.Errorf("two callers had two chimes")
		}
	}
	if c, err := cc.get(context.Background(), format.DSD64RateHz); err != nil || c != first {
		t.Errorf("the next caller had %v, %v, not the chime published", c, err)
	}
	if n := g.count(); n != 1 {
		t.Errorf("%d builds for one rate, want 1", n)
	}
}

// TestAWaitersCancellationEndsOnlyItsWait: a caller waiting for another's
// build whose own context ends has its context's error at once, while the
// build goes on, and the caller that began it, and every other waiter,
// still has its chime.
func TestAWaitersCancellationEndsOnlyItsWait(t *testing.T) {
	g := newGated(t)
	waiting := make(chan struct{}, 8)
	cc := &cache{build: g.build, waiting: func() { waiting <- struct{}{} }}
	builder, other, quitter := make(chan result, 1), make(chan result, 1), make(chan result, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { c, err := cc.get(context.Background(), format.DSD64RateHz); builder <- result{c, err} }()
	within(t, g.entered, "build")
	go func() { c, err := cc.get(context.Background(), format.DSD64RateHz); other <- result{c, err} }()
	go func() { c, err := cc.get(ctx, format.DSD64RateHz); quitter <- result{c, err} }()
	within(t, waiting, "first waiter")
	within(t, waiting, "second waiter")
	cancel()
	select {
	case a := <-quitter:
		if !errors.Is(a.err, context.Canceled) || a.c != nil {
			t.Errorf("the waiter whose context ended had %v, %v", a.c, a.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter whose context ended had no answer while the build was held")
	}
	g.release <- nil
	for name, ch := range map[string]chan result{"the caller that began the build": builder, "the other waiter": other} {
		select {
		case a := <-ch:
			if a.err != nil || a.c == nil {
				t.Errorf("%s had %v, %v", name, a.c, a.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s had no answer", name)
		}
	}
	if n := g.count(); n != 1 {
		t.Errorf("%d builds, want 1", n)
	}
}

// TestACancelledBuildPublishesNothing: a build whose caller's context ends
// publishes nothing, a caller that was waiting for it then builds again
// and has its chime, and so does the cache's next caller, with no third
// build.
func TestACancelledBuildPublishesNothing(t *testing.T) {
	g := newGated(t)
	waiting := make(chan struct{}, 8)
	cc := &cache{build: g.build, waiting: func() { waiting <- struct{}{} }}
	builder, waiter := make(chan result, 1), make(chan result, 1)
	go func() { c, err := cc.get(context.Background(), format.DSD64RateHz); builder <- result{c, err} }()
	within(t, g.entered, "build")
	go func() { c, err := cc.get(context.Background(), format.DSD64RateHz); waiter <- result{c, err} }()
	within(t, waiting, "waiter")
	g.release <- context.Canceled
	if a := <-builder; !errors.Is(a.err, context.Canceled) || a.c != nil {
		t.Errorf("the caller whose build was cancelled had %v, %v", a.c, a.err)
	}
	within(t, g.entered, "second build")
	g.release <- nil
	a := <-waiter
	if a.err != nil || a.c == nil {
		t.Fatalf("the waiter had %v, %v after the build it waited for was cancelled", a.c, a.err)
	}
	if c, err := cc.get(context.Background(), format.DSD64RateHz); err != nil || c != a.c {
		t.Errorf("the next caller had %v, %v", c, err)
	}
	if n := g.count(); n != 2 {
		t.Errorf("%d builds, want 2", n)
	}
}

// TestACancellationBeforePublicationPublishesNothing: a build whose
// caller's context ends after the build's last ask of it, and before it is
// published, publishes nothing; the caller has its context's error, and
// the next caller builds again.
func TestACancellationBeforePublicationPublishesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	builds := 0
	cc := &cache{build: func(context.Context, int) (*Chime, error) {
		builds++
		if builds == 1 {
			cancel()
		}
		return stubChime(format.DSD64RateHz), nil
	}}
	if c, err := cc.get(ctx, format.DSD64RateHz); !errors.Is(err, context.Canceled) || c != nil {
		t.Errorf("a caller whose context ended before publication had %v, %v", c, err)
	}
	if cc.slots[0].chime != nil {
		t.Error("a build whose context ended before publication was published")
	}
	if c, err := cc.get(context.Background(), format.DSD64RateHz); err != nil || c == nil || builds != 2 {
		t.Errorf("the next caller had %v, %v after %d builds", c, err, builds)
	}
}

// TestNoCallerCanChangeTheChime: what a caller reads is a copy, so writing
// into its buffer after a read leaves the cache's bytes, and the golden, as
// they were; and ReadAt reads at an offset and stops at the end.
func TestNoCallerCanChangeTheChime(t *testing.T) {
	c, err := Get(context.Background(), format.DSD64RateHz)
	if err != nil {
		t.Fatal(err)
	}
	size := len(c.planes[0])
	got := [][]byte{make([]byte, size), make([]byte, size)}
	if n, err := c.ReadAt(got, 0); n != size || err != nil {
		t.Fatalf("a read of the whole chime answered %d, %v", n, err)
	}
	if !bytes.Equal(got[0], c.planes[0]) || !bytes.Equal(got[1], c.planes[1]) {
		t.Fatal("a read is not the chime")
	}
	for _, p := range got {
		for i := range p {
			p[i] ^= 0xFF
		}
	}
	if d := digest(c); d != goldens[format.DSD64RateHz] {
		t.Errorf("after a caller wrote into what it read, the chime hashes to %s, its golden is %s", d, goldens[format.DSD64RateHz])
	}
	tail := [][]byte{make([]byte, 100), make([]byte, 100)}
	if n, err := c.ReadAt(tail, int64(size-40)); n != 40 || !errors.Is(err, io.EOF) || !bytes.Equal(tail[1][:40], c.planes[1][size-40:]) {
		t.Errorf("a read past the end answered %d, %v", n, err)
	}
	if n, err := c.ReadAt(tail, int64(size)); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("a read at the end answered %d, %v", n, err)
	}
	if _, err := c.ReadAt([][]byte{make([]byte, 4), make([]byte, 5)}, 0); err == nil {
		t.Error("a read into planes of two lengths was answered")
	}
	if _, err := c.ReadAt(tail, -1); err == nil {
		t.Error("a read at -1 was answered")
	}
}

// TestEachCallerHasAChimeOfItsOwn: a caller that writes over the chime Get
// answered it, its rate and its planes, leaves the next caller's as it was.
// The case puts back what it wrote in its cleanup, which, where Get answers
// the chime the cache keeps, puts the cache's back.
func TestEachCallerHasAChimeOfItsOwn(t *testing.T) {
	c, err := Get(context.Background(), format.DSD64RateHz)
	if err != nil {
		t.Fatal(err)
	}
	rate, planes := c.bitRate, c.planes
	t.Cleanup(func() { c.bitRate, c.planes = rate, planes })
	c.bitRate, c.planes = 0, [2][]byte{}
	next, err := Get(context.Background(), format.DSD64RateHz)
	if err != nil {
		t.Fatal(err)
	}
	want := format.NativeDSD{BitRateHz: format.DSD64RateHz, Channels: 2, Frames: int64(format.DSD64RateHz) * 24 / 10}
	if f := next.Format(); f != want {
		t.Errorf("after a caller wrote over its chime, the next caller's is %+v, want %+v", f, want)
	}
}

// coldCache stands a fresh cache, whose build is b, in for the package's,
// which Get asks, and puts the package's back in the test's cleanup.
func coldCache(t *testing.T, b func(context.Context, int) (*Chime, error)) {
	t.Helper()
	kept := chimes
	chimes = &cache{build: b}
	t.Cleanup(func() { chimes = kept })
}

// TestGetOnAnEndedContextIsTheChimeKeptOrNothing: through Get, a caller
// whose context has ended is answered, on a cold cache, its context's error
// and the zero Chime, the cache keeping nothing, so the next caller builds;
// once a rate's chime is kept, such a caller is answered that chime.
func TestGetOnAnEndedContextIsTheChimeKeptOrNothing(t *testing.T) {
	coldCache(t, build)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	c, err := Get(ended, format.DSD64RateHz)
	if !errors.Is(err, context.Canceled) || c.bitRate != 0 || c.planes[0] != nil || c.planes[1] != nil {
		t.Errorf("Get on a cold cache with its context ended answered %+v, %v; want the zero Chime and context.Canceled", c.Format(), err)
	}
	if s := chimes.slots[0]; s.chime != nil || s.flight != nil {
		t.Fatal("Get on a cold cache with its context ended left a chime or a build in the cache")
	}
	if c, err := Get(context.Background(), format.DSD64RateHz); err != nil || digest(c) != goldens[format.DSD64RateHz] {
		t.Fatalf("the next caller had %v and a chime hashing to %s", err, digest(c))
	}
	c, err = Get(ended, format.DSD64RateHz)
	if err != nil || digest(c) != goldens[format.DSD64RateHz] {
		t.Errorf("a caller whose context has ended, the chime kept, had %v and a chime hashing to %s; want the chime kept", err, digest(c))
	}
}

// TestGetAnswersNoChimeWhereItsBuildMadeNone: a build that answers neither a
// chime nor an error, which build never does, leaves Get's caller the zero
// Chime and errNoChime, and Get dereferences nothing.
func TestGetAnswersNoChimeWhereItsBuildMadeNone(t *testing.T) {
	coldCache(t, func(context.Context, int) (*Chime, error) { return nil, nil })
	c, err := Get(context.Background(), format.DSD64RateHz)
	if !errors.Is(err, errNoChime) || c.bitRate != 0 || c.planes[0] != nil || c.planes[1] != nil {
		t.Errorf("Get over a build that made no chime answered %+v, %v; want the zero Chime and errNoChime", c.Format(), err)
	}
}

// BenchmarkChimeDSD64, DSD128 and DSD256 time a chime's build and Get's
// answer from the cache. build is a whole build each iteration: run alone
// in a fresh process with -benchtime 1x it is that process's first build
// (cold), and its later iterations in one process are rebuilds (warm).
// cached is Get on a warm cache, which allocates nothing.
func BenchmarkChimeDSD64(b *testing.B)  { benchChime(b, format.DSD64RateHz) }
func BenchmarkChimeDSD128(b *testing.B) { benchChime(b, 2*format.DSD64RateHz) }
func BenchmarkChimeDSD256(b *testing.B) { benchChime(b, 4*format.DSD64RateHz) }

func benchChime(b *testing.B, rate int) {
	b.Run("build", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := build(context.Background(), rate); err != nil {
				b.Fatal(err)
			}
		}
		steps := 2 * sectionFrames * (rate / 44100)
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(steps), "ns/step")
	})
	b.Run("cached", func(b *testing.B) {
		if _, err := Get(context.Background(), rate); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if _, err := Get(context.Background(), rate); err != nil {
				b.Fatal(err)
			}
		}
	})
}
