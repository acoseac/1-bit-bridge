package backup

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Test seams for packages that drive a verification through their own
// callers (cmd/bridge's ticker and CLI, the console). Each is process-global:
// a test that uses one MUST NOT call t.Parallel(), and puts it back in a
// t.Cleanup once every verification it started has returned. Production
// code never calls them.

// afterCheck, nil in production, runs once a verification's check has
// ended, before its verdict is written, with the backups folder's lock
// held: a test parks a verification there.
var afterCheck func(dir string)

// SetAfterCheckForTest sets the function a verification runs once its check
// has ended and before it writes its verdict, holding the lock, and returns
// the restore.
func SetAfterCheckForTest(fn func(dir string)) (restore func()) {
	prev := afterCheck
	afterCheck = fn
	return func() { afterCheck = prev }
}

// onSlotWait, nil in production, runs when a verification finds this
// process's slot held and begins to wait its turn.
var onSlotWait func()

// SetOnSlotWaitForTest sets the function a verification runs when it begins
// to wait its turn behind another of this process's verifications, and
// returns the restore.
func SetOnSlotWaitForTest(fn func()) (restore func()) {
	prev := onSlotWait
	onSlotWait = fn
	return func() { onSlotWait = prev }
}

// SetSyncVerdictForTest replaces the barrier a verdict's write runs once its
// rename has landed (fsutil.SyncParentDir), and returns the restore.
func SetSyncVerdictForTest(fn func(path string) error) (restore func()) {
	prev := syncVerdictDir
	syncVerdictDir = fn
	return func() { syncVerdictDir = prev }
}

// SetSyncVetoForTest replaces the barrier that makes the veto of a snapshot
// about to be checked again durable (fsutil.SyncParentDir), and returns the
// restore.
func SetSyncVetoForTest(fn func(path string) error) (restore func()) {
	prev := syncVetoDir
	syncVetoDir = fn
	return func() { syncVetoDir = prev }
}

// SetSyncsParentDirForTest sets whether a directory can be synced here, as a
// verification asks before it sets a VERIFIED aside (fsutil.SyncsParentDir:
// false on Windows), and returns the restore.
func SetSyncsParentDirForTest(syncs bool) (restore func()) {
	prev := syncsParentDir
	syncsParentDir = syncs
	return func() { syncsParentDir = prev }
}

// SetMetadataForTest replaces what a verification, a prune and a reader ask
// of a path's metadata (the resolution of its links, the prune's Lstat of
// its folder), each nil for the platform's own, and returns the restore: a
// test stands a mount whose metadata does not answer in for them.
func SetMetadataForTest(resolve func(string) (string, error), lstatFn func(string) (os.FileInfo, error)) (restore func()) {
	prevResolve, prevLstat := resolveLinks, lstat
	if resolve != nil {
		resolveLinks = resolve
	}
	if lstatFn != nil {
		lstat = lstatFn
	}
	return func() { resolveLinks, lstat = prevResolve, prevLstat }
}

// SetReadDirForTest replaces the listing of the backups folder List,
// ReadStatus and the orphan sweep take, and returns the restore.
func SetReadDirForTest(fn func(name string) ([]os.DirEntry, error)) (restore func()) {
	prev := readDir
	readDir = fn
	return func() { readDir = prev }
}

// SetOnPruneForTest sets the function a prune runs as it begins, under the
// backups folder's lock, and returns the restore.
func SetOnPruneForTest(fn func()) (restore func()) {
	prev := onPrune
	onPrune = fn
	return func() { onPrune = prev }
}

// SetOpenProbeForTest replaces the open a probe of the backups folder's lock
// makes (ProbeBackupsLock, StuckCheckOf), and returns the restore.
func SetOpenProbeForTest(fn func(path string) (*os.File, error)) (restore func()) {
	prev := openProbe
	openProbe = fn
	return func() { openProbe = prev }
}

// SetVerifyReadRateForTest sets how many bytes a second a verification
// reads at most (0: unpaced), and returns the restore.
func SetVerifyReadRateForTest(rate int64) (restore func()) {
	prev := verifyReadRate
	verifyReadRate = rate
	return func() { verifyReadRate = prev }
}

// SetTryLockBackupsForTest replaces the try a wait for the backups folder's
// lock makes (fsutil.TryLock), and returns the restore.
func SetTryLockBackupsForTest(fn func(*os.File) error) (restore func()) {
	prev := tryLockBackups
	tryLockBackups = fn
	return func() { tryLockBackups = prev }
}

// SetOnLowPriorityForTest sets the function a verification's thread runs once
// its priority is lowered, and returns the restore.
func SetOnLowPriorityForTest(fn func()) (restore func()) {
	prev := onLowPriority
	onLowPriority = fn
	return func() { onLowPriority = prev }
}

// SetOnDatabaseOpenForTest sets the function a verification runs just
// before SQLite opens the snapshot's database, and returns the restore.
func SetOnDatabaseOpenForTest(fn func()) (restore func()) {
	prev := onDatabaseOpen
	onDatabaseOpen = fn
	return func() { onDatabaseOpen = prev }
}

// VetoFileName is the file a verdict's write keeps in the bundle while the
// write is not known to be durable.
const VetoFileName = vetoFileName

// OwnerLockName is the file in a snapshot whose lock its owner holds.
const OwnerLockName = ownerLockName

// StuckMarkerName is the marker a check that did not end leaves in its
// backups folder, beside the lock it holds.
const StuckMarkerName = stuckMarkerName

// RootVetoPath is where a verdict's write keeps the veto of the snapshot in
// dir when it cannot keep it in the bundle.
func RootVetoPath(dir string) string { return rootVetoPath(dir) }

// SetOnManifestReadForTest sets the function a verification runs before each
// read of a snapshot's manifest, and returns the restore.
func SetOnManifestReadForTest(fn func(path string)) (restore func()) {
	prev := onManifestRead
	onManifestRead = fn
	return func() { onManifestRead = prev }
}

// SetOpenBackupsLockForTest replaces the open of the backups folder's lock
// file (fsutil.OpenLockFile), and returns the restore.
func SetOpenBackupsLockForTest(fn func(dir, name string) (*os.File, error)) (restore func()) {
	prev := openBackupsLock
	openBackupsLock = fn
	return func() { openBackupsLock = prev }
}

// SetVerifyDeadlineForTest sets a verification's deadline (VerifyDeadline),
// and returns the restore.
func SetVerifyDeadlineForTest(d time.Duration) (restore func()) {
	prev := verifyDeadline
	verifyDeadline = d
	return func() { verifyDeadline = prev }
}

// SetOnLockWaitForTest sets the function a wait for the backups folder's
// lock runs once it finds the lock held, and returns the restore.
func SetOnLockWaitForTest(fn func()) (restore func()) {
	prev := onLockWait
	onLockWait = fn
	return func() { onLockWait = prev }
}

// SetVerdictWriteFailuresForTest sets the failures a verdict's write meets:
// at its staging (given the bundle), its rename (given the manifest's path)
// and the making of a veto (given the veto's path), each nil for none, and
// returns the restore.
func SetVerdictWriteFailuresForTest(stage func(dir string) error, rename func(path string) error, veto func(path string) error) (restore func()) {
	prevStage, prevRename, prevVeto := failStage, failRename, failVeto
	failStage, failRename, failVeto = stage, rename, veto
	return func() { failStage, failRename, failVeto = prevStage, prevRename, prevVeto }
}

// SetOnLockReleaseForTest sets the function a holder of the backups folder's
// lock runs as it lets the lock go, before its unlock, given the folder, and
// returns the restore: a test stands an unlock that does not answer in.
func SetOnLockReleaseForTest(fn func(dir string)) (restore func()) {
	prev := onLockRelease
	onLockRelease = fn
	return func() { onLockRelease = prev }
}

// SetOnOwnerReleaseForTest sets the function a snapshot's owner lock runs as
// it is let go, before its unlock, given the snapshot's resolved path, and
// returns the restore.
func SetOnOwnerReleaseForTest(fn func(dir string)) (restore func()) {
	prev := onOwnerRelease
	onOwnerRelease = fn
	return func() { onOwnerRelease = prev }
}

// SetOnClaimAnswerForTest sets the function a claim runs once its claim has
// answered, before its caller is handed the answer, and returns the restore.
func SetOnClaimAnswerForTest(fn func()) (restore func()) {
	prev := onClaimAnswer
	onClaimAnswer = fn
	return func() { onClaimAnswer = prev }
}

// SetReleaseBoundForTest sets how long a holder's letting go of an ownership
// or of the backups folder's lock may take before its hold is given up
// (StopWriteBound), and returns the restore.
func SetReleaseBoundForTest(d time.Duration) (restore func()) {
	prev := releaseBound
	releaseBound = d
	return func() { releaseBound = prev }
}

// SetRemoveVetoForTest replaces the removal of a veto (os.Remove), given the
// veto's path, and returns the restore: a test stands a removal that does not
// answer in for it.
func SetRemoveVetoForTest(fn func(path string) error) (restore func()) {
	prev := removeVeto
	removeVeto = fn
	return func() { removeVeto = prev }
}

// SetLookupVetoForTest replaces a reader's lookup of a veto by its name
// (os.Lstat), and returns the restore.
func SetLookupVetoForTest(fn func(path string) (os.FileInfo, error)) (restore func()) {
	prev := lookupVeto
	lookupVeto = fn
	return func() { lookupVeto = prev }
}

// SetReadBackFailureForTest sets the failure a verification's read-back of
// its record meets, given the snapshot (nil for none), and returns the
// restore.
func SetReadBackFailureForTest(fn func(dir string) error) (restore func()) {
	prev := failReadBack
	failReadBack = fn
	return func() { failReadBack = prev }
}

// TakeOwnerForTest takes the owner lock of the snapshot in dir as a capture
// does, so the snapshot reads as one whose verification is still owed, and
// answers its release.
func TakeOwnerForTest(dir string) (release func(), err error) {
	o, err := takeOwner(dir)
	if err != nil {
		return nil, err
	}
	return o.release, nil
}

// onVerifyAnswer and onVerifyExpired synchronize tests with the two outcome
// handoffs. Their callbacks must return unless dir is the test's snapshot.
var onVerifyAnswer atomic.Pointer[func(string)]
var onVerifyExpired atomic.Pointer[func(string)]

// SetVerifyHandoffForTest installs hooks before a worker hands its answer to
// its caller and once the caller chooses expiration, and returns the restore.
// Both hooks receive the caller's dir as passed to VerifySnapshot or
// FinishCapture: expiration can precede the worker's path resolution. This
// handoff seam is the exception to hooks that receive resolved snapshot paths.
func SetVerifyHandoffForTest(answer, expired func(string)) func() {
	var a, e *func(string)
	if answer != nil {
		a = &answer
	}
	if expired != nil {
		e = &expired
	}
	oldAnswer, oldExpired := onVerifyAnswer.Swap(a), onVerifyExpired.Swap(e)
	return func() { onVerifyAnswer.Store(oldAnswer); onVerifyExpired.Store(oldExpired) }
}

// onVerifyEnd, nil in production, runs once a verification's worker has let
// go of everything it held (SetOnVerifyEndForTest).
var onVerifyEnd atomic.Pointer[func(string)]

// SetOnVerifyEndForTest installs the hook a verification's worker runs last,
// once it has let go of everything it held, in verifyAndThen's order: the
// steps it gave up ended, the snapshot's ownership, the backups folder's
// marker and lock, this process's slot, then the mark of a hold given up
// (where the lock's let-go overran releaseBound, the slot goes while it is
// still under way, the mark standing until it ends, and the hook runs after
// that); and returns the restore. The hook receives the caller's dir as
// passed, as SetVerifyHandoffForTest's do. A worker runs the hook that stood
// as its verification began, never one set later: so a test sets it before
// the verification it waits for begins, and no worker begun earlier runs it
// (backlog B849's second round). A worker answers its caller before it lets
// go of any of it, so a test whose next step needs the lock, the slot or the
// ownership free waits for this, never for the answer (backlog B849). A
// verification refused before its worker starts runs none. Its callback must
// return, and ignore every dir but its test's.
func SetOnVerifyEndForTest(fn func(dir string)) (restore func()) {
	var p *func(string)
	if fn != nil {
		p = &fn
	}
	prev := onVerifyEnd.Swap(p)
	return func() { onVerifyEnd.Store(prev) }
}

// aroundVerifyEnd, nil in production, runs a verification worker's last step
// (SetAroundVerifyEndForTest).
var aroundVerifyEnd atomic.Pointer[func(dir string, end func())]

// SetAroundVerifyEndForTest installs fn in place of a verification worker's
// last step, given the caller's dir as passed and that step, end (the call of
// the end hook SetOnVerifyEndForTest sets), which fn must call once; and
// returns the restore. The worker comes to it once it has dropped its hold,
// which is all ActiveHoldsForTest counts, so a test parks a worker there, past
// every drain, and knows when its end hook has had its turn (backlog B849's
// second round).
func SetAroundVerifyEndForTest(fn func(dir string, end func())) (restore func()) {
	var p *func(string, func())
	if fn != nil {
		p = &fn
	}
	prev := aroundVerifyEnd.Swap(p)
	return func() { aroundVerifyEnd.Store(prev) }
}

// ActiveHoldsForTest counts workers still retaining a hold, including a
// cancellation handoff that has neither a slot nor an overrun mark yet.
// Tests join their callers before waiting for this count to reach zero.
func ActiveHoldsForTest() int64 { return activeHolds.Load() }

// VerifyExpiryErrorForTest returns the outer expiry's production error for
// stopErr (nil for watchdog expiry), so reporter tests use its actual wrapper.
func VerifyExpiryErrorForTest(stopErr error) error { return verifyExpiryError(stopErr) }

// stepBoundFixture shortens only named steps of one snapshot in a test;
// setup and the real SQLite check keep their normal deadline.
type stepBoundFixture struct {
	root, snapshot string
	within         time.Duration
	steps          map[string]bool
}

var stepBoundForTest atomic.Pointer[stepBoundFixture]

// SetStepBoundForTest bounds the named steps from their admission for the
// resolved snapshot dir. It returns a restore to run after callers and holds
// have ended. Other snapshots and steps keep their production bounds.
func SetStepBoundForTest(dir string, within time.Duration, steps ...string) func() {
	fixture := &stepBoundFixture{root: filepath.Dir(dir), snapshot: filepath.Base(dir), within: within, steps: make(map[string]bool, len(steps))}
	for _, step := range steps {
		fixture.steps[step] = true
	}
	prev := stepBoundForTest.Swap(fixture)
	return func() { stepBoundForTest.Store(prev) }
}

// beforeFilesCheck is the test seam between the two component checks.
var beforeFilesCheck func(string)

// SetBeforeFilesCheckForTest installs a hook after the database check and before
// the files check. Restore it only after all verification holds have ended.
func SetBeforeFilesCheckForTest(fn func(string)) func() {
	prev := beforeFilesCheck
	beforeFilesCheck = fn
	return func() { beforeFilesCheck = prev }
}

// filesCheckIncompleteForTest names one resolved snapshot whose files check
// expires internally, leaving the verification caller's context untouched.
var filesCheckIncompleteForTest atomic.Pointer[string]

// SetFilesCheckIncompleteForTest makes the files check of the resolved snapshot
// dir answer INCOMPLETE with ReasonDeadline through its own check context.
// Resolve dir before installing the seam, as for SetStepBoundForTest. Restore
// only after all callers and verification holds have ended.
func SetFilesCheckIncompleteForTest(dir string) (restore func()) {
	prev := filesCheckIncompleteForTest.Swap(&dir)
	return func() { filesCheckIncompleteForTest.Store(prev) }
}
