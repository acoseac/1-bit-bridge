# B634: the console's player bar plays to the bridge's own DAC

Design and PR plan, 2026-11-22, for bridge 0.2.4. Docs only: this round writes no code.

**Status.** For review, round 2, on the branch head `808b19ad`, whose code is bridge main `9f98e0c1`'s
(the two commits between them change this file alone), so every `path:line` cited holds at both.
Round 1 (`e58896c2`) settled R1 to R12. Round 2 takes the decision of 2026-11-22, option
B (§1), and settles C1 to C5 under the binding E1 to E12; §15 says where each is settled, and §14
where round 1's R1 to R12 now are. The owner's decision of 2026-11-22 stands. The parked 11-13
design of a larger B634 stays input only: its passcode, its LAN listener and its live DAC switch
are not part of this design.

**What round 2 changes.** The compound edit is an OpenHome controller's, the last admitted command
winning, in place of round 1's chain of generations (§2). Every console command is an operation
with an id, run one at a time on the server, whose outcome is published (§2.3). The browser starts
only once the bridge's silence is published after the console's own Pause (§3). One publisher
orders every state the bar takes (§4). An entry's track URI and its artwork URL are built apart,
every limit is checked before `DeleteAll`, a long list is windowed, and a paused handover keeps the
queue (§6).

**In one paragraph.** An output button on the console's player bar picks *this browser* or *the
bridge's player*. With the bridge's player picked, the bar drives the player's engine through an
admin-only control API under `/api/output/player/`, follows it on the console's existing event
stream, and edits the engine's queue as every OpenHome controller does: a replace is a `DeleteAll`
and its inserts, the first step refused when the tab's view is stale, the later ones unconditional,
and the last admitted command wins. The console claims no exactness the other controllers lack,
and says what happened: it reports success only when the state after its last step is what it
made, and otherwise that the queue or the transport changed, with the actual state. Each command is
an operation with an id whose outcome the stream publishes, so a lost answer is resolved by
watching, never by sending the command again. The engine stays the one owner of the queue and the
state and gains no command. DSD plays only on the bridge's player, by its DSD policy. A switch moves
the queue, the track and, for PCM, the position from one output to the other, and the browser
starts only once the bridge is known silent.

## 0. What changes, at a glance

| Today | Where | B634 |
|---|---|---|
| The bar plays the library in the browser alone, through one audio element | `internal/admin/static/player/audio.js:1-5`, `:340-358` | The bar plays to this browser or to the bridge's player, picked by an output button (§7) |
| The bar hides while nothing is selected | `internal/admin/static/player/nowplaying.js:182-188` | On a box that runs the player the bar stays, empty, to offer the output (§7.1) |
| A DSD row has no play handler and reads "DSD — not playable in a browser" | `internal/admin/static/player/views.js:745-770`; `format.js:179-185` | With the bridge's player picked, a row's playability is that player's (§6.7, §7.6) |
| Serve's player plays only what an OpenHome controller inserts, the opt-in off by default | `internal/player/openhome/writes.go:388-413`; `.claude/rules/openhome.md` (the opt-in's bullet) | The console inserts and plays through the engine, OpenHome on or off (§5) |
| The console's view of the player starts nothing | `cmd/bridge/serve_player_console.go:25-33`; `.claude/rules/cli-serve.md` (the console's view's bullet) | A control of its own, beside the view, starts playback through the engine's commands alone (§5.1) |
| The console's event stream carries no player state | `internal/admin/handlers_events.go:96-107`, `:257-265` | A `player` event, one publisher's, quiet through steady playback (§4) |
| The catalog's track rows answer playability for the browser alone | `internal/admin/handlers_player.go:810-889`; `internal/admin/player_audio.go:102-138`, `:245-256` | An additive `bridge` field answers it for the bridge's player (§6.7) |

No `/v1` route, PROTOCOL.md section or iOS change: the app already reads and drives the same engine
through OpenHome, and reads the console's entries in the form of its own inserts (§6.6).

## 1. The hazard, the decisions and the premises

**The hazard.** A Linux box runs `bridge serve` with the player on and bound to a DAC. The console's
player bar plays the library only in the browser (`audio.js:3-5`). The bridge's player plays only
what an OpenHome controller inserts (`writes.go:388-413`), in practice the 1-bit app's cast, and the
surface is off by default. Without the app nothing reaches the DAC, and a DSD-only album cannot be
played from the console at all: its rows have no play handler (`views.js:746`, `:768-770`).

**The owner's decision (2026-11-22).** An output button on the bar picks this browser or the
bridge's player, which the console then drives, so the bridge works without the app. In 0.2.4. The
console on the home LAN is B989's, designed apart; this design works on loopback. DSD plays only
through the bridge's player, by its DSD policy: what the DAC takes natively or as DoP, the rest
converted to PCM (B728 B11, `.claude/rules/player.md`, "Player DSD converted to PCM").

**The decision (2026-11-22): option B.** The console edits the queue as every OpenHome
controller already does, and the last admitted command wins. The 1-bit app's cast is a `DeleteAll`
followed by inserts (the iOS repo's `OpenHomeHandoff.swift`, `:599` and `:629`, as the brief cites
it; that repo was not read for this round), and the bridge's `DeleteAll` and `Insert` are separate,
unguarded edits (`internal/player/openhome/writes.go:119-145`). So the console adds no engine change
and claims no exactness the other controllers lack; round 1's exact chaining (R1, R12) is withdrawn.
**The bounded fallback to option C was not taken**: C1's contract for "replace and play" is stated
in §2 within the budget (no engine change, no second queue owner), and §12 says why B is kept.

**Premises checked for round 2**, each at `808b19ad`:

- `SetRepeat` moves the generation: `setRepeat` marks its change saved
  (`internal/player/engine/edits.go:147-156`), which is a visible change
  (`internal/player/engine/owner.go:736-749`), and a command conditioned on an earlier generation
  is refused before it runs (`owner.go:601-602`).
- A command whose context ends after the owner admitted it answers `ReasonCancelled`, "it may have
  taken effect", and its reply is dropped (`internal/player/engine/engine.go:936-970`).
- A resume starts the backend's call and publishes nothing
  (`internal/player/engine/transport.go:887-897`); only its answer makes the transport Playing
  (`transport.go:1381-1412`). A Pause sent while it is under way waits behind it
  (`owner.go:480-496`).
- The generation moves at most once a step, and the queue file's revision only when the file's
  content changes (`owner.go:751-770`); a snapshot carries no sequence of its own
  (`internal/player/engine/snapshot.go:285-318`).
- An insert is refused past `MaxEntries`, past 64 KiB of metadata, and past 16 MiB of metadata in
  the queue, the entries already there counted (`edits.go:9-41`;
  `internal/player/engine/queue.go:11-21`).
- An insert into an empty queue selects its entry (`edits.go:30-36`); `InsertAfter(0)` inserts at
  the head (`queue.go:139-150`); an insert after an ID the queue no longer holds is refused
  (`edits.go:16-17`).

Round 1's four corrected citations stand: a DSD row reads "DSD — not playable in a browser"
(`format.js:182`); the console server is `Server.ServeListener` in `internal/admin/admin.go:2247-2350`
and its middleware `internal/admin/middleware_auth.go`, there being no `server.go`; OpenHome's
`Read` and `ReadList` are in `internal/player/openhome/actions.go:348-353` and `:384-402`;
`playerRefusal` is at `internal/admin/handlers_player_output.go:436-460`, and the shuffle refusal at
`edits.go:158-165`.

## 2. The concurrency and partial-failure contract (C1)

### 2.1 What the engine gives

- **One owner, a reply after its step's snapshot.** The owner runs every command and message in
  turn, and a command's reply is sent once its step has published its snapshot (`engine.go:1042-1058`;
  `owner.go:287-303`): a snapshot read after a reply is that step's or a later one.
- **IDs name entries exactly.** An entry ID is never handed out twice, by any run, a crash included
  (`internal/player/engine/doc.go:238-240`), and the engine has no move. So a queue's IDs name its
  membership and its order exactly, and an ID the engine handed the console names the console's
  entry and no other.
- **The steps protect themselves.** `InsertAfter(id)` inserts after that entry, or at the head for
  0, and is refused while the queue does not hold `id` (`edits.go:16-17`); `PlayID(id)` plays that
  entry or is refused `entry_not_found`. An insert chained after the console's previous entry, and a
  play of the console's entry, therefore land nowhere once another controller has deleted them.
- **A hold comes before the conditions.** Under maintenance every command but Stop is refused,
  whatever its generation; a Stop does nothing, and under the DSD check's hold ends the check
  (`owner.go:593-600`; `doc.go:98-100`).
- **Every command the engine takes is answered**: by its reply once it has run or a later command
  overtook it, and at shutdown with `ReasonClosed` (`engine.go:936-970`; `owner.go:574-583`). Only a
  context that ends after the admission drops the answer (`engine.go:957-961`), and
  `engine.WithAdmission` says when the owner admitted a command (`engine.go:1000-1016`).
- **Pause** does nothing stopped, paused or releasing, and pauses a playing session; it waits behind
  a session still starting and behind a pause or a resume under way (`doc.go:71-83`;
  `owner.go:480-496`). A release that carries a play (a `PlayID` while playing) plays after a Pause
  sent during it (`.claude/rules/openhome.md`, the bullet on outcomes a controller can read as odd).

### 2.2 How a compound edit's steps go (E1)

**The first step of every console edit carries `IfGeneration(at)`**, `at` being the generation of the
state the bar acted on (§4): a stale tab's edit is refused with nothing changed.

**Every later step is unconditional, OpenHome-style, and the queue's IDs are compared after each
step.** After each step the control reads one snapshot (no command: `Snapshot.Len` and `IDAt`,
`snapshot.go:382-383`, `:419-425`) and goes on only while the queue's IDs are exactly what its own
steps made: the entries it kept (none for a replace, the queue it read for an append) and then the
IDs it was handed, in its order. At the first ID it did not make, or the first of its own that is
missing, it stops and reports `changed`.

- **Only IDs are compared, never the generation.** A repeat setting, a skip, a track's advance, a
  release's acknowledgement or an output report moves the generation and changes no ID, so none of
  them stops an edit (C4).
- **At most one more console insert** lands after another controller's change of the queue: the
  check after a step and the next step are two calls. The edit then stops, and never deletes what it
  inserted.
- **The play runs only on a queue that is the console's alone**, which the check before it shows.
  Between that check and the play the last admitted command wins: a `DeleteAll` there leaves the play
  nothing to play (`entry_not_found`), and an insert leaves it playing the console's entry in a queue
  that also holds the other controller's, which the end reports as `changed`.
- **The seek is the one step under a bounded retry**, since it acts on whatever is heard: it carries
  `IfGeneration` of a publication that shows the console's entry selected and playing; refused for a
  conflict, the control reads again and retries while that still shows, three tries at most, and
  otherwise the track plays from its start, the answer saying why.

**Why the IDs after each step, where the brief offers them once at the end.** Compared once at the
end, a cast whose `DeleteAll` lands between the console's `DeleteAll` and its first insert would take
every console insert at its head, and then the console's play over its own (the parked design's
first-insert finding). Compared after each step, the console stops at its next check, with one
insert at most in the cast's queue, and plays nothing. The cost is one snapshot read a step.

**Why no per-step chain.** Round 1 conditioned every step on the generation of a snapshot read after
the step before. Another controller's repeat change between two steps then refused the next step
before it ran (`edits.go:147-156`; `owner.go:601-602`): an edit left half done by a change that
touched neither its queue nor its transport, which C4 forbids. The IDs carry what the chain needed,
and nothing else.

### 2.3 Operations (E2, E3)

Every console command is an operation of the control layer (`consoleControl`, §5.1).

- **Its id** is 32 lower-case hexadecimal digits the tab draws (`crypto.getRandomValues`) and keeps
  in session storage with the operation's kind before it sends the request, so a reloaded tab knows
  which operation it waited for. A request whose id names an operation the control has run answers
  that operation's record and never runs it again; an id the control ran and no longer keeps a
  record of (it keeps the last 256 ids) is refused, `operation_forgotten`. A resent request is never
  a second edit.
- **One at a time.** The control runs operations in their order of arrival. One that arrives while
  another runs ends the older one's steps not yet sent, and the older sends nothing more. Its step in
  flight, sent before, runs on in the engine ahead of the newer operation's commands, and its answer
  finishes the older record. A Stop is sent at once: the engine takes it after that step, and a play
  still waiting there is overtaken by it (`owner.go:549-570`; `doc.go:114-134`). Every other
  operation first waits for that step's answer, so the steps of two console operations never
  interleave.
- **Steps never run on the request's context.** Each runs on a context the request's end does not
  end (`context.WithoutCancel`), under `engine.WithAdmission`, and the control waits for the engine's
  own answer, which every command the engine takes gets (§2.1) within the engine's own bounds
  (`ReserveTimeout`, `StartTimeout`, `ReleaseTimeout`, `CallTimeout`:
  `internal/player/engine/reserve.go:26-35`; `internal/player/engine/backend.go:11-22`). No admitted
  step's answer is ever `ReasonCancelled`, and no outcome is guessed (E2).
- **The request's end** (the tab closed, the connection lost), seen while the handler still waits,
  ends the steps not yet sent, as a newer operation does: the step in flight is still answered and
  recorded. The handler answers when the operation ends or, past 30 s, with the record as it stands
  (`running`), and that answer ends nothing: the operation goes on, and its record says how it ended.
- **Its record** is published with the state (§4.3), and so reaches every tab on the event stream
  (E3): `{id, kind, status, sent, of, last}`, `sent` being the steps sent of the `of` planned and
  `last` the word of the last step's answer. The status is `running`, then one of:
  - `done`: every step ran, and the publication after the last shows what the operation made (§2.4);
  - `changed`: another controller acted on the queue or the transport during the operation: the
    steps after its change of the queue were not sent, the play found the console's entry gone, a
    play of the console's was superseded before it began (`ReasonSuperseded`, `doc.go:114-134`), or a
    play followed the console's Pause; the state shows what is there;
  - `refused`: the engine refused a step for a reason of its own (at the first step: stale, a hold,
    the output; later: a reservation not written, a full queue, the play's output or configuring);
    the steps before it stand, as `sent` and the state show;
  - `cancelled`: steps were left unsent because the request ended or a newer operation came, `sent`
    saying how far it went, the step in flight answered first;
  - `unknown`: the engine shut down while a step of it was admitted and unanswered.

  The control keeps the last eight records. A tab that finds no record of its operation in a new
  epoch (§4.2) takes it as `unknown`: the serve that ran it has ended, and none of its commands can
  run now.
- **The bar** shows an operation it has no answer for as unconfirmed until the record's final status
  is published (§7.2), and never sends the operation again.

### 2.4 The steps

```text
replace(op, at, T[0..n-1], k, p, play)       the switch to the bridge: k its track, p its position
 0. Every track and every limit is checked, and a long list windowed (§6.2, §6.3), before the
    engine is asked anything; a request none of whose tracks passes is refused, nothing changed.
 1. DeleteAll(IfGeneration(at)).              refused: refused, nothing changed.
 2. id[k] := InsertAfter(0, T[k])             the queue is empty: T[k] is selected (edits.go:30-36)
    id[0] := InsertAfter(0, T[0]), then T[1] .. T[k-1], each after the one before
    T[k+1] after id[k], then T[k+2] .. T[n-1], each after the one before
    After each insert: the queue's IDs exactly the console's (§2.2), else changed.
 3. If play: PlayID(id[k]), after the same check.   refused: the queue in place, not started.
 4. If p > 0 and T[k] is PCM of known length: the seek (§2.2), SeekAbsolute(p) under
    IfGeneration of a publication showing id[k] selected and playing, three tries; else
    the track plays from its start, and the answer says why.
 5. done when the publication after the last step shows the queue's IDs exactly the console's
    and, with play, an entry of the console's selected and playing (id[k], or the one after an
    entry the player skipped at its open); without play, id[k] selected and the transport
    stopped. Otherwise changed.

append(op, at, T[0..n-1])
 0. As replace's step 0, the room counted against the queue as it stands (§6.4).
 1. base := the IDs of a snapshot at `at` (else refused, stale, nothing changed);
    id[0] := InsertAfter(the last of base, T[0], IfGeneration(at)), the last being 0 for an
    empty queue, whose first track is then selected (§6.4).
 2. T[i] after id[i-1]; after each, the IDs exactly base and the console's, else changed.
 3. done when the publication after the last insert shows base and then the console's entries.

delete(op, at, entry)     Delete(entry, IfGeneration(at)); done when the entry is gone.

transport(op, at, cmd)    one step: Play, Next, Previous, PlayIndex(i), SeekAbsolute(s) and
                          SetRepeat(on) with IfGeneration(at); Pause and Stop with none (§2.6).
                          done when the engine answered nil (a play command's nil is its playback
                          begun, or past the end a stop: doc.go:85-90, :114-134) and, for Pause,
                          the publication after it shows the transport paused or stopped;
                          otherwise changed (a play that followed the Pause, a play superseded).

switch-out(op)            the switch to this browser (§3.2).
```

Step 2's order is round 1's corrected by E6: the start track goes first, into the emptied queue,
which selects it with nothing played; then the first of the tracks before it at the head, and each
next one after the one before it; then the tracks after it. With `play` true the selection changes
nothing (the play names `id[k]`); with `play` false it is how the start track is selected, since no
command selects an entry without playing it (§6.5). `DeleteAll` on an empty, stopped queue with
nothing selected changes nothing visible (`edits.go:117-145`), and an edit that finds the queue so
goes on from there; one that releases a session answers at once and the release runs beside the
inserts, a play behind it waiting for its acknowledgement (`doc.go:79-80`).

### 2.5 The contract, operation by operation (C1)

For each operation: (a) another controller acts between two of its steps; (b) a step is refused;
(c) the request is lost after the engine admitted a step; (d) the engine holds the player (the DSD
check, maintenance, a configuration reservation, an unconfirmed release).

**Replace and play** (an album's or a row's Play, and the switch to the bridge with its track and
position)

- (a) Before `DeleteAll`: refused stale, nothing changed. After it: an edit of the queue stops the
  console at its next check, with at most one more insert of its own, nothing played: `changed`, the
  queue as it is. A change of the transport alone (a play, a skip, a seek), of repeat, or the
  engine's own report changes no ID: the console goes on, and its play, admitted last, wins (`done`).
  Between the play and the seek, another controller's play fails the seek's check: `done`, played
  from the start, the answer saying why.
- (b) `DeleteAll` refused: `refused`, nothing changed. An insert refused (`queue_full` after another
  controller's inserts, a reservation not written, IDs exhausted): `refused`, the entries before it
  in place, nothing played. The play refused (the output, `configuring`, `release_unconfirmed`, every
  entry unplayable): `refused`, the queue in place and not started.
- (c) The steps not sent are ended; the step in flight is answered and recorded; `cancelled`, with
  `sent`. Nothing plays unless the play was the step in flight, and then the record says it played.
- (d) A check or maintenance refuses `DeleteAll` (`check`, `maintenance`): nothing changed. A hold
  granted during the edit (the Player page's DSD check) refuses the step after it: `refused`,
  partial. A configuration reservation (the DSD choice's write) and an unconfirmed release refuse the
  play alone (`configuring`, `release_unconfirmed`): the queue in place, not started.

**Replace without play** (the switch to the bridge from a paused browser, §6.5): as above with no
play and no seek; `done` when the queue is the console's, its start track selected, the transport
stopped.

**Append** (Add to queue)

- (a) Before the first insert: refused stale. After it: an edit of the queue stops the console, at
  most one more insert landed: `changed`. A change of the transport or of repeat changes nothing.
- (b) The first refused: nothing changed. A later one: `refused`, the inserts before it in place.
  The room is checked first (§6.4), so `queue_full` comes only from another controller's inserts
  during the append.
- (c) As replace's.
- (d) A hold refuses the inserts; a configuration reservation and an unconfirmed release refuse no
  edit.

**Delete** (a row's remove in the queue panel)

- (a) One step, conditioned on `at`: a change before it refuses it stale, nothing changed.
- (b) `entry_not_found` (another controller deleted it): its word.
- (c) One step: answered and recorded.
- (d) A hold refuses it.

**Play, Next, Previous, an index, Seek, Repeat**

- (a) One step, conditioned on `at`: a change before it refuses it stale, nothing changed. Once it is
  admitted, the last admitted command wins.
- (b) Its word, nothing changed (`empty_queue`, `seek_unsupported` for DSD, `entry_not_found`).
- (c) Answered and recorded.
- (d) A hold refuses it; whatever starts a session is refused `configuring` and
  `release_unconfirmed` (`doc.go:101-106`).

**Pause and Stop** (no generation, §2.6)

- (a) Pause pauses what plays when it runs, and Stop stops it; a play admitted after either wins,
  and the bar shows it.
- (b) Pause is refused under a hold; Stop is never refused while the engine runs (it ends the DSD
  check, and does nothing under maintenance).
- (c) Answered and recorded.
- (d) As (b).

**The switch to this browser** (§3.2)

- (a) A play admitted after the console's Pause (a release that carried a play, or another
  controller's play) shows in the publication after it: the control pauses again, three Pauses at
  most within 10 s, then `changed`, the browser not started.
- (b) The Pause refused under a hold: `refused` (`check`, `maintenance`), the browser not started; for
  the check, the bar offers Stop.
- (c) `cancelled`, or `unknown` in a new epoch: the browser is not started; the pick is made again.
- (d) As (b); the test tone in the slot is refused `tone` the same way (§3.1).

### 2.6 Pause and Stop carry no generation

They ask for silence whatever plays. A stale refusal would leave the DAC sounding against an explicit
ask, the switch to the browser needs a Pause that is never refused for staleness (C2), and C3 asks
for Stop wherever the engine takes it. This reverses round 1, which conditioned them on the budget's
"each carrying `IfGeneration`": what that condition guards against, a tab acting on a state it did
not see, does not reach a command whose effect does not depend on the state.

### 2.7 Partial, never rolled back

The console never deletes what it inserted: that would be one more edit on a queue another
controller is using. A partial queue is shown as it is, with one line: "The queue changed while the
console was replacing it: it holds 3 of the album's 10 tracks, and the player is stopped." A retry is
the user's, a new operation from the state the bar shows, never a continuation (R4).

### 2.8 The cases (C1, E2, E3)

- `TestALostReplaceHeldBetweenStepsThenAnotherPickNeverPlaysBoth` (C1): a replace held at the
  control's seam after its first insert (`controlSeams.afterStep`, nil in production), its
  connection dropped, and then the bar's pick of this browser. The switch-out ends the replace's
  unsent steps; no console insert and no play reaches the engine after the switch-out's Pause; and
  the browser's go comes only once the replace's record reads `cancelled` and the bridge's silence
  is published after the Pause. A second case parks the replace's insert in the engine, behind a
  reservation write held at `engine.Config.Seams`, in place of the seam: the go waits for that
  insert's answer. Control: a newer operation that does not end the older one's unsent steps (red:
  the replace's next insert and, with play, its `PlayID` land after the switch-out's Pause, and the
  bridge plays beside the browser).
- `TestAnAdmittedStepParkedWhenTheRequestEndsIsAnsweredNotCancelled` (E2): an insert admitted and
  parked behind a held reservation write, and a Pause admitted and parked behind a resume held in the
  stand-in backend, when their request ends. Each record reads `running` until the engine answers,
  then its final status with that step's own answer: the replace `cancelled`, its later steps
  unsent, and the Pause the outcome of its one step; no `ReasonCancelled` reaches a record. Control:
  the step on the request's context (red: the record says the step `cancelled` before the engine
  answered, and the parked insert lands after the record said nothing more ran).
- `TestEveryOperationsOutcomeIsPublished` (E3): an operation of each final status, and a tab's reload
  between its request and its answer, which finds its own record in the first frame. Control: the
  record kept in the handler's answer alone (red: the reloaded tab finds nothing).
- `TestARepeatOrAReportBetweenTwoStepsChangesNothingOfTheEdit` (C4): a `SetRepeat` and an output
  report landed at the seam between every two steps of a replace and of an append: each ends `done`,
  its queue whole. Control: round 1's chain (red: refused at the step after the change).
- `TestACastAfterTheConsolesDeleteAllStopsItAtTheNextCheck`: a cast's `DeleteAll` and insert landed
  at the seam after the console's `DeleteAll`: one console insert in the cast's queue, no play,
  `changed`. Control: the IDs compared at the end alone (red: every insert lands and the console's
  album plays).
- `TestAResentOperationNeverRunsTwice`: a request sent again with its id answers the first record;
  an id past the kept 256 is `operation_forgotten`.

## 3. The browser starts only on the bridge's silence (C2)

### 3.1 Observed, settled silence

The browser starts only on a publication (§4) that:

- was captured after the answer of the console's last command for the switch, with every earlier
  console operation's record final (§2.3);
- shows the engine running, its transport `stopped` or `paused`, not starting and not releasing;
- shows no release unconfirmed, no hold (the DSD check plays its chime on the DAC), and no test tone
  in the device slot (`playback.Backend.ToneStatus`, `internal/player/playback/tone.go:643`; the
  tone plays outside the engine, `tone.go:156-172`).

A refusal is not silence, and neither is a Pause's success alone: a Pause during a release that
carries a play succeeds, and the play follows (§2.1). A player that does not run (off at boot,
failed to start, its engine closed) is silent too: nothing the console or a controller sends reaches
a DAC then (a residual for a closed engine, §13).

### 3.2 The switch to this browser is one operation

`POST /api/output/player/switch-out` runs on the server:

1. It ends the older console operation's unsent steps, and waits for its step in flight (§2.3).
2. It reads the publication now, whose transport says whether the browser plays after: playing,
   starting, or a release that carries a play. With no engine running it sends nothing and answers
   `done` with the queue of the last publication that had one, the browser taking it without
   playing.
3. It sends a Pause, with no generation, even when that publication shows the bridge paused or
   stopped: a resume another controller sent before it, which the engine shows as paused until its
   answer (`transport.go:887-897`, `:1381-1412`), holds the Pause behind it (`owner.go:480-496`),
   and the Pause then pauses what the resume started (E4). Paused or stopped with nothing under way,
   the Pause does nothing (`doc.go:73`).
4. It waits, within 10 s, for a publication captured after the Pause's answer in which the
   transport has settled (not starting, not releasing). Silent (§3.1): `done`. Playing: another
   Pause, three at most, then `changed`. A hold: `refused` (`check`, `maintenance`). The tone in the
   slot: `refused` (`tone`).
5. Its answer carries the publication that showed the silence, with that publication's queue (each
   entry's track, §4.2), its selection and, for PCM, its position: one capture, so no cast and no
   track change lands between the pause and the reads (C3). The browser takes them, and plays when
   step 2 found the bridge playing.

### 3.3 The promise, narrowed

> **The console never starts this browser while it knows the bridge's player plays. Another
> controller can still start the bridge's player after the console's last check, and the bar then
> shows the bridge playing.**

The bar's help states it in those words (PR 2's records, §10). It needs no coordination with other
controllers. What it leaves is a play another controller sends after the console's Pause, which the
engine runs after it: the last admitted command wins. A resume already under way when the Pause
arrives is not left: the Pause waits behind it and pauses what it started (§3.2, step 3; E4). While
this browser is chosen the bar still follows the frames (§7.1), so a publication showing the bridge
playing puts a line on the bar, "The bridge's player is playing too: another controller started it",
and the menu's line says what plays; the console stops neither.

### 3.4 The cases (C2, E4)

- `TestTheSwitchOutPausesAResumeUnderWayBeforeTheBrowserStarts` (E4): another controller's Play from
  paused, held in the stand-in backend's Resume, so the publication reads paused; the switch-out's
  Pause waits behind the resume and pauses what it started; the answer's publication is captured
  after that pause. Control: the switch-out judging silence from the publication before its Pause
  and sending none when it reads paused (red: the answer comes while the resume, let go, plays).
- `TestAPlayAfterTheSwitchOutsCheckShowsTheBridgePlaying` (E4's residual): another controller's Play
  after the switch-out's answer: the next frame shows the bridge playing, and the bar (PR 2) says so
  with the browser playing.
- `TestARefusalOrAPausesSuccessIsNotSilence` (C2): a switch-out refused under the check, and a Pause
  during a release that carries a play: neither starts the browser. Control: the bar starting the
  browser on the Pause's answer alone (red).
- `TestTheSwitchOutsQueueIsTheSilencesOwn` (C3): a cast landed at the seam right after the silence's
  publication: the answer's queue is the one before the cast, the next frame shows the cast.

## 4. One publisher orders the state (C3, E5)

### 4.1 The publisher

`consoleControl` runs one publisher goroutine (`cmd/bridge/serve_player_publisher.go`, new), in the
shape of serve's supervisor loop (`cmd/bridge/serve_player.go:609-651`).

- **It wakes on** the engine's `Changed()`, taken before each read so a publish after the read wakes
  it (`engine.go:436-444`); its own subscription to the hotplug feed
  (`internal/player/hotplug/feed.go:68-82`); the live config holder's change
  (`serve_player.go:155-162`); a change of an operation's record; a 500 ms tick for what has no
  signal of its own (the backend's `Facts`, `internal/player/playback/live.go:63-86`, and
  `ToneStatus`); and a handler's ask for a capture after an answer.
- **Each wake captures, on that one goroutine,** one engine snapshot, `Facts`, `ToneStatus`, the
  feed's latest snapshot, the live config's withholding, the records and the clock, and builds the
  state (§4.3). When the state differs from the last one published, the publisher gives it the next
  revision in the same step and publishes it: the state, its epoch and revision, the capture's time,
  its encoded bytes, and the engine snapshot it read, which holds the queue (`snapshot.go:288-318`).
  A state equal to the last publishes nothing.
- **Nothing else assigns a revision.** No handler stamps one after reads of its own: a handler asks
  the publisher for a capture made after its operation's last answer, and answers that publication.

### 4.2 The order, the epoch and the queue

- **The revision** counts one epoch's publications, from 1, by one each.
- **The epoch** is 16 random bytes, hex, drawn when serve builds the control: one serve process, one
  epoch. Two serves never run on one data directory at once (`lockServeDataDir`,
  `.claude/rules/cli-serve.md`), and the console's port is one process's.
- **The bar's rule.** Every state the bar takes carries its epoch and revision: a frame, a
  command's answer, `GET now`, `GET queue`. Within an epoch the higher revision wins, and a lower or
  equal one is dropped. A state of an epoch the bar has not seen is newer than everything it holds:
  it takes it, and drops for good the epochs it held, since a serve that ended answers no more. A
  state of an epoch it dropped is dropped.
- **The queue.** `GET queue` answers the queue of the publication it is served from, with that
  publication's epoch, revision and token, each entry named through the resolver's `StoredPath`
  (`internal/player/library/library.go:504-527`) and hydrated through the catalog, as the album view
  does (`player_audio.go:203-271`); an entry whose track is gone is kept, with its word. The bar keeps
  the queue of the highest publication it read, and reads it again when a newer state's token
  differs from that queue's (the token moves on every change of the queue's membership or order,
  `snapshot.go:377-380`). The queue is in no frame.
- **The stream.** `GET /api/events` (`handlers_events.go:52-383`) sends a `player` frame when the
  latest publication's revision differs from the last one this connection sent, its bytes the
  publication's, encoded once; a connection's first frame is the latest publication. It is sent while
  a control is wired and serve's plan lists its player (`servePlayerPlan.lists`,
  `serve_player.go:76-83`), started or not; a box whose player is off, not Linux or public sends
  none. app.js hands it to the bar as a `bridge:player` window event (`app.js:5739-5750`). While the
  stream reconnects with the bridge's player chosen, the bar reads `GET now` every 2 s, each answer a
  publication ordered as the rest.
- **The stream against the console's server**, as round 1 found it: `WriteTimeout` stays unset
  (`admin.go:2293-2309`), the stream ends when shutdown starts (`.claude/rules/cli-serve.md`, the
  event stream's bullet), a long-lived GET checks its Origin (`handlers_events.go:63-68`), and the
  console has no connection cap and, in loopback mode, no rate limit (`middleware_auth.go:28-39`). The
  design adds no long-lived request.

### 4.3 The state, its parts kept apart

```text
epoch, revision, at  the publication's order, and the server's clock at its capture
running, why         the player runs, its engine started; else off, starting, start_failed, closed
offered, why         the bridge's player can be an output: running, its output known and available,
                     the live config withholding nothing (disabled, binding_changed)
transport            stopped, playing, paused, releasing (and what follows it), maintenance; starting
                     apart: nothing heard yet (snapshot.go:323-330)
hold                 none, check, maintenance, or configuring (the DSD choice's write)
release              confirmed, or unconfirmed since when (snapshot.go:444-451)
commands             what the engine takes now, by its table (doc.go:71-106) read off this snapshot:
                     stop while the engine runs; pause, play, next, previous, index, seek, repeat and
                     the edits as the hold, the release and the output allow
silent               §3.1's predicate, from this one capture
tone                 whether the test tone holds the device slot
generation, token    the snapshot's generation (Generation.String, snapshot.go:78-79), and the queue's
                     token and length
selected, repeat     the selected entry; repeat-all
track                the occurrence heard: its entry, its catalog path and row, the decoder's details,
                     named through StoredPath and the store's row as the view names it
                     (serve_player_console.go:459-494), never from a controller's metadata
anchor               the position (§4.4)
signal, seek         what the DAC receives (pcm, dop, native, converted, its format, rate and valid
                     bits; converted never called bit-exact), and whether a seek is offered (PCM of
                     known length alone: seek.go:119-153)
lastStop, lastAttempt  why playback last stopped, and the last play attempt (snapshot.go:460-467)
operations           the last eight records (§2.3)
```

Eligibility (`offered`), the transport, the hold, the release and the commands are separate fields,
and the bar reads each for what it says (§7):

- the pick of the bridge's player is enabled while `offered` holds, no hold stands and the release
  is confirmed;
- a bar showing the bridge's player keeps showing it through a hold, an unconfirmed release or the
  output going away: each is a line, and only the commands the engine takes are enabled. Stop is
  enabled while the engine runs, the DSD check included;
- when eligibility returns, the bar shows the state as published, whatever it is: never a prescribed
  "stopped".

### 4.4 The anchor

The position is an anchor the publisher keeps: `{id, occurrence, entry, seconds, at, advancing}`,
`seconds` and `at` the snapshot's position, frames over the rate, and when the backend measured them
(`backend.go:102-121`). The publisher replaces it, its id being the revision of the first
publication that carries it, only when the occurrence changes, `advancing` (playing and heard)
changes, or the measured position departs from the anchor's extrapolation by more than a second (a
seek, a stall). Otherwise every publication carries the same anchor, its id and time included, so
steady playback publishes nothing (`.claude/rules/admin-console.md`, the SSE stream's bullet).

The bar extrapolates from the anchor's own time, never from a frame's arrival: the position is
`seconds + (now − (at + offset))`, `offset` being the client's clock less the server's, taken as the
least `received − capture time` over the epoch's publications. A frame for another change (a repeat
setting, a queue update) carries the same anchor and moves nothing; a reconnection's first frame
carries it too, and the position goes on where it was. Round 1 extrapolated from each frame's
arrival, so a repeat change during steady playback took the position back to the anchor's seconds.

### 4.5 The cases (C3, E5)

- `TestEveryStateIsOrderedByThePublishersRevision`: an older answer delivered after a newer frame
  is dropped; a command's answer, `GET queue` and a frame served from one publication agree.
- `TestARevisionIsAssignedOnlyToTheCaptureItNames`: a capture held at the publisher's seam between
  its reads and its publish: no handler answers a revision for it before it is published. Control: a
  handler stamping the publisher's counter after its own read (red: two answers of one revision with
  two states).
- `TestTheAnchorKeepsItsIDAndTimeThroughChangesThatMoveNoPosition`: a repeat change, an append and an
  output report during steady playback publish new revisions with the same anchor. Control: the
  anchor's time set at each publication (red).
- `TestStopIsOfferedWhereverTheEngineTakesIt` (the DSD check, maintenance, a release unconfirmed, the
  output gone) and `TestTheCommandsOfferedAreTheOnesTheEngineTakes` (every state the engine fake
  reaches: each command listed is taken, each left out is refused). Control: the commands derived
  from `offered` alone (red: Stop disabled under the check).
- PR 2's: `TestTheBarsPositionHoldsThroughARepeatChange`, `TestTheBarsPositionHoldsThroughAQueueUpdate`,
  `TestTheBarsPositionHoldsAcrossAReconnection` (control: the anchor taken from the frame's arrival,
  red), `TestANewEpochWinsAndAnOldOneIsDropped` (control: the revision compared across epochs, red),
  `TestAHoldNeverWipesTheView`, `TestReturningEligibilityShowsTheActualState` (the output back while
  another controller plays: the bar shows it playing; control: round 1's "bridge, stopped", red).

## 5. The control API

### 5.1 The interface and its guards (R8)

`internal/admin` imports no player package (`handlers_player_output.go:8-13`), so the control is an
interface of the console's, beside `PlayerOutput` (`:75-117`), that `cmd/bridge` implements:

```go
// PlayerControl is the bridge's own player as the console's player bar drives it (B634).
type PlayerControl interface {
	Now(ctx context.Context) PlayerPublication                       // §4: a capture after now
	Latest() PlayerPublication                                       // the stream's
	Wake() <-chan struct{}                                           // closed at the next publication
	Queue(ctx context.Context) PlayerQueue                           // §4.2
	Plays() PlayerPlays                                              // §6.7: the codecs and each DSD rate's way
	Replace(ctx context.Context, r PlayerReplace) PlayerAnswer       // §2.4
	Append(ctx context.Context, r PlayerAppend) PlayerAnswer
	Delete(ctx context.Context, r PlayerDelete) PlayerAnswer
	Transport(ctx context.Context, r PlayerTransport) PlayerAnswer
	SwitchOut(ctx context.Context, r PlayerSwitchOut) PlayerAnswer   // §3.2
}
```

`Deps.PlayerControl` holds it; `cmd/bridge/serve_player_control.go` (new) is `consoleControl`, built
in `runServe` beside `newConsolePlayer` over the same `bridgePlayer`, `bridgeOutput` and boot plan.
The context each command takes is the request's, which the control watches for the client's
departure alone (§2.3) and never hands the engine. Generations cross the seam as their strings,
parsed by `engine.ParseGeneration` in `cmd/bridge` (`snapshot.go:78-99`); entries as `uint32`;
tracks as catalog paths. The view (`consolePlayer`) stays what it is, starting nothing. The control
reads the engine only after `ready`, as the view does (`serve_player_console.go:299-306`), and only
for a player serve started. Once serve's stop begins the control takes no new operation
(`player_not_running`, `closed`); its publisher and its operations' goroutines are joined, bounded,
in serve's player stop, after the engine's Run has ended, so that every step they wait on has its
answer (`ReasonClosed` at the latest, §2.1), and before the store they read closes (§10, the
cli-serve.md line).

**The guards** are the console's own, on every route: `boundaryMiddleware`, `csrfGuard` and
`sessionMiddleware` (`admin.go:2175`): a loopback source and a loopback Host, a JSON body type, a
matching Origin. Then the control's own refusal, `controlRefusal`, asked before anything is read: no
control wired, 503 `player_not_wired`; a public bridge, a signed-in session included, 403
`player_not_available` (a public bridge runs no player: `config.PlayerEnabled`); a host that is not
Linux, 409 `player_not_supported`.

**Control is not config.** `controlRefusal` asks neither the player block's `Unread` nor the managed
settings: those guard a write of `output.player` (`handlers_player_output.go:436-460`), which a
control route never makes. An unreadable player block on disk blocks no Stop and no Pause of a
player that runs (R8). The control routes take neither the console's lock (`lockPlayerWrite`,
`:462-480`) nor bridge.yaml's: they write no config, and the engine's owner orders their commands.

### 5.2 The routes

| Method and path | Body | Does |
|---|---|---|
| `GET /api/output/player/now` | — | a publication captured now (§4) |
| `GET /api/output/player/queue` | — | the queue of a publication captured now (§4.2) |
| `POST /api/output/player/queue` | `op`, `at`, `album` or `tracks`, `start`, `position`, `play` | replace (§2.4) |
| `POST /api/output/player/queue/append` | `op`, `at`, `album` or `tracks` | append |
| `POST /api/output/player/queue/delete` | `op`, `at`, `entry` | delete one entry |
| `POST /api/output/player/transport` | `op`, `at`, `command`, and `index`, `seconds` or `on` | the transport; `at` with every command but `pause` and `stop`, which carry none (§2.6) |
| `POST /api/output/player/switch-out` | `op` | the switch to this browser (§3.2) |

- `op` is the operation's id (§2.3); another shape is 400 `bad_operation`.
- `at` is the generation string of the state the bar acted on; one that does not parse is 400
  `bad_generation`.
- `album` is a catalog album ID, expanded on the server to its tracks in the catalog's order, never
  a path prefix (`.claude/rules/admin-console.md`: an album is a set of tracks; identity scopes
  travel as IDs and are expanded server-side). `tracks` is a list of catalog paths. Exactly one of
  the two; a list past the queue's limits is windowed (§6.3), and the answer says so.
- `start` is an index into the list (0 by default), `position` whole seconds into that track (none
  by default), `play` whether to start it. With `play` false the whole list is sent and the track at
  `start` is selected (§6.5).
- `command` is `play` (`Play`), `pause` (`Pause`), `stop` (`Stop`), `next` (`Next`), `previous`
  (`Previous`), `index` (`PlayIndex(index)`: the queue panel is positional, and under the tab's
  generation an index names the entry the tab saw), `seek` (`SeekAbsolute(seconds)`) or `repeat`
  (`SetRepeat(on)`). There is no shuffle (R10).
- Every body goes through `decodePlayerBody` (one JSON object, no key it does not take, nothing after
  it, `handlers_player_output.go:541-592`), with a route constant of its own for its log line
  (`:600-607`).

### 5.3 Answers and refusals

Every answer carries the operation's record and the publication captured after its last answer (or,
for a refusal before any step, as it stood). `done` is 200; `running` past the handler's 30 s is
202; every other status, and every refusal before the engine, is the console's error envelope
(`error`, `message`) with the record and the publication:

| Condition | Status | `error` |
|---|---|---|
| no control wired; a public bridge; not Linux | 503; 403; 409 | `player_not_wired`; `player_not_available`; `player_not_supported` |
| no player serve runs: off at boot, no DAC bound, refused at boot | 409 | `player_not_running`, `reason` `off` |
| the player starting, its start failed, its engine closed (`ReasonClosed`) | 409 | `player_not_running`, `reason` `starting`, `start_failed` or `closed` |
| a body that does not decode; an `at` that is no generation; an `op` that is no id | 400 | `bad_json`; `bad_generation`; `bad_operation` |
| an `op` the control ran and keeps no record of | 409 | `operation_forgotten` |
| no track of the request plays on the bridge's player | 409 | `not_playable`, each track's word |
| an append the queue has no room for | 409 | `queue_full`, with the room left (§6.4) |
| a record `changed` (§2.3: the queue edited, the console's entry gone, a play superseded, a play after the Pause) | 409 | `changed`, with `sent`, `of` and `last` |
| a record `cancelled` (answered to a request a newer operation overtook) | 409 | `cancelled`, with `sent` |
| a record `unknown` | 409 | `outcome_unknown` |
| a record `refused`: `ReasonConflict` | 409 | `stale` |
| `ReasonMaintenance` under the DSD check's hold (its reason `check`) | 409 | `check`: Stop ends the check |
| `ReasonMaintenance`, any other hold | 409 | `maintenance` |
| `ReasonConfiguring` (a DAC's DSD choice is being written) | 409 | `configuring` |
| `ReasonOutputUnknown`; `ReasonOutputUnavailable` | 409 | `output_unknown`; `output_unavailable` with the report's word, or the live config's (`disabled`, `binding_changed`) |
| `ReasonReleaseUnconfirmed` | 409 | `release_unconfirmed` |
| `ReasonEmptyQueue`; `ReasonIDNotFound` or `ReasonIndexNotFound` | 409 | `empty_queue`; `entry_not_found` |
| `ReasonSeekUnavailable`, `ReasonSeekOutOfRange`, `ReasonSeekUnsupported` | 409 | the reason's word |
| `ReasonFull` | 409 | `queue_full` |
| `ReasonReservation`, `ReasonFileStuck`, `ReasonIDsExhausted` | 409 | `edits_refused` with the engine's word |
| `ReasonOvertaken` | 409 | `overtaken` |
| a play that did not happen (`unplayable`, `all_unplayable`, `backend_error`, `backend_timeout`) | 409 | its word |
| the switch-out with the test tone in the slot | 409 | `tone` |

The engine's reasons are its own words (`internal/player/engine/errors.go:12-125`); the table is the
one map from them, and a reason it does not name answers `player_refused` with the reason beside it.

## 6. The queue's sources and its entries

### 6.1 A track's path and its reference

The bar names a track by the catalog's path (`playerTrackDTO.Path`, `handlers_player.go:810-811`),
the name the audio, download and trash routes take already, or names an album by its ID. The control
maps a path to the player's reference on the server: `library.Reference(path)`, which is
`dlna.TrackID("", path)` (`library.go:148-156`), the ID `/dlna/file/` names and the DLNA adapter
builds with an empty root (`cmd/bridge/dlna_wiring.go:751`, `:924-930`). No new index: the catalog's
reads supply the paths, and the resolver's own index of paths by ID (`library.go:18-31`) checks them.

### 6.2 Every track and every limit checked before `DeleteAll` (R12, E7)

Step 0 (§2.4) asks, of every track and before the engine is asked anything:

1. the catalog has its row (`CatalogTrackRowsForPaths`); a path it has not is `not_in_library`;
2. `Member(id)` passes (`library.go:481-502`): not `not_indexed`, `routed`, `sacd_virtual_track`,
   `outside_library`, `unknown_container` (WavPack and Monkey's Audio), `ambiguous_reference` or
   `index_unreadable`;
3. its codec is one the player plays: the element's codecs (`serve_player_output.go:160-188`,
   `:475-481`), flac, wav, aiff and alac, and dsf and dff while the effective DSD set plays any rate;
   a lossy codec is `lossy_refused`, the decoders' own word (`internal/player/decode/refuse.go:13`);
4. for DSD, its rate's way in the published set: native, DoP or converted; refused with the set's
   own word (`internal/api/outputs_bridge.go:311-316`), or `dsd_rate_not_played` for a rate the set
   names nowhere (DSD512, which admission refuses);
5. its reference fits the engine's 128 bytes (`queue.go:18-19`), which §6.6's choice of host keeps;
6. its metadata (§6.6) fits the engine's 64 KiB an entry (`queue.go:14-15`); a track whose metadata
   would pass it is left out, `metadata_too_large`. The builder's escaping keeps it XML 1.0 text.

And of the list: at most `MaxEntries` (1000, `queue.go:12-13`) and at most 16 MiB of metadata in all
(`queue.go:16-17`), which §6.3's window keeps; for an append, the room the queue leaves as it stands
(§6.4). The engine checks the same at each insert (`edits.go:18-23`); asking them first is what
keeps a replace from emptying the queue and then stopping at an insert the engine refuses.

**A mixed-source list** (an album with a track on an upstream server, or a WavPack beside FLACs) is
handled before any destructive step: the tracks that fail are left out, listed in the answer with
their words, and the bar says "2 of 12 tracks are left out: 1 is on another server, 1 is a WavPack
file". A list none of whose tracks passes is refused, nothing changed. A switch whose current track
fails is refused, and the browser plays on as §7.3 says. A track that passes and fails at its open (a
PCM rate or depth the DAC does not take) is the engine's skip, with the backend's word
(`doc.go:153-157`), which the queue panel shows on its row.

### 6.3 A list longer than the queue takes (C4)

A list of more tracks than the queue takes (1000 entries, or 16 MiB of metadata in all) is windowed,
never cut silently. The window holds the start track, up to 100 tracks before it, so that Previous
plays the track before it and the ones before that, and the tracks after it as far as the limits
allow; where the list ends first, the window reaches further back. Past 16 MiB it gives up tracks from
its far end after the start first, then from its far end before it. A track step 0 left out takes no
place in it. The answer names it: "The bridge's player holds 1,000 tracks: tracks 3,901 to 4,900 of
this playlist's 5,214 are queued."

### 6.4 An append, and an append to an empty queue (C4, E7, E11)

- **Its room** is the queue's as it stands: `MaxEntries` less the entries it holds, and 16 MiB less
  the metadata it holds, summed from `ReadList` over its IDs (`snapshot.go:400-414`, which copies
  each entry's metadata: at most 16 MiB, once an append). An append that does not fit is refused
  whole before any insert, `queue_full` with the room left ("The queue has room for 10 more tracks;
  this album has 12"), nothing inserted: a partial album at the end of the queue is an edit nobody
  asked for, and the user removes entries or replaces the queue.
- **An append to an empty queue selects its first track and starts nothing.** The engine selects the
  entry an insert puts into an empty queue (`edits.go:30-36`), so a later Play plays the queue from
  its first track, as OpenHome's `Id` reads after an insert into an empty playlist; the console adds
  no command of its own, and does not start what the user only added.
- **Fallback C's append would be this one** (E11): refused whole where the room is short. With no
  `DeleteAll`, every "play this album" of option C is an append, so once the queue is full every play
  is refused until the user removes entries one at a time (§12).

### 6.5 What a paused handover keeps (E6)

The switch to the bridge from a paused browser sends the whole queue with `play` false. Round 1 sent
it from the current track onward and lost the tracks before it. The start track now goes first, into
the emptied queue, which selects it; then the first of the tracks before it at the head and each
next one after the one before it; then the tracks after it, the first of them after the start track
(§2.4, step 2). So `[A, B, C]` paused on B is the engine's `[A, B, C]`, B selected, stopped, and
nothing played.

What is lost is the position in B: the engine places no stopped player at a position, since a seek
needs a track to seek in (`seek.go:119-142`). The bridge's Play then plays B from its start, and
Previous plays A (Previous from stopped plays the entry before the selection: `doc.go:77-78`). The
bar says that the position stayed in the browser.

`TestAPausedHandoverKeepsTheQueueBeforeItsTrack`: `[A, B, C]` paused on B gives the engine's
`[A, B, C]`, B selected, stopped, and Previous then plays A. Control: round 1's rule, the list from
the start onward (red: the queue is `[B, C]`, and Previous restarts B).

### 6.6 What a console entry holds (R7, C4, E8, E9)

- **The reference** is the track's canonical URI in the form 1-bit's own Insert sends,
  `http://<host>:7790/dlna/file/<id>` (`internal/player/trackref/trackref.go:7-25`): the queue's
  canonical identity, on port 7790 whatever `dlna.listenAddress` says (`openhome.DefaultTrackPort`,
  `internal/player/openhome/inventory.go:12-15`; `cmd/bridge/serve_player_openhome.go:44-60`). Its
  host is the first an Insert's confinement takes (the same inventory, `newPlayerHosts`,
  `serve_player_openhome.go:24-42`, and the same LAN-host rule, `writes.go:415-487`, exported for
  this use), a private IPv4 address first, among those that keep the URI within 128 bytes. The
  resolver parses it again at every open and looks at neither its host nor its port
  (`trackref.go:177-193`; `library.go:7-16`), so it plays with OpenHome on or off, and after the
  box's address changes.
- **The metadata** is the DIDL-Lite item the bridge's ContentDirectory answers for that track
  (`dlna.DIDLForTrack` inside `dlna.WrapDIDLLite`, `internal/dlna/content_directory_xml.go:186-196`,
  `:483`), from the store's row as the DLNA adapter converts it (`manifestTrackToDLNATrackInfo`,
  `dlna_wiring.go:924`), made by one exported builder (a `TrackInfo` method over today's unexported
  `toDIDLOpts`, `internal/dlna/content_directory.go:271-308`) that takes **the track-URI base and the
  artwork base apart** (E8). Browse passes the one server URL it uses today as both, so its bytes do
  not change; the console passes:
  - the track-URI base `http://<host>:7790`, so `<res>` is the reference itself: the source's alone,
    since a rendition never stands in for the source;
  - the artwork base, the DLNA listener's own endpoint, so `albumArtURI` is on the port the listener
    bound, and only while that listener runs, mounts its artwork route (`ServerConfig.Artwork`,
    `.claude/rules/dlna-upnp.md`, the artwork bullet) and takes connections on the reference's host
    (`dlna.Server.AcceptsOn`, `internal/dlna/server.go:643-675`); a listener bound to one address
    gives that address's base, as its own ContentDirectory does (`internal/dlna/host_guard.go:263-291`).
    Otherwise there is no `albumArtURI`, which a controller would fetch from a closed port or be
    refused. The dlna package answers the base from the listener's own state (`acceptState`, given
    the bound port and whether artwork is mounted, set at Start and cleared at Stop), never from a
    value of `cmd/bridge`'s.
  The item is under 64 KiB and XML 1.0 text, the bounds an Insert's metadata is held to
  (`writes.go:489-525`), which step 0 checks (§6.2).
- **A box with no LAN-eligible host** (none `/v1/health` advertises that an Insert takes, and none
  the DLNA listener takes connections on) writes the canonical URI on the loopback literal,
  `http://127.0.0.1:7790/dlna/file/<id>`, and metadata with no `<res>` and no `albumArtURI` (E9). The
  reference stays canonical, which `trackref.Parse` takes and the resolver plays, and no URL in the
  entry sends a controller to a host it cannot reach. Round 1's fallback, the bare id and a
  `<res>` holding the route's path alone, is dropped (C4): nothing shows that a controller, the 1-bit
  app included, renders either, and a relative `<res>` is no URI a renderer can fetch.
- **The console names an entry by its reference alone** (`StoredPath`), never by its metadata, as
  the Player page names the track heard (`.claude/rules/player.md`, the Player page's bullet in
  "Player inside bridge serve").

`TestTheTrackURIAndTheArtworkURLAreBuiltApart` (`dlna.listenAddress` on another port than 7790: the
reference and `<res>` on 7790, `albumArtURI` on the listener's port),
`TestNoArtworkURLOnAHostTheDLNAListenerDoesNotServe` (a host `/v1/health` advertises that the DLNA
listener takes no connection on, and DLNA off: no `albumArtURI`),
`TestABoxWithNoLANHostWritesTheLoopbackURIAndNoRes`, `TestTheBrowseItemIsTheSameBytesAfterTheExport`.
Controls: the artwork on the track URI's base (red: the other-port case), `albumArtURI` without the
listener's check (red: the unserved-host case), the bare id where there is no host (red: the no-LAN
case).

### 6.7 What a row says of the bridge's player

Each catalog track row gains an additive field, present while the player runs:

```text
"bridge": { "way": "pcm" | "native" | "dop" | "converted" | "none", "reason": "<word>" }
```

It is one pure function of the row (its path, codec, routed flag, DSD flag and rate) and of the
player's published facts (`Plays`: the codecs and each DSD rate's way), which step 0 runs too, so a
row and an edit cannot disagree on anything but membership. `Member` runs at the edit alone: its
cost is an index seek a track, and a playlist of tens of thousands of rows would pay it at every
page. `none`'s words are `routed`, `sacd_virtual_track` (by `manifest.IsSACDVirtualPath`),
`unknown_container` (by `formatgate.Gated`), `lossy_refused`, `not_a_container`, the DSD set's own
words, and `dsd_rate_not_played`. `converted` is said as converted to PCM, never as bit-exact
(CLAUDE.md, "No server-side transcoding on the wire, ever").

## 7. The bar (R2, R4, R5, R6, R10)

### 7.1 The output button and the empty bar

The output button sits in the bar's right zone. Its menu has two choices: *This browser*, and *The
bridge's player* with the DAC's name (from the Player page's status, read when the menu opens) and
what it is doing ("playing «title»", "stopped"). The bridge's choice is enabled while the state says
`offered`, no hold and a confirmed release (§4.3); otherwise it is disabled with the word: no DAC was
bound when the bridge started, the player is starting, did not start or has stopped; the DAC is not
there (the report's word); the player is turned off or another DAC is chosen; the DSD check or a
maintenance hold; a release not confirmed. The choice is kept in session storage
(`bridge-player-output`) beside the queue (`audio.js:10`), so a full page load keeps it. The bar
follows the frames whichever output is chosen: with this browser chosen, for the menu's line, for
§3.3's line, and for the generation a switch carries.

**The empty bar (R2).** While the stream sends a `player` frame (§4.2), the bar shows even with
nothing selected: a slim strip, "Nothing playing", with the output button. So a browser with empty
session storage can pick the bridge's player before any track is chosen. On a box that sends no frame
(its player off, not Linux, public) the bar behaves as today (`nowplaying.js:182-188`).

### 7.2 The states

| State | The bar shows | What plays | Entered by | Left by |
|---|---|---|---|---|
| browser | the browser's queue and track, as today | the browser | boot (the default); a switch-out `done` | a pick of the bridge's player |
| to bridge | "Moving to the bridge's player…", the transport disabled | nothing: the browser has stopped | a pick of the bridge's player with a queue in the browser | the replace's answer or record (§7.3) |
| bridge | the engine's state (§4.3): its track, position, transport and queue, a hold, an unconfirmed release or the output's absence each as a line, and only the commands the engine takes enabled | the bridge's player | a switch-in that ran a step; a pick with nothing in the browser; a restored choice | a pick of this browser |
| to browser | "Moving to this browser…", the bridge's view kept | the bridge's player until its pause | a pick of this browser in bridge | the switch-out's answer or record (§7.4) |
| unconfirmed | the last publication, with "The bridge's player has not said how the last command ended: this is what it is doing" | whatever the engine does, never the browser | an operation whose answer is lost, or past the bar's 30 s | its record's final status (§2.3); or a new epoch, the state as it is and the pick to make again |

There is no "gone" state: a bridge's player that becomes unavailable while chosen stays the bar's
state, its view kept, with a line ("The DAC is not connected", "The player stopped") and only the
commands the engine takes enabled. When it is offered again the bar shows the state as published.

**The rule over every state** (R4, C2): the browser starts only on a switch-out's `done` (§3.2), or
after a switch-in refused with nothing changed, on a publication showing the bridge silent (§3.1);
never on a lost or unknown answer, a refusal alone, or a Pause's answer alone. The bar applies a
state by §4.2's rule alone, so an older answer arriving last never wins.

### 7.3 The switch to the bridge's player

From a bar with a queue in the browser:

1. **The browser stops** at once: its element pauses and lets go of its source (`audio.js:550-554`),
   and its position is read.
2. **The replace** (§2.4) sends the browser's queue as listed, never a shuffle order (§7.5), `start`
   its current track, `play` whether the browser was playing, `position` its whole seconds for PCM;
   a paused browser's whole queue with `play` false (§6.5).
3. **`done`**: bridge.
4. **Refused with nothing changed** (`sent` 0: stale, the current track not playable there, not
   offered, a hold): back to the browser, which plays on if it was playing only where the answer's
   publication shows the bridge silent (§3.1); otherwise it stays stopped, and the bar says the
   bridge's player is playing. Where the browser refuses a play that no click started, the bar asks
   for a press, as it does today (`audio.js:452-458`).
5. **`changed`, a later step `refused`, `cancelled`**: bridge, the engine as it is; the browser stays
   stopped, since the switch happened in part.
6. **Lost, or past 30 s**: unconfirmed until the record is final; the browser stays stopped.

From a bar with nothing in the browser, the pick is the bridge's at once and sends nothing.

**The position (R5).** It carries over for PCM: `PlayID` starts the track from its start
(`engine.go:486-489`), and the seek follows as soon as `PlayID` answers, once the Start has answered
(`owner.go:704-729`; `transport.go:1299-1316`), with the track's details known by then
(`.claude/rules/player.md`, the bullet on each occurrence's Details), under §2.2's bounded retry. So
the DAC can sound the track's first moments, from the stream's start to the seek's release: a
fragment the start's own latency bounds, which the acceptance run measures (§11, A12). It is
accepted for 0.2.4: no engine command starts a track at a frame but a seek's own continuation
(`StartRequest.From`). If the owner finds it worse than a click, the bar sends no position and the
bridge plays the track from its start, a change of one field. DSD plays from the track's start: the
engine does not seek DSD, converted included (`seek.go:119-131`). A DSD track the browser was
playing through its FLAC sidecar (`format.js:155-176`) plays on the bridge as DSD, from its start.

### 7.4 The switch to this browser

The switch-out (§3.2):

- **`done`**: the browser takes the answer's queue as its own (each entry's track, a gone entry left
  out), its selected entry as the current track and, for PCM, its position, and plays if the bridge
  was playing. The browser's seek is its element's, made before it plays, so nothing of the track's
  start is heard. Where the browser refuses a play that no click started, the bar asks for a press.
- **`changed`**: the bar stays on the bridge's player and says "The bridge's player went on playing:
  another controller started it"; the pick is to be made again.
- **`refused`**: its word, the check's as "The DSD check is playing on the DAC: Stop ends it", with
  Stop offered; the tone's as "The test tone is playing on the DAC".
- **Lost, or past 30 s**: unconfirmed until the record is final; the browser starts only on `done`.

**A DSD track switched back to the browser (R5).** The browser plays its fresh FLAC sidecar from the
position where it has one, said to be a converted copy as today (`format.js:155-176`;
`nowplaying.js:266`); otherwise the bar shows it as not playable in this browser and stays paused
there, and Next moves to a track the browser plays (`audio.js:620-646`).

### 7.5 The controls while the bridge's player is chosen (R6, R10)

| Control | While the bridge's player is chosen |
|---|---|
| Play and pause | `play`, `pause`; pause carries no generation (§2.6) |
| Next, previous | `next`, `previous` |
| Stop | shown in the bridge's mode alone, enabled while the engine runs, the DSD check included (C3): `stop`, which lets the DAC go at once, where a pause lets it go after 30 s |
| Seek | the slider while `seek` (PCM of known length); disabled for DSD, with "The bridge's player does not seek within DSD" |
| Repeat | two states, off and all (`repeat`); repeat-one is not offered: "The bridge's player repeats the whole queue or nothing" |
| Shuffle | disabled, with "The bridge's player plays the queue in order": the engine refuses shuffle on (`edits.go:158-165`); no browser-made order is sent, and no browser advance stands in for one |
| The album and playlist "Shuffle" buttons | disabled, with the same words (`views.js:440-448`, `:1908-1911`) |
| Volume and mute | disabled, with "Fixed level: set it on the DAC or the amplifier": the player has no mixer, and its element says its volume is fixed (`serve_player_output.go:172`); the browser's controls are `nowplaying.js:77-105` |
| The persisted browser modes | the browser's shuffle and repeat in session storage (`audio.js:806-839`) stay as they are, and apply again when the browser is chosen |
| MediaSession | every action handler set to null, its metadata cleared and its playback state `none` while the bridge's player is chosen, and set again when the browser is (`audio.js:753-767`) |
| The queue panel | a Queue button opens the engine's queue: the entry heard marked, a row's click is `index`, its remove button is `delete`, a skipped entry shows its word |
| Add to queue | the album's and playlist's "Add to queue" (`views.js:449-452`, `:1912-1915`), and a row's own, placed in the row's Download cell so the row keeps its columns, are `append` |
| A stale answer | the bar takes the state it carries, says "The player changed since this page last read it", and sends nothing more |

Every control the engine does not take now is disabled by the state's `commands` (§4.3), and every
control that disables itself around its request goes through `setDisabled`
(`.claude/rules/admin-console.md`, its bullet). The MediaSession's handlers are cleared rather than
pointed at the bridge: a session belongs to an element that plays in the page, most browsers drop one
with nothing audible, and a hardware key left on the browser's handlers would resume the browser
beside the bridge (§12).

### 7.6 Rows by output

One function decides a row's playability, its chip and its handler, from the row and the chosen
output: the browser's (`play`, as today, `format.js:155-185`) or the bridge's player's (`bridge`,
§6.7). With the bridge's player chosen, a DSD row the set plays has a play handler and reads its way
("DSD64, native", "DSD128, DoP", "DSD256, converted to PCM"), and a row it does not play reads why
("On another server: the bridge's player plays this bridge's own files"). An output change redraws
the view in place. The album's "N of M can't play in a browser" note (`views.js:461-474`) follows the
output: "… can't play on the bridge's player". So does the playlist's and smart mix's action row,
which today offers no button when no track plays in the browser (`views.js:1900-1903`): a DSD-only
playlist gets its Play with the bridge's player chosen.

## 8. Coexistence

- **An app cast** (`DeleteAll`, inserts and `SeekId`, none with a generation: `writes.go:119-145`)
  replaces the queue, as today. During a console edit it stops the console at its next check, with
  one console insert at most in the cast's queue and no console play (§2.2); after the console's
  play it wins. The bar shows the cast within a frame, and reads the queue on its token.
- **A stale tab's edit** is refused `stale` at its first step, nothing changed; the bar shows the
  state, and an edit sent again acts on it, knowingly.
- **Two console tabs** share one order: operations run one at a time and a newer one ends the older
  one's unsent steps (§2.3), so inside the console too the last admitted command wins. Of two
  replaces, the second's first step is refused `stale` when the first ran a step after the second tab
  read the state. A tab with this browser chosen plays apart from the bridge: two outputs, by design.
- **An OpenHome controller** reads a console entry as a full entry (§6.6), and its `TransportState`,
  `Id`, `Time` and `Info` follow the console's commands, which are the engine's.
- **Every surface shows the engine's state**: the console by its frames, the app and every OpenHome
  controller by their reads, the Player page by its status, and `GET /v1/outputs` by its element.
- **The DSD check** (B728 B12) refuses the console's edits and transport commands but Stop, and the
  console's Stop ends it, as every Stop does (`.claude/rules/output.md`, the check's hold;
  `owner.go:593-600`). The bar keeps its view and offers Stop (§4.3).
- **A DAC's DSD choice being written** refuses whatever would start a session (`configuring`) for the
  write's length; edits go on. The bar says "The player is saving its DSD choice".
- **The console's test tone** (B601): a play preempts it; a tone is refused while the player sounds
  (`.claude/rules/output.md`, the tone's bullets); a switch-out with the tone in the slot is refused
  `tone`.
- **B755** (the bridge's player playing another bridge's tracks): until it ships, the console queues
  this bridge's own playable originals alone (§6.2), as an OpenHome Insert does. **B671** (the app
  handing the phone's queue to the bridge's player): a queue it hands over is the engine's like any
  other, which the console reads by its publications and never edits unseen.

## 9. The tests, red first

Each case is red on its PR's base through the real route, the real engine and, for the bar, the
shipped modules; each fix's control is the fix taken out as an uncommitted edit, and its named cases
turn red (§10). The brief's five classes from round 1, and round 2's, and where each is pinned:

| Class | PR 1 (the server) | PR 2 (the bar) |
|---|---|---|
| The API's answers and refusals | `TestTheControlRoutesRefuseWhereThePlayerCannotRun`, `TestEveryEngineRefusalHasItsAnswer`, `TestAStaleGenerationChangesNothing`, `TestAnUnreadablePlayerBlockStopsNothingTheControlAsks`, `TestTheControlRoutesAreGuardedLikeEveryConsoleWrite`, `TestAResentOperationNeverRunsTwice`, `TestEveryReadOnlyAPIRouteAnswers` (the existing smoke test, over the two new GETs) | `TestAStaleCommandRereadsAndNeverReplays` |
| A compound edit raced by another controller (C1, C4) | `TestACastAfterTheConsolesDeleteAllStopsItAtTheNextCheck`, `TestARepeatOrAReportBetweenTwoStepsChangesNothingOfTheEdit`, `TestAnEditBetweenTwoInsertsStopsTheConsoleWithOneMoreAtMost`, `TestAPlayBetweenTheLastInsertAndPlayIDIsOvertakenByIt`, `TestAReplaceOfAnEmptyStoppedQueueGoesOn`, `TestAReplaceWhilePlayingPlaysAfterItsOwnRelease`, `TestTheSeeksRetryFollowsItsOwnEntryAlone` | `TestAPartialReplaceIsShownAsItIs` |
| A lost request and the operations (C1, E2, E3) | `TestALostReplaceHeldBetweenStepsThenAnotherPickNeverPlaysBoth`, `TestAnAdmittedStepParkedWhenTheRequestEndsIsAnsweredNotCancelled`, `TestEveryOperationsOutcomeIsPublished`, `TestANewerOperationEndsTheOldersUnsentSteps`, `TestAStopIsSentAtOnceBesideAnOperationInFlight` | `TestAnUnconfirmedOperationClearsOnlyOnItsRecord`, `TestAReloadedTabFindsItsOperation` |
| The bar's switch both ways (C2, E4, E6) | `TestTheSwitchOutPausesAResumeUnderWayBeforeTheBrowserStarts`, `TestAPlayAfterTheSwitchOutsCheckShowsTheBridgePlaying`, `TestTheSwitchOutsQueueIsTheSilencesOwn`, `TestAPausedHandoverKeepsTheQueueBeforeItsTrack`, `TestAHandoverSeeksPCMAndNotDSD` | `TestTheSwitchToTheBridgeStopsTheBrowserFirst`, `TestTheSwitchToTheBrowserStartsItOnlyOnSilence`, `TestARefusalOrAPausesSuccessIsNotSilence`, `TestAnUnknownSwitchNeverStartsTheBrowser` (both directions), `TestARefusalThatChangedNothingLetsTheBrowserPlayOnOnlyBesideSilence`, `TestThePositionCarriesOverForPCM` |
| The order of the state (C3, E5) | `TestEveryStateIsOrderedByThePublishersRevision`, `TestARevisionIsAssignedOnlyToTheCaptureItNames`, `TestTheAnchorKeepsItsIDAndTimeThroughChangesThatMoveNoPosition`, `TestStopIsOfferedWhereverTheEngineTakesIt`, `TestTheCommandsOfferedAreTheOnesTheEngineTakes`, `TestThePlayerFrameIsQuietThroughSteadyPlayback` | `TestTheBarsPositionHoldsThroughARepeatChange`, `TestTheBarsPositionHoldsThroughAQueueUpdate`, `TestTheBarsPositionHoldsAcrossAReconnection`, `TestANewEpochWinsAndAnOldOneIsDropped`, `TestAHoldNeverWipesTheView`, `TestReturningEligibilityShowsTheActualState` |
| Entries and limits (C4, E7, E8, E9) | `TestEveryLimitIsCheckedBeforeDeleteAll`, `TestAnAppendThatDoesNotFitIsRefusedWhole`, `TestALongListIsWindowedWithPreviousIntact`, `TestAnAppendToAnEmptyQueueSelectsItsFirstTrackAndPlaysNothing`, `TestTheTrackURIAndTheArtworkURLAreBuiltApart`, `TestNoArtworkURLOnAHostTheDLNAListenerDoesNotServe`, `TestABoxWithNoLANHostWritesTheLoopbackURIAndNoRes`, `TestTheBrowseItemIsTheSameBytesAfterTheExport`, `TestAConsoleEntryIsAFullOpenHomeEntry` | — |
| The player off or the DAC gone | `TestThePlayerFrameSaysWhyTheBridgeIsNotOffered` (off at boot, starting, start failed, closed, DAC unplugged, turned off live, another DAC chosen, a check, a release unconfirmed), `TestTheControlRefusesWhatTheOutputRefuses` (edits taken, starts refused) | `TestTheBarKeepsTheViewWhileTheBridgeIsUnavailable`, `TestTheBridgesReturnPlaysNothing` |
| A DSD track by the policy | `TestTheCatalogSaysWhatTheBridgesPlayerPlays` (pcm, native, DoP, converted and refused rates, routed, virtual, gated, lossy; none without a player), `TestADSDRateTheSetRefusesIsLeftOutBeforeTheReplace`, `TestRoutedVirtualGatedAndLossyTracksAreLeftOutBeforeTheReplace` (R3) | `TestRowsFollowTheChosenOutput`, `TestADSDOnlyAlbumPlaysOnTheBridgeFromAnEmptyBar`, `TestADSDTrackSwitchedToTheBrowser` |

And beside them, PR 1's `TestTheConsolesMetadataIsTheContentDirectorysItem`,
`TestTheTransportTakesRepeatAllAlone` and `TestShuffleIsNoControlOp` (R10),
`TestThePlayerFrameFollowsTheEngine` (R9), `TestTheTransportPausesAndPlays`,
`TestTheTransportNextAndPrevious` and `TestAnAppendAddsAtTheEndFromTheTabsGeneration`; PR 2's
`TestTheBarOffersItsOutputFromEmptySessionStorage` (R2), `TestPickingTheBridgeFromAnEmptyBarSendsNothing`,
`TestTheBarDisablesWhatTheEngineDoesNotDo` (R6, R10) and
`TestTheMediaSessionIsClearedWhileTheBridgeIsChosen` (R6).

Each race is made by parking, never by stress (`.claude/rules/build-ci-tests.md`, the window
bullet): the control has instance seams between its steps and in its publisher between a capture's
reads and its publish (`controlSeams`, nil in production); the engine's own (`engine.Config.Seams`)
holds a reservation write; the stand-in backend holds a Start or a Resume. Serve-level cases run the
real engine on a stand-in backend, as serve's player tests do (`.claude/rules/player.md`, the serve
tests' bullet), and the resolver over a real index.

## 10. The PRs

Two PRs, stacked: the server, then the bar. PR 2 cannot be reviewed for its behaviour without PR 1,
and PR 1 is useful alone (the routes can be driven by hand).

### PR 1: the console's control of the bridge's own player (server)

- **Files.** New: `internal/admin/handlers_player_control.go` (the interface, the routes, the DTOs,
  the refusal map, the album expansion, the queue's hydration) and its tests;
  `cmd/bridge/serve_player_control.go` (`consoleControl`: the operations, their steps and records,
  the checks, the window, the references and metadata), `cmd/bridge/serve_player_publisher.go` (the
  publisher, its epoch and the anchor), and their tests, unit and through serve. Changed:
  `internal/admin/admin.go` (the seven routes; `Deps.PlayerControl`),
  `internal/admin/handlers_events.go` (the `player` event from the publisher),
  `internal/admin/handlers_player.go` and `player_audio.go` (the rows' `bridge` field),
  `internal/dlna/content_directory.go` (the exported item builder and its two bases),
  `internal/dlna/server.go` (the artwork base the listener answers), `internal/player/openhome/writes.go`
  (the LAN-host rule exported, one rule for an Insert's URI and a console entry's), `cmd/bridge/main.go`
  (the wiring beside `newConsolePlayer`, and the joins in the player's stop).
- **Red first.** §9's PR 1 column, each seen red on the PR's base (the routes 404, the frame and the
  field absent; a names-only shim where a case calls code the PR adds), then green.
- **Controls**, each the fix taken out as an uncommitted edit, its named cases red, then `git status`
  clean:
  - the IDs compared at the end alone: `TestACastAfterTheConsolesDeleteAllStopsItAtTheNextCheck`;
  - round 1's per-step chain: `TestARepeatOrAReportBetweenTwoStepsChangesNothingOfTheEdit`;
  - a newer operation that does not end the older one's unsent steps (C1):
    `TestALostReplaceHeldBetweenStepsThenAnotherPickNeverPlaysBoth`,
    `TestANewerOperationEndsTheOldersUnsentSteps`;
  - a step on the request's context (E2): `TestAnAdmittedStepParkedWhenTheRequestEndsIsAnsweredNotCancelled`;
  - the record in the handler's answer alone (E3): `TestEveryOperationsOutcomeIsPublished`;
  - the switch-out sending no Pause when it reads paused (E4):
    `TestTheSwitchOutPausesAResumeUnderWayBeforeTheBrowserStarts`;
  - the switch-out's queue read after its answer: `TestTheSwitchOutsQueueIsTheSilencesOwn`;
  - a handler stamping a revision after its own reads (E5): `TestARevisionIsAssignedOnlyToTheCaptureItNames`;
  - the anchor's time set at each publication: `TestTheAnchorKeepsItsIDAndTimeThroughChangesThatMoveNoPosition`;
    the measured position in every publication: `TestThePlayerFrameIsQuietThroughSteadyPlayback`;
  - the commands derived from `offered` alone: `TestStopIsOfferedWhereverTheEngineTakesIt`;
  - the paused handover from the start onward (E6): `TestAPausedHandoverKeepsTheQueueBeforeItsTrack`;
  - the limits checked after `DeleteAll` (E7): `TestEveryLimitIsCheckedBeforeDeleteAll`; the append's
    room from the request alone: `TestAnAppendThatDoesNotFitIsRefusedWhole`;
  - the window cut from the start onward: `TestALongListIsWindowedWithPreviousIntact`;
  - the artwork on the track URI's base, and `albumArtURI` without the listener's check (E8):
    `TestTheTrackURIAndTheArtworkURLAreBuiltApart`, `TestNoArtworkURLOnAHostTheDLNAListenerDoesNotServe`;
  - the bare id where no LAN host is (E9): `TestABoxWithNoLANHostWritesTheLoopbackURIAndNoRes`;
  - `controlRefusal` asking `Unread` (R8): `TestAnUnreadablePlayerBlockStopsNothingTheControlAsks`;
  - the metadata built apart from the ContentDirectory's builder: `TestTheConsolesMetadataIsTheContentDirectorysItem`.
- **Runs.** `make fmt vet test build-all` and `make docs-check`, through the build slot, on
  `GOTOOLCHAIN=go1.27.3`; `-race -count=50` on the race cases and the publisher's.
- **Records** (C5, E10): the table below; and a new entry under `ops/engineering-log/2026-11/`.

### PR 2: the player bar's output (the bar)

- **Files.** New: `internal/admin/static/player/output.js` (the output, §7.2's states, the operations
  and their ids in session storage, §4.2's order and epochs, the anchor's extrapolation, the frame's
  listener) and its tests. Changed: `nowplaying.js` (the output button, the empty bar, the bridge's
  mode, the queue panel), `audio.js` (the switch's two halves, the MediaSession), `views.js` and
  `format.js` (rows by output, the Add and Shuffle buttons, a row's Add), `boot.js`, `app.js` (the
  `player` event), `player.css`, `internal/admin/templates/layout.html` (the output glyph),
  `output.html` (the help text).
- **Red first.** §9's PR 2 column, through the shipped modules under node where node is installed
  (CI runs them; a host without node skips those cases, which the PR says), plus structural cases
  that run everywhere: `TestThePlayerFrameIsWiredFromTheStreamToTheBar` and
  `TestEveryPlayerControlStateFieldIsRenderedSomewhere` (the guard every console payload has, both
  directions); and the existing `TestNoConsoleControlDisablesItselfOutsideSetDisabled`,
  `TestPlayerEmittedClassesAreStyled` and `TestTrackSubgridColumnsMatchTheRow` stay green.
- **Controls**: the browser started on a Pause's answer alone
  (`TestARefusalOrAPausesSuccessIsNotSilence`); on a lost answer (`TestAnUnknownSwitchNeverStartsTheBrowser`);
  the unconfirmed state cleared before the record (`TestAnUnconfirmedOperationClearsOnlyOnItsRecord`);
  the request sent before the browser stops (`TestTheSwitchToTheBridgeStopsTheBrowserFirst`); the
  anchor from the frame's arrival (the three position cases); the revision compared across epochs
  (`TestANewEpochWinsAndAnOldOneIsDropped`); the view wiped on a hold (`TestAHoldNeverWipesTheView`);
  round 1's "bridge, stopped" on eligibility's return (`TestReturningEligibilityShowsTheActualState`);
  the MediaSession's handlers left; the bar hidden when empty; DSD rows left unplayable with the
  bridge chosen; a shuffle order sent.
- **Runs.** As PR 1, and a browser session on a seeded bridge before the suite is believed about the
  bar (`.claude/rules/build-ci-tests.md`, "`hidden === false` says nothing about an ancestor"), at
  375 px and on the desktop, keyboard and screen reader included.
- **Records.** `.claude/rules/admin-console.md`: one line for the bar's output (the browser starts
  only on the bridge's published silence; operations resolved by their records, never sent again;
  states ordered by epoch and revision; the anchor's own time; the MediaSession cleared; shuffle,
  repeat-one and volume disabled; rows by output), its long form in the History file. The help text:
  the Player page's hint (`output.html`) and the bar's own words, which state §3.3's promise word
  for word. CLAUDE.md's `internal/admin` row, a few words. A new entry under
  `ops/engineering-log/2026-11/`. 1-bit.app's pages are outward-facing copy, the owner's to approve,
  and not in either PR.

### The records and their room (C5, E10)

The sizes are `808b19ad`'s, under `scripts/check-doc-budgets.py`'s budgets (100,000 bytes a rules
file, 64,000 for CLAUDE.md); main moves, so each PR measures again at its own base and keeps a margin
of 1 KB. Each move is one `scripts/doc-split/move_entry.py <file> --line N`, each checked with
`--dry-run` at `808b19ad` (nothing written), made one at a time since the line numbers move, and
proved by `verify_moves.py --base <the PR's base>`; each lands verbatim in the History file named.

| File | At `808b19ad` | Room | Adds | Moves first |
|---|---|---|---|---|
| `.claude/rules/player.md` | 99,975 B | 25 B | PR 1: one line in "Player inside bridge serve" on the console's control (its operations, the IDs after each step, the publisher, the entries' two bases), about 900 B | "The feed is the serve's, its snapshots immutable, its mailboxes one slot each." (line 109, 1,449 B, saving 1,234 B) and "A pause is the kernel's PAUSE when the setup offers it…" (line 79, 1,539 B, saving 1,188 B), to `ops/History-rules-player.md`; in reserve, "Validate logs why the player is off once each time the reason changes…" (line 91, 1,481 B, saving 1,141 B). The two largest, "The check after open reads the card from the kernel" (line 96, 5,481 B) and line 139 (1,626 B), cannot move as they stand |
| `.claude/rules/admin-console.md` | 98,296 B | 1,704 B | PR 1: the control API's line, about 700 B; PR 2: the bar's output line, about 700 B | PR 1: "`/api/jobs` is guarded in BOTH directions, down to the LEAF." (line 116, 3,028 B, saving 2,927 B); PR 2, where less than 1 KB is left: "A console save of bridge.yaml that is refused or fails…" (line 889, 2,744 B, saving 2,280 B); both to `ops/History-rules-admin-console.md`. The five largest entries, the Player page's (line 895, 8,822 B) and its test tone's (line 897, 4,649 B) among them, cannot move: `move_entry.py` refuses each, its own "Don't" sentence past a 600-byte pointer |
| `.claude/rules/cli-serve.md` | 97,184 B | 2,816 B | PR 1: one line on the control's publisher and operations, joined in serve's player stop after the engine's Run and before the store closes, about 450 B | none: `move_entry.py --largest` finds one candidate (line 68, 4,901 B) and refuses it, so the line stays within the room |
| `.claude/rules/dlna-upnp.md` | 89,884 B | 10,116 B | PR 1: the item builder's two bases and the artwork base the listener answers, beside the artwork bullet, about 500 B | none |
| `.claude/rules/openhome.md` | 45,573 B | 54,427 B | PR 1: the LAN-host rule shared with the console, on the confinement's line | none |
| `CLAUDE.md` | 58,659 B | 5,341 B | PR 2: a few words in `internal/admin`'s row | none |

## 11. The acceptance list

In a browser alone, on the owner's Pi with its DAC bound and the player running, the app closed, a
fresh browser profile (empty session storage), the library holding a DSD-only album and a PCM album.
Each step names the test that pins it; the Pi run is the hardware's.

| | Step | Pinned by | On the Pi |
|---|---|---|---|
| A1 | The bar shows, empty, with its output button | `TestTheBarOffersItsOutputFromEmptySessionStorage` | the button on the library page |
| A2 | Pick the bridge's player | `TestPickingTheBridgeFromAnEmptyBarSendsNothing`, `TestThePlayerFrameFollowsTheEngine` | the bar shows the player stopped |
| A3 | Open the DSD-only album: its rows read their DSD way | `TestRowsFollowTheChosenOutput`, `TestTheCatalogSaysWhatTheBridgesPlayerPlays` | the words match the Player page's DSD set |
| A4 | Play the album | `TestADSDOnlyAlbumPlaysOnTheBridgeFromAnEmptyBar`, `TestAReplaceOfAnEmptyStoppedQueueGoesOn` | sound; the signal line native, DoP or converted, never bit-exact for converted |
| A5 | Pause, then play | `TestTheTransportPausesAndPlays` | silence, then sound; after 30 s paused the DAC is free for MPD |
| A6 | Seek, in the PCM album (DSD: disabled) | `TestAHandoverSeeksPCMAndNotDSD`, `TestTheBarDisablesWhatTheEngineDoesNotDo` | the track goes on from the slider's place |
| A7 | Skip: next and previous | `TestTheTransportNextAndPrevious` | the next track, then back |
| A8 | Add a track (a row's Add to queue) | `TestAnAppendAddsAtTheEndFromTheTabsGeneration` | the queue panel holds it at the end |
| A9 | Switch back to the browser | `TestTheSwitchToTheBrowserStartsItOnlyOnSilence`, `TestThePositionCarriesOverForPCM`, `TestADSDTrackSwitchedToTheBrowser` | the DAC goes quiet first; the laptop plays the PCM track from the bridge's place |
| A10 | With the app open and OpenHome on: cast from the app while the bar has the bridge chosen; then press a control from before the cast | `TestACastAfterTheConsolesDeleteAllStopsItAtTheNextCheck`, `TestAStaleGenerationChangesNothing` | the bar follows the cast; the press is refused, then acts on the cast |
| A11 | Unplug the DAC with the bridge chosen; plug it back | `TestTheBarKeepsTheViewWhileTheBridgeIsUnavailable`, `TestTheBridgesReturnPlaysNothing` | the view stays, with the DAC's line; back, nothing plays by itself |
| A12 | Switch a playing PCM track to the bridge: listen to the handover | `TestAHandoverSeeksPCMAndNotDSD` | the owner judges the fragment from the track's start (§7.3) |
| A13 | Close the browser while the bridge plays | — | playback goes on |
| A14 | Pause the browser in the middle of an album, pick the bridge, then press Previous | `TestAPausedHandoverKeepsTheQueueBeforeItsTrack` | the bridge holds the whole album, that track selected and stopped; Previous plays the track before it |
| A15 | With the box off its network (the console on the box's own screen), play an album from the console; back on the network, open the app with OpenHome on | `TestABoxWithNoLANHostWritesTheLoopbackURIAndNoRes` | the owner sees how the app shows entries with no stream URL (§13) |

## 12. Alternatives weighed

- **Option C, the bounded fallback** (the console appends, drives the transport, and "play this album"
  appends the album and plays its first new entry, with no `DeleteAll`). Lost: C1's contract is
  stated for B within the budget (§2), so the fallback's condition does not hold; and C leaves the
  old queue ahead of the album, so Previous plays the old queue's tail, and with no `DeleteAll` the
  queue fills: once it holds 1000 entries every "play this album" is refused whole (§6.4) until the
  user removes entries one at a time.
- **Round 1's chain of generations.** Lost: a repeat change or an output report between two steps
  refuses the next step before it runs, an unprovoked partial edit (C4, E1).
- **The engine answers each command's generation; the console counts generations.** Lost, as in
  round 1: an engine change, and not exact for a command whose effect spans owner steps; the IDs need
  neither.
- **The IDs compared once, at the end.** Lost: a cast landing after the console's `DeleteAll` takes
  every console insert and then the console's play over its own (§2.2).
- **A bounded retry for every step.** Lost: an insert retried after a change inserts again into a
  queue another controller changed; the IDs need no retry. Kept for the seek alone, which acts on
  whatever is heard.
- **An operation run to its end whatever the client does** (round 1). Lost: a later pick could not
  stop it, so a lost replace could start the bridge after the browser started (C1's case).
- **The steps on the request's context.** Lost: a context that ends after admission drops the
  engine's answer (E2).
- **Console operations interleaved, without the one-at-a-time order.** Lost: the switch-out's
  silence could be published between an older operation's steps, and that operation's play land
  after the browser started.
- **Pause and Stop conditioned on the generation** (round 1). Lost now: a stale refusal leaves the
  DAC sounding against an explicit ask, the switch-out needs a Pause that is never refused for
  staleness, and Stop is offered wherever the engine takes it (§2.6).
- **The switch-out as a Pause followed by the bar's reads of `now` and the queue.** Lost: a cast or a
  track change can land between the Pause and either read (C3); one publication holds the queue, the
  selection and the position.
- **The switch-out judging silence from a paused snapshot, sending no Pause.** Lost: a resume under
  way reads paused until it answers (E4).
- **Stop in place of the switch-out's Pause.** Lost to the owner's budget ("from the bridge to the
  browser, the bridge pauses"); a Stop would also end a DSD check the user started, which the
  switch-out refuses instead.
- **An anchor and a diff per connection** (round 1). Lost: nothing orders HTTP answers against
  frames (E5), and the extrapolation from a frame's arrival made the position jump at a repeat
  change (C3).
- **An epoch from the clock.** Lost: a box with no clock of its own sets it from the network after
  boot, so a later serve can start with an earlier time.
- **Round 1's no-LAN fallback, the bare id and a relative `<res>`.** Lost: nothing shows that a
  controller renders either (C4), and a relative `<res>` is no URI a renderer can fetch.
- **Refusing edits on a box with no LAN host.** Lost: a box played from its own desktop's browser,
  with no network, could not play from the console at all.
- **Refusing a list longer than the queue takes.** Lost: a long playlist could never play on the
  bridge from the console; the window keeps its start and what comes before it.
- **Appending what fits.** Lost: a partial album at the end of the queue that nobody asked for.
- **A maintenance hold around the compound edit.** Lost: it refuses the console's own commands too,
  every mutation but `Stop`, holds off the app's casts for the edit's length, and is B634b's
  mechanism with the eight findings it drew.
- **The OpenHome surface over loopback as the console's transport.** Lost: it binds LAN addresses
  alone and is off by default, the console must work with it off, and its writes carry no generation
  (`writes.go:119-145`).
- **A bounded poll, or a long poll, for live updates.** Lost: the console's stream is open on every
  page already; a poll adds a request a second a tab while the bridge plays; a long poll holds a
  second connection a tab against the browser's six.
- **The position in every frame.** Lost: it changes every tick and poisons the diff; the anchor
  keeps steady playback silent.
- **An additive catalog ID field for tracks.** Lost: membership is checked on the server at every
  edit anyway, the path is what the catalog, the audio route and the trash carry already, and the ID
  is a pure function of the path (`library.Reference`).
- **Refuse a mixed-source album whole.** Lost: an album with one track on another server could never
  play on the bridge from the console; the web player plays what it can and says what it cannot.
- **Carry the browser's shuffle order to the bridge.** Lost: a browser-made permutation frozen into
  the engine's queue stands in for a shuffle the engine refuses (R6).
- **Route the MediaSession's actions to the bridge.** Lost: most browsers drop a session with nothing
  audible, so the keys would work sometimes, and a lingering one shows the browser as the player.
- **Keep the browser playing until the bridge confirms.** Lost: the two overlap, and on an unknown
  answer both play (R4).
- **Pause, seek and play as the handover, for no fragment from the start.** Lost: three steps where
  two do, and the pause waits behind the start, so the fragment it saves is the start's own latency.
- **Play the bridge from the track's start, no position.** Lost to the owner's decision ("from its
  track and position"); it stays the fallback if the Pi's fragment is too much (§7.3).
- **Ask before replacing a queue another controller made.** Lost: the output menu says what the
  bridge's player is doing before the pick, every OpenHome controller replaces a queue the same way,
  and a second prompt at every switch costs more than it saves.
- **A browser-side list of the bridge's queue, inserting a track at a time as the engine plays.**
  Lost: a second queue owner and a browser-driven advance (R6).
- **One route for each transport command.** Lost: the Player page's writes are separate routes
  because each writes a config field; the transport is one engine surface, one route with a
  `command`.

## 13. Residuals and what stays open

- **One console insert in another controller's queue**, at most, when it edits the queue between two
  of the console's steps (§2.2); the console never deletes it.
- **The last admitted command wins**: a play another controller sends after the console's last
  check starts the bridge beside the browser, and the bar shows it (§3.3).
- **An engine owner that stops answering** (its design rules it out: the owner never blocks on a call
  or a write) keeps an operation `running` and the bar unconfirmed until serve restarts.
- **A closed engine counts as silent** (§3.1). After a panic in Run the backend releases every
  session, bounded, and the supervisor logs the outcome (`.claude/rules/player.md`, the bullet on
  faults staying in the player); until that release answers, a call the engine abandoned can still
  hold the device.
- **A press at a track change**: Play, Next, Previous, an index, a seek, a repeat change or an edit
  sent within a frame's latency of a change carries a generation the change moved, and is refused
  `stale` once; Pause and Stop never are.
- **The fragment from the track's start** at a PCM handover (§7.3), the owner's to judge on the Pi.
- **A PCM rate or depth the DAC does not take** is known only at the open, as the engine's skip with
  the backend's word; the row does not predict it.
- **Entries written while the box had no LAN-eligible host** name the loopback address and carry no
  `<res>` and no `albumArtURI` (§6.6): a controller that reaches the box once it has one shows them
  by their DIDL titles, with no stream URL and no art, and an Insert of such a URI is refused (600);
  the console's next replace writes LAN hosts again. No live controller test was made of this form
  in this round (E9); A15 is where the owner sees it.
- **OpenHome controllers act without a generation**: a cast replaces the queue at any moment, and the
  console follows it.
- **Six console tabs** take a browser's six connections to the console, the stream's property
  already, unchanged here.
- **Not in this design**: B634b, the live DAC switch and the quiet tone from the bar; B989, the
  console on the home LAN; B755 and B671 (§8); the queue panel for the browser's own queue, which the
  bar has never had.

## 14. Round 1's review, R1 to R12, where each now is

| | Settled in |
|---|---|
| R1 | Withdrawn by option B for the exact chain (§1); what stands of it: the empty queue and the no-op (§2.4), a deterministic race at the control's seam after `DeleteAll` (§2.8, `TestACastAfterTheConsolesDeleteAllStopsItAtTheNextCheck`), partial completion without a rollback (§2.7) |
| R2 | §7.1 the output button in the empty bar; §7.6 rows by output; §11 the acceptance from empty session storage and a DSD-only album |
| R3 | §6.1 the server-side path-to-reference mapping; §6.2 membership and the mixed-source rule before any destructive step; §9 routed, virtual, gated and lossy cases |
| R4 | §2.3 the operations, their records and no retry; §7.2 the unconfirmed state; §9 an answer lost after admission, both directions |
| R5 | §7.3 seek for PCM alone, under §2.2's bounded retry, and the fragment from the start; §7.4 a DSD track switched back to the browser |
| R6 | §7.5 every control: the persisted modes, the Shuffle buttons, the MediaSession, volume and mute, none emulated |
| R7 | §6.6 the reference and the metadata, read back through OpenHome, played by the resolver with OpenHome on or off |
| R8 | §5.1 the interface `cmd/bridge` implements; control refusals apart from the config's, Stop and Pause never blocked by an unreadable block |
| R9 | §4 the stream's `player` event from one publisher, the anchor for the position, details and signal in the state; §4.2 the stream against `WriteTimeout`, connections and rate limits |
| R10 | §5.2 no shuffle command, repeat-all alone; §7.5 shuffle disabled |
| R11 | this round is this document alone; the cases, controls and runs are the PRs' plan (§9, §10) |
| R12 | §6.2 every track and every limit checked before `DeleteAll`; the exact chain withdrawn by option B (§1); §2.5 what the queue holds when another controller acts between two steps |

## 15. Round 2's review, C1 to C5 and E1 to E12

| | Settled in |
|---|---|
| C1 | §2.5 the contract for every operation under the four conditions; §2.3 the mechanism (operation ids, one at a time, the unsent steps ended at the request's end, the step in flight resolved, the records published); §2.7 never a rollback, partial shown as partial; §2.8 the lost replace held between steps followed by another output pick |
| C2 | §3.1 observed, settled silence; §3.2 the switch-out as one operation; §3.3 the narrowed promise, in the bar's help word for word (§10, PR 2) |
| C3 | §4.1 and §4.2 one publisher, its revision and epoch, every state ordered by them; §4.4 the anchor's own id and time; §4.3 eligibility, transport, hold, release and commands apart, Stop wherever the engine takes it; §3.2 the switch-out's queue from the silence's own publication |
| C4 | §6.6 the track URI on 7790 and the artwork on the DLNA listener's own endpoint, the bare-id and relative fallback dropped; §6.3 the window with Previous intact; §2.2 no foreign change stops an edit unprovoked; §6.4 an append to an empty queue selects its first track, and why |
| C5 | §10, the records' table: each entry to move, its size, its destination and what it saves |
| E1 | §2.2: unconditional steps after the first, the IDs compared after each step and at the end; the seek alone under a bounded retry; why no per-step chain |
| E2 | §2.3: steps on a context the request does not end, the in-flight step's own answer awaited, the unsent steps ended apart; §2.8's parked-step case |
| E3 | §2.3: each operation's id and final status in the published state, on the event stream; §2.8's reload case |
| E4 | §3.2, step 3: the Pause sent whatever the snapshot reads, so a resume under way is paused; §3.3 the play after the check as the residual, shown as the bridge playing |
| E5 | §4.1 one serialized publisher woken by the engine, the feed, the config, the records and a tick, capturing and assigning the revision together; §4.2 the epoch, the bar's rule and the queue's place in it |
| E6 | §6.5: the whole queue, its start track selected, the position the one thing lost; §2.4 step 2's order |
| E7 | §6.2 every limit before `DeleteAll`; §6.3 the window; §6.4 the append's room, refused whole |
| E8 | §6.6: the exported builder's two bases, `albumArtURI` only where the DLNA listener runs, mounts artwork and takes the host |
| E9 | §6.6: no LAN host, the canonical URI on the loopback literal with no `<res>`; §13 the residual |
| E10 | §10, the records' table: sizes at `808b19ad`, each move dry-run, a 1 KB margin |
| E11 | §6.4: fallback C's append refused whole where the room is short, and what that does to C |
| E12 | §9 and §10: the E2, E4, E6 and E7 cases, each with its control |

**The exits.** Neither applies. C2's promise needs no coordination with other controllers (§3.3),
and no P2 needs an engine change: C3's publisher and order are `cmd/bridge`'s and the bar's, C4's
artwork base is an accessor of the DLNA server's own state, its window and limits are the control's,
and no engine command, field or seam is added.
