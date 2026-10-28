package alsapcm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

func (s *DSDSink) loopNative() {
	defer func() {
		if r := recover(); r != nil {
			s.setNativeError(fmt.Errorf("native writer panic: %T", r))
		}
		if s.dev != nil {
			if e := s.closeNative(); e != nil {
				s.setNativeError(errors.Join(ErrReleaseUnconfirmed, e))
			}
		}
		s.mu.Lock()
		s.released = true
		s.active = false
		if s.activation != nil {
			s.activation.reply <- errors.New("native activation cancelled")
			s.activation = nil
		}
		if s.releaseCancel != nil {
			s.releaseCancel()
		}
		s.notify()
		s.mu.Unlock()
		close(s.done)
	}()
	for {
		s.mu.Lock()
		release, mode, paused, active, finish := s.releasing, s.mode, s.paused, s.active, s.finishing
		r := s.activation
		s.mu.Unlock()
		if release {
			// The previous Write has returned and its prefix is accounted before this
			// cutoff. No producer or writer can submit the unaccepted suffix now.
			s.mu.Lock()
			s.ring.sub = s.ring.wr
			s.carryBits = 0
			s.carry = [2][4]byte{}
			s.mu.Unlock()
			if mode == TrackChange && active && !paused && s.dev != nil {
				if e := s.nativeSilence(nativeFrames(s.params.RateHz, s.cfg.Tail), true, false); e != nil && !s.expectedReleaseCancellation(e) {
					s.setNativeError(errors.Join(ErrReleaseUnconfirmed, e))
					return
				}
				if !s.immediate() {
					remaining := s.releaseDeadline.Sub(s.seams.now()) - nativeCloseMargin
					if remaining <= 0 {
						s.setNativeError(errors.Join(ErrReleaseUnconfirmed, context.DeadlineExceeded))
						return
					}
					if e := s.dev.Drain(s.releaseCtx, remaining); e != nil && !s.expectedReleaseCancellation(e) {
						s.setNativeError(errors.Join(ErrReleaseUnconfirmed, e))
					} else if e == nil {
						if e := s.measureNative(); e != nil {
							s.setNativeError(e)
						}
					}
				}
			}
			return
		}
		if r != nil {
			e := s.activateNative(r)
			s.mu.Lock()
			if s.activation == r {
				s.activation = nil
			}
			if errors.Is(e, ErrReleaseUnconfirmed) {
				s.err = errors.Join(s.err, e)
				s.revoked = true
			}
			r.reply <- e
			s.notify()
			s.mu.Unlock()
			if e != nil {
				s.mu.Lock()
				terminal := errors.Is(e, ErrReleaseUnconfirmed)
				held := s.paused && !s.releasing && !terminal
				if !terminal && !held && !s.releasing {
					s.err = errors.Join(s.err, e)
				}
				if held && !r.resume {
					s.active = false
				}
				s.mu.Unlock()
				if !held {
					s.beginRelease(Immediate)
				}
			}
			continue
		}
		if paused {
			if e := s.pauseNative(); e != nil {
				if errors.Is(e, context.Canceled) || ReasonOf(e) == ReasonCancelled {
					continue
				}
				s.setNativeError(e)
				return
			}
			continue
		}
		if !active {
			select {
			case <-s.kick:
			case <-s.done:
				return
			}
			continue
		}
		s.mu.Lock()
		n := min(s.ring.pending(), int64(s.params.PeriodFrames))
		b := s.ring.span(s.ring.wr, n)
		s.inflight = s.ring.wr + int64(len(b)/s.ring.frameBytes)
		s.mu.Unlock()
		if len(b) > 0 {
			if e := s.writeNative(b, false, false, false); e != nil {
				if ReasonOf(e) == ReasonCancelled || errors.Is(e, context.Canceled) {
					continue
				}
				s.setNativeError(e)
				return
			}
			continue
		}
		if finish {
			s.beginRelease(TrackChange)
			continue
		}
		if e := s.watchNative(s.params.PeriodTime()); e != nil && ReasonOf(e) != ReasonCancelled && !errors.Is(e, context.Canceled) {
			s.setNativeError(e)
			return
		}
	}
}
func (s *DSDSink) setNativeError(e error) {
	s.mu.Lock()
	s.err = errors.Join(s.err, e)
	s.beginReleaseLocked(Immediate)
	s.notify()
	s.mu.Unlock()
}
func (s *DSDSink) immediate() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releasing && s.mode == Immediate
}
func (s *DSDSink) activateNative(r *nativeRequest) error {
	ctx := r.ctx
	reopened := false
	if s.dev == nil {
		// Only a reopen needs the combined lifetime while locating. Initial
		// activation's caller already owns the cancellation fence.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(r.ctx)
		stop := context.AfterFunc(s.life, cancel)
		defer stop()
		defer cancel()
		ep := s.cfg.Endpoint
		if s.cfg.Locate != nil {
			var e error
			ep, e = s.cfg.Locate(ctx)
			if e != nil {
				return e
			}
		}
		d, e := s.seams.open(ep, s.devCfg)
		if e != nil {
			return e
		}
		s.dev = d
		reopened = true
		p := d.Params()
		if e := dopPrecision(s.cfg, p); e != nil {
			return nativeCleanupError(e, s.closeNative())
		}
		if p.BufferTime() > 500*time.Millisecond || p.BufferTime()+nativeFrameDuration(nativeFrames(p.RateHz, s.cfg.Tail), p.RateHz)+nativeCloseMargin >= nativeReleaseBudget || p.BufferFrames != s.params.BufferFrames || p.PeriodFrames != s.params.PeriodFrames || (s.cfg.Pause == PauseKernel && !p.CanPause()) {
			e := errors.New("native reopen geometry/pause policy changed")
			if ce := s.closeNative(); ce != nil {
				return errors.Join(ErrReleaseUnconfirmed, e, ce)
			}
			return e
		}
		s.mu.Lock()
		s.params = p
		s.mu.Unlock()
	}
	if e := admit(s.cfg.Check, s.dev, s.dev.Params().Endpoint, "native activation"); e != nil {
		if reopened {
			if ce := s.closeNative(); ce != nil {
				return errors.Join(ErrReleaseUnconfirmed, e, ce)
			}
		}
		return e
	}
	s.mu.Lock()
	if ctx.Err() != nil || s.life.Err() != nil || s.revoked || s.releasing || (s.paused && (!r.resume || r.pauseEpoch != s.pauseEpoch)) {
		s.mu.Unlock()
		if reopened {
			return nativeCleanupError(errors.New("native activation fenced"), s.closeNative())
		}
		return errors.New("native activation fenced")
	}
	wasActive := s.active
	s.active = true
	s.paused = false
	s.pauseApplied = false
	s.generation = r.gen
	s.released = false
	s.mu.Unlock()
	if wasActive && !reopened && s.cfg.Pause == PauseKernel {
		st, e := s.dev.status()
		if e != nil {
			return e
		}
		s.mu.Lock()
		fenced := s.life.Err() != nil || s.revoked || s.releasing || r.pauseEpoch != s.pauseEpoch
		s.mu.Unlock()
		if fenced {
			return errors.New("native resume fenced after status")
		}
		if st.state == statePaused {
			if e = s.dev.Pause(false); e != nil {
				return e
			}
		}
	}
	if !wasActive || reopened {
		s.dopPhase = 0x05
		return s.nativeSilence(nativeFrames(s.params.RateHz, s.cfg.Prefill), false, false)
	}
	return nil
}

// nativeContext installs cancellation under the same lock as the command's
// fence. A Pause/Stop that won before this point gives a cancelled context.
func (s *DSDSink) nativeContext(tail, allowPaused bool) (context.Context, context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parent := s.life
	if tail && s.releaseCtx != nil {
		parent = s.releaseCtx
	}
	ctx, cancel := context.WithCancel(parent)
	s.waitIdle = false
	s.waitCancel = cancel
	if s.life.Err() != nil || (s.releasing && (!tail || s.mode == Immediate)) || s.revoked || (s.paused && !allowPaused) {
		cancel()
	}
	return ctx, cancel
}
func (s *DSDSink) writeNative(b []byte, silence, tail, allowPaused bool) error {
	ctx, cancel := s.nativeContext(tail, allowPaused)
	defer cancel()
	// Only this writer owns phase. Ring/carry bytes have no markers, so an
	// unaccepted suffix can be repacked after interruption or a new run.
	wire := b
	if s.cfg.Kind == format.KindDoP {
		wire = s.dopScratch[:len(b)*2]
		marker := s.dopPhase
		for i := 0; i < len(b); i += 4 {
			_, next, err := format.PackDoP(wire[i*2:i*2+8], b[i:i+2], b[i+2:i+4], marker)
			if err != nil {
				return err
			}
			marker = next
		}
	}
	n, e := s.dev.Write(ctx, wire)
	if s.cfg.Kind == format.KindDoP {
		// Write returns accepted stereo frames, including an errored prefix.
		s.dopPhase ^= byte((n & 1) * 0xff)
	}
	s.mu.Lock()
	s.inflight = 0
	if n > 0 {
		s.committed = true
		if !silence {
			s.ring.written(int64(n))
		}
		s.notify()
	}
	s.mu.Unlock()
	if !s.run.add(int64(n), silence) {
		return errors.New("native run ledger exhausted")
	}
	if e != nil {
		return e
	}
	return s.measureNative()
}
func (s *DSDSink) nativeSilence(frames int, tail, allowPaused bool) error {
	dopSilence := [2]byte{0x69, 0x69}
	for i := range s.scratch {
		s.scratch[i] = 0x69
		if s.cfg.Kind == format.KindDoP {
			s.scratch[i] = dopSilence[i%2]
		}
	}
	for frames > 0 {
		n := min(frames, s.params.PeriodFrames)
		if e := s.writeNative(s.scratch[:n*s.ring.frameBytes], true, tail, allowPaused); e != nil {
			return e
		}
		frames -= n
	}
	return nil
}
func (s *DSDSink) measureNative() error {
	st, e := s.dev.status()
	if e != nil {
		return e
	}
	if st.state == stateXrun || st.state == stateSuspended || st.state == stateDisconnected {
		return s.dev.stateError("native status", st.state)
	}
	var frames int64
	switch st.state {
	case stateRunning, stateDraining:
		if st.delay < 0 {
			return nil
		}
		frames = max(int64(0), s.run.written-st.delay)
	case statePaused:
		frames = int64(st.hwPtr)
	case stateSetup:
		frames = s.run.written
	default:
		return nil
	}
	frames = min(frames, s.run.written)
	played := s.run.base + s.run.contentAmong(frames)
	s.run.fold(frames)
	s.mu.Lock()
	s.played = max(s.played, min(played*int64(s.word), s.submitted))
	i := 0
	for i < s.ledgerN && s.ledger[i].end <= s.played {
		i++
	}
	if i > 0 {
		copy(s.ledger[:], s.ledger[i:s.ledgerN])
		s.ledgerN -= i
	}
	s.notify()
	s.mu.Unlock()
	return nil
}
func (s *DSDSink) watchNative(d time.Duration) error {
	s.mu.Lock()
	if s.releasing || s.activation != nil || (s.paused && !s.pauseApplied) || (!s.paused && (s.finishing || s.ring.pending() > 0)) {
		s.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.waitCancel = cancel
	s.waitIdle = true
	s.mu.Unlock()
	defer cancel()
	_, e := s.dev.wait(ctx, d)
	if e != nil {
		return e
	}
	return s.measureNative()
}
func (s *DSDSink) pauseNative() error {
	s.mu.Lock()
	applied, at, released := s.pauseApplied, s.pauseAt, s.released
	s.mu.Unlock()
	if released {
		<-s.kick
		return nil
	}
	if !applied {
		if e := s.measureNative(); e != nil {
			return e
		}
		st, e := s.dev.status()
		if e != nil {
			return e
		}
		if s.cfg.Pause == PauseKernel && st.state == stateRunning {
			if e := s.dev.Pause(true); e != nil {
				return e
			}
		}
		s.mu.Lock()
		s.pauseApplied = true
		s.notify()
		s.mu.Unlock()
	}
	left := 30*time.Second - s.seams.now().Sub(at)
	if left <= 0 {
		if e := s.measureNative(); e != nil {
			return e
		}
		s.mu.Lock()
		playedFrames := s.played / int64(s.word)
		if playedFrames < s.ring.base {
			s.mu.Unlock()
			return errors.New("native retained prefix lost")
		}
		s.ring.wr = playedFrames
		s.mu.Unlock()
		if e := s.closeNative(); e != nil {
			return errors.Join(ErrReleaseUnconfirmed, e)
		}
		s.run = run{base: playedFrames}
		s.mu.Lock()
		s.released = true
		s.active = false
		s.notify()
		s.mu.Unlock()
		return nil
	}
	if s.cfg.Pause == PauseZeros {
		st, e := s.dev.status()
		if e != nil {
			return e
		}
		if st.state == stateRunning {
			return s.nativeSilence(s.params.PeriodFrames, false, true)
		}
	}
	e := s.watchNative(min(left, s.params.PeriodTime()))
	if errors.Is(e, context.Canceled) || ReasonOf(e) == ReasonCancelled {
		return nil
	}
	return e
}

// closeNative preserves teardown errors (the PCM release semantics are left
// untouched). The writer retains exclusive ownership through every ioctl and
// close, even when the caller has already received ErrReleaseUnconfirmed.
func (s *DSDSink) closeNative() error {
	d := s.dev
	if d == nil {
		return nil
	}
	s.dev = nil
	err := d.releaseNative()
	if err != nil {
		s.mu.Lock()
		s.cleanupErr = errors.Join(s.cleanupErr, ErrReleaseUnconfirmed, err)
		s.mu.Unlock()
	}
	return err
}

// expectedReleaseCancellation suppresses only the cancellation caused by an
// Immediate escalation. A device error established before that fence survives.
func (s *DSDSink) expectedReleaseCancellation(e error) bool {
	return s.immediate() && (errors.Is(e, context.Canceled) || ReasonOf(e) == ReasonCancelled)
}
