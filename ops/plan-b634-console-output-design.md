# B634: the console's player bar plays to the bridge's own DAC

Design and PR plan, 2026-11-22, for bridge 0.2.4. Docs only: this round writes no code.

**Status.** For review, round 1, on bridge main `9f98e0c1`. The owner's decision of 2026-11-22 is
binding (§1). The brief's review, R1 to R12, is folded in, and §13 says where each is settled. Every
current behaviour this design relies on or changes is cited as `path:line` at `9f98e0c1`. The parked
11-13 design of a larger B634 (its brief and its decision note) is input: its passcode, its LAN
listener and its live DAC switch are not part of this design, and its compound-edit finding, the
unguarded first insert after `DeleteAll` (carried as B634c), is answered in §2.

**In one paragraph.** An output button on the console's player bar picks *this browser* or *the
bridge's player*. With the bridge's player picked, the bar drives the player's engine through an
admin-only control API under `/api/output/player/`: it follows the engine on the console's existing
event stream, replaces, extends and trims the engine's queue with tracks from the library, and sends
the transport commands, each carrying the generation the bar last read. The engine stays the one
owner of the queue and the state, and gains no command. A compound edit ("play this album now") is a
chain of the engine's own commands, each step's `IfGeneration` taken from a snapshot read after the
step before and held only while that snapshot's queue holds exactly the console's entries (§2). No
method answers the generation a command produced, and none needs to: the engine never hands an
entry ID out twice, so the queue's IDs tell the console's change from anyone else's. DSD plays only
on the bridge's player, by its DSD policy. A switch moves the queue, the track and, for PCM, the
position from one output to the other, and never lets both play.

## 0. What changes, at a glance

| Today | Where | B634 |
|---|---|---|
| The bar plays the library in the browser alone, through one audio element | `internal/admin/static/player/audio.js:1-5`, `:340-358` | The bar plays to this browser or to the bridge's player, picked by an output button (§6) |
| The bar hides while nothing is selected | `internal/admin/static/player/nowplaying.js:182-188` | On a box that runs the player the bar stays, empty, to offer the output (§6.1) |
| A DSD row has no play handler and reads "DSD — not playable in a browser" | `internal/admin/static/player/views.js:745-770`; `format.js:179-185` | With the bridge's player picked, a row's playability is that player's (§4.4, §6.6) |
| Serve's player plays only what an OpenHome controller inserts, the opt-in off by default | `internal/player/openhome/writes.go:388-413`; `.claude/rules/openhome.md` (the opt-in's bullet) | The console inserts and plays through the engine, OpenHome on or off (§3) |
| The console's view of the player starts nothing | `cmd/bridge/serve_player_console.go:25-33`; `.claude/rules/cli-serve.md` (the console's view's bullet) | A control of its own, beside the view, starts playback through the engine's commands alone (§3.1) |
| The console's event stream carries no player state | `internal/admin/handlers_events.go:96-107`, `:257-265` | A `player` event, quiet through steady playback (§5) |
| The catalog's track rows answer playability for the browser alone | `internal/admin/handlers_player.go:810-889`; `internal/admin/player_audio.go:102-138`, `:245-256` | An additive `bridge` field answers it for the bridge's player (§4.4) |

No `/v1` route, PROTOCOL.md section or iOS change: the app already reads and drives the same engine
through OpenHome, and reads the console's entries in the form of its own inserts (§4.3).

## 1. The hazard, the decision and the premises

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

**Premises checked.** Every premise of the brief holds at `9f98e0c1`, with four citations to
correct, none of which changes the design:

- a DSD row reads "DSD — not playable in a browser", not "may not play" (`format.js:182`), as R2 says;
- the console server is `Server.ServeListener` in `internal/admin/admin.go:2247-2350` and its
  middleware is `internal/admin/middleware_auth.go`; there is no `server.go` (R9);
- OpenHome's `Read` and `ReadList` are in `internal/player/openhome/actions.go:348-353` and
  `:384-402`; `writes.go:388-413` stores what they return (R7);
- `playerRefusal` is at `internal/admin/handlers_player_output.go:436-460` (R8), and the shuffle
  refusal at `internal/player/engine/edits.go:158-165` (R6, R10).

## 2. The compound edit (R1, R12)

A replace ("play this album now") is a `DeleteAll` and one `InsertAfter` a track, then a `PlayID`
and, for a switch, a `SeekAbsolute`. Another controller can act between any two of them. This
section says which generation each step carries, and why the guarantee holds without an engine
change.

### 2.1 What the engine gives

- **A reply follows its step's snapshot.** A command's reply is sent once the step that decided it
  has published its snapshot (`internal/player/engine/engine.go:1042-1058`;
  `internal/player/engine/owner.go:287-303`; `doc.go:24-26`). A snapshot read after a reply is that
  step's or a later one.
- **The generation moves at most once a step.** `publishIfDirty` advances it once for every visible
  change the step made (`owner.go:751-770`), and a condition is checked against the published
  generation with the step's own pending change counted (`owner.go:681-699`). So a command
  dispatched after another's visible change in the same step, expecting the earlier generation, is
  refused (`ReasonConflict`, `owner.go:601-602`); one dispatched before it shares its bump.
- **A command shares its step with parked commands alone.** A step is one command or one message,
  then every parked command nothing blocks any more, in its lane's order (`owner.go:252-262`,
  `:510-528`): those a message unblocked (edits park behind a reservation write that has not
  landed, at most `ReserveTimeout`, 5 s: `edits.go:167-203`; `reserve.go:26-35`), and those the
  command itself unblocked. `DeleteAll` can unblock a parked play (its release ends the transport
  call the play waited behind, `transport.go:687-697`), which then finds the queue it emptied and is
  refused `empty_queue` (`transport.go:316-323`), changing nothing.
- **No command answers its generation.** `DeleteAll` answers an error and `InsertAfter` an ID and an
  error (`engine.go:520-537`); the reply carries neither a generation nor a snapshot
  (`engine.go:927-934`).
- **An entry ID is never handed out twice**, by any run, a crash included (`doc.go:238-240`;
  `.claude/rules/player.md`, "No ID and no token is handed out twice"). The list of a queue's IDs
  therefore names its membership and its order exactly: any insert adds an ID never seen before, any
  delete takes one away, and the engine has no move.
- **`DeleteAll` can be a no-op, and can outlive its answer.** On an empty, stopped queue with nothing
  selected it changes nothing visible, and no generation moves (`edits.go:119-145`). With a session
  it releases the session and answers at once (`edits.go:134-137`); the release's acknowledgement is
  a later step that moves the generation again (`internal/player/engine/transport.go:796-876`), and
  so is a release past its deadline (`transport.go:742-778`).
- **An insert into an empty queue selects its entry** and starts nothing (`edits.go:30-36`).
- **An admitted command may have run.** A command whose context ends after the owner admitted it
  answers `ReasonCancelled` without `ErrNotAdmitted`, "it may have taken effect"
  (`engine.go:957-961`); `engine.WithAdmission` tells the caller whether it was admitted
  (`engine.go:1000-1016`).

### 2.2 Which generation a step chains on

R1 asks for each step to chain on the generation the console's own previous step produced. No
method answers it (§2.1). Two ways to have it were weighed, and both fail on the engine's own
behaviour, not only on the budget:

- **The engine answers each command's generation** (an engine change: `reply` would carry the
  generation its step published). For a command whose effect spans owner steps, the generation that
  comes with its answer is either too early or too late. `DeleteAll` answers in the step that
  releases, and the acknowledgement, a later step, moves the generation again, so the next step
  chained on the answer is refused for the edit's own release. `Stop` answers in the
  acknowledgement's step (`engine.go:467-472`), so a cast's insert landing between the release and
  its acknowledgement is in the generation that answer carries, and a `DeleteAll` chained on it
  deletes that insert. The answer's generation is exact for a one-step command alone.
- **The console counts it** (the read generation plus one, from `Generation.String`,
  `internal/player/engine/snapshot.go:78-99`): it reads the engine's representation from outside the
  package, counts on one bump a step, and refuses the edit at its own release's acknowledgement.

So no generation, answered or counted, can tell the console's change from another controller's
across a step that spans two of the owner's. The queue's IDs can. **The chain takes the generation
of a snapshot read after each step, and takes it only while that snapshot holds exactly what the
console's steps made: its IDs the console's own, in the console's order, and its transport the one
the console left.** `IfGeneration` on the next step then refuses anything that changed between that
read and that step. Together they hold the guarantee: **no step of a console edit runs on a queue,
or a transport, that another controller changed since the console's previous step, and no step
overwrites that change.** What such a snapshot may hold beside the console's own change is a change
that touches neither: the engine's own (a release acknowledged, an output report) or another
controller's repeat setting, which no step of the edit writes (§12).

### 2.3 The chain

The inputs are the generation the tab last read (`at`), the tracks T1 to Tn, already checked and
given their references and metadata (§4.2, §4.3), the index k to start at, a position p in seconds
or none, and whether to play.

```text
replace(at, T1..Tn, k, p, play)
 0. Nothing has been asked of the engine yet: every track is checked (§4.2) and a request
    none of whose tracks plays is refused here, nothing changed.
 1. s := Snapshot(). If s.Generation() != at: stale, nothing changed.
    If s.Edits() refuses edits (snapshot.go:487-488): that refusal, nothing changed.
 2. DeleteAll(IfGeneration(at)).
    A conflict: stale, nothing changed. Any other refusal: its word, nothing changed.
 3. Settle. Loop: ch := Changed(); s := Snapshot(); until s shows the transport Stopped
    (which holds no session: snapshot.go:17-18) and nothing starting, an unconfirmed
    release allowed, within ReleaseTimeout + 500 ms; past it, partial, 0 of n. Every
    snapshot read must hold no entry: one that does ends the edit, partial, 0 of n.
    (Changed is taken before the read, so a publish after the read wakes the loop.)
    G := s.Generation(); prev := 0; ours := [].
    On an empty stopped queue step 2 moved nothing, and G is at again.
 4. For i in 1..n:
      id, err := InsertAfter(prev, ref_i, meta_i, IfGeneration(G))
      A conflict: partial, i-1 of n. Another refusal: partial, its word.
      An unknown outcome (§3.5): unknown, i-1 or i of n.
      ours := ours + [id]
      s := Snapshot(): require s.IDs() == ours, the transport Stopped and nothing starting,
      else partial, i of n.
      G := s.Generation(); prev := id
 5. If play: PlayID(id_k, IfGeneration(G)).
      A conflict: partial, n of n, not started. A refusal: the queue in place, not
      started, its word. Superseded: done, the state shows what plays.
 6. If p > 0 and Tk's row is PCM with a length (the console knows the track it asked
    for; the snapshot shows a track only once it is heard): s := Snapshot(); require
    s.Selected() == id_k and the transport Playing; SeekAbsolute(p, IfGeneration(G'))
    with G' := s.Generation(). The engine takes the seek from the starting occurrence's
    own details (seek.go:119-139), and refuses DSD whatever the row said.
      A conflict or a refusal: done, played from the track's start, and the answer says
      why (§6.3).

append(at, T1..Tn)
 Step 0. Step 1. base := s.IDs(); prev := the last of base (0 for an empty queue).
 Step 4 with ours := base + the inserted IDs, checking the IDs alone: an append changes no
 transport, and the transport may move while it runs (a track ends, another controller
 plays) without touching the queue.

delete(at, entry)    Delete(entry, IfGeneration(at)): one step.
```

R1's empty queue and no-op are steps 2 and 3: `DeleteAll` on an empty, stopped queue with nothing
selected publishes nothing, so the settle reads the same generation and the chain goes on from `at`
itself; one that a foreign change follows is seen by the same reads. `PlayID` starts the track from
its start (`engine.go:486-489`); the seek after it is §6.3's handover.

### 2.4 What each interleaving meets

| Another controller's command lands | The engine | The console's chain |
|---|---|---|
| after the tab's read and before `DeleteAll`'s dispatch | moves the generation | `DeleteAll` refused stale; nothing of the console's ran |
| in `DeleteAll`'s own step, before it (a parked edit drained first) | counts its pending change | `DeleteAll` refused (`owner.go:681-699`) |
| in `DeleteAll`'s own step, after it (both parked behind a reservation) | one bump for both | step 3's read holds its entry: partial, 0 of n |
| between `DeleteAll`'s answer and the first insert (R1's race; the parked design's first-insert finding) | its own step | landing before step 3's last read, that read holds its entry: partial; landing after it, the first insert's `IfGeneration` refuses: partial |
| between an insert's answer and the read after it | its own step | the read's IDs differ (an insert, a delete, a `DeleteAll`) or its transport moved (a play): partial |
| between that read and the next insert | moves the generation | the insert refused stale: partial |
| one that changes neither the queue nor the transport (repeat; or the engine's own release or output report) | moves the generation | taken in if it lands before the read, which no step of the edit undoes; refused stale if after it |
| between the last read and `PlayID` | moves the generation | `PlayID` refused: the queue in place, not started |
| after `PlayID` was admitted | — | the last admitted command wins, and the bar shows it |

The first-insert finding (a cast's `DeleteAll` and `Insert` between the page's `DeleteAll` and its
first insert, the page's album then played over the cast) cannot recur: the console's insert runs
only on a snapshot that holds no entry, read at a generation nothing has moved since, and its
`PlayID` only on a queue that is exactly its own. A cast's bare `DeleteAll` in that window is taken
in, since it leaves nothing to overwrite; the cast's insert after it is then seen, and the console
stops before it plays anything.

### 2.5 Partial completion

There is no rollback: the console never deletes what it inserted, which would be one more edit on a
queue another controller is using. The engine then holds the console's first i entries and whatever
the other controller did, stopped unless that controller played. The answer is 409 `queue_changed`
with `progress` (`inserted`, `of`) and the state as read then (§3.4), and the bar shows the queue as
it is, with one line: "The queue changed while the console was replacing it: it holds 3 of the
album's 10 tracks, and the player is stopped." A retry is the user's, a new edit from the state the
bar shows, never a continuation from where the first stopped (R4).

### 2.6 The review's question

R1 asks whether returning the generation from the existing methods counts as an engine change. This
design proposes none: §2.2 shows that an answered generation would not make the chain exact for a
step that spans the owner's steps, that the IDs check is needed with it or without it, and that with
the IDs check the existing methods hold the guarantee. The review decides whether that reading of
R12 ("chain each step on the generation the console's own previous step produced") stands.

## 3. The control API

### 3.1 The interface and its guards (R8)

`internal/admin` imports no player package (`handlers_player_output.go:8-13`), so the control is an
interface of the console's, beside `PlayerOutput` (`:75-117`), that `cmd/bridge` implements:

```go
// PlayerControl is the bridge's own player as the console's player bar drives it (B634).
type PlayerControl interface {
	State(ctx context.Context, live *config.Config) PlayerControlState // §3.3
	Queue(ctx context.Context) PlayerControlQueue                      // §3.3
	Plays() PlayerPlays                                                // §4.4: the codecs and each DSD rate's way
	Replace(ctx context.Context, r PlayerReplace) PlayerControlOutcome // §2.3
	Append(ctx context.Context, r PlayerAppend) PlayerControlOutcome
	Delete(ctx context.Context, at string, entry uint32) PlayerControlOutcome
	Transport(ctx context.Context, r PlayerTransportCommand) PlayerControlOutcome
}
```

`Deps.PlayerControl` holds it; `cmd/bridge/serve_player_control.go` (new) is `consoleControl`, built
in `runServe` beside `newConsolePlayer` over the same `bridgePlayer`, `bridgeOutput` and boot plan.
Generations cross the seam as their strings (`Generation.String`, `snapshot.go:78-99`), parsed by
`engine.ParseGeneration` in `cmd/bridge`; entries as `uint32`; tracks as catalog paths. The view
(`consolePlayer`) stays what it is, starting nothing. The control reads the engine only after
`ready`, as the view does (`serve_player_console.go:299-306`), and only for a player serve started.

**The guards** are the console's own, on every route: `boundaryMiddleware`, `csrfGuard` and
`sessionMiddleware` (`admin.go:2175`): a loopback source and a loopback Host, a JSON body type, a
matching Origin. Then the control's own refusal, `controlRefusal`, asked before anything is read:

- no control wired: 503 `player_not_wired`;
- a public bridge, a signed-in session included: 403 `player_not_available` (a public bridge runs no
  player: `config.PlayerEnabled`);
- a host that is not Linux: 409 `player_not_supported`.

**Control is not config.** `controlRefusal` asks neither the player block's `Unread` nor the managed
settings: those guard a write of `output.player` (`handlers_player_output.go:436-460`), which a
control route never makes. An unreadable player block on disk therefore blocks no Stop and no Pause
of a player that runs (R8). The control routes take neither the console's lock (`lockPlayerWrite`,
`:462-480`) nor bridge.yaml's: they write no config, the engine's owner orders their commands, and
§2's checks make a compound edit consistent; the test tone's route takes neither either
(`.claude/rules/admin-console.md`, the tone's bullet).

### 3.2 The routes

| Method and path | Body | Does |
|---|---|---|
| `GET /api/output/player/now` | — | the state (§3.3) |
| `GET /api/output/player/queue` | — | the queue (§3.3) |
| `POST /api/output/player/queue` | `at`, `album` or `tracks`, `start`, `position`, `play` | replace (§2.3) |
| `POST /api/output/player/queue/append` | `at`, `album` or `tracks` | append (§2.3) |
| `POST /api/output/player/queue/delete` | `at`, `entry` | delete one entry |
| `POST /api/output/player/transport` | `at`, `op`, and `index`, `seconds` or `on` | the transport |

- `at` is the generation string the bar last read; one that does not parse is 400 `bad_generation`.
- `album` is a catalog album ID, expanded on the server to its tracks in the catalog's order, never
  a path prefix (`.claude/rules/admin-console.md`: an album is a set of tracks; identity scopes
  travel as IDs and are expanded server-side). `tracks` is a list of catalog paths. Exactly one of
  the two; a list past the engine's `MaxEntries` (1000, `internal/player/engine/queue.go:12-13`) is
  cut to the 1000 from `start` onward, `start` then naming the cut list's first track, and the
  answer says so.
- `start` is an index into the list (0 by default), `position` whole seconds into that track (none by
  default), `play` whether to start it. With `play` false the list is sent from `start` onward, so
  the track at `start` is the one the engine selects (an insert into an empty queue selects it,
  `edits.go:30-36`): no command selects an entry without playing it.
- `op` is `play` (`Play`), `pause` (`Pause`), `stop` (`Stop`), `next` (`Next`), `previous`
  (`Previous`), `index` (`PlayIndex(index)`: the bar's queue panel is positional, and under the
  tab's generation an index names the entry the tab saw), `seek` (`SeekAbsolute(seconds)`) or
  `repeat` (`SetRepeat(on)`). There is no shuffle op (R10). Every command carries
  `IfGeneration(at)` (`engine.go:458-548`).
- Every body goes through `decodePlayerBody` (one JSON object, no key it does not take, nothing after
  it, `handlers_player_output.go:541-592`), with a route constant of its own for its log line
  (`:600-607`).

### 3.3 The state and the queue

`State` reads one engine snapshot (`snapshot.go:288-502`), the backend's published facts
(`playback.Backend.Facts`, `cmd/bridge/serve_player_output.go:174-185`: in memory, no state file, no
lock a reading of the box holds), the hotplug feed's latest resolution, and the live config the
console hands it, for the withholding (`serve_player_console.go:278-297`). It reads nothing of the
box: the view's DAC reading stays `Report`'s alone (`serve_player_output.go:171`).

```text
offered, why      whether the bridge's player can be picked now, and why not (§6.1)
generation        Generation.String of the snapshot
token, entries    the queue's token and length (snapshot.go:377-383): the bar reads the
                  queue again when the token moves
transport         stopped, starting, playing, paused, releasing, maintenance, as the Player
                  page words it (serve_player_console.go:624-631); pending while releasing
selected, repeat  the selected entry; repeat-all
track             the occurrence heard: its entry, its catalog path and row (title, artist,
                  album, album ID), and the decoder's details (codec, bits, rate, channels,
                  length); named through the resolver's StoredPath and the store's row, once
                  an occurrence, as the view names it (serve_player_console.go:459-494),
                  never from a controller's metadata
position          an anchor: the occurrence, its seconds, whether it advances (§5.2)
signal            what the DAC receives, from the backend's observation of that occurrence:
                  pcm, dop, native or converted, its format, rate and valid bits; a converted
                  occurrence is never called bit-exact
seek              true for PCM content of known length alone, never for native, DoP or
                  converted DSD (seek.go:119-146; engine.TimeSeek)
lastStop          why playback last stopped, and its entry (snapshot.go:461)
```

`Queue` reads one snapshot and answers its generation, token, selection and repeat, and each entry:
its ID, its catalog path through `StoredPath` (`internal/player/library/library.go:504-527`), or the
word for one the library no longer has (`not_indexed`), and its skip's word where the engine skipped
it (`snapshot.go:469-482`). The console hydrates the paths through the catalog, as the album view
does (`player_audio.go:203-271`), keeping an entry whose track is gone.

### 3.4 Answers and refusals

A command that did what it was asked answers 200 with `outcome` (`done`, or `superseded` for a play a
later command replaced before it began: `internal/player/engine/errors.go:94-100`), the state after,
the tracks left out (§4.2) and a note (a cut list, a seek not made). Every other answer is the
console's error envelope (`error`, `message`) with the state as read and, for a compound edit,
`progress`:

| Condition | Status | `error` |
|---|---|---|
| no control wired; a public bridge; not Linux | 503; 403; 409 | `player_not_wired`; `player_not_available`; `player_not_supported` |
| no player serve runs: off at boot, no DAC bound, refused at boot | 409 | `player_not_running`, `reason` `off` |
| the player starting, its start failed, its engine closed (`ReasonClosed`) | 409 | `player_not_running`, `reason` `starting`, `start_failed` or `closed` |
| a body that does not decode; an `at` that is no generation | 400 | `bad_json`; `bad_generation` |
| `ReasonConflict` | 409 | `stale` |
| `ReasonMaintenance` under the DSD check's hold (its reason `check`) | 409 | `check`: a Stop ends the check, as every Stop does |
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
| no track of the request plays on the bridge's player | 409 | `not_playable`, each track's word |
| another controller changed the queue during a compound edit | 409 | `queue_changed` with `progress` |
| a step the engine admitted and did not answer within its bound | 409 | `outcome_unknown` |
| a step the engine never admitted within its bound | 409 | `not_admitted` (it did not run) |

The engine's reasons are its own words (`errors.go:12-125`); the table is the one map from them, and
a reason it does not name answers `player_refused` with the reason beside it.

### 3.5 Deadlines and unknown outcomes (R4)

- **Each step is bounded on the server.** It runs on a context detached from the request
  (`context.WithoutCancel`), bounded by what the engine may take: an edit `engine.ReserveTimeout` and
  a second, as OpenHome's edit budget (`writes.go:39-49`); a transport command `ReleaseTimeout`,
  `StartTimeout` and a second, 8 s (`internal/player/engine/backend.go:11-22`); the settle
  `ReleaseTimeout` and 500 ms. Each runs under `engine.WithAdmission`.
- **An admitted request runs to its end.** The client going away does not stop it: a tab closed
  halfway through a replace must not leave a partial queue nobody asked for. The steps are short
  (a 1000-track replace is about a thousand owner steps and a thousand snapshot reads), and each is
  bounded.
- **Admitted and unanswered is an unknown outcome.** A step past its bound that the engine admitted
  may yet run. The request stops there, answers `outcome_unknown` with the state as read then, and
  never sends the step again. A step the engine never admitted did not run, and the request stops
  there with `not_admitted`.
- **The bar's own bound is 30 s.** Past it, or on a lost connection, the outcome is unknown to the
  bar, which reads the state (`GET now`, then the frames) and never sends the request again (§6.2).
- **A partial edit is retried by the user alone**, as a new edit from the state the bar shows
  (§2.5).

## 4. The queue's sources (R3, R7)

### 4.1 A track's path and its reference

The bar names a track by the catalog's path (`playerTrackDTO.Path`, `handlers_player.go:810-811`),
the name the audio, download and trash routes take already, or names an album by its ID. The control
maps a path to the player's reference on the server: `library.Reference(path)`, which is
`dlna.TrackID("", path)` (`library.go:148-156`), the ID `/dlna/file/` names and the DLNA adapter
builds with an empty root (`cmd/bridge/dlna_wiring.go:751`, `:924-930`). No new index: the catalog's
reads (an album's detail, a playlist, a search) supply the paths, and the resolver's own index of
paths by ID (`library.go:18-31`) checks them. An additive catalog ID field lost (§11).

### 4.2 Every track checked before anything changes (R12)

Step 0 of §2.3 asks, of every track and before the engine is asked anything:

1. the catalog has its row (`CatalogTrackRowsForPaths`); a path it has not is `not_in_library`;
2. `Member(id)` passes (`library.go:481-502`): not `not_indexed`, `routed`, `sacd_virtual_track`,
   `outside_library`, `unknown_container` (WavPack and Monkey's Audio), `ambiguous_reference` or
   `index_unreadable`;
3. its codec is one the player plays: the element's codecs (`serve_player_output.go:160-188`,
   `:475-481`), flac, wav, aiff and alac, and dsf and dff while the effective DSD set plays any rate;
   a lossy codec is `lossy_refused`, the decoders' own word (`internal/player/decode/refuse.go:13`);
4. for DSD, its rate's way in the published set: native, DoP or converted; refused with the set's
   own word (`internal/api/outputs_bridge.go:311-316`), or `dsd_rate_not_played` for a rate the set
   names nowhere (DSD512, which admission refuses).

**A mixed-source list** (an album with a track on an upstream server, or a WavPack beside FLACs) is
handled before any destructive step: the tracks that fail are left out, listed in the answer with
their words, and the bar says "2 of 12 tracks are left out: 1 is on another server, 1 is a WavPack
file". A list none of whose tracks passes is refused, nothing changed. A switch whose current track
fails is refused (§6.3). A track that passes and fails at its open (a PCM rate or depth the DAC does
not take) is the engine's skip with the backend's word (`doc.go:153-157`), which the queue panel
shows on its row (§12).

### 4.3 What a console entry holds (R7)

OpenHome's `Read` and `ReadList` give a controller each entry's reference and metadata byte for byte
(`actions.go:348-353`, `:384-402`), and 1-bit expects a track URI and DIDL-Lite there. A console
entry is built to be indistinguishable in form from 1-bit's own:

- **The reference** is the track's canonical URI, `http://<host>:7790/dlna/file/<id>`
  (`internal/player/trackref/trackref.go:7-25`, `:89-98`), in the form 1-bit's own Insert sends: the
  port 1-bit names in every track URI whatever `dlna.listenAddress` says (`openhome.DefaultTrackPort`,
  `internal/player/openhome/inventory.go:12-15`; `cmd/bridge/serve_player_openhome.go:43-60`), and
  the first host `/v1/health` advertises that an Insert's confinement takes, a private IPv4 address
  first: the same inventory (`openhome.Inventory` from `newPlayerHosts`,
  `serve_player_openhome.go:23-41`) and the same LAN-host rule (`writes.go:415-487`, exported for
  this use), so a console entry is one an Insert would have taken. The resolver parses it again at
  every open and looks at neither its host nor its port (`trackref.go:177-193`; `library.go:7-16`),
  so it plays with OpenHome on or off, and after the box's address changes. On a box with no such
  host, the reference is the bare track ID, which the resolver takes too (`trackref.go:181-187`),
  and the metadata's `<res>` names the route's path alone (§12).
- **The metadata** is the DIDL-Lite item the bridge's ContentDirectory answers for that track
  (`dlna.DIDLForTrack` inside `dlna.WrapDIDLLite`, `internal/dlna/content_directory_xml.go:186-196`,
  `:483`), from the store's row as the DLNA adapter converts it (`manifestTrackToDLNATrackInfo`,
  `dlna_wiring.go:924`), on the reference's host and port, so its `<res>` is the reference: the
  source's `<res>` alone, no rendition's, since a rendition never stands in for the source; and an
  `albumArtURI` only while the DLNA listener mounts its artwork route
  (`.claude/rules/dlna-upnp.md`, the artwork bullet). It is under 64 KiB and XML 1.0 text, the
  bounds an Insert's metadata is held to (`writes.go:489-525`; `queue.go:14-17`). One exported
  builder in `internal/dlna` (a `TrackInfo` method over today's unexported `toDIDLOpts`,
  `internal/dlna/content_directory.go:271-308`) makes both the Browse item and the console's, so
  the two cannot drift.
- **The console names an entry by its reference alone** (`StoredPath`), never by its metadata, as
  the Player page names the track heard (`.claude/rules/player.md`, the Player page's bullet in
  "Player inside bridge serve").

### 4.4 What a row says of the bridge's player

Each catalog track row gains an additive field, present while the player runs:

```text
"bridge": { "way": "pcm" | "native" | "dop" | "converted" | "none", "reason": "<word>" }
```

It is one pure function of the row (its path, codec, routed flag, DSD flag and rate) and of the
player's published facts (`Plays`: the codecs and each DSD rate's way), which step 0 runs too, so a
row and an enqueue cannot disagree on anything but membership. `Member` runs at the enqueue alone:
its cost is an index seek a track, and a playlist of tens of thousands of rows would pay it at every
page. `none`'s words are `routed`, `sacd_virtual_track` (by `manifest.IsSACDVirtualPath`),
`unknown_container` (by `formatgate.Gated`), `lossy_refused`, `not_a_container`, the DSD set's own
words, and `dsd_rate_not_played`. `converted` is said as converted to PCM, never as bit-exact
(CLAUDE.md, "No server-side transcoding on the wire, ever").

## 5. Live updates (R9)

### 5.1 The `player` event

The console's stream, `GET /api/events` (`handlers_events.go:52-383`), gains a named event,
`player`: §3.3's state with §5.2's anchor, published at the fast tick (500 ms, `:267-268`),
diff-suppressed per connection (`:144-155`), its first frame at connect (`:257-265`). It is published
while a control is wired and serve's plan lists its player (`servePlayerPlan.lists`,
`cmd/bridge/serve_player.go:76-83`: the player enabled on the boot config, serve holding its data
directory's lock), whether or not the player started; a box whose player is off, not Linux or public
sends none. app.js hands it to the bar as a `bridge:player` window event, as it hands the pool's
progress to the variant panels (`internal/admin/static/app.js:5739-5750`).
Every controller-visible change moves the frame, since the generation is in it; so do the first
sound (`starting` turning false), the details and the signal, which the generation does not follow
(`snapshot.go:66-72`; `internal/player/engine/events.go:164-177`;
`.claude/rules/player.md`, the generation's bullet). The queue is not in the frame: its token is, and
the bar reads `GET queue` when the token moves. `GET now` serves the first paint, and the bar reads
it every 2 s while the bridge's player is chosen and the stream is reconnecting.

### 5.2 The position's anchor

The connection keeps the anchor it last published (the occurrence, its seconds, whether it
advances) and puts it in each frame unchanged, so the frame's bytes do not move while the track
plays. It takes a new one only when the occurrence, the transport or `starting` changes, or when
the measured position departs from the anchor's extrapolation by more than a second (a seek, a
stall). The bar extrapolates from the frame's arrival while the anchor advances. Steady playback
sends no frame: anything that moves on every tick poisons the per-connection diff
(`.claude/rules/admin-console.md`, the SSE stream's bullet), and the Jobs page's worker grid already
sends an immutable start and ticks locally (`app.js:5752-5756`).

### 5.3 The stream against the console server

`WriteTimeout` stays unset (`admin.go:2293-2309`; `.claude/rules/admin-console.md`, the
`WriteTimeout` bullet), so the stream stays open; it ends when shutdown starts
(`.claude/rules/cli-serve.md`, the event stream's bullet); a long-lived GET checks its Origin
(`handlers_events.go:63-68`; `.claude/rules/auth-tls.md`). The console has no connection cap and, in
loopback mode, no rate limit: the login limiter is public mode's (`admin.go:2273-2310`;
`middleware_auth.go:28-39`). The design adds no long-lived request. A browser keeps six connections
an origin over HTTP/1.1 and every console tab holds one for the stream already; a long poll would
hold a second, so three tabs would take them all (§11).

## 6. The bar (R2, R4, R5, R6, R10)

### 6.1 The output button and the empty bar

The output button sits in the bar's right zone. Its menu has two choices: *This browser*, and *The
bridge's player* with the DAC's name (from the Player page's status, read when the menu opens) and
what it is doing ("playing «title»", "stopped"). The bridge's choice is enabled only while the frame
says `offered`: the player runs, its output is known and available, the live config withholds
nothing, no hold (maintenance or the DSD check) and no unconfirmed release stands. Otherwise it is
disabled with `why`'s words: no DAC was bound when the bridge started, the player is starting, did
not start or has stopped; the DAC is not there (the report's word); the player is turned off or
another DAC is chosen; the DSD check or a maintenance hold; a release not confirmed. The choice is
kept in session storage (`bridge-player-output`) beside the queue (`audio.js:10`), so a full page
load keeps it. The bar follows the frames whichever output is chosen: with this browser chosen, for
the menu's line and for the generation a switch carries.

**The empty bar (R2).** While the stream sends a `player` frame (§5.1), the bar shows even with
nothing selected: a slim strip, "Nothing playing", with the output button. So a browser with empty
session storage can pick the bridge's player before any track is chosen. On a box that sends no
frame (its player off, not Linux, public) the bar behaves as today (`nowplaying.js:182-188`).

### 6.2 The states

| State | The bar shows | What plays | Entered by | Left by |
|---|---|---|---|---|
| browser | the browser's queue and track, as today | the browser | boot (the default); a switch to the browser that completed | a pick of the bridge's player |
| to bridge | "Moving to the bridge's player…", the transport disabled | nothing: the browser has stopped | a pick of the bridge's player with a queue in the browser | the answer (§6.3) |
| bridge | the engine's state (§3.3): its track, position, transport and queue | the bridge's player | a switch that completed; a pick with nothing in the browser; a restored choice while offered | a pick of the browser; `offered` turning false |
| unconfirmed | bridge's view, with "The bridge's player did not answer: this is what it is doing" | whatever the engine does, never the browser | an unknown or lost answer to a switch either way | toward the bridge: a frame showing the switch's track playing (bridge), or any frame once the bar's 30 s have passed (bridge, saying the switch did not finish); toward the browser: a read showing the bridge silent (§6.4, step 2); a pick, which starts over |
| to browser | "Moving to this browser…" | the bridge's player until its pause | a pick of the browser in bridge | the pause's answer (§6.4) |
| gone | "The bridge's player is not available: …"; the transport disabled, the queue panel still the engine's | nothing on the bridge | `offered` turning false in bridge | `offered` true again: bridge, stopped, since the output's return plays nothing (`.claude/rules/player.md`, the output's bullet); a pick of the browser |

**The rule over every state: the browser starts only once the bridge's player is known to be
silent, and a lost or unknown answer never starts the browser** (R4). The bar shows the last state
it received, from a frame or a command's answer; an older one shown for less than a tick gives way
to the next frame.

### 6.3 The switch to the bridge's player

From a bar with a queue in the browser:

1. **The browser stops** at once: its element pauses and lets go of its source (`audio.js:550-554`),
   and its position is read.
2. **The request** replaces the engine's queue with the browser's queue as listed, never a shuffle
   order (§6.5), `start` its current track, `play` whether the browser was playing, and `position`
   its whole seconds for PCM. A browser that was paused moves its queue stopped, from its current
   track onward, so that track is the one the engine selects (§3.2's `play` false); its position is
   not kept, since no command places a stopped player at a position.
3. **Done**: bridge.
4. **Refused with nothing changed** (stale, the current track not playable there, not offered, any
   refusal at or before `DeleteAll`): back to browser, and a browser that was playing plays on, the
   answer saying the bridge did nothing. Where the browser refuses a play that no click started,
   the bar asks for a press, as it does today (`audio.js:452-458`).
5. **Partial, or the queue in place and not started**: bridge, showing the engine as it is; the
   browser stays stopped, since the switch happened.
6. **Unknown or lost**: unconfirmed. The browser stays stopped.

From a bar with nothing in the browser, the pick is the bridge's at once and sends nothing.

**The position (R5).** It carries over for PCM: `PlayID` starts the track from its start
(`engine.go:486-489`), and `SeekAbsolute` follows as soon as `PlayID` answers, which it does once
the Start has answered (`owner.go:704-729`; `transport.go:1299-1316`), with the track's details
known by then (`.claude/rules/player.md`, the bullet on each occurrence's Details). So the DAC can
sound the track's first moments, from the stream's start to the seek's release, before the
position: a fragment the start's own latency bounds, which the acceptance run measures on the Pi
(§10, A12). It is accepted
for 0.2.4: no engine command starts a track at a frame but a seek's own continuation
(`StartRequest.From`), and adding one is outside this budget. If the owner finds it worse than a
click, the bar sends no position and the bridge plays the track from its start, a change of one
field. Another controller's command between `PlayID` and the seek refuses the seek by its
generation, and the answer says "played from the start: the player changed before the seek". A
track the engine skipped at its open moves the selection, so the seek's check (step 6) sends none.
DSD plays from the track's start: the engine does not seek DSD, converted included (`seek.go:119-131`).
A DSD track the browser was playing through its FLAC sidecar (`format.js:155-176`) plays on the
bridge as DSD, from its start.

### 6.4 The switch to this browser

1. **The bridge pauses** if it sounds: `transport pause` at the tab's generation.
2. **Done**: the bar reads the queue (`GET queue`) and the state. The browser takes the engine's
   queue as its own (each entry's track, a gone entry left out), its selected entry as the current
   track and, for PCM, the engine's position, and plays if the bridge was playing. The browser's
   seek is its element's, made before it plays, so nothing of the track's start is heard. Where the
   browser refuses a play that no click started, the bar asks for a press (`audio.js:452-458`).
3. **Stale**: the bar reads the state again; if the bridge no longer sounds, step 2; otherwise it
   stays bridge and says the player changed, so the pick is to be made again.
4. **Unknown or lost**: the bar reads the state; while it reads the bridge sounding it starts
   nothing, and stays unconfirmed; once it reads it silent, step 2.
5. **The bridge silent already** (stopped, paused): no pause is sent, step 2.

**A DSD track switched back to the browser (R5).** The browser plays its fresh FLAC sidecar from
the position where it has one, said to be a converted copy as today (`format.js:155-176`;
`nowplaying.js:266`); otherwise the bar shows it as not playable in this browser and stays paused
there, and Next moves to a track the browser plays (`audio.js:620-646`).

### 6.5 The controls while the bridge's player is chosen (R6, R10)

| Control | While the bridge's player is chosen |
|---|---|
| Play and pause | `play`, `pause` |
| Next, previous | `next`, `previous` |
| Stop | shown in the bridge's mode alone: `stop`, which lets the DAC go at once, where a pause lets it go after 30 s |
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

Every control that disables itself around its request goes through `setDisabled`
(`.claude/rules/admin-console.md`, its bullet). The MediaSession's handlers are cleared rather than
pointed at the bridge: a session belongs to an element that plays in the page, most browsers drop
one with nothing audible, and one that lingers shows the browser as the player; a hardware key left
on the browser's handlers would resume the browser beside the bridge (§11).

### 6.6 Rows by output

One function decides a row's playability, its chip and its handler, from the row and the chosen
output: the browser's (`play`, as today, `format.js:155-185`) or the bridge's player's (`bridge`,
§4.4). With the bridge's player chosen, a DSD row the set plays has a play handler and reads its
way ("DSD64, native", "DSD128, DoP", "DSD256, converted to PCM"), and a row it does not play reads
why ("On another server: the bridge's player plays this bridge's own files"). An output change
redraws the view in place. The album's "N of M can't play in a browser" note (`views.js:461-474`)
follows the output: "… can't play on the bridge's player". So does the playlist's and smart mix's
action row, which today offers no button at all when no track plays in the browser
(`views.js:1900-1903`): a DSD-only playlist gets its Play with the bridge's player chosen.

## 7. Coexistence

- **An app cast** (`DeleteAll`, inserts and `SeekId`, none with a generation: `writes.go:119-145`)
  replaces the queue, as today. The console's frame shows it within a tick, the bar reads the queue
  on its token, and a bar in the bridge's mode shows the cast's queue and track.
- **A stale tab's command**, sent from a generation before the cast, is refused `stale` with
  nothing changed; the bar shows the cast, and a command sent again acts on the cast's queue,
  knowingly.
- **An OpenHome controller** reads a console entry as a full entry (§4.3), and its `TransportState`,
  `Id`, `Time` and `Info` follow the console's commands, which are the engine's.
- **The last admitted command wins**, and every surface shows the engine's state: the console by its
  frames, the app and every OpenHome controller by their reads, the Player page by its status, and
  `GET /v1/outputs` by its element.
- **Two console tabs** each carry their own generation: of two racing replaces the second is refused
  `stale`. A tab with this browser chosen plays apart from the bridge: two outputs, by design.
- **The DSD check** (B728 B12) holds the engine: the frame says `check`, the transport is refused
  `check`, and the console's Stop ends it, as every Stop does (`.claude/rules/output.md`, the check's
  hold; `owner.go:593-600`).
- **A DAC's DSD choice being written** refuses whatever would start a session (`configuring`), for
  the write's length; edits go on. The bar says "The player is saving its DSD choice".
- **The console's test tone** (B601): a play preempts it, and a tone is refused while the player
  sounds (`.claude/rules/output.md`, the tone's bullets).
- **B755** (the bridge's player playing another bridge's tracks): until it ships, the console queues
  this bridge's own playable originals alone (§4.2), as an OpenHome Insert does. **B671** (the app
  handing the phone's queue to the bridge's player): a queue it hands over is the engine's like any
  other, which the console reads with its generation and never overwrites unseen.

## 8. The tests, red first

Each case is red on its PR's base through the real route, the real engine and, for the bar, the
shipped modules; each fix's control is the fix taken out as an uncommitted edit, and its named cases
turn red (§9). The brief's five classes, and where each is pinned:

| The brief asks for | PR 1 (the server) | PR 2 (the bar) |
|---|---|---|
| The API's answers and refusals | `TestTheControlRoutesRefuseWhereThePlayerCannotRun`, `TestEveryEngineRefusalHasItsAnswer`, `TestAStaleGenerationChangesNothing`, `TestAnUnreadablePlayerBlockStopsNothingTheControlAsks`, `TestTheControlRoutesAreGuardedLikeEveryConsoleWrite`, `TestEveryReadOnlyAPIRouteAnswers` (the existing smoke test, over the two new GETs) | `TestAStaleCommandRereadsAndNeverReplays` |
| A compound edit raced by another controller | `TestTheChainStopsAtTheFirstChangeItDidNotMake` (a table over §2.4's rows, on a stand-in engine adapter that yields each interleaving), `TestAnInsertBetweenDeleteAllsAnswerAndTheFirstInsertStopsTheReplace` (R1's race, parked at the control's seam), `TestAnInsertBetweenTheReadAndTheFirstInsertIsRefusedByItsGeneration`, `TestAnEditDrainedInTheConsolesOwnStepIsSeenByItsIDs` (a reservation write held at `engine.Config.Seams`), `TestACastBetweenTwoInsertsLeavesAPartialQueueStopped`, `TestAPlayBetweenTheLastInsertAndPlayIDIsNotOverwritten`, `TestAReplaceOfAnEmptyStoppedQueueChainsOnItsOwnGeneration`, `TestAReplaceWhilePlayingWaitsForItsOwnRelease`, `TestTheReplaceChainsOnWhatItsOwnStepsMade`, `TestAReplaceChecksEveryTrackBeforeItsDeleteAll` | `TestAPartialReplaceIsShownAsItIs` |
| The bar's switch both ways | `TestAHandoverSeeksPCMAndNotDSD`, `TestAnAnswerLostAfterAdmissionIsUnknownNotRetried` (a replace's `PlayID` and a pause, each held in the stand-in backend past its step's bound), `TestTheControlRunsAnAdmittedReplaceToItsEndWhenTheClientLeaves` | `TestTheSwitchToTheBridgeStopsTheBrowserFirst`, `TestTheSwitchToTheBrowserPausesTheBridgeFirst`, `TestAnUnknownSwitchNeverStartsTheBrowser` (both directions: an answer lost after the server's admission), `TestARefusalThatChangedNothingLetsTheBrowserPlayOn`, `TestThePositionCarriesOverForPCM` |
| The player off or the DAC gone | `TestThePlayerFrameSaysWhyTheBridgeIsNotOffered` (off at boot, starting, start failed, closed, DAC unplugged, turned off live, another DAC chosen, a check, a release unconfirmed), `TestTheControlRefusesWhatTheOutputRefuses` (edits taken, starts refused) | `TestTheBarShowsTheBridgeGone`, `TestTheBridgesReturnPlaysNothing` |
| A DSD track by the policy | `TestTheCatalogSaysWhatTheBridgesPlayerPlays` (pcm, native, DoP, converted and refused rates, routed, virtual, gated, lossy; none without a player), `TestADSDRateTheSetRefusesIsLeftOutBeforeTheReplace` | `TestRowsFollowTheChosenOutput`, `TestADSDOnlyAlbumPlaysOnTheBridgeFromAnEmptyBar`, `TestADSDTrackSwitchedToTheBrowser` |

And beside them, PR 1's `TestRoutedVirtualGatedAndLossyTracksAreLeftOutBeforeTheReplace` (R3),
`TestAConsoleEntryIsAFullOpenHomeEntry` (R7: the reference and the item read back through OpenHome's
`Read` and `ReadList`, and the resolver opens the entry with OpenHome on and off),
`TestTheConsolesMetadataIsTheContentDirectorysItem`, `TestTheTransportTakesRepeatAllAlone` and
`TestShuffleIsNoControlOp` (R10), `TestThePlayerFrameFollowsTheEngine` and
`TestThePlayerFrameIsQuietThroughSteadyPlayback` (R9); PR 2's
`TestTheBarOffersItsOutputFromEmptySessionStorage` (R2), `TestTheBarDisablesWhatTheEngineDoesNotDo`
(R6, R10) and `TestTheMediaSessionIsClearedWhileTheBridgeIsChosen` (R6).

Each race is made by parking, never by stress (`.claude/rules/build-ci-tests.md`, the window
bullet): the control has an instance seam between its steps (`controlSeams.afterStep`, nil in
production), and the engine's own (`engine.Config.Seams`) holds a reservation write. Serve-level
cases run the real engine on a stand-in backend, as serve's player tests do
(`.claude/rules/player.md`, the serve tests' bullet), and the resolver over a real index.

## 9. The PRs

Two PRs, stacked: the server, then the bar. PR 2 cannot be reviewed for its behaviour without PR 1,
and PR 1 is useful alone (the routes can be driven by hand).

### PR 1: the console's control of the bridge's own player (server)

- **Files.** New: `internal/admin/handlers_player_control.go` (the interface, the routes, the DTOs,
  the refusal map, the album expansion, the queue's hydration) and its tests;
  `cmd/bridge/serve_player_control.go` (`consoleControl`: the chain, the checks, the references and
  metadata, the state) and its tests, unit and through serve. Changed: `internal/admin/admin.go`
  (the six routes; `Deps.PlayerControl`), `internal/admin/handlers_events.go` (the `player` event
  and its anchor), `internal/admin/handlers_player.go` and `player_audio.go` (the rows' `bridge`
  field), `internal/dlna/content_directory.go` (the exported item builder),
  `internal/player/openhome/writes.go` (the LAN-host rule exported, one rule for an Insert's URI and
  a console entry's), `cmd/bridge/main.go` (the wiring beside `newConsolePlayer`).
- **Red first.** §8's PR 1 column, each seen red on the PR's base (the routes 404, the frame and
  the field absent; a names-only shim where a case calls code the PR adds), then green.
- **Controls**, each the fix taken out as an uncommitted edit, its cases red, then `git status`
  clean: the IDs check removed, the chain on each fresh snapshot's generation alone (the race cases
  and the partial case red); `IfGeneration` dropped from the inserts (the read-to-insert case red);
  every step on `at` (`TestTheReplaceChainsOnWhatItsOwnStepsMade` red: the chain refuses its own
  changes); the settle removed (`TestAReplaceWhilePlayingWaitsForItsOwnRelease` red); the check after
  `DeleteAll` (the check-first case red); `controlRefusal` asking `Unread` (the R8 case red); the
  steps on the request's context (the client-leaves case red); a step sent again after an unknown
  outcome (the unknown case red); the position in every frame (the quiet-stream case red); the
  metadata built apart from the ContentDirectory's builder (the metadata case red).
- **Runs.** `make fmt vet test build-all` and `make docs-check`, through the build slot, on
  `GOTOOLCHAIN=go1.27.3`; `-race -count=50` on the race cases.
- **Records.** `.claude/rules/admin-console.md`: one line for the control API (its routes, its
  guards, control apart from config, its answers), its long form in
  `ops/History-rules-admin-console.md`. `.claude/rules/cli-serve.md`: one line for serve's console
  control beside its view, and the view's own bullet reworded so that the view starts nothing and
  the control starts playback through the engine's commands alone. `.claude/rules/player.md`: one
  line in "Player inside bridge serve" for the chain (§2.2's rule and its IDs check) and the
  entries' reference and metadata (§4.3). The rules files are near their 100 KB budget at
  `9f98e0c1` (admin-console.md 98,296 bytes, player.md 99,975, cli-serve.md 97,184), so the PR moves
  long forms out first (`scripts/doc-split/move_entry.py`, proved by `verify_moves.py`), never cuts.
  A new entry under `ops/engineering-log/2026-11/`.

### PR 2: the player bar's output (the bar)

- **Files.** New: `internal/admin/static/player/output.js` (the output, §6.2's states, the commands,
  the frame's listener, the anchor's tick) and its tests. Changed: `nowplaying.js` (the output button,
  the empty bar, the bridge's mode, the queue panel), `audio.js` (the switch's two halves, the
  MediaSession), `views.js` and `format.js` (rows by output, the Add and Shuffle buttons, a row's
  Add), `boot.js`, `app.js` (the `player` event), `player.css`,
  `internal/admin/templates/layout.html` (the output glyph), `output.html` (the help text).
- **Red first.** §8's PR 2 column, through the shipped modules under node where node is installed
  (CI runs them; a host without node skips those cases, which the PR says), plus
  structural cases that run everywhere: `TestThePlayerFrameIsWiredFromTheStreamToTheBar` and
  `TestEveryPlayerControlStateFieldIsRenderedSomewhere` (the guard every console payload has, both
  directions); and the existing `TestNoConsoleControlDisablesItselfOutsideSetDisabled`,
  `TestPlayerEmittedClassesAreStyled` and `TestTrackSubgridColumnsMatchTheRow` stay green.
- **Controls**: the browser started on an unknown answer (the unknown case red); the browser
  started before the bridge's pause is confirmed (the switch-to-browser case red); the request sent
  before the browser stops (the switch-to-bridge case red); the MediaSession's handlers left (its
  case red); the bar hidden when empty (the empty-storage case red); DSD rows left unplayable with
  the bridge chosen (the rows case red); a shuffle order sent (the disabled-controls case red).
- **Runs.** As PR 1, and a browser session on a seeded bridge before the suite is believed about the
  bar (`.claude/rules/build-ci-tests.md`, "`hidden === false` says nothing about an ancestor"), at
  375 px and on the desktop, keyboard and screen reader included.
- **Records.** `.claude/rules/admin-console.md`: one line for the bar's output and its states (the
  browser starts only once the bridge is known silent; the MediaSession cleared; shuffle, repeat-one
  and volume disabled; rows by output), its long form in the History file. The help text: the
  Player page's hint (`output.html`) and the bar's own words. CLAUDE.md's `internal/admin` row, a
  few words. A new entry under `ops/engineering-log/2026-11/`. 1-bit.app's pages are outward-facing
  copy, the owner's to approve, and not in either PR.

## 10. The acceptance list

In a browser alone, on the owner's Pi with its DAC bound and the player running, the app closed, a
fresh browser profile (empty session storage), the library holding a DSD-only album and a PCM album.
Each step names the test that pins it; the Pi run is the hardware's.

| | Step | Pinned by | On the Pi |
|---|---|---|---|
| A1 | The bar shows, empty, with its output button | `TestTheBarOffersItsOutputFromEmptySessionStorage` | the button on the library page |
| A2 | Pick the bridge's player | `TestPickingTheBridgeFromAnEmptyBarSendsNothing`, `TestThePlayerFrameFollowsTheEngine` | the bar shows the player stopped |
| A3 | Open the DSD-only album: its rows read their DSD way | `TestRowsFollowTheChosenOutput`, `TestTheCatalogSaysWhatTheBridgesPlayerPlays` | the words match the Player page's DSD set |
| A4 | Play the album | `TestADSDOnlyAlbumPlaysOnTheBridgeFromAnEmptyBar`, `TestTheReplaceChainsOnWhatItsOwnStepsMade` | sound; the signal line native, DoP or converted, never bit-exact for converted |
| A5 | Pause, then play | `TestTheTransportPausesAndPlays` | silence, then sound; after 30 s paused the DAC is free for MPD |
| A6 | Seek, in the PCM album (DSD: disabled) | `TestAHandoverSeeksPCMAndNotDSD`, `TestTheBarDisablesWhatTheEngineDoesNotDo` | the track goes on from the slider's place |
| A7 | Skip: next and previous | `TestTheTransportNextAndPrevious` | the next track, then back |
| A8 | Add a track (a row's Add to queue) | `TestAnAppendAddsAtTheEndFromTheTabsGeneration` | the queue panel holds it at the end |
| A9 | Switch back to the browser | `TestTheSwitchToTheBrowserPausesTheBridgeFirst`, `TestThePositionCarriesOverForPCM`, `TestADSDTrackSwitchedToTheBrowser` | the DAC goes quiet; the laptop plays the PCM track from the bridge's place |
| A10 | With the app open and OpenHome on: cast from the app while the bar has the bridge chosen; then press a control from before the cast | `TestACastIsFollowedAndAStaleCommandIsRefused` | the bar follows the cast; the press is refused, then acts on the cast |
| A11 | Unplug the DAC with the bridge chosen; plug it back | `TestTheBarShowsTheBridgeGone`, `TestTheBridgesReturnPlaysNothing` | gone; back, stopped, nothing plays by itself |
| A12 | Switch a playing PCM track to the bridge: listen to the handover | `TestAHandoverSeeksPCMAndNotDSD` | the owner judges the fragment from the track's start (§6.3) |
| A13 | Close the browser while the bridge plays | — | playback goes on |

## 11. Alternatives weighed

- **The engine answers each command's generation.** Lost: not exact for a command whose effect
  spans owner steps (§2.2), the IDs check is needed with it, and it is an engine change.
- **The console counts the next generation from `Generation.String`.** Lost: reads the engine's
  representation from outside it, and refuses the edit at its own release's acknowledgement.
- **`Stop`, then `DeleteAll`.** Lost: one more step and one more window; `DeleteAll` stops playback
  itself, and the settle covers its release.
- **A maintenance hold around the compound edit.** Lost: it refuses the console's own commands too,
  every mutation but `Stop`, holds off the app's casts for the edit's length, and is B634b's
  mechanism with the eight findings it drew.
- **The OpenHome surface over loopback as the console's transport.** Lost: it binds LAN addresses
  alone and is off by default, the console must work with it off, and its writes carry no generation
  (`writes.go:119-145`).
- **A bounded poll, or a long poll, for live updates.** Lost: the console's stream is open on every
  page already; a poll adds a request a second a tab while the bridge plays; a long poll holds a
  second connection a tab against the browser's six (§5.3).
- **The position in every frame.** Lost: it changes every tick and poisons the per-connection diff;
  the anchor keeps steady playback silent.
- **An additive catalog ID field for tracks.** Lost: membership is checked on the server at every
  enqueue anyway, the path is what the catalog, the audio route and the trash carry already, and the
  ID is a pure function of the path (`library.Reference`).
- **Refuse a mixed-source album whole.** Lost: an album with one track on another server could never
  play on the bridge from the console; the web player plays what it can and says what it cannot.
- **Carry the browser's shuffle order to the bridge.** Lost: a browser-made permutation frozen into
  the engine's queue stands in for a shuffle the engine refuses (R6).
- **Route the MediaSession's actions to the bridge.** Lost: most browsers drop a session with
  nothing audible, so the keys would work sometimes, and a lingering one shows the browser as the
  player.
- **Keep the browser playing until the bridge confirms.** Lost: the two overlap, and on an unknown
  answer both play (R4).
- **Resume the browser after any failed switch.** Lost: an unknown answer may have started the
  bridge (R4).
- **Pause, seek and play as the handover, for no fragment from the start.** Lost: three steps where
  two do, and the pause waits behind the start, so the fragment it saves is the start's own latency.
- **Play the bridge from the track's start, no position.** Lost to the owner's decision ("from its
  track and position"); it stays the fallback if the Pi's fragment is too much (§6.3).
- **Ask before replacing a queue another controller made.** Lost: the output menu says what the
  bridge's player is doing before the pick, every OpenHome controller replaces a queue the same way,
  and a second prompt at every switch costs more than it saves.
- **Stop and pause with no generation** (the parked page's rule). Lost to the budget's "each carrying
  `IfGeneration`"; its cost is a press within a frame of a track change refused once (§12).
- **A browser-side list of the bridge's queue, inserting a track at a time as the engine plays.**
  Lost: a second queue owner and a browser-driven advance (R6).
- **One route for each transport command.** Lost: the Player page's writes are separate routes
  because each writes a config field; the transport is one engine surface, one route with an `op`.

## 12. Residuals and what stays open

- **Changes the chain takes in.** A repeat setting of another controller's, an output report and a
  late release acknowledgement that land between a step and the read after it are taken in; none
  is a change of the queue or the transport, and no step of the edit writes them. One that lands
  between the read and the next step refuses that step, a partial edit with no change of the queue
  behind it.
- **A press at a track change.** A command sent within a frame's latency of a track change carries a
  generation the change moved, and is refused `stale` once.
- **The fragment from the track's start** at a PCM handover (§6.3), the owner's to judge on the Pi.
- **A PCM rate or depth the DAC does not take** is known only at the open, as the engine's skip with
  the backend's word; the row does not predict it (the parked page's eligibility finding, unchanged
  here).
- **A box with no advertised LAN address** writes the bare track ID, and a metadata `<res>` with the
  route's path alone; a controller that comes once a LAN address does shows such an entry by its
  title.
- **OpenHome controllers act without a generation**: a cast replaces the queue at any moment, and
  the console follows it.
- **Six console tabs** take a browser's six connections to the console, the stream's property
  already, unchanged here.
- **Not in this design**: B634b, the live DAC switch and the quiet tone from the bar; B989, the
  console on the home LAN; B755 and B671 (§7); the queue panel for the browser's own queue, which
  the bar has never had.

## 13. The brief's review, point by point

| | Settled in |
|---|---|
| R1 | §2.3 the explicit chain, the empty queue and the no-op (steps 2 and 3); §8 the deterministic race at the control's seam between `DeleteAll`'s answer and the first insert; §2.5 partial completion, no rollback; §2.2 and §2.6 the guarantee held without an engine change, and the question for the review |
| R2 | §6.1 the output button in the empty bar; §6.6 rows by output; §10 the acceptance from empty session storage and a DSD-only album |
| R3 | §4.1 the server-side path-to-reference mapping; §4.2 membership and the mixed-source rule before any destructive step; §8 routed, virtual, gated and lossy cases |
| R4 | §3.5 the deadlines, the unknown outcome, reconciliation by reading the state, no retry; §6.2 the unconfirmed state; §8 an answer lost after admission, both directions |
| R5 | §6.3 seek for PCM alone, the handover with another controller between, the fragment from the start; §6.4 a DSD track switched back to the browser |
| R6 | §6.5 every control: the persisted modes, the Shuffle buttons, the MediaSession, volume and mute, none emulated |
| R7 | §4.3 the reference and the metadata, read back through OpenHome, played by the resolver with OpenHome on or off |
| R8 | §3.1 the interface `cmd/bridge` implements; control refusals apart from the config's, Stop and Pause never blocked by an unreadable block |
| R9 | §5 the stream's `player` event, the anchor for the position, details and signal in the frame; the stream against `WriteTimeout`, connections and rate limits |
| R10 | §3.2 no shuffle op, repeat-all alone; §6.5 shuffle disabled |
| R11 | this round is this document alone; the cases, controls and runs are the PRs' plan (§8, §9) |
| R12 | §4.2 every track checked before `DeleteAll`; §2.3 each step chained on the console's own verified snapshot; §2.4 and §2.5 what the queue holds when another controller acts between two steps, and that no step overwrites its change |
