## 2026-11-24 — B1015: the bar's button carries out what it shows before it returns

### What

Four ways the player bar's button, at `a10be5cc` (#1566), did other than what it showed, each
shown red by its cases there, then green, with its controls. Three of them predate the branch and
stand on main at `4560b06b` too, their cases red over main's modules; the second came with this
branch's displayed-action dispatch (main's click paused the track there, letting the row go).

- The click over a resume of the browser's own that waits on the gate let the wait go and left
  what this browser meant to play standing: with the browser's pause overtaken by its resume (both
  before either event), a network or decode error after the click retried the track on its old
  start's permit and played it before the barrier answered. The same error before the click did
  the same, and a play of the browser's own made in the gap before the click unpaused the element,
  whose later event then asked the gate again and played on the barrier's answer.
- The Play the bar showed over the track the browser had just resumed, its play event still to
  come, did nothing (the element plays), so a row asked before it kept its place behind the barrier
  and the answer played the row over the track the reader chose.
- An interruption's event older than the bar's own pauses, delivered first, took the record of the
  first of them, so every later pause event took the record of the one before it, and the last,
  the protective pause of a resume made after the lock screen's Pause, found none and let that
  resume's wait go: the barrier's answer played nothing.
- While a resume waits the button said Play, its glyph the play glyph, which the reduced-motion
  rule shows in place of the ring, and its click cancelled the wait.

### The changes

Production, `audio.js` and `nowplaying.js` (`a327cba1`): 42 lines added and 21 removed, 15 of the
added code and 27 comment. No server change, route or protocol field, and no change to the gate's
design.

- **The click over a wait is the Pause.** `toggle()` over a waiting resume calls `pauseNow`, as
  over the Pause it shows: the reader's generation moves, the intent ends, the element is paused at
  once if the browser had played it again (a pause recorded as the reader's), and the starts the
  gate holds are let go. And a waiting resume means nothing to play until the gate lets it go on:
  `ownPlay` clears the intent as it pauses the element, and its admission sets it, so a retry
  during the wait plays nothing and the admission plays the reloaded track once.
- **The bar's Play over the browser's own play is a start.** Over an element that plays, the
  lock screen's Play does nothing, and so does the bar's over its own start whose play event is
  still to come (`marks` not empty: a double click); over the browser's own play the bar's Play
  shown is the reader's start, its generation moved, and takes the place of a row asked before it.
- **A wait made amid the browser's own changes is let go by no pause event.** The pause listener
  notes whether each pause event came with the element playing again (`pauseOvertaken`), and
  `ownPlay` gives its wait that note (`amid`): the browser's own changes were still arriving as the
  bar read them. An unrecorded pause event ends a wait only where it was not made amid them; the
  reader's action or the gate's answer ends one that was.
- **The button says Cancel resume.** The snapshot carries `resumeWaits`; over a wait the button's
  name and title say Cancel resume and its glyph is the sprite's stop (`i-np-stop`), which the
  reduced-motion rule shows in place of the ring; the title follows the action in both outputs. An
  admission the gate makes at once emits, so the bar says Play before the element's events come.

### Why the wait made amid the browser's changes is judged apart

The bar cannot see the browser's transitions until their events come. After a resume's protective
pause, a run of events "pause, play that finds the element paused, pause" is either an older pause
and resume of the browser's, queued before the protective pause, followed by the protective pause's
own event, or the protective pause's event followed by a newer play and interruption of the
browser's: the same events in the same order, and no reading of the element at any of them tells
the two apart. The record cannot be matched to its event in both. What differs in the cases that
showed the defect is how the wait began: amid an overtaken pause event, the browser's changes
still arriving. A wait made on a settled element keeps the record's order, and a later
interruption ends it as before. The cost, stated in the long form: a newer interruption during a
wait made amid the browser's changes does not end that wait; the gate's answer, or the reader's
action, does.

### Tests and records

Twelve harness scenarios, each run by a Go test of its name in `player_output_bar_test.go`, and a
structural test in `player_output_wiring_test.go`:

- `TestCancellingAResumeWaitEndsWhatItsRetryWouldPlay` (the click, then a network error: no play
  before or after the barrier's answer); `TestAnErrorWhileAResumeWaitsPlaysOnlyOnItsAdmission`
  (the error first: no play until the answer, then the reloaded track once; on track 2 the error,
  then the click: none); `TestACancelledResumeLeavesTheElementPausedThoughTheBrowserPlaysAgain`.
- `TestTheBarsPlayOverTheBrowsersOwnPlaySupersedesAnOlderRow` (track 1 plays through admission,
  never row 7); `TestTheLockScreensPlayOverTheBrowsersOwnPlayLeavesTheRowToPlay` (the same
  sequence with the lock screen's Play: row 7 plays once); `TestADoubleClickOfTheBarsPlayStartsOnce`.
- `TestAResumeAfterAPauseOutlivesAnOlderInterruptionsEvent` (interruption, resume, the lock
  screen's Pause and a resume before any event, the barrier held: the wait lives to the answer,
  which plays track 1 once); `TestAResumeAmidTheBrowsersOwnInterruptionsKeepsItsWait`
  (interruption, resume, interruption, resume); `TestAnInterruptionAfterAResumeWaitsEndsTheWait`
  (a wait made on a settled element, then a play and an interruption: the wait ends, no replay).
- `TestTheResumeWaitsButtonSaysCancelResume` (name, title, glyph and busy class while it waits;
  Play again once activated; the answer starts nothing); `TestAResumeAdmittedAtOnceNeverSaysCancelResume`;
  `TestTheResumeWaitsCancelCueShowsUnderReducedMotion` (the glyph nowplaying.js names is in the
  sprite, the button carries the busy class, and a reduced-motion block shows the glyph and hides
  the ring); `TestNextOrARowDuringAResumeWaitStartsOnlyTheReadersChoice`.
- The long form in `ops/History-rules-admin-console.md` says each of these beside its claim, and
  now cites `TestAContinuationOrADeferredPlayPlaysNothingOnceTheReaderActed` and
  `TestTheLockScreensPlayKeepsAResumeWaitingOnItsBarrier`, which it had left out. The rule's line
  stands as it was.

No shipped scenario's assertion or driving changed.

### Red first, and the controls

Over the player modules at `a10be5cc`: red, the two cancel cases, the paused-at-once case, the
bar's Play over the browser's own play, the post-Pause resume and the interleaved resume, and the
Cancel resume case; green there, as controls, the lock screen's row case, the double click, the
later interruption, the immediate admission and Next or a row during a wait. The structural test
failed at `a10be5cc` (nowplaying.js named no glyph for a wait).

Twelve controls, each a mutation of a copy of HEAD's player modules, predicted in writing before
it ran, the whole harness run over it, all as predicted:

| The mutation | Red |
|---|---|
| the click over a wait only lets it go, no Pause | the paused-at-once case |
| a waiting resume keeps the intent | the error-first case |
| both of those | the two cancel cases and the paused-at-once case |
| the bar's Play over any element that plays does nothing | the bar's Play over the browser's own play |
| the bar's Play over its own start in flight starts again | the double click, `TestTheLockScreensPlayAndPauseDoWhatTheySayWhileAnEventIsQueued` |
| the lock screen's Play supersedes as the bar's does | the lock screen's row case |
| every unrecorded pause lets a wait go | the post-Pause resume, the interleaved resume |
| the fix replaced by a reordering of the pause listener's condition, so an overtaken event takes no record | the interleaved resume alone |
| no unrecorded pause lets a wait go | the later interruption |
| the admission at once does not emit | the immediate admission |
| the button says Play over a wait | the Cancel resume case |
| the title stays as it was made | the Cancel resume case |

And the structural test, by a prebuilt test binary over copies of the three files it reads: green
at HEAD, red with the stop symbol taken out of the sprite, with a glyph the sprite lacks, with the
reduced-motion rule hiding the glyph, and with the busy class no longer following the wait.

### The runs

At `697cb963`, in one session, on `GOTOOLCHAIN=go1.27.3` with `GOPROXY=off`, the release tags and
node on the path, so the node cases ran: `make fmt` (it rewrote nothing); `make vet`, and with
`GOOS` and `GOARCH` set for linux/arm, linux/386, linux/arm64, windows/amd64, windows/arm64 and
darwin/arm64 (exit 0, 4 to 5 s each); `make docs-check`; `make test P=8` with `-count=1` (exit 0
in 2,017 s, 93 packages ok: cmd/bridge 1,990.0 s, internal/manifest 1,658.4 s, internal/admin
1,529.8 s, internal/backup 704.0 s); `make build-all` (25 s); then with `-v` the source guards by
name, all passing and none skipped: the nineteen `2026-11-24-b1015--fix-b1015-bar-intent.md`
names, `TestTheResumeWaitsCancelCueShowsUnderReducedMotion`, and
`TestThePhoneBarKeepsItsWordsWithTheBridgeChosen`, which reads nowplaying.js, with cmd/bridge's
`TestEveryCitedTestNameExists`, `TestNoBlankKeepers`, `TestNoDocblockNamesAnotherDeclaration` and
`TestEveryBackgroundGoroutineDrainsOnCleanup`; the thirty-one cases by name, that entry's nineteen
and these twelve, all passing, none skipped; with `-race -count=20`, the six guards that entry's
race run names and the thirty-one cases (exit 0 in 161 s); and the harness's 166 scenarios, every
check passing. The tree was clean at the end.

### Not run

- No real browser ran these sequences. The harness's element queues the media element's play and
  pause events as tasks, a load removing them, as the specification's load algorithm does; a
  browser session should press the bar's Cancel resume while a resume of the browser's own waits
  on the barrier and then cut the network, press the bar's Play just after the browser resumed the
  track by itself with a row asked before it, resume after the lock screen's Pause just after an
  interruption, and look at the button under reduced motion while a resume waits.
