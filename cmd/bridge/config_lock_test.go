package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// Every writer of bridge.yaml takes the lock beside it around its
// read-modify-write, and reads the file again under it (backlog B506): two
// writers take turns, and neither saves a copy read before its turn. Until
// then library add and remove and init saved with no lock, bind narrowed the
// window with SaveIf's byte comparison alone, and a serve's console save wrote
// its own copy over whatever another writer had saved since it read the file.

// configLockWait is how long a writer waits for bridge.yaml's lock before it
// refuses.
const configLockWait = config.LockWait

// configBusy is what a writer says when it gave up waiting for the lock.
const configBusy = "another bridge command or the running bridge is changing"

// holdConfigLock takes the lock beside the config file at cfgPath through an
// open of its own, as another bridge command would, and returns what lets it
// go: the lock is `.<name>.lock` in the config's directory, an flock or a
// LockFileEx on the open file.
func holdConfigLock(t *testing.T, cfgPath string) (release func()) {
	t.Helper()
	f, err := fsutil.OpenLockFile(filepath.Dir(cfgPath), "."+filepath.Base(cfgPath)+".lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.TryLock(f); err != nil {
		_ = f.Close()
		t.Fatalf("take bridge.yaml's lock: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = fsutil.Unlock(f)
			_ = f.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// saveConfigAt saves cfg as the config at path in a turn of its lock, as a
// writer with nothing to read first does (a test's fixture).
func saveConfigAt(cfg *config.Config, path string) error {
	lock, err := config.LockFile(path)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	return cfg.Save(lock)
}

// libraryDir makes an empty library root named name.
func libraryDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// lockTestConfig writes a loopback config over roots whose admin port
// nothing listens on (so library add, bind and restore find no bridge
// running), with tail appended, and returns its path and data dir.
func lockTestConfig(t *testing.T, roots []string, tail string) (cfgPath, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	dataDir = filepath.Join(dir, "data")
	var b strings.Builder
	b.WriteString("libraryRoots:\n")
	for _, r := range roots {
		fmt.Fprintf(&b, "  - '%s'\n", r)
	}
	fmt.Fprintf(&b, "listenAddress: 127.0.0.1:7788\nadminAddress: 127.0.0.1:1\ndataDir: '%s'\nscanIntervalSec: 3600\nlibraryName: Test Library\n", dataDir)
	b.WriteString(tail)
	cfgPath = filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataDir
}

// readBytes returns the file at path, or nil when there is none.
func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runLibraryCapture runs `bridge library` with args on env.
func runLibraryCapture(env libraryEnv, args ...string) (code int, out string) {
	var o, e bytes.Buffer
	code = runLibrary(context.Background(), args, env, &o, &e)
	return code, o.String() + e.String()
}

// TestLibraryChangesKeepASettingsSaveThatLandsAfterTheirRead: a settings
// save of the running bridge that lands between library add's (or
// remove's) read of the config and its save is kept beside the new root
// list, because the command reads the file again once it holds the lock.
// Until B506 it saved the copy it read before, and the settings save was
// gone.
func TestLibraryChangesKeepASettingsSaveThatLandsAfterTheirRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		// roots are the config's roots, args the command, and want the roots
		// it leaves.
		roots func(a, b string) []string
		args  func(cfgPath, a, b string) []string
		want  func(a, b string) []string
	}{
		{"library add",
			func(a, _ string) []string { return []string{a} },
			func(cfgPath, _, b string) []string { return []string{"add", "--config", cfgPath, b} },
			func(a, b string) []string { return []string{a, b} }},
		{"library remove",
			func(a, b string) []string { return []string{a, b} },
			func(cfgPath, _, b string) []string { return []string{"remove", "--config", cfgPath, b} },
			func(a, _ string) []string { return []string{a} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			music, extra := libraryDir(t, "Music"), libraryDir(t, "Extra")
			cfgPath, _ := lockTestConfig(t, tc.roots(music, extra), "")
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			// The running bridge's console, which saves through the holder
			// serve keeps its live copy in (admin.Deps.CfgHolder).
			holder := config.NewRuntimeConfig(cfg)
			env := libraryEnv{beforeLock: func() {
				if err := holder.Update(cfgPath, func(c *config.Config) error {
					c.LibraryName = "Renamed"
					return nil
				}); err != nil {
					t.Errorf("the settings save: %v", err)
				}
			}}
			if code, out := runLibraryCapture(env, tc.args(cfgPath, music, extra)...); code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			after, err := config.Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if after.LibraryName != "Renamed" {
				t.Errorf("the command wrote over the settings save that landed after its read: libraryName %q", after.LibraryName)
			}
			if want := tc.want(music, extra); !slices.Equal(after.LibraryRoots, want) {
				t.Errorf("libraryRoots %q, want %q", after.LibraryRoots, want)
			}
		})
	}
}

// TestBindKeepsALibraryAddThatLandsInItsWindow: a library add that starts
// while bind holds the lock, between bind's read under it and its save,
// waits for bind's turn, then reads the file bind wrote and adds its root to
// it, so the binding and the root both stay. Until B506 the add saved at
// once, and bind's byte comparison refused the binding.
func TestBindKeepsALibraryAddThatLandsInItsWindow(t *testing.T) {
	root := dacTree(t)
	music, extra := libraryDir(t, "Music"), libraryDir(t, "Extra")
	cfgPath, _ := lockTestConfig(t, []string{music}, "output:\n  player:\n    room: Study\n")
	added := make(chan string, 1)
	started := false
	env := dacEnv()
	env.beforeSave = func() {
		started = true
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		drainLoopOnCleanup(t, cancel, done, "the library add started in bind's window")
		go func() {
			defer close(done)
			var o, e bytes.Buffer
			code := runLibrary(ctx, []string{"add", "--config", cfgPath, extra}, libraryEnv{}, &o, &e)
			added <- fmt.Sprintf("exit %d\n%s%s", code, o.String(), e.String())
		}()
		// Without the lock the add saves at once; with it, it waits for this
		// bind's turn. Either way bind goes on after a moment.
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
		}
	}
	_, errOut, code := runDAC(t, env, append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...)
	if !started {
		t.Fatalf("bind never reached its save: exit %d, stderr %q", code, errOut)
	}
	addOut := <-added
	if code != 0 {
		t.Errorf("bind: exit %d, stderr %q", code, errOut)
	}
	if !strings.HasPrefix(addOut, "exit 0\n") {
		t.Errorf("library add: %s", addOut)
	}
	after, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Output.Player.Device.Binding != config.BindingSerial {
		t.Errorf("the binding is gone: %+v", after.Output.Player.Device)
	}
	if !slices.Equal(after.LibraryRoots, []string{music, extra}) {
		t.Errorf("libraryRoots %q, want the added root beside the binding", after.LibraryRoots)
	}
	if after.Output.Player.Room != "Study" {
		t.Errorf("room %q, want Study", after.Output.Player.Room)
	}
}

// TestServeRefusesASettingsSaveOverAConfigAnotherWriterChanged: a console
// save of a running serve reads bridge.yaml again under the lock, and when
// another writer changed it since serve read it (a library add whose probe
// found no bridge a moment before serve started, init, an edit), the save is
// refused and the file is left as that writer left it. Until B506 the
// console wrote its own copy over it.
func TestServeRefusesASettingsSaveOverAConfigAnotherWriterChanged(t *testing.T) {
	b := startConsoleBridge(t, "libraryName: Before\n", nil)
	cfgPath := filepath.Join(filepath.Dir(b.dataDir), "bridge.yaml")
	f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("scanIntervalSec: 7200\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	edited := readBytes(t, cfgPath)

	status, body := patchSettings(t, b, `{"libraryName":"After"}`)
	if status == http.StatusOK {
		t.Errorf("the console's save went through over a file another writer changed: %s", body)
	}
	if !strings.Contains(body, "changed") || !strings.Contains(body, "restart") {
		t.Errorf("the refusal does not say the file changed and what to do: %d %s", status, body)
	}
	if after := readBytes(t, cfgPath); !bytes.Equal(after, edited) {
		t.Errorf("the console wrote over the other writer's change; the file is\n%s", after)
	}
}

// TestAConsoleSaveWaitsForTheLockThenRefuses: a console save while
// another writer holds bridge.yaml's lock (a CLI beside the running bridge)
// waits for it, and once the wait is over refuses, saying another bridge
// command is changing the file, with the file and the running bridge's name
// as they were; once the lock is free the next save goes through.
func TestAConsoleSaveWaitsForTheLockThenRefuses(t *testing.T) {
	b := startConsoleBridge(t, "libraryName: Before\n", nil)
	cfgPath := filepath.Join(filepath.Dir(b.dataDir), "bridge.yaml")
	before := readBytes(t, cfgPath)
	release := holdConfigLock(t, cfgPath)
	start := time.Now()
	status, body := patchSettings(t, b, `{"libraryName":"After"}`)
	if took := time.Since(start); took < configLockWait {
		t.Errorf("the console answered in %s, before the lock's wait of %s was over", took, configLockWait)
	}
	if status == http.StatusOK || !strings.Contains(body, configBusy) {
		t.Errorf("the console's save while another held the lock: %d %s", status, body)
	}
	if after := readBytes(t, cfgPath); !bytes.Equal(after, before) {
		t.Errorf("the console wrote the file while another held its lock:\n%s", after)
	}
	release()
	if status, body := patchSettings(t, b, `{"libraryName":"After"}`); status != http.StatusOK {
		t.Errorf("the console's save once the lock was free: %d %s", status, body)
	}
	if cfg, err := config.Load(cfgPath); err != nil || cfg.LibraryName != "After" {
		t.Errorf("after the free save the file loads with libraryName %v, %v", cfg, err)
	}
}

// patchSettings sends body to the console's settings PATCH.
func patchSettings(t *testing.T, b *consoleBridge, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, b.adminBase+"/api/settings", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.console.Do(req)
	if err != nil {
		t.Fatalf("PATCH /api/settings: %v; stderr=%s", err, b.stderr.String())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// configWriterCase is one writer of bridge.yaml, run by a test that holds
// the lock.
type configWriterCase struct {
	name string
	// setup lays out what the writer runs over and returns the config's
	// path, and the run.
	setup func(t *testing.T) (cfgPath string, run func() (int, string))
}

// configWriterCases are the CLI writers of bridge.yaml, each with a change
// to make.
func configWriterCases() []configWriterCase {
	return []configWriterCase{
		{name: "library add", setup: func(t *testing.T) (string, func() (int, string)) {
			cfgPath, _ := lockTestConfig(t, []string{libraryDir(t, "Music")}, "")
			extra := libraryDir(t, "Extra")
			return cfgPath, func() (int, string) { return runLibraryCapture(libraryEnv{}, "add", "--config", cfgPath, extra) }
		}},
		{name: "library remove", setup: func(t *testing.T) (string, func() (int, string)) {
			extra := libraryDir(t, "Extra")
			cfgPath, _ := lockTestConfig(t, []string{libraryDir(t, "Music"), extra}, "")
			return cfgPath, func() (int, string) { return runLibraryCapture(libraryEnv{}, "remove", "--config", cfgPath, extra) }
		}},
		{name: "init's rewrite", setup: func(t *testing.T) (string, func() (int, string)) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			lib := testLibrary(t)
			writeLoopbackInstall(t, cfgDir, lib, freeLoopbackPort(t), freeLoopbackPort(t))
			return filepath.Join(cfgDir, "bridge.yaml"), func() (int, string) {
				return runInit(t, "--yes", "--force", "--no-service", "--skip-doctor", "--dir", cfgDir, "--library", lib, "--name", "Rewritten")
			}
		}},
		{name: "output dac bind", setup: func(t *testing.T) (string, func() (int, string)) {
			root := dacTree(t)
			cfgPath, _ := lockTestConfig(t, []string{libraryDir(t, "Music")}, "")
			return cfgPath, func() (int, string) {
				out, errOut, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...)
				return code, out + errOut
			}
		}},
		{name: "serve's seed", setup: func(t *testing.T) (string, func() (int, string)) {
			cfgPath := filepath.Join(t.TempDir(), "bridge.yaml")
			return cfgPath, func() (int, string) {
				wrote, err := writeAutoInitConfig(cfgPath)
				if err != nil {
					return 1, err.Error()
				}
				return 0, fmt.Sprintf("wrote %v", wrote)
			}
		}},
		{name: "restore", setup: func(t *testing.T) (string, func() (int, string)) {
			cfgPath, _, snap := seedRestorableInstall(t, closedAdminAddr(t))
			// A change since the snapshot, which a restore would undo.
			f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("libraryName: Since\n"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			return cfgPath, func() (int, string) {
				code, out, errOut := runRestoreYes(t, cfgPath, snap)
				return code, out + errOut
			}
		}},
	}
}

// TestEveryConfigWriterWaitsForTheLockThenRefuses: while another open holds
// bridge.yaml's lock, each writer waits for it, and once the wait is over
// refuses, saying another bridge command or the running bridge is changing
// the file, with the file as it was. Until B506 none of them asked.
func TestEveryConfigWriterWaitsForTheLockThenRefuses(t *testing.T) {
	for _, tc := range configWriterCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfgPath, run := tc.setup(t)
			before := readBytes(t, cfgPath)
			holdConfigLock(t, cfgPath)
			start := time.Now()
			code, out := run()
			took := time.Since(start)
			if code == 0 {
				t.Errorf("the writer went through while another held the lock (in %s):\n%s", took, out)
			} else if !strings.Contains(out, configBusy) {
				t.Errorf("the refusal does not say another writer holds the file (exit %d):\n%s", code, out)
			}
			if took < configLockWait {
				t.Errorf("the writer answered in %s, before the lock's wait of %s was over", took, configLockWait)
			}
			if after := readBytes(t, cfgPath); !bytes.Equal(after, before) {
				t.Errorf("the writer changed the file while another held the lock:\n%s", after)
			}
		})
	}
}

// TestARestoreRefusedTheLockRestoresNothing: a restore that cannot have
// bridge.yaml's lock is refused before it writes any file, the database
// included, so a busy config never leaves a half-restored install.
func TestARestoreRefusedTheLockRestoresNothing(t *testing.T) {
	cfgPath, dataDir, snap := seedRestorableInstall(t, closedAdminAddr(t))
	store, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	const since = "Artist/Album/Since.flac"
	if err := store.UpsertTrack(context.Background(), &manifest.Track{Path: since, Size: 1, ModTime: time.Unix(0, 0).UTC(), Title: "Since"}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	holdConfigLock(t, cfgPath)
	if code, out, errOut := runRestoreYes(t, cfgPath, snap); code == 0 {
		t.Fatalf("restore went through while another held the config's lock:\n%s%s", out, errOut)
	}
	if !trackExists(t, filepath.Dir(dataDir), since) {
		t.Error("the refused restore replaced the database")
	}
}

// TestTheDataDirLockComesBeforeTheConfigLock: a command that takes both
// locks, bind and restore, takes the data dir's first and bridge.yaml's
// inside it, never the other way, so two commands cannot each hold the lock
// the other waits for. Held, bridge.yaml's lock finds the command holding
// the data dir while it waits; with both held, the command is refused at the
// data dir at once, never after waiting out bridge.yaml's lock.
func TestTheDataDirLockComesBeforeTheConfigLock(t *testing.T) {
	type command struct {
		name string
		// setup lays out what the command runs over and returns the config,
		// the data dir, and the run.
		setup func(t *testing.T) (cfgPath, dataDir string, run func() (int, string))
	}
	for _, c := range []command{
		{"output dac bind", func(t *testing.T) (string, string, func() (int, string)) {
			root := dacTree(t)
			cfgPath, dataDir := lockTestConfig(t, []string{libraryDir(t, "Music")}, "")
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			return cfgPath, dataDir, func() (int, string) {
				out, errOut, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...)
				return code, out + errOut
			}
		}},
		{"restore", func(t *testing.T) (string, string, func() (int, string)) {
			cfgPath, dataDir, snap := seedRestorableInstall(t, closedAdminAddr(t))
			return cfgPath, dataDir, func() (int, string) {
				code, out, errOut := runRestoreYes(t, cfgPath, snap)
				return code, out + errOut
			}
		}},
	} {
		t.Run(c.name+", with bridge.yaml's lock held", func(t *testing.T) {
			cfgPath, dataDir, run := c.setup(t)
			release := holdConfigLock(t, cfgPath)
			var code int
			var out string
			// The command takes no context: the cancel the drain asks for
			// stops nothing, and the drain waits for the command to return.
			_, cancel := context.WithCancel(context.Background())
			finished := make(chan struct{})
			drainLoopOnCleanup(t, cancel, finished, "the "+c.name+" waiting for bridge.yaml's lock")
			go func() {
				defer close(finished)
				code, out = run()
			}()
			held, early := dataDirHeldMidwayThroughTheWait(t, dataDir, finished)
			release()
			<-finished
			switch {
			case early:
				t.Errorf("the command returned before bridge.yaml's lock was free, so it never waited for it (exit %d):\n%s", code, out)
			case !held:
				t.Errorf("the command did not hold the data dir while it waited for bridge.yaml's lock (exit %d):\n%s", code, out)
			}
			if code != 0 {
				t.Errorf("the command did not go through once bridge.yaml's lock was free (exit %d):\n%s", code, out)
			}
		})
		t.Run(c.name+", with both locks held", func(t *testing.T) {
			cfgPath, dataDir, run := c.setup(t)
			release, err := lockServeDataDir(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(release)
			holdConfigLock(t, cfgPath)
			start := time.Now()
			code, out := run()
			took := time.Since(start)
			if code == 0 || !strings.Contains(out, "already running on the data dir") {
				t.Errorf("the command was not refused at the data dir (exit %d):\n%s", code, out)
			}
			if took >= configLockWait {
				t.Errorf("the command was refused after %s: it waited for bridge.yaml's lock before it asked for the data dir", took)
			}
		})
	}
}

// dataDirHeldMidwayThroughTheWait asks once, halfway through bridge.yaml's
// lock wait, whether another open holds dataDir's lock: held. early is a
// command that returned before then. One question, not a poll: the data
// dir's lock is a try that never waits, so a question that held it at the
// moment the command asked for it would refuse the command. Halfway through
// the wait a command that takes the data dir first took it long before.
func dataDirHeldMidwayThroughTheWait(t *testing.T, dataDir string, finished <-chan struct{}) (held, early bool) {
	t.Helper()
	select {
	case <-finished:
		return false, true
	case <-time.After(configLockWait / 2):
	}
	release, err := lockServeDataDir(dataDir)
	if err == nil {
		release()
		return false, false
	}
	var heldErr *serveDataDirHeldError
	if !errors.As(err, &heldErr) {
		t.Fatalf("ask for the data dir's lock: %v", err)
	}
	return true, false
}
