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
// bridge's player and reading its way, a routed one reading why, and in this
// browser a DSD row the bridge's player plays saying so (B1021).
func TestRowsFollowTheChosenOutput(t *testing.T) { barScenario(t, 4) }

// TestADSDRowSaysWhereItPlays pins B1021's item 4: in this browser a DSD row
// says it cannot play here and, by its catalog way, that the bridge's player
// plays it (its rate natively, as DoP, or converted to PCM, never called
// bit-exact); a row the bridge's player refuses, or one with no bridge field,
// keeps the browser's words alone; the bridge's player's chips are as they
// were; and the album page draws each, in either output.
func TestADSDRowSaysWhereItPlays(t *testing.T) { barScenario(t, 10) }

// TestTheAlbumsCountLineSaysHowManyPlayWhere pins the album's count line
// (B1021): in this browser it says how many rows cannot play here and how
// many of those the bridge's player plays, pointing to Output, whose pick
// says whether it can be chosen now (free and held alike), and offers
// Download in words the page shows only where it shows the link; with the
// bridge's player chosen it is as it was.
func TestTheAlbumsCountLineSaysHowManyPlayWhere(t *testing.T) { barScenario(t, 12) }

// TestAnAlbumNoRowOfWhichPlaysHereOffersNoPlayShuffleOrAdd pins B1021's item
// 6: where no row of an album plays on the output chosen, its Play, Shuffle
// and Add to queue are disabled, each naming the count line that says why,
// and a disabled Play sends no barrier; a row the queue plays (a fresh FLAC
// copy of a DSD row included) makes them live, Play then asking the barrier
// as every start does; and with the bridge's player chosen Shuffle stays off.
func TestAnAlbumNoRowOfWhichPlaysHereOffersNoPlayShuffleOrAdd(t *testing.T) {
	barScenario(t, 14)
}

// TestAnOutputSwitchRedrawsAnAlbumsRowsLineAndControls pins that a switch of
// the output, which redraws the route as boot.js wires it, gives an open
// album the rows, the count line and the controls of the output picked.
func TestAnOutputSwitchRedrawsAnAlbumsRowsLineAndControls(t *testing.T) { barScenario(t, 5) }

// TestTheBarCarriesItsSeekAndBothTimesAndSaysHowTallItIs pins that the bar
// keeps the seek and both times in its scrub row, which a phone now shows
// (B1021's item 1), and hands the height it renders at to what sits above it
// (main's bottom padding, the output menu, the queue panel, the A–Z rail).
func TestTheBarCarriesItsSeekAndBothTimesAndSaysHowTallItIs(t *testing.T) { barScenario(t, 4) }

// TestTheSeekLandsOnlyAtItsChange pins the seek where it is shown, wider than
// 768px, as it always was: in this browser an input and the pointer's lift
// seek nothing and the change lands one seek where the value was let go, and
// with the bridge's player chosen the same, its seek a command (B1021 keeps
// that seek as it was and hides it on a phone, since WebKit fires no change
// after a touch).
func TestTheSeekLandsOnlyAtItsChange(t *testing.T) { barScenario(t, 5) }

// TestTheRailFollowsTheTrackPastAPreview pins the 2px rail, a phone's
// progress where the seek is hidden, to the track's position at every tick
// in either output, whatever preview the seek still holds: an input with no
// change after it (a drag in a wider window that a resize to a phone's width
// cut short) keeps the seek's thumb, and the rail painted with the thumb
// stood still with it.
func TestTheRailFollowsTheTrackPastAPreview(t *testing.T) { barScenario(t, 3) }

// TestASeekLandsWhereItIsLetGoOnce pins that the bar's seek lands once, where
// its value was let go: a drag under way seeks nothing, the lift of the
// pointer that began it lands it, since WebKit fires no change after a touch
// moves the value (B1021), and the change after it nothing more; another
// pointer's lift or cancel lands nothing, nor does a change while the pointer
// is down, and the pointer's cancel puts the thumb back where the track is; a
// key's change lands its value with no pointer down.
func TestASeekLandsWhereItIsLetGoOnce(t *testing.T) { barScenario(t, 9) }

// TestADragLandsOnlyOnTheBridgesOccurrenceItWasMadeOn pins that a drag of the
// bar's seek with the bridge's player chosen lands only on the occurrence it
// was made on, while the player takes seeks: one whose seek the player
// stopped taking meanwhile seeks nothing at its lift or its change, even with
// the seek given back before the lift, and a fresh drag then lands; the same
// entry heard again (a new occurrence) ends the drag and a queue edit on the
// occurrence keeps it; another track, or a DSD one whose seek is off,
// published while the finger is down, seeks nothing.
func TestADragLandsOnlyOnTheBridgesOccurrenceItWasMadeOn(t *testing.T) { barScenario(t, 7) }

// TestAnAlbumAsksThisBrowserWhatTheQueuePlays pins that a row's playability in
// this browser is the queue's own (B1021): a universal row whose type this
// browser's decoder does not take, with no copy it does, is unplayable, its
// album's Play, Shuffle and Add to queue disabled with the count line and the
// row's chip saying why, and its Play sends no barrier; a row it decodes keeps
// them live; and a row with no play field is unplayable with its reason an
// element the page renders where its title points.
func TestAnAlbumAsksThisBrowserWhatTheQueuePlays(t *testing.T) { barScenario(t, 8) }

// TestADragInThisBrowserLandsOnlyOnTheTrackItWasMadeOn pins the same in this
// browser: another track loaded while the finger is down, or the output moved
// to the bridge's player, seeks nothing at the lift or the change after it.
func TestADragInThisBrowserLandsOnlyOnTheTrackItWasMadeOn(t *testing.T) { barScenario(t, 3) }

// TestASecondPointerNeitherTakesNorEndsTheFirstsDrag pins the drag's owner
// (B1021): a second pointer, primary or not, lands nothing at its lift while
// the first drags, and its move of the range is never what lands; the
// first's lift lands its own value, once, its last where it moved again
// after the other.
func TestASecondPointerNeitherTakesNorEndsTheFirstsDrag(t *testing.T) { barScenario(t, 4) }

// TestADragOnTheBridgesPlayerEndsWithItsRun pins the engine's run in a
// drag's identity: a drag made before the engine restarted under one serve,
// only its generation's run changed and its entry and occurrence restored as
// they were, seeks nothing at its lift or the change after it, while a queue
// edit within one run keeps the drag.
func TestADragOnTheBridgesPlayerEndsWithItsRun(t *testing.T) { barScenario(t, 3) }

// TestADragInThisBrowserEndsWithItsPlay pins the play in this browser's: a
// position update on the play keeps the drag, and the same entry played
// again under repeat all, or replayed under repeat one with no load, seeks
// nothing at the lift; a drag on the replay lands.
func TestADragInThisBrowserEndsWithItsPlay(t *testing.T) { barScenario(t, 4) }

// TestAnAlbumRedrawsWhenTheOutputFallsBackToThisBrowser pins that the route
// redraws on the effective output: GET now answering that no player is wired,
// with the bridge picked, makes this browser the output, and the album
// redraws once, its FLAC row and its Play, Shuffle and Add to queue live.
func TestAnAlbumRedrawsWhenTheOutputFallsBackToThisBrowser(t *testing.T) { barScenario(t, 4) }

// TestAConfirmedDecodeFailureDisablesTheAlbumsHopelessPlay pins that the
// open album follows a confirmed decode failure, by the row's path, with
// rows parsed afresh per request: the row reads why, its album's controls are
// disabled and their Play sends no barrier while the skip is held; the skip,
// let go, goes on as it did.
func TestAConfirmedDecodeFailureDisablesTheAlbumsHopelessPlay(t *testing.T) { barScenario(t, 6) }

// TestATapOnTheRailJumpsOnlyFromTheLetterItBeganOn pins the rail by touch: a
// tap on a live letter asks for its bucket once, one on an empty letter asks
// for nothing whatever live letter its click is handed, and a key's or a
// mouse's click right after still asks once, as does a click no touch on
// the rail began.
func TestATapOnTheRailJumpsOnlyFromTheLetterItBeganOn(t *testing.T) { barScenario(t, 4) }

// TestAPointerThatBeganNoDragNeverSeeks pins the seek's refusal of a pointer
// that went down during another's drag (B1021): in either order of the two
// pointers' moves the owner's lift lands its own last value once, and after
// it the other pointer's moves, lift and change, across a change of track,
// seek nothing and leave the thumb at the track's time; one touch with no
// change after its lift, a key's step and an assistive adjustment each land
// once.
func TestAPointerThatBeganNoDragNeverSeeks(t *testing.T) { barScenario(t, 7) }

// TestADragEndsAtAReplayItsRenderHasNotSeen pins that a repeat-one replay is
// a new play at the lift itself: a drag held across it and let go before any
// render of it lands nothing, the thumb put back, and one begun after it,
// before its first render, lands once.
func TestADragEndsAtAReplayItsRenderHasNotSeen(t *testing.T) { barScenario(t, 2) }

// TestADragEndsAtTheRetryAndOutlivesAPrimesCancel pins the automatic retry as
// a new play (a drag held across it lands nothing) and the preloader's cancel
// as none (a drag across it lands once).
func TestADragEndsAtTheRetryAndOutlivesAPrimesCancel(t *testing.T) { barScenario(t, 2) }

// TestADecodeFailureClosesTheAlbumThatStartedIt pins a confirmed decode
// failure, after a retry, on the album page that started the play: it
// redraws once, its row and count line say why and its controls are
// disabled, its Play sends no barrier while the skip is held, no snapshot
// after redraws it again, the stored queue keeps the catalog's play, and the
// skip goes on as it did, the source refused on that page to the end; and a
// reload over the stored queue, a new page, restores the row with the same
// source, which its Play asks the barrier for and plays again.
func TestADecodeFailureClosesTheAlbumThatStartedIt(t *testing.T) { barScenario(t, 10) }

// TestAFailedVariantIsThatSourceAlone pins the failure's key, a row's path and
// its variant: a DSD row whose browser variant A failed has its album's Play
// disabled, and a fresh answer naming variant B enables it and plays B.
func TestAFailedVariantIsThatSourceAlone(t *testing.T) { barScenario(t, 6) }

// TestTheRailTakesEveryPointerDownForItsClick pins the rail's record of where
// a pointer went down, made at every pointerdown in the document: a click a
// touch outside the rail is handed jumps after an empty tap that made no
// click, and a key's click beside a held touch jumps while that touch's
// handed click does not.
func TestTheRailTakesEveryPointerDownForItsClick(t *testing.T) { barScenario(t, 2) }

// TestAPreviewNoGestureHoldsGivesWayAtTheNextTick pins that no gesture leaves
// the seek's preview standing: an input after a lift, with no pointer down
// and no change after it, gives the thumb back to the track at the bar's next
// tick.
func TestAPreviewNoGestureHoldsGivesWayAtTheNextTick(t *testing.T) { barScenario(t, 1) }

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
// barrier, the skip let go of, and on its done the reader's start runs, the
// skip never, and plays the next playable track; the gates are watched from
// before the skip asks.
func TestPlayOverAFailedTrackStartsAgain(t *testing.T) { barScenario(t, 4) }

// TestPlayOverATrackThatFailedForGoodReloadsIt pins Play over a track whose
// network error (code 2) came back after its retry: a start that reloads the
// track, its automatic retry there again.
func TestPlayOverATrackThatFailedForGoodReloadsIt(t *testing.T) { barScenario(t, 4) }

// TestPlayOverATrackThatFailedToDecodeForGoodReloadsIt is the same for a
// decode error (code 3), each code's reload pinned by a case of its own.
func TestPlayOverATrackThatFailedToDecodeForGoodReloadsIt(t *testing.T) { barScenario(t, 4) }

// TestPlayOverAFailedLastTrackStopsAtTheEnd pins Play over the last track,
// failed, with repeat off: before its skip's timer fires, it stops at the
// queue's end through a barrier and plays nothing, and the skip then does
// nothing more.
func TestPlayOverAFailedLastTrackStopsAtTheEnd(t *testing.T) { barScenario(t, 3) }

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

// TestAfterAnAbortTheButtonDoesWhatItShows pins that after a fetch the user
// aborted (code 1), which fires no pause, the bar's Pause, still shown,
// pauses and asks the gate nothing, and its Play, shown once stopped,
// starts the track it showed through the gate.
func TestAfterAnAbortTheButtonDoesWhatItShows(t *testing.T) { barScenario(t, 4) }

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

// A resume a pause overtook, the bar's Pause shown at once, and a Stop.

// TestTheBrowsersOwnResumeAPauseOvertookPlaysNothingOnItsBarriersDone pins
// that the browser's own resume of a paused track, its play event queued
// before the lock screen's Pause while a row's start waits on its barrier,
// asks nothing when that event comes: the barrier's done, beside a silent
// bridge, plays neither that track nor the row the Pause let go of.
func TestTheBrowsersOwnResumeAPauseOvertookPlaysNothingOnItsBarriersDone(t *testing.T) {
	barScenario(t, 4)
}

// TestTheBrowsersOwnResumeAPauseOvertookPlaysNothingOnANoPlayerRefusal pins
// the same with the barrier refused as no player offered, a refusal that
// lets a start it holds go on in this browser.
func TestTheBrowsersOwnResumeAPauseOvertookPlaysNothingOnANoPlayerRefusal(t *testing.T) {
	barScenario(t, 5)
}

// TestTheBrowsersOwnResumeAPauseOvertookAsksNothingWhereNoPlayerIsOffered
// pins it where the gate already knows no player is offered and would let it
// go on at once, and that a resume of the browser's own made after the Pause
// still plays.
func TestTheBrowsersOwnResumeAPauseOvertookAsksNothingWhereNoPlayerIsOffered(t *testing.T) {
	barScenario(t, 3)
}

// TestAfterAnAbortThePausePressedShowsPlayAtOnce pins that after a fetch the
// user aborted (code 1), the bar's Pause pressed shows Play at once, before
// the element's pause event, and that Play pressed before that event asks the
// gate once, plays the track the bar showed, and its intent outlives that
// event: the track's end plays the next track.
func TestAfterAnAbortThePausePressedShowsPlayAtOnce(t *testing.T) { barScenario(t, 4) }

// TestAStopLetsGoOfAStartWaitingForTheEpoch pins that the lock screen's Stop
// lets go of a row's start waiting for the epoch: the first frame sends no
// barrier and plays nothing, the queue stays empty, no wait is left to time
// out, Play on the cleared queue does nothing, and a row played after it
// plays.
func TestAStopLetsGoOfAStartWaitingForTheEpoch(t *testing.T) { barScenario(t, 6) }

// TestAStopLetsGoOfAStartWaitingOnItsBarrier pins that the lock screen's Stop
// lets go of a row's start waiting on its barrier: the barrier's answer plays
// nothing, the queue stays empty, Play on it does nothing, and a row played
// after it plays.
func TestAStopLetsGoOfAStartWaitingOnItsBarrier(t *testing.T) { barScenario(t, 4) }

// A reader's action while a switch to this browser is outstanding, the
// switch to the bridge refused, and a resume's wait (B1015). Each switch is
// sent from an unconfirmed bar with this browser chosen, the console keeping
// each tab's last record alone, as serve's control does; a row is the
// library album page's own (views.js's renderAlbum and its rows' buttons).

// TestAStopWhileASwitchToThisBrowserIsOutstandingTakesNothing pins that the
// lock screen's Stop while the switch is outstanding stands: its answer, done
// beside a silent bridge, takes nothing and plays nothing, the queue empty at
// no track and this browser the output in its own phase.
func TestAStopWhileASwitchToThisBrowserIsOutstandingTakesNothing(t *testing.T) { barScenario(t, 6) }

// TestAPauseWhileASwitchToThisBrowserIsOutstandingTakesNothing pins the same
// for the lock screen's Pause: this browser's own queue stays paused on its
// track, and the bar's Play then plays it through a barrier of its own.
func TestAPauseWhileASwitchToThisBrowserIsOutstandingTakesNothing(t *testing.T) { barScenario(t, 7) }

// TestARowWhileASwitchToThisBrowserIsOutstandingPlaysAndTakesNothing pins
// that a row of the library played while the switch is outstanding plays
// through a barrier of its own and stays: the switch's answer, the console
// having run it first, takes nothing and plays nothing.
func TestARowWhileASwitchToThisBrowserIsOutstandingPlaysAndTakesNothing(t *testing.T) {
	barScenario(t, 8)
}

// TestTheSwitchsLateAnswerLeavesThePhaseARowsLostBarrierSet pins that the
// switch's answer after a row whose barrier was lost takes nothing and leaves
// the bar unconfirmed with its line, as that loss left it.
func TestTheSwitchsLateAnswerLeavesThePhaseARowsLostBarrierSet(t *testing.T) { barScenario(t, 8) }

// TestTheSwitchsAnswerAfterTheBridgePlaysAgainSaysSoAndTakesNothing pins that
// with a state showing the bridge playing before the answer, after the
// reader's Pause, the answer takes nothing and the bar says the bridge plays
// again, where it said so before this change.
func TestTheSwitchsAnswerAfterTheBridgePlaysAgainSaysSoAndTakesNothing(t *testing.T) {
	barScenario(t, 6)
}

// TestAPausesEventAfterThePickOfThisBrowserLetsItsHandoverPlay pins that a
// pause event is no action of the reader's: the lock screen's Pause, then the
// pick of this browser before the Pause's own event, then that event and the
// answer with silence, and the handover is taken and plays once.
func TestAPausesEventAfterThePickOfThisBrowserLetsItsHandoverPlay(t *testing.T) { barScenario(t, 5) }

// TestASeekOrAnAddWhileASwitchToThisBrowserIsOutstandingLeavesItsHandoverToPlay
// pins what is no action of the reader's for the switch: an album's Add to
// queue, a seek, the level, shuffle and repeat, after which the answer takes
// the bridge's queue and plays it, as before this change.
func TestASeekOrAnAddWhileASwitchToThisBrowserIsOutstandingLeavesItsHandoverToPlay(t *testing.T) {
	barScenario(t, 7)
}

// TestAStopBeforeTheSwitchsLostAnswerIsReadBackTakesAndStartsNothing pins the
// switch whose answer was lost: its record read back done after the lock
// screen's Stop takes nothing and asks no barrier for its handover.
func TestAStopBeforeTheSwitchsLostAnswerIsReadBackTakesAndStartsNothing(t *testing.T) {
	barScenario(t, 8)
}

// TestAStopBeforeAReloadIsNotSeenAndTheMoveComesBackPaused pins what this
// change leaves as it was: the reader's generation is the document's own, so
// a reload whose document reads the switch back takes the bridge's queue back
// paused over the Stop made before it, playing nothing and saying to press
// Play.
func TestAStopBeforeAReloadIsNotSeenAndTheMoveComesBackPaused(t *testing.T) { barScenario(t, 7) }

// TestARowPlayedAfterAReloadStaysWhenTheMoveIsReadBack pins that a row the
// reader plays in the reloaded document, while its read back of the earlier
// document's switch is held, stays when that record comes: nothing taken.
func TestARowPlayedAfterAReloadStaysWhenTheMoveIsReadBack(t *testing.T) { barScenario(t, 9) }

// TestAHandoverPlaysAndGoesOnWithNoReaderActionBetween pins the switch with
// no action of the reader's between its sending and its answer: the handover
// plays once, from its position, and goes on to its next track on the
// switch's permit, the take's own count of an action voiding none of it.
func TestAHandoverPlaysAndGoesOnWithNoReaderActionBetween(t *testing.T) { barScenario(t, 3) }

// TestANextWhileTheSwitchToTheBridgeIsRefusedForNoPlayerStands pins that the
// bar's Next, which drives this browser once GET now says no player is
// offered, stands when the switch to the bridge is then refused for no
// player: the browser's old track is not resumed over it.
func TestANextWhileTheSwitchToTheBridgeIsRefusedForNoPlayerStands(t *testing.T) { barScenario(t, 5) }

// TestANextWhileTheSwitchToTheBridgeIsRefusedWithNothingChangedStands pins
// the same for the switch refused with nothing changed.
func TestANextWhileTheSwitchToTheBridgeIsRefusedWithNothingChangedStands(t *testing.T) {
	barScenario(t, 6)
}

// TestCancellingAResumeWaitLeavesNothingForItsRetryToPlay pins that the bar's
// press over a resume of the browser's own waiting on its barrier ends what
// this browser meant to play: a network error after it reloads the track and
// plays nothing, and the barrier's answer plays nothing either.
func TestCancellingAResumeWaitLeavesNothingForItsRetryToPlay(t *testing.T) { barScenario(t, 3) }

// TestAnErrorWhileAResumeWaitsPlaysOnlyOnTheResumesAdmission pins that a
// network error while a resume of the browser's own waits on its barrier
// reloads the track and plays it only on the resume's admission, once, and
// nothing where the barrier refuses it.
func TestAnErrorWhileAResumeWaitsPlaysOnlyOnTheResumesAdmission(t *testing.T) { barScenario(t, 5) }

// What a switch's end settles, and a failure during a resume's wait
// (B1015): a switch to this browser settles only its own moving words and
// its own phase, and a failure past its retry, or a source the browser
// cannot use, ends a resume of the browser's own waiting on its barrier.

// TestAStopAfterARowRetiresTheSwitchsMovingWordsWhenItIsAnswered pins that a
// switch answered after a row's barrier the lock screen's Stop let go of,
// which left this browser's phase, still retires its own moving words: an
// empty line, an empty queue, nothing played, nothing pending.
func TestAStopAfterARowRetiresTheSwitchsMovingWordsWhenItIsAnswered(t *testing.T) {
	barScenario(t, 11)
}

// TestAnInheritedSwitchLeavesTheWarningOfARowLostSinceTheReload pins that a
// switch an earlier document left, read back after a row of the new document
// lost its barrier's answer, leaves that row's unconfirmed and its words, which
// the row's own read back then settles.
func TestAnInheritedSwitchLeavesTheWarningOfARowLostSinceTheReload(t *testing.T) {
	barScenario(t, 10)
}

// TestAnInheritedSwitchLeavesTheWarningOfAnInheritedBarrierStillUnresolved
// pins that an operation another earlier document left, of the switch's pick
// and unresolved, holds the warning too: the switch read back in a third
// document, after that document's lock-screen Stop, which sends nothing,
// leaves that barrier's unconfirmed, it alone pending.
func TestAnInheritedSwitchLeavesTheWarningOfAnInheritedBarrierStillUnresolved(t *testing.T) {
	barScenario(t, 10)
}

// TestAGoneSwitchLeavesTheWarningOfARowLostSinceItWasSent pins the same in one
// document for a switch whose record the console no longer keeps: its
// operation_gone leaves the row's unconfirmed and its words.
func TestAGoneSwitchLeavesTheWarningOfARowLostSinceItWasSent(t *testing.T) { barScenario(t, 9) }

// TestASwitchsReadBackClearsItsOwnWarningBesideAnOlderPicksLostBarrier pins
// that a barrier lost under an older pick holds no warning for the switch: the
// switch's record read back clears its own unconfirmed at once.
func TestASwitchsReadBackClearsItsOwnWarningBesideAnOlderPicksLostBarrier(t *testing.T) {
	barScenario(t, 6)
}

// TestTheSwitchsAnswerKeepsTheGatesWordsAsItSettlesThePhase pins that the
// phase and the line settle apart: a row refused beside the bridge playing
// leaves the gate's words, which the switch's answer keeps as it moves the
// phase to this browser's.
func TestTheSwitchsAnswerKeepsTheGatesWordsAsItSettlesThePhase(t *testing.T) { barScenario(t, 8) }

// TestAFailurePastItsRetryEndsAResumeWait pins that a network error past its
// retry while a resume of the browser's own waits on its barrier ends that
// wait: its admission plays nothing, the failure still shown.
func TestAFailurePastItsRetryEndsAResumeWait(t *testing.T) { barScenario(t, 4) }

// TestAFailurePastItsRetryEndsAResumeWaitAfterAStartWithNoPermit pins the
// same where playback began with no player offered, so with no permit.
func TestAFailurePastItsRetryEndsAResumeWaitAfterAStartWithNoPermit(t *testing.T) {
	barScenario(t, 6)
}

// TestASourceTheBrowserCannotUsePlaysNothingOnTheResumesAdmission pins that a
// resume waiting as a source the browser cannot use fails is admitted only
// to whether its skip goes on: its admission plays nothing of the failed
// track, and the skip goes on through a barrier of its own.
func TestASourceTheBrowserCannotUsePlaysNothingOnTheResumesAdmission(t *testing.T) {
	barScenario(t, 5)
}

// TestASourceTheBrowserCannotUseShowsNoSpinnerForTheResumeItLeaves pins that
// a resume whose source failed before its protective pause's event shows no
// spinner over the failed track.
func TestASourceTheBrowserCannotUseShowsNoSpinnerForTheResumeItLeaves(t *testing.T) {
	barScenario(t, 1)
}

// TestTheBarsPlayOverASourceTheBrowserCannotUseIsAStartThoughAResumeWaits pins
// that the bar's Play over the failed track while such a resume waits is a
// start, never that resume's cancel: it plays the next track on the
// barrier's answer.
func TestTheBarsPlayOverASourceTheBrowserCannotUseIsAStartThoughAResumeWaits(t *testing.T) {
	barScenario(t, 2)
}

// TestAResumesRefusalAfterASourceTheBrowserCannotUseStillEndsItsSkip pins
// what the queue does next, as before: the resume's barrier refused after its
// source failed ends what this browser meant to play, so the skip's timer
// asks no barrier and plays nothing of the next track.
func TestAResumesRefusalAfterASourceTheBrowserCannotUseStillEndsItsSkip(t *testing.T) {
	barScenario(t, 4)
}

// TestAResumesAdmissionAfterASourceTheBrowserCannotUseLetsItsSkipGoOn pins the
// other order: the browser's own pause delivered, then its resume admitted
// after its source failed, means to play again, so the skip's timer plays the
// next track through a barrier of its own, the failed source never played.
func TestAResumesAdmissionAfterASourceTheBrowserCannotUseLetsItsSkipGoOn(t *testing.T) {
	barScenario(t, 3)
}

// TestTheNextTrackAfterASkipStartedWhileAResumeWaitedRetriesItsFirstError pins
// that the resume's wait goes with the track it was for: the skip starts the
// next track while the resume's barrier is held, and that track's first
// network error, after the old barrier's admission, retries once.
func TestTheNextTrackAfterASkipStartedWhileAResumeWaitedRetriesItsFirstError(t *testing.T) {
	barScenario(t, 4)
}

// TestASkipTimerBeforeAResumesAnswerPlaysNothingOfTheNextTrack pins the order
// with the timer first: the skip takes the resume's place behind its barrier,
// which ends what this browser meant to play, so nothing more is asked and
// nothing of the next track plays.
func TestASkipTimerBeforeAResumesAnswerPlaysNothingOfTheNextTrack(t *testing.T) { barScenario(t, 3) }

// TestAResumeHeldAcrossTheStopAfterThreeUnplayableTracksStartsNothingAfterIt
// pins that the stop after three unplayable tracks in a row ends a resume
// waiting across it: its admission, then the failed row's removal, start no
// successor and play nothing more.
func TestAResumeHeldAcrossTheStopAfterThreeUnplayableTracksStartsNothingAfterIt(t *testing.T) {
	barScenario(t, 4)
}
