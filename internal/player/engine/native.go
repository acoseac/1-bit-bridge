package engine

import (
	"context"
	"errors"
	"slices"
	"time"
)

// NativeTuple identifies a native carrier without requiring ALSA in the engine.
type NativeTuple struct {
	Format  string
	BitRate int
}

// TransitionEdge is an admitted transport boundary, supplied with the plan.
type TransitionEdge struct{ From, To string }

// NativePlan is one admission result. The engine stores it without selection.
// PolicyID and all policy values survive transitions and released pauses.
type NativePlan struct {
	Tuple         NativeTuple
	PolicyID      string
	Pause         string
	Tail, Prefill time.Duration
	Edges         []TransitionEdge
}

// NativePlanEvents supplies the immutable plan before an occurrence can start.
type NativePlanEvents interface {
	NativePlan(SessionID, OccurrenceID, NativePlan)
}

// ReleaseMode is the disposition of the accepted outgoing prefix.
type ReleaseMode uint8

const (
	// ReleaseImmediate cancels music and any outgoing tail.
	ReleaseImmediate ReleaseMode = iota
	// ReleaseTrackChange plays the accepted prefix and admitted outgoing tail.
	ReleaseTrackChange
)

// ReleaseCause names the intent that selected a release.
type ReleaseCause string

const (
	// ReleaseNavigation changes the selected track.
	ReleaseNavigation ReleaseCause = "navigation"
	// ReleaseStop stops without successor playback.
	ReleaseStop ReleaseCause = "stop"
	// ReleaseFault terminates a faulted session.
	ReleaseFault ReleaseCause = "fault"
)

// ReleaseToken names a retiring session and its authoritative cutoff, in logical
// bits per channel. Cutoff is published only after the in-flight write returns.
type ReleaseToken struct {
	Session    SessionID
	Generation uint64
	Cutoff     int64
}

// ReleaseRequest establishes a release; the backend supplies its final cutoff.
type ReleaseRequest struct {
	Token ReleaseToken
	Mode  ReleaseMode
	Cause ReleaseCause
}

// NativeLifecycle extends the unchanged PCM backend calls with typed native
// release and an asynchronous, nonblocking escalation fence.
type NativeLifecycle interface {
	ReleaseNative(context.Context, ReleaseRequest) error
	EscalateNative(ReleaseRequest)
}

// RevokeInput permanently revokes a session's native activation permission.
type RevokeInput struct {
	Session SessionID
	Reason  string
}

// NativePrefix bounds one occurrence in its output's logical-bit coordinates.
// End is min(occurrence end, that output's hardware-accepted cutoff).
type NativePrefix struct {
	Occurrence OccurrenceID
	Start, End int64
}

// NativeEvent is measured outgoing accounting; it never carries queue intent.
type NativeEvent struct {
	Prefixes   []NativePrefix
	Kind       string
	Occurrence OccurrenceID
	Details    Details
	Position   Position
	Fault      Fault
}

// NativeReleaseEvents receives ordered accounting followed by the ordinary
// release acknowledgement. The first event is the authoritative cutoff.
type NativeReleaseEvents interface {
	Retiring(ReleaseToken, NativeEvent)
}

// ErrReleaseUnconfirmed is terminal failure to establish successful release.
// A subsequent successful cleanup acknowledgement never restores autoplay.
var ErrReleaseUnconfirmed = errors.New("player: native release unconfirmed")

type retiringLedger struct {
	token    ReleaseToken
	cutoff   bool
	chain    []*occ
	prefixes map[OccurrenceID]NativePrefix
	faulted  bool
}
type evNativePlan struct {
	s SessionID
	o OccurrenceID
	p NativePlan
}
type evRetiring struct {
	token ReleaseToken
	event NativeEvent
}

func (s *session) native() bool {
	if s.hasCurrent {
		return s.currentNative
	}
	if len(s.chain) == 0 {
		return false
	}
	o := s.chain[0]
	return o.plan != nil || (o.details != nil && o.details.Bits == 1 && o.details.Codec == "DSD")
}
func (e *Engine) onNativePlan(m evNativePlan) {
	s := e.current(m.s)
	if s == nil {
		return
	}
	i := s.index(m.o)
	if i < 0 || s.chain[i].plan != nil {
		return
	}
	p := m.p
	p.Edges = slices.Clone(p.Edges)
	s.chain[i].plan = &p
}
func (e *Engine) onRetiring(t ReleaseToken, n NativeEvent) {
	s := e.o.sessions[t.Session]
	if s == nil || s.retiring == nil || s.phase == sessAcked {
		return
	}
	r := s.retiring
	if r.token.Generation != t.Generation {
		return
	}
	if n.Kind == "cutoff" {
		if !r.cutoff && t.Cutoff >= 0 {
			r.token = t
			r.prefixes = make(map[OccurrenceID]NativePrefix)
			for _, p := range n.Prefixes {
				if p.Start >= 0 && p.End > p.Start {
					r.prefixes[p.Occurrence] = p
				}
			}
			r.cutoff = true
		}
		return
	}
	if !r.cutoff || t != r.token {
		return
	}
	e.retireEvent(s, n)
}
func (e *Engine) retireEvent(s *session, n NativeEvent) {
	r := s.retiring
	if r == nil {
		return
	}
	var oc *occ
	oid := n.Occurrence
	if n.Kind == "position" {
		oid = n.Position.Occurrence
	}
	for _, c := range r.chain {
		if c.id == oid {
			oc = c
			break
		}
	}
	bound := int64(0)
	if r.cutoff {
		p, accepted := r.prefixes[oid]
		if !accepted {
			oc = nil
		} else {
			bound = p.End - p.Start
		}
	}
	switch n.Kind {
	case "details":
		if oc != nil {
			d := n.Details
			oc.details = &d
		}
	case "audible":
		if oc != nil {
			e.hear(oc)
			e.o.pos = Position{Occurrence: oc.id}
			e.touched()
		}
	case "position":
		if oc != nil && e.o.audible.Occurrence == oid && n.Position.Frames >= e.o.pos.Frames && (!r.cutoff || n.Position.Frames <= bound) {
			e.o.pos = n.Position
			e.touched()
		}
	case "fault":
		e.recordRetiringFault(s, n.Fault)
	}
}
func (e *Engine) recordRetiringFault(s *session, f Fault) {
	if s.retiring.faulted {
		return
	}
	s.retiring.faulted = true
	if f.At.IsZero() {
		f.At = e.clk.Now()
	}
	for _, o := range s.retiring.chain {
		if o.id == f.Occurrence {
			f.Entry = o.entry
		}
	}
	e.o.faults = append(slices.Clone(e.o.faults), f)
	if len(e.o.faults) > MaxFaults {
		e.o.faults = e.o.faults[len(e.o.faults)-MaxFaults:]
	}
	e.playFailed(refuse("play", ReasonFault, string(f.Reason)))
	e.setIntent(IntentStop, string(f.Reason))
	e.touched()
}
func (e *Engine) ordinaryRetiring(sid SessionID, n NativeEvent) bool {
	s := e.o.sessions[sid]
	if s == nil || s.retiring == nil {
		return false
	}
	// Ordinary events already in the mailbox cross into the ledger before its
	// authoritative cutoff. Afterwards only matching token events are accepted.
	if !s.retiring.cutoff {
		e.retireEvent(s, n)
	}
	return true
}
func (e *Engine) escalateRetiring() {
	b, ok := e.be.(NativeLifecycle)
	if !ok {
		return
	}
	for _, s := range e.o.sessions {
		if s.retiring != nil && s.phase != sessAcked {
			req := ReleaseRequest{Token: s.retiring.token, Mode: ReleaseImmediate, Cause: ReleaseStop}
			e.spawn(func() any { b.EscalateNative(req); return nil })
		}
	}
}

// CurrentOccurrenceEvents identifies the occurrence holding the transport before
// activation, even while the DAC has not made it audible yet.
type CurrentOccurrenceEvents interface {
	CurrentOccurrence(SessionID, OccurrenceID, *NativePlan)
}
type evCurrentOccurrence struct {
	s SessionID
	o OccurrenceID
	p *NativePlan
}

func (v *events) CurrentOccurrence(s SessionID, o OccurrenceID, p *NativePlan) {
	var cp *NativePlan
	if p != nil {
		x := *p
		x.Edges = slices.Clone(p.Edges)
		cp = &x
	}
	v.e.post(evCurrentOccurrence{s, o, cp})
}

// A transport installed during pause owns a fresh effective release deadline.
// Invalidating the sequence also fences a previous timer already in the mailbox.
func (e *Engine) armPauseDeadline(s *session) {
	s.stopPause()
	s.pauseSeq++
	seq := s.pauseSeq
	s.pauseTimer = e.clk.AfterFunc(e.effectivePauseRelease(s)+ReleaseTimeout, func() { e.post(msgPauseOverdue{s: s, seq: seq}) })
}
