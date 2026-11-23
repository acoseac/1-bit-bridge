## 2026-11-23 — B634 PR 2, round 3: no start outside the gate, no latch that outlives its barrier

### What

The reviews of round 2 (`b5123238`) found four routes by which the bar still played beside the
bridge's player or stopped answering:

1. What playback does by itself started this browser without the gate: a track's end, a
   repeat-one replay, the skip past a source that cannot play and a removed row's successor,
   while another controller had started the bridge.
2. The retry after a network or decode error asked the start's permit only where one existed, so
   a take made stopped, or a queue restored at page load, that met a Range fetch's failure after
   its metadata played beside the bridge, or beside the tab it was copied from.
3. A barrier whose request never settles (a half-open connection) is lost at 30 s and resolved by
   its record, but its latch was released only by its answer or its request's promise: every
   later start joined it, and nothing played or said why until a reload.
4. A switch to this browser whose answer was lost, superseded by a pick of the bridge that sends
   nothing (an empty browser), was stored with no word of the pick: a reload restored it as the
   newest pick's and its record read back moved the output back.

This round changes `audio.js`, `output.js` and `boot.js`, the bar's harness and its cases, and the
records. No server change, route, protocol field or server state.

### The items

- **S1 (Q1, Q3, Q5, Q7).** What playback does by itself goes on only through audio.js's
  `continueStart`, which asks `gateContinue`, an interface of the output's that boot.js wires
  beside the start gate (Q7: a continuation check on the output, not a permit that speaks). The
  next track at a track's end and the retry after an error go on their start's permit, asked
  again, the bar saying that the bridge plays where it no longer holds. A start let go on where
  no player was offered holds no permit, and what follows it asks the gate (Q1). So does a
  continuation that starts the element anew: the skip, the repeat-one replay (a track's end has
  paused the element) and a removed row's successor (loaded stopped). A continuation never
  displaces a start the reader asked for; a start the reader asks replaces one. When the gate
  lets it go on, it asks again whether this browser still means to play what it continues: no
  load since, and no pause (Q3). Until it runs, the browser shows itself stopped.
- **S2 (Q8).** That intent is audio.js's own, since the element cannot say it: load() pauses it
  with no event, and a track's end pauses it before `ended`. A load that means to play sets it,
  and so does a play a start or a continuation lets go on. A load that does not, the reader's
  pause and the browser's own pause (any pause but the end's), `suspend`, `clearQueue`, the stop
  at the queue's end and a restored queue each clear it. The retry keeps it from before the
  reload and plays only with it. The skip's timer reads it where it read `state.playing`.
- **S3 (Q2, Q9).** A barrier's latch is released first by every end of its operation (its done
  however it came, unknown, refused) and by a newer pick, never by its request's promise. A
  barrier resolved after its answer was lost starts nothing and says "This browser did not start:
  press Play again"; Play sends a barrier of its own.
- **S4 (Q4, Q10).** Session storage keeps with each pending operation whether a newer pick has
  superseded it (`stale`), written by every pick as it is made. A reload restores a superseded one
  with a pick no pick there takes, reads it back until it resolves, and holds the phase only for
  the operations no pick superseded.
- **Q6.** The harness's element pauses on `load()` with no event and resets its position and
  error, pauses at a track's end before `ended` (`end()`), records every `play()` with its source,
  and lets a case hold the modules' long timers and fire them.

### Checked first

- **A pick of the bridge from an empty browser advances `pickGen`** (round 2's `pick`). So
  within the document, a superseded switch moves nothing. What the pick did not do was store
  it, so a reload lost the supersession: S4 stores it at the pick.
- **C2's refutation holds.** Round 2's line describes round 1's `settleUnknown`, which C2 cited:
  it took a handover only for a switch-out that had stored one, and none did. Since round 2,
  `settleUnknown` takes nothing at all: its inherited branch sets the phase and the line. Nothing
  there takes or wipes a queue.
- **A gated start that a refusal drops.** A switch to the bridge refused with a not-offered word
  resumes at once, since its `gateStart` then finds no player offered; one refused otherwise
  resumes through a barrier where the browser played. A barrier refused with a not-offered word
  dropped its start; it now lets it go on, with no permit to ask
  (`TestABarrierRefusedForNoPlayerLetsItsStartGoOn`). The barrier's other refusals still drop
  the start with their line, since none of them proves the start safe.

### Red first, and the controls

Fifteen new cases, all under node (v24.22.3). The round's new harness was run over `b5123238`'s
modules: all fifteen were red, each for its behavioural reason (a play beside the bridge, a
continuation past the gate, a latch that sent no barrier, a reload that held the phase and moved
the output). `TestEveryEndOfABarrierFreesTheNextStart`'s refused phase is green there by design,
since the request's promise released that latch; its other phases are red.
`TestEveryStartOfThisBrowserAsksTheGate` now holds every continuation to `continueStart` and the
continuation gate's wiring.

Thirty controls over scratch copies of the committed modules, no tracked file touched, each
predicted in writing before its run, at the commit that made its item: all as predicted.

- The generation's re-check is green alone, as predicted. Every change of the playback either
  clears the intent or replaces the continuation at the latch, so it is a second layer.
- The run marked before the re-check is green alone and red with the retry's own intent check
  taken out.
- The first run of the retry's intent control, at `350d585d`, found that `continueStart` marked
  a continuation run before its re-check, so one refused there kept a stale playing flag.
  `ed603a62` fixes it, and the control, run again, is red only where a retry with no intent asks
  for a barrier.

Run again at the round's last code commit (`c4dfef67`), every control is as predicted, with two
exceptions:

- The S1 control that silences `gateContinue`'s line is also red at
  `TestARetryPlaysOnlyWhatThisBrowserMeantToPlay`, a later case that reads that line too.
- The pick's latch control was anchored again after S4 changed `newPick`; it is red as predicted.

Five more controls edit the tracked modules for the structural guard and restore them from
memory: all red, the tree clean after each.

### Residuals

- The harness fires an element's events at once and its play() always resolves. A browser fires
  `pause` a task after `pause()`, so toggle clears the intent itself first.
- Beside an offered player, each repeat-one loop, each skip and each removed row's successor
  crosses a barrier: one round trip, and the console's own Pause of a silent player.
- The half of the rule that keeps a continuation from displacing a start the reader asked for,
  where that start waits for the player's word (`waiting`), has no route today. Once GET now has
  said that no player is offered, the bar never asks it again, and no playback exists before the
  first frame. It is kept so that the gate has one rule.
- Round 2's Lows stay for the follow-up after the merge.

### Not run here

The browser session: the skip past a source that cannot play, once, through the gate, in a
browser that rejects a failed source's play; the gap a repeat-one loop takes beside an offered
player; the bar's line and its stopped Play while a continuation waits on its barrier; a lock
screen's pause and a headset's while a skip waits; the 375 px measurement.
