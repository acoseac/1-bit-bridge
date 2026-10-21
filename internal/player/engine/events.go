package engine

import "slices"

// events is the Events every session is started with: each report becomes
// a message to the owner, which applies it only while its session is
// current.
type events struct{ e *Engine }

func (v *events) Prepared(s SessionID, o OccurrenceID, j Join) {
	v.e.post(evPrepared{s: s, o: o, j: Join{Gapless: j.Gapless, Differs: slices.Clone(j.Differs)}})
}

func (v *events) Refused(s SessionID, o OccurrenceID, why Unplayable) {
	v.e.post(evRefused{s: s, o: o, u: why})
}

func (v *events) Submitted(s SessionID, o OccurrenceID) { v.e.post(evSubmitted{s: s, o: o}) }
func (v *events) Audible(s SessionID, o OccurrenceID)   { v.e.post(evAudible{s: s, o: o}) }
func (v *events) Position(s SessionID, p Position)      { v.e.post(evPosition{s: s, p: p}) }
func (v *events) Fault(s SessionID, f Fault)            { v.e.post(evFault{s: s, f: f}) }
func (v *events) Finished(s SessionID)                  { v.e.post(evFinished{s: s}) }
func (v *events) DeviceReleased(s SessionID)            { v.e.post(evDeviceReleased{s: s}) }
func (v *events) Released(s SessionID)                  { v.e.post(evReleased{s: s}) }

// The messages the owner handles besides commands: backend events, call
// results, timers, the reservation writes and the writer's reports.
type (
	evPrepared struct {
		s SessionID
		o OccurrenceID
		j Join
	}
	evRefused struct {
		s SessionID
		o OccurrenceID
		u Unplayable
	}
	evSubmitted struct {
		s SessionID
		o OccurrenceID
	}
	evAudible struct {
		s SessionID
		o OccurrenceID
	}
	evPosition struct {
		s SessionID
		p Position
	}
	evFault struct {
		s SessionID
		f Fault
	}
	evFinished       struct{ s SessionID }
	evDeviceReleased struct{ s SessionID }
	evReleased       struct{ s SessionID }
	msgCallDone      struct {
		c *call
		r callResult
	}
	msgCallExpired     struct{ c *call }
	msgReleaseDeadline struct{ s *session }
	msgPauseOverdue    struct {
		s   *session
		seq uint64
	}
	msgMaintExpiry struct{ seq uint64 }
	msgReserveDone struct{ err error }
	msgPersisted   struct {
		written  uint64
		err      error
		failures int
	}
	msgWriterExited struct{}
	msgGraceOver    struct{}
)

// handle applies one message.
func (e *Engine) handle(m any) {
	o := &e.o
	var what string
	switch m := m.(type) {
	case evPrepared:
		what = "prepared"
		if s := e.current(m.s); s != nil {
			if i := s.index(m.o); i >= 0 && s.chain[i].stage < stagePrepared {
				s.chain[i].stage = stagePrepared
			}
			j := m.j
			o.join = &j
			e.touched()
		}
	case evRefused:
		what = "refused"
		e.onRefused(m.s, m.o, m.u)
	case evSubmitted:
		what = "submitted"
		if s := e.current(m.s); s != nil {
			if i := s.index(m.o); i >= 0 && s.chain[i].stage < stageSubmitted {
				s.chain[i].stage = stageSubmitted
			}
			e.reconcile(s)
		}
	case evAudible:
		what = "audible"
		e.onAudible(m.s, m.o)
	case evPosition:
		what = "position"
		if s := e.current(m.s); s != nil && o.audible.Occurrence != 0 && m.p.Occurrence == o.audible.Occurrence {
			o.pos = m.p
			e.touched()
		}
	case evFault:
		what = "fault"
		e.onFault(m.s, m.f)
	case evFinished:
		what = "finished"
		e.onFinished(m.s)
	case evDeviceReleased:
		what = "device released"
		e.onDeviceReleased(m.s)
	case evReleased:
		what = "released"
		if s := o.sessions[m.s]; s != nil {
			e.acked(s)
		}
	case msgCallDone:
		what = "call " + m.c.kind.String()
		e.callDone(m.c, m.r)
	case msgCallExpired:
		what = "call " + m.c.kind.String() + " expired"
		e.callExpired(m.c)
	case msgReleaseDeadline:
		what = "release deadline"
		e.releaseDeadline(m.s)
	case msgPauseOverdue:
		what = "pause overdue"
		e.pauseOverdue(m.s, m.seq)
	case msgMaintExpiry:
		what = "maintenance expiry"
		e.maintenanceExpiry(m.seq)
	case msgReserveDone:
		what = "reservation"
		e.reserveDone(m.err)
	case msgPersisted:
		what = "persisted"
		o.persist.Written = max(o.persist.Written, m.written)
		o.persist.Failures = m.failures
		o.persist.Err = ""
		if m.err != nil {
			o.persist.Err = m.err.Error()
		}
		e.touched()
	case msgWriterExited:
		what = "writer exited"
		o.writerDone = true
	case msgGraceOver:
		what = "grace"
		o.graceOver = true
	}
	if e.sm.handled != nil {
		o.notes = append(o.notes, note{what: what})
	}
}
