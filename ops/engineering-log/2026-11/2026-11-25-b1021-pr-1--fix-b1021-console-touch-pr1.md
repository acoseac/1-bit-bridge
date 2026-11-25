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
  quarters of a 20 s track left it at 2.6 s). The seek now lands once: a key's at its change,
  a drag's at the lift of the pointer that began it (below).
- With a track playing on that 568px phone the rail's letters shrink to about 9px, and a glyph
  taller than its box took a tap meant for its neighbour; each letter is clipped to its box.

Seven more, in those changes or beside them, each fixed here:

1. The seek landed at any lift or cancel: a cancelled drag wrote its preview and left the
   thumb where the finger had been, and a second finger's lift wrote where the first had moved
   it while the first was still down.
2. A lift landed on whatever was heard by then: a drag on one of the bridge's tracks let go
   after another was published sent `seek(40)` for that one, one let go after a DSD track whose
   seek is off sent a seek too, and one whose seek permission went while the finger was down
   landed once it came back; in this browser a drag let go after the next track had loaded
   sought on it. On main a `change` after the track changed did the same.
3. The seek's case let a write during the drag pass: a seek that wrote at every `input` passed
   all four of its checks.
4. In this browser a `universal` row whose type the decoder does not take, with no variant,
   read as playable: its album's Play, Shuffle and Add to queue stayed live, and Play asked the
   bridge for the start's barrier and played nothing, as on main; a row with no `play` field
   read as playable too.
5. On a 320×568 phone the open navigation drawer reached under the three-row bar: its last
   control (bottom 419px) sat under the bar's top (396px), and a tap there took the bar.
6. On a phone held sideways (568×320) the three-row bar left 148px above it: the queue panel's
   top sat at -24px and the output menu's at -121px, its first and last choices out of reach.
7. There too the A–Z rail had 49px between the top bar and the bar: 7 of its 27 letters took a
   tap (8 on main, where the rail ran 571px from the window's top).

No server change, no route, and no change to `audio.js` or `output.js`.

### The changes

- `player.css`: up to 768px the scrub row is a row of the bar of its own (`.np-center`'s
  contents placed by grid areas), its seek 44px tall; up to 480px three rows (the words beside
  the output and the queue, then the seek and both times, then the transport), and from 481px
  to 768px, a phone held sideways among them, two; every button of the bar up to 640px 44px
  square; the bar's rendered height (`--np-bar-h`) read by `--player-bottom-inset`, the output
  menu and the queue panel, which stay within the window above the bar (the queue within half
  of it where that fits), each scrolling inside; with the bar shown, the shell's drawer, open
  below 1024px, ends above it; the rail between `--az-top` (below the top bar under 1024px) and
  `--az-foot` (the bar, or the window's foot), its letters shrinking to fit, each clipped to its
  box, and where the window is 500px tall or less in seven columns of four rows; a row's reason
  chip wraps within 12rem, and up to 640px takes a line of its own under the title; the count
  line's Download words hide where the link does; on a touch screen an album's action buttons
  are 44px square.
- `nowplaying.js`: a ResizeObserver hands the bar's rendered height to the page. The seek lands
  once: a key's at its change; a drag's at the lift of the pointer that began it (its
  `pointerId`), heard on the document too, never at a cancel, which puts the thumb back where
  the track is, another pointer's lift or cancel, or a change while that pointer is down; and
  only on what it was begun on: render's `seekOn`, the output with the bridge's anchor
  occurrence and entry or this browser's loaded track, while the seek is offered. A drag whose
  occurrence went, or whose seek permission went, lands nothing, even once seeking is offered
  again; a queue edit on the same occurrence keeps it.
- `format.js`: `playFor`'s browser branch asks the queue's question and nothing else
  (`resolvePlayable`: the row's own file this browser's decoder takes, its fresh variant, or an
  engine-dependent attempt); a row it cannot play reads "Not playable in a browser" where
  `unplayableReason` has no words, and after them it composes "the bridge's player plays it"
  and the way the bridge's chip names (DSD64, native; DoP; converted to PCM, never called
  bit-exact) for a row whose `bridge.way` is not `none`.
- `views.js`: the count line says how many of the rows this browser cannot play the bridge's
  player plays, "choose it under Output, which says whether it can be chosen now", and offers
  Download in words the page hides where it hides the link; Play, Shuffle and Add to queue are
  disabled where no row plays on the output chosen, each naming the count line.
- The field guard's exemption list (`barStateExempt`) loses `anchor.occurrence`, which the bar
  now reads for the drag's occurrence: the guard fails while a field the bar reads stays on it.

Production against `3bedfc23`: 217 lines added (126 of code, 87 of comment, 4 blank) and 54
removed; the seven fixes, against `dfb68c7b`, added 111 (70 of code) and removed 22. The rules
file `admin-console.md` made room first: two entries' long forms moved verbatim to its History
file, and each reference to them was checked; then three entries (the phone's bar, where a DSD
row plays, the album's controls), each a line and its long form. The seven fixes extend the
first two entries, lines and long forms, in the commit that makes them (`a5c8c868`). No rules
line said the rail was a phone's position; the comments in `player.css` and `nowplaying.js`
did, and say otherwise now.

### Choices the brief left open

- The phone's bar takes three rows up to 480px: at 320px nothing less leaves the words a
  readable width beside a 44px seek and the transport. It is 158px tall on a phone (62px
  before) and 112px from 641px to 768px; held sideways, a phone keeps two rows, since three
  took more than half of a 320px-tall window. What sits above the bar reads its rendered height.
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
- A drag is bound to the occurrence the bar renders, not to a path: the same entry heard again
  is a new occurrence and ends the drag, and a queue edit (a new generation) keeps it. No
  pointer capture: on a trial build a capture on the range input stopped WebKit's slider
  following a mouse drag (a drag to half of a 300 s track landed at 68 s), so the document hears
  a lift away from the seek instead.
- Where the window is short the rail takes columns rather than letters too small to tap: 27
  letters in one column need 216px of padding alone, and a phone held sideways had 49px.

### Tests

New harness cases (`internal/admin/testdata/bar_harness.mjs`): `TestADSDRowSaysWhereItPlays`,
`TestTheAlbumsCountLineSaysHowManyPlayWhere`,
`TestAnAlbumNoRowOfWhichPlaysHereOffersNoPlayShuffleOrAdd`,
`TestAnOutputSwitchRedrawsAnAlbumsRowsLineAndControls`,
`TestTheBarCarriesItsSeekAndBothTimesAndSaysHowTallItIs`, `TestASeekLandsWhereItIsLetGoOnce`,
`TestADragLandsOnlyOnTheBridgesOccurrenceItWasMadeOn`,
`TestADragInThisBrowserLandsOnlyOnTheTrackItWasMadeOn` and
`TestAnAlbumAsksThisBrowserWhatTheQueuePlays`. The seek's case makes real gestures: a finger
down with its `pointerId` and moved through two values with no write before its lift, a second
finger's lift and cancel, a change while the finger is down and then its cancel (no write, the
thumb back), its own lift (one write at the last value, none at the change after it), and a
key's change with no pointer. `TestRowsFollowTheChosenOutput` changes one string, the browser
chip of a native-capable DSD row, keeping its check that the browser cannot play the row (the
old words are held for a way of `none` and for a row with no bridge field in
`TestADSDRowSaysWhereItPlays`), and its PCM row carries its type (`audio/flac`) on a page set
up, as the queue's question needs. Source pins, on every host:
`TestThePhoneBarShowsItsSeekItsTimesAndItsWords` (the three rows read from the 480px block,
failing where the 640px block sets them), `TestWhatSitsAboveTheBarClearsItAsItRenders`,
`TestTheDrawerAndThePanelsStayClearOfTheBarAndInTheWindow`,
`TestTheRailTakesColumnsWhereTheWindowIsShort`, `TestTheDSDChipWrapsUnderItsTitleOnAPhone`,
`TestAnAlbumsControlsTakeAFingerOnATouchScreen` and `TestAnOutputSwitchRedrawsTheRoute`.

Red first: the cases and pins were committed before the change they test (`5eff83bc`;
`1209f385` for the seek's lift and the rail under the top bar; `e2428d8a` for the seven fixes),
but for two assertions, which landed with their fixes: the album's buttons' 44px on a touch
screen (`TestAnAlbumsControlsTakeAFingerOnATouchScreen`) with its CSS in `6e14eaab`, and the
letters' clip (`overflow: hidden` in `TestWhatSitsAboveTheBarClearsItAsItRenders`) with its
fix in `5c6a7c8f`. Neither condition holds at `3bedfc23`, and each has a control of its own at
`a5c8c868`, red in its test: the touch screen's 44px rule removed; the letters' `min-height:
0` removed with the clip kept; the clip removed with the shrink kept. Run at each commit over
its own harness and modules: at `5eff83bc` six cases failed (17 checks) and three pins (12
assertions; the fourth, `TestAnOutputSwitchRedrawsTheRoute`, pins boot.js's redraw as it
already was); at `1209f385` the seek's case (4 checks) and the rail's pin (2 assertions). The
final harness over the modules of `dfb68c7b`: the seek's case failed 5 checks, the bridge's
drag 6, this browser's drag 2 and the album's question 4; over main's, 3, 6, 2 and 4; and the
new pins over `dfb68c7b`'s stylesheet, three failed (5 assertions). The harness has 176 cases, every one green, the
167 on main with their assertions unchanged but the one string above.

Controls, each a mutation of a copy (the harness over a copied module directory; the pins as
a prebuilt test binary in a copied `static` tree, since they read the files as they run), with
its red predicted in a file before the run. 31 of 31 as predicted for the first changes, among
them Play re-enabled (the barrier goes out from the disabled album's Play), the bridge words
dropped, a converted row called bit-exact, a way of `none` given the clause, the variant's
playability taken back, the seek landed at its change alone and landed twice, the height
handed as a constant, the scrub row hidden again, the inset and the panels back on `--bar-h`,
the rail back under the top bar, the letters left to overflow their boxes; the control that
showed the Download words on a phone also failed `TestPlayerEmittedClassesAreStyled`, the
class then styled nowhere. 18 of 18 for the seven fixes, at `a5c8c868`: the seek landed at
every `input` (red in all three drag cases), at a cancel, at any pointer's lift, at a change
under a drag; the drag kept across another occurrence; the occurrence told by its path; a queue
edit ending the drag; a lost drag taken back; the seek permission not asked; a cancel leaving
the thumb where the finger left it; the old playability short-circuit back; the fallback reason
dropped; the drawer uncapped; the panels uncapped, or capped with no scrolling; the queue held
to half the window alone; the rail in one column when short; three rows up to 640px. And the
three controls above.

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
refused) from fixtures. Each cell is before → after; this table was taken before the seven
fixes, whose own measurements follow it.

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

### WebKit, the seven fixes: main, `dfb68c7b` and `a5c8c868`

The same WebKit and library, three test bridges on loopback, one at each build, each checked
to carry its revision's player files byte for byte: in the iPhone SE (320×568) and the same
phone held sideways (568×320), by touch, and where a row says so by mouse (WebKit's touch
input here takes taps, not drags). Rows marked * answer the bridge's player's state from
fixtures, as above: playing a 300 s track with seeking offered, then another track published,
then a DSD track with seeking off; stopped with 12 entries queued and a DAC name long enough to
wrap for the panels.

| | main | `dfb68c7b` | `a5c8c868` |
|---|---|---|---|
| a tap at ¾ of the seek, 20 s track, 320×568: time heard, seeks made | no seek shown | 16.5 s, 1 | 16.5 s, 1 |
| the same at 568×320 | no seek shown | 16.3 s, 1 | 16.3 s, 1 |
| the same on the 834px tablet | 2.2 s, 0 | 16.5 s, 1 | 16.5 s, 1 |
| the same by mouse at 1280×800 | 16.4 s, 1 | 16.4 s, 1 | 16.4 s, 1 |
| a mouse drag in this browser held across Next, then let go: seeks on the next track | no seek shown | 1 (at 12.7 s) | 0 |
| a mouse drag to half, let go on the seek, then one let go above it* | no seek shown | 150 s, 229 s | 150 s, 229 s |
| a drag let go after another track was published* | no seek shown | `seek(40)` | none |
| a drag let go after a DSD track with seeking off* | no seek shown | `seek(100)` | none |
| the open drawer at its foot, 320×568: its bottom / the bar's top; its last control takes a tap | 462/492, yes | 462/396, no (the bar takes it) | 369/396, yes |
| the queue panel at 568×320*: top / bottom / the bar's top; first and last entries take a tap | 74/234/244, yes | -24/136/148, yes | 24/184/196, yes |
| the output menu at 568×320*: top / bottom; scrolls; first and last choices take a tap | -23/234, no, yes | -121/136, no, no | 8/184, yes, yes |
| the A–Z rail at 568×320, every letter live: letters a tap at their centre takes, each asking its own offset | 8/27 (rail 571px tall from 8px) | 7/27 (49px) | 27/27 (seven columns, 167×83 from 73px) |
| the same at 320×568 | 20/27 | 27/27 | 27/27 |
| the rail at 568×320 with three letters live (A, M, Z): live letters reached; taps on the 24 empty ones that jumped | 0/3; 0 | 1/3; 3 | 3/3; 8 |

The taps on empty letters that jumped did so to the nearest live letter's offset: WebKit's
touch adjustment hands a tap on a disabled button to a live one within the finger's area, on
every build that has a live letter there. The empty letter is disabled and jumps nowhere of its
own; keeping that tap on it would need the letters to answer taps themselves, which this change
leaves alone. The screenshots are kept with the run, outside the repository.

### Runs

The harness's 176 cases, all green. The whole console package, once, at `a5c8c868`: ok in
354 s. The bar's Go tests under the race detector, ten times, at `5c6a7c8f`, idle: 228 s, ok.
The gate ran on `b882b658`, this entry before this paragraph's wording was corrected, in one
session of the shared build lock: formatting, vet, vet with the release tags for the six
release targets and for linux/386 and linux/arm, the docs' budgets, the source guards by name,
the bar's Go tests (184, in `player_output_bar_test.go` and `player_touch_layout_test.go`) under
the race detector ten times idle and ten times beside four busy processes, the whole
race-enabled suite and the cross-builds. Its results are in the PR's description.

### Left open

- No real iPhone or iPad was used: WebKit's WPE port emulating touch is what was measured, and
  how a real iOS range input takes a tap is not shown here. Nor is a touch drag: the cancel, the
  second finger and a finger held across a track change are shown in the harness alone.
- The 834px tablet keeps the wider layout's 16px-tall seek (it now lands on a tap) and its
  32px and 40px bar buttons, the touch sizes the next PR takes up.
- On a 568px phone with a track playing, the rail's letters are about 9px tall: each takes a
  tap at its centre, and the bar's three 44px rows are what leaves them that room.
- A tap on an empty letter of the rail can jump to a live neighbour by WebKit's touch
  adjustment, as above.
