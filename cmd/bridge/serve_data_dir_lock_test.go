package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestASecondServeOfALiveDataDirChangesNothing boots a bridge and then a
// second `bridge serve` of the same data dir, on the live bridge's own
// ports and on ports of its own, and requires the second to refuse before
// it changes anything the live bridge owns (backlog B208). Before the
// data dir lock the second ran every step of its wiring first: on the
// live bridge's ports it failed only at the bind, after its batch
// coordinator had marked the live bridge's running batches interrupted
// for good, and its exit removed the live bridge's server.pid (so `bridge
// doctor` then failed the live bridge's own ports) and rewrote
// tokens.json; on ports of its own it never failed at all, and two
// bridges served one database.
func TestASecondServeOfALiveDataDirChangesNothing(t *testing.T) {
	t.Run("on the live bridge's ports", func(t *testing.T) { requireASecondServeChangesNothing(t, false) })
	t.Run("on ports of its own", func(t *testing.T) { requireASecondServeChangesNothing(t, true) })
}

// requireASecondServeChangesNothing boots a live bridge with a paired
// device and a running batch, runs a second serve of its data dir (on the
// live bridge's ports, or on ports of its own), and requires the second to
// refuse and the live bridge's state to be as it was.
func requireASecondServeChangesNothing(t *testing.T, ownPorts bool) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "data")
	adminAddr := fmt.Sprintf("127.0.0.1:%d", freeLoopbackPort(t))
	liveCfg := writeServeConfig(t, filepath.Join(dir, "live.yaml"), lib, dataDir,
		freeLoopbackTCPAndUDPAddr(t), adminAddr)
	live := bootServe(t, "--config", liveCfg)
	waitForAdminReady(t, adminAddr, live.done, live.stderr)
	before := recordLiveBridge(t, liveCfg, dataDir)

	secondCfg := liveCfg
	if ownPorts {
		secondCfg = writeServeConfig(t, filepath.Join(dir, "second.yaml"), lib, dataDir,
			"127.0.0.1:0", "127.0.0.1:0")
	}
	code, stdout, stderr := runSecondServe(t, "--config", secondCfg)

	if code != 1 {
		t.Errorf("the second serve exited %d, want 1; stderr=%s", code, stderr)
	}
	if strings.Contains(stdout, "listening on") {
		t.Errorf("the second serve served the live bridge's data dir; stdout=%s", stdout)
	}
	if !strings.Contains(stderr, "already running") || !strings.Contains(stderr, dataDir) {
		t.Errorf("the second serve's refusal does not say another serve holds %s; stderr=%s", dataDir, stderr)
	}
	before.requireUnchanged(t)
	if !waitForListen(adminAddr, 2*time.Second) {
		t.Errorf("the live bridge's console stopped answering after the second serve; stderr=%s", live.stderr.String())
	}
}

// liveBridgeState is what a second serve of a live bridge's data dir must
// leave as it found it: the bridge's pid file, a batch it is running, and
// its token file, by identity (every writer of it renames a new file into
// place) and by content (a write in place would keep the identity).
type liveBridgeState struct {
	dataDir, pidPath, wantPID, tokensPath string
	batch                                 uuid.UUID
	tokens                                os.FileInfo
	tokenBytes                            []byte
}

// recordLiveBridge pairs a device with the bridge serving dataDir (so its
// token file exists), records a batch it is running, and returns that
// state with the pid file it found.
func recordLiveBridge(t *testing.T, cfgPath, dataDir string) liveBridgeState {
	t.Helper()
	if out, errOut, code := runCapture(t, "pair", "--config", cfgPath, "--name", "Phone"); code != 0 {
		t.Fatalf("pair exited %d: %s%s", code, out, errOut)
	}
	s := liveBridgeState{
		dataDir:    dataDir,
		pidPath:    filepath.Join(dataDir, serverPIDFileName),
		wantPID:    strconv.Itoa(os.Getpid()),
		tokensPath: filepath.Join(dataDir, tokensFileName),
		batch:      seedRunningBatch(t, dataDir),
	}
	if got := readTrimmed(t, s.pidPath); got != s.wantPID {
		t.Fatalf("the live bridge's %s holds %q, want %q", serverPIDFileName, got, s.wantPID)
	}
	var err error
	if s.tokens, err = os.Stat(s.tokensPath); err != nil {
		t.Fatal(err)
	}
	if s.tokenBytes, err = os.ReadFile(s.tokensPath); err != nil {
		t.Fatal(err)
	}
	return s
}

// requireUnchanged requires the live bridge's state to be as
// recordLiveBridge found it: the same pid file, the batch still running,
// and the same token file, not one rewritten in its place, holding the
// same bytes.
func (s liveBridgeState) requireUnchanged(t *testing.T) {
	t.Helper()
	if got := readTrimmed(t, s.pidPath); got != s.wantPID {
		t.Errorf("after the second serve, the live bridge's %s holds %q, want %q", serverPIDFileName, got, s.wantPID)
	}
	if got := batchStatus(t, s.dataDir, s.batch); got != "running" {
		t.Errorf("after the second serve, the live bridge's running batch is %q", got)
	}
	if after, err := os.Stat(s.tokensPath); err != nil || !os.SameFile(s.tokens, after) {
		t.Errorf("the second serve rewrote %s (err %v)", tokensFileName, err)
	}
	if after, err := os.ReadFile(s.tokensPath); err != nil || !bytes.Equal(s.tokenBytes, after) {
		t.Errorf("the second serve changed what %s holds (err %v)", tokensFileName, err)
	}
}

// TestServeFreesTheDataDirWhenItReturns stops a serve and starts another
// on the same data dir in this process, as the launcher menu does: the
// lock goes with the runServe that took it, not with the process.
//
// No automatic collection runs meanwhile. An *os.File nothing references
// is closed by its finalizer, which releases its lock, so a serve that
// dropped its lock file instead of releasing it at return would pass here
// whenever a collection happened to run first, and would let a second
// serve in while the first still served.
func TestServeFreesTheDataDirWhenItReturns(t *testing.T) {
	gcPercent := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gcPercent) })
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := writeServeConfig(t, filepath.Join(dir, "bridge.yaml"), lib, filepath.Join(dir, "data"),
		"127.0.0.1:0", "127.0.0.1:0")
	first := bootServe(t, "--config", cfg)
	first.stop()
	select {
	case code := <-first.done:
		if code != 0 {
			t.Fatalf("the first serve exited %d; stderr=%s", code, first.stderr.String())
		}
	case <-serveGiveUp(t):
		t.Fatalf("the first serve did not stop; stderr=%s\nserve's goroutines:\n%s", first.stderr.String(), serveStacks())
	}
	second := bootServe(t, "--config", cfg)
	if s := second.stderr.String(); strings.Contains(s, "already running") {
		t.Errorf("the second serve saw the first's lock: %s", s)
	}
}

// TestServeStartsWhereTheDataDirCannotBeLocked gives serve a data dir whose
// lock file cannot be opened (a directory stands where it goes) and
// requires serve to say so and serve all the same: only a lock another
// serve HOLDS refuses, and a filesystem that keeps no locks must not stop
// a bridge from starting.
func TestServeStartsWhereTheDataDirCannotBeLocked(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	dataDir := filepath.Join(dir, "data")
	for _, d := range []string{lib, filepath.Join(dataDir, serveLockFileName)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := writeServeConfig(t, filepath.Join(dir, "bridge.yaml"), lib, dataDir, "127.0.0.1:0", "127.0.0.1:0")
	served := bootServe(t, "--config", cfg)
	if s := served.stderr.String(); !strings.Contains(s, "could not lock") {
		t.Errorf("serve did not say it could not lock the data dir; stderr=%s", s)
	}
}

// TestServeLockFileThatLinksOutOfTheDataDirIsNotFollowed plants the lock
// file as a link to a file outside the data dir, and requires
// lockServeDataDir to refuse it (an error that is not "held", so serve
// says so and serves) rather than create or lock the file the link names:
// a serve run as root would do either as root.
func TestServeLockFileThatLinksOutOfTheDataDirIsNotFollowed(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside")
	if err := os.Symlink(outside, filepath.Join(dataDir, serveLockFileName)); err != nil {
		t.Skipf("this host makes no symlink here: %v", err)
	}
	release, err := lockServeDataDir(dataDir)
	release()
	var held *serveDataDirHeldError
	if err == nil || errors.As(err, &held) {
		t.Errorf("lockServeDataDir over a link out of the data dir = %v, want an error that is not a held lock", err)
	}
	if _, statErr := os.Lstat(outside); !os.IsNotExist(statErr) {
		t.Errorf("the file the link names was created (lstat: %v)", statErr)
	}
}

// writeServeConfig writes a loopback config serving lib from dataDir on
// the API and console addresses given, with no Bonjour advertisement on
// the LAN, and returns its path.
func writeServeConfig(t *testing.T, path, lib, dataDir, listenAddr, adminAddr string) string {
	t.Helper()
	body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\nlistenAddress: %s\nadminAddress: %s\n"+
		"disableHttp3: true\nmdns:\n    enabled: false\n",
		lib, dataDir, listenAddr, adminAddr)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runSecondServe runs `bridge serve` with args on a goroutine and returns
// its exit code and output once it has exited. A serve that starts
// serving instead is reported, stopped and waited for, so the caller's
// assertions about what it changed run after its teardown as well.
func runSecondServe(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out, errOut := &safeBuffer{}, &safeBuffer{}
	exitCode := make(chan int, 1)
	exited := make(chan struct{})
	go func(stdout, stderr io.Writer) {
		defer close(exited)
		exitCode <- run(ctx, append([]string{"serve"}, args...), stdout, stderr)
	}(out, errOut)
	drainServeOnCleanup(t, cancel, exited, exitCode, errOut)
	giveUp := serveGiveUp(t)
	for {
		select {
		case <-exited:
			return <-exitCode, out.String(), errOut.String()
		case <-giveUp:
			t.Fatalf("the second serve neither exited nor served before the test's deadline; stderr=%s\nserve's goroutines:\n%s",
				errOut.String(), serveStacks())
		case <-time.After(25 * time.Millisecond):
		}
		if strings.Contains(out.String(), "listening on") {
			// It is serving: stop it, so what it changes on the way out
			// is changed before the caller looks.
			cancel()
			select {
			case <-exited:
			case <-serveDrainGiveUp(t):
				t.Fatalf("the second serve did not stop before the test's deadline; stderr=%s", errOut.String())
			}
			return <-exitCode, out.String(), errOut.String()
		}
	}
}

// seedRunningBatch records a batch the live bridge is running, through a
// store of the test's own on the live bridge's database, and returns its
// id.
func seedRunningBatch(t *testing.T, dataDir string) uuid.UUID {
	t.Helper()
	st, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := uuid.New()
	now := time.Now().UnixNano()
	if err := st.InsertUpscaleBatch(context.Background(), manifest.UpscaleBatchRow{
		ID: id, TargetRate: 176400, TargetBits: 24, Kind: "upscale", Status: "running",
		TotalFiles: 3, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// batchStatus reads the status of batch id from dataDir's database.
func batchStatus(t *testing.T, dataDir string, id uuid.UUID) string {
	t.Helper()
	st, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.ListUpscaleBatches(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r.Status
		}
	}
	t.Fatalf("batch %s is not in the database", id)
	return ""
}

// readTrimmed reads path, space trimmed, or "" when it cannot be read.
func readTrimmed(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
