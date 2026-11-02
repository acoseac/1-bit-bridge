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
	asBridge(t)
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

// serve boots the real serve on the install's settings and binary.
func (r *refusedInstall) serve(t *testing.T, opts serveOpts) (code int, stderr string) {
	t.Helper()
	if opts.configPath == "" {
		opts.configPath = r.cfg
	}
	opts.binaryPath = r.bin
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
			if st.Status == "installing" || st.RejectedVersion != version.ServerVersion || st.RejectedConfigDigest == "" {
				t.Errorf("the marker does not hold this version back for these settings: %+v", st)
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

func TestServeRestoresTheBackupEvenWhereItCannotRecordTheRefusal(t *testing.T) {
	r := newRefusedInstall(t, "scanIntervalSec: 010\n")
	code, stderr := r.serve(t, serveOpts{saveUpdateState: func(string, updater.State) error {
		return errors.New("the disk is full")
	}})
	if code != supervision.RestartExitCode {
		t.Fatalf("serve = %d, want %d: the backup is back whether or not the hold is recorded; stderr=%s",
			code, supervision.RestartExitCode, stderr)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q, want %s", got, previousVersion)
	}
	if !strings.Contains(updaterLines(stderr), "the disk is full") {
		t.Errorf("serve did not say the hold could not be recorded; stderr=%s", stderr)
	}
}

// TestTheBootRollbackLeavesABackupOnceAnotherVersionOpenedTheDataDir is the
// boot rollback's half of the same rule: a version that is not the backup's
// has written the data dir under this marker, so putting the backup back
// would be a downgrade past what it wrote.
func TestTheBootRollbackLeavesABackupOnceAnotherVersionOpenedTheDataDir(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.TargetVersion = "9.9.9"
	r.marker.DataDirOpenedBy = version.ServerVersion
	r.saveMarker(t, r.dataDir)
	files := watch(t, r.bin, r.bak)
	var stderr bytes.Buffer
	maybeRollbackOnBoot(&stderr, r.dataDir, r.bin)
	files.requireUnchanged(t)
	if st, err := updater.LoadState(r.dataDir); err != nil || st.Status != "" {
		t.Errorf("the marker = %+v (%v), want it cleared", st, err)
	}
	if !strings.Contains(updaterLines(stderr.String()), "opened the data dir") {
		t.Errorf("the boot rollback did not say why it left the backup; stderr=%s", stderr.String())
	}
}
