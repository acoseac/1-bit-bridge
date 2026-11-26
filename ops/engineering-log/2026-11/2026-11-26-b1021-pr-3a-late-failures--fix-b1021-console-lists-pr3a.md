## 2026-11-26 — B1021 PR 3a: a late failure, an aborted body read and an album's source names, on a page its route left

### What

Read line by line at `5c6107b2` (the list work with `main`'s album editions merged in) before
anything changed. Each held:

1. **A late failure painted on a page its route had left.** The catches that clear a view and paint
   `errorState` asked `isAborted` alone (`views.js`: the album's `:697-699`, the artist's
   `:1322-1324`, the favorites' `:1846-1848`, a playlist's and a smart mix's in
   `renderCollectionDetail` `:2443-2445`, a folder's `:2731-2733`, a key's tracks' `:2983-2985`, a
   search's `:3040-3042`), and the smart mixes' its generation too (`:2176-2178`). A boosted
   navigation to an operator page (`app.js:9871-9939`) aborts no read of the player's and moves no
   generation, so a 500 that came after it cleared the page's view, out of the document now, and
   painted its error there; and a render of the same entry that a newer one replaced, which a
   refined search draws on the entry and the view it keeps (`boot.js:281-284`, `:665`), painted its
   failure over the newer results wherever that failure was no abort (item 2 made one of an abort).
   The list's two paging catches already ask `stale(mine)`, the generations and ownership, before
   they clear the body (`views.js:1592-1593`, `:1703`, `:1747`): no change there.
2. **An abort that ended an error's body read was read as the error.** `errorFrom`'s catch
   (`api.js:61-72`), whose one caller is `getJSON` (`:54`), swallowed every failure of the body's
   read, so a 404 or a 500 whose body was still being read when `abortReads` (`:96-99`, every
   route's, `boot.js:720`) or a second request under its key (`api.js:44-46`) aborted it rejected
   with a plain `Error` and its status, which `isAborted` (`:102-104`) does not match.
3. **An album's source names, come after its page was left, drew it all the same.** `main`'s album
   page asks its source names where one edition holds rows of more than one source, and asked only
   its generation once they came (`views.js:748-749`). Past a boost, which moves none, the stale
   render subscribed to the player (`:755-763`), wrote the edition slot the album pages share
   (`setup`, `:814-815`) and painted its detail into the view out of the document (`:826`, `:833`);
   another album in editions, opened after, showed its default edition in place of the one picked.
4. **Three small ones.** The no-match Clear (`views.js:1803-1808`) was retired by a filter's commit
   alone (`whileDrawn`): a genre opened by Forward keeps the genres' list in the page while it looks
   its name up (`renderAxisAlbums` clears nothing first), and the old Clear emptied the field and
   took the focus, though its commit stopped at its route (`:1777`). The filtered-jump check
   (`list_harness.mjs:1293-1297`) pressed Back to the top whatever the jump had done, and the
   model's click fires a hidden button (`navigate`, `:602`), so a jump that did nothing passed. And
   the earlier record of the WebKit runs named a fixture composer.

### The changes

- **A failure paints only where its render was not overtaken** (`views.js`): `overtaken(ctx, at)`,
  the route's generation moved since the request was asked or the page no longer the route's
  (`owned`), is asked beside `isAborted` by each of the eight catches above, each with the
  generation taken before its request (the favorites, a folder and a search now take their ctx
  whole). A boost moves no generation and a refined search keeps its ownership, so each half alone
  misses one.
- **An abort stays an abort** (`api.js`): `errorFrom` rethrows an abort that ends the body's read;
  a body that is no JSON keeps the status and the status's words, as before.
- **The album page asks ownership past its source names** (`views.js`), beside its generation,
  before it subscribes, keeps an edition or paints.
- **The old Clear acts only while its route owns the page** (`views.js`): `gone()` first.

Production lines since `5c6107b2`: 36 added (26 code, 9 comment, 1 blank) and 15 removed, in
`views.js` and `api.js`.

### Tests

The page model (`testdata/list_harness.mjs`) answers a search's albums and an album's rows where a
case sets them, and holds a request deaf to its abort (`hold(path, when, deaf)`), which stands in
for a failure that is no abort. New scenarios, each a Go test of its name:

- `TestALateFailurePaintsOnlyThePageThatAsked` (app.js's own boost router): an album, an artist, a
  playlist, a smart mix, the smart mixes, the favorites, a folder, a key's tracks and a search, each
  request held and then answering 500: let go while the page is its route's, the error shows; let
  go once an operator page replaced the page, the page's view, out of the document, keeps the very
  spinner it had and shows no error (item 1).
- `TestAFailureOfASupersededRenderLeavesTheNewerResults`: a search refined on its own entry, the
  first one's request held deaf to its abort and answering 500 once the refined results stand:
  they stand still (item 1).
- `TestAnAbortedBodyReadStaysAnAbort`: `getJSON` under a key, a 404 whose `json()` has begun when
  `abortReads` aborts the request's signal, rejects with an abort; a 404 whose body is no JSON, with
  no abort, keeps its status and "404 Not Found" (item 2).
- `TestAnAlbumLeftWhileItsSourceNamesAreHeldDoesNothingOnceTheyCome` (app.js's own boost router):
  an album in two editions, the other one picked; an unscoped album whose one edition holds this
  library's rows and an upstream's, a fresh source-name lookup held once its heading is drawn, an
  operator page in its place, the names let go: nothing drawn on its view, no redraw once a row it
  would play here is marked undecodable and the player changes (the subscription's one effect), and
  the first album, opened again, shows the edition picked (item 3).

Extended: `TestAListLeftForAnAxisPageAsksNothingWhileItsNameIsHeld` filters the genres to nothing,
goes Forward to a genre with its name held, puts the focus on the page-top search and presses the
old Clear: the field keeps "zzzz" and the focus stays where it was, and the genre's grid opens once
its name comes; `TestNoCallbackOfAListAFilterReplacedActs` asserts that a jump to M in the filtered
list puts its first M row first and shows its own Back to the top before pressing it (item 4).

Over `5c6107b2`'s player modules exactly these are red, the rest green: item 1's nine pages after a
boost (each view's spinner replaced by "Something went wrong / The list could not be read.", the
smart mixes' with its Try again) and the superseded search (the error over the refined results);
item 2's abort (an `Error`, "404 Not Found", status 404); item 3's three (two nodes drawn on the
view, one redraw, "FLAC 96/24 · 3" shown over the picked "FLAC 44.1/16 · 3"); item 4's Clear (the
field emptied and focused). The failures while a page is its route's, the body that is no JSON and
the filtered jump's new checks are green there.

**Negative controls**, each predicted in its runner before the run, on copies of the committed
modules with every list scenario run over each, seventeen in all and each red as predicted: each of
the eight catches put back as it was (its page's check; a search's also the superseded search's),
`overtaken` by ownership alone (the superseded search alone), by the generation alone (the nine
pages after a boost), every catch as it was (all ten), the abort swallowed again (its check), every
body failure rethrown (the body that is no JSON), the album's names by the generation alone (its
three), the old Clear unguarded (its check), and a filtered jump that does nothing (the two new
checks), which over `5c6107b2`'s own harness leaves every scenario green.

**Counts**: the list harness, 19 scenarios before and 23 after, all green; the bar's harness
unchanged, 214.

### The rule

The list entry's bold sentence, in its line in `admin-console.md` and its long form alike: an
answer neither subscribes nor keeps an edition for a page its route no longer owns, a failure paints
neither there nor over a newer render of its entry, a list's Clear does not act there, and an abort
that ends an error's body is not read as a failure. The long form says how and names the four new
tests.

### WebKit

WebKit 27.2, headless, on two test bridges on loopback over the generated library the first record
describes (440 albums, 4 playlists): `5c6107b2` and `51a61f6c`, the server alike at both. Contexts: a
1280×800 window by mouse, and an iPhone 14 (390×664) by touch, its coarse pointer simulated by the
touch context. A request was held by the browser until let go, then answered 500 or let through;
item 3's two albums were answers the browser served for two of the library's albums, which holds no
upstream's rows and no album in two editions. Two passes at each build; every named check agreed
across them, and no page error.

- **Item 1.** A playlist's page and the key page (`/tracks?camelot=8A`), each request held while
  Stats replaced the page, then answered 500. Base: the old view's spinner replaced by "Something
  went wrong" and the 500's message, in both contexts. Head: the same spinner, the view's only
  child, and no error. Answered 500 while the page was its route's, the error shows at both builds.
  A refined search ("ar", then "art"), the first request answered 500 once the refined results
  stood: at both builds the page had already aborted it (its own record: AbortError) and the
  results stood. In a real engine the refinement's own abort ends the first request, so the
  generation's half is shown by the harness's deaf hold alone.
- **Item 2.** A same-origin test page serving each build's own `api.js`, and a 404 whose body stalls
  after 16 of its 4,096 bytes: `getJSON` under a key, `abortReads()` once the body's read had begun
  (10 to 12 ms after the call). The engine rejects that read with AbortError ("Fetch is aborted") at
  both builds. Base: `getJSON` rejected with an `Error`, "404 Not Found", status 404, which
  `isAborted` does not match. Head: with the engine's AbortError (a DOMException, code 20). A 404
  whose body is no JSON, read whole: "404 Not Found", status 404, at both.
- **Item 3.** An album in two editions (96 kHz/24 and 44.1 kHz/16, the second picked) and an album
  whose one folder holds two local rows and two of an upstream's, its source names held once its
  heading was drawn, Stats in its place, the names let go. Base: the view out of the document drew
  the album's detail (its tabs, its source filter, four rows), and a redraw came once a row it would
  play was marked undecodable and the player changed. Head: nothing drawn (the view stays empty, as
  it was), no redraw; a live album page measured the same way redraws once at both builds, so the
  measurement reaches a subscription. Back into the player through Browse, the first album shows the
  edition picked at the head and its default at the base. By Back, which lands on the mixed album's
  own entry and draws it live, the first album shows its default at both builds: that live page
  takes the one slot the album pages share, as the slot's rule says another album does.

### Runs

On `a35f176a`, in one build-slot session (13:35 to 14:28): cmd/bridge's doc guards
(`TestNoDocblockNamesAnotherDeclaration`, `TestEveryCitedTestNameExists`, `TestNoBlankKeepers`),
`make fmt` (the tree unchanged after it), `make vet`, `GOOS=linux GOARCH=arm make vet`,
`GOOS=linux GOARCH=386 make vet` and `GOOS=windows GOARCH=amd64 go vet -tags
ts_omit_webclient,ts_omit_debugeventbus ./...`, `make docs-check`, and the console's 19 guards and
pins by name, as the earlier record lists them, passed; every wrapper of the two harnesses (237: the
bar's 214 and the list's 23) under `-race -count=10`, first with no busy process of the session's
own, 2,370 of 2,370 in 524 s, then beside four busy processes, 2,370 of 2,370 in 476 s, none skipped
in either (the browser runs above shared the machine for the first minute or so of the first);
`make test`, 93 packages ok in 2,113 s (cmd/bridge 1,975.7 s, internal/manifest 1,631.4 s,
internal/admin 1,530.9 s); `make build-all`, the six targets. The record's commit changes no code:
on it, `make docs-check` and cmd/bridge's doc guards again.

### Left open

The earlier records' residuals stand. A late answer, not a failure, of the favorites, a folder or a
search after a boost still draws into the view out of the document: it retitles and scrolls
nothing (those pages title themselves before their request or not at all) and no later page shows
that view.
