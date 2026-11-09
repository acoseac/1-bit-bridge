package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// The console's save that turns update checks off (backlog B761's round 3).
//
// `bridge update`, another process, reads the switch from the settings file
// at its install's decision under the install marker's lock (its
// InstallOptions.RereadSwitch, in decideUnderLock) and commits right after,
// and serve's off transition (ChecksSwitched) does not reach that process: an
// off the console saved between the read and the commit was installed past
// (Codex on #1446's second round). So the console's save of an off holds the
// install marker's lock while it decides and writes (LockForChecksOff): it
// lands before an install's decision, which then reads it and refuses, or
// after the install's swap, never between the two.
//
// The order of the locks such a save takes, which every path that holds more
// than one of them keeps:
//
//  1. the install marker's lock (update-state.lock, LockState), which an
//     install holds from just before its decision to after its swap
//     (lockMarkerForUpdate), and an off-save from before its decision whether
//     the off may be saved (ChecksOffRefused) to after the updater is told;
//  2. the config holder's lock (config.RuntimeConfig's mutex, and the turn of
//     bridge.yaml's own file lock that the holder's save takes), taken inside
//     the first through RuntimeConfig.UpdateChecked, and never the other way:
//     the marker lock is never asked for inside UpdateChecked's callback,
//     which runs with the holder's lock held;
//  3. the switch's in-process gate (Updater.gateMu), which ChecksSwitched
//     takes once the save is published, and an install's commit takes with
//     the marker lock held (decideUnderLock): the same order.
//
// Nothing in-process is held while the marker lock is waited for: the console
// takes it before its own lock (admin's Server.mu), which it holds across the
// holder's save and the updater's notice, and an install takes it only after
// its download, holding none of the others. The marker lock is a kernel lock
// on an open file (flock, LockFileEx), so two takers in one process exclude
// each other as two processes do, and a path that took it once waits on
// itself if it asks again: decideUnderLock runs under the install's hold, and
// never asks for it.

// ErrChecksOffLocked is what a *ChecksOffLockError matches: the install
// marker's lock could not be held for a save that turns update checks off,
// and nothing was saved.
var ErrChecksOffLocked = errors.New("the install marker's lock cannot be held for a save that turns update checks off")

// ChecksOffLockError is LockForChecksOff's refusal: the lock's own answer
// (a *StateLockError), in words that name no path.
type ChecksOffLockError struct {
	cause error
}

func (e *ChecksOffLockError) Error() string {
	if errors.Is(e.cause, ErrStateLockHeld) {
		return fmt.Sprintf("update checks were not turned off: %s (an install of another version, a start's housekeeping or a "+
			"store's migration), and the off waits for it so that an install cannot read the checks on and then install; "+
			"nothing was saved: save it again once that is done", e.cause)
	}
	return fmt.Sprintf("update checks were not turned off: %s, which keeps an install from reading the checks on "+
		"and then installing; nothing was saved", e.cause)
}

// Is makes a *ChecksOffLockError match ErrChecksOffLocked.
func (e *ChecksOffLockError) Is(target error) bool { return target == ErrChecksOffLocked }

func (e *ChecksOffLockError) Unwrap() error { return e.cause }

// LockForChecksOff takes the install marker's lock in dataDir for a save of
// the settings that turns update checks off, first of the locks it takes
// (the order above), and answers the hold, which its caller lets go once the
// save is published and the updater told.
//
// It waits as every taker of the lock does, LockState's wait (30 s), which an
// install's hold fits inside: its marker read, its two marker writes and its
// swap, which on Windows first waits up to scmStopWait for the SCM to stop
// the service (TestAnSCMStopFitsInsideTheMarkersLockWait holds it to half the
// wait). On Windows the service that a `bridge update` past its decision stops
// is the one serving the console's save, and the save's wait ends with that
// stop (the console's requests take serve's context): nothing is saved, and
// the version the swap installed starts with update checks as they were.
//
// On a filesystem that keeps no locks it answers a hold that is not held,
// and no error: the save goes on, since no install commits on such a
// filesystem (lockMarkerForUpdate refuses it there) and no crash rollback
// acts on one either, so nothing is left to keep apart. A holder past the
// wait, every try answering ENOLCK and any other failure of the lock are a
// *ChecksOffLockError, and a stop in the wait is ctx's error: either way
// nothing may be saved. A taker whose ctx ended once the lock is taken lets
// it go and answers ctx's error.
func LockForChecksOff(ctx context.Context, dataDir string) (*StateLock, error) {
	lock, err := LockState(ctx, dataDir)
	if err != nil {
		var refused *StateLockError
		if errors.As(err, &refused) {
			return nil, &ChecksOffLockError{cause: err}
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		lock.Release()
		return nil, err
	}
	return lock, nil
}

// ChecksOffWithin bounds the backup's checks an off-save's decision runs
// (ChecksOffRefused) together, with the install marker's lock held: a release
// binary answers its version and its settings check in milliseconds, and the
// bound is a third of LockState's wait, so an install that waits for the
// lock behind the decision still takes it.
const ChecksOffWithin = 10 * time.Second

// ChecksOffSave is what an off-save's decision (ChecksOffRefused) asks of the
// running bridge.
type ChecksOffSave struct {
	// DataDir holds the install marker.
	DataDir string
	// BinaryPath is the binary serve runs as, beside which the crash
	// rollback's backup stands (BinaryPath+".bak").
	BinaryPath string
	// ConfigPath is the settings file the save writes, as its name is
	// written: the backup's settings check reads it, and a copy of it with
	// the key written off beside it.
	ConfigPath string
	// Current is the running version, version.ServerVersion.
	Current string
	// Within bounds the backup's checks together; 0 is ChecksOffWithin.
	Within time.Duration

	// check and probe stand in for CheckConfig and ProbeVersionWithin in a
	// test; nil in production.
	check func(ctx context.Context, backup, configPath string) ConfigCheck
	probe func(ctx context.Context, backup string, within time.Duration) (string, error)
}

// ErrChecksOffKeepsTheBackup is what a *ChecksOffRefusal matches.
var ErrChecksOffKeepsTheBackup = errors.New("an off saved now would keep a crash rollback from putting the backup back")

// ChecksOffRefusal is ChecksOffRefused's refusal of an off: the install of
// Target is on probation, and its backup, of Previous, refuses settings that
// write update.checkEnabled.
type ChecksOffRefusal struct {
	Target, Previous string
}

func (e *ChecksOffRefusal) Error() string {
	return fmt.Sprintf("update checks were not turned off: %s is on probation since its install, and its backup, %s, "+
		"refuses settings that write update.checkEnabled, so an off saved now would keep a crash rollback from putting %s "+
		"back if %s keeps failing to start. Turn the checks off once %s is confirmed (a start of it answered as ready and "+
		"stayed up for %s), or now without writing the key: set BRIDGE_UPDATE_CHECK_ENABLED=false in the service's "+
		"environment and restart the service (%s does not read that variable, so it checks for updates again after a "+
		"rollback); nothing was saved", e.Target, e.Previous, e.Previous, e.Target, e.Target, ProbationWindow, e.Previous)
}

// Is makes a *ChecksOffRefusal match ErrChecksOffKeepsTheBackup.
func (e *ChecksOffRefusal) Is(target error) bool { return target == ErrChecksOffKeepsTheBackup }

// ErrChecksOffUnchecked is what a *ChecksOffUncheckedError matches.
var ErrChecksOffUnchecked = errors.New("whether an off saved now would keep a crash rollback from putting the backup back could not be told")

// ChecksOffUncheckedError is ChecksOffRefused's refusal, failing closed, of an
// off whose backup did not answer what the decision asks it: Detail says what
// failed, for the bridge's log alone, and may name the bridge's own files.
type ChecksOffUncheckedError struct {
	Target, Previous string
	Detail           string
}

func (e *ChecksOffUncheckedError) Error() string {
	return fmt.Sprintf("update checks were not turned off: %s is on probation since its install, and its backup, %s, did not "+
		"answer whether it takes settings that write update.checkEnabled, so whether an off saved now would keep a crash "+
		"rollback from putting it back cannot be told; nothing was saved. Try again, or turn the checks off without writing "+
		"the key: set BRIDGE_UPDATE_CHECK_ENABLED=false in the service's environment and restart the service",
		e.Target, e.Previous)
}

// Is makes a *ChecksOffUncheckedError match ErrChecksOffUnchecked.
func (e *ChecksOffUncheckedError) Is(target error) bool { return target == ErrChecksOffUnchecked }

// ChecksOffMarkerError is ChecksOffRefused's refusal, failing closed, of an
// off over an install marker it could not read (backlog B761's round 4): what
// the boot's crash rollback would do cannot be told from a marker that was not
// read, so its words name no version, no backup and no path, and give the one
// way to turn the checks off without writing the key. Detail is the read's
// error without its path, for the bridge's log alone (an install marker's read
// error names no path in a refusal or a log line, B613's round 5), and Unwrap
// keeps the error for code. It matches ErrChecksOffUnchecked.
type ChecksOffMarkerError struct {
	Detail string
	err    error
}

func (e *ChecksOffMarkerError) Error() string {
	return "update checks were not turned off: the install marker could not be read, so whether an off saved now " +
		"would keep a crash rollback from putting a backup back cannot be told; nothing was saved. To turn the checks " +
		"off without writing update.checkEnabled, set BRIDGE_UPDATE_CHECK_ENABLED=false in the service's environment " +
		"and restart the service"
}

// Is makes a *ChecksOffMarkerError match ErrChecksOffUnchecked.
func (e *ChecksOffMarkerError) Is(target error) bool { return target == ErrChecksOffUnchecked }

func (e *ChecksOffMarkerError) Unwrap() error { return e.err }

// ChecksOffRefused decides whether an off of update checks may be saved now,
// under the install marker's lock its caller holds (LockForChecksOff), and
// answers nil where it may (backlog B761's round 3, the Claude review's Medium
// on #1446). The console's off writes update.checkEnabled: false, and a
// release from before the switch refuses settings that write that key (its
// loader's KnownFields decode), so the crash rollback's settings check of such
// a backup refuses, and a bridge whose new version keeps crashing would go on
// with it.
//
// It asks what the boot asks before its crash rollback acts, in its order
// (cmd/bridge's recoverCrashedInstall): the marker, read under the lock, at the
// crash rollback's own checks at their limit (CrashRollbackAtLimit: an install
// of Current armed, swapped, over another version, no migration of the store
// begun, no other version recorded over the data dir), whatever its count;
// then a regular file at BinaryPath+".bak", the backup it would put back. Where
// either rules the crash rollback out, the save takes nothing from it, and is
// no refusal. Otherwise it asks the backup as the crash rollback asks it
// (CheckConfig: its own `serve --validate-config`), first of the settings as
// the file holds them with update.checkEnabled written false
// (config.WithUpdateChecksOff, in a copy beside the settings file, so that
// relative paths read as they do there, removed after): one that takes them,
// or that predates the check (which the crash rollback refuses whatever the
// settings), takes nothing from the rollback. One that refuses them is then
// asked its version (ProbeVersionWithin) and the settings as they are: of
// PreviousVersion and taking them, so that the key the save writes is what
// it refuses, the off is refused (*ChecksOffRefusal); of another version, or
// refusing the settings already, the crash rollback refuses it whatever the
// off, and the off is saved.
//
// The checks run within s.Within together, ChecksOffWithin by default. One
// that does not answer within it, fails, or cannot be asked (the copy cannot
// be written), refuses the off, failing closed (*ChecksOffUncheckedError); a
// stop of ctx is ctx's error.
//
// The marker is read as every start reads it, through LoadState, which
// answers a marker that is not there, and one that does not parse (with its
// Warn), as no install: a zero State and no error. The boot does the same with
// both: its crash rollback (cmd/bridge's recoverCrashedInstall) finds no
// install armed in either, and its admission (AdmitBoot) records nothing over
// them, so no crash rollback acts on either, and the off is saved. Any other
// failure to read the marker (a permission, EIO, EMFILE, a directory in its
// place) refuses the off, failing closed (*ChecksOffMarkerError, backlog
// B761's round 4). A start that cannot read the marker either does nothing
// with it but remove it (recoverCrashedInstall leaves it to its admission,
// which removes it, durably), but this serve cannot tell whether the start
// that reads it next fails too: an error that does not last (EMFILE on a busy
// serve, EIO) passes, and a start run as another user, root among them, reads
// what this one may not, and its crash rollback may then act on an install
// the off kept from its backup.
func ChecksOffRefused(ctx context.Context, s ChecksOffSave) error {
	st, err := LoadState(s.DataDir)
	if err != nil {
		return &ChecksOffMarkerError{Detail: fsutil.ErrorWithoutPath(err), err: err}
	}
	if ok, _ := CrashRollbackAtLimit(st, s.Current); !ok || !backupBeside(s.BinaryPath) {
		return nil
	}
	within := s.Within
	if within <= 0 {
		within = ChecksOffWithin
	}
	check, probe := s.check, s.probe
	if check == nil {
		check = CheckConfig
	}
	if probe == nil {
		probe = ProbeVersionWithin
	}
	cctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	backup := s.BinaryPath + ".bak"
	unchecked := func(detail string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return &ChecksOffUncheckedError{Target: st.TargetVersion, Previous: st.PreviousVersion, Detail: detail}
	}
	withKey, cleanup, err := checksOffCopy(s.ConfigPath)
	if err != nil {
		return unchecked(err.Error())
	}
	defer cleanup()
	switch v := check(cctx, backup, withKey); v.Verdict {
	case ConfigValid, ConfigCheckUnsupported:
		return nil
	case ConfigCheckFailed:
		return unchecked("the backup's settings check of the off did not answer: " + v.Detail)
	}
	remaining := time.Until(deadlineOf(cctx))
	if remaining <= 0 {
		return unchecked("the backup's checks ran past their bound")
	}
	got, err := probe(cctx, backup, remaining)
	if err != nil {
		return unchecked("the backup did not say its version: " + err.Error())
	}
	if !SameVersion(got, st.PreviousVersion) {
		return nil
	}
	switch v := check(cctx, backup, s.ConfigPath); v.Verdict {
	case ConfigValid:
		return &ChecksOffRefusal{Target: st.TargetVersion, Previous: st.PreviousVersion}
	case ConfigRefused, ConfigCheckUnsupported:
		return nil
	default:
		return unchecked("the backup's settings check of the settings as they are did not answer: " + v.Detail)
	}
}

// deadlineOf is ctx's deadline, which ChecksOffRefused gave it.
func deadlineOf(ctx context.Context) time.Time {
	d, _ := ctx.Deadline()
	return d
}

// checksOffCopy writes, beside the settings file at configPath, a copy of it
// with update.checkEnabled written false (config.WithUpdateChecksOff), for the
// backup's settings check to read: beside it, so that its relative paths read
// against the directory they read against there. cleanup removes it.
func checksOffCopy(configPath string) (path string, cleanup func(), err error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "", nil, fmt.Errorf("the settings could not be read: %w", err)
	}
	withKey, err := config.WithUpdateChecksOff(raw)
	if err != nil {
		return "", nil, fmt.Errorf("the settings could not be written with the key: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(configPath), "."+filepath.Base(configPath)+".checks-off-*")
	if err != nil {
		return "", nil, fmt.Errorf("the copy of the settings could not be made: %w", err)
	}
	cleanup = func() { _ = os.Remove(f.Name()) }
	if _, err := f.Write(withKey); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("the copy of the settings could not be written: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("the copy of the settings could not be written: %w", err)
	}
	return f.Name(), cleanup, nil
}
