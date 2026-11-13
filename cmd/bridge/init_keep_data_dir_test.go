package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

// Backlog B664: `bridge init` run again over an install's config, which it
// keeps (--yes without --force, or a no at "Overwrite?"), installed and
// started the service on it without a look at its data directory. The unit
// systemd and launchd run has the data directory for its working directory,
// so a data directory that was not there (moved, never made, its dataDir
// edited) failed the unit at its chdir before serve could make it, and init
// had said it was done; one serve refuses (an entry in its way, B606; its
// record's refusal, B651 and B741) was installed all the same, and so was a
// config serve does not load. A run that keeps the config now loads it as
// serve does, asks the check serve makes of the record beside it, and
// prepares the data directory as serve prepares it, before its preflight and
// any service work, refusing, with nothing made or installed, what serve
// would refuse.

// installCall is a unit init asked its host to install, and whether its
// working directory was a directory then: systemd and launchd change into it
// as they start the unit, which `systemctl --user enable --now` and `launchctl
// bootstrap` do as init installs it.
type installCall struct {
	params          packaging.Params
	workingDirIsDir bool
}

// initHostCalls is what a stand-in for init's host was asked.
type initHostCalls struct {
	installs []installCall
	spawns   int
	opened   int
}

// standInForInitHost puts a stand-in in initHost's place for the rest of the
// test, which installs, starts and opens nothing and records what it is
// asked, and a home of the test's own in the environment, where init makes
// its log's directory (packaging.DefaultLogPath). init runs on the test's
// goroutine, so the calls need no lock.
func standInForInitHost(t *testing.T) *initHostCalls {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	calls := &initHostCalls{}
	install := func(p packaging.Params) (string, error) {
		info, err := os.Stat(p.WorkingDir)
		calls.installs = append(calls.installs, installCall{params: p, workingDirIsDir: err == nil && info.IsDir()})
		return filepath.Join(home, "the test's unit"), nil
	}
	prev := initHost
	initHost = initHostOps{
		install:        install,
		installStartup: install,
		installSCM:     install,
		spawnDetached:  func(string, string, string) error { calls.spawns++; return nil },
		openBrowser:    func(string) { calls.opened++ },
	}
	t.Cleanup(func() { initHost = prev })
	return calls
}

// requireInstalledOn fails the test unless init installed one unit, whose
// working directory is dataDir, the data directory serve uses, and was a
// directory as the unit was installed.
func requireInstalledOn(t *testing.T, calls *initHostCalls, dataDir string) {
	t.Helper()
	switch {
	case len(calls.installs) != 1:
		t.Fatalf("init installed %d units, want 1", len(calls.installs))
	case calls.installs[0].params.WorkingDir != dataDir:
		t.Errorf("init installed the unit with the working directory %s, want the data directory serve uses, %s",
			calls.installs[0].params.WorkingDir, dataDir)
	case !calls.installs[0].workingDirIsDir:
		t.Errorf("init installed the unit with the working directory %s, which was not a directory: systemd and "+
			"launchd fail the unit at its chdir, before serve can make it", dataDir)
	}
}

// requireNoServiceWork fails the test unless init installed, started and
// opened nothing.
func requireNoServiceWork(t *testing.T, calls *initHostCalls) {
	t.Helper()
	if len(calls.installs) != 0 || calls.spawns != 0 || calls.opened != 0 {
		t.Errorf("init installed %d units, started %d bridges and opened %d pages, want none",
			len(calls.installs), calls.spawns, calls.opened)
	}
}

// The suite's own stand-in for init's host (TestMain's): it refuses each
// ask, and records it for TestMain to fail the suite on, since no test may
// install a unit with the host's service manager, start a bridge or open a
// browser.
var (
	errSuiteAsksNoInitHost = errors.New("the test suite installs no service, starts no bridge and opens no page: " +
		"a test that runs init's launch path stands in for its host (standInForInitHost)")
	initHostAsksMu sync.Mutex
	initHostAsks   []string
)

// refusingInitHost is that stand-in.
func refusingInitHost() initHostOps {
	ask := func(what string) {
		initHostAsksMu.Lock()
		defer initHostAsksMu.Unlock()
		initHostAsks = append(initHostAsks, what)
	}
	install := func(what string) func(packaging.Params) (string, error) {
		return func(p packaging.Params) (string, error) {
			ask(what + " for " + p.ConfigPath)
			return "", errSuiteAsksNoInitHost
		}
	}
	return initHostOps{
		install:        install("install a service"),
		installStartup: install("install the Startup launcher"),
		installSCM:     install("install the SCM service"),
		spawnDetached: func(_, cfgPath, _ string) error {
			ask("start a bridge for " + cfgPath)
			return errSuiteAsksNoInitHost
		},
		openBrowser: func(url string) { ask("open " + url) },
	}
}

// initHostAsked is what the suite's stand-in was asked.
func initHostAsked() []string {
	initHostAsksMu.Lock()
	defer initHostAsksMu.Unlock()
	return slices.Clone(initHostAsks)
}

// entriesUnder is each entry under dir, by its path below dir in slash form,
// with what it is: a directory, a file's bytes, or where a link leads, read
// without following it; nil where dir is not there.
func entriesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		entry := filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			entry += " -> " + target
		case d.IsDir():
			entry += "/"
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			entry += " " + strconv.Quote(string(b))
		}
		out = append(out, entry)
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// directoriesToMake is each directory on dir's way, dir included, that is not
// there, from the nearest one that is, as entriesUnder spells it below base,
// sorted: what MkdirAll makes of dir.
func directoriesToMake(t *testing.T, base, dir string) []string {
	t.Helper()
	var out []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(base, d)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			t.Fatalf("%s is not below %s", d, base)
		}
		out = append(out, filepath.ToSlash(rel)+"/")
	}
	slices.Sort(out)
	return out
}

// newEntries is what after holds that before does not.
func newEntries(before, after []string) []string {
	var out []string
	for _, e := range after {
		if !slices.Contains(before, e) {
			out = append(out, e)
		}
	}
	return out
}

// keptInstall is the install a run of bridge init keeps: bridge.yaml in
// cfgDir, below base, naming the library lib, which serve starts on (both
// ports the kernel's, no HTTP/3, no mDNS), with no bridge running.
type keptInstall struct {
	base, cfgDir, cfg, lib string
}

// newKeptInstall makes its config dir and its library, and no config.
func newKeptInstall(t *testing.T) keptInstall {
	t.Helper()
	base := t.TempDir()
	k := keptInstall{base: base, cfgDir: filepath.Join(base, "cfg"), lib: filepath.Join(base, "Music")}
	k.cfg = filepath.Join(k.cfgDir, "bridge.yaml")
	mkdirs(t, k.cfgDir, k.lib)
	return k
}

// writeConfig writes its config, dataDirLine its dataDir line ("" for none,
// the default) and extra any line after the rest.
func (k keptInstall) writeConfig(t *testing.T, dataDirLine, extra string) {
	t.Helper()
	body := "libraryRoots:\n  - " + k.lib + "\n" + dataDirLine +
		"listenAddress: 127.0.0.1:0\nadminAddress: 127.0.0.1:0\ndisableHttp3: true\nmdns:\n    enabled: false\n" + extra
	if err := os.WriteFile(k.cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// keep runs bridge init over it: flags after --dir and --library, and stdin
// for its prompts.
func (k keptInstall) keep(t *testing.T, stdin string, flags ...string) (int, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := initCmd(append([]string{"--dir", k.cfgDir, "--library", k.lib}, flags...), strings.NewReader(stdin), &out, &errOut)
	return code, stripANSI("--- stdout ---\n" + out.String() + "\n--- stderr ---\n" + errOut.String())
}

// keepRuns are the three ways a run keeps the config: --yes without --force,
// a no at "Overwrite?", and --yes with --skip-doctor, which skips the
// preflight and never the data directory's check.
var keepRuns = []struct {
	name  string
	stdin string
	flags []string
}{
	{"kept by --yes", "", []string{"--yes"}},
	{"kept by a no at Overwrite?", "n\n", nil},
	{"kept by --yes with --skip-doctor", "", []string{"--yes", "--skip-doctor"}},
}

// keptConfigWords closes each refusal of a run that keeps the config.
const keptConfigWords = "the config was NOT changed, and no service was installed or started."

// TestAKeptConfigsDataDirIsMadeBeforeItsServiceIsInstalled (case 1): a run
// that keeps the config, whose data directory is not there and is no data
// directory its record names, makes it as serve would before it installs the
// service, whose unit then finds its working directory, and a serve starts on
// it after; nothing but the directories on the data directory's way is made,
// and the config and the record are as they were. The data directory is
// serve's: the config loaded as serve loads it, a relative or default dataDir
// and BRIDGE_DATA_DIR included, and one set on purpose to a new directory
// while the record names the old. On main the run installed the unit on a
// working directory that was not there, the file's dataDir where
// BRIDGE_DATA_DIR names another, and answered 0.
func TestAKeptConfigsDataDirIsMadeBeforeItsServiceIsInstalled(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	disk := func(k keptInstall) string { return filepath.Join(k.base, "disk", "bridge") }
	for _, tc := range []struct {
		name string
		// dataDirLine is the config's dataDir line, "" for none, and dataDir
		// the data directory serve uses, of the install k.
		dataDirLine, dataDir func(k keptInstall) string
		// set sets up what the row needs beyond the config.
		set   func(t *testing.T, k keptInstall)
		stdin string
		flags []string
	}{
		{name: "an absolute dataDir, kept by --yes", flags: []string{"--yes"}},
		{name: "an absolute dataDir, kept by a no at Overwrite?", stdin: "n\n"},
		{name: "an absolute dataDir, kept by --yes with --skip-doctor", flags: []string{"--yes", "--skip-doctor"}},
		{name: "a relative dataDir", flags: []string{"--yes", "--skip-doctor"},
			dataDirLine: func(keptInstall) string { return "dataDir: state/bridge\n" },
			dataDir:     func(k keptInstall) string { return filepath.Join(k.cfgDir, "state", "bridge") }},
		{name: "no dataDir", flags: []string{"--yes", "--skip-doctor"},
			dataDirLine: func(keptInstall) string { return "" },
			dataDir:     func(k keptInstall) string { return filepath.Join(k.cfgDir, "data") }},
		{name: "BRIDGE_DATA_DIR over the file's dataDir", flags: []string{"--yes", "--skip-doctor"},
			dataDir: func(k keptInstall) string { return filepath.Join(k.base, "env", "bridge") },
			set: func(t *testing.T, k keptInstall) {
				t.Setenv("BRIDGE_DATA_DIR", filepath.Join(k.base, "env", "bridge"))
			}},
		{name: "a dataDir set to a new directory on purpose", flags: []string{"--yes", "--skip-doctor"},
			set: func(t *testing.T, k keptInstall) {
				old := filepath.Join(k.base, "old", "bridge")
				mkdirs(t, old)
				touch(t, filepath.Join(old, "bridge.db"), filepath.Join(old, serveLockFileName))
				writeIdentity(t, old, id)
				writeRecordWithIdentity(t, k.cfg, old, id)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := standInForInitHost(t)
			k := newKeptInstall(t)
			line, dataDir := "dataDir: "+disk(k)+"\n", disk(k)
			if tc.dataDirLine != nil {
				line = tc.dataDirLine(k)
			}
			if tc.dataDir != nil {
				dataDir = tc.dataDir(k)
			}
			k.writeConfig(t, line, "")
			if tc.set != nil {
				tc.set(t, k)
			}
			before, toMake := entriesUnder(t, k.base), directoriesToMake(t, k.base, dataDir)
			cfgBefore := mustRead(t, k.cfg)
			recordBefore, recordErr := os.ReadFile(dataDirRecordFile(k.cfg))

			code, out := k.keep(t, tc.stdin, tc.flags...)
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("a run keeping the config exited %d", code)
			}
			requireInstalledOn(t, calls, dataDir)
			if want := "made the data directory " + dataDir; !strings.Contains(out, want) {
				t.Errorf("the run does not say it made the data directory: no %q", want)
			}
			if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
				t.Fatalf("the data directory %s is not a directory after the run (stat: %v)", dataDir, err)
			} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
				t.Errorf("the data directory was made %v, want 0700, as serve makes it", info.Mode().Perm())
			}
			if made := newEntries(before, entriesUnder(t, k.base)); !slices.Equal(made, toMake) {
				t.Errorf("the run made %q, want the data directory and the directories on its way that were not there, %q", made, toMake)
			}
			if got := mustRead(t, k.cfg); got != cfgBefore {
				t.Errorf("the run changed the config it keeps:\n%s", got)
			}
			switch after, err := os.ReadFile(dataDirRecordFile(k.cfg)); {
			case errors.Is(recordErr, fs.ErrNotExist) && !errors.Is(err, fs.ErrNotExist):
				t.Errorf("the run wrote a record beside the config (read: %v): only serve writes one", err)
			case recordErr == nil && string(after) != string(recordBefore):
				t.Errorf("the run changed the record beside the config: %q", after)
			}

			// The unit's serve: a start on the data directory the run made.
			stopServed(t, bootServe(t, "--config", k.cfg))
		})
	}
}

// TestAKeptConfigWhoseDataDirServeRefusesInstallsNoService (case 2): a run
// that keeps a config whose data directory serve's preparation refuses (a
// regular file, a link to one or a link to nothing, at it or above it: serve's
// own table, unusableDataDirs) exits 1, saying what serve says of it, and
// installs, starts and makes nothing, kept each way. On main the run installed
// the service and answered 0.
func TestAKeptConfigWhoseDataDirServeRefusesInstallsNoService(t *testing.T) {
	for _, shape := range unusableDataDirs {
		for _, run := range keepRuns {
			t.Run(shape.name+", "+run.name, func(t *testing.T) {
				calls := standInForInitHost(t)
				k := newKeptInstall(t)
				dataDir, why := shape.lay(t, k.base)
				k.writeConfig(t, "dataDir: "+dataDir+"\n", "")
				before := entriesUnder(t, k.base)

				code, out := k.keep(t, run.stdin, run.flags...)
				defer logRunOnFailure(t, out)
				if code != 1 {
					t.Errorf("a run keeping a config whose data directory serve refuses exited %d, want 1", code)
				}
				for _, want := range []string{"init: the data directory " + dataDir + " cannot be used: " + why, keptConfigWords} {
					if !strings.Contains(out, want) {
						t.Errorf("the run does not say %q", want)
					}
				}
				requireNoServiceWork(t, calls)
				if after := entriesUnder(t, k.base); !slices.Equal(before, after) {
					t.Errorf("the run changed what is there:\nbefore %q\nafter  %q", before, after)
				}
			})
		}
	}
}

// TestAKeptConfigWhoseDataDirsRecordRefusesItInstallsNoService: a run that
// keeps a config whose data directory the record beside it refuses (B651,
// B741) asks the check serve makes before anything else, its preflight
// included, and refuses as every command that makes the data directory
// does, in serve's words under its own name, making, installing and starting
// nothing, the config and the record as they were, kept each way: the data
// directory's volume away (the directory a missing data directory can be),
// its identity missing, another identity there, a marker or a record that
// does not read, and a record this user may not read, which the service init
// installs reads as this user too. On main the run graded its preflight and
// installed the service.
func TestAKeptConfigWhoseDataDirsRecordRefusesItInstallsNoService(t *testing.T) {
	const id, other = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	for _, tc := range []struct {
		name string
		// lay lays out the data directory and the record, and returns the
		// refusal's first sentence.
		lay func(t *testing.T, k keptInstall, dataDir string) string
		// denied marks the row that needs mode bits to deny this user.
		denied bool
		// says is what the refusal says beside its first sentence.
		says string
	}{
		{name: "its volume away", lay: func(t *testing.T, k keptInstall, dataDir string) string {
			writeRecordWithIdentity(t, k.cfg, dataDir, id)
			return "the data directory " + dataDir + " held this bridge's database, and holds neither bridge.db nor " +
				serveLockFileName + " now"
		}, says: "bridge init makes and writes nothing there: mount the volume and run it again"},
		{name: "its identity missing", lay: func(t *testing.T, k keptInstall, dataDir string) string {
			mkdirs(t, dataDir)
			touch(t, filepath.Join(dataDir, "bridge.db"), filepath.Join(dataDir, serveLockFileName))
			writeRecordWithIdentity(t, k.cfg, dataDir, id)
			return noIdentityLine(dataDir, "bridge.db and "+serveLockFileName)
		}, says: "bridge init makes and writes nothing there"},
		{name: "another identity", lay: func(t *testing.T, k keptInstall, dataDir string) string {
			mkdirs(t, dataDir)
			touch(t, filepath.Join(dataDir, "bridge.db"), filepath.Join(dataDir, serveLockFileName))
			writeIdentity(t, dataDir, other)
			writeRecordWithIdentity(t, k.cfg, dataDir, id)
			return otherIdentityLine(dataDir)
		}, says: "bridge init makes and writes nothing there"},
		{name: "a marker that does not read", lay: func(t *testing.T, k keptInstall, dataDir string) string {
			mkdirs(t, dataDir)
			touch(t, filepath.Join(dataDir, "bridge.db"), filepath.Join(dataDir, serveLockFileName))
			if err := os.WriteFile(dataDirMarkerFile(dataDir), []byte("{\"id\": "), 0o600); err != nil {
				t.Fatal(err)
			}
			writeRecordWithIdentity(t, k.cfg, dataDir, id)
			return unreadableIdentityLine(dataDir, "does not read as one")
		}, says: "bridge init cannot tell it from another data directory, and makes and writes nothing there"},
		{name: "a record that does not read", lay: func(t *testing.T, k keptInstall, dataDir string) string {
			if err := os.WriteFile(dataDirRecordFile(k.cfg), []byte("{\"dataDir\": "), 0o600); err != nil {
				t.Fatal(err)
			}
			return unreadableRecordLead(dataDir)
		}, says: "bridge init cannot tell it from another data directory, and makes and writes nothing there"},
		{name: "a record this user may not read", denied: true, lay: func(t *testing.T, k keptInstall, dataDir string) string {
			writeRecordWithIdentity(t, k.cfg, dataDir, id)
			chmodForTest(t, dataDirRecordFile(k.cfg), 0)
			return unreadableRecordLead(dataDir)
		}, says: "run bridge init as the user the service runs as, and make it readable by that user if it is not"},
	} {
		for _, run := range keepRuns {
			t.Run(tc.name+", "+run.name, func(t *testing.T) {
				if tc.denied {
					skipUnlessModeBitsDeny(t, "root reads a mode-0000 record, so the check reads it and refuses nothing")
				}
				calls := standInForInitHost(t)
				k := newKeptInstall(t)
				mnt := filepath.Join(k.base, "usb")
				mkdirs(t, mnt)
				dataDir := filepath.Join(mnt, "bridge")
				k.writeConfig(t, "dataDir: "+dataDir+"\n", "")
				first := tc.lay(t, k, dataDir)
				before, cfgBefore := entriesUnder(t, mnt), mustRead(t, k.cfg)

				code, out := k.keep(t, run.stdin, run.flags...)
				defer logRunOnFailure(t, out)
				if tc.denied {
					chmodForTest(t, dataDirRecordFile(k.cfg), 0o600)
				}
				if code != 1 {
					t.Errorf("a run keeping a config whose data directory its record refuses exited %d, want 1", code)
				}
				mustRefuseFor(t, out, "init: "+first, dataDirRecordFile(k.cfg))
				for _, want := range []string{tc.says, keptConfigWords} {
					if !strings.Contains(out, want) {
						t.Errorf("the run does not say %q", want)
					}
				}
				// The check comes before the preflight, which grades a
				// certificate in a data directory it cannot see, and whose
				// port lines warn of the kernel's ports whenever it runs.
				if strings.Contains(out, "preflight warnings") || strings.Contains(out, "port-api") {
					t.Errorf("the run graded its preflight before it refused the data directory")
				}
				requireNoServiceWork(t, calls)
				if after := entriesUnder(t, mnt); !slices.Equal(before, after) {
					t.Errorf("the run changed what is under the data directory's mount point:\nbefore %q\nafter  %q", before, after)
				}
				if got := mustRead(t, k.cfg); got != cfgBefore {
					t.Errorf("the run changed the config it keeps:\n%s", got)
				}
			})
		}
	}
}

// TestAKeptConfigThatDoesNotLoadInstallsNoService: a run that keeps a config
// serve does not load (a misspelt key, a fraction where a whole number goes,
// text that is not YAML, a file this user cannot read) exits 1 in the load's
// words, and makes no data directory, the config's nor init's own beside it,
// and installs nothing, kept each way. On main the run took init's own data
// directory for the install's, made none, and installed the service on it.
func TestAKeptConfigThatDoesNotLoadInstallsNoService(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		// denied marks the row that needs mode bits to deny this user.
		denied bool
	}{
		{name: "a misspelt key", extra: brokenTypo},
		{name: "a fraction where a whole number goes", extra: "scanIntervalSec: 1.5\n"},
		{name: "text that is not YAML", extra: "libraryName: [\n"},
		{name: "a config this user cannot read", denied: true},
	} {
		for _, run := range keepRuns {
			t.Run(tc.name+", "+run.name, func(t *testing.T) {
				if tc.denied {
					skipUnlessModeBitsDeny(t, "root reads a mode-0000 config, so it loads")
				}
				calls := standInForInitHost(t)
				k := newKeptInstall(t)
				k.writeConfig(t, "dataDir: "+filepath.Join(k.base, "disk", "bridge")+"\n", tc.extra)
				before := entriesUnder(t, k.base)
				if tc.denied {
					chmodForTest(t, k.cfg, 0)
				}

				code, out := k.keep(t, run.stdin, run.flags...)
				defer logRunOnFailure(t, out)
				if tc.denied {
					chmodForTest(t, k.cfg, 0o600)
				}
				if code != 1 {
					t.Errorf("a run keeping a config that does not load exited %d, want 1", code)
				}
				for _, want := range []string{"init: bridge serve does not start on the config this run keeps: load " + k.cfg, keptConfigWords} {
					if !strings.Contains(out, want) {
						t.Errorf("the run does not say %q", want)
					}
				}
				requireNoServiceWork(t, calls)
				if after := entriesUnder(t, k.base); !slices.Equal(before, after) {
					t.Errorf("the run changed what is there:\nbefore %q\nafter  %q", before, after)
				}
			})
		}
	}
}

// TestAKeptConfigWithAGoodDataDirIsKeptAsItIs (case 3, a guard): over an
// install a serve has served from, its data directory there and holding the
// identity its record holds, a run that keeps the config installs the service
// on it and changes nothing in it, the config and the record as they were,
// and a serve starts on it again. Green on main too.
func TestAKeptConfigWithAGoodDataDirIsKeptAsItIs(t *testing.T) {
	v := newIdentityVolume(t)
	calls := standInForInitHost(t)
	before, cfgBefore, recordBefore := entriesUnder(t, v.dataDir), mustRead(t, v.cfg), mustRead(t, dataDirRecordFile(v.cfg))
	var stdout, stderr strings.Builder
	code := initCmd([]string{"--yes", "--dir", v.base, "--library", v.lib}, strings.NewReader(""), &stdout, &stderr)
	out := stripANSI(stdout.String() + stderr.String())
	defer logRunOnFailure(t, out)
	if code != 0 {
		t.Fatalf("a run keeping the config of a healthy install exited %d", code)
	}
	requireInstalledOn(t, calls, v.dataDir)
	if after := entriesUnder(t, v.dataDir); !slices.Equal(before, after) {
		t.Errorf("the run changed what the data directory holds:\nbefore %.600q\nafter  %.600q", before, after)
	}
	if mustRead(t, v.cfg) != cfgBefore || mustRead(t, dataDirRecordFile(v.cfg)) != recordBefore {
		t.Errorf("the run changed the config or the record beside it")
	}
	stopServed(t, bootServe(t, "--config", v.cfg))
}

// TestANewConfigsDataDirIsMadeBeforeItsServiceIsInstalled (case 4, a guard):
// a first install makes its data directory, 0700, before it installs the
// service on it, as it always has. Green on main too.
func TestANewConfigsDataDirIsMadeBeforeItsServiceIsInstalled(t *testing.T) {
	calls := standInForInitHost(t)
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	code, out := runInit(t, "--yes", "--skip-doctor", "--dir", cfgDir, "--library", testLibrary(t),
		"--listen-address", "127.0.0.1:0", "--admin-address", "127.0.0.1:0")
	defer logRunOnFailure(t, out)
	if code != 0 {
		t.Fatalf("a first install exited %d", code)
	}
	dataDir := initDataDirFor(cfgDir)
	requireInstalledOn(t, calls, dataDir)
	if info, err := os.Stat(dataDir); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Errorf("the data directory was made %v, want 0700", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "bridge.yaml")); err != nil {
		t.Errorf("the first install wrote no config: %v", err)
	}
}
