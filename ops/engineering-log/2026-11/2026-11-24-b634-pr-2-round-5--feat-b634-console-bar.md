## 2026-11-24 — B634 PR 2, round 5: a reload never leaves a stale protective pause; the census counts

### What

The review of round 4 (`23507760`) asked for three changes, all in the bar's browser half:

1. A load could land between the protective pause of the browser's own play and that pause's
   event. The load removed the event, and the pause's count stayed. The browser's next real pause
   was then taken for the protective one. The intent stayed, and the next removal of the current
   row started its successor, a start nobody asked for.
2. The census of the console's sound sources allowed a site by whether it held a find, not by how
   many. A third element made in `init`, beside the two it names, passed it.
3. With no gate wired, a start ran. A boot can throw after `audio.init()`, which restores the saved
   queue and sets its source, and before it wires the two gates. Then `requestStart` ran the start
   with no permit, `continueStart` went on, and the play listener took the browser's own play for
   the bar's.

This round changes `audio.js`, the bar's harness and its cases, the two source tests, and the rules
line with its long form. No server change, route, protocol field or server state. `output.js` and
`boot.js` are unchanged.

### The changes

- **A load clears the protective pauses' count.** For the element that plays, `loadEl` zeroes
  `ownPauses` with the marks. The load algorithm removes the `pause` events those pauses wait for,
  as it removes the `play` events the marks wait for.
- **The census counts.** Each allowed site carries its count of finds: `init` 2, `engineCanPlay` 1,
  `playEl` 1, `setMediaSession` 1, `wireMediaSession` 1. A site found another number of times fails
  the census, which names the count and the reason.
- **With no gate wired, every start is refused.**
  - Without the start gate, `requestStart` runs nothing.
  - Without the continuation gate, `continueStart` and `ownPlay` drop.
  - The play listener hands every play the bar did not mark to `ownPlay`, whatever the gate. There
    the play is paused and refused, as a refused start is.

  The harness wires a gate that lets every start go on for the cases about something else, which
  relied on the pass-through before; `unwired` stands for the failed boot.
  `TestEveryStartOfThisBrowserAsksTheGate` required the play listener's branch with the gate in its
  condition, `if (!marks.length && continueGate)`, which is the fault. It now requires the branch
  without it. It also requires that each way a start asks its gate (`requestStart`,
  `continueStart`, `ownPlay`) asks only where the gate is wired, refuses otherwise, and calls its
  callback nowhere else.

In `audio.js` the round adds 14 lines and removes 11: 9 added lines of code, 5 of comment.

### Red first, and the controls

Against the round's base modules (`23507760`), with the harness's new cases:

- The load case with no player offered: red at its second check, the third track playing.
- The same sequence beside the bridge's player playing: green, as the review said. It stays as a
  guard: the gate refuses the browser's resume at once.
- The two unwired cases: red. The browser's own play sounded. The lock screen's Play played the
  first track, and its Next the second.
- The other 89 cases: green with the harness's pass-through gate wired. The harness change alone
  changes no verdict.

The census's red is a mutation, not HEAD. With a third `createElement("audio")` in `init`, round
4's census passed and the new one failed ("3 found, the census allows 2"). Round 4's census was built
from `efc9a9b4`'s test file. With the mutation removed, the new one passed. The mutated bytes were
restored from memory, and `audio.js` read back as committed.

The new source pin over `5324cd34`'s modules, before the fail-closed change: red, naming the
listener's branch and each missing refusal. The census over the same modules: green.

Five controls, each predicted in writing first, all as predicted:

- Leaving the count alone on load, over a scratch copy of `efc9a9b4`'s modules: red at the
  no-player case alone. The guard stays green.
- Four over the tracked `audio.js`, restored from memory, each run with every bar case, the pin and
  the census (95 tests), the tree clean after each:
  - `requestStart`'s pass-through put back: red at the lock screen's case and the pin.
  - The listener's `&& continueGate` put back: red at the browser's own play and the pin.
  - `ownPlay` admitting the play with no gate: the same two.
  - `continueStart` going on with no gate: the pin alone, since no case reaches it. An unwired boot
    starts nothing a continuation could follow.

### The runs

Each through the shared build lock, on `GOTOOLCHAIN=go1.27.3` with `GOPROXY=off` and node v24.22.3
on the path.

- `make fmt vet test build-all docs-check` and the rest in one session, at `82d8dd4b`:
  - `make fmt` rewrote nothing.
  - `make vet`, and the same for linux/arm, linux/386, windows/amd64, windows/arm64 and
    darwin/arm64: exit 0, 2 to 5 s each.
  - `make docs-check`: exit 0.
  - `scripts/qualification-version -base a79c69b2 -head HEAD`: OK.
  - `make test` at `P=8`: exit 0 in 2,014 s, 93 packages ok, cmd/bridge 1,982 s, internal/admin
    1,519 s.
  - The harness's cases by name with `-v`: 93 of 93 PASS, none skipped.
  - `make build-all`: exit 0 in 26 s. The tree was clean after.
- The source guards, run by name before it, all pass:
  - `TestNoConsoleControlDisablesItselfOutsideSetDisabled`, `TestPlayerEmittedClassesAreStyled`,
    `TestTrackSubgridColumnsMatchTheRow`, `TestAppJSHasNoCallsToDeletedHelpers`,
    `TestByteFormattersAgree`, `TestEveryStartOfThisBrowserAsksTheGate`,
    `TestNoSoundSourceOfTheConsoleBypassesTheGate` and `TestFailureAutoAdvanceIsGuarded`;
  - `TestEveryCitedTestNameExists`, `TestNoBlankKeepers` and
    `TestNoDocblockNamesAnotherDeclaration`.
- No other race run: the server did not change.

### Residuals

- A boot that throws before it wires the gates mounts no bar. Nothing on the page says why the lock
  screen's Play does nothing.
- Only the source pin holds `continueStart`'s refusal with no gate: no case reaches it.
- These stay for the follow-up:
  - the spinner left on when a wait is cancelled;
  - the reader calls the count of actions leaves out;
  - a later call's mark taken by an earlier event;
  - a second start in one playback;
  - the harness's promises on a load, and its missing pending-play case;
  - the earlier rounds' Lows.
