package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B660: an install counts as healthy only once it has answered and
// stayed up. The version installed booting is no proof: until B660 its first
// boot's housekeeping stamped the install installed before anything listened,
// and the boot after it removed the backup, so a new version that crashed
// after its first housekeeping (under Restart=always, about two seconds
// later) had its backup deleted by its own second start, after `bridge
// update` had promised a rollback.

// probationWatch collects what serve's confirmation of an install does, as
// serveOpts.probation's observer is told it.
type probationWatch struct {
	mu     sync.Mutex
	seen   []probationEvent
	signal chan struct{}
}

// newProbationWatch returns a watch and the seams that report to it, with
// timing in place of production's: a zero field keeps production's.
func newProbationWatch(timing probationTiming) (*probationWatch, *probationSeams) {
	w := &probationWatch{signal: make(chan struct{}, 1)}
	return w, &probationSeams{timing: timing, observe: w.observe}
}

func (w *probationWatch) observe(e probationEvent) {
	w.mu.Lock()
	w.seen = append(w.seen, e)
	w.mu.Unlock()
	select {
	case w.signal <- struct{}{}:
	default:
	}
}

// events is what the confirmation has done so far.
func (w *probationWatch) events() []probationEvent {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]probationEvent(nil), w.seen...)
}

// waitFor returns the first event of kind, waiting for one until the test's
// deadline (serveGiveUpTime).
func (w *probationWatch) waitFor(t *testing.T, kind probationEventKind) probationEvent {
	t.Helper()
	giveUp := serveGiveUpTime(t)
	for {
		for _, e := range w.events() {
			if e.kind == kind {
				return e
			}
		}
		wait := withinTheTest
		if !giveUp.IsZero() {
			wait = time.Until(giveUp)
		}
		select {
		case <-w.signal:
		case <-time.After(wait):
			t.Fatalf("the confirmation never did %d; it did %+v", kind, w.events())
		}
	}
}

// shortProbation is a window a test can wait out, with asks quick enough
// that a round comes within it.
var shortProbation = probationTiming{window: 300 * time.Millisecond, ask: 20 * time.Millisecond,
	retry: 20 * time.Millisecond, round: 5 * time.Second}

// bootWith boots the real serve on r's settings and binary with opts, as boot
// does, until it listens.
func (r *refusedInstall) bootWith(t *testing.T, opts serveOpts) servedBridge {
	t.Helper()
	opts.configPath, opts.binaryPath = r.cfg, r.bin
	return launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) int {
		return runServe(ctx, opts, stdout, stderr)
	})
}

// markerOf reads the install marker in dataDir.
func markerOf(t *testing.T, dataDir string) updater.State {
	t.Helper()
	st, err := updater.LoadState(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// startThatPanics runs serve on r's install with a panic where serve runs its
// player's state preflight, right after its admission, on serve's own
// goroutine, and recovers it around runServe, as a supervisor's restart
// follows a crash: a start that did not end cleanly. runServe recovers
// nothing itself.
func (r *refusedInstall) startThatPanics(t *testing.T) {
	t.Helper()
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		var out, errs bytes.Buffer
		runServe(t.Context(), serveOpts{configPath: r.cfg, binaryPath: r.bin,
			playerStatePreflight: func(string, bool) error { panic("a crash of the start") }}, &out, &errs)
	}()
	if !panicked {
		t.Fatal("serve returned where its player's preflight panics: the start was meant to crash there")
	}
}

// serveToItsEnd runs serve with opts on a goroutine until it exits, and
// returns its code and what it said; a serve that prints its banner first
// went on to serve, which it says in served, stopped at once.
func serveToItsEnd(t *testing.T, opts serveOpts) (code int, stderr string, served bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stdout, errs := &safeBuffer{}, &safeBuffer{}
	codes, done := make(chan int, 1), make(chan struct{})
	go func() {
		defer close(done)
		codes <- runServe(ctx, opts, stdout, errs)
	}()
	drainLoopOnCleanup(t, cancel, done, "serve")
	giveUp := serveGiveUpTime(t)
	for giveUp.IsZero() || time.Now().Before(giveUp) {
		select {
		case code := <-codes:
			return code, errs.String(), false
		case <-time.After(10 * time.Millisecond):
		}
		if strings.Contains(stdout.String(), "listening on https://") {
			cancel()
			return <-codes, errs.String(), true
		}
	}
	t.Fatalf("serve neither exited nor listened before the test's deadline; stderr=%s", errs.String())
	return 0, "", false
}

// TestACrashAfterTheFirstBootKeepsTheBackup: the housekeeping of a start of
// the version installed runs before anything listens, so it confirms nothing:
// the install stays on probation and the backup stays beside the binary, at
// that start and at the next, as two starts that crashed after their
// housekeeping leave them. Until B660 the first stamped the install installed
// and the second removed the backup.
func TestACrashAfterTheFirstBootKeepsTheBackup(t *testing.T) {
	r := newRefusedInstall(t, "")
	// What the start's admission recorded before its housekeeping.
	r.marker.DataDirOpenedBy = version.ServerVersion
	r.saveMarker(t, r.dataDir)
	for boot := 1; boot <= 2; boot++ {
		var stderr bytes.Buffer
		if err := bootHousekeeping(t.Context(), &stderr, r.dataDir, r.bin, version.ServerVersion, nil); err != nil {
			t.Fatalf("the housekeeping of start %d = %v; stderr=%s", boot, err, stderr.String())
		}
		if st := markerOf(t, r.dataDir); st.Status != "installing" || !st.InstalledAt.IsZero() {
			t.Errorf("the marker after the housekeeping of start %d = %+v, want the install still on probation: nothing listened yet",
				boot, st)
		}
		if _, err := os.Lstat(r.bak); err != nil {
			t.Fatalf("the backup after the housekeeping of start %d: %v; want it kept until the install is confirmed", boot, err)
		}
	}
}

// TestAnInstallIsConfirmedOnlyOnceItAnsweredAndStayedUp boots this build's
// serve over an install of it: the install stays on probation, its backup
// kept, from the start through the window that begins at its first ready
// answer, and is stamped installed at the window's end; the start after it
// removes the backup, as after any confirmed install.
func TestAnInstallIsConfirmedOnlyOnceItAnsweredAndStayedUp(t *testing.T) {
	r := newRefusedInstall(t, "")
	watchEvents, seams := newProbationWatch(shortProbation)
	sb := r.bootWith(t, serveOpts{probation: seams})
	if st := markerOf(t, r.dataDir); st.Status != "installing" || st.FailedStarts != 1 {
		t.Fatalf("the marker once serve listens = %+v, want the install on probation, this start counted; stderr=%s", st, sb.stderr.String())
	}
	watchEvents.waitFor(t, probationWindow)
	if st := markerOf(t, r.dataDir); st.Status != "installing" {
		t.Fatalf("the marker as the window starts = %+v, want the install on probation", st)
	}
	if _, err := os.Lstat(r.bak); err != nil {
		t.Fatalf("the backup during the window: %v", err)
	}
	watchEvents.waitFor(t, probationConfirmed)
	st := markerOf(t, r.dataDir)
	if st.Status != "installed" || st.InstalledAt.IsZero() || st.FailedStarts != 0 || st.DataDirOpenedBy != version.ServerVersion {
		t.Fatalf("the marker after the window = %+v, want the install confirmed, its count cleared, its record kept", st)
	}
	if _, err := os.Lstat(r.bak); err != nil {
		t.Fatalf("the backup after the confirmation: %v; want it kept for one more start", err)
	}
	stopServed(t, sb)
	if after := markerOf(t, r.dataDir); after != st {
		t.Errorf("the marker after the stop = %+v, want it as the confirmation left it, %+v", after, st)
	}

	stopServed(t, r.bootWith(t, serveOpts{}))
	if _, err := os.Lstat(r.bak); !os.IsNotExist(err) {
		t.Errorf("the backup after the start that follows: lstat %v, want it removed", err)
	}
}

// TestThreeFailedStartsRollBackAnInstallThatMigratedNothing: three starts of
// this build over its install crash right after their admission, before the
// store opens. The fourth start puts the backup back before it writes
// anything: the hold and the veto of this version durable before the rename,
// exit 75 for the service manager to start the backup's version, no store, no
// pid file, the settings untouched. The next boot, as the backup's version,
// keeps the hold.
func TestThreeFailedStartsRollBackAnInstallThatMigratedNothing(t *testing.T) {
	r := newRefusedInstall(t, "")
	settings := watch(t, r.cfg)
	for start := 1; start <= updater.ProbationFailedStarts; start++ {
		r.startThatPanics(t)
	}
	var atRename updater.State
	code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin,
		restoreBackup: func(dst, ext string) error {
			// On serve's goroutine: t.Error, never t.Fatal.
			st, err := updater.LoadState(r.dataDir)
			if err != nil {
				t.Error(err)
			}
			atRename = st
			return updater.RollbackBinary(dst, ext)
		}})
	if served || code != supervision.RestartExitCode {
		t.Fatalf("the fourth start = %d (served %v), want %d before it served: three starts did not end cleanly; stderr=%s",
			code, served, supervision.RestartExitCode, stderr)
	}
	if atRename.Status != "" || !atRename.RollbackStarted || atRename.RejectedVersion != version.ServerVersion ||
		atRename.MigrationVeto != version.ServerVersion || atRename.RejectedConfigDigest != "" ||
		atRename.FailedStarts != updater.ProbationFailedStarts || atRename.DataDirOpenedBy != version.ServerVersion {
		t.Errorf("the marker as the backup was put back = %+v, want the rollback's record: this version held back for every setting "+
			"and its migrations vetoed, the count and the record of the data dir kept", atRename)
	}
	if !strings.Contains(atRename.RejectedReason, "did not end cleanly") {
		t.Errorf("the record's reason = %q, want it to say the starts did not end cleanly", atRename.RejectedReason)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q after the rollback, want %s", got, previousVersion)
	}
	if _, err := os.Lstat(r.bak); !os.IsNotExist(err) {
		t.Errorf("the backup is still there (lstat %v): it was not put back", err)
	}
	requireNoStoreOpened(t, r.dataDir)
	settings.requireUnchanged(t)
	if said := updaterLines(stderr); !strings.Contains(said, previousVersion) {
		t.Errorf("the fourth start did not say which version it put back; stderr=%s", stderr)
	}

	// The backup's version starts: its admission and its housekeeping finish
	// the record, and keep the hold and the veto.
	if _, err := updater.AdmitBoot(t.Context(), r.dataDir, previousVersion, nil); err != nil {
		t.Fatalf("the backup's version's admission = %v", err)
	}
	var housekeeping bytes.Buffer
	if err := bootHousekeeping(t.Context(), &housekeeping, r.dataDir, r.bin, previousVersion, nil); err != nil {
		t.Fatalf("the backup's version's housekeeping = %v; stderr=%s", err, housekeeping.String())
	}
	st := markerOf(t, r.dataDir)
	if updater.InstallArmed(st) || st.RejectedVersion != version.ServerVersion || st.MigrationVeto != version.ServerVersion {
		t.Errorf("the marker after the backup's version's start = %+v, want no install armed, the hold and the veto kept", st)
	}
	if hold := updater.HeldBack(r.dataDir, r.cfg, version.ServerVersion); hold.Version == "" {
		t.Errorf("the auto-installer is not held back from %s after its rollback", version.ServerVersion)
	}
}

// TestARollbackIsRefusedAndReportedOnceAMigrationBegan: this build's first
// start over its install migrates a store a step behind, and records that a
// migration began though its admission recorded that it opened the data dir
// first. Three starts then crash, and the fourth, which the count asks to roll
// back, refuses, since the backup's version does not read the store past its
// own migrations: it records why, says so, and goes on serving.
func TestARollbackIsRefusedAndReportedOnceAMigrationBegan(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.withAnOlderStore(t)
	stopServed(t, r.bootWith(t, serveOpts{}))
	if st := markerOf(t, r.dataDir); st.StoreMigratedBy != version.ServerVersion || st.DataDirOpenedBy != version.ServerVersion {
		t.Fatalf("the marker after a start that migrated the store = %+v, want it to record that %s began a migration", st, version.ServerVersion)
	}
}
