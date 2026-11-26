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
// focus in the field; and the playlists show the filtered collection.
func TestTheFilterNarrowsTheListInPlace(t *testing.T) { listScenario(t, 14) }

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
// the place's row is found by its id; and Smart Mixes do the same.
func TestAWholeCollectionRestoresFromItsFirstRowByItsRowsID(t *testing.T) { listScenario(t, 7) }

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
// a letter of the rail drawn for the old q and the Try again of a restore
// that failed before it ask for nothing, and the filtered list stands from
// its first row.
func TestNoCallbackOfAListAFilterReplacedActs(t *testing.T) { listScenario(t, 6) }
