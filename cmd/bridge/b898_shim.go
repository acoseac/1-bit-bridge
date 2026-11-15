// Backlog B898 PR 1's tests come first in its history, over main's behaviour
// (this file, and the next commit, which removes it and lands the fix).

package main

// A names-only shim of backlog B898's names over main's serve, for the red
// runs at main: main's behaviour behind each.

import (
	"context"
	"time"
)

type updaterAction bool

const (
	updaterInstall  updaterAction = false
	updaterRollback updaterAction = true
)

// logUpdaterFailure logs nothing: main does not.
func logUpdaterFailure(updaterAction, error) {}

const crashRollbackEndsTheStart = "this start ends here, before it writes anything, and is not counted"

type bootSeams struct {
	restore, remove func(dst, suffix string) error
	probeVersion    func(ctx context.Context, binary string, within time.Duration) (string, error)
	probeBound      time.Duration
}
