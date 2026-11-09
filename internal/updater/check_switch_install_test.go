//go:build !windows

package updater

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The update check's switch and installs (backlog B761): while update checks
// are off no new release is installed, by hand or automatically, a candidate
// a check found before included; an install not yet past its decision under
// the install marker's lock is stopped by the off transition, during its
// download or at that decision; one past it finishes its swap; and a restart
// the auto-installer deferred after a swap that committed still fires, with
// its gates, asking GitHub nothing.

// switchedInstall is an install of latest over current, on disk, with an
// auto-installing Updater asking the fake release server, its update check's
// switch sw.
type switchedInstall struct {
	fix      *installFixture
	upd      *Updater
	sw       *testSwitch
	dir      string
	livePath string
	restarts *atomic.Int32
}

// newSwitchedInstall builds it, edit changing the Updater's options and the
// auto-installer's install options before New; its status is that of a check
// that found latest.
func newSwitchedInstall(t *testing.T, current, latest string, edit func(*Options, *InstallOptions)) *switchedInstall {
	t.Helper()
	fix := newInstallFixture(t, latest)
	dir := t.TempDir()
	livePath := filepath.Join(dir, "bridge")
	if err := os.WriteFile(livePath, []byte("bridge-binary-"+current), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &switchedInstall{fix: fix, sw: &testSwitch{}, dir: dir, livePath: livePath, restarts: &atomic.Int32{}}
	auto := &InstallOptions{DataDir: dir, BinaryPath: livePath, Force: true, Verifier: noopVerifier}
	opts := Options{
		RepoOverride:       "fake/repo",
		Client:             NewClient("fake/repo", 30*time.Second).WithBaseURL(fix.server.URL),
		AutoInstall:        true,
		AutoInstallOpts:    auto,
		AutoInstallRestart: func() { s.restarts.Add(1) },
		CheckEnabled:       s.sw.on,
	}
	if edit != nil {
		edit(&opts, auto)
	}
	s.upd = New(opts)
	s.upd.mu.Lock()
	s.upd.status.CurrentVersion = current
	s.upd.status.LatestVersion = fix.latestVersion
	s.upd.status.UpdateAvailable = true
	s.upd.status.LastCheck = time.Now().UTC()
	s.upd.mu.Unlock()
	return s
}

// manual is a console's install options for s.
func (s *switchedInstall) manual() InstallOptions {
	return InstallOptions{DataDir: s.dir, BinaryPath: s.livePath, Force: true, Verifier: noopVerifier}
}

// turnOff turns update checks off and tells the updater, as serve's settings
// save does once its commit is published.
func (s *switchedInstall) turnOff() {
	s.sw.off.Store(true)
	s.upd.ChecksSwitched()
}

// requireNothingInstalled requires the install as it was before any of it:
// the binary current's, no backup, no install marker, and the marker's lock
// free.
func (s *switchedInstall) requireNothingInstalled(t *testing.T, current string) {
	t.Helper()
	if got, err := os.ReadFile(s.livePath); err != nil || string(got) != "bridge-binary-"+current {
		t.Errorf("the binary = %q (%v), want %s's, untouched", got, err, current)
	}
	if _, err := os.Lstat(s.livePath + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a backup is beside the binary (lstat: %v): nothing was to be swapped", err)
	}
	if _, err := os.Lstat(StatePath(s.dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an install marker was written (lstat: %v): nothing was to be armed", err)
	}
	lock, err := LockState(context.Background(), s.dir)
	if err != nil || !lock.Held() {
		t.Errorf("the install marker's lock after the install = %v (held %v), want it free", err, lock != nil && lock.Held())
	}
	if lock != nil {
		lock.Release()
	}
}

// parked holds an install at one of its seams (holdBeforeDecision,
// holdBeforeSwap) until the test lets it go, or the test's end does.
type parked struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newParked(t *testing.T) *parked {
	p := &parked{reached: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(p.let)
	return p
}

// hook is the seam's function: it says the install reached it, and waits.
func (p *parked) hook() {
	close(p.reached)
	<-p.release
}

// let lets the install go on.
func (p *parked) let() { p.once.Do(func() { close(p.release) }) }

// waitFor waits for ch to close, saying what it waits for if it never does.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(testWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// installInBackground runs s's Install with opts on a goroutine of its own
// and hands back its answer.
func (s *switchedInstall) installInBackground(opts InstallOptions) <-chan error {
	answered := make(chan error, 1)
	go func() {
		_, err := s.upd.Install(context.Background(), opts)
		answered <- err
	}()
	return answered
}

func answerOf(t *testing.T, answered <-chan error) error {
	t.Helper()
	select {
	case err := <-answered:
		return err
	case <-time.After(testWait):
		t.Fatal("the install never answered")
		return nil
	}
}

// TestNoAutoInstallOfACandidateFoundBeforeUpdateChecksWentOff is the
// post-poll race: a check that answered found 0.2.0, the switch went off
// before the auto-installer ran, and the auto-installer installs nothing,
// asks nothing and does not try (no "auto-installing" line).
func TestNoAutoInstallOfACandidateFoundBeforeUpdateChecksWentOff(t *testing.T) {
	s := newSwitchedInstall(t, "0.1.0", "0.2.0", nil)
	if st := checkNow(t, s.upd); !st.UpdateAvailable || s.fix.requests.Load() != 1 {
		t.Fatalf("the check before the switch went off: %+v after %d request(s), want 0.2.0 found", st, s.fix.requests.Load())
	}
	s.sw.off.Store(true) // after the check answered, before the auto-installer runs

	rec := loggingtest.Record(t)
	s.upd.maybeAutoInstall(context.Background())
	s.requireNothingInstalled(t, "0.1.0")
	if n := s.fix.requests.Load(); n != 1 {
		t.Errorf("the auto-installer asked the release server %d more time(s), want none", n-1)
	}
	if got := rec.Lines("auto-installing"); len(got) != 0 {
		t.Errorf("the auto-installer began an install with update checks off:\n%s", strings.Join(got, "\n"))
	}
	if n := s.restarts.Load(); n != 0 {
		t.Errorf("the bridge was restarted %d time(s) for an install that never happened", n)
	}
}

// TestAnInstallIsRefusedWhileUpdateChecksAreOff: an install asked for by
// hand, the console's or `bridge update`'s, is refused while update checks
// are off, ErrUpdateChecksOff, before it asks anything, the candidate a
// check found before included, and the status it answers says the checks are
// off.
func TestAnInstallIsRefusedWhileUpdateChecksAreOff(t *testing.T) {
	s := newSwitchedInstall(t, "0.1.0", "0.2.0", nil)
	s.sw.off.Store(true)
	st, err := s.upd.Install(context.Background(), s.manual())
	if !errors.Is(err, ErrUpdateChecksOff) {
		t.Errorf("an install with update checks off = %v, want ErrUpdateChecksOff", err)
	}
	if st.CheckEnabled || st.UpdateAvailable {
		t.Errorf("the status the refused install answered = %+v, want checks off and no update shown", st)
	}
	if n := s.fix.requests.Load(); n != 0 {
		t.Errorf("the refused install asked the release server %d time(s), want 0", n)
	}
	s.requireNothingInstalled(t, "0.1.0")
}

// TestTurningUpdateChecksOffDuringADownloadStopsTheInstall: the install's
// archive is on the wire, held there, when update checks are turned off; the
// off transition stops it there, the install answers ErrUpdateChecksOff, and
// nothing is swapped, backed up or armed.
func TestTurningUpdateChecksOffDuringADownloadStopsTheInstall(t *testing.T) {
	s := newSwitchedInstall(t, "0.1.0", "0.2.0", nil)
	arrived, ended := make(chan struct{}), make(chan struct{})
	// Ended at the test's end too, before the fixture's server closes, which
	// waits for the handler: a failed test's download never answers.
	testDone := make(chan struct{})
	t.Cleanup(func() { close(testDone) })
	hold := func(r *http.Request) {
		close(arrived)
		select {
		case <-r.Context().Done():
			close(ended)
		case <-testDone:
		}
		// The client gave up, or the test did: answer nothing.
		panic(http.ErrAbortHandler)
	}
	s.fix.archiveHold.Store(&hold)

	answered := s.installInBackground(s.manual())
	waitFor(t, arrived, "the install's download")
	s.turnOff()
	waitFor(t, ended, "the download the switch stopped to end")
	if err := answerOf(t, answered); !errors.Is(err, ErrUpdateChecksOff) {
		t.Errorf("the install the switch stopped answered %v, want ErrUpdateChecksOff", err)
	}
	s.requireNothingInstalled(t, "0.1.0")
}

// TestUpdateChecksTurnedOffJustBeforeAnInstallsDecisionStopItBeforeItsSwap:
// the install has downloaded and checked its candidate and holds the install
// marker's lock, just before its decision under it, when update checks are
// turned off. The decision asks the switch again and refuses, whether the off
// transition reached the install (signalled) or the switch alone moved: the
// boundary is that decision, and nothing is swapped, backed up or armed.
func TestUpdateChecksTurnedOffJustBeforeAnInstallsDecisionStopItBeforeItsSwap(t *testing.T) {
	for _, signalled := range []bool{false, true} {
		name := "the switch alone moved"
		if signalled {
			name = "the off transition reached the install"
		}
		t.Run(name, func(t *testing.T) {
			s := newSwitchedInstall(t, "0.1.0", "0.2.0", nil)
			park := newParked(t)
			opts := s.manual()
			opts.holdBeforeDecision = park.hook
			answered := s.installInBackground(opts)
			waitFor(t, park.reached, "the install's decision")
			s.sw.off.Store(true)
			if signalled {
				s.upd.ChecksSwitched()
			}
			park.let()
			if err := answerOf(t, answered); !errors.Is(err, ErrUpdateChecksOff) {
				t.Errorf("the install the switch stopped at its decision answered %v, want ErrUpdateChecksOff", err)
			}
			s.requireNothingInstalled(t, "0.1.0")
		})
	}
}

// TestASwapPastItsDecisionFinishesWhenUpdateChecksGoOff: past the decision,
// its marker armed and just before its swap, the install is committed, and an
// off transition then stops nothing of it: the swap finishes.
func TestASwapPastItsDecisionFinishesWhenUpdateChecksGoOff(t *testing.T) {
	s := newSwitchedInstall(t, "0.1.0", "0.2.0", nil)
	park := newParked(t)
	opts := s.manual()
	opts.holdBeforeSwap = park.hook
	answered := s.installInBackground(opts)
	waitFor(t, park.reached, "the install's swap")
	s.turnOff()
	park.let()
	if err := answerOf(t, answered); err != nil {
		t.Fatalf("the install committed before the switch went off answered %v, want its swap", err)
	}
	if got, err := os.ReadFile(s.livePath); err != nil || string(got) != "bridge-binary-0.2.0" {
		t.Errorf("the binary = %q (%v), want 0.2.0's", got, err)
	}
	if got, err := os.ReadFile(s.livePath + ".bak"); err != nil || string(got) != "bridge-binary-0.1.0" {
		t.Errorf("the backup = %q (%v), want 0.1.0's", got, err)
	}
	if st, err := LoadState(s.dir); err != nil || st.Status != "installing" || !st.SwapStarted || st.TargetVersion != "0.2.0" {
		t.Errorf("the install marker = %+v (%v), want 0.2.0 installing, swapped", st, err)
	}
}

// TestARestartDeferredAfterACommittedSwapFiresWhileUpdateChecksAreOff: the
// auto-installer swapped 0.2.0 in and deferred its restart for a download
// that began during the install; update checks are then turned off. The poll
// loop's ticks go on with no request, and each fires the deferred restart
// only through its gates: not while the download is in flight, nor outside
// the quiet hours, nor while 0.2.0 is held back; once all three let it, the
// restart fires, its marker restamped, and GitHub is asked nothing.
func TestARestartDeferredAfterACommittedSwapFiresWhileUpdateChecksAreOff(t *testing.T) {
	tracker := NewTracker()
	var began atomic.Bool
	var clock atomic.Int64
	clock.Store(time.Date(2026, 11, 9, 12, 0, 0, 0, time.Local).UnixNano())
	window := atomic.Bool{}
	s := newSwitchedInstall(t, "0.1.0", "0.2.0", func(o *Options, auto *InstallOptions) {
		auto.Force = false
		auto.Sessions = tracker
		auto.Verifier = func(context.Context, string) error {
			// A download begins during the install, so its restart defers.
			if began.CompareAndSwap(false, true) {
				tracker.Begin()
			}
			return nil
		}
		o.Now = func() time.Time { return time.Unix(0, clock.Load()) }
		o.LiveQuietHours = func() (int, int) {
			if window.Load() {
				return 2 * 60, 4 * 60 // 02:00-04:00, which 12:00 is outside
			}
			return 0, 0
		}
	})
	s.upd.maybeAutoInstall(context.Background())
	if got, err := os.ReadFile(s.livePath); err != nil || string(got) != "bridge-binary-0.2.0" {
		t.Fatalf("the auto-install did not swap: the binary = %q (%v)", got, err)
	}
	if n := s.restarts.Load(); n != 0 || !s.upd.pendingRestart.Load() {
		t.Fatalf("restarts = %d, pending %v: want the restart deferred for the download", n, s.upd.pendingRestart.Load())
	}
	swapped, err := LoadState(s.dir)
	if err != nil {
		t.Fatal(err)
	}

	s.turnOff()
	asked := s.fix.requests.Load()
	ticks := installTickSeam(s.upd)
	runLoop(t, s.upd)
	ticks.waitAsked(t) // the start, which asks nothing with update checks off
	tickOnce := func(what string, wantRestarts int32) {
		t.Helper()
		ticks.fire(t)
		ticks.waitAsked(t)
		if n := s.restarts.Load(); n != wantRestarts {
			t.Errorf("%s: restarts = %d, want %d", what, n, wantRestarts)
		}
	}

	tickOnce("a tick while the download is in flight", 0)
	tracker.End()
	window.Store(true)
	tickOnce("a tick outside the quiet hours", 0)
	window.Store(false)
	held := swapped
	held.RejectedVersion = "0.2.0"
	if err := SaveState(s.dir, held); err != nil {
		t.Fatal(err)
	}
	tickOnce("a tick while 0.2.0 is held back", 0)
	if err := SaveState(s.dir, swapped); err != nil {
		t.Fatal(err)
	}
	restamping := time.Now().UTC()
	tickOnce("a tick once every gate lets it", 1)

	if n := s.fix.requests.Load(); n != asked {
		t.Errorf("the release server was asked %d time(s) after update checks went off, want 0", n-asked)
	}
	st, err := LoadState(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.AttemptedAt.Before(restamping) {
		t.Errorf("the marker's time after the deferred restart = %v, want it restamped at the restart (%v or after)", st.AttemptedAt, restamping)
	}
	want := swapped
	want.AttemptedAt = st.AttemptedAt
	if st != want {
		t.Errorf("the marker after the restamp = %+v, want it as the swap left it but for its time, %+v", st, want)
	}
}

// TestAManualInstallGainsNoRestartWhileUpdateChecksAreOff: an install by hand
// swaps and leaves the restart to the operator; with update checks then off,
// the poll loop's ticks restart nothing, auto-install on or not, since a
// manual install never takes the auto-installer's restart intent.
func TestAManualInstallGainsNoRestartWhileUpdateChecksAreOff(t *testing.T) {
	s := newSwitchedInstall(t, "0.1.0", "0.2.0", nil)
	if _, err := s.upd.Install(context.Background(), s.manual()); err != nil {
		t.Fatalf("the manual install: %v", err)
	}
	s.turnOff()
	ticks := installTickSeam(s.upd)
	runLoop(t, s.upd)
	ticks.waitAsked(t)
	for i := 0; i < 2; i++ {
		ticks.fire(t)
		ticks.waitAsked(t)
	}
	if n := s.restarts.Load(); n != 0 {
		t.Errorf("the ticks restarted the bridge %d time(s) after a manual install, want 0", n)
	}
}
