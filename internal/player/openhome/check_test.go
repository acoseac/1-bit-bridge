package openhome

import (
	"net/http"
	"slices"
	"testing"
	"time"
)

// The player's DSD check through the surface (B728 B12 PR 2): while the check
// holds the engine (engine.EnterCheck), the surface's Stop is the engine's,
// which ends the check and answers once the check has let go, and every other
// write that reaches the engine is 501, as in maintenance; the surface asks
// nothing of the check, and starts nothing when it ends. The engine runs on the
// real clock, so a Stop the check holds is answered at the latest
// engine.ReleaseTimeout (2 s) after it parks: the test makes its checks inside
// that window, as the Stops' tests do.

// TestAnOpenHomeStopEndsTheCheckThroughTheEngine pins the Stop's one
// interception, through the SOAP layer: with an entry playing, the check's
// hold interrupts it, and TransportState reads Stopped (maintenance). A Stop
// then gives its request's slot back once the engine has admitted it, the
// hold's stop signal closes, and the Stop waits; once the check lets go (its
// exit) the Stop answers success, the transport Stopped and no session
// started.
func TestAnOpenHomeStopEndsTheCheckThroughTheEngine(t *testing.T) {
	p := newTestPlayer(t)
	waits := newWaitingStops()
	h := p.handler("")
	h.seams = &Seams{StopWaiting: waits.seam}
	p.add(uriOf(goldenID), nil)
	must(t, "play", p.eng.Play(bg))
	s := p.be.session(t, 1)
	s.hear(s.first.ID, nil)
	p.heardNow(s.first.ID)
	hold, err := p.eng.EnterCheck(bg, true)
	must(t, "the check's hold, interrupting what plays", err)
	if got := read(t, h, playlist, "TransportState")["Value"]; got != "Stopped" {
		t.Errorf("TransportState %s while the check holds the engine, want Stopped", got)
	}
	stop := send(h, playlist, "Stop")
	waits.wait(t, 1)
	select {
	case <-hold.Stop:
	case <-time.After(testWait):
		t.Fatalf("waited %v for the check's stop signal at an OpenHome Stop", testWait)
	}
	if !stop.pending() {
		t.Fatal("the Stop answered before the check let go")
	}
	must(t, "the check lets go", p.eng.ExitMaintenance(bg, hold.Lease))
	if r := stop.answer(t); r.status != http.StatusOK {
		t.Errorf("the Stop once the check let go: %d, fault %d, want success", r.status, r.fault)
	}
	if got := read(t, h, playlist, "TransportState")["Value"]; got != "Stopped" {
		t.Errorf("TransportState %s once the check let go, want Stopped", got)
	}
	if n := p.be.sessionCount(); n != 1 {
		t.Errorf("%d sessions started, want the one the check interrupted: the check's end started one", n)
	}
}

// TestOpenHomesOtherWritesAre501DuringACheck pins the other writes during the
// check: with two entries and the check holding the engine, every write that
// reaches the engine, otherwise valid (Play, Pause, Next, Previous, SeekId,
// SeekIndex, the two seeks within a track, SetRepeat, SetShuffle(0), DeleteId,
// DeleteAll and an Insert of a track of the bridge's), is 501, Action Failed
// (the engine's maintenance), the queue, Id, TransportState and Repeat as they
// were and no session started; the writes that answer without the engine
// answer as they do outside a check (SetStandby, SetSourceIndex,
// SetSourceIndexByName, SetShuffle(1), an Insert its confinement refuses); and
// once the check lets go, Play succeeds and starts.
func TestOpenHomesOtherWritesAre501DuringACheck(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	a := p.add(uriOf(goldenID), nil)
	b := p.add(uriOf(trackB), nil)
	hold, err := p.eng.EnterCheck(bg, false)
	must(t, "the check's hold", err)
	for _, w := range [][]string{
		{"Play"}, {"Pause"}, {"Next"}, {"Previous"},
		{"SeekId", "Value", u(b)}, {"SeekIndex", "Value", "1"},
		{"SeekSecondAbsolute", "Value", "30"}, {"SeekSecondRelative", "Value", "5"},
		{"SetRepeat", "Value", "1"}, {"SetShuffle", "Value", "0"},
		{"DeleteId", "Value", u(a)}, {"DeleteAll"},
		{"Insert", "AfterId", u(b), "Uri", uriOf(trackC), "Metadata", ""},
	} {
		if r := soapFault(t, h, playlist, w[0], w[1:]...); r.fault != errActionFailed {
			t.Errorf("%s during the check: fault %d, want 501", w[0], r.fault)
		}
	}
	read(t, h, product, "SetStandby", "Value", "0")
	if r := soapFault(t, h, product, "SetStandby", "Value", "1"); r.fault != faultNotImplemented.code {
		t.Errorf("SetStandby(1) during the check: fault %d, want %d", r.fault, faultNotImplemented.code)
	}
	read(t, h, product, "SetSourceIndex", "Value", "0")
	if r := soapFault(t, h, product, "SetSourceIndex", "Value", "1"); r.fault != faultSourceNotFound.code {
		t.Errorf("SetSourceIndex(1) during the check: fault %d, want %d", r.fault, faultSourceNotFound.code)
	}
	read(t, h, product, "SetSourceIndexByName", "Value", sourceName)
	if r := soapFault(t, h, playlist, "SetShuffle", "Value", "1"); r.fault != faultShuffle.code {
		t.Errorf("SetShuffle(1) during the check: fault %d, want %d", r.fault, faultShuffle.code)
	}
	if r := soapFault(t, h, playlist, "Insert", "AfterId", u(b), "Uri", "http://127.0.0.1:7790/dlna/file/"+trackC, "Metadata", ""); r.fault != errArgumentValueInvalid {
		t.Errorf("an Insert its confinement refuses, during the check: fault %d, want %d", r.fault, errArgumentValueInvalid)
	}
	if got := queueOf(t, h); !slices.Equal(got, []uint32{a, b}) {
		t.Errorf("the queue %v after the refused writes, want a, b", got)
	}
	if got := read(t, h, playlist, "Id")["Value"]; got != u(a) {
		t.Errorf("Id %s after the refused writes, want the first, %d", got, a)
	}
	if got := read(t, h, playlist, "TransportState")["Value"]; got != "Stopped" {
		t.Errorf("TransportState %s during the check, want Stopped", got)
	}
	if got := read(t, h, playlist, "Repeat")["Value"]; got != "0" {
		t.Errorf("Repeat %s after the refused SetRepeat, want 0", got)
	}
	if n := p.be.sessionCount(); n != 0 {
		t.Errorf("the refused writes started %d sessions", n)
	}
	must(t, "the check lets go", p.eng.ExitMaintenance(bg, hold.Lease))
	read(t, h, playlist, "Play")
	p.startUnderWay()
	if s := p.be.session(t, 1); s.first.Entry != a {
		t.Errorf("Play once the check let go started %d, want the first, %d", s.first.Entry, a)
	}
}
