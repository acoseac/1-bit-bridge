package main

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B760's second round: a panic on serve's goroutine counts a ready
// start again only where its stop's take-back found the marker holding the
// count the start wrote and reached its write, whatever that write answered,
// and never where a take-back began and found no count of its own.

// takeBackAnswer is one answer a take-back's ask got (serveOpts.takeBackAsked).
type takeBackAnswer struct {
	reached bool
	err     error
}

// tellTakeBackAnswers is a takeBackAsked that hands each answer to answers,
// never blocking the take-back: an answer past the buffer is dropped.
func tellTakeBackAnswers(answers chan<- takeBackAnswer) func(bool, error) {
	return func(reached bool, err error) {
		select {
		case answers <- takeBackAnswer{reached: reached, err: err}:
		default:
		}
	}
}

// nextTakeBackAnswer waits for the take-back's next answer, until the wait on
// serve gives up (serveGiveUp).
func nextTakeBackAnswer(t *testing.T, answers <-chan takeBackAnswer) takeBackAnswer {
	t.Helper()
	select {
	case a := <-answers:
		return a
	case <-serveGiveUp(t):
		t.Fatalf("the take-back of the start's count never answered\nserve's goroutines:\n%s", serveStacks())
		return takeBackAnswer{}
	}
}

// TestAPanicAfterTheStopOfAStartWhoseCountNeverLandedCountsNothing: a start
// whose count's write failed before it published leaves the marker as it was,
// answers as ready, and its stop's take-back, armed, begins as the stop
// arrives, finds no count of its own and writes nothing; the teardown then
// panics, and nothing is counted again, nor said to be. Until B760's second
// round a take-back begun was enough: the marker one below the count the
// admission tried read as the take-back's, and the panic wrote a count the
// start never had, saying its stop had taken it back.
func TestAPanicAfterTheStopOfAStartWhoseCountNeverLandedCountsNothing(t *testing.T) {
	r := newRefusedInstall(t, "")
	// A start of this version opened the data dir before, so this start's
	// count is its admission's only write, over one start before it that did
	// not end cleanly; the store is at the ladder's top, so nothing migrates,
	// and the confirmation is an hour away.
	r.marker.DataDirOpenedBy, r.marker.FailedStarts = version.ServerVersion, 1
	r.withACurrentStore(t)
	marker := watch(t, updater.StatePath(r.dataDir))
	teardown := newParkedTeardown()
	events, seams := newProbationWatch(longProbation)
	answers := make(chan takeBackAnswer, 16)
	sb, panicked := r.launchServeThatMayPanic(t, serveOpts{probation: seams, upnpStopping: teardown.seam,
		saveUpdateState: func(string, updater.State) error { return errors.New("the disk is full") },
		takeBackAsked:   tellTakeBackAnswers(answers),
		teardownHold:    func() { panic(teardownPanic) }})
	teardown.releaseFirst(t)
	events.waitFor(t, probationWindow)
	if !strings.Contains(sb.stderr.String(), "was not counted against its install") {
		t.Fatalf("the start's count was not refused: the test shows nothing; stderr=%s", sb.stderr.String())
	}
	sb.stop()
	teardown.waitParked(t)
	// The take-back the start's ready answer armed has run before the
	// teardown panics: a panic before the stop's notice reached it would leave
	// no take-back begun, and the test would show nothing.
	if a := nextTakeBackAnswer(t, answers); a.reached || a.err != nil {
		t.Fatalf("the take-back's answer = %+v, want no write and no error: the marker holds no count of this start's", a)
	}
	teardown.release.close()
	waitForThePanic(t, sb, panicked, teardownPanic)
	marker.requireUnchanged(t)
	// Both of countAgain's lines begin so; "counted again" alone is in the
	// admission's own "was not counted against its install".
	if strings.Contains(sb.stderr.String(), "panicked after its stop") {
		t.Errorf("serve said it counted again a start it never counted; stderr=%s", sb.stderr.String())
	}
}

// TestAPanicAfterATakeBackWhoseWritePublishedThenFailedCountsTheStartAgainOnce:
// a ready start's stop takes its count back as it arrives, in a write that
// publishes and then fails, as one whose barrier fails after its rename does,
// and the take-back, asking again, finds its count gone back and answers that
// it wrote nothing, with no error; the teardown then panics, and the start is
// counted again, once, the marker back at its count. A take-back judged by its
// last answer would leave the panic's start uncounted.
func TestAPanicAfterATakeBackWhoseWritePublishedThenFailedCountsTheStartAgainOnce(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.withACurrentStore(t)
	teardown := newParkedTeardown()
	events, seams := newProbationWatch(longProbation)
	answers := make(chan takeBackAnswer, 16)
	var saves atomic.Int32
	sb, panicked := r.launchServeThatMayPanic(t, serveOpts{probation: seams, upnpStopping: teardown.seam,
		saveTakeBack: func(dataDir string, st updater.State) error {
			n := saves.Add(1)
			if err := updater.SaveState(dataDir, st); err != nil {
				return err
			}
			if n == 1 {
				return errors.New("the data dir would not fsync")
			}
			return nil
		},
		takeBackAsked: tellTakeBackAnswers(answers),
		teardownHold:  func() { panic(teardownPanic) }})
	teardown.releaseFirst(t)
	events.waitFor(t, probationWindow)
	r.requireCount(t, 1, "while serve runs, ready")
	sb.stop()
	teardown.waitParked(t)
	if a := nextTakeBackAnswer(t, answers); a.err == nil {
		t.Fatalf("the take-back's first answer = %+v, want its write's failure: the test shows nothing", a)
	}
	if a := nextTakeBackAnswer(t, answers); a.reached || a.err != nil {
		t.Fatalf("the take-back's second answer = %+v, want no write and no error, its count gone back already", a)
	}
	if n := saves.Load(); n != 1 {
		t.Fatalf("the take-back wrote %d times, want once: its second ask finds its count gone back", n)
	}
	r.requireCount(t, 0, "once the take-back's write published")
	teardown.release.close()
	waitForThePanic(t, sb, panicked, teardownPanic)
	r.requireCount(t, 1, "after the teardown of its stop panicked")
	if n := strings.Count(sb.stderr.String(), "so it is counted again"); n != 1 {
		t.Errorf("serve said %d times that it counted the start again, want once; stderr=%s", n, sb.stderr.String())
	}
}
