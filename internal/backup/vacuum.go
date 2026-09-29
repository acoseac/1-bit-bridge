package backup

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// A snapshot copies the manifest database with VACUUM INTO, which serve's
// startup and scheduled snapshots run on the context a shutdown cancels.
// The driver stops a running statement with sqlite3_interrupt, which SQLite
// reads only between the steps of a statement that is running, so a
// cancelled snapshot went on in three places (backlog B63, measured
// 2026-09-29; the record is in ops/engineering-log.md):
//
//   - The commit. VACUUM INTO builds its output in the page cache it takes
//     from this connection, and the pages still there at the end are written
//     by the commit, which reads no interrupt. With SQLite's default 2 MB
//     cache a smaller database is written whole by the commit: of 3,818
//     cancels that landed while the statement ran, 2,053 finished the copy
//     anyway (an 820 KB database on the dev Mac), and on a Windows host under
//     disk contention the commit spent 2.3 s writing a 316 KB copy after its
//     cancel, which then was thrown away. That is what held a serve test's
//     snapshot past runServe's return on the Windows CI runner.
//     snapshotSourceQuery's cache_size(-64) makes the copy write as it goes,
//     between the steps an interrupt stops, and leaves the commit 64 KB:
//     220 of 3,722 finished anyway.
//   - A lock. busy_timeout(5000) handed a locked source to SQLite's busy
//     handler, which sleeps through the interrupt: a cancel 300 ms into the
//     wait was answered by "database is locked" 5 s later. SQLite now waits
//     in place for a moment only, and retryWhileBusy waits out a longer lock,
//     which a cancel ends.
//   - The start. The driver checks the context before it runs the statement
//     and then arms an interrupt, and one that lands before the statement
//     starts is cleared by it (sqlite3Step resets the flag when no other
//     statement is active), so that VACUUM copies the whole database. Rare
//     (2 of 4,000 cancels timed at random across the statement) and the
//     whole copy when it happens, so the statement checks the context
//     itself once it is running: its INTO target is vacuumTarget, which
//     refuses a cancelled snapshot before anything is copied.

// snapshotSourceQuery opens the manifest database for VACUUM INTO: read-only,
// so it cannot disturb the running writer, with SQLite's own busy wait kept
// to 100 ms (retryWhileBusy waits out a longer lock) and a 64 KB page cache,
// which VACUUM INTO gives its output too.
//
// The cache size costs a snapshot nothing measurable: VACUUM INTO of a
// 118 MB database took 326 to 698 ms whatever the cache, from -16 to the
// default -2000, on the dev Mac.
const snapshotSourceQuery = "mode=ro&_pragma=busy_timeout(100)&_pragma=cache_size(-64)"

// snapshotBusyPatience is how long a snapshot waits, in all, for a database
// another connection holds locked: what busy_timeout(5000) gave it before.
const snapshotBusyPatience = 5 * time.Second

// vacuumTargetFunc is the SQL function the snapshot's VACUUM INTO names its
// output with (vacuumTarget); vacuumIntoSQL calls it.
const vacuumTargetFunc = "backup_vacuum_target"

// vacuumIntoSQL is the snapshot's statement: the output file is the
// function's first argument, and the second names the vacuumCall whose
// context the function checks. One literal, and vacuumTargetFunc spelled
// out in it (TestTheVacuumStatementCallsItsTargetFunction).
const vacuumIntoSQL = "VACUUM INTO backup_vacuum_target(?, ?)"

func init() {
	// A connection learns its functions when it opens, and modernc reads
	// the list without a lock, so the function is registered before any
	// connection can open.
	sqlite.MustRegisterScalarFunction(vacuumTargetFunc, 2, vacuumTarget)
}

// vacuumCall is one VACUUM INTO in flight: the context its snapshot runs on,
// and whether vacuumTarget refused it.
type vacuumCall struct {
	ctx     context.Context
	refused atomic.Bool
}

// vacuumCalls holds every vacuumCall in flight by the id its statement
// passes vacuumTarget.
var vacuumCalls struct {
	mu   sync.Mutex
	next int64
	byID map[int64]*vacuumCall
}

// startVacuumCall records a VACUUM INTO about to run on ctx, and returns it
// with its id and the function that forgets it.
func startVacuumCall(ctx context.Context) (call *vacuumCall, id int64, end func()) {
	call = &vacuumCall{ctx: ctx}
	vacuumCalls.mu.Lock()
	defer vacuumCalls.mu.Unlock()
	if vacuumCalls.byID == nil {
		vacuumCalls.byID = make(map[int64]*vacuumCall)
	}
	vacuumCalls.next++
	id = vacuumCalls.next
	vacuumCalls.byID[id] = call
	return call, id, func() {
		vacuumCalls.mu.Lock()
		delete(vacuumCalls.byID, id)
		vacuumCalls.mu.Unlock()
	}
}

// vacuumTarget is the INTO target of vacuumIntoSQL: it answers the output
// path it is given, unless the snapshot's context is done, when it fails
// the statement. SQLite evaluates it once the statement is running and
// before OP_Vacuum copies anything, so a cancel the driver's interrupt did
// not reach stops the VACUUM here. One that lands after this check reaches
// a running statement, where the interrupt holds.
func vacuumTarget(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	id, _ := args[1].(int64)
	vacuumCalls.mu.Lock()
	call := vacuumCalls.byID[id]
	vacuumCalls.mu.Unlock()
	if call == nil {
		return nil, errors.New("backup: VACUUM INTO target named no snapshot in flight")
	}
	if err := call.ctx.Err(); err != nil {
		call.refused.Store(true)
		return nil, err
	}
	return args[0], nil
}

// vacuumInto runs SQLite's VACUUM INTO to make a clean atomic copy of a
// WAL-mode database, and stops when ctx is cancelled, wherever the copy is
// but in the commit's last 64 KB (the notes at the top of this file).
func vacuumInto(ctx context.Context, srcDB, dstDB string) error {
	db, err := sql.Open("sqlite", dsn.File(srcDB, snapshotSourceQuery))
	if err != nil {
		return err
	}
	defer db.Close()
	call, id, end := startVacuumCall(ctx)
	defer end()
	err = retryWhileBusy(ctx, func() error {
		// VACUUM INTO refuses if the destination already exists; clear it
		// so re-running a snapshot in the same second (collision-suffix
		// paths), or an attempt after a locked one, doesn't bail. The
		// parent dir is already 0700.
		_ = os.Remove(dstDB)
		// SQLite, not this process, creates the destination, so as root
		// it was root's. VACUUM INTO accepts an EMPTY file as well as none,
		// and writes into it, so an empty one precreated with the snapshot
		// directory's owner keeps that owner. A no-op unless this process
		// is root.
		if err := fsutil.Precreate(dstDB, 0o600, dstDB); err != nil {
			return err
		}
		_, err := db.ExecContext(ctx, vacuumIntoSQL, dstDB, id)
		if call.refused.Load() {
			// One %w: ctxerr reads this as the cancellation it wraps.
			return fmt.Errorf("stopped as its VACUUM started: %w", ctx.Err())
		}
		return err
	})
	if err != nil {
		// A failed VACUUM INTO can leave a partial/corrupt fragment on
		// disk. Remove it so the snapshot dir doesn't accumulate broken
		// DB files (and a later reader can't mistake one for a good copy).
		_ = os.Remove(dstDB)
		return err
	}
	// VACUUM INTO writes the destination at the umask-default mode
	// (typically 0644). The file contains hashed-token references
	// and is sensitive enough to warrant the same 0600 the rest of
	// the snapshot uses. Chmod after the write so a tester reading
	// `ls -l` sees consistent perms across the bundle.
	if err := os.Chmod(dstDB, 0o600); err != nil {
		// Don't leave a 0644 copy of token-hash data behind if we
		// couldn't lock it down — unlink it and surface the error.
		_ = os.Remove(dstDB)
		return err
	}
	return nil
}

// retryWhileBusy runs attempt until it answers something other than
// SQLITE_BUSY, waiting between tries for up to snapshotBusyPatience in all,
// and gives up at once when ctx is done: the wait SQLite's busy handler
// would make sleeps through a cancel. When the patience runs out it answers
// the last busy error, as busy_timeout did.
func retryWhileBusy(ctx context.Context, attempt func() error) error {
	giveUp := time.Now().Add(snapshotBusyPatience)
	pause := 10 * time.Millisecond
	for {
		err := attempt()
		if !isBusy(err) {
			return err
		}
		// The attempt was refused a lock, not stopped: a snapshot the
		// shutdown cancelled meanwhile ends here rather than trying again.
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		left := time.Until(giveUp)
		if left <= 0 {
			return err
		}
		wait := time.NewTimer(min(pause, left))
		select {
		case <-ctx.Done():
			wait.Stop()
			return ctx.Err()
		case <-wait.C:
		}
		pause = min(2*pause, 250*time.Millisecond)
	}
}

// isBusy reports whether err is SQLite's SQLITE_BUSY, extended codes
// (BUSY_RECOVERY, BUSY_SNAPSHOT, BUSY_TIMEOUT) included.
func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}
