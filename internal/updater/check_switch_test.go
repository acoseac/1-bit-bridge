package updater

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The update check's switch (backlog B761): while update.checkEnabled is off
// the updater asks GitHub nothing, at its start, at a tick of its poll loop
// or for "Check now"; the off transition stops every check under way through
// its own context; turned on, the loop's wait starts again at a full interval
// and asks nothing; and the status shows nothing a check found before. Every
// request is counted by a GitHub of the test's own (Options.Client), and the
// poll loop's ticks come from the test (Updater.after), so a tick is reached
// with no real hour.

// testWait bounds each wait for an event in these tests: generous, since a
// loaded race run is slow, and reached only by a test that has failed.
const testWait = time.Minute

// testSwitch is an update check's switch a test moves, on until it is turned
// off.
type testSwitch struct{ off atomic.Bool }

func (s *testSwitch) on() bool { return !s.off.Load() }

// admitted is an admission of u's, as an install's start makes one, for a
// test that calls the decision under the install marker's lock itself
// (decideUnderLock), which commits it; ended when the test ends.
func admitted(t *testing.T, u *Updater) *admission {
	t.Helper()
	_, a, ok := u.admit(context.Background())
	if !ok {
		t.Fatal("no admission while update checks are off")
	}
	t.Cleanup(func() { u.done(a) })
	return a
}

// checkNow is u.CheckNow on a background context, failing the test on an
// error.
func checkNow(t *testing.T, u *Updater) Status {
	t.Helper()
	st, err := u.CheckNow(context.Background())
	if err != nil {
		t.Fatalf("CheckNow: %v", err)
	}
	return st
}

// releaseStandIn is a GitHub releases API of the test's own whose latest
// release is its tag, counting every request it is asked. While holding is
// set it holds each request for the latest release, handing the test a
// heldRequest, until the test releases it or the request's client gives up.
type releaseStandIn struct {
	srv      *httptest.Server
	requests atomic.Int32
	holding  atomic.Bool
	held     chan heldRequest

	mu       sync.Mutex
	releases []chan struct{}
}

// heldRequest is one request the stand-in holds.
type heldRequest struct {
	// ended is closed once the request's client has given up on it.
	ended <-chan struct{}
	// release answers it.
	release chan struct{}
}

func newReleaseStandIn(t *testing.T, tag string) *releaseStandIn {
	t.Helper()
	s := &releaseStandIn{held: make(chan heldRequest, 8)}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		if r.URL.Path != "/repos/fake/repo/releases/latest" {
			http.NotFound(w, r)
			return
		}
		if s.holding.Load() {
			ended, release := make(chan struct{}), make(chan struct{})
			s.mu.Lock()
			s.releases = append(s.releases, release)
			s.mu.Unlock()
			s.held <- heldRequest{ended: ended, release: release}
			select {
			case <-r.Context().Done():
				close(ended)
				// The client gave up: answer nothing.
				panic(http.ErrAbortHandler)
			case <-release:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tag_name":%q,"html_url":%q,"assets":[]}`, tag, "https://example.invalid/releases/"+tag)
	}))
	t.Cleanup(s.srv.Close)
	// Before the server's Close, which waits for every request: a request
	// a failed test still holds is answered.
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, r := range s.releases {
			select {
			case <-r:
			default:
				close(r)
			}
		}
	})
	return s
}

// client is an updater Client that asks s, on a transport of its own. Its
// timeout outlasts every wait here, so that a request s holds ends within
// one only where its check is stopped.
func (s *releaseStandIn) client(t *testing.T) *Client {
	t.Helper()
	c := NewClient("fake/repo", 2*testWait).WithBaseURL(s.srv.URL)
	tr := &http.Transport{}
	c.http.Transport = tr
	c.download.Transport = tr
	t.Cleanup(tr.CloseIdleConnections)
	return c
}

// waitHeld waits for the next request the stand-in holds.
func (s *releaseStandIn) waitHeld(t *testing.T) heldRequest {
	t.Helper()
	select {
	case h := <-s.held:
		return h
	case <-time.After(testWait):
		t.Fatal("no request reached GitHub")
		return heldRequest{}
	}
}

// waitEnded waits for the client to give the held request up.
func (h heldRequest) waitEnded(t *testing.T) {
	t.Helper()
	select {
	case <-h.ended:
	case <-time.After(testWait):
		t.Fatal("the request held was never stopped: it stayed on the wire")
	}
}

// tickSeam stands in for the poll loop's timer (Updater.after): every wait
// the loop arms has a channel of its own, buffered 1, so that a test fires
// one wait and not another, and the test is handed each wait as the loop
// arms it. holdNext holds the loop at its next arming, its schedule taken
// and its select not yet reached, so that a test can make a tick and a
// notification ready together.
type tickSeam struct {
	asked chan armedWait
	hold  atomic.Pointer[chan struct{}]

	mu   sync.Mutex
	last armedWait
}

// armedWait is one wait the loop armed: its length and its own channel.
type armedWait struct {
	d    time.Duration
	tick chan time.Time
}

// fire hands w its tick, whether or not the loop still waits on it: a wait
// the loop has left keeps the tick, and nobody reads it.
func (w armedWait) fire() {
	select {
	case w.tick <- time.Now():
	default:
	}
}

func installTickSeam(u *Updater) *tickSeam {
	s := &tickSeam{asked: make(chan armedWait, 64)}
	u.after = func(d time.Duration) <-chan time.Time {
		w := armedWait{d: d, tick: make(chan time.Time, 1)}
		s.asked <- w
		if h := s.hold.Swap(nil); h != nil {
			<-*h
		}
		return w.tick
	}
	return s
}

// holdNext holds the loop at its next arming, inside the seam's after, until
// release (or the test's end). Called once the loop runs, so that the release
// at the test's end comes before runLoop's stop.
func (s *tickSeam) holdNext(t *testing.T) (release func()) {
	ch := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	s.hold.Store(&ch)
	return release
}

// waitArmed waits for the loop to arm its next wait, which it does once the
// turn before it is done, and returns it; fire then fires it.
func (s *tickSeam) waitArmed(t *testing.T) armedWait {
	t.Helper()
	select {
	case w := <-s.asked:
		s.mu.Lock()
		s.last = w
		s.mu.Unlock()
		return w
	case <-time.After(testWait):
		t.Fatal("the poll loop never asked for its next wait")
		return armedWait{}
	}
}

// waitAsked is waitArmed's wait's length.
func (s *tickSeam) waitAsked(t *testing.T) time.Duration {
	t.Helper()
	return s.waitArmed(t).d
}

// fire hands the last wait the loop armed its tick.
func (s *tickSeam) fire(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	w := s.last
	s.mu.Unlock()
	if w.tick == nil {
		t.Fatal("the poll loop has armed no wait to fire")
	}
	w.fire()
}

// switchedUpdater is an Updater that asks gh, its update check's switch sw,
// its poll loop's timer the returned seam.
func switchedUpdater(t *testing.T, gh *releaseStandIn, sw *testSwitch, opts Options) (*Updater, *tickSeam) {
	t.Helper()
	opts.RepoOverride = "fake/repo"
	opts.Client = gh.client(t)
	opts.CheckEnabled = sw.on
	u := New(opts)
	return u, installTickSeam(u)
}

// runLoop runs u's poll loop until the test ends, and waits for it to
// return then.
func runLoop(t *testing.T, u *Updater) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		u.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testWait):
			t.Error("the poll loop did not return once its context ended")
		}
	})
}

// TestNoRequestAtTheStartOrATickWhileUpdateChecksAreOff: with update checks
// off the poll loop asks GitHub nothing at its start, nor at a tick of its
// timer, which the seam fires with no real hour, twice; its status says the
// checks are off and that none was made.
func TestNoRequestAtTheStartOrATickWhileUpdateChecksAreOff(t *testing.T) {
	gh := newReleaseStandIn(t, "v9.9.9")
	sw := &testSwitch{}
	sw.off.Store(true)
	u, ticks := switchedUpdater(t, gh, sw, Options{})
	runLoop(t, u)

	ticks.waitAsked(t) // the start is done: its first wait is asked for
	if n := gh.requests.Load(); n != 0 {
		t.Fatalf("the start asked GitHub %d time(s) with update checks off, want 0", n)
	}
	for i := 1; i <= 2; i++ {
		ticks.fire(t)
		ticks.waitAsked(t) // the tick is done: the next wait is asked for
		if n := gh.requests.Load(); n != 0 {
			t.Fatalf("tick %d: GitHub was asked %d time(s) with update checks off, want 0", i, n)
		}
	}
	if st := u.Status(); st.CheckEnabled || !st.LastCheck.IsZero() {
		t.Errorf("the status with update checks off = %+v, want CheckEnabled false and no check made", st)
	}
}

// TestARequestAtTheStartAndAtATickWhileUpdateChecksAreOn is the positive
// control: with update checks on the start asks once, and a tick once more,
// and the status shows what the check found.
func TestARequestAtTheStartAndAtATickWhileUpdateChecksAreOn(t *testing.T) {
	gh := newReleaseStandIn(t, "v9.9.9")
	u, ticks := switchedUpdater(t, gh, &testSwitch{}, Options{})
	runLoop(t, u)

	ticks.waitAsked(t)
	if n := gh.requests.Load(); n != 1 {
		t.Fatalf("the start asked GitHub %d time(s) with update checks on, want 1", n)
	}
	ticks.fire(t)
	ticks.waitAsked(t)
	if n := gh.requests.Load(); n != 2 {
		t.Fatalf("after a tick GitHub was asked %d time(s) with update checks on, want 2", n)
	}
	if st := u.Status(); !st.CheckEnabled || !st.UpdateAvailable || st.LatestVersion != "9.9.9" {
		t.Errorf("the status = %+v, want checks on and 9.9.9 available", st)
	}
}

// TestCheckNowAsksNothingWhileUpdateChecksAreOff: "Check now" is refused
// while update checks are off, ErrUpdateChecksOff and no request, and asks
// once when they are on again.
func TestCheckNowAsksNothingWhileUpdateChecksAreOff(t *testing.T) {
	gh := newReleaseStandIn(t, "v9.9.9")
	sw := &testSwitch{}
	sw.off.Store(true)
	u := New(Options{RepoOverride: "fake/repo", Client: gh.client(t), CheckEnabled: sw.on})

	st, err := u.CheckNow(context.Background())
	if !errors.Is(err, ErrUpdateChecksOff) {
		t.Errorf("Check now with update checks off = %v, want ErrUpdateChecksOff", err)
	}
	if n := gh.requests.Load(); n != 0 {
		t.Errorf("Check now with update checks off asked GitHub %d time(s), want 0", n)
	}
	if st.CheckEnabled || st.UpdateAvailable {
		t.Errorf("the status Check now answered = %+v, want checks off and no update", st)
	}

	sw.off.Store(false)
	if st := checkNow(t, u); !st.UpdateAvailable || gh.requests.Load() != 1 {
		t.Errorf("Check now with update checks on again: %+v after %d request(s), want 9.9.9 found by one", st, gh.requests.Load())
	}
}

// TestTurningUpdateChecksOffStopsTheScheduledCheckUnderWay: a check a tick
// began is on the wire, held there, when update checks are turned off, and
// the off transition (ChecksSwitched) stops it where it is: its request ends,
// the loop goes on to its next wait, and the stopped check is a stop, not a
// failure: nothing of it is recorded or logged. The next tick asks nothing.
func TestTurningUpdateChecksOffStopsTheScheduledCheckUnderWay(t *testing.T) {
	gh := newReleaseStandIn(t, "v9.9.9")
	sw := &testSwitch{}
	u, ticks := switchedUpdater(t, gh, sw, Options{})
	rec := loggingtest.Record(t)
	runLoop(t, u)
	ticks.waitAsked(t) // the start's check answered
	before := u.cached()

	gh.holding.Store(true)
	ticks.fire(t)
	held := gh.waitHeld(t)
	sw.off.Store(true)
	u.ChecksSwitched()
	held.waitEnded(t)
	ticks.waitAsked(t)
	if st := u.cached(); st.LastError != "" || st.LastCheck != before.LastCheck {
		t.Errorf("a check the switch stopped changed the status: %+v, was %+v", st, before)
	}
	if got := rec.Failures(msgPoll); len(got) != 0 {
		t.Errorf("a check the switch stopped was logged as one that failed:\n%s", strings.Join(got, "\n"))
	}

	ticks.fire(t)
	ticks.waitAsked(t)
	if n := gh.requests.Load(); n != 2 {
		t.Errorf("GitHub was asked %d time(s), want 2: the start's and the stopped one", n)
	}
}

// TestTurningUpdateChecksOffStopsACheckNowUnderWayBesideTheScheduledOne: a
// "Check now" asked while a scheduled check is on the wire overlaps it, and
// the off transition stops both; the "Check now" answers ErrUpdateChecksOff.
func TestTurningUpdateChecksOffStopsACheckNowUnderWayBesideTheScheduledOne(t *testing.T) {
	gh := newReleaseStandIn(t, "v9.9.9")
	sw := &testSwitch{}
	u, ticks := switchedUpdater(t, gh, sw, Options{})
	rec := loggingtest.Record(t)
	runLoop(t, u)
	ticks.waitAsked(t)

	gh.holding.Store(true)
	ticks.fire(t)
	scheduled := gh.waitHeld(t)
	answered := make(chan error, 1)
	go func() {
		_, err := u.CheckNow(context.Background())
		answered <- err
	}()
	manual := gh.waitHeld(t)

	sw.off.Store(true)
	u.ChecksSwitched()
	scheduled.waitEnded(t)
	manual.waitEnded(t)
	select {
	case err := <-answered:
		if !errors.Is(err, ErrUpdateChecksOff) {
			t.Errorf("the Check now the switch stopped answered %v, want ErrUpdateChecksOff", err)
		}
	case <-time.After(testWait):
		t.Fatal("the Check now the switch stopped never answered")
	}
	ticks.waitAsked(t)
	if got := rec.Failures(msgPoll); len(got) != 0 {
		t.Errorf("a check the switch stopped was logged as one that failed:\n%s", strings.Join(got, "\n"))
	}
}

// TestTurningUpdateChecksOnRearmsAFullWaitAndAsksNothing: turned on, the
// poll loop's wait starts again at a full interval, the cadence's, asking
// GitHub nothing for it; the tick at its end checks.
func TestTurningUpdateChecksOnRearmsAFullWaitAndAsksNothing(t *testing.T) {
	gh := newReleaseStandIn(t, "v9.9.9")
	sw := &testSwitch{}
	sw.off.Store(true)
	u, ticks := switchedUpdater(t, gh, sw, Options{LiveCheckInterval: func() time.Duration { return 3 * time.Hour }})
	runLoop(t, u)
	ticks.waitAsked(t)

	sw.off.Store(false)
	u.ChecksSwitched()
	if d := ticks.waitAsked(t); d != 3*time.Hour {
		t.Errorf("the wait the loop asked for once checks were on = %v, want a full interval, 3h", d)
	}
	if n := gh.requests.Load(); n != 0 {
		t.Errorf("turning update checks on asked GitHub %d time(s), want 0", n)
	}
	ticks.fire(t)
	ticks.waitAsked(t)
	if n := gh.requests.Load(); n != 1 {
		t.Errorf("the tick after checks were turned on asked GitHub %d time(s), want 1", n)
	}
}

// TestATickArmedWhileUpdateChecksWereOffAsksNothingOnceTheyAreOn: the
// settings save publishes the switch on before it tells the updater
// (ChecksSwitched), and a wait the loop armed while checks were off can end
// in between: its tick asks GitHub nothing, though the switch reads on by
// then, and the notification after it asks nothing either; the wait the loop
// arms for it is a full interval, and its tick checks.
func TestATickArmedWhileUpdateChecksWereOffAsksNothingOnceTheyAreOn(t *testing.T) {
	gh := newReleaseStandIn(t, "v9.9.9")
	sw := &testSwitch{}
	sw.off.Store(true)
	u, ticks := switchedUpdater(t, gh, sw, Options{LiveCheckInterval: func() time.Duration { return 3 * time.Hour }})
	runLoop(t, u)
	armedOff := ticks.waitArmed(t)

	sw.off.Store(false) // published on; the updater not told yet
	armedOff.fire()
	ticks.waitArmed(t) // the tick is done
	if n := gh.requests.Load(); n != 0 {
		t.Fatalf("the tick of a wait armed while update checks were off asked GitHub %d time(s) once they read on, "+
			"before the updater was told; want 0", n)
	}
	u.ChecksSwitched()
	rearmed := ticks.waitArmed(t)
	if rearmed.d != 3*time.Hour || gh.requests.Load() != 0 {
		t.Fatalf("the wait armed for checks turned on = %v after %d request(s), want a full interval, 3h, and none", rearmed.d, gh.requests.Load())
	}
	rearmed.fire()
	ticks.waitArmed(t)
	if n := gh.requests.Load(); n != 1 {
		t.Errorf("the tick a full interval after checks were turned on asked GitHub %d time(s), want 1", n)
	}
}

// TestATickArmedBeforeUpdateChecksWentOffAsksNothingOnceTheyAreOnAgain: the
// off transition rearms nothing, so the wait the loop armed while update
// checks were on is still its wait while they are off; when they read on
// again, published before the updater is told, that wait's tick asks GitHub
// nothing, though it was armed with the switch on: a transition came between
// (the schedule's generation). The notification then rearms a full interval,
// and that wait's tick checks.
func TestATickArmedBeforeUpdateChecksWentOffAsksNothingOnceTheyAreOnAgain(t *testing.T) {
	gh := newReleaseStandIn(t, "v9.9.9")
	sw := &testSwitch{}
	u, ticks := switchedUpdater(t, gh, sw, Options{LiveCheckInterval: func() time.Duration { return 3 * time.Hour }})
	runLoop(t, u)
	armedOn := ticks.waitArmed(t) // the start's check is done; this wait is armed with checks on
	if n := gh.requests.Load(); n != 1 {
		t.Fatalf("the start asked GitHub %d time(s) with update checks on, want 1", n)
	}

	sw.off.Store(true)
	u.ChecksSwitched()  // off: nothing under way, and the loop keeps its wait
	sw.off.Store(false) // on again, published; the updater not told yet
	armedOn.fire()
	ticks.waitArmed(t) // the tick is done
	if n := gh.requests.Load(); n != 1 {
		t.Fatalf("the tick of a wait armed before update checks went off asked GitHub %d more time(s) once they read on "+
			"again, before the updater was told; want none", n-1)
	}
	u.ChecksSwitched()
	rearmed := ticks.waitArmed(t)
	if rearmed.d != 3*time.Hour || gh.requests.Load() != 1 {
		t.Fatalf("the wait armed for checks turned on again = %v after %d request(s), want a full interval, 3h, and only "+
			"the start's", rearmed.d, gh.requests.Load())
	}
	rearmed.fire()
	ticks.waitArmed(t)
	if n := gh.requests.Load(); n != 2 {
		t.Errorf("the tick a full interval after checks were turned on again asked GitHub %d time(s) in all, want 2", n)
	}
}

// TestAWakeAndAnOldTickReadyTogetherAskNothing: the loop armed a wait while
// update checks were off, and before its select both that wait's tick and the
// notification that they are on are ready (the seam holds the loop between
// its arming and its select). Whichever the select takes, nothing is asked of
// GitHub: the old wait's tick is refused, being armed under another schedule,
// and the notification rearms; the wait the loop then waits on is a full
// interval, and its tick checks. Without the schedule the select takes the
// tick about half the time, so a few runs show it.
func TestAWakeAndAnOldTickReadyTogetherAskNothing(t *testing.T) {
	gh := newReleaseStandIn(t, "v9.9.9")
	sw := &testSwitch{}
	sw.off.Store(true)
	u, ticks := switchedUpdater(t, gh, sw, Options{LiveCheckInterval: func() time.Duration { return 3 * time.Hour }})
	runLoop(t, u)
	first := ticks.waitArmed(t)
	release := ticks.holdNext(t)
	first.fire() // a tick while off; the loop's next arming is held
	old := ticks.waitArmed(t)

	sw.off.Store(false)
	u.ChecksSwitched() // the notification, ready
	old.fire()         // and the old wait's tick, ready too
	inspect := ticks.holdNext(t)
	release()
	next := ticks.waitArmed(t)
	if n := gh.requests.Load(); n != 0 {
		t.Fatalf("with the notification and a tick armed while off ready together GitHub was asked %d time(s), want 0", n)
	}
	// The loop is held at this arming: a notification it has not taken yet
	// rearms it once more.
	pending := len(u.wake) > 0
	inspect()
	if pending {
		next = ticks.waitArmed(t)
	}
	if next.d != 3*time.Hour || gh.requests.Load() != 0 {
		t.Fatalf("the wait armed for checks turned on = %v after %d request(s), want a full interval, 3h, and none", next.d, gh.requests.Load())
	}
	old.fire() // the obsolete wait again: nothing reads it
	next.fire()
	ticks.waitArmed(t)
	if n := gh.requests.Load(); n != 1 {
		t.Errorf("the tick a full interval after checks were turned on asked GitHub %d time(s), want 1", n)
	}
}

// TestUpdateChecksTurnedOffAndOnAgainQuickly: off and on again while a check
// is on the wire stops that check, whatever came after, and the loop then
// waits a full interval, asking nothing, before the next check, which runs
// and is recorded.
func TestUpdateChecksTurnedOffAndOnAgainQuickly(t *testing.T) {
	gh := newReleaseStandIn(t, "v9.9.9")
	gh.holding.Store(true)
	sw := &testSwitch{}
	u, ticks := switchedUpdater(t, gh, sw, Options{})
	runLoop(t, u)
	held := gh.waitHeld(t) // the start's check

	sw.off.Store(true)
	u.ChecksSwitched()
	sw.off.Store(false)
	u.ChecksSwitched()
	held.waitEnded(t)
	ticks.waitAsked(t) // the loop's first wait, after the stopped check
	ticks.waitAsked(t) // and its rearm, for checks turned on
	if n := gh.requests.Load(); n != 1 {
		t.Errorf("GitHub was asked %d time(s) across the switch, want only the stopped check's", n)
	}
	if st := u.cached(); !st.LastCheck.IsZero() || st.UpdateAvailable || st.LastError != "" {
		t.Errorf("the stopped check was recorded: %+v", st)
	}

	gh.holding.Store(false)
	ticks.fire(t)
	ticks.waitAsked(t)
	if st := u.Status(); !st.CheckEnabled || !st.UpdateAvailable || gh.requests.Load() != 2 {
		t.Errorf("the check after checks were on again: %+v after %d request(s), want 9.9.9 found by the second", st, gh.requests.Load())
	}
}

// TestAStatusWhileUpdateChecksAreOffShowsNoUpdate: a check found 9.9.9, and
// the auto-installer and an earlier poll left their reason and error; with
// update checks off the status shows none of it as current, CheckEnabled
// false and the last check's time kept, and on again it shows it all.
func TestAStatusWhileUpdateChecksAreOffShowsNoUpdate(t *testing.T) {
	gh := newReleaseStandIn(t, "v9.9.9")
	sw := &testSwitch{}
	u := New(Options{RepoOverride: "fake/repo", Client: gh.client(t), CheckEnabled: sw.on})
	checkNow(t, u)
	u.mu.Lock()
	u.status.DeferredReason = "would orphan device(s): \"iPhone\" on 1.0 (requires 2.0+)"
	u.status.LastError = "github status 503: an earlier poll's failure"
	u.mu.Unlock()

	on := u.Status()
	if !on.CheckEnabled || !on.UpdateAvailable || on.LatestVersion != "9.9.9" || on.ReleaseNotesURL == "" ||
		on.DeferredReason == "" || on.LastError == "" || on.LastCheck.IsZero() {
		t.Fatalf("the status with update checks on = %+v, want everything the check and the gates left", on)
	}

	sw.off.Store(true)
	off := u.Status()
	want := on
	want.CheckEnabled = false
	want.LatestVersion, want.UpdateAvailable, want.ReleaseNotesURL = "", false, ""
	want.DeferredReason, want.LastError = "", ""
	if off != want {
		t.Errorf("the status with update checks off = %+v, want %+v", off, want)
	}

	sw.off.Store(false)
	if again := u.Status(); again != on {
		t.Errorf("the status with update checks on again = %+v, want %+v", again, on)
	}
}
