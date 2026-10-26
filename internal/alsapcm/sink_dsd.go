package alsapcm

import (
	"context"
	"errors"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"sync"
	"time"
)

// ReleaseMode controls the native sink's outgoing hardware prefix.
type ReleaseMode int

const (
	// Immediate discards all queued content and interrupts a graceful release.
	Immediate ReleaseMode = iota
	// TrackChange retains only the accepted hardware prefix and a bounded tail.
	TrackChange
)

// ErrReleaseUnconfirmed means cleanup still owns the device, or failed. Never
// open a successor on this answer; Done is the actual cleanup completion fence.
var ErrReleaseUnconfirmed = errors.New("native release unconfirmed")

const nativeReleaseBudget = 2 * time.Second
const nativeCloseMargin = 500 * time.Millisecond
const nativeLedgerMax = 1024

// DSDConfig is an explicit provisional native transport and lifecycle policy.
// Check re-reads the harness's descriptor and control snapshot after open and
// at activation/reopen. It never writes mixer controls.
type DSDConfig struct {
	Endpoint                  Endpoint
	Format                    format.ALSAFormat
	BitRate                   int
	Pause                     PauseMode
	Tail, Prefill, BufferTime time.Duration
	RingBytes, LedgerLimit    int
	Check                     func(NodeInfo) error
}

// DSDPosition counts logical bits per channel, excluding all silence/padding.
type DSDPosition struct {
	Submitted, Written, Played int64
	Paused, Released           bool
}
type nativeOccurrence struct {
	id  uint64
	end int64
}
type nativeRequest struct {
	gen        uint64
	resume     bool
	pauseEpoch uint64
	reply      chan error
}

// DSDSink owns native data separately from the unchanged PCM sink. Its writer
// alone owns the device, including cleanup that outlives a caller's deadline.
type DSDSink struct {
	life              context.Context
	pauseEpoch        uint64
	waitIdle          bool
	releaseDeadline   time.Time
	mu                sync.Mutex
	producer          sync.Mutex
	cfg               DSDConfig
	seams             nativeSeams
	devCfg            Config
	params            Params
	dev               *Device // writer-owned after open
	ring              ring
	run               run // writer-owned
	word              int
	carry             [2][4]byte
	carryBits         int
	submitted, played int64
	ledger            [nativeLedgerMax]nativeOccurrence
	ledgerN           int
	active            bool
	paused            bool
	pauseApplied      bool
	released          bool
	revoked           bool
	committed         bool
	finishing         bool
	releasing         bool
	generation        uint64
	pauseAt           time.Time
	mode              ReleaseMode
	releaseCtx        context.Context
	releaseCancel     context.CancelFunc
	waitCancel        context.CancelFunc
	activation        *nativeRequest
	err               error
	changed           chan struct{}
	kick              chan struct{}
	done              chan struct{}
	scratch           []byte
}
type nativeSeams struct {
	open func(Endpoint, Config) (*Device, error)
	now  func() time.Time
}

// OpenDSDSink opens an inert native sink. Activate is mandatory.
func OpenDSDSink(ctx context.Context, c DSDConfig) (*DSDSink, error) {
	return openNativeBounded(ctx, c, nativeSeams{open: openNativeDevice, now: time.Now})
}
func (c DSDConfig) validate() (DSDConfig, error) {
	if format.DSDWordBytes(c.Format) == 0 {
		return c, errors.New("native format required")
	}
	if c.BitRate != 2822400 && c.BitRate != 5644800 && c.BitRate != 11289600 {
		return c, errors.New("native logical rate unsupported")
	}
	if c.Pause != PauseKernel && c.Pause != PauseZeros {
		return c, errors.New("explicit native pause policy required")
	}
	if c.Tail < 0 || c.Tail > 100*time.Millisecond || c.Prefill < 0 || c.Prefill > 500*time.Millisecond {
		return c, errors.New("native silence policy out of range")
	}
	if c.BufferTime == 0 {
		c.BufferTime = DefaultBufferTime
	}
	if c.BufferTime < 0 || c.BufferTime > 500*time.Millisecond {
		return c, errors.New("native buffer exceeds 500 ms")
	}
	if c.RingBytes == 0 {
		c.RingBytes = 4 << 20
	}
	// Reserve space for the fixed ledger, carry and the period scratch below.
	if c.RingBytes < 8 || c.RingBytes > MaxRingBytes-128*1024 {
		return c, errors.New("native ring budget")
	}
	if c.LedgerLimit == 0 {
		c.LedgerLimit = nativeLedgerMax
	}
	if c.LedgerLimit < 1 || c.LedgerLimit > nativeLedgerMax {
		return c, errors.New("native ledger budget")
	}
	return c, nil
}
func openDSDSink(ctx context.Context, c DSDConfig, h nativeSeams) (*DSDSink, error) {
	var err error
	c, err = c.validate()
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	w := format.DSDWordBytes(c.Format)
	dc := Config{RateHz: c.BitRate / (8 * w), Format: c.Format, BufferTime: c.BufferTime}
	d, err := h.open(c.Endpoint, dc)
	if err != nil {
		return nil, err
	}
	p := d.Params()
	if p.BufferTime() > 500*time.Millisecond || p.BufferTime()+nativeFrameDuration(nativeFrames(p.RateHz, c.Tail), p.RateHz)+nativeCloseMargin >= nativeReleaseBudget {
		return nil, nativeCleanupError(errors.New("native readback exceeds release budget"), d.releaseNative())
	}
	if c.Pause == PauseKernel && !p.CanPause() {
		return nil, nativeCleanupError(errors.New("native kernel pause unavailable"), d.releaseNative())
	}
	if err = admit(c.Check, d, c.Endpoint, "native open"); err != nil {
		return nil, nativeCleanupError(err, d.releaseNative())
	}
	if c.RingBytes/p.NativeFrameBytes() <= p.BufferFrames {
		return nil, nativeCleanupError(errors.New("native ring must retain hardware buffer plus lookahead"), d.releaseNative())
	}
	scratchBytes := p.PeriodFrames * p.NativeFrameBytes()
	if c.RingBytes+scratchBytes+nativeLedgerMax*16+8+4096 > MaxRingBytes {
		return nil, nativeCleanupError(errors.New("native aggregate memory budget"), d.releaseNative())
	}
	s := &DSDSink{life: ctx, cfg: c, seams: h, devCfg: dc, params: p, dev: d, word: w * 8, ring: newRing(int64(c.RingBytes/p.NativeFrameBytes()), p.NativeFrameBytes(), int64(p.BufferFrames)), changed: make(chan struct{}), kick: make(chan struct{}, 1), done: make(chan struct{}), scratch: make([]byte, scratchBytes)}
	go s.loopNative()
	go func() {
		select {
		case <-ctx.Done():
			s.Revoke()
		case <-s.done:
		}
	}()
	return s, nil
}

// nativeFrames rounds silence up, never shortening an explicitly chosen time.
func nativeFrames(rate int, d time.Duration) int {
	return int((int64(rate)*int64(d) + int64(time.Second) - 1) / int64(time.Second))
}
func nativeFrameDuration(n, rate int) time.Duration {
	return time.Duration((int64(n)*int64(time.Second) + int64(rate) - 1) / int64(rate))
}
func (s *DSDSink) notify() { close(s.changed); s.changed = make(chan struct{}); poke(s.kick) }
func (s *DSDSink) interrupt() {
	if s.waitCancel != nil {
		s.waitCancel()
	}
	s.notify()
}

// Activate rechecks admission before the first write, including silence. gen
// must increase for a new activation after pause/release; a revoked sink cannot
// be reactivated. A pause racing the admission check wins before the first write.
func (s *DSDSink) Activate(ctx context.Context, gen uint64) error {
	if e := ctx.Err(); e != nil {
		s.Revoke()
		return e
	}
	s.mu.Lock()
	if s.revoked || s.releasing || s.paused || s.activation != nil || gen == 0 || gen <= s.generation {
		s.mu.Unlock()
		return errors.New("native activation fenced")
	}
	r := &nativeRequest{gen: gen, reply: make(chan error, 1)}
	s.activation = r
	s.interrupt()
	s.mu.Unlock()
	select {
	case e := <-r.reply:
		return e
	case <-ctx.Done():
		s.Revoke()
		return ctx.Err()
	case <-s.done:
		return <-r.reply
	}
}

// Revoke permanently cancels activation and begins Immediate cleanup.
func (s *DSDSink) Revoke() { s.beginRelease(Immediate) }

// WriteDSD returns accepted logical bits PER CHANNEL, including bits in the
// partial-word carry. validBits is a count starting at offset in each equally
// sized plane. On partial acceptance/error, retry with offset+n, validBits-n;
// never resend the accepted prefix. Accepted input is copied before return.
// Zero bits completes an empty occurrence without allocating a ledger entry;
// Finish, not a zero-bit Write, declares final EOF.
func (s *DSDSink) WriteDSD(ctx context.Context, id uint64, left, right []byte, offset, validBits int) (int, error) {
	if len(left) != len(right) || offset < 0 || validBits < 0 || offset/8 > len(left) || int64(validBits) > int64(len(left))*8-int64(offset) {
		return 0, errors.New("invalid native bit counts")
	}
	s.producer.Lock()
	defer s.producer.Unlock()
	accepted := 0
	for accepted < validBits {
		if err := ctx.Err(); err != nil {
			return accepted, err
		}
		s.mu.Lock()
		if s.releasing || s.revoked || s.finishing || s.err != nil {
			s.mu.Unlock()
			return accepted, errors.New("native admission closed")
		}
		if s.ledgerN == 0 || s.ledger[s.ledgerN-1].id != id {
			if s.ledgerN == s.cfg.LedgerLimit {
				s.mu.Unlock()
				return accepted, errors.New("native occurrence budget exceeded")
			}
			s.ledger[s.ledgerN] = nativeOccurrence{id: id, end: s.submitted}
			s.ledgerN++
		}
		if s.ring.free() == 0 {
			ch := s.changed
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return accepted, ctx.Err()
			case <-ch:
			}
			continue
		}
		n := min(validBits-accepted, s.word-s.carryBits)
		for ch, src := range [2][]byte{left, right} {
			if (offset+accepted)%8 == 0 && s.carryBits%8 == 0 && n%8 == 0 {
				copy(s.carry[ch][s.carryBits/8:], src[(offset+accepted)/8:(offset+accepted+n)/8])
				continue
			}
			for i := range n {
				at := offset + accepted + i
				dst := s.carryBits + i
				bit := (src[at/8] >> uint(7-at%8)) & 1
				s.carry[ch][dst/8] |= bit << uint(7-dst%8)
			}
		}
		accepted += n
		s.submitted += int64(n)
		s.carryBits += n
		s.ledger[s.ledgerN-1].end = s.submitted
		if s.carryBits == s.word {
			s.flushCarry()
		}
		if s.waitIdle && s.waitCancel != nil {
			s.waitCancel()
		}
		s.notify()
		s.mu.Unlock()
	}
	return accepted, nil
}
func (s *DSDSink) flushCarry() {
	var b [8]byte
	w := s.word / 8
	_, _ = format.PackDSD(b[:], s.carry[0][:w], s.carry[1][:w], s.cfg.Format)
	s.ring.put(s.ring.sub, b[:2*w])
	s.ring.sub++
	s.carry = [2][4]byte{}
	s.carryBits = 0
}

// Finish ends admission, pads final carry with phased 0x69 and drains naturally.
func (s *DSDSink) Finish(ctx context.Context) error {
	if e := ctx.Err(); e != nil {
		s.cancelFinish()
		return errors.Join(ErrReleaseUnconfirmed, e)
	}
	s.producer.Lock()
	s.mu.Lock()
	if s.releasing {
		s.mu.Unlock()
		s.producer.Unlock()
		return s.waitRelease(ctx)
	}
	if s.carryBits > 0 {
		for i := s.carryBits; i < s.word; i++ {
			bit := (byte(0x69) >> uint(7-i%8)) & 1
			for ch := range 2 {
				s.carry[ch][i/8] |= bit << uint(7-i%8)
			}
		}
		s.flushCarry()
	}
	s.finishing = true
	s.interrupt()
	s.mu.Unlock()
	s.producer.Unlock()
	for {
		s.mu.Lock()
		releasing, ch := s.releasing, s.changed
		s.mu.Unlock()
		if releasing {
			return s.waitRelease(ctx)
		}
		select {
		case <-s.done:
			return s.result()
		case <-ctx.Done():
			s.cancelFinish()
			return errors.Join(ErrReleaseUnconfirmed, ctx.Err())
		case <-ch:
		}
	}
}

// cancelFinish establishes an Immediate release only if no owner has selected
// a cutoff. Cancelling the producer cannot escalate a chosen TrackChange.
func (s *DSDSink) cancelFinish() {
	s.mu.Lock()
	if !s.releasing {
		s.beginReleaseLocked(Immediate)
	}
	s.mu.Unlock()
}

// BeginRelease establishes the cutoff synchronously, without waiting for cleanup.
// Callers can cancel producers afterwards and still process Stop during the tail.
func (s *DSDSink) BeginRelease(m ReleaseMode) {
	if m != Immediate && m != TrackChange {
		return
	}
	s.beginRelease(m)
}
func (s *DSDSink) beginRelease(m ReleaseMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beginReleaseLocked(m)
}
func (s *DSDSink) beginReleaseLocked(m ReleaseMode) {
	interrupt := !s.releasing || (m == Immediate && s.mode != Immediate)
	if !s.releasing {
		s.releasing = true
		s.mode = m
		s.releaseDeadline = time.Now().Add(nativeReleaseBudget)
		s.releaseCtx, s.releaseCancel = context.WithDeadline(context.Background(), s.releaseDeadline)
	} else if m == Immediate {
		s.mode = Immediate
	}
	if m == Immediate {
		s.revoked = true
		if s.releaseCancel != nil {
			s.releaseCancel()
		}
	}
	if interrupt {
		s.interrupt()
	}
}

// Release returns within the shared two-second release budget. A timeout never
// transfers device ownership: the writer keeps cleaning up; Done is its fence.
// An Immediate call can escalate a TrackChange even while an ioctl is parked.
func (s *DSDSink) Release(ctx context.Context, m ReleaseMode) error {
	if m != Immediate && m != TrackChange {
		return errors.New("invalid native release mode")
	}
	s.beginRelease(m)
	return s.waitRelease(ctx)
}
func (s *DSDSink) waitRelease(ctx context.Context) error {
	select {
	case <-s.done:
		return s.result()
	default:
	}
	s.mu.Lock()
	deadline := s.releaseDeadline
	s.mu.Unlock()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-s.done:
		return s.result()
	case <-ctx.Done():
		return errors.Join(ErrReleaseUnconfirmed, ctx.Err())
	case <-timer.C:
		return ErrReleaseUnconfirmed
	}
}
func (s *DSDSink) result() error { s.mu.Lock(); defer s.mu.Unlock(); return s.err }

// Pause freezes content progress; after thirty seconds hardware is released.
func (s *DSDSink) Pause(ctx context.Context) error {
	s.mu.Lock()
	if s.releasing {
		s.mu.Unlock()
		return errors.New("native releasing")
	}
	if !s.paused {
		s.pauseAt = s.seams.now()
	}
	s.pauseEpoch++
	s.paused = true
	s.interrupt()
	for s.active && !s.pauseApplied {
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return s.result()
		case <-ch:
		}
		s.mu.Lock()
	}
	s.mu.Unlock()
	return nil
}

// Resume revalidates a released endpoint before resuming retained content.
func (s *DSDSink) Resume(ctx context.Context) error {
	s.mu.Lock()
	if s.generation == 0 || s.releasing || s.revoked || s.err != nil || s.activation != nil {
		s.mu.Unlock()
		return errors.New("native resume fenced")
	}
	if !s.paused {
		s.mu.Unlock()
		return nil
	}
	r := &nativeRequest{gen: s.generation + 1, resume: true, pauseEpoch: s.pauseEpoch, reply: make(chan error, 1)}
	s.activation = r
	s.interrupt()
	s.mu.Unlock()
	select {
	case e := <-r.reply:
		return e
	case <-ctx.Done():
		s.Revoke()
		return ctx.Err()
	case <-s.done:
		return <-r.reply
	}
}

// Committed is true once any hardware frame, even silence, was accepted.
func (s *DSDSink) Committed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.committed }

// Position returns measured content accounting.
func (s *DSDSink) Position() DSDPosition {
	s.mu.Lock()
	defer s.mu.Unlock()
	return DSDPosition{Submitted: s.submitted, Written: min(s.ring.wr*int64(s.word), s.submitted), Played: s.played, Paused: s.paused, Released: s.released}
}

// Params returns the exact committed transport geometry.
func (s *DSDSink) Params() Params { s.mu.Lock(); defer s.mu.Unlock(); return s.params }

// Done closes only after actual cleanup, including a parked close, completes.
func (s *DSDSink) Done() <-chan struct{} { return s.done }

// Transport identifies the native source and exact opened carrier separately.
func (s *DSDSink) Transport() Transport {
	p := s.Params()
	return Transport{Kind: format.KindNativeDSD, RateHz: p.RateHz, Channels: 2, Format: s.cfg.Format, ValidBits: 1, LogicalRate: s.cfg.BitRate, PackingVersion: 1}
}
