package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
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

func TestALockThatCannotBeTakenFallsBackToTheProbe(t *testing.T) {
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
		if code == 0 || !strings.Contains(stderr, "a bridge is answering on "+u.Host) {
			t.Fatalf("fallback did not keep the probe refusal, exit %d\nstderr:\n%s", code, stderr)
		}
		if strings.Contains(stderr, "already running") {
			t.Fatalf("a lock that could not be taken was reported as held\nstderr:\n%s", stderr)
		}
	})
	t.Run("nothing listening", func(t *testing.T) {
		cfgPath, dataDir, snap := seedRestorableInstall(t, closedAdminAddr(t))
		blockDataDirLock(t, dataDir)
		code, stdout, stderr := runRestoreYes(t, cfgPath, snap)
		if code != 0 || !strings.Contains(stdout, "Restore complete") {
			t.Fatalf("fallback refused a stopped bridge, exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
	})
	t.Run("ephemeral admin port", func(t *testing.T) {
		cfgPath, dataDir, _ := seedRestorableInstall(t, "127.0.0.1:0")
		blockDataDirLock(t, dataDir)
		code, _, stderr := runClearMissingYes(t, cfgPath)
		if code == 0 || !strings.Contains(stderr, "no fixed port") {
			t.Fatalf("fallback did not keep the ephemeral-port refusal, exit %d\nstderr:\n%s", code, stderr)
		}
		if strings.Contains(stderr, "a bridge is answering") {
			t.Fatalf("ephemeral fallback said a bridge was answering\nstderr:\n%s", stderr)
		}
	})
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
	cmd.Stderr = &strings.Builder{}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
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
		t.Fatalf("holder did not take the lock (%q, %v)", line, err)
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
	for time.Now().Before(deadline) {
		if strings.Contains(stderr.String(), "Type 'yes' to continue:") {
			return
		}
		select {
		case code := <-done:
			t.Fatalf("restore exited %d before the prompt\nstderr:\n%s", code, stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("restore never prompted\nstderr:\n%s", stderr.String())
}
