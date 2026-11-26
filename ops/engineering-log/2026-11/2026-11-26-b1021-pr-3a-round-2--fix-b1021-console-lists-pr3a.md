## 2026-11-26 — B1021 PR 3a round 2: the place, the crumb and the filter hold through every path

### What

Round 1's head, `d4283cd5`, read line by line against eight defects reported on it (line numbers
at that head). Each held:

1. **A jump and Back to the top stopped paging** (`views.js:1393`). `load()` takes the sentinel
   out (`stopPaging`) and, for a jump, keeps its container and skips `paint()`, the one place the
   sentinel went back into the document; the observer then watched a node out of it, and
   scrolling to the end of the jump's 60 rows asked for nothing.
2. **A late answer scrolled an operator page** (`views.js:1283`, `app.js:9914`). A boosted
   navigation pushes its own entry and replaces `<main>` with no route of the player's, so the
   generation did not move: a restore answered after it passed `stale()` and called
   `window.scrollTo()` on the operator page. A genre's or a composer's albums await their name
   (`views.js:1174`) before the list registers: answered after a boost, the name retitled the
   operator page and the grid's first page scrolled it, and a return there before the list
   registered would leave the router's own restore (`boot.js:763`) to scroll it.
3. **A failed restore left the list ready** (`views.js:1378`, `:1472`; `boot.js:761`).
   `ctx.listed()` after the failed load and the router's completion both marked it ready, and Try
   again cleared neither, so leaving while the retry waited for its second page wrote a place
   measured on its first page over the place it was restoring.
4. **The 1,000-row re-base ran on the Playlists** (`views.js:1470`, `:1607`). Their endpoint
   answers the whole collection from its first row whatever the offset, so a place 1,200 rows in
   set the window's start to 1,200 and took the DOM's row 0, the first playlist, for its row.
   Smart Mixes' adapter answers whole too (`views.js:1862`), and a place round 1 wrote over a
   re-based window kept its start of 1,200 at the next restore and the next write.
5. **A filter's URL reached the crumb's record only with a later place**
   (`views.js:1450`, `:1368`, `:1374-1379`). A filter that matches nothing, or that the server
   refuses (a `q` past 256 bytes), writes no place, and a matching one's write waits for its
   answer, so after Back, the filter and Forward, the crumb went back to an entry that no longer
   held its trail's URL.
6. **A reload emptied the crumb's record** (`boot.js:443`). It lived in the page's memory, while
   the trail, the depths, the keys and the list's entry outlive a reload, so the crumb pushed a
   fresh list and history grew.
7. **The old rail and the old Try again stayed live while a filter's rows were on their way**
   (`views.js:1335`, `:1378`, `:1453`). A letter started a load at the old q's offset with the new
   q, an old Try again one at the old place, either superseding the filter's own, and a positive
   count stood over an empty grid.
8. **The field's 16px lost the cascade** (`player.css:1258`). The coarse pointer's rule came
   before the field's shared style, whose `0.9rem` (14.4px at the default root size) won at the
   same specificity, under the size iOS zooms into a focused field; the source pin found the
   coarse rule and not which rule won.

### The changes

- **Paging** (`views.js`): once a load's rows stand, the sentinel is put back below them before
  it is armed, so a jump and Back to the top, which keep their container and paint nothing, page
  on.
- **Ownership** (`boot.js`, `views.js`): `route()` captures its entry's key and its view at its
  start and hands `ctx.owns` (the entry still the one shown, the view still in the document). A
  list's loads take it into `stale()`, so after each fetch a load that no longer owns its page
  neither pages on nor scrolls; a genre's or a composer's name is taken only while the page is
  owned, so it retitles nothing; and the router's own restore runs only for an owned page that is
  no list. A view called without the check (two tests that drive `renderMixes` by hand) owns its
  page.
- **Readiness** (`views.js`, `boot.js`): each load marks the list unready as it begins and ready
  once its rows stand (`ctx.listed(on)`); a load that fails or is superseded leaves it unready,
  and `route()` marks only a page that is no list ready. A list with no rows (empty, or a filter
  matching nothing) stays unready: it has nothing to measure.
- **Whole lists** (`views.js`): `renderPagedList` takes `whole`, set by Playlists and Smart
  Mixes. Their window starts at the first row whatever start the place names, with no re-base (so
  one request and no Back to the top), and the place's row is found by its id, else at the index
  it was measured at in its window (`anchor − place.start`), which recovers a place round 1 wrote
  over a re-based window.
- **The crumb's record** (`boot.js`): each entry's latest URL by its key, in session storage
  (`bridge-player-entries`, the latest 200 entries), written as the router routes an entry and as
  a filter commits (`ctx.moved`), right after its `replaceState` and before its load; `savePlace`
  no longer writes it. A crumb goes back only where the record holds its trail entry's key at its
  href; a record that cannot be read, holds no such entry or could not be written (a failed write
  drops it whole) leaves the crumb pushing. Session storage, because a descendant's trail is a copy
  taken as it was pushed and the ancestor's own state cannot be read before going back to it.
- **A filter's commit** (`views.js`) counts itself (`commits`), and a body's callbacks, its
  rail's letters and its Try again, act only while no commit has come since they were drawn
  (`whileDrawn`).
- **CSS** (`player.css`): the coarse pointer's rule follows the field's shared style.

Production lines: 129 added and 37 removed (CSS included): views.js +69 −21, boot.js +53 −12,
player.css +5 −3, handlers_player.go +2 −1 (a comment, one of the smaller defects below).

### Smaller defects

Done (the first two each with a check red at round 1's head and its control):

- A filter's commit gives the source banner's Show all sources the URL it now holds, which kept
  the q of the moment it was drawn.
- A load the server refused (400) offers no Try again, which would be refused again; the field is
  the way on.
- A filter matching nothing words its query in a heading that wraps anywhere
  (`.player-empty h2`), where a 256-byte unbroken query could run a phone's page sideways (no
  check: the page model has no layout, and WebKit did not run this round).
- The comment on Recently Added says the store has a first-seen column, `first_indexed_at`, that
  the catalog does not read.
- Round 1's entry: its opening worded neutrally, a build tool's name taken out, and pointers to
  the two controls its list lacked, both run here.

Listed for a follow-up:

- `renderAxisAlbums` looks a genre's or a composer's name up among the first 200 of its list, so
  one past them gets no heading or crumb label.
- A redraw while typing rebuilds the field from the URL, the keystrokes after the last pause lost
  (round 1's residual R4), while the rule's line says not to rebuild the field while typing.
- A source's root crumb whose URL is a trail entry's goes back to that entry (round 1's choice,
  recorded in its entry), where a structural crumb otherwise pushes.
- The words of a refused filter are the server's ("q is longer than 256 bytes"), and the field
  takes more than 256 bytes.

### Tests

The page model (`testdata/list_harness.mjs`) gains: a hold that takes a predicate on the query, a
failure of the next request for a path (500), the server's 400 for a `q` past 256 bytes, the
Smart Mixes endpoint, and a reload (the tab's history and session storage kept; the document, the
window's listeners, the timers, the frames, the observers and every module made anew, the modules
from a copy of their directory, since node evaluates a module once per URL). New scenarios, each a
Go test of its name:

- `TestALateAnswerTouchesNoOperatorPage` (app.js's own boost router): a restore held across a
  boost and let go, then a genre's name held across one: no scroll after the operator page's own,
  no retitle (item 2).
- `TestAListWritesNoPlaceUntilALoadsRowsStand`: a failed restore keeps the entry's place; its
  Try again held at page two and a row of page one opened keep it; a retry that stands restores
  it and a scroll records a new one; a restore superseded by a jump whose rows are held, left,
  keeps its place (item 3, the last part the router's own completion).
- `TestAWholeCollectionRestoresFromItsFirstRowByItsRowsID`: 1,300 playlists back at row 1,200
  from their first row, one request, no Back to the top; a place round 1 re-based at 1,200 lands
  on its row and the next place starts at 0; a playlist gone above the row found by id; one whose
  row is gone landing at its measured index; Smart Mixes alike (item 4).
- `TestACrumbPushesWhereItsEntryNowHoldsAnotherURL`: a filter matching nothing at scroll 0 with
  no write pending, and a matching one whose answer is held when Forward comes the moment the URL
  moved: both crumbs push (item 5).
- `TestACrumbReturnsToItsEntryAfterAReload`: after a reload the crumb goes back one entry, the
  list at its place, Forward reaching the album; an ancestor whose filter moved its URL before
  Forward and a reload: the crumb pushes (item 6).
- `TestNoCallbackOfAListAFilterReplacedActs`: an old letter and an old Try again (of a restore
  1,200 rows in that failed) pressed while the filter's rows are held ask for no old window, and
  the filtered list stands from its first row (item 7).

Extended: `TestBackReturnsTheAlbumListToItsPlace` and `TestARailJumpIsTheWindowBackRestores` page
past the 60 rows after Back to the top and after a jump (item 1); `TestTheFilterNarrowsTheListInPlace`
checks the banner's link and the refused filter (two of the smaller defects); `TestAListsHeadPinsBelowWhatIsPinnedAboveIt`
requires the last rule setting the field's `min-height` and `font-size` to be the coarse pointer's
(item 8, `cssRulesInOrder`, a walk of the stylesheet's rules in source order with their `@media`).

Over round 1's player modules every new and extended case is red: item 1's two paging checks
(60 rows, no `offset=60`), item 2's three (a `scrollTo` of 37, the title "Genre 03" for "Stats", a
`scrollTo` of 0), item 3's four (the place overwritten at anchor 0, and at 648 by the jump), item
4's six, item 5's (a `go(-1)` to `?q=zzzz`), item 6's (no `go`, a push and a longer history after
the reload), item 7's four (offsets 648 and 1200 asked, empty grids), the banner's and the refused
filter's two; item 8's pin
over round 1's stylesheet fails on the `.player-search input, .list-filter` rule.

**Negative controls**, each predicted before its run, on copies of the committed modules (never a
tracked file) with every list scenario run over each, and on copies of the stylesheet under a
prebuilt test binary: the sentinel not put back (the two paging cases red); `stale()` without
ownership (the late restore's scroll); the axis name without it (the retitle); the router's
completion without it (the scroll after the operator page's own); a load that ends without
marking the list ready (predicted two cases red, eleven were: with no readiness no place is ever
written, so every restore case fails); the router's completion marking a list ready (the
supersession part); a whole list started from its place's start, re-based, found by offset, or
falling back to the window's start (each red in the whole-collection case); the filter's URL not
recorded (three crumb cases); the record kept in memory (the reload case); the old rail live, the
old Try again live, the commit retiring nothing (the replaced-body case); the banner's link kept,
Try again offered on a 400 (the filter case); a crumb with no trail entry going back
(`TestACrumbOutsideThisTabsHistoryPushesAsBefore`, its dedicated control); in the stylesheet, round
1's, a later `0.9rem`, the coarse rule first, and the rows' `content-visibility: visible` taken
out (each red). One control stayed green as predicted: a load that begins without marking the
list unready, since every load that can be left part way begins from an unready list (a route's
first, a retry after a failure, one after a superseded one), and a jump from a standing list
writes the jump's start, the place a jump writes anyway. The first form of the later-`0.9rem`
control put the rule before the coarse one, and its green was right; moved after it, red.

**Counts**: the list harness, 11 scenarios before and 17 after, all green.

### WebKit

Not run this round: the host had no WebKit build to drive (no job folder with its scripts, its
runtime or its browsers), and getting one means a download this round may not make. Items 1, 2,
5, 6 and 8 rest on the page model and the source pin above; item 8 in a real engine is the
computed font size of the field under a coarse pointer, still to be measured.

### Runs

On `8d8a5699`, in one build-slot session (10:42 to 11:14): `make fmt` (the pinned toolchain's
gofmt; the tree unchanged after it), `make vet`, `make test` (93 packages ok; cmd/bridge 1,802.1 s,
internal/admin 1,469.0 s, internal/manifest 1,393.2 s), `make build-all` (the six targets) and
`make docs-check`, exit 0 after 1,919 s. Then `GOOS=linux GOARCH=arm make vet`, `GOOS=linux
GOARCH=386 make vet` and `GOOS=windows GOARCH=amd64 go vet -tags ts_omit_webclient,ts_omit_debugeventbus
./...`, clean; the console's guards and the touch pins by name (`TestNoConsoleControlDisablesItselfOutsideSetDisabled`,
`TestPlayerEmittedClassesAreStyled`, `TestPlayerCSSDoesNotHijackOperatorTableClasses`,
`TestEveryPushStateCarriesTheRouteTrail`, `TestPlayerBootCallIsLast`, `TestEveryPlayerModuleLoads`,
`TestPlayerHeadsMatchServerRoutes`, `TestPlayerRoutesTableCoversEveryPlayerHead`,
`TestCrumbRootsAreRealSections`, the eight of `player_touch_layout_test.go`, and the server's two
filter tests), 19, passed; every wrapper of the bar's harness (193) and the list's (17) under
`-race -count=10`: idle, 2,100 of 2,100 in 523 s; beside four busy processes on the host's 12
CPUs, 2,100 of 2,100 in 557 s; none skipped in either. The bar's harness is unchanged this round
(193 scenarios before and after).

### Left open

Round 1's residuals stand (R1 to R6), and the smaller defects listed above.
