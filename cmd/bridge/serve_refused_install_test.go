package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// The early rollback (backlog B613): an updater that runs no settings check
// (0.2.1's, 0.2.2's) installs a version that refuses the bridge's settings at
// load, and its serve exits 2 before the boot rollback it carries is ever
// reached, so the service manager starts it again and again. These tests lay
// out what that updater leaves behind, an armed marker naming this version and
// the previous binary's backup beside the new one, and boot the real serve.

// previousVersion is the version the updater installed this one over, which
// its backup reports.
const previousVersion = "0.0.0-previous"

// refusedInstall is what an updater leaves behind: a binary standing in for
// the new one, the backup beside it (a copy of the test binary that answers
// as the bridge of previousVersion), an armed marker for this version in the
// data dir, and the settings.
type refusedInstall struct {
	dir, cfg, dataDir, bin, bak string
	marker                      updater.State
}

// newRefusedInstall lays the install out in a directory of its own, with
// settings that serve loads but for extra, which writeSettings appends on
// line 7 onwards.
func newRefusedInstall(t *testing.T, extra string) *refusedInstall {
	t.Helper()
	dir := t.TempDir()
	r := &refusedInstall{dir: dir, cfg: filepath.Join(dir, "bridge.yaml"), dataDir: filepath.Join(dir, "data")}
	r.writeSettings(t, "dataDir: "+r.dataDir+"\n", extra)
	if err := os.MkdirAll(r.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "bridge"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	r.bin = filepath.Join(dir, "bin", name)
	if err := os.MkdirAll(filepath.Dir(r.bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.bin, []byte("the new bridge binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.bak = installBridgeCopy(t, r.bin+".bak", previousVersion)
	r.marker = updater.State{
		Status: "installing", TargetVersion: version.ServerVersion, PreviousVersion: previousVersion,
		AttemptedAt: time.Now().UTC().Add(-time.Minute), SwapStarted: true,
	}
	r.saveMarker(t, r.dataDir)
	return r
}

// writeSettings writes the settings file: a library (lines 1 and 2), the
// data dir line (3; "" leaves the key out, and extra then starts on line 6),
// loopback listeners, and extra (line 7 on).
func (r *refusedInstall) writeSettings(t *testing.T, dataDirLine, extra string) {
	t.Helper()
	lib := filepath.Join(r.dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("libraryRoots:\n  - %s\n%slistenAddress: 127.0.0.1:0\nadminAddress: 127.0.0.1:0\ndisableHttp3: true\n%s",
		lib, dataDirLine, extra)
	if err := os.WriteFile(r.cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r *refusedInstall) saveMarker(t *testing.T, dataDir string) {
	t.Helper()
	if err := updater.SaveState(dataDir, r.marker); err != nil {
		t.Fatal(err)
	}
}

// testBackupProbeTimeout is how long these tests give the backup, a fresh
// copy of the test binary, to say its version (serveOpts.backupProbeTimeout),
// where production gives a release binary updater.VersionProbeTimeout. The
// copy answers in milliseconds where its pages are warm, and in about a
// second as the race-instrumented binary of make test, but its first start on
// CI's Windows leg took longer than production's 10 s on two PRs on
// 2026-11-03, failing serve's rollback (backlog B706). Only the number
// differs: the run, its kill at the bound and the refusal past it are
// production's.
const testBackupProbeTimeout = 2 * time.Minute

// serve boots the real serve on the install's settings and binary, giving
// the backup testBackupProbeTimeout to say its version unless opts gives
// another bound.
func (r *refusedInstall) serve(t *testing.T, opts serveOpts) (code int, stderr string) {
	t.Helper()
	if opts.configPath == "" {
		opts.configPath = r.cfg
	}
	opts.binaryPath = r.bin
	if opts.backupProbeTimeout == 0 {
		opts.backupProbeTimeout = testBackupProbeTimeout
	}
	var out, errs bytes.Buffer
	code = runServe(t.Context(), opts, &out, &errs)
	return code, errs.String()
}

// watchedFiles records each file a test watches, so it can require them
// unchanged.
type watchedFiles map[string][]byte

func watch(t *testing.T, paths ...string) watchedFiles {
	t.Helper()
	w := watchedFiles{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		w[p] = b
	}
	return w
}

func (w watchedFiles) requireUnchanged(t *testing.T) {
	t.Helper()
	for p, want := range w {
		got, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s changed (%v): it must be left as it was", filepath.Base(p), err)
		}
	}
}

// requireNoStoreOpened asserts nothing of serve's boot wrote the data dir:
// no store, no token file, no pid file, no certificate.
func requireNoStoreOpened(t *testing.T, dataDir string) {
	t.Helper()
	for _, name := range []string{filepath.Base(manifest.DefaultDBPath(dataDir)), tokensFileName, serverPIDFileName, "server.crt", "server.key"} {
		if _, err := os.Lstat(filepath.Join(dataDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s is in the data dir (lstat %v): serve went on past its settings", name, err)
		}
	}
}

// reportedVersion runs path as the bridge and returns the version it names.
func reportedVersion(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command(path, "version").Output()
	if err != nil {
		t.Fatalf("%s version: %v", filepath.Base(path), err)
	}
	return strings.TrimSpace(string(out))
}

func TestServeRollsBackAnInstallWhoseVersionRefusesItsSettings(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		says  []string
		// lay changes the install before serve boots, for the cases that
		// find the data dir another way.
		lay func(t *testing.T, r *refusedInstall)
	}{
		{name: "a leading zero", extra: "scanIntervalSec: 010\n", says: []string{"line 7:", "leading zero"}},
		{name: "a fraction", extra: "scanIntervalSec: 21600.5\n", says: []string{"line 7:", "!!float"}},
		{name: "a Tailscale mode it does not know", extra: "tailscale:\n  mode: tnset\n", says: []string{"tailscale.mode"}},
		// Refused past the load, by serveConfigRefusal, before the lock.
		{name: "two library roots with one folder name", says: []string{"libraryRoots", "one folder name"},
			lay: func(t *testing.T, r *refusedInstall) {
				a, b := filepath.Join(r.dir, "a", "Music"), filepath.Join(r.dir, "b", "Music")
				for _, d := range []string{a, b} {
					if err := os.MkdirAll(d, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				body := fmt.Sprintf("libraryRoots:\n  - %s\n  - %s\ndataDir: %s\nlistenAddress: 127.0.0.1:0\nadminAddress: 127.0.0.1:0\ndisableHttp3: true\n",
					a, b, r.dataDir)
				if err := os.WriteFile(r.cfg, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "the data dir named relative to the settings", extra: "scanIntervalSec: 010\n", says: []string{"leading zero"},
			lay: func(t *testing.T, r *refusedInstall) {
				r.writeSettings(t, "dataDir: data\n", "scanIntervalSec: 010\n")
			}},
		{name: "no dataDir key, the default", extra: "scanIntervalSec: 010\n", says: []string{"leading zero"},
			lay: func(t *testing.T, r *refusedInstall) {
				// The default, ./data, is relative to the settings file,
				// which is beside the data dir the install wrote.
				r.writeSettings(t, "", "scanIntervalSec: 010\n")
			}},
		{name: "BRIDGE_DATA_DIR over the file's", extra: "scanIntervalSec: 010\n", says: []string{"leading zero"},
			lay: func(t *testing.T, r *refusedInstall) {
				r.writeSettings(t, "dataDir: "+filepath.Join(r.dir, "not-this-one")+"\n", "scanIntervalSec: 010\n")
				t.Setenv("BRIDGE_DATA_DIR", r.dataDir)
			}},
		{name: "the settings reached through a link", extra: "scanIntervalSec: 010\n", says: []string{"leading zero"},
			lay: func(t *testing.T, r *refusedInstall) {
				link := filepath.Join(r.dir, "linked")
				if err := os.Symlink(r.dir, link); err != nil {
					t.Skipf("this host makes no symlink here: %v", err)
				}
				r.writeSettings(t, "dataDir: data\n", "scanIntervalSec: 010\n")
				r.cfg = filepath.Join(link, "bridge.yaml")
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, tc.extra)
			if tc.lay != nil {
				tc.lay(t, r)
			}
			settings := watch(t, r.cfg)
			code, stderr := r.serve(t, serveOpts{configPath: r.cfg})
			if code != supervision.RestartExitCode {
				t.Fatalf("serve = %d, want %d, the code every service manager starts the restored binary on; stderr=%s",
					code, supervision.RestartExitCode, stderr)
			}
			if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
				t.Errorf("the binary reports %q after the rollback, want %s", got, previousVersion)
			}
			if _, err := os.Lstat(r.bak); !os.IsNotExist(err) {
				t.Errorf("the backup is still there (lstat %v): it was not put back", err)
			}
			st, err := updater.LoadState(r.dataDir)
			if err != nil {
				t.Fatal(err)
			}
			if st.Status != "" || st.RejectedVersion != version.ServerVersion || st.RejectedConfigDigest == "" {
				t.Errorf("the marker does not hold this version back for these settings: %+v", st)
			}
			if st.MigrationVeto != version.ServerVersion {
				t.Errorf("the marker does not veto this version's store migrations: %+v", st)
			}
			// The restored binary's boot, whatever its version, keeps it: an
			// install marker is what 0.2.1's and 0.2.2's boot clears, and they
			// read Status "" as nothing to do; a release with round 3 reads the
			// record as the rollback finished, which keeps the hold and
			// restores nothing (BootRollbackFinished).
			if got := updater.DecideBootAction(st, previousVersion, time.Now().UTC()); got != updater.BootRollbackFinished {
				t.Errorf("the restored binary's boot over the marker = %v, want BootRollbackFinished", got)
			}
			for _, s := range tc.says {
				if !strings.Contains(st.RejectedReason, s) {
					t.Errorf("RejectedReason = %q, want it to say %q", st.RejectedReason, s)
				}
			}
			settings.requireUnchanged(t)
			requireNoStoreOpened(t, r.dataDir)
			if !strings.Contains(stderr, "config load failed") && !strings.Contains(stderr, "config: ") {
				t.Errorf("serve did not print the refusal first; stderr=%s", stderr)
			}
			if said := updaterLines(stderr); !strings.Contains(said, previousVersion) {
				t.Errorf("serve did not say which version it restored; stderr=%s", stderr)
			}
		})
	}
}

func TestServeLeavesAnInstallItCannotProveAsItIs(t *testing.T) {
	cases := []struct {
		name string
		lay  func(t *testing.T, r *refusedInstall)
		// says is what serve gives as its reason, when it gives one.
		says string
	}{
		{name: "no marker", lay: func(t *testing.T, r *refusedInstall) {
			if err := updater.ClearState(r.dataDir); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a marker for another version", lay: func(t *testing.T, r *refusedInstall) {
			r.marker.TargetVersion = "9.9.9"
			r.saveMarker(t, r.dataDir)
		}},
		{name: "a marker that never reached the swap", lay: func(t *testing.T, r *refusedInstall) {
			r.marker.SwapStarted = false
			r.saveMarker(t, r.dataDir)
		}, says: "swap"},
		{name: "an install already confirmed", lay: func(t *testing.T, r *refusedInstall) {
			r.marker.Status, r.marker.InstalledAt = "installed", time.Now().UTC()
			r.saveMarker(t, r.dataDir)
		}},
		{name: "a marker past the boot window", lay: func(t *testing.T, r *refusedInstall) {
			r.marker.AttemptedAt = time.Now().UTC().Add(-updater.RecencyWindow() - time.Hour)
			r.saveMarker(t, r.dataDir)
		}, says: "older"},
		{name: "a marker dated in the future", lay: func(t *testing.T, r *refusedInstall) {
			r.marker.AttemptedAt = time.Now().UTC().Add(time.Hour)
			r.saveMarker(t, r.dataDir)
		}, says: "future"},
		{name: "a marker that does not parse", lay: func(t *testing.T, r *refusedInstall) {
			if err := os.WriteFile(updater.StatePath(r.dataDir), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a marker naming no previous version", lay: func(t *testing.T, r *refusedInstall) {
			r.marker.PreviousVersion = ""
			r.saveMarker(t, r.dataDir)
		}, says: "no other version"},
		{name: "a marker this version already started under", lay: func(t *testing.T, r *refusedInstall) {
			r.marker.DataDirOpenedBy = version.ServerVersion
			r.saveMarker(t, r.dataDir)
		}, says: "opened the data dir"},
		{name: "no backup", lay: func(t *testing.T, r *refusedInstall) {
			if err := os.Remove(r.bak); err != nil {
				t.Fatal(err)
			}
		}, says: "backup"},
		{name: "a backup of another version", lay: func(t *testing.T, r *refusedInstall) {
			t.Setenv(runAsBridgeVersionEnv, "0.0.0-another")
		}, says: "0.0.0-another"},
		{name: "a backup that does not run", lay: func(t *testing.T, r *refusedInstall) {
			if err := os.Remove(r.bak); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(r.bak, []byte("not a program"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, says: "backup"},
		{name: "a backup that is a link", lay: func(t *testing.T, r *refusedInstall) {
			moved := r.bak + ".real"
			if err := os.Rename(r.bak, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, r.bak); err != nil {
				t.Skipf("this host makes no symlink here: %v", err)
			}
		}, says: "backup"},
		{name: "the data dir held by another process", lay: func(t *testing.T, r *refusedInstall) {
			holdDataDirLock(t, r.dataDir)
		}, says: "already running"},
		{name: "a data dir the settings name in no certain way", lay: func(t *testing.T, r *refusedInstall) {
			r.writeSettings(t, "dataDir: ["+r.dataDir+"]\n", "")
		}},
		{name: "settings that name another data dir since the install", lay: func(t *testing.T, r *refusedInstall) {
			r.writeSettings(t, "dataDir: "+filepath.Join(r.dir, "moved")+"\n", "scanIntervalSec: 010\n")
		}},
		{name: "a restore that fails", lay: func(t *testing.T, r *refusedInstall) {}, says: "could not put"},
		{name: "a rollback stopped before its rename, with no backup left", lay: func(t *testing.T, r *refusedInstall) {
			r.marker = rollbackStoppedBeforeItsRename(r.marker)
			r.saveMarker(t, r.dataDir)
			if err := os.Remove(r.bak); err != nil {
				t.Fatal(err)
			}
		}, says: "backup"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "scanIntervalSec: 010\n")
			tc.lay(t, r)
			watched := []string{r.cfg, r.bin}
			if _, err := os.Lstat(r.bak); err == nil {
				watched = append(watched, r.bak)
			}
			if _, err := os.Lstat(updater.StatePath(r.dataDir)); err == nil {
				watched = append(watched, updater.StatePath(r.dataDir))
			}
			files := watch(t, watched...)
			opts := serveOpts{}
			if tc.name == "a restore that fails" {
				opts.restoreBackup = func(string, string) error { return errors.New("the rename was refused") }
			}
			code, stderr := r.serve(t, opts)
			if code != 2 {
				t.Fatalf("serve = %d, want 2, the config refusal as today; stderr=%s", code, stderr)
			}
			files.requireUnchanged(t)
			// The reason is the updater's own line: the refusal before it
			// names the settings file, whose path can hold any word.
			said := updaterLines(stderr)
			switch {
			case tc.says == "" && said != "":
				t.Errorf("serve explained an install it had no reason to look at: %s", said)
			case tc.says != "" && !strings.Contains(said, tc.says):
				t.Errorf("serve did not say why it left the install (%q); its updater lines: %q; stderr=%s", tc.says, said, stderr)
			}
		})
	}
}

// updaterLines are serve's lines about the install, those it begins with
// "updater: ", joined.
func updaterLines(stderr string) string {
	var lines []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "updater: ") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// TestServeDisarmsTheEarlyRollbackBeforeItsFirstStoreAccess boots serve twice
// on one install. The first boot's settings load and its boot ends at the TLS
// step, after it took the data dir and before any store opened (an earlier
// boot that might have gone on to migrate the store and crash). The second
// boot's settings are refused: the early rollback must not restore the
// backup, since the first boot may have written what the backup's version
// does not know.
func TestServeDisarmsTheEarlyRollbackBeforeItsFirstStoreAccess(t *testing.T) {
	r := newRefusedInstall(t, "")
	// End the first boot at TLS: a certificate path that is a directory.
	r.writeSettings(t, "dataDir: "+r.dataDir+"\n", "tlsCertPath: "+r.dataDir+"\ntlsKeyPath: "+r.dataDir+"\n")
	code, stderr := r.serve(t, serveOpts{})
	if code != 1 || !strings.Contains(stderr, "TLS material") {
		t.Fatalf("the first boot = %d, want it to end at TLS (1); stderr=%s", code, stderr)
	}
	if _, err := os.Lstat(manifest.DefaultDBPath(r.dataDir)); !os.IsNotExist(err) {
		t.Fatalf("the first boot opened the store (lstat %v)", err)
	}
	st, err := updater.LoadState(r.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.DataDirOpenedBy != version.ServerVersion || st.Status != "installing" {
		t.Fatalf("the marker after the first boot = %+v, want it to record that %s opened the data dir", st, version.ServerVersion)
	}

	r.writeSettings(t, "dataDir: "+r.dataDir+"\n", "scanIntervalSec: 010\n")
	files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
	code, stderr = r.serve(t, serveOpts{})
	if code != 2 {
		t.Fatalf("the second boot = %d, want 2; stderr=%s", code, stderr)
	}
	files.requireUnchanged(t)
	if !strings.Contains(updaterLines(stderr), "opened the data dir") {
		t.Errorf("the second boot did not say why it kept the new binary; stderr=%s", stderr)
	}
}

func TestServeDoesNotStartWhereItCannotDisarmTheEarlyRollback(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.writeSettings(t, "dataDir: "+r.dataDir+"\n", "tlsCertPath: "+r.dataDir+"\ntlsKeyPath: "+r.dataDir+"\n")
	files := watch(t, updater.StatePath(r.dataDir))
	code, stderr := r.serve(t, serveOpts{saveUpdateState: func(string, updater.State) error {
		return errors.New("the disk is full")
	}})
	if code != 1 || strings.Contains(stderr, "TLS material") || !strings.Contains(updaterLines(stderr), "the disk is full") {
		t.Fatalf("serve = %d, want 1 before its TLS step, naming the failed record; stderr=%s", code, stderr)
	}
	files.requireUnchanged(t)
	requireNoStoreOpened(t, r.dataDir)
}

// The hold is recorded before the backup is put back (backlog B613's round
// 2, Codex's finding 5 and Argon's): the backup restored is 0.2.1 or 0.2.2,
// whose boot reads an install marker it cannot finish (its binary is not the
// version the marker names, and the backup is gone) as an install that
// failed, and clears it. A rollback that renamed first and then could not
// record the hold, or stopped between the two, left that binary free to
// install this version again, unchecked. So the hold goes first, in a form
// that binary's boot keeps (State.RollbackStarted, Status ""); a hold that
// cannot be recorded leaves the install as it is; and a start of this version
// that finds the hold recorded and the backup not back finishes the rollback.

// rollbackStoppedBeforeItsRename is st, an install marker, as the early
// rollback writes it just before it renames the backup back: the hold, and
// the veto of the install's version's store migrations (round 4).
func rollbackStoppedBeforeItsRename(st updater.State) updater.State {
	st.Status, st.RollbackStarted = "", true
	st.RejectedVersion = st.TargetVersion
	st.RejectedReason = st.TargetVersion + " refused this bridge's settings at its first start and was rolled back to " + st.PreviousVersion
	st.RejectedConfigDigest = "a digest of the settings"
	st.MigrationVeto = st.TargetVersion
	return st
}

func TestServeLeavesTheBackupWhereItCannotRecordTheHold(t *testing.T) {
	r := newRefusedInstall(t, "scanIntervalSec: 010\n")
	files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
	code, stderr := r.serve(t, serveOpts{saveUpdateState: func(string, updater.State) error {
		return errors.New("the disk is full")
	}})
	if code != 2 {
		t.Fatalf("serve = %d, want 2, the config refusal as today: a backup put back with no hold recorded installs this version again; stderr=%s",
			code, stderr)
	}
	files.requireUnchanged(t)
	if said := updaterLines(stderr); !strings.Contains(said, "not rolling back") || !strings.Contains(said, "the disk is full") {
		t.Errorf("serve did not say the hold could not be recorded, so the backup stays; its updater lines: %q", said)
	}
}

func TestTheHoldIsRecordedBeforeTheBackupIsPutBack(t *testing.T) {
	r := newRefusedInstall(t, "scanIntervalSec: 010\n")
	var atRename updater.State
	code, stderr := r.serve(t, serveOpts{restoreBackup: func(binaryPath, suffix string) error {
		st, err := updater.LoadState(r.dataDir)
		if err != nil {
			t.Error(err)
		}
		atRename = st
		return updater.RollbackBinary(binaryPath, suffix)
	}})
	if code != supervision.RestartExitCode {
		t.Fatalf("serve = %d, want %d; stderr=%s", code, supervision.RestartExitCode, stderr)
	}
	if atRename.RejectedVersion != version.ServerVersion || atRename.RejectedConfigDigest == "" || !atRename.RollbackStarted {
		t.Errorf("the marker as the backup was put back = %+v, want this version held back for these settings already", atRename)
	}
	// And this version's store migrations vetoed, before the rename too
	// (round 4): a command of this version, running from its image, must
	// find the veto however this start ends after the rename.
	if atRename.MigrationVeto != version.ServerVersion {
		t.Errorf("the marker as the backup was put back = %+v, want this version's store migrations vetoed already", atRename)
	}
	// Should the rename land and nothing after it, the restored binary's boot
	// must keep the hold: Status "" for 0.2.1's and 0.2.2's, and the rollback
	// finished for a release with round 3 (BootRollbackFinished).
	if atRename.Status != "" {
		t.Errorf("the marker as the backup was put back has Status %q, which 0.2.1's and 0.2.2's boot acts on", atRename.Status)
	}
	if got := updater.DecideBootAction(atRename, previousVersion, time.Now().UTC()); got != updater.BootRollbackFinished {
		t.Errorf("the restored binary's boot over the marker as the backup was put back = %v, want BootRollbackFinished", got)
	}
}

func TestAStartFinishesARollbackStoppedBeforeItsRename(t *testing.T) {
	r := newRefusedInstall(t, "scanIntervalSec: 010\n")
	r.marker = rollbackStoppedBeforeItsRename(r.marker)
	r.saveMarker(t, r.dataDir)
	code, stderr := r.serve(t, serveOpts{})
	if code != supervision.RestartExitCode {
		t.Fatalf("serve = %d, want %d: the hold is recorded and the backup not back, so this start puts it back; stderr=%s",
			code, supervision.RestartExitCode, stderr)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q, want %s", got, previousVersion)
	}
	st, err := updater.LoadState(r.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "" || st.RejectedVersion != version.ServerVersion || st.RejectedConfigDigest == "" ||
		!strings.Contains(st.RejectedReason, "leading zero") {
		t.Errorf("the marker after the rollback = %+v, want this version held back for these settings", st)
	}
}

// TestACommandThatMigratesTheStoreDisarmsTheEarlyRollback: a command other
// than serve can be the first of the new version to open the store, and its
// open migrates it (`bridge manifest clear-missing` here; doctor, a scan or a
// backup alike). Its settings load where serve's later may not, by an edit or
// another environment, and the early rollback must not then put back a
// backup that predates the migration (backlog B613's round 2, Codex's
// finding 1).
func TestACommandThatMigratesTheStoreDisarmsTheEarlyRollback(t *testing.T) {
	r := newRefusedInstall(t, "")
	// The store as the previous version left it, one migration behind: made
	// with no install armed, so the open that makes it records nothing.
	if err := updater.ClearState(r.dataDir); err != nil {
		t.Fatal(err)
	}
	storeOneMigrationBehind(t, r.dataDir)
	r.saveMarker(t, r.dataDir)

	var out, errOut bytes.Buffer
	if code := manifestCmd(t.Context(), []string{"clear-missing", "--yes", "--config", r.cfg}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("manifest clear-missing = %d; stderr=%s", code, errOut.String())
	}
	st, err := updater.LoadState(r.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.DataDirOpenedBy != version.ServerVersion {
		t.Fatalf("the marker after a command migrated the store = %+v, want it to record that %s opened the data dir", st, version.ServerVersion)
	}

	r.writeSettings(t, "dataDir: "+r.dataDir+"\n", "scanIntervalSec: 010\n")
	files := watch(t, r.bin, r.bak)
	code, stderr := r.serve(t, serveOpts{})
	if code != 2 {
		t.Fatalf("serve = %d, want 2: the backup predates the store's migration; stderr=%s", code, stderr)
	}
	files.requireUnchanged(t)
	if !strings.Contains(updaterLines(stderr), "opened the data dir") {
		t.Errorf("serve did not say why it kept the new binary; stderr=%s", stderr)
	}
}

// storeOneMigrationBehind writes the store in dataDir as the version before
// this one left it, one step of the ladder behind: every step but the last,
// and user_version there, so the last step's columns and index are not
// there yet (manifest.WriteDatabaseAtVersionForTest). Until round 4 it set
// user_version back on a store this build had made, which left them in
// place, and the open after it ran the step over a schema that had it
// already: no store a release left looks like that.
func storeOneMigrationBehind(t *testing.T, dataDir string) {
	t.Helper()
	if err := manifest.WriteDatabaseAtVersionForTest(manifest.DefaultDBPath(dataDir), latestStoreVersion(t)-1); err != nil {
		t.Fatal(err)
	}
}

// TestTheBootKeepsARecordedHoldWhereItClearsAnInstall: each boot step that
// clears an install marker (the backup's cleanup after a confirmed install,
// an install past the boot window, one that never reached its swap, one that
// did not come up) keeps a hold recorded beside it, which is about another
// version and the settings, not the install; dropping it costs one more
// download and check of a version that refuses them (backlog B613's round
// 2, Codex's finding 7). The step that puts the backup back leaves the veto
// of the replaced install's store migrations beside the hold (round 4). And
// each step that leaves the backup where it is keeps the record that a
// version other than the backup's opened the data dir, beside the backup's
// version, which an operator's rollback of that backup refuses on (backlog
// B705): until B705 the refused boot rollback's clear dropped it, and this
// test asserted that it did, as the rule then said.
func TestTheBootKeepsARecordedHoldWhereItClearsAnInstall(t *testing.T) {
	hold := updater.State{RejectedVersion: "9.9.9", RejectedReason: "9.9.9 refuses this bridge's settings: line 4: x",
		RejectedConfigDigest: "a digest of the settings"}
	now := time.Now().UTC()
	cases := []struct {
		name   string
		marker updater.State
		// bakGone says the boot step removes or restores the backup.
		bakGone bool
		// veto is the version whose store migrations the step leaves vetoed.
		veto string
		// opened is the version the step leaves recorded as having opened
		// the data dir, beside the backup's.
		opened string
	}{
		{name: "the backup's cleanup after a confirmed install", bakGone: true,
			marker: updater.State{Status: "installed", TargetVersion: version.ServerVersion, PreviousVersion: previousVersion,
				AttemptedAt: now.Add(-time.Hour), InstalledAt: now.Add(-time.Minute), SwapStarted: true}},
		{name: "the backup's cleanup after a confirmed install whose version opened the data dir", bakGone: true,
			marker: updater.State{Status: "installed", TargetVersion: version.ServerVersion, PreviousVersion: previousVersion,
				AttemptedAt: now.Add(-time.Hour), InstalledAt: now.Add(-time.Minute), SwapStarted: true,
				DataDirOpenedBy: version.ServerVersion}},
		{name: "an install past the boot window",
			marker: updater.State{Status: "installing", TargetVersion: "9.9.8", PreviousVersion: previousVersion,
				AttemptedAt: now.Add(-updater.RecencyWindow() - time.Hour), SwapStarted: true}},
		{name: "an install past the boot window, the data dir opened since", opened: "9.9.8",
			marker: updater.State{Status: "installing", TargetVersion: "9.9.8", PreviousVersion: previousVersion,
				AttemptedAt: now.Add(-updater.RecencyWindow() - time.Hour), SwapStarted: true, DataDirOpenedBy: "9.9.8"}},
		{name: "an install that never reached its swap",
			marker: updater.State{Status: "installing", TargetVersion: "9.9.8", PreviousVersion: previousVersion,
				AttemptedAt: now.Add(-time.Minute)}},
		{name: "an install that never reached its swap, the data dir opened since", opened: version.ServerVersion,
			marker: updater.State{Status: "installing", TargetVersion: "9.9.8", PreviousVersion: previousVersion,
				AttemptedAt: now.Add(-time.Minute), DataDirOpenedBy: version.ServerVersion}},
		{name: "an install that did not come up", bakGone: true, veto: "9.9.8",
			marker: updater.State{Status: "installing", TargetVersion: "9.9.8", PreviousVersion: previousVersion,
				AttemptedAt: now.Add(-time.Minute), SwapStarted: true}},
		{name: "an install that did not come up, the data dir opened since", opened: "9.9.8",
			marker: updater.State{Status: "installing", TargetVersion: "9.9.8", PreviousVersion: previousVersion,
				AttemptedAt: now.Add(-time.Minute), SwapStarted: true, DataDirOpenedBy: "9.9.8"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dataDir, bin := filepath.Join(dir, "data"), filepath.Join(dir, "bridge")
			for path, body := range map[string]string{bin: "the running binary", bin + ".bak": "the backup"} {
				if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			st := tc.marker
			st.RejectedVersion, st.RejectedReason, st.RejectedConfigDigest = hold.RejectedVersion, hold.RejectedReason, hold.RejectedConfigDigest
			if err := updater.SaveState(dataDir, st); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			maybeRollbackOnBoot(t.Context(), &stderr, dataDir, bin)
			if _, err := os.Lstat(bin + ".bak"); tc.bakGone != os.IsNotExist(err) {
				t.Fatalf("the backup after the boot step: lstat %v, want it gone %v; stderr=%s", err, tc.bakGone, stderr.String())
			}
			got, err := updater.LoadState(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			want := hold
			want.MigrationVeto = tc.veto
			if tc.opened != "" {
				want.DataDirOpenedBy, want.PreviousVersion = tc.opened, previousVersion
			}
			if got != want {
				t.Errorf("the marker after the boot step = %+v, want the hold and no more but its veto and the data dir's record, %+v; stderr=%s",
					got, want, stderr.String())
			}
			if refused := updater.RollbackRefusal(got) != ""; refused != (tc.opened != "") {
				t.Errorf("an operator's rollback over the marker the boot step leaves refuses: %v, want %v", refused, tc.opened != "")
			}
		})
	}
}

// TestTheBootRollbackLeavesABackupOnceAnotherVersionOpenedTheDataDir is the
// boot rollback's half of the same rule: a version that is not the backup's
// has written the data dir under this marker, so putting the backup back
// would be a downgrade past what it wrote. The install is cleared and its
// record of the data dir kept, so the console's Roll back of the backup the
// boot left refuses for the same reason (backlog B705).
func TestTheBootRollbackLeavesABackupOnceAnotherVersionOpenedTheDataDir(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.TargetVersion = "9.9.9"
	r.marker.DataDirOpenedBy = version.ServerVersion
	r.saveMarker(t, r.dataDir)
	files := watch(t, r.bin, r.bak)
	var stderr bytes.Buffer
	maybeRollbackOnBoot(t.Context(), &stderr, r.dataDir, r.bin)
	files.requireUnchanged(t)
	if st, err := updater.LoadState(r.dataDir); err != nil || st.Status != "" || st.DataDirOpenedBy != version.ServerVersion {
		t.Errorf("the marker = %+v (%v), want the install cleared and its record that %s opened the data dir kept", st, err, version.ServerVersion)
	}
	if !strings.Contains(updaterLines(stderr.String()), "opened the data dir") {
		t.Errorf("the boot rollback did not say why it left the backup; stderr=%s", stderr.String())
	}
	files = watchInstall(t, r)
	if err := rollBackNow(t, r); !errors.Is(err, updater.ErrRollbackRefused) {
		t.Fatalf("the console's rollback after the boot left the backup = %v, want it refused", err)
	}
	files.requireUnchanged(t)
}
