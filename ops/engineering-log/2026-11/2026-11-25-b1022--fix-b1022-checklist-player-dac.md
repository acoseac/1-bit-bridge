## 2026-11-25 — B1022: the checklist's DAC line never points MPD at the bridge's own player's DAC

### What

On main (`3bedfc23`) the player checklist's DAC line (`internal/outputcheck`, `dacLine`) judged
the cards of GET /v1/outputs' kind `mpd` elements alone (`cmd/bridge`'s `checklistInput`). On a
box whose bridge's own player played through a USB DAC that MPD's element listed and MPD's output
did not name, the line said `<DAC> (USB 25ce:0025) found, but MPD's output does not name it` and
its fix pointed MPD's `audio_output` at that card (`hw:CARD=DAC`), in `bridge doctor` and on the
console's checklist panel alike. ALSA gives a card to one player at a time: with MPD set to it
too, whichever of the two opens the card second fails.

The change is `internal/outputcheck` and `cmd/bridge/output_checklist.go`, with one comment in
`internal/api/outputs.go`. GET /v1/outputs, the player, MPD's configuration and the Player, Music
and Phone lines are as they were.

### The card each element names

The key is the ALSA card index, `OutputDAC.Card`, read where each element is built:

- kind `mpd`: `dacAt` (`internal/api/outputs.go`) sets it to the card's index in the outputs
  builder's reading of `/proc/asound` (`card.Index`);
- kind `bridge`: `readPlayerDACObserved` (`cmd/bridge/serve_player_output_dac.go`) sets it to the
  index of the card the boot binding resolves to in its own bracketed reading (`found.Index`); the
  element's `dac` is null while the player is unavailable or that reading cannot be had.

Neither the element's id (`bridge:` and the player's UUID) nor a USB id (two DACs of one model
share one) is a key, and card 0 is a card like any other. The two readings are not one: the MPD
half of the body is its builder's answer, kept 500 ms after a build that can take seconds, and the
player's element is built afresh, so a card renumbered between the two is the residual.

### The change

Production: `internal/outputcheck/outputcheck.go` 75 lines added and 17 removed (51 of the added
code, 23 comment), `cmd/bridge/output_checklist.go` 50 added and 13 removed (33 code, 16 comment),
and three lines of comment in `internal/api/outputs.go`.

- `outputcheck.DAC` gains `Card`, and `Input` gains `Bridge *BridgePlayer`: the bridge's own player
  while it is on, its `DAC` the card it plays through, nil while that card is not known.
- `checklistInput` reads the element of kind `bridge` for its card alone (`checklistDAC`, shared
  with MPD's elements): it adds no player, path rule or candidate card, and the checklist still
  shows on MPD's gate alone.
- `bridge doctor`'s one-shot builder lists no element of the player's, so its source gains
  `BridgePlayerOn`: `config.PlayerEnabled` on the GOOS its builder stands for
  (`LocalOutputsConfig.GOOS`: this process's in production, Linux in the tests' seam). With it on,
  the input carries the player with no card.
- `dacLine`, by case, `<DAC>` being `dacName` of the player's element (its name and USB id):
  - the player's card, with no card MPD's output names and no other candidate: OK,
    `<DAC> is the bridge's own player's DAC; MPD's output only matters when 1-bit casts over MPD`,
    no fix;
  - MPD's output (`isMPDsOutput` true) on the player's card: Warn,
    `<DAC> is both the bridge's own player's DAC and MPD's output`, fix
    `It plays for one of them at a time: point MPD at another DAC, or cast over MPD only while the bridge's own player is off.`;
  - candidates beside the player's card: that card left out, and the Warn and its `hw:CARD=` fix
    as before over the others, naming the first of them;
  - MPD's output on another card: its OK line as before;
  - the player on and its card not known (`dac` null; `bridge doctor`), where the line gave a
    `hw:CARD=` fix: Warn, `The bridge's own player is on; its DAC is shown on the console's Player page`,
    fix `Check the bridge's own player's DAC on the console's Player page before pointing MPD at a DAC.`;
    with MPD's output named, or no card at all, the line as before;
  - every other line word for word as before, each pinned whole.
- Not built: a line for MPD's several outputs with the player's card among them
  (`MPD's outputs may include <DAC>, the bridge's own player's DAC`). The body lists the cards of
  an MPD device that does not resolve and those of several devices that resolve alike,
  `isMPDsOutput` null on each (PROTOCOL.md), so the checklist cannot tell that case from the
  candidates above, and telling the two apart is a change to GET /v1/outputs. The line judges both
  as candidates, the player's card left out; the line that case gets, with GET /v1/outputs
  unchanged, is this PR's next entry's
  (`2026-11-25-b1022-player-on--fix-b1022-checklist-player-dac.md`).

### Tests

- `TestTheDACLineNeverPointsMPDAtTheBridgesOwnPlayersDAC` (`internal/outputcheck`): 18 rows, each
  line whole: the cases above, cards 0, 2 and 3, and every line as it was.
- `TestEveryLineIsGreenOrSaysItsFixInOneSentence`: three rows added, the player's card, MPD's
  output on it, and the player with no card.
- `TestChecklistInputMatchesTheBridgesOwnPlayersCard`: an `mpd` element and the player's on the
  same card, on another card of the same model, and with the player disabled, the input's cards
  and the DAC line whole.
- `TestTheChecklistReadsTheBridgesElementForItsCardAlone`, which replaces
  `…ChecklistReadsNoBridgeElement`: beside MPD's element only the player's card is added, the
  element alone adds that card and nothing else, one whose `dac` is null the player with no card,
  and with MPD's gate shut a body carrying the player's element shows no checklist and is not read.
- `TestTheConsoleCallsTheBridgesOwnPlayersDACItsOwn`: the console's panel (`consoleOutputChecklist`)
  over the reported box's two elements, its DAC line whole.
- `TestBridgeDoctorNeverPointsMPDAtADACWhileTheBridgesOwnPlayerIsOn`: `doctorOutputChecklist` with
  MPD's gate open over the `idle` capture's one USB card, the player off and on.

### Red at main

Over main's `outputcheck.go` with only the new names added (`DAC.Card`, `Input.Bridge`,
`BridgePlayer`) and main's `output_checklist.go`, by `go test -overlay`: 8 of the table's 18 rows
red and the 10 that pin a line as it was green; the console's case red with main's line,
`USB DAC (USB 25ce:0025) found, but MPD's output does not name it` and the `hw:CARD=DAC` fix; the
doctor's case red with the player on and green with it off;
`TestChecklistInputMatchesTheBridgesOwnPlayersCard` red in all three cases, the disabled one
because main's input carries no card index (predicted green);
`TestTheChecklistReadsTheBridgesElementForItsCardAlone` red; `TestChecklistInputReadsTheOutputsBody`
and `TestEveryLineIsGreenOrSaysItsFixInOneSentence` green.

### Controls

Twelve, each a mutation of the committed file in an overlay, the red tests predicted by name before
the run; all twelve red exactly as predicted, `git status` unchanged after each, about 9 s each:

| Mutation | Red |
|---|---|
| the player's card with no other candidate a Warn | two table rows, the shape row, the console's case |
| the player's card left a candidate | three table rows, the console's case |
| MPD's output matched to the player by USB id | the input case on another card of the same model |
| card 0 taken for no card | the table row for card 0 |
| no warning for MPD's output on the player's card | two table rows, the input case on the same card |
| the player with no card given the `hw:CARD=` fix | two table rows, the doctor's case with the player on |
| the Player-page line for every state of a player with no card | the two table rows that pin the lines as they were |
| `checklistInput` reads no element of the player's | two input cases, the console's case, the element's test |
| `checklistDAC` drops the card index | three input cases, the element's test |
| the player's card also an MPD candidate | two input cases, the element's test |
| `bridge doctor` does not read whether the config turns the player on | the doctor's case with the player on |
| the checklist shown with MPD's gate shut | the element's test |

### The rules

`output.md`'s composer line: its Never sentence names what `checklistInput` reads now, and the
renamed test. The checklist entry: its sentence on what `checklistInput` reads. `output.md` stood at
99,948 of its 100,000 bytes, so the checklist entry moved to its long form in
`ops/History-rules-output.md` (`scripts/doc-split/move_entry.py --line 54`), leaving its title and
its Don't sentence; `scripts/doc-split/verify_moves.py --base 3bedfc23` passed over a copy holding
the move alone, with the composer line's and the "What stays MPD's" paragraph's edits declared, and
the moved entry is the base's line with that one sentence replaced. A new line for the DAC line's
rule points at its long form, a new section of that History file. `output.md` is 96,910 bytes.

### Runs

One build-slot session at `afca0a54`, on Go 1.27.3 with the release tags: `make fmt`, which
rewrote nothing; `make vet` on linux/amd64 and for darwin/amd64, darwin/arm64, linux/arm64,
windows/amd64, windows/arm64, linux/386 and linux/arm; `cmd/bridge`'s source guards by name
(`TestNoDocblockNamesAnotherDeclaration`, `TestEveryCitedTestNameExists`, `TestNoBlankKeepers`,
`TestEveryBackgroundGoroutineDrainsOnCleanup`); `make test P=8 TEST_PKGS='-count=1 ./...'`, 93
packages ok in 2,030 s (`cmd/bridge` 1,992 s, `internal/manifest` 1,656 s, `internal/admin`
1,466 s); `make build-all`, six binaries; `make docs-check`. Each passed, and `git status` was
clean after. The citation guard read this entry: a test name planted in it failed the guard,
naming the entry, and passed once the bytes were put back.

`7e6df818` then took the fixture addresses out of the new tests (the DAC line reads none, and the
doctor's case takes the config `writePlayerConfig` writes). At it the red at main and the twelve
controls ran again with the same results, and the same session ran again: each step passed,
`make test` 93 packages ok in 2,012 s (`cmd/bridge` 1,989 s), and `git status` was clean after.
