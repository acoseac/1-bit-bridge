## 2026-11-26 — B1021 PR 3a: a list keeps the reader's place, and a filter narrows it

### What

The owner's reports, each measured at the base in WebKit before anything changed (the numbers
are in "WebKit, base and head" below):

- **A list started over.** Albums opens on Recently Added; past its first page (60 rows) a tile
  opens the album, and the album's "Albums" crumb pushed a new entry, `{ scrollY: 0, trail: [] }`,
  whose list rendered from offset 0 and scrolled to 0. Back re-rendered from offset 0 as well and
  applied the entry's `scrollY` once, after the first page's first 40 rows, so a place below them
  landed short and a place past the first page was never fetched; a rail jump's window was
  recorded nowhere. An output change and `player:rerender` re-routed the page the same way.
- **History was written past WebKit's rate.** The scroll listener wrote `scrollY` into the entry
  every 40px; WebKit refuses past 100 history writes in 10 seconds ("SecurityError: Attempt to use
  history.replaceState() more than 100 times per 10 seconds"), and while it refuses, the router's
  `pushState` throws after `preventDefault`, so a tap goes nowhere.
- **A jump landed under the top bar below 1024px**: `container.scrollIntoView` with no
  `scroll-margin` anywhere.
- **Nothing narrowed a list**: no list view and no list endpoint took a name filter.

### The changes

- **The server's filter** (`handlers_player.go`, `handlers_player_collections.go`,
  `internal/librarycat/sortkey.go`): `q` on GET `/api/player/albums`, `/artists`, `/genres`,
  `/composers` and `/playlists`, trimmed, at most 256 bytes (257 is 400 `bad_request`), folded by
  `librarycat.SortKey`, the export of the one fold `Catalog.Search` matches by, an empty fold
  filtering nothing. A row matches where its fold holds the query's: an album on its title or its
  album artist, an artist, a genre and a composer on their sort names, a playlist on its name
  folded the same way. It narrows each list after its source and axis filters and before its sort,
  buckets and paging, so `total`, `buckets` and every page are the filtered set's, and a genre
  list's rail is decided on the list before it (a filtered genre list that happens to read in
  letter order draws none). The filtered rows are kept in storage of the request's own
  (`namedOnly`): `narrowArtists` and `narrowAxis` hand back the cached catalog's own slices when no
  source narrows them, and a filter compacting those in place would write into every later
  request's catalog.
- **History** (`boot.js`): `pushRoute` stamps `depth` (the current entry's plus one) and a fresh
  `key` (random, unique across a tab's loads, since `history.state` outlives a reload); `route()`
  stamps an entry that has neither (a first load, a boosted navigation's `{ boost: true }`) at
  depth 0 by `replaceState`, keeping what it holds; `trailFor` records each page's depth and key,
  which `readTrail` keeps only as an integer and a string. A plain click on a link inside
  `#player-crumb` whose href is a trail entry of the current entry, with a depth below the current
  one, and whose entry still holds that href in this page's life (`entryURLs`, written at each
  route and each write of a place), calls `history.go(entryDepth − currentDepth)` and pushes
  nothing; popstate routes it. Every other crumb pushes as it did. The ancestor's URL is checked
  because an entry can change its URL in place after a descendant recorded it: Back to a filtered
  list, the filter edited (`replaceState`), Forward, then the crumb, whose recorded depth now leads
  to the edited list. After a reload the map is empty and every crumb pushes.
- **The place** (`boot.js`'s `savePlace`, `views.js`'s `renderPagedList`): a list records
  `{ href, start, anchor, anchorId, delta, snapshotAt }` in its own entry: the URL it was drawn
  for, its window's first offset, the offset and id of the first row whose bottom is below what is
  pinned over the list, that row's top less the pinned edge, and the catalog's `snapshotAt`. The
  pinned edge is the rows' own computed `scroll-margin-top`, so the CSS is its one source. One
  writer writes it (or a page's `scrollY` where the page is no list), only into the entry the page
  was routed for (its `key`), only while its view is in the document and once its list has
  rendered or restored: just before every navigation (each `pushRoute`; a crumb's `history.go`;
  any link's click, which is the one moment an operator link's navigation can be caught, since
  app.js's boost router pushes its own entry after a fetch, replaces `<main>` and fires no
  `pagehide`; and `route()`'s start, where an output change and `player:rerender` route the same
  entry), at `pagehide`, at a jump, and at most once per 500 ms while the page scrolls. A write
  WebKit refuses is caught and dropped, never thrown at the navigation after it. A list drawn on
  the URL its place was measured on restores it: the rows from `start` through the anchor and on
  until the anchor row can stand at its delta, or the list's end, in pages of 200 (the server's
  cap) appended at once rather than by `chunkAppend`'s frames, then one scroll. The row is found by
  its offset, or by its id among the restored rows where `snapshotAt` differs. A window restored
  past row 0 asks the list's first page (`limit=1`) for the rail and the total the server sends
  there alone, and offers Back to the top, which jumps to row 0 (Recently Added and Year have no
  rail, and the section's own link does nothing on its own URL). Past 1,000 rows of a window the
  restore re-bases at the page that holds the anchor, `anchor − ((anchor − start) mod 60)`. The
  sentinel is armed once the rows stand, so its first answer pages on from them. A list with no
  place on its URL (a push, a sort, quality, needs or filter change) scrolls to its top itself; it
  is its page's one scroll writer, and `route()`'s `scrollTo(state.scrollY)` runs only for pages
  that are no list. A failed restore shows its error with Try again, which restores again.
- **The filter's field** (`views.js`): Albums (every grid, the genre and composer album grids
  included), Artists, Genres, Composers and Playlists get a head after the source banner holding a
  search field labelled and placeheld "Filter albums" (the list's noun), its value the URL's `q`.
  Typing commits after the page-top search's 400 ms pause, Enter at once, Escape clears; the
  pause belongs to its list's route and entry, and once either has moved on it writes and fetches
  nothing. A change writes `q` into the list's own entry by `replaceState` (no new entry; an empty
  field removes it), drops its place, replaces the count line, the rail and the rows below the
  head, never the field, whose focus and caret stay, scrolls to the list's top only where the
  reader is past it, and announces the new count. A filter that matches nothing says "No albums
  match “q”" with a Clear button, and a list opened on one keeps its head. On a genre's or a
  composer's album grid the field writes `q` alone into the page's own URL, never the grid's
  path-derived axis. `route()` leaves the focus in a list's field as it leaves it in the page-top
  search's, and the redrawn list gives the focus to its new field. `api.playlists` takes `q`.
- **CSS** (`player.css`): the head sticks under what is pinned above it (`--list-head-top`: the
  shell's 65px top bar below 1024px, nothing from 1024px), z-index 4 (the ledger's popovers), at a
  fixed `--list-head-h` of 56px; a window 500px tall or less pins no head (`--list-head-pin` 0) and
  keeps PR 1's rail. `--az-top` adds `--list-head-pin` at every width, `--az-foot` unchanged. The
  rows (`.list-body > .grid`, `.list-body > .rows`) carry `scroll-margin-top` of everything pinned
  over them, and are `content-visibility: visible` (`.rows` is `auto`), so their heights are there
  as they go in. The field shares the page-top search's style and, under `(any-pointer: coarse)`,
  is 44px tall with 16px text.

Production lines: 425 added and 95 removed (CSS included), the added 287 of code, 122 of comment
and 16 blank (`git diff -U0` of the seven production files against the base): boot.js +111, views.js
+209, api.js +4, player.css +33, handlers_player.go +57, handlers_player_collections.go +7,
sortkey.go +4.

### Choices

The brief's open decisions, as taken: breadcrumb links alone go back (an album's artist byline
whose href is a trail ancestor still pushes); the filter on Albums (every grid), Artists, Genres,
Composers and Playlists, Smart Mixes not; an album matches on its title or its album artist; the
parameter is `q`; the pause 400 ms, one character filtering; up to 1,000 rows restored, past that
the window re-based at the anchor's page; `history.scrollRestoration` left `auto`, since WebKit
showed no restore of its own landing after the list's (below); the head pinned where the window is
taller than 500px; the coarse query `(any-pointer: coarse)`.

Made where the brief left a choice:

- **The head pins by default and a window 500px tall or less un-pins it** (`@media (max-height:
  500px)`), where the brief's pin named a `(min-height: 501px)` block: main's
  `TestTheBarTakesRowsForAFingerFrom641px` (B1021 PR 2) refuses any `(min-height: 501px)` in
  player.css, since two height blocks leave the heights between them (a fractional 500.5px) with
  neither. The default with one override covers every height, and the windows the head pins in are
  the brief's.
- An entry the router did not push is stamped depth 0.
- A jump keeps the rail and the count line and replaces the rows alone, so a key on a letter
  keeps its focus; the rail is rebuilt only for a fresh window (a load, a restore, a filter).
- `chunkAppend` is handed the route's generation and the list's together, so a jump or a filter
  change also stops the chunks a previous window had still to append.
- An error now keeps the banner and the head above it (it cleared the banner too).
- The source's root crumb, prepended to a trail, matches the trail's first entry where their URLs
  are one (`/albums?source=…`); it then goes back to that entry, whose URL is the one the link
  shows, so `crumbs()` is unchanged.

### Found while working

- `renderAxisAlbums` finds a genre's or a composer's name by asking for the first 200 of the
  list (`limit: 200`): in a library with more than 200 genres or composers, one past the first 200
  gets no heading or crumb label. Older than this PR; a follow-up.
- `handlers_player.go`'s comment on Recently Added says the store has no first-seen column; it has
  `first_indexed_at`, which the catalog does not read. Left, with the ordering it describes.
- The comment on `playerTimeLayout` said the client compares `snapshotAt` to notice a rebuild
  mid-scroll, which no module did; a restored place now compares it, and the comment says so.
- At the base, a jump on a phone landed at the foot of the loaded window, thousands of pixels
  below the jump's first row (WebKit, below): `scrollIntoView` ran while the emptied container was
  short. The head's jump scrolls once its rows are in.

### Tests

- **The server**: `TestTheListsTakeANameFilter` (each catalog list: "beatles" finds "The
  Beatles" and "eric" finds "Éric …"; total, rows and buckets the filtered set's; page 2 the
  filtered list's, `buckets` absent there by unmarshalling and checking the key; a fold that is
  empty, "  " and "!?", filters nothing; 257 bytes is 400 and 256 is 200; the playlists answer the
  whole filtered collection; after a selective filter the unfiltered artists, genres and composers
  are row for row the lists before it) and `TestAFilteredGenreListDrawsNoRail`. Both red at the
  base, its two handlers and `sortkey.go` overlaid (`go test -overlay`) with a names-only
  `SortKey` shim: every list answered unfiltered.
- **The page model**: `testdata/list_harness.mjs`, run like the bar's harness, one scenario a Go
  test with the same JSON report and `barNode`, through `harnessScenario`, which `barScenario` now
  calls with the bar's harness's name. It models a session history (entries of a state and a URL;
  a push drops the entries ahead; `go` and `back` dispatch popstate after a turn; `length`), the
  window's scroll clamped to the document's height with its scroll events, each row's box from its
  index (a column count and a row height a case sets, the list's top, less the scroll), the pinned
  edge the rows' `scroll-margin-top` a case sets, an `IntersectionObserver` answering when the
  sentinel is within its margin, `requestAnimationFrame` run by `settle()`, the shell's elements by
  id, every timer of 50ms or more held until a case advances the clock, focus moved to the body
  when its element leaves the document, and a fetch answering the list endpoints by `offset`,
  `limit`, `sort` and `q` from a generated library of 1,400 albums, 400 artists, 80 genres, 80
  composers and 6 playlists. It imports and boots the shipped boot.js (and so audio.js, output.js,
  nowplaying.js) and drives views.js through it. To import and boot boot.js it stubs the audio
  element's media methods, `navigator.mediaSession`, the bridge player's GET now (no player
  offered, unless a case holds it), and the album, artist and playlist pages' detail answers (no
  tracks). The operator case runs app.js's own boost router (`wireBoostRouter`, `boostNavigate`,
  `boostSwap`, assembled from app.js by `withReachedFunctions`, its page init and stream recycle
  stubbed). Eleven scenarios, each a Go test of its name:
  `TestBackReturnsTheAlbumListToItsPlace` (row 150 of Recently Added at its delta after Back, the
  window from row 0, rows 0 to 150 in pages of 200, one scroll; a place at row 199 of one column
  with rows behind it, standing once the observer has paged on; a place 1,200 rows in, re-based,
  and Back to the top reaching row 0), `TestBackReturnsTheArtistAndAxisListsToTheirPlace`,
  `TestTheCrumbReturnsToTheEntryTheReaderCameFrom` (`go(-1)`, the length unchanged, Forward
  reaching the album; `go(-2)` from the byline's artist; and Back, the filter edited, Forward and
  the crumb pushing), `TestACrumbOutsideThisTabsHistoryPushesAsBefore`,
  `TestARailJumpIsTheWindowBackRestores` (with the rail kept and A reached after the restore),
  `TestAPlaceIsRestoredOnlyOnTheURLItWasMeasuredOn`, `TestAPlaceIsWrittenOnlyIntoItsOwnEntry`,
  `TestARedrawOfTheSameEntryKeepsTheListsPlace` (the output's real change, GET now answering that
  no player is offered after the stored bridge pick, right after a scroll whose write is pending),
  `TestBackFromAnOperatorPageReturnsTheListToItsPlace` (the same, the operator link through the
  real boost router, then Back), `TestTheFilterNarrowsTheListInPlace` and
  `TestAFilterPauseWritesNothingAfterANavigation`. Over the base's player modules ten are red; the
  eleventh, `TestACrumbOutsideThisTabsHistoryPushesAsBefore`, pins the pushes that stay and is
  green there by design.
- **Source pins** (`player_touch_layout_test.go`): `TestAListsHeadPinsBelowWhatIsPinnedAboveIt`
  (the head sticky at its fixed height outside any media block, un-pinned in `(max-height:
  500px)`, `--list-head-top` 65px below 1024px, `--az-top` taking the head, the rows' scroll
  margin, the field's 44px and 16px under the coarse query). The one assertion changed:
  `TestWhatSitsAboveTheBarClearsItAsItRenders`'s `(max-width: 1023px)` `--az-top`, now `calc(65px +
  var(--list-head-pin) + var(--space-2))`: below 1024px the rail sticks below the top bar and the
  head.
- **Negative controls**, 27, each predicted in writing before its run and run on copies (the
  player modules copied and mutated for the harness; a Go file overlaid; player.css mutated in a
  copy of the package's static tree under a prebuilt test binary), so no tracked file was edited:
  restoring by `scrollY` alone (eight scenarios red), no restore for rows, the crumb pushing, the
  jump's start not recorded, no `href` check, no `key` check (two red: the genre album grid is a
  list too, and wrote into the genres' entry), the filter re-rendering the view (two red: its own,
  and the filter change of the URL test, which then restored the old rows on the new URL), the
  pause outliving its route, no save at a redraw, no save at an operator link, no first-page rail
  past row 0, no Back to the top, no rows below the anchor, the crumb trusting a moved entry, no
  re-base, no words for a filter that matches nothing, a redraw taking the focus; on the server,
  filtering the page after paging, the genre rail decided on the filtered list, compacting the
  catalog's slices in place, no length bound, no fold; in the CSS, the head pinned in a short
  window, the rail's top without the head, no scroll margin, 15px text, and the phone's rail top
  without the head. All 27 went red as predicted; the predictions for the `key` and the
  re-rendering controls were narrower at their first run and were corrected with the reason above
  before the final run. The row-199 case first read the reader's own scroll clamped at the loaded
  rows' end, so its place was not row 199: the model's reader now scrolls again once the observer
  has paged on, and the case asserts its premise.
- **Counts**: the bar's harness, 193 scenarios before and after, all green; the list harness, none
  before, 11 after, all green.

### WebKit, base and head

WebKit 27.2, headless, on two test bridges on loopback over one generated library of invented
names (440 albums of short FLAC tracks, 330 album artists, 75 genres spread unevenly, so the genre
list is ordered by track count and has no rail, 75 composers, titles under every letter, 20 under
M, "The Brass Lanterns" the only fold holding "BRA"), with 4 playlists written through the bridge's
own `PUT /v1/playlists/{id}` with a paired token. One script measured both builds: the base
(`eb3a1633`) and the head, first `28edfb38` and then `251a813f`, which gives a list's rows their
heights. Contexts: the iPhone SE (3rd gen) 375×667, the iPhone 14 390×664 and the iPad Pro 11
834×1194 by touch, and a 1280×800 laptop by mouse; 320×568 for (h). The pinned edge is the bottom
of what is pinned at the window's top (the shell's top bar below 1024px and, at the head, the
list's head below it); a row's delta is its top less that edge; #n is its index among the loaded
rows.

- **(a) Albums, Recently Added, row 150, a tile, Back.** Base: the top row came back as #36, #36,
  #30 and #33 (SE, iPhone 14, iPad, laptop), after one request (`offset=0&limit=60`) and `route()`'s
  `scrollTo(state.scrollY)` 4–5 ms after the Back against a document of the first chunk's 40 tiles
  (5,644px at the SE for a target of 19,681), the next page never asked. Head: #150 at its delta in
  all four (−58, −59.5, −63.7 and −76.3px before and after), one request (`offset=0&limit=200`) and
  one `scrollTo`, 46–47 ms after the Back, to the scrollY it left (19,695 at the SE).
- **The browser's own restore**: none in either build. scrollY, sampled every 50 ms for 2.5 s from
  the Back, moved once, at the page's own scroll, and never after; at the popstate it was 0
  (WebKit's restore ran on the short album page). `history.scrollRestoration` stays `auto`.
- **(b) The crumb.** Base: `history.length` 3 → 4, the list at #0, and Forward no longer reached
  the album. Head: 3 → 3, #150 at its delta, and Forward reaching the album, in all four.
- **(c) A jump to M, about 800px more, a tile, Back.** Base: M's window lost in all four, the list
  from "Alder Islands" (#0). Head: the window from M's first ("Magnolia Engines", list #204) and the
  top row at its delta in all four, through two requests (`offset=0&limit=1` for the rail and the
  total, then `offset=204&limit=200`); the rail there, every letter live, and A from it drawing the
  list from row 0.
- **(d) Artists, row 150; Genres, row 40.** Artists: base #38, #34, #27 and #24 (row 148 at the
  laptop); head the same row at its delta in all four. Genres: base #0 in all four, and at
  `28edfb38` too: the restore's `scrollTo` had the right target (2,918px at the SE) 10 ms after the
  Back, but the rows' container was `content-visibility: auto`, whose rows WebKit gave no heights
  yet, so the document was the window's height (667px) and the scroll clamped to 0 (the base failed
  the same way at 5 ms). At `251a813f`, the rows `content-visibility: visible`, row 40 at its delta in
  all four (−15.4, −15.4, −10.8 and −10.8px), one request (`offset=0&limit=200`) and one scroll, the
  document 5,417px tall at the SE as it landed; composers likewise at the iPhone 14 and the laptop.
  Every other number of `251a813f`'s run repeats `28edfb38`'s but those a run's timing moves (the
  steps a scroll took, a fling's last row, the frame-driven fling's 49 writes).
- **(e) The first row after a jump to M below 1024px**, against the pinned edge. Base: at the SE
  from 3,000px down the first M tile's top was at 0, its upper 65px under the top bar; from near
  the top at the SE, and from either start at the iPhone 14, the page ended at the foot of the two
  loaded pages, the first M tile 14,000 to 46,000px above it (the jump scrolled the emptied
  container into view, then appended); at the iPad the emptied page could not scroll (288px, below
  the bar). Head: the first M tile's top at the head's bottom (121px) on both phones from both
  starts, and 0.3px above it at the iPad (the scroll offset's rounding).
- **(f) The filter** (the head alone), at 390 and 1280: "bra", three keys 100 ms apart, made one
  request with `q=bra`, 608 ms after the first key; the field kept the focus, its value and its
  `selectionStart` 3; the URL became `/albums?q=bra` with `history.length` unchanged (2 → 2); the
  field's top stayed put once its head stuck (70.8px at 390 from scrollY 500, 5.8px at 1280 from
  250, to 2,000); the rail's top 8px below the head's bottom; "zzzz" and Enter: "No albums match
  “zzzz”" and Clear.
- **(g) A 30 s fling through Albums.** WebKit refuses history writes past 100 in 10 s ("Attempt to
  use history.replaceState() more than 100 times per 10 seconds"). Base: by the laptop's wheel, 379
  writes, 77 refused (7.7–10.1 s, 18.0–20.2 s and 28.4–30.2 s), and a tap during a refusal dead, its
  `pushState` refused after `preventDefault`; a simulated 60 Hz phone fling (a 16 ms timer scrolling
  60px and dispatching the scroll event itself, since headless WebKit scrolls the iPhone 14 at about
  8 frames a second): 1,669 writes, 1,369 refused, a tap during a refusal dead; the frame-driven
  fling at that rate, 175 writes, none refused. Head: 55, 58 and 49 writes, none refused, no page
  error, and every tap navigating, during the fling at the base's refusing moments (9.7 s and 2.0 s)
  and right after it. The hazard is real, and the coalesced writer clears it.
- **(h) No sideways scroll**: `scrollWidth` equal to `clientWidth` on Albums, Artists and Genres at
  320, 390 and 834px, at the base and with the head.

Where the evidence answers from fixtures: the page model's library is generated for each case and
its geometry is the case's; in WebKit every number is a real bridge's over real files, and the one
simulated input is the 60 Hz fling.

### Runs

On `251a813f`, in one build-slot session (07:27 to 08:04): `make fmt` (the pinned toolchain's
gofmt; the tree unchanged after it), `make vet`, `GOOS=linux GOARCH=arm make vet`, `GOOS=linux
GOARCH=386 make vet` and `GOOS=windows GOARCH=amd64 go vet -tags ts_omit_webclient,ts_omit_debugeventbus
./...`, all clean; `make docs-check`, ok; the console's guards by name
(`TestNoConsoleControlDisablesItselfOutsideSetDisabled`, `TestPlayerEmittedClassesAreStyled`,
`TestPlayerCSSDoesNotHijackOperatorTableClasses`, `TestEveryPushStateCarriesTheRouteTrail`,
`TestPlayerBootCallIsLast`, `TestEveryPlayerModuleLoads`, `TestPlayerHeadsMatchServerRoutes`,
`TestPlayerRoutesTableCoversEveryPlayerHead`, `TestCrumbRootsAreRealSections` and the
`player_touch_layout_test.go` pins), 17 passed; `make test` with `TEST_PKGS='-count=1 ./...'`, 93
packages ok in 2,167 s (cmd/bridge 2,022.8 s, internal/manifest 1,628.5 s, internal/admin 1,579.8 s);
`make build-all`, ok. A first session on `28edfb38` was stopped in its `make test` once WebKit had
shown the genre list's defect; its fmt, its four vets, docs-check and the guards had passed. Before
it, on the tree committed as `28edfb38`: internal/admin whole without the race detector, 371.6 s,
ok; the new Go tests and the eleven list scenarios' tests under `-race -count=10`, 150 passes; the
bar's harness, 193 of 193, and the list's, 11 of 11, over the head's modules. This entry was
committed after the session, with `make docs-check` and cmd/bridge's doc guards run again over it.

### Left open

- A place over a rebuilt catalog whose anchor moved past the restored rows lands by its offset.
- A place more than 1,000 rows into its window restores with the window re-based at the anchor's
  page: the rows above it come back by the rail or Back to the top, not by scrolling up.
- Smart Mixes take no filter: a dozen generated families on the page that switches the feature.
- A redraw while typing (an output change) rebuilds the field from the URL: keystrokes after the
  last pause are lost.
- iOS Safari's zoom into a focused field under 16px is pinned at the source alone: WebKit here
  does not reproduce it (it is the iOS UI process's).
- Recently Added orders by the last re-index of an album's oldest track: unchanged.
- A write the coalesced writer still has pending when the reader presses Back is dropped (it would
  land in the entry Back reaches), so up to 500 ms of scrolling before a Back is not in the place
  Forward restores.
