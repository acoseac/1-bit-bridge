package alsapcm

import (
	"context"
	"fmt"
)

// How a pause pauses, as SinkStats.PausedBy names it.
const (
	pausedKernel = "pause" // the kernel's PAUSE
	pausedZeros  = "zeros" // the stream runs on, on zeros
	pausedIdle   = "idle"  // the stream had not started: nothing is written
)

// doneCtx is a context that is already done. A wait the writer starts with
// a command pending runs with it, so the wait ends at once and the writer
// takes the command.
var doneCtx = func() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}()

// loop is the writer, the one goroutine that drives the device. It takes
// one command at a time, and between commands feeds the device, holds a
// pause, or waits.
func (s *Sink) loop() {
	defer close(s.exited)
	for {
		c, done := s.next()
		if done {
			return
		}
		if c != nil {
			c.reply <- s.do(c)
			continue
		}
		switch s.state {
		case SinkPlaying:
			s.feed()
		case SinkPaused:
			s.hold()
		default:
			s.idle()
		}
	}
}

// next takes the next command, and reports whether the sink is done.
func (s *Sink) next() (*sinkCmd, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == SinkStopped || s.state == SinkEnded {
		return nil, true
	}
	if len(s.cmds) == 0 {
		return nil, false
	}
	c := s.cmds[0]
	s.cmds[0] = nil
	s.cmds = s.cmds[1:]
	return c, false
}

// do carries out one command.
func (s *Sink) do(c *sinkCmd) error {
	switch c.op {
	case opPause:
		return s.pause()
	case opResume:
		return s.resume()
	case opRecover:
		return s.recover()
	case opDrain:
		return s.drain(c.ctx)
	}
	return s.drop()
}

// waitContext is the context a wait on the device runs with: it ends with
// the sink's life, and at the next command (command cancels it). With a
// command already pending it is done, so the wait ends at once.
func (s *Sink) waitContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cmds) > 0 {
		return doneCtx
	}
	if s.wake == nil || s.wake.Err() != nil {
		s.wake, s.wakeStop = context.WithCancel(s.life)
	}
	return s.wake
}

// feed writes the next frames of the lookahead, at most a period, and
// measures; with nothing to write it waits for frames or a command.
func (s *Sink) feed() {
	ctx := s.waitContext()
	s.mu.Lock()
	chunk := s.ring.span(s.ring.wr, min(s.ring.pending(), int64(s.params.PeriodFrames)))
	s.mu.Unlock()
	if len(chunk) == 0 {
		s.idle()
		return
	}
	n, err := s.dev.Write(ctx, chunk)
	if !s.wrote(int64(n), false) {
		return
	}
	if err != nil {
		s.fault(err, "write")
		return
	}
	if _, err := s.measure(); err != nil {
		s.fault(err, "status")
	}
}

// idle waits for frames or a command, or the end of the sink's life.
func (s *Sink) idle() {
	select {
	case <-s.kick:
	case <-s.life.Done():
		s.end(&Error{Reason: ReasonCancelled, Op: "sink", Endpoint: s.cfg.Endpoint, Detail: "the sink's context ended", Err: s.life.Err()})
	}
}

// wrote records n frames written to the kernel, content or silence. It
// reports false, having ended the sink, when the run's record has no room
// for them: a pause by zeros reserves the segments it needs, so that is a
// defect, and a position kept without the record would lie.
func (s *Sink) wrote(n int64, silence bool) bool {
	if n <= 0 {
		return true
	}
	if !s.run.add(n, silence) {
		s.end(&Error{Reason: ReasonBadState, Op: "write", Endpoint: s.cfg.Endpoint,
			Detail: "the record of content and silence written is full: the position could not be kept"})
		return false
	}
	c := s.dev.Counters()
	s.mu.Lock()
	s.live = c
	if silence {
		s.silence += n
	} else {
		s.ring.written(n)
	}
	s.mu.Unlock()
	if !silence {
		poke(s.room)
	}
	return true
}

// fault settles an error from the device. A cancel ends a wait for a
// command, unless the sink's life is over; an xrun stops the sink until
// Recover; anything else, a disconnect, a suspend, a stall, ends it.
func (s *Sink) fault(err error, op string) {
	switch ReasonOf(err) {
	case ReasonCancelled:
		if s.life.Err() != nil {
			s.end(err)
		}
	case ReasonXrun:
		s.xrun(err, op)
	default:
		s.end(err)
	}
}

// measure asks the kernel how much of the run it has played and
// establishes the content played from it, and returns the stream's state.
// While the stream runs or drains, what it holds is STATUS's delay, the
// driver's own included; before it starts, all of the run, though STATUS
// reports a delay of 0 there; while it is paused, where the delay is 0
// too, what lies between its pointers. A reading this cannot use leaves
// what was established.
func (s *Sink) measure() (State, error) {
	st, err := s.dev.Status()
	if err != nil {
		return 0, err
	}
	var queued int64
	switch st.State {
	case stateRunning, stateDraining:
		if st.Delay < 0 {
			return st.State, nil
		}
		queued = min(st.Delay, s.run.written)
	case statePrepared:
		queued = s.run.written
	case statePaused:
		queued = int64(st.ApplPtr - st.HWPtr)
		if queued < 0 || queued > s.run.written {
			return st.State, nil
		}
	default:
		return st.State, s.dev.stateError("status", int32(st.State))
	}
	s.establish(s.run.written-queued, st.State, queued)
	return st.State, nil
}

// establish records that the kernel has played the first p frames of the
// run, and the content among them, past what the run began with, as
// played. Neither goes back, and the position never passes what was
// written.
func (s *Sink) establish(p int64, stream State, queued int64) {
	s.runPlayed = max(s.runPlayed, p)
	s.run.fold(s.runPlayed)
	played := s.run.base + s.run.contentAmong(s.runPlayed)
	at := s.now()
	s.mu.Lock()
	s.pos.Played = min(max(s.pos.Played, played), s.ring.wr)
	s.pos.Stream, s.pos.Queued, s.pos.Measured = stream, queued, at
	s.mu.Unlock()
}

// xrun stops the sink at an underrun. The kernel stops a stream once its
// pointer reaches the last frame written, so every frame written was
// consumed: the content played is all of it, the gap is at its end, and
// the frames still held play after Recover. What the DAC was sent between
// the underrun and the stop is the driver's to say, not the sink's.
func (s *Sink) xrun(err error, op string) {
	s.runPlayed = s.run.written
	s.run.fold(s.run.written)
	at := s.now()
	s.mu.Lock()
	s.pos.Played, s.pos.Queued, s.pos.Stream, s.pos.Measured = s.ring.wr, 0, State(stateXrun), at
	s.gaps[s.ngaps%maxGaps] = Gap{At: s.ring.wr, Lookahead: s.ring.pending(), Op: op, Noticed: at}
	s.ngaps++
	s.setStateLocked(SinkXrun, err)
	s.mu.Unlock()
}

// setStateLocked moves the sink to st, with s.mu held, and tells whoever
// waits on Changed, or for room.
func (s *Sink) setStateLocked(st SinkState, err error) {
	s.state, s.err = st, err
	close(s.changed)
	s.changed = make(chan struct{})
	poke(s.room)
}

// end closes the device and ends the sink: stopped when err is nil, ended
// with err otherwise. Commands still waiting are answered with the end.
func (s *Sink) end(err error) {
	s.closeDevice()
	st := SinkStopped
	if err != nil {
		st = SinkEnded
	}
	s.mu.Lock()
	s.setStateLocked(st, err)
	cmds := s.cmds
	s.cmds = nil
	if s.wakeStop != nil {
		s.wakeStop()
	}
	s.mu.Unlock()
	for _, c := range cmds {
		c.reply <- s.doneError(c.op)
	}
}

// doneError is the answer to op once the sink is done: nothing for a
// Drop, and why the sink stopped for anything else.
func (s *Sink) doneError(op sinkOp) error {
	switch {
	case op == opDrop:
		return nil
	case s.state == SinkStopped:
		return s.stoppedError(sinkOpNames[op])
	}
	return &Error{Reason: ReasonOf(s.err), Op: sinkOpNames[op], Endpoint: s.cfg.Endpoint, Detail: "the sink has ended", Err: s.err}
}

// closeDevice closes the device, which drops the stream and frees its
// parameters first, once. close(2)'s own error changes nothing: the
// descriptor is gone either way.
func (s *Sink) closeDevice() {
	if s.dev == nil {
		return
	}
	c := s.dev.Counters()
	_ = s.dev.Close()
	s.dev = nil
	s.mu.Lock()
	s.counters = addCounters(s.counters, c)
	s.live = Counters{}
	s.mu.Unlock()
}

// addCounters sums two devices' counters.
func addCounters(a, b Counters) Counters {
	return Counters{Writes: a.Writes + b.Writes, Partial: a.Partial + b.Partial, Waits: a.Waits + b.Waits, Interrupted: a.Interrupted + b.Interrupted}
}

// badState refuses op in the sink's state.
func (s *Sink) badState(op string) error {
	detail := "the sink is " + s.state.String()
	switch s.state {
	case SinkXrun:
		detail = "the sink stopped at an xrun: recover or drop it"
	case SinkPaused, SinkReleased:
		detail = "the sink is " + s.state.String() + ": resume or drop it"
	case SinkPlaying:
		detail = "the sink is playing: nothing stopped it"
	}
	return &Error{Reason: ReasonBadState, Op: op, Endpoint: s.cfg.Endpoint, Detail: detail}
}

// pause pauses: a stream that has not started by writing nothing more; one
// that runs with the kernel's PAUSE when the mode allows and the hardware
// offers it, and otherwise with zeros, which the writer then writes after
// what is queued. A pause by zeros needs two free segments in the run's
// record, its silence and the content after it.
func (s *Sink) pause() error {
	switch s.state {
	case SinkPaused, SinkReleased:
		return nil
	case SinkXrun:
		return s.badState("pause")
	}
	stream, err := s.measure()
	if err != nil {
		s.fault(err, "pause")
		return err
	}
	by := pausedZeros
	switch {
	case stream == statePrepared:
		by = pausedIdle
	case s.cfg.Pause != PauseZeros && s.params.CanPause():
		err := s.dev.Pause(true)
		switch {
		case err == nil:
			by = pausedKernel
			if _, err := s.measure(); err != nil {
				s.fault(err, "pause")
				return err
			}
		case ReasonOf(err) == ReasonNotSupported && s.cfg.Pause == PauseAuto:
			// The setup offered PAUSE and the kernel refused it: zeros.
		case ReasonOf(err) == ReasonNotSupported:
			return err
		default:
			s.fault(err, "pause")
			return err
		}
	}
	if by == pausedZeros && s.run.room() < 2 {
		return &Error{Reason: ReasonBadState, Op: "pause", Endpoint: s.cfg.Endpoint,
			Detail: "too many pauses by zeros within one hardware buffer: pause again once it has played"}
	}
	s.unexplained = 0
	s.deadline = s.now().Add(s.cfg.ReleaseAfter)
	s.mu.Lock()
	s.pausedBy = by
	s.pauses++
	s.setStateLocked(SinkPaused, nil)
	s.mu.Unlock()
	return nil
}

// hold keeps a pause until a command or the release. A pause by zeros
// writes a period of zeros and measures; a pause by PAUSE, or of a stream
// that had not started, waits for an error condition on the stream (a
// suspend, a device gone) until the release is due.
func (s *Sink) hold() {
	left := s.deadline.Sub(s.now())
	if left <= 0 {
		s.release()
		return
	}
	ctx := s.waitContext()
	if s.pausedBy == pausedZeros {
		n, err := s.dev.Write(ctx, s.zeros)
		if !s.wrote(int64(n), true) {
			return
		}
		if err != nil {
			s.fault(err, "pause")
			return
		}
		if _, err := s.measure(); err != nil {
			s.fault(err, "status")
		}
		return
	}
	rev, err := s.dev.wait(ctx, left)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			s.end(s.dev.kernelError("poll", err))
		} else if s.life.Err() != nil {
			s.end(&Error{Reason: ReasonCancelled, Op: "pause", Endpoint: s.cfg.Endpoint, Err: s.life.Err()})
		}
	case rev == 0:
		s.unexplained = 0
	case rev&(pollHup|pollNval) != 0:
		s.end(&Error{Reason: ReasonDisconnected, Op: "pause", Endpoint: s.cfg.Endpoint, Detail: "the device went away", Err: pollEvents(rev)})
	case rev&pollErr != 0:
		if err := s.dev.explain("pause"); err != nil {
			s.fault(err, "pause")
			return
		}
		if s.unexplained++; s.unexplained > 1 {
			s.end(&Error{Reason: ReasonKernel, Op: "pause", Endpoint: s.cfg.Endpoint,
				Detail: "poll keeps reporting an error the stream's state does not explain"})
		}
	}
}

// release lets the device go after a pause has held it for ReleaseAfter,
// and keeps the position: the frames the DAC had not played are taken back
// from what was written, and written again first when Resume opens the
// device again. Frames the store no longer holds cannot be given again,
// and the sink ends rather than skip them.
func (s *Sink) release() {
	if _, err := s.measure(); err != nil {
		s.fault(err, "pause")
		return
	}
	unplayed := s.run.content - s.run.contentAmong(s.runPlayed)
	s.mu.Lock()
	kept := s.ring.wr - s.ring.base
	s.mu.Unlock()
	if unplayed > kept {
		s.end(&Error{Reason: ReasonBadState, Op: "release", Endpoint: s.cfg.Endpoint, Detail: fmt.Sprintf(
			"the %d frames the DAC had not played are not all held (%d are): a release would skip them", unplayed, kept)})
		return
	}
	s.closeDevice()
	s.mu.Lock()
	s.ring.wr -= unplayed
	s.pos.Played, s.pos.Queued = s.ring.wr, 0
	s.releases++
	s.setStateLocked(SinkReleased, nil)
	s.mu.Unlock()
}

// resume plays on: PAUSE released, zeros no longer written, or, for a
// released sink, the device opened again.
func (s *Sink) resume() error {
	switch s.state {
	case SinkPlaying:
		return nil
	case SinkReleased:
		return s.reopen()
	case SinkXrun:
		return s.badState("resume")
	}
	if s.pausedBy == pausedKernel {
		if err := s.dev.Pause(false); err != nil {
			s.fault(err, "resume")
			return err
		}
		if _, err := s.measure(); err != nil {
			s.fault(err, "status")
			return err
		}
	}
	s.mu.Lock()
	s.setStateLocked(SinkPlaying, nil)
	s.mu.Unlock()
	return nil
}

// reopen opens the device again for a released sink, for the same
// endpoint and parameters, which the kernel commits exactly or refuses. A
// refusal, busy above all, leaves the sink released. A new run starts at
// the first content frame the DAC had not played.
func (s *Sink) reopen() error {
	dev, err := s.open(s.cfg.Endpoint, s.devCfg)
	if err != nil {
		return err
	}
	p := dev.Params()
	if err := checkSetup(s.cfg, p); err != nil {
		_ = dev.Close()
		return err
	}
	if int64(p.BufferFrames)+int64(p.PeriodFrames) > s.ring.frames {
		_ = dev.Close()
		return &Error{Reason: ReasonBadConfig, Op: "resume", Endpoint: s.cfg.Endpoint, Detail: fmt.Sprintf(
			"the device came back with a buffer of %d frames, which the store of %d cannot hold with a period more", p.BufferFrames, s.ring.frames)}
	}
	if need := p.PeriodFrames * p.FrameBytes(); need > cap(s.zeros) {
		s.zeros = make([]byte, need)
	} else {
		s.zeros = s.zeros[:need]
	}
	s.dev = dev
	s.runPlayed = 0
	s.mu.Lock()
	s.run = run{base: s.ring.wr}
	s.params = p
	s.ring.keep = int64(p.BufferFrames)
	s.pos.Stream, s.pos.Queued = State(statePrepared), 0
	s.reopens++
	s.setStateLocked(SinkPlaying, nil)
	s.mu.Unlock()
	return nil
}

// recover prepares a stream an xrun stopped, and plays on from the next
// content frame: a new run starts where the content played ends.
func (s *Sink) recover() error {
	if s.state != SinkXrun {
		return s.badState("recover")
	}
	if err := s.dev.Prepare(); err != nil {
		s.fault(err, "recover")
		return err
	}
	at := s.now()
	s.runPlayed = 0
	s.mu.Lock()
	s.run = run{base: s.ring.wr}
	s.gaps[(s.ngaps-1)%maxGaps].Recovered = at
	s.pos.Stream, s.pos.Queued = State(statePrepared), 0
	s.setStateLocked(SinkPlaying, nil)
	s.mu.Unlock()
	return nil
}

// drain writes what the store holds, has the kernel play out the buffer
// (Device.Drain, bounded by the buffer's time and drainSlack), and closes
// the device: the sink is stopped. Paused, released or stopped by an
// xrun, a sink has nothing it may play out until it is resumed or
// recovered.
func (s *Sink) drain(ctx context.Context) error {
	if s.state != SinkPlaying {
		return s.badState("drain")
	}
	s.mu.Lock()
	s.setStateLocked(SinkDraining, nil)
	s.mu.Unlock()
	dctx, cancel := context.WithCancel(s.waitContext())
	defer cancel()
	defer context.AfterFunc(ctx, cancel)()
	for {
		s.mu.Lock()
		chunk := s.ring.span(s.ring.wr, min(s.ring.pending(), int64(s.params.PeriodFrames)))
		s.mu.Unlock()
		if len(chunk) == 0 {
			break
		}
		n, err := s.dev.Write(dctx, chunk)
		if !s.wrote(int64(n), false) {
			return s.err
		}
		if err == nil {
			_, err = s.measure()
		}
		if err != nil {
			return s.drainFailed(ctx, err)
		}
	}
	if err := s.dev.Drain(dctx, s.params.BufferTime()+drainSlack); err != nil {
		return s.drainFailed(ctx, err)
	}
	s.runPlayed = s.run.written
	s.run.fold(s.run.written)
	at := s.now()
	s.mu.Lock()
	s.pos.Played, s.pos.Queued, s.pos.Stream, s.pos.Measured = s.ring.wr, 0, State(stateSetup), at
	s.mu.Unlock()
	s.end(nil)
	return nil
}

// drainFailed settles a drain that did not reach its end: an xrun stops the
// sink as anywhere else; a cancel ends the sink when its life is over, and
// stops it when the drain's own context ended; a Drop from another
// goroutine woke it, and the Drop, taken next, stops the sink; anything
// else ends the sink.
func (s *Sink) drainFailed(ctx context.Context, err error) error {
	switch ReasonOf(err) {
	case ReasonXrun:
		s.xrun(err, "drain")
		return err
	case ReasonCancelled:
		switch {
		case s.life.Err() != nil:
			s.end(err)
		case ctx.Err() != nil:
			s.end(nil)
		default:
			return &Error{Reason: ReasonCancelled, Op: "drain", Endpoint: s.cfg.Endpoint, Detail: "dropped while draining"}
		}
		return err
	}
	s.end(err)
	return err
}

// drop stops the stream at once and closes the device: the sink is
// stopped. Where a stream is open, the position is measured first.
func (s *Sink) drop() error {
	if s.dev != nil && (s.state == SinkPlaying || s.state == SinkPaused || s.state == SinkDraining) {
		_, _ = s.measure()
	}
	s.end(nil)
	return nil
}
