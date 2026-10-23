package playback

import (
	"io"
	"log/slog"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// panickingFault counts delivery attempts, including ones that never
// return. The embedded interface is unused: these steps send only Fault.
type panickingFault struct {
	engine.Events
	attempts int
}

func (e *panickingFault) Fault(engine.SessionID, engine.Fault) {
	e.attempts++
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
			ev := &panickingFault{}
			s.ev = ev
			s.chain = []*occ{{id: 1}}
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
