## 2026-11-26 — B1036: an album's Delete names every edition it moves

### Why

An album whose folders hold the same songs shows one edition at a time (B1023), and its Play, Shuffle and Add to queue take the edition shown. Its Delete stays the album's: it moves every file of every edition, never a routed track or an SACD image's, which are no files. At 386bcb6a its confirmation counted the editions and named none (`views.js`, `deleteButton`, 417 to 452: `k`, the editions holding the files, in `Move 6 files of 2 editions of "<album>" to the trash?`), so a reader who picked the CD rip and pressed Delete, meaning that rip, moved the DSD rip's files too. The files stay recoverable in the trash, but the trash retires their rows at once (`handlers_trash.go`, `retireAndRescan`), so every paired phone lost both editions until a restore and a rescan. The owner's decision of 2026-11-26 kept the album's Delete as it was, its files, its request and the server, and changed its confirmation alone.

### What the confirmation says now

On an album of two editions or more, the CD rip shown:

```text
Move 7 files from "Album" to the trash?

Whichever edition is shown, these files are moved:
DSD64, 4 tracks: 4 files
FLAC 44.1/16, 3 tracks: 3 files

They stay recoverable from the Library page until the trash is emptied.
```

Each edition whose files it moves is named by its own name, the picker's (`ed.name`, with its folder's words where another edition reads the same), and how many files it moves of it: an edition's files are those of its rows the trash is sent (`files.has`), so an edition of one local row and one routed row, `FLAC 44.1/16, 2 tracks, Mix`, reads `1 file`. It never says it deletes every edition, which an edition it leaves alone would make false; the edition shown can be one it leaves alone (an SACD image's, the one the bridge's player opens on). After the trash's sentence it names each edition it leaves alone, with why, and the upstream's sentence follows as before:

```text
Move 3 files from "Album" to the trash?

Whichever edition is shown, these files are moved:
FLAC 44.1/16, 3 tracks, Album [2004]: 3 files

They stay recoverable from the Library page until the trash is emptied.

Left alone:
FLAC 44.1/16, 3 tracks, Album: on an upstream server

3 tracks on an upstream server are left alone.
```

An edition with no file of its own is left alone: an SACD image's where any of its rows is an image's track (`imageOf`), read `an SACD image, whose tracks are not files`, else an upstream's, `on an upstream server`. The first line no longer counts editions, which the list names. An album of one edition keeps its words; where its rows hold an SACD image's tracks beside its files it says so after the upstream's sentence, as that one says its routed tracks:

```text
Move 1 file from "Album" to the trash?

They stay recoverable from the Library page until the trash is emptied.

1 track on an upstream server is left alone.

2 tracks of an SACD image are left alone: they are not files.
```

One track reads `1 track of an SACD image is left alone: it is not a file.` Which files are moved, the request, the server and how an album's rows make editions are unchanged.

### Tests and their controls

New in the bar's harness, `TestAnAlbumsDeleteNamesEachEditionItMoves` (10 checks): two rips on the bridge's player, which opens on the DSD rip, the CD rip picked before Delete is pressed (pressed, its rows alone shown, nothing asked or sent yet), the confirmation naming both rips with their files whichever edition is shown, and the request; an edition of one local and one routed row beside an edition alike but for its folder, both named in full in the picker (`FLAC 44.1/16, 2 tracks, Mix`, `FLAC 44.1/16, 2 tracks, Solo`), the mixed one with `1 file`, the upstream's sentence, the local paths alone sent; an album of one edition with an SACD image's track beside its files, and one with an upstream's track and two of an image's, their words and their requests. Changed: `TestAnAlbumsDeleteMovesEveryEditionsFiles` (8 checks to 10), the three editions' words, and its two cases with an upstream's edition, each now its words and its request apart, the image's and the upstream's editions named as left alone; `TestAnSACDImageIsAnEditionOfItsOwn` (5 to 6), the image shown, the files' edition named and the image's left alone, its words and its request apart; `TestAnAlbumOfOneEditionDrawsAsItDid` (21 to 23), its Delete called off sending nothing, then taken asking the same words and sending the album's three paths once, in order. Every case asserts the paths the trash is sent, in order. The harness held 221 scenarios before and 222 after, every one green.

The tests went in first (77166794) and were run over that commit's own modules: 8 checks red in three scenarios, each a confirmation's words (the two rips, the image's edition, the three editions, the two cases with an upstream's edition, the mixed edition, and both albums of one edition with an image's tracks); green there, as they should be, every request's paths and the album of one edition's own words.

Controls, each predicted in a file before it ran, each an exact-once change of `views.js` in the working tree at e8c04b35, restored from the commit, the tree clean after each: 11, every one turning exactly its predicted checks of the four scenarios red.

- The old confirmation: the 8 red at the tests' commit.
- An album of one edition's first line saying `of 1 edition of`: its words, and both albums of one edition with an image's tracks; the second ask of the one called off and then taken stayed green, being the first's words again.
- The upstream's sentence dropped: the 4 confirmations that say it.
- The request sent without its first path: the 9 checks of a request's paths and the three editions' answer (`29 moved to trash`); sent reversed: the 8 checks of a request of two paths or more. Beside them, in the redraw scenarios, the first turned four answers red by their count, one fewer moved, and both the answer kept for the Delete a redraw builds, which is given only where its files are the ones asked.
- An edition's files counted by its rows: the mixed edition (`2 files`) and the editions of an image's or an upstream's, listed as moved.
- An edition's name without its folder's words: the picker's names of the two editions alike but for their folders, and the confirmations naming such editions.
- An edition of upstream tracks alone listed as moved: the two confirmations with one.
- The editions left alone not named: the three confirmations that name one.
- An album of one edition's image sentence dropped: its two cases.
- The list's heading without `Whichever edition is shown`: the six confirmations of an album in editions.

### Runs

On e8c04b35, in one session of the shared build lock: formatting (no file rewritten), the static checks, the docs' check, and the album, edition and Delete wrappers (22 of the harness's) with the two edition Go tests and three of the console's source guards (`TestAppJSHasNoCallsToDeletedHelpers` among them) under the race detector once (27 tests, ok, 28.5 s). Outside it: the whole `internal/admin` package without the race detector, node on the path (ok, 386 s), cmd/bridge's citation, docblock and keeper guards, and the harness's 222 scenarios. The commit that adds this entry changes it alone; on it the docs' check and the citation guard ran again.

### Lines

Production: 23 lines added and 9 removed in `views.js`, 13 of code and 10 of comment among those added. Tests: 128 lines added and 27 removed.

### What stays open

- A confirmation names each edition on a line of its own, so an album of many editions asks a long question.
- An album with no title is named `""` in it, as before.
