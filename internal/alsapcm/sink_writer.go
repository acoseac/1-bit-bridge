package alsapcm

import (
	"context"
	"fmt"
	"time"
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
			if reply, err := s.do(c); reply {
				c.reply <- err
			}
			continue
		}
		if s.drainCmd != nil && s.drainCmd.ctx.Err() != nil {
			s.abandonDrain()
			continue
		}
		switch s.state {
		case SinkPlaying, SinkDraining:
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

// do carries out one command, and reports whether it is answered now: a
// Drain is answered when the drain ends.
func (s *Sink) do(c *sinkCmd) (bool, error) {
	switch c.op {
	case opPause:
		return true, s.pause()
	case opResume:
		return true, s.resume()
	case opRecover:
		return true, s.recover()
	case opDrain:
		return s.startDrain(c)
	}
	return true, s.drop()
}

// waitContext is the context a wait on the device runs with: it ends with
// the sink's life, at the next command (command cancels it), and when the
// Drain under way is cancelled (startDrain's callback cancels it). With a
// command already pending, or that Drain's context already ended, it is
// done, so the wait ends at once: the callback cancels under the lock this
// looks under, so a cancel that came after the loop last looked at the
// drain is seen here or cancels what this returns, never neither.
func (s *Sink) waitContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cmds) > 0 || (s.drainCmd != nil && s.drainCmd.ctx.Err() != nil) {
		return doneCtx
	}
	if s.wake == nil || s.wake.Err() != nil {
		s.wake, s.wakeStop = context.WithCancel(s.life)
	}
	return s.wake
}

// feed writes the next frames of the lookahead, at most a period, and
// measures. With nothing to write it waits on the stream for frames or a
// command (watch), or, draining, has the kernel play out the buffer.
func (s *Sink) feed() {
	ctx := s.waitContext()
	s.mu.Lock()
	chunk := s.ring.span(s.ring.wr, min(s.ring.pending(), int64(s.params.PeriodFrames)))
	s.mu.Unlock()
	if len(chunk) == 0 {
		if s.state == SinkDraining {
			s.finishDrain()
		} else {
			s.watch()
		}
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

// watch waits, with the stream open and nothing to write, for frames, a
// command or the end of the sink's life. A stream that runs plays on
// meanwhile, and the kernel can stop it with no write to find out: an
// underrun, a suspend, a device gone each raise an error condition, which
// the wait watches for (Device.wait, settle). It measures each period, so
// the position moves while the producer is late. A Write that submits
// frames while the writer watches ends the wait.
func (s *Sink) watch() {
	ctx := s.waitContext()
	s.mu.Lock()
	if s.ring.pending() > 0 {
		s.mu.Unlock()
		return
	}
	s.watching = true
	s.mu.Unlock()
	rev, err := s.dev.wait(ctx, max(s.params.PeriodTime(), time.Millisecond))
	s.mu.Lock()
	s.watching = false
	s.mu.Unlock()
	s.settle(ctx, rev, err, "wait")
	if err == nil && rev == 0 {
		if _, err := s.measure(); err != nil {
			s.fault(err, "status")
		}
	}
}

// idle waits for a command, or the end of the sink's life, where there is
// no stream to watch: stopped at an xrun, or released.
func (s *Sink) idle() {
	select {
	case <-s.kick:
	case <-s.life.Done():
		err := &Error{Reason: ReasonCancelled, Op: "sink", Endpoint: s.cfg.Endpoint, Detail: "the sink's context ended", Err: s.life.Err()}
		s.end(err, err)
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
		err := &Error{Reason: ReasonBadState, Op: "write", Endpoint: s.cfg.Endpoint,
			Detail: "the record of content and silence written is full: the position could not be kept"}
		s.end(err, err)
		return false
	}
	s.unexplained = 0
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
			s.end(err, err)
		}
	case ReasonXrun:
		s.xrun(err, op)
	default:
		s.end(err, err)
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
// the underrun and the stop is the driver's to say, not the sink's. A
// Drain under way ends with the xrun.
func (s *Sink) xrun(err error, op string) {
	s.xrunPaused = s.state == SinkPaused
	s.runPlayed = s.run.written
	s.run.fold(s.run.written)
	at := s.now()
	s.mu.Lock()
	s.pos.Played, s.pos.Queued, s.pos.Stream, s.pos.Measured = s.ring.wr, 0, State(stateXrun), at
	s.gaps[s.ngaps%maxGaps] = Gap{At: s.ring.wr, Lookahead: s.ring.pending(), Op: op, Noticed: at}
	s.ngaps++
	s.setStateLocked(SinkXrun, err)
	s.mu.Unlock()
	s.answerDrain(err)
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
// with err otherwise. A Drain under way is answered with drainErr, and the
// commands still waiting with the end.
func (s *Sink) end(err, drainErr error) {
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
	s.answerDrain(drainErr)
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

// badState refuses op in the sink's state. A sink an xrun stopped refuses
// with the xrun, as Write does, so a caller that recovers on an xrun
// recovers whichever call found it.
func (s *Sink) badState(op string) error {
	detail := "the sink is " + s.state.String()
	switch s.state {
	case SinkXrun:
		return &Error{Reason: ReasonOf(s.err), Op: op, Endpoint: s.cfg.Endpoint, Detail: "the sink stopped at an xrun: recover or drop it", Err: s.err}
	case SinkPaused, SinkReleased:
		detail = "the sink is " + s.state.String() + ": resume or drop it"
	case SinkPlaying, SinkDraining:
		detail = "the sink is " + s.state.String() + ": nothing stopped it"
	}
	return &Error{Reason: ReasonBadState, Op: op, Endpoint: s.cfg.Endpoint, Detail: detail}
}

// pause pauses: a stream that has not started by writing nothing more; one
// that runs with the kernel's PAUSE when the mode allows and the hardware
// offers it, and otherwise with zeros, which the writer then writes after
// what is queued. A pause by zeros needs two free segments in the run's
// record, its silence and the content after it. A drain under way waits
// for the resume.
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
	s.settle(ctx, rev, err, "pause")
}

// settle settles how a wait on the stream ended (watch, hold), op naming
// the wait: a cancel ends the sink when its life is over, and is otherwise
// a command or frames for the writer; a timeout is nothing; an error
// condition is a device gone, or a state explain types. One the state
// explains away twice in a row is a kernel that keeps waking the wait for
// nothing, and ends the sink.
func (s *Sink) settle(ctx context.Context, rev int16, err error, op string) {
	switch {
	case err != nil:
		if ctx.Err() == nil {
			kerr := s.dev.kernelError("poll", err)
			s.end(kerr, kerr)
		} else if s.life.Err() != nil {
			cerr := &Error{Reason: ReasonCancelled, Op: op, Endpoint: s.cfg.Endpoint, Err: s.life.Err()}
			s.end(cerr, cerr)
		}
	case rev == 0:
		s.unexplained = 0
	case rev&(pollHup|pollNval) != 0:
		derr := &Error{Reason: ReasonDisconnected, Op: op, Endpoint: s.cfg.Endpoint, Detail: "the device went away", Err: pollEvents(rev)}
		s.end(derr, derr)
	case rev&pollErr != 0:
		if err := s.dev.explain(op); err != nil {
			s.fault(err, op)
			return
		}
		if s.unexplained++; s.unexplained > 1 {
			kerr := &Error{Reason: ReasonKernel, Op: op, Endpoint: s.cfg.Endpoint,
				Detail: "poll keeps reporting an error the stream's state does not explain"}
			s.end(kerr, kerr)
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
		err := &Error{Reason: ReasonBadState, Op: "release", Endpoint: s.cfg.Endpoint, Detail: fmt.Sprintf(
			"the %d frames the DAC had not played are not all held (%d are): a release would skip them", unplayed, kept)}
		s.end(err, err)
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

// playing is the state a sink plays on in: draining when a Drain waits.
func (s *Sink) playing() SinkState {
	if s.drainCmd != nil {
		return SinkDraining
	}
	return SinkPlaying
}

// resume plays on: PAUSE released, zeros no longer written, or, for a
// released sink, the device opened again.
func (s *Sink) resume() error {
	switch s.state {
	case SinkPlaying, SinkDraining:
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
	s.setStateLocked(s.playing(), nil)
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
	if err := admit(s.cfg, dev, "resume"); err != nil {
		_ = dev.Close()
		return err
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
	s.setStateLocked(s.playing(), nil)
	s.mu.Unlock()
	return nil
}

// recover prepares a stream an xrun stopped, and plays on from the next
// content frame: a new run starts where the content played ends. A sink
// the xrun stopped while paused (by zeros that came too late) stays paused
// on the prepared stream, writing nothing until Resume, and its release
// stays due when the pause began.
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
	if s.xrunPaused {
		s.pausedBy = pausedIdle
		s.setStateLocked(SinkPaused, nil)
	} else {
		s.setStateLocked(SinkPlaying, nil)
	}
	s.mu.Unlock()
	return nil
}

// startDrain takes a Drain: the producer is done, and the writer goes on
// writing what the store holds, pauses included, until it is empty and
// finishDrain has the kernel play out the buffer. It is answered then, or
// when the drain ends some other way. The drain's context ending wakes the
// writer, which drops the stream (abandonDrain). The callback that wakes it
// cancels a wait only while its own Drain is under way: one that runs late,
// after the Drain was answered (an xrun), leaves the next Drain's waits
// alone.
func (s *Sink) startDrain(c *sinkCmd) (bool, error) {
	if s.state != SinkPlaying {
		return true, s.badState("drain")
	}
	s.mu.Lock()
	s.drainCmd = c
	s.finishing = true
	s.setStateLocked(SinkDraining, nil)
	s.mu.Unlock()
	s.drainStop = s.afterFunc(c.ctx, func() {
		s.mu.Lock()
		if s.drainCmd == c && s.wakeStop != nil {
			s.wakeStop()
		}
		s.mu.Unlock()
		poke(s.kick)
	})
	return false, nil
}

// answerDrain answers the Drain under way, if there is one, with err, once
// the sink no longer counts it as under way: its caller acts on the answer
// at once, and a Recover sent the moment a Drain reports an xrun must not
// find the drain still playing out its last frames.
func (s *Sink) answerDrain(err error) {
	c := s.drainCmd
	if c == nil {
		return
	}
	s.drainStop()
	s.drainStop = nil
	s.mu.Lock()
	s.drainCmd = nil
	s.finishing, s.finalDrain = false, false
	s.mu.Unlock()
	c.reply <- err
}

// finishDrain has the kernel play out the buffer once the store is empty,
// with Device.Drain's bound, closes the device, and answers the Drain: the
// sink is stopped. A command that came first is taken first; once the
// kernel drains, the sender lets nothing but a Drop through, since an
// interrupted Device.Drain drops the stream and cuts off its end.
func (s *Sink) finishDrain() {
	s.mu.Lock()
	if len(s.cmds) > 0 {
		s.mu.Unlock()
		return
	}
	s.finalDrain = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.finalDrain = false
		s.mu.Unlock()
	}()
	dctx, cancel := context.WithCancel(s.waitContext())
	defer cancel()
	defer s.afterFunc(s.drainCmd.ctx, cancel)()
	if err := s.dev.Drain(dctx, s.params.BufferTime()+drainSlack); err != nil {
		s.drainFailed(err)
		return
	}
	s.runPlayed = s.run.written
	s.run.fold(s.run.written)
	at := s.now()
	s.mu.Lock()
	s.pos.Played, s.pos.Queued, s.pos.Stream, s.pos.Measured = s.ring.wr, 0, State(stateSetup), at
	s.mu.Unlock()
	s.end(nil, nil)
}

// drainFailed settles a kernel drain that did not reach its end: an xrun
// stops the sink as anywhere else, and the Drain says so; a cancel ends
// the sink when its life is over, and stops it when the Drain's own
// context ended; a Drop woke it, and the Drop, taken next, stops the sink
// and answers the Drain; anything else ends the sink.
func (s *Sink) drainFailed(err error) {
	switch ReasonOf(err) {
	case ReasonXrun:
		s.xrun(err, "drain")
	case ReasonCancelled:
		switch {
		case s.life.Err() != nil:
			s.end(err, err)
		case s.drainCmd.ctx.Err() != nil:
			s.end(nil, err)
		}
	default:
		s.end(err, err)
	}
}

// abandonDrain stops a sink whose Drain's context ended before the drain
// did: the stream is dropped, and the Drain is answered with the cancel.
func (s *Sink) abandonDrain() {
	err := &Error{Reason: ReasonCancelled, Op: "drain", Endpoint: s.cfg.Endpoint, Detail: "the drain's context ended: dropped", Err: s.drainCmd.ctx.Err()}
	if s.dev != nil && s.state != SinkXrun {
		_, _ = s.measure()
	}
	s.end(nil, err)
}

// drop stops the stream at once and closes the device: the sink is
// stopped, and a Drain under way says it was dropped. Where a stream is
// open, the position is measured first.
func (s *Sink) drop() error {
	if s.dev != nil && (s.state == SinkPlaying || s.state == SinkPaused || s.state == SinkDraining) {
		_, _ = s.measure()
	}
	s.end(nil, &Error{Reason: ReasonCancelled, Op: "drain", Endpoint: s.cfg.Endpoint, Detail: "dropped while draining"})
	return nil
}
