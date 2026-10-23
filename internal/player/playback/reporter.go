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
	s.mu.Lock()
	if s.releasing || len(s.chain) == 0 {
		s.mu.Unlock()
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
		s.faultSent = true
		f := *s.fault
		switch {
		case f.Occurrence != 0:
		case heard != nil:
			f.Occurrence, f.Frames = heard.id, s.posFrames
		default:
			f.Occurrence = s.chain[0].id
		}
		evs = append(evs, func() { s.ev.Fault(id, f) })
	}
	// A pause's release of the device: the sink's own, or, holding no device,
	// the pause release passed.
	if s.paused && !s.relSent && !s.faultSent {
		c := s.cur
		switch {
		case c != nil && !c.closed && !c.drained && posOf(c).State == alsapcm.SinkReleased:
			s.relSent = true
			n := Note{Session: id, Kind: NoteReleased, Params: c.sink.Params(), Transport: c.transport, Unit: c.unitLabel()}
			evs = append(evs, func() {
				s.b.notify(n)
				s.ev.DeviceReleased(id)
			})
		case s.relFired && (c == nil || c.closed || c.drained):
			s.relSent = true
			evs = append(evs, func() { s.ev.DeviceReleased(id) })
		}
	}
	done := s.faultSent
	if s.drained && !s.finSent && s.fault == nil {
		s.finSent, done = true, true
		evs = append(evs, func() { s.ev.Finished(id) })
	}
	// What this step reported of the occurrences before the one heard was
	// the last of them.
	s.retireLocked(heard)
	s.mu.Unlock()
	for _, e := range evs {
		e()
	}
	return done
}
