// Backlog B898 PR 1's tests come first in its history, over main's behaviour
// (this file, and the next commit, which removes it and lands the fix).

package updater

// A names-only shim of backlog B898's API over main's production code, for
// the red runs at main: every name the tests use, main's behaviour behind
// each. Nothing here waits, probes or classifies; main's code calls none of
// the seams it declares.

import (
	"errors"
	"fmt"
	"os"
	"time"
)

type Outcome int

const (
	NothingMoved Outcome = iota
	PutBack
	BinaryMissing
)

var (
	ErrBinaryInUse       = errors.New("a file of the swap is in use by another process")
	ErrBinaryMissing     = errors.New("the bridge's program file is not on disk")
	ErrMarkerNotRestored = errors.New("the install marker could not be put back as it was read")
)

type SwapError struct {
	Outcome  Outcome
	InUse    bool
	Attempts int
	Waited   time.Duration
	Recover  string
}

func (e *SwapError) Error() string { return "a swap error (shim)" }

type MarkerNotRestoredError struct{ Swap, Marker error }

func (e *MarkerNotRestoredError) Error() string   { return fmt.Sprintf("%v; %v", e.Swap, e.Marker) }
func (e *MarkerNotRestoredError) Unwrap() []error { return []error{e.Swap, e.Marker} }

type RenameStage int

const (
	StageStaging RenameStage = iota
	StageProbe
	StageVacate
	StagePlace
	StagePutBack
	StageInstall
)

type RenameRefusal struct {
	Stage    RenameStage
	From, To string
	Err      error
	Attempt  int
	Wait     time.Duration
}

func SetRenameObserverForTest(func(RenameRefusal)) (restore func()) { return func() {} }

func SetMarkerWriteObserverForTest(func(string)) (restore func()) { return func() {} }

var (
	forwardWaits = []time.Duration{0, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
		400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, 1600 * time.Millisecond}
	recoveryWaits = []time.Duration{0, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
		400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond}
)

func waitsTotal(ws []time.Duration) time.Duration {
	var total time.Duration
	for _, d := range ws {
		total += d
	}
	return total
}

func ForwardWait() time.Duration  { return waitsTotal(forwardWaits) }
func RecoveryWait() time.Duration { return waitsTotal(recoveryWaits) }

func ConfigCheckBound() time.Duration { return configCheckTimeout + waitDelay }

const StateLockWait = defaultStateLockWait

// The seams the tests set: main's code calls none of them.
var (
	renameInUse  = inUse
	probeMovable = movable
	renameWait   = time.Sleep
)

func inUse(error) bool     { return false }
func movable(string) error { return nil }

const checkStagePrefix = ".bridge-check-"

const latchMissing = "the bridge's program file is not on disk: the bridge log names the rename that puts it back"

// RollbackRefusalAt is main's predicate: it asks nothing of the binary.
func RollbackRefusalAt(st State, readErr error, _ string) string {
	return RollbackRefusalOn(st, readErr)
}

type RestoreSeams struct {
	Rename func(oldpath, newpath string) error
	Probe  func(path string) error
	InUse  func(error) bool
	Wait   func(time.Duration)
}

// ReplaceRunningBinaryForTest is main's put-back, its renames through
// s.Rename (renameFunc, set for the call).
func ReplaceRunningBinaryForTest(s RestoreSeams) func(dst, backupExt string) error {
	return func(dst, ext string) error {
		prev := renameFunc
		if s.Rename != nil {
			renameFunc = s.Rename
		}
		defer func() { renameFunc = prev }()
		return replaceRunningBinary(dst, dst+ext)
	}
}

// swapByVacate is main's Windows swap after its SCM stop, on every host,
// its two renames through renameFunc so that a test's refusals reach it.
func swapByVacate(dst, newBinary, backupExt string, markSwapStarted func() error) error {
	bak := dst + backupExt
	if err := armSwap(markSwapStarted); err != nil {
		return err
	}
	staged, err := stageIntoDir(newBinary, dst, 0)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(staged)
		}
	}()
	if err := renameFunc(dst, bak); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", dst, bak, err)
	}
	if err := renameFunc(staged, dst); err != nil {
		if rerr := renameFunc(bak, dst); rerr != nil {
			return fmt.Errorf("install %s -> %s failed (%v); rollback also failed (%v); manual recovery needed",
				staged, dst, err, rerr)
		}
		return fmt.Errorf("install %s -> %s: %w (rolled back)", staged, dst, err)
	}
	committed = true
	return nil
}
