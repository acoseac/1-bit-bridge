## 2026-11-23 — B634 PR 1, round 2: the Stop's order, effects through the waiter, records with their publication

### What

Seven findings against round 1's head (`43dad598`), each checked there by line, with the five
points that bind them (S1 to S5), fixed on this branch once main's `9b66c2af` was merged in
(`1c505a76`). One engine change, the waiter's mark (item 2); everything else is the control's.

### The findings, and what changed

1. **A Stop let an overtaken step play beside the browser** (`90c8e0d8`). A step checks whether
   its operation's unsent steps were ended, unlocks, then sends; a Stop waits for nothing and
   cleared the control's current operation when it settled, so a switch-out admitted after it,
   or behind it, waited for nothing, and a replace's PlayID past its check reached the engine
   after the switch-out's Pause, starting the bridge from Stopped. Handing the current operation
   back at the Stop's settle (the first fix proposed) changes nothing for a switch-out admitted
   before the Stop settles, which has taken the Stop's end already, nor for two Stops in a row
   (S1): the control keeps every operation admitted and not yet ended, each new one ends their
   unsent steps, and every one but a Stop waits for each of them. The step's in-flight channel,
   written and never read, is gone.
2. **A refusal through the play waiter read as no effect** (`8dc87ce9`). A play command's rule
   moves the selection and releases or starts a session before it waits; the engine can then
   refuse it `release_unconfirmed` at the release's deadline, `output_unavailable` as the output
   goes, `maintenance` as a hold begins, or `output_unknown`, `configuring` or `empty_queue` after
   a release, and the control read each by its reason alone, `none`. The engine now wraps every
   refusal it answers a waiter with in `engine.ErrAfterChange` (its three answer sites: the
   settle, a hold's grant, the shutdown), the reason and the text the refusal's own, and the
   control reads it `unknown`. A refusal decided before the rule ran (a play sent once the release
   is unconfirmed) stays `none`.
3. **A final record could be read before its publication** (`1e73d2a6`). The publisher sets a
   record final on its capture, its silence the next revision, and publishes that capture right
   after; the handler's answer past its wait, the read back (which read the revision first) and a
   retry at the tab's mark (which read under the control's lock, S2) could pair it with the
   publication before. One read, `published`, now serves the three: a final record whose
   operation has not ended waits for that end within `publishWait` (1 s) and reads as running, its
   silence and handover dropped, past it; the record is read again once its publication is made;
   the revision is read after, whether or not the end had come (S3); a retry reads outside the
   lock. A publisher that ended before a final evaluation, or between it and its publication,
   stood in a capture of the caller's and answered the old publication (S4): `ask` now says
   whether a final ask's publication was made, and an operation with none ends `unknown`, no
   silence, no handover.
4. **The closed engine's silence wait heard neither its end nor the stop** (`15a006b3`): a newer
   operation that ended it waited behind it to the barrier's bound, and after the control's stop
   the loop spun to its deadline. It now ends at its operation's end, cancelled, and at the stop,
   unknown.
5. **An engine closing under the barrier** (`15225a99`): settle never watched the engine's end,
   polling a frozen snapshot to the bound, and a Pause answered `closed` ended the switch-out
   unknown. Settle now answers closed once the engine's Run has returned or its snapshot shows it
   closing, and both go to the closed engine's judgement (K7), the handover carrying whether the
   bridge was playing when the barrier began.
6. **A store error inserted a track with no names** (`a24303cd`): `entryFor` dropped a
   `GetTrack` error unlogged and took a row gone since the catalog's read for an empty one. Either
   now leaves the track out `not_in_library`, the error logged.
7. **Tests for the limits, the read back and a cancelled preparation** (`5b72657a`). A track
   whose tag puts its item over 64 KiB, the window past 16 MiB, a reference over 128 bytes, and
   the read back's handover hydrated through the console's own route had no test. And a
   preparation went on after its operation was ended, the next operation waiting for it; stopping
   the loop early would have let the rebuilding take tracks never checked for entries that passed
   (S5): the preparation asks before each track, answers cancelled and nothing else, and a replace
   and an append finish cancelled before reading any of it. The rig holds a Start by its track's
   path since `ed1cc19f`, so no case spells the rig's address.

### Found while building

- **The control's stop could miss an operation it had admitted** (`a65ab12f`). `run` counted an
  operation for the stop's join after `admit` returned, outside the control's lock, so a stop
  that closed the control in between waited for nothing it had admitted. The first gate's
  `make test` found it: under `-race` item 4's stop case reported the join's `Add` beside the
  stop's `Wait`. `admit` now counts the operation in the step that admits it, under the lock the
  stop closes the control under; `TestTheControlsStopJoinsAnOperationAdmittedJustBeforeIt` holds a
  pause admitted and not yet started while the control stops.

### Red first

Each item's cases run at the code before its fix, by an overlay of that commit's files
(`go test -overlay`, the tree untouched), the seams a case parks at inserted where it names one:

- Item 1, at `1c505a76` with the step's `beforeSend` seam: all three cases of
  `TestAStepAStopOvertookInFlightReachesTheEngineBeforeTheSwitchOutsPause` red, "the replace sent
  play_id after the switch-out's pause" and "the bridge plays once the switch-out has answered".
  Let go at the switch-out's Pause, the PlayID showed only the first: the barrier paused again;
  the case lets it go once the switch-out's answer is published.
- Item 2, at `90c8e0d8`: the three cases of `TestARefusalThroughThePlayWaiterIsAnEffectNotKnown`,
  effect none.
- Item 3, at `8dc87ce9` with the read back's `readRecord` seam between its two reads: the seven
  cases of `TestAFinalRecordIsReadOnlyWithItsPublication`,
  `TestAReadBackReadsItsRevisionAfterTheRecordsPublication` and
  `TestASwitchOutThePublisherNeverPublishedIsUnknown`, each a done record whose silence names a
  revision its publication has not reached (silence 2 at 1; the read back's 3 at 2).
- Item 4, at `1e73d2a6`: `TestASwitchOutWaitingForAClosedEnginesFactsHearsItsEnd`, the pause
  answered running behind the wait, and the switch-out running after the stop.
- Item 5, at `15a006b3`: `TestAnEngineThatClosesUnderTheBarrierIsJudgedByItsClosedFacts`, refused
  `release_unconfirmed` at the barrier's 3 s, and unknown for the closed Pause. Its first form,
  settle waiting on a held Start, passed at the base: the engine's stop released the starting
  session and showed it stopped before it showed itself closing, a silence the base rightly took.
  It holds a release instead, which the stop cannot finish.
- Item 6, at `15225a99`: `TestATrackWhoseRowCannotBeReadIsLeftOut`, done with four steps sent.
- Item 7, at `a24303cd`: `TestACancelledPreparationLooksUpNothingMoreAndSendsNothing`, three and
  two lookups. The limits' cases and the route's hydration pass at the base, as cases of what was
  already right do; their controls are their pins.
- The join, at `1419bb24` with the `admitted` seam at `run`'s start, before its count:
  `TestTheControlsStopJoinsAnOperationAdmittedJustBeforeIt`, "the control's stop returned while
  an operation it had admitted had not run".

### The controls

Each the fix taken out as an uncommitted edit, its named cases run, the bytes put back from memory
and the tree asked clean after each; predictions written before each ran (`.tmp`, not tracked).
All red for the reason predicted but where the table says:

| | Taken out | Red |
|---|---|---|
| R1 | the chain (a non-Stop waits for the operation admitted just before it alone) | item 1's three cases, both messages |
| R1b | the older ones' unsent steps ended (round 1's M3 on the new code) | `TestALostReplaceHeldBetweenStepsThenAnotherPickNeverPlaysBoth`, `TestANewerOperationEndsTheOldersUnsentSteps` |
| R2 | the engine's mark | item 2's three cases, effect none |
| R2b | the control's reading of the mark | the same |
| R3 | the wait for the publication | the answer's and the retry's four cases; the read back's and S4's green, not theirs |
| R3b | the read back's revision read after the record | the read back's case, and also the first case's read back made just after its let-go: the same defect |
| R3c | an ended publisher's fallback evaluated | S4's first case |
| R3d | the evaluated record kept with no publication | S4's second case |
| R4 | the wait's end on its operation's | item 4's first case |
| R4b | the wait's end at the stop | item 4's second case |
| R5 | settle watching the engine's end | item 5's settle case |
| R5b | a closed Pause to the closed engine's judgement | item 5's Pause case |
| R6 | a row that does not read left out | item 6's case, the nameless entries inserted |
| R7a | an item over 64 KiB left out | `TestATagOverTheEnginesLimitIsLeftOutBeforeDeleteAll`, DeleteAll sent |
| R7b | the window's metadata bound | `TestTheWindowGivesUpMetadataPastTheQueuesTotal`, its three cases |
| R7c | the reference's length check alone | green: `engine.NewReference` refuses past 128 bytes too |
| R7c2 | both checks | `TestAReferenceOverTheLimitIsLeftOut`, the 129-byte reference taken |
| R7d | the preparation's ask between tracks | S5's two cases, three and two lookups |
| R7e | a cancelled preparation finished cancelled | S5's two cases, refused `not_playable` |
| R7f | the route's rows for the handover | `TestTheReadBackRouteHydratesItsHandoverThroughTheCatalog` |
| RJ | the join counted in `admit` (counted in `run` again) | `TestTheControlsStopJoinsAnOperationAdmittedJustBeforeIt`, the stop returning at once |
