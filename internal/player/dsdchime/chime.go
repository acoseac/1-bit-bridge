// Package dsdchime makes the chime the bridge player's DSD check plays
// (B728 B12, the design's §6.2): at DSD64, DSD128 and DSD256, 0.5 s of DSD
// silence, 0.7 s of 880 Hz on the left, 0.7 s of 1320 Hz on the right and
// 0.5 s of silence. Each tone is modulated at its own rate by the fifth-order
// CIFF loop of the bridge's DSD fixtures
// (internal/transcode/testdata/gen/dsd_fixtures.py), whose bits it
// reproduces, and nothing is repeated from another rate. Silence, the pads
// and each tone's other channel, is 0x69 bytes.
//
// Every bit is the same on every architecture: the arithmetic is IEEE
// float64 with each product rounded on its own (an explicit conversion,
// which forbids a fused multiply-add), and the tones and their edges are
// rotations by constants rounded once from higher precision, with no libm
// call.
//
// Each tone's 0.7 s is 10 ms of zero input, the tone with its 20 ms
// raised-cosine edges, and a tail of zero input. From rest, the loop with no
// input makes 96 69 69 96 over and over, 32 samples whose spectrum lies at
// multiples of the rate over 32 as 0x69's lies at multiples of the rate over
// 8, so a tone's join to the pad before it is as clean as 0x69 itself; its
// tail is as long as its join to the pad after it needs (rates' tail).
//
// A chime is built on its rate's first use and kept for the process:
// 1,693,440, 3,386,880 and 6,773,760 bytes. Get's contract:
//   - concurrent first callers of a rate cause one build and one publication;
//   - the build runs on the context of the caller that began it, asks it every
//     65,536 samples and once more before it publishes, and a build it ended
//     publishes nothing: the next caller builds again;
//   - a caller waiting for another's build stops with its own context, which
//     ends nothing but its wait;
//   - no caller can change the audio: a Chime is read by copying (ReadAt).
//
// Nothing calls it yet but its tests: the player's check will.
package dsdchime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// level is the tones' peak in the loop's units, the one-bit range (a bit is
// ±1): 0.5 × 10^(−30/20), the qualification harness's listening level
// (scripts/qualification/listening-assets/sdm.c, its paragraph on levels),
// rounded once from higher precision. It is 6.02 dB below "−30 dBFS of the
// one-bit full scale": the bridge's own conversion, which maps the one-bit
// range to PCM full scale, reads it at −36.02 dBFS, and a DAC that maps 50 %
// modulation to full scale plays it at −30 dBFS, the console tone's ceiling.
// A listen on a real DAC is owed before any route exposes the check.
const level = 0x1.030dc4ea03a72p-6

// The programme, in frames of 1/44,100 s, each a whole number of bytes at
// every rate (a rate's samples a frame are its multiple of 44,100: 64 at
// DSD64).
const (
	padFrames     = 22050 // 0.5 s
	sectionFrames = 30870 // each tone's 0.7 s
	leadFrames    = 441   // 10 ms of zero input before each tone
	rampFrames    = 882   // each edge's 20 ms, and the shortest tail
	chimeFrames   = 2*padFrames + 2*sectionFrames
)

// rates holds, for DSD64, DSD128 and DSD256, the rotations, each the (cos,
// sin) of its step rounded to nearest from 256-bit arithmetic: 880 Hz's and
// 1320 Hz's 2πf over the rate, and the edges' π over 20 ms of samples; and
// each tone's tail, in frames past 20 ms: the first, counting from none,
// whose join to 0x69 stands within 0.01 of 0x69's own offset and leaves at
// most −101 dB SA-CD in the audio band, a decibel inside the bound
// TestTheChimesJoinsToSilenceAreClean holds it to.
var rates = [3]struct {
	tone [2][2]float64
	ramp [2]float64
	tail [2]int
}{
	{
		tone: [2][2]float64{{0x1.ffffbf9c910fdp-1, 0x1.00c688cb85a2ap-9}, {0x1.ffff6f204a2f4p-1, 0x1.8129b9029a8e3p-9}},
		ramp: [2]float64{0x1.fffffff2b254bp-1, 0x1.d2dd695d33ea6p-15},
		tail: [2]int{0, 4},
	},
	{
		tone: [2][2]float64{{0x1.ffffefe724033p-1, 0x1.00c690de31cc1p-10}, {0x1.ffffdbc81143ep-1, 0x1.8129d4419f111p-10}},
		ramp: [2]float64{0x1.fffffffcac953p-1, 0x1.d2dd69603c46dp-16},
		tail: [2]int{0, 1},
	},
	{
		tone: [2][2]float64{{0x1.fffffbf9c8fccp-1, 0x1.00c692e2dcdc8p-11}, {0x1.fffff6f2043c8p-1, 0x1.8129db1160601p-11}},
		ramp: [2]float64{0x1.ffffffff2b255p-1, 0x1.d2dd6960fe5dep-17},
	},
}

var (
	// errRate refuses a rate the chime is not made at.
	errRate = errors.New("dsdchime: the chime is made at DSD64, DSD128 and DSD256")
	// errBounds ends a modulation whose loop left its bounds.
	errBounds = errors.New("dsdchime: the modulator left its bounds")
)

// bound is the largest magnitude an integrator may reach. The loop holds a
// sine of up to 0.8 of the one-bit range with its integrators under 0.7, and
// above that they run away (TestTheLoopRefusesToLeaveItsBounds): bound is
// far from both, so a stream within it is the stable loop's.
const bound = 4.0

// Chime is one rate's chime, two channels of canonical native DSD
// (format.NativeDSD: bit 7 earliest), read by copying.
type Chime struct {
	bitRate int
	planes  [2][]byte
}

// Format is the chime's stream: its rate, two channels and its samples a
// channel. It names no container: the chime is made, not demuxed.
func (c *Chime) Format() format.NativeDSD {
	return format.NativeDSD{BitRateHz: c.bitRate, Channels: 2, Frames: 8 * int64(len(c.planes[0]))}
}

// ReadAt copies each channel's bytes from off on into dst[0] and dst[1],
// two planes of one length, as many as they hold and the chime has left,
// and answers how many, with io.EOF when that is fewer than they hold. It
// changes nothing, so any number of callers may read at once.
func (c *Chime) ReadAt(dst [][]byte, off int64) (int, error) {
	switch {
	case len(dst) != 2 || len(dst[0]) != len(dst[1]):
		return 0, errors.New("dsdchime: dst is not two planes of one length")
	case off < 0:
		return 0, fmt.Errorf("dsdchime: a read at %d", off)
	case off >= int64(len(c.planes[0])):
		return 0, io.EOF
	}
	n := copy(dst[0], c.planes[0][off:])
	copy(dst[1], c.planes[1][off:])
	if n < len(dst[0]) {
		return n, io.EOF
	}
	return n, nil
}

// cache keeps each rate's chime once built: build makes one, and waiting,
// a test's, hears each caller begin to wait for another's build.
type cache struct {
	build   func(context.Context, int) (*Chime, error)
	waiting func()
	mu      sync.Mutex
	slots   [3]struct {
		chime  *Chime
		flight chan struct{} // closed when the build under way ends
	}
}

var chimes = &cache{build: build}

// Get is the chime at bitRateHz, DSD64, DSD128 or DSD256, built on the
// rate's first use (the package's contract).
func Get(ctx context.Context, bitRateHz int) (*Chime, error) { return chimes.get(ctx, bitRateHz) }

func (c *cache) get(ctx context.Context, bitRateHz int) (*Chime, error) {
	i, err := rateIndex(bitRateHz)
	if err != nil {
		return nil, err
	}
	s := &c.slots[i]
	for {
		c.mu.Lock()
		if ch := s.chime; ch != nil {
			c.mu.Unlock()
			return ch, nil
		}
		if done := s.flight; done != nil {
			c.mu.Unlock()
			if c.waiting != nil {
				c.waiting()
			}
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		s.flight = done
		c.mu.Unlock()
		return c.publish(ctx, bitRateHz, i, done)
	}
}

// publish builds on ctx, as the caller that began the build, and keeps the
// chime unless the build failed or ctx ended before the keeping; either way
// it ends the flight, a build that panics included.
func (c *cache) publish(ctx context.Context, bitRateHz, i int, done chan struct{}) (ch *Chime, err error) {
	s := &c.slots[i]
	defer func() {
		c.mu.Lock()
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			s.chime = ch
		} else {
			ch = nil
		}
		s.flight = nil
		c.mu.Unlock()
		close(done)
	}()
	return c.build(ctx, bitRateHz)
}

func rateIndex(bitRateHz int) (int, error) {
	for i := range rates {
		if bitRateHz == format.DSD64RateHz<<i {
			return i, nil
		}
	}
	return 0, fmt.Errorf("%w: not %d Hz", errRate, bitRateHz)
}

// build makes the chime at bitRateHz: the pads, then each tone's section.
func build(ctx context.Context, bitRateHz int) (*Chime, error) {
	i, err := rateIndex(bitRateHz)
	if err != nil {
		return nil, err
	}
	bpf := bitRateHz / 44100 / 8
	c := &Chime{bitRate: bitRateHz}
	for ch := range c.planes {
		c.planes[ch] = make([]byte, chimeFrames*bpf)
		silence(c.planes[ch])
	}
	for ch := range c.planes {
		at := (padFrames + ch*sectionFrames) * bpf
		if err := toneOf(i, ch, rates[i].tail[ch]).modulate(ctx, c.planes[ch][at:at+sectionFrames*bpf]); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// toneOf is channel ch's section at rates[i], 880 Hz on the left and 1320 Hz
// on the right, with a tail of tail frames past 20 ms.
func toneOf(i, ch, tail int) tone {
	r := &rates[i]
	spf := format.DSD64RateHz << i / 44100
	t := tone{amp: level, c: r.tone[ch][0], s: r.tone[ch][1], rc: r.ramp[0], rs: r.ramp[1], limit: bound,
		lead: leadFrames * spf, ramp: rampFrames * spf}
	t.n = sectionFrames*spf - t.lead - (rampFrames+tail)*spf
	return t
}

// silence fills p with 0x69, DSD silence.
func silence(p []byte) {
	if len(p) > 0 {
		p[0] = 0x69
	}
	for k := 1; k < len(p); k *= 2 {
		copy(p[k:], p[:k])
	}
}

// tone is one channel's section: lead samples of zero input, then n of a
// sine of peak amp, in the one-bit range, at phase zero at its start and
// rotated by (c, s) a sample, rising and falling over ramp samples along
// the raised cosine the rotation (rc, rs) traces; then zero input. limit is
// the largest magnitude an integrator may reach.
type tone struct {
	amp, c, s, rc, rs, limit float64
	lead, n, ramp            int
}

// modulate fills dst with t, eight samples a byte, bit 7 earliest, through
// the fifth-order CIFF loop: the generator's modulate() line for line, but
// for its feed-forward weights, all 1, whose products are exact, and the
// quantizer's ±1, made from the bit with no branch (one on a coin toss
// mispredicts half the time). It asks ctx every 65,536 samples, and ends
// with errBounds once an integrator passes t.limit.
func (t tone) modulate(ctx context.Context, dst []byte) error {
	const b0, b1, b2, b3, b4 = 0.40, 0.20, 0.10, 0.05, 0.02
	var s0, s1, s2, s3, s4 float64
	y, x := 0.0, 1.0   // the tone: sin and cos of its phase
	ey, ex := 0.0, 1.0 // an edge: sin and cos of π·j/ramp
	var acc uint64
	for k := range 8 * len(dst) {
		if k&0xFFFF == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		u := 0.0
		if j := k - t.lead; j >= 0 && j < t.n {
			env := 1.0
			switch {
			case j < t.ramp:
				env = 0.5 - float64(0.5*ex)
				ey, ex = float64(ey*t.rc)+float64(ex*t.rs), float64(ex*t.rc)-float64(ey*t.rs)
			case j >= t.n-t.ramp:
				if j == t.n-t.ramp {
					ey, ex = 0, 1
				}
				env = 0.5 + float64(0.5*ex)
				ey, ex = float64(ey*t.rc)+float64(ex*t.rs), float64(ex*t.rc)-float64(ey*t.rs)
			}
			u = float64(float64(t.amp*env) * y)
			y, x = float64(y*t.c)+float64(x*t.s), float64(x*t.c)-float64(y*t.s)
		}
		v := u + s0 + s1 + s2 + s3 + s4
		var bit uint64
		if v >= 0 {
			bit = 1
		}
		acc = acc<<1 | bit
		if k&7 == 7 {
			dst[k>>3] = byte(acc)
		}
		e := u - math.Float64frombits(0x3FF0000000000000|(bit^1)<<63) // u - (+1 or -1)
		s4 = s4 + float64(b4*s3)
		s3 = s3 + float64(b3*s2)
		s2 = s2 + float64(b2*s1)
		s1 = s1 + float64(b1*s0)
		s0 = s0 + float64(b0*e)
		if !(math.Abs(s0) <= t.limit && math.Abs(s1) <= t.limit && math.Abs(s2) <= t.limit && math.Abs(s3) <= t.limit && math.Abs(s4) <= t.limit) {
			return errBounds
		}
	}
	return nil
}
