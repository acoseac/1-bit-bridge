package updater

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// ServeLockFileName is the file in a data dir on which a running `bridge
// serve` holds an exclusive kernel lock from before its first write there
// until it returns (cmd/bridge's lockServeDataDir, backlog B208), and which
// `bridge restore`, `reset-player`, `manifest clear-missing` and `output dac
// bind` hold for their runs.
const ServeLockFileName = "server.lock"

// dataDirHolds is what this process holds dataDir's lock as (NoteDataDirLock):
// for each data dir, the holds a serve took and the holds a command took.
var dataDirHolds = struct {
	sync.Mutex
	serve, command map[string]int
}{serve: map[string]int{}, command: map[string]int{}}

// NoteDataDirLock records that this process holds dataDir's lock
// (ServeLockFileName), as a serve where serve is true and as a command
// otherwise, until release: ServeHoldsDataDir answers from it for this
// process, since a probe of the lock cannot tell this process's own serve
// from its own restore.
func NoteDataDirLock(dataDir string, serve bool) (release func()) {
	key := filepath.Clean(dataDir)
	dataDirHolds.Lock()
	holds := dataDirHolds.command
	if serve {
		holds = dataDirHolds.serve
	}
	holds[key]++
	dataDirHolds.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			dataDirHolds.Lock()
			defer dataDirHolds.Unlock()
			if holds[key]--; holds[key] <= 0 {
				delete(holds, key)
			}
		})
	}
}

// RunningServeCounted reports whether the marker st in dataDir counts the
// start of a serve running now (backlog B660's second round): an install of
// current awaiting its confirmation, a start counted, and a serve holding the
// data dir's lock (ServeHoldsDataDir), which is asked only then, so a command
// or a doctor that has no count to read tries no lock.
func RunningServeCounted(dataDir string, st State, current string) bool {
	return st.FailedStarts > 0 && AwaitsConfirmation(st, current) && ServeHoldsDataDir(dataDir)
}

// MigrationHeldIn is MigrationHeld for a store open of current in dataDir
// over the marker st, the start of a serve running now told by
// RunningServeCounted.
func MigrationHeldIn(dataDir string, st State, current string) string {
	return MigrationHeld(st, current, RunningServeCounted(dataDir, st, current))
}

// ServeHoldsDataDir reports whether a `bridge serve` holds dataDir's lock now
// (backlog B660's second round): this process's own, by what it noted
// (NoteDataDirLock), and another process's by a try of the lock, which finds
// it held. It never waits, so it takes no part in the order the locks are
// waited for in (server.lock, the ladder, update-state.lock); a try that
// finds the lock free lets go of it at once (what that leaves is backlog
// B760). A lock file that is not there, or that cannot be opened or tried,
// reads as none.
func ServeHoldsDataDir(dataDir string) bool {
	key := filepath.Clean(dataDir)
	dataDirHolds.Lock()
	serve, command := dataDirHolds.serve[key], dataDirHolds.command[key]
	dataDirHolds.Unlock()
	switch {
	case serve > 0:
		return true
	case command > 0:
		return false
	}
	f, err := os.Open(filepath.Join(dataDir, ServeLockFileName))
	if err != nil {
		return false
	}
	defer f.Close()
	if err := fsutil.TryLock(f); err != nil {
		return errors.Is(err, fsutil.ErrLocked)
	}
	_ = fsutil.Unlock(f)
	return false
}
