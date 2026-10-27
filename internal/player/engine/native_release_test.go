package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

type nativeRecorder struct {
	Backend
	requests    chan ReleaseRequest
	escalations chan ReleaseRequest
}

func (b *nativeRecorder) Release(ctx context.Context, s SessionID) error {
	b.requests <- ReleaseRequest{Token: ReleaseToken{Session: s}, Mode: ReleaseImmediate}
	return b.Backend.Release(ctx, s)
}
func (b *nativeRecorder) ReleaseNative(ctx context.Context, r ReleaseRequest) error {
	b.requests <- r
	return b.Backend.Release(ctx, r.Token.Session)
}
func (b *nativeRecorder) EscalateNative(r ReleaseRequest) { b.escalations <- r }

func TestNativeNavigationRelease(t *testing.T) {
	for _, action := range []string{"next", "previous", "seekid", "seekindex"} {
		t.Run(action, func(t *testing.T) {
			h := newHarness(t)
			var rec *nativeRecorder
			h.tweak = func(c *Config) {
				rec = &nativeRecorder{Backend: c.Backend, requests: make(chan ReleaseRequest, 8), escalations: make(chan ReleaseRequest, 8)}
				c.Backend = rec
			}
			h.boot()
			ids, s := h.playing(1, "a", "b", "c")
			m := h.appliedMark()
			s.ev.(DetailsEvents).Details(s.id, s.first.ID, Details{Bits: 1, Codec: "DSD", RateHz: 2822400})
			h.waitApplied("handled details", m)
			held := newGate()
			h.be.script(func() {
				h.be.onRelease = func(ctx context.Context, s *fakeSession) error {
					if e := held.wait(ctx, h.be, true); e != nil {
						return e
					}
					return s.release(ctx)
				}
			})
			answer := async(func() error {
				switch action {
				case "next":
					return h.eng.Next(bg)
				case "previous":
					return h.eng.Previous(bg)
				case "seekid":
					return h.eng.PlayID(bg, ids[0])
				default:
					return h.eng.PlayIndex(bg, 0)
				}
			})
			r := <-rec.requests
			if r.Mode != ReleaseTrackChange {
				t.Error("DSD navigation did not select TrackChange")
			}
			mark := h.appliedMark()
			stopped := async(func() error { return h.eng.Stop(bg) })
			h.waitApplied("stop", mark)
			select {
			case x := <-rec.escalations:
				if x.Mode != ReleaseImmediate {
					t.Error("Stop did not escalate Immediate")
				}
			case <-time.After(*watchdog):
				t.Error("Stop did not publish asynchronous escalation")
			}
			held.open()
			_ = h.result("navigation superseded", answer)
			h.must("stopped", h.result("stop", stopped))
		})
	}
}

func TestNativeRetiringPrefixKeepsIntent(t *testing.T) {
	h := newHarness(t)
	var rec *nativeRecorder
	h.tweak = func(c *Config) {
		rec = &nativeRecorder{Backend: c.Backend, requests: make(chan ReleaseRequest, 8), escalations: make(chan ReleaseRequest, 8)}
		c.Backend = rec
	}
	h.boot()
	ids, s := h.playing(0, "a", "b", "c")
	next := s.prepared(s.first.ID, 1)
	s.submitted(next.ID)
	mark := h.appliedMark()
	s.ev.(DetailsEvents).Details(s.id, s.first.ID, Details{Bits: 1, Codec: "DSD", RateHz: 2822400})
	h.waitApplied("handled details", mark)
	held := newGate()
	h.be.script(func() {
		h.be.onRelease = func(ctx context.Context, s *fakeSession) error {
			if e := held.wait(ctx, h.be, true); e != nil {
				return e
			}
			return s.release(ctx)
		}
	})
	answer := async(func() error { return h.eng.PlayID(bg, ids[2]) })
	r := <-rec.requests
	r.Token.Cutoff = 32
	ev := s.ev.(NativeReleaseEvents)
	ev.Retiring(r.Token, NativeEvent{Kind: "cutoff", Prefixes: []NativePrefix{{Occurrence: s.first.ID, End: 31}, {Occurrence: next.ID, Start: 31, End: 38}}})
	ev.Retiring(r.Token, NativeEvent{Kind: "details", Occurrence: next.ID, Details: Details{Bits: 1, Codec: "DSD", RateHz: 2822400}})
	ev.Retiring(r.Token, NativeEvent{Kind: "audible", Occurrence: next.ID})
	ev.Retiring(r.Token, NativeEvent{Kind: "position", Position: Position{Occurrence: next.ID, Frames: 7, RateHz: 2822400}})
	mark = h.appliedMark()
	s.ev.Position(s.id, Position{Occurrence: next.ID, Frames: 999, RateHz: 2822400})
	h.waitApplied("handled position", mark)
	if h.snap().Selected() != ids[2] || h.snap().Pending() != IntentPlay {
		t.Error("outgoing prefix changed destination")
	}
	if h.snap().Position().Frames != 7 {
		t.Errorf("outgoing prefix not accounted or late event accepted: %+v", h.snap().Position())
	}
	ev.Retiring(r.Token, NativeEvent{Kind: "fault", Fault: Fault{Reason: ReasonXrun, Occurrence: next.ID}})
	// An ordinary position is a barrier after the fault, not another native event.
	mark = h.appliedMark()
	s.ev.Position(s.id, Position{})
	h.waitApplied("handled position", mark)
	if h.snap().Pending() != IntentStop || len(h.snap().Faults()) != 1 {
		t.Error("tail fault lost or did not cancel autoplay")
	}
	held.open()
	_ = h.result("faulted navigation", answer)
	if h.be.starts() != 1 {
		t.Fatal("fault started successor")
	}
}

func TestNativeRetiringRejects(t *testing.T) {
	for _, kind := range []string{"prepared", "submitted", "unrelated", "late"} {
		t.Run(kind, func(t *testing.T) {
			h := startHarness(t)
			_, s := h.playing(0, "a", "b")
			next := s.prepared(s.first.ID, 1)
			mark := h.appliedMark()
			s.ev.(DetailsEvents).Details(s.id, s.first.ID, Details{Bits: 1, Codec: "DSD", RateHz: 2822400})
			h.waitApplied("handled details", mark)
			held := newGate()
			h.be.script(func() {
				h.be.onRelease = func(ctx context.Context, s *fakeSession) error {
					if e := held.wait(ctx, h.be, true); e != nil {
						return e
					}
					return s.release(ctx)
				}
			})
			stopped := async(func() error { return h.eng.Stop(bg) })
			h.waitSnap("retiring", func(s *Snapshot) bool { return s.State() == Releasing })
			token := ReleaseToken{Session: s.id, Generation: 1, Cutoff: 32}
			ev := s.ev.(NativeReleaseEvents)
			ev.Retiring(token, NativeEvent{Kind: "cutoff", Prefixes: []NativePrefix{{Occurrence: s.first.ID, End: 32}}})
			ev.Retiring(token, NativeEvent{Kind: "position", Position: Position{Occurrence: s.first.ID, Frames: 7, RateHz: 2822400}})
			if kind == "late" {
				held.open()
				h.must("stop", h.result("stop", stopped))
			}
			switch kind {
			case "prepared":
				ev.Retiring(token, NativeEvent{Kind: "prepared", Occurrence: next.ID})
			case "submitted":
				ev.Retiring(token, NativeEvent{Kind: "submitted", Occurrence: next.ID})
			case "unrelated":
				token.Generation++
				ev.Retiring(token, NativeEvent{Kind: "position", Position: Position{Occurrence: s.first.ID, Frames: 31, RateHz: 2822400}})
			case "late":
				ev.Retiring(token, NativeEvent{Kind: "position", Position: Position{Occurrence: s.first.ID, Frames: 31, RateHz: 2822400}})
			}
			mark = h.appliedMark()
			s.ev.Position(s.id, Position{})
			h.waitApplied("handled position", mark)
			if h.snap().Position().Frames != 7 || h.snap().Selected() == next.Entry {
				t.Fatalf("accepted %s event: %+v", kind, h.snap().Position())
			}
			if kind != "late" {
				held.open()
				h.must("stop", h.result("stop", stopped))
			}
		})
	}
}

type blockedNativeEscalation struct {
	*nativeRecorder
	entered, release chan struct{}
}

func (b *blockedNativeEscalation) EscalateNative(r ReleaseRequest) {
	close(b.entered)
	<-b.release
	b.nativeRecorder.EscalateNative(r)
}
func TestNativeEscalationDoesNotBlockOwner(t *testing.T) {
	h := newHarness(t)
	var rec *blockedNativeEscalation
	h.tweak = func(c *Config) {
		rec = &blockedNativeEscalation{nativeRecorder: &nativeRecorder{Backend: c.Backend, requests: make(chan ReleaseRequest, 8), escalations: make(chan ReleaseRequest, 8)}, entered: make(chan struct{}), release: make(chan struct{})}
		c.Backend = rec
	}
	h.boot()
	_, s := h.playing(0, "a", "b")
	mark := h.appliedMark()
	s.ev.(DetailsEvents).Details(s.id, s.first.ID, Details{Bits: 1, Codec: "DSD"})
	h.waitApplied("handled details", mark)
	held := newGate()
	h.be.script(func() {
		h.be.onRelease = func(ctx context.Context, s *fakeSession) error {
			if e := held.wait(ctx, h.be, true); e != nil {
				return e
			}
			return s.release(ctx)
		}
	})
	next := async(func() error { return h.eng.Next(bg) })
	<-rec.requests
	stop := async(func() error { return h.eng.Stop(bg) })
	<-rec.entered
	var once sync.Once
	release := func() { once.Do(func() { close(rec.release) }) }
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(bg, time.Second)
	defer cancel()
	if e := h.eng.SetRepeat(ctx, true); e != nil {
		t.Errorf("escalation blocked owner: %v", e)
	}
	release()
	held.open()
	_ = h.result("next", next)
	_ = h.result("stop", stop)
}

func TestNativeStartFailureDoesNotSkip(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "b")
	h.available()
	h.be.script(func() {
		h.be.onStart = func(ctx context.Context, s *fakeSession) error {
			s.ev.(NativePlanEvents).NativePlan(s.id, s.first.ID, NativePlan{Tuple: NativeTuple{Format: "DSD_U32_BE", BitRate: 2822400}, PolicyID: "retained"})
			return &Unplayable{Reason: ReasonBackend, Detail: "native activation failed"}
		}
	})
	_ = h.eng.PlayID(bg, ids[0])
	sn := h.waitSnap("faulted without fallback", func(sn *Snapshot) bool { return sn.State() == Stopped })
	if h.be.starts() != 1 || len(sn.Faults()) == 0 || len(sn.Skips()) != 0 || sn.Pending() != IntentStop {
		t.Fatalf("native activation retried/skipped instead of faulting: starts=%d faults=%v skips=%v", h.be.starts(), sn.Faults(), sn.Skips())
	}
}
func TestPCMNavigationKeepsRelease(t *testing.T) {
	h := newHarness(t)
	var rec *nativeRecorder
	h.tweak = func(c *Config) {
		rec = &nativeRecorder{Backend: c.Backend, requests: make(chan ReleaseRequest, 8), escalations: make(chan ReleaseRequest, 8)}
		c.Backend = rec
	}
	h.boot()
	h.playing(0, "a", "b")
	h.must("PCM next", h.eng.Next(bg))
	if r := <-rec.requests; r.Mode != ReleaseImmediate {
		t.Fatalf("PCM Next changed release: %+v", r)
	}
}
