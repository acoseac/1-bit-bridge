package manifest

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// openStoreBusyBudget bounds how long OpenStore goes on trying its open
// once SQLite has answered it SQLITE_BUSY, and openStoreBusyPause is the
// first pause between tries, doubling to openStoreBusyPauseMax. Variables
// so tests can shorten them.
var (
	openStoreBusyBudget   = 5 * time.Second
	openStoreBusyPause    = 10 * time.Millisecond
	openStoreBusyPauseMax = 250 * time.Millisecond
)

// openStorePing, when set, pings in place of db.Ping on each of
// OpenStore's tries: tests answer SQLITE_BUSY from it. Nil in production.
var (
	openStorePingMu sync.Mutex
	openStorePing   func(path string, db *sql.DB) error
)

func setOpenStorePing(fn func(path string, db *sql.DB) error) func() {
	openStorePingMu.Lock()
	prev := openStorePing
	openStorePing = fn
	openStorePingMu.Unlock()
	return func() {
		openStorePingMu.Lock()
		openStorePing = prev
		openStorePingMu.Unlock()
	}
}

// pingStore pings db, opened on the database at path, through
// openStorePing when a test has set it.
func pingStore(path string, db *sql.DB) error {
	openStorePingMu.Lock()
	fn := openStorePing
	openStorePingMu.Unlock()
	if fn != nil {
		return fn(path, db)
	}
	return db.Ping()
}

// openStoreNow and openStoreSleep are the clock openStoreDB reads and
// sleeps on between its busy tries: time.Now and time.Sleep, unless a test
// has set another through setOpenStoreClock.
var (
	openStoreClockMu sync.Mutex
	openStoreNow     = time.Now
	openStoreSleep   = time.Sleep
)

func setOpenStoreClock(now func() time.Time, sleep func(time.Duration)) func() {
	openStoreClockMu.Lock()
	prevNow, prevSleep := openStoreNow, openStoreSleep
	openStoreNow, openStoreSleep = now, sleep
	openStoreClockMu.Unlock()
	return func() {
		openStoreClockMu.Lock()
		openStoreNow, openStoreSleep = prevNow, prevSleep
		openStoreClockMu.Unlock()
	}
}

// openStoreClock returns the clock openStoreDB runs its busy tries on. An
// open reads it once, as it starts.
func openStoreClock() (now func() time.Time, sleep func(time.Duration)) {
	openStoreClockMu.Lock()
	defer openStoreClockMu.Unlock()
	return openStoreNow, openStoreSleep
}

// openStoreDB opens the database at path through uri and pings it, which
// opens the first connection and runs the DSN's pragmas. While SQLite
// answers that with SQLITE_BUSY (isSQLiteBusy), it closes that *sql.DB and
// tries again, pausing openStoreBusyPause, then twice as long each time up
// to openStoreBusyPauseMax, until openStoreBusyBudget has passed since the
// first busy answer, and then fails with the last one. The pauses and the
// budget run on openStoreClock's clock. Any other error fails at once. The
// first busy answer is logged at Info.
//
// OpenStore holds the ladder lock through the open, the ping and the
// migration, so two openers never overlap there, but one opener's Close,
// after its OpenStore has returned, can overlap the next one's open:
// TestConcurrentOpensOfAnOlderDatabaseAllSucceed failed once on the
// windows-latest leg (2026-10-21) with "ping sqlite: database is locked
// (5) (SQLITE_BUSY)". SQLite (3.53.4, read in modernc's translation)
// retries every SQLITE_BUSY a new connection's pragmas can meet through
// busy_timeout's handler, and contention on the WAL index comes back as
// WAL_RETRY or BUSY_RECOVERY, so that plain code 5 came after the
// handler's 5 s had run out, most likely against the database file's own
// lock. The last connection to close a WAL database holds that lock
// EXCLUSIVE from before its checkpoint until it has deleted -shm and -wal
// (sqlite3WalClose), and Windows retries each delete for up to 1.4 s while
// another handle has the file open. The budget runs from the first busy
// answer, not the first try, so the next try waits busy_timeout's 5 s
// again. The same overlap comes when `bridge scan` or `bridge doctor`
// opens the store while `bridge serve` closes or reopens it. The
// migration's statements are not tried again: nothing has failed there.
func openStoreDB(path, uri string) (*sql.DB, error) {
	now, sleep := openStoreClock()
	var giveUp time.Time
	pause, said := openStoreBusyPause, false
	for {
		db, err := sql.Open("sqlite", uri)
		if err != nil {
			return nil, fmt.Errorf("open sqlite: %w", err)
		}
		// A single connection for writes keeps WAL + busy_timeout friendly.
		// Reads are fine via the default pool; modernc.org/sqlite uses the
		// standard database/sql connection pool.
		err = pingStore(path, db)
		if err == nil {
			return db, nil
		}
		db.Close() // release the handle + background goroutines; mirrors the migrate() error path in OpenStore
		if !isSQLiteBusy(err) {
			return nil, storeOpenErr(path, "ping sqlite", err)
		}
		at := now()
		if giveUp.IsZero() {
			giveUp = at.Add(openStoreBusyBudget)
		}
		left := giveUp.Sub(at)
		if left <= 0 {
			return nil, storeOpenErr(path, "ping sqlite", err)
		}
		if !said {
			said = true
			logger.Info("the database was busy as the store opened; trying again",
				"db", filepath.Base(path), "budget", openStoreBusyBudget, "err", err)
		}
		sleep(min(pause, left))
		pause = min(2*pause, openStoreBusyPauseMax)
	}
}

// isSQLiteBusy reports whether err is SQLite's SQLITE_BUSY by its primary
// code, so the extended codes (BUSY_RECOVERY, BUSY_SNAPSHOT, BUSY_TIMEOUT)
// count too.
func isSQLiteBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}
