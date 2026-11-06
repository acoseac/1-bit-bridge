package dsdconv

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

var (
	// ErrChannels refuses a source that is not stereo.
	ErrChannels = errors.New("dsdconv: the converter takes stereo DSD only")
	// ErrSeek is Seek's answer to any frame but 0 before the first read:
	// converted DSD is not positioned in time (B728 §4.7).
	ErrSeek = errors.New("dsdconv: converted DSD is not positioned in time")
	// ErrWindow is ReadFrames' answer at a held occurrence's window point:
	// the frames before it are out, and the last Window frames wait for
	// Continue or End.
	ErrWindow = errors.New("dsdconv: the occurrence's last frames wait for what follows it")
	// ErrClosed is every call's answer after Close.
	ErrClosed = errors.New("dsdconv: the converter is closed")
	// ErrDetached is a successor's answer once its predecessor went back to
	// its window (Rewind): it follows nothing now, and plays no more.
	ErrDetached = errors.New("dsdconv: the occurrence before this one went back to its window")
	// ErrJoin refuses a Continue that is not a continuous join or comes in a
	// state that cannot take one (no window point, already decided, a
	// successor read or joined already), an End or Rewind with no window
	// point to go to, and a successor's read before its predecessor's window
	// handed it the chain.
	ErrJoin = errors.New("dsdconv: not a continuous join here")

	// errUndecided is the engine's when it needs a byte past a held
	// occurrence that nobody said the end of: never past ReadFrames.
	errUndecided = errors.New("dsdconv: the window was computed before its successor was named")
)

// chunkBytes is what a converter asks its source for at a time, per
// channel: one DSF block, so the demuxer reads whole block groups.
const chunkBytes = 4096

// maxEmpty is the reads in a row that may give nothing, and no error,
// before the source is taken for one that makes no progress.
const maxEmpty = 100

// The final rounding: stage 3's sum carries 2^60 for 1.0, and a 24-bit
// sample's step is 2^-23 of it, a 16-bit's 2^-15.
const (
	shift24 = 2*scaleBits - 23
	shift16 = 2*scaleBits - 15
)

// Options are a converter's own, fixed at New.
type Options struct {
	// Hold stops ReadFrames at the window point of an occurrence that can be
	// continued (its Frames a multiple of D, at least Window frames long):
	// ErrWindow, until Continue or End. An occurrence that cannot be
	// continued never holds, and ends zero-padded.
	Hold bool
	// First is the bytes of each channel already read from the source, which
	// the backend's open reads to prove the content gives bits: the
	// converter copies them at New and reads the source after them, so the
	// caller's buffers are its own again.
	First [][]byte
}

// Converter is one occurrence's conversion: a decode.Decoder over its
// source. It owns the source from New on and closes it once. A converter is
// not safe for use by more than one goroutine at a time, and the
// occurrences of one continuous chain share their state, so one goroutine
// (the backend's producer) calls them all.
type Converter struct {
	src  decode.DSDDecoder
	dsd  format.NativeDSD
	plan Plan
	g    *geometry

	in cursor // this occurrence's bytes
	// e is the chain's state: this converter's own, or, once its
	// predecessor's window is done, its predecessor's (Continue).
	e    *engine
	snap snapshot // the window point's, allocated at New

	frames  int64 // floor(Frames/D)
	base    int64 // the chain's frame of this occurrence's frame 0
	emitted int64
	clipped int64
	dither  uint64

	hold     bool // Options.Hold, and the occurrence can be continued
	holding  bool // at the window point, waiting for Continue or End
	saved    bool // snap holds the window point
	decided  bool // Continue or End since the window point
	next     *Converter
	pred     *Converter
	adopted  bool // a successor its predecessor handed the chain
	handed   bool // a predecessor that handed the chain to next
	detached bool
	started  bool
	closed   bool
}

// snapshot is a converter at its window point: the chain's state, the
// buffered bytes and the pending error with it, and the occurrence's counts
// and dither.
type snapshot struct {
	e       engine
	emitted int64
	clipped int64
	dither  uint64
}

// cursor is where an occurrence's bytes come from: First's copy, then the
// source, never past the length the source declares.
type cursor struct {
	src    decode.DSDDecoder
	first  [2][]byte
	off    int
	total  int64 // bytes a channel the source declares
	bits   int64 // one-bit samples a channel the source declares
	pulled int64
	// holds says the chain does not end with these bytes: their occurrence
	// holds at its window point, and Continue or End says what follows.
	holds bool
	// done is set once the engine has read every byte and marked its end.
	done   bool
	planes [2][]byte // what ReadDSD is handed, so a read allocates nothing
	empty  int
}

// engine is the chain's state: the bytes staged, the stages' delay lines,
// the phases, the end of the input and what ended it.
type engine struct {
	g *geometry
	// line[c][i] is the chain's byte base+i of channel c, for i < hi: the
	// stage-1 delay line and the buffered source bytes after it.
	line [2][]byte
	base int64
	hi   int
	// The chain's bytes below inEnd are real, staged or still to come from
	// cur, then queued. Once ended, nothing follows inEnd: from it on the
	// bytes are numeric zero, and endBits are the chain's one-bit samples.
	inEnd   int64
	ended   bool
	endBits int64
	// partialAt is the chain's last byte when its bits end inside it, with
	// partialMask its bits that are samples (bit 7 earliest); -1 otherwise.
	partialAt   int64
	partialMask byte
	cur, queued *cursor
	// pending is the error that ended the input early, reported after the
	// frames of what came.
	pending error
	// s1[c][i] is stage 1's output s1Base+i, for i < s1Hi; s2 the same.
	s1     [2][]int64
	s1Base int64
	s1Hi   int
	s1Next int64
	s2     [2][]int64
	s2Base int64
	s2Hi   int
	s2Next int64
	frame  int64 // the chain's next output frame
}

// The delay lines' room past the windows they must hold, so they are moved
// down only once in so many values.
const (
	s1Slack = 512
	s2Slack = 512
)

// allocEngine is an engine's buffers for g, as New allocates them.
func allocEngine(g *geometry) engine {
	e := engine{g: g, partialAt: -1}
	for c := 0; c < 2; c++ {
		e.line[c] = make([]byte, g.t+chunkBytes)
		e.s1[c] = make([]int64, 2*g.c2+1+s1Slack)
		e.s2[c] = make([]int64, 2*g.c3+1+s2Slack)
	}
	return e
}

// prime starts a fresh chain at numeric zero, reading from in: every
// stage-1 value whose window ends before the first byte is zero, so is
// every stage-2 value whose window ends before the first such value, and
// the delay lines hold those zeros up to the first value computed.
func (e *engine) prime(in *cursor) {
	g := e.g
	m0 := floorDiv(-int64(4*g.t), int64(g.m1)) + 1
	p0 := -floorDiv(int64(g.c2)-m0, 2)
	if p0 < -int64(g.c3) {
		p0 = -int64(g.c3)
	}
	e.s2Base, e.s2Next = -int64(g.c3), p0
	e.s2Hi = int(p0 - e.s2Base)
	e.s1Base = 2*p0 - int64(g.c2)
	e.s1Next = m0
	if m0 < e.s1Base {
		e.s1Next = e.s1Base
	}
	e.s1Hi = int(e.s1Next - e.s1Base)
	for c := 0; c < 2; c++ {
		clear(e.s1[c][:e.s1Hi])
		clear(e.s2[c][:e.s2Hi])
	}
	e.cur, e.inEnd = in, in.total
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// copyFrom makes e src: every field, and the buffers' contents up to what
// src holds. e's buffers are its own and as long as src's.
func (e *engine) copyFrom(src *engine) {
	for c := 0; c < 2; c++ {
		copy(e.line[c][:src.hi], src.line[c][:src.hi])
		copy(e.s1[c][:src.s1Hi], src.s1[c][:src.s1Hi])
		copy(e.s2[c][:src.s2Hi], src.s2[c][:src.s2Hi])
	}
	e.g = src.g
	e.base, e.hi = src.base, src.hi
	e.inEnd, e.ended, e.endBits = src.inEnd, src.ended, src.endBits
	e.partialAt, e.partialMask = src.partialAt, src.partialMask
	e.cur, e.queued, e.pending = src.cur, src.queued, src.pending
	e.s1Base, e.s1Hi, e.s1Next = src.s1Base, src.s1Hi, src.s1Next
	e.s2Base, e.s2Hi, e.s2Next = src.s2Base, src.s2Hi, src.s2Next
	e.frame = src.frame
}

// New makes a converter of src's DSD to plan's PCM. It refuses a plan that
// is not one of Plans (ErrPlan), a source that is not stereo
// (ErrChannels), a rate but DSD64, DSD128 and DSD256 (*RateError) and a
// negative Frames, and allocates everything its reads use, its snapshot's
// room included.
func New(src decode.DSDDecoder, plan Plan, opt Options) (*Converter, error) {
	if src == nil {
		return nil, errors.New("dsdconv: no source")
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	f := src.Format()
	if f.Channels != 2 {
		return nil, fmt.Errorf("%w: %d channels", ErrChannels, f.Channels)
	}
	g, err := geometryFor(f.BitRateHz, plan)
	if err != nil {
		return nil, err
	}
	if f.Frames < 0 {
		return nil, fmt.Errorf("dsdconv: the source declares %d one-bit samples", f.Frames)
	}
	d := int64(g.d)
	c := &Converter{src: src, dsd: f, plan: plan, g: g, frames: f.Frames / d, dither: ditherSeed}
	c.hold = opt.Hold && f.Frames%d == 0 && c.frames >= int64(g.h)
	c.in = cursor{src: src, total: f.BytesPerChannel(), bits: f.Frames, holds: c.hold}
	if opt.First != nil {
		if len(opt.First) != 2 || len(opt.First[0]) != len(opt.First[1]) || int64(len(opt.First[0])) > c.in.total {
			return nil, errors.New("dsdconv: First is not two equal planes within the source's length")
		}
		for ch := range c.in.first {
			c.in.first[ch] = append([]byte(nil), opt.First[ch]...)
		}
	}
	own := allocEngine(g)
	c.e = &own
	c.e.prime(&c.in)
	c.snap.e = allocEngine(g)
	return c, nil
}

// Format is the converted stream: the plan's rate and valid bits, stereo,
// ContainerConverted, and floor(Frames/D) frames.
func (c *Converter) Format() format.PCM {
	return format.PCM{RateHz: c.plan.RateHz, Channels: 2, ValidBits: c.plan.ValidBits, Container: ContainerConverted, Frames: c.frames}
}

// Source is the DSD stream converted, as its source declares it.
func (c *Converter) Source() format.NativeDSD { return c.dsd }

// Plan is the converter's plan.
func (c *Converter) Plan() Plan { return c.plan }

// Window is H at the source's rate under the plan.
func (c *Converter) Window() int { return c.g.h }

// Emitted is the frames of this occurrence ReadFrames has given: the
// window's are counted once, however often it was computed again.
func (c *Converter) Emitted() int64 { return c.emitted }

// Clipped is this occurrence's samples saturated at full scale, each
// channel's counted: the count of the frames Emitted counts.
func (c *Converter) Clipped() int64 { return c.clipped }

// Holding reports whether the converter waits at its window point.
func (c *Converter) Holding() bool { return c.holding }

// ReadFrames converts the next frames into dst, per decode.Decoder: n is
// samples (two a frame), and io.EOF ends the occurrence. A source that ended
// early (a *decode.Refusal, an io.EOF before its declared length, an I/O
// error) has the frames of what it gave flushed as an end, then that error
// on every call. A held occurrence stops at its window point with
// ErrWindow. A context that ends returns the frames made with its error;
// what was read is kept, and the next call goes on.
func (c *Converter) ReadFrames(ctx context.Context, dst []int32) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	switch {
	case c.closed:
		return 0, ErrClosed
	case c.detached:
		return 0, ErrDetached
	case c.pred != nil && !c.adopted:
		return 0, ErrJoin
	case len(dst)%2 != 0:
		return 0, fmt.Errorf("dsdconv: dst length %d is not a multiple of 2 channels", len(dst))
	}
	c.started = true
	want := len(dst) / 2
	for n := 0; n < want; {
		if c.emitted >= c.limit() {
			return 2 * n, c.finish()
		}
		if c.hold && !c.decided && c.emitted == c.frames-int64(c.g.h) {
			if err := c.window(ctx); err != nil {
				return 2 * n, err
			}
			if c.hold {
				c.holding = true
				return 2 * n, ErrWindow
			}
			continue
		}
		if err := c.e.ensure(ctx, c.e.frame); err != nil {
			return 2 * n, err
		}
		if c.emitted >= c.limit() {
			continue
		}
		a0, a1 := c.e.stage3()
		dst[2*n], dst[2*n+1] = c.quantize(a0), c.quantize(a1)
		c.e.frame++
		c.emitted++
		n++
		if n%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return 2 * n, err
			}
		}
	}
	return len(dst), nil
}

// limit is the frames this occurrence gives: floor(Frames/D), fewer when
// the input ended early.
func (c *Converter) limit() int64 {
	if !c.e.ended {
		return c.frames
	}
	if end := c.e.endBits/int64(c.g.d) - c.base; end < c.frames {
		return end
	}
	return c.frames
}

// finish ends the occurrence: a continued one hands the chain to its
// successor, whose frame 0 is the chain's next; the last of a chain reports
// what ended its input early, if anything did.
func (c *Converter) finish() error {
	if c.next != nil {
		if !c.handed {
			c.next.e, c.next.base, c.next.adopted = c.e, c.e.frame, true
			c.handed = true
		}
		return io.EOF
	}
	if c.e.ended && c.e.pending != nil {
		return c.e.pending
	}
	return io.EOF
}

// window reaches the window point: every byte of the occurrence read, so a
// source that ends short is known first, and the snapshot taken, before any
// bit of a successor enters. A source that did end short leaves nothing to
// hold: the occurrence ends as it is.
func (c *Converter) window(ctx context.Context) error {
	if c.saved {
		return nil
	}
	e := c.e
	if err := e.fill(ctx, e.inEnd-1); err != nil {
		return err
	}
	if e.ended {
		c.hold = false
		return nil
	}
	c.snap.e.copyFrom(e)
	c.snap.emitted, c.snap.clipped, c.snap.dither = c.emitted, c.clipped, c.dither
	c.saved = true
	return nil
}

// Continue continues a held occurrence into next, its continuous successor
// (Continuous), made with New and not read: the window is computed from
// next's first bits, which the chain reads from next's bytes, and once the
// window's last frame is out, next carries the chain on from this
// occurrence's delay lines.
func (c *Converter) Continue(next *Converter) error {
	switch {
	case c.closed:
		return ErrClosed
	case next == nil || next == c || !c.holding || c.decided:
		return ErrJoin
	case next.started || next.adopted || next.pred != nil || next.closed || next.detached:
		return ErrJoin
	case !Continuous(c.dsd, c.plan, next.dsd, next.plan):
		return ErrJoin
	}
	c.next, next.pred = next, c
	c.decided, c.holding = true, false
	c.e.queued = &next.in
	return nil
}

// End ends a held occurrence with nothing after it, or a successor that
// abuts: its window is flushed with numeric zeros.
func (c *Converter) End() error {
	switch {
	case c.closed:
		return ErrClosed
	case !c.holding || c.decided:
		return ErrJoin
	}
	c.decided, c.holding = true, false
	c.e.ended, c.e.endBits = true, 8*c.e.inEnd
	return nil
}

// Rewind goes back to the window point's snapshot, for a successor
// replaced or cut: the occurrence holds again, its counts are the
// snapshot's, and the successor named before follows nothing
// (ErrDetached). Nothing about the window is kept, so computing it again,
// for the same successor's bits or another's, gives what it gave first.
func (c *Converter) Rewind() error {
	switch {
	case c.closed:
		return ErrClosed
	case c.detached:
		return ErrDetached
	case !c.saved:
		return ErrJoin
	}
	if c.next != nil {
		c.next.detach()
		c.next = nil
	}
	c.e.copyFrom(&c.snap.e)
	c.emitted, c.clipped, c.dither = c.snap.emitted, c.snap.clipped, c.snap.dither
	c.holding, c.decided, c.handed = true, false, false
	return nil
}

func (c *Converter) detach() {
	if c.detached {
		return
	}
	c.detached = true
	if c.next != nil {
		c.next.detach()
	}
}

// Seek refuses every frame but 0 before the first read, which positions
// nothing: seeking within converted DSD stays unsupported (B728 §4.7), and
// the source is never sought.
func (c *Converter) Seek(ctx context.Context, frame int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.closed {
		return ErrClosed
	}
	if frame == 0 && !c.started {
		return nil
	}
	return ErrSeek
}

// Close closes the source, once; every call after it is ErrClosed. It
// leaves the chain's state to the occurrences after this one.
func (c *Converter) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	return c.src.Close()
}

// quantize rounds stage 3's sum to the plan's bits, half to even at 24 and
// after TPDF dither of ±1 LSB at 16 (two 32-bit halves of one xorshift64
// draw, their difference), and saturates at ±(2^(bits-1) - 1), counted.
func (c *Converter) quantize(acc int64) int32 {
	var v, max int64
	if c.plan.ValidBits == 16 {
		x := c.dither
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		c.dither = x
		acc += (int64(uint32(x)) - int64(x>>32)) << (shift16 - 32)
		v, max = roundShift(acc, shift16), 1<<15-1
	} else {
		v, max = roundShift(acc, shift24), 1<<23-1
	}
	if v > max {
		v = max
		c.clipped++
	} else if v < -max {
		v = -max
		c.clipped++
	}
	return int32(v)
}

// roundShift is x/2^s rounded to the nearest integer, ties to even.
func roundShift(x int64, s uint) int64 {
	return (x + (int64(1)<<(s-1) - 1) + (x>>s)&1) >> s
}

// Continuous is the one rule a join of two converted occurrences is
// classified by: continuous when they share the plan and the DSD rate, the
// predecessor's Frames is a multiple of D (an unaligned boundary has no
// phase that keeps both its own k·D and the continuous stream's), and each
// is at least H frames long (a shorter successor would let the
// predecessor's tail depend on the occurrence after it, and a shorter
// predecessor's window would start before it). Anything else abuts, which
// is still gapless on one PCM transport.
func Continuous(pred format.NativeDSD, predPlan Plan, succ format.NativeDSD, succPlan Plan) bool {
	if predPlan != succPlan || pred.BitRateHz != succ.BitRateHz || pred.Channels != 2 || succ.Channels != 2 {
		return false
	}
	g, err := geometryFor(pred.BitRateHz, predPlan)
	if err != nil {
		return false
	}
	d, h := int64(g.d), int64(g.h)
	return pred.Frames%d == 0 && pred.Frames/d >= h && succ.Frames/d >= h
}

// pull reads the next bytes of the occurrence into d0 and d1: First's
// copy, then the source's, at most what the source declares. A source's
// io.EOF before its declared length is a truncated refusal; a read that
// gives nothing too often is io.ErrNoProgress.
func (in *cursor) pull(ctx context.Context, d0, d1 []byte) (int, error) {
	want := len(d0)
	if left := in.total - in.pulled; int64(want) > left {
		want = int(left)
	}
	if want <= 0 {
		return 0, nil
	}
	if in.off < len(in.first[0]) {
		n := copy(d0[:want], in.first[0][in.off:])
		copy(d1[:n], in.first[1][in.off:])
		in.off += n
		in.pulled += int64(n)
		return n, nil
	}
	in.planes[0], in.planes[1] = d0[:want], d1[:want]
	n, err := in.src.ReadDSD(ctx, in.planes[:])
	in.planes[0], in.planes[1] = nil, nil
	if n < 0 || n > want {
		return 0, fmt.Errorf("dsdconv: the source gave %d bytes for a read of %d", n, want)
	}
	in.pulled += int64(n)
	if n > 0 {
		in.empty = 0
	}
	switch {
	case err == nil:
		if n == 0 {
			if in.empty++; in.empty >= maxEmpty {
				return 0, io.ErrNoProgress
			}
		}
		return n, nil
	case errors.Is(err, io.EOF):
		if in.pulled < in.total {
			return n, &decode.Refusal{Reason: decode.ReasonTruncated,
				Message: fmt.Sprintf("the DSD source ended after %d of its %d bytes a channel", in.pulled, in.total)}
		}
		return n, nil
	}
	return n, err
}

// fill stages the chain's bytes up to need, or learns they are numeric
// zero. A cancel keeps what was read and is returned; any other error ends
// the input at what came, as an end, and is the pending error.
func (e *engine) fill(ctx context.Context, need int64) error {
	for need >= e.base+int64(e.hi) {
		have := e.base + int64(e.hi)
		if have >= e.inEnd {
			if !e.cur.done {
				e.cursorEnd()
				continue
			}
			if e.queued != nil {
				e.cur, e.queued = e.queued, nil
				e.inEnd += e.cur.total
				continue
			}
			if e.ended {
				return nil
			}
			return errUndecided
		}
		if e.hi == len(e.line[0]) {
			e.compact()
			if e.hi == len(e.line[0]) {
				return errors.New("dsdconv: the delay line has no room")
			}
		}
		room := len(e.line[0]) - e.hi
		if left := e.inEnd - have; int64(room) > left {
			room = int(left)
		}
		n, err := e.cur.pull(ctx, e.line[0][e.hi:e.hi+room], e.line[1][e.hi:e.hi+room])
		e.hi += n
		if e.cur.pulled >= e.cur.total && !e.cur.done {
			e.cursorEnd()
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			e.truncate(err)
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// cursorEnd marks the end of the current cursor's bytes: its last byte's
// padding, and the chain's end unless its occurrence holds.
func (e *engine) cursorEnd() {
	in := e.cur
	in.done = true
	pad := int64(0)
	if r := in.bits % 8; r != 0 {
		e.partialAt, e.partialMask = e.inEnd-1, byte(0xFF)<<(8-r)
		pad = 8 - r
	}
	if !in.holds {
		e.ended, e.endBits = true, 8*e.inEnd-pad
	}
}

// truncate ends the input at the bytes that came, with err pending.
func (e *engine) truncate(err error) {
	e.inEnd = e.base + int64(e.hi)
	e.ended, e.endBits = true, 8*e.inEnd
	if e.partialAt >= e.inEnd {
		e.partialAt = -1
	}
	e.queued, e.pending = nil, err
}

// compact moves the staged bytes down past those no window needs again.
func (e *engine) compact() {
	g := e.g
	w0 := (e.s1Next*int64(g.m1) - int64(4*g.t)) / 8
	drop := w0 - e.base
	if drop <= 0 {
		return
	}
	if drop > int64(e.hi) {
		drop = int64(e.hi)
	}
	for c := 0; c < 2; c++ {
		copy(e.line[c], e.line[c][drop:e.hi])
	}
	e.hi -= int(drop)
	e.base += drop
}

// ensure computes the stage outputs frame f's stage 3 takes.
func (e *engine) ensure(ctx context.Context, f int64) error {
	g := e.g
	for need2 := int64(g.m3)*f + int64(g.c3); e.s2Next <= need2; {
		for need1 := 2*e.s2Next + int64(g.c2); e.s1Next <= need1; {
			if err := e.stage1(ctx); err != nil {
				return err
			}
		}
		e.stage2()
	}
	return nil
}

// stage1 computes s1[s1Next] for both channels: its window's T bytes
// through the tables, or, where the window holds bytes before the stream,
// after its end or the stream's partial last byte, byte by byte.
func (e *engine) stage1(ctx context.Context) error {
	g := e.g
	w0 := (e.s1Next*int64(g.m1) - int64(4*g.t)) / 8
	last := w0 + int64(g.t) - 1
	if last >= e.base+int64(e.hi) {
		if err := e.fill(ctx, last); err != nil {
			return err
		}
	}
	var v0, v1 int64
	if w0 >= 0 && last < e.base+int64(e.hi) && (e.partialAt < w0 || e.partialAt > last) {
		off := int(w0 - e.base)
		l0 := e.line[0][off : off+g.t]
		l1 := e.line[1][off : off+g.t]
		for j := range g.tabs {
			row := &g.tabs[j]
			v0 += int64(row[l0[j]])
			v1 += int64(row[l1[j]])
		}
	} else {
		v0, v1 = e.stage1Edge(w0)
	}
	if e.s1Hi == len(e.s1[0]) {
		keep := 2*e.s2Next - int64(g.c2) - e.s1Base
		for c := 0; c < 2; c++ {
			copy(e.s1[c], e.s1[c][keep:e.s1Hi])
		}
		e.s1Hi -= int(keep)
		e.s1Base += keep
	}
	e.s1[0][e.s1Hi], e.s1[1][e.s1Hi] = v0, v1
	e.s1Hi++
	e.s1Next++
	return nil
}

// stage1Edge is stage 1 over a window that is not all real, whole bytes:
// a byte before the stream or past its end is numeric zero, and the
// partial last byte counts its sample bits alone, never its padding.
func (e *engine) stage1Edge(w0 int64) (v0, v1 int64) {
	g := e.g
	for j := 0; j < g.t; j++ {
		idx := w0 + int64(j)
		if idx < 0 || (e.ended && idx >= e.inEnd) {
			continue
		}
		off := idx - e.base
		if off < 0 || off >= int64(e.hi) {
			panic(fmt.Sprintf("dsdconv: byte %d of the chain is not staged (%d to %d)", idx, e.base, e.base+int64(e.hi)))
		}
		b0, b1 := e.line[0][off], e.line[1][off]
		if idx != e.partialAt {
			v0 += int64(g.tabs[j][b0])
			v1 += int64(g.tabs[j][b1])
			continue
		}
		for i := 0; i < 8; i++ {
			bit := byte(0x80) >> i
			if e.partialMask&bit == 0 {
				continue
			}
			tap := g.g1[8*j+i]
			if b0&bit != 0 {
				v0 += tap
			} else {
				v0 -= tap
			}
			if b1&bit != 0 {
				v1 += tap
			} else {
				v1 -= tap
			}
		}
	}
	return v0, v1
}

// stage2 computes s2[s2Next] for both channels: the half-band over s1,
// centred on s1[2·s2Next], its zero taps skipped, rounded back to 2^30.
func (e *engine) stage2() {
	g := e.g
	o := int(2*e.s2Next - e.s1Base)
	if e.s2Hi == len(e.s2[0]) {
		keep := int64(g.m3)*e.frame - int64(g.c3) - e.s2Base
		for c := 0; c < 2; c++ {
			copy(e.s2[c], e.s2[c][keep:e.s2Hi])
		}
		e.s2Hi -= int(keep)
		e.s2Base += keep
	}
	centre := g.g2[g.c2]
	for c := 0; c < 2; c++ {
		w := e.s1[c]
		v := centre * w[o]
		for i, k := range g.pairs2 {
			v += g.h2[i] * (w[o-k] + w[o+k])
		}
		e.s2[c][e.s2Hi] = roundShift(v, scaleBits)
	}
	e.s2Hi++
	e.s2Next++
}

// stage3 is the plan's low-pass over s2 centred on s2[M3·frame], for both
// channels, at 2^60 for 1.0.
func (e *engine) stage3() (int64, int64) {
	g := e.g
	h := g.h3
	c := len(h) - 1
	o := int(int64(g.m3)*e.frame - e.s2Base)
	x0 := e.s2[0][o-c : o+c+1]
	x1 := e.s2[1][o-c : o+c+1]
	a0, a1 := h[0]*x0[c], h[0]*x1[c]
	for k := 1; k <= c; k++ {
		a0 += h[k] * (x0[c-k] + x0[c+k])
		a1 += h[k] * (x1[c-k] + x1[c+k])
	}
	return a0, a1
}
