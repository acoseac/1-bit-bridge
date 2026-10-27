package playback

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
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

type step struct {
	kind stepKind
	o    *occ
	wait <-chan struct{}
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
		switch st.kind {
		case stepWrite:
			if !s.writeChunk(st.o, samples, packed) {
				return
			}
		case stepWait:
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
	if s.ctx.Err() != nil || s.fault != nil {
		return step{kind: stepStop}
	}
	if w := s.writing; w != nil {
		if !w.dropped {
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
	if c := s.cur; c != nil && !c.drained && !c.closed && c.transport == o.transport && (o.plan == nil || c.policyID == o.plan.PolicyID) {
		// Gapless: its frames follow the last ones in the same sink.
		o.out, o.start = c, c.submitted
		s.writing = o
		return step{kind: stepWrite, o: o}
	}
	return step{kind: stepTransition, o: o}
}

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

// serveLocked makes the cuts asked of the producer, which is in no Write.
func (s *session) serveLocked() {
	for len(s.reqs) > 0 {
		r := s.reqs[0]
		s.reqs[0] = nil
		s.reqs = s.reqs[1:]
		ok, err := s.sinkCutLocked(r.o)
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
		if !s.writeAll(o, packed[:n]) {
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
	s.decodeErr = &engine.Fault{Reason: engine.ReasonDecodeFailed, Occurrence: o.id, Frames: o.written,
		Detail: fmt.Sprintf("decoding occurrence %d failed after %d frames: %v", o.id, o.written, err)}
	s.writing = nil
	s.closeOcc(o)
}

// writeAll hands b, o's packed frames, to o's sink. A cut that needs the
// producer ends the Write under way, and is served before the next one; a
// cut that drops o drops the rest of b with it. It reports false when the
// sink stops taking frames (an xrun, an end) or the session ends.
func (s *session) writeAll(o *occ, b []byte) bool {
	fb := int64(format.PackedBytes(alsapcm.Channels, o.out.transport.Format))
	l := s.hold()
	defer l.release()
	for len(b) > 0 {
		l.lock()
		s.serveLocked()
		if o.dropped || s.writing != o {
			l.unlock()
			return true
		}
		if s.ctx.Err() != nil {
			l.unlock()
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
		default:
			s.between = nil
			l.unlock()
			var err error
			if out, err = s.openOutput(s.ctx, o); err != nil {
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
	// next turn follows.
	l.lock()
	sinkPaused := false
	for {
		switch {
		case s.ctx.Err() != nil:
			l.unlock()
			return false
		case o.dropped:
			l.unlock()
			s.closeOutput(out)
			return true
		case s.paused == sinkPaused:
			s.currentLocked(o, out, sinkPaused)
			l.unlock()
			if err := s.activateOutput(s.ctx, o, out); err != nil {
				return s.openFailed(o, err)
			}
			return true
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
			l.unlock()
			return true
		}
		sinkPaused = !sinkPaused
	}
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
	if s.ctx.Err() != nil {
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
