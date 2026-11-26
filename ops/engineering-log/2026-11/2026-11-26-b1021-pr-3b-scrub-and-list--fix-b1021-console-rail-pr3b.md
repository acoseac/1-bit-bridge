## 2026-11-26 — B1021 PR 3b: the lists' final head taken, and a scrub held to the list it drives

### What

The branch took PR 3a's final head, and the rail's scrub was checked line by line at that
merged head against what its list does under a held finger. Each of these held there:

- **A held finger read a rail its own jump had moved.** A jump empties the list at once
  (`load`'s `clear(container)` before its fetch); the page shortened, and in a window 500px tall
  or less, the rail being PR 1's seven columns, the sticky rail stood 87px lower until the rows
  came. Every move after the jump read the rail where it then stood (`under` clamps the finger into
  the rail's live box, then `elementFromPoint`), so a finger held on M read F, the letter above
  it, and jumped there.
- **A scrub wrote a place for each live letter it crossed.** Each jump's load ended in
  `ctx.savePlace`, a `replaceState`: a drag from # to Z, each letter's rows in before the next
  move, made 26 writes in the list's page model, where WebKit allows 100 in 10 seconds and refuses
  a navigation's own push past them.
- **A frame queued before a boost appended to the list the page had left.** `chunkAppend`'s guard
  is the generation its caller gives it, and the list's (`listGen`) moved with neither a boost to
  an operator page nor anything else that leaves the route's page in place: the list's rest went
  into the detached list (40 rows became 60).
- **The old rail stayed scrubbable while a filter's rows were on their way.** Its callbacks were
  the filter's (`whileDrawn`), but the scrub took the finger and moved the callout before it asked
  them: a finger moved after the commit showed Z, and a new finger was captured and shown C.
- **A primary pointer took a scrub over** (`(owner && !e.isPrimary)`): a pen going down on the
  rail a touch scrubbed took it, the touch's capture and its end lost.
- **The click a scrub's lift owed it outlived the lift.** `owed`, set at the owner's `pointerup`,
  was cleared at the next pointerdown alone: after a lift that made no click (on the grid's empty
  cell, or past the rail) beside a pointer that had gone down elsewhere, the next click with a
  detail above 0 and no pointerdown before it (an assistive technology's), a second or a minute
  later, was swallowed; a key's own click (detail 0) was never.
- **A decade's band read its year index unclamped at its top** (`buckets[-1]`, undefined, fell
  back to the first year by `|| b`).

Three held and needed nothing: a window restored past its first row draws the rail its first
page sends, kind and buckets, for the same filter and sort (`load`'s `head`, `fetchPage(0, 1, q)`,
handed to `paint(head || r)`); no module imports `alphabetRail` (its name remains in this PR's
first entry alone); and PR 1's two rail cases differ from main's by the function's name alone.
The callout's count (the audit's item 15) stays deferred, as this PR's first entry and the
long form record: the callout says an entry's words. The server's rail test named the
composers' and the genres' lists in its comment and asked only the artists'.

### The merge

PR 3a's final head (`54859e3c`) merged into this branch (`88c2a36e`) as one merge commit
(`2b79efa6`), made from the two tips' trees with no `git merge`: each file both sides changed
since `c2306c2a` (this branch's earlier merge of PR 3a, which both tips hold the content of) went
through a three-way merge over that base, six with one conflict each, resolved by rule:

- `handlers_player.go`: both sides corrected the same comment on Recently Added's sort; this
  branch's (the store keeps `first_indexed_at`, the catalog does not read it, and why the sort
  does not) says what PR 3a's does and more, and stands.
- `views.js`: PR 3a wrapped the rail's callback in `whileDrawn`, this branch renamed the rail and
  handed it the kind: `indexRail(r.buckets, whileDrawn(…), r.rail)`.
- `player_output_list_test.go` and `list_harness.mjs`: each side appended its cases at one place;
  both kept, PR 3a's first.
- The rules file and its History: PR 3a's new line for the list entry, then this branch's rail
  entry, the base's list line unchanged on this branch.

Proven by content: of PR 3a's 103 hunks since that base, 100 are in the merge's diff from this
branch verbatim, one more with its leading blank line aligned at the other end, and the other
two are the two resolved lines above; of this branch's 59, 57 are in the diff from PR 3a's head
verbatim and two are the same two lines. The edition picker came with main through both tips,
alike. The merged rules file was 100,319 bytes: `move_entry.py` moved one entry neither side
touched (a managed control's, B68) verbatim to History, leaving its pointer (98,020 bytes), and
`verify_moves.py --base` the merge passed (one block moved).

### The changes

- **A jump keeps the list's height until its rows land** (`views.js`'s `load`: its `min-height`
  set to the height it had before `clear`, let go at the first append). The rail no longer
  moves under a held finger, which reads the letter it stays on.
- **A scrub's place is written once, at its lift.** The rail tells the list that a finger holds
  it (`onJump(b, true)`), and its jumps' loads write no place as they land; the scrub's end, a
  lift, a cancel or the rail's lost capture of a scrub that jumped, calls the list's `lift`,
  which writes the place if the last jump's rows stand (`savePlace` asks the list's `ready`),
  else leaves it to their landing, which then writes it. A key's jump and a mouse's write theirs
  as they land, as before. The lift writes only while no filter has committed since the rail was
  drawn and the route still owns the page (`whileDrawn`, `gone`). PR 3a's scroll writer, at most
  once in 500ms, still writes where the jumps scroll the page (once in a drag from # to Z,
  below): bounded by its own throttle, and the place it writes is the list's own.
- **The rail asks the list whether it is live** (`indexRail`'s `live`: no filter committed since
  it was drawn, the route still its page's). While it is not, a pointerdown takes no scrub, so
  nothing is captured or shown, and a move ends the scrub under way, its callout hidden.
- **One owner per scrub.** The first touch or pen to go down on the rail while no scrub holds it
  owns the scrub, a touch that is not primary included (where another finger rests on the glass,
  every later one is not primary); while it does, no other pointer, primary or not, touch or pen,
  starts or takes it, and only its own lift, cancel or the rail's lost capture ends it. The
  owner's own pointer going down again, its end never delivered, starts it anew: PR 1's tap case
  sends taps of one pointer with no lift, as its model of WebKit did.
- **The owed click expires** (`owedUntil`): at the next pointerdown or keydown, or a second after
  the lift. WebKit's tap makes its click within 2ms after its `pointerup` (below), well inside it.
- **`chunkAppend`'s generation is no generation once the page is not its route's** (`ownedGen`:
  `NaN`, which nothing matches), for the list's rows and for its two other callers, an artist's
  albums and the favorites, so a frame queued before a boost appends nothing. `ui.js` imports no
  router or view state: the ownership is folded into the generation each caller hands it.
- **A decade's band holds its year index to its ends** (`Math.max(0, …)`; the fallback stays for
  a band with no height).

### Choices

- **The place at the lift, not a throttle of its own**: one write a scrub from its jumps, and
  none for a scrub a filter retired or a route left; the scroll writer's throttle bounds the rest.
- **The rail's liveness as a question it asks at each pointer event**, not a signal the list
  fires: the old rail asks the same counter the body's other callbacks do (`commits`); its callout
  goes at the finger's next move or lift, and the body that replaces it when the rows come takes
  the rail away.
- **A touch that is not primary may start a scrub on an idle rail**: refusing it would leave the
  rail dead under a finger while another rests elsewhere on the glass.
- **The bound on the owed click is a second**, the measured click (below) coming within
  2ms of its lift.

### Tests

Red first, each new case over the merged head's modules (`eeb7aea6`), green at the head:

- **The list's page model** (`list_harness.mjs`, whose list now takes its `min-height` into its
  height and lays out an element a case gives a box, and whose elements record the captures they
  take): `TestAHeldFingerReadsTheRailWhereItsJumpFoundIt` (a 320px window, the rail's seven
  columns 87px lower while the list is shorter than the window, a finger down on M with M's rows
  held, moved where it went down: at the merged head it asked for F, offsets 648 then 270, and
  the callout said F; at the head M alone, the callout M, the rail where it stood; once M's rows
  land the list's height is its own), `TestAScrubWritesItsPlaceOnceAtItsLift` (a drag across all
  26 live letters, each landing before the next move: 26 writes at the merged head, none at the
  head until the lift's one, Z's offset 1,347; Z's rows held past the lift: at the merged head
  M's write at once and Z's on landing, at the head nothing until Z's rows land and then Z's
  alone; a key's jump and a mouse's each write as their rows land; a lift once the route has
  moved on writes nothing), `TestTheOldRailTakesNoScrubWhileAFiltersRowsAreHeld` (a finger on M
  while "Album 1" commits, its rows held: the finger's move asks nothing at both, and the callout,
  Z at the merged head, is gone at the head; a new finger on the old rail: captured and shown at
  the merged head, not taken at the head; the filtered list then stands under a rail of its own)
  and, through app.js's own boost router, `TestAFrameQueuedBeforeABoostAppendsNothing` (M's
  first 40 rows in and its frame held, Stats boosted in, the frame let go: 60 rows at the merged
  head, 40 at the head).
- **The rail alone** (`bar_harness.mjs`, whose elements record their captures):
  `TestNoPointerTakesAScrubFromItsOwner` (a touch that is not primary owns the idle rail; a
  primary pen and a primary touch down on it then take nothing and their lift, cancel and lost
  capture end nothing; the owner's move jumps and its lift ends the scrub; the pen's next down
  then takes it: at the merged head the pen took it over, captures 5, 9 and 6) and
  `TestAScrubsLiftOwesItsClickASecondAtMost` (after a scrub a mouse went down beside, lifted
  where no click is made: a click 900ms on is the tap's, one past the second jumps, and so does
  one after a keydown, both swallowed at the merged head; a tap on the short grid's empty cell or
  on an empty letter jumps nothing, nor the click each is handed; a key's click after them
  jumps). `TestTheYearRailFollowsThePhonesBands` gains the band's ends (a finger above the rail,
  the first decade's first year; below it, the last decade's last): green at the merged head
  too, by the fallback.
- **The server**: `TestTheAlbumRailFollowsEverySort` now asks the composers' list (`letters`, five
  buckets: the composers file by surname) and a genre list ordered by its counts (neither key),
  over the name filter's seeded library.
- **Negative controls**, 22, predicted in writing before the run, all as predicted, none touching
  the tree (the player modules copied and mutated; Go by `go test -overlay`): no `min-height` (F
  asked); the height never let go; a scrub's landing writing its place (26 writes); the lift
  writing nothing; `end` calling no lift; the old rail always live; `live` unasked at a move, and
  at a pointerdown; the list's generation without the ownership, and `ownedGen` returning the
  generation; the admission this PR's first entry describes put back (the takeover case red,
  PR 1's tap case green); any
  owner refusing its own pointer (PR 1's tap case red, which is why the owner's own pointer
  starts it anew); the owed click with no expiry; no keydown listener; a bound of 800ms (the
  900ms click red); the decade index unclamped with no fallback (red), and with the fallback
  alone (green, as at the merged head); and on the server the composers' kind dropped and a
  count-ordered genre list given letters. The lift's own route guard dropped stays green: the
  place writer writes nothing for a page whose entry is not the list's, a second layer.
- **Harness counts**: the bar's 219 scenarios at the merged head, 221 at the head (979 checks,
  every one clean); the list's 25 and 29 (188 checks, every one clean).

### WebKit and Chromium, the merged head and the head

WebKit 27.2 and Chromium 159, on two test bridges on loopback over this PR's generated library
(468 albums of invented names; the title buckets # at 0, A at 4, F at 104, M at 244, Z at 448,
none under Q, U or X), each on a copy of the data directory this PR's first entry prepared, its
index times holding at every start: the merged head (`eeb7aea6`) and the head (`c9a4023d`),
which differ in `ui.js`, `views.js` and the tests alone. Input: WebKit by taps; Chromium's drags
as touch events sent through its input protocol. Every touch context is simulated (an emulated
device or a touch window); a held request is held by the browser's request routing until let
go; step 3's frames are held by a test hook on `requestAnimationFrame`; the filter is set in its
field by script.

- **A held finger** (Chromium 568×320, M's request held): at the merged head the rail went from
  73 to 160 within 300ms, the first move where the finger went down read F, asked for it and
  showed F; at the head the rail stayed at 73, the callout said M at every move, and M alone was
  asked. A WebKit tap on M with M held: the rail 73 to 160 at the merged head (its pointerup over
  T, no click), 73 to 73 at the head (its pointerup and its click on M).
- **The place writes** (Chromium 568×320 and Pixel 7): a drag from # to Z waiting at each live
  letter for its rows wrote 24 places during it at the merged head, one a letter; at the head the
  jumps wrote none, the lift one, Z's offset (448), and the scroll writer one during the drag,
  armed by the first jump's scroll. A fast drag (moves 16ms apart): 22 and 21 writes during it at
  the merged head; none at the head, then the lift's one. Z's request held past the lift: the
  head wrote nothing until Z's rows landed, then Z's place; the merged head wrote M's as it
  landed, then Z's. A tap, a mouse's click and WebKit's taps: one write as their rows land at
  both builds, and the scroll writer's after a jump that scrolled, alike.
- **The old rail while a filter's rows are held** (`q=lantern` held, Chromium 568×320 and Pixel
  7, and WebKit): no jump asked at either build. At the merged head the held finger's move to Z
  showed Z, and a new touch on C, moved to N, showed C and N and was captured by the rail; at
  the head the move hid the callout and the new touch showed none and was not captured. A WebKit
  tap on C showed the callout at the merged head and never at the head. Once the answer came,
  both: the filtered list (A, M and Z live) under a rail of its own.
- **A frame held before a boost to Stats** (WebKit iPhone 14, Chromium Pixel 7): the left list
  grew from 40 rows to 60 at the merged head and stayed at 40 at the head.
- **A tap's click** (the head, 20 taps a context on live letters): 1, 1 and 2ms after its
  pointerup (least, median, most) in WebKit's iPhone SE, iPhone 14 and iPad, 0ms in Chromium's
  Pixel 7; one click and one request a tap, 80 of 80. On empty letters WebKit's phones handed the
  click to a live neighbour within 2ms, its iPad and Chromium made none, and none asked anything.
  The owed click's bound is a second, about 500 times the most.
- **This PR's first entry's taps at the head** (WebKit's iPhone SE, iPhone 14, iPad and 568×320,
  and the laptop by mouse): one request a live letter, none from the 24 empty letters of
  `q=lantern`, no callout, the laptop's click one request; at 568×320 every press made a click
  (24 of 24), where the rail moving under the finger had left 8 of 24 at that entry's heads.
- **PR 3a's late-answer cases at the head**, by its own two evidence scripts (WebKit, a laptop
  and an iPhone 14): 32 and 100 checks, each reading as at PR 3a's own heads.

### Runs

- One build-slot session on `c9a4023d` (15:09–16:04 UTC): `make fmt` (the tree unchanged after
  it) and the pinned toolchain's gofmt over the nine Go files changed since main (nothing
  listed); `make vet`, `GOOS=linux GOARCH=arm make vet`, `GOOS=linux GOARCH=386 make vet` and
  `GOOS=windows GOARCH=amd64 go vet -tags ts_omit_webclient,ts_omit_debugeventbus ./...`; the
  console's guards and pins by name; cmd/bridge's source guards (its three doc guards and the
  goroutine-drain guard); the 251 tests of the list, rail and bar harnesses' files and the
  server's three rail tests `-race -count=10` (579 s), and again beside four CPU hogs (576 s),
  every run green; `make test P=8`: 92 of 93 packages ok (cmd/bridge 1,996 s of its 45 minutes,
  internal/manifest 1,659 s, internal/backup 715 s), internal/admin failing one case,
  `TestAppJSHasNoCallsToDeletedHelpers`: ui.js read as calling `onJump` undeclared, the guard
  reading a function's parameters only where its list holds no parentheses, and `indexRail`'s
  `live = () => true` held one. The next commit names the default (`ALWAYS_LIVE`), and the guard
  passes; `make build-all` (29 s); `make docs-check`.
- Before it: the new cases red over the merged head's modules and green at the head (above),
  every harness scenario at the head (221 and 29, clean), the 22 controls.
- `python3 scripts/rules-for.py --changed` before each commit: `admin-console.md` and
  `build-ci-tests.md`, both read whole.
- The gate runs again on this record's commit; a record cannot hold the runs of its own commit,
  so their numbers are the report's.

### Left open

- The callout's count under a finger (the audit's item 15), as this PR's first entry records.
- What that entry leaves open stands: Recently Added by `AddedAt`; the reader's offset at the
  request; the touch laptop pinned at the source; no 44px column at 500px or less; the client's
  clock beside the server's around a month's turn.
- A scrub a filter retired keeps its callout until the finger's next move or lift; the body that
  replaces the list when its rows come takes it away with the rail.
