//go:build !race

package manifest

// compactFixtureRows is the row count the compaction fixtures seed. See the
// `race` build's copy for why there are two: modernc.org/sqlite under the race
// detector measured ~48x on bulk row churn, and these tests have no concurrency
// for the detector to find.
//
// This is the size the fragmentation property was established at, and the size
// the macOS and Windows legs run.
const compactFixtureRows = 4000

const raceBuild = false
