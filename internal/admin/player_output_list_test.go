package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The player's lists keep the reader's place and take a name filter (B1021
// PR 3a): the shipped boot.js, booting the player as a page does, and
// views.js's lists, under node over testdata/list_harness.mjs's page model,
// case by case, as the bar's cases run (harnessScenario). A host without
// node skips these cases; under CI they fail without it.

// listScenario runs the list harness's scenario named as the calling test
// is, with args after it.
func listScenario(t *testing.T, checks int, args ...string) {
	t.Helper()
	harnessScenario(t, "list_harness.mjs", checks, args...)
}

// TestBackReturnsTheAlbumListToItsPlace: Albums by Recently Added scrolled to
// row 150, a tile opened, Back: the window starts at row 0, rows 0 to 150
// are loaded in pages of 200, and row 150 is the top row again at its delta,
// after one scroll; a place at row 199 with more rows behind it stands once
// the observer has paged on; a place 1,200 rows in comes back with its
// window re-based at its page, and Back to the top reaches row 0, whose list
// pages on past its first 60 rows.
func TestBackReturnsTheAlbumListToItsPlace(t *testing.T) { listScenario(t, 15) }

// TestBackReturnsTheArtistAndAxisListsToTheirPlace: the artists' grid, the
// genres' rows and the playlists, one page, each stand at their top row
// after a row opened and Back.
func TestBackReturnsTheArtistAndAxisListsToTheirPlace(t *testing.T) { listScenario(t, 5) }

// TestTheCrumbReturnsToTheEntryTheReaderCameFrom: the album's Albums crumb
// goes back one entry (history.go(-1), the length unchanged, Forward reaching
// the album) and the list stands at its place; from the artist its byline
// opens, two entries back; and after Back, a filter edited in place and
// Forward, the crumb pushes the URL it names.
func TestTheCrumbReturnsToTheEntryTheReaderCameFrom(t *testing.T) { listScenario(t, 7) }

// TestACrumbOutsideThisTabsHistoryPushesAsBefore: a structural crumb, a trail
// entry an older build recorded with no depth, and a source's root crumb
// that is no trail entry's URL each push, and their lists start at the top.
func TestACrumbOutsideThisTabsHistoryPushesAsBefore(t *testing.T) { listScenario(t, 6) }

// TestARailJumpIsTheWindowBackRestores: a jump to M, a scroll, a row opened,
// Back: the list's first row is M's first, the top row stands where it
// stood, and the restored window keeps its rail, from which A is reached,
// and A's window pages on past its first 60 rows.
func TestARailJumpIsTheWindowBackRestores(t *testing.T) { listScenario(t, 6) }

// TestAPlaceIsRestoredOnlyOnTheURLItWasMeasuredOn: after Back has restored
// the place, a sort change and then a filter change each start the list at
// its top.
func TestAPlaceIsRestoredOnlyOnTheURLItWasMeasuredOn(t *testing.T) { listScenario(t, 3) }

// TestAPlaceIsWrittenOnlyIntoItsOwnEntry: a write pending as Back comes lands
// nowhere (the entry Back reaches keeps its own place), and nothing is
// written into an entry while its list is still rendering.
func TestAPlaceIsWrittenOnlyIntoItsOwnEntry(t *testing.T) { listScenario(t, 4) }

// TestARedrawOfTheSameEntryKeepsTheListsPlace: the output's change, which
// routes the same entry again (boot.js's onOutput), right after a scroll
// whose write is still pending, keeps the list's top row where it stood.
func TestARedrawOfTheSameEntryKeepsTheListsPlace(t *testing.T) { listScenario(t, 3) }

// boostPrefix is what app.js's boost router reads at the top level besides
// its functions: its bindings, and the page init and the event stream's
// recycle, which the case needs nothing of (stubs). wireBoostRouter runs as
// app.js's DOMContentLoaded does.
const boostPrefix = `
const BOOST_ENABLED = true;
let pageAbort = null;
const pageLeaving = new EventTarget();
let lastBoostPath = location.pathname;
let boostLiveRegion = null;
let boostGen = 0;
function dispatchPageInit() {}
function recycleEventStream() {}
wireBoostRouter();
`

// TestBackFromAnOperatorPageReturnsTheListToItsPlace: a scrolled list, its
// write still pending, left by an operator link through app.js's own boost
// router (its entry pushed after a fetch, the shell swapped out, no
// pagehide) and Back, which swaps the shell in again: the list stands at its
// place.
func TestBackFromAnOperatorPageReturnsTheListToItsPlace(t *testing.T) {
	listScenario(t, 3, boostScriptFile(t))
}

// boostScriptFile writes app.js's boost router, assembled from the shipped
// app.js, to a file of the test's own for the list harness to run.
func boostScriptFile(t *testing.T) string {
	t.Helper()
	script := withReachedFunctions(t, boostPrefix, readConsoleJS(t, "static/app.js"), "wireBoostRouter")
	for _, fn := range []string{"function wireBoostRouter(", "function boostNavigate(", "function boostSwap(", "function boostIsPlayerPath("} {
		if !strings.Contains(script, fn) {
			t.Fatalf("%s is not in app.js: the case runs nothing of the shipped boost router", fn)
		}
	}
	path := filepath.Join(t.TempDir(), "boost.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTheFilterNarrowsTheListInPlace: three keystrokes make one request after
// the pause, with q; the same field keeps the focus and its value; q is
// written into the list's own entry; the count line, the rail and the words
// announced are the filtered first page's; Escape clears; a filter matching
// nothing says so, with Clear; a list opened on such a filter keeps its
// field; a genre's grid writes q alone into its URL; a redraw keeps the
// focus in the field; the playlists show the filtered collection; a source's
// banner leads out of the source with the filter kept; and a filter past the
// server's 256 bytes offers no Try again.
func TestTheFilterNarrowsTheListInPlace(t *testing.T) { listScenario(t, 16) }

// TestAFilterPauseWritesNothingAfterANavigation: a keystroke, then a row
// opened inside the pause: the album's URL and entry stay as the navigation
// left them, and no list is asked for.
func TestAFilterPauseWritesNothingAfterANavigation(t *testing.T) { listScenario(t, 2) }

// TestALateAnswerTouchesNoOperatorPage: through app.js's own boost router,
// an operator page replaces a list whose restore is held, and then a genre's
// albums whose name's lookup is held: neither answer, let go after, scrolls
// the operator page after its own scroll, nor retitles it.
func TestALateAnswerTouchesNoOperatorPage(t *testing.T) {
	listScenario(t, 5, boostScriptFile(t))
}

// TestAListWritesNoPlaceUntilALoadsRowsStand: a restore that fails leaves
// the entry's place as it was; its Try again, held at its second page, and a
// row of its first page opened keep that place; a retry that stands restores
// it, and a scroll after it records a new one; and a restore a jump
// superseded, left while the jump's rows are held, keeps its place.
func TestAListWritesNoPlaceUntilALoadsRowsStand(t *testing.T) { listScenario(t, 7) }

// TestAWholeCollectionRestoresFromItsFirstRowByItsRowsID: the playlists,
// 1,300 of them, come back to row 1,200 from their first row, asked for once,
// with no Back to the top; a place an earlier build re-based at 1,200 lands on
// its row and the next place starts at the first row; a playlist gone above
// the place's row is found by its id, and one whose row is gone lands at the
// index it was measured at; and Smart Mixes do the same.
func TestAWholeCollectionRestoresFromItsFirstRowByItsRowsID(t *testing.T) { listScenario(t, 8) }

// TestACrumbPushesWhereItsEntryNowHoldsAnotherURL: after Back, a filter
// that matches nothing (the list at its top, no write pending) and Forward,
// the album's crumb pushes; and so it does after a filter whose answer is
// still held when Forward comes the moment the URL moved.
func TestACrumbPushesWhereItsEntryNowHoldsAnotherURL(t *testing.T) { listScenario(t, 5) }

// TestACrumbReturnsToItsEntryAfterAReload: a reload on the album keeps what
// its crumb needs: the crumb goes back one entry, the list at its place,
// Forward reaching the album; and once the list's filter has moved its URL,
// Forward and a reload, the crumb pushes.
func TestACrumbReturnsToItsEntryAfterAReload(t *testing.T) { listScenario(t, 6) }

// TestNoCallbackOfAListAFilterReplacedActs: while a filter's rows are held,
// a letter of the rail drawn for the old q, the Try again of a restore that
// failed before it, a jump's Back to the top and the Clear of a filter that
// matched nothing ask for nothing (the Clear empties no field), and the
// filtered list stands from its first row, its count announced; a jump in
// that list puts M's first row first and shows its own Back to the top,
// which reaches the list's first row.
func TestNoCallbackOfAListAFilterReplacedActs(t *testing.T) { listScenario(t, 15) }

// TestAListLeftForAnAxisPageAsksNothingWhileItsNameIsHeld: a genre opened
// from the genres' rows, its name held, and the genres' list, still in the
// page, scrolled to its end; and a composer reached by Forward from a
// composers' list whose restore failed, its name held, and that list's Try
// again: neither list asks for anything, and each grid opens once its name
// comes; and the genres filtered to nothing, a genre reached by Forward, its
// name held, the focus on another control: the old Clear leaves the field's
// words and the focus where they were.
func TestAListLeftForAnAxisPageAsksNothingWhileItsNameIsHeld(t *testing.T) { listScenario(t, 11) }

// TestALateFailurePaintsOnlyThePageThatAsked: through app.js's own boost
// router, an album, an artist, a playlist, a smart mix, the smart mixes, the
// favorites, a folder, a key's tracks and a search, each one's request held
// and then answering 500: let go while the page is its route's, it shows the
// error; let go once an operator page has replaced the page, the page's view
// keeps the spinner it had and shows no error.
func TestALateFailurePaintsOnlyThePageThatAsked(t *testing.T) {
	listScenario(t, 18, boostScriptFile(t))
}

// TestAFailureOfASupersededRenderLeavesTheNewerResults: a search refined on
// its own history entry, the first one's request held where its abort
// reaches nothing and then answering 500 once the refined search's results
// stand: they stand still.
func TestAFailureOfASupersededRenderLeavesTheNewerResults(t *testing.T) { listScenario(t, 3) }

// TestAnAbortedBodyReadStaysAnAbort: api.js's getJSON over a 404 whose body
// is still being read when abortReads aborts its request rejects with an
// abort, and over a 404 whose body is no JSON, with no abort, with the
// status and the status's words.
func TestAnAbortedBodyReadStaysAnAbort(t *testing.T) { listScenario(t, 2) }

// TestAnAlbumLeftWhileItsSourceNamesAreHeldDoesNothingOnceTheyCome: through
// app.js's own boost router, an album whose one edition holds this library's
// rows and an upstream's, its heading drawn and its source names held while
// an operator page replaces it: the names, let go after, draw nothing on its
// page, subscribe nothing to the player, and leave another album's edition
// pick as it was.
func TestAnAlbumLeftWhileItsSourceNamesAreHeldDoesNothingOnceTheyCome(t *testing.T) {
	listScenario(t, 5, boostScriptFile(t))
}

// TestALateDetailAnswerRetitlesNoOperatorPage: through app.js's own boost
// router, an operator page replaces an album, an artist, a playlist, a smart
// mix and a key's tracks while each one's answer is held: let go after, none
// retitles or scrolls the operator page.
func TestALateDetailAnswerRetitlesNoOperatorPage(t *testing.T) {
	listScenario(t, 5, boostScriptFile(t))
}

// TestARestoredListKeepsItsRailAndItsKind: a window restored past the list's
// first row draws the rail its first page sends, of the kind it says: by
// Title a jump to M, an album, Back, then the restored rail's jump to Z; by
// Year the decades, from the 1990s to the 1960s; by Recently Added the
// periods, from 2025 to "?"; every albums request carrying the reader's tz.
func TestARestoredListKeepsItsRailAndItsKind(t *testing.T) { listScenario(t, 9) }

// TestARapidScrubAppendsOnlyTheLastJumpsRows: a scrub's jump to M, its first
// rows in and a frame holding the rest of its page, then its jump to Z
// before that frame runs: the list is Z's rows alone, in order.
func TestARapidScrubAppendsOnlyTheLastJumpsRows(t *testing.T) { listScenario(t, 2) }
