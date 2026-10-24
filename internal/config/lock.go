package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// LockWait is how long a writer of a config file waits for the file's lock
// (LockFile) before it refuses. An ordinary turn is milliseconds: one read,
// one staged write with its fsync, one rename, and on Windows at most
// atomicwrite's 750 ms of rename retries. The long turns are a CLI's own,
// library add and remove changing the library index under the lock and a
// restore copying a snapshot, and a writer behind one of those for longer
// than this is better told to try again than left waiting: a console save
// is a request someone is watching. SQLite's busy wait is the same 5 s, so a
// console request waiting on either is bounded alike.
const LockWait = 5 * time.Second

// lockRetry is how often LockFile asks again while another writer holds the
// lock.
const lockRetry = 25 * time.Millisecond

// ErrBusy is what a *BusyError matches: another writer held the lock for
// the whole of the wait.
var ErrBusy = errors.New("config: another writer holds the config file's lock")

// BusyError is LockFile's refusal once the wait is over. It names the config
// file and nothing it holds.
type BusyError struct {
	// Path is the config file.
	Path string
	// Wait is how long LockFile waited.
	Wait time.Duration
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("another bridge command or the running bridge is changing %s, and held its lock for longer than %s, "+
		"so nothing was written: try again", e.Path, e.Wait)
}

// Is makes a *BusyError match ErrBusy.
func (e *BusyError) Is(target error) bool { return target == ErrBusy }

// LockFileName is the name of the lock file beside the config file at path:
// `.bridge.yaml.lock` beside bridge.yaml. Hidden, as Save's staged files
// beside it are, and never removed: only the lock on it means anything.
func LockFileName(path string) string {
	return "." + filepath.Base(path) + ".lock"
}

// FileLock is one writer's turn at the config file at its path: the lock
// LockFile took, which Save and SaveIf write under and Unlock gives back.
type FileLock struct {
	path string
	// f is the open lock file, nil when no lock was taken.
	f *os.File
	// unheld is why no lock is held although LockFile answered: a
	// filesystem that keeps none.
	unheld error
	// released is set once Unlock has run.
	released bool
}

// LockFile takes the lock every writer of the config file at path holds
// around its read-modify-write (backlog B506), and returns the turn it
// took. The writer then reads the file again, changes what it read, saves
// it through Save or SaveIf with this lock, and calls Unlock: so two writers
// take turns, and neither saves a copy read before its turn. Every writer of
// bridge.yaml in this tree takes it: library add and remove (Edit), bridge
// init's save, serve --init-if-missing's seed, bridge output dac bind, a
// restore, and serve's console saves (RuntimeConfig.Update).
//
// The lock is an exclusive kernel lock (fsutil.TryLock: flock on unix,
// LockFileEx on Windows) on the file LockFileName names, beside the config
// file. Not on the config file itself: Save replaces that by a rename, which
// a lock on the old file would not follow, and on Windows a locked range
// refuses every other handle's reads, and the config is read by everything.
// Not in the data dir: the config names its data dir, which a writer can
// change, and init and serve's seed write a config before any data dir is
// known. It is held only around one read-modify-write, never for a
// process's life, so serve's console saves and a CLI writer beside a running
// serve take turns. A process that holds both this lock and the data dir's
// (lockServeDataDir in cmd/bridge: bind, restore, and serve itself) takes
// the data dir's first and this one inside it.
//
// LockFile waits for another writer for up to LockWait, asking every
// lockRetry, then refuses with a *BusyError, having taken nothing. An error
// opening the lock file (a directory this user cannot write, which Save
// could not write in either) is returned as it is. A filesystem that keeps
// no locks (some NFS and SMB mounts) is not a refusal: the turn is taken
// without the lock, said once per file in the log, and the writers there are
// back to their own byte comparisons (SaveIf, the console's check of the
// file it last read), as before the lock.
func LockFile(path string) (*FileLock, error) {
	return lockFile(path, fsutil.TryLock, LockWait)
}

// lockFile is LockFile with the try and the wait as parameters, for its
// tests.
func lockFile(path string, try func(*os.File) error, wait time.Duration) (*FileLock, error) {
	f, err := fsutil.OpenLockFile(filepath.Dir(path), LockFileName(path))
	if err != nil {
		return nil, fmt.Errorf("open the lock beside %s: %w", path, err)
	}
	deadline := time.Now().Add(wait)
	for {
		err := try(f)
		if err == nil {
			return &FileLock{path: path, f: f}, nil
		}
		if !errors.Is(err, fsutil.ErrLocked) {
			_ = f.Close()
			warnUnlocked(path, err)
			return &FileLock{path: path, unheld: err}, nil
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, &BusyError{Path: path, Wait: wait}
		}
		time.Sleep(lockRetry)
	}
}

// Path is the config file this turn is at.
func (l *FileLock) Path() string { return l.path }

// Unheld is why no lock is held for this turn, nil when it is: a filesystem
// that keeps no locks.
func (l *FileLock) Unheld() error { return l.unheld }

// Unlock gives the lock back. It unlocks before it closes: on Windows the
// release a close makes can lag, and the next writer would find it held.
// Calling it again does nothing.
func (l *FileLock) Unlock() {
	if l == nil || l.released {
		return
	}
	l.released = true
	if l.f != nil {
		_ = fsutil.Unlock(l.f)
		_ = l.f.Close()
	}
}

// heldFor answers nil when l is a turn a save may write under: one LockFile
// gave and Unlock has not given back.
func (l *FileLock) heldFor() error {
	switch {
	case l == nil:
		return errors.New("config: a save without the config file's lock")
	case l.released:
		return fmt.Errorf("config: a save of %s after its lock was given back", l.path)
	}
	return nil
}

// unlockedWarned is the config files a no-lock warning has been written for,
// so that it is written once per file and process (warnUnlocked).
var unlockedWarned sync.Map

// warnUnlocked says, once for path, that its writers take turns without the
// lock, and why.
func warnUnlocked(path string, err error) {
	if _, seen := unlockedWarned.LoadOrStore(path, true); seen {
		return
	}
	validateLogger.Warn("config: no lock can be taken beside the config file, so its writers do not take turns there",
		"path", path, "err", err)
}

// Edit is one read-modify-write of the config file at path in a turn of its
// lock: it takes the lock (LockFile), loads the file as it is now, and hands
// that copy to fn, which changes it and answers whether to save it; a save
// goes through Save under the same turn. An error of fn's is returned as it
// is, and nothing is saved. Loading under the lock is the point: a copy read
// before the turn can be older than what another writer saved in between,
// and saving it would write over that writer's change.
func Edit(path string, fn func(cfg *Config) (save bool, err error)) error {
	l, err := LockFile(path)
	if err != nil {
		return err
	}
	defer l.Unlock()
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	save, err := fn(cfg)
	if err != nil || !save {
		return err
	}
	return cfg.Save(l)
}
