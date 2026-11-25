## 2026-11-25 — B1021 PR 1: the console by touch on phones, and where a DSD row plays

### What

An audit of the console as phones, a tablet and laptop windows see it in WebKit found six
things a touch reader meets, each fixed here:

1. Up to 768px the bar hid its scrub row (`.np-scrub`, the seek and both times): a phone had
   the 2px rail alone, no way to seek and no numbers, in either output.
2. At 320px the bar's words (the track or "Stopped", the queue count, a notice) shared one row
   with the transport and the extras and rendered 8px wide with the bridge's player chosen,
   40px in this browser.
3. On a 568px phone the A–Z rail ran 571px tall from where it sticks, W to Y under the bar;
   and below 1024px the shell's top bar (65px, sticky, above the rail) covered its first
   letters, at every width up to 1023px.
4. In this browser a DSD row the bridge's player plays read exactly like one it refuses ("DSD —
   not playable in a browser"), and the album's count line said "download those instead"
   where no Download link is shown (up to 640px).
5. The DSD reason chip, nowrap beside the title, squeezed the title to 33px and ran the page
   25px sideways at 320px.
6. An album no row of which plays on the output chosen kept Play, Shuffle and Add to queue
   live, and its Play asked the bridge for the start's barrier before it said "Nothing in this
   selection can play in a browser".

Two more found while measuring in WebKit, both fixed:

- WebKit, driven by touch, moves a range input's value (`input`) and fires no `change` at the
  lift, so a tapped seek never landed and its thumb stayed where the finger left it (on the
  834px tablet layout too, where the seek was shown before this change: a tap at three
  quarters of a 20 s track left it at 2.6 s). The seek now lands once, at its change or at the
  lift (`pointerup`, `pointercancel`) of the pointer that moved it.
- With a track playing on that 568px phone the rail's letters shrink to about 9px, and a glyph
  taller than its box took a tap meant for its neighbour; each letter is clipped to its box.

No server change, no route, and no change to `audio.js` or `output.js`.

### The changes

- `player.css`: up to 768px the scrub row is a row of the bar of its own (`.np-center`'s
  contents placed by grid areas), its seek 44px tall; up to 640px three rows (the words beside
  the output and the queue, then the seek and both times, then the transport), every button
  there 44px square; the bar's rendered height (`--np-bar-h`) read by `--player-bottom-inset`,
  the output menu and the queue panel; the rail between `--az-top` (below the top bar under
  1024px) and `--az-foot` (the bar, or the window's foot), its letters shrinking to fit, each
  clipped to its box; a row's reason chip wraps within 12rem, and up to 640px takes a line of
  its own under the title; the count line's Download words hide where the link does; on a
  touch screen an album's action buttons are 44px square.
- `nowplaying.js`: a ResizeObserver hands the bar's rendered height to the page; the seek's
  commit at its change or its pointer's lift.
- `format.js`: `playFor`'s browser branch plays a row no browser decodes where the queue plays
  it (its fresh variant, `resolvePlayable`) and composes, after `unplayableReason`'s words,
  "the bridge's player plays it" and the way the bridge's chip names (DSD64, native; DoP;
  converted to PCM, never called bit-exact) for a row whose `bridge.way` is not `none`.
- `views.js`: the count line says how many of the rows this browser cannot play the bridge's
  player plays, "choose it under Output, which says whether it can be chosen now", and offers
  Download in words the page hides where it hides the link; Play, Shuffle and Add to queue are
  disabled where no row plays on the output chosen, each naming the count line.

Production against `3bedfc23`: 124 lines added (62 of code, 62 of comment) and 51 removed.
The rules file `admin-console.md` made room first: two entries' long forms moved to its
History file by `move_entry.py` (`verify_moves.py`: PASS), then three entries (the phone's bar,
where a DSD row plays, the album's controls), each a line and its long form. No rules line
said the rail was a phone's position; the comments in `player.css` and `nowplaying.js` did, and
say otherwise now.

### Choices the brief left open

- The phone's bar takes three rows: at 320px nothing less leaves the words a readable width
  beside a 44px seek and the transport. It is 158px tall on a phone (62px before) and 112px
  from 641px to 768px; what sits above it reads its rendered height, so nothing is covered.
- The bar's buttons up to 640px, and an album's action buttons on a touch screen, are 44px
  square now: the brief holds these items' controls to 44px. Touch sizes elsewhere (the
  tablet's bar, rows, other buttons) are left to the next PR.
- The count line's wording: "N of M can't play in a browser. The bridge's player plays K of
  them: choose it under Output, which says whether it can be chosen now. Download those
  instead." It reads the same whether the pick is free or held, since the catalog can describe
  a row while the pick is held; the Output menu's pick says why it is held.
- With the bridge's player chosen, Shuffle stays off with its reason in a title, as before (the
  title-only reasons are the next PR's); the controls this change disables give theirs in the
  count line.
- The chip's wrap applies in both outputs: within 12rem the longer words of this browser leave
  a title in the shared columns wider than before (99px at 834px against 64px, 265px against
  230px at 1280px).

### Tests

New harness cases (`internal/admin/testdata/bar_harness.mjs`, under node):
`TestADSDRowSaysWhereItPlays`, `TestTheAlbumsCountLineSaysHowManyPlayWhere`,
`TestAnAlbumNoRowOfWhichPlaysHereOffersNoPlayShuffleOrAdd`,
`TestAnOutputSwitchRedrawsAnAlbumsRowsLineAndControls`,
`TestTheBarCarriesItsSeekAndBothTimesAndSaysHowTallItIs` and `TestASeekLandsWhereItIsLetGoOnce`.
`TestRowsFollowTheChosenOutput` changes one string, the browser chip of a native-capable DSD
row, keeping its check that the browser cannot play the row; the old words are held for a way
of `none` and for a row with no bridge field in `TestADSDRowSaysWhereItPlays`. Source pins, on
every host: `TestThePhoneBarShowsItsSeekItsTimesAndItsWords`,
`TestWhatSitsAboveTheBarClearsItAsItRenders`, `TestTheDSDChipWrapsUnderItsTitleOnAPhone`,
`TestAnAlbumsControlsTakeAFingerOnATouchScreen` and `TestAnOutputSwitchRedrawsTheRoute`.

Red first: the cases and pins were committed before the change they test (`5eff83bc`, then
`1209f385` for the seek's lift and the rail under the top bar). Run at each of those commits,
over its own harness and modules: at `5eff83bc` six cases failed (17 checks) and three pins
(12 assertions; the fourth, `TestAnOutputSwitchRedrawsTheRoute`, pins boot.js's redraw as it
already was); at `1209f385` the seek's case (4 checks) and the rail's pin (2 assertions). The
per-letter clip went in with its pin (`5c6a7c8f`), shown by its control below. The harness has
173 cases, every one green, the 167 on main with their assertions unchanged but the one string
above.

Controls, each a mutation of a copy (the harness over a copied module directory; the pins as
a prebuilt test binary in a copied `static` tree, since they read the files as they run), with
its red predicted in a file before the run: 31 of 31 as predicted, among them Play re-enabled
(the barrier goes out from the disabled album's Play), the bridge words dropped, a converted
row called bit-exact, a way of `none` given the clause, the variant's playability taken back,
the seek landed at its change alone and landed twice, the height handed as a constant, the
scrub row hidden again, the inset and the panels back on `--bar-h`, the rail back under the
top bar, the letters left to overflow their boxes. The control that showed the Download words
on a phone also failed `TestPlayerEmittedClassesAreStyled`, the class then styled nowhere.

### WebKit, before (`3bedfc23`) and after (this branch)

WebKit 27.2 (build 2388, the WPE port, headless) on Linux, against two test bridges on
loopback, one at each build, over a generated library of invented names: an album of two
20-second FLAC tracks under each letter, a DSD128 album of three DSF tracks, and a mixed album
(FLAC, DSD64, DSD128, DSD512). Seven contexts: the iPhone SE (320×568), a 320×640 phone, a
360×740 phone, the iPhone 14 (390×664), a 700×1000 tablet (the 641-768px range), the iPad Pro
11 (834×1194), all by touch, and a 1280×800 laptop by mouse. The seek case plays a real FLAC
track in this browser's output (silently: no audio device is opened).

The host has no DAC, so the bridge's player cannot run: the rows marked * answer the player's
state (offered, stopped, three entries queued; and held by the DSD check), its queue, its
frame on the event stream and the catalog's `bridge` field (DSD to DSD256 native, DSD512
refused) from fixtures. Each cell is before → after.

| | SE 320 | 320×640 | 360 | 390 | 700 | 834 | 1280 |
|---|---|---|---|---|---|---|---|
| seek (w×h) | 0×0 → 188×44 | 0×0 → 188×44 | 0×0 → 228×44 | 0×0 → 258×44 | 0×0 → 540×44 | 166×16 → 166×16 | 265×16 → 265×16 |
| a tap at ¾ of the seek, 20 s track | none → 16.5 s | none → 16.5 s | none → 16.4 s | none → 16.4 s | none → 16.3 s | 2.6 s → 16.5 s | 16.3 s → 16.3 s |
| both times (width px) | 0/0 → 37/37 | 0/0 → 37/37 | 0/0 → 37/37 | 0/0 → 37/37 | 0/0 → 37/37 | 37/37 → 37/37 | 37/37 → 37/37 |
| bar height, a track playing | 62 → 158 | 62 → 158 | 62 → 158 | 62 → 158 | 68 → 112 | 82 → 82 | 82 → 82 |
| main's bottom padding | 100 → 198 | 100 → 198 | 100 → 198 | 100 → 198 | 108 → 152 | 108 → 122 | 108 → 122 |
| the words' width, a track playing | 40 → 97 | 40 → 97 | 80 → 97 | 97 → 97 | 97 → 97 | 68 → 68 | 97 → 97 |
| the words' width, the bridge's player stopped* | 8 → 132 | 8 → 132 | 48 → 132 | 78 → 132 | 132 → 132 | 68 → 68 | 118 → 118 |
| bar buttons under 44px, the bridge's player* | 5 → 0 | 5 → 0 | 5 → 0 | 5 → 0 | 8 → 8 | 8 → 8 | 8 → 8 |
| rail letters a tap at their centre reaches, above the bar | 21/27 → 27/27 | 24/27 → 27/27 | 25/27 → 27/27 | 25/27 → 27/27 | 24/27 → 27/27 | 24/27 → 27/27 | 27/27 → 27/27 |
| rail letter height | 19.5 → 9.4 | 19.5 → 12 | 19.5 → 15.8 | 19.5 → 12.9 | 14.9 → 14.9 | 14.9 → 14.9 | 14.9 → 14.9 |
| DSD row: title / chip width* | 33/227 → 209/192 | 33/227 → 209/192 | 33/227 → 249/192 | 52/227 → 279/192 | 126/227 → 161/192 | 64/227 → 99/192 | 230/227 → 265/192 |
| the chip under the title* | no → yes | no → yes | no → yes | no → yes | no → no | no → no | no → no |
| the DSD album's page, sideways overflow (px)* | 25 → 0 | 25 → 0 | 0 → 0 | 0 → 0 | 0 → 0 | 0 → 0 | 0 → 0 |
| its Play, Shuffle, Add to queue* | live → disabled | live → disabled | live → disabled | live → disabled | live → disabled | live → disabled | live → disabled |
| their height* | 32 → 44 | 32 → 44 | 32 → 44 | 32 → 44 | 32 → 44 | 32 → 44 | 32 → 32 |
| barrier POSTs from a tap on that Play* | 1 → 0 | 1 → 0 | 1 → 0 | 1 → 0 | 1 → 0 | 1 → 0 | 1 → 0 |
| the Output button* | 32×32 → 44×44 | 32×32 → 44×44 | 32×32 → 44×44 | 32×32 → 44×44 | 32×32 → 32×32 | 32×32 → 32×32 | 32×32 → 32×32 |
| the queue panel's bottom / the bar's top* | 482/492 → 384/396 | 554/564 → 456/468 | 654/664 → 556/568 | 578/588 → 480/492 | 906/918 → 862/874 | 1100/1098 → 1086/1098 | 706/704 → 692/704 |
| the last row's bottom / the bar's top | 468/492 → 370/396 | 540/564 → 442/468 | 640/664 → 542/568 | 564/588 → 466/492 | 855/918 → 838/864 | 764/1098 → 776/1098 | 602/704 → 602/704 |

The output menu's bottom sat 12px above the bar's top in every context, before and after*.
The DSD rows' words after the change, in this browser*: "DSD — not playable in a browser · the
bridge's player plays it (DSD128, native)"; the DSD512 row of the mixed album keeps "DSD — not
playable in a browser". The count line on a phone after the change*: "3 of 3 can't play in a
browser. The bridge's player plays them: choose it under Output, which says whether it can be
chosen now." (its Download sentence hidden there, shown from 641px); before, "3 of 3 can't
play in a browser — download those instead.". With the pick held by the DSD check* the count
line reads the same and the Output menu's pick is disabled with "The DSD check is playing on
the DAC: Stop ends it". Following the remedy*: a tap on Output opens the menu, its pick of
the bridge's player is live, and the tap on it switches the output with nothing sent (this
browser had no queue): the album page redraws with the bridge's chips ("DSD128, native"), no
count line, Play and Add to queue live and Shuffle off. No page measured overflowed sideways
after the change, at any width: the Artists page, the DSD and the mixed albums in this
browser*, and the DSD album with the bridge's player chosen*. The screenshots are kept with
the run, outside the repository.

### Runs

The harness's 173 cases under node, all green. The bar's Go tests (the 179 tests of
`player_output_bar_test.go` and `player_touch_layout_test.go`) under `-race -count=10` at
`5c6a7c8f`, idle: 228 s, ok. The gate (`make fmt vet test build-all docs-check` in one build-slot session, the
release-tag vets, the source guards, and the same race run idle and beside four CPU busy
loops) ran on this entry's commit; its results are in the PR's description, which no record
of its own commit can hold.

### Left open

- No real iPhone or iPad was used: WebKit's WPE port emulating touch is what was measured, and
  how a real iOS range input takes a tap is not shown here.
- The 834px tablet keeps the wider layout's 16px-tall seek (it now lands on a tap) and its
  32px and 40px bar buttons, the touch sizes the next PR takes up.
- On a 568px phone with a track playing, the rail's letters are about 9px tall: each takes a
  tap at its centre, and the bar's three 44px rows are what leaves them that room.
