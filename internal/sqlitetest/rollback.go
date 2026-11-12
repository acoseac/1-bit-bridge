package sqlitetest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

// watching is the RollbackWatch every rollback hook reports to, or nil when
// none is armed.
var watching atomic.Pointer[RollbackWatch]

// countRollbacks gives a connection the driver has just opened a rollback
// hook (sqlite3_rollback_hook) that counts its rollbacks on the armed watch,
// when the connection's database is the watch's. SQLite calls it once a
// transaction has rolled back, by a ROLLBACK or by an error that ends the
// transaction, and not for a statement that rolls back alone, a savepoint's
// ROLLBACK TO, or a connection's close (modernc forgets the hook first). name
// is the DSN the connection was opened by; its part before any query names
// the file. A connection the watch's Connector makes replaces the hook with
// its own (conn.rolledBack), which counts too.
func countRollbacks(c sqlite.ExecQuerierContext, name string) error {
	h, ok := c.(sqlite.HookRegisterer)
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

// RollbackWatch counts the transactions rolled back on the connections to
// one database file, and hands out the rollback of one transaction there
// (RollbackOf, HoldRollbackOf, HoldReturnOf, WaitProofOf), which runs on a
// connection the watch's Connector made. WatchRollbacks or
// WatchRollbacksUntil arms it; Seen and WaitPast read the count. A count says
// only that some transaction on the file rolled back: another connection's, a
// reader's among them, counts too; database/sql marks a transaction done, so
// that it answers sql.ErrTxDone, before it calls the driver's Rollback; and
// the count moves as SQLite rolls the transaction back, inside the driver's
// ROLLBACK, before that statement ends and the driver's Rollback returns. So a
// count past the one read before a stop, followed by sql.ErrTxDone, proves
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

	// conns numbers the connections the watch's Connector makes, so that a
	// failure can name one.
	conns atomic.Int64
	// proof is the WaitProof under way on the watch, or nil: each wait on
	// the watch answers its asks (proofAsks, stillWaiting).
	proof atomic.Pointer[WaitProof]
	// beforeBind, when a test of this package sets it before any binding,
	// runs as a connection binds an expectation, just before the binding asks
	// the transaction's context: a cancel made during a binding.
	beforeBind func(*conn)
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
// and not as a hung test binary. Each time it decides to wait on, it answers
// the ask of a WaitProof under way on the watch.
func (w *RollbackWatch) WaitPast(t testing.TB, n int, stopped <-chan struct{}) bool {
	t.Helper()
	giveUp := w.giveUp()
	for {
		asked, ask := w.proofAsks()
		w.mu.Lock()
		seen, changed := w.seen, w.changed
		w.mu.Unlock()
		if seen > n {
			return true
		}
		w.stillWaiting(asked)
		select {
		case <-changed:
		case <-stopped:
			return false
		case <-giveUp:
			t.Fatalf("sqlitetest: no transaction rolled back on %s %s (%d seen, waiting past %d)", w.path, w.bound, seen, n)
			return false
		case <-ask:
		}
	}
}

// Connector wraps base, the connector of the registered "sqlite" driver for a
// DSN of the watch's file (sqlite.NewConnector, which keeps every function,
// collation and connection hook registered on that driver, this package's
// included), so that a transaction on a connection it makes can be watched
// at the driver: RollbackOf and the holds take only such a transaction. A
// store opens its connections through it by the store's test seam
// (manifest.SetConnectorForTest), set for the watch's path before the store
// opens. Each connection is the driver's, passed on with every interface
// database/sql asks of a connection that modernc's implements (so a
// connection a transaction's context rolled back is kept, as without the
// wrapper), and with a rollback hook of its own in place of the one init
// gave it (conn.rolledBack).
func (w *RollbackWatch) Connector(base driver.Connector) driver.Connector {
	return &connector{w: w, base: base}
}

// connector is the watch's wrap of the driver's connector.
type connector struct {
	w    *RollbackWatch
	base driver.Connector
}

// driverConn is what a connection the wrapper passes on is: every interface
// database/sql asks of a connection that modernc's implements, and the
// rollback hook. modernc's implements no driver.NamedValueChecker, and the
// wrapper passes none on, so database/sql converts arguments as it would.
type driverConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
	sqlite.HookRegisterer
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	dc, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	inner, ok := dc.(driverConn)
	if !ok {
		_ = dc.Close()
		return nil, fmt.Errorf("sqlitetest: the driver's connection, a %T, lacks an interface the watch's Connector passes on", dc)
	}
	wc := &conn{w: c.w, id: c.w.conns.Add(1), inner: inner}
	inner.RegisterRollbackHook(wc.rolledBack)
	return wc, nil
}

// Driver is the driver base's connections are the driver's of, so a
// database opened through the watch's Connector reports the driver one opened
// with sql.Open does.
func (c *connector) Driver() driver.Driver { return c.base.Driver() }

// conn is a connection the watch's Connector made: the driver's, whose
// transactions it passes on as tx, and what is expected of the transaction it
// runs. database/sql uses a connection on one goroutine at a time, and SQLite
// calls its rollback hook on the goroutine running the statement that rolled
// back; mu guards what a test's goroutine reads beside them.
type conn struct {
	w     *RollbackWatch
	id    int64
	inner driverConn

	mu sync.Mutex
	// txCtx is the context the transaction the connection runs began on,
	// which database/sql passed the driver's BeginTx, or nil while it runs
	// none. database/sql gives the driver no other context of the
	// transaction's: the one its Tx derives from this, whose end rolls the
	// transaction back, it keeps to itself.
	txCtx context.Context
	// rollingBack is set while the driver's Rollback of that transaction
	// runs (tx.Rollback), and rolledBackInSQLite once SQLite has rolled the
	// transaction back inside it.
	rollingBack, rolledBackInSQLite bool
	rollbacks                       []*Rollback
	atBoundary                      []*RollbackHold // held as the driver's Rollback is called
	inSQLite                        []*RollbackHold // held once SQLite has rolled the transaction back inside it
}

func (c *conn) Prepare(query string) (driver.Stmt, error) { return c.inner.Prepare(query) }

func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.inner.PrepareContext(ctx, query)
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.inner.QueryContext(ctx, query, args)
}

func (c *conn) Ping(ctx context.Context) error         { return c.inner.Ping(ctx) }
func (c *conn) ResetSession(ctx context.Context) error { return c.inner.ResetSession(ctx) }
func (c *conn) IsValid() bool                          { return c.inner.IsValid() }
func (c *conn) Begin() (driver.Tx, error)              { return c.BeginTx(context.Background(), driver.TxOptions{}) }

// BeginTx begins the driver's transaction and keeps the context it began on,
// which a binding asks (conn.bind).
func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	inner, err := c.inner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.txCtx = ctx
	c.mu.Unlock()
	return &tx{c: c, inner: inner}, nil
}

// ExecContext takes a binding's bindPragma itself, and passes every other
// statement on to the driver.
func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if token, ok := bindingToken(query); ok {
		return driver.RowsAffected(0), c.bind(token)
	}
	return c.inner.ExecContext(ctx, query, args)
}

// Close closes the driver's connection, after what was expected of the
// transaction it runs has ended: database/sql calls no Rollback on a
// connection it closes, so that rollback can no longer come, and a wait for
// it fails at once, naming the connection.
func (c *conn) Close() error {
	c.end(fmt.Sprintf("its connection, the watch's connection %d, closed before the driver rolled it back", c.id))
	return c.inner.Close()
}

// end ends, for why, everything expected of the transaction the connection
// runs, and forgets the context it began on: that transaction's rollback can
// no longer come at the driver.
func (c *conn) end(why string) {
	c.mu.Lock()
	rollbacks := c.rollbacks
	holds := append(append([]*RollbackHold(nil), c.atBoundary...), c.inSQLite...)
	c.rollbacks, c.atBoundary, c.inSQLite, c.txCtx = nil, nil, nil, nil
	c.mu.Unlock()
	for _, r := range rollbacks {
		r.lose(why)
	}
	for _, h := range holds {
		h.lose(why)
	}
}

// rolledBack is the rollback hook of a connection the watch's Connector made,
// which SQLite calls once a transaction has rolled back on it, on the
// goroutine running the statement that rolled it back, and which counts the
// rollback on the watch while it is armed. Inside the driver's Rollback it is
// SQLite having rolled the transaction back, before the ROLLBACK statement
// ends (SQLite still sets the connection's autocommit state and halts the
// statement after it), the driver finalizes the statement and its Rollback
// returns: a hold there holds it here. Anywhere else an error rolled the
// transaction back (a statement's ON CONFLICT ROLLBACK, SQLITE_FULL, an I/O
// error, a COMMIT that failed and the driver cleaned up after), so its
// rollback can no longer come at the driver, and what was expected of it is
// lost at once: database/sql's ROLLBACK of it then finds no transaction, and
// the connection goes back to the pool holding nothing of it.
func (c *conn) rolledBack() {
	if watching.Load() == c.w {
		c.w.note()
	}
	c.mu.Lock()
	if !c.rollingBack {
		c.mu.Unlock()
		c.end("its transaction rolled back by an error before its ROLLBACK")
		return
	}
	c.rolledBackInSQLite = true
	rollbacks, holds := c.rollbacks, c.inSQLite
	c.inSQLite = nil
	c.mu.Unlock()
	for _, r := range rollbacks {
		r.rollBackInSQLite()
	}
	for _, h := range holds {
		h.park()
	}
}

// tx is a transaction on a connection the watch's Connector made: the
// driver's, whose Commit and Rollback tell what was expected of it how it
// ended.
type tx struct {
	c     *conn
	inner driver.Tx
}

// Commit commits the driver's transaction, which ends it without a rollback,
// whatever the COMMIT answers: database/sql calls no Rollback after a Commit.
func (t *tx) Commit() error {
	err := t.inner.Commit()
	why := "its transaction committed first"
	if err != nil {
		why = fmt.Sprintf("its transaction's COMMIT failed (%v), after which database/sql calls no Rollback", err)
	}
	t.c.end(why)
	return err
}

// Rollback is the driver's Rollback of the transaction, which database/sql
// calls once it has marked the transaction done (Tx.rollback): every rollback
// expected of the transaction has begun as it is called, and a hold at the
// boundary holds it here, before the driver runs its ROLLBACK. Once the
// driver's Rollback has returned, each expected rollback has run where it
// returned no error and SQLite rolled the transaction back inside it (its
// rollback hook), and failed, with the driver's error, where it returned one:
// the hook alone fires before the statement ends and the driver finalizes it,
// so it says nothing of the return.
func (t *tx) Rollback() error {
	c := t.c
	c.mu.Lock()
	c.rollingBack, c.rolledBackInSQLite = true, false
	rollbacks, holds := c.rollbacks, c.atBoundary
	c.atBoundary = nil
	c.mu.Unlock()
	for _, r := range rollbacks {
		r.begin()
	}
	for _, h := range holds {
		h.park()
	}
	err := t.inner.Rollback()
	c.mu.Lock()
	inSQLite := c.rolledBackInSQLite
	c.rollingBack, c.rolledBackInSQLite = false, false
	rollbacks, late := c.rollbacks, c.inSQLite
	c.rollbacks, c.inSQLite, c.txCtx = nil, nil, nil
	c.mu.Unlock()
	for _, r := range rollbacks {
		r.returned(err, inSQLite)
	}
	for _, h := range late {
		h.lose("the driver's Rollback returned with SQLite having rolled nothing back inside it")
	}
	return err
}

// bindPragma is the PRAGMA a transaction runs to name, to the connection the
// watch's Connector made that it runs on, an expectation a test binds there
// (bindTo). That connection takes it before the driver sees it; on any other
// SQLite runs it, and since SQLite knows no such PRAGMA, it does nothing.
const bindPragma = "sqlitetest_rollback_of"

// bindingToken returns the token query, a bindPragma, carries.
func bindingToken(query string) (int64, bool) {
	rest, ok := strings.CutPrefix(query, "PRAGMA "+bindPragma+" = ")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	return n, err == nil
}

// binding is an expectation bindTo has named and no connection has taken.
type binding struct {
	w *RollbackWatch
	// attach binds the expectation to the connection, under its lock.
	attach func(*conn)
	// heard is sent the connection that took the binding, whether it bound
	// the expectation or refused it.
	heard chan *conn
}

// pending holds each binding by its token, from bindTo's naming of it until a
// connection takes it or bindTo returns, whichever is first.
var pending = struct {
	sync.Mutex
	next  int64
	binds map[int64]*binding
}{binds: map[int64]*binding{}}

// bind takes the binding token names, on the goroutine running the
// transaction's statement, and binds its expectation to the connection while
// the transaction the connection runs has not been stopped: once the context
// it began on is done, the binding is refused, whether or not database/sql has
// rolled it back yet, since the rollback a stop makes is taken before the
// stop. database/sql's own rollback waits for the statement running the
// binding (Tx.closemu), so a binding that lands lands before it.
func (c *conn) bind(token int64) error {
	pending.Lock()
	b := pending.binds[token]
	delete(pending.binds, token)
	pending.Unlock()
	if b == nil {
		return fmt.Errorf("no expectation is named %d", token)
	}
	b.heard <- c
	if b.w != c.w {
		return fmt.Errorf("the transaction runs on a connection another watch's Connector made, not the watch on %s's", b.w.path)
	}
	if before := c.w.beforeBind; before != nil {
		before(c)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.txCtx == nil:
		return errors.New("the connection runs no transaction")
	case c.txCtx.Err() != nil:
		return fmt.Errorf("the context the transaction began on is done (%w): its rollback is taken before the stop that makes it", context.Cause(c.txCtx))
	}
	b.attach(c)
	return nil
}

// bindTo binds attach's expectation to the connection tx runs on, which the
// watch's Connector must have made. tx runs a bindPragma carrying the
// binding's token, which that connection takes, before the driver sees it,
// and binds on this goroutine before ExecContext returns, asking the context
// tx began on, which the connection kept from its BeginTx (conn.bind):
// database/sql hands the driver no other context of the transaction's. The
// statement itself runs on context.Background, so that what refuses it is the
// transaction's own state (sql.ErrTxDone once database/sql has marked it
// done) or the context it began on, never another. It fails the test where tx
// cannot run the statement (it is done already), where the connection refuses
// the binding, and where no connection of the watch's took it: tx runs on a
// connection to another file, or on one its store opened without the watch's
// Connector. The token is pending until a connection takes it or bindTo
// returns.
func (w *RollbackWatch) bindTo(t testing.TB, tx *sql.Tx, attach func(*conn)) {
	t.Helper()
	b := &binding{w: w, attach: attach, heard: make(chan *conn, 1)}
	pending.Lock()
	pending.next++
	token := pending.next
	pending.binds[token] = b
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
	case <-b.heard:
	default:
		t.Fatalf("sqlitetest: the transaction runs on a connection the watch on %s did not make: one to another file, or one its store opened without the watch's Connector", w.path)
	}
}

// Rollback is the rollback of one transaction at the driver, on that
// transaction's own connection, taken before the stop that makes it
// (RollbackOf): database/sql's own once the transaction's context is done
// (Tx.awaitDone), or any other Rollback of it. It has run once the driver's
// Rollback, which database/sql calls only once it has marked the transaction
// done, has returned no error, SQLite having rolled the transaction back
// inside it; it has failed where that Rollback returned an error, which it
// keeps; and it can no longer come where the transaction committed, an error
// rolled it back before its ROLLBACK, or its connection closed first. One
// that came to an end before its wait began is kept. Another connection's
// rollback, a reader's on the same file included, is none of these.
type Rollback struct {
	w *RollbackWatch

	began    chan struct{} // closed once the driver's Rollback was called
	inSQLite chan struct{} // closed once SQLite rolled the transaction back inside it
	ran      chan struct{} // closed once it has run, and on no other end
	ended    chan struct{} // closed once it has run, failed or can no longer come, what it came to written first
	err      error         // the driver's error, where its Rollback returned one
	why      string        // why it can no longer come, where it cannot

	beginning, rolling, ending sync.Once
}

// RollbackOf takes the rollback of the transaction tx, which must still be
// open and not yet stopped, on its connection, as a test does before the stop
// that makes it. It runs a statement in tx that changes nothing (bindTo), and
// fails the test where tx does not run on a connection the watch's Connector
// made.
func (w *RollbackWatch) RollbackOf(t testing.TB, tx *sql.Tx) *Rollback {
	t.Helper()
	r := newRollback(w)
	w.bindTo(t, tx, func(c *conn) { c.rollbacks = append(c.rollbacks, r) })
	return r
}

func newRollback(w *RollbackWatch) *Rollback {
	return &Rollback{
		w: w, began: make(chan struct{}), inSQLite: make(chan struct{}),
		ran: make(chan struct{}), ended: make(chan struct{}),
	}
}

func (r *Rollback) begin()            { r.beginning.Do(func() { close(r.began) }) }
func (r *Rollback) rollBackInSQLite() { r.rolling.Do(func() { close(r.inSQLite) }) }

// returned is the driver's Rollback returning err, SQLite having rolled the
// transaction back inside it or not: the rollback has run only where both
// hold, and a Rollback that returned an error has failed with it.
func (r *Rollback) returned(err error, inSQLite bool) {
	switch {
	case err != nil:
		r.ending.Do(func() {
			r.err = err
			close(r.ended)
		})
	case !inSQLite:
		r.lose("the driver's Rollback returned no error, with SQLite having rolled nothing back inside it")
	default:
		r.ending.Do(func() {
			close(r.ran)
			close(r.ended)
		})
	}
}

func (r *Rollback) lose(why string) {
	r.ending.Do(func() {
		r.why = why
		close(r.ended)
	})
}

// Wait blocks until the transaction's rollback has run at the driver, and
// fails the test if it has failed or cannot come (the transaction committed,
// an error rolled it back first, or its connection closed), or has not come
// before the watch's waits give up.
func (r *Rollback) Wait(t testing.TB) {
	t.Helper()
	r.WaitUnless(t, nil)
}

// WaitUnless is Wait that answers false, without failing, if stopped is
// closed first, and true once the rollback has run; a rollback that has come
// to an end is answered first, whatever else is ready. A nil stopped never
// closes. Each time it decides to wait on, it answers the ask of a WaitProof
// under way on the watch.
func (r *Rollback) WaitUnless(t testing.TB, stopped <-chan struct{}) bool {
	t.Helper()
	giveUp := r.w.giveUp()
	for {
		asked, ask := r.w.proofAsks()
		select {
		case <-r.ended:
			return r.answer(t)
		default:
		}
		r.w.stillWaiting(asked)
		select {
		case <-r.ended:
			return r.answer(t)
		case <-stopped:
			return false
		case <-giveUp:
			t.Fatalf("sqlitetest: no rollback of the transaction on %s %s: %s", r.w.path, r.w.bound, r.where())
			return false
		case <-ask:
		}
	}
}

// answer is what a wait answers for a rollback that has come to an end: true
// where it has run, and a failure where it failed or can no longer come.
func (r *Rollback) answer(t testing.TB) bool {
	t.Helper()
	select {
	case <-r.ran:
		return true
	default:
	}
	if r.err != nil {
		t.Fatalf("sqlitetest: the driver's Rollback of the transaction on %s returned an error: %v", r.w.path, r.err)
	} else {
		t.Fatalf("sqlitetest: the transaction on %s can no longer roll back at the driver: %s", r.w.path, r.why)
	}
	return false
}

// where says how far a rollback that never came to an end went.
func (r *Rollback) where() string {
	select {
	case <-r.inSQLite:
		return "SQLite rolled the transaction back inside the driver's ROLLBACK, and the driver's Rollback never returned"
	default:
	}
	select {
	case <-r.began:
		return "the driver's Rollback was called, and SQLite never rolled the transaction back inside it"
	default:
	}
	return "the driver's Rollback was never called"
}

// RollbackHold holds the rollback of one transaction at the driver until the
// test lets it go: at its boundary, where database/sql, once it has marked
// the transaction done, has called the driver's Rollback and the driver has
// not yet run its ROLLBACK (HoldRollbackOf), or once SQLite has rolled the
// transaction back inside that ROLLBACK, before the statement ends and the
// driver's Rollback returns (HoldReturnOf).
type RollbackHold struct {
	w  *RollbackWatch
	at string // where it holds the rollback, for its failures

	reached chan struct{} // closed once the rollback waits at the hold
	out     chan struct{} // closed to let it go on
	gone    chan struct{} // closed, why written first, once it can no longer come
	why     string

	reaching, leaving, ending sync.Once
}

// HoldRollbackOf holds the next rollback of the transaction tx, which must
// still be open and not yet stopped, at its boundary, as RollbackOf takes it;
// the test's end lets a rollback it holds go.
func (w *RollbackWatch) HoldRollbackOf(t testing.TB, tx *sql.Tx) *RollbackHold {
	t.Helper()
	h := newHold(w, "at its boundary, before the driver's ROLLBACK")
	t.Cleanup(h.Release)
	w.bindTo(t, tx, func(c *conn) { c.atBoundary = append(c.atBoundary, h) })
	return h
}

// HoldReturnOf holds the next rollback of the transaction tx, which must
// still be open and not yet stopped, inside the driver's ROLLBACK once SQLite
// has rolled the transaction back, in its rollback hook, before the statement
// ends and the driver's Rollback returns, as RollbackOf takes it; the test's
// end lets a rollback it holds go.
func (w *RollbackWatch) HoldReturnOf(t testing.TB, tx *sql.Tx) *RollbackHold {
	t.Helper()
	h := newHold(w, "once SQLite rolled it back, before the driver's Rollback returned")
	t.Cleanup(h.Release)
	w.bindTo(t, tx, func(c *conn) { c.inSQLite = append(c.inSQLite, h) })
	return h
}

func newHold(w *RollbackWatch, at string) *RollbackHold {
	return &RollbackHold{w: w, at: at, reached: make(chan struct{}), out: make(chan struct{}), gone: make(chan struct{})}
}

// park is the rollback waiting at the hold, on the goroutine running it,
// until Release.
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

// Reached blocks until the transaction's rollback waits at the hold, and
// fails the test if it can no longer come or has not come before the watch's
// waits give up.
func (h *RollbackHold) Reached(t testing.TB) {
	t.Helper()
	select {
	case <-h.reached:
	case <-h.gone:
		t.Fatalf("sqlitetest: the held transaction on %s can no longer roll back at the driver: %s", h.w.path, h.why)
	case <-h.w.giveUp():
		t.Fatalf("sqlitetest: the held transaction's rollback on %s never reached its hold %s %s", h.w.path, h.at, h.w.bound)
	}
}

// Release lets a rollback the hold holds go on, and one not held yet pass it.
// Idempotent.
func (h *RollbackHold) Release() { h.leaving.Do(func() { close(h.out) }) }
