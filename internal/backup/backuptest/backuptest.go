// Package backuptest parks a SQLite VACUUM in the middle of its copy, so a
// test can cancel a snapshot while the copy is running rather than before it
// starts. Like net/http/httptest it is imported only by tests, so none of it
// reaches the binary.
//
// The park is a collation. VACUUM copies an index with an append fast path
// that compares no keys, EXCEPT an index with a column whose collation is not
// BINARY: SQLite rebuilds that one with ordinary seeks, because a collation
// may have been redefined since the keys were written (insert.c,
// xferOptimization). Every seek calls the collation, and here the collation is
// a Go function. So a VACUUM of a database WriteSource wrote calls back into
// this package mid-copy, with its destination file already created, and a
// test can stop it there with no hook in production code.
//
// The collation is registered in init because modernc's Driver.Open reads its
// collation list without a lock. Registering it while another goroutine opens
// a connection would be a data race, and init runs before any test can open
// one.
package backuptest

import (
	"database/sql"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"modernc.org/sqlite"
)

// collation is the name WriteSource's index is ordered under.
const collation = "backuptest_park"

// sourceRows is how many rows WriteSource writes. A VACUUM compares keys only
// from the second row on, and ReleaseUntil needs enough comparisons to give
// the driver's interrupt several chances to land (see its docblock).
const sourceRows = 32

// armed is the Park every comparison waits on, or nil when none is armed.
var armed atomic.Pointer[Park]

func init() {
	sqlite.MustRegisterCollationUtf8(collation, compare)
}

// compare orders two keys byte by byte, as SQLite's BINARY collation does,
// once the armed Park, if there is one, has let it go.
func compare(a, b string) int {
	if p := armed.Load(); p != nil {
		p.stop()
	}
	return strings.Compare(a, b)
}

// WriteSource adds to the SQLite database at path, creating it if it is
// absent, a table whose index is ordered under the park's collation, so a
// VACUUM of that database compares keys through this package. It opens the
// database in WAL mode, as the bridge opens its own manifest database.
//
// Call it before ParkVacuum: writing the rows compares keys too.
func WriteSource(t testing.TB, path string) {
	t.Helper()
	if armed.Load() != nil {
		t.Fatal("backuptest: WriteSource with a Park armed would park its own inserts")
	}
	db, err := sql.Open("sqlite", dsn.File(path, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatalf("backuptest: open %s: %v", path, err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE parked (k TEXT)`,
		`CREATE INDEX parked_k ON parked (k COLLATE ` + collation + `)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("backuptest: %s: %v", stmt, err)
		}
	}
	for i := 0; i < sourceRows; i++ {
		if _, err := db.Exec(`INSERT INTO parked (k) VALUES (?)`, fmt.Sprintf("key-%02d", i)); err != nil {
			t.Fatalf("backuptest: insert row %d: %v", i, err)
		}
	}
}

// Park holds every key comparison under the collation until the test lets
// it go, one comparison at a time. ParkVacuum arms it; Wait and ReleaseUntil
// drive it.
type Park struct {
	arrived chan struct{} // a comparison is waiting to be let go
	next    chan struct{} // lets the waiting comparison go
	off     chan struct{} // closed when the test ends: nothing waits any more
}

// ParkVacuum arms a Park, and disarms it when the test ends. One at a time:
// the collation is process-wide, so a second Park would share the first's
// comparisons.
func ParkVacuum(t testing.TB) *Park {
	t.Helper()
	p := &Park{arrived: make(chan struct{}), next: make(chan struct{}), off: make(chan struct{})}
	if !armed.CompareAndSwap(nil, p) {
		t.Fatal("backuptest: a Park is already armed")
	}
	t.Cleanup(func() {
		armed.CompareAndSwap(p, nil)
		close(p.off)
	})
	return p
}

// stop is one comparison's wait: it announces itself, then waits to be let
// go. Both halves give way once the test has ended, so a VACUUM that a failed
// test abandoned runs to completion instead of holding the goroutine that
// started it.
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

// Wait blocks until a comparison is parked. A VACUUM is then in the middle
// of copying WriteSource's index, and its destination file exists. It fails
// the test if none arrives, so a VACUUM that never ran reads as a failure and
// not as a hung test binary.
func (p *Park) Wait(t testing.TB) {
	t.Helper()
	select {
	case <-p.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("backuptest: no VACUUM compared a key within 10s")
	}
}

// ReleaseUntil lets go the comparison Wait saw, then each one after it, one
// at a time, until done is closed, and reports how many it let go.
//
// One at a time, and after a yield, because a cancel reaches a running
// statement through a goroutine of the driver's own: modernc's
// interruptOnDone calls sqlite3_interrupt once the context is done. That
// goroutine has to run before the copy finishes, or the cancel arrives too
// late to stop it, and a copy small enough for a unit test finishes in
// microseconds once it is let go. The copy cannot move while a comparison is
// parked, and the yield before each release is where the driver's goroutine
// runs: a plain hand-off between this goroutine and the parked one can pass
// the processor back and forth between the two and leave it waiting.
//
// Both halves were measured, 200 cancelled snapshots each under -race.
// Letting every comparison go at once let the copy finish before the cancel
// reached it in 2 runs at GOMAXPROCS=1. One at a time without the yield
// never failed, but the cancel landed as late as the 31st comparison, the
// last one WriteSource's rows produce. As written it lands on the first or
// the second.
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
			t.Fatal("backuptest: the VACUUM neither compared again nor finished within 10s")
		}
	}
}
