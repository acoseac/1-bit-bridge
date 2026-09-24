// Package sqlitetest parks a running SQLite statement from inside it, so a
// test can cancel a statement while it runs rather than before it starts.
// Like net/http/httptest it is imported only by tests, so none of it reaches
// the binary.
//
// The park is a collation. SQLite compares two keys through an index's
// collation whenever a statement seeks in that index or inserts a key into
// it, and the collation registered here is a Go function. So a test that
// creates an index under it, over a column the statement it wants to stop
// will write, gets a call back into this package from inside that statement:
// an UPDATE that moves a row's key, an INSERT, a DELETE, or VACUUM rebuilding
// the index (internal/backup/backuptest). Nothing in production code knows.
// A key is compared only against keys already in the index, so the index
// needs at least one other row for the statement to reach the collation, and
// a partial index (CREATE INDEX ... WHERE ...) keeps statements the test does
// not want to stop out of it.
//
// The collation is registered in init because modernc's Driver.Open reads its
// collation list without a lock. Registering it while another goroutine opens
// a connection would be a data race, and init runs before any test can open
// one. Every connection opened after init has it, including a manifest.Store's.
//
// This was backuptest's park, written for VACUUM (#998), until the scanner's
// cancellation tests needed the same stop inside an UPDATE.
package sqlitetest

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// Collation is the name an index is ordered under to make the statements
// that maintain it park: `CREATE INDEX i ON t (col COLLATE sqlitetest_park)`.
// A collation compares TEXT, so an index over an integer column orders an
// expression that yields text, such as `CAST(col AS TEXT)`.
const Collation = "sqlitetest_park"

// armed is the Park every comparison waits on, or nil when none is armed.
var armed atomic.Pointer[Park]

func init() {
	sqlite.MustRegisterCollationUtf8(Collation, compare)
}

// compare orders two keys byte by byte, as SQLite's BINARY collation does,
// once the armed Park, if there is one, has let it go.
func compare(a, b string) int {
	if p := armed.Load(); p != nil {
		p.stop()
	}
	return strings.Compare(a, b)
}

// Armed reports whether a Park is armed. A test that writes rows under the
// collation checks it first: with a Park armed, the writes would park.
func Armed() bool { return armed.Load() != nil }

// Park holds every key comparison under the collation until the test lets
// it go, one comparison at a time. Arm arms it; Wait and ReleaseUntil drive
// it.
type Park struct {
	arrived chan struct{} // a comparison is waiting to be let go
	next    chan struct{} // lets the waiting comparison go
	off     chan struct{} // closed on Disarm: nothing waits any more
	offOnce sync.Once
}

// Arm arms a Park, and disarms it when the test ends. One at a time: the
// collation is process-wide, so a second Park would share the first's
// comparisons.
func Arm(t testing.TB) *Park {
	t.Helper()
	p := &Park{arrived: make(chan struct{}), next: make(chan struct{}), off: make(chan struct{})}
	if !armed.CompareAndSwap(nil, p) {
		t.Fatal("sqlitetest: a Park is already armed")
	}
	t.Cleanup(p.Disarm)
	return p
}

// Disarm stops the Park holding anything: a statement that compares keys
// afterwards runs straight through, and one still waiting is let go. A test
// that runs more statements after the one it stopped, such as a second scan,
// disarms first. Idempotent; Arm's cleanup calls it too.
func (p *Park) Disarm() {
	p.offOnce.Do(func() {
		armed.CompareAndSwap(p, nil)
		close(p.off)
	})
}

// stop is one comparison's wait: it announces itself, then waits to be let
// go. Both halves give way once the Park is disarmed, so a statement that a
// failed test abandoned runs to completion instead of holding the goroutine
// that started it.
func (p *Park) stop() {
	select {
	case p.arrived <- struct{}{}:
	case <-p.off:
		return
	}
	select {
	case <-p.next:
	case <-p.off:
	}
}

// Wait blocks until a comparison is parked: a statement that maintains an
// index under the collation is then stopped part-way. It fails the test if
// none arrives, so a statement that never ran reads as a failure and not as
// a hung test binary.
func (p *Park) Wait(t testing.TB) {
	t.Helper()
	select {
	case <-p.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("sqlitetest: no statement compared a key within 10s")
	}
}

// ReleaseUntil lets go the comparison Wait saw, then each one after it, one
// at a time, until done is closed, and reports how many it let go.
//
// One at a time, and after a yield, because a cancel reaches a running
// statement through a goroutine of the driver's own: modernc's
// interruptOnDone calls sqlite3_interrupt once the context is done. That
// goroutine has to run before the statement finishes, or the cancel arrives
// too late to stop it, and a statement small enough for a unit test finishes
// in microseconds once it is let go. The statement cannot move while a
// comparison is parked, and the yield before each release is where the
// driver's goroutine runs: a plain hand-off between this goroutine and the
// parked one can pass the processor back and forth between the two and leave
// it waiting.
//
// Both halves were measured on a VACUUM, 200 cancelled snapshots each under
// -race (#998). Letting every comparison go at once let the copy finish
// before the cancel reached it in 2 runs at GOMAXPROCS=1. One at a time
// without the yield never failed, but the cancel landed as late as the 31st
// comparison, the last one the source's rows produce. As written it lands on
// the first or the second.
func (p *Park) ReleaseUntil(t testing.TB, done <-chan struct{}) int {
	t.Helper()
	released := 0
	for {
		runtime.Gosched()
		select {
		case p.next <- struct{}{}:
			released++
		case <-done:
			return released
		}
		select {
		case <-p.arrived:
		case <-done:
			return released
		case <-time.After(10 * time.Second):
			t.Fatal("sqlitetest: the statement neither compared again nor finished within 10s")
		}
	}
}
