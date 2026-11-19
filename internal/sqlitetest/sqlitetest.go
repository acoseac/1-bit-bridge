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
// A test cancels a parked statement through the Park (Cancel), which waits
// until the driver has interrupted the statement before anything lets it go
// (backlog B909), wherever the statement's connection is the one open on its
// database; a test whose database has a pool of several, a scan's or a
// serve's, cancels and lets go, and the interrupt's timing is left to
// ReleaseUntil's yields. To find the connection the package keeps every
// connection the driver opens, by the file its DSN names, through a second
// connection hook.
//
// It also watches transactions roll back (RollbackWatch). database/sql rolls
// a transaction back from a goroutine of its own once the transaction's
// context is done (Tx.awaitDone), whatever the goroutine that runs the
// transaction is doing, and a test that needs that rollback to have happened
// before its next step (the commit it beat, backlog B662) waits for it where
// it happens: at the driver, on a connection to the test's database file. The
// hook is installed on every connection opened after init, as the collation
// is, and costs an atomic load per rollback while no watch is armed.
package sqlitetest

import (
	"context"
	"database/sql/driver"
	"runtime"
	"slices"
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
	sqlite.RegisterConnectionHook(hookRollbacks)
	sqlite.RegisterConnectionHook(trackConnection)
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
// WaitUnless, Cancel and ReleaseUntil drive it.
type Park struct {
	arrived chan struct{} // a comparison is waiting to be let go
	next    chan struct{} // lets the waiting comparison go
	off     chan struct{} // closed on Disarm: nothing waits any more
	offOnce sync.Once

	// giveUp fires when a wait on the statement gives up, and bound says
	// when that is, for the failure.
	giveUp func() <-chan time.Time
	bound  string

	// awaiting, nil but in this package's tests, runs each time Cancel has
	// found no interrupt yet, before it sleeps: only a Cancel that waits for
	// the interrupt runs it.
	awaiting func()
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

// Cancel calls cancel, the cancel of the context the parked statement runs
// on, and returns once the driver has interrupted the statement: from then on
// the statement comes back as that context's error, however its comparisons
// are let go (ReleaseUntil) and however soon it then ends. The statement's
// connection is the one connection to the database at path that is open and
// not interrupted as Cancel begins; with none, or more than one, Cancel fails
// the test before it cancels anything, since it cannot tell which interrupt
// to wait for. It fails the test too where no interrupt has come when the
// Park's waits give up, and returns, waiting no more, once the Park is
// disarmed.
//
// The driver stops a running statement from a goroutine of its own, which
// runs once the statement's context is done (modernc's interruptOnDone): it
// marks the call stopped, which is what the call answers from then on whether
// or not SQLite sees the interrupt, then calls sqlite3_interrupt, which sets
// the connection's interrupt flag. Nothing tells a test when that goroutine
// has run, and a statement let go before it has can end first and answer no
// error, ReleaseUntil's yields notwithstanding: backlog B909, an UPDATE that
// moved one key of an 8-row index, on CI's -race job. So Cancel waits for the
// flag, which the connection reports through IsValid, false once it is set
// (sqlite3_is_interrupted, an atomic load that takes no lock of SQLite's).
// It asks from the test's goroutine, never from the comparison, which stays
// parked and asks its connection nothing, and it asks between sleeps, since
// the flag announces nothing. An in-memory database's connection reports
// itself valid whatever its flag, so Cancel serves a file database alone.
func (p *Park) Cancel(t testing.TB, path string, cancel context.CancelFunc) {
	t.Helper()
	live := liveConns(path)
	if len(live) != 1 {
		t.Fatalf("sqlitetest: Cancel waits for the interrupt of the one connection to %s that is open and not interrupted, and %d are", path, len(live))
		return
	}
	cancel()
	giveUp := p.giveUp()
	for pause := 50 * time.Microsecond; live[0].live(); pause = min(2*pause, 5*time.Millisecond) {
		if p.awaiting != nil {
			p.awaiting()
		}
		next := time.NewTimer(pause)
		select {
		case <-next.C:
		case <-p.off:
			next.Stop()
			return
		case <-giveUp:
			next.Stop()
			t.Fatalf("sqlitetest: the cancel did not interrupt the parked statement %s", p.bound)
			return
		}
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
//
// The yield does not make the driver's goroutine run, though: another
// processor can take it and then be kept off the CPU while this one lets the
// statement end (backlog B909, on CI's -race job). A test that cancels the
// statement calls Cancel before this, which waits for the interrupt; the
// yield is all a test has that cannot name the statement's connection.
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

// conns is every connection the driver has opened since init that was open
// and not interrupted when it last opened one, by the file its DSN names
// (trackConnection): where Cancel finds a parked statement's connection.
var conns struct {
	mu     sync.Mutex
	byFile map[string][]*tracked
}

// tracked is a connection the driver opened, as Cancel asks it.
type tracked struct {
	// lock is the lock the driver's connection takes to close and to
	// interrupt (its embedded sync.Mutex), held across IsValid so that its
	// read of the connection's handle never races a close.
	lock  connLock
	valid driver.Validator
}

// connLock is what a connection's lock answers: sync.Mutex's methods.
type connLock interface {
	Lock()
	Unlock()
	TryLock() bool
}

// live reports whether the connection is open and not interrupted.
func (c *tracked) live() bool {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.valid.IsValid()
}

// gone reports whether the connection has closed or been interrupted, asking
// only where its lock is free at once: one a close or an interrupt holds now
// is kept, and asked again at the next open.
func (c *tracked) gone() bool {
	if !c.lock.TryLock() {
		return false
	}
	defer c.lock.Unlock()
	return !c.valid.IsValid()
}

// trackConnection records in conns a connection the driver has just opened,
// under the file its DSN names (name up to any query), and drops every
// connection recorded that has closed or been interrupted since (gone), so
// that conns keeps little more than the connections open; it waits on no
// other connection's lock, which a close holds across SQLite's close, a WAL
// database's checkpoint among it. A connection that does not take the lock
// its close takes, or does not report whether it is valid, is not recorded,
// and Cancel finds none to wait for.
func trackConnection(conn sqlite.ExecQuerierContext, name string) error {
	lock, locks := conn.(connLock)
	valid, reports := conn.(driver.Validator)
	if !locks || !reports {
		return nil
	}
	file, _, _ := strings.Cut(name, "?")
	conns.mu.Lock()
	defer conns.mu.Unlock()
	if conns.byFile == nil {
		conns.byFile = make(map[string][]*tracked)
	}
	for f, list := range conns.byFile {
		kept := list[:0]
		for _, c := range list {
			if !c.gone() {
				kept = append(kept, c)
			}
		}
		if len(kept) == 0 {
			delete(conns.byFile, f)
		} else {
			conns.byFile[f] = kept
		}
	}
	conns.byFile[file] = append(conns.byFile[file], &tracked{lock: lock, valid: valid})
	return nil
}

// liveConns is the connections conns records for the database at path, by
// the name dsn.File gives it or by the path itself, that are open and not
// interrupted now. It asks each under that connection's lock, waiting for it,
// which a close holds across SQLite's close, so it asks them once it has let
// the registry's lock go, which every open takes (trackConnection); and it
// asks a copy of what the registry holds, a slice of its own, since an open
// compacts a list in place.
func liveConns(path string) []*tracked {
	conns.mu.Lock()
	candidates := slices.Concat(conns.byFile[dsn.File(path, "")], conns.byFile[path])
	conns.mu.Unlock()
	var live []*tracked
	for _, c := range candidates {
		if c.live() {
			live = append(live, c)
		}
	}
	return live
}

// watching is the RollbackWatch every rollback hook reports to, or nil when
// none is armed.
var watching atomic.Pointer[RollbackWatch]

// hookRollbacks gives a connection the driver has just opened a rollback hook
// (sqlite3_rollback_hook) that tells the armed watch, when the connection's
// database is the watch's. SQLite calls it once a transaction has rolled back,
// by a ROLLBACK or by an error that ends the transaction, and not for a
// statement that rolls back alone or for a connection's close. name is the
// DSN the connection was opened by; its part before any query names the file.
func hookRollbacks(conn sqlite.ExecQuerierContext, name string) error {
	h, ok := conn.(sqlite.HookRegisterer)
	if !ok {
		return nil
	}
	file, _, _ := strings.Cut(name, "?")
	h.RegisterRollbackHook(func() {
		if w := watching.Load(); w != nil && w.names(file) {
			w.note()
		}
	})
	return nil
}

// RollbackWatch counts the transactions rolled back at the driver on the
// connections to one database file, so a test can wait for the rollback
// database/sql makes on a goroutine of its own when a transaction's context
// is done. WatchRollbacks or WatchRollbacksUntil arms it; Seen and WaitPast
// read it. It counts rollbacks on every connection to the file, so the test
// says which transaction it waits for by what it then finds: a transaction
// whose rollback it waited for answers sql.ErrTxDone.
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
