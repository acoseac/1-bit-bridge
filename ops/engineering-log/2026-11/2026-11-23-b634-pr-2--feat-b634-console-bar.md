## 2026-11-23 — B634 PR 2: the player bar's output, this browser or the bridge's player

### What

The bar's half of B634 (`ops/plan-b634-console-output-design.md`, §10's PR 2), stacked on PR 1's
server (#1555) at `a79c69b2`. New: `internal/admin/static/player/output.js`, the output and its
phases, the operations under a tab identity each document draws, their records read back, the
order of the states by epoch and revision, the queue read against `queueRev`, the barrier every
start of the browser crosses, the anchor's extrapolation by GET now's clock samples, and the
stream's lifecycle; and its harness, `internal/admin/testdata/bar_harness.mjs`, which runs the
shipped modules under node over a fake console, a fake clock and a model of the page. Changed:
`nowplaying.js` (the output button and its menu, the empty strip, the bridge's mode, the queue
panel), `audio.js` (the switch's two halves: the handover read before the source is let go, a
suspension that advances the playback generation, the take and the resume at a position, the
MediaSession), `views.js` and `format.js` (rows by output through `playFor`, the Add and Shuffle
buttons, a row's Add), `boot.js` (`wireOutput`), `app.js` (the `player` event, the stream's
lifecycle with a serial for each open), `player.css`, `layout.html` (three glyphs) and
`output.html` (the help, the promise word for word). On the server, the brief's Lows L1 to L5 and
M1's clock, and nothing else (M9).

### The server's Lows

- **L1 (M4, P5).** `prepare` asks `endedNow` between a track's `Member` and its `entryFor` (a seam,
  `afterMember`, holds a case there) and once the distinct-track loop is done, so a single track's
  replace whose operation ends inside its lookup is cancelled with nothing sent; the case cancels
  inside a `GetTrack` hook answering `(nil, err)` and `(nil, nil)`.
  `TestACancelInsideTheLastLookupIsCancelled`, `TestACancelInsideTheMembershipCheckLooksUpNothing`.
- **L2 (M5).** Asked first whether a zero publication is reachable: only where the publisher ends
  before its first publication, since its run publishes its first capture before it takes any ask
  or the quit, and `settleOn` sets the operation's publication after the publisher's answer on every
  final path. So no server fix: `TestASettledAnswerCarriesThePublicationThatSettledIt` pins the
  settled answer, a retry at the tab's mark and the read back (revision at or above the silence,
  above 0), and a publisher ended after a publication (that publication kept, the record unknown,
  silence and handover cleared). The bar takes a publication of revision 0 as no state
  (`TestARevisionZeroIsNoState`).
- **L3 (M6).** `waitsBehind` hears a newer operation's wait on its own goroutine, through a seam
  (`awaitPrev`) that replaced the three `waitEnded` callers' poll of the records, which the
  admission fills before that goroutine starts; `awaitClosed` is a seam before
  `silenceWithoutEngine`'s select. No poll, sleep or deadline loop is left.
  `TestTheWaitForANewerOperationIsHeardWhereItWaits` holds the admission and requires the wait
  unheard there, then heard once it is let go.
- **L4.** The cancel hook's `time.After` fallback fails the case instead of passing it on
  (`cancelHook`), and the premise is given up a second earlier (`premiseGiveUp`), so a broken
  premise fails by its own name.
- **L5 (M7, P3, P4).** `run` asks `c.quit` again once its predecessors have ended, before the body
  (a select with a default): an operation the stop ends before its first step ends unknown, sent 0,
  effect none, its body never run. `TestTheControlsStopEndsAnOperationWaitingBehindAnOlderOne`
  goes through the real `stop()`: the older operation held at its seam, the newer one waiting
  behind it, `stop()` from another goroutine, the newer one asserted ended before the older one is
  let go. `TestAPublicationCarriesTheLastRecordsWithoutTheirHandovers` pins the published records
  (the last eight, oldest first, no handover and no tracks left out, each read back whole by its
  own tab).
- **M1 (P1).** `admin.PlayerControl` gains `Clock`, the control's `now`, which stamps every
  capture's `at`; GET now sets `X-Bridge-Clock` to it as it answers (`time.RFC3339Nano`, UTC).
  `TestGETNowSaysTheServersClockAtSend` (the header the clock at send while the publication is
  older), `TestTheControlsClockIsTheClockAtSend`.

Red at the base: five of the eight (L1's two, L5's stop, M1's two); the other three are pins and
green there by design (L2's, the records' cap, L3's hearing of the wait).

### The bar, against the brief's M and P items

- **M1, the clock.** The offset is the least `received − X-Bridge-Clock` over GET now's samples of
  the current epoch and connection, never `received − at`: a tab opened on an anchor 100 s old
  would read that age as skew and show the anchor's seconds for ever
  (`TestAClockSampleIsTheServersClockAtSend`, the aged anchor). Sampled on load and at each stream
  connection; the anchor's seconds, unextrapolated, until a sample lands; a sample of an earlier
  connection, epoch or sampling generation dropped whole (P7); a failure extrapolates nothing and
  is asked again within 5 s; reset at a new epoch, a new connection, `visibilitychange` to visible,
  a `pageshow` from the back/forward cache and a clock jump past a second.
- **M2, the identity.** Each document draws a tab identity of its own, its count from 0; session
  storage keeps one only to read the inherited operation back (P2: the boot reads `{tab, seq,
  epoch}` for that alone). `TestClonedTabsNeverShareAnOperation` clones the storage and sends a
  switch-out from each document: identical commands under distinct identities, so the second is
  never answered with the first's record, whatever the bridge did between them. It does not model a
  restart of the bridge's playback between the two; the distinct identities are what it pins.
  `operation_conflict` takes a new identity and sends nothing again
  (`TestAnOperationConflictTakesANewTabIdentity`).
- **M3, the suspension.** `suspend` advances the browser's playback generation, so a failure probe
  pending against the track it played marks nothing after the switch
  (`TestTheSwitchInvalidatesTheBrowsersPendingFailureProbe`), and the restored position's seek is
  generation-guarded (P6, `TestTheSwitchCancelsTheRestoredPosition`).
- **P8, the stream's lifecycle.** app.js gives each open a serial, a native reconnection (the same
  EventSource) included, and drops what a retired one dispatches;
  `TestTheStreamsLifecycleReachesTheBar` runs app.js's shipped functions through a native
  reconnection and a boosted navigation's replacement, each with a clock answer that lands late.

### Red first, and the controls

Every new bar case, 50 of them (45 under node in `player_output_bar_test.go`, the stream's
lifecycle, and four structural cases), was run at the base with the base's modules and a names-only
`output.js`: all 50 red. Each fix's control is the fix taken out as an uncommitted edit, run over
the whole bar family, the file put back from HEAD after, the tree clean after each:

- §10's twenty (the browser started on a Pause's answer, on a lost answer; the unconfirmed state
  cleared before the record; the request before the browser stops; the anchor from the frame's
  arrival; the view wiped on a hold; "stopped" on eligibility's return; the MediaSession's handlers
  left; the bar hidden when empty; DSD rows unplayable with the bridge chosen; a shuffle order
  sent; G1, G2, G3, both G4s, G5, G8, G9, G11): each red at its named cases.
- The brief's nine (M1's offset from the capture's time; M2's stored identity kept; M3's advance
  removed; P2's conflict keeping the identity; P6's guard removed; P7's sampling generation
  removed; P8's retired source taken and one serial for each EventSource; M5's revision 0 taken):
  each red at its case, and P6's alone.
- One more: a row drawn by the browser's playability whatever the output, red at
  `TestEveryRowDecidesByTheOutputChosen`, a structural case added here because
  `TestRowsFollowTheChosenOutput` runs `playFor` itself and nothing held the views to it.
- The server's twelve (L1 twice, L2's publication zeroed, L5's quit falling through, the records'
  cap, a published handover, a published left-out list, M1's wall clock in the control, no header,
  the handler's own clock, L3's admission stand-in, L4's broken premise): each red.

Notes on three of them. "DSD rows" is red at `TestRowsFollowTheChosenOutput` alone: the DSD-only
album's case drives `output.js` (the pick from an empty bar, the replace by the album's id), which
`playFor` does not reach, since the album page draws its Play whatever the rows say. The anchor's
control needed two replacements (the arrival recorded in `take`, read in `position`) and went red at
eight cases, the field guard among them, since it reads a field the state does not carry. L3's
first control, a poll of the records, stayed green (the poll's race); the admission seam held made
it deterministic and red.

### Deviations

- The rules line for the bar's output in `.claude/rules/admin-console.md` was not written: the
  session's harness refused edits to `.claude/`. Its long form is in
  `ops/History-rules-admin-console.md`, and the line is handed over as an OLD/NEW note, so the two
  land in separate commits.
- `.claude/rules/player.md`'s line for the console's control is unchanged (its room at the base,
  1,014 B, is under the 1 KB margin); the Lows' detail went to its long form in
  `ops/History-rules-player.md`, which is new long-form detail and no change of the line.

### Not run here

The browser session on a seeded bridge (§10's runs), which a later session holds: the output menu
at 375 px and on the desktop (no overflow, `scrollWidth == clientWidth`), the empty strip, focus and
the keyboard in the menu (Escape back to the button), the aria-live line, the names a screen reader
reads for the output button and the disabled controls, `.track-dl-cell` on a phone, a real
EventSource's reconnection, and a real clock's skew between the browser and the bridge.
