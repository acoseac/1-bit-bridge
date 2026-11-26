## 2026-11-26 — B1023: the album's Delete keeps its request through a redraw, and its confirmation counts editions

### Why

The album's one Delete was made once a render and moved by each pick into the repainted row, so a pick kept its request and its answer. A redraw did not: an output switch, a variant job's rerender, or a visit away and back draws the page again, and its new Delete read Delete… and was live while the first request was out, its answer landing on the button the redraw had taken away. What the code did at f073f0c7, by line: `views.js` 435 to 447, the answer of the button pressed, wrote its text alone on success (the button stayed disabled) and on a refusal enabled it again, put Delete… back and alerted; neither routed. Main's `appendDeleteAction` (main's `views.js` 436 to 449) did the same on the button it made. The album's key is the route's id, the catalog's album identity, which its editions drawn afresh from a later fetch (`albumEditions` runs on each fetch's rows) do not change. The pending code at 0874cdd6 (`views.js` 410 to 480 and 690 to 700: a set of paths in flight, a map of deletions by album and path, a count of answers and the page shown, whose callbacks held the render's nodes, with a read of the album again to reconcile) was more than this change's budget of one record and a handler holding no node; only its idea, the button's words read at build, carries.

### What the Delete does now

One map by album id (`trashing`) holds the paths asked, the render of the album shown now (`at`, which each render of the album takes at its start), and an answer kept for that render's Delete (`said`). A Delete built while its album's request is out reads Deleting… and is disabled, so a pick and a redraw show the request alike and no second Delete is live beside it. The answer (`trashAlbum`) holds no node: it goes on the album's Delete shown when it lands, found by its id (`album-delete`) and its album; else, where the album's render shown then has not built its Delete yet (its settings still out), to the Delete that render builds, if its files are the ones asked; else nowhere, an answer for an album not shown changing nothing on the page or on that album's old Delete. Each album's request is its own and each answer clears only its own entry; a later render of the album builds a fresh Delete, and drops an answer kept for a render that built none. A refusal gives the Delete shown back and is said wherever the reader is, as before; an answer whose every file failed says so on the Delete, disabled, as before. A redraw where deleting is off, or with no file to move, offers no Delete and reads no settings for none, as before.

### The confirmation

On an album of two editions or more the confirmation says how many editions hold the files, `of 1 edition of` where one does (one edition's files beside an edition on an upstream, an SACD image's files); an album of one edition keeps main's words. Before, the count showed only past one edition, so those albums read as albums of one.

### The trash under two requests

Two presses can send one album's files twice at once, and the trash takes each request on its own. `TestTwoTrashesOfOneListMoveEachFileOnce` holds the first request at its root record once its first file has moved, through the existing seam, while the second runs the whole list: each file is moved by one request and refused by the other, each result's `Paths` (what the handler retires, whatever the counts) holds exactly the files that request moved, and every file restores with its own bytes, its root record naming its own path. One case gives each request a stamp of its own, the other gives both one nanosecond, so both move files into one stamp's folder. No production seam was added.

### Tests and their controls

New in the bar's harness: `TestAnAlbumsDeleteKeepsItsRequestThroughARedraw` (7 checks: an output switch with the request held, the answer said on the redraw's Delete, the redraw after it, an answer while a redraw waits for its settings, a redraw where deleting is off or with no file to move, the redraw after an answer no Delete took, an answer kept for a redraw whose files differ), `TestAnAlbumsDeleteAnswerIsSaidOnTheDeleteShown` (4: the redraw's Delete disabled while the request is out, a refusal giving it back, said once, a press then sending one new request, an answer whose every file failed) and `TestEachAlbumsDeleteKeepsItsOwnRequest` (7: one view taking two albums in turn, each a generation of its own, both requests out). The held pick case asks for exactly one Delete; the one-edition-beside-an-upstream case and the SACD case ask for `of 1 edition of`; the SACD case opens on the image, the edition the bridge's player plays best, and asks for its Delete before any pick; the playback case's bridge fixture carries the album's 30 rows as its count. The harness's page finds an element by id under its body, as a browser's does. The harness held 199 scenarios before and 202 after, every one green.

The checks went in first (4f68f4aa, and ebe72076 for three more) and were run against `views.js` at f073f0c7: 12 checks red in five scenarios, each for its case (the redraw's Delete live, the answer on the button taken away, the edition count missing); green there, as they should be, the refusal's and the retry's checks, which pass where the redraw's Delete was live all along, and the fresh Deletes after an answer. The playback case is green at f073f0c7 and on the fix.

Controls, each predicted in a file before it ran, each on a copy of `static/player`: 12, every one turning exactly its predicted checks red, two with more beside that follow from the predicted ones. A redraw's Delete reading no request (9 checks); an answer on any album's Delete shown; no answer kept for the Delete a render builds; a render taking no request at its start; an answer clearing every album's request; a kept answer outliving its render; a kept answer going to a Delete of other files; the edition count only past one edition; an edition count on an album of one; editions always plural; Delete made from the edition shown's rows, which the SACD case, opening on the image, now turns red; and an album in editions sent by its id where the catalog counts its rows, which the playback case's count now turns red. The trash test's control, the stat failure branch adding its missing source to `Paths`, turned both cases red on both requests' `Paths` alone, through an overlay of the package, the tree untouched.

### WebKit

WebKit 27.2, headless, against two test bridges built at f073f0c7 and with this change (ebe72076), each over a fresh copy of the first entry's generated library of invented names, with Delete allowed, on the three-edition album. The trash request was held in the page's network, the bridge moving nothing of it until the run let it go; the output switch was a frame offering the bridge's player, the output picked, which is fixtures here (simulated: this host has no DAC), and the album was drawn again for it, its DSD64 edition shown.

- At f073f0c7: pressed, the Delete read Deleting…; after the switch the redraw's Delete read Delete…, live, and a press sent a second request for the same 30 files, confirmed a second time; let go, that Delete read its own request's answer, `30 moved to trash`, the first request's answer landing on no button shown.
- With this change: after the switch the page held one Delete, Deleting…, disabled, and a press sent nothing; let go, the bridge's answer was said on it, `30 moved to trash`, one request in all.
- At both, the confirmation read `Move 30 files of 3 editions of "Field Notes" to the trash?` and main's second line.

The library, configs, scripts, results and screenshots are kept outside the repository.

### Runs

On ebe72076, the code as it ships with its rule, in one session of the shared build lock: formatting (no file rewritten), the pinned formatter's check, the static checks, the static checks with the release tags for the six release targets and for linux/386 and linux/arm, the docs' check, the console's source guards and the citation, docblock and keeper guards by name, the album and edition wrappers (30 of the harness's and two Go tests) under the race detector ten times with the host idle (ok, 112 s) and ten times beside four busy processes (ok, 102 s), the trash test under the race detector twenty times (40 cases, ok), the whole race-enabled suite (93 packages ok in 2,018 s: `cmd/bridge` 1,992 s, `internal/manifest` 1,656 s, `internal/admin` 1,547 s) and the cross-builds (24 s). The harness's 202 scenarios ran green, and the controls over copies of the console's modules. The commit that adds this entry changes it alone; on it the docs' check, the citation guard and the harness ran again.

### Lines

Production: 63 lines added and 19 removed in `views.js` (45 of code, 18 of comment among those added); tests 433 lines added and 22 removed, 156 of them the trash test.

### What stays open

- An answer kept for a render that builds no Delete (deleting turned off, its settings failed) stays in the map until the album's next render, one entry an album.
- After Delete answers, the page keeps its rows until it is drawn again, as before editions.
