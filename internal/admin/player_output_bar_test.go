package admin

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The player bar's output (B634, PR 2): the shipped output.js, format.js,
// audio.js and nowplaying.js under node, case by case
// (testdata/bar_harness.mjs), and app.js's forwarding of the event stream's
// lifecycle. A host without node skips these cases; CI runs them. Each case
// is one scenario of the harness, named as the test is, which reports named
// checks: the test fails on any check that failed, and on fewer checks than
// the scenario makes (a scenario that stopped checking anything).

// barResult is what a scenario reports.
type barResult struct {
	Pass []string `json:"pass"`
	Fail []struct {
		Name   string `json:"name"`
		Detail string `json:"detail"`
	} `json:"fail"`
}

// barScenario runs the harness's scenario named as the calling test is, and
// requires checks of its checks to pass and none to fail.
func barScenario(t *testing.T, checks int) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped player modules")
	}
	harness, err := filepath.Abs(filepath.Join("testdata", "bar_harness.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.Abs(filepath.Join("static", "player"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, harness, fileURL(dir)+"/", t.Name()).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var r barResult
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &r); err != nil {
		t.Fatalf("the harness answered %s: %v", raw, err)
	}
	for _, f := range r.Fail {
		t.Errorf("%s: %s", f.Name, f.Detail)
	}
	if len(r.Fail) == 0 && len(r.Pass) < checks {
		t.Errorf("%d checks passed of the %d this case makes: it has stopped checking something (%v)", len(r.Pass), checks, r.Pass)
	}
}

// TestAStaleCommandRereadsAndNeverReplays pins §7.5's stale answer: a command
// refused stale takes the state its answer carries, reads the queue again
// for that state's queueRev, says the player changed, and is never sent
// again.
func TestAStaleCommandRereadsAndNeverReplays(t *testing.T) { barScenario(t, 4) }

// TestAPartialReplaceIsShownAsItIs pins §2.7: a replace another controller
// interrupted is shown as it is, with how many of its tracks the queue holds
// and what the player does, and nothing is rolled back or sent again.
func TestAPartialReplaceIsShownAsItIs(t *testing.T) { barScenario(t, 3) }

// TestAnUnconfirmedOperationClearsOnlyOnItsRecord pins §2.3 and G3: an
// operation whose answer is lost is unconfirmed until its record says how it
// ended, a change of the engine's state that looks like its effect clearing
// nothing, and it is never sent again.
func TestAnUnconfirmedOperationClearsOnlyOnItsRecord(t *testing.T) { barScenario(t, 4) }

// TestAReloadedTabFindsItsOperation pins G5 and M2: a document that inherits
// a pending operation in session storage takes a tab identity of its own
// and reads the inherited one back by the identity it was sent under.
func TestAReloadedTabFindsItsOperation(t *testing.T) { barScenario(t, 4) }

// TestTheSwitchToTheBridgeStopsTheBrowserFirst pins §7.3, step 1: the
// browser's handover is read, the browser stops, and only then does the
// replace go.
func TestTheSwitchToTheBridgeStopsTheBrowserFirst(t *testing.T) { barScenario(t, 2) }

// TestTheSwitchToTheBrowserStartsItOnlyOnSilence pins §7.4: the browser takes
// the bridge's queue only on the barrier's done, never while the barrier has
// not answered, whatever the frames meanwhile read.
func TestTheSwitchToTheBrowserStartsItOnlyOnSilence(t *testing.T) { barScenario(t, 4) }

// TestARefusalOrAPausesSuccessIsNotSilence pins C2: a barrier refused under
// the DSD check, whose publication reads silent, and one whose Pause
// succeeded and the bridge went on playing, start nothing.
func TestARefusalOrAPausesSuccessIsNotSilence(t *testing.T) { barScenario(t, 4) }

// TestAnUnknownSwitchNeverStartsTheBrowser pins R4 in both directions: a
// switch to the bridge and one to the browser whose answers are lost, and
// whose outcomes read back unknown or are made unknown by a new epoch,
// start nothing.
func TestAnUnknownSwitchNeverStartsTheBrowser(t *testing.T) { barScenario(t, 4) }

// TestARefusalThatChangedNothingLetsTheBrowserPlayOnOnlyBesideSilence pins
// §7.3, step 4: a switch to the bridge refused with nothing changed resumes
// the browser through the barrier where the bridge reads silent, and sends
// nothing and starts nothing where it plays.
func TestARefusalThatChangedNothingLetsTheBrowserPlayOnOnlyBesideSilence(t *testing.T) {
	barScenario(t, 4)
}

// TestThePositionCarriesOverForPCM pins R5: the switch to the bridge carries
// a PCM track's whole seconds and none for DSD.
func TestThePositionCarriesOverForPCM(t *testing.T) { barScenario(t, 2) }

// TestTheBarsPositionHoldsThroughARepeatChange pins §4.4: the position is the
// anchor's own, so a frame for a repeat change moves nothing.
func TestTheBarsPositionHoldsThroughARepeatChange(t *testing.T) { barScenario(t, 3) }

// TestTheBarsPositionHoldsThroughAQueueUpdate pins §4.4: a frame for a queue
// update moves no position.
func TestTheBarsPositionHoldsThroughAQueueUpdate(t *testing.T) { barScenario(t, 1) }

// TestTheBarsPositionHoldsAcrossAReconnection pins §4.4 and M1: a
// reconnection's first frame carries the anchor, and once the new
// connection's clock sample lands the position goes on where it was.
func TestTheBarsPositionHoldsAcrossAReconnection(t *testing.T) { barScenario(t, 2) }

// TestANewEpochWinsAndAnOldOneIsDropped pins §4.2: a new epoch is taken from
// a connection's first frame, and the old one's states, by frame or by
// answer, are dropped.
func TestANewEpochWinsAndAnOldOneIsDropped(t *testing.T) { barScenario(t, 2) }

// TestAHoldNeverWipesTheView pins §4.3: through the DSD check's hold the bar
// keeps the bridge's view, its track included, and enables Stop alone.
func TestAHoldNeverWipesTheView(t *testing.T) { barScenario(t, 3) }

// TestReturningEligibilityShowsTheActualState pins §4.3: when the bridge's
// player is offered again the bar shows what it does, another controller's
// playback included, never a prescribed "stopped", and sends nothing.
func TestReturningEligibilityShowsTheActualState(t *testing.T) { barScenario(t, 2) }

// TestTheBarKeepsTheViewWhileTheBridgeIsUnavailable pins §7.2: the DAC away,
// the bar stays on the bridge's view with the DAC's word and only the
// commands the engine takes.
func TestTheBarKeepsTheViewWhileTheBridgeIsUnavailable(t *testing.T) { barScenario(t, 3) }

// TestTheBridgesReturnPlaysNothing pins §7.2: the DAC back, the bar sends
// nothing and shows the player as published.
func TestTheBridgesReturnPlaysNothing(t *testing.T) { barScenario(t, 2) }

// TestRowsFollowTheChosenOutput pins §7.6: one decision gives a row its
// playability and its chip for the output chosen, a DSD row playing on the
// bridge's player and reading its way, a routed one reading why.
func TestRowsFollowTheChosenOutput(t *testing.T) { barScenario(t, 4) }

// TestADSDOnlyAlbumPlaysOnTheBridgeFromAnEmptyBar pins A2 to A4: from empty
// session storage the bridge's player is picked with nothing sent, and a
// DSD-only album is played on it by its id.
func TestADSDOnlyAlbumPlaysOnTheBridgeFromAnEmptyBar(t *testing.T) { barScenario(t, 4) }

// TestTheBarOffersItsOutputFromEmptySessionStorage pins R2 and K1: GET now's
// answer offers the bridge's player before the stream opens, the first frame
// lets it be picked, and a box that offers none says so at once.
func TestTheBarOffersItsOutputFromEmptySessionStorage(t *testing.T) { barScenario(t, 3) }

// TestPickingTheBridgeFromAnEmptyBarSendsNothing pins §7.3: with nothing in
// the browser the pick is the bridge's at once, kept in session storage, and
// sends nothing.
func TestPickingTheBridgeFromAnEmptyBarSendsNothing(t *testing.T) { barScenario(t, 3) }

// TestEveryBrowserStartGoesThroughTheBarrier pins G1: a switch to the bridge
// refused with nothing changed resumes the browser only on the barrier's
// done, and sends no barrier where the bridge plays.
func TestEveryBrowserStartGoesThroughTheBarrier(t *testing.T) { barScenario(t, 4) }

// TestALateAnswerFromBeforeARestartNeverReplacesTheNewEpoch pins G2: the old
// serve's answer after the restart's first frame, and before it, never ends
// on the old epoch, and the new serve's answer while the stream reconnects
// is held until the first frame confirms it, then taken by revision.
func TestALateAnswerFromBeforeARestartNeverReplacesTheNewEpoch(t *testing.T) { barScenario(t, 4) }

// TestAnOperationIsConfirmedByItsRecordNeverByAChange pins G3: a Pause of a
// stopped bridge, which changes nothing the engine publishes, is confirmed
// by its answer's record under a revision of its own.
func TestAnOperationIsConfirmedByItsRecordNeverByAChange(t *testing.T) { barScenario(t, 1) }

// TestTheFirstStateOfAnEpochLeadsToAQueueReadWhateverTheToken pins G4: the
// first state of an epoch reads the queue, its token 0 or not, and so does
// the next queueRev.
func TestTheFirstStateOfAnEpochLeadsToAQueueReadWhateverTheToken(t *testing.T) { barScenario(t, 4) }

// TestAnOlderQueueReadNeverWinsOverANewerFrame pins G4: a queue read held
// while a frame with a newer queueRev arrives is dropped and made again.
func TestAnOlderQueueReadNeverWinsOverANewerFrame(t *testing.T) { barScenario(t, 1) }

// TestAReloadedTabReadsItsOperationBack pins G5: a reloaded tab whose
// switch-out is not among the stream's last eight reads it back whole, its
// handover included.
func TestAReloadedTabReadsItsOperationBack(t *testing.T) { barScenario(t, 1) }

// TestARecoveredHandoverIsHistory pins G8: a switch-out read back done is
// history: its handover is taken into the browser, stopped, and the browser
// starts only on a new barrier's done where the bridge reads silent, and
// not at all where it plays again.
func TestARecoveredHandoverIsHistory(t *testing.T) { barScenario(t, 4) }

// TestARefusedSwitchInReadsNothingChangedFromItsEffect pins G9: a switch to
// the bridge whose stale first step was sent reads nothing changed from its
// record's effect, never from the count sent.
func TestARefusedSwitchInReadsNothingChangedFromItsEffect(t *testing.T) { barScenario(t, 2) }

// TestAClockSampleIsTheServersClockAtSend pins M1: a fresh tab given an aged,
// advancing anchor shows the anchor's seconds until GET now's sample of the
// server's clock lands, and then the anchor advanced by its own age.
func TestAClockSampleIsTheServersClockAtSend(t *testing.T) { barScenario(t, 2) }

// TestALateClockSampleIsDropped pins M1: a sample asked on an earlier
// connection, or before the epoch was taken, is dropped whole.
func TestALateClockSampleIsDropped(t *testing.T) { barScenario(t, 2) }

// TestAFailedClockSampleExtrapolatesNothing pins M1: a sample lost,
// malformed or with no clock leaves the position unextrapolated, and is
// asked again within 5 s.
func TestAFailedClockSampleExtrapolatesNothing(t *testing.T) { barScenario(t, 6) }

// TestAResetSamplesTheClockAgain pins M1: a page shown again and a clock jump
// forget the samples and sample again.
func TestAResetSamplesTheClockAgain(t *testing.T) { barScenario(t, 6) }

// TestAClockSampleAskedBeforeAResetIsDropped pins P7: a sample held across a
// clock jump within one connection and epoch is dropped by its sampling
// generation, and only one asked after the reset enables extrapolation.
func TestAClockSampleAskedBeforeAResetIsDropped(t *testing.T) { barScenario(t, 2) }

// TestClonedTabsNeverShareAnOperation pins M2: two documents from one session
// storage send identical commands under identities of their own.
func TestClonedTabsNeverShareAnOperation(t *testing.T) { barScenario(t, 2) }

// TestAnOperationConflictTakesANewTabIdentity pins P2: an injected 409
// operation_conflict makes the bar take a new tab identity, its count reset,
// and send nothing again.
func TestAnOperationConflictTakesANewTabIdentity(t *testing.T) { barScenario(t, 3) }

// TestARevisionZeroIsNoState pins M5's note: a publication of revision 0, the
// latest before the publisher's first, is no state.
func TestARevisionZeroIsNoState(t *testing.T) { barScenario(t, 2) }

// TestTheSwitchReadsTheBrowsersHandoverBeforeItLetsGoOfItsSource pins G11
// through the shipped audio.js: the handover's position is read before the
// source is let go (which resets the element's), the queue as listed and
// never a shuffle order, and the saved queue keeps the position read.
func TestTheSwitchReadsTheBrowsersHandoverBeforeItLetsGoOfItsSource(t *testing.T) {
	barScenario(t, 4)
}

// TestTheSwitchInvalidatesTheBrowsersPendingFailureProbe pins M3: a source's
// failure probe that finishes after the switch changes nothing of the track
// it was probing.
func TestTheSwitchInvalidatesTheBrowsersPendingFailureProbe(t *testing.T) { barScenario(t, 2) }

// TestTheSwitchCancelsTheRestoredPosition pins P6: a restored queue's seek,
// parked on its metadata, never seeks the handover's track after a switch.
func TestTheSwitchCancelsTheRestoredPosition(t *testing.T) { barScenario(t, 1) }

// TestTheMediaSessionIsClearedWhileTheBridgeIsChosen pins R6: with the
// bridge's player chosen every MediaSession handler is cleared, its metadata
// and its playback state too.
func TestTheMediaSessionIsClearedWhileTheBridgeIsChosen(t *testing.T) { barScenario(t, 3) }

// TestADSDTrackSwitchedToTheBrowser pins R5 and §7.4: a DSD track the browser
// cannot play stays, paused, with its reason, Next moving to one it plays;
// one with a fresh FLAC sidecar plays it from the position, said converted.
func TestADSDTrackSwitchedToTheBrowser(t *testing.T) { barScenario(t, 3) }

// TestTheBarShowsItsEmptyStripWhereThePlayerIsOffered pins R2 on the shipped
// bar: a box that offers its player shows the bar with nothing chosen, an
// empty strip with its output button.
func TestTheBarShowsItsEmptyStripWhereThePlayerIsOffered(t *testing.T) { barScenario(t, 3) }

// TestTheBarDisablesWhatTheEngineDoesNotDo pins R6 and R10 on the shipped
// bar: with the bridge's player chosen only the commands the engine takes
// are enabled, Stop shown, shuffle, volume and mute disabled with their
// words, repeat two states.
func TestTheBarDisablesWhatTheEngineDoesNotDo(t *testing.T) { barScenario(t, 7) }
