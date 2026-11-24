## 2026-11-24 — B634 PR 2, round 4: no play the bar did not start sounds outside the gate

### What

The reviews of round 3 (`c9c80855`) left two Mediums and one Low, all in one class: a start that
does not ask the gate.

1. The browser resumes the element by itself when a call ends or another app gives the audio focus
   back. The `play` handler took that as the bar's own, so the browser sounded beside a bridge
   another controller had started meanwhile. At that track's end the album halted with no line,
   since the browser's pause had cleared the intent.
2. A refused continuation kept the intent. Once the bridge stopped, removing the current row
   started its successor through a fresh barrier, a start the reader never asked for.
3. The harness's element could not show a refused or a pending play: `play()` always resolved.

This round closes the class, not only the two instances. It changes `audio.js` and `output.js`,
the bar's harness and its cases, the bar's Go test files and the records. No server change, route,
protocol field or server state.

### The items

- **T1 (W1, W3, W4, W5, W8).** Every play the bar makes goes through `playEl`, which marks it:
  - The mark is single-use (W1). The `play` event the call causes takes it; the call's promise
    clears it once it settles, if no event took it; and every load takes all, since the load
    algorithm removes the events they wait for. A play on an element already playing makes none.
  - A play with no mark is the browser's own. `ownPlay` pauses the element at once, in the handler,
    and asks the gate as a fresh start with a continuation's priority, so it never takes a
    reader's place (W5).
  - Admitted, it resumes through `playEl` and sets the intent, but only where the playback is the
    same and the reader has done nothing since (W4). A count of the reader's transport calls, and
    of every pause that is not the bar's own, says so.
  - The protective pause's event is the bar's own and cancels nothing, so an admitted resume goes
    on at the track's end (W3). While it waits the bar shows it starting, and Play/Pause cancels it.
  - A play the browser refuses (`NotAllowedError`, a source it cannot use) ends the intent wherever
    the bar made it, the retry and repeat-one included (W8). It says "Press play to start." unless
    the bar's line says the bridge plays.
- **T2 (W6, W7).** The gate takes a drop from whatever asks, and calls it once on every way that
  never runs a request:
  - `gateContinue`'s refused permit;
  - the start gate's refusals (the bridge sounding, this browser not the output, the wait dropped
    at 10 s);
  - a start that replaces a waiting or pending one, and a pick;
  - the barrier's ends that start nothing.

  `continueStart`'s drop ends the intent for its own playback generation. In review, a deferred
  start its permit refuses (V4's path) showed the same fault: once the bridge went silent, a retry
  after an error started it again. Its refusal ends the intent too (`fe94d387`).
- **T3 (W2, W11).** The harness's element follows the media element's steps:
  - `play()` on a paused element flips `paused` at once, and its `play` event and its promise's
    settlement come later, as queued tasks. A settlement resolves only the promises pending at its
    call, as the spec's notify about playing does. Resolving every pending play let one call's
    settlement take a later call's mark.
  - `pause()` queues its event with the `AbortError` of every pending play.
  - `load()` removes the queued tasks and refuses the pending plays.
  - A track's end and a media error are queued too, code 4 refusing the pending plays.
  - A case can hold a play pending or have it refused at once.

  Every existing case passes unchanged under it, at `c9c80855`'s modules and now. The skip cases
  keep their order (the error after the metadata, the play settled), now said to stand for a
  swallowed rejection; a realistic order is the follow-up's (W11).
- **T4 (W10).** `TestNoSoundSourceOfTheConsoleBypassesTheGate` reads every script under `static/`,
  comments stripped, and the templates' markup. It looks for a media element's `play()`, autoplay
  by property, attribute or markup, an audio or video element, an Audio object, a Web Audio
  context and a MediaSession handler, each placed in the top-level function holding it. The sites
  it allows:
  - `.play(`: `audio.js`'s `playEl`.
  - An element: `audio.js`'s `init` (the element that plays and the preloader, never played) and
    `format.js`'s `engineCanPlay` (the `canPlayType` probe, never played).
  - A handler: `audio.js`'s `setMediaSession` (it clears them) and `wireMediaSession`, whose eight
    handlers must be the ones that start only through `toggle` and `advance`.
  - Autoplay, an Audio object, a Web Audio context and media markup: none.

  A named site that holds none fails it too.
- **T5.** Under CI, `barScenario` fails where node is missing, so a CI green means the bar's cases
  ran; a host without node still skips them.
- **W9.** A start let go on where no player was offered holds no permit. Its play deferred to the
  metadata now asks the gate that holds then; with still no player offered it plays as before.

### The refutations

K1 (`loadAt`'s deferred play under a null permit) was reopened by W9 and is fixed above. K2 to K5
are kept as the brief's review confirmed them by line:

- K2: every gate hands a permit, every play path stores it, and a stale one is checked against the
  newest state.
- K3: `requestStart`'s late run after "not offered" becomes "offered" plays what this browser meant
  to play, after a barrier came back done with silence.
- K4: every end of the barrier releases the latch first, and `waiting` exists only before the
  player's word, its drop sending nothing.
- K5: `pick("browser")` with inherited operations: a switch-in sets the output before its send, and
  the other inherited operations start nothing.

### Red first, and the controls

The harness's new cases are ten: seven for T1, one for T2 and two for W9. Two cases were
extended: `TestARemovedRowsSuccessorAsksTheGate` (W7: four tracks, and a last removal that starts
nothing) and `TestADeferredStartAsksItsPermitAgain` (the deferred start's refusal). Against
`c9c80855`'s modules:

- Every new case is red for its behavioural reason, but W9's no-player side, which is green there
  by design.
- Both extended cases are red at their new checks.
- The census, run over `c9c80855`'s modules in the tree, is red: five plays outside `playEl`, and
  no `playEl`.
- T5, with `c9c80855`'s test file under CI and no node: the case skipped.

Eighteen controls over scratch copies of the committed modules, each predicted in writing first:

- T2's four (the refusal keeping the intent, the brief's; no drop at the permit's refusal, at the
  sounding refusal, at the barrier's refused permit): all as predicted.
- T1's nine (the brief's three: no default-deny branch, a sticky mark, no re-check when the
  answer runs; and one each for W3, W4, W5, W8 and W1's mark per call, alone and with a sticky
  mark): all as predicted, two with one more red. Taking the protective pause for a cancellation
  also dropped the "starting" state T1(d) checks. Asking the resume as a reader's start left a
  refused resume undropped, so T1(c)'s later Play cancelled it instead of playing. A mark for every
  call alone is green: its settlement still takes it.
- W9's two: as predicted. The deferred play that never plays is also red at the W9 case's line,
  which only the gate's refusal says.
- T3's: the skip let go past the gate, under the queued model, is red at the skip and the three
  barrier cases.
- The deferred start's refusal keeping the intent: red as predicted.

Seven controls edit the tracked modules for the census and restore them from memory: an unmarked
`play()`, an Audio object (the brief's two), autoplay, a Web Audio context, a third element, a
handler that plays, and autoplay markup. All were red, the tree clean after each.

### The runs

Each through the shared build lock, on `GOTOOLCHAIN=go1.27.3` with `GOPROXY=off` and node v24.22.3
on the path.

- `make fmt vet test build-all docs-check` and the rest in one session, at `998568f4`:
  - `make fmt` rewrote nothing.
  - `make vet`, and the same for linux/arm, linux/386, windows/amd64, windows/arm64 and
    darwin/arm64: exit 0, 2 to 5 s each.
  - `make docs-check` and its self-tests: exit 0.
  - `scripts/qualification-version -base a79c69b2 -head HEAD`: OK.
  - `make test` at `P=8`: exit 0 in 2,014 s, 93 packages ok, cmd/bridge 1,982 s, internal/admin
    1,516 s.
  - The harness's cases by name with `-v`: 89 of 89 PASS, none skipped.
  - `make build-all`: exit 0 in 26 s. The tree was clean after.
- The source guards, run by name before it: `TestNoConsoleControlDisablesItselfOutsideSetDisabled`,
  `TestPlayerEmittedClassesAreStyled`, `TestTrackSubgridColumnsMatchTheRow`,
  `TestAppJSHasNoCallsToDeletedHelpers`, `TestByteFormattersAgree`,
  `TestEveryStartOfThisBrowserAsksTheGate`, `TestNoSoundSourceOfTheConsoleBypassesTheGate`,
  `TestEveryCitedTestNameExists`, `TestNoBlankKeepers` and
  `TestNoDocblockNamesAnotherDeclaration` pass.
- internal/admin whole, without `-race`, at `d36d53ef` (T1): ok in 351 s.
- No other race run: the server did not change.

### Residuals

- Whether `play` fires when a call ends, and whether sound escapes before the handler's pause, is
  the device check's.
- A browser that lets a page play only within a gesture may refuse the admitted resume's play after
  the gate's await. The bar then says "Press play to start.", and the reader presses Play.
- The harness's code-4 failure refuses the pending plays and leaves `paused` false, as the spec's
  dedicated media source failure steps do. No case this round depends on it.
- The generation scope of a continuation's drop has no route that delivers a stale drop today:
  every change of playback clears the intent or replaces the request at the latch.
- The player modules grew by 221 lines (168 code, 46 comment), against the brief's about 150.
- Round 2's and round 3's Lows, the browser session's L1 and L2, and the skip's eligibility in a
  realistic order stay for the follow-up.
