package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// serverPIDFileName is the file `bridge serve` writes under DataDir while
// it is running. The name is fixed because doctor derives the same path
// independently (cmd/bridge/doctor.go sets Deps.OwnPIDFile to
// filepath.Join(cfg.DataDir, "server.pid")); the two must agree.
const serverPIDFileName = "server.pid"

// writeServerPIDFile records this process's PID under dataDir so
// `bridge doctor` can tell "the port is bound by MY bridge" apart from
// "another process owns this port".
//
// doctor has read this file since it was written, and nothing ever wrote
// it. The consequence is a false FAIL: on a live install, `doctor`
// probes the port, finds it bound, cannot attribute it, and reports a
// conflict against the operator's own running bridge. It also left PR
// #432's native Windows port→PID attribution unreachable — that code
// only runs once there is a PID to attribute to.
//
// Written atomically because doctor may read it at any moment; a
// half-written file would parse as a different PID or fail outright.
//
// A write failure is NOT fatal. The pidfile is diagnostic only, and
// refusing to serve because a diagnostic aid could not be written would
// trade a working bridge for a tidier doctor report.
func writeServerPIDFile(dataDir string) (path string, err error) {
	if dataDir == "" {
		return "", fmt.Errorf("no data dir")
	}
	path = filepath.Join(dataDir, serverPIDFileName)
	// WriteBytes MkdirAll's the parent itself; the third argument is the
	// temp-file prefix, not a mode.
	if err := atomicwrite.WriteBytes(path, []byte(strconv.Itoa(os.Getpid())), ".pid-"); err != nil {
		return path, fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// removeServerPIDFile clears the pidfile on graceful shutdown.
//
// doctor does not take a leftover file on trust: checkPort asks the OS
// whether that PID actually holds the port (isPIDListeningOnPort). A PID
// that is no longer running excuses nothing, so a stale file leaves a
// bound port graded as a conflict. A RECYCLED one is excused as though it
// were ours: reported as our own bridge when the process that inherited it
// holds the port, and otherwise answered by the liveness arm's warn or
// same-uid ok. Removing the file on the way out keeps the common case
// clean, and leaves a stale file only after an exit that skipped this.
func removeServerPIDFile(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
}
