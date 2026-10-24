package playback

import (
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// report is the session's reporter, the one goroutine that sends its
// events, so they reach the engine in order. It wakes on its tick, on a
// change of the sink's state and when another goroutine has news, and each
// time sends what has become true: each successor's Prepared or Refused,
// its Submitted once its first frames are in the sink, each boundary the
// played content crossed (Audible), the position of what is heard, a fault,
// a pause's release of the device, and Finished. It ends after a fault or
// Finished, and at the session's release.
func (s *session) report() {
	ticks, stop := s.b.cfg.Tick()
	defer stop()
	for {
		// The channel is taken before the state is read, so a change after
		// the read wakes the next wait.
		var changed <-chan struct{}
		s.mu.Lock()
		if s.cur != nil {
			changed = s.cur.sink.Changed()
		}
		s.mu.Unlock()
		if s.reportStep() {
			return
		}
		select {
		case <-ticks:
		case <-changed:
		case <-s.wakeR:
		case <-s.ctx.Done():
			return
		}
	}
}

// reportStep sends what has become true since the last step, and reports
// whether the session has nothing more to report.
//
// A boundary is the played content crossing an occurrence's first frame:
// the sink's Position.Played, the content frames the kernel establishes the
// DAC played, never silence, past the frame the occurrence started at in
// that sink. Played crossing several in one measurement reports each, in
// order; the decoder's end of an occurrence reports nothing.
func (s *session) reportStep() bool {
	var evs []func()
	l := s.hold()
	defer l.release()
	l.lock()
	if s.releasing || len(s.chain) == 0 {
		l.unlock()
		return true
	}
	id := s.id
	// The successors' stages. The first occurrence's Prepared and Submitted
	// are its Start's answer.
	for _, o := range s.chain[1:] {
		if !o.opened() {
			continue
		}
		oid := o.id
		if o.refused != nil {
			if !o.repRefused {
				o.repRefused = true
				u := *o.refused
				evs = append(evs, func() { s.ev.Refused(id, oid, u) })
			}
			continue
		}
		if o.dropped {
			continue
		}
		if !o.repPrepared {
			o.repPrepared = true
			j := o.join
			evs = append(evs, func() { s.ev.Prepared(id, oid, j) })
		}
		if o.written > 0 && !o.repSubmitted {
			o.repSubmitted = true
			evs = append(evs, func() { s.ev.Submitted(id, oid) })
		}
	}
	positions := map[*output]alsapcm.Position{}
	posOf := func(out *output) alsapcm.Position {
		p, ok := positions[out]
		if !ok {
			p = out.sink.Position()
			positions[out] = p
		}
		return p
	}
	// The boundaries crossed, in play order.
	var heard *occ
	for _, o := range s.chain {
		if o.dropped || o.out == nil {
			continue
		}
		if o.crossed {
			heard = o
			continue
		}
		if o.written == 0 || posOf(o.out).Played <= o.start {
			break
		}
		o.crossed, heard = true, o
		oid := o.id
		evs = append(evs, func() { s.ev.Audible(id, oid) })
	}
	// What is heard: its content played, at most what it gave the sink.
	if heard != nil {
		p := posOf(heard.out)
		frames := min(max(p.Played-heard.start, 0), heard.written)
		if heard.id != s.posOcc || frames != s.posFrames {
			s.posOcc, s.posFrames = heard.id, frames
			pos := engine.Position{Occurrence: heard.id, Frames: frames, RateHz: heard.out.transport.RateHz, Measured: p.Measured}
			evs = append(evs, func() { s.ev.Position(id, pos) })
		}
	}
	// A sink that stopped: an xrun, or an end that is not the release's.
	if c := s.cur; s.fault == nil && c != nil && !c.closed {
		p := posOf(c)
		switch p.State {
		case alsapcm.SinkXrun:
			s.fault = &engine.Fault{Reason: engine.ReasonXrun, Detail: errorText(p.Err)}
		case alsapcm.SinkEnded:
			if alsapcm.ReasonOf(p.Err) != alsapcm.ReasonCancelled {
				s.fault = &engine.Fault{Reason: faultReason(p.Err), Detail: errorText(p.Err)}
			}
		}
	}
	if s.fault != nil && !s.faultSent {
		f := *s.fault
		switch {
		case f.Occurrence != 0:
		case heard != nil:
			f.Occurrence, f.Frames = heard.id, s.posFrames
		default:
			f.Occurrence = s.chain[0].id
		}
		evs = append(evs, func() {
			// Queuing is not delivery: an earlier callback may panic. Mark
			// the attempt here so recovery sends an unreached fault once,
			// but never retries a Fault callback that itself panicked.
			s.mu.Lock()
			s.faultSent = true
			s.mu.Unlock()
			s.ev.Fault(id, f)
		})
	}
	// A pause's release of the device: the sink's own, or, holding no device,
	// the pause release passed, once no sink is current. A sink that played
	// out its last frames and is still current is the producer's, on its way
	// to the next transport (transition) and yet to record the occurrence it
	// opens a sink for, so a resume told of the release then would find none
	// to open; at the end of the session one is followed by Finished.
	if s.paused && !s.relSent && s.fault == nil {
		c := s.cur
		switch {
		case c != nil && !c.closed && !c.drained && posOf(c).State == alsapcm.SinkReleased:
			s.relSent = true
			n := Note{Session: id, Kind: NoteReleased, Params: c.sink.Params(), Transport: c.transport, Unit: c.unitLabel()}
			evs = append(evs, func() {
				s.b.notify(n)
				s.ev.DeviceReleased(id)
			})
		case s.relFired && c == nil:
			s.relSent = true
			evs = append(evs, func() { s.ev.DeviceReleased(id) })
		}
	}
	done := s.fault != nil
	if s.drained && !s.finSent && s.fault == nil {
		s.finSent, done = true, true
		evs = append(evs, func() { s.ev.Finished(id) })
	}
	// What this step reported of the occurrences before the one heard was
	// the last of them.
	s.retireLocked(heard)
	l.unlock()
	for _, e := range evs {
		e()
	}
	return done
}

// recoverReporter is deferred last by the reporter: a panic on it is
// recovered, logged once, and is the session's fault, which the reporter,
// the one goroutine that sends the session's events, sends itself, since it
// sends nothing after, unless a fault was sent already or the session is
// being released. The fault names the occurrence last heard, with its
// frames, or else the first.
func (s *session) recoverReporter() {
	r := recover()
	if r == nil {
		return
	}
	const where = "the reporter"
	s.b.logPanic(where, r)
	s.faulted(where)
	s.mu.Lock()
	send := !s.faultSent && !s.releasing
	var f engine.Fault
	if send {
		s.faultSent = true
		f = *s.fault
		if f.Occurrence == 0 {
			f.Occurrence, f.Frames = s.posOcc, s.posFrames
			if f.Occurrence == 0 && len(s.chain) > 0 {
				f.Occurrence = s.chain[0].id
			}
		}
	}
	ev, id := s.ev, s.id
	s.mu.Unlock()
	if !send {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.b.logPanic("the reporter's report of its fault", r)
		}
	}()
	ev.Fault(id, f)
}
