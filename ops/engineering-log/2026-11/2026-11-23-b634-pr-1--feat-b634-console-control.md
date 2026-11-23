## 2026-11-23 — B634 PR 1: the console's control of the bridge's own player, the server

### What

The server half of the player bar's output (`ops/plan-b634-console-output-design.md`, §10's PR 1),
built on the design's round 3 at `f5cb4407` with its brief's bindings, J1 to J10 and K1 to K11,
which the design now records (its §17). New: `cmd/bridge/serve_player_control.go` (the console's
control, `consoleControl`: operations, records and marks, the edits' steps, the barrier, the
entries), `cmd/bridge/serve_player_publisher.go` (one publisher, its revision, `queueRev` and the
anchor), `internal/admin/handlers_player_control.go` (the eight routes under
`/api/output/player/`, their refusals and bounds, the catalog rows' `bridge` field). Changed: the
ContentDirectory's item builder exported with its two bases and a resource option
(`internal/dlna`), `Server.ArtworkBase`, the LAN-host rule and the metadata check exported from
`internal/player/openhome`, the closed engine's facts kept by serve's player, the event stream's
player frame, serve's wiring.

### The premises, checked at the base (J9)

- **One `bridgePlayer` runs at most one engine run.** `startServePlayer` starts one supervisor,
  which starts the player once (`p.start`, one `playerRun`, `eng.Run` on one goroutine); its watch
  returns when Run ends, a panic included ("the player is off until bridge serve next starts"),
  and its deferred shutdown runs once. No supervised restart makes a second engine, and every
  `runServe` makes a fresh `bridgePlayer`, so the closed engine's facts are kept per player and a
  restart starts a fresh control with a fresh epoch. K7's four facts are all read in `shutdown()`:
  Run returned within `bounds.engine`; `releaseAll`'s error, asked only after a panic; the
  backend's Wait and the goroutines still running; the snapshot's unconfirmed release.
- **An entry's fields are public:** `Snapshot.Read` and `ReadList` answer `engine.Entry{ID, Ref,
  Meta}`.
- **`MaxReadList` is `MaxEntries`, 1,000** (`queue.go`): a full queue's IDs fit one `ReadList`.

### Found while building

- **A DeleteAll of an empty queue is taken whatever the edits' state.** With every reservation
  write refused, the engine still answers a DeleteAll of an empty queue nil (it advances no
  token), so the state's commands list a replace only where its first insert would be taken: the
  commands' case tries a replace as the console sends it, DeleteAll and an insert.
- **The lost replace's case, as the design states it, needs its handler to have answered.** A
  request's end ends its operation's unsent steps itself (§2.3), so a replace whose connection
  drops while the handler waits is ended by that drop, and no newer operation's admission is
  tested; the case holds the replace past the handler's wait (it answers `running`), so the drop
  is one no handler sees, and only the switch-out can end it.
- **The switch-out's queue case needs a seam after the answer's publication.** A cast landed after
  the answer returns cannot tell a handover taken from the answering capture from one read later;
  `controlSeams.afterFinal` (nil in production) runs once the answering publication is made and
  before the operation's end is signalled, and the case lands its cast there.
- **A source pin that matches text matches commentary.** The builder's pin first looked for
  `<res` as text in every file of the package and found a backquoted name in a comment of
  `dlna_wiring.go`; it now walks the AST's string literals.
- **The strict writer's medium seam is the process's, so a test's rigs share one install.** The first
  `make test` failed three cases under `-race` alone: each made a second rig while the first rig's
  engine was writing a reservation, and the second rig's `persistentMedium` set the seam again under
  that write. `ctlMedium` now installs it once for every rig alive at a time and puts it back when
  the last one's engine has been joined.

### Red first

The 76 cases this PR adds, each run alone from a test binary built at the base's code: an overlay of
the base's versions of every file the PR changes (with the two struct fields and the one `Deps`
field the cases name added), names-only shims of the three files it adds (every function body
replaced by one that returns its results' zero values), and shims of the four exported names it adds
to `internal/dlna` and `internal/player/openhome`. 75 were red: the control's cases on its zero
answers, the routes' and serve's on a 404, the builder's and the artwork base's on their empty
answers. The 76th, `TestTheConsoleBuildsItsMetadataWithTheContentDirectorysBuilder`, reads its
source from disk, which an overlay does not change; at the base the file is absent and the case
fails at its parse. Two cases first passed there, `TestShuffleIsNoControlOp` and
`TestNoArtworkURLOnAHostTheDLNAListenerDoesNotServe`, each asserting an absence alone; each now asks
what is there as well (the transport's eight commands and repeat offered; the entry's item and its
`<res>`), and both are red at the base.

### The controls

Each the fix taken out as an uncommitted edit, its named cases run, the bytes put back from memory
and the file's `git diff` asked clean after every run; predictions written before any ran
(`.tmp`, not tracked). All thirty went red for the reason predicted, the tree clean after each:

| | Taken out | Red |
|---|---|---|
| M1 | the IDs compared after each step (only at the end) | `TestACastAfterTheConsolesDeleteAllStopsItAtTheNextCheck`: the queue held the cast's entry and three console inserts |
| M2 | (round 1's per-step chain put back) | `TestARepeatOrAReportBetweenTwoStepsChangesNothingOfTheEdit`: the second step refused stale |
| M3 | a newer operation ending the older one's unsent steps | `TestALostReplaceHeldBetweenStepsThenAnotherPickNeverPlaysBoth` (a session started) and `TestANewerOperationEndsTheOldersUnsentSteps` (the older replace done) |
| M4 | (a step on the request's context) | `TestAnAdmittedStepParkedWhenTheRequestEndsIsAnsweredNotCancelled`: the parked insert answered cancelled |
| M5 | the records in the publication | `TestEveryOperationsOutcomeIsPublished` |
| M6 | the switch-out's Pause whatever the bridge reads | `TestTheSwitchOutPausesAResumeUnderWayBeforeTheBrowserStarts`: no pause sent |
| M7 | the handover from the answering capture | `TestTheSwitchOutsQueueIsTheSilencesOwn`: the handover held the cast's queue |
| M8 | revisions assigned by the publisher alone | `TestARevisionIsAssignedOnlyToTheCaptureItNames`: two answers of one revision with two states |
| M9 | the anchor's own time | `TestTheAnchorKeepsItsIDAndTimeThroughChangesThatMoveNoPosition` |
| M10 | the anchor kept within its extrapolation | `TestThePlayerFrameIsQuietThroughSteadyPlayback`: steady playback published |
| M11 | the commands read off the engine's table | `TestStopIsOfferedWhereverTheEngineTakesIt`, `TestTheCommandsOfferedAreTheOnesTheEngineTakes` |
| M12 | the tracks before the start track | `TestAPausedHandoverKeepsTheQueueBeforeItsTrack`: the queue `[B, C]` |
| M13 | the limits before DeleteAll | `TestEveryLimitIsCheckedBeforeDeleteAll`: DeleteAll sent, effect some |
| M14 | the append's room from the queue | `TestAnAppendThatDoesNotFitIsRefusedWhole`: inserts landed until full |
| M15 | the window's reach before the start | `TestALongListIsWindowedWithPreviousIntact`: tracks 601 to 1,500, not 501 |
| M16 | the artwork's own base and the listener's check | `TestTheTrackURIAndTheArtworkURLAreBuiltApart`, `TestNoArtworkURLOnAHostTheDLNAListenerDoesNotServe` |
| M17 | the loopback URI where no LAN host is (the bare id put back) | `TestABoxWithNoLANHostWritesTheLoopbackURIAndNoRes` |
| M18 | (`controlRefusal` asking `Unread`) | `TestAnUnreadablePlayerBlockStopsNothingTheControlAsks`: GET now refused |
| M19 | the ContentDirectory's builder (a literal item) | `TestTheConsolesMetadataIsTheContentDirectorysItem`, `TestTheConsoleBuildsItsMetadataWithTheContentDirectorysBuilder` |
| M20 | the epoch check | `TestAnOperationForAnotherEpochNeverRuns`: it ran |
| M21 | an answer published whatever it found, records compared | `TestACommandThatChangesNothingVisibleIsAnsweredUnderARevisionOfItsOwn`: the old revision answered |
| M22 | `queueRev` by the IDs (by the token) | `TestThePublicationSaysWhenTheQueueLastChanged`: the queue loaded at token 0 moved nothing |
| M23 | marks for 4,096 tabs (256) | `TestAResentOperationNeverRunsTwice`: the resend after 300 tabs ran |
| M24 | records by tab (by recency) | `TestATabsLastOperationCanBeReadBackWhateverOtherTabsDid`: the record gone |
| M25 | the wait for the release a replace began | `TestAPausedReplaceWaitsForTheReleaseItBegan`: release none |
| M26 | the barrier's bound on an unconfirmed release | `TestTheBarrierRefusesAReleaseThatStaysUnconfirmed`: the handler answered running |
| M27 | a closed engine's facts | `TestAClosedEngineIsSilentOnlyOnceItsSessionsWereLetGo`: done before the facts |
| M28 | effect from the engine's answer (from `sent`) | `TestAStaleFirstStepChangedNothingThoughItWasSent`: effect some |
| M29 | a read answered at once (waiting for the next publication) | `TestEveryExplicitCaptureCompletesWhenNothingChanged`: 10 s |
| M30 | (J1) the barrier judged on its answering capture | `TestTheSwitchOutsAnswerIsTheCaptureThatSawSilence`: the answer not silent |

### The records' room

At this branch's base the five rules files measured as the design's table had them at `808b19ad`
(player.md 99,975 B, admin-console.md 98,296 B, cli-serve.md 97,184 B, dlna-upnp.md 89,884 B,
openhome.md 45,573 B). Moved first, by `move_entry.py`, verbatim to their History files: player.md's
"The feed is the serve's…" and "A pause is the kernel's PAUSE…", and admin-console.md's "`/api/jobs`
is guarded in BOTH directions…". After the new lines: player.md 98,446 B, admin-console.md
96,091 B, cli-serve.md 97,653 B, dlna-upnp.md 90,634 B, openhome.md 45,946 B.
