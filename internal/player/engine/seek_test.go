package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
)

// pcmMinute is a minute of PCM at 44.1 kHz, as the backend reports it.
var pcmMinute = Details{Frames: 60 * 44100, RateHz: 44100, Bits: 16, Channels: 2, Codec: "FLAC", Lossless: true}

// heardAt plays the entry at index i of refs, its content reported as
// d as every session starts, heard and measured frames in, its hand-off
// named (what follows it, or the end), and returns the IDs and the session.
func (h *harness) heardAt(i int, d Details, frames int64, refs ...string) ([]uint32, *fakeSession) {
	h.t.Helper()
	h.be.script(func() { h.be.startDetails = &d })
	ids, s := h.playing(i, refs...)
	s.prepared(s.first.ID, 1)
	h.measure(s, s.first.ID, frames)
	return ids, s
}

// measure reports frames of o played through s and waits until the engine
// holds that position.
func (h *harness) measure(s *fakeSession, o OccurrenceID, frames int64) {
	h.t.Helper()
	s.position(o, frames)
	h.waitSnap("the position measured", func(sn *Snapshot) bool {
		p := sn.Position()
		return p.Occurrence == o && p.Frames == frames
	})
}

// heard waits until s's occurrence o is heard, as s reports it.
func (h *harness) heard(s *fakeSession, o OccurrenceID) *Snapshot {
	h.t.Helper()
	s.audible(o)
	return h.waitSnap("the occurrence heard", func(sn *Snapshot) bool {
		a, ok := sn.Audible()
		return ok && a.Occurrence == o && sn.State() == Playing && !sn.Starting()
	})
}

// transportCalls is the calls since mark, the Prepares aside.
func (h *harness) transportCalls(mark int) []string {
	var out []string
	for _, c := range h.be.callsSince(mark) {
		if !strings.HasPrefix(c, "prepare") {
			out = append(out, c)
		}
	}
	return out
}

// unchanged fails the test unless sn is before as a reader sees it: the
// generation, the transport, the selection, the track and the position.
func unchanged(t *testing.T, what string, before, sn *Snapshot) {
	t.Helper()
	bt, bok := before.Track()
	at, aok := sn.Track()
	if sn.Generation() != before.Generation() || sn.State() != before.State() || sn.Selected() != before.Selected() ||
		bok != aok || bt.Occurrence != at.Occurrence || sn.Position() != before.Position() || sn.TrackCount() != before.TrackCount() {
		t.Errorf("%s changed the engine: %s at %d, track %d, position %+v; was %s at %d, track %d, position %+v",
			what, sn.State(), sn.Selected(), at.Occurrence, sn.Position(), before.State(), before.Selected(), bt.Occurrence, before.Position())
	}
}

// TestASeekPlaysTheTrackHeardFromItsTarget pins a seek while playing (B674):
// the session is released to play, the track heard kept, its position the
// target at once, the selection and the queue's token untouched; once the
// release is acknowledged a session starts whose first occurrence is the one
// heard, under its ID, from the target's frame, and the seek answers as it
// starts; heard, its position counts from the target, with no track counted
// again and no change of the details.
func TestASeekPlaysTheTrackHeardFromItsTarget(t *testing.T) {
	h := startHarness(t)
	ids, s := h.heardAt(0, pcmMinute, 5*44100, "a", "b")
	x := s.first.ID
	before := h.snap()
	g := holdReleases(h)
	mark := h.be.mark()
	done := async(func() error { return h.eng.SeekAbsolute(bg, 30) })
	sn := h.waitSnap("the seek's release", func(sn *Snapshot) bool { return sn.State() == Releasing })
	if sn.Pending() != IntentPlay || sn.Selected() != ids[0] || sn.Token() != before.Token() {
		t.Errorf("during the seek's release: pending %s, selected %d, token %d; want play, a, %d", sn.Pending(), sn.Selected(), sn.Token(), before.Token())
	}
	if tr, ok := sn.Track(); !ok || tr.Occurrence != x || tr.Details != pcmMinute || sn.TrackCount() != 1 {
		t.Errorf("during the seek's release the track is %+v (%v), %d heard; want occurrence %d kept", tr, ok, sn.TrackCount(), x)
	}
	if p := sn.Position(); p != (Position{Occurrence: x, Frames: 30 * 44100, RateHz: 44100}) {
		t.Errorf("during the seek's release the position is %+v, want the target", p)
	}
	g.open()
	h.must("seek", h.result("the seek", done))
	s2 := h.be.nth(2)
	if s2.first.ID != x || s2.first.Entry != ids[0] || s2.from != 30*44100 {
		t.Fatalf("the seek started occurrence %d of %d from %d; want %d of a from %d", s2.first.ID, s2.first.Entry, s2.from, x, 30*44100)
	}
	if calls := h.transportCalls(mark); !slices.Equal(calls, []string{"release", fmt.Sprintf("start %d", ids[0])}) {
		t.Errorf("the seek's calls %v, want release, start a", calls)
	}
	if sn := h.snap(); sn.State() != Playing || !sn.Starting() || sn.Position().Frames != 30*44100 {
		t.Errorf("as the seek answers: %s (starting %v) at %+v", sn.State(), sn.Starting(), sn.Position())
	}
	sn = h.heard(s2, x)
	if p := sn.Position(); p != (Position{Occurrence: x, Frames: 30 * 44100, RateHz: 44100}) || sn.TrackCount() != 1 || sn.DetailsCount() != before.DetailsCount() {
		t.Errorf("heard from the target: position %+v, %d tracks, %d changes of the details; want the target, 1, %d", p, sn.TrackCount(), sn.DetailsCount(), before.DetailsCount())
	}
	h.measure(s2, x, 32*44100)
	if sn := h.snap(); sn.Selected() != ids[0] || sn.Token() != before.Token() || sn.Generation() == before.Generation() {
		t.Errorf("after the seek: selected %d, token %d, generation %v; want a, the queue's token, a generation moved", sn.Selected(), sn.Token(), sn.Generation())
	}
}

// TestARelativeSeekCountsFromThePosition pins the relative seek's base: the
// position the backend last measured; while the session at a seek's target
// starts, that target; and a target before the start is the start, one at or
// past the end refused, changing nothing.
func TestARelativeSeekCountsFromThePosition(t *testing.T) {
	h := startHarness(t)
	_, s := h.heardAt(0, pcmMinute, 10*44100, "a")
	x := s.first.ID
	h.must("seek +5 s", h.eng.SeekRelative(bg, 5))
	if s2 := h.be.nth(2); s2.first.ID != x || s2.from != 15*44100 {
		t.Fatalf("+5 s at 10 s started %d from %d, want %d from %d", s2.first.ID, s2.from, x, 15*44100)
	}
	h.must("seek -3 s while the target's session starts", h.eng.SeekRelative(bg, -3))
	s3 := h.be.nth(3)
	if s3.first.ID != x || s3.from != 12*44100 {
		t.Fatalf("-3 s from the target 15 s started %d from %d, want %d from %d", s3.first.ID, s3.from, x, 12*44100)
	}
	h.heard(s3, x)
	h.measure(s3, x, 13*44100)
	h.must("seek -100 s", h.eng.SeekRelative(bg, -100))
	s4 := h.be.nth(4)
	if s4.first.ID != x || s4.from != 0 {
		t.Fatalf("-100 s at 13 s started %d from %d, want %d from the start", s4.first.ID, s4.from, x)
	}
	before := h.snap()
	h.refused("+60 s from the start of a minute", h.eng.SeekRelative(bg, 60), ReasonSeekOutOfRange)
	unchanged(t, "a relative seek to the end", before, h.snap())
	if h.be.starts() != 4 {
		t.Errorf("a refused seek started a session")
	}
}

// TestTheSeeksBoundaries pins the target's edges on a minute of content: 0
// plays the track again from its first frame, the last whole second plays,
// and the exact end (nothing would be left to play), past it, an absolute
// target before the start, one the arithmetic cannot hold, and any in content
// of no known length or rate are seek_out_of_range; a relative target the
// arithmetic cannot hold below the start is the start; content the backend
// reported nothing of is seek_unavailable. A refusal makes no call and
// changes nothing.
func TestTheSeeksBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		d        *Details
		relative bool
		seconds  int64
		from     int64
		refused  Reason
	}{
		{"zero is the start", &pcmMinute, false, 0, 0, ""},
		{"the last whole second", &pcmMinute, false, 59, 59 * 44100, ""},
		{"the exact end", &pcmMinute, false, 60, 0, ReasonSeekOutOfRange},
		{"past the end", &pcmMinute, false, 61, 0, ReasonSeekOutOfRange},
		{"before the start", &pcmMinute, false, -1, 0, ReasonSeekOutOfRange},
		{"past the arithmetic", &pcmMinute, false, math.MaxInt64, 0, ReasonSeekOutOfRange},
		{"relative past the arithmetic", &pcmMinute, true, math.MaxInt64, 0, ReasonSeekOutOfRange},
		{"relative below the arithmetic", &pcmMinute, true, math.MinInt64, 0, ""},
		{"no known length", &Details{RateHz: 44100, Bits: 16, Channels: 2, Codec: "FLAC", Lossless: true}, false, 1, 0, ReasonSeekOutOfRange},
		{"no known rate", &Details{Frames: 60 * 44100, Bits: 16, Channels: 2, Codec: "WAV", Lossless: true}, false, 1, 0, ReasonSeekOutOfRange},
		{"nothing reported", nil, false, 1, 0, ReasonSeekUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := startHarness(t)
			if c.d != nil {
				h.be.script(func() { h.be.startDetails = c.d })
			}
			_, s := h.playing(0, "a")
			s.prepared(s.first.ID, 1)
			h.measure(s, s.first.ID, 5*44100)
			before, mark := h.snap(), h.be.mark()
			seek := h.eng.SeekAbsolute
			if c.relative {
				seek = h.eng.SeekRelative
			}
			err := seek(bg, c.seconds)
			if c.refused != "" {
				h.refused(c.name, err, c.refused)
				unchanged(t, c.name, before, h.snap())
				if calls := h.be.callsSince(mark); len(calls) != 0 {
					t.Errorf("a refused seek made the calls %v", calls)
				}
				return
			}
			h.must(c.name, err)
			if s2 := h.be.nth(2); s2.first.ID != s.first.ID || s2.from != c.from {
				t.Errorf("started occurrence %d from %d, want %d from %d", s2.first.ID, s2.from, s.first.ID, c.from)
			}
		})
	}
}

// TestAPausedSeekStaysPausedAtItsTarget pins a seek while paused: the paused
// session is released and the transport stays paused at the target with no
// session, the track kept and the position the target; another seek there
// moves the target with no call; Play starts the session at the target,
// continuing the occurrence; a Stop there forgets the target and the track.
func TestAPausedSeekStaysPausedAtItsTarget(t *testing.T) {
	h := startHarness(t)
	_, s := h.heardAt(0, pcmMinute, 5*44100, "a")
	x := s.first.ID
	h.must("pause", h.eng.Pause(bg))
	h.must("seek while paused", h.eng.SeekAbsolute(bg, 20))
	sn := h.snap()
	if sn.State() != Paused || h.be.starts() != 1 || !s.isAcked() {
		t.Fatalf("after a seek while paused: %s with %d sessions (the paused one released %v); want paused with none", sn.State(), h.be.starts(), s.isAcked())
	}
	if tr, ok := sn.Track(); !ok || tr.Occurrence != x || sn.Position() != (Position{Occurrence: x, Frames: 20 * 44100, RateHz: 44100}) {
		t.Errorf("paused at the target: track %+v (%v), position %+v", tr, ok, sn.Position())
	}
	mark := h.be.mark()
	h.must("seek +5 s while paused at the target", h.eng.SeekRelative(bg, 5))
	if calls := h.be.callsSince(mark); len(calls) != 0 || h.snap().Position().Frames != 25*44100 {
		t.Errorf("a seek at a target no session plays made %v and left the position at %+v; want none, 25 s", calls, h.snap().Position())
	}
	h.must("play", h.eng.Play(bg))
	if s2 := h.be.nth(2); s2.first.ID != x || s2.from != 25*44100 {
		t.Fatalf("Play at the target started %d from %d, want %d from %d", s2.first.ID, s2.from, x, 25*44100)
	}
	h.must("pause again", h.eng.Pause(bg))
	h.must("seek while paused again", h.eng.SeekAbsolute(bg, 40))
	h.must("stop", h.eng.Stop(bg))
	if sn := h.snap(); sn.State() != Stopped {
		t.Fatalf("stop at the target: %s", sn.State())
	} else if _, ok := sn.Track(); ok {
		t.Errorf("stopped at a seek's target, the track is still heard")
	}
	h.must("play after the stop", h.eng.Play(bg))
	if s3 := h.be.nth(3); s3.first.ID == x || s3.from != 0 {
		t.Errorf("Play after a stop at a target started %d from %d; want a new occurrence from the start", s3.first.ID, s3.from)
	}
}

// TestARepeatedSeekMovesItsTarget pins seeks one after another: a seek while
// the release the last made is under way moves the target with no release of
// its own, and both answer once the session at the last target starts; a
// seek while that session starts releases it and starts one at its own.
func TestARepeatedSeekMovesItsTarget(t *testing.T) {
	h := startHarness(t)
	_, s := h.heardAt(0, pcmMinute, 5*44100, "a")
	x := s.first.ID
	g := holdReleases(h)
	mark := h.be.mark()
	first := async(func() error { return h.eng.SeekAbsolute(bg, 30) })
	h.waitSnap("the seek's release", func(sn *Snapshot) bool { return sn.State() == Releasing })
	applied := h.appliedMark()
	second := async(func() error { return h.eng.SeekAbsolute(bg, 40) })
	h.waitApplied("seek absolute", applied)
	if p := h.snap().Position(); p.Frames != 40*44100 {
		t.Errorf("the position during the release, after the second seek: %+v, want 40 s", p)
	}
	g.open()
	h.must("the first seek", h.result("the first seek", first))
	h.must("the second seek", h.result("the second seek", second))
	if s2 := h.be.nth(2); s2.first.ID != x || s2.from != 40*44100 {
		t.Fatalf("the seeks started %d from %d, want %d from the last target, %d", s2.first.ID, s2.from, x, 40*44100)
	}
	if calls := h.transportCalls(mark); !slices.Equal(calls, []string{"release", fmt.Sprintf("start %d", h.be.nth(2).first.Entry)}) {
		t.Errorf("two seeks made %v, want one release and one start", calls)
	}
	h.must("a seek while the target's session starts", h.eng.SeekAbsolute(bg, 50))
	if s3 := h.be.nth(3); s3.first.ID != x || s3.from != 50*44100 {
		t.Fatalf("a seek while the target's session started %d from %d, want %d from %d", s3.first.ID, s3.from, x, 50*44100)
	}
}

// seekReleased starts a seek to 30 s of a, b's a, playing, and holds its
// release: the seek, the release's gate, a's occurrence and the IDs.
func seekReleased(t *testing.T, h *harness) (<-chan error, gate, OccurrenceID, []uint32) {
	t.Helper()
	ids, s := h.heardAt(0, pcmMinute, 5*44100, "a", "b")
	g := holdReleases(h)
	done := async(func() error { return h.eng.SeekAbsolute(bg, 30) })
	h.waitSnap("the seek's release", func(sn *Snapshot) bool { return sn.State() == Releasing })
	return done, g, s.first.ID, ids
}

// TestWhatSupersedesASeekDropsIt pins what replaces a seek's playback before
// it starts: a Stop, a Next, a PlayID of its own entry, a DeleteAll, a
// deletion of its entry and maintenance. Each forgets the target and the
// track the seek kept, the seek answers superseded (or maintenance), and no
// session ever starts at the target: what follows plays from an entry's
// start, as a new occurrence, or nothing plays.
func TestWhatSupersedesASeekDropsIt(t *testing.T) {
	for _, c := range []struct {
		name   string
		do     func(h *harness, ids []uint32) error
		why    Reason
		starts uint32 // 0: nothing starts; otherwise the index+1 of the entry that plays from its start
	}{
		{"stop", func(h *harness, _ []uint32) error { return h.eng.Stop(bg) }, ReasonSuperseded, 0},
		{"next", func(h *harness, _ []uint32) error { return h.eng.Next(bg) }, ReasonSuperseded, 2},
		{"play id of its own entry", func(h *harness, ids []uint32) error { return h.eng.PlayID(bg, ids[0]) }, ReasonSuperseded, 1},
		{"delete all", func(h *harness, _ []uint32) error { return h.eng.DeleteAll(bg) }, ReasonSuperseded, 0},
		{"a deletion of its entry", func(h *harness, ids []uint32) error { return h.eng.Delete(bg, ids[0]) }, ReasonSuperseded, 2},
		{"maintenance", func(h *harness, _ []uint32) error {
			_, err := h.eng.EnterMaintenance(bg, h.snap().Generation(), "the test")
			return err
		}, ReasonMaintenance, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := startHarness(t)
			done, g, x, ids := seekReleased(t, h)
			other := async(func() error { return c.do(h, ids) })
			h.waitSnap("the seek's target forgotten", func(sn *Snapshot) bool { _, ok := sn.Track(); return !ok })
			g.open()
			h.must(c.name, h.result(c.name, other))
			h.refused("the superseded seek", h.result("the seek", done), c.why)
			if c.starts == 0 {
				sn := h.waitSnap("nothing playing", func(sn *Snapshot) bool { return sn.State() == Stopped || sn.State() == Maintenance })
				if h.be.starts() != 1 {
					t.Errorf("a session started after the seek was dropped")
				}
				if _, ok := sn.Track(); ok {
					t.Errorf("the track the seek kept is still heard")
				}
				return
			}
			s2 := h.be.nth(2)
			if s2.first.Entry != ids[c.starts-1] || s2.first.ID == x || s2.from != 0 {
				t.Errorf("after the seek was dropped, occurrence %d of %d started from %d; want a new occurrence of %d from its start",
					s2.first.ID, s2.first.Entry, s2.from, ids[c.starts-1])
			}
		})
	}
}

// TestTheOutputGoingAwayDropsASeek pins a seek against the output (the
// hotplug watcher's reports): the output going away while the seek's release
// is under way drops the pending play and the target, nothing starts, the
// position stays frozen at the target, and the seek answers with the
// output's reason; going away while paused at a target stops there,
// forgetting it, and its return plays nothing; and once the output has gone
// away a seek finds no track to seek in.
func TestTheOutputGoingAwayDropsASeek(t *testing.T) {
	t.Run("during the seek's release", func(t *testing.T) {
		h := startHarness(t)
		done, g, x, _ := seekReleased(t, h)
		h.report(OutputReport{Available: false, Reason: "device_missing"})
		g.open()
		h.refused("the seek", h.result("the seek", done), ReasonOutputUnavailable)
		sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
		if h.be.starts() != 1 {
			t.Errorf("a session started at the target with the output gone")
		}
		if _, ok := sn.Track(); ok || sn.Position().Occurrence != x || sn.Position().Frames != 30*44100 {
			t.Errorf("the output gone during a seek: track heard %v, position %+v; want none heard, the position frozen at the target", ok, sn.Position())
		}
	})
	t.Run("paused at the target", func(t *testing.T) {
		h := startHarness(t)
		h.heardAt(0, pcmMinute, 5*44100, "a")
		h.must("pause", h.eng.Pause(bg))
		h.must("seek", h.eng.SeekAbsolute(bg, 20))
		h.report(OutputReport{Available: false, Reason: "device_missing"})
		sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
		if _, ok := sn.Track(); ok {
			t.Errorf("stopped by the output at a seek's target, the track is still heard")
		}
		h.available()
		if sn := h.snap(); sn.State() != Stopped || h.be.starts() != 1 {
			t.Errorf("the output's return played: %s, %d sessions", sn.State(), h.be.starts())
		}
	})
	t.Run("after the output went away", func(t *testing.T) {
		h := startHarness(t)
		_, s := h.heardAt(0, pcmMinute, 5*44100, "a")
		h.report(OutputReport{Available: false, Reason: "device_missing"})
		h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
		h.refused("a seek with nothing held", h.eng.SeekAbsolute(bg, 10), ReasonSeekUnavailable)
		if h.be.starts() != 1 || !s.isAcked() {
			t.Errorf("a refused seek started a session")
		}
	})
}

// TestTheReleasedSessionsEventsAreNotTheTracks pins the session as a seek's
// fence: what the released session reports after the seek (a position of
// the occurrence the seek continues, under the same ID, a boundary into its
// successor, a fault and its end), while the session at the target starts and
// once it is heard and measured, changes none of the track, its position, the
// selection or the transport: the owner handles each and discards it.
func TestTheReleasedSessionsEventsAreNotTheTracks(t *testing.T) {
	h := startHarness(t)
	ids, s := h.heardAt(0, pcmMinute, 5*44100, "a", "b")
	x := s.first.ID
	next := s.successor(x)
	s.submitted(next.ID)
	h.must("seek", h.eng.SeekAbsolute(bg, 30))
	s2 := h.be.nth(2)
	stale := func(frames int64) {
		t.Helper()
		mark := h.appliedMark()
		s.position(x, frames)
		s.audible(next.ID)
		s.fault(Fault{Reason: ReasonXrun, Occurrence: next.ID, Frames: 10})
		s.finished()
		for _, what := range []string{"position", "audible", "fault", "finished"} {
			h.waitApplied("handled "+what, mark)
		}
	}
	stale(50 * 44100)
	if sn := h.snap(); sn.State() != Playing || !sn.Starting() || sn.Selected() != ids[0] || len(sn.Faults()) != 0 || sn.Position().Frames != 30*44100 {
		t.Errorf("the released session's events while the target's session starts: %s (starting %v) at %d, %d faults, position %+v; want it starting at a, the target",
			sn.State(), sn.Starting(), sn.Selected(), len(sn.Faults()), sn.Position())
	}
	h.heard(s2, x)
	h.measure(s2, x, 31*44100)
	stale(55 * 44100)
	sn := h.snap()
	if p := sn.Position(); p != (Position{Occurrence: x, Frames: 31 * 44100, RateHz: 44100}) {
		t.Errorf("the position after the released session's events: %+v, want the session at the target's, 31 s", p)
	}
	if sn.Selected() != ids[0] || sn.State() != Playing || len(sn.Faults()) != 0 || sn.TrackCount() != 1 {
		t.Errorf("after the released session's events: %s at %d, %d faults, %d tracks; want playing a, none, 1", sn.State(), sn.Selected(), len(sn.Faults()), sn.TrackCount())
	}
	if tr, _ := sn.Track(); tr.Occurrence != x {
		t.Errorf("the track heard is occurrence %d, want %d", tr.Occurrence, x)
	}
	// The successor's gapless hand-off goes on from the target: the session
	// at the target names b after the occurrence it continues.
	if n := s2.successor(x); n.Entry != ids[1] || n.ID == next.ID {
		t.Errorf("the session at the target named %d (occurrence %d) next, want b anew", n.Entry, n.ID)
	}
}

// TestASeekWaitsItsTurn pins a seek's lane: sent while a start is under way,
// it waits, and runs once the start answers, with the content the backend
// reported then; one waiting there that a Stop overtakes never runs; one
// whose context has ended is not admitted; one sent while a Pause is under
// way runs once the pause is made, as a paused seek.
func TestASeekWaitsItsTurn(t *testing.T) {
	holdStarts := func(h *harness) gate {
		g := newGate()
		h.be.script(func() {
			h.be.onStart = func(ctx context.Context, s *fakeSession) error {
				if err := g.wait(ctx, h.be, true); err != nil {
					return err
				}
				return s.open()
			}
		})
		return g
	}
	t.Run("behind a start", func(t *testing.T) {
		h := startHarness(t)
		h.be.script(func() { h.be.startDetails = &pcmMinute })
		ids := h.insert("a")
		h.available()
		g := holdStarts(h)
		play := async(func() error { return h.eng.PlayID(bg, ids[0]) })
		s := h.be.nth(1)
		applied := h.appliedMark()
		seek := async(func() error { return h.eng.SeekAbsolute(bg, 30) })
		h.waitApplied("admitted seek absolute", applied)
		if n := h.be.count("release"); n != 0 {
			t.Fatalf("a seek behind a start released %d sessions before the start answered", n)
		}
		g.open()
		h.must("play", h.result("play", play))
		h.must("seek", h.result("seek", seek))
		if s2 := h.be.nth(2); s2.first.ID != s.first.ID || s2.from != 30*44100 {
			t.Errorf("the seek behind a start started %d from %d, want %d from %d", s2.first.ID, s2.from, s.first.ID, 30*44100)
		}
	})
	t.Run("overtaken by a stop", func(t *testing.T) {
		h := startHarness(t)
		h.be.script(func() { h.be.startDetails = &pcmMinute })
		ids := h.insert("a")
		h.available()
		g := holdStarts(h)
		play := async(func() error { return h.eng.PlayID(bg, ids[0]) })
		h.be.nth(1)
		applied := h.appliedMark()
		seek := async(func() error { return h.eng.SeekAbsolute(bg, 30) })
		h.waitApplied("admitted seek absolute", applied)
		stop := async(func() error { return h.eng.Stop(bg) })
		h.refused("the seek a stop overtook", h.result("seek", seek), ReasonOvertaken)
		g.open()
		h.must("stop", h.result("stop", stop))
		_ = h.result("play", play)
		h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
		if h.be.starts() != 1 {
			t.Errorf("an overtaken seek started a session")
		}
	})
	t.Run("not admitted", func(t *testing.T) {
		h := startHarness(t)
		h.heardAt(0, pcmMinute, 5*44100, "a")
		before := h.snap()
		ctx, cancel := context.WithCancel(bg)
		cancel()
		err := h.eng.SeekAbsolute(ctx, 30)
		if !errors.Is(err, ErrNotAdmitted) {
			t.Fatalf("a seek whose context had ended: %v, want not admitted", err)
		}
		unchanged(t, "a seek not admitted", before, h.snap())
	})
	t.Run("behind a pause", func(t *testing.T) {
		h := startHarness(t)
		_, s := h.heardAt(0, pcmMinute, 5*44100, "a")
		g := newGate()
		h.be.script(func() {
			h.be.onPause = func(ctx context.Context, _ *fakeSession) error { return g.wait(ctx, h.be, true) }
		})
		pause := async(func() error { return h.eng.Pause(bg) })
		h.be.waitFor("the pause", func() bool { return h.be.countLocked("pause") == 1 })
		applied := h.appliedMark()
		seek := async(func() error { return h.eng.SeekAbsolute(bg, 30) })
		h.waitApplied("admitted seek absolute", applied)
		g.open()
		h.must("pause", h.result("pause", pause))
		h.must("seek", h.result("seek", seek))
		sn := h.snap()
		if sn.State() != Paused || h.be.starts() != 1 || !s.isAcked() || sn.Position().Frames != 30*44100 {
			t.Errorf("a seek behind a pause: %s, %d sessions (released %v), position %+v; want paused at the target with none", sn.State(), h.be.starts(), s.isAcked(), sn.Position())
		}
	})
}

// TestDSDRefusesASeekAndChangesNothing pins DSD's refusal: native DSD known
// by its Details, and DoP known by its native plan alone (DoP is DSD carried
// in PCM words), playing or paused, refuse a seek with seek_unsupported,
// making no call and changing nothing: not the transport, the track, its
// position or the session.
func TestDSDRefusesASeekAndChangesNothing(t *testing.T) {
	native := Details{Frames: 60 * 2822400, RateHz: 2822400, Bits: 1, Channels: 2, Codec: "DSD", Lossless: true}
	dop := NativePlan{Tuple: NativeTuple{Transport: "dop", Format: "S32_LE", BitRate: 2822400}, PolicyID: "dop-64"}
	for _, c := range []struct {
		name string
		d    *Details
		plan *NativePlan
	}{{"native DSD by its details", &native, nil}, {"DoP by its plan", nil, &dop}, {"DoP by its plan beside PCM-shaped details", &pcmMinute, &dop}} {
		for _, paused := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, paused %v", c.name, paused), func(t *testing.T) {
				h := startHarness(t)
				ids := h.insert("a")
				h.available()
				h.must("play", h.eng.PlayID(bg, ids[0]))
				s := h.be.nth(1)
				if c.plan != nil {
					s.ev.(NativePlanEvents).NativePlan(s.id, s.first.ID, *c.plan)
				}
				if c.d != nil {
					s.details(s.first.ID, *c.d)
				}
				h.heard(s, s.first.ID)
				s.prepared(s.first.ID, 1)
				h.measure(s, s.first.ID, 5*44100)
				if paused {
					h.must("pause", h.eng.Pause(bg))
				}
				before, mark := h.snap(), h.be.mark()
				h.refused("an absolute seek in DSD", h.eng.SeekAbsolute(bg, 10), ReasonSeekUnsupported)
				h.refused("a relative seek in DSD", h.eng.SeekRelative(bg, -1), ReasonSeekUnsupported)
				unchanged(t, "a seek in DSD", before, h.snap())
				if calls := h.be.callsSince(mark); len(calls) != 0 {
					t.Errorf("a seek in DSD made the calls %v", calls)
				}
			})
		}
	}
}

// TestASeekWhoseStartFailsStops pins a seek whose session at the target the
// backend refuses to start (another process took the device during the
// release): the seek answers the backend's failure, playback stops with it,
// and the track the seek kept is no longer heard.
func TestASeekWhoseStartFailsStops(t *testing.T) {
	h := startHarness(t)
	h.heardAt(0, pcmMinute, 5*44100, "a")
	h.be.script(func() {
		h.be.onStart = func(context.Context, *fakeSession) error {
			return fmt.Errorf("the device is held: %w", ErrDeviceBusy)
		}
	})
	h.refused("a seek whose start fails", h.eng.SeekAbsolute(bg, 30), ReasonBackend)
	sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
	if _, ok := sn.Track(); ok || !sn.LastAttempt().Busy {
		t.Errorf("after a seek whose start found the device busy: track heard %v, last attempt %+v", ok, sn.LastAttempt())
	}
}
