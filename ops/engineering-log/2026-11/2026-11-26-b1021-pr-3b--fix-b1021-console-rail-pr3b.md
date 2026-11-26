## 2026-11-26 — B1021 PR 3b: one index rail for every list and sort, a finger's where a finger is used

### What

The owner's reports, read at the base (`c2306c2a`, PR 3a merged over main `a23aab63`):

- **The default sort had no rail.** Albums opens on Recently Added; `albumBuckets` answered no
  buckets for Recently Added or Year, and the DTO's comment called that absence the client's sign
  not to draw a rail, so a library of thousands of albums was reached 60 rows at a time.
- **The client could draw letters alone.** `alphabetRail` built # and A–Z and looked each up by
  key: a bucket keyed `2025` or `this-month` would have drawn 27 disabled letters.
- **The Year sort placed years it cannot file among real ones**: 12345 sorted first and 74 below
  1001, where the phone files both under "?" with the undated and sinks them; a "?" key would have
  come in two runs.
- **On a touch screen the letters were mouse-sized, and a finger had to hit one.** The rail was
  sized by width alone (`(max-width: 640px)`), the console's one pointer query `(pointer: coarse)`
  matched no touch laptop whose main pointer is its touchpad, and each letter was a click: no drag.

### The changes

- **The server** (`handlers_player.go`). Every album sort's first page carries its index and the
  kind it is, `rail`, beside `buckets`, both absent where there is none or the list is empty
  (`playerPageMeta.index`):
  - Recently Added, `dates` (`albumRail`): `this-month` from the first instant of the reader's
    month, a future album with it (the sort puts it first, so the runs keep the sort's order),
    `this-year` from the year's (none in January, when the two instants are one), each earlier
    year by its year in the reader's zone, and `?` for an `AddedAt` of 0 or less. The zone is the
    request's `tz`, the minutes `getTimezoneOffset` gives, positive west of UTC, so UTC+2 is −120
    and the zone is `time.FixedZone("", -tz*60)`; a whole number within 840; absent, UTC; anything
    else, 400 `bad_request` (`readerZone`). The clock is `s.now()`. `AddedAt` is in nanoseconds.
  - Year, `years`: a bucket a year from 1000 to 9999 and `?` for the rest; the sort sinks a year
    outside that range with the undated (`yearSortValue`, the phone's), so `?` is one run, last,
    and each tile keeps its tagged year.
  - Title and Artist, `letters`, as before; the artists' and the composers' lists say `letters`
    too, and a genre list ordered by track count still sends neither.
  - The comments that called the store's added date absent (it keeps `first_indexed_at`, which
    the catalog does not read) and the DTO's kinds are corrected.
- **One rail** (`ui.js`'s `indexRail`, `alphabetRail` before it, which PR 1's two cases now call by
  that name). It draws the buckets by `rail`, letters where none is named (a server before the
  kinds sent no other):
  - letters as PR 1 drew them: # and A–Z, empty letters disabled at creation;
  - years as the phone's index: a band each year where there are at most 16 dated keys (`’82`),
    else a band to each decade's run (`’80s`), its years shared out top to bottom by the finger's
    height in the band; `?` a band of its own; every band an equal share, none sized by its count;
    the century on every label (`1982`, `1980s`) unless every year is in the ten decades ending
    with the current one, one decision for the whole rail; a click or a key on a decade's band
    jumping to its first year, the newest as the list sorts;
  - dates an entry each, in the list's order, labelled short (this month's three letters, this
    year's four digits, each earlier year, `?`) and named whole ("This month", "Earlier this year",
    "2025", "Date unknown"), the years' `?` named "Year unknown";
  - the nav's label names its kind ("Jump to letter", "Jump to year", "Jump to date added"), each
    live entry's title counts what is under it, and U+2019 marks an elided century.
- **A finger's rail** (`player.css`). Wherever any pointer is coarse, one column at least 44px
  wide, `calc(100dvh - var(--az-top) - var(--az-foot))` tall (its `100vh` fallback first), its
  entries sharing that height (`flex: 1 1 0`) and each the column's whole width, no padding or gap
  around them, so a finger anywhere on the rail is on the entry at its height; no text selection
  and no touch callout; 8px from the list, where 12px left a 390px phone's albums one column of
  tiles. A window 500px tall or less keeps PR 1's seven columns as tall as their
  letters, by the short window's block after the coarse one (`height: auto`). `touch-action: none`
  holds on the rail in every window, the short grid's included. With a fine pointer alone the rail
  keeps its compact look.
- **The scrub** (`ui.js`). A touch's or a pen's pointerdown on the rail captures its pointer, which
  owns the scrub until its `pointerup`, its `pointercancel` or the rail's own `lostpointercapture`
  (its entry's, fired as the implicit capture moves to the rail, ends nothing). A pointer beside
  the owner moves nothing; a primary pointer going down takes it over, the owner's end lost. It
  takes the entry at the contact point (the event's target, which WebKit gives, PR 1's finding),
  or, where the target is the rail itself, the one `elementFromPoint` gives at the point held
  inside the rail's box; at each move, the entry `elementFromPoint` gives at the finger held there.
  The list jumps (`onJump`, which the list's place writer records) at each new live bucket, never
  from an empty letter, and nothing more at the end. A callout inside the rail (`.az-callout`,
  `aria-hidden`, `pointer-events: none`, in the rail's stacking context, the ledger's popover
  range) says where the list is (in a decade's band, the year under the finger) beside the rail at
  the finger's height, its middle held inside the rail's height so what is pinned above the rail
  and the bar below cover none of it, its words wrapping within the room left of the rail
  (`width: max-content`, capped at the window's width less the rail's, the gutter and its own
  margin), placed before the jump (which empties the list and can move the rail as it does), and
  hides at the end; it goes with the rail when a route clears the view.
- **The clicks.** PR 1's capture listener records the pointer's type beside its target. A click
  (detail above 0) whose pointer went down on the rail by touch or pen is the scrub's: it jumps
  nothing and takes the record. So is the click a scrub's lift makes, whatever pointer went down
  elsewhere since: the owner's `pointerup` sets `owed`, which the next pointerdown clears, as no
  click follows a later pointerdown before its own. A mouse's click keeps PR 1's rule (it jumps
  where it went down on that entry or outside the rail); a key's click (detail 0) jumps and
  leaves the record.
- **The list** (`views.js`): `renderPagedList` hands the rail `r.rail` with `r.buckets`, and the
  albums request carries `tz`. A window restored past the first row already asked the first page
  for its rail (PR 3a), which now carries the kind; and a jump's rows are appended under that
  jump's generation alone (PR 3a's `listGen`), so a rapid scrub never appends an earlier bucket's
  frame: both now have cases of their own.

### Choices

- **No `(min-height: 501px)` block.** The finger's column is the `(any-pointer: coarse)` block's
  default and the short window's `(max-height: 500px)` block after it is the one override,
  because `TestTheBarTakesRowsForAFingerFrom641px` refuses any `(min-height: 501px)` in
  player.css: two height blocks leave a window 500.5px tall in neither. The pin reads the coarse
  block and the order of the two.
- **`touch-action: none` on the rail outside every media block**, so every pointer the scrub takes
  scrubs in every window (a pen on a desktop reports no coarse pointer).
- **The callout inside the rail**, positioned absolutely: it is no flex or grid item, and it leaves
  with the rail; on `document.body` it could outlive a route.
- **No click listener on the rail itself.** A click that lands on the rail's own box jumps nothing
  and the record is replaced at the next pointerdown, which comes before any pointer's click; a
  click listener would make the nav a clickable node for WebKit's touch adjustment, which hands
  an empty letter's tap to the nearest clickable one (PR 1's measurement).
- **A decade's band picks its year by the finger's height in the band** in every layout, the short
  grid's included, where a band is a cell 18px tall.
- **The two layout fixes the evidence asked for**, each a commit of its own after the measurement
  that showed it: the coarse rail 8px from the list (at 12px a 390px phone's album grid fell from
  two columns of 143.6px to one of 302px, two needing 304px), and the callout wrapping within the
  room left of the rail and held inside the rail's height (at 568×320 "Earlier this year" ran
  15px past the window's left edge beside the date grid, then, wrapped, 20px under the top bar),
  then placed before the jump (placed after it, it was measured against the rail while the
  jump's emptied list held a short window's rail 87px lower, and stood at the rail's top until
  the next move).
- **A touch's tap in a decade's band takes the year at its height**, as a scrub's touch-down
  does (the phone's scrubber selects at touch-down by position); a mouse's click and a key take
  the band's first year. Over an empty letter the callout keeps the live letter the list is at.
- **The open decisions as the brief set them**: `(any-pointer: coarse)`; the reader's UTC offset
  as `tz`; the short date labels; the list following a scrub at each new live entry; touch and pen
  scrubbing, a mouse keeping PR 1's click; short windows on PR 1's grid with the scrub across it;
  years outside 1000-9999 under `?`; the callout's words alone.
- **The iOS app's index rules** are taken from the premise check's reading of them (the app's
  `library-lists.md` and its index views), which this host has no copy of.

### Tests

- **The server** (`handlers_player_rail_test.go`, over a seeded library whose `indexed_at` the
  case writes in the store's database before anything asks for the catalog, and a fixed clock,
  `testNow`): `TestTheAlbumRailFollowsEverySort` (for Recently Added, by `sort=recent` and with no
  sort, Year, Title and Artist: the keys, offsets and counts, and `rail`, on the first page; both
  absent from the second, by unmarshalling and checking the keys; the name filter `q` narrowing
  them; the artists' list saying letters), `TestTheDateRailFilesByTheReadersCalendar` (this month
  with an album indexed an hour ago and one 30 days ahead, earlier this year, 2025 and 2019, `?`;
  `tz` absent, 0, 60, −60, 840 and −840, an album at 23:30 UTC on 31 October and one on 31 December
  moving to this month and to earlier this year an hour east of UTC and staying put an hour west;
  in January no earlier this year; `tz` 900, −841, 841, 1.5, `abc` and a space answered 400) and
  `TestTheYearSortFilesYearsItCannotPlaceUnderUnknown` (12345, 74 and none last, in one `?` run,
  the tiles keeping 12345 and 74). All three red at the base, whose albums answered no rail and
  no buckets for Recently Added or Year (`["Wide" "Mid" "Late" "Early" "None"]` by year), and
  green at the head.
- **The rail alone** (`testdata/bar_harness.mjs`, ui.js over the bar's DOM model, which gains a
  no-op `setPointerCapture`, a box a case lays out and `elementFromPoint`, and an inline style that
  keeps what is set on it; `railModel` lays a rail's entries out in columns and answers
  `elementFromPoint` by their boxes; `atDate` holds the clock): `TestTheYearRailFollowsThePhonesBands`
  (16 years 16 bands, `’82`, and `?`; 17 years a band each decade; an 1874 album puts the century
  on every label and every decade's; no band carries a size of its own; a key on a decade's band
  jumps to its first year; a finger at 0.1, 0.6 and 0.95 of the 1980s band picks 1987, 1983 and
  1982, and the callout names 1985 at 0.3), `TestTheDateRailNamesItsPeriods` (Mar, 2026, 2025,
  2019, ? in order; their names whole; the nav's name; the titles' counts; a key on each),
  `TestAFingerScrubsTheRail` (a finger down on A jumps once and the callout names it at the
  finger's height; a drag over A B C C D E F M N Y Z jumps A C D M Z, the callout keeping the live
  letter over an empty one and following the finger; the lift and the touch's click jump nothing
  and hide the callout; a finger down on an empty letter jumps nothing; a cancel ends the scrub; a
  finger on the rail's edge, and past it, takes the letter at its height; a mouse's click jumps
  once with no callout; a callout 40px tall stays inside the rail's height at its first and last
  letters; and the callout is placed against the rail where it stood before the jump moved it, red
  over the commit before it, whose callout was placed after the jump), `TestAScrubIsItsOwnersAlone` (a second finger beside the owner moves
  nothing; the owner's letter's lost capture ends nothing and the rail's ends the scrub and hides
  the callout; after it the owner's moves and lift and the second finger's click jump nothing; a
  key's click jumps; a tap's click after a mouse went down elsewhere is the tap's, one jump; the
  mouse's own click after it jumps) and `TestAScrubCrossesTheShortWindowsGrid` (seven columns: a
  drag across two rows jumps at each new live letter by the cell under the finger, the empty last
  cell moves nothing, a tap at a row's left or right edge takes the cell there). All five red at
  the base (no such rail). PR 1's `TestATapOnTheRailJumpsOnlyFromTheLetterItBeganOn` and
  `TestTheRailTakesEveryPointerDownForItsClick` stay green with their assertions unchanged, their
  rail now called by its new name; at the head a touch's tap jumps at its pointerdown and its click
  is the scrub's.
- **The list's page model** (`testdata/list_harness.mjs`, whose fake server now sends each sort's
  rail as the server does: letters by title, years by year, dates by Recently Added, by an album's
  place in the list): `TestARestoredListKeepsItsRailAndItsKind` (by Title a jump to M, an album,
  Back, the window from M under the letter rail, then its jump to Z; by Year the decades, a jump
  to the 1990s, an album, Back, then the 1960s; by Recently Added a jump to 2025, an album, Back,
  then `?`; every albums request carrying `tz`) and `TestARapidScrubAppendsOnlyTheLastJumpsRows` (a
  scrub's jump to M, its first 40 rows in and a frame holding the rest, then its jump to Z before
  that frame runs: the list is Z's 53 rows alone, in order). Over the base's modules the first is
  red at its Year rail (letters drawn for years) and the second red (no scrub); its guard is PR
  3a's, which the case now holds.
- **Source pins** (`player_touch_layout_test.go`): `TestTheRailTakesAFingerWhereAnyPointerIsCoarse`
  (in the `(any-pointer: coarse)` block the rail's `min-width: 44px`, its height's calc with the
  `100vh` fallback, no padding or gap, its entries stretched and sharing the height, no selection
  or touch callout; `touch-action: none` outside every media block; the short window's block after
  the coarse one with `height: auto`; the callout absolute beside the rail and no pointer's).
  `TestTheRailTakesColumnsWhereTheWindowIsShort` and `TestWhatSitsAboveTheBarClearsItAsItRenders`
  unchanged. No existing assertion changed.
- **Negative controls**, 34, each predicted in writing before its run, all as predicted, none
  touching the tree (Go by `go test -overlay`; the player modules copied and mutated for the
  harnesses; player.css mutated in a copy of the package's static tree under a prebuilt test
  binary): the year sort sent the dates rail; the zone UTC whatever `tz` says; the zone's sign
  flipped; no sink (two `?` runs); the rail on every page; `tz` accepted to 900; a future album not
  this month's; the artists' rail with no kind; a jump at every move; bands sized by their count;
  the century elided on the recent bands alone; no `owed` (two jumps for one tap); any lost capture
  ending the scrub; a second finger taking it over; no clamp into the rail (the edge and past it);
  no point lookup where the target is the rail (both edge cases); the callout left shown at the
  end (two cases); the dates' long labels; a decade's key jumping to its last year; per-year bands
  below 16 years only; a touch's click on the rail jumping again (PR 1's case red); the kind dropped
  in views.js; `listGen` without the jump's generation; no `tz`; the restore painting from its own
  page rather than the first's (this case and PR 3a's red); the finger's rail by width; the
  `touch-action` inside the coarse block only; no `100dvh` height; the callout taking pointers; the
  short block without `height: auto`; the coarse block after the short one; the coarse rail at 12px from the list; the callout on one line
  past the room left of the rail; and its middle no longer held inside the rail's height. Each control red
  in the case named, and the green ones predicted green (a jump at every move leaves the grid case
  green, its moves each a new letter; no `owed` and the click rule each leave the finger's case
  green, the record or `owed` covering its click).
- **Harness counts**: the bar's harness 214 scenarios at the base and 219 at the head (968 checks,
  every one clean), the list's 11 and 13 (each run directly; the operator case needs app.js's
  boost router, which its Go test hands it).

### WebKit and Chromium, base and head

WebKit 27.2 (headless) and Chromium 159 (headless shell), on two test bridges on loopback over one
generated library of invented names: 468 albums of short FLAC tracks, 66 distinct years from 1960
to 2025 and 6 albums with none, a genre ("Chamber") of 27 albums over 13 years (2006-2018) and one
with none, titles under every letter but Q, U and X, `q=lantern` leaving exactly A, M and Z live.
The albums' index times were spread over months and years by writing `indexed_at` (and
`first_indexed_at`, the same value) in each throwaway library's own database with the bridge
stopped (a helper that opens the store as the bridge does), then restarting each bridge twice,
each start's full scan leaving every album's `addedAt` as written: this month 25 (one of them in
the future), earlier in 2026 60, 2025 80, 2024 80, 2023 70, 2022 50, 2021 40, 2020 30, 2019 28,
and 5 never indexed. Nothing answered from a fixture. A track of the library plays first in each
tab, so the bar is shown. Builds: the base `c2306c2a`, the head `2bf4c413`, then `8afb20f4`,
`10529e19` and `395de119` for the measurements the layout fixes move. Input: WebKit by `touchscreen.tap` (it delivers taps,
not drags) and the laptop by mouse; every drag and cancel in Chromium, as touch events sent
through its input protocol (simulated, a viewport with touch, not a device). Contexts: the
iPhone SE 320×568, the iPhone 14 390×664, the iPad Pro 11 834×1194, the iPhone SE held sideways
568×320 and a 1280×800 laptop by mouse in WebKit; the Pixel 7 412×839 and touch windows of
568×320, 320×568 and 834×1194 in Chromium; and the iPhone 14 in America/New_York and Asia/Tokyo.

- **The pointer queries.** Every touch context, both engines: `(pointer: coarse)` and
  `(any-pointer: coarse)` true, `(hover: none)` true; the laptop: all three false; the same at
  the base and the head. No context reports a fine main pointer beside a coarse one.
- **The finger's column, stuck** (the title list 600px down). The rail 44px wide (base 34.7px on
  the phones, 30px on the iPad), its entries 42px wide, the column's whole width inside its
  border, and equal: 9.8px on the SE (base 8.2), 13.3 on the iPhone 14 (11.8), 32.5 on the iPad
  (14.9) and 19.8 on the Pixel 7 (18.3). Its height is the brief's calc: 266px on the SE against
  291.6px from its top (129) to the bar's top, 362 against 387.6 on the iPhone 14, 881 against
  907.2 on the iPad (the base's 445.6), so it stops 25.6px above the bar (26.2 on the iPad), the
  room `--az-foot` keeps, as at the base. `touch-action: none` (base `auto`). On the laptop the
  rail is as at the base: 30px wide, entries 14.9px.
- **Taps at a row's edges**, 2px inside the left and right of the rail's box and of the entry's,
  at D, M and T: 12 of 12 select their row's letter in the SE, iPhone 14 and iPad (base 10 of 12
  on the SE, where the two at the rail's edges at T's height hit the nav and WebKit handed their
  click to S).
- **A drag from # to Z** (Chromium, Pixel 7): 24 requests, one per new live letter, none at Q, U
  or X, at the buckets' offsets in order (0, 4, 24, … 448); the callout naming each live letter
  and, over an empty one, the live letter the list is at (P at Q, T at U, W at X); after the lift
  nothing more asked and the callout hidden; the list's first row "Zenith Letters", #448, Z's
  offset; the page's scroll moved by the jumps alone (400, 389 at the first, 433 after). The base:
  no request; Chromium cancelled the pointer after two or three moves and panned the page from
  400 to 0. Ended by a cancel instead: the requests stop at the cancel and the callout hides.
- **WebKit taps on each live letter**: one request each (24 of 24), the tap's click taken by the
  scrub, in every WebKit touch context; with `q=lantern`, the 24 empty letters: 0 requests (base
  0 of 24 too, PR 1's). The laptop by mouse: a click on M one request, every live letter one
  each, no callout visible at any time, the rail 30px.
- **Year.** Chamber's 13 years: `’18` … `’06` and `?`, named "2018" … "Year unknown"; the
  library's 66: `’20s` `’10s` `’00s` `’90s` `’80s` `’70s` `’60s` `?`, named "2020s" … A drag down
  the `’80s` band (Chromium, 66.9px tall) names 1989 at its top and each year down to 1980 near its
  foot, each new year one request. A WebKit tap on a decade's band takes the year at its height (`’80s`
  at its middle 1985), as a scrub's touch-down does; a mouse's click takes its first year, the
  newest (1989). The base drew no rail for Year.
- **Recently Added**: `Nov` ("This month"), `2026` ("Earlier this year"), `2025` … `2019`, `?`
  ("Date unknown"), each tap one request whose first row is that period's first album (#0, #25,
  #85, … #463); the requests carry `tz` (0; 300 in New York; −540 in Tokyo) and the server's
  periods there move the boundary albums as their zones put them (an album at 02:00 UTC on 1
  November is this month's in UTC and Tokyo and earlier this year's in New York; one at 20:00 UTC
  on 31 December 2025 is earlier this year's in Tokyo alone). The base drew no rail and sent no
  `tz`.
- **568×320**: 27 of 27 letters reachable in the seven columns (base 27 of 27), the grid
  64.1px tall (base 83.1, its gutters gone under a coarse pointer); a Chromium drag along row 1,
  down to row 2's end and along it: 14 requests for 14 new letters; through all four rows in
  reading order, 30 for 24 live letters, the moves from a row's end to the next row's start
  crossing letters again.
- **Widths beside the rail, and the callout.** At 390 (WebKit, iPhone 14) the album grid keeps two
  columns, 141px each beside the 44px rail 8px from it (the base two of 143.6px beside its 34.7px
  rail; `2bf4c413`, the rail 12px from the list, one column of 302px), Recently Added's and Year's
  two of 141px where the base drew them no rail (two of 167px); at 320 one column of 236px (the
  base 241.3px, and 288px with no rail); `scrollWidth` equal to `clientWidth` in every list and
  context, the callout shown or not. Held mid-rail (Chromium), the callout stands left of the rail
  at the finger's height (its centre within 1.3px of it): one line at 320, 412 and 834 ("M"
  49.8×46.8px, "1984", "Earlier this year" 242px); at 568×320 "Earlier this year" wraps to two
  lines, 220×75.6px from x 7 (at `2bf4c413` it ran from x −15, past the window's edge). At each
  rail's first and last entry its middle is held inside the rail's height: its top 9.1px below what
  is pinned above the rail (at `8afb20f4` it ran under the list's head by 3.5-8.5px, and at 568×320
  under the shell's top bar by 5.6px, wrapped by 20px) and its bottom at the rail's, within its
  1px border, 24.7px above the bar; at `395de119` a hold on Z at 568×320 stands beside the finger
  (13.7px above it, the lowest the 64.1px grid allows), where `10529e19`, placing it after the
  jump, had it at the rail's top. In 36 holds (the first, middle and last entries of the title,
  date and year rails in the four Chromium windows) none is covered and none runs past the window.
- **The restore** (PR 3a's, now with the rail's kind): by Title a tap on M, an album, Back, the
  window from M's first row at its delta under "Jump to letter", then Z drawing the list from
  #448; by Year `’90s`, an album, Back, the same row under "Jump to year", then `’60s` (#427, one
  request); by Recently Added `2025`, an album, Back, "Jump to date added", then `?`; in every
  WebKit context.

### Runs

- One build-slot session on `2bf4c413` (08:47–09:23 UTC): `make fmt` (the tree unchanged after it)
  and the pinned toolchain's gofmt over the five changed Go files (nothing listed); `make vet`,
  `GOOS=linux GOARCH=arm make vet`, `GOOS=linux GOARCH=386 make vet` and `GOOS=windows
  GOARCH=amd64 go vet -tags ts_omit_webclient,ts_omit_debugeventbus ./...`; the console's guards
  and pins by name (`TestNoConsoleControlDisablesItselfOutsideSetDisabled`,
  `TestPlayerEmittedClassesAreStyled`, `TestPlayerCSSDoesNotHijackOperatorTableClasses`,
  `TestEveryPlayerModuleLoads` and every `player_touch_layout_test.go` pin) and cmd/bridge's doc
  guards; the thirteen new and rail tests `-race -count=10` (130 passes, 53 s); `make test P=8`
  (93 packages ok, 2,058 s; cmd/bridge 2,019.7 s of its 45 minutes, internal/admin 1,593 s);
  `make build-all` (40 s); `make docs-check` (admin-console.md at 99.7% of its budget). The tree
  clean after it.
- `python3 scripts/rules-for.py --changed` before each commit: `admin-console.md` and
  `build-ci-tests.md`, both read whole.
- After the layout commits (`8afb20f4`, `10529e19`, `395de119`): both harnesses whole (219 and 13
  scenarios, clean), the console's guards and pins, and every negative control again over each new
  head (34, all as predicted). The full gate runs again on this record's commit; a record cannot hold the
  runs of its own commit, so their numbers are the report's.

### Left open

- Recently Added files by `AddedAt`, the earliest `indexed_at` of an album's tracks, which a
  re-index of every track moves: such an album shows under "This month". Ordering by
  `first_indexed_at` (the store has it, the catalog does not read it) changes the sort and the
  catalog's projection, an item of its own.
- The date rail's boundaries use the reader's UTC offset at the request: in a month with a clock
  change, an album indexed within an hour of a boundary may file in the period beside it.
- A touch laptop (a fine main pointer with a coarse one beside it) is pinned at the source alone:
  no context here reports one (every touch context, in both engines, reports both queries coarse,
  the laptop neither).
- A window 500px tall or less keeps PR 1's seven columns: no 44px column there.
- A finger reads no count: the callout says an entry's words alone, and the titles that count
  them reach a mouse (the audit's item 15 stays open).
- The client labels this month and this year by its own clock and the server files them by its
  own, so in the minutes around a month's turn the two can name different months.
