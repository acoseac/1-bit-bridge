package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

// lockServeDataDir takes the lock that keeps a second `bridge serve` off a
// data dir a live one serves, and returns what releases it. runServe asks
// before its first write to the data dir and before it binds anything: a
// second serve used to run all of its wiring and fail only at the bind,
// and by then its batch coordinator had marked the live bridge's running
// batches interrupted, its exit had removed the live bridge's server.pid
// and rewritten tokens.json, and a second serve on ports of its own never
// failed at all (backlog B208).
//
// It answers a *serveDataDirHeldError when another open holds the lock.
// For `bridge serve` that is the one answer that refuses. Any other error
// means no lock could be taken (a filesystem that keeps no locks, a lock
// file this user cannot open). Serve warns and continues, since a lock
// that cannot be taken must not stop a bridge from starting; restore and
// clear-missing fall back to the admin-port probe, and they also probe a
// fixed admin port when this lock was taken (probeAdminBesideTheLock).
// release is never nil,
// and does nothing when no lock was taken.
//
// A kernel lock, not a lock FILE. The kernel drops it with the process
// however it exits, so a killed holder does not block the next serve,
// restore, or clear-missing. `bridge restore` and `bridge manifest
// clear-missing` hold this same lock for their whole run
// (holdDataDirForMutation).
func lockServeDataDir(dataDir string) (release func(), err error) {
	release = func() {
		// Nothing to release: no lock has been taken.
	}
	if err := fsutil.MkdirAll(dataDir, 0o700); err != nil {
		return release, err
	}
	// The data dir's owner's file, never followed out of the data dir,
	// and opened again when a create races another serve's on macOS 27
	// (fsutil.OpenLockFile).
	f, err := fsutil.OpenLockFile(dataDir, serveLockFileName)
	if err != nil {
		return release, err
	}
	if err := fsutil.TryLock(f); err != nil {
		_ = f.Close()
		if errors.Is(err, fsutil.ErrLocked) {
			return release, &serveDataDirHeldError{dataDir: dataDir, pid: recordedServePID(dataDir)}
		}
		return release, err
	}
	return func() {
		// Unlock before the close: on Windows the release a close makes
		// can lag, and a restart in this process would find it held.
		_ = fsutil.Unlock(f)
		_ = f.Close()
	}, nil
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

// holdDataDirForMutation takes the serve data-dir lock for a command that
// writes the store from a second process, and keeps it until release.
// refused means another open already holds the lock: the refusal is the
// lock's own message, already written to stderr, and the caller stops.
// fallback means the lock could not be taken at all (a filesystem that
// keeps no locks, a permissions error, a directory where the lock file
// goes). The caller then keeps the admin-port probe. A taken lock is
// held across the caller's confirmation prompt and its last write, and
// the caller still probes a fixed admin port (probeAdminBesideTheLock):
// a serve that continued after a lock error other than contention holds
// no lock. release is never nil.
func holdDataDirForMutation(dataDir, cmd string, stderr io.Writer) (release func(), fallback, refused bool) {
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
