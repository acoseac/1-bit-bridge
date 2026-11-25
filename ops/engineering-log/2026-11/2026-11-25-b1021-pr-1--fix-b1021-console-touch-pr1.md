## 2026-11-25 — B1021 PR 1: the console by touch on phones, and where a DSD row plays

The seek's gesture handling is not in what this PR ships: it was taken out before the merge,
the seek put back as main has it and hidden up to 768px, where the phone shows its times
read-only beside the rail (the section "The seek taken out", near the end). The sections before
it record what the branch did and measured at the commits they name; where they describe a seek
that lands at a pointer's lift, a drag's owner or identity, a count of plays, a phone's 44px seek
or the cases that tested them, they describe code that no longer ships.

### What

An audit of the console as phones, a tablet and laptop windows see it in WebKit found six
things a touch reader meets, each fixed here (the first in part: the times, not the seek):

1. Up to 768px the bar hid its scrub row (`.np-scrub`, the seek and both times): a phone had
   the 2px rail alone, no way to seek and no numbers, in either output. Its times are shown
   now; its seek stays hidden there.
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

Two more found while measuring in WebKit, the second fixed:

- WebKit, driven by touch, moves a range input's value (`input`) and fires no `change` at the
  lift, so a tapped seek never landed and its thumb stayed where the finger left it (on the
  834px tablet layout too, where the seek was shown before this change: a tap at three
  quarters of a 20 s track left it at 2.6 s). The branch landed a drag at the lift of the
  pointer that began it (below); that was taken out, and the seek lands at its change as on
  main, so a touch seek is an item of its own.
- With a track playing on that 568px phone the rail's letters shrink to about 9px, and a glyph
  taller than its box took a tap meant for its neighbour; each letter is clipped to its box.

Seven more, in those changes or beside them, each fixed here (the first three in the seek's
gesture handling, which went with it):

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

No server change and no route; `audio.js` gains the key of the source it chose, and records a
confirmed decode failure in `format.js`'s set, and `output.js` only `listen`'s redraw on the
output in effect (all below).

### The changes

- `player.css`: up to 768px the scrub row is a row of the bar of its own (`.np-center`'s
  contents placed by grid areas), its seek hidden and both times at its two ends, read-only
  beside the rail; up to 480px three rows (the words beside the output and the queue, then the
  transport, then the times), and from 481px to 768px, a phone held sideways among them, two,
  the times' row last at both widths; wider, the seek as on main; every button of the bar up
  to 640px 44px square; the bar's rendered height (`--np-bar-h`) read by
  `--player-bottom-inset`, the output menu and the queue panel, which stay within the window
  above the bar (the queue within half
  of it where that fits), each scrolling inside; with the bar shown, the shell's drawer, open
  below 1024px, ends above it; the rail between `--az-top` (below the top bar under 1024px) and
  `--az-foot` (the bar, or the window's foot), its letters shrinking to fit, each clipped to its
  box, and where the window is 500px tall or less in seven columns of four rows; a row's reason
  chip wraps within 12rem, and up to 640px takes a line of its own under the title; the count
  line's Download words hide where the link does; on a touch screen an album's action buttons
  are 44px square.
- `nowplaying.js`: a ResizeObserver hands the bar's rendered height to the page, and the rail
  paints where the track is at every tick (`setRail`), never the seek's preview; the seek's
  handlers are main's. (Until the seek was taken out, the seek landed a drag at the lift of the
  pointer that began it, owned by that pointer and bound to the play it began on: the
  sections below.)
- `format.js`: `playFor`'s browser branch asks the queue's question and nothing else
  (`resolvePlayable`: the row's own file this browser's decoder takes, its fresh variant, or an
  engine-dependent attempt); a row it cannot play reads "Not playable in a browser" where
  `unplayableReason` has no words, and after them it composes "the bridge's player plays it"
  and the way the bridge's chip names (DSD64, native; DoP; converted to PCM, never called
  bit-exact) for a row whose `bridge.way` is not `none`. `resolvePlayable` passes over a source
  in the page's set of confirmed failures, keyed by the row's path and the variant its play
  names (`sourceKey`), and hands back the key of the source it chose; `markUnplayable`
  records one.
- `views.js`: the count line says how many of the rows this browser cannot play the bridge's
  player plays, "choose it under Output, which says whether it can be chosen now", and offers
  Download in words the page hides where it hides the link; Play, Shuffle and Add to queue are
  disabled where no row plays on the output chosen, each naming the count line; a row that
  played here at render and no longer does redraws the open album, once a failure.
- `audio.js`: the source a load or a restore chose is kept, and a decode failure the byte probe
  confirms records it, the catalog's `play` left as it was sent. (A count of plays the seek's
  drag was bound to went with the seek.)
- `output.js`: `listen` redraws the route when the output in effect changes: the pick, or this
  browser where no player is offered.
- `ui.js`: one capture listener a document records what every pointer went down on, and a
  pointer's click on a live letter of the A–Z rail jumps only where it went down on that
  letter or outside the rail, taking the record; a key's click is the letter's own.
- The field guard's exemption list (`barStateExempt`) is main's: it lost `anchor.occurrence`
  while the seek's drag read it, and has it back since the drag went.

Production against `3bedfc23`, as it ships: 257 lines added (139 of code, 110 of comment, 8
blank) and 76 removed. At `171029c7`, before the seek was taken out, 336 added (207 of code,
120 of comment, 9 blank) and 80 removed; the seven fixes, against `dfb68c7b`, added 111 (70 of
code) and removed 22, and the five after them, against `0dedea79`, 72 (47 of code) and 22, and
the four after those, against `06b6d41b`, 110 (71 of code) and 67. The rules
file `admin-console.md` made room first: two entries' long forms moved verbatim to its History
file, and each reference to them was checked; then three entries (the phone's bar, where a DSD
row plays, the album's controls), each a line and its long form. The seven fixes extend the
first two entries, lines and long forms, in the commit that makes them (`a5c8c868`), and the
five after them the first and the third (`6ab95527`), and the four after those all three, with
the guard that pins the decode-only mark to its new call; the seek's taking out rewrote the
first entry's long form (`6ce45510`), its line left for a commit of its own (the last
sections). No rules line said the rail was a phone's position; the comments in `player.css`
and `nowplaying.js` did, and now say it is a phone's progress line beside the times.

### Choices the brief left open

- The phone's bar takes three rows up to 480px: at 320px nothing less leaves the words a
  readable width beside the transport. With the phone's seek a 44px row, it was 158px tall on
  a phone (62px before) and 112px from 641px to 768px; the times' row that replaced it is
  shorter, and the bar's height with it is not measured in WebKit here. Held sideways, a phone
  keeps two rows, since three took more than half of a 320px-tall window. What sits above the
  bar reads its rendered height.
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
- While the seek's drag was in, it was bound to the occurrence the bar renders, not to a path:
  the same entry heard again was a new occurrence and ended the drag, and a queue edit (a new
  generation) kept it. No pointer capture: on a trial build a capture on the range input
  stopped WebKit's slider following a mouse drag (a drag to half of a 300 s track landed at
  68 s), so the document heard a lift away from the seek instead. Both went with the drag; the
  capture's finding is the touch seek's item's to read.
- Where the window is short the rail takes columns rather than letters too small to tap: 27
  letters in one column need 216px of padding alone, and a phone held sideways had 49px.

### Tests

New harness cases (`internal/admin/testdata/bar_harness.mjs`): `TestADSDRowSaysWhereItPlays`,
`TestTheAlbumsCountLineSaysHowManyPlayWhere`,
`TestAnAlbumNoRowOfWhichPlaysHereOffersNoPlayShuffleOrAdd`,
`TestAnOutputSwitchRedrawsAnAlbumsRowsLineAndControls`,
`TestTheBarCarriesItsSeekAndBothTimesAndSaysHowTallItIs`, three cases of the seek's gesture
(its landing at a lift, the bridge's occurrence, this browser's track), and
`TestAnAlbumAsksThisBrowserWhatTheQueuePlays`; and for the five fixes after them three more of
the seek's (the second pointer, the engine's run, this browser's play),
`TestAnAlbumRedrawsWhenTheOutputFallsBackToThisBrowser`,
`TestAConfirmedDecodeFailureDisablesTheAlbumsHopelessPlay` and
`TestATapOnTheRailJumpsOnlyFromTheLetterItBeganOn`. The seek's cases were retired with its
gesture handling (the section "The seek taken out"). The harness's album answers are parsed
afresh per request, so no page's rows are the queue's objects, and a pointer's move fired
`pointermove` before its input, as a browser's does. The seek's first case made real gestures: a finger
down with its `pointerId` and moved through two values with no write before its lift, a second
finger's lift and cancel, a change while the finger is down and then its cancel (no write, the
thumb back), its own lift (one write at the last value, none at the change after it), and a
key's change with no pointer. `TestRowsFollowTheChosenOutput` changes one string, the browser
chip of a native-capable DSD row, keeping its check that the browser cannot play the row (the
old words are held for a way of `none` and for a row with no bridge field in
`TestADSDRowSaysWhereItPlays`), and its PCM row carries its type (`audio/flac`) on a page set
up, as the queue's question needs. Source pins, on every host:
`TestThePhoneBarShowsItsTimesAndItsWordsButNoSeek` (named for the phone's 44px seek it pinned
until the seek was taken out; the three rows read from the 480px block, failing where the
640px block sets them), `TestWhatSitsAboveTheBarClearsItAsItRenders`,
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
new pins over `dfb68c7b`'s stylesheet, three failed (5 assertions). The five fixes' cases were
committed first (`75fb6748`), and over the modules of `0dedea79` the pointer's case failed 2
checks, the restart's 2, the replay's 3, the fallback's 1, the decode failure's 3 and the
rail's 2; `04d7e917` added a check each to the pointer's and the rail's cases that holds at
both ends (the owner's last value after the other pointer moved; a click no touch on the rail
began), and the decode failure's single redraw; `a064b431` made the restart's case change the
engine's run alone, red over `0dedea79`'s modules (a 210 s seek). The harness then had 182
cases, every one green, the 167 on main with their assertions unchanged but the one string
above.

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
to half the window alone; the rail in one column when short; three rows up to 640px. 15 of 15
for the five after them, at `6ab95527`: a second pointer taking the drag; another pointer's
input taken as the owner's; an input following the last pointer down alone (red only where
the owner moves again); the run left out; the whole generation in its place (red in the
queue edit's checks of both bridge cases); the play left out; a replay not counted; a load not
counted; the pick in place of the output in effect; the failures not taken at render (a storm
of redraws, which the case cuts at three); no redraw on a failure; failures matched by object,
not path; the click not asked where its pointer went down; a touch kept past its click; a key's
click taken for a pointer's. And the
three controls above.

### WebKit, before (`3bedfc23`) and after the first changes, a phone's seek shown

The "after" column is the branch's layout with a phone's 44px seek row: the seek's rows, the
bar's height and main's bottom padding below are not what ships, which shows the times alone
there and no seek (the section "The seek taken out"); the other rows are the layout's.

WebKit 27.2 (build 2388, headless) on Linux, against two test bridges on
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
touch adjustment hands a tap on a disabled button's click to a live one within the finger's
area, the next section's fourth fix. The screenshots are kept with the run, outside the
repository.

### Five more, in those changes and beside them

The first two were in the seek's gesture handling, which went with it.

1. A second pointer took the drag: one the browser calls primary (a mouse beside a finger, a
   touch beside a mouse) replaced the first's drag and its lift landed the first's preview; and
   the range is one value, so another pointer's move was what the first's lift landed (on main,
   the change at a mouse's release landed the value a tap had moved the range to).
2. The drag's identity left out the engine's run and this browser's play: an engine restarted
   under one serve, its queue restored, hears the same entry as occurrence 1 again, and a drag
   made before it landed on it; and the same entry loaded again under repeat all, or replayed
   under repeat one, which loads nothing, took a drag made on the play before it (on main too,
   at the release's change).
3. The album redrew on the output picked, not the one in effect: with the bridge's player picked,
   GET now saying no player is wired made this browser the output and left the album drawn for
   the bridge's player, its Play disabled over a row this browser plays (on main the row stayed
   so, its Play live).
4. A confirmed decode failure (a media error of code 4, the byte probe answered with the file)
   marks the queue's row unplayable, and an album shown after the queue started, its rows parsed
   afresh, kept a live Play whose second press asked the barrier again for nothing, on main too.
5. WebKit's touch adjustment handed a tap on an empty letter's click to the nearest live letter:
   at 568×320 with three live letters, 8 of the 24 empty taps jumped. The tap's pointerdown
   reaches the empty letter itself, at the contact point, so the rail can tell.

And two records: the cases' lists in the rules' History named the runtime the harness runs on,
and a comment in `player.css` quoted an album's title from main's evidence; both say it in
plain words now. The changes are in the list above (`nowplaying.js`, `audio.js`, `output.js`,
`format.js`, `views.js`, `ui.js`).

### WebKit, the five fixes: main, `0dedea79` and `6ab95527`

The same WebKit and library, a test bridge at each build, each checked to carry its revision's
player files byte for byte. WebKit's touch input here takes taps, not drags, so a drag is a
mouse's, and the second pointer a touch tap. Rows marked * answer the bridge's player's state
from fixtures, as above.

| | main | `0dedea79` | `6ab95527` |
|---|---|---|---|
| on the 834px tablet, a mouse drag to half of a 20 s track, a touch tap at four fifths under it, then the mouse let go: seeks | 16.3 s, at the release | 16.3 s, at the tap's lift, the mouse still down | 10 s, at the release |
| with the bridge's player chosen, 320×568*: a drag to half of 300 s, let go | no seek shown | 150 s | 150 s |
| the same, the engine restarted under it (only its generation's run changed, the entry heard as occurrence 1 again)* | no seek shown | 212 s | none |
| the same, a queue edit within that run* | no seek shown | 117 s | 117 s |
| repeat one, 834px: a mouse drag held from before the track's end, let go 0.3 s into its replay: seeks on the replay | 10 s | 10 s | none |
| the bridge's player picked*, an album of FLAC rows the catalog says nothing of, 320×568: its Play, Shuffle, Add to queue | live, off, live | off, off, off | off, off, off |
| then GET now says no player is wired*: the same, and the row | unchanged, the row unplayable | unchanged, the row unplayable | live, live, live, the row live |
| the A–Z rail at 568×320, every letter live: letters a tap at their centre takes, each asking its own offset | 8/27 | 27/27 | 27/27 |
| the same at 320×568 | 20/27 | 27/27 | 27/27 |
| the rail at 568×320 with A, M and Z live: live letters reached and asking right; empty letters tapped; of them, jumps | 0/3; 8; 0 | 3/3; 24; 8 | 3/3; 24; 0 |

In the first row WebKit moved the range to the tap's position under the mouse's drag, so the
two pointers did share it. A stream reconnecting to a new epoch renders the reconnecting bar
before the new epoch's first frame, which ends a drag on every build: the restart measured is
the engine's under one serve. The screenshots are kept with the run, outside the repository.

### Four more, and the last

The first two were in the seek's gesture handling, which went with it.

1. A pointer that began no drag could seek: one that went down during another's drag was turned
   away, and nothing remembered it, so once the owner lifted its inputs previewed and its change
   landed through a key's path, on whatever played by then. A touch owning a drag on one track,
   a mouse pressed beside it, the touch lifted (its value landed once), the track changed, and
   the mouse's move and release sought the new track.
2. A drag outlived its play: a repeat-one replay and the automatic retry start a new play with
   no render between, and the lift read the state the bar last rendered, so a lift before the
   replay's render, or after the retry, which moved neither the generation nor the replays,
   landed on the new play; and a drag begun after a replay, before its render, landed nothing.
3. A confirmed decode failure left the album page that started the play actionable: the failure
   marked the catalog's `play` in place, on an object that page's row shares with the queue, so
   the page's check found the row marked already and redrew nothing, and its Play sent a barrier
   for nothing; a DSD row whose one browser variant failed was `none` already and changed
   nothing; and the queue stored for a reload kept the mark past it.
4. The rail's record of where a touch began went stale: a live letter's click alone cleared it,
   so an empty tap that made no click refused the next click handed to a live letter, and a
   key's click cleared a held touch's record, whose handed click then jumped.

The event order, recorded first in WebKit 27.2 (an iPhone SE and an iPad Pro 11, by touch and
by mouse) and in Chromium 159, headless (a Pixel 7 and a desktop window with touch, and a touch
drag given to it as touch events, lifted and cancelled): an `input` comes only while the
pointer that moves the range is down, and none after its `pointerup`; Chromium fires `change`
right after a touch's or a mouse's `pointerup`, WebKit after a mouse's alone, and neither after
a `pointercancel`; and a touch tapped on the range under a mouse's drag moves the range (its
`input` between its own down and up), the mouse's release then firing `change` at the touch's
value. So a refused pointer's trailing `change` follows inputs the bar undid, and an assistive
adjustment's follows its own input, which previews: the bar tells them apart by that input,
with no timer.

The decisions then stated in the rules: a new play (a replay, repeat one's or repeat all's, or
the automatic retry) ended a held drag and put the thumb back; a source this browser confirmed
it cannot decode, its path and its variant, stays unplayable until the page reloads, a
replacement at the same path and variant included, and the queue stored for a reload keeps the
catalog's `play`, so a reloaded page plays the row again; and a key's or an assistive
adjustment's input and change, with no pointer down, still previewed and landed. The first and
the last went with the seek's gesture handling; the second stands, and a reload proves it
now (the next section).

At `f1020508`, in WebKit again: a touch tap on the seek lands once in every context; a touch
tapped under a mouse's drag lands nothing and the mouse's own value lands at its release; nothing
lands on a repeat-one replay; the rail's 27 letters each ask their own offset at 568×320 and
320×568, and with three live letters none of the 24 empty taps jumps.

Red first: the cases were committed first (`6a55bed5`, and `76334826` for the tick's), each
failing over the modules of `06b6d41b`: the seek's case of a pointer that began no drag 2
checks, of the replay 2, of the retry and the prime's cancel 1,
`TestADecodeFailureClosesTheAlbumThatStartedIt` 5, `TestAFailedVariantIsThatSourceAlone` 1,
`TestTheRailTakesEveryPointerDownForItsClick` 2 and the seek's case of the preview at the
tick 1 (the seek's four retired since, with its gesture handling); their positive checks pass there (the
owner's own lift in either order, one touch with no change after it, a key's step, an assistive
adjustment, a primed track let go of under a drag, a fresh answer naming another variant). The
harness's pointerdown bubbles through the element's ancestors to the document, as a browser's
does, and the fallback case's enabled controls are asserted to exist and be enabled, the one
assertion of an earlier case changed. One earlier case renders its album twice under one route
generation, so both renders heard the failure: the page asks one redraw a failure. Controls,
each a mutation of a copy of the committed modules: 17 of 17 as predicted (a later
input taken after the owner's lift; the pointerdown's play from the render; the landing's from
the render; a load, or the replay, not counted as a play; the preloader counted; no settling at
the tick; the element's URL as the failure's key; the catalog's `play` marked again; the variant
left out of the key; a redraw a listener; the resolver passing a failed file, or a failed
variant; no redraw at a failure; the rail's record kept past a pointer's click, taken by a key's
click, or blind to a pointerdown outside a rail).

### The seek taken out

Three hazards were still open in the seek's gesture handling at `171029c7`: the input and the
change that trail a pointer the bar refused; a tick between an input with no pointer down and
its change; and a seek permission withdrawn and given back while an adjustment was pending.
The touch seek is an item of its own; this PR ships the rest, the seek as main has it, hidden
up to 768px, where a phone shows its times.

Taken out, by file:

- `nowplaying.js`: the pointers down and the drag's owner; the drag's identity (the play it
  began on, a drag lost, the landing's question and the key's); the landing at a pointer's lift
  and the document's lift listeners; the settle at the tick, the follow at render and the
  put-back with its paint. The seek's handlers are main's, byte for byte.
- `audio.js`: the count of plays (its declaration, its moves at a load and at the repeat-one
  replay, the snapshot's `play`), which the drag's identity alone read. The source key
  (`state.source`, taken at a load and a restore) stays, and so does main's generation check
  that drops a stale failure probe: the failed source's record needs both.
- `player_output_wiring_test.go`: main's file, `anchor.occurrence` back on `barStateExempt`,
  since the bar reads no occurrence now (the guard fails with it off the list: a control
  below).
- The harness: the ten cases that tested the gesture handling alone (its landing at a lift,
  the bridge's occurrence, this browser's track, the second pointer, the engine's run, this
  browser's play, the pointer that began no drag, the replay, the retry and the prime's
  cancel, the preview at the tick), their Go wrappers and the helper that made their pointers'
  events. No kept case's assertion changed.

Kept as they were at `171029c7`: `format.js`, `views.js`, `output.js` and `ui.js`;
`nowplaying.js`'s ResizeObserver; `player.css`'s rows, panels, drawer, rail, chip and album
buttons; the History's second and third B1021 entries.

Changed for the phone (`6ce45510`):

- `player.css`, up to 768px: the seek hidden with the chip and the volume; the times at the
  scrub row's two ends (`justify-content: space-between`), read-only; the rail shown, hidden
  from 769px as on main. Up to 480px the rows are the words, the transport, then the times,
  the order from 481px to 768px, where the times' row is the last too. Wider than 768px the
  seek is main's.
- `nowplaying.js`: the rail paints where the track is at every tick (`setRail`), and the
  seek's fill alone takes a preview (`setProgress`). Main's seek keeps a preview from an input
  until a change, and WebKit fires no change after a touch; with the rail painted from the
  seek's fill, as on main, a preview a wider window left standing through a resize to a
  phone's width held the rail where the preview was while the track played on (the rail's
  case below, red over main's modules).

Tests:

- `TestTheSeekLandsOnlyAtItsChange` (5 checks): in this browser an input and a pointer's lift
  write no time and the change writes one, at the value let go (100 s of 200); with the
  bridge's player chosen an input and a lift send no seek and the change sends one (150 s of
  300).
- `TestTheRailFollowsTheTrackPastAPreview` (3 checks): a pointer down and an input with no
  change after it, and at each tick the rail at the track's position, in this browser (25%,
  then 50%) and on the bridge's player (its anchor's).
- `TestADecodeFailureClosesTheAlbumThatStartedIt` (10 checks, 3 new): the source refused on
  its page to the end; then a reload, a new page over the stored queue with modules of its
  own: the row restored, paused, the same source on its element, and its Play asks the barrier
  and plays that source again.
- `TestThePhoneBarShowsItsTimesAndItsWordsButNoSeek`, the pin renamed: its 44px seek
  assertion replaced by the seek hidden up to 768px, the times at the scrub row's ends, no rule
  up to 768px hiding the rail and one hiding it from 769px, and the times' row last up to 768px
  and up to 480px; its row and button assertions as they were.

The harness has 167 cases on main, 189 at `171029c7` and 181 now (ten retired, two new); main's
167 are all there, with their assertions as the sections above say (the one string, the album
answers parsed afresh, the pointerdown that bubbles), and every kept case of this PR keeps its
assertions, the decode failure's gaining three.

Red first (`f760047e`, before the change): over `171029c7`'s modules the seek's case failed 2
checks (the lift landed: a time written in this browser, `seek(150)` sent to the bridge's
player), the rail's case 2 (this browser's: the held drag kept the rail at 90%) and the pin 3
assertions (the seek shown, the times side by side, the times' row not last up to 480px); over
main's modules the seek's case passed, the rail's case failed its 3 checks (the rail at 90% in
both outputs) and the decode failure's 3 new checks failed (main's page that started the play
draws no reason for the row, and main marks the catalog's `play`, which the stored queue kept:
no row restored after the reload), beside its earlier checks, which main does not meet either.

Controls, each a mutation of the committed tree (`6ce45510`), its red predicted in a file
before the run and the file restored after it: 11 of 11 as predicted. The rail painted
from the seek's fill again (the rail's case red on its 3 checks, the seek's green); the rail
left out of this browser's tick (its two checks) or the bridge's (its one); the seek landed at
its input, or at a pointer's lift (the seek's case red, in both outputs: all five checks, and
four, more than predicted, each a consequence of the mutation); `anchor.occurrence` off the
exemption list (the field guard red); the seek shown up to 768px; the times not at the row's
ends; the rows up to 480px back to words, times, transport; the rail hidden up to 768px (the
pin red, an assertion each); the catalog's `play` marked in place again beside the key (the
decode failure's case red on the stored queue and both reload checks).

In WebKit 27.2 at `a9ced775` (its code is `6ce45510`'s), headless, emulating an iPhone SE and
an iPad Pro 11 by touch, at 320, 768, 769 and 834px: a track playing in this browser, and the
bridge's player's state, queue and frames from fixtures, every other route served by the bridge
measured, which has no DAC. Up to 768px the range is not displayed and both times are, at the
two ends of their row (its `justify-content` is `space-between`), in both outputs; at 769px the
range shows, 134 by 16px, between the times. The 2px rail shows up to 768px and not at 769px,
and follows playback: in this browser from 4.2% to 11.7% of 20 s while the audio's time read
1.0 s, then 2.5 s; on the bridge's player from 3.72% to 4.22% of 300 s (11.2 s to 12.7 s)
while the elapsed time read 0:11, then 0:13, a label rounded to the second. At 834px, in both
outputs, an input and then a `pointerup` wrote no seek and the change after them wrote one; a
mouse drag wrote none during the drag and one at the change after its lift; a touch tap on the
range wrote none, as on main. In this browser's output alone, a preview held at nine tenths of
the range at 1024px, then the window narrowed to 768px before the lift: the rail painted the
track's position (9.0%, 16.5% and 19.0% at 2.0, 3.6 and 4.1 s of 20 s), not the preview's. The
album's Play, Shuffle and Add to queue are 44px tall at 320, 768, 769 and 834px, enabled in this
browser's output; in the bridge's they are disabled with the reason drawn, since its player has
said nothing of what each row plays.

The rules file's line for the bar's entry (`admin-console.md`) takes the long form's title
and bold sentence from `6ce45510` word for word, in a commit of its own (`a9ced775`).

### Runs

Before the seek was taken out: the harness's 189 cases, all green; the whole console package,
once, at `a5c8c868`, ok in 354 s; the bar's Go tests under the race detector, ten times, at
`5c6a7c8f`, idle, 228 s, ok; and the gate on `b882b658`, `06b6d41b` and `171029c7`, each in one
session of the shared build lock: formatting, vet, vet with the release tags for the six
release targets and for linux/386 and linux/arm, the docs' budgets, the source guards by name,
the bar's Go tests (`player_output_bar_test.go` and `player_touch_layout_test.go`) under the
race detector ten times idle and ten times beside four busy processes, the whole race-enabled
suite and the cross-builds. Their results are in the PR's description.

After it, on `6ce45510`, each through the shared build lock: formatting (no file rewritten),
vet, vet with the release tags for the six release targets and for linux/386 and linux/arm,
and the docs' check, all clean; the whole race-enabled suite, ok (93 packages, `cmd/bridge` in
1,896 s and `internal/admin` in 1,547 s); the cross-builds of the six targets; and the bar's Go
tests (the 189 of `player_output_bar_test.go` and `player_touch_layout_test.go`) under the
race detector ten times idle, ok in 373 s, and ten times beside four busy processes, ok in
381 s. Outside the lock, the whole console package once without the race detector: ok in
308 s. These ran with this entry part-written, the code as it ships; on the commit that
carries this sentence the docs' check and the citation guard ran again.

### Left open

- No real iPhone or iPad was used: WebKit emulating touch is what was measured, and how a
  real iOS range input takes a tap is not shown here.
- The bar's height with the times' row, which is shorter than the 44px seek row the tables
  above measured, and the A–Z rail's letters beside it on a 568px phone (about 9px tall with
  that row, each taking a tap at its centre), not measured again.
- Playlists, mixes and favorites read the set of failed sources at their next render and do
  not redraw live, and `collectionActions` hides rather than disables where nothing plays:
  both for the next PR.
- The 834px tablet keeps the wider layout's 16px-tall seek, which a tap does not land, as on
  main, and its 32px and 40px bar buttons, the touch sizes the next PR takes up.
- For the touch seek's own item:
  - Wider than 768px a touch on the seek lands nothing in WebKit, as on main, the seek's code
    being main's byte for byte (the 834px tablet's tap at three quarters of a 20 s track sent no
    seek on main and one at `f1020508`).
  - Main's seek keeps a preview's thumb after an input with no change after it (a touch in
    WebKit) until a later change, and paints no position on the thumb meanwhile; the rail no
    longer follows it.
  - The three hazards above, the event order recorded in "Four more, and the last", and the
    pointer capture's finding under "Choices the brief left open".
  - No touch drag was measured in WebKit, whose touch input here takes taps alone; Chromium
    took one given to it as touch events. Whether an ordinary horizontal scrub on a phone ends
    in `pointercancel` is shown by no evidence here; an event trace of a finger's drag on a
    device would settle it.
  - A design that follows a pointer's lift must also meet a pointer whose `pointerup` and
    `pointercancel` both never reach the document (no browser measured does this): the
    retired design kept refusing input until that pointer went down and up again.
