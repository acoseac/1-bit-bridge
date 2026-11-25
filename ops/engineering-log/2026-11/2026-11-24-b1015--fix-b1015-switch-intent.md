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
36 comment) for the reader's generation and its comparisons, then 30 added and 27 removed (16 of
the added code and 14 comment) for what a switch's end settles and the resume's wait, then, in
`audio.js` alone, 15 added and 8 removed (6 of the added code and 9 comment) for the mark a
source the browser cannot use leaves on a waiting resume. The commits: `1ae65a01` (the cases,
red at main where the change is due), `1aa02b06` (the change), `6d972d88` and `a353c9f8` (this
entry, the rule's line and its long form, the runs); `03bfb173` and `f3438deb` (the cases for
what a switch's end settles and the resume's wait, red at `a353c9f8`), `15b29b0a` (that change),
`ccbbd67b` and `4eb68e7f` (their records and runs); `0ca3cf71` (the cases for the mark, three of
them red at `4eb68e7f`, and the three-document case's history), `4802c532` (the mark),
`4a7fb4ef` (the mark's spinner and Play cases), and the records after them.

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
  which would overwrite the queue the reader cleared or chose. `take`'s own advance comes after
  the comparison, and `permitFor` ignores the reader's generation and checks the pick's
  (`gen !== pickGen`): a later pick voids the old permit, and `take` advancing the reader's
  generation leaves it standing, so the handover's deferred play and its continuations keep their
  permit. An entry a document inherits carries the generation as that document boots
  (`bootIdentity`; nothing stored carries it over), and `finishInherited` compares it as the
  others do, so a row the reader starts in the reloaded document stays when the earlier document's
  switch is read back.
- **The switch's end settles what it shows, and nothing a newer operation shows**
  (`ended(to, words)`), at every end of the switch: its answer and its read back after a
  reader's action, an inherited read back, `operation_gone` or an epoch's end (`unknown`), a
  refusal before admission (`refused`, whose words `send` now leaves to the switch), and a record
  that is not done, here and in `finishInherited`. Ownership is decided by the operations, never
  by the phase's value alone: the phase is the switch's while it shows the switch's move
  (`toBrowser`, which only a switch sets, a newer one superseding this one), or `unconfirmed` while
  no other operation of the current pick is lost in this document (`ops`, `lost`, `ours`) or
  inherited and not yet resolved (`inherited`, `ours`). A lost barrier of an older pick holds
  nothing, since `live` is resolution alone and `newPick` leaves older lost entries to their own
  read backs; and `unconfirmed` cannot say whose loss set it. Where the switch holds the phase it
  becomes `to` (this browser's after a reader's action, the bridge's for a refusal, a gone record
  or one not done, as on main). The line is the switch's when it shows the switch's moving words
  (`WORDS.toBrowser`, which no other operation sets), retired whatever the phase is now, or, with
  the phase held, its unconfirmed line, the connection's reconnecting words over it, or none; it
  becomes `words`: `WORDS.bridgeAgain` exactly where main shows it (the permit failing at the
  answer, the bridge sounding at a read back), otherwise empty, and the switch's own gone,
  refused or changed words where main shows those. A newer start's or operation's line stays:
  the gate's words for a row it refused, a newer barrier's completion words, a newer loss's
  `WORDS.unconfirmed` until that operation's own read back. The state frames' rule governs from
  the next state on.
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
- **A failure past its retry ends a resume's wait** (`onError`): a network or decode error past
  its retry sets `resumeWait = null` beside `intent = false`, so the wait's admission is inert by
  its own identity check.
- **A source the browser cannot use marks a resume's wait** (`onError`, `resumeWait.skip`), and
  the wait's answer settles only whether that source's skip goes on, as on main: admitted, it
  sets the intent and plays nothing (`ownPlay`'s `if (!wait.skip) playEl()`), so the skip's timer
  asks the gate for the next track; refused, its drop ends the intent, so the timer starts
  nothing; a timer that fires first takes the resume's place behind its barrier, and the
  resume's drop ends the intent, so nothing plays. `15b29b0a` ended the wait there too, which this entry said until `4eb68e7f`: the
  admission played nothing, as now, but the intent no longer followed the answer, so after a
  refusal the timer sent a barrier of its own and played the next track, after the browser's own
  pause the inert admission left the intent ended and the timer played nothing, and a timer that
  fired first played the next track on the resume's barrier. The mark goes with its track:
  `load()` ends a marked wait, so the next track's first network error retries rather than being
  left to the old wait, and so does the stop after three unplayable tracks in a row
  (`handleSourceError`), so an admission after it sets no intent. A marked wait is no wait the bar
  shows: the pause listener's spinner (`state.loading`) and `toggle`'s cancel pass it over, so the
  bar's Play over the failed track is a start through the gate.

What is no action of the reader's, and advances nothing, as on main: a seek (the scrubber and
the lock screen's), an album's **Add to queue** (`enqueue`), removing a row that is not playing,
the level, and the shuffle and repeat settings; a handover taken after them replaces the queue
and the position, as before (`TestASeekOrAnAddWhileASwitchToThisBrowserIsOutstandingLeavesItsHandoverToPlay`).

### The cases, their reds and the controls

Browser-module scenarios in `internal/admin/testdata/bar_harness.mjs`, each with a caller in
`player_output_bar_test.go`. The switch is sent from an unconfirmed bar, this browser already the
output, so a row's start is admitted as a barrier. The console it meets keeps its records as
serve's control does (`theConsole`): each tab's last operation alone
(`cmd/bridge/serve_player_control.go`'s `admit` and `Operation`), so a read back of a sequence a
newer operation superseded answers `operation_gone`, and the eight most recent records, which
every publication of it carries, handovers left out (`recentRecords`). Its history is a
possible one, asserted where it is used: the setup barrier read back running at revision 38,
below the switch's silence, 40, below each later barrier's, 41 and up, one still running as its
answer is lost included; the switch's admission ends the setup barrier cancelled, and every
publication after it holds the handover, 09.flac, as a one-track queue, paused; a row's barrier
admitted after the switch finds the switch run to done and only its HTTP answer held. The nothing-changed refusal's terminal state, at revision
6, above every publication before it, comes by the stream before GET now says no player is
offered, and its HTTP answer, carrying that same state, comes last. A row is the library's own
control: the shipped `views.js`'s album page (`renderAlbum`) rendered over the scenarios' DOM
model, its row's `.track-title` button clicked, which plays through `playOn` (`libraryRows`); the
model's `querySelector` and a new `querySelectorAll` take a class selector for it. A reload is a
fresh copy of the player's modules over the same session storage, as a new document has them
(`reloaded`). Stop, Pause and Next are the lock screen's handlers or the bar's buttons; no case
moves the generation itself.

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
  resumed track 1 over the Next's track 2. The second, its history corrected as above, is green
  at `a353c9f8` and red at main: no barrier sent for a resume, no play of track 1, track 2
  still selected.
- `TestCancellingAResumeWaitLeavesNothingForItsRetryToPlay`: at main the network error's retry
  after the spinner's press played. Its sequence queues the browser's pause and its play while
  the element plays (`resumeQueued`), so the pause's event finds the element playing and the
  intent stands; main's `browserResumeWaits` delivers the pause's event first, which ends the
  intent, and the case run that way stayed green under its control (measured).
- `TestAnErrorWhileAResumeWaitsPlaysOnlyOnTheResumesAdmission`: at main the retry played at once
  and again on the admission; now nothing until the admission, then once, and nothing at all
  where the barrier refuses.

Red at `a353c9f8` (and, but for one, at main) and green with what the switch's end settles and
the resume's wait:

- `TestAStopAfterARowRetiresTheSwitchsMovingWordsWhenItIsAnswered`: the switch's answer held, a
  library row played, the lock screen's Stop while the row's answer is held, the row's barrier
  answered (its start released, so the bar takes this browser's phase), then the switch's answer:
  at `a353c9f8` the empty bar kept "Moving to this browser…"; now an empty line, an empty queue,
  nothing of the handover played and no operation pending.
- `TestAnInheritedSwitchLeavesTheWarningOfARowLostSinceTheReload`: an inherited switch's record
  held, a row of the new document whose barrier's answer is lost, then the record: at
  `a353c9f8` and at main the switch cleared the row's warning; now `unconfirmed` and
  `WORDS.unconfirmed` stay as the switch settles, and the row's own read back then gives this
  browser's phase and the row's words (`WORDS.startAgain`).
- `TestAnInheritedSwitchLeavesTheWarningOfAnInheritedBarrierStillUnresolved`: three documents of
  one tab; the second plays a row whose barrier the console still runs as its answer is lost, its
  admission published above the switch's silence (asserted); the third inherits the first's
  switch and that barrier, and its reader presses the lock screen's Stop, which sends nothing; the
  switch's record read back done takes and plays nothing, the queue empty at index -1 and paused,
  the inherited barrier's key the one operation pending and its warning kept. At `a353c9f8` the
  read back set this browser's phase and cleared the line; at main it took the move stopped,
  09.flac at index 0, with `WORDS.pressPlay`. Until `4eb68e7f` the third document played a row of
  its own: its barrier, admitted at the console, would have ended the running barrier's unsent
  steps, as the switch's admission ends the setup barrier's, and the fixture kept that barrier
  running beside it.
- `TestAGoneSwitchLeavesTheWarningOfARowLostSinceItWasSent`: one document, the switch's and a
  row's answers both lost, the row's read back held: the switch's `operation_gone` set the
  bridge's phase and its gone words at `a353c9f8`; now the row's warning stays until its own read
  back.
- `TestTheSwitchsAnswerKeepsTheGatesWordsAsItSettlesThePhase`: the bridge playing, a row refused
  by the gate (`WORDS.bridgePlaying`, the phase still `toBrowser`), then the answer: at
  `a353c9f8` the line became `WORDS.bridgeAgain`; now this browser's phase with the gate's words.
- `TestAFailurePastItsRetryEndsAResumeWait` (an ordinary permit) and
  `TestAFailurePastItsRetryEndsAResumeWaitAfterAStartWithNoPermit` (a start made while no player
  was offered, then an offered silent frame on an open stream): a resume waiting on a held
  barrier, a network error and a second past its retry, then the admission: at `a353c9f8` the
  admission played the failed source; now no play attempt after the second error or the
  admission, the element paused, the failure still shown. The first is red at main too (its
  first retry played at once); the second is green at main, whose first retry took the wait's
  place behind the barrier.
- `TestASourceTheBrowserCannotUsePlaysNothingOnTheResumesAdmission`: a code-4 error while the
  resume waits, then the admission: at `a353c9f8` and at main the admission played the failed
  track; now nothing, and the skip, when its timer fires, plays the next track through a barrier
  of its own. Named for the wait's end until `0ca3cf71` renamed it for the mark, its assertions
  unchanged.

Red at `4eb68e7f` and green with the mark, each a code-4 error while a resume of the browser's own
waits on its held barrier, the skip's timer held until the case fires it:

- `TestAResumesRefusalAfterASourceTheBrowserCannotUseStillEndsItsSkip`: the resume's barrier
  refused, then the timer: at `4eb68e7f` it sent a barrier of its own and played track 2; now, as
  at main, it asks nothing and plays nothing, the queue at track 1, paused.
- `TestAResumesAdmissionAfterASourceTheBrowserCannotUseLetsItsSkipGoOn`: the browser's own pause
  delivered first, which ends the intent, then the error and the admission, then the timer: at
  `4eb68e7f` the timer played nothing; at main and at `a353c9f8` the admission played the failed
  track; now nothing of it plays and the timer plays track 2 through a barrier of its own.
- `TestASkipTimerBeforeAResumesAnswerPlaysNothingOfTheNextTrack`: the timer before the barrier
  answers: at `4eb68e7f` track 2 played on that barrier's answer; now, as at main, no barrier more
  is asked and nothing plays.

Green at `4eb68e7f` and with the mark, pins of where the mark ends and what it shows:

- `TestTheNextTrackAfterASkipStartedWhileAResumeWaitedRetriesItsFirstError`: no player offered
  while the barrier is held, the timer starts track 2 at once, the old barrier is admitted, then
  track 2's first network error: it retries once. Red at `a353c9f8`, where the wait outlived the
  load and took the retry over; green at main, whose retry played at once.
- `TestAResumeHeldAcrossTheStopAfterThreeUnplayableTracksStartsNothingAfterIt`: the third
  unplayable track in a row while the resume waits, then the admission, then the failed row
  removed: no successor starts. Red at main and at `a353c9f8`, where the admission set the intent
  again past the stop and the removal started track 4 through a barrier.
- `TestASourceTheBrowserCannotUseShowsNoSpinnerForTheResumeItLeaves`: the resume's pause, its play
  and the failure queued together, the protective pause's event after the failure: no spinner over
  the failed track. Red at main and at `a353c9f8`.
- `TestTheBarsPlayOverASourceTheBrowserCannotUseIsAStartThoughAResumeWaits`: the bar's Play over
  the failed track while the resume waits is a start, which plays track 2 on the barrier's answer.
  Red at main and at `a353c9f8`, where it cancelled the resume and nothing played.

Green at main and with the change, kept as pins:
`TestAPausesEventAfterThePickOfThisBrowserLetsItsHandoverPlay` (a Pause, the pick before the
Pause's event, the event, then the answer: the handover taken and played once),
`TestASeekOrAnAddWhileASwitchToThisBrowserIsOutstandingLeavesItsHandoverToPlay`,
`TestAStopBeforeAReloadIsNotSeenAndTheMoveComesBackPaused` (the residual below: the queue of one
at index 0, 09.flac, nothing played, paused, the line `WORDS.pressPlay`) and
`TestAHandoverPlaysAndGoesOnWithNoReaderActionBetween` (a playing handover at 30 s with two
tracks plays once from its position and goes on to its second track, no barrier). Green at
`a353c9f8` and kept: `TestASwitchsReadBackClearsItsOwnWarningBesideAnOlderPicksLostBarrier` (the
setup barrier, of the older pick, still lost as the switch's record is read back, its own read
back failed and its `operation_gone` later: the switch clears its own warning at once). The
undisturbed switches main pins stay green with their assertions unchanged, among them
`TestTheSwitchToTheBrowserStartsItOnlyOnSilence`, `TestACopiedTabTakesTheMoveStoppedAndStartsNothing`
and `TestAReloadKeepsANewerPicksSupersession`, and so does every scenario before this.

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
  `TestRemovingARowThatIsNotPlayingCancelsNoResume`;
- phase equality in place of the ownership rule (the phase held while it is `toBrowser` or
  `unconfirmed`, no operation asked): the inherited-and-lost-row case, the gone case and the
  lost-row late answer red;
- the moving words retired only with the phase held: the Stop-after-a-row case alone;
- the line coupled to the phase again (set whenever the phase is held, never otherwise): the
  gate's-words case and the Stop-after-a-row case;
- a lost entry of any pick holding the phase: the older pick's case alone;
- inherited operations of the pick holding nothing: the three-document case alone;
- `resumeWait = null` removed past the retry: both failure cases; removed for a source the
  browser cannot use, at `4eb68e7f`: its case alone.

The mark's controls, the same way on `4a7fb4ef`'s modules, all as predicted, and no other case
red in any:

- the mark replaced by the wait's end, as at `4eb68e7f`: the refusal, admission-after-pause and
  timer-first cases red;
- the mark removed, nothing done at code 4, as at main: the admission case, the
  admission-after-pause case, the next-track retry case, the spinner case and the Play case red;
- the stop after three unplayable tracks leaving the marked wait: the case held across it alone;
- the load leaving the marked wait: the next-track retry case alone;
- the admission playing the failed source: the admission and admission-after-pause cases;
- the spinner's exclusion removed: the spinner case alone;
- the Play's exclusion removed: the Play case alone;
- inherited operations of the pick holding nothing, on the rewritten three-document case: it
  alone.

### What this does not cover

- **Across a reload** (graded P2, silent): an operation a new document inherits carries that
  document's generation alone, so a Stop pressed before the reload is not seen: the reloaded
  document takes a done switch-out stopped, as on main, the bridge's queue comes back paused and
  nothing plays. The generation is not persisted.
- **A resume lost after a Pause, and an interruption consuming a later record** (graded P2,
  silent): the pause count (`ownPauses`) can take a Pause's event for a protective one and end a
  genuine resume, leaving the browser silent until another Play.
- **A positioned handover's metadata listener plays before a resume's admission** (graded P2,
  also on main): a playing handover at a position waits for its metadata; the browser resumes by
  itself before it, its barrier held, and an error's retry keeps the playback generation and the
  listener, so the metadata seeks and plays on the older permit while the admission is pending,
  and the admission plays again. Its `loadedmetadata` listener is left as it is.
- **The spinner's control is labelled Play** (graded P2): a reader who presses that Play cancels
  the resume.
- **A shown Pause after an interruption requests a start** (graded P2, serious): the label reads
  `state.playing` while `toggle` reads `el.paused`, so a click on the Pause still shown restarts
  the track against the reader's choice, through the gate; telling the bar's and the lock screen's
  handlers apart, which this does not do, is what it needs.
- **An inherited operation of another kind** (also on main, not graded): a barrier, a command or
  an edit an earlier document left, settled by its own read back or as gone, still sets the phase
  and its words whatever a newer operation of the pick shows; the rule above is the switch's.

What the bar does around the browser's own resumes is otherwise as on main: the pause count,
`ownPlay`'s comparison and the button's label are unchanged, and so is `toggle`'s dispatch but
for a marked wait, which it passes over.

### Runs

At `6d972d88`, on the pinned toolchain, offline, with the release tags:

- The full build-and-test target (formatting, the vets, the race-checked suite, the six release
  builds and the doc checks) in one session holding the build slot: exit 0 in 2,161 s. The
  formatting step rewrote nothing; in the race-checked suite `cmd/bridge` took 1,970 s and
  `internal/admin` 1,488 s. In the same session the release-tag vets for linux/arm64,
  darwin/arm64, darwin/amd64, windows/amd64, windows/arm64, linux/arm and linux/386: each exit 0.
- The source guards by name, each passed: `TestEveryStartOfThisBrowserAsksTheGate`,
  `TestTheGateGuardReadsWhatAGateCallRunsAtOnce`, `TestNoSoundSourceOfTheConsoleBypassesTheGate`,
  `TestTheHistoryListsTheMediaSessionsStartsAsTheyAreSet`,
  `TestEveryNodeLookupOfTheOutputsTestsIsBarNode`, `TestFailureAutoAdvanceIsGuarded`,
  `TestThePlayerFrameIsWiredFromTheStreamToTheBar`, `TestEveryRowDecidesByTheOutputChosen`,
  `TestEveryPlayerControlStateFieldIsRenderedSomewhere`,
  `TestNoConsoleControlDisablesItselfOutsideSetDisabled`, `TestTheBarsPromiseIsSaidWordForWord`
  and `cmd/bridge`'s `TestEveryCitedTestNameExists`.
- The browser-module scenarios: all 151 run, none failing. Against main's modules the 136 older
  ones pass and 11 of the 15 new ones fail, as listed above.
- The race-checked bar tests (`player_output_bar_test.go`, 152 tests), ten times each: 1,520
  passed, none failed or skipped, on an idle machine (207 s); and 1,520 passed, none failed,
  beside four CPU-bound processes started for the run and stopped by their process IDs after it
  (170 s, a load average of about 4.4 on 16 cores).

At `ccbbd67b`, the same way, with what the switch's end settles and the resume's wait:

- The full build-and-test target in one session holding the build slot: exit 0 in 2,122 s, the
  formatting step rewriting nothing, `cmd/bridge` 1,980 s and `internal/admin` 1,496 s in the
  race-checked suite, the six release builds and the doc checks passing; in the same session the
  release-tag vets for the same seven targets, each exit 0.
- The same twelve source guards by name, each passed.
- The browser-module scenarios: all 160 run, none failing. Against `a353c9f8`'s modules the 151
  before pass and 8 of the 9 new ones fail, all but the older pick's pin; against main's, 8 of
  the 9 new ones fail, all but the start made with no permit, and the 11 listed first.
- The race-checked bar tests (161 tests), ten times each: 1,610 passed, none failed or skipped, on
  an idle machine (215 s); and 1,610 passed, none failed, beside four CPU-bound processes started
  for the run and stopped by their process IDs after it (184 s, a load average of about 4.4).
