package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// The console's control of the player (B634), PR 2's binding Lows of PR 1's
// round 2 (L1 to L5) and the clock GET now says (M1), over the real engine on
// the stand-in backend (ctlRig), every race parked at a seam.

// waitsBehind closes its channel once an operation of kind reaches its wait
// for an older operation's end, on that operation's own goroutine
// (controlSeams.awaitPrev), keeping a hook the case set before it. Set it
// before the operation is sent: the seam is read on the operation's goroutine.
func waitsBehind(c *consoleControl, kind string) <-chan struct{} {
	ch := make(chan struct{})
	var once sync.Once
	prev := c.seams.awaitPrev
	c.seams.awaitPrev = func(k, p string) {
		if prev != nil {
			prev(k, p)
		}
		if k == kind {
			once.Do(func() { close(ch) })
		}
	}
	return ch
}

// waitsForClosedFacts closes its channel once a switch-out reaches its wait
// for a closed engine's facts (controlSeams.awaitClosed), on its own
// goroutine. Set it before the switch-out is sent.
func waitsForClosedFacts(c *consoleControl) <-chan struct{} {
	ch := make(chan struct{})
	var once sync.Once
	c.seams.awaitClosed = func() { once.Do(func() { close(ch) }) }
	return ch
}

// reached waits for ch until the test's deadline.
func reached(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-serveGiveUp(t):
		t.Fatalf("%s was never reached", what)
	}
}

// readBackEnded reads the rig's tab's operation seq back until its record is
// no longer running, until the test's deadline, or fails with what premise
// carries first: a hook's failure, handed to the test's goroutine, asked
// before each read, so a record that ended beside it is never taken for the
// case's outcome.
func readBackEnded(t *testing.T, r *ctlRig, seq uint64, premise <-chan string) admin.PlayerRecord {
	t.Helper()
	for {
		select {
		case msg := <-premise:
			t.Fatal(msg)
		default:
		}
		wake := r.c.Wake()
		if rb := r.c.Operation(r.c.epoch, r.tab, seq); rb.Record != nil && rb.Record.Status != admin.PlayerOpRunning {
			return *rb.Record
		}
		select {
		case <-wake:
		case msg := <-premise:
			t.Fatal(msg)
		case <-serveGiveUp(t):
			t.Fatal("the operation never ended")
		}
	}
}

// failingLookupStore is the library index whose read of one track's whole row
// runs hook first and then fails (fail) or finds no row; every read counted.
type failingLookupStore struct {
	controlStore
	path string
	fail bool
	hook func()
	n    *atomic.Int32
}

func (s failingLookupStore) GetTrack(ctx context.Context, path string) (*manifest.Track, error) {
	s.n.Add(1)
	if path != s.path {
		return s.controlStore.GetTrack(ctx, path)
	}
	s.hook()
	if s.fail {
		return nil, errors.New("the index could not be read")
	}
	return nil, nil
}

// cancelHook is a hook that ends ctx's request and waits until its handler
// has returned (returned), which ends the operation's unsent steps first; a
// request that does not return by premiseGiveUp hands the case's premise to
// the test's goroutine (premise) and lets the hook go.
func cancelHook(t *testing.T, cancel context.CancelFunc, returned <-chan struct{}, premise chan<- string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			select {
			case <-returned:
			case <-premiseGiveUp(t):
				premise <- "the request whose end the hook made never returned: the case's premise, its operation's unsent steps ended, does not hold"
			}
		})
	}
}

// premiseGiveUp fires a second before serveGiveUp, so a hook that gives up
// on its premise is heard before a wait on the test's goroutine gives up on
// what that premise was for; never where the test binary has no deadline.
func premiseGiveUp(t *testing.T) <-chan time.Time {
	at := serveGiveUpTime(t)
	if at.IsZero() {
		return nil
	}
	return firesAt(at.Add(-time.Second))
}

// TestACancelInsideTheLastLookupIsCancelled pins L1 (M4, P5): an operation
// whose request ends inside the lookup of its only track's row, a lookup that
// then answers an error or no row, is cancelled, nothing sent and no track
// left out: the preparation asks once more, after its last distinct track,
// whether its operation's unsent steps were ended, before any result is read.
// Without that check it read the lookup's answer and refused not_playable,
// the track left out.
func TestACancelInsideTheLastLookupIsCancelled(t *testing.T) {
	for _, tc := range []struct {
		name         string
		fail, append bool
	}{
		{name: "a replace whose lookup fails", fail: true},
		{name: "a replace whose row is gone"},
		{name: "an append whose lookup fails", fail: true, append: true},
		{name: "an append whose row is gone", append: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := defaultCtlTracks[0].path
			var n atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			returned := make(chan struct{})
			premise := make(chan string, 1)
			hook := cancelHook(t, cancel, returned, premise)
			r := newCtlRig(t, ctlOptions{store: func(cs controlStore) controlStore {
				return failingLookupStore{controlStore: cs, path: path, fail: tc.fail, hook: hook, n: &n}
			}})
			op := r.op()
			go func() {
				if tc.append {
					r.c.Append(ctx, admin.PlayerAppend{Op: op, Digest: r.digest(), At: r.gen(), Tracks: []string{path}})
				} else {
					r.c.Replace(ctx, admin.PlayerReplace{Op: op, Digest: r.digest(), At: r.gen(), Tracks: []string{path}, Play: true})
				}
				close(returned)
			}()
			rec := readBackEnded(t, r, op.Seq, premise)
			if rec.Status != admin.PlayerOpCancelled || rec.Error != "" || rec.Sent != 0 || len(rec.LeftOut) != 0 || n.Load() != 1 {
				t.Fatalf("an operation cancelled inside its last lookup: %s %q, sent %d, left out %v, %d lookups; want cancelled, nothing sent or left out",
					rec.Status, rec.Error, rec.Sent, rec.LeftOut, n.Load())
			}
			if r.eng.Snapshot().Len() != 0 {
				t.Fatalf("the queue holds %d entries, want none", r.eng.Snapshot().Len())
			}
		})
	}
}

// TestACancelInsideTheMembershipCheckLooksUpNothing pins P5's first check: an
// operation whose request ends while the library's membership of its track
// is asked (library.Member runs on a context the request's end does not end,
// so it answers as if nothing happened) reads no row after it: cancelled, no
// lookup, nothing sent. Without the check between the membership and the
// entry's build, the entry's build read the track's row.
func TestACancelInsideTheMembershipCheckLooksUpNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		append bool
	}{{name: "a replace"}, {name: "an append", append: true}} {
		t.Run(tc.name, func(t *testing.T) {
			path := defaultCtlTracks[0].path
			var n atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			returned := make(chan struct{})
			premise := make(chan string, 1)
			hook := cancelHook(t, cancel, returned, premise)
			r := newCtlRig(t, ctlOptions{seams: &controlSeams{}, store: func(cs controlStore) controlStore {
				return failingLookupStore{controlStore: cs, path: "never", hook: func() {}, n: &n}
			}})
			r.c.seams.afterMember = func(p string) {
				if p == path {
					hook()
				}
			}
			op := r.op()
			go func() {
				if tc.append {
					r.c.Append(ctx, admin.PlayerAppend{Op: op, Digest: r.digest(), At: r.gen(), Tracks: []string{path}})
				} else {
					r.c.Replace(ctx, admin.PlayerReplace{Op: op, Digest: r.digest(), At: r.gen(), Tracks: []string{path}, Play: true})
				}
				close(returned)
			}()
			rec := readBackEnded(t, r, op.Seq, premise)
			if rec.Status != admin.PlayerOpCancelled || rec.Sent != 0 || len(rec.LeftOut) != 0 || n.Load() != 0 {
				t.Fatalf("an operation cancelled inside its membership check: %s, sent %d, left out %v, %d lookups; want cancelled, no lookup",
					rec.Status, rec.Sent, rec.LeftOut, n.Load())
			}
		})
	}
}

// TestASettledAnswerCarriesThePublicationThatSettledIt pins L2 (M5): a
// switch-out that ends done, its record's silence above 0, is answered with
// a publication of the control's epoch, with bytes, whose revision is at or
// above that silence, in its settled answer, in a retry of it at its tab's
// mark and in the record read back (settleOn sets the operation's
// publication after the publisher's answer, on every path); and a switch-out
// the publisher ended under, after a publication existed, is answered with
// that publication, its record unknown with no silence and no handover.
func TestASettledAnswerCarriesThePublicationThatSettledIt(t *testing.T) {
	t.Run("a switch-out done", func(t *testing.T) {
		r := newCtlRig(t, ctlOptions{})
		wantRecord(t, "a replace that plays", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
		sw := admin.PlayerSwitchOut{Op: r.op(), Digest: r.digest(), Take: true}
		a := r.c.SwitchOut(context.Background(), sw)
		rec := wantRecord(t, "the switch-out", a, admin.PlayerOpDone, "", "")
		carries := func(what string, epoch string, rev uint64, bytes int, silence uint64) {
			t.Helper()
			if silence == 0 || rev < silence || epoch != r.c.epoch || bytes == 0 {
				t.Fatalf("%s: revision %d of epoch %q, %d bytes, silence %d; want the control's epoch, bytes and a revision at or above a silence above 0",
					what, rev, epoch, bytes, silence)
			}
		}
		carries("the settled answer", a.Player.Epoch, a.Player.Revision, len(a.Player.Bytes), rec.Silence)
		again := r.c.SwitchOut(context.Background(), sw)
		if again.Record == nil || again.Record.Status != admin.PlayerOpDone {
			t.Fatalf("a retry at the tab's mark: %+v, want the done record", again.Record)
		}
		carries("a retry", again.Player.Epoch, again.Player.Revision, len(again.Player.Bytes), again.Record.Silence)
		rb := r.c.Operation(r.c.epoch, r.tab, sw.Op.Seq)
		if rb.Record == nil || rb.Record.Status != admin.PlayerOpDone {
			t.Fatalf("the read back: %+v, want the done record", rb.Record)
		}
		carries("the read back", rb.Epoch, rb.Revision, 1, rb.Record.Silence)
	})
	t.Run("the publisher ended after a publication", func(t *testing.T) {
		r := newCtlRig(t, ctlOptions{})
		wantRecord(t, "a replace that plays", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
		last, ok := r.c.Latest()
		if !ok || last.Revision == 0 {
			t.Fatal("no publication before the publisher's end")
		}
		if !r.c.pub.stop(controlStopBound) {
			t.Fatal("the publisher did not stop")
		}
		a := r.switchOut(true)
		rec := wantRecord(t, "a switch-out the publisher ended under", a, admin.PlayerOpUnknown, "", "")
		if rec.Silence != 0 || rec.Handover != nil {
			t.Fatalf("the record: silence %d, handover %+v; want neither", rec.Silence, rec.Handover)
		}
		if a.Player.Revision != last.Revision || a.Player.Epoch != r.c.epoch || len(a.Player.Bytes) == 0 {
			t.Fatalf("the answer's publication: revision %d of %q, %d bytes; want the last one made, %d", a.Player.Revision, a.Player.Epoch,
				len(a.Player.Bytes), last.Revision)
		}
	})
}

// TestTheWaitForANewerOperationIsHeardWhereItWaits pins L3 (M6): a case that
// holds itself until a newer operation waits for an older one hears that
// wait on the newer operation's own goroutine (waitsBehind), never its
// admission, which fills the control's records before that goroutine starts:
// a newer operation held at its admission has not been heard, and once let go
// it is heard, its wait the one the case means.
func TestTheWaitForANewerOperationIsHeardWhereItWaits(t *testing.T) {
	r := newCtlRig(t, ctlOptions{seams: &controlSeams{}})
	at, release := make(chan struct{}), make(chan struct{})
	admittedAt, admittedGo := make(chan struct{}), make(chan struct{})
	var once, letGo, letAdmitted sync.Once
	free := func() { letGo.Do(func() { close(release) }) }
	freeAdmitted := func() { letAdmitted.Do(func() { close(admittedGo) }) }
	t.Cleanup(free)
	t.Cleanup(freeAdmitted)
	r.c.seams.afterStep = func(kind, step string, err error) {
		if kind == opKindReplace && step == "insert" {
			once.Do(func() { close(at); <-release })
		}
	}
	r.c.seams.admitted = func(kind string) {
		if kind == opKindTransport {
			close(admittedAt)
			<-admittedGo
		}
	}
	behind := waitsBehind(r.c, opKindTransport)
	replaced := make(chan admin.PlayerAnswer, 1)
	go func() { replaced <- r.replace(r.paths(), 0, 0, false) }()
	reached(t, at, "the replace's first insert")
	newer := make(chan admin.PlayerAnswer, 1)
	go func() { newer <- r.command(admin.PlayerCmdRepeat, func(c *admin.PlayerCommand) { c.On = true }) }()
	reached(t, admittedAt, "the newer operation's admission")
	r.c.mu.Lock()
	last := r.c.recent[len(r.c.recent)-1].rec.Kind
	r.c.mu.Unlock()
	if last != opKindTransport {
		t.Fatalf("the newer operation admitted, the last record is a %s", last)
	}
	select {
	case <-behind:
		t.Fatal("the wait was heard while the newer operation's goroutine had not started: a case would let the older one go before the wait it tests")
	default:
	}
	freeAdmitted()
	reached(t, behind, "the newer operation's wait for the older one")
	free()
	if a := <-replaced; a.Record == nil || a.Record.Status != admin.PlayerOpCancelled {
		t.Fatalf("the older replace: %+v, want cancelled", a.Record)
	}
	<-newer
}

// TestTheControlsStopEndsAnOperationWaitingBehindAnOlderOne pins L5's first
// path (M7, P3, P4): the control's real stop, the engine still running, while
// a newer operation waits for an older one's unfinished steps: the newer one
// ends unknown with nothing sent, its effect none, its body never run, and
// nothing starts; the older one let go only once the newer one has ended, so
// the stop joins both within its bound. Before the fix the quit fell through
// to the newer one's body, which played.
func TestTheControlsStopEndsAnOperationWaitingBehindAnOlderOne(t *testing.T) {
	r := newCtlRig(t, ctlOptions{seams: &controlSeams{}})
	at, release := make(chan struct{}), make(chan struct{})
	var once, letGo sync.Once
	free := func() { letGo.Do(func() { close(release) }) }
	t.Cleanup(free)
	var newerSent atomic.Int32
	r.c.seams.afterStep = func(kind, step string, err error) {
		if kind == opKindReplace && step == "insert" {
			once.Do(func() { close(at); <-release })
		}
	}
	r.c.seams.beforeSend = func(kind, step string) {
		if kind == opKindTransport {
			newerSent.Add(1)
		}
	}
	behind := waitsBehind(r.c, opKindTransport)
	older := make(chan admin.PlayerAnswer, 1)
	go func() { older <- r.replace(r.paths(), 0, 0, false) }()
	reached(t, at, "the older replace's first insert")
	newer := make(chan admin.PlayerAnswer, 1)
	go func() { newer <- r.command(admin.PlayerCmdPlay, nil) }()
	reached(t, behind, "the newer play's wait for the older replace")
	stopped := make(chan struct{})
	go func() { r.c.stop(); close(stopped) }()
	var a admin.PlayerAnswer
	select {
	case a = <-newer:
	case <-serveGiveUp(t):
		t.Fatal("the newer play never ended once the control stopped")
	}
	free()
	select {
	case <-stopped:
	case <-serveGiveUp(t):
		t.Fatal("the control's stop never returned")
	}
	<-older
	rec := a.Record
	if rec == nil || rec.Status != admin.PlayerOpUnknown || rec.Sent != 0 || rec.Effect != admin.PlayerEffectNone || newerSent.Load() != 0 {
		t.Fatalf("the play the stop ended behind the replace: %+v, %d steps sent; want unknown, nothing sent, effect none", rec, newerSent.Load())
	}
	if _, _, starts := r.standIn.counts(); starts != 0 {
		t.Fatalf("%d sessions started after the control's stop", starts)
	}
	if s := r.eng.Snapshot().State(); s != engine.Stopped {
		t.Fatalf("the engine is %s after the control's stop, want stopped", s)
	}
}

// TestAPublicationCarriesTheLastRecordsWithoutTheirHandovers pins L5's second
// path (recentRecords): a publication carries the last eight records,
// whatever their tab, oldest first, and none with its handover or the tracks
// it left out, which a tab reads back from its own record alone.
func TestAPublicationCarriesTheLastRecordsWithoutTheirHandovers(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	tabs := []string{}
	for i := range 10 {
		r.tab = fmt.Sprintf("%032x", i+1)
		r.seq = 0
		tabs = append(tabs, r.tab)
		switch i {
		case 6:
			// A track the library does not hold, left out beside one it does.
			a := r.replace([]string{"Nowhere/00 X.flac", r.paths()[0]}, 1, 0, false)
			if rec := wantRecord(t, "a replace with a track left out", a, admin.PlayerOpDone, "", ""); len(rec.LeftOut) != 1 {
				t.Fatalf("the replace left out %v, want the one track", rec.LeftOut)
			}
		case 7:
			if rec := wantRecord(t, "a switch-out that takes", r.switchOut(true), admin.PlayerOpDone, "", ""); rec.Handover == nil {
				t.Fatal("the switch-out's record holds no handover")
			}
		default:
			wantRecord(t, "a pause", r.command(admin.PlayerCmdPause, nil), admin.PlayerOpDone, "", "")
		}
	}
	a := r.command(admin.PlayerCmdStop, nil)
	tabs = append(tabs, r.tab)
	st := stateOf(t, a.Player)
	if len(st.Operations) != recentRecords {
		t.Fatalf("the publication carries %d records, want the last %d", len(st.Operations), recentRecords)
	}
	got := []string{}
	for _, rec := range st.Operations {
		got = append(got, rec.Tab+"/"+rec.Kind)
		if rec.Handover != nil || rec.LeftOut != nil {
			t.Fatalf("a published record carries its handover %+v or what it left out %v", rec.Handover, rec.LeftOut)
		}
	}
	want := []string{}
	for i, tab := range tabs[len(tabs)-recentRecords:] {
		kind := opKindTransport
		switch len(tabs) - recentRecords + i {
		case 6:
			kind = opKindReplace
		case 7:
			kind = opKindSwitchOut
		}
		want = append(want, tab+"/"+kind)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the published records %v, want the last eight, oldest first: %v", got, want)
	}
	if rb := r.c.Operation(r.c.epoch, tabs[7], 1); rb.Record == nil || rb.Record.Handover == nil {
		t.Fatalf("the switch-out's tab reads back %+v, want its handover", rb.Record)
	}
	if rb := r.c.Operation(r.c.epoch, tabs[6], 1); rb.Record == nil || len(rb.Record.LeftOut) != 1 {
		t.Fatalf("the replace's tab reads back %+v, want what it left out", rb.Record)
	}
}

// TestTheControlsClockIsTheClockAtSend pins M1 (P1) in the control: its clock,
// which GET now's X-Bridge-Clock reads as the answer is sent, is the clock
// that stamps every capture's at, injected here as the backend's measurement
// is; with nothing changed since a publication, GET now answers it, older
// than the clock at send.
func TestTheControlsClockIsTheClockAtSend(t *testing.T) {
	var mu sync.Mutex
	clock := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	advance := func(d time.Duration) {
		mu.Lock()
		clock = clock.Add(d)
		mu.Unlock()
	}
	r := newCtlRig(t, ctlOptions{now: now})
	wantRecord(t, "a replace that plays", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
	r.position(44100*5, now())
	captured := stateOf(t, r.c.Now(context.Background()))
	if captured.At != now().Format(time.RFC3339Nano) {
		t.Fatalf("a capture's at %q, want the control's clock %q", captured.At, now().Format(time.RFC3339Nano))
	}
	if captured.Anchor == nil || captured.Anchor.At != captured.At {
		t.Fatalf("the anchor %+v, want it measured on the control's clock at %s", captured.Anchor, captured.At)
	}
	advance(7 * time.Second)
	pub := r.c.Now(context.Background())
	if st := stateOf(t, pub); st.Revision != captured.Revision || st.At != captured.At {
		t.Fatalf("GET now with nothing changed answered revision %d at %s, want %d at %s", st.Revision, st.At, captured.Revision, captured.At)
	}
	if got := r.c.Clock(); !got.Equal(now()) {
		t.Fatalf("the control's clock at send %v, want %v, 7 s after the publication it answers", got, now())
	}
}
