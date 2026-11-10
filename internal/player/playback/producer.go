package playback

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdconv"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// stepKind is what the producer does next.
type stepKind int

const (
	stepStop       stepKind = iota // the session faulted, or ends
	stepWrite                      // the next chunk of the occurrence it writes
	stepWait                       // nothing to do until woken: a successor named or opened, the end, a resume
	stepTransition                 // drain the sink, close it, and open one for the next occurrence's transport
	stepFinish                     // play out the last content: finished, or the decode failure's fault
)

// String names k as the ProducerStep seam says it.
func (k stepKind) String() string {
	switch k {
	case stepStop:
		return "stop"
	case stepWrite:
		return "write"
	case stepWait:
		return "wait"
	case stepTransition:
		return "transition"
	case stepFinish:
		return "finish"
	}
	return "unknown"
}

type step struct {
	install bool
	// decided is a converted occurrence's join window decided (holdLocked):
	// what follows was settled, or the window ends, and the producer reads the
	// window next.
	decided bool
	kind    stepKind
	o       *occ
	wait    <-chan struct{}
	// awaits is the occurrence a wait is for while a choice's write is
	// pending that plays its rate otherwise (the ChoiceAwaited seam).
	awaits *occ
	// verdict is the occurrence a wait is for while the reconciliation of a
	// set published since its admission has yet to judge it (unjudgedLocked,
	// the Unjudged seam, B728 B9 round 17).
	verdict *occ
}

// produce is the session's producer, the one goroutine that decodes, packs
// and writes, and the only one in the sink's producer side while it runs
// (Write, Drain, Cut, a transport's open). It writes each occurrence's
// frames straight after the last one's when their transports are one, with
// nothing between; for another transport it drains the sink, closes it and
// opens one for it, an honest gap; and once nothing follows it plays out
// what it gave the sink. It writes no frame twice and skips none, and never
// calls Recover: a sink that stops at an xrun or ends stops it, and the
// reporter reports the fault.
func (s *session) produce() {
	defer s.producerExit()
	samples := make([]int32, chunkFrames*alsapcm.Channels)
	packed := make([]byte, chunkFrames*alsapcm.Channels*4)
	for {
		st := s.next()
		s.b.producerStep(s.id, st)
		switch st.kind {
		case stepWrite:
			if st.install {
				s.installed(st.o, st.o.out, false)
			}
			if st.decided {
				s.b.converting("decided", st.o.id)
			}
			if !s.writeChunk(st.o, samples, packed) {
				return
			}
		case stepWait:
			if st.awaits != nil {
				s.b.choiceAwaited(st.awaits.id)
			}
			if st.verdict != nil {
				s.b.unjudged(st.verdict.id)
			}
			if !s.waitWake(st.wait) {
				return
			}
		case stepTransition:
			if !s.transition(st.o) {
				return
			}
		case stepFinish:
			s.finish()
			return
		default:
			return
		}
	}
}

// producerExit answers the cuts still asked of the producer, which the
// callers make themselves from now on, and tells Start, when its first
// frames never reached the sink.
func (s *session) producerExit() {
	l := s.hold()
	defer l.release()
	l.lock()
	s.producerDone = true
	s.serveLocked()
	l.unlock()
	s.signalFirst(errors.New("player backend: the session stopped before its first frames reached the sink"))
	wake(s.wakeR)
}

// next decides the producer's next step.
func (s *session) next() step {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serveLocked()
	s.awaitingWrite = false
	if s.ctx.Err() != nil || s.fault != nil {
		return step{kind: stepStop}
	}
	if w := s.writing; w != nil {
		if !w.dropped {
			if w.stale {
				// Being handed off again (B728 B7): the engine's replacement
				// drops it, and wakes the producer.
				return step{kind: stepWait}
			}
			if w.holding {
				// A converted occurrence at its window point (B728 B11): its
				// last frames wait for what follows.
				return s.holdLocked(w)
			}
			return step{kind: stepWrite, o: w}
		}
		s.writing = nil
		s.closeOcc(w)
	}
	if s.decodeErr != nil {
		return step{kind: stepFinish}
	}
	o := s.toWriteLocked()
	if o == nil {
		if t := s.tailLocked(); t != nil && t.eof && s.endAfter == t.id {
			s.ending = true
			return step{kind: stepFinish}
		}
		return step{kind: stepWait}
	}
	if !o.opened() {
		return step{kind: stepWait, wait: o.done}
	}
	if o.stale {
		return step{kind: stepWait}
	}
	// One answer decides a join, Prepared's and this: Transport.Join, which
	// tells same-carrier PCM from DoP by kind, valid bits, logical rate and
	// packing version, never by format and rate alone; a DSD successor
	// joins only under its sink's policy too.
	c := s.cur
	gapless := c != nil && !c.drained && !c.closed && c.transport.Join(o.transport).Gapless && (o.plan == nil || c.policyID == o.plan.PolicyID)
	if p := s.b.pendingFor(o); p != nil {
		// A choice's write pending would play its rate otherwise (B728 B9): it
		// stays out of every sink until the write settles, so the write's
		// classification of it (a re-plan, which interrupts nothing) still
		// holds when the write publishes. Out of a sink it would join, it waits
		// only while that sink holds more than its floor of lookahead (round
		// 2): past it the sink would run dry, so the sink drains and plays out
		// what it holds, an honest gap, and the successor's own sink opens
		// once the write settles (transition), as one of another transport's
		// does, its sink before it drained first.
		if gapless && !outFloorLocked(c) {
			s.awaitingWrite = true
			return step{kind: stepWait, wait: p.done, awaits: o}
		}
		return step{kind: stepTransition, o: o}
	}
	if gapless {
		if err := s.b.checkEdge(o.transport, o.plan, o.permission); err != nil {
			o.refused = &engine.Unplayable{Reason: "transition_unapproved", Detail: err.Error()}
			o.dropped = true
			wake(s.wakeR)
			return step{kind: stepFinish}
		}
		if s.unjudgedLocked(o) {
			// A gapless DSD successor joins only under the set published now,
			// asked with no file read or lock on the audio path (B728 B7): one
			// admitted under a set a refresh, or a choice's write, has replaced
			// waits for that publish's reconciliation, which keeps it under the
			// new set, hands it off again or revokes the session's DSD, and wakes
			// the producer (judged). Only while its sink holds more than its floor
			// (B728 B9 round 17), as for a write pending: past it the sink would
			// run dry, so it drains for the successor's own transport, whose sink
			// opens once the successor is judged (transition).
			if outFloorLocked(c) {
				return step{kind: stepTransition, o: o}
			}
			s.awaitingWrite = true
			return step{kind: stepWait, wait: s.judged, verdict: o}
		}
		if o.plan != nil && o.permission != nil {
			s.mu.Unlock()
			err := s.b.checkPermission(s.ctx, o.permission, nil, edgeKey(o.transport, o.plan))
			if err != nil {
				s.b.Revoke(engine.RevokeInput{Session: s.id, Reason: "gapless DSD authorization changed"})
			}
			s.mu.Lock()
			if err != nil {
				o.refused = &engine.Unplayable{Reason: "native_revoked", Detail: err.Error()}
				o.dropped = true
				wake(s.wakeR)
				return step{kind: stepStop}
			}
			if s.ctx.Err() != nil || s.fault != nil {
				return step{kind: stepStop}
			}
			if o.dropped || s.cur != c {
				return step{kind: stepWait}
			}
			if s.b.pendingFor(o) != nil {
				// A choice's write became pending while the lock was let go:
				// the next step decides with it, at once.
				return step{kind: stepWait, wait: ready}
			}
		}
		s.b.rememberEdge(o.transport, o.plan, o.permission)
		if o.conv != nil {
			// A converted occurrence in a sink opened for PCM (B728 B11): the
			// sink's reopens check the node as the DAC its set was computed for.
			c.carryDSD(o.admitted)
		}
		// Gapless: its frames follow the last ones in the same sink.
		o.out, o.start = c, c.submitted
		s.writing = o
		return step{kind: stepWrite, o: o, install: true}
	}
	return step{kind: stepTransition, o: o}
}

// ready is a wait that is over: the producer's next step decides again at
// once.
var ready = func() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// toWriteLocked is the first occurrence still to be written: not dropped
// and not all in its sink.
func (s *session) toWriteLocked() *occ {
	for _, o := range s.chain {
		if !o.dropped && !o.eof {
			return o
		}
	}
	return nil
}

// serveLocked makes the cuts asked of the producer, which is in no Write and
// no converter's read: a cut through a converted occurrence's join window
// (windowCutLocked) is made here alone, the producer's goroutine being the
// one that calls a converter, unless the producer is done, when the window
// can be submitted no more.
func (s *session) serveLocked() {
	for len(s.reqs) > 0 {
		r := s.reqs[0]
		s.reqs[0] = nil
		s.reqs = s.reqs[1:]
		var ok bool
		var err error
		switch {
		case r.o.dropped:
			ok = true
		case r.o.crossed:
		case r.o.inWindowLocked() && s.producerDone:
		case r.o.inWindowLocked():
			ok, err = s.windowCutLocked(r.o.contPred, r.o)
		default:
			ok, err = s.sinkCutLocked(r.o)
		}
		r.done <- cutAnswer{ok: ok, err: err}
	}
}

// waitWake waits to be woken, or for also, until the session ends.
func (s *session) waitWake(also <-chan struct{}) bool {
	select {
	case <-s.wakeP:
	case <-also:
	case <-s.ctx.Done():
		return false
	}
	return true
}

// writeChunk decodes o's next chunk (the one its open decoded first, to
// begin with), packs it in o's sink's format and writes it. The end of o is
// eof; a decode that fails after content played is the session's fault,
// once what was decoded before it has played (finish). It reports false
// when the sink stopped taking frames or the session ends.
func (s *session) writeChunk(o *occ, samples []int32, packed []byte) bool {
	if o.plan != nil {
		return s.writeNativeChunk(o)
	}
	if o.conv != nil {
		return s.writeConvertedChunk(o, samples, packed)
	}
	buf, err := o.first, o.firstErr
	o.first, o.firstErr = nil, nil
	if buf == nil {
		var n int
		n, err = o.dec.ReadFrames(s.ctx, samples)
		buf = samples[:n]
	}
	if len(buf) > 0 {
		n, perr := format.Pack(packed, buf, o.pcm.ValidBits, o.out.transport.Format)
		if perr != nil {
			s.failDecode(o, perr)
			return true
		}
		if !s.writeAll(o, packed[:n], o.epoch) {
			return false
		}
	}
	switch {
	case err == nil:
		return true
	case errors.Is(err, io.EOF):
		s.mu.Lock()
		if s.writing == o && !o.dropped {
			o.eof = true
			s.writing = nil
			s.closeOcc(o)
		}
		s.mu.Unlock()
		wake(s.wakeR)
		return true
	case s.ctx.Err() != nil:
		return false
	}
	s.failDecode(o, err)
	return true
}

// failDecode records a decode of o that failed after content played: the
// producer plays out what it wrote, and the session faults then.
func (s *session) failDecode(o *occ, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o.dropped || s.writing != o {
		return
	}
	s.decodeErr = &engine.Fault{Reason: engine.ReasonDecodeFailed, Occurrence: o.id, Frames: o.from + o.written,
		Detail: fmt.Sprintf("decoding occurrence %d failed after %d frames: %v", o.id, o.from+o.written, err)}
	s.writing = nil
	s.closeOcc(o)
}

// writeConvertedChunk is writeChunk for a converted occurrence (B728 B11):
// its converter's next frames, packed in its transport's format and written.
// The converter stops at its window point (dsdconv.ErrWindow), its frames
// before the window in the sink: the occurrence holds, and the producer's
// next step decides its window (holdLocked). At its end, its window's frames
// in the sink, it is closed, unless a successor's first bits are in its
// window (cont), which a cut of that successor needs its converter's snapshot
// for: it is then let go of once that successor is heard (retireLocked), cut
// or the session ends. What it read under an earlier epoch, before it went
// back to its window point (windowCutLocked), is not written, and neither is
// the rest of a chunk of an occurrence cut meanwhile.
func (s *session) writeConvertedChunk(o *occ, samples []int32, packed []byte) bool {
	l := s.hold()
	defer l.release()
	l.lock()
	epoch := o.epoch
	l.unlock()
	n, err := o.conv.ReadFrames(s.ctx, samples)
	o.clipped.Store(o.conv.Clipped())
	if n > 0 {
		m, perr := format.Pack(packed, samples[:n], o.pcm.ValidBits, o.out.transport.Format)
		if perr != nil {
			s.failDecode(o, perr)
			return true
		}
		if !s.writeAll(o, packed[:m], epoch) {
			return false
		}
	}
	l.lock()
	if o.epoch != epoch || o.dropped || s.writing != o {
		l.unlock()
		return true
	}
	switch {
	case err == nil:
		l.unlock()
		return true
	case errors.Is(err, dsdconv.ErrWindow):
		o.holding, o.window = true, o.written
		l.unlock()
		s.b.converting("holding", o.id)
		return true
	case errors.Is(err, io.EOF):
		o.eof = true
		s.writing = nil
		if o.cont == nil {
			s.closeOcc(o)
		}
		l.unlock()
		s.saidClipping(o)
		wake(s.wakeR)
		s.b.converting("ended", o.id)
		return true
	case s.ctx.Err() != nil:
		l.unlock()
		return false
	}
	l.unlock()
	s.failDecode(o, err)
	return true
}

// writeAll hands b, o's packed frames read under its epoch, to o's sink. A
// cut that needs the producer ends the Write under way, and is served before
// the next one; a cut that drops o drops the rest of b with it, and so does
// o's return to its window point (a converted occurrence's, windowCutLocked),
// which moves its epoch. It reports false when the sink stops taking frames
// (an xrun, an end) or the session ends.
func (s *session) writeAll(o *occ, b []byte, epoch uint64) bool {
	fb := int64(format.PackedBytes(alsapcm.Channels, o.out.transport.Format))
	l := s.hold()
	defer l.release()
	for len(b) > 0 {
		l.lock()
		s.serveLocked()
		if o.dropped || s.writing != o || o.epoch != epoch {
			l.unlock()
			return true
		}
		if s.ctx.Err() != nil {
			l.unlock()
			return false
		}
		if s.releasedLocked() {
			// The session is released, by the engine's taking it before its
			// Release came or by that Release: the producer hands the sink
			// nothing more, at this write or any after it (B674 round 3).
			l.unlock()
			s.b.fenced("write")
			return false
		}
		if s.wctx == nil || s.wctx.Err() != nil {
			s.wctx, s.wcancel = context.WithCancel(s.ctx)
		}
		wctx, out := s.wctx, o.out
		s.inWrite = true
		l.unlock()
		n, err := out.sink.Write(wctx, b)
		l.lock()
		s.inWrite = false
		o.written += int64(n)
		out.submitted += int64(n)
		l.unlock()
		if n > 0 {
			s.signalFirst(nil)
			wake(s.wakeR)
		}
		b = b[int64(n)*fb:]
		switch {
		case err == nil:
		case alsapcm.ReasonOf(err) == alsapcm.ReasonCancelled && s.ctx.Err() == nil:
			// A cut asked the producer: served at the loop's top.
		default:
			s.signalFirst(err)
			wake(s.wakeR)
			return false
		}
	}
	return true
}

// transition moves the session to o's transport: the sink now playing
// drains, everything given to it played, and is closed; then a sink for o
// is opened, by the producer, or, while the session is paused, by the
// Resume, so a device another process took meanwhile refuses the resume.
// It reports false when the session cannot go on.
func (s *session) transition(o *occ) bool {
	l := s.hold()
	defer l.release()
	l.lock()
	old := s.cur
	l.unlock()
	if old != nil {
		if !old.drained {
			if err := s.drainOutput(old); err != nil {
				// The sink stopped (the reporter reads why from its state), or
				// the session ends.
				return false
			}
			l.lock()
			old.drained = true
			l.unlock()
			wake(s.wakeR)
			s.b.transportDrained()
		}
		s.closeOutput(old)
	}
	var out *output
	for out == nil {
		l.lock()
		s.serveLocked()
		if s.cur == old {
			s.cur = nil
		}
		// With no sink current, the reporter looks again: a pause that holds
		// no device reports its release only then (reportStep).
		pend := s.b.pendingFor(o)
		switch {
		case s.ctx.Err() != nil:
			l.unlock()
			return false
		case o.dropped:
			stale := s.handed
			s.between, s.handed = nil, nil
			l.unlock()
			wake(s.wakeR)
			if stale != nil {
				s.closeOutput(stale)
			}
			return true
		case s.handed != nil:
			out, s.handed, s.between = s.handed, nil, nil
			l.unlock()
		case s.paused:
			s.between = o
			l.unlock()
			wake(s.wakeR)
			if !s.waitWake(nil) {
				return false
			}
		case o.stale:
			// Being handed off again (B728 B7): the engine's replacement drops
			// it, and the producer's next step waits for that.
			l.unlock()
			return true
		case pend != nil:
			// A choice's write pending would play its rate otherwise (B728 B9):
			// no sink is opened for it until the write settles.
			l.unlock()
			s.b.choiceAwaited(o.id)
			select {
			case <-pend.done:
				s.b.choiceWoke(o.id)
			case <-s.ctx.Done():
				return false
			}
		case s.unjudgedLocked(o):
			// Admitted under a set a publish has replaced since, which that
			// publish's reconciliation has not judged yet (B728 B9 round 17): a
			// choice's write is pending only until it publishes, and no sink of
			// its own opens until the reconciliation has judged it, so one the
			// set refuses is handed off again, or its session's DSD revoked,
			// before any device opens for it. Its sink before it has drained, so
			// nothing plays out meanwhile; the reconciliation, which every
			// publish runs and which never waits on the producer, ends the wait
			// (judged), and so does the session's stop or release (its
			// context).
			judged := s.judged
			l.unlock()
			s.b.unjudged(o.id)
			if !s.waitWake(judged) {
				return false
			}
		default:
			// The claim, in the step that found no choice's write pending that
			// plays o's rate otherwise (B728 B9 round 2, point 1): a write that
			// classifies what it would interrupt while the sink opens counts o
			// as reaching it, and its adoption and activation wait for the write.
			s.between, o.claimed = nil, true
			l.unlock()
			var err error
			if out, err = s.openOutput(s.ctx, o); err != nil {
				l.lock()
				o.claimed = false
				l.unlock()
				return s.openFailed(o, err)
			}
		}
	}
	// The sink is already the session's (s.outs: the teardown closes it,
	// after every file), and it becomes the current one once it is in the
	// session's pause state. A pause that came while it opened found no
	// device to pause: the sink takes it before a frame is written to it, and
	// holds its device paused from then on, letting it go after the pause
	// release as any paused sink does. The command goes through call, never
	// under the session's lock: a sink whose writer the kernel holds answers
	// no command, and a Release must not wait on it. Until the sink is
	// current, a pause or a resume meanwhile is one with no device, which the
	// next turn follows. A choice's write pending that plays o's rate
	// otherwise is waited for before the sink is adopted, and one o was handed
	// off again for meanwhile leaves the sink closed unwritten (B728 B9 round
	// 2, point 1).
	l.lock()
	sinkPaused := false
	for {
		pend := s.b.pendingFor(o)
		switch {
		case s.ctx.Err() != nil:
			l.unlock()
			return false
		case o.dropped:
			l.unlock()
			s.closeOutput(out)
			return true
		case s.releasedLocked():
			// Released since the sink opened, by the producer or by a Resume
			// (handed): it is closed at once, never written to, and is never
			// the current one (B674 round 3).
			l.unlock()
			s.closeOutput(out)
			s.b.fenced("adopt")
			return false
		case o.stale:
			// Handed off again while its sink opened (a choice's write landed,
			// or a refresh changed the set): the sink is closed unwritten, and
			// the producer's next step waits for the replacement.
			o.claimed = false
			l.unlock()
			s.closeOutput(out)
			return true
		case pend != nil:
			l.unlock()
			s.b.choiceAwaited(o.id)
			select {
			case <-pend.done:
				s.b.choiceWoke(o.id)
			case <-s.ctx.Done():
				return false
			}
			l.lock()
			continue
		case s.paused == sinkPaused:
			s.currentLocked(o, out, sinkPaused)
			l.unlock()
			err := s.activateOutput(s.ctx, o, out)
			l.lock()
			o.claimed = false
			released, revoked := s.releasedLocked(), s.nativeRevoked
			taken := (o.dropped || o.stale) && !o.leading && (errors.Is(err, errReplanned) || errors.Is(err, alsapcm.ErrActivationFenced))
			l.unlock()
			// A failed activation is judged by its cause, in this order (B728 B9
			// round 4, point 1). A release, which the activation's fence answers
			// before the session's life has ended, ends the session, no fault,
			// beside a drop too (round 3). A revocation of the session's DSD
			// keeps its fault, so the session ends with it however the failure is
			// judged: Backend.Revoke records it while a sink is open, and a
			// revocation of o, claimed, is recorded in the reconciliation's own
			// step before o is marked stale (round 6); once Revoke has run, the
			// sink and decoder are left to the teardown. An occurrence dropped or
			// handed off again, or revoked before Revoke ran, whose activation
			// answered that drop's own fence, the backend's (errReplanned) or its
			// sink's (alsapcm.ErrActivationFenced: the drop, dropLocked, or the
			// reconciliation, reconcileDSD, revoked the sink, at its admission or in
			// its prefill), is taken back from the sink with none of its content
			// written, and the session goes on, or, revoked, stops at the next
			// step on the fault already recorded; the take-back's close waits for
			// the sink's end, and a release the sink cannot confirm is the
			// session's fault (closeOutput), so nothing follows a node whose
			// close failed (round 5). Any other failure, the device's refusal
			// beside a drop among them, is the session's fault.
			switch {
			case err == nil:
				return true
			case released:
				s.b.fenced("activate")
				return false
			case revoked:
				return s.openFailed(o, err)
			case taken:
				return s.unadopt(o, out)
			}
			return s.openFailed(o, err)
		}
		cmd := out.sink.Pause
		if sinkPaused {
			cmd = out.sink.Resume
		}
		l.unlock()
		err := s.call(s.ctx, cmd)
		l.lock()
		switch {
		case s.ctx.Err() != nil:
			l.unlock()
			return false
		case err != nil:
			// The sink took neither (it ended: the reporter reads why from its
			// state). It is current as it is.
			s.currentLocked(o, out, false)
			o.claimed = false
			l.unlock()
			return true
		}
		sinkPaused = !sinkPaused
	}
}

// unadopt takes back out, the sink just opened and made current for o,
// once o's activation found it handed off again while a choice's write it
// waited for settled, or dropped (its replacement, an edit's, a cut: B728 B9
// round 3, point 1), errReplanned, or the sink's activation answered its own
// fence for such a drop or hand-off, which revoked it at its admission or in
// its prefill (alsapcm.ErrActivationFenced, round 4): none of o's content
// reached the sink, so it is closed, nothing is current, and the producer's
// next step waits for o's replacement or goes on to what follows. A dropped
// o's decoder and content are closed here: the drop left them to the
// producer, o being the occurrence it writes (dropLocked), and nothing else
// closes them until the session's end. A session paused meanwhile is paused
// holding no device, its release reported as such a pause's is.
func (s *session) unadopt(o *occ, out *output) bool {
	s.mu.Lock()
	if s.cur == out {
		s.cur, s.currentNative = nil, false
	}
	if s.writing == o {
		s.writing = nil
		if o.dropped {
			s.closeOcc(o)
		}
	}
	o.out, o.claimed = nil, false
	if s.paused {
		_ = s.pausedLocked(true)
	}
	s.mu.Unlock()
	s.closeOutput(out)
	wake(s.wakeP)
	wake(s.wakeR)
	return true
}

// currentLocked makes out, the sink just opened for o, the current one,
// and o the occurrence the producer writes; paused says the sink holds the
// session's pause. s.mu is held.
func (s *session) currentLocked(o *occ, out *output, paused bool) {
	s.cur = out
	s.currentNative = out.native != nil
	o.out, o.start, o.transport = out, 0, out.transport
	s.writing = o
	if paused {
		s.pausedLocked(false)
	}
}

// openFailed settles a transport's sink that did not open: a DAC that takes
// none of its packings refuses o before any of it played, and the session
// waits for what follows; any other refusal (busy, the check after open, the
// unit gone) ends the session with a fault, after content played.
func (s *session) openFailed(o *occ, err error) bool {
	if s.ctx.Err() != nil || errors.Is(err, errReleased) {
		// The session ends, released before or at the open (B674 round 3):
		// no fault.
		return false
	}
	var te *transportError
	s.mu.Lock()
	ok := errors.As(err, &te)
	if ok {
		if !o.dropped {
			o.refused = &engine.Unplayable{Reason: ReasonTransport, Detail: te.Error()}
			o.dropped = true
			s.closeOcc(o)
		}
	} else if s.fault == nil {
		s.fault = &engine.Fault{Reason: faultReason(err), Detail: "the device could not be opened for the next transport: " + err.Error()}
	}
	s.mu.Unlock()
	wake(s.wakeR)
	return ok
}

// finish plays out the last content: the sink drains, and the session is
// finished, everything handed off played, or faults with the decode failure
// it played out to.
func (s *session) finish() {
	s.mu.Lock()
	out := s.cur
	if out != nil && (out.drained || out.closed) {
		out = nil
	}
	s.mu.Unlock()
	if out != nil {
		if err := s.drainOutput(out); err != nil {
			return
		}
		s.mu.Lock()
		out.drained = true
		s.mu.Unlock()
		s.closeOutput(out)
	}
	s.mu.Lock()
	if s.decodeErr != nil {
		s.fault = s.decodeErr
	} else {
		s.drained = true
	}
	s.mu.Unlock()
	wake(s.wakeR)
}
