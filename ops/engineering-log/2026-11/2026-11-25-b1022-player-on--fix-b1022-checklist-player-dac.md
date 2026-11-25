## 2026-11-25 — B1022: while the bridge's own player is on, the checklist's DAC line names no card to point MPD at

### What

After this PR's first entry (`2026-11-25-b1022--fix-b1022-checklist-player-dac.md`, at `a5d6a4e9`)
the DAC line could still point MPD at a card the bridge's own player may hold:

- several MPD candidates with the player's card among them: the line took the player's card out
  and pointed MPD at another with "found, but MPD's output does not name it", though MPD's
  several outputs may name both; the body lists the cards of several devices that resolve and
  of one that does not alike (`isMPDsOutput` null on each, PROTOCOL.md);
- the player on with its card not known: MPD's confirmed output read OK, and no card at all
  read as the zero-card Fail, whose fix points MPD's `audio_output` at a card;
- serve before the player has an identity: GET /v1/outputs lists no element of the player's
  then, so the console's checklist read the player as off and gave the `hw:CARD=` fix;
- the body's two readings: its MPD half is its builder's answer, kept 500 ms after a build that
  can take seconds, while the player's element is read afresh, so the player's DAC renumbered
  between them, its ALSA id kept, read as a free candidate whose `hw:CARD=<id>` named the
  player's DAC;
- the doctor's wording, which sent the reader to the console's Player page whether or not
  serve runs, and promised a DAC that page may not show (a player enabled with no DAC bound).

GET /v1/outputs is unchanged.

### The change

Production: `internal/outputcheck/outputcheck.go` 85 lines added and 55 removed (64 of the added
code, 20 comment), `cmd/bridge/output_checklist.go` 10 added and 6 removed (comments alone), and
`cmd/bridge/main.go` 4 added and 1 removed (one line of code, three of comment).

- `dacLine` is main's line, word for word, while the player is off, and `playerDACLine` while it
  is on; no line of `playerDACLine` names a card to point MPD at. Its cases, `<DAC>` the player's
  element's `dacName`:
  - the player's card, with no card MPD's output names and no other candidate: OK, as before;
  - MPD's output on the player's card, looked for over every confirmed card first: the warn it
    was;
  - MPD's output on another card: its OK line, as before, a card renumbered since MPD's half
    named it among them; that line gives no fix, and the warning it misses is the residual;
  - several candidates with the player's card among them: warn, `MPD's output may include
    <DAC>, the bridge's own player's DAC`, fix `Check MPD's audio_output before changing it: the
    bridge's own player plays through <DAC>.`;
  - candidates without the player's card (elsewhere, or renumbered since MPD's half was read):
    warn, `<A> found, and the checklist cannot tell whether MPD's output names it`, or for several
    `<A>, <B> found, and the checklist cannot tell which of them, if any, MPD's output names`,
    the same fix;
  - the player on with its card not known (an element whose `dac` is null, serve's player before
    its identity, `bridge doctor`): MPD's output named is a warn of its own, `<A> is MPD's output,
    and the checklist cannot tell whether the bridge's own player plays through it too`, fix
    "Check the player's DAC binding (`bridge output dac inspect`, or the console's Player page):
    if it names this DAC, point MPD at another one, or cast over MPD only while the bridge's own
    player is off."; every other state, no card at all among them, is a warn, `The bridge's own
    player is on, and the checklist cannot tell which DAC it plays through`, fix "Check the
    player's DAC binding (`bridge output dac inspect`, or the console's Player page) before
    changing MPD's output.": a line the doctor reaches names the CLI beside the console, and none
    promises a DAC.
- Serve's checklist source reads the player's gate as serve decided it at boot
  (`BridgePlayerOn: playerView.Enabled`: the boot plan's `lists`, enabled on the boot snapshot with
  the data directory's lock held, no I/O); with no element of the player's in the body, the input
  carries the player with no card.
- `bridge doctor` reads `config.PlayerEnabled` over the configuration it loaded, its file and its
  own environment, and that is its guarantee: a service whose environment turns the player on
  beside a file that leaves it off is one the doctor run from a shell reads as off. Asking the
  running service whether its player is on was not built; the doctor's comment and the rules'
  long form say the limit.
- Comments: `Input.Bridge` and `BridgePlayer` (`internal/outputcheck`), the source's
  `BridgePlayerOn` and `doctorOutputChecklist` (`cmd/bridge/output_checklist.go`), serve's literal
  (`cmd/bridge/main.go`).

Two instructions for this change differed on the player with no card known and no card listed:
one kept the zero-card Fail (no card exists to point MPD at), the other put the warning on the
player's DAC binding ahead of it. The Fail's fix points MPD's `audio_output` at a card, so the
stricter, the warning, is taken. MPD's output named while the player's card is not known gets a
warning of its own, apart from the one for the other states. The first entry's rows that gave
`hw:CARD=` beside the player (a free DAC beside the player's card, a DAC neither uses with the
player's elsewhere) are superseded by the lines above.

### Tests

- `TestTheDACLineNeverPointsMPDAtTheBridgesOwnPlayersDAC`: 22 rows, each line whole; the
  superseded rows replaced, new rows for several candidates with and without the player's card
  (cards 0, 2 and 3), the player's DAC renumbered from card 2 to 3 with its ALSA id kept (one MPD
  candidate, and MPD's output named), MPD's output on the player's card after another confirmed
  card, and the player with no card beside MPD's output named and beside no card; every
  player-off row as it was.
- `TestEveryLineIsGreenOrSaysItsFixInOneSentence`: four rows added for the new lines.
- `TestChecklistInputMatchesTheBridgesOwnPlayersCard`: the player's element carries a redacted id,
  so a match by id fails the same-card case.
- `TestAPlayersDACRenumberedSinceMPDsHalfGetsNoCardFix`: the renumbered DAC through
  `checklistInput`, one MPD candidate.
- `TestTheConsoleKnowsThePlayerIsOnBesideABodyWithNoElementOfIts`: the console's panel with a
  source that says the player is on and a body with no element of the player's.
- `TestServeChecklistReadsTheLiveRootsAndTheRoute`: serve's literal names `BridgePlayerOn:
  playerView.Enabled`.
- `TestServesChecklistKnowsThePlayerIsOnBeforeItHasAnIdentity`: serve booted with MPD's gate on
  and its own player enabled with no DAC bound and no identity; the console's DAC line over the
  production source.
- `TestBridgeDoctorNeverPointsMPDAtADACWhileTheBridgesOwnPlayerIsOn`: seven rows through
  `doctorOutputChecklist` with MPD's gate open: the player off, off beside MPD's output named, on,
  on with no DAC bound, on through the doctor's own environment, on beside MPD's output named,
  and on with no card.

### Red at the base

Over `a5d6a4e9`'s `outputcheck.go`, `output_checklist.go` and `main.go` by `go test -overlay`, the
new names needing no shim: 20 of the 21 predicted reds, among them 10 of the table's 22 rows (the
12 others green, every player-off row with them), the renumbered case, the console's case, five of
the doctor's seven rows and the serve case. `TestServeChecklistReadsTheLiveRootsAndTheRoute`
parses `main.go` from disk, which an overlay does not reach; the control below removes the line
on disk and shows it red.

### Controls

Ten, each a mutation of the committed file, by overlay but for the one on `main.go`, written on
disk and its bytes put back after; the red tests predicted by name before the run, all ten red
exactly as predicted, `git status` unchanged after each, about 10 s each:

| Mutation | Red |
|---|---|
| several candidates with the player's card read as candidates without it | the two rows of that case |
| candidates without the player's card get the line of a box with no player | three rows, the renumbered case |
| the player with no card beside MPD's output named is OK | the table's row, the doctor's row |
| the player with no card and no card listed is the zero-card Fail | the table's row, the doctor's row, the serve case |
| MPD's output on the player's card looked for at the first confirmed card alone | the row of that case |
| serve's literal without `BridgePlayerOn` (on disk) | the literal's test, the serve case |
| the source's word that the player is on not read | the console's case, the serve case, five doctor rows |
| the doctor judging the player on another host than its builder's | five doctor rows |
| MPD's output matched to the player by ALSA id | the same-card input case, the renumbered-and-named row |
| card 0 no card among the candidates | the row with the player's card 0 among them |

The first run of the controls passed `-overlay` between `-tags` and its value, so the overlay
ones failed to set up; run again with the flags in order, and the first mutation, which left a
variable unused, made a condition that keeps it.

### The rules

`output.md`'s line on the DAC line is retitled to the rule above, its Don't sentence and its tests
with it, and its long form in `ops/History-rules-output.md` rewritten; the "What stays MPD's"
paragraph there says the source reads the player's boot gate; the checklist entry's pointer gains
the period it lacked. `scripts/doc-split/verify_moves.py --base a5d6a4e9` lists the retitled line
and its long form as missing beside their successors, and pairs every other edit.
