## 2026-11-18 — B903AUDFU: the fault rule's exception stated, its guards pinned, and a deletion's stale fault

Branch `fix/b903audfu`, from `6282e5a6` (main with #1507, B903AUD, and #1508).
Two commits: the tests (`8626c065`), then the rule's words with this record. No
production change, so every production line cited here is `6282e5a6`'s and
HEAD's alike.

### What was said, and what holds

- **The exception.** The fault entry's title and its Don't
  (`.claude/rules/player.md` 122; its long form, `ops/History-rules-player.md`
  137) said that a successor's fault before it is heard moves the selection on
  past the entry heard, with no case where it stays. `fault`
  (`internal/player/engine/transport.go` 1590-1607) selects the entry
  `following` the entry heard only where there is one (1601-1605), and
  `following` (910-913, through `walk`, 918-934) passes over the entries refused
  in the pass (927) and ends at the queue's end with repeat off (923-925): where
  nothing playable follows the entry heard in the pass, the selection stays on
  it. The long form's detail named the last entry with repeat off and not the
  refused case; the title and both Don'ts named neither.
- **The guard.** The brief named the occurrence equality
  (`heard.Occurrence == s.chain[0].id`, 1601) as what keeps a fault of the
  occurrence heard from moving the selection, untested. That guard is
  `successor`: `onFault` passes `i > 0 && …` (1574), and the occurrence heard
  is the chain's first (`onAudible` trims the chain to it, 1499-1500).
  `TestAFailureAfterContentPlayedIsAFault`
  (`internal/player/engine/unplayable_test.go` 107-120) checked the selection
  that fault leaves once the transport has stopped (117), which a control
  without `successor` reddens (K1main below); the fault's own step and the Play
  after it had no assertion. The equality decides only while nothing is heard
  (`o.audible` zero: a session's start clears it, `startSession` 635-637),
  where `following(0)` is the queue's first entry (`queue.after`,
  `internal/player/engine/queue.go` 164-167), and its one case, a session
  started at the queue's first entry, could not tell the two apart.
- **The deleted entry heard.** The brief's third point, the entry heard removed
  while it plays and a later successor's fault leaving the selection on it,
  does not happen. Deleting the selection (`delete`,
  `internal/player/engine/edits.go` 49-79) goes through `selectionDeleted`
  (73-74, 90-115): the entry after it is selected and the session released in
  the deletion's own step, and a fault the released session reports after that
  is not taken (`onFault`'s `current`, 1565-1568; `current`, 900-906, takes a
  session starting or live only). No exception is written for it. A native
  session differs in its release: what reaches it while it retires goes to its
  ledger (`ordinaryRetiring`, `native.go` 246-257), which records a fault and
  drops a pending play (`recordRetiringFault`, 225-245), as
  `TestNativeRetiringPrefixKeepsIntent` pins for a release a PlayID made.

### The rule's words

The pointer's title ends "moves the selection on past the entry heard, unless
nothing playable follows that entry in the pass (B903AUD)", and its Don't reads
"Don't leave the selection on the entry heard when a successor faults unheard
and something playable follows that entry in the pass, or let the fault's stop
name another entry than the one that faulted"; the long form opens with the
same title and closes with the same Don'ts. Its detail says that where
`following` is 0 (the last entry with repeat off, or every entry after it
refused in the pass) the selection stays on the entry heard; that a fault of the
session's first occurrence, the one heard or the one starting, is no
successor's, leaves the selection on its entry, and a Play from Stopped starts
that entry again; that with nothing heard yet the selection stays on the
session's first entry wherever it stands in the queue ("the first entry", which
read as the queue's, is the session's); and that deleting the entry heard
selects the entry after it and releases the session, so an ordinary session's
later fault is not taken, while a native one's ledger records it. The test list
gains the deletion test.

### Tests (8626c065)

- `TestAFailureAfterContentPlayedIsAFault`: a heard, b after it, a faults. The
  fault's own step (`recordSteps`, `faultStep`) and the stopped transport keep
  the selection on a, their stop naming a (`faultStops`), and a Play from
  Stopped starts a (`playsFrom`); its earlier checks stay.
- `TestASuccessorsFaultSelectsTheQueuesNextAfterTheEntryHeard`, two rows:
  - "every entry after it refused in the pass": a heard, b refused in the pass,
    repeat off. b must be refused after its occurrence was handed off, which
    only a later lap's hand-off of b can do: an edit starts a new pass
    (`insert` and `delete` clear `o.pass`, `edits.go` 32 and 67), and a refusal
    cuts the chain at the occurrence refused (`onRefused`, 1611-1625). So a and
    b play with repeat on and every Cut held; b's second lap is refused at its
    Prepare, which leaves b's first occurrence no longer next (`reconcile`'s
    walk passes over b, 955-965) and asks its Cut; then repeat goes off
    (`setRepeat`, `edits.go` 148-156, keeps the pass). b's first occurrence
    faults: the selection stays on a, and Play starts a.
  - "nothing heard yet, the session started past the queue's first entry":
    PlayID b, c named after it, nothing heard; c faults: the selection stays on
    b, the fault and its stop name c, and Play starts b.
- `TestASuccessorsFaultAfterTheEntryHeardWasDeletedIsNotTaken`: a heard, b
  named, every Release held (`holdReleases`); deleting a leaves the transport
  releasing to play b, b selected. a's session then reports b's fault: once the
  owner has handled it, no fault is recorded, and the state, the pending play,
  the last stop and the selection are as the deletion left them; the release
  let go, b starts.
- `holdCalls` with no Prepare to hold (0) sets no Prepare hook, so each Prepare
  is the fake's own answer, which refuses a reference scripted unplayable; its
  other callers script none.

### Controls

Predicted in writing before any ran (`.tmp/_b903audfu/controls.predictions.txt`,
not committed). Each made exact-once replacements in HEAD's `transport.go`,
written to a scratch folder and mapped by `go test -overlay`, so the tree, the
tests' commit, was never edited, and `git status` was clean after each. The six
fault tests ran `-count=1 -v -watchdog 20s` under each, none waiting on a
watchdog.

| | mutation | predicted red | seen |
|---|---|---|---|
| K0 | none | none | none |
| K1 | `successor && ` dropped from `fault`'s condition | `TestAFailureAfterContentPlayedIsAFault`: b selected (a wanted) at the fault's step and once stopped, then its final check | that test alone, as predicted |
| K2 | the occurrence equality dropped | the nothing-heard row started at b: a, the queue's first, selected (b wanted) at the fault's step and once stopped, and Play starting a | that row alone, as predicted; the nothing-heard row started at a stays green |
| K3 | `following` replaced by `o.q.after` in `fault` | the refused row: b selected (a wanted) at the fault's step and once stopped, and Play starting b | that row alone; its first two as predicted, and the Play started a session at b, still scripted unplayable with nothing playable after it, so it answered `unplayable` before its entry check |
| K4 | `onFault` takes its session from `o.sessions` (a releasing one's fault taken) | the deletion test: the fault recorded, the pending play turned to stop | that test alone, both as predicted, the selection still b |
| K1main | K1 with main's `unplayable_test.go` overlaid too | `TestAFailureAfterContentPlayedIsAFault` at its selection check once stopped (117) | as predicted |

Over the whole engine package, for information and not predicted by name, K1
reddens `TestAFailureAfterContentPlayedIsAFault` alone, and K4 the deletion
test and `TestMaintenanceDiscardsEarlierSessionsEvents`, at its second round:
the fault K4 takes in its first round, while the release is held, changes no
pending intent (maintenance released the session to stop, `maintenance.go` 65),
so its step publishes nothing, and the record shows with the acknowledgement's
snapshot. In the deletion test the same fault turns the pending play to stop,
so its own step publishes it.

### The record

`player.md` went from 99,865 bytes to 99,975, under its 100,000 by 25, so the
next addition there must move an entry out first (`move_entry.py --largest`
offers L79, the sink's pause entry, for 1,188 bytes). The pointer is 594 bytes,
keeps every bolded sentence of its long form (`mdblocks.dropped_donts` finds
none left out), and opens the long form. In the working tree `verify_moves.py
--base HEAD` lists as missing exactly the two base blocks the change rewrites,
the pointer and its long form, with their two successors the only new blocks
and no citation broken; `--edit` cannot pair two rewritten blocks that share a
start, so each half was proven alone, the other file put back to HEAD's bytes,
and each passed with `--edit`. `check-doc-budgets.py` passed.

### Runs

On the tests' commit, Go 1.27.3: `go vet` of the engine package and go1.27.3's
gofmt over the two test files clean; the four touched tests (the three changed
and `TestASuccessorsFaultBeforeItIsHeardMovesTheSelectionOn`, which calls the
changed `holdCalls`) under `-race -count=20` green; cmd/bridge's doc guards
(`TestNoDocblockNamesAnotherDeclaration`, `TestEveryCitedTestNameExists`,
`TestNoBlankKeepers`) green over the tree. The gate after the last commit is in
the branch's report: a record cannot hold the runs made on its own commit.
