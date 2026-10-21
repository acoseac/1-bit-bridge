package engine

import (
	"context"
	"slices"
	"testing"
)

// TestACommandCancelledBeforeAdmissionDoesNotRun pins admission: a context
// that has ended before the owner takes the command, or ends as it takes
// it, leaves the queue as it was.
func TestACommandCancelledBeforeAdmissionDoesNotRun(t *testing.T) {
	h := newHarness(t)
	cancelAt := make(chan context.CancelFunc, 1)
	h.tweak = func(c *Config) {
		c.seams.admitted = func(cmd *command) {
			select {
			case cancel := <-cancelAt:
				cancel()
			default:
			}
		}
	}
	h.boot()
	ctx, cancel := context.WithCancel(bg)
	cancel()
	_, err := h.eng.InsertAfter(ctx, 0, ref("a"), nil)
	h.refused("an insert with an ended context", err, ReasonCancelled)

	ctx, cancel = context.WithCancel(bg)
	cancelAt <- cancel
	_, err = h.eng.InsertAfter(ctx, 0, ref("b"), nil)
	h.refused("an insert whose context ended as the owner took it", err, ReasonCancelled)
	if sn := h.snap(); sn.Len() != 0 {
		t.Fatalf("a command that was not admitted ran: %v", sn.IDs())
	}
}

// TestAfterRunReturnsEveryCommandIsClosed pins the closed engine: with a
// context that never ends, every command answers ReasonClosed at once.
func TestAfterRunReturnsEveryCommandIsClosed(t *testing.T) {
	h := startHarness(t)
	h.insert("a")
	h.shutdown()
	for name, err := range map[string]error{
		"play":   h.eng.Play(context.Background()),
		"stop":   h.eng.Stop(context.Background()),
		"output": h.eng.ReportOutput(context.Background(), OutputReport{Generation: 9, Available: true}),
	} {
		h.refused(name+" after Run", err, ReasonClosed)
	}
	_, err := h.eng.InsertAfter(context.Background(), 0, ref("b"), nil)
	h.refused("insert after Run", err, ReasonClosed)
	_, err = h.eng.EnterMaintenance(context.Background(), h.snap().Generation(), "after")
	h.refused("enter after Run", err, ReasonClosed)
	if !h.snap().Closed() {
		t.Fatal("the last snapshot is not closed")
	}
	if got := h.snap().IDs(); len(got) != 1 {
		t.Fatalf("reads after Run: %v", got)
	}
}

// TestShutdownJoinsWithinItsBound pins Run's shutdown mid-command with a
// backend that never answers: a Stop waiting on a release that never comes
// when Run is cancelled, every mutation refused from then on, Run back
// within ReleaseTimeout and shutdownGrace of the clock, the call that never
// answered abandoned and logged, and every other goroutine joined.
func TestShutdownJoinsWithinItsBound(t *testing.T) {
	h := startHarness(t)
	_, s := h.playing(0, "a", "b")
	h.be.script(func() {
		h.be.onRelease = func(ctx context.Context, fs *fakeSession) error {
			<-h.be.cleanup
			return nil
		}
	})
	stop := async(func() error { return h.eng.Stop(bg) })
	h.be.waitFor("the release", func() bool { return s.releaseAsked })
	start := h.clk.Now()
	h.cancel()
	h.waitSnap("closed", func(sn *Snapshot) bool { return sn.Closed() })
	_, err := h.eng.InsertAfter(bg, 0, ref("x"), nil)
	h.refused("an insert during shutdown", err, ReasonClosed)
	h.drive(h.ran)
	h.down = true
	if took := h.clk.Now().Sub(start); took > ReleaseTimeout+shutdownGrace {
		t.Fatalf("Run took %v of the clock to return, more than %v", took, ReleaseTimeout+shutdownGrace)
	}
	_ = h.result("the stop", stop)
	if n := h.eng.abandoned.Load(); n != 1 {
		t.Fatalf("%d calls abandoned, want the release that never answered", n)
	}
	if n := h.eng.live.Load(); n != 1 {
		t.Fatalf("%d goroutines still running after Run, want only the abandoned call", n)
	}
	if h.logs.count("abandoned") != 1 {
		t.Fatalf("the abandoned call was not logged once: %v", h.logs.all())
	}
	h.be.close()
	h.joined(h.eng)
}

// TestSnapshotsAreImmutable pins the copies: changing what an accessor
// returns changes nothing, and a snapshot taken before a change still shows
// what it showed.
func TestSnapshotsAreImmutable(t *testing.T) {
	h := startHarness(t)
	ids, s := h.playing(0, "a", "b")
	s.fault(Fault{Reason: ReasonXrun, Occurrence: s.first.ID})
	sn := h.waitSnap("the fault", func(sn *Snapshot) bool { return len(sn.Faults()) == 1 })
	token, before := sn.IDArray()
	sn.IDs()[0] = 99
	e, _ := sn.Read(ids[0])
	e.Meta[0] = 'X'
	es, _ := sn.ReadList(ids)
	es[1].Meta[0] = 'Y'
	sn.Faults()[0].Reason = "changed"
	sn.Skips()[ids[0]] = Skip{Reason: "changed"}
	h.insert("c")
	h.must("delete a", h.eng.Delete(bg, ids[0]))
	if tok, got := sn.IDArray(); tok != token || !slices.Equal(got, before) {
		t.Fatalf("an old snapshot changed: token %d IDs %v, was %d %v", tok, got, token, before)
	}
	if e, _ := sn.Read(ids[0]); string(e.Meta) != "meta:a" {
		t.Fatalf("an old snapshot's metadata is %q", e.Meta)
	}
	if es, _ := sn.ReadList(ids); string(es[1].Meta) != "meta:b" {
		t.Fatalf("a ReadList copy reached the queue: %q", es[1].Meta)
	}
	if sn.Faults()[0].Reason != ReasonXrun || len(sn.Skips()) != 0 {
		t.Fatalf("faults %v skips %v", sn.Faults(), sn.Skips())
	}
	if e, _ := h.snap().Read(ids[1]); string(e.Meta) != "meta:b" {
		t.Fatalf("the live metadata is %q", e.Meta)
	}
}
