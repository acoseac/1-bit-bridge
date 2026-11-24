## 2026-11-24 — B1005: a Stop lets go of a waiting start, and an event a later call overtook decides nothing

### What

The last changes of B1005 (#1563), on `7ab8f6f8`: three defects of the bar's browser half, each
shown red by its cases against the player modules of the commits named below, then green, with its
controls. The code is `audio.js` (and comments in `output.js`); the rest is the harness and its
cases, the rule's line and its long form, and a correction to this PR's earlier entry. No server
change, route, protocol field or server state, and no change to the gate's design.

### The changes

Production, `audio.js` and `output.js`: 40 lines added and 31 removed (7 of the added code and 33
comment, 9 of the removed code).

- **A resume of the browser's own that a pause overtook asks nothing** (`audio.js`, `ownPlay`;
  `8b87168a`). With track 1 paused and a row's start waiting on its barrier, the browser resumed
  track 1 by itself, queuing its play event, and the lock screen's Pause came before that event and
  let go of the row's start. The event then reached `ownPlay`, which read the reader's action count
  as it stood after the Pause, and the gate let the resume take the slot the Pause let go of, so the
  barrier's answer, done beside a silent bridge or refused for no player, played track 1 again;
  where the gate already knew no player was offered, it played track 1 at once. `ownPlay` now asks
  nothing when the element is paused at the play event, which only a pause made since that play
  leaves (a load removes the queued event); that pause's own event follows. The gate is unchanged:
  the slot a Pause let go of stays open to a start asked since, so a resume of the browser's own
  made after the Pause still plays on the barrier in flight. Before the explicit Pause let go of
  starts (`25dcbf6c`) the gate refused that resume for the row's waiting start, and the barrier's
  answer played the row the Pause meant to cancel.
- **The bar's Pause shows Play at once, and a pause a play overtook ends no intent** (`audio.js`,
  `toggle`, `pauseNow` and the pause listener; `59140826`). The bar's Pause paused the element and
  showed nothing until the element's pause event, a task later, so after a fetch the user aborted
  (code 1), which fires no pause, a second press on the Pause still shown asked the gate for a
  start. The bar's Pause is now `pauseNow`, the lock screen's, which shows the bar stopped at once.
  The Play then shown, pressed before the pause event, started the track, and the event, coming
  with the element playing again, ended that Play's intent, so the track's end played nothing more:
  the pause listener now ends the intent only where the element is still paused at the event.
  `load()`'s own aborts are not read here: neither change looks at `abort`, and a load sets the
  element's error back to none.
- **A Stop lets go of a start the gate holds** (`audio.js`, `clearQueue`; `80603825`). The lock
  screen's Stop cleared the queue and left a row's start waiting for the epoch or behind its
  barrier, so the first frame sent the barrier and its answer, like a barrier already in flight,
  put the row back into the cleared queue and played it. This predates the PR: main's modules do
  the same. `clearQueue` now calls the gate's cancel (`startCancel`, `output.js`'s `cancelStarts`),
  as the explicit Pause does: a start waiting for the epoch is dropped, sending nothing, and one
  behind the barrier runs nothing on its answer, the barrier ending quietly.

### Tests and records

- The cases, each a harness scenario run by its Go test:
  `TestTheBrowsersOwnResumeAPauseOvertookPlaysNothingOnItsBarriersDone` and
  `TestTheBrowsersOwnResumeAPauseOvertookPlaysNothingOnANoPlayerRefusal` (track 1 paused, the row's
  barrier held, the browser's resume queued, the lock screen's Pause, then the play event, the
  barrier's answer as that event ends, and the Pause's own event: neither track 1 nor the row plays,
  track 1 stays selected, paused and shown stopped);
  `TestTheBrowsersOwnResumeAPauseOvertookAsksNothingWhereNoPlayerIsOffered` (the same where no
  player is offered, and a resume of the browser's own made after the Pause still plays);
  `TestAfterAnAbortThePausePressedShowsPlayAtOnce` (after the abort, the Pause pressed shows Play
  before its pause event; that Play pressed before the event asks the gate once and plays track 1,
  and the track's end plays track 2); `TestAStopLetsGoOfAStartWaitingForTheEpoch` and
  `TestAStopLetsGoOfAStartWaitingOnItsBarrier` (no barrier sent before the first frame, nothing
  played, the queue empty at index -1, Play on it doing nothing, and a row played after the Stop
  playing through a barrier of its own). `TestAfterAnAbortTheButtonDoesWhatItShows` now also
  asserts the track its Play starts: index 0, `01.flac`.
- The rule's line and its long form (`35e1fd3e`): a play with no mark that a pause has overtaken
  asks nothing. The long form says what a Stop, the bar's Pause and an overtaken pause event do,
  and cites beside its claims the cases and source guards of this PR it did not cite: fourteen
  scenarios and `TestTheGateGuardReadsWhatAGateCallRunsAtOnce`,
  `TestTheHistoryListsTheMediaSessionsStartsAsTheyAreSet` and
  `TestTheOutputMenuTakesTheFocusOfAPressOnItsWords`.
- This PR's earlier entry (`8c8824c1`): its Not run section says what was not run and what was not
  changed, and no longer where or why.

### Red first, and the controls

The new cases ran under node over copies of the player modules at `7ab8f6f8`, at `25dcbf6c` and at
main's `80e6d288`, the tracked tree untouched:

- the overtaken resume, done and refused: red at `7ab8f6f8` on "the paused track is not played
  again" (track 1 played a second time) and green there on "the row the Pause let go of is not
  played"; at `25dcbf6c` and `80e6d288` the reverse, track 1 not played again and row 7 played;
- the same where no player is offered: red at all three, track 1 played again;
- the abort case: red at all three, Pause still shown after the press and a press on it asking the
  gate (at `25dcbf6c` the first press already asked it); at `7ab8f6f8` and `80e6d288` the track's
  end then played nothing more;
- the two Stop cases: red at all three, one barrier sent and row 7 played into a queue of two.

`TestAfterAnAbortTheButtonDoesWhatItShows`'s added assertion was green at `7ab8f6f8`, whose Play
already started the track it showed.

Ten controls, each predicted before it ran, each a mutation of a copy of the player modules, all as
predicted. A play a pause overtook asking the gate again: the three resume cases red, the slot a
Pause let go of taken by a later resume (`TestTheBrowsersOwnPlayTakesTheSlotAPauseLetGo`) and an
admitted resume green. A continuation never taking a slot a Pause let go of: that case red, the
three resume cases green, so the gate's admission is still what a later resume needs. The
browser's own play never asking the gate: the no-player case's later resume and four other cases
red. The bar's Pause showing nothing until its event: the abort case red on its first two checks.
A pause a play overtook ending the intent: the abort case red at the track's end. Play over an
abort moving on as over a source it cannot use: both abort cases red. A Stop letting go of
nothing: both Stop cases red, the explicit Pause's cases green. The cancel keeping the start
behind the barrier: the barrier cases of the Stop and of the Pause red; keeping the one waiting
for the epoch: the epoch cases red. Play/Pause going on over a cleared queue: both Stop cases red
on their plain Play.

Each commit's own harness ran green over its own modules: 133 scenarios at `8b87168a`, 134 at
`59140826`, 136 at `80603825`.
