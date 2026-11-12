package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// restoreWaitReserve is what a capped wait leaves of the test binary's
// deadline for the cleanups that run after it (restoreWaitWithin).
const restoreWaitReserve = 10 * time.Second

// restoreWaitWithin is how long a restore test's wait for an event may take:
// limit, the wait's own maximum, capped by the test binary's deadline less
// restoreWaitReserve, so that a missed event fails its case while the
// cleanups after it still have time to run; with no deadline (-timeout 0) the
// maximum alone applies (backlog B871).
func restoreWaitWithin(t *testing.T, limit time.Duration) time.Duration {
	t.Helper()
	if deadline, ok := t.Deadline(); ok {
		if left := time.Until(deadline) - restoreWaitReserve; left < limit {
			return max(left, 0)
		}
	}
	return limit
}

// awaitRestoreEvent waits for c to close, failing the test past its wait.
func awaitRestoreEvent(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(restoreWaitWithin(t, 30*time.Second)):
		t.Fatalf("%s never came", what)
	}
}

// lookWorkerEnd answers a channel closed once the worker of bridge restore's
// look at dir, the snapshot as the command was given it, has ended
// (backup.SetOnCheckSourceEndForTest): it can outlive the command's answer.
// It is installed before the look it hears, and its restore, registered here,
// runs after every cleanup the test registers later, a wait for that end
// among them.
func lookWorkerEnd(t *testing.T, dir string) <-chan struct{} {
	t.Helper()
	ended := make(chan struct{})
	var once sync.Once
	t.Cleanup(backup.SetOnCheckSourceEndForTest(func(d string) {
		if d == dir {
			once.Do(func() { close(ended) })
		}
	}))
	return ended
}

// restoreRun is a bridge restore run on a goroutine of its own (start), its
// exit code sent on done once it returns. Its join, which newRestoreRun
// registers, waits for that return at the test's end, bounded, on every exit
// path: make it before registering the cleanups that let the run go on (its
// context's cancel, its stdin's close, a stalled hook's release), which then
// run first, cleanups running last-registered-first.
type restoreRun struct {
	done    chan int
	ended   chan struct{}
	started atomic.Bool
}

// newRestoreRun makes a restoreRun and registers its join.
func newRestoreRun(t *testing.T) *restoreRun {
	t.Helper()
	r := &restoreRun{done: make(chan int, 1), ended: make(chan struct{})}
	t.Cleanup(func() {
		if !r.started.Load() {
			return
		}
		select {
		case <-r.ended:
		case <-time.After(restoreWaitWithin(t, 30*time.Second)):
			t.Error("bridge restore never returned")
		}
	})
	return r
}

// start runs bridge restore on ctx with args.
func (r *restoreRun) start(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) {
	r.started.Store(true)
	go func() {
		defer close(r.ended)
		r.done <- restoreCmd(ctx, args, stdin, stdout, stderr)
	}()
}

// code is the run's exit code, failing the test past its wait.
func (r *restoreRun) code(t *testing.T) int {
	t.Helper()
	select {
	case code := <-r.done:
		return code
	case <-time.After(restoreWaitWithin(t, 30*time.Second)):
		t.Fatal("bridge restore never returned")
	}
	return 0
}

// restoreArgs is bridge restore's arguments for the snapshot snap into the
// install at cfgPath, with --yes where yes.
func restoreArgs(cfgPath, snap string, yes bool) []string {
	if yes {
		return []string{"--config", cfgPath, "--yes", snap}
	}
	return []string{"--config", cfgPath, snap}
}

// pathName names a case by the way bridge restore is run.
func pathName(yes bool) string {
	if yes {
		return "--yes"
	}
	return "asking for confirmation"
}

// requireStoppedBeforeThePrompt fails the test unless bridge restore exited 1
// in the plain stop's words (backup.CheckStopped), asked nothing and restored
// nothing.
func requireStoppedBeforeThePrompt(t *testing.T, code int, stdout, stderr string) {
	t.Helper()
	if code != 1 || !strings.Contains(stderr, "stopped before its prompt") || strings.Contains(stderr, "did not answer") {
		t.Errorf("bridge restore stopped = %d, want 1 in the words of a plain stop before its prompt\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if strings.Contains(stderr, "Type 'yes'") || strings.Contains(stdout, "Restore from") || strings.Contains(stdout, "Restore complete") {
		t.Errorf("bridge restore went on past its stop:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
}

// TestBridgeRestoreStoppedDuringItsLookExitsBeforeItsPrompt is backlog B871's
// item 1 through the command: a Ctrl-C that lands while bridge restore's look
// at the snapshot reads its manifest, the read answering within the stop's
// grace, stops the command before its prompt, on the path that asks and on
// --yes alike, exit 1 in the plain stop's words, the install as it was; never
// "Type 'yes'". The install's admin port is the kernel's, so that nothing but
// the data dir's lock stands between the look and the prompt. Before the fix
// the look answered nil, and the prompt came.
func TestBridgeRestoreStoppedDuringItsLookExitsBeforeItsPrompt(t *testing.T) {
	for _, yes := range []bool{false, true} {
		t.Run(pathName(yes), func(t *testing.T) {
			_, _, snapA := seedRestorableInstall(t, closedAdminAddr(t))
			cfgB, dataB, _ := seedRestorableInstall(t, "127.0.0.1:0")
			before := readInstallState(t, cfgB, dataB)
			resolved, err := fsutil.ResolveLinks(snapA)
			if err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(resolved, backup.ManifestFile)
			ended := lookWorkerEnd(t, snapA)
			entered, out := make(chan struct{}), make(chan struct{})
			var armed atomic.Bool
			armed.Store(true)
			t.Cleanup(backup.SetReadSourceManifestForTest(func(p string) ([]byte, error) {
				if p == manifest && armed.CompareAndSwap(true, false) {
					close(entered)
					<-out
				}
				return os.ReadFile(p)
			}))
			run := newRestoreRun(t)
			ctx, cancel := context.WithCancel(context.Background())
			release := sync.OnceFunc(func() { close(out) })
			t.Cleanup(func() {
				cancel()
				release()
				if run.started.Load() {
					awaitRestoreEvent(t, ended, "the end of the look's worker")
				}
			})

			var stdout, stderr safeBuffer
			run.start(ctx, restoreArgs(cfgB, snapA, yes), strings.NewReader(""), &stdout, &stderr)
			awaitRestoreEvent(t, entered, "the look's read of the manifest")
			cancel()
			release()
			code := run.code(t)
			requireStoppedBeforeThePrompt(t, code, stdout.String(), stderr.String())
			requireInstallUntouched(t, cfgB, dataB, before)
		})
	}
}

// TestBridgeRestoreStoppedJustAfterItsLookExitsBeforeItsPrompt is backlog
// B871's item 1, its second half: bridge restore asks its stop once more
// right after its look has answered, before the path that asks and --yes
// split, so a Ctrl-C that lands there stops both alike, exit 1 in the plain
// stop's words, before the config is read, the data dir's lock is taken or the
// prompt shown, the install as it was. Before the fix the path that asks
// showed "Type 'yes'", and --yes went on to the restore's own stop.
func TestBridgeRestoreStoppedJustAfterItsLookExitsBeforeItsPrompt(t *testing.T) {
	for _, yes := range []bool{false, true} {
		t.Run(pathName(yes), func(t *testing.T) {
			_, _, snapA := seedRestorableInstall(t, closedAdminAddr(t))
			cfgB, dataB, _ := seedRestorableInstall(t, "127.0.0.1:0")
			before := readInstallState(t, cfgB, dataB)
			ended := lookWorkerEnd(t, snapA)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			prev := onRestoreChecked
			onRestoreChecked = cancel
			t.Cleanup(func() { onRestoreChecked = prev })

			var stdout, stderr strings.Builder
			code := restoreCmd(ctx, restoreArgs(cfgB, snapA, yes), strings.NewReader(""), &stdout, &stderr)
			awaitRestoreEvent(t, ended, "the end of the look's worker")
			requireStoppedBeforeThePrompt(t, code, stdout.String(), stderr.String())
			requireInstallUntouched(t, cfgB, dataB, before)
		})
	}
}

// freshInstallConfig writes the config of an install whose data dir is not
// made yet, its admin port the kernel's, and answers the config and the data
// dir: a refusal before bridge restore's data dir lock leaves the data dir
// unmade, and one after it does not (holdDataDirForMutation makes it).
func freshInstallConfig(t *testing.T) (cfgPath, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir = filepath.Join(dir, "data")
	cfgPath = filepath.Join(dir, "bridge.yaml")
	body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\nlistenAddress: 127.0.0.1:0\nadminAddress: 127.0.0.1:0\ndisableHttp3: true\nmdns:\n    enabled: false\n",
		lib, dataDir)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataDir
}

// withSchema rewrites the schema the manifest of the snapshot at snap
// records, keeping every other field as written.
func withSchema(t *testing.T, snap string, v int) {
	t.Helper()
	p := filepath.Join(snap, backup.ManifestFile)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(readBytes(t, p), &m); err != nil {
		t.Fatal(err)
	}
	m["schemaVersion"] = json.RawMessage(fmt.Sprint(v))
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestBridgeRestoreRefusesANewerSchemaBeforeItsPrompt is backlog B871's item
// 2 through the command: a snapshot of a schema newer than this bridge takes
// is refused by bridge restore's look before its prompt, on the path that
// asks and on --yes alike, exit 1 in the precise words the restore refuses it
// in once confirmed, never as a path that holds no snapshot (exit 2), before
// the data dir is made or locked. The schemas this bridge takes, 1 and
// SchemaVersion, are the controls: the path that asks reaches its prompt, and
// --yes restores. Before the fix a newer schema reached the prompt, and --yes
// made the data dir before the restore refused it.
func TestBridgeRestoreRefusesANewerSchemaBeforeItsPrompt(t *testing.T) {
	for _, v := range []int{backup.SchemaVersion + 1, 1, backup.SchemaVersion} {
		for _, yes := range []bool{false, true} {
			t.Run(fmt.Sprintf("schema %d, %s", v, pathName(yes)), func(t *testing.T) {
				_, _, snapA := seedRestorableInstall(t, closedAdminAddr(t))
				withSchema(t, snapA, v)
				cfgB, dataB := freshInstallConfig(t)
				ended := lookWorkerEnd(t, snapA)
				var stdout, stderr strings.Builder
				code := restoreCmd(context.Background(), restoreArgs(cfgB, snapA, yes), strings.NewReader(""), &stdout, &stderr)
				awaitRestoreEvent(t, ended, "the end of the look's worker")
				switch {
				case v > backup.SchemaVersion:
					want := fmt.Sprintf("snapshot schema v%d not supported (this bridge supports v%d)", v, backup.SchemaVersion)
					if code != 1 || !strings.Contains(stderr.String(), want) || strings.Contains(stderr.String(), "not a recognized snapshot") {
						t.Errorf("bridge restore of a snapshot of schema %d = %d, want 1 saying %q\nstdout:\n%s\nstderr:\n%s", v, code, want, stdout.String(), stderr.String())
					}
					if strings.Contains(stderr.String(), "Type 'yes'") || strings.Contains(stdout.String(), "Restore from") {
						t.Errorf("bridge restore asked for its confirmation of a snapshot of schema %d:\n%s%s", v, stdout.String(), stderr.String())
					}
					if _, err := os.Lstat(dataB); !errors.Is(err, fs.ErrNotExist) {
						t.Errorf("bridge restore made the data dir before it refused a snapshot of schema %d: %v", v, err)
					}
				case yes:
					if code != 0 || !strings.Contains(stdout.String(), "Restore complete") {
						t.Errorf("bridge restore --yes of a snapshot of schema %d = %d, want it restored\nstdout:\n%s\nstderr:\n%s", v, code, stdout.String(), stderr.String())
					}
				default:
					if !strings.Contains(stderr.String(), "Type 'yes'") {
						t.Errorf("bridge restore of a snapshot of schema %d never asked for its confirmation (exit %d)\nstdout:\n%s\nstderr:\n%s", v, code, stdout.String(), stderr.String())
					}
				}
			})
		}
	}
}

// TestBridgeRestoreSaysWhatItsLookCouldNotRead is backlog B871's item 4
// through the command: what its look at the snapshot could not read is said
// by its kind. A path that holds no snapshot, nothing there (a missing path, a
// link to nothing, at the snapshot or at its manifest), no folder where one
// goes (a regular file given, or in the way) or a manifest that does not
// decode or holds no schema, is invalid input: exit 2, "not a recognized
// snapshot directory". A lookup or a manifest's read this user may not make,
// or that fails in I/O, is no such thing: exit 1, its cause's words and the
// snapshot as typed. The failures are injected through the look's seams,
// never by a mode. Before the fix every one of them was exit 2, its cause
// dropped. The look refuses before the config is read, so the config named is
// none.
func TestBridgeRestoreSaysWhatItsLookCouldNotRead(t *testing.T) {
	snapshotAt := func(t *testing.T, manifest string) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "2026-11-12T00-00-00Z")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if manifest != "" {
			if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	link := func(t *testing.T, to, at string) {
		t.Helper()
		if err := os.Symlink(to, at); err != nil {
			t.Skipf("no symbolic link can be made here: %v", err)
		}
	}
	regularFile := func(t *testing.T) string {
		t.Helper()
		file := filepath.Join(t.TempDir(), "snapshot.tar")
		if err := os.WriteFile(file, []byte("not a folder"), 0o600); err != nil {
			t.Fatal(err)
		}
		return file
	}
	for _, tc := range []struct {
		name  string
		given func(t *testing.T) string
		// failLookup and failRead, set, make the snapshot's lookup or the
		// manifest's read fail with them; the refusal then carries the cause,
		// exit 1, where a path that holds no snapshot exits 2.
		failLookup, failRead error
	}{
		{name: "a missing path", given: func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") }},
		{name: "a link to nothing", given: func(t *testing.T) string {
			dir := t.TempDir()
			link(t, filepath.Join(dir, "gone"), filepath.Join(dir, "snapshot"))
			return filepath.Join(dir, "snapshot")
		}},
		{name: "a path through a regular file", given: func(t *testing.T) string { return filepath.Join(regularFile(t), "2026-11-12T00-00-00Z") }},
		{name: "a regular file given", given: regularFile},
		{name: "a folder with no manifest", given: func(t *testing.T) string { return snapshotAt(t, "") }},
		{name: "a manifest that is a link to nothing", given: func(t *testing.T) string {
			snap := snapshotAt(t, "")
			link(t, filepath.Join(snap, "gone.json"), filepath.Join(snap, backup.ManifestFile))
			return snap
		}},
		{name: "a manifest that does not decode", given: func(t *testing.T) string { return snapshotAt(t, "{not json") }},
		{name: "a manifest with no schema", given: func(t *testing.T) string { return snapshotAt(t, "{}") }},
		{name: "the snapshot's lookup refused", given: func(t *testing.T) string { return snapshotAt(t, `{"schemaVersion":2}`) }, failLookup: syscall.EACCES},
		{name: "the snapshot's lookup failing in I/O", given: func(t *testing.T) string { return snapshotAt(t, `{"schemaVersion":2}`) }, failLookup: syscall.EIO},
		{name: "the manifest's read refused", given: func(t *testing.T) string { return snapshotAt(t, `{"schemaVersion":2}`) }, failRead: syscall.EACCES},
		{name: "the manifest's read failing in I/O", given: func(t *testing.T) string { return snapshotAt(t, `{"schemaVersion":2}`) }, failRead: syscall.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			given := tc.given(t)
			ended := lookWorkerEnd(t, given)
			if tc.failLookup != nil {
				abs, err := filepath.Abs(given)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(backup.SetMetadataForTest(func(p string) (string, error) {
					if p == abs {
						return "", &fs.PathError{Op: "lstat", Path: p, Err: tc.failLookup}
					}
					return fsutil.ResolveLinks(p)
				}, nil))
			}
			if tc.failRead != nil {
				resolved, err := fsutil.ResolveLinks(given)
				if err != nil {
					t.Fatal(err)
				}
				manifest := filepath.Join(resolved, backup.ManifestFile)
				t.Cleanup(backup.SetReadSourceManifestForTest(func(p string) ([]byte, error) {
					if p == manifest {
						return nil, &fs.PathError{Op: "open", Path: p, Err: tc.failRead}
					}
					return os.ReadFile(p)
				}))
			}
			cfg := filepath.Join(t.TempDir(), "none.yaml")
			var stdout, stderr strings.Builder
			code := restoreCmd(context.Background(), restoreArgs(cfg, given, true), strings.NewReader(""), &stdout, &stderr)
			awaitRestoreEvent(t, ended, "the end of the look's worker")
			cause := tc.failLookup
			if cause == nil {
				cause = tc.failRead
			}
			if cause == nil {
				if code != 2 || !strings.Contains(stderr.String(), "is not a recognized snapshot directory") {
					t.Errorf("bridge restore of %s = %d, want 2 as a path that holds no snapshot\nstderr:\n%s", tc.name, code, stderr.String())
				}
				return
			}
			msg := stderr.String()
			if code != 1 || strings.Contains(msg, "not a recognized snapshot") || !strings.Contains(msg, cause.Error()) || !saysPath(msg, given) {
				t.Errorf("bridge restore whose %s = %d, want 1 with its cause's words (%q) and %s as typed\nstderr:\n%s", tc.name, code, cause.Error(), given, msg)
			}
		})
	}
}
