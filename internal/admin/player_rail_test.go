package admin

import "testing"

// The jump rail's kinds and its scrub (B1021 PR 3b): ui.js's rail alone,
// under node over testdata/bar_harness.mjs's DOM model, its entries laid out
// by the case (railModel) so a point names the entry under it; a host
// without node skips these cases, and under CI they fail without it.

// TestTheYearRailFollowsThePhonesBands: 16 years make a band each and 17 a
// band each decade, "?" one of its own, the century dropped only where every
// year is in the ten decades ending with this one (an 1874 album puts it on
// every label), no band sized by its count; a key on a decade's band jumps to
// its first year and a finger's height in it picks its year, which the
// callout names.
func TestTheYearRailFollowsThePhonesBands(t *testing.T) { barScenario(t, 10) }

// TestTheDateRailNamesItsPeriods: an entry to each period in the list's order,
// labelled short (this month's name, this year's digits, each earlier year,
// ?) and named whole ("This month", "Earlier this year", "Date unknown"),
// counted in its title, each jumping to its period, the callout saying its
// words.
func TestTheDateRailNamesItsPeriods(t *testing.T) { barScenario(t, 5) }

// TestAFingerScrubsTheRail: a finger down on a live letter jumps to it once,
// a drag jumps at each new live letter and never at an empty one, the
// callout naming the letter the list is at beside the rail at the finger's
// height; the lift, the touch's click after it and a cancel jump nothing
// more and hide the callout; a finger on the rail's edge or past it takes
// the letter at its height; a mouse's click is PR 1's, one jump and no
// callout; and a callout taller than the room at the first or the last
// letter stays inside the rail's height.
func TestAFingerScrubsTheRail(t *testing.T) { barScenario(t, 12) }

// TestAScrubIsItsOwnersAlone: a second finger on the rail beside the scrub's
// owner moves nothing; the owner's letter losing the capture to the rail ends
// nothing, the rail losing it ends the scrub and hides the callout; after
// that the owner's moves, its lift and the second finger's click jump
// nothing while a key's click jumps; and a tap's click after a mouse went
// down elsewhere is the tap's, one jump for the tap, the mouse's own click
// after it jumping.
func TestAScrubIsItsOwnersAlone(t *testing.T) { barScenario(t, 7) }

// TestAScrubCrossesTheShortWindowsGrid: in the short window's seven columns a
// drag across two rows jumps at each new live letter by the cell under the
// finger, the grid's empty last cell moves nothing, and a tap on a row's left
// or right edge takes the cell there.
func TestAScrubCrossesTheShortWindowsGrid(t *testing.T) { barScenario(t, 3) }
