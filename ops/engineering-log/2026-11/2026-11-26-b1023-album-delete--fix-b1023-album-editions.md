## 2026-11-26 — B1023: Delete is the album's again, one button through every pick

### Why

The two B1023 entries before this one built a Delete of the edition shown, and with it the reconciliation of the trash's answer it needed: paths held across redraws, the album read again after each answer, rows held off a page after an answer it could not read. Each change to it opened more of the same kind: settled paths still playable while the album was read again, a failed rollback taken back, a gateway's timeout read as nothing moved, a reload restoring the album-id send too early, a stale render after a change of output. What the owner asked for is the picker and Play, Shuffle and Add to queue on the edition shown, never a Delete of it. So Delete is the album's again, as it was before editions, and a Delete of the edition shown is a later item, with those hazards as its first cases.

### What was taken out

From `static/player/views.js`: the paths a trash request carried and their settlement (`trashing`, `settleTrash`), the count of answers and the album page they reached (`answers`, `albumShown`), the album read again after an answer and the rows it held off (`reread`, `keepOf`, `deletions`), the line for rows held off a page (`heldNote`), the regrouping a deletion made, and the deletion's half of when an album goes by its id. From the bar's harness: four cases, by what they pinned (an edition's Delete held across picks; a Delete held across a regrouping and a redraw; an album a deletion leaves with one edition; the trash's answer read back from the library), and the fixture support only they used (a held trash that dropped rows, a hook on the album's read, a refused answer).

### What Delete is now

`deleteButton(d.tracks, a.title, eds)` is main's `appendDeleteAction` made once a render, the settings read once, and moved by each paint into the row the paint draws, so a pick keeps its request and its answer on the one button and no second Delete is live beside one under way. It moves every file of every edition but routed tracks, which it says it leaves alone as before, and an SACD image's virtual tracks, which are no files the trash can move (left out silently, as since the first of these entries). Its confirmation keeps main's words and, where the files it moves are more than one edition's, says how many: `Move 30 files of 3 editions of "<album>" to the trash?`. Editions are counted by the files they hold, so an edition on an upstream or an SACD image's counts for none, and an album whose files are one edition's keeps main's words exactly. An album with no file the trash can move offers no Delete and reads no settings for one. Its answer is said on it, and the page stays as it was, as main's did.

### What stays as it was, and what follows from it

The editions, the picker, the default by output, Play, Shuffle and Add to queue on the edition shown, the oversized edition's refusal, the slot let go at any change of output, the count line and the disabled rule are unchanged. An edition still goes to the bridge's player as its paths, playback's scope and no part of deleting: the album's rows skip a row deleted or suppressed since the catalog was built, which its id still expands to, so an album of one edition goes by its id only where the catalog's count is its rows' (`a.trackCount === d.tracks.length`), and as its rows where the catalog counts more. A title's list and a row's Add are held to the bridge's player's bounds on every album page, an album sent by its id too: its toolbar stays live and its words (`ROW_WORDS`) send the reader to Play.

### Tests and their controls

The playback case keeps its six playback checks under a name without Delete, `TestPlayShuffleAndAddTakeTheEditionShown`; the SACD case's two Delete checks now ask the album's Delete, which stays with the image shown and moves the files alone, in main's words; the one-edition case's bridge fixture carries the album's track count, as the server's answer does, its checks as they were. New: `TestAnAlbumsDeleteMovesEveryEditionsFiles` (8 checks: the request and the words on three editions, a pick while the answer is held, the answer and one settings read, the page as it was, an album of local files beside an upstream's and an image's, one of one local edition beside an upstream's, one with no file to move), `TestAnAlbumTheCatalogCountsMoreOfGoesAsTheRowsItShows` (3) and `TestAnAlbumSentByItsIdHoldsItsRowsToTheBridgesBounds` (6). The harness held 200 scenarios before and 199 after, every one green.

The tests went in first (d7da1bdb), and that commit's tree was run: 15 checks red in four scenarios. Green there, as they should be: the id sends of the two new id cases and their row Adds, and every kept case.

Controls, each predicted in a file before it ran, each on a copy of `static/player`: 13, of which 12 turned exactly their predicted checks red and some more beside. One predicted check of the first (the edition shown's rows for Delete's) stayed green, since the album's Delete is made at render, while the files' edition is shown, before the case picks the image; that mutation turned five other checks red. The rest: no edition count in the words; every edition counted, files or none; an image's tracks sent; routed tracks sent; the settings read for an image alone; a Delete made again at every paint; the Delete left out of a repainted row; by id whatever the catalog counts; an album's rows not held to the bounds; an edition's words on an album sent by its id; that album refused on its toolbar; no words for it at all.

### WebKit

WebKit 27.2, headless, against two test bridges built with the Delete of the edition shown (6d0ee8f1) and with the album's Delete (39fdd405), each over a fresh copy of the first entry's generated library of invented names, with Delete allowed, in this browser, on the three-edition album; the trash request was held in the page's network, the bridge moving nothing of it until the run let it go.

- With the album's Delete: the confirmation read `Move 30 files of 3 editions of "Field Notes" to the trash?` and main's second line; the one request named 30 paths, every row of the album, in its three folders. With the CD rip picked meanwhile the one Delete read Deleting…, disabled, and a press sent nothing; let go, the bridge's answer was said on it (`30 moved to trash`) and the page stayed as it was, three editions, the CD rip's four rows shown.
- With the Delete of the edition shown: the confirmation named the FLAC 192/24 edition and its request 13 paths of one folder; with the CD rip picked the Delete was live and a press sent a second request, the CD rip's 4; after the answers the page was read again, one edition left, no picker.
- At both builds the album page read the console's settings twice after its fetch, one read Delete's and one the Variants tab's gear's (the album grid reads none).

The library, configs, scripts, results and screenshots are kept outside the repository.

### Runs

On ad962904, the code as it ships with its rule, in one session of the shared build lock: formatting (no file rewritten), the pinned gofmt, vet, vet with the release tags for the six release targets and for linux/386 and linux/arm, the docs' check, the console's source guards and the citation, docblock and keeper guards by name, the album and edition wrappers (27 of the harness's and the two Go tests) under the race detector ten times (ok, 98 s), the whole race-enabled suite (93 packages ok in 2,016 s: `cmd/bridge` 1,990 s, `internal/manifest` 1,655 s, `internal/admin` 1,540 s) and the cross-builds (24 s). The harness's 199 scenarios ran green with node on the path, and the controls over copies of the console's modules. The commit that adds this entry changes it alone; on it the docs' check, the citation guard and the harness ran again.

### Lines

Production: 49 lines added and 128 removed in `views.js` (33 of code, 16 of comment among those added), 79 net fewer; tests 176 lines added and 323 removed.

### What stays open

- A Delete of the edition shown, the later item, its first cases the hazards above.
- After Delete answers, the page keeps its rows, as before editions, a moved file's included, until it is drawn again.
