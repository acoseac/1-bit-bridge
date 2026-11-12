package backup_test

import (
	"context"
	"errors"
	"fmt"
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

// fakeSnapshot lays out, as resolved, a folder named as a snapshot is, in a
// folder of its own, holding a manifest.json whose bytes are manifest: all
// that bridge restore's look at a snapshot before its prompt reads
// (backup.CheckRestoreSource).
func fakeSnapshot(t *testing.T, manifest string) string {
	t.Helper()
	dir := filepath.Join(resolvedTestDir(t, t.TempDir()), "2026-11-12T00-00-00Z")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// checkWorkerEnd answers a channel closed once the worker of the look at dir
// before bridge restore's prompt has ended (SetOnCheckSourceEndForTest). It
// is installed before the look it hears, and its restore, registered here,
// runs after every cleanup the test registers later, a wait for that end
// among them.
func checkWorkerEnd(t *testing.T, dir string) <-chan struct{} {
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

// awaitClosed waits for c to close, failing the test past its wait
// (waitWithin).
func awaitClosed(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(waitWithin(t, 30*time.Second)):
		t.Fatalf("%s never came", what)
	}
}

// requirePlainStop fails the test unless err, what bridge restore's look at
// the snapshot given answered, is the stop of a look whose read answered:
// its context's cancel in its chain, the snapshot named as given, and nothing
// of a folder that did not answer or a path that holds no snapshot.
func requirePlainStop(t *testing.T, err error, given string) {
	t.Helper()
	if !errors.Is(err, context.Canceled) || errors.Is(err, backup.ErrSourceNotAnswering) || errors.Is(err, backup.ErrNotASnapshot) {
		t.Fatalf("the look answered %v, want a plain stop carrying context.Canceled alone", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "stopped before its prompt") || strings.Contains(msg, "did not answer") || !says(msg, given) {
		t.Errorf("the stop says %q, want it to say the restore stopped before its prompt, naming %s, and nothing of a folder that did not answer", msg, given)
	}
}

// TestARestoreStoppedAsItsLookAnswersIsAPlainStop is backlog B871's item 1:
// bridge restore's look at the snapshot before its prompt
// (backup.CheckRestoreSource) asks its stop whichever answer its wait took, so
// a Ctrl-C that lands while the look's read is under way, the read answering
// within the stop's grace, refuses with a plain stop, its context's cancel in
// the chain, never the look's answer, which led to the prompt, and never the
// refusal of a folder that did not answer, since nothing was left reading. The
// stop and the worker's answer can be ready together, and the wait's select
// may take the worker's: the second way makes them so, before the wait, and
// takes either at random, so it is shown under -count. Past the grace the
// refusal is the one of a folder that did not answer (ErrSourceNotAnswering).
func TestARestoreStoppedAsItsLookAnswersIsAPlainStop(t *testing.T) {
	for _, how := range []string{"its read answering after the stop", "its answer and the stop ready together", "its read not answering within the grace"} {
		t.Run(how, func(t *testing.T) {
			snap := fakeSnapshot(t, fmt.Sprintf(`{"schemaVersion":%d}`, backup.SchemaVersion))
			manifest := filepath.Join(snap, backup.ManifestFile)
			ended := checkWorkerEnd(t, snap)
			p := newParking()
			var armed atomic.Bool
			t.Cleanup(backup.SetReadSourceManifestForTest(func(path string) ([]byte, error) {
				if path == manifest && armed.CompareAndSwap(true, false) {
					p.park()
				}
				return os.ReadFile(path)
			}))
			ctx, cancel := context.WithCancel(context.Background())
			if how == "its answer and the stop ready together" {
				// The worker has answered, and the stop comes, before the wait
				// begins: both are ready to it.
				t.Cleanup(backup.SetOnCheckAwaitForTest(func(d string) {
					if d == snap {
						select {
						case <-ended:
						case <-time.After(waitWithin(t, 30*time.Second)):
						}
						cancel()
					}
				}))
			}
			// The caller is joined first, then the look's worker, and only then
			// are the seams put back.
			var started atomic.Bool
			t.Cleanup(func() {
				if started.Load() {
					awaitClosed(t, ended, "the end of the look's worker")
				}
			})
			callers := joinBackupCallers(t, func() { cancel(); p.release() })

			if how != "its answer and the stop ready together" {
				armed.Store(true)
			}
			done := make(chan error, 1)
			started.Store(true)
			callers.start(func() { done <- backup.CheckRestoreSource(ctx, snap, 20*time.Second) })
			if how != "its answer and the stop ready together" {
				awaitEntered(t, p.entered, "the look's read of the manifest")
				cancel()
			}
			if how == "its read answering after the stop" {
				p.release()
			}
			err := requireReturnedWithin(t, done, time.Now(), backup.StopWriteBound+2*time.Second, "the look stopped")
			if how == "its read not answering within the grace" {
				if !errors.Is(err, backup.ErrSourceNotAnswering) || !errors.Is(err, context.Canceled) || err == nil || !strings.Contains(err.Error(), "did not answer within") {
					t.Errorf("the look whose read did not answer within the grace answered %v, want the refusal of a folder that did not answer, carrying its stop", err)
				}
				return
			}
			requirePlainStop(t, err, snap)
		})
	}
}

// TestTheLookBeforeThePromptRefusesASchemaThisBridgeDoesNotTake is backlog
// B871's item 2: bridge restore's look at the snapshot before its prompt
// refuses a snapshot of a schema newer than this bridge takes, in the words
// the restore refuses it in once confirmed, never as a path that holds no
// snapshot (ErrNotASnapshot, bridge restore's exit 2); the schemas this bridge
// takes, 1 and SchemaVersion, pass. Until then the look took any schema above
// 0 (round 1's LooksLikeSnapshotDir, whose doc wanted the precise refusal
// after the prompt), and the newer one was refused only once confirmed.
func TestTheLookBeforeThePromptRefusesASchemaThisBridgeDoesNotTake(t *testing.T) {
	for _, v := range []int{1, backup.SchemaVersion, backup.SchemaVersion + 1} {
		t.Run(fmt.Sprintf("schema %d", v), func(t *testing.T) {
			snap := fakeSnapshot(t, fmt.Sprintf(`{"schemaVersion":%d}`, v))
			ended := checkWorkerEnd(t, snap)
			err := backup.CheckRestoreSource(context.Background(), snap, 20*time.Second)
			awaitClosed(t, ended, "the end of the look's worker")
			if v <= backup.SchemaVersion {
				if err != nil {
					t.Errorf("the look at a snapshot of schema %d answered %v, want nil", v, err)
				}
				return
			}
			want := fmt.Sprintf("snapshot schema v%d not supported (this bridge supports v%d)", v, backup.SchemaVersion)
			if err == nil || errors.Is(err, backup.ErrNotASnapshot) || !strings.Contains(err.Error(), want) {
				t.Errorf("the look at a snapshot of schema %d answered %v, want %q and never ErrNotASnapshot", v, err, want)
			}
		})
	}
}

// TestTheLookBeforeThePromptKeepsTheCauseOfAReadThatFailed is backlog B871's
// item 4: bridge restore's look at the snapshot before its prompt refuses as
// a path that holds no snapshot (ErrNotASnapshot, bridge restore's exit 2)
// only what is invalid input, nothing there (a missing path, a link to
// nothing, at the snapshot's lookup or at its manifest), something that is no
// folder where one goes (a regular file given, or in the way), or a manifest
// that does not decode or holds no schema; a lookup or a read of the manifest
// that this user may not make, or that fails in I/O, keeps its cause, the
// snapshot named as given, and is no ErrNotASnapshot. The failures are
// injected through the look's own seams, never by a mode, which root and
// Windows do not honour. Until then every failure of either read was
// ErrNotASnapshot, its cause dropped by bridge restore's words.
func TestTheLookBeforeThePromptKeepsTheCauseOfAReadThatFailed(t *testing.T) {
	type look struct {
		given string
		// failLookup and failRead, set, make the snapshot's lookup or the
		// manifest's read fail with them.
		failLookup, failRead error
	}
	link := func(t *testing.T, to, at string) {
		t.Helper()
		if err := os.Symlink(to, at); err != nil {
			t.Skipf("no symbolic link can be made here: %v", err)
		}
	}
	for _, tc := range []struct {
		name  string
		stand func(t *testing.T) look
		// cause, nil for a path that holds no snapshot, is what the
		// refusal carries in its place.
		cause error
	}{
		{"a missing path", func(t *testing.T) look {
			return look{given: filepath.Join(resolvedTestDir(t, t.TempDir()), "missing")}
		}, nil},
		{"a link to nothing", func(t *testing.T) look {
			dir := resolvedTestDir(t, t.TempDir())
			link(t, filepath.Join(dir, "gone"), filepath.Join(dir, "snapshot"))
			return look{given: filepath.Join(dir, "snapshot")}
		}, nil},
		{"a path through a regular file", func(t *testing.T) look {
			file := filepath.Join(resolvedTestDir(t, t.TempDir()), "snapshot.tar")
			if err := os.WriteFile(file, []byte("not a folder"), 0o600); err != nil {
				t.Fatal(err)
			}
			return look{given: filepath.Join(file, "2026-11-12T00-00-00Z")}
		}, nil},
		{"a regular file given", func(t *testing.T) look {
			file := filepath.Join(resolvedTestDir(t, t.TempDir()), "snapshot.tar")
			if err := os.WriteFile(file, []byte("not a folder"), 0o600); err != nil {
				t.Fatal(err)
			}
			return look{given: file}
		}, nil},
		{"a folder with no manifest", func(t *testing.T) look {
			snap := fakeSnapshot(t, "{}")
			if err := os.Remove(filepath.Join(snap, backup.ManifestFile)); err != nil {
				t.Fatal(err)
			}
			return look{given: snap}
		}, nil},
		{"a manifest that is a link to nothing", func(t *testing.T) look {
			snap := fakeSnapshot(t, "{}")
			if err := os.Remove(filepath.Join(snap, backup.ManifestFile)); err != nil {
				t.Fatal(err)
			}
			link(t, filepath.Join(snap, "gone.json"), filepath.Join(snap, backup.ManifestFile))
			return look{given: snap}
		}, nil},
		{"a manifest that does not decode", func(t *testing.T) look {
			return look{given: fakeSnapshot(t, "{not json")}
		}, nil},
		{"a manifest with no schema", func(t *testing.T) look {
			return look{given: fakeSnapshot(t, "{}")}
		}, nil},
		{"the snapshot's lookup refused", func(t *testing.T) look {
			return look{given: fakeSnapshot(t, `{"schemaVersion":2}`), failLookup: syscall.EACCES}
		}, syscall.EACCES},
		{"the snapshot's lookup failing in I/O", func(t *testing.T) look {
			return look{given: fakeSnapshot(t, `{"schemaVersion":2}`), failLookup: syscall.EIO}
		}, syscall.EIO},
		{"the manifest's read refused", func(t *testing.T) look {
			return look{given: fakeSnapshot(t, `{"schemaVersion":2}`), failRead: syscall.EACCES}
		}, syscall.EACCES},
		{"the manifest's read failing in I/O", func(t *testing.T) look {
			return look{given: fakeSnapshot(t, `{"schemaVersion":2}`), failRead: syscall.EIO}
		}, syscall.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := tc.stand(t)
			ended := checkWorkerEnd(t, l.given)
			if l.failLookup != nil {
				t.Cleanup(backup.SetMetadataForTest(func(p string) (string, error) {
					if p == l.given {
						return "", &fs.PathError{Op: "lstat", Path: p, Err: l.failLookup}
					}
					return fsutil.ResolveLinks(p)
				}, nil))
			}
			if l.failRead != nil {
				manifest := filepath.Join(l.given, backup.ManifestFile)
				t.Cleanup(backup.SetReadSourceManifestForTest(func(path string) ([]byte, error) {
					if path == manifest {
						return nil, &fs.PathError{Op: "open", Path: path, Err: l.failRead}
					}
					return os.ReadFile(path)
				}))
			}
			err := backup.CheckRestoreSource(context.Background(), l.given, 20*time.Second)
			awaitClosed(t, ended, "the end of the look's worker")
			if tc.cause == nil {
				if !errors.Is(err, backup.ErrNotASnapshot) {
					t.Errorf("the look at %s answered %v, want ErrNotASnapshot", tc.name, err)
				}
				return
			}
			if err == nil || errors.Is(err, backup.ErrNotASnapshot) || !errors.Is(err, tc.cause) {
				t.Fatalf("the look whose %s answered %v, want its cause, %v, and no ErrNotASnapshot", tc.name, err, tc.cause)
			}
			msg := err.Error()
			if !says(msg, l.given) || !strings.Contains(msg, tc.cause.Error()) || !strings.Contains(msg, "nothing was restored") {
				t.Errorf("the look whose %s says %q, want it to name %s as given, its cause's words (%q), and that nothing was restored",
					tc.name, msg, l.given, tc.cause.Error())
			}
		})
	}
}
