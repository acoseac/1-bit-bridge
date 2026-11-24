## 2026-11-23 — B634 PR 2, round 2: the bar never plays in both places, never sticks moving

### What

Three reviews of round 1 (`33caa109`; its rules line `2f94ce48`; main merged at `725e8134`) found
that ordinary use could play in both places, or in neither: a row's click, Play, Next or a
lock-screen control started this browser beside the bridge's player playing for another
controller, since only the switch's own starts crossed the barrier; a pick refused before it became
an operation (`epoch_changed`, `operation_conflict`, a body's bound) ended nothing, the bar saying
"Moving…" for ever with every control disabled; and a slow pick of the bridge followed by a pick of
this browser let the first one's late record set the output back to the bridge, so Play sounded
both. This round changes the player's modules (`output.js`, `audio.js`, `nowplaying.js`, `views.js`,
`boot.js`), `player.css`, the bar's harness and its cases, and the records. The server is unchanged:
C7 is refuted below, so no race run is owed.

### What round 1's entry said that round 2 changed

- Round 1 said every start of the browser goes through the barrier. That held for the switch's
  starts alone (§3.2's three); a row, Play, Next and the MediaSession started the element directly.
  Every start now asks one gate (R1).
- Round 1 said session storage keeps one operation only, so a reload can read back the one it
  inherited. It kept one slot, cleared at boot, so a second reload read nothing, and a conflict's new
  identity orphaned the older operations. It now keeps every operation not yet resolved, by epoch,
  tab and count (R7, V3).
- Round 1 called a record read back history whose start waits for a new barrier's `done` (G8).
  A copy of the tab carries that record in its session storage and started the browser beside the
  original's. A record another document left now starts nothing (R8). The sending tab's own lost
  switch-out keeps G8.
- Round 1 said a failed clock sample is asked again at most every 5 s, and that the offset is taken
  over the epoch and the connection. The code retried only once `offered` was known, and takes the
  offset over the sampling generation. The records now say what the code does (C10, V6).

### The items

- **R1 (V6, V7), the gate.** audio.js's starts route through `requestStart` to output.js's
  `gateStart`, set by boot.js before the bar mounts. The callers found:
  - `views.js`: `playOn` (a row's title, an album's and a playlist's Play) and the Shuffle button,
    now one `playQueue` with `shuffle`.
  - `nowplaying.js`: Play (`audio.toggle`), Previous and Next (`audio.advance`).
  - The MediaSession's play, pause, previous and next handlers, which call `toggle` and `advance`.
  - output.js's own starts, all after a barrier's `done`: `take`, `resume` and `start`.

  Continuations, not starts: the element's `ended` (`advance` with `auto`), the skip timer after an
  unplayable source, the error retry (which asks the start's permit), and `removeAt` (no callers;
  keeps what plays). With GET now's typed not-offered refusal the start goes on at once (a box that
  offers no player sends no frame). Before that is known, or with a player offered and no epoch, the
  start waits; it is dropped after 10 s with its line. A first GET now that failed is asked again
  within 5 s. A pause never waits.
- **R2 (V2, V8).** The structured refusals from the code (the not-offered words, the body's bounds
  and shapes, `catalog_unavailable`, the playlist read's `internal`, `cross_origin`,
  `epoch_changed`, `replayed`, `operation_conflict`, a closed control's `player_not_running`) end
  the switch-in, the switch-out and the barrier, each with its line. Any other answer without a
  record leaves the operation unconfirmed.
- **R3 (V11).** A pick generation: an operation sent under an older pick moves neither the output,
  the phase nor the browser, whatever its end. A pick of the bridge with an empty browser counts too.
- **R4 (V4).** A switch-out's and a barrier's `done` read the latest accepted state through a
  permit, which the deferred play to the metadata and the error retry ask again. A refused switch-in
  judges silence on the latest accepted state.
- **R5.** A switch-out's, a switch-in's and an edit's stream record (no handover, no tracks left out)
  triggers a read back, which alone completes it.
- **R6 (V1, V10).** A request 30 s old is expired and lost. Its answer is not read, and nothing after
  the await acts for it.
- **R8 (V5, V9).** An inherited switch-out takes its handover stopped, with "press Play". An
  inherited switch-in refused with nothing changed resumes nothing.
- **R9 (V12).** `command("index")` is refused and the panel's rows are disabled while the queue is
  behind `queueRev`. `queueRev` is in the panel's key; a removal by entry ID stays enabled.
- **R10.** At 375 px the transport (182 px) and Queue, Output and Mute (112 px, with gaps) left the
  words 23 px, and the 40 px cover with its 10 px gap took all of it. Below 640 px with the bridge
  chosen the cover and the disabled Mute are not drawn: 23 + 40 + 10 + 32 + 8 = 113, less the 50 px
  that `.np-now` no longer spends on the cover, leaves 63 px for `.np-meta`.

The checks, C1 to C10:

- **C1 holds**, fixed: a recovered switch-out read back changed, refused or unknown now says so.
- **C2 is refuted.** `settleUnknown` takes a handover only where one was stored, and a switch-out
  stores none (round 1's :787 and :651).
- **C3 is refuted.** An answer from a new epoch carries no record, since the new serve answers an
  old epoch's command `epoch_changed` before admission (`admit`, :610). A new-epoch state dropped
  outside a reconnection is overtaken by the new connection's first frame, published after it.
- **C4 holds**, fixed: a queue that is behind is read again within 2 s.
- **C5 is refuted** as a defect. By M1's rule a sample belongs to its connection and epoch, and the
  load's has neither. The epoch's first frame asks again, and the load's answer still settles
  `offered`. The records now say so (C10).
- **C6 is refuted.** Each view draws one track list at a time (the album's source filter clears its
  box first), so `why-<i>` is unique in the document.
- **C7 is refuted** against round 1's L1 text. `prepare` asks `endedNow()` at every distinct track
  (:1274), after `Member` (:1305) and after the loop (:1315).
- **C8 holds**, fixed: the queue closes on Escape, and the queue and the menu on an outside click.
- **C9 holds**, fixed: a focused pick going disabled hands the focus to the other pick.
- **C10 holds**: the records are corrected.

### Red first, and the controls

22 cases: 19 new, 2 new structural on every host (`TestEveryStartOfThisBrowserAsksTheGate` walks
audio.js's call graph; `TestThePhoneBarKeepsItsWordsWithTheBridgeChosen`), and
`TestARecoveredHandoverIsHistory` restructured to the sending tab's own lost switch-out. They ran
against `725e8134`'s modules with a names-only shim: `opKey`, the bounds, a `gateStart` that starts
at once as round 1 did, and a no-op `setStartGate`. 20 were red, each for its behavioural reason.
The 2 green by design are `TestADeferredStartPlaysWhileItsPermitHolds` (V4's positive side) and the
restructured G8 case.

30 controls, each the fix taken out as an uncommitted edit over the whole bar family, the tree clean
after each, were all red at their named cases. This record names none of them, so it shows their
count alone (corrected by B1005's entry). Two needed their case sharpened first:

- V2's (every no-record answer taken for a refusal): the switch-in case passed through a barrier
  that was itself lost. It now also requires the output still the bridge and no barrier sent.
- R4's (a refused switch-in judged on its answer's state): the gate's own check of the latest state
  masked it. It is now shown by a hold in the latest state, which is not silent though nothing
  sounds.

### The runs

Each through the shared build lock, on `GOTOOLCHAIN=go1.27.3` with `GOPROXY=off`.

- `make fmt vet test build-all docs-check`, one session. The first, at `db0db736`, failed in
  `make test` at PR 1's `TestARefusalThroughThePlayWaiterIsAnEffectNotKnown`, its maintenance case:
  "enter maintenance: generation_conflict". The case reads the engine's generation once its Start is
  parked and enters maintenance with it, so the generation moved between the two. Round 2 changed no
  server code or test, and round 1's two gates passed the case. Its cause is not established here,
  and the round's budget takes no server change, so it is left for its own follow-up. The second
  session, on `db0db736`'s code (its child, `f8d93093`, adds this record alone): exit 0 in 1,955 s, 93 packages ok, the six cross-builds and docs-check with its
  self-tests; `make fmt` rewrote nothing.
- The release-tag vets, `make vet` for linux/arm, linux/386, windows/amd64, windows/arm64 and
  linux/amd64: exit 0, 4 to 6 s each.
- The source guards, run by name with `-v`: `TestNoConsoleControlDisablesItselfOutsideSetDisabled`,
  `TestPlayerEmittedClassesAreStyled`, `TestTrackSubgridColumnsMatchTheRow`,
  `TestAppJSHasNoCallsToDeletedHelpers`, `TestByteFormattersAgree`, `TestEveryCitedTestNameExists`
  and `TestNoBlankKeepers` pass.
- `scripts/qualification-version -base a79c69b2 -head HEAD`: OK.
- No race run: the server did not change.

### Residuals

- PR 1's `TestARefusalThroughThePlayWaiterIsAnEffectNotKnown`, its maintenance case, failed in this
  round's first gate session ("enter maintenance: generation_conflict"), its cause not established,
  and is left for its own follow-up (the runs above).
- Two documents of one tab coordinate nothing beyond R8's rule; a cross-tab lock or channel is not
  this round's.
- A reload no longer resumes by itself.
- Every browser start while a player is offered waits for the barrier's round trip. A browser that
  lets a page play only within its user gesture (WebKit's rule) may refuse the play that follows
  the await with NotAllowedError, which the bar shows as "Press play to start."; the browser session
  should try Safari.
- The element's own advance at a track's end continues beside a bridge another controller started
  meanwhile. That is a continuation, and the console stops neither (§3.3).
