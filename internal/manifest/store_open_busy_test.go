package manifest

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// OpenStore tries its open again while SQLite answers it SQLITE_BUSY
// (openStoreDB). These tests answer its ping through openStorePing with
// errors the driver really returned (makeDriverErrors), so what is tried
// again is decided by the driver's own error type and codes.

// openStoreBusyMessage is the Info line OpenStore logs at its first busy
// answer.
const openStoreBusyMessage = "the database was busy as the store opened; trying again"

// driverErrors holds errors the driver returned, one of each kind these
// tests answer a ping with.
type driverErrors struct {
	busy         error // SQLITE_BUSY: a write while another connection holds the write lock, with no busy wait
	busySnapshot error // SQLITE_BUSY_SNAPSHOT: a write from a read snapshot another connection's commit made stale
	notBusy      error // SQLITE_ERROR: a query of a table that does not exist
}

// makeDriverErrors makes the driver return each error driverErrors holds,
// on a database of its own, and checks each one's code.
func makeDriverErrors(t *testing.T) driverErrors {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "errors.db")
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", dsn.File(path, "_pragma=journal_mode(WAL)&_pragma=busy_timeout(0)"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	a, b := open(), open()
	if _, err := a.Exec(`CREATE TABLE t (v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	holder, err := a.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	exec := func(stmt string) {
		t.Helper()
		if _, err := holder.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	var e driverErrors
	exec(`BEGIN IMMEDIATE`)
	_, e.busy = b.Exec(`INSERT INTO t VALUES (1)`)
	exec(`ROLLBACK`)

	exec(`BEGIN`)
	var n int
	if err := holder.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(`INSERT INTO t VALUES (2)`); err != nil {
		t.Fatal(err)
	}
	_, e.busySnapshot = holder.ExecContext(ctx, `INSERT INTO t VALUES (3)`)
	exec(`ROLLBACK`)

	_, e.notBusy = b.Exec(`SELECT v FROM no_such_table`)

	for _, c := range []struct {
		name string
		err  error
		code int
	}{
		{"busy", e.busy, sqlite3.SQLITE_BUSY},
		{"busy snapshot", e.busySnapshot, sqlite3.SQLITE_BUSY_SNAPSHOT},
		{"not busy", e.notBusy, sqlite3.SQLITE_ERROR},
	} {
		var se *sqlite.Error
		if !errors.As(c.err, &se) || se.Code() != c.code {
			t.Fatalf("%s: the driver returned %v, want a *sqlite.Error with code %d", c.name, c.err, c.code)
		}
	}
	return e
}

// pingScript counts OpenStore's pings of one database and keeps the
// *sql.DB each was handed.
type pingScript struct {
	calls int
	dbs   []*sql.DB
}

// scriptPings answers OpenStore's pings of the database at path until the
// test ends: the n-th (from 1) gets answer(n), or the real ping when that
// is nil. A ping of any other database gets the real ping.
func scriptPings(t *testing.T, path string, answer func(n int) error) *pingScript {
	t.Helper()
	p := &pingScript{}
	t.Cleanup(setOpenStorePing(func(at string, db *sql.DB) error {
		if at != path {
			return db.Ping()
		}
		p.calls++
		p.dbs = append(p.dbs, db)
		if err := answer(p.calls); err != nil {
			return err
		}
		return db.Ping()
	}))
	return p
}

// shortenBusyPauses gives OpenStore budget for its busy tries, with pauses
// of 1 ms doubling to 20 ms, until the test ends.
func shortenBusyPauses(t *testing.T, budget time.Duration) {
	t.Helper()
	prevBudget, prevPause, prevMax := openStoreBusyBudget, openStoreBusyPause, openStoreBusyPauseMax
	openStoreBusyBudget, openStoreBusyPause, openStoreBusyPauseMax = budget, time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() {
		openStoreBusyBudget, openStoreBusyPause, openStoreBusyPauseMax = prevBudget, prevPause, prevMax
	})
}

// requireClosed fails the test unless every *sql.DB in dbs was closed.
func requireClosed(t *testing.T, dbs []*sql.DB) {
	t.Helper()
	for i, db := range dbs {
		if err := db.Ping(); err == nil || !strings.Contains(err.Error(), "database is closed") {
			t.Errorf("try %d left its *sql.DB open (a ping answered %v)", i+1, err)
		}
	}
}

// fakeOpenStoreClock is a clock for OpenStore's busy tries that moves only
// when the open sleeps on it, by the pause it sleeps, and at once.
type fakeOpenStoreClock struct {
	at time.Time
}

// now returns the clock's time.
func (c *fakeOpenStoreClock) now() time.Time { return c.at }

// sleep moves the clock on by d.
func (c *fakeOpenStoreClock) sleep(d time.Duration) { c.at = c.at.Add(d) }

// useFakeOpenStoreClock runs OpenStore's busy tries on a fakeOpenStoreClock
// until the test ends.
func useFakeOpenStoreClock(t *testing.T) *fakeOpenStoreClock {
	t.Helper()
	c := &fakeOpenStoreClock{at: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	t.Cleanup(setOpenStoreClock(c.now, c.sleep))
	return c
}

// TestOpenStoreTriesABusyPingAgain: a ping SQLite answers busy, by the
// plain code or an extended one, is tried again on a new *sql.DB, and the
// failed one is closed. The store opens on the *sql.DB whose ping
// answered, migrated, and the retry is logged once, at Info.
func TestOpenStoreTriesABusyPingAgain(t *testing.T) {
	e := makeDriverErrors(t)
	shortenBusyPauses(t, 5*time.Second)
	rec := loggingtest.Record(t)
	path := filepath.Join(t.TempDir(), "bridge.db")
	busy := []error{e.busy, e.busySnapshot, e.busy}
	pings := scriptPings(t, path, func(n int) error {
		if n <= len(busy) {
			return busy[n-1]
		}
		return nil
	})

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore after %d busy pings: %v", len(busy), err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if pings.calls != len(busy)+1 {
		t.Fatalf("pinged %d times, want %d: one for each busy answer and the one that opened", pings.calls, len(busy)+1)
	}
	requireClosed(t, pings.dbs[:len(busy)])
	if pings.dbs[len(busy)] != s.db {
		t.Error("the store holds a *sql.DB other than the one whose ping answered")
	}
	if v := readUserVersion(t, s.db); v != latestVersion {
		t.Errorf("user_version %d, want %d: the store opened without its migration", v, latestVersion)
	}
	if lines := rec.Lines(openStoreBusyMessage); len(lines) != 1 || !strings.HasPrefix(lines[0], "INFO ") {
		t.Errorf("logged %q, want one Info line", lines)
	}
}

// TestOpenStoreGivesUpOnABusyPingPastItsBudget: a ping that stays busy
// fails the open with that busy error once openStoreBusyBudget has passed
// since the first busy answer, and no later than one pause past it, after
// the tries the pause schedule gives; every try's *sql.DB is closed. The
// open runs on a fake clock that moves only by the pauses it sleeps, so
// the bounds are the schedule's and not the host's. Production's schedule
// pauses 10, 20, 40, 80 and 160 ms, then 250 ms eighteen times, then the
// 190 ms left of its 5 s: 24 pauses, so 25 pings.
func TestOpenStoreGivesUpOnABusyPingPastItsBudget(t *testing.T) {
	if openStoreBusyBudget != 5*time.Second || openStoreBusyPause != 10*time.Millisecond || openStoreBusyPauseMax != 250*time.Millisecond {
		t.Fatalf("the schedule is %s of pauses from %s doubling to %s; this test counts the tries of 5s from 10ms to 250ms",
			openStoreBusyBudget, openStoreBusyPause, openStoreBusyPauseMax)
	}
	const wantPings = 25
	// Past this many pings the script answers an error that is not busy, so
	// an open with no limit fails the test instead of hanging it.
	const pingCap = 1000
	e := makeDriverErrors(t)
	clock := useFakeOpenStoreClock(t)
	path := filepath.Join(t.TempDir(), "bridge.db")
	var firstBusy time.Time
	pings := scriptPings(t, path, func(n int) error {
		if n == 1 {
			firstBusy = clock.now()
		}
		if n > pingCap {
			return errors.New("the open was still trying past the test's cap")
		}
		return e.busy
	})

	s, err := OpenStore(path)
	if err == nil {
		_ = s.Close()
		t.Fatal("OpenStore opened while every ping was busy")
	}
	tried := clock.now().Sub(firstBusy)
	if tried < openStoreBusyBudget {
		t.Errorf("gave up %s after the first busy answer, inside its %s budget", tried, openStoreBusyBudget)
	}
	if limit := openStoreBusyBudget + openStoreBusyPauseMax; tried > limit {
		t.Errorf("still trying %s after the first busy answer, past its %s budget and one %s pause",
			tried, openStoreBusyBudget, openStoreBusyPauseMax)
	}
	if pings.calls != wantPings {
		t.Errorf("pinged %d times, want %d: the first try and one after each pause the schedule gives", pings.calls, wantPings)
	}
	if !errors.Is(err, e.busy) {
		t.Errorf("err = %v, want the busy error", err)
	}
	requireClosed(t, pings.dbs)
}

// TestOpenStoreTriesAgainAfterABusyAnswerSlowerThanItsBudget: the budget
// runs from the first busy answer, not from the first try. A plain busy
// answer at the open comes only once busy_timeout's own 5 s have run out
// (openStoreDB), so a budget counted from the first try would be spent
// before it could try again. Here the first ping takes twice the budget
// before it answers busy, and the open still tries again and opens.
func TestOpenStoreTriesAgainAfterABusyAnswerSlowerThanItsBudget(t *testing.T) {
	e := makeDriverErrors(t)
	const budget = 200 * time.Millisecond
	shortenBusyPauses(t, budget)
	path := filepath.Join(t.TempDir(), "bridge.db")
	pings := scriptPings(t, path, func(n int) error {
		if n == 1 {
			time.Sleep(2 * budget)
			return e.busy
		}
		return nil
	})

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore after a busy answer that took %s: %v", 2*budget, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if pings.calls != 2 {
		t.Errorf("pinged %d times, want 2", pings.calls)
	}
}

// TestOpenStoreFailsAtOnceOnAPingErrorThatIsNotBusy: a ping that fails
// with any other error, SQLite's or not, fails the open on its first try:
// nothing is tried again and nothing is logged about a busy database. The
// second case's text is SQLite's busy message; the code decides, not the
// text.
func TestOpenStoreFailsAtOnceOnAPingErrorThatIsNotBusy(t *testing.T) {
	e := makeDriverErrors(t)
	shortenBusyPauses(t, time.Second)
	rec := loggingtest.Record(t)
	for _, c := range []struct {
		name string
		err  error
	}{
		{"SQLITE_ERROR", e.notBusy},
		{"an error that is not SQLite's", errors.New("database is locked")},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bridge.db")
			pings := scriptPings(t, path, func(int) error { return c.err })
			s, err := OpenStore(path)
			if err == nil {
				_ = s.Close()
				t.Fatal("OpenStore opened on a failed ping")
			}
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want the ping's error", err)
			}
			if pings.calls != 1 {
				t.Fatalf("pinged %d times, want 1: an error that is not busy was tried again", pings.calls)
			}
			requireClosed(t, pings.dbs)
		})
	}
	if lines := rec.Lines(openStoreBusyMessage); len(lines) != 0 {
		t.Errorf("logged %q for errors that are not busy", lines)
	}
}
