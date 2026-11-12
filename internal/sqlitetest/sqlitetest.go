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
//
// It also watches transactions roll back (RollbackWatch). database/sql rolls
// a transaction back from a goroutine of its own once the transaction's
// context is done (Tx.awaitDone), whatever the goroutine that runs the
// transaction is doing, and a test that needs that rollback to have happened
// before its next step (the commit it beat, backlog B662) waits for it where
// it happens: at the driver, on the transaction's own connection
// (RollbackWatch.RollbackOf). Two hooks are installed on every connection
// opened after init, as the collation is: a rollback hook
// (sqlite3_rollback_hook), which costs an atomic load per rollback while no
// watch is armed, and an authorizer (sqlite3_set_authorizer), which SQLite
// asks as it prepares each statement and which allows every one. The
// authorizer is how the hooks learn which connection a test's transaction
// runs on (a statement the transaction runs names the test's expectation) and
// when the driver begins its ROLLBACK there, after database/sql has marked the
// transaction done and before SQLite rolls it back (backlog B869).
package sqlitetest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
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
	// In init for the collation's reason: modernc's Driver.Open reads its
	// connection hooks without the lock RegisterConnectionHook takes.
	sqlite.RegisterConnectionHook(hookConnection)
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
// it go, one comparison at a time. Arm or ArmUntil arms it; Wait,
// WaitUnless and ReleaseUntil drive it.
type Park struct {
	arrived chan struct{} // a comparison is waiting to be let go
	next    chan struct{} // lets the waiting comparison go
	off     chan struct{} // closed on Disarm: nothing waits any more
	offOnce sync.Once

	// giveUp fires when a wait on the statement gives up, and bound says
	// when that is, for the failure.
	giveUp func() <-chan time.Time
	bound  string
}

// perWaitBound is how long a Park armed by Arm waits for each thing it waits
// for: a statement the test itself started, in its own process, reaching its
// first comparison or its next one.
const perWaitBound = 10 * time.Second

// Arm arms a Park whose waits each give up perWaitBound after they begin, and
// disarms it when the test ends. One at a time: the collation is
// process-wide, so a second Park would share the first's comparisons.
func Arm(t testing.TB) *Park {
	t.Helper()
	return arm(t, func() <-chan time.Time { return time.After(perWaitBound) }, "within 10s")
}

// ArmUntil is Arm for a statement that runs inside work the test does not
// pace, such as a whole `bridge serve` booting and then tearing down: every
// wait gives up at the instant at, and never when at is zero. A bound per
// wait there is a guess at how long that work's disk writes take, which a
// starved host exceeds (backlog B63: a serve's boot and teardown each took
// longer than 10 s on a Windows host whose disk 24 writers kept busy).
func ArmUntil(t testing.TB, at time.Time) *Park {
	t.Helper()
	if at.IsZero() {
		return arm(t, func() <-chan time.Time { return nil }, "(no deadline)")
	}
	return arm(t, func() <-chan time.Time { return time.After(time.Until(at)) },
		"by "+at.Format("15:04:05.000"))
}

func arm(t testing.TB, giveUp func() <-chan time.Time, bound string) *Park {
	t.Helper()
	p := &Park{
		arrived: make(chan struct{}), next: make(chan struct{}), off: make(chan struct{}),
		giveUp: giveUp, bound: bound,
	}
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
// none arrives before the Park's waits give up, so a statement that never ran
// reads as a failure and not as a hung test binary.
func (p *Park) Wait(t testing.TB) {
	t.Helper()
	p.WaitUnless(t, nil)
}

// WaitUnless is Wait for a statement another goroutine's work runs: it
// answers true once a comparison is parked, and false, without failing, if
// stopped is closed first. So a test that parks a statement inside a serve
// reports the serve's own end, an exit code and what it printed, where Wait
// would report only that nothing arrived. A nil stopped never closes.
func (p *Park) WaitUnless(t testing.TB, stopped <-chan struct{}) bool {
	t.Helper()
	select {
	case <-p.arrived:
		return true
	case <-stopped:
		return false
	case <-p.giveUp():
		t.Fatalf("sqlitetest: no statement compared a key %s", p.bound)
		return false
	}
}

// ReleaseUntil lets go the comparison Wait saw, then each one after it, one
// at a time, until done is closed, and reports how many it let go. It fails
// the test if the statement neither compares again nor lets done close
// before the Park's waits give up; with done a serve's exit, that covers the
// whole teardown after the cancel, which is why a serve test arms its Park
// with ArmUntil.
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
		case <-p.giveUp():
			t.Fatalf("sqlitetest: the statement neither compared again nor finished %s", p.bound)
		}
	}
}

// watching is the RollbackWatch every rollback hook reports to, or nil when
// none is armed.
var watching atomic.Pointer[RollbackWatch]

// hookConnection gives a connection the driver has just opened its two hooks.
// The rollback hook (sqlite3_rollback_hook) tells the armed watch, when the
// connection's database is the watch's, and the rollbacks expected of the
// transaction the connection runs. SQLite calls it once a transaction has
// rolled back, by a ROLLBACK or by an error that ends the transaction, and
// not for a statement that rolls back alone or for a connection's close. The
// authorizer (connection.authorize) allows every statement. name is the DSN
// the connection was opened by; its part before any query names the file.
func hookConnection(conn sqlite.ExecQuerierContext, name string) error {
	h, ok := conn.(sqlite.HookRegisterer)
	if !ok {
		return nil
	}
	file, _, _ := strings.Cut(name, "?")
	c := &connection{file: file}
	h.RegisterRollbackHook(func() {
		if w := watching.Load(); w != nil && w.names(file) {
			w.note()
		}
		c.rolledBack()
	})
	if a, ok := conn.(sqlite.AuthorizerRegisterer); ok {
		return a.RegisterAuthorizer(c.authorize)
	}
	return nil
}

// bindPragma is the PRAGMA a transaction runs to name, to the hooks of its
// connection, the rollback a test expects of it (bindTo). SQLite knows no such
// PRAGMA, so the statement does nothing, but SQLite asks the connection's
// authorizer about it first, with its value: the expectation's token.
const bindPragma = "sqlitetest_rollback_of"

// connection is what the hooks know of one connection: the file it was opened
// on, and the rollbacks expected of the transaction it runs.
type connection struct {
	file string

	mu sync.Mutex
	// rollingBack is set from the moment the driver prepares a ROLLBACK on
	// the connection until SQLite has rolled a transaction back there, or the
	// connection's next transaction begins.
	rollingBack bool
	rollbacks   []*Rollback
	holds       []*RollbackHold
}

// authorize is the connection's authorizer, which SQLite asks as it prepares
// each statement on the connection, on the goroutine preparing it, and which
// allows every statement. It binds the expectation a bindPragma names to the
// connection, and tells the expectations bound there when the driver begins a
// ROLLBACK, as database/sql's Tx.rollback calls the driver's Rollback once it
// has marked the transaction done, or begins or commits a transaction instead
// (SQLite names BEGIN, COMMIT and ROLLBACK as a transaction action).
func (c *connection) authorize(action sqlite.AuthorizerActionCode, arg1, arg2, _, _ string) sqlite.AuthorizerReturnCode {
	switch {
	case action == sqlite.AuthPragma && strings.EqualFold(arg1, bindPragma):
		c.bind(arg2)
	case action == sqlite.AuthTransaction && arg1 == "ROLLBACK":
		c.rollbackBegins()
	case action == sqlite.AuthTransaction:
		c.transactionEnds(arg1)
	}
	return sqlite.AuthorizerOK
}

// pending holds each expectation bindTo has named and no connection has bound
// yet, by its token.
var pending = struct {
	sync.Mutex
	next  int64
	binds map[int64]func(*connection)
}{binds: map[int64]func(*connection){}}

// bind binds the expectation token names to the connection, once: a statement
// prepared again (SQLite does after a schema change) finds it taken.
func (c *connection) bind(token string) {
	n, err := strconv.ParseInt(token, 10, 64)
	if err != nil {
		return
	}
	pending.Lock()
	bind := pending.binds[n]
	delete(pending.binds, n)
	pending.Unlock()
	if bind != nil {
		bind(c)
	}
}

// rollbackBegins is the driver preparing a ROLLBACK on the connection: every
// rollback expected there has begun, and every hold there holds it here,
// before SQLite rolls the transaction back.
func (c *connection) rollbackBegins() {
	c.mu.Lock()
	c.rollingBack = true
	rollbacks, holds := c.rollbacks, c.holds
	c.holds = nil
	c.mu.Unlock()
	for _, r := range rollbacks {
		r.begin()
	}
	for _, h := range holds {
		h.park()
	}
}

// rolledBack is SQLite's rollback hook on the connection: where the driver
// had begun a ROLLBACK there, each rollback expected there has run. A
// rollback no ROLLBACK began, an error's, completes none.
func (c *connection) rolledBack() {
	c.mu.Lock()
	if !c.rollingBack {
		c.mu.Unlock()
		return
	}
	c.rollingBack = false
	rollbacks := c.rollbacks
	c.rollbacks = nil
	c.mu.Unlock()
	for _, r := range rollbacks {
		r.complete()
	}
}

// transactionEnds is the driver preparing a BEGIN or a COMMIT on the
// connection: the transaction a rollback was expected of ended, or ends,
// without one, and a later transaction's rollback there must not stand in
// for it.
func (c *connection) transactionEnds(op string) {
	c.mu.Lock()
	c.rollingBack = false
	rollbacks, holds := c.rollbacks, c.holds
	c.rollbacks, c.holds = nil, nil
	c.mu.Unlock()
	why := "its connection prepared a " + op + " first"
	for _, r := range rollbacks {
		r.lose(why)
	}
	for _, h := range holds {
		h.lose(why)
	}
}

// RollbackWatch counts the transactions rolled back at the driver on the
// connections to one database file, and hands out the rollback of one
// transaction on it (RollbackOf, HoldRollbackOf). WatchRollbacks or
// WatchRollbacksUntil arms it; Seen and WaitPast read the count. A count says
// only that some transaction on the file rolled back: another connection's, a
// reader's among them, counts too, and database/sql marks a transaction done,
// so that it answers sql.ErrTxDone, before it calls the driver's Rollback. So
// a count past the one read before a stop, followed by sql.ErrTxDone, proves
// neither that the rollback counted was the transaction's nor that its own
// has run (backlog B869): a test that waits for one transaction's rollback
// takes RollbackOf before the stop.
type RollbackWatch struct {
	// dsnFile and path are the two names a connection may have opened the
	// file by: a DSN built by dsn.File, up to its query, and the plain path.
	dsnFile, path string

	mu      sync.Mutex
	seen    int
	changed chan struct{} // closed, and replaced, at each rollback

	// giveUp fires when a wait gives up, and bound says when that is, for
	// the failure.
	giveUp func() <-chan time.Time
	bound  string
}

// WatchRollbacks arms a RollbackWatch on the database file at path whose
// waits each give up perWaitBound after they begin, and disarms it when the
// test ends. One at a time: the hook is process-wide, as the Park's
// collation is.
func WatchRollbacks(t testing.TB, path string) *RollbackWatch {
	t.Helper()
	return watchRollbacks(t, path, func() <-chan time.Time { return time.After(perWaitBound) }, "within 10s")
}

// WatchRollbacksUntil is WatchRollbacks for a rollback inside work the test
// does not pace, such as a whole `bridge serve`: every wait gives up at the
// instant at, and never when at is zero (ArmUntil's reason).
func WatchRollbacksUntil(t testing.TB, path string, at time.Time) *RollbackWatch {
	t.Helper()
	if at.IsZero() {
		return watchRollbacks(t, path, func() <-chan time.Time { return nil }, "(no deadline)")
	}
	return watchRollbacks(t, path, func() <-chan time.Time { return time.After(time.Until(at)) },
		"by "+at.Format("15:04:05.000"))
}

func watchRollbacks(t testing.TB, path string, giveUp func() <-chan time.Time, bound string) *RollbackWatch {
	t.Helper()
	w := &RollbackWatch{
		dsnFile: dsn.File(path, ""), path: path,
		changed: make(chan struct{}), giveUp: giveUp, bound: bound,
	}
	if !watching.CompareAndSwap(nil, w) {
		t.Fatal("sqlitetest: a RollbackWatch is already armed")
	}
	t.Cleanup(func() { watching.CompareAndSwap(w, nil) })
	return w
}

// names reports whether a connection opened by file, a DSN up to its query,
// is a connection to the watch's database.
func (w *RollbackWatch) names(file string) bool {
	return file == w.dsnFile || file == w.path
}

// note counts one rollback and wakes every wait.
func (w *RollbackWatch) note() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen++
	close(w.changed)
	w.changed = make(chan struct{})
}

// Seen reports how many transactions have rolled back on the file since the
// watch was armed. A test reads it before the step that makes the rollback,
// and waits past it.
func (w *RollbackWatch) Seen() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen
}

// WaitPast blocks until more than n transactions have rolled back on the file
// and answers true, or answers false, without failing, if stopped is closed
// first; a nil stopped never closes. It fails the test if neither comes before
// the watch's waits give up, so a rollback that never came reads as a failure
// and not as a hung test binary.
func (w *RollbackWatch) WaitPast(t testing.TB, n int, stopped <-chan struct{}) bool {
	t.Helper()
	giveUp := w.giveUp()
	for {
		w.mu.Lock()
		seen, changed := w.seen, w.changed
		w.mu.Unlock()
		if seen > n {
			return true
		}
		select {
		case <-changed:
		case <-stopped:
			return false
		case <-giveUp:
			t.Fatalf("sqlitetest: no transaction rolled back on %s %s (%d seen, waiting past %d)", w.path, w.bound, seen, n)
			return false
		}
	}
}

// bindTo names bind's expectation to the hooks of the connection tx runs on:
// tx runs a bindPragma carrying its token, whose authorization there calls
// bind with that connection, on this goroutine, before ExecContext returns.
// It fails the test where tx cannot run it (it is done already), where no
// connection's hooks heard it, or where the connection is to another file
// than the watch's, which bind is then not called with.
func (w *RollbackWatch) bindTo(t testing.TB, tx *sql.Tx, bind func(*connection)) {
	t.Helper()
	took := make(chan string, 1)
	pending.Lock()
	pending.next++
	token := pending.next
	pending.binds[token] = func(c *connection) {
		if w.names(c.file) {
			bind(c)
		}
		took <- c.file
	}
	pending.Unlock()
	defer func() {
		pending.Lock()
		delete(pending.binds, token)
		pending.Unlock()
	}()
	if _, err := tx.ExecContext(context.Background(), fmt.Sprintf("PRAGMA %s = %d", bindPragma, token)); err != nil {
		t.Fatalf("sqlitetest: naming the expected rollback to the transaction's connection: %v", err)
	}
	select {
	case file := <-took:
		if !w.names(file) {
			t.Fatalf("sqlitetest: the transaction runs on a connection to %s, not to the watched %s", file, w.path)
		}
	default:
		t.Fatal("sqlitetest: no connection's hooks heard the transaction: it runs on a connection opened before init, or through another driver")
	}
}

// Rollback is the rollback of one transaction at the driver, on that
// transaction's own connection, taken before the stop that makes it
// (RollbackOf): database/sql's own once the transaction's context is done
// (Tx.awaitDone), or any other Rollback of it. It has run once the driver has
// begun its ROLLBACK there, which database/sql calls only once it has marked
// the transaction done, and SQLite has then rolled the transaction back
// (its rollback hook, inside that ROLLBACK); a rollback that runs before its
// wait begins is kept. Another connection's rollback, a reader's on the same
// file included, runs none.
type Rollback struct {
	w *RollbackWatch

	began chan struct{} // closed once the driver began its ROLLBACK
	ran   chan struct{} // closed once SQLite rolled the transaction back inside it
	gone  chan struct{} // closed, why written first, once it can no longer come
	why   string

	beginning, ending sync.Once
}

// RollbackOf takes the rollback of the transaction tx, which must still be
// open, on its connection, as a test does before the stop that makes it. It
// runs a statement in tx that changes nothing (bindTo), and fails the test
// where tx is not a transaction on the watch's file.
func (w *RollbackWatch) RollbackOf(t testing.TB, tx *sql.Tx) *Rollback {
	t.Helper()
	r := &Rollback{w: w, began: make(chan struct{}), ran: make(chan struct{}), gone: make(chan struct{})}
	w.bindTo(t, tx, func(c *connection) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.rollingBack = false
		c.rollbacks = append(c.rollbacks, r)
	})
	return r
}

func (r *Rollback) begin()    { r.beginning.Do(func() { close(r.began) }) }
func (r *Rollback) complete() { r.ending.Do(func() { close(r.ran) }) }

func (r *Rollback) lose(why string) {
	r.ending.Do(func() {
		r.why = why
		close(r.gone)
	})
}

// Wait blocks until the transaction's rollback has run at the driver, and
// fails the test if it cannot come (the transaction committed, or its
// connection began another) or has not come before the watch's waits give up.
func (r *Rollback) Wait(t testing.TB) {
	t.Helper()
	r.WaitUnless(t, nil)
}

// WaitUnless is Wait that answers false, without failing, if stopped is
// closed first, and true once the rollback has run; one that has run is
// answered first, whatever else is ready. A nil stopped never closes.
func (r *Rollback) WaitUnless(t testing.TB, stopped <-chan struct{}) bool {
	t.Helper()
	select {
	case <-r.ran:
		return true
	default:
	}
	select {
	case <-r.ran:
		return true
	case <-r.gone:
		t.Fatalf("sqlitetest: the transaction on %s can no longer roll back at the driver: %s", r.w.path, r.why)
	case <-stopped:
		return false
	case <-r.w.giveUp():
		where := "the driver never began its ROLLBACK"
		select {
		case <-r.began:
			where = "the driver began its ROLLBACK, and SQLite never rolled the transaction back"
		default:
		}
		t.Fatalf("sqlitetest: no rollback of the transaction on %s %s: %s", r.w.path, r.w.bound, where)
	}
	return false
}

// RollbackHold holds the rollback of one transaction at its boundary: the
// driver's ROLLBACK on the transaction's connection, prepared once
// database/sql has marked the transaction done, waits there, before SQLite
// rolls the transaction back, until the test lets it go (HoldRollbackOf).
type RollbackHold struct {
	w *RollbackWatch

	reached chan struct{} // closed once the driver's ROLLBACK waits at the boundary
	out     chan struct{} // closed to let it go on
	gone    chan struct{} // closed, why written first, once it can no longer come
	why     string

	reaching, leaving, ending sync.Once
}

// HoldRollbackOf holds the next rollback of the transaction tx, which must
// still be open, at its boundary, as RollbackOf takes it; the test's end lets
// a rollback it holds go.
func (w *RollbackWatch) HoldRollbackOf(t testing.TB, tx *sql.Tx) *RollbackHold {
	t.Helper()
	h := &RollbackHold{w: w, reached: make(chan struct{}), out: make(chan struct{}), gone: make(chan struct{})}
	t.Cleanup(h.Release)
	w.bindTo(t, tx, func(c *connection) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.rollingBack = false
		c.holds = append(c.holds, h)
	})
	return h
}

// park is the driver's ROLLBACK waiting at the boundary, on the goroutine
// that prepares it, until Release.
func (h *RollbackHold) park() {
	h.reaching.Do(func() { close(h.reached) })
	<-h.out
}

func (h *RollbackHold) lose(why string) {
	h.ending.Do(func() {
		h.why = why
		close(h.gone)
	})
}

// Reached blocks until the driver's ROLLBACK of the transaction waits at the
// boundary, and fails the test if it can no longer come or has not come
// before the watch's waits give up.
func (h *RollbackHold) Reached(t testing.TB) {
	t.Helper()
	select {
	case <-h.reached:
	case <-h.gone:
		t.Fatalf("sqlitetest: the held transaction on %s can no longer roll back at the driver: %s", h.w.path, h.why)
	case <-h.w.giveUp():
		t.Fatalf("sqlitetest: the driver never began the held transaction's ROLLBACK on %s %s", h.w.path, h.w.bound)
	}
}

// Release lets a rollback held at the boundary go on, and one not held yet
// pass it. Idempotent.
func (h *RollbackHold) Release() { h.leaving.Do(func() { close(h.out) }) }

// The windows a WaitProof gives the wait it proves to answer wrongly in: with
// nothing else rolled back on the file, and once a reader's transaction has.
// A wait that takes another rollback for the held one answers within
// microseconds; a right one never answers in them.
const (
	proofQuiet  = 100 * time.Millisecond
	proofWindow = 500 * time.Millisecond
)

// WaitProof is the schedule that tells a wait for a transaction's own
// rollback at the driver from one that takes another rollback on its file for
// it (backlog B869), run on a goroutine of its own (BesideAReader) while the
// test runs the wait.
type WaitProof struct {
	hold     *RollbackHold
	returned atomic.Bool
	released atomic.Bool
	stop     chan struct{} // closed as the test ends
	done     chan struct{}
	// err and early are what the schedule found, written before done closes.
	err   error
	early string
}

// BesideAReader starts the schedule against the rollback h holds, and
// returns at once. Once the driver's ROLLBACK waits at the boundary, the
// schedule gives the wait proofQuiet to answer with nothing else rolled back,
// rolls a reader's read-only transaction back on a connection of its own to
// the watch's file, opened here, before anything is held, gives the wait
// proofWindow more, and only then lets the held rollback go. The test runs the
// wait on its own goroutine meanwhile and calls Returned the moment it
// returns.
func (h *RollbackHold) BesideAReader(t testing.TB) *WaitProof {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(h.w.path, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("sqlitetest: the reader's connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	p := &WaitProof{hold: h, stop: make(chan struct{}), done: make(chan struct{})}
	go p.run(conn, h.w.giveUp())
	// Registered after the connection's, so run before them: the schedule
	// ends, and the held rollback goes, before its connection closes.
	t.Cleanup(func() {
		close(p.stop)
		h.Release()
		select {
		case <-p.done:
		case <-time.After(perWaitBound):
			t.Error("sqlitetest: the proof's schedule never ended")
		}
	})
	return p
}

// run is the schedule, giving up at giveUp, the watch's, for the held
// rollback to begin. It makes no call on the test, which Returned does, and
// lets the held rollback go whatever it finds.
func (p *WaitProof) run(conn *sql.Conn, giveUp <-chan time.Time) {
	defer close(p.done)
	defer p.hold.Release()
	select {
	case <-p.hold.reached:
	case <-p.hold.gone:
		p.err = fmt.Errorf("the held rollback can no longer come: %s", p.hold.why)
		return
	case <-giveUp:
		p.err = fmt.Errorf("the driver never began the held transaction's ROLLBACK %s", p.hold.w.bound)
		return
	case <-p.stop:
		return
	}
	if p.answersWithin(proofQuiet) {
		p.early = "with no other rollback on the file"
		return
	}
	seen := p.hold.w.Seen()
	if err := readAndRollBack(conn); err != nil {
		p.err = fmt.Errorf("the reader beside the held rollback: %w", err)
		return
	}
	if p.hold.w.Seen() <= seen {
		p.err = errors.New("the watch did not count the reader's rollback")
		return
	}
	if p.answersWithin(proofWindow) {
		p.early = "once a reader's transaction rolled back on another connection to the file"
		return
	}
	p.released.Store(true)
}

// answersWithin reports whether the wait under proof returns within d, or
// the test ends first.
func (p *WaitProof) answersWithin(d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if p.returned.Load() {
			return true
		}
		select {
		case <-p.stop:
			return true
		default:
		}
	}
	return p.returned.Load()
}

// readAndRollBack runs a read-only transaction on conn that reads the file's
// schema and rolls back.
func readAndRollBack(conn *sql.Conn) error {
	tx, err := conn.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&n); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Rollback()
}

// Returned is the test saying that the wait under proof has returned, the
// moment it has: it fails the test where that wait returned while the
// rollback was still held at its boundary, before SQLite had rolled the
// transaction back, and waits for the schedule to end first.
func (p *WaitProof) Returned(t testing.TB) {
	t.Helper()
	released := p.released.Load()
	p.returned.Store(true)
	select {
	case <-p.done:
	case <-time.After(perWaitBound):
		t.Fatal("sqlitetest: the proof's schedule never ended")
	}
	if p.err != nil {
		t.Fatalf("sqlitetest: %v", p.err)
	}
	if !released {
		t.Errorf("the wait returned while the transaction's rollback was still held at the driver, before SQLite rolled it back: %s", p.early)
	}
}
