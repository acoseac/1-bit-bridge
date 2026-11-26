## 2026-11-26 — B1021 PR 3a round 3: what a list its route left, a late detail answer and a replaced body's last two buttons may do

### What

Three ownership paths still open at `7a9d1506`, read line by line there before anything changed.
Each held:

1. **A list its route had left could still ask, and cancel the next page's lookup.** A genre's or a
   composer's page keeps the list it came from in the page while it looks its name up
   (`renderAxisAlbums`, `views.js:1140-1198`, clears nothing before `renderAlbums`). That list's
   `page()` checked only `loading` before its request (`views.js:1433-1438`) and `load()` stopped
   its paging, marked the list unready, reset its window and asked the first page for the rail
   (`fetchPage(0, 1, q)`, `views.js:1378-1390`) before its first `stale()` (`:1391`). The genres'
   and the composers' pages and the name's lookup share one request key (`api.js:113-114`), under
   which a request cancels the one before it (`api.js:43-46`), and the lookup returns on that
   cancellation before the grid (`views.js:1186`, `:1198`). So the old list's sentinel, scrolled to
   while a genre's name was looked up, or its Try again after a restore that failed, pressed while
   a composer's name was, left the axis page unopened under its own URL.
2. **A detail page's late answer retitled an operator page.** A boosted navigation to an operator
   page (`app.js:9871-9939`) replaces `<main>` and pushes its own entry with no route of the
   player's, and its `leave` event reaches no player module, so a detail answer in flight lands
   after it. The album checked the generation alone before its retitle (`views.js:501`, `:507`), the
   artist too (`:1042`, `:1044`), and a playlist's or a smart mix's page (`:2141-2152`) and a key's
   tracks (`:2677-2689`) nothing at all: each wrote its name into `document.title`
   (`setAxisTitle`, `:1210-1215`) over the operator page's. Folders title themselves before their
   request (`:2420`), and the router's own completion scroll already asks ownership
   (`boot.js:802-806`); no detail page scrolls by itself.
3. **Back to the top and the no-match Clear outlived the body a filter replaced.** The button was
   built once with an unguarded click (`views.js:1272-1273`) and put back by every paint
   (`:1363`), and Clear's click emptied the field and committed again (`:1503-1508`), while the
   rail and the two Try agains were already guarded (`:1360`, `:1408`, `:1450`). The filter's
   commit leaves the old body standing until its answer paints (`:1475-1501`). Pressed while a
   filter's rows were on their way, the old Back to the top asked the first page again (the
   commit's load had already dropped the container, `:1386`, so its own load painted the new q's
   rows), which cancelled the filter's own request, left its count unannounced and scrolled as a
   jump does; the old Clear emptied the field, took `q` out of the URL and asked for the whole list.

### The changes

- **A list its route left asks nothing** (`views.js`): `gone()` (the generation moved, or the page
  is no longer the route's, `ctx.owns`) is asked first in `load()` and `page()`, before either
  touches the list or makes a request, and `stale()` is `gone()` beside its own generation.
- **A detail answer retitles only its route's page** (`views.js`): the album and the artist ask
  ownership beside their generation (`owned(ctx)`, which a caller handing no check passes, as the
  album page's direct callers do), and a playlist's, a smart mix's and a key's pages ask it once
  their answer comes: every route ends their request, a boost none.
- **Back to the top and Clear are drawn with their body** (`views.js`): Back to the top is built by
  each paint, its click under the same rule as the rail's letters and the Try agains (it acts only
  while no filter commit has come since it was drawn), so a later body's button keeps working; the
  no-match Clear takes the same rule.

Production lines since `7a9d1506`: 31 added and 14 removed, all in `views.js`.

### Tests

The page model (`testdata/list_harness.mjs`) answers a playlist's, a smart mix's and a key's
pages, which it answered 404 before. New scenarios, each a Go test of its name:

- `TestAListLeftForAnAxisPageAsksNothingWhileItsNameIsHeld`: a genre opened from the genres' rows
  (80, so a sentinel past the first 60), its name held, the genres' list scrolled to its end: no
  request, and the genre's grid opens once its name comes; a composer reached by Forward from a
  composers' list whose restore of a jump's window failed, its name held, and that list's Try again
  pressed: no request, and the composer's grid opens (item 1).
- `TestALateDetailAnswerRetitlesNoOperatorPage` (app.js's own boost router): an album, an artist, a
  playlist, a smart mix and a key's tracks, each answer held while an operator page replaces its
  page and let go after: the operator page keeps its title and is scrolled by nothing (item 2).

Extended: `TestNoCallbackOfAListAFilterReplacedActs` presses, while a filter's rows are held, a
jump's Back to the top and the Clear of a filter that matched nothing: neither asks anything, the
field and the URL keep the new q, and the filtered list stands with its count announced; then a
jump in the filtered list and its own Back to the top reach its first row (item 3).

Over `7a9d1506`'s player modules the three are red and the 16 others green: item 1's four checks
(`offset=60&limit=60` asked, the genre's grid not opened, `offset=0&limit=1` asked, the composer's
grid not opened), item 2's five (the titles "D Album 0003", "A Artist 078", "Playlist 2", "Mix 2"
and "A minor" over "Stats"), item 3's four (`offset=0` asked, the count line's announcement still
the unfiltered list's, the field emptied with `q` gone and one request, the whole list standing).

**Negative controls**, each predicted before its run, on copies of the committed modules with
every list scenario run over each, ten in all and each red as predicted: `page()` without its
check (the sentinel's two checks), `load()` without its (the Try again's two), the album's, the
artist's, the collection pages' and the key page's ownership each taken out (each its own checks),
Back to the top's click unguarded (its two), Back to the top bound once as the list is built
(only the check of a later body's button: the old one is retired either way), Clear unguarded (its
two), and `gone()` without ownership (the late restore's scroll, the earlier record's case).

**Counts**: the list harness, 17 scenarios before and 19 after, all green; the bar's harness
unchanged, 193.

### The rule

The list entry's long form (`ops/History-rules-admin-console.md`) names the detail pages beside the
list's answers, says a list's load or page begins only while its route owns its page, and adds Back
to the top and Clear to what a filter's commit retires, with the two new tests. Its line in
`admin-console.md` and its bold sentence stand as they are: "let an answer page on, scroll or
retitle a page its route no longer owns" and "let a callback of a body the filter replaced act"
already say it.

### WebKit

WebKit 27.2, headless, on three test bridges on loopback over the generated library the first record
describes (440 albums, 75 genres ordered by track count with no rail, 75 composers with one, 4
playlists): `d4283cd5`, `7a9d1506` and `ea72ad18`, the server alike at all three. Contexts: a
1280×800 window by mouse, and an iPhone 14 (390×664) by touch, its coarse pointer simulated by the
touch context; the field's size also in the iPhone SE (3rd gen) and the iPad Pro 11, by touch, their
coarse pointer simulated likewise. A request was held by the browser until let go. Four passes; every
named check agreed across them, no page error, and none of the 769 history writes logged refused.

The earlier record's items 1, 2, 5, 6 and 8, `d4283cd5` against `ea72ad18`, in both contexts:

- **Paging past a jump** (its item 1). Base: a jump to M (list #204), scrolled to its rows' end,
  asked nothing and kept 60 tiles, and so did Back to the top's window. Head: `offset=264` asked, 60
  tiles to 236; after Back to the top `offset=60`, 60 to 440.
- **A late answer after a boost** (item 2). Base: a restore held across a boosted navigation to
  Stats and let go scrolled the operator page (scrollY 0 to 76 by mouse, 0 to 59 by touch), and a
  genre's name held across one wrote "Midnight Blues" into the operator page's title, its grid then
  asking its albums and scrolling to 0. Head: no scroll after the operator page's own, its title
  kept, nothing asked.
- **The crumb after a filter's commit** (item 5). Base: `history.go(-1)` to `/albums?q=zzzz`,
  `history.length` 3 to 3. Head: a push of `/albums`, 3 to 4.
- **The crumb after a reload** (item 6). Base: a push (3 to 4), the list at #0, Forward staying on
  the list. Head: `go(-1)` (3 to 3), #150 at its delta (−76.3px by mouse, −59.5px by touch) as it
  stood, Forward reaching the album.
- **The field under a coarse pointer** (item 8, simulated). Base: 14.4px text, its min-height 44px
  already, 44.3px tall, in the SE, the iPhone 14 and the iPad. Head: 16px, min-height 44px, 46.8px
  tall. By mouse 14.4px and no min-height at every build.

This record's three, `7a9d1506` against `ea72ad18`, in both contexts:

- **Item 1.** Base: two `genres?offset=60&limit=60` while a genre's name was held, the lookup
  cancelled and the page left "Genre" over the old rows; the old Try again asked
  `composers?offset=0&limit=1`, that lookup cancelled too, no grid. Head: nothing asked while either
  name was held, both answered, both grids open ("30 albums in Tidal Ambient", "13 albums in Fabian
  Sonnhoff").
- **Item 2.** Base: the album's, the artist's, the playlist's, a smart mix's and a key's names each
  written over "Stats — …". Head: "Stats — …" kept for all five. The library gives the mix engine
  nothing to build (its regeneration answers no families), so the mix page's answer was a made-up
  mix the browser served for it, drawn by the same rendering as a playlist's; the key page is real
  (`/api/library/browse?camelot=8A` answers 200, "A minor", no tracks).
- **Item 3.** Base: the old Back to the top asked the filter's first page again, the filter's own
  request cancelled (the rows then drawn were right, as above); the old Clear emptied the field, took
  q out of the URL and asked for the whole list, which stood, 440 albums. Head: one request; the old
  Clear asked nothing and left the field and q, 22 albums after the answer. A jump within the
  filtered list and its own Back to the top reached its row zero at both builds.

### Runs

On `ea72ad18`, in one build-slot session (11:31 to 12:22): cmd/bridge's doc guards
(`TestNoDocblockNamesAnotherDeclaration`, `TestEveryCitedTestNameExists`, `TestNoBlankKeepers`),
`make fmt` (the tree unchanged after it), `make vet`, `GOOS=linux GOARCH=arm make vet`,
`GOOS=linux GOARCH=386 make vet` and `GOOS=windows GOARCH=amd64 go vet -tags
ts_omit_webclient,ts_omit_debugeventbus ./...`, `make docs-check`, the console's guards and pins by
name (`TestNoConsoleControlDisablesItselfOutsideSetDisabled`, `TestPlayerEmittedClassesAreStyled`,
`TestPlayerCSSDoesNotHijackOperatorTableClasses`, `TestEveryPushStateCarriesTheRouteTrail`,
`TestPlayerBootCallIsLast`, `TestEveryPlayerModuleLoads`, `TestPlayerHeadsMatchServerRoutes`,
`TestPlayerRoutesTableCoversEveryPlayerHead`, `TestCrumbRootsAreRealSections`, the eight of
`player_touch_layout_test.go` and the server's two filter tests), 19, passed; every wrapper of the
two harnesses (212: the bar's 193 and the list's 19) under `-race -count=10`, first with no busy
process of the session's own, 2,120 of 2,120 in 412 s, then beside four busy processes, 2,120 of
2,120 in 390 s, none skipped in either (the browser runs above shared the machine for part of both);
`make test`, 93 packages ok in 2,193 s (cmd/bridge 2,039.7 s, internal/manifest 1,691.3 s,
internal/admin 1,593.2 s); `make build-all`, the six targets. The record's commit changes no code:
on it, `make docs-check` and cmd/bridge's doc guards again.

### Left open

The earlier records' residuals stand. A real smart mix's page stays unmeasured in a real engine (the
test library builds none).
