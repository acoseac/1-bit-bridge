package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// The console's off of update checks through the real serve (backlog B761's
// round 3): the save holds the install marker's lock in the data dir while it
// decides and writes, so an install of another process, `bridge update`,
// past its reread of the switch, finishes its swap before the off lands; and
// while an install is on probation over a backup that refuses settings that
// write update.checkEnabled, the off is refused, since it would keep the
// crash rollback from putting the backup back.

// offAnswer is what the console answered a settings PATCH: its status, its
// error code and its words.
type offAnswer struct {
	status        int
	code, message string
	err           error
}

// patchSettingsAt sends body to the settings PATCH of the console at admin,
// on a client whose timeout outlasts every wait of the install marker's lock
// and a backup's checks. It calls nothing of t, so a goroutine can run it.
func patchSettingsAt(admin, body string) offAnswer {
	req, err := http.NewRequest(http.MethodPatch, admin+"/api/settings", strings.NewReader(body))
	if err != nil {
		return offAnswer{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 3 * testBackupProbeTimeout}).Do(req)
	if err != nil {
		return offAnswer{err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env struct{ Error, Message string }
	_ = json.Unmarshal(raw, &env)
	return offAnswer{status: resp.StatusCode, code: env.Error, message: env.Message}
}

// switchShown is updateCheckEnabled as the console at admin shows it.
func switchShown(t *testing.T, admin string) bool {
	t.Helper()
	var got struct {
		UpdateCheckEnabled bool `json:"updateCheckEnabled"`
	}
	getJSON(t, &http.Client{Timeout: 30 * time.Second}, admin+"/api/settings", "", &got)
	return got.UpdateCheckEnabled
}

// requireOffNotSaved requires a refused off's settings file to be as it was
// and the running switch still on.
func requireOffNotSaved(t *testing.T, cfgPath string, before []byte, admin string) {
	t.Helper()
	if got := readBytes(t, cfgPath); string(got) != string(before) {
		t.Errorf("the refused off changed the settings:\n%s", got)
	}
	if !switchShown(t, admin) {
		t.Error("the running switch is off: the refused off was published")
	}
}

// offBridge boots serve with update checks on, nothing in the environment
// to say otherwise, and a releases API of the test's own.
func offBridge(t *testing.T) (b *consoleBridge, cfgPath string) {
	t.Helper()
	persistentMedium(t)
	t.Setenv(updateCheckVar, "")
	os.Unsetenv(updateCheckVar)
	gh := newReleasesStandIn(t)
	b = startConsoleBridge(t, "", nil, func(o *serveOpts) { o.updateReleases = gh.client(t) })
	return b, filepath.Join(filepath.Dir(b.lib), "bridge.yaml")
}

// TestTheConsolesOffWaitsForTheInstallMarkersLockAndLandsAfter: another
// process holds the install marker's lock in serve's data dir, as `bridge
// update` does from just before its decision until after its swap. The
// console's off waits for it, saving nothing while it is held (the file says
// nothing of the switch and the running switch is on), and lands once it is
// let go: the file writes the key false and the switch is off.
func TestTheConsolesOffWaitsForTheInstallMarkersLockAndLandsAfter(t *testing.T) {
	b, cfgPath := offBridge(t)
	install, err := updater.LockState(t.Context(), b.dataDir)
	if err != nil || !install.Held() {
		t.Fatalf("hold the install marker's lock as an install does: %+v, %v", install, err)
	}
	defer install.Release()
	waiting := make(chan struct{})
	var once sync.Once
	t.Cleanup(updater.SetStateLockWaitingForTest(func(string) { once.Do(func() { close(waiting) }) }))
	answered := make(chan offAnswer, 1)
	go func() { answered <- patchSettingsAt(b.adminBase, `{"updateCheckEnabled":false}`) }()
	select {
	case <-waiting:
	case a := <-answered:
		t.Fatalf("the off answered %d %s %q (%v) while an install held the install marker's lock: it did not wait",
			a.status, a.code, a.message, a.err)
	case <-time.After(time.Minute):
		t.Fatal("the off never waited for the install marker's lock")
	}
	if v, there := fileUpdateCheck(t, cfgPath); there {
		t.Errorf("the off wrote update.checkEnabled = %v while the install held the lock", v)
	}
	if !switchShown(t, b.adminBase) {
		t.Error("the off was published while the install held the lock")
	}
	install.Release()
	select {
	case a := <-answered:
		if a.err != nil || a.status != http.StatusOK {
			t.Fatalf("the off once the lock was let go = %d %s %q (%v), want it saved", a.status, a.code, a.message, a.err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the off never landed once the lock was let go")
	}
	if v, there := fileUpdateCheck(t, cfgPath); !there || v != false {
		t.Errorf("the file holds update.checkEnabled = %v (there %v), want false", v, there)
	}
	if switchShown(t, b.adminBase) || readUpdatesPanel(t, b).CheckEnabled {
		t.Error("the switch is on after the off landed")
	}
}

// TestTheConsolesOffWhereTheInstallMarkersLockIsNotKept: on a filesystem that
// keeps no locks the off is saved, since no install commits there; a lock
// that fails, and one another process holds past the wait, refuse it, 409
// marker-locked, in words that say nothing was saved, the file and the running
// switch as they were.
func TestTheConsolesOffWhereTheInstallMarkersLockIsNotKept(t *testing.T) {
	t.Run("a filesystem that keeps no locks", func(t *testing.T) {
		b, cfgPath := offBridge(t)
		t.Cleanup(updater.SetStateTryLockForTest(func(*os.File) error { return syscall.EOPNOTSUPP }))
		if a := patchSettingsAt(b.adminBase, `{"updateCheckEnabled":false}`); a.err != nil || a.status != http.StatusOK {
			t.Fatalf("the off where no lock is kept = %d %s %q (%v), want it saved", a.status, a.code, a.message, a.err)
		}
		if v, there := fileUpdateCheck(t, cfgPath); !there || v != false {
			t.Errorf("the file holds update.checkEnabled = %v (there %v), want false", v, there)
		}
	})
	t.Run("a lock that fails", func(t *testing.T) {
		b, cfgPath := offBridge(t)
		before := readBytes(t, cfgPath)
		t.Cleanup(updater.SetStateTryLockForTest(func(*os.File) error { return syscall.EIO }))
		a := patchSettingsAt(b.adminBase, `{"updateCheckEnabled":false}`)
		if a.err != nil || a.status != http.StatusConflict || a.code != "marker-locked" ||
			!strings.Contains(a.message, "update checks were not turned off") || !strings.Contains(a.message, "nothing was saved") {
			t.Fatalf("the off whose lock fails = %d %s %q (%v), want 409 marker-locked saying nothing was saved", a.status, a.code, a.message, a.err)
		}
		requireOffNotSaved(t, cfgPath, before, b.adminBase)
	})
	t.Run("another process holds the lock past the wait", func(t *testing.T) {
		b, cfgPath := offBridge(t)
		before := readBytes(t, cfgPath)
		t.Cleanup(updater.SetStateLockWaitForTest(300 * time.Millisecond))
		install, err := updater.LockState(t.Context(), b.dataDir)
		if err != nil || !install.Held() {
			t.Fatalf("hold the install marker's lock: %+v, %v", install, err)
		}
		defer install.Release()
		a := patchSettingsAt(b.adminBase, `{"updateCheckEnabled":false}`)
		if a.err != nil || a.status != http.StatusConflict || a.code != "marker-locked" ||
			!strings.Contains(a.message, "held the install marker's lock") || !strings.Contains(a.message, "nothing was saved") {
			t.Fatalf("the off behind a holder past the wait = %d %s %q (%v), want 409 marker-locked", a.status, a.code, a.message, a.err)
		}
		requireOffNotSaved(t, cfgPath, before, b.adminBase)
	})
}

// TestTheConsolesOffIsRefusedWhileACrashRollbackWouldPutBackABackupThatRefusesTheKey:
// serve runs an install of its version over previousVersion, on probation,
// its store at the ladder's top so that no migration rules the crash rollback
// out, and the backup beside the binary is a release from before the switch,
// whose strict decoder refuses settings that write update.checkEnabled. The
// console's off is refused, 409 update-checks-off-refused, in words that say
// when it can be saved and how to turn the checks off without the key, the
// settings as they were and the switch on. Once the install is confirmed,
// and over a backup that has the switch, the off is saved.
func TestTheConsolesOffIsRefusedWhileACrashRollbackWouldPutBackABackupThatRefusesTheKey(t *testing.T) {
	t.Setenv(updateCheckVar, "")
	os.Unsetenv(updateCheckVar)
	t.Run("on probation over a backup from before the switch", func(t *testing.T) {
		t.Setenv(runAsBridgeCheckEnv, "predatesUpdateSwitch")
		r := newRefusedInstall(t, "")
		r.withACurrentStore(t)
		_, seams := newProbationWatch(longProbation)
		sb, admin := r.bootConsole(t, serveOpts{probation: seams})
		before := readBytes(t, r.cfg)
		a := patchSettingsAt(admin, `{"updateCheckEnabled":false}`)
		if a.err != nil || a.status != http.StatusConflict || a.code != "update-checks-off-refused" {
			t.Fatalf("the off during the probation = %d %s %q (%v), want 409 update-checks-off-refused", a.status, a.code, a.message, a.err)
		}
		for _, says := range []string{version.ServerVersion + " is on probation", "its backup, " + previousVersion,
			"once " + version.ServerVersion + " is confirmed", "BRIDGE_UPDATE_CHECK_ENABLED=false", "restart the service",
			previousVersion + " does not read that variable", "nothing was saved"} {
			if !strings.Contains(a.message, says) {
				t.Errorf("the refusal does not say %q: %q", says, a.message)
			}
		}
		if strings.Contains(a.message, r.dir) {
			t.Errorf("the refusal names a path: %q", a.message)
		}
		requireOffNotSaved(t, r.cfg, before, admin)
		requireBackupThere(t, r)
		stopServed(t, sb)
	})
	t.Run("the install confirmed", func(t *testing.T) {
		t.Setenv(runAsBridgeCheckEnv, "predatesUpdateSwitch")
		r := newRefusedInstall(t, "")
		r.withACurrentStore(t)
		events, seams := newProbationWatch(shortProbation)
		sb, admin := r.bootConsole(t, serveOpts{probation: seams})
		events.waitFor(t, probationConfirmed)
		if a := patchSettingsAt(admin, `{"updateCheckEnabled":false}`); a.err != nil || a.status != http.StatusOK {
			t.Fatalf("the off once the install is confirmed = %d %s %q (%v), want it saved", a.status, a.code, a.message, a.err)
		}
		if v, there := fileUpdateCheck(t, r.cfg); !there || v != false {
			t.Errorf("the file holds update.checkEnabled = %v (there %v), want false", v, there)
		}
		stopServed(t, sb)
	})
	t.Run("on probation over a backup that has the switch", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		r.withACurrentStore(t)
		_, seams := newProbationWatch(longProbation)
		sb, admin := r.bootConsole(t, serveOpts{probation: seams})
		if a := patchSettingsAt(admin, `{"updateCheckEnabled":false}`); a.err != nil || a.status != http.StatusOK {
			t.Fatalf("the off over a backup that has the switch = %d %s %q (%v), want it saved", a.status, a.code, a.message, a.err)
		}
		if v, there := fileUpdateCheck(t, r.cfg); !there || v != false {
			t.Errorf("the file holds update.checkEnabled = %v (there %v), want false", v, there)
		}
		stopServed(t, sb)
	})
}

// requirePOSIXReadRefusal skips a case that makes a file this user may not
// read: mode 000 refuses nobody on Windows, nor root.
func requirePOSIXReadRefusal(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a file this user may not read needs POSIX modes and a user that is not root")
	}
}

// unreadableMarker writes an install marker of an install of this version
// over previousVersion into dataDir and makes it unreadable to this user,
// for the rest of the test.
func unreadableMarker(t *testing.T, dataDir string) {
	t.Helper()
	if err := updater.SaveState(dataDir, updater.State{Status: "installing", TargetVersion: version.ServerVersion,
		PreviousVersion: previousVersion, SwapStarted: true, AttemptedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	marker := updater.StatePath(dataDir)
	if err := os.Chmod(marker, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(marker, 0o600) })
}

// TestTheConsolesOffOverAnInstallMarkerThatCannotBeReadIsRefused: serve runs
// with update checks on, and the install marker in its data dir is one its
// user may not read; the console's off is refused, failing closed (backlog
// B761's round 4), 409 update-checks-off-unchecked in the marker's own words:
// the install marker could not be read, and the one way to turn the checks off
// without the key, with no version (none was read, so none is empty either),
// no backup and no path; the settings and the running switch stay as they
// were.
func TestTheConsolesOffOverAnInstallMarkerThatCannotBeReadIsRefused(t *testing.T) {
	requirePOSIXReadRefusal(t)
	b, cfgPath := offBridge(t)
	before := readBytes(t, cfgPath)
	unreadableMarker(t, b.dataDir)
	a := patchSettingsAt(b.adminBase, `{"updateCheckEnabled":false}`)
	if a.err != nil || a.status != http.StatusConflict || a.code != "update-checks-off-unchecked" {
		t.Fatalf("the off over a marker that cannot be read = %d %s %q (%v), want 409 update-checks-off-unchecked",
			a.status, a.code, a.message, a.err)
	}
	for _, says := range []string{"the install marker could not be read", "nothing was saved",
		"BRIDGE_UPDATE_CHECK_ENABLED=false", "restart the service"} {
		if !strings.Contains(a.message, says) {
			t.Errorf("the refusal does not say %q: %q", says, a.message)
		}
	}
	for _, never := range []string{"probation", previousVersion, "backup,", "  ", " ,", b.dataDir} {
		if strings.Contains(a.message, never) {
			t.Errorf("the refusal says %q, of a marker it could not read: %q", never, a.message)
		}
	}
	requireOffNotSaved(t, cfgPath, before, b.adminBase)
}

// TestTheOffsAskSaysWhatRefusedItInTheLog: serve's ask for an off
// (updateChecksOffSave) logs its refusal at Warn by its kind: over an install
// marker it could not read, that the marker could not be read, with the read's
// error and no path (an install marker's read error names none in a log line);
// it answers the console's sentinel for it and lets the install marker's lock
// go.
func TestTheOffsAskSaysWhatRefusedItInTheLog(t *testing.T) {
	requirePOSIXReadRefusal(t)
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unreadableMarker(t, dataDir)
	logs := loggingtest.Record(t)
	release, err := updateChecksOffSave(dataDir, filepath.Join(dir, "bridge"), filepath.Join(dir, "bridge.yaml"), 0)(t.Context())
	if release != nil || !errors.Is(err, admin.ErrUpdateChecksOffUnchecked) {
		t.Fatalf("the ask over a marker that cannot be read = %v (a release %v), want admin.ErrUpdateChecksOffUnchecked",
			err, release != nil)
	}
	lines := logs.Lines("an off of update checks was refused: the install marker could not be read")
	if len(lines) != 1 || !strings.Contains(lines[0], syscall.EACCES.Error()) {
		t.Errorf("the log's line for the refusal = %q, want one naming the read's error:\n%s", lines, strings.Join(logs.All(), "\n"))
	}
	if leaked := logs.Carrying(dataDir); len(leaked) != 0 {
		t.Errorf("the log names the data dir: %q", leaked)
	}
	lock, err := updater.LockState(t.Context(), dataDir)
	if err != nil || !lock.Held() {
		t.Errorf("the install marker's lock after the refusal = %v (held %v), want it let go", err, lock.Held())
	}
	lock.Release()
}

// TestACrashRollbackWithTheVariableOffPutsAReleaseBeforeTheSwitchBack: update
// checks turned off the way the refusal says, by BRIDGE_UPDATE_CHECK_ENABLED
// in the service's environment with no key in the settings, leave the crash
// rollback to a release from before the switch whole: three starts that
// crashed, and the next puts the backup back and exits 75. The backup's
// stand-in ignores the variable as such a release does (its loader binds only
// the variables of the fields it has; the record of round 3 in
// ops/engineering-log.md has 0.2.3's own binary run with it set).
func TestACrashRollbackWithTheVariableOffPutsAReleaseBeforeTheSwitchBack(t *testing.T) {
	t.Setenv(updateCheckVar, "false")
	t.Setenv(runAsBridgeCheckEnv, "predatesUpdateSwitch")
	r := newRefusedInstall(t, "")
	for start := 1; start <= updater.ProbationFailedStarts; start++ {
		r.startThatPanics(t)
	}
	code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin})
	if served || code != supervision.RestartExitCode {
		t.Fatalf("the start after the crashes, the variable off and no key = %d (served %v), want %d before it served; stderr=%s",
			code, served, supervision.RestartExitCode, stderr)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q after the rollback, want %s", got, previousVersion)
	}
	if v, there := fileUpdateCheck(t, r.cfg); there {
		t.Errorf("the settings hold update.checkEnabled = %v after the rollback, want no key", v)
	}
}
