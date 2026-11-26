## 2026-11-25 — B1021 PR 2: the console sized by the pointer, its reasons in words a finger reaches, and the bar's cover with the bridge's player

### What

At the base (`8ef04ab3`, B1021 PR 1 merged), in WebKit and in its code:

1. With the bridge's player chosen the bar painted no cover, the owner's report: a DSD64 track
   playing on a laptop, the bar's square empty while the album page showed the cover. In this
   browser, once it took the bridge's queue, the bar painted the queue's one cover: handed a
   FLAC album after playing another, it showed the other album's cover, after the take and
   after a reload, and handed an album with no cover, the same.
2. A tablet and a touch laptop met a mouse's sizes. By touch at 834×1194: the bar's buttons 40
   and 32px, 2px apart; the output menu's picks 39px; the queue panel's entries 33px, 1.9px
   apart, each Remove 59×26px; the sections 34.9px and the crumbs 25.4px; a button 32px, a
   select 31px, a switch's row 22px, a disclosure row 21px, the gear 32px, a radio 12px. The
   bar's one row from 769px did not hold its controls: at 834, 844 (a phone held sideways) and
   1194px by touch the DSD chip lay over Stop and Repeat, and the words had 68, 70.5 and 96px.
3. The bar's reasons lived in titles alone: Shuffle, Repeat, Mute, the volume and the seek with
   the bridge's player chosen, a queue entry while its queue was read again, and the DAC's
   details on the chip, which is hidden up to 768px.
4. A phone's text: tokens of 13.5, 12.5 and 11.5px, a hint's and a button's text 12px, the
   intros 12.5px, a chip 11.5px.
5. Below 1024px the tablet band was 220px wide where the content began at 216px.
6. Settings ran 77, 107 and 147px sideways at 390, 360 and 320px, and the duplicates' page 7px
   at 320px; at 390px the stat cards stopped 38px short of the page.
7. The empty strip in this browser offered Mute, and on a laptop the volume, with nothing to
   mute; the section strip scrolled with no sign of it; a hover's paint stuck after a tap.
8. In this browser a render painted neither the times nor the rail, which the next tick did,
   up to 500 ms later; and a track that cannot seek kept the previous track's fetched band.

A tap at the centre of each disabled bar control, with the bridge's player chosen, fired
nothing in any coarse context measured, at the base and after.

### The changes

1. **The cover.** A catalog row (`playerTrackDTO`) gains `artworkMBID`, a pointer, and
   `artworkVersion`, the album DTO's names, so `coverURL` takes a row as it takes an album.
   `hydrateByPath` alone fills them (the bridge's queue, a read back's handover, a switch-out's
   answer), from the album the catalog files the track under, the cover the album page draws;
   an album with no cover is an empty `artworkMBID` the row carries, and every other answer
   leaves both out as before. One painter (`paintArt`) draws both outputs: the bridge's heard
   entry's cover, and in this browser a row's own cover where the row states one, an empty one
   included, else the queue's. It replaces the image only when its URL changes, moves the
   link's destination and name with the album whatever the image does (a local cover is named
   by its bytes, so two albums can share one URL), gives an image that fails to load way to the
   pages' mark and asks for it no more while its URL holds, and ignores the failure of an image
   the bar has moved past. With a track and no cover the square draws the mark (♪, held inside
   it by `.np-art`'s position and sized for its box), and with no track it stays empty.
2. **Touch sizes by the pointer.** One `(any-pointer: coarse)` block in each stylesheet, at
   every width: in player.css the bar's buttons 44px with the transport's 8px apart, the output
   menu's picks, the queue panel's entries (44px, 8px apart) and each Remove (44px), and the
   sections and crumbs (44px, their words centred by `align-content`, which keeps a crumb's
   ellipsis); in app.css a button, a select, a switch's row and a disclosure row 44px, an
   `<a class="btn">` an inline-block, the gear 44px, a radio's glyph 24px in a 44px label row,
   and the Player page's DAC radio, which no label holds, a 44px target itself. From 641px a
   coarse pointer's bar takes rows (`.np-center`'s contents placed by grid areas): three where
   the window is taller than 500px (the words beside the extras, the transport, the times) and
   two where it is not, its toggles and chip hidden there. The floors main sets by width stay.
   The hover paints of the bar, its menu and queue panel, the sections and the crumbs wait for
   `(hover: hover)`. The output menu's children keep their height and the menu scrolls inside
   (the section "Found while measuring").
3. **The reasons in words.** The output menu holds a line of what the bridge's player does not
   do (`#np-bridge-limits`, `WORDS`' own sentences), shown while the player is offered;
   Shuffle, Repeat, Mute, the volume and the seek name it (`aria-describedby`) while the
   bridge's player is chosen, and none does once this browser is the output; their titles and
   `disabled` stay. The queue panel says `WORDS.queueBehind` at its top while its queue is read
   again, each entry it disables naming the line. The bridge's pick holds a line of its own for
   the DAC while the player plays ("What the DAC is handed:", `dacDetails`), the chip keeping
   its title.
4. **A phone's pages.** Up to 640px, whatever the pointer, the tokens are 15, 14 and 12.5px, a
   hint's and a button's text 14px, a chip 12.5px, and the intros keep the body's size. Below
   1024px `--shell-gutter` is 20px, 16px up to 520px, and main's side padding reads it. Settings'
   one column and Jobs' cards take `minmax(0, 1fr)`, Settings' selects their column's width at
   most; up to 640px the stat cards take one full column, a switch's badges a line under its
   words, and the duplicates' tier table scrolls inside its panel.
5. **The cosmetics.** The empty strip in this browser draws neither Mute nor the volume; up to
   768px the section strip fades at its trailing edge over a 48px spacer.
6. **The render's paint.** One function paints this browser's times, rail and fetched band
   (`paintBrowserTimes`), at every tick and at every render, setting the seek's value only while
   no scrub holds it and never painting the rail from a preview; a track whose length is not in,
   or which cannot seek, shows no band.

Production: 271 lines added and 78 removed in player.css, app.css, nowplaying.js and the two
handlers, 147 of the added lines code (79 CSS declarations, 69 lines of JavaScript, 4 of Go).
No route, no `/v1` answer and none of audio.js, output.js, format.js, views.js, ui.js, boot.js,
app.js or a template changes.

### Choices

- The rows that carry a cover are `hydrateByPath`'s alone. A handover's rows carry an explicit
  empty cover where the album has none, so this browser shows the mark at once and after a
  reload, with audio.js unchanged; every other answer (an album, a playlist, a mix, favorites)
  is as it was, so this browser keeps one cover for a queue it builds itself.
- The bridge's cover stays hidden up to 640px, its width the words'.
- A square with a track and no cover draws the pages' mark; with no track it is empty.
- The query is `(any-pointer: coarse)`: a touch laptop's main pointer is its touchpad, which
  `(pointer: coarse)` reads alone. The floors main sets by width are kept beside it, so a fine
  pointer measures the base's sizes but for a phone's text.
- The bar's rows under a coarse pointer from 641px, three where the window is taller than
  500px and two where it is not, as above.
- The disabled controls keep `disabled` and their titles; their words are the menu's line and
  `aria-describedby`. An `aria-disabled` control whose tap writes its reason was rejected: it
  changes what the disabled controls' checks read.
- Radios: a 24px glyph in a 44px label row, and the label-less DAC radio a 44px target.
- Left to their own items: the album page's track rows, header and actions, with the album's
  and the collections' Shuffle reason (B1023's); the A–Z rail, the lists' rows and tiles and the
  album grid's toolbar (the next console PR's); the coarse floors on `.btn` and `select` reach
  the album page's buttons and the toolbar's selects by class and element, as expected; PR 1's
  two items for playlists, mixes and favorites; player.css's metadata sized in rem literals.
- Hover: the bar's, its panels', the sections' and the crumbs' paints; the tiles', the rows'
  and the track rows' are their items'.
- The tests: the source pins in a file of their own (`console_touch_pointer_test.go`), the
  harness's cases after `TestTheRailFollowsTheTrackPastAPreview`.

### Found while measuring

- With Settings' column at `minmax(0, 1fr)` the page still ran sideways in WebKit: a select's
  longest option made it 426px wide, and a switch's badges shared its words' row; with the
  larger text the duplicates' tier table took that page from 7 to 34px sideways at 320px. The
  selects now take their column's width at most, the badges a line under the words, and the
  table scrolls inside its panel; the narrow pages' pin took these first (`9b1aae5f`).
- The output menu is a column the room above the bar caps, and the coarse floor on a pick
  stands in for a flex item's own minimum, its words' height: where the menu's words outgrew the
  room, the bridge's pick shrank toward 44px and its words ran over the menu's next line, by
  72px at 320×568 while a long title played, 61px at 568×320 and 55px at 844×390; with the
  player stopped they ran 9px and 7px below the pick's own box at those two. Each child of the
  menu keeps its height whatever the pointer (`.np-menu > *`,
  `flex-shrink: 0`) and the menu scrolls inside, as the base's did; the pin came first
  (`c3313bc6`), the fix after it (`fe2ac5e6`). A scan of every control a coarse floor reaches,
  for words below its own box, found none other: the bar with its menu and its queue panel open
  in the 13 contexts, 22 pages at 320, 360 and 390px, and the operator pages at 390, 834 and
  1194px by touch.

### Tests

The route (`handlers_player_control_test.go`):
`TestTheBridgesQueueAndItsHandoversCarryEachAlbumsCover`, the bridge's queue, a read back's
handover and a switch-out's answer carrying the cover of each row's album, the empty one for an
album with none, a row the catalog does not hold none, and the album detail's rows neither
field. The bar's harness (`bar_harness.mjs`), with their checks:
`TestTheBarShowsTheCoverOfTheEntryTheBridgePlays` 7,
`TestACoverThatFailsGivesWayToTheMarkAndOneLeftBehindChangesNothing` 3,
`TestThisBrowserShowsTheCoverOfTheTrackItTookFromTheBridge` 3,
`TestAHandedOverAlbumWithNoCoverKeepsTheMarkAfterAReload` 1,
`TestTheMenuSaysWhatTheBridgesPlayerDoesNotDoAndItsControlsNameIt` 4,
`TestTheQueuePanelSaysWhyItsEntriesWaitWhileItsQueueIsReadAgain` 3,
`TestTheMenuSaysWhatTheDACIsHandedWhileTheBridgePlays` 4 and
`TestARenderPaintsThisBrowsersTimesRailAndBandForTheTrackItShows` 6: the harness's 181 cases
become 189. The source pins (`console_touch_pointer_test.go`):
`TestEveryControlTakesAFingerWhereACoarsePointerIs`,
`TestTheOutputMenuScrollsRatherThanSqueezeAPick`, `TestTheBarTakesRowsForAFingerFrom641px`,
`TestTheBarAndTheSectionsHoverOnlyForAPointerThatHovers`, `TestAPhonesTextIsReadable`,
`TestTheTabletBandEndsWhereTheContentBegins`, `TestNoNarrowGridKeepsAMinContentTrack` and
`TestTheBarDrawsNothingToMuteAndTheSectionsSayTheyScroll`. Every scenario and pin on main
keeps its assertions; `TestThePhoneBarKeepsItsWordsWithTheBridgeChosen`'s comment says why the
bridge's cover is hidden up to 640px.

Red first, each committed before its change and run over the base's production files: at
`10bd3350` the route's test failed 9 assertions, the eight cases 20 of their 31 checks, and
seven pins 31 assertions (two at their first, no coarse block standing in either stylesheet);
the narrow pages' pin, extended at `9b1aae5f`, 6; the menu's pin, at `c3313bc6`, 1. The
harness, its wrappers and the route's test are those of `10bd3350`.

Controls, each a mutation of the committed tree with its red predicted in a file before the
run and the file restored from the commit after it, run over the 29 new and kept tests the
items touch: 27 of 27 as predicted at `f5f70d23`. The fill removed from the route; no cover
painted; the queue's cover taken over the row's; the image rebuilt at every render; the link
moved only with a new image; the menu's line left out; the seek not naming it; the controls
naming it after a switch to this browser; the queue panel's line shown while its queue is
current; the DAC's details written over the pick's line; the render's paint removed; the band
kept for a track that cannot seek; the queue's 8px removed; player.css's floors moved into a
width block; app.css's floor off `summary`; the DAC radio's 44px removed; a tall window's bar
in two rows; a short window's bar keeping its toggles; the picks' hover outside
`(hover: hover)`; a phone's `--text-sm` back to 12.5px; the intro shrunk again; main's padding a
number again; the stat cards' column removed; the selects' cap removed; the strip drawing the
volume; the fade's spacer removed; the square not positioned. Each went red exactly where its
prediction said, every other test of the set green. And 2 of 2 at `04c7c8b4` for the menu:
its rule removed, and its rule held to a coarse pointer's block, each red in its pin alone.

### WebKit, base and head

WebKit 27.2, headless, on two test bridges over a generated library of invented names (a FLAC
album and a DSD64 album with covers the scanner reads, among others), one at the base and one at
the head (`fe2ac5e6`, its production code what ships), one script measuring both. By touch:
320×568, 360×740, 390×664, 568×320, 844×390, 700×1000, 834×1194 and 1194×834; by mouse:
390×844, 834×1194, 1280×800 light and dark, and 1440×900. This browser played real audio; the
host has no DAC, so the bridge's player's state, queue and command answers came from fixtures,
the queue's rows the album detail's with, at the head, the cover fields `hydrateByPath` fills:
every number of the bridge's output below is of that simulated state.

- **Touch, by touch:** at the head no button of the bar under 44px in any coarse context (at
  the base 32 and 40px from 700px up), the transport's buttons 8px or more apart (2px), the
  picks 44px (39px), the queue's entries 44px tall and 8px apart (33px, 1.9px), each Remove
  44px tall (26px), the sections and crumbs 44px (34.9, 25.4px); the operator controls at
  390, 834 and 1194px: buttons, selects, switch rows, disclosure rows and the gear 44px, a
  radio's glyph 24px in a 44px row, the DAC radio 44px. No part of the bar over another and
  none clipped (the base's chip over Stop and Repeat at 834, 844 and 1194px gone), the last
  row, the open menu and the open queue panel clear of the bar and in the window. The bar 144px
  tall in three rows from 700 to 1194px and 92px in two at 844×390; the words' room at 834px
  304px in this browser and 249px with the bridge's player chosen (68px at the base), at
  1194px 400 and 345px (96px), at 700px 458 and 406px (242 and 160px), at 844×390 222 and
  118px (70.5px), and at phone widths the base's; with the player stopped over a queue the
  whole message fits at 834 and 1194px, where its second line was clipped, and has 118px at
  844×390 (70.5px).
- **By mouse:** the bar's buttons, the picks and the queue panel at 834, 1280 and 1440px, and the
  sections, the crumbs and the operator controls at 834 and 1280px, at the base's sizes (the
  bridge's pick taller by the DAC's line while it plays); at 390px a phone's larger text grows a
  button from 32 to 34px, a pick from 39 to 41px and a section from 34.9 to 37.3px.
- **The cover:** at 1280×800 dark and at 834×1194 by touch, with the bridge's player playing the
  DSD64 track, the bar's square shows the album page's cover URL, loaded (500px wide), linked to
  its album, where the base drew an empty square; in this browser both builds show its own. At
  390px the bridge's square is hidden, this browser's shown. Handed the FLAC album after a
  queue with another cover, this browser shows the FLAC album's cover after the take and after
  a reload (the base: the other's); handed an album with no cover, the mark (the base: the
  other's).
- **The pages:** no page of the audit's list runs sideways at 320, 360 or 390px (the base:
  Settings 147, 107 and 77px, the duplicates' page 7px at 320px), Settings and Jobs at 320px
  with a long string in their names included; the stat cards at 390px take the page's 358px
  (320px). At 390px the text is 15px, a hint and a button 14px, a chip 12.5px.
- **The band:** at 834 and 1000px by touch and at 834px by mouse it ends at 204px, the content
  beginning at 216px (220px at the base); from 1024px unchanged.
- **The strip and the menu:** up to 768px the section strip fades at its trailing edge; the
  menu's lines read at 12.5px up to 640px.

### Runs

On `e6361043`, in one session of the shared build lock: formatting with go.mod's toolchain, no
file rewritten; vet, and vet with the release tags for the six release targets and for
linux/386 and linux/arm, all clean; the whole race-enabled suite, ok (93 packages, `cmd/bridge`
in 1,977 s, `internal/manifest` in 1,625 s and `internal/admin` in 1,494 s, 2,124 s in all);
the cross-builds of the six targets; the docs' check; the source guards by name, the console's
seven (`TestPlayerEmittedClassesAreStyled`, `TestPlayerCSSDoesNotHijackOperatorTableClasses`,
`TestTrackSubgridColumnsMatchTheRow`, `TestNoConsoleControlDisablesItselfOutsideSetDisabled`,
`TestEveryPlayerControlStateFieldIsRenderedSomewhere`,
`TestThePhoneBarKeepsItsWordsWithTheBridgeChosen` and
`TestEveryNodeLookupOfTheOutputsTestsIsBarNode`) and cmd/bridge's four
(`TestNoDocblockNamesAnotherDeclaration`, `TestEveryCitedTestNameExists`, `TestNoBlankKeepers`
and `TestEveryBackgroundGoroutineDrainsOnCleanup`), all passing; the harness's 189 scenarios,
every one passing; and the bar's Go tests (the 190 of `player_output_bar_test.go`,
`player_touch_layout_test.go`'s, the new pins and the route's test, 206 in all) under the race
detector ten times idle, ok in 267 s, and ten times beside four busy processes, ok in 234 s.
These ran with this section not yet written, the code as it ships; on the commit that carries
it the docs' check and the citation guard ran again.

### Left open

- No real iPhone, iPad or touch laptop was used: WebKit emulating touch is what was measured.
- The touch seek, the tablet's 16px seek included, is an item of its own.
- This browser keeps one cover for a queue it builds itself (an album added to the queue, a
  queue added after Stop, favorites), and the lock screen's artwork is audio.js's: both left
  to a later item.
- The bar reads its queue again only when `queueRev` moves, so a cover the catalog gains
  while the same entry plays shows at the next read.
- A mouse at 769-1023px keeps the bar's one row: at 834px with the bridge's player playing the
  DSD chip lies over Stop and Repeat, as at the base, and the words have 72px.
- The album page's track rows, header and actions, the album's and the collections' Shuffle
  reason (B1023's); the A–Z rail, the lists' rows and tiles and the album grid's toolbar, which
  the coarse floors on `.btn` and `select` now reach (the next console PR's); playlists, mixes
  and favorites redrawing on a decode failure, and `collectionActions` hiding where nothing
  plays (PR 1's two items); player.css's metadata in rem literals on a phone.
