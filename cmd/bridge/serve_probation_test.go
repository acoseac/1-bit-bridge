package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
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
	return w.waitForCount(t, kind, 1)
}

// waitForCount returns the nth event of kind, waiting for it until the test's
// deadline (serveGiveUpTime), or two minutes in a binary with none.
func (w *probationWatch) waitForCount(t *testing.T, kind probationEventKind, n int) probationEvent {
	t.Helper()
	giveUp := serveGiveUpTime(t)
	if giveUp.IsZero() {
		giveUp = time.Now().Add(2 * time.Minute)
	}
	for {
		seen := 0
		for _, e := range w.events() {
			if e.kind == kind {
				if seen++; seen == n {
					return e
				}
			}
		}
		select {
		case <-w.signal:
		case <-time.After(time.Until(giveUp)):
			t.Fatalf("the confirmation's %v never came %d time(s); it did %+v", kind, n, w.events())
		}
	}
}

// String names an event's kind in a test's failure, where a number says
// nothing.
func (k probationEventKind) String() string {
	switch k {
	case probationRound:
		return "round"
	case probationWindow:
		return "window"
	case probationConfirmed:
		return "confirmation"
	case probationNotReady:
		return "not-ready"
	case probationUnconfirmed:
		return "unconfirmed"
	case probationEnded:
		return "end"
	}
	return fmt.Sprintf("kind %d", int(k))
}

// count is how many events of kind the confirmation has done.
func (w *probationWatch) count(kind probationEventKind) int {
	n := 0
	for _, e := range w.events() {
		if e.kind == kind {
			n++
		}
	}
	return n
}

// shortProbation is a window a test can wait out, with asks quick enough
// that a round comes within it.
var shortProbation = probationTiming{window: 300 * time.Millisecond, ask: 20 * time.Millisecond,
	retry: 20 * time.Millisecond, round: 5 * time.Second}

// bootWith boots the real serve on r's settings and binary with opts, as boot
// does, until it listens, giving the copied backup the tests' probe bound.
func (r *refusedInstall) bootWith(t *testing.T, opts serveOpts) servedBridge {
	t.Helper()
	opts.configPath, opts.binaryPath = r.cfg, r.bin
	if opts.backupProbeTimeout == 0 {
		opts.backupProbeTimeout = testBackupProbeTimeout
	}
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
			backupProbeTimeout:   testBackupProbeTimeout,
			playerStatePreflight: func(string, bool) error { panic("a crash of the start") }}, &out, &errs)
	}()
	if !panicked {
		t.Fatal("serve returned where its player's preflight panics: the start was meant to crash there")
	}
}

// serveToItsEnd runs serve with opts on a goroutine until it exits, and
// returns its code and what it said; a serve that prints its banner first
// went on to serve, which it says in served, stopped at once. The copied
// backup gets the tests' probe bound unless opts gives another.
func serveToItsEnd(t *testing.T, opts serveOpts) (code int, stderr string, served bool) {
	t.Helper()
	if opts.backupProbeTimeout == 0 {
		opts.backupProbeTimeout = testBackupProbeTimeout
	}
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
// own migrations: it records why, says so on its stderr, and goes on serving,
// and `bridge doctor` and the console's Updates panel say it, failing.
func TestARollbackIsRefusedAndReportedOnceAMigrationBegan(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.withAnOlderStore(t)
	stopServed(t, r.bootWith(t, serveOpts{}))
	st := markerOf(t, r.dataDir)
	if st.StoreMigratedBy != version.ServerVersion || st.DataDirOpenedBy != version.ServerVersion {
		t.Fatalf("the marker after a start that migrated the store = %+v, want it to record that %s began a migration", st, version.ServerVersion)
	}
	if st.FailedStarts != 0 {
		t.Fatalf("the marker after a start that stopped cleanly = %+v, want its count taken back", st)
	}
	for start := 1; start <= updater.ProbationFailedStarts; start++ {
		r.startThatPanics(t)
	}
	binaries := watch(t, r.bin, r.bak)
	_, seams := newProbationWatch(longProbation)
	sb, admin := r.bootConsole(t, serveOpts{probation: seams})
	if said := updaterLines(sb.stderr.String()); !strings.Contains(said, "not rolling back the install") || !strings.Contains(said, "began a migration") {
		t.Errorf("the fourth start did not say why it kept the install; its updater lines: %q", said)
	}
	st = markerOf(t, r.dataDir)
	if !strings.Contains(st.RollbackRefused, "began a migration") || st.FailedStarts != updater.ProbationFailedStarts+1 || st.Status != "installing" {
		t.Errorf("the marker as the fourth start serves = %+v, want the refusal recorded and this start counted", st)
	}
	if strings.ContainsAny(st.RollbackRefused, `/\`) {
		t.Errorf("the recorded refusal %q names a path", st.RollbackRefused)
	}
	if line := doctorLineOf(t, r.cfg); line == nil || line.Status != "fail" || !strings.Contains(line.Summary, "the backup is not put back") {
		t.Errorf("bridge doctor's install-probation line = %+v, want a fail saying the backup is not put back", line)
	}
	if panel := probationPanel(t, admin); panel.Level != "fail" || !strings.Contains(panel.Says, "the backup is not put back") {
		t.Errorf("the console's Probation row = %+v, want a fail saying the backup is not put back", panel)
	}
	stopServed(t, sb)
	binaries.requireUnchanged(t)
}

// longProbation keeps a start on probation for as long as a test runs, its
// first ready answer quick.
var longProbation = probationTiming{window: time.Hour, ask: 20 * time.Millisecond, retry: 20 * time.Millisecond, round: 5 * time.Second}

// writeListeners writes r's settings with the API and the console on the
// addresses given.
func (r *refusedInstall) writeListeners(t *testing.T, listen, admin string) {
	t.Helper()
	body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\nlistenAddress: %s\nadminAddress: %s\ndisableHttp3: true\n",
		filepath.Join(r.dir, "Music"), r.dataDir, listen, admin)
	if err := os.WriteFile(r.cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// bootConsole boots serve on r's install as bootWith does, with its API and
// its console on loopback ports the test holds and hands it, and returns the
// console's base URL once the console answers.
func (r *refusedInstall) bootConsole(t *testing.T, opts serveOpts) (servedBridge, string) {
	t.Helper()
	lan, console := holdLoopback(t), holdLoopback(t)
	r.writeListeners(t, lan.addr, console.addr)
	opts.addrOverride, opts.lanListener, opts.adminListener = lan.addr, lan.ln, console.ln
	sb := r.bootWith(t, opts)
	waitForAdminReady(t, console.addr, sb.done, sb.stderr)
	return sb, "http://" + console.addr
}

// doctorLineOf runs `bridge doctor --json` on the settings at cfg, as an
// operator beside the bridge would, and returns its install-probation line,
// nil where it has none.
func doctorLineOf(t *testing.T, cfg string) *jsonDoctorCheck {
	t.Helper()
	var stdout, stderr bytes.Buffer
	run(t.Context(), []string{"doctor", "--json", "--config", cfg}, &stdout, &stderr)
	var report jsonDoctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("bridge doctor --json: %v; stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	for i := range report.Checks {
		if report.Checks[i].Name == installProbationCheckName {
			return &report.Checks[i]
		}
	}
	return nil
}

// probationRow is the console's Updates panel's Probation row, as GET
// /api/updates carries it.
type probationRow struct {
	Says  string `json:"probation"`
	Level string `json:"probationLevel"`
}

func probationPanel(t *testing.T, admin string) probationRow {
	t.Helper()
	var row probationRow
	getJSON(t, &http.Client{Timeout: 30 * time.Second}, admin+"/api/updates", "", &row)
	return row
}

// TestAHeldScanDoesNotDelayTheConfirmation: a start whose first scan is
// parked on the scanner's own seam for the whole window, parked before the
// window starts and still parked when it ends, is confirmed all the same: a
// scan, and how far it got, are not readiness's business, so a slow first
// scan cannot hold an install's confirmation back, and nothing is rolled
// back. The rounds read the real store beside the parked scan.
func TestAHeldScanDoesNotDelayTheConfirmation(t *testing.T) {
	r := newRefusedInstall(t, "")
	writeHiResFLACFixture(t, filepath.Join(r.dir, "Music", "Artist", "Album", "01.flac"))
	binaries := watch(t, r.bin, r.bak)
	parked, released, returned := newOnceCloser(), newOnceCloser(), newOnceCloser()
	t.Cleanup(released.close)
	var parkedAtWindow atomic.Bool
	events, seams := newProbationWatch(shortProbation)
	observe := seams.observe
	seams.observe = func(e probationEvent) {
		switch e.kind {
		case probationRound:
			// The window starts at the first ready answer: it waits, on the
			// confirmation's own goroutine, until the scan is parked, so the
			// whole window passes beside the parked scan.
			select {
			case <-parked.ch:
			case <-time.After(withinTheTest):
			}
		case probationWindow:
			select {
			case <-parked.ch:
				parkedAtWindow.Store(true)
			default:
			}
		}
		observe(e)
	}
	sb := r.bootWith(t, serveOpts{probation: seams, scannerBoot: holdTheFirstExtraction(parked, released, returned)})
	events.waitFor(t, probationConfirmed)
	if !parkedAtWindow.Load() {
		t.Fatal("the scan was not parked as the window started: the test shows nothing")
	}
	select {
	case <-returned.ch:
		t.Fatal("the scan was let go before the confirmation")
	default:
	}
	if st := markerOf(t, r.dataDir); st.Status != "installed" {
		t.Errorf("the marker = %+v, want the install confirmed beside a scan held for the whole window", st)
	}
	released.close()
	stopServed(t, sb)
	binaries.requireUnchanged(t)
}

// TestAStopDuringAHeldScanIsNotAFailedStart: serve's stop while its first
// scan is parked, the install still on probation, ends serve cleanly, and
// the start takes back its count: a stop is no failed start, whatever the
// scan was doing.
func TestAStopDuringAHeldScanIsNotAFailedStart(t *testing.T) {
	r := newRefusedInstall(t, "")
	writeHiResFLACFixture(t, filepath.Join(r.dir, "Music", "Artist", "Album", "01.flac"))
	binaries := watch(t, r.bin, r.bak)
	parked, released, returned := newOnceCloser(), newOnceCloser(), newOnceCloser()
	t.Cleanup(released.close)
	_, seams := newProbationWatch(longProbation)
	sb := r.bootWith(t, serveOpts{probation: seams, scannerBoot: holdTheFirstExtraction(parked, released, returned)})
	select {
	case <-parked.ch:
	case <-time.After(withinTheTest):
		t.Fatal("the first scan never reached its first file")
	}
	if st := markerOf(t, r.dataDir); st.FailedStarts != 1 {
		t.Fatalf("the marker while the scan is parked = %+v, want this start counted", st)
	}
	stopServed(t, sb)
	select {
	case <-returned.ch:
	default:
		t.Error("serve stopped with its scan still parked")
	}
	if st := markerOf(t, r.dataDir); st.FailedStarts != 0 || st.Status != "installing" {
		t.Errorf("the marker after the stop = %+v, want this start's count taken back, the install still on probation", st)
	}
	binaries.requireUnchanged(t)
}

// holdTheFirstExtraction parks serve's scan at its first extracted file,
// telling parked, until released or the scan's own end (serve's stop),
// telling returned once it lets go.
func holdTheFirstExtraction(parked, released, returned *onceCloser) func(*manifest.Scanner) {
	return func(sc *manifest.Scanner) {
		sc.SetAfterExtractHookForTests(func(string) {
			parked.close()
			select {
			case <-sc.InFlightScanDone():
			case <-released.ch:
			}
			returned.close()
		})
	}
}

// TestAStartThatNeverAnswersIsNeverConfirmed: a start whose store never
// answers a round is not ready, never confirmed, keeps running and rolls
// nothing back, since not ready is no proof of a broken version: at the
// window's first lapse it says so once, records NotReady, and the doctor
// fails on it.
func TestAStartThatNeverAnswersIsNeverConfirmed(t *testing.T) {
	r := newRefusedInstall(t, "")
	binaries := watch(t, r.bin, r.bak)
	events, seams := newProbationWatch(shortProbation)
	seams.storeRead = func(context.Context) error { return errors.New("the store is away") }
	sb := r.bootWith(t, serveOpts{probation: seams})
	events.waitFor(t, probationNotReady)
	more := events.count(probationRound) + 3
	events.waitForCount(t, probationRound, more)
	st := markerOf(t, r.dataDir)
	if st.Status != "installing" || !st.NotReady {
		t.Errorf("the marker past the window = %+v, want the install unconfirmed, NotReady recorded", st)
	}
	if n := strings.Count(sb.stderr.String(), "has not answered as ready"); n != 1 {
		t.Errorf("serve said it has not answered as ready %d times, want once; stderr=%s", n, sb.stderr.String())
	}
	if line := doctorLineOf(t, r.cfg); line == nil || line.Status != "fail" || !strings.Contains(line.Summary, "did not answer as ready") {
		t.Errorf("bridge doctor's install-probation line = %+v, want a fail saying a start did not answer", line)
	}
	select {
	case code := <-sb.done:
		t.Fatalf("serve exited %d: a start that does not answer keeps running", code)
	default:
	}
	if n := events.count(probationConfirmed); n != 0 {
		t.Errorf("the install was confirmed %d times by a start that never answered", n)
	}
	stopServed(t, sb)
	binaries.requireUnchanged(t)
	if st := markerOf(t, r.dataDir); st.Status != "installing" || st.FailedStarts != 0 {
		t.Errorf("the marker after the stop = %+v, want the install unconfirmed and this start taken back", st)
	}
}

// firstRoundOf boots serve on r with seams and the options given, and
// returns its first round's answer.
func firstRoundOf(t *testing.T, r *refusedInstall, timing probationTiming, opts serveOpts) (probationEvent, servedBridge) {
	t.Helper()
	events, seams := newProbationWatch(timing)
	if opts.probation != nil {
		seams.storeRead = opts.probation.storeRead
	}
	opts.probation = seams
	sb := r.bootWith(t, opts)
	return events.waitFor(t, probationRound), sb
}

// acceptsNothing is a bound listener whose Accept takes nothing until it is
// closed: a connect to it completes in the kernel's backlog, and nobody
// accepts it.
type acceptsNothing struct {
	net.Listener
	once   sync.Once
	closed chan struct{}
}

func (l *acceptsNothing) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *acceptsNothing) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

// TestASocketNobodyAcceptsOnIsNotReady: a connect to the API listener's
// address that the kernel completes while this process's accept loop takes
// nothing is not a ready answer.
func TestASocketNobodyAcceptsOnIsNotReady(t *testing.T) {
	r := newRefusedInstall(t, "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	nobody := &acceptsNothing{Listener: ln, closed: make(chan struct{})}
	t.Cleanup(func() { _ = nobody.Close() })
	round, sb := firstRoundOf(t, r, probationTiming{window: time.Hour, ask: 20 * time.Millisecond, retry: 20 * time.Millisecond,
		round: 300 * time.Millisecond}, serveOpts{lanListener: nobody, addrOverride: ln.Addr().String()})
	if round.ready || !strings.Contains(round.why, "accept loop") {
		t.Errorf("the first round over a listener nobody accepts on = %+v, want not ready, the accept loop named", round)
	}
	stopServed(t, sb)
}

// TestAHealthAnswerOfAnotherVersionIsNotReady: /v1/health answering 200 as
// another version than this start's is not a ready answer.
func TestAHealthAnswerOfAnotherVersionIsNotReady(t *testing.T) {
	r := newRefusedInstall(t, "")
	round, sb := firstRoundOf(t, r, longProbation, serveOpts{wrapAPIHandler: func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/v1/health" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"serverVersion":"9.9.9"}`)
				return
			}
			h.ServeHTTP(w, req)
		})
	}})
	if round.ready || !strings.Contains(round.why, `"9.9.9"`) {
		t.Errorf("the first round over a health answer of another version = %+v, want not ready, naming it", round)
	}
	stopServed(t, sb)
}

// TestAStoreThatDoesNotAnswerIsNotReady: a store read that does not answer
// within the round is not a ready answer.
func TestAStoreThatDoesNotAnswerIsNotReady(t *testing.T) {
	r := newRefusedInstall(t, "")
	round, sb := firstRoundOf(t, r, probationTiming{window: time.Hour, ask: 20 * time.Millisecond, retry: 20 * time.Millisecond,
		round: 300 * time.Millisecond}, serveOpts{probation: &probationSeams{storeRead: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}})
	if round.ready || !strings.Contains(round.why, "the store did not answer") {
		t.Errorf("the first round over a store that does not answer = %+v, want not ready, the store named", round)
	}
	stopServed(t, sb)
}

// TestAHealthCallThatNeverReturnsIsNotReadyAndDoesNotHoldTheStop: GET
// /v1/health run in-process takes no cancel, so a call still out at the
// round's bound is a no, no second round starts beside it, and serve's stop,
// a clean one, does not wait for it.
func TestAHealthCallThatNeverReturnsIsNotReadyAndDoesNotHoldTheStop(t *testing.T) {
	r := newRefusedInstall(t, "")
	released, entered := newOnceCloser(), newOnceCloser()
	var returned atomic.Bool
	t.Cleanup(released.close)
	events, seams := newProbationWatch(probationTiming{window: time.Hour, ask: 20 * time.Millisecond, retry: 20 * time.Millisecond,
		round: 200 * time.Millisecond})
	sb := r.bootWith(t, serveOpts{probation: seams, wrapAPIHandler: func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/v1/health" {
				entered.close()
				<-released.ch
				returned.Store(true)
				return
			}
			h.ServeHTTP(w, req)
		})
	}})
	first := events.waitFor(t, probationRound)
	if first.ready || !strings.Contains(first.why, "did not answer within") {
		t.Errorf("the first round over a health call that never returns = %+v, want not ready at its bound", first)
	}
	second := events.waitForCount(t, probationRound, 2)
	if second.ready || !strings.Contains(second.why, "has not answered yet") {
		t.Errorf("the second round = %+v, want none started beside the one still out", second)
	}
	select {
	case <-entered.ch:
	default:
		t.Fatal("the health call never reached the handler")
	}
	sb.stop()
	select {
	case code := <-sb.done:
		if code != 0 {
			t.Errorf("serve = %d after its stop, want 0; stderr=%s", code, sb.stderr.String())
		}
	case <-time.After(withinTheTest):
		t.Fatalf("serve's stop waited for the health call that never returns; stderr=%s", sb.stderr.String())
	}
	if returned.Load() {
		t.Error("the health call returned before the stop ended: the test shows nothing")
	}
	if st := markerOf(t, r.dataDir); st.FailedStarts != 0 {
		t.Errorf("the marker after a clean stop = %+v, want this start taken back", st)
	}
	released.close()
}

// TestAConfirmationReadsTheMarkerUnderItsLock: the confirmation at the
// window's end waits for the install marker's lock, then reads the marker
// again: an install of a newer version recorded while it waited stays as it
// is, and is not stamped confirmed; nor does the stop after it touch it.
func TestAConfirmationReadsTheMarkerUnderItsLock(t *testing.T) {
	r := newRefusedInstall(t, "")
	events, seams := newProbationWatch(probationTiming{window: time.Second, ask: 20 * time.Millisecond, retry: 20 * time.Millisecond,
		round: 5 * time.Second})
	sb := r.bootWith(t, serveOpts{probation: seams})
	events.waitFor(t, probationWindow)
	held, waits := holdTheMarkersLock(t, r.dataDir)
	select {
	case <-waits:
	case <-time.After(withinTheTest):
		t.Fatal("the confirmation never waited for the install marker's lock")
	}
	newer := updater.State{Status: "installing", TargetVersion: "9.9.9", PreviousVersion: version.ServerVersion,
		AttemptedAt: time.Now().UTC().Truncate(time.Second), SwapStarted: true}
	if err := updater.SaveState(r.dataDir, newer); err != nil {
		t.Fatal(err)
	}
	held.Release()
	events.waitFor(t, probationEnded)
	if got := markerOf(t, r.dataDir); got != newer {
		t.Errorf("the marker after the confirmation = %+v, want the newer install as recorded, %+v", got, newer)
	}
	if n := events.count(probationConfirmed); n != 0 {
		t.Errorf("the install was confirmed %d times over a newer install's marker", n)
	}
	stopServed(t, sb)
	if got := markerOf(t, r.dataDir); got != newer {
		t.Errorf("the marker after the stop = %+v, want the newer install as recorded", got)
	}
}

// holdTheMarkersLock holds dataDir's install marker's lock until the test
// lets it go or ends, and returns where a waiter's first wait is signalled.
func holdTheMarkersLock(t *testing.T, dataDir string) (*updater.StateLock, <-chan struct{}) {
	t.Helper()
	waits := make(chan struct{}, 1)
	t.Cleanup(updater.SetStateLockWaitingForTest(func(string) { signalOnce(waits) }))
	l, err := updater.LockState(t.Context(), dataDir)
	if err != nil || !l.Held() {
		t.Fatalf("hold the install marker's lock: %+v, %v", l, err)
	}
	t.Cleanup(l.Release)
	return l, waits
}

// TestAStopWhileAConfirmationWaitsWritesNothing: a stop that lands while the
// confirmation waits for the install marker's lock ends it, and serve, with
// nothing written: the marker byte for byte as it was, since the take-back,
// which asks for the lock for the rest of the stop's budget (stopBudget),
// finds it held throughout, and says so.
func TestAStopWhileAConfirmationWaitsWritesNothing(t *testing.T) {
	r := newRefusedInstall(t, "")
	events, seams := newProbationWatch(probationTiming{window: 200 * time.Millisecond, ask: 20 * time.Millisecond,
		retry: 20 * time.Millisecond, round: 5 * time.Second})
	sb := r.bootWith(t, serveOpts{probation: seams})
	events.waitFor(t, probationWindow)
	held, waits := holdTheMarkersLock(t, r.dataDir)
	select {
	case <-waits:
	case <-time.After(withinTheTest):
		t.Fatal("the confirmation never waited for the install marker's lock")
	}
	marker := watch(t, updater.StatePath(r.dataDir))
	sb.stop()
	select {
	case code := <-sb.done:
		if code != 0 {
			t.Errorf("serve = %d after its stop, want 0; stderr=%s", code, sb.stderr.String())
		}
	case <-time.After(withinTheTest):
		t.Fatalf("serve's stop waited for the install marker's lock; stderr=%s", sb.stderr.String())
	}
	marker.requireUnchanged(t)
	if !strings.Contains(sb.stderr.String(), "was not taken back") {
		t.Errorf("serve did not say its count was left; stderr=%s", sb.stderr.String())
	}
	held.Release()
}

// TestTheDoctorReadsOkBesideAHealthyStartOnProbation: beside a start on
// probation that has answered, whose own count is the marker's one, `bridge
// doctor`'s install-probation line is ok, saying the install is on
// probation.
func TestTheDoctorReadsOkBesideAHealthyStartOnProbation(t *testing.T) {
	r := newRefusedInstall(t, "")
	events, seams := newProbationWatch(longProbation)
	sb := r.bootWith(t, serveOpts{probation: seams})
	events.waitFor(t, probationWindow)
	if st := markerOf(t, r.dataDir); st.FailedStarts != 1 || st.Status != "installing" {
		t.Fatalf("the marker beside a healthy start = %+v, want this start counted, the install on probation", st)
	}
	if line := doctorLineOf(t, r.cfg); line == nil || line.Status != "ok" || !strings.Contains(line.Summary, "on probation") {
		t.Errorf("bridge doctor's install-probation line = %+v, want ok, on probation", line)
	}
	stopServed(t, sb)
}
