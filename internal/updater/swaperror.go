package updater

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// The outcomes of a swap or a rollback that failed, and the recovery latch
// (backlog B898).
var (
	// ErrBinaryInUse is what a *SwapError matches where another process's
	// hold on a file of it ended the operation and the binary is in its place.
	ErrBinaryInUse = errors.New("the bridge's binary was held by another process")
	// ErrBinaryMissing is what a *SwapError matches where the operation left no
	// binary at its path, and what Install, Rollback and the auto-installer's
	// restart refuse with while it is not there (binaryLatch).
	ErrBinaryMissing = errors.New("the bridge's binary is not at its path")
	// ErrMarkerNotRestored is what a *MarkerNotRestoredError matches.
	ErrMarkerNotRestored = errors.New("the install marker could not be put back as it was read")
)

// Outcome is what a failed swap or rollback left of the binary.
type Outcome int

const (
	// NothingMoved: every file is where it was.
	NothingMoved Outcome = iota
	// PutBack: the binary was moved out of its place, the rename into it was
	// refused, and the binary was put back.
	PutBack
	// BinaryMissing: there is no binary at its path.
	BinaryMissing
)

// A *SwapError is a swap's or a rollback's failure: what it left (Outcome),
// whether another process's hold ended it (InUse), the attempts and waits of
// the step that ended it, and, for BinaryMissing, the one rename that puts the
// binary back (Recover, which names paths: for the log and the CLI, never a
// console body). The outcome is the step's, never the errno's.
type SwapError struct {
	Outcome  Outcome
	InUse    bool
	Attempts int
	Waited   time.Duration
	Recover  string
	msg      string
	errs     []error
}

func (e *SwapError) Error() string { return e.msg }

// Unwrap is the step's error, and the put-back's where there was one.
func (e *SwapError) Unwrap() []error { return e.errs }

// Is makes e match ErrBinaryInUse where a hold ended it and the binary is in
// its place, and ErrBinaryMissing where it is not.
func (e *SwapError) Is(target error) bool {
	switch target {
	case ErrBinaryInUse:
		return e.InUse && e.Outcome != BinaryMissing
	case ErrBinaryMissing:
		return e.Outcome == BinaryMissing
	}
	return false
}

// failed is the *SwapError of the step r, its message msg.
func failed(outcome Outcome, r stepResult, msg string) *SwapError {
	return &SwapError{Outcome: outcome, InUse: r.inUse, Attempts: r.attempts, Waited: r.waited, msg: msg, errs: []error{r.err}}
}

// A *MarkerNotRestoredError is a failed swap or rollback whose install marker
// could not be written back as it was read: it matches ErrMarkerNotRestored
// and unwraps to both errors.
type MarkerNotRestoredError struct{ Swap, Marker error }

func (e *MarkerNotRestoredError) Error() string {
	return fmt.Sprintf("%v; nor could the install marker be put back as it was (%v)", e.Swap, e.Marker)
}

func (e *MarkerNotRestoredError) Unwrap() []error { return []error{e.Swap, e.Marker} }

// Is makes e match ErrMarkerNotRestored.
func (e *MarkerNotRestoredError) Is(target error) bool { return target == ErrMarkerNotRestored }

// binaryLatch is the recovery latch (backlog B898), the disk's and no
// process's: nil where the bridge's binary is at binaryPath, an error matching
// ErrBinaryMissing where nothing is, and where that cannot be told, the
// Lstat's refusal, which refuses too. Install and Rollback ask it holding the
// install marker's lock, and the auto-installer's restart before it is
// requested, so whatever process asks, for whatever release and however old
// the marker, nothing is installed, rolled back or restarted over a binary a
// failed put-back left missing until a rename puts it back. Its words name no
// path.
func binaryLatch(binaryPath string) error {
	_, err := os.Lstat(binaryPath)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return &latchError{}
	}
	return &latchError{untold: err}
}

// The latch's words, which name no path and no error's text: a reason the
// console's Updates panel shows as it is (RollbackRefusalAt).
const (
	latchMissing = "the bridge's program file is not on disk: the bridge log names the rename that puts it back"
	latchUntold  = "whether the bridge's program file is on disk cannot be told"
)

// latchError is binaryLatch's refusal: the binary not at its path, or, with
// untold set, the Lstat's own refusal.
type latchError struct{ untold error }

func (e *latchError) Error() string {
	if e.untold != nil {
		return fmt.Sprintf("%s (%s)", latchUntold, fsutil.ErrorWithoutPath(e.untold))
	}
	return latchMissing
}

func (e *latchError) Unwrap() error { return e.untold }

// Is makes a binary that is not there match ErrBinaryMissing.
func (e *latchError) Is(target error) bool { return target == ErrBinaryMissing && e.untold == nil }

// latchWords is the reason binaryLatch's refusal err gives the Updates panel.
func latchWords(err error) string {
	if errors.Is(err, ErrBinaryMissing) {
		return latchMissing
	}
	return latchUntold
}
