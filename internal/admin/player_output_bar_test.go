package admin

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The player bar's output (B634, PR 2): the shipped output.js, format.js,
// audio.js and nowplaying.js under node, case by case
// (testdata/bar_harness.mjs), and app.js's forwarding of the event stream's
// lifecycle. A host without node skips these cases; under CI they fail
// without it, so they always run there. Each case
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

// barNode is node for a test that executes the shipped player modules: a
// host without it skips the test, and under CI the test fails, so a CI green
// means the case ran (round 4, T5). Every node lookup of the player output's
// tests is this one (TestEveryNodeLookupOfTheOutputsTestsIsBarNode).
func barNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node not installed under CI: the player output's node cases run there, never skip")
		}
		t.Skip("node not installed; this test executes the shipped player modules")
	}
	return node
}

// TestEveryNodeLookupOfTheOutputsTestsIsBarNode holds every
// player_output_*_test.go to one lookup of node, barNode's, so none of them
// skips under CI: each call of exec.LookPath, found by the files' syntax,
// placed in the function that holds it.
func TestEveryNodeLookupOfTheOutputsTestsIsBarNode(t *testing.T) {
	files, err := filepath.Glob("player_output_*_test.go")
	if err != nil || len(files) < 5 {
		t.Fatalf("only %d of the player output's test files found (%v): the guard has stopped reading them", len(files), err)
	}
	var sites []string
	for _, f := range files {
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel && sel.Sel.Name == "LookPath" {
					if pkg, isID := sel.X.(*ast.Ident); isID && pkg.Name == "exec" {
						sites = append(sites, f+":"+fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if len(sites) != 1 || sites[0] != "player_output_bar_test.go:barNode" {
		t.Errorf("the player output's tests look node up at %v: every lookup is barNode's, which fails under CI, and it alone looks", sites)
	}
}

// barScenario runs the harness's scenario named as the calling test is, and
// requires checks of its checks to pass and none to fail.
func barScenario(t *testing.T, checks int) {
	t.Helper()
	node := barNode(t)
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
func TestAnOperationIsConfirmedByItsRecordNeverByAChange(t *testing.T) { barScenario(t, 2) }

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

// TestARecoveredHandoverIsHistory pins G8: this tab's own switch-out, its
// answer lost and its record read back done, is history: its handover is
// taken into the browser, stopped, and the browser starts only on a new
// barrier's done where the bridge reads silent, and not at all where it
// plays again. A record another document left is R8's
// (TestACopiedTabTakesTheMoveStoppedAndStartsNothing): it starts nothing.
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

// Round 2 (B634 PR 2): the bar never plays in both places, and never sticks
// moving.

// TestARefusalBeforeAdmissionEndsTheSwitchToTheBridge pins R2 and V2: a
// switch to the bridge refused before it became an operation (epoch_changed,
// operation_conflict with its new identity, a body's bound, the not-offered
// words) ends back on this browser with the refusal's line, the browser
// resumed only through the gate and only because it played; an unclassified
// 5xx and a network error prove nothing and leave it unconfirmed.
func TestARefusalBeforeAdmissionEndsTheSwitchToTheBridge(t *testing.T) { barScenario(t, 11) }

// TestARefusalBeforeAdmissionEndsTheSwitchToThisBrowserAndTheBarrier pins R2
// for the switch to this browser (back on the bridge with its line, or this
// browser the output where no player is offered) and for the barrier, which
// starts nothing.
func TestARefusalBeforeAdmissionEndsTheSwitchToThisBrowserAndTheBarrier(t *testing.T) {
	barScenario(t, 6)
}

// TestOnlyTheNewestPickMovesTheOutput pins R3 and V11: a switch superseded by
// a newer pick moves neither the output, the phase nor the browser, whether
// it ends done, lost, gone, refused before admission, or behind a pick of
// the bridge that ended at once.
func TestOnlyTheNewestPickMovesTheOutput(t *testing.T) { barScenario(t, 8) }

// TestAStartReadsTheLatestStateAccepted pins R4: a switch-out's or a
// barrier's done beside a state accepted since its silence that shows the
// bridge playing starts nothing, and a refused switch-in judges silence on
// the latest state accepted, never on its answer's: a bridge that plays, or
// a hold that is not silent though nothing sounds, sends no barrier.
func TestAStartReadsTheLatestStateAccepted(t *testing.T) { barScenario(t, 4) }

// TestAStreamRecordWithoutItsWholeRecordIsReadBack pins R5: a published
// record carries no handover and no tracks left out, so a switch-out's and a
// replace's is completed by its record read back whole alone.
func TestAStreamRecordWithoutItsWholeRecordIsReadBack(t *testing.T) { barScenario(t, 3) }

// TestARequestUnansweredPast30SecondsIsUnconfirmed pins R6 and V1: a request
// 30 s old is lost, unconfirmed and read back, and its late answer, before
// or after the read back resolves it, is not read.
func TestARequestUnansweredPast30SecondsIsUnconfirmed(t *testing.T) { barScenario(t, 7) }

// TestEveryUnresolvedOperationIsKeptUntilItResolves pins R7 and V3: a
// conflict keeps the older identity's operation, each resolved by its own
// record by epoch, tab and count; and an inherited operation stays stored,
// read back by each reload, until it resolves.
func TestEveryUnresolvedOperationIsKeptUntilItResolves(t *testing.T) { barScenario(t, 4) }

// TestACopiedTabTakesTheMoveStoppedAndStartsNothing pins R8 and V9: a copy
// of a tab, its session storage carrying a switch-out the original sent,
// reads it back, past the read back's throttle, takes the handover stopped
// and sends no barrier and starts nothing; the reader presses Play.
func TestACopiedTabTakesTheMoveStoppedAndStartsNothing(t *testing.T) { barScenario(t, 5) }

// TestACopiedTabsRecoveredSwitchInStartsNothing pins V5: a copy's recovered
// switch-in, refused with nothing changed, makes this browser the output and
// plays nothing, by no resume, no take to play and no start.
func TestACopiedTabsRecoveredSwitchInStartsNothing(t *testing.T) { barScenario(t, 3) }

// TestARecoveredSwitchOutThatDidNotFinishSaysHowItEnded pins C1: an
// inherited switch-out read back changed, refused or unknown stays on the
// bridge in its record's words.
func TestARecoveredSwitchOutThatDidNotFinishSaysHowItEnded(t *testing.T) { barScenario(t, 3) }

// TestAFailedQueueReadIsAskedAgain pins C4: a queue read that failed is read
// again with no new state to prompt it.
func TestAFailedQueueReadIsAskedAgain(t *testing.T) { barScenario(t, 2) }

// TestEveryStartOfThisBrowserGoesThroughTheGate pins R1 and V7 through the
// shipped audio.js and nowplaying.js with the gate wired as boot.js wires
// it: a row's play, Play, Next and the lock screen's Play and Next start
// nothing beside the bridge playing; beside a bridge that does not sound a
// start plays on the barrier's done alone; Pause never waits.
func TestEveryStartOfThisBrowserGoesThroughTheGate(t *testing.T) { barScenario(t, 8) }

// TestAStartWaitsToKnowWhetherAPlayerIsOffered pins V6: on a box that sends
// no frame a start waits for GET now, which a failed first answer asks
// again, and goes on in this browser on its typed refusal.
func TestAStartWaitsToKnowWhetherAPlayerIsOffered(t *testing.T) { barScenario(t, 3) }

// TestAStartBeforeTheFirstFrameWaitsForTheEpoch pins V7: with a player
// offered and no epoch yet a start waits for the first frame, and one that
// never comes drops it with its line.
func TestAStartBeforeTheFirstFrameWaitsForTheEpoch(t *testing.T) { barScenario(t, 3) }

// TestADeferredStartAsksItsPermitAgain pins V4: a start's play deferred to
// the track's metadata plays nothing once a newer state shows the bridge
// playing; and round 4's T2 class: that refusal ends the intent, so a retry
// after an error, the bridge silent since, plays nothing.
func TestADeferredStartAsksItsPermitAgain(t *testing.T) { barScenario(t, 3) }

// TestADeferredStartPlaysWhileItsPermitHolds is V4's other side: with nothing
// newer the deferred play plays, from its position.
func TestADeferredStartPlaysWhileItsPermitHolds(t *testing.T) { barScenario(t, 2) }

// TestAPositionalPickWaitsForTheQueueItNames pins R9 and V12: while the queue
// the panel drew is behind the state's queueRev every row's pick is disabled
// and a pick by position sends nothing, until the queue is read.
func TestAPositionalPickWaitsForTheQueueItNames(t *testing.T) { barScenario(t, 4) }

// TestTheMenuAndTheQueueCloseOnEscapeAndOutsideClicks pins C8: Escape closes
// the queue, its focus back on its button, and a click outside closes the
// queue and the output menu, one inside leaving it open.
func TestTheMenuAndTheQueueCloseOnEscapeAndOutsideClicks(t *testing.T) { barScenario(t, 6) }

// TestAPickThatGoesDisabledHandsItsFocusToTheOtherPick pins C9: the bridge's
// pick disabled while it holds the focus gives the focus to this browser's
// pick, inside the open menu, never to the page.
func TestAPickThatGoesDisabledHandsItsFocusToTheOtherPick(t *testing.T) { barScenario(t, 2) }

// Round 3 (B634 PR 2): no start outside the gate, no latch that outlives its
// barrier.

// TestASkipPastATrackThatCannotPlayAsksTheGate pins S1 and Q5: the skip past
// a source that played and then could not, its probe done, starts the next
// track only as a start does, on a barrier of its own beside a silent
// bridge, and plays nothing beside a bridge that plays, saying so.
func TestASkipPastATrackThatCannotPlayAsksTheGate(t *testing.T) { barScenario(t, 4) }

// TestATracksEndAsksItsStartsPermitAgain pins S1: the next track at a
// track's end plays at once on its start's permit, and nothing where a state
// since shows the bridge playing, the bar saying so.
func TestATracksEndAsksItsStartsPermitAgain(t *testing.T) { barScenario(t, 3) }

// TestARepeatOneReplayAsksTheGate pins S1 and Q5: a repeat-one replay starts
// the element its end paused only as a start does, on a barrier of its own,
// and plays nothing beside a bridge that plays.
func TestARepeatOneReplayAsksTheGate(t *testing.T) { barScenario(t, 4) }

// TestARemovedRowsSuccessorAsksTheGate pins S1 and Q5: the row that plays
// removed, its successor plays on a barrier of its own, and beside a bridge
// that plays it is loaded and stays paused, the bar saying so; and round 4's
// T2 and W7: that refusal ends the intent, so the next removal, the bridge
// silent again, starts nothing.
func TestARemovedRowsSuccessorAsksTheGate(t *testing.T) { barScenario(t, 4) }

// TestARefusedContinuationEndsTheIntentToPlay pins round 4's T2 and W6: a
// continuation refused at once, and one its barrier refuses at its done, end
// what this browser meant to play, so nothing starts once the bridge is
// silent again; a held barrier that answers done still plays its
// continuation.
func TestARefusedContinuationEndsTheIntentToPlay(t *testing.T) { barScenario(t, 4) }

// TestPlaybackLetGoWithNoPlayerOfferedAsksTheGateToGoOn pins Q1: playback a
// start let go on where no player was offered holds no permit, so once a
// player is offered and plays, its next track plays nothing.
func TestPlaybackLetGoWithNoPlayerOfferedAsksTheGateToGoOn(t *testing.T) { barScenario(t, 4) }

// TestAContinuationBehindABarrierStopsForAPause pins Q3: a skip waiting on
// its barrier, the element paused before the answer, neither plays nor
// moves. The bar's Play/Pause over the failed track is a start
// (TestPlayOverAFailedTrackStartsAgain), and the lock screen's Pause a pause
// (TestTheLockScreensPauseOverAFailedTrackStaysAPause).
func TestAContinuationBehindABarrierStopsForAPause(t *testing.T) { barScenario(t, 2) }

// TestAContinuationBehindABarrierStopsForTheBrowsersOwnPause pins Q3 and Q8:
// the browser's own pause of the element, like the reader's, ends what this
// browser means to play, so a skip waiting on its barrier plays nothing.
func TestAContinuationBehindABarrierStopsForTheBrowsersOwnPause(t *testing.T) { barScenario(t, 2) }

// TestAContinuationNeverDisplacesAStartTheReaderAsked pins S1: a track's end
// while the reader's row waits on its barrier replays nothing, and the row
// plays on the barrier's done.
func TestAContinuationNeverDisplacesAStartTheReaderAsked(t *testing.T) { barScenario(t, 3) }

// TestARetryPlaysOnlyWhatThisBrowserMeantToPlay pins S2, Q6 and Q8: the
// retry after a network error reloads the source, which pauses the element,
// and plays nothing for a take made stopped, plays again what played, and
// plays nothing where a state since shows the bridge playing.
func TestARetryPlaysOnlyWhatThisBrowserMeantToPlay(t *testing.T) { barScenario(t, 4) }

// TestARestoredQueuesRetryPlaysNothing pins S2: a queue restored paused at
// page load plays nothing when its source fails and is retried.
func TestARestoredQueuesRetryPlaysNothing(t *testing.T) { barScenario(t, 2) }

// TestABarrierLostPastItsDeadlineFreesTheNextStart pins S3, Q2 and Q9: a
// barrier whose request never settles, lost at 30 s and read back done,
// starts nothing, asks for Play again, and the next start sends a barrier of
// its own and starts on its answer.
func TestABarrierLostPastItsDeadlineFreesTheNextStart(t *testing.T) { barScenario(t, 3) }

// TestEveryEndOfABarrierFreesTheNextStart pins Q9: a barrier a restart's new
// epoch made unknown, one a newer pick superseded, and one refused before it
// ran free the next start, which sends a barrier of its own.
func TestEveryEndOfABarrierFreesTheNextStart(t *testing.T) { barScenario(t, 7) }

// TestAReloadKeepsANewerPicksSupersession pins S4, Q4 and Q10: a switch to
// this browser lost, then superseded by a pick of the bridge that ends at
// once, is stored superseded, so a reload, and a second one, read it back
// done with its handover and move nothing, the phase not held for it.
func TestAReloadKeepsANewerPicksSupersession(t *testing.T) { barScenario(t, 6) }

// TestABarrierRefusedForNoPlayerLetsItsStartGoOn pins round 3's check of a
// gated start a refusal drops: a barrier refused because no player is
// offered lets its start go on in this browser, as a start goes where none
// is offered.
func TestABarrierRefusedForNoPlayerLetsItsStartGoOn(t *testing.T) { barScenario(t, 2) }

// Round 4 (B634 PR 2): no play the bar did not start sounds outside the gate.

// TestTheBrowsersOwnResumeBesideAPlayingBridgeStaysPaused pins T1(a): the
// browser's own resume of the element after its own pause, the bridge
// playing since, is paused at once, the bar playing nothing and saying why.
func TestTheBrowsersOwnResumeBesideAPlayingBridgeStaysPaused(t *testing.T) { barScenario(t, 3) }

// TestTheBrowsersOwnResumeBesideASilentBridgeGoesThroughTheGate pins T1(b):
// beside a silent bridge the browser's own resume plays again through a
// barrier and one play of the bar's, and the intent it sets carries the
// album on at the track's end.
func TestTheBrowsersOwnResumeBesideASilentBridgeGoesThroughTheGate(t *testing.T) { barScenario(t, 2) }

// TestAPlayMarkLivesOnlyUntilItsEventOrItsSettlement pins T1(c) and W1: a
// play the browser refuses, and one on an element already playing, leave no
// mark a later play of the browser's own could take.
func TestAPlayMarkLivesOnlyUntilItsEventOrItsSettlement(t *testing.T) { barScenario(t, 4) }

// TestTheBrowsersOwnResumeWaitingOnItsGateEndsForTheReader pins T1(d) and
// W4: the browser's own resume waiting on its barrier is shown starting, the
// reader's Play/Pause cancels it, and a queue replaced meanwhile plays in
// its place.
func TestTheBrowsersOwnResumeWaitingOnItsGateEndsForTheReader(t *testing.T) { barScenario(t, 4) }

// TestAnAdmittedResumeOfTheBrowsersOwnGoesOnAtTheTracksEnd pins W3: the
// protective pause cancels nothing, so with no player offered an admitted
// resume plays on to the next track at the track's end.
func TestAnAdmittedResumeOfTheBrowsersOwnGoesOnAtTheTracksEnd(t *testing.T) { barScenario(t, 2) }

// TestTheBrowsersOwnResumeNeverTakesAReadersPlace pins W5: the browser's own
// resume while the reader's row waits on its barrier takes nothing of it, and
// the row plays on the barrier's done.
func TestTheBrowsersOwnResumeNeverTakesAReadersPlace(t *testing.T) { barScenario(t, 2) }

// TestARejectedPlayEndsTheIntentToPlay pins W8: the retry's play and a
// repeat-one replay the browser refuses each leave the element paused, say
// so, and end the intent, so nothing starts after them.
func TestARejectedPlayEndsTheIntentToPlay(t *testing.T) { barScenario(t, 4) }

// TestADeferredStartWithNoPermitAsksTheGateThatHoldsWhenItRuns pins W9: a
// resume let go on where no player was offered, its play deferred to the
// metadata, asks the gate as it is when the metadata comes, and a player
// offered since that plays refuses it.
func TestADeferredStartWithNoPermitAsksTheGateThatHoldsWhenItRuns(t *testing.T) { barScenario(t, 3) }

// TestADeferredStartWithNoPermitPlaysWhileNoPlayerIsOffered is W9's other
// side: with still no player offered the deferred play plays, from its
// position.
func TestADeferredStartWithNoPermitPlaysWhileNoPlayerIsOffered(t *testing.T) { barScenario(t, 2) }

// TestALoadClearsTheProtectivePausesCount pins that a load landing between
// the protective pause of the browser's own play and that pause's event
// takes the pause's count with it, so the browser's next pause still ends
// the intent and the next removal starts nothing.
func TestALoadClearsTheProtectivePausesCount(t *testing.T) { barScenario(t, 2) }

// TestALoadBesideASoundingBridgeLeavesTheBrowserSilent guards the same
// sequence beside the bridge's player playing: the gate refuses the
// browser's resume at once, so the browser stays silent, the bar saying why.
func TestALoadBesideASoundingBridgeLeavesTheBrowserSilent(t *testing.T) { barScenario(t, 2) }

// TestAnUnwiredGateRefusesTheBrowsersOwnPlay pins that with no gate wired (a
// boot that threw after the restore set a source) the browser's own play is
// paused and refused, as a refused start is.
func TestAnUnwiredGateRefusesTheBrowsersOwnPlay(t *testing.T) { barScenario(t, 2) }

// TestAnUnwiredGateRefusesEveryStart pins that with no gate wired every start
// is refused, the lock screen's Play and Next included.
func TestAnUnwiredGateRefusesEveryStart(t *testing.T) { barScenario(t, 4) }

// TestAContinuationBehindABarrierLeavesAReplacedQueueAlone pins that a queue
// the reader replaced while a skip waits on its barrier takes the skip's
// place behind it and plays from its start on the barrier's done, the skip
// moving nothing of it. The skip's own look at the generation when it runs
// is TestAContinuationBehindABarrierLeavesATakenQueueAlone's.
func TestAContinuationBehindABarrierLeavesAReplacedQueueAlone(t *testing.T) { barScenario(t, 3) }

// The bar's follow-ups to PR 2 (B1005): a resume's wait the reader cancels,
// a track that fails, a deferred start, a handover's gone entry and the
// output menu's focus.

// TestAResumeWaitTheReaderCancelsStopsItsSpinner pins that a row clicked or
// Next pressed while the browser's own resume waits on its barrier, refused
// beside the bridge playing, ends that resume and its spinner, and the bar
// is told.
func TestAResumeWaitTheReaderCancelsStopsItsSpinner(t *testing.T) { barScenario(t, 4) }

// TestRemovingARowThatIsNotPlayingCancelsNoResume pins which calls cancel
// the browser's own resume: removing a row that is not playing does not,
// nor do an append, a seek, the level, shuffle or repeat.
func TestRemovingARowThatIsNotPlayingCancelsNoResume(t *testing.T) { barScenario(t, 2) }

// TestRemovingTheOnlyRowLeavesNoTrackAndNothingPlaying pins that removing
// the row that plays, with no playable track after it, leaves the queue
// pointing at no track and the element stopped.
func TestRemovingTheOnlyRowLeavesNoTrackAndNothingPlaying(t *testing.T) { barScenario(t, 2) }

// TestAnAlbumSkipsPastATrackTheBrowserCannotPlay pins the skip in the
// browser's own order: a source refused before its metadata, the bar's play
// still pending and refused with NotSupportedError, is skipped through a
// barrier of its own, the bar showing it stopped meanwhile.
func TestAnAlbumSkipsPastATrackTheBrowserCannotPlay(t *testing.T) { barScenario(t, 2) }

// TestTheStopAfterThreeUnplayableTracksStartsNothingMore pins that the stop
// after three unplayable tracks in a row ends what this browser meant to
// play, so removing its row starts nothing.
func TestTheStopAfterThreeUnplayableTracksStartsNothingMore(t *testing.T) { barScenario(t, 2) }

// TestASkipUnderRepeatOneMovesPastTheFailedTrack pins that the skip past a
// source that cannot play moves on under repeat one, where a track that
// plays still repeats at its end.
func TestASkipUnderRepeatOneMovesPastTheFailedTrack(t *testing.T) { barScenario(t, 3) }

// TestAPlayingHandoverWhoseSourceFailsBeforeItsMetadataSkips pins that a
// handover taken to play at a position, whose source fails before the
// metadata its play waits for, skips to the next track.
func TestAPlayingHandoverWhoseSourceFailsBeforeItsMetadataSkips(t *testing.T) { barScenario(t, 2) }

// TestAHandoverTakenStoppedWhoseSourceFailsStartsNothing is its other side:
// a handover taken stopped skips nothing and plays nothing.
func TestAHandoverTakenStoppedWhoseSourceFailsStartsNothing(t *testing.T) { barScenario(t, 2) }

// TestPlayOverAFailedTrackStartsAgain pins that the bar's Play over a track
// whose source cannot play is a start: it takes the skip's place behind the
// barrier and plays the next playable track on its done.
func TestPlayOverAFailedTrackStartsAgain(t *testing.T) { barScenario(t, 4) }

// TestPlayOverATrackThatFailedForGoodReloadsIt pins Play over a track whose
// network error came back after its retry: a start that reloads the track,
// its automatic retry there again.
func TestPlayOverATrackThatFailedForGoodReloadsIt(t *testing.T) { barScenario(t, 4) }

// TestPlayOverAFailedLastTrackStopsAtTheEnd pins Play over the last track,
// failed, with repeat off: it stops at the queue's end and plays nothing.
func TestPlayOverAFailedLastTrackStopsAtTheEnd(t *testing.T) { barScenario(t, 1) }

// TestTheLockScreensPauseOverAFailedTrackStaysAPause pins that the
// MediaSession's Pause is a pause, never Play/Pause: over a failed track
// whose skip waits on its barrier, it plays nothing.
func TestTheLockScreensPauseOverAFailedTrackStaysAPause(t *testing.T) { barScenario(t, 3) }

// TestAPlayBeforeAHandoversMetadataIsItsOnlyStart pins one start per
// playback: the reader's Play before a positioned handover's metadata plays,
// and the metadata then seeks and starts nothing more.
func TestAPlayBeforeAHandoversMetadataIsItsOnlyStart(t *testing.T) { barScenario(t, 2) }

// TestAPauseBeforeAHandoversMetadataCancelsItsDeferredStart pins that the
// reader's Play and then Pause before a handover's metadata leave its
// deferred start, with its permit, playing nothing.
func TestAPauseBeforeAHandoversMetadataCancelsItsDeferredStart(t *testing.T) { barScenario(t, 3) }

// TestAHandoverWhoseSelectedEntryIsGoneStartsTheNextKeptEntry pins that a
// handover whose selected entry has no track selects the next kept entry and
// plays it from its start.
func TestAHandoverWhoseSelectedEntryIsGoneStartsTheNextKeptEntry(t *testing.T) { barScenario(t, 2) }

// TestAHandoverWhoseSelectedLastEntryIsGoneIsTakenStopped pins that one with
// no kept entry after the gone one is taken stopped at its first kept entry,
// from its start.
func TestAHandoverWhoseSelectedLastEntryIsGoneIsTakenStopped(t *testing.T) { barScenario(t, 2) }

// TestATrackThatFailsForGoodPlaysNothingWhenItsBarrierAnswers pins that a
// failure past the retry ends what this browser meant to play, so the
// retry's barrier, answering done later, plays nothing over it.
func TestATrackThatFailsForGoodPlaysNothingWhenItsBarrierAnswers(t *testing.T) { barScenario(t, 3) }

// TestAContinuationBehindABarrierLeavesATakenQueueAlone pins a continuation's
// look at the generation when it runs: a queue output.js hands this browser while a skip waits on
// its barrier, which the skip's let-go then finds is no longer its playback.
func TestAContinuationBehindABarrierLeavesATakenQueueAlone(t *testing.T) { barScenario(t, 3) }

// TestAnUnwiredGateLetsNoTrackGoOnByItself pins continueStart with no gate
// wired: a track's end goes on to no next track.
func TestAnUnwiredGateLetsNoTrackGoOnByItself(t *testing.T) { barScenario(t, 3) }

// TestTheOldestMarkGoesToTheFirstPlayEventAndNothingUnaskedSounds pins the
// marks' order: the browser's own play queued before the reader's Pause and
// Play takes the reader's mark, the reader's own event is then paused and
// asked of the gate, and nothing sounds that the gate did not let.
func TestTheOldestMarkGoesToTheFirstPlayEventAndNothingUnaskedSounds(t *testing.T) {
	barScenario(t, 2)
}

// TestTheHarnessElementSettlesWhatALoadDrops pins the harness's element: a
// load settles the promises of the tasks it removes, a play's resolved and
// those a pause took refused with AbortError, as a browser's does.
func TestTheHarnessElementSettlesWhatALoadDrops(t *testing.T) { barScenario(t, 2) }

// TestTheOutputMenuClosesAsTheFocusLeavesIt pins the output menu's focus:
// moving between its picks leaves it open, the focus leaving it for anything
// else (the Output button included) closes it, and a click on the Output
// button while it is open closes it once, the press keeping the focus.
func TestTheOutputMenuClosesAsTheFocusLeavesIt(t *testing.T) { barScenario(t, 5) }

// TestTheOutputButtonClaimsNoPopup pins that the Output button carries no
// aria-haspopup: its menu is a group of buttons, no menu role.
func TestTheOutputButtonClaimsNoPopup(t *testing.T) { barScenario(t, 2) }

// An explicit Pause, a handover's wait, the failure streak and an abort
// (B1005's second round).

// TestAnExplicitPauseCancelsAStartWaitingForTheEpoch pins that the lock
// screen's Pause lets go of a row's start waiting for the epoch: the first
// frame then sends no barrier and plays nothing, the selection the browser
// held stays, no wait is left to time out, and a fresh Play still plays.
func TestAnExplicitPauseCancelsAStartWaitingForTheEpoch(t *testing.T) { barScenario(t, 6) }

// TestAnExplicitPauseCancelsAStartWaitingOnItsBarrier pins that the lock
// screen's Pause and the bar's Pause each let go of a row's start waiting on
// its barrier: the barrier's answer plays nothing, the selection stays, and a
// fresh Play plays what the browser held.
func TestAnExplicitPauseCancelsAStartWaitingOnItsBarrier(t *testing.T) { barScenario(t, 6) }

// TestAnExplicitPauseCancelsAPlayThatTookASkipsPlace pins that the lock
// screen's Pause lets go of a Play over a failed track that took its skip's
// place behind the barrier, and a fresh Play then moves on.
func TestAnExplicitPauseCancelsAPlayThatTookASkipsPlace(t *testing.T) { barScenario(t, 3) }

// TestAPauseDuringAHandoversWaitStopsItsSpinner pins that the lock screen's
// Pause over a positioned handover waiting for its metadata shows it stopped
// and not starting at once, and that its metadata then plays nothing.
func TestAPauseDuringAHandoversWaitStopsItsSpinner(t *testing.T) { barScenario(t, 3) }

// TestAReaderPlaySupersedesAHandoversDeferredStart pins that the reader's
// Play during a positioned handover's wait supersedes the play its metadata
// defers: the metadata, first, seeks to the position and plays nothing, and
// the Play plays once, there, when the gate lets it. The answer-first order
// is TestAPlayBeforeAHandoversMetadataIsItsOnlyStart's.
func TestAReaderPlaySupersedesAHandoversDeferredStart(t *testing.T) { barScenario(t, 2) }

// TestARefusedReaderPlayDuringAHandoversWaitShowsItStopped pins that the same
// Play, its barrier refused, leaves the seek, plays nothing, and shows the
// handover stopped and not starting.
func TestARefusedReaderPlayDuringAHandoversWaitShowsItStopped(t *testing.T) { barScenario(t, 1) }

// TestATrackThatPlaysToItsEndBreaksTheFailureStreak pins that a track that
// played to its end breaks a streak of unplayable ones: an album of bad and
// good tracks in turn reaches its sixth track, no stop said.
func TestATrackThatPlaysToItsEndBreaksTheFailureStreak(t *testing.T) { barScenario(t, 1) }

// TestThreeFailuresInARowStopOnTheThirdThoughEachHadItsPlayEvent pins that a
// play event breaks no streak: three unplayable tracks in a row, each with
// its play event before its error, stop on the third.
func TestThreeFailuresInARowStopOnTheThirdThoughEachHadItsPlayEvent(t *testing.T) {
	barScenario(t, 2)
}

// TestAFailureAfterARepeatOneReplayStartsANewStreak pins the streak under
// repeat one: after two failures the third track plays to its end, and its
// replay's failure schedules a skip as the first of a new streak.
func TestAFailureAfterARepeatOneReplayStartsANewStreak(t *testing.T) { barScenario(t, 3) }

// TestTheBrowsersOwnPlayTakesTheSlotAPauseLetGo pins that a start the lock
// screen's Pause let go of holds no place behind its barrier: the browser's
// own play then takes it and resumes what the browser held on its answer.
func TestTheBrowsersOwnPlayTakesTheSlotAPauseLetGo(t *testing.T) { barScenario(t, 2) }

// TestABarrierWhoseStartAPauseLetGoEndsQuietly pins that a barrier whose
// start the lock screen's Pause let go of, lost at 30 s and read back done,
// brings the bar back to this browser's phase with no line of a start, plays
// nothing, and a Play after it still plays.
func TestABarrierWhoseStartAPauseLetGoEndsQuietly(t *testing.T) { barScenario(t, 2) }

// TestAPlayAfterAnExplicitPauseOutlivesThatPausesEvent pins that the cancel
// is the explicit Pause's, never the element's pause event: a fresh Play made
// before that event takes the barrier in flight and plays on its answer, its
// intent going on at the track's end.
func TestAPlayAfterAnExplicitPauseOutlivesThatPausesEvent(t *testing.T) { barScenario(t, 3) }
