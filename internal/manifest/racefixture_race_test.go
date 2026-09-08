//go:build race

package manifest

// compactFixtureRows is the row count the compaction fixtures seed.
//
// Small under `-race`, for a measured reason. This project's SQLite driver is
// modernc.org/sqlite — pure Go, so every page operation is Go code the race
// detector instruments, and the multiplier is not the usual eight. Measured on
// this package: deleting 2,000 rows through DeleteTracksBatch takes 1.43s
// normally and 69s under `-race`, roughly 48x. That one fixture was the largest
// single cost in the whole CI gate.
//
// Nothing about these tests is concurrent — they seed rows, delete rows, and
// vacuum, on one goroutine — so the detector has nothing to find in them. The
// code paths still execute here, at a size that costs seconds, so a race
// introduced into Compact or PageStats would still be caught. The FRAGMENTATION
// property those fixtures exist to demonstrate needs the full size, and it is
// asserted in the `!race` build, which is what the macOS and Windows legs run
// (`go test ./...`, no `-race`, whole suite, about three minutes).
const compactFixtureRows = 400

// raceBuild says which of the two fixture sizes is in play, so a test can name
// the mode in its output instead of leaving a reader to guess why a number
// changed.
const raceBuild = true
