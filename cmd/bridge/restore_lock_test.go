package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// b261HolderEnv names the data dir a re-exec'd test process holds the
// serve lock on, until the parent kills it.
const b261HolderEnv = "BRIDGE_B261_HOLD_DATA_DIR"

// seedRestorableInstall writes a one-track install and a snapshot of it.
// adminAddr is the address the write gate probes.
func seedRestorableInstall(t *testing.T, adminAddr string) (cfgPath, dataDir, snap string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir = filepath.Join(dir, "data")
	cfgPath = filepath.Join(dir, "bridge.yaml")
	body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\nlistenAddress: 127.0.0.1:0\nadminAddress: %s\ndisableHttp3: true\nmdns:\n    enabled: false\n",
		lib, dataDir, adminAddr)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	const trackPath = "Artist/Album/Song.flac"
	if err := store.UpsertTrack(ctx, &manifest.Track{
		Path: trackPath, Size: 1, ModTime: time.Unix(0, 0).UTC(), Title: "Song",
	}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{trackPath}, 99); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	snap, err = backup.Snapshot(ctx, backup.Sources{
		DataDir:    dataDir,
		ManifestDB: manifest.DefaultDBPath(dataDir),
		BridgeYAML: cfgPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataDir, snap
}

func closedAdminAddr(t *testing.T) string {
	t.Helper()
	return "127.0.0.1:" + closedLoopbackPort(t)
}

// squatAdminPort stands in for the parallel test that took a serve test's
// admin port in CI (backlog B562: run 40565409736, "admin listen
// 127.0.0.1:35347: bind: address already in use"). It binds addr if it can
// and holds it to the end of the test, so a test that leaves its admin port
// free between choosing it and serve's bind fails every run, as CI's did
// once. A port the test holds (holdLoopback) cannot be bound, and the
// squatter takes nothing.
func squatAdminPort(t *testing.T, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return
	}
	t.Cleanup(func() { _ = ln.Close() })
	t.Errorf("the admin port %s was free between the test's choice and serve's bind: a parallel test could take it", addr)
}

// holdDataDirLock takes the serve lock and records this process in
// server.pid, so a refusal can name the holder the lock itself records.
func holdDataDirLock(t *testing.T, dataDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dataDir, serverPIDFileName), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := lockServeDataDir(dataDir)
	if err != nil {
		t.Fatalf("take the data dir lock: %v", err)
	}
	t.Cleanup(release)
}

// blockDataDirLock puts a directory where the lock file goes, the shape
// serve already uses for "the lock cannot be taken".
func blockDataDirLock(t *testing.T, dataDir string) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dataDir, serveLockFileName), 0o700); err != nil {
		t.Fatal(err)
	}
}

func runRestoreYes(t *testing.T, cfgPath, snap string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := restoreCmd(context.Background(), []string{"--config", cfgPath, "--yes", snap}, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func runClearMissingYes(t *testing.T, cfgPath string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := manifestClearMissingCmd(context.Background(), []string{"--config", cfgPath, "--yes"}, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestAHeldDataDirLockRefusesRestoreAndClearMissing(t *testing.T) {
	cfgPath, dataDir, snap := seedRestorableInstall(t, closedAdminAddr(t))
	holdDataDirLock(t, dataDir)
	pid := strconv.Itoa(os.Getpid())

	code, _, stderr := runRestoreYes(t, cfgPath, snap)
	if code == 0 {
		t.Errorf("restore proceeded while the data dir lock was held\nstderr:\n%s", stderr)
	} else if !strings.Contains(stderr, "already running") || !strings.Contains(stderr, dataDir) || !strings.Contains(stderr, pid) {
		t.Errorf("restore refusal does not name the holder the lock records (pid %s, %s)\nstderr:\n%s", pid, dataDir, stderr)
	} else if strings.Contains(stderr, "a bridge is answering") {
		t.Errorf("restore fell through to the admin-port probe\nstderr:\n%s", stderr)
	}

	code, _, stderr = runClearMissingYes(t, cfgPath)
	if code == 0 {
		t.Fatalf("clear-missing proceeded while the data dir lock was held\nstderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "already running") || !strings.Contains(stderr, dataDir) || !strings.Contains(stderr, pid) {
		t.Fatalf("clear-missing refusal does not name the holder the lock records (pid %s, %s)\nstderr:\n%s", pid, dataDir, stderr)
	}
	if strings.Contains(stderr, "a bridge is answering") {
		t.Fatalf("clear-missing fell through to the admin-port probe\nstderr:\n%s", stderr)
	}
	if !trackExists(t, filepath.Dir(dataDir), "Artist/Album/Song.flac") {
		t.Fatal("clear-missing purged the row while the lock was held")
	}
}

func TestRestoreHoldsTheDataDirLockThroughThePrompt(t *testing.T) {
	cfgPath, dataDir, snap := seedRestorableInstall(t, closedAdminAddr(t))
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	var stdout, stderr safeBuffer
	done := make(chan int, 1)
	go func() {
		done <- restoreCmd(context.Background(), []string{"--config", cfgPath, snap}, pr, &stdout, &stderr)
	}()
	waitForRestorePrompt(t, &stderr, done)

	release, err := lockServeDataDir(dataDir)
	if err == nil {
		release()
		_, _ = pw.Write([]byte("no\n"))
		<-done
		t.Fatal("the data dir lock was free while restore waited at the prompt")
	}
	var held *serveDataDirHeldError
	if !errors.As(err, &held) {
		_, _ = pw.Write([]byte("no\n"))
		<-done
		t.Fatalf("lock error while restore waits: %v", err)
	}

	code, _, secondErr := runRestoreYes(t, cfgPath, snap)
	if code == 0 || !strings.Contains(secondErr, "already running") || !strings.Contains(secondErr, dataDir) {
		t.Errorf("a second restore during the prompt exited %d\nstderr:\n%s", code, secondErr)
	}

	if _, err := pw.Write([]byte("no\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code == 0 {
			t.Errorf("the prompting restore completed after 'no'")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the prompting restore did not return")
	}
}

func TestAServeStartedWhileRestoreHoldsTheLockRefuses(t *testing.T) {
	cfgPath, dataDir, snap := seedRestorableInstall(t, closedAdminAddr(t))
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	var stdout, stderr safeBuffer
	done := make(chan int, 1)
	go func() {
		done <- restoreCmd(context.Background(), []string{"--config", cfgPath, snap}, pr, &stdout, &stderr)
	}()
	waitForRestorePrompt(t, &stderr, done)

	code, serveOut, serveErr := runSecondServe(t, "--config", cfgPath)
	if _, err := pw.Write([]byte("no\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the prompting restore did not return")
	}
	if code != 1 || !strings.Contains(serveErr, "already running") || !strings.Contains(serveErr, dataDir) {
		t.Fatalf("serve started while restore held the lock exited %d\nstdout:\n%s\nstderr:\n%s", code, serveOut, serveErr)
	}
	if strings.Contains(serveOut, "listening on") {
		t.Fatalf("serve bound listeners while restore held the lock\nstdout:\n%s", serveOut)
	}
}

func TestRestoreRequiresAnExclusiveDataDirLock(t *testing.T) {
	t.Run("answering admin", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)
		u, err := url.Parse(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		cfgPath, dataDir, snap := seedRestorableInstall(t, u.Host)
		blockDataDirLock(t, dataDir)
		code, _, stderr := runRestoreYes(t, cfgPath, snap)
		if code == 0 || !strings.Contains(stderr, "exclusive lock could not be taken") {
			t.Fatalf("restore did not require its lock, exit %d\nstderr:\n%s", code, stderr)
		}
		if strings.Contains(stderr, "already running") {
			t.Fatalf("a lock that could not be taken was reported as held\nstderr:\n%s", stderr)
		}
	})
	t.Run("nothing listening", func(t *testing.T) {
		cfgPath, dataDir, snap := seedRestorableInstall(t, closedAdminAddr(t))
		blockDataDirLock(t, dataDir)
		code, stdout, stderr := runRestoreYes(t, cfgPath, snap)
		if code == 0 || !strings.Contains(stderr, "exclusive lock could not be taken") || strings.Contains(stdout, "Restore complete") {
			t.Fatalf("restore ran without its lock, exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
	})
	t.Run("ephemeral admin port", func(t *testing.T) {
		cfgPath, dataDir, snap := seedRestorableInstall(t, "127.0.0.1:0")
		blockDataDirLock(t, dataDir)
		code, stdout, stderr := runRestoreYes(t, cfgPath, snap)
		if code == 0 || !strings.Contains(stderr, "exclusive lock could not be taken") || strings.Contains(stdout, "Restore complete") {
			t.Fatalf("restore ran without its lock, exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		code, _, stderr = runClearMissingYes(t, cfgPath)
		if code == 0 || !strings.Contains(stderr, "no fixed port") {
			t.Fatalf("fallback did not keep the ephemeral-port refusal, exit %d\nstderr:\n%s", code, stderr)
		}
		if strings.Contains(stderr, "a bridge is answering") {
			t.Fatalf("ephemeral fallback said a bridge was answering\nstderr:\n%s", stderr)
		}
	})
}

// TestARestoreOverAnUnlockedServeRefusesWithTheProbe is a serve whose
// lock failed for a reason other than contention: it keeps serving, and
// holds no lock. Once that block is gone a restore takes the lock. The
// admin port is fixed and the serve is answering on it, so the restore
// must still refuse with the probe's message.
//
// The admin port is held from the moment it is chosen and handed to serve
// (serveOpts.adminListener), never learned and released: a port free
// between the choice and serve's bind was taken by a parallel test in CI,
// and serve exited "address already in use" (backlog B562). The config on
// disk names that port, and the restore finds the serve through it as it
// would in production.
//
// The restore runs once serve's verification of the snapshot has ended
// (serveBesideItsSnapshot.restore, backlog B885): serve's boot finishes the
// check the snapshot owes in its place, and a read of the manifest beside
// its verdict's rename is refused on Windows.
func TestARestoreOverAnUnlockedServeRefusesWithTheProbe(t *testing.T) {
	b := startServeBesideItsSnapshot(t, nil)
	code, stdout, stderr := b.restore(t, nil)
	if code == 0 || !strings.Contains(stderr, "a bridge is answering on "+b.adminAddr) {
		t.Fatalf("restore wrote under an unlocked serve, exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

// errSharingViolation is Windows' refusal of an open of a file another handle
// holds without sharing it, in the words Windows gives it, which a test
// answers where the read beside a rename meets it.
var errSharingViolation = errors.New("The process cannot access the file because it is being used by another process.")

// serveBesideItsSnapshot is a bridge serve on an install beside a snapshot of
// its data dir taken before it started. It holds no data dir lock (a
// directory stood where its lock file goes, gone once serve answers), so a
// restore of the snapshot takes that lock and is refused by its probe of the
// admin port serve answers on.
type serveBesideItsSnapshot struct {
	cfgPath, snap, adminAddr string
	// checkEnded is closed once serve's verification of the snapshot has
	// ended, its worker's end included: serve's boot finishes the check the
	// snapshot owes in its place (decideStartupPass), and that check's
	// verdict is written over the snapshot's manifest by a rename.
	checkEnded <-chan struct{}
}

// startServeBesideItsSnapshot seeds the install and its snapshot; runs
// before, when set, with the snapshot where it resolves (the path the
// backup's hooks are given) and the channel serve's verification of it closes
// as it ends, for a test's own hooks; sets the hook that hears that end
// before serve starts (a verification runs the end hook that stood as it
// began, backlog B849's second round), which runs atEnd, what before
// returns, in the verification worker's last step, once it has let go of
// everything it held and before it closes the channel; and starts serve.
func startServeBesideItsSnapshot(t *testing.T, before func(snap string, checkEnded <-chan struct{}) (atEnd func())) serveBesideItsSnapshot {
	t.Helper()
	admin := holdLoopback(t)
	cfgPath, dataDir, snap := seedRestorableInstall(t, admin.addr)
	resolved, err := fsutil.ResolveLinks(snap)
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	var atEnd func()
	if before != nil {
		atEnd = before(resolved, ended)
	}
	var endOnce sync.Once
	t.Cleanup(backup.SetOnVerifyEndForTest(func(dir string) {
		if dir != resolved {
			return
		}
		if atEnd != nil {
			atEnd()
		}
		endOnce.Do(func() { close(ended) })
	}))
	blockDataDirLock(t, dataDir)
	squatAdminPort(t, admin.addr)
	served := launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) int {
		return runServe(ctx, serveOpts{configPath: cfgPath, adminListener: admin.ln}, stdout, stderr)
	})
	if s := served.stderr.String(); !strings.Contains(s, "could not lock") {
		t.Fatalf("serve did not start unlocked\nstderr:\n%s", s)
	}
	waitForAdminReady(t, admin.addr, served.done, served.stderr)
	if err := os.Remove(filepath.Join(dataDir, serveLockFileName)); err != nil {
		t.Fatal(err)
	}
	release, err := lockServeDataDir(dataDir)
	if err != nil {
		t.Fatalf("the lock was not takeable once the block was gone: %v", err)
	}
	release()
	return serveBesideItsSnapshot{cfgPath: cfgPath, snap: snap, adminAddr: admin.addr, checkEnded: ended}
}

// restore is bridge restore --yes of the snapshot beside serve, once serve's
// verification of it has ended (awaitItsCheck; waiting runs as that wait
// begins).
func (b serveBesideItsSnapshot) restore(t *testing.T, waiting func()) (code int, stdout, stderr string) {
	t.Helper()
	b.awaitItsCheck(t, waiting)
	return runRestoreYes(t, b.cfgPath, b.snap)
}

// awaitItsCheck waits for serve's verification of the snapshot to end, until
// the test's deadline (backlog B885): its verdict is written over the
// snapshot's manifest by a rename, and the restore reads that manifest before
// its prompt (backup.CheckRestoreSource) holding no lock, which on Windows is
// refused beside the rename ("being used by another process"), exit 2: not a
// recognized snapshot. waiting, when set, runs as the wait begins.
func (b serveBesideItsSnapshot) awaitItsCheck(t *testing.T, waiting func()) {
	t.Helper()
	if waiting != nil {
		waiting()
	}
	select {
	case <-b.checkEnded:
	case <-serveGiveUp(t):
		t.Fatal("serve's verification of its snapshot did not end before the test's deadline")
	}
}

// TestTheRestoreBesideAServeRunsOnceItsCheckOfTheSnapshotHasEnded is backlog
// B885's order, held where it matters, in two stages. First serve's
// verification of the snapshot is parked after its check and before its
// verdict's write (backup.SetAfterCheckForTest), and once it is parked the
// test lets it go, with no restore under way: the park clears and the verdict
// is written. Then the verification's end, its worker's last step
// (backup.SetOnVerifyEndForTest), is held until the restore begins its wait
// for that end, or until the restore's read of the snapshot's manifest before
// its prompt comes (backup.SetRestorePreflightReadForTest), whichever is
// first, and that read must find the verification ended. The restore waits
// for the end, so its wait lets the end go, the verification ends, and the
// read follows it; the restore is refused, as ever, by the probe of the
// bridge answering on the admin port. Its control is the wait dropped: the
// end is then held until the read comes, which finds the verification not
// ended, every run, by the barrier and not the scheduler, and the case fails
// there. The read answers Windows' refusal while the check is parked, and
// reads the file otherwise. An ordering regression test, not a reproduction
// of windows-latest's run 43416311381.
func TestTheRestoreBesideAServeRunsOnceItsCheckOfTheSnapshotHasEnded(t *testing.T) {
	var parked, overran atomic.Bool
	var entered, injected, early atomic.Int32
	parkedAt, unparked := newGate(), newGate()
	checkGo, endGo := newGate(), newGate()
	bound := serveGiveUpTime(t)
	// hold waits for g on the verification's worker, until the test's
	// deadline, saying an overrun to the test's goroutine.
	hold := func(g *gate) {
		select {
		case <-g.ch:
		case <-firesAt(bound):
			overran.Store(true)
		}
	}
	b := startServeBesideItsSnapshot(t, func(snap string, checkEnded <-chan struct{}) func() {
		manifestPath := filepath.Join(snap, backup.ManifestFile)
		t.Cleanup(backup.SetAfterCheckForTest(func(dir string) {
			if dir != snap {
				return
			}
			parked.Store(true)
			parkedAt.open()
			hold(checkGo)
			parked.Store(false)
			unparked.open()
		}))
		t.Cleanup(backup.SetRestorePreflightReadForTest(func(path string, read func(string) (backup.Manifest, error)) (backup.Manifest, error) {
			if path != manifestPath {
				return read(path)
			}
			entered.Add(1)
			select {
			case <-checkEnded:
			default:
				// The read came before the verification ended: the order
				// this case pins, broken. The end held for it goes now.
				early.Add(1)
				endGo.open()
			}
			if parked.Load() {
				injected.Add(1)
				return backup.Manifest{}, &fs.PathError{Op: "open", Path: path, Err: errSharingViolation}
			}
			return read(path)
		}))
		// Registered after the seams, so it runs before they are put back,
		// and after serve's drain: both holds let go and the verification's
		// worker joined.
		t.Cleanup(func() {
			checkGo.open()
			endGo.open()
			select {
			case <-checkEnded:
			case <-serveDrainGiveUp(t):
				t.Error("serve's verification of its snapshot did not end once its holds were let go")
			}
		})
		return func() { hold(endGo) }
	})
	// Both let go before serve's drain too, so serve stops with nothing held.
	t.Cleanup(func() { checkGo.open(); endGo.open() })

	// The first stage: the check parked, then let go with no restore under
	// way; the park clears, the verdict is written, and nothing is injected.
	select {
	case <-parkedAt.ch:
	case <-serveGiveUp(t):
		t.Fatal("serve's verification of its snapshot never came to its verdict's write")
	}
	checkGo.open()
	select {
	case <-unparked.ch:
	case <-serveGiveUp(t):
		t.Fatal("serve's verification of its snapshot did not go on once its check was let go")
	}
	// The second: the end held until the restore's wait for it begins
	// (endGo.open, as the wait begins) or its read comes.
	code, stdout, stderr := b.restore(t, endGo.open)
	if n := entered.Load(); n < 1 {
		t.Fatalf("the restore never read the snapshot's manifest before its prompt through the seam: exit %d\nstderr:\n%s", code, stderr)
	}
	if n := early.Load(); n != 0 {
		t.Errorf("the restore read the snapshot's manifest %d time(s) before serve's verification of it had ended, "+
			"the read Windows refuses beside that verification's verdict rename", n)
	}
	if n := injected.Load(); n != 0 {
		t.Errorf("the restore read the snapshot's manifest %d time(s) while serve's verification of it was parked "+
			"before its verdict's write", n)
	}
	if code == 0 || !strings.Contains(stderr, "a bridge is answering on "+b.adminAddr) {
		t.Errorf("the restore beside serve = %d, want refused by its probe of the bridge on %s\nstdout:\n%s\nstderr:\n%s",
			code, b.adminAddr, stdout, stderr)
	}
	if overran.Load() {
		t.Error("serve's verification stayed held until the test's deadline")
	}
}

func TestAFreeLockLetsRestoreProceedOnAnEphemeralAdminPort(t *testing.T) {
	cfgPath, _, snap := seedRestorableInstall(t, "127.0.0.1:0")
	code, stdout, stderr := runRestoreYes(t, cfgPath, snap)
	if code != 0 || !strings.Contains(stdout, "Restore complete") {
		t.Fatalf("restore refused a free data dir whose admin port is ephemeral, exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if strings.Contains(stderr, "no fixed port") || strings.Contains(stderr, "a bridge is answering") {
		t.Fatalf("restore probed the admin port while it held the lock\nstderr:\n%s", stderr)
	}
}

func TestAKilledDataDirHolderDoesNotBlockRestore(t *testing.T) {
	if dir := os.Getenv(b261HolderEnv); dir != "" {
		release, err := lockServeDataDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hold: %v\n", err)
			os.Exit(2)
		}
		defer release()
		os.Stdout.WriteString("holding\n")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}

	cfgPath, dataDir, snap := seedRestorableInstall(t, closedAdminAddr(t))
	cmd := exec.Command(os.Args[0], "-test.run=^TestAKilledDataDirHolderDoesNotBlockRestore$")
	cmd.Env = append(os.Environ(), b261HolderEnv+"="+dataDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	holderErr := &strings.Builder{}
	cmd.Stderr = holderErr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("holder did not start: %v\nstderr:\n%s", err, holderErr.String())
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || !strings.Contains(line, "holding") {
		t.Fatalf("holder did not take the lock (%q, %v)\nstderr:\n%s", line, err, holderErr.String())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	deadline := time.Now().Add(5 * time.Second)
	var code int
	var out, stderr string
	for {
		code, out, stderr = runRestoreYes(t, cfgPath, snap)
		if code == 0 {
			break
		}
		if !strings.Contains(stderr, "already running") || time.Now().After(deadline) {
			t.Fatalf("a killed holder still blocked restore, exit %d\nstdout:\n%s\nstderr:\n%s", code, out, stderr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out, "Restore complete") {
		t.Fatalf("restore did not finish after the holder was killed\nstdout:\n%s", out)
	}
}

func waitForRestorePrompt(t *testing.T, stderr *safeBuffer, done <-chan int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for time.Now().Before(deadline) {
		if strings.Contains(stderr.String(), "Type 'yes' to continue:") {
			return
		}
		select {
		case code := <-done:
			t.Fatalf("restore exited %d before the prompt\nstderr:\n%s", code, stderr.String())
		case <-tick.C:
		}
	}
	t.Fatalf("restore never prompted\nstderr:\n%s", stderr.String())
}
