package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// serveLockFileName is the file under DataDir on which a running `bridge
// serve` holds an exclusive kernel lock (fsutil.TryLock), from before its
// first write there until runServe returns (backlog B208). The file is
// never removed: only the lock on it means anything, and unlinking a lock
// file while it is held lets a later serve lock a NEW file at the same
// path beside the old holder.
const serveLockFileName = "server.lock"

// serveDataDirHeldError is lockServeDataDir's answer when another open
// holds the data dir's lock: a serve, a restore, or a clear-missing, in
// another process or in this one (the launcher menu and the tests run
// serve in-process). The message names a serve, which is what writes
// server.pid. A holder that writes none is still this error, with pid 0.
type serveDataDirHeldError struct {
	dataDir string
	// pid is the pid server.pid names, 0 when it names none. A serve
	// writes that file after it takes the lock. A restore or a
	// clear-missing does not, so a refusal while one of those holds the
	// lock names a pid only when an earlier serve left the file.
	pid int
}

func (e *serveDataDirHeldError) Error() string {
	holder := "another bridge serve"
	if e.pid > 0 {
		holder = fmt.Sprintf("another bridge serve (pid %d, from %s)", e.pid, serverPIDFileName)
	}
	return fmt.Sprintf("%s is already running on the data dir %s", holder, e.dataDir)
}

// dataDirUnusableError is prepareServeDataDir's refusal of a data dir it
// cannot prepare: one MkdirAll cannot make, or that is not a directory once
// made. why says what stands in the way, naming the entry of the path that
// does, and err is the error that showed it, which errors.Is and errors.As
// reach: MkdirAll's (the EEXIST of a link to nothing, the ENOTDIR of a
// regular file), or a stat's.
type dataDirUnusableError struct {
	dataDir string
	why     string
	err     error
}

func (e *dataDirUnusableError) Error() string {
	return fmt.Sprintf("the data directory %s cannot be used: %s", e.dataDir, e.why)
}

func (e *dataDirUnusableError) Unwrap() error { return e.err }

// dataDirUnlockedError is lockServeDataDir's answer when no lock could be
// taken on a data dir that is there and no other open holds the lock (B537
// item 1): lockPath is the lock file; noLocks says that TryLock's answer is
// one fsutil.KeepsNoLocks reads, a filesystem or platform that gave no lock
// (ENOLCK, EOPNOTSUPP, ENOTSUP, errors.ErrUnsupported, ENOSYS among them;
// Windows' ERROR_NOT_SUPPORTED and ERROR_INVALID_FUNCTION); and err is that
// answer, or any other (a permission, I/O or descriptor error), or the lock
// file's open that failed, which is never marked noLocks. ENOLCK is no proof
// that the filesystem keeps no locks: an NFS mount whose lock manager is not
// running answers it, and so does a Linux kernel out of lock records while
// another open holds the lock, so a serve that goes on after it may run
// beside another.
type dataDirUnlockedError struct {
	lockPath string
	noLocks  bool
	err      error
}

func (e *dataDirUnlockedError) Error() string {
	if e.noLocks {
		return fmt.Sprintf("the filesystem gave no lock on %s (%v)", e.lockPath, e.err)
	}
	return fmt.Sprintf("could not lock %s (%v)", e.lockPath, e.err)
}

func (e *dataDirUnlockedError) Unwrap() error { return e.err }

// prepareServeDataDir makes the data dir where it is missing, through every
// directory above it that is missing too, and then requires a directory
// there, before anything takes its lock or writes in it (backlog B606, the
// owner's decision of 2026-10-31: serve refuses a data directory it cannot
// prepare). Run as root, each directory it makes takes the owner of the
// directory it is made in (fsutil.MkdirAll), so a `sudo bridge serve` over
// an install another user owns leaves one that user can write.
//
// A link that leads to a directory is taken, the data directory and its
// ancestors alike (Linux and NAS installs keep one there), and so is a
// directory not made yet below such a link, which MkdirAll makes through it.
// What it refuses, with a *dataDirUnusableError naming the data directory
// and the entry in its way, is an entry of the path that is no directory: a
// regular file, a link to one, or a link to nothing, which MkdirAll stops at
// (EEXIST) rather than make what it names; and a data directory MkdirAll
// cannot make for any other reason, a parent this user cannot write
// included. Whether the data directory can be written is not asked here: a
// lock file that cannot be opened in a directory that is there is the lock's
// fallback (lockServeDataDir), and writability is the player's preflight's
// (backup.AdoptPlayerState, backup.CheckPlayerStart) and bridge doctor's.
func prepareServeDataDir(dataDir string) error {
	return prepareServeDataDirWith(dataDir, fsutil.MkdirAll)
}

// prepareServeDataDirWith is prepareServeDataDir making the directory with
// mkdirAll: a test's stand-in for a data directory replaced between MkdirAll's
// answer and the stat after it.
func prepareServeDataDirWith(dataDir string, mkdirAll func(string, os.FileMode) error) error {
	if err := mkdirAll(dataDir, 0o700); err != nil {
		return &dataDirUnusableError{dataDir: dataDir, why: whyNotMade(err), err: err}
	}
	// MkdirAll answers nil for a directory there, and the stat asks again,
	// for a directory replaced since.
	info, err := os.Stat(dataDir)
	switch {
	case err != nil:
		return &dataDirUnusableError{dataDir: dataDir, why: fmt.Sprintf("it cannot be read: %v", err), err: err}
	case !info.IsDir():
		why := notADirectory(dataDir)
		if why == "" {
			why = dataDir + " is not a directory"
		}
		return &dataDirUnusableError{dataDir: dataDir, why: why, err: &fs.PathError{Op: "stat", Path: dataDir, Err: syscall.ENOTDIR}}
	}
	return nil
}

// whyNotMade says what stands in the way of a data directory MkdirAll could
// not make: the entry its error names, when that entry is there and is no
// directory (notADirectory), with the error beside it; and the error alone
// otherwise.
func whyNotMade(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		if why := notADirectory(pe.Path); why != "" {
			return fmt.Sprintf("%s (%v)", why, err)
		}
	}
	return fmt.Sprintf("it could not be made: %v", err)
}

// notADirectory says what entry is, when it is there and is no directory: a
// regular file or another kind of entry, a symbolic link that leads to one,
// named with where it leads, or a symbolic link to nothing. It answers "" for
// a directory, a link to one, and an entry it cannot read.
func notADirectory(entry string) string {
	info, err := os.Lstat(entry)
	switch {
	case err != nil:
		return ""
	case info.Mode()&fs.ModeSymlink == 0:
		if info.IsDir() {
			return ""
		}
		return fmt.Sprintf("%s is %s, not a directory", entry, fsutil.EntryKind(info.Mode()))
	}
	target, err := os.Stat(entry)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return entry + " is a symbolic link to nothing"
	case err != nil || target.IsDir():
		return ""
	}
	resolved, err := filepath.EvalSymlinks(entry)
	if err != nil {
		return fmt.Sprintf("%s is a symbolic link to %s, not a directory", entry, fsutil.EntryKind(target.Mode()))
	}
	return fmt.Sprintf("%s leads to %s, %s, not a directory", entry, resolved, fsutil.EntryKind(target.Mode()))
}

// lockServeDataDir takes the lock that keeps a second `bridge serve` off a
// data dir a live one serves, and returns what releases it. runServe asks
// before its first write to the data dir and before it binds anything: a
// second serve used to run all of its wiring and fail only at the bind,
// and by then its batch coordinator had marked the live bridge's running
// batches interrupted, its exit had removed the live bridge's server.pid
// and rewritten tokens.json, and a second serve on ports of its own never
// failed at all (backlog B208).
//
// dataDir must be one prepareServeDataDir prepared: the lock makes nothing,
// and a data dir that is not there is a lock file that cannot be opened.
// It answers a *serveDataDirHeldError when another open holds the lock.
// For `bridge serve` that is the one answer that refuses. Any other error
// is a *dataDirUnlockedError, and means no lock could be taken: a
// filesystem that gave none (noLocks, the answers fsutil.KeepsNoLocks
// reads, ENOLCK among them, which does not show that no other open holds
// it), or any other failure, a lock file this user cannot open (a
// permission error, a read-only filesystem, a directory standing where it
// goes), an I/O or a descriptor error (B537 item 1). Serve says which and
// continues, since a lock that cannot be taken must not stop a bridge from
// starting; restore and reset-player refuse; clear-missing and the DAC bind
// fall back to the admin-port probe, and every one of them also probes a
// fixed admin port when this lock was taken (probeAdminBesideTheLock).
// release is never nil, and does nothing when no lock was taken.
//
// A kernel lock, not a lock FILE. The kernel drops it with the process
// however it exits, so a killed holder does not block the next serve,
// restore, or clear-missing. `bridge restore`, `bridge reset-player`,
// `bridge manifest clear-missing` and `bridge output dac bind` hold this
// same lock for their whole run (holdDataDirForMutation).
func lockServeDataDir(dataDir string) (release func(), err error) {
	return lockServeDataDirWith(dataDir, nil)
}

// lockServeDataDirWith is lockServeDataDir taking the lock with try, nil
// being fsutil.TryLock: a boot test's stand-in for what the filesystem
// answers (serveOpts.dataDirTryLock).
func lockServeDataDirWith(dataDir string, try func(*os.File) error) (release func(), err error) {
	release = func() {
		// Nothing to release: no lock has been taken.
	}
	if try == nil {
		try = fsutil.TryLock
	}
	lockPath := filepath.Join(dataDir, serveLockFileName)
	// The data dir's owner's file, never followed out of the data dir,
	// and opened again when a create races another serve's on macOS 27
	// (fsutil.OpenLockFile).
	f, err := fsutil.OpenLockFile(dataDir, serveLockFileName)
	if err != nil {
		return release, &dataDirUnlockedError{lockPath: lockPath, err: err}
	}
	if err := try(f); err != nil {
		_ = f.Close()
		if errors.Is(err, fsutil.ErrLocked) {
			return release, &serveDataDirHeldError{dataDir: dataDir, pid: recordedServePID(dataDir)}
		}
		return release, &dataDirUnlockedError{lockPath: lockPath, noLocks: fsutil.KeepsNoLocks(err), err: err}
	}
	return func() {
		// Unlock before the close: on Windows the release a close makes
		// can lag, and a restart in this process would find it held.
		_ = fsutil.Unlock(f)
		_ = f.Close()
	}, nil
}

// dataDirHold is serve's lock on its data directory (lockServeDataDir),
// shared by everything that may write there: runServe holds it until it
// returns, and its player (startServePlayer) for as long as anything the
// player started still runs. The lock is released when the last holder lets
// go. A player's engine whose state write outlived its stop bound, an fsync
// that does not return, keeps it past runServe's return, and so does, for
// good, one whose Run a panic ended before its shutdown joined those writes
// (bridgePlayer.settle): in production main's os.Exit follows that return,
// and the kernel releases the lock with the process, so no other serve
// takes the data directory while a write may still land; a serve started
// again in this process (the launcher menu, the tests) is refused it until
// the write ends, or for the life of the process after such a panic.
type dataDirHold struct {
	mu      sync.Mutex
	holders int
	release func()
}

// newDataDirHold is a hold on a lock lockServeDataDir took, held once, by
// runServe: release is what lockServeDataDir returned.
func newDataDirHold(release func()) *dataDirHold {
	return &dataDirHold{holders: 1, release: release}
}

// acquire adds a holder; the caller holds the lock already, through runServe.
func (h *dataDirHold) acquire() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.holders++
}

// done lets one holder go, and releases the lock when it was the last.
func (h *dataDirHold) done() {
	h.mu.Lock()
	h.holders--
	last := h.holders == 0
	h.mu.Unlock()
	if last {
		h.release()
	}
}

// recordedServePID is the pid dataDir's server.pid names, or 0.
func recordedServePID(dataDir string) int {
	b, err := os.ReadFile(filepath.Join(dataDir, serverPIDFileName))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// holdDataDirForMutation prepares the serve data dir for a command that
// writes it from a second process (prepareServeDataDir), takes its lock, and
// keeps it until release. Its callers are restore, reset-player,
// clear-missing and the DAC bind. refused means the command stops, its
// refusal already written to stderr: a data dir that cannot be prepared (a
// regular file or a link to nothing where it goes, or above it), which every
// caller refuses before it writes anything, as serve does (backlog B606); or
// another open already holding the lock, in the lock's own message. fallback
// means the lock could not be taken at all on a data dir that is there (a
// filesystem that gave no lock, a permissions error, a directory where the
// lock file goes): each caller keeps its own policy for that, restore and
// reset-player refusing, clear-missing and the DAC bind keeping the
// admin-port probe. A taken lock is held across the caller's confirmation
// prompt and its last write, and the caller still probes a fixed admin port
// (probeAdminBesideTheLock): a serve that continued after a lock error other
// than contention holds no lock. release is never nil.
func holdDataDirForMutation(dataDir, cmd string, stderr io.Writer) (release func(), fallback, refused bool) {
	if err := prepareServeDataDir(dataDir); err != nil {
		fmt.Fprintf(stderr, "%s: %v.\n", cmd, err)
		fmt.Fprintln(stderr, "Move what stands in its way, or set dataDir in bridge.yaml to a directory, then re-run this command.")
		return func() {
			// Nothing to release: no lock has been taken.
		}, false, true
	}
	release, err := lockServeDataDir(dataDir)
	if err == nil {
		return release, false, false
	}
	var held *serveDataDirHeldError
	if errors.As(err, &held) {
		fmt.Fprintf(stderr, "%s: %s.\n", cmd, held.Error())
		fmt.Fprintln(stderr, "Stop it first, then re-run this command.")
		return release, false, true
	}
	return release, true, false
}
