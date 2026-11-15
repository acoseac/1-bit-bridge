package manifest

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// The store's lifetime lock is the kernel's, so its tests hold it in a second
// process: this test binary run again as a helper (TestStoreLockHelperProcess),
// told what to do by its environment, saying what it did on its stdout, one
// line at a time, and waiting on its stdin until its parent closes it.
const (
	lockHelperModeEnv = "BRIDGE_STORE_LOCK_HELPER"
	lockHelperPathEnv = "BRIDGE_STORE_LOCK_PATH"
	// lockHelperReadOnlyEnv makes the helper's open of the lock file for
	// writing answer EACCES (a user who may read it alone).
	lockHelperReadOnlyEnv = "BRIDGE_STORE_LOCK_READONLY"
	// lockHelperRecordLocksEnv makes the helper's tries POSIX record locks,
	// as a Linux NFS mount carries flock out (recordLockSeams).
	lockHelperRecordLocksEnv = "BRIDGE_STORE_LOCK_RECORD"
)

// TestStoreLockHelperProcess is the helper the lock's tests run as a second
// process; it skips unless its environment names a mode:
//
//   - open: a real OpenStore; "ready"; then a "close" line closes it
//     ("closed"), and its stdin's end ends it.
//   - exclusive: HoldStoreExclusive; "ready" or "refused <err>"; held until
//     its stdin ends.
//   - shared: the opener's hold alone (holdStoreShared); "ready" or
//     "refused <err>"; held until its stdin ends.
//   - try-exclusive: HoldStoreExclusive once, "held" or "refused <err>",
//     let go at once.
func TestStoreLockHelperProcess(t *testing.T) {
	mode := os.Getenv(lockHelperModeEnv)
	if mode == "" {
		t.Skip("the store lock's helper process: run by its tests alone")
	}
	path := os.Getenv(lockHelperPathEnv)
	if os.Getenv(lockHelperRecordLocksEnv) != "" {
		restore := useRecordLocks()
		defer restore()
	}
	if os.Getenv(lockHelperReadOnlyEnv) != "" {
		restore := SetStoreLockOpenForTest(func(real func(dir, name, like string, write bool) (*os.File, error), dir, name, like string, write bool) (*os.File, error) {
			if write {
				return nil, &os.PathError{Op: "open", Path: filepath.Join(dir, name), Err: fs.ErrPermission}
			}
			f, err := real(dir, name, like, write)
			if err == nil {
				fmt.Println("opened for reading")
			}
			return f, err
		})
		defer restore()
	}
	in := bufio.NewScanner(os.Stdin)
	switch mode {
	case "open":
		s, err := OpenStore(path)
		if err != nil {
			fmt.Printf("refused %v\n", err)
			os.Exit(0)
		}
		fmt.Println("ready")
		for in.Scan() {
			if in.Text() == "close" {
				_ = s.Close()
				fmt.Println("closed")
			}
		}
	case "exclusive":
		h, err := HoldStoreExclusive(path)
		if err != nil {
			fmt.Printf("refused %v\n", err)
			os.Exit(0)
		}
		fmt.Println("ready")
		for in.Scan() {
		}
		h.Release()
	case "shared":
		release, err := holdStoreShared(path)
		if err != nil {
			fmt.Printf("refused %v\n", err)
			os.Exit(0)
		}
		fmt.Println("ready")
		for in.Scan() {
		}
		release()
	case "try-exclusive":
		h, err := HoldStoreExclusive(path)
		if err != nil {
			fmt.Printf("refused %v\n", err)
			os.Exit(0)
		}
		fmt.Println("held")
		h.Release()
	default:
		fmt.Printf("unknown mode %q\n", mode)
	}
	os.Exit(0)
}

// lockHelper is a helper process a test started.
type lockHelper struct {
	t     *testing.T
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string
	done  chan struct{}
}

// startLockHelper starts the helper in mode over the database at path, with
// env's NAME=VALUE pairs set too, and kills it at the test's end.
func startLockHelper(t *testing.T, mode, path string, env ...string) *lockHelper {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreLockHelperProcess$", "-test.timeout=2m")
	cmd.Env = append(os.Environ(), lockHelperModeEnv+"="+mode, lockHelperPathEnv+"="+path)
	cmd.Env = append(cmd.Env, env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &lockHelper{t: t, cmd: cmd, stdin: stdin, lines: make(chan string, 16), done: make(chan struct{})}
	go func() {
		defer close(h.done)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			h.lines <- sc.Text()
		}
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return h
}

// next is the helper's next line, within 30 s; "" where its output ended.
func (h *lockHelper) next() string {
	h.t.Helper()
	for {
		select {
		case l := <-h.lines:
			// The test binary's own lines (PASS, a skip, coverage) are not
			// the helper's.
			if l == "PASS" || strings.HasPrefix(l, "=== ") || strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "ok ") {
				continue
			}
			return l
		case <-h.done:
			select {
			case l := <-h.lines:
				return l
			default:
				return ""
			}
		case <-time.After(30 * time.Second):
			h.t.Fatalf("the helper said nothing for 30 s")
			return ""
		}
	}
}

// expect requires the helper's next line to begin with want.
func (h *lockHelper) expect(want string) string {
	h.t.Helper()
	got := h.next()
	if !strings.HasPrefix(got, want) {
		h.t.Fatalf("the helper said %q, want %q", got, want)
	}
	return got
}

// send writes one line to the helper.
func (h *lockHelper) send(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.stdin, line+"\n"); err != nil {
		h.t.Fatal(err)
	}
}

// stop ends the helper by its stdin's end and waits for it.
func (h *lockHelper) stop() {
	h.t.Helper()
	_ = h.stdin.Close()
	if err := h.cmd.Wait(); err != nil {
		h.t.Fatalf("the helper ended with %v", err)
	}
}

// kill kills the helper (SIGKILL; TerminateProcess on Windows) and waits for
// it: the kernel lets its locks go.
func (h *lockHelper) kill() {
	h.t.Helper()
	if err := h.cmd.Process.Kill(); err != nil {
		h.t.Fatal(err)
	}
	_ = h.cmd.Wait()
}

// otherProcessTries asks a helper process to take the store's lock at path
// exclusively, once, and answers what it said: "held", or "refused …".
func otherProcessTries(t *testing.T, path string, env ...string) string {
	t.Helper()
	h := startLockHelper(t, "try-exclusive", path, env...)
	got := h.next()
	h.stop()
	return got
}

// waitForOtherProcessToHold asks again until another process takes the lock
// at path exclusively, within 10 s.
func waitForOtherProcessToHold(t *testing.T, path string, env ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := otherProcessTries(t, path, env...)
		if got == "held" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("another process still cannot take the lock: %s", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// dirState is every entry of dir: its kind, and a regular file's size and
// SHA-256, so two states compare byte for byte.
func dirState(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.Mode().IsRegular() {
			out[e.Name()] = fi.Mode().Type().String()
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		out[e.Name()] = fmt.Sprintf("file %d %s", len(b), hex.EncodeToString(sum[:]))
	}
	return out
}

// requireNoDatabaseFiles requires that none of the database's files is at
// path: no bridge.db, -wal, -shm or -journal.
func requireNoDatabaseFiles(t *testing.T, path string) {
	t.Helper()
	for _, p := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s was made (%v)", filepath.Base(p), err)
		}
	}
}

// setStoreLockTry stands in for the lock's try, restored at the test's end.
func setStoreLockTry(t *testing.T, fn func(f *os.File, exclusive bool) error) {
	t.Helper()
	t.Cleanup(SetStoreLockTryForTest(fn))
}

// answering is a try that answers err, of the kind exclusive, or the
// platform's own for the other kind.
func answering(err error) func(*os.File, bool) error {
	return func(*os.File, bool) error { return err }
}

// realTry is the platform's own try.
func realTry(f *os.File, exclusive bool) error {
	if exclusive {
		return fsutil.TryLock(f)
	}
	return fsutil.TryLockShared(f)
}
