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

// client is an updater Client that asks s, on a transport of its own.
func (s *releaseStandIn) client(t *testing.T) *Client {
	t.Helper()
	c := NewClient("fake/repo", 30*time.Second).WithBaseURL(s.srv.URL)
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

// tickSeam stands in for the poll loop's timer (Updater.after): it hands
// the test each wait the loop asks for, as the loop asks it, and fires the
// loop's tick when the test says.
type tickSeam struct {
	asked chan time.Duration
	tick  chan time.Time
}

func installTickSeam(u *Updater) *tickSeam {
	s := &tickSeam{asked: make(chan time.Duration, 64), tick: make(chan time.Time)}
	u.after = func(d time.Duration) <-chan time.Time {
		s.asked <- d
		return s.tick
	}
	return s
}

// waitAsked waits for the loop to ask for its next wait, which it does once
// the turn before it is done, and returns the wait's length.
func (s *tickSeam) waitAsked(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-s.asked:
		return d
	case <-time.After(testWait):
		t.Fatal("the poll loop never asked for its next wait")
		return 0
	}
}

// fire hands the wait the loop is in its tick.
func (s *tickSeam) fire(t *testing.T) {
	t.Helper()
	select {
	case s.tick <- time.Now():
	case <-time.After(testWait):
		t.Fatal("the poll loop was not waiting for a tick")
	}
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
	if st := u.cached(); !st.LastCheck.IsZero() || st.UpdateAvailable {
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
