## 2026-11-24 — B1015: a reader's action during a switch to this browser stands

### What

On main (`10987990`), with this browser chosen and the bar unconfirmed (an operation's answer
lost, or one inherited at load), the lock-screen handlers are installed and the bar drives
`audio.js`. The reader picks **This browser** again, which sends a switch-out that takes the
bridge's queue (`take: true`), and while its answer is outstanding presses Stop (the lock
screen's `stop`, `clearQueue`), Pause, or plays a row of the library. Nothing told the switch:
its answer still passed `mine`, and `take(h, !!h.playing, permit)` put the bridge's queue back
at index 0 and played it; a record read back after a lost answer did the same through
`recoverSwitchOut`, and a document that read an earlier document's switch back replaced a row
the reader had started in it (`finishInherited`, `take(h, false)`). A Stop was undone and the
cleared queue sounded. Two more cases of the same kind: a switch to the bridge refused, or
refused with nothing changed, after GET now had said no player is offered (so the bar's Next
reached `audio.advance`), resumed the browser's old index over the reader's Next; and an error's
retry while a resume of the browser's own waited on its barrier played at once on the old
permit, and after a cancel of that wait played what the reader had cancelled.

The change is `audio.js` and `output.js` alone. No server change, route, protocol field or
server state, no change to `boot.js` or `env.browser`, and none to the gate's design: every
start of this browser still asks the one gate.

### The changes

Production, `audio.js` and `output.js`: 73 lines added and 21 removed (37 of the added code and
36 comment). The commits: `1ae65a01` (the cases, red at main where the change is due),
`1aa02b06` (the change), and this entry with the rule's line and its long form.

- **The reader's generation** (`output.js`, `readerGen`, `readerActed()` on the output object).
  `audio.js`'s `readerAction()` calls `currentOutput()?.readerActed?.()`, so each call of the
  reader's own advances it: `clearQueue`, `pauseNow`, `toggle` (both branches, the cancel of a
  resume's wait among them), `playQueue`, `advance`, removing the playing row, `take` and
  `suspend`. The pause listener's call for an unmarked pause (`readerAction(false)`) keeps what
  it does on main (the intent, the resume's wait, the `actions` count) and advances nothing:
  `pauseNow` advanced it as the Pause was pressed, and its `el.pause()` queues a pause event the
  listener still reads as unmarked, so a Pause and then a quick **This browser** pick would
  otherwise see that event as a second action and drop the handover just asked for. An
  interruption during a switch therefore changes nothing for the switch, as on main.
- **The switch-out's three admissions compare it.** `switchOut` reads `readerGen` after
  `newPick`, before `send`. Where it has moved by the answer (compared before `handoverOf`, so
  the stopped take and the playing one are both covered), or by the read back of a lost answer
  (`recoverSwitchOut(rec, gen)`), this browser becomes the output (`setOutput("browser")`) and
  nothing is taken and nothing started: never `take(h, false)` in place of `take(h, true)`,
  which would overwrite the queue the reader cleared or chose. `leftAlone(own, sounds)` does the
  rest: the phase becomes this browser's only where it is still the switch's own (`toBrowser` at
  the answer; at the read back, the `unconfirmed` the switch's own loss set), so a phase set
  since (a row's barrier lost meanwhile) stays with its line; the line is `WORDS.bridgeAgain`
  exactly where main shows it (the permit failing at the answer, the bridge sounding at the read
  back), otherwise empty; the state frames' rule governs from the next state on. `take`'s own
  advance comes after the comparison, and `permitFor` reads no generation, so the handover's
  deferred play and its continuations keep their permit. An entry a document inherits carries
  the generation as that document boots (`bootIdentity`; nothing stored carries it over), and
  `finishInherited` compares it as the others do, so a row the reader starts in the reloaded
  document stays when the earlier document's switch is read back.
- **The switch-in's two admissions compare it.** `switchIn` reads it after `b.suspend(h)`,
  whose own advance it must not see; where it has moved, the refusal before admission and
  `resolveSwitchIn` with effect none move the output, the phase and the line as on main and skip
  `gateStart(resume)`, so the reader's Next stands.
- **A cancelled resume's wait ends the intent** (`toggle`'s cancel branch sets `intent = false`
  beside `readerAction()`), so an error's retry after it (`const wanted = intent`) plays nothing.
- **An error's retry during a resume's wait leaves the wait to play it** (`onError`): the retry
  reloads the source and returns while `resumeWait` stands, so the resume's admission plays the
  reloaded source once, and a cancel or a refusal plays nothing. On main it started at once, on
  the old permit, beside a bridge whose newer state had not reached the tab.

What is no action of the reader's, and advances nothing, as on main: a seek (the scrubber and
the lock screen's), an album's **Add to queue** (`enqueue`), removing a row that is not playing,
the level, and the shuffle and repeat settings; a handover taken after them replaces the queue
and the position, as before (`TestASeekOrAnAddWhileASwitchToThisBrowserIsOutstandingLeavesItsHandoverToPlay`).

### The cases, their reds and the controls

Harness scenarios in `internal/admin/testdata/bar_harness.mjs`, each with a Go caller in
`player_output_bar_test.go`. The switch is sent from an unconfirmed bar over a console that
keeps its records as serve's control does, each tab's last operation alone
(`cmd/bridge/serve_player_control.go`'s `admit` and `Operation`), so a read back of a sequence a
newer operation superseded answers `operation_gone`, and where a row's barrier is admitted after
the switch, the console has run the switch to done first and held only its HTTP answer
(`theConsole`). A row is the library's own control: the shipped `views.js`'s album page
(`renderAlbum`) rendered over the harness's DOM model, its row's `.track-title` button clicked,
which plays through `playOn` (`libraryRows`); the model's `querySelector` and a new
`querySelectorAll` take a class selector for it. A reload is a fresh copy of the player's
modules over the same session storage, as a new document has them (`reloaded`). Stop, Pause and
Next are the lock screen's handlers or the bar's buttons; no case moves the generation itself.

Red at main and green with the change:

- `TestAStopWhileASwitchToThisBrowserIsOutstandingTakesNothing`: at main 09.flac played after
  the Stop; now nothing is taken or played, the queue empty at index -1.
- `TestAPauseWhileASwitchToThisBrowserIsOutstandingTakesNothing`: at main 09.flac played; now
  this browser's queue stays paused on track 1, and the bar's Play plays it through a barrier.
- `TestARowWhileASwitchToThisBrowserIsOutstandingPlaysAndTakesNothing`: the row plays through
  its own barrier and selects 08.flac; at main the late answer played 09.flac over it.
- `TestTheSwitchsLateAnswerLeavesThePhaseARowsLostBarrierSet`: at main the answer played 09.flac
  and set this browser's phase; now the phase stays unconfirmed with its line.
- `TestTheSwitchsAnswerAfterTheBridgePlaysAgainSaysSoAndTakesNothing`: at main the queue was
  replaced by 09.flac, paused; now nothing is taken and the line is `WORDS.bridgeAgain`.
- `TestAStopBeforeTheSwitchsLostAnswerIsReadBackTakesAndStartsNothing`: at main the read back
  took 09.flac and sent a barrier that played it.
- `TestARowPlayedAfterAReloadStaysWhenTheMoveIsReadBack`: at main the read back replaced the row
  with 09.flac, paused, saying to press Play.
- `TestANextWhileTheSwitchToTheBridgeIsRefusedForNoPlayerStands` and
  `TestANextWhileTheSwitchToTheBridgeIsRefusedWithNothingChangedStands`: at main the refusal
  resumed track 1 over the Next's track 2.
- `TestCancellingAResumeWaitLeavesNothingForItsRetryToPlay`: at main the network error's retry
  after the spinner's press played. Its sequence queues the browser's pause and its play while
  the element plays (`resumeQueued`), so the pause's event finds the element playing and the
  intent stands; main's `browserResumeWaits` delivers the pause's event first, which ends the
  intent, and the case run that way stayed green under its control (measured).
- `TestAnErrorWhileAResumeWaitsPlaysOnlyOnTheResumesAdmission`: at main the retry played at once
  and again on the admission; now nothing until the admission, then once, and nothing at all
  where the barrier refuses.

Green at main and with the change, kept as pins:
`TestAPausesEventAfterThePickOfThisBrowserLetsItsHandoverPlay` (a Pause, the pick before the
Pause's event, the event, then the answer: the handover taken and played once),
`TestASeekOrAnAddWhileASwitchToThisBrowserIsOutstandingLeavesItsHandoverToPlay`,
`TestAStopBeforeAReloadIsNotSeenAndTheMoveComesBackPaused` (the residual below: the queue of one
at index 0, 09.flac, nothing played, paused, the line `WORDS.pressPlay`) and
`TestAHandoverPlaysAndGoesOnWithNoReaderActionBetween` (a playing handover at 30 s with two
tracks plays once from its position and goes on to its second track, no barrier). The undisturbed
switches main pins stay green with their assertions unchanged, among them
`TestTheSwitchToTheBrowserStartsItOnlyOnSilence`, `TestACopiedTabTakesTheMoveStoppedAndStartsNothing`
and `TestAReloadKeepsANewerPicksSupersession`.

The controls, each a mutation of a copy of the committed modules run through every scenario
(the tracked tree untouched), each prediction written before its run, all as predicted:

- the answer's comparison removed: the Stop, Pause, row, lost-row and bridge-again cases red,
  no other;
- `recoverSwitchOut`'s removed: the read-back case alone;
- `finishInherited`'s removed: the reload-and-row case alone;
- the switch-in refusal's removed: its Next case alone; `resolveSwitchIn`'s removed: its own
  alone;
- `intent = false` dropped from the cancel: the cancel case alone;
- the retry's immediate start restored: the error-while-waiting case alone;
- the pause listener's call advancing the generation: the pause-event-after-the-pick pin alone;
- `permitFor` reading the generation: the undisturbed handover red, and main's
  `TestADeferredStartPlaysWhileItsPermitHolds`;
- `take`'s advance moved before the comparison (the answer advancing it first): the undisturbed
  handover, the pause-event and the seek-or-add pins red, and fourteen of main's switch-out cases;
- `enqueue` made a reader's action: the seek-or-add pin red, and main's
  `TestRemovingARowThatIsNotPlayingCancelsNoResume`.

### What this does not cover

- **Across a reload** (graded P2, silent): an operation a new document inherits carries that
  document's generation alone, so a Stop pressed before the reload is not seen: the reloaded
  document takes a done switch-out stopped, as on main, the bridge's queue comes back paused and
  nothing plays. The generation is not persisted.
- **A resume lost after a Pause, and an interruption consuming a later record** (graded P2,
  silent): the pause count (`ownPauses`) can take a Pause's event for a protective one and end a
  genuine resume, leaving the browser silent until another Play.
- **A terminal error leaving a resume to try a failed source** (graded P2): the second error ends
  the intent, and the wait's admission still plays the failed source.
- **The spinner's control is labelled Play** (graded P2): a reader who presses that Play cancels
  the resume.
- **A shown Pause after an interruption requests a start** (graded P2, serious): the label reads
  `state.playing` while `toggle` reads `el.paused`, so a click on the Pause still shown restarts
  the track against the reader's choice, through the gate; telling the bar's and the lock screen's
  handlers apart, which this does not do, is what it needs.

What the bar does around the browser's own resumes is otherwise as on main: the pause count,
`ownPlay`'s comparison, `toggle`'s dispatch and the button's label are unchanged.
