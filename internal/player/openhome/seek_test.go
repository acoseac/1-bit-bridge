package openhome

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// minute is a minute of 16-bit 44.1 kHz FLAC, as the backend reports it.
var minute = engine.Details{Frames: 60 * 44100, RateHz: 44100, Bits: 16, Channels: 2, Codec: "FLAC", Lossless: true}

// timeNow is Time through h: its TrackCount, Duration and Seconds.
func timeNow(t *testing.T, h *handler) (tracks, duration, seconds string) {
	t.Helper()
	got := read(t, h, timeSvc, "Time")
	return got["TrackCount"], got["Duration"], got["Seconds"]
}

// detailed reports d of o's content through s, as the backend does, and
// waits until the engine's track heard holds them.
func (p *testPlayer) detailed(s *fakeSession, o engine.OccurrenceID, d engine.Details) {
	p.tb.Helper()
	s.ev.(engine.DetailsEvents).Details(s.id, o, d)
	p.waitSnap("the details reported", func(sn *engine.Snapshot) bool {
		tr, ok := sn.Track()
		return ok && tr.Occurrence == o && tr.HasDetails && tr.Details == d
	})
}

// measured reports frames of o's content played through s, and waits until
// the engine holds the position.
func (p *testPlayer) measured(s *fakeSession, o engine.OccurrenceID, frames int64) {
	p.tb.Helper()
	s.position(o, frames, 44100)
	p.waitSnap("the position measured", func(sn *engine.Snapshot) bool {
		pos := sn.Position()
		return pos.Occurrence == o && pos.Frames == frames
	})
}

// TestASeekWithinTheTrackIsTheEnginesSeek pins Playlist's two seeks within a
// track through the SOAP layer (B674): SeekSecondAbsolute plays the track
// heard from its target and SeekSecondRelative from its position plus the
// value, each answering success once the session started at the target
// plays; the session started for a seek continues the occurrence heard, from
// the target's frame (the seconds at the content's rate), so the track, its
// TrackCount and Id stay as they were while Time's Seconds reads the target
// at once and then the target plus what is played; what the released session
// still reports (a position, its end) is not the track's; a relative target
// before the start is the start; and a target at or past the content's end,
// the exact end included, is 803, Seek failed, changing nothing.
func TestASeekWithinTheTrackIsTheEnginesSeek(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	a := p.add(uriOf(goldenID), []byte("meta a"))
	p.add(uriOf(trackB), nil)
	read(t, h, playlist, "Play")
	s1 := p.be.session(t, 1)
	x := s1.first.ID
	s1.hear(x, &minute)
	p.heardNow(x)
	p.measured(s1, x, 10*44100)
	if tracks, duration, seconds := timeNow(t, h); tracks != "1" || duration != "60" || seconds != "10" {
		t.Fatalf("before the seek: Time %s, %s, %s; want 1, 60, 10", tracks, duration, seconds)
	}

	read(t, h, playlist, "SeekSecondAbsolute", "Value", "30")
	s2 := p.be.session(t, 2)
	if s2.first.ID != x || s2.first.Entry != a || s2.from != 30*44100 {
		t.Fatalf("the seek started occurrence %d of entry %d from frame %d; want occurrence %d of %d from %d",
			s2.first.ID, s2.first.Entry, s2.from, x, a, 30*44100)
	}
	if tracks, duration, seconds := timeNow(t, h); tracks != "1" || duration != "60" || seconds != "30" {
		t.Errorf("as the seek answers: Time %s, %s, %s; want the track it continues, 1, 60, and the target, 30", tracks, duration, seconds)
	}
	if got := selected(t, h); got != u(a) {
		t.Errorf("Id %s after the seek, want the entry it plays, %d", got, a)
	}
	if got := state(t, h); got != "Buffering" {
		t.Errorf("TransportState %s while the session at the target starts, want Buffering", got)
	}

	// What the released session reports now is not the track's: the events
	// reach the engine before the new session's, in order.
	s1.position(x, 50*44100, 44100)
	s1.ev.Finished(s1.id)
	s2.hear(x, &minute)
	p.waitSnap("the session at the target heard", func(sn *engine.Snapshot) bool { return sn.State() == engine.Playing && !sn.Starting() })
	if tracks, _, seconds := timeNow(t, h); tracks != "1" || seconds != "30" {
		t.Errorf("after the released session's position and end: Time %s tracks at %s s; want 1 at 30", tracks, seconds)
	}
	p.measured(s2, x, 32*44100)
	if _, _, seconds := timeNow(t, h); seconds != "32" {
		t.Errorf("two seconds past the target: Seconds %s, want 32", seconds)
	}
	// Once the session at the target is heard too: the released session's
	// position of the same occurrence and its end, then a report of the new
	// session's, which the engine handles after them.
	s1.position(x, 55*44100, 44100)
	s1.ev.Finished(s1.id)
	later := minute
	later.Bits = 24
	p.detailed(s2, x, later)
	if _, _, seconds := timeNow(t, h); seconds != "32" || state(t, h) != "Playing" {
		t.Errorf("after the released session's late position and end: Seconds %s, %s; want 32, Playing", seconds, state(t, h))
	}

	read(t, h, playlist, "SeekSecondRelative", "Value", "-5")
	s3 := p.be.session(t, 3)
	if s3.first.ID != x || s3.from != 27*44100 {
		t.Fatalf("a relative seek of -5 s at 32 s started occurrence %d from %d; want %d from %d", s3.first.ID, s3.from, x, 27*44100)
	}
	if tracks, _, seconds := timeNow(t, h); tracks != "1" || seconds != "27" {
		t.Errorf("after the relative seek: Time %s tracks at %s s; want 1 at 27", tracks, seconds)
	}

	read(t, h, playlist, "SeekSecondRelative", "Value", "-100")
	s4 := p.be.session(t, 4)
	if s4.first.ID != x || s4.from != 0 {
		t.Fatalf("a relative seek before the start started occurrence %d from %d; want %d from the start", s4.first.ID, s4.from, x)
	}

	before := p.eng.Snapshot()
	for _, value := range []string{"60", "61", "4294967295"} {
		if r := soapFault(t, h, playlist, "SeekSecondAbsolute", "Value", value); r.fault != 803 || r.description != "Seek failed" {
			t.Errorf("SeekSecondAbsolute(%s) in a track of 60 s: %d %q, want 803", value, r.fault, r.description)
		}
	}
	if r := soapFault(t, h, playlist, "SeekSecondRelative", "Value", "60"); r.fault != 803 {
		t.Errorf("SeekSecondRelative(60) from the start of a track of 60 s: fault %d, want 803", r.fault)
	}
	after := p.eng.Snapshot()
	if p.be.sessionCount() != 4 || after.Generation() != before.Generation() || after.Position() != before.Position() {
		t.Errorf("a seek at or past the end changed the engine: %d sessions, position %+v (was %+v)", p.be.sessionCount(), after.Position(), before.Position())
	}
	s4.hear(x, &minute)
	p.waitSnap("the session at the start heard", func(sn *engine.Snapshot) bool { return sn.State() == engine.Playing && !sn.Starting() })
	if tracks, _, seconds := timeNow(t, h); tracks != "1" || seconds != "0" {
		t.Errorf("after every seek: Time %s tracks at %s s; want the one track at 0", tracks, seconds)
	}
}

// TestASeekWhilePausedStaysPausedAtItsTarget pins a seek while paused through
// the SOAP layer: success, TransportState Paused and Time's Seconds the
// target at once, no session started until Play, which starts one at the
// target that continues the occurrence heard.
func TestASeekWhilePausedStaysPausedAtItsTarget(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	p.add(uriOf(goldenID), nil)
	read(t, h, playlist, "Play")
	s := p.be.session(t, 1)
	s.hear(s.first.ID, &minute)
	p.heardNow(s.first.ID)
	p.measured(s, s.first.ID, 10*44100)
	read(t, h, playlist, "Pause")
	read(t, h, playlist, "SeekSecondAbsolute", "Value", "20")
	if got := state(t, h); got != "Paused" {
		t.Errorf("TransportState %s after a seek while paused, want Paused", got)
	}
	if tracks, _, seconds := timeNow(t, h); tracks != "1" || seconds != "20" {
		t.Errorf("paused at the target: Time %s tracks at %s s, want 1 at 20", tracks, seconds)
	}
	if n := p.be.sessionCount(); n != 1 {
		t.Errorf("%d sessions after a seek while paused, want the first alone", n)
	}
	read(t, h, playlist, "Play")
	if s2 := p.be.session(t, 2); s2.first.ID != s.first.ID || s2.from != 20*44100 {
		t.Errorf("Play at the target started %d from %d, want %d from %d", s2.first.ID, s2.from, s.first.ID, 20*44100)
	}
}

// TestASeekIsATransportWrite pins the seeks as the transport writes they are
// (B674), within the same budget and admission: one parked behind a Pause
// under way answers success once its budget (1 s) ends, the engine having
// admitted it; a Stop then overtakes it, and the reads show it never ran: no
// session at its target, the position where it was, Stopped.
func TestASeekIsATransportWrite(t *testing.T) {
	p := newTestPlayer(t)
	spy := newSpy(p.eng)
	h := p.handlerWith("", spy, Limits{Write: 2 * time.Second}.orDefaults())
	p.add(uriOf(goldenID), nil)
	read(t, h, playlist, "Play")
	s := p.be.session(t, 1)
	s.hear(s.first.ID, &minute)
	p.heardNow(s.first.ID)
	p.measured(s, s.first.ID, 10*44100)
	p.be.holdPauses(t)
	pause := send(h, playlist, "Pause")
	p.be.callsBegun(t, "Pause", 1)
	seeks := spy.watch("SeekAbsolute")
	seek := send(h, playlist, "SeekSecondAbsolute", "Value", "30")
	admitted(t, seeks)
	if r := seek.answer(t); r.status != http.StatusOK {
		t.Fatalf("a seek parked past its budget: %d, fault %d, want success", r.status, r.fault)
	}
	if got := spy.answered("SeekAbsolute"); len(got) != 1 || engine.ReasonOf(got[0]) != engine.ReasonCancelled || errors.Is(got[0], engine.ErrNotAdmitted) {
		t.Fatalf("the engine answered %v, want a cancellation of a command it admitted", got)
	}
	read(t, h, playlist, "Stop")
	if n := p.be.sessionCount(); n != 1 {
		t.Errorf("%d sessions started, want the first alone: the overtaken seek ran", n)
	}
	if got := state(t, h); got != "Stopped" {
		t.Errorf("TransportState %s, want Stopped", got)
	}
	if pos := p.eng.Snapshot().Position(); pos.Frames != 10*44100 {
		t.Errorf("the position after the overtaken seek: %+v, want where it was, 10 s", pos)
	}
	pause.answer(t)
}
