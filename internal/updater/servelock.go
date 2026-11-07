package updater

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// ServeLockFileName is the file in a data dir on which a running `bridge
// serve` holds an exclusive kernel lock from before its first write there
// until it returns (cmd/bridge's lockServeDataDir, backlog B208), and which
// `bridge restore`, `reset-player`, `manifest clear-missing` and `output dac
// bind` hold for their runs. It says nothing of whether a start is counted:
// a command holds it too (backlog B660's third round, StartHold).
const ServeLockFileName = "server.lock"

// startLockPrefix and startLockSuffix frame the name of a counted start's own
// lock file in the data dir, serve-start-<token>.lock (StartHold).
const (
	startLockPrefix = "serve-start-"
	startLockSuffix = ".lock"
)

// startLockName is the lock file of the start whose token is token.
func startLockName(token string) string { return startLockPrefix + token + startLockSuffix }

// isStartToken reports whether token is one a start makes: 32 lower-case hex
// digits, so that a token read from the marker names a file in the data dir
// and nothing else.
func isStartToken(token string) bool {
	if len(token) != 32 {
		return false
	}
	for _, c := range token {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// StartHold is a counted start's hold on a lock of its own (backlog B660's
// third round): serve's admission makes a random token, takes the lock its
// token names (serve-start-<token>.lock) on a file made for it, then writes the
// token into the install marker with the start's count (State.RunningStart),
// and the start holds it until it returns. Only a counted start takes such a
// lock, so the count holds a start running now (RunningServeCounted) only
// where the marker names a token whose lock is held: never by the data dir's
// lock, which a command holds too, and never after a crash or a kill, whose
// lock the kernel lets go with the process. No process liveness is read.
type StartHold struct {
	dataDir, token string

	mu sync.Mutex
	f  *os.File
}

// newStartHold makes a fresh token and takes the lock it names. A lock that
// cannot be taken leaves no file.
func newStartHold(dataDir string) (*StartHold, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(raw[:])
	f, err := fsutil.OpenLockFile(dataDir, startLockName(token))
	if err != nil {
		return nil, err
	}
	if err := fsutil.TryLock(f); err != nil {
		_ = f.Close()
		_ = os.Remove(filepath.Join(dataDir, startLockName(token)))
		return nil, err
	}
	return &StartHold{dataDir: dataDir, token: token, f: f}, nil
}

// Token is the start's token, "" for none.
func (h *StartHold) Token() string {
	if h == nil {
		return ""
	}
	return h.token
}

// Release lets go of the start's lock and removes its file. Safe on nil and
// more than once.
func (h *StartHold) Release() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f == nil {
		return
	}
	_ = fsutil.Unlock(h.f)
	_ = h.f.Close()
	h.f = nil
	_ = os.Remove(filepath.Join(h.dataDir, startLockName(h.token)))
}

// startRunning reports whether the start whose token is token holds its lock
// now: a try of the lock the token names, which finds it held. It never waits,
// so it takes no part in the order the locks are waited for in (server.lock,
// the ladder, update-state.lock), and a try that finds the lock free lets go
// of it at once. A token that is not a start's, a lock file that is not
// there, and one that cannot be opened or tried, read as none; it makes
// nothing.
func startRunning(dataDir, token string) bool {
	if !isStartToken(token) {
		return false
	}
	f, err := os.Open(filepath.Join(dataDir, startLockName(token)))
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

// sweepStartLocks removes the lock files of starts that hold them no more (a
// start killed, or a removal that failed), which a counted start's admission
// does before it makes its own: a file whose lock is held, and one that cannot
// be opened or tried, stays.
func sweepStartLocks(dataDir string) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		token, ok := strings.CutPrefix(name, startLockPrefix)
		if !ok {
			continue
		}
		if token, ok = strings.CutSuffix(token, startLockSuffix); !ok || !isStartToken(token) || !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dataDir, name)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		if fsutil.TryLock(f) != nil {
			_ = f.Close()
			continue
		}
		_ = fsutil.Unlock(f)
		_ = f.Close()
		_ = os.Remove(path)
	}
}

// RunningServeCounted reports whether the marker st in dataDir counts the
// start of a serve running now (backlog B660's rounds 2 and 3): an install of
// current awaiting its confirmation, a start counted, and the start whose
// token the marker names holding the lock that token names (StartHold),
// which is tried only then, so a command or a doctor that has no count to
// read tries no lock. The token and the count come from the one read st, so
// a caller that holds the install marker's lock reads them as they stand
// together (disarm): a take-back lowers the count and clears the token in one
// write.
func RunningServeCounted(dataDir string, st State, current string) bool {
	return st.FailedStarts > 0 && st.RunningStart != "" && AwaitsConfirmation(st, current) &&
		startRunning(dataDir, st.RunningStart)
}

// MigrationHeldIn is MigrationHeld for a store open of current in dataDir
// over the marker st, the start of a serve running now told by
// RunningServeCounted.
func MigrationHeldIn(dataDir string, st State, current string) string {
	return MigrationHeld(st, current, RunningServeCounted(dataDir, st, current))
}

// EndStart clears the token of the start whose token is token from dataDir's
// install marker, where the marker still names it, and keeps the start's
// count (backlog B660's third round): serve calls it as each counted start
// returns, past its take-back, which clears the token itself where it lands,
// so a start that returned is never read as running, the launcher's serve in
// a process that lives on included. ctx bounds the wait for the install
// marker's lock; on a filesystem that keeps none it goes on without it, as a
// take-back does. It reports whether it wrote.
func EndStart(ctx context.Context, dataDir, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	if st, err := LoadState(dataDir); err == nil && st.RunningStart != token {
		return false, nil
	}
	lock, err := LockState(ctx, dataDir)
	if err != nil {
		return false, err
	}
	defer lock.Release()
	st, err := LoadState(dataDir)
	if err != nil {
		return false, fmt.Errorf("the install marker cannot be read: %w", markerIOError{err})
	}
	if st.RunningStart != token {
		return false, nil
	}
	st.RunningStart = ""
	if err := SaveState(dataDir, st); err != nil {
		return false, markerIOError{err}
	}
	return true, nil
}
