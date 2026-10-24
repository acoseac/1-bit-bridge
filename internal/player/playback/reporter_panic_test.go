package playback

import (
	"io"
	"log/slog"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// panickingFault counts delivery attempts, including ones that never
// return. Prepared observes the step before the queued fault is attempted.
type panickingFault struct {
	engine.Events
	attempts int
	prepared func()
	fault    func()
}

func (e *panickingFault) Prepared(engine.SessionID, engine.OccurrenceID, engine.Join) {
	e.prepared()
}

func (e *panickingFault) Fault(engine.SessionID, engine.Fault) {
	e.attempts++
	if e.fault != nil {
		e.fault()
	}
	panic("fault callback")
}

// TestReporterNeverRetriesAPanickingFaultDelivery runs the reporter step
// and its recovery synchronously: no Release can race recovery and hide a
// repeated attempt. It also exercises recovery's guarded Fault callback.
func TestReporterNeverRetriesAPanickingFaultDelivery(t *testing.T) {
	for _, where := range []string{"step", "recovery"} {
		t.Run(where, func(t *testing.T) {
			b := &Backend{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			s := newSession(b, 1)
			defer s.cancel()
			ev := &panickingFault{
				prepared: func() {
					if s.faultSent {
						t.Error("queued fault counted as attempted before Prepared")
					}
				},
				fault: func() {
					if !s.faultSent {
						t.Error("Fault callback entered without recording its attempt")
					}
				},
			}
			s.ev = ev
			ready := make(chan struct{})
			close(ready)
			s.chain = []*occ{{id: 1}, {id: 2, done: ready}}
			func() {
				defer s.recoverReporter()
				if where == "recovery" {
					panic("reporter")
				}
				s.fault = &engine.Fault{Reason: engine.ReasonXrun}
				s.reportStep()
			}()
			if ev.attempts != 1 {
				t.Errorf("Fault delivery attempted %d times; want once", ev.attempts)
			}
			if !s.faultSent {
				t.Error("attempted delivery was not recorded")
			}
		})
	}
}

// releasingFault begins release from the event ahead of a step's fault.
// It runs on the reporter's goroutine, without timing a racing Stop.
type releasingFault struct {
	engine.Events
	s   *session
	got []engine.Fault
}

func (e *releasingFault) Prepared(engine.SessionID, engine.OccurrenceID, engine.Join) {
	e.s.mu.Lock()
	e.s.releasing = true
	e.s.mu.Unlock()
}

func (e *releasingFault) Fault(_ engine.SessionID, f engine.Fault) {
	e.got = append(e.got, f)
}

// TestReporterDeliversAQueuedFaultAfterReleaseBegins pins ordinary delivery:
// release begins after the fault was queued, and no callback panics.
func TestReporterDeliversAQueuedFaultAfterReleaseBegins(t *testing.T) {
	b := &Backend{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := newSession(b, 1)
	defer s.cancel()
	ev := &releasingFault{s: s}
	s.ev = ev
	ready := make(chan struct{})
	close(ready)
	s.chain = []*occ{{id: 1}, {id: 2, done: ready}}
	s.fault = &engine.Fault{Reason: engine.ReasonXrun, Detail: "device underrun"}
	if !s.reportStep() {
		t.Error("fault did not end the step")
	}
	// Another step during release cannot duplicate the delivery.
	s.reportStep()
	if len(ev.got) != 1 {
		t.Fatalf("queued fault delivered %d times; want once", len(ev.got))
	}
	if f := ev.got[0]; f.Reason != engine.ReasonXrun || f.Detail != "device underrun" || f.Occurrence != 1 {
		t.Errorf("delivered fault %+v; want the queued xrun on occurrence 1", f)
	}
}
