package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
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

// ErrChangedInTurn is what a *ChangedInTurnError matches.
var ErrChangedInTurn = errors.New("config: the config file changed during a writer's turn")

// ChangedInTurnError is a save's refusal of a config file that is no longer
// the file its turn found once it was taken, or its own last write in the
// turn left: another writer changed or removed it, or made it where there was
// none. With the lock held that is a writer that takes no turns (an edit, or
// a bridge older than the lock); where the filesystem keeps no locks it can be
// any writer. It names the file and nothing it holds.
type ChangedInTurnError struct {
	// Path is the config file, as its writer named it.
	Path string
}

func (e *ChangedInTurnError) Error() string {
	return fmt.Sprintf("%s was changed by another writer while this one had its turn at it (an edit, or a bridge "+
		"older than the config's lock), so nothing was written over it: try again, to work from the file as it is now", e.Path)
}

// Is makes a *ChangedInTurnError match ErrChangedInTurn.
func (e *ChangedInTurnError) Is(target error) bool { return target == ErrChangedInTurn }

// LockFileName is the name of the lock file beside the config file at path:
// `.bridge.yaml.lock` beside bridge.yaml. Hidden, as Save's staged files
// beside it are, and never removed: only the lock on it means anything.
func LockFileName(path string) string {
	return "." + filepath.Base(path) + ".lock"
}

// FileLock is one writer's turn at the config file at its path: the lock
// LockFile took, the file as the turn found it, which Save, SaveIf and
// SaveBytes write over and only over, and what Unlock gives back.
type FileLock struct {
	// path is the config file as its writer named it, for what the turn
	// says.
	path string
	// dir is the directory a write of the file lands in, as the OS resolves
	// path (fsutil.ResolveParent), and target the file in it: the lock is
	// beside it, and a save stages its file in dir and renames it onto
	// target.
	dir, target string
	// f is the open lock file, nil when no lock was taken.
	f *os.File
	// unheld is why no lock is held although LockFile answered: a
	// filesystem that keeps none.
	unheld error
	// found is the file's bytes as the turn found them once it was taken, or
	// as its own last write left them; absent says there was no file.
	found  []byte
	absent bool
	// released is set once Unlock has run.
	released bool
}

// LockFile takes the lock every writer of the config file at path holds
// around its read-modify-write (backlog B506), reads the file as it is once
// it holds the lock, and returns the turn. The writer decides on what it
// reads in the turn (Edit loads the turn's own read), saves through Save,
// SaveIf or SaveBytes with this turn, and calls Unlock: so two writers take
// turns, and neither saves a copy read before its turn. Every writer of
// bridge.yaml in this tree takes it: library add and remove (Edit), bridge
// init's save, serve --init-if-missing's seed, bridge output dac bind, a
// restore, and serve's console saves (RuntimeConfig.Update).
//
// The lock is an exclusive kernel lock (fsutil.TryLock: flock on unix,
// LockFileEx on Windows) on the file LockFileName names, beside the config
// file in the directory the OS writes it in, as path resolves
// (fsutil.ResolveParent): so two names of one file, one through a link and a
// `..` after it, take one lock, where the directory filepath.Dir cleans the
// name to gave them two. Not on the config file itself: Save replaces that by
// a rename, which a lock on the old file would not follow, and on Windows a
// locked range refuses every other handle's reads, and the config is read by
// everything. Not in the data dir: the config names its data dir, which a
// writer can change, and init and serve's seed write a config before any data
// dir is known. Made by a process running as root, the lock file takes the
// config file's owner, the owner Save keeps for the file it replaces, and the
// directory's only while there is no config file, so that the config's owner
// can open it for its next save (fsutil.OpenLockFileLike). It is held only
// around one read-modify-write, never for a process's life, so serve's
// console saves and a CLI writer beside a running serve take turns. A process
// that holds both this lock and the data dir's (lockServeDataDir in
// cmd/bridge: bind, restore, and serve itself) takes the data dir's first and
// this one inside it.
//
// LockFile waits for another writer for up to LockWait, asking every
// lockRetry, then refuses with a *BusyError, having taken nothing. An error
// opening the lock file (a directory this user cannot write, which Save could
// not write in either), or reading the config, is returned as it is. A
// filesystem that keeps no locks (fsutil.KeepsNoLocks: some NFS and SMB
// mounts) is not a refusal: the turn is taken without the lock, said once per
// file in the log, and there nothing keeps another writer out of it but the
// comparison below. Any other failure to take the lock is returned, and no
// turn is taken.
//
// Every save in the turn compares the file with the one the turn found, once
// its new file is staged and just before the rename (stillAsFound), and
// refuses with a *ChangedInTurnError when they differ, writing nothing: with
// the lock held only a writer that takes no turns (an edit, a bridge older
// than the lock) can change the file meanwhile, and without it any writer
// can. That narrows the window to the instant between the comparison and the
// rename, and does not close it: no rename can be made conditional on the
// file it replaces.
func LockFile(path string) (*FileLock, error) {
	return lockFile(path, fsutil.TryLock, LockWait)
}

// lockFile is LockFile with the try and the wait as parameters, for its
// tests.
func lockFile(path string, try func(*os.File) error, wait time.Duration) (*FileLock, error) {
	dir, name, err := fsutil.ResolveParent(path)
	if err != nil {
		return nil, fmt.Errorf("find the directory %s is written in: %w", path, err)
	}
	l := &FileLock{path: path, dir: dir, target: filepath.Join(dir, name)}
	f, err := fsutil.OpenLockFileLike(dir, LockFileName(path), l.target)
	if err != nil {
		return nil, fmt.Errorf("open the lock beside %s: %w", path, err)
	}
	deadline := time.Now().Add(wait)
	waited := false
	for {
		err := try(f)
		switch {
		case err == nil:
			l.f = f
		case errors.Is(err, fsutil.ErrLocked):
			if !waited {
				waited = true
				noteLockWaited(l.target)
			}
			if !time.Now().Before(deadline) {
				_ = f.Close()
				return nil, &BusyError{Path: path, Wait: wait}
			}
			time.Sleep(lockRetry)
			continue
		case fsutil.KeepsNoLocks(err):
			_ = f.Close()
			warnUnlocked(path, err)
			l.unheld = err
		default:
			_ = f.Close()
			return nil, fmt.Errorf("take the lock beside %s: %w", path, err)
		}
		if err := l.find(); err != nil {
			l.Unlock()
			return nil, err
		}
		noteTurnTaken(l.target)
		return l, nil
	}
}

// readTurnFile is how a turn reads the config file, and ReadFile: through
// fsutil.ReadAsFile, so a named pipe there is refused rather than waited on.
// SetTurnReadForTest replaces it.
var readTurnFile = fsutil.ReadAsFile

// SetTurnReadForTest makes every read of a config file that a turn makes, and
// ReadFile, go through fn, and returns what puts the read before it back.
// Tests only, and process-global: a test that sets it does not call
// t.Parallel, and puts it back once every writer it started has returned.
func SetTurnReadForTest(fn func(path string) ([]byte, error)) (restore func()) {
	prev := readTurnFile
	readTurnFile = fn
	return func() { readTurnFile = prev }
}

// ReadFile reads the config file at path as a turn reads it: for a writer's
// own comparison in its turn (bind's SaveIf), which reads what the turn's
// comparison reads.
func ReadFile(ctx context.Context, path string) ([]byte, error) {
	return readTurnFile(path)
}

// find reads the config file as the turn finds it once it is taken: through
// fsutil.ReadAsFile, so a named pipe there is refused rather than waited on.
func (l *FileLock) find() error {
	data, err := readTurnFile(l.target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		l.found, l.absent = nil, true
	case err != nil:
		return fmt.Errorf("read %s in its turn: %w", l.path, err)
	default:
		l.found, l.absent = data, false
	}
	return nil
}

// stillAsFound answers nil while the config file is the one the turn found,
// or its own last write left, and a *ChangedInTurnError once it is not:
// other bytes, no file where there was one, or a file where there was none.
// A read that fails otherwise is an error too, since it cannot say the file
// is unchanged.
func (l *FileLock) stillAsFound() error {
	now, err := readTurnFile(l.target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if l.absent {
			return nil
		}
	case err != nil:
		return fmt.Errorf("read %s again before replacing it: %w", l.path, err)
	case !l.absent && bytes.Equal(now, l.found):
		return nil
	}
	return &ChangedInTurnError{Path: l.path}
}

// write puts data in place as the config file in this turn: staged in the
// directory the file is written in, given the owner of the file it replaces,
// synced, and renamed onto the file only while it is still the one the turn
// found (stillAsFound) and unchanged, when given, answers nil. Once it lands
// it is what the turn compares with.
func (l *FileLock) write(data []byte, unchanged func(context.Context) error) error {
	if err := l.heldFor(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(l.dir, ".bridge-*.yaml")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	// Panic-safety FD close. Registered AFTER the Remove defer so it
	// runs first (LIFO) — Windows requires the FD closed before
	// Remove can unlink. See internal/auth/auth.go for the rationale
	// in detail; pattern repeats across every atomic-write helper.
	defer func() { _ = tmp.Close() }()
	// A `sudo bridge library add` beside a service install stages this
	// file as root; the service reads it at its next start.
	if err := fsutil.KeepOwner(tmp, l.target); err != nil {
		tmp.Close()
		return fmt.Errorf("keep the config's owner: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	noteSaveStaged(l.target)
	if err := l.stillAsFound(); err != nil {
		return err
	}
	if unchanged != nil {
		if err := unchanged(context.Background()); err != nil {
			return err
		}
	}
	if err := atomicwrite.RenameWithRetry(tmpName, l.target); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	tmpName = "" // suppress defer cleanup
	l.found, l.absent = append([]byte(nil), data...), false
	return nil
}

// SaveBytes writes data as the config file in this turn, as Save writes a
// config, and only over the file the turn found: for a restore, which puts a
// snapshot's copy of the file back as it is.
func (l *FileLock) SaveBytes(data []byte) error {
	return l.write(data, nil)
}

// Path is the config file this turn is at, as its writer named it.
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
// lock: it takes the lock (LockFile), loads the file as the turn found it,
// and hands that copy to fn, which changes it and answers whether to save it;
// a save goes through Save under the same turn, so it writes only over the
// file the turn found. An error of fn's is returned as it is, and nothing is
// saved. Loading in the turn is the point: a copy read before the turn can be
// older than what another writer saved in between, and saving it would write
// over that writer's change.
func Edit(path string, fn func(cfg *Config) (save bool, err error)) error {
	l, err := LockFile(path)
	if err != nil {
		return err
	}
	defer l.Unlock()
	if l.absent {
		return fmt.Errorf("read config %q: %w", path, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist})
	}
	cfg, err := loadData(path, l.found)
	if err != nil {
		return err
	}
	save, err := fn(cfg)
	if err != nil || !save {
		return err
	}
	return cfg.Save(l)
}

// lockWaitedHooks holds, by turnKey, what LockWaitedForTest asked to be
// told: nothing in production.
var lockWaitedHooks sync.Map

// lockWaitedHook is one LockWaitedForTest's channel, closed once.
type lockWaitedHook struct {
	once sync.Once
	ch   chan struct{}
}

// LockWaitedForTest returns a channel that is closed the first time a writer
// of the config file at path finds the file's lock held and starts to wait
// for it, and what stops that. Tests only: it is how a test knows that a
// writer reached a turn another writer holds, rather than guessing how long
// the writer takes to get there. One hook per config file at a time.
func LockWaitedForTest(path string) (waited <-chan struct{}, stop func()) {
	key := turnKey(path)
	h := &lockWaitedHook{ch: make(chan struct{})}
	lockWaitedHooks.Store(key, h)
	return h.ch, func() { lockWaitedHooks.CompareAndDelete(key, h) }
}

// noteLockWaited closes the channel LockWaitedForTest handed out for key, if
// any.
func noteLockWaited(key string) {
	v, ok := lockWaitedHooks.Load(key)
	if !ok {
		return
	}
	h := v.(*lockWaitedHook)
	h.once.Do(func() { close(h.ch) })
}

// saveStagedHooks holds, by turnKey, what OnSaveStagedForTest asked a save
// to run: nothing in production.
var saveStagedHooks sync.Map

// OnSaveStagedForTest makes each save of the config file at path run fn once
// it has staged the new file, just before the rename that puts it in place,
// and returns what stops that. Tests only: it is the window a test lands
// another writer in there, which no writer that takes the lock can reach (an
// edit, or a bridge older than the lock, can). One hook per config file at a
// time.
func OnSaveStagedForTest(path string, fn func()) (stop func()) {
	key := turnKey(path)
	h := &fn
	saveStagedHooks.Store(key, h)
	return func() { saveStagedHooks.CompareAndDelete(key, h) }
}

// noteSaveStaged runs what OnSaveStagedForTest asked for key, if anything.
func noteSaveStaged(key string) {
	if v, ok := saveStagedHooks.Load(key); ok {
		(*v.(*func()))()
	}
}

// turnTakenHooks holds, by turnKey, what OnTurnTakenForTest asked a turn to
// run: nothing in production.
var turnTakenHooks sync.Map

// OnTurnTakenForTest makes each turn of the config file at path run fn once
// LockFile has taken it and read the file, before the turn's writer does
// anything in it, and returns what stops that. Tests only: it is the window
// between the turn's read and its writer's work (a console root change's
// library index change, a restore's first write), where a test lands a writer
// that takes no turns (an edit, or a bridge older than the lock). One hook per
// config file at a time.
func OnTurnTakenForTest(path string, fn func()) (stop func()) {
	key := turnKey(path)
	h := &fn
	turnTakenHooks.Store(key, h)
	return func() { turnTakenHooks.CompareAndDelete(key, h) }
}

// noteTurnTaken runs what OnTurnTakenForTest asked for key, if anything.
func noteTurnTaken(key string) {
	if v, ok := turnTakenHooks.Load(key); ok {
		(*v.(*func()))()
	}
}

// turnKey is the key the test hooks and a writer's turn agree on for the
// config file at path: the file the OS writes, as LockFile resolves it, so
// that a hook set through one name of the file hears a writer given another.
func turnKey(path string) string {
	if dir, name, err := fsutil.ResolveParent(path); err == nil {
		return filepath.Join(dir, name)
	}
	return filepath.Clean(path)
}
