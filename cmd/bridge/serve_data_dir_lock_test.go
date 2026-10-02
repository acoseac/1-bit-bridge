package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	for _, c := range []struct {
		name     string
		ownPorts bool
	}{
		{"on the live bridge's ports", false},
		{"on ports of its own", true},
	} {
		t.Run(c.name, func(t *testing.T) {
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

			// A device paired with the live bridge, and a batch it is running.
			if out, errOut, code := runCapture(t, "pair", "--config", liveCfg, "--name", "Phone"); code != 0 {
				t.Fatalf("pair exited %d: %s%s", code, out, errOut)
			}
			batch := seedRunningBatch(t, dataDir)
			pidPath := filepath.Join(dataDir, serverPIDFileName)
			wantPID := strconv.Itoa(os.Getpid())
			if got := readTrimmed(t, pidPath); got != wantPID {
				t.Fatalf("the live bridge's %s holds %q, want %q", serverPIDFileName, got, wantPID)
			}
			tokensPath := filepath.Join(dataDir, tokensFileName)
			tokensBefore, err := os.Stat(tokensPath)
			if err != nil {
				t.Fatal(err)
			}

			secondCfg := liveCfg
			if c.ownPorts {
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
			if got := readTrimmed(t, pidPath); got != wantPID {
				t.Errorf("after the second serve, the live bridge's %s holds %q, want %q", serverPIDFileName, got, wantPID)
			}
			if got := batchStatus(t, dataDir, batch); got != "running" {
				t.Errorf("after the second serve, the live bridge's running batch is %q", got)
			}
			if tokensAfter, err := os.Stat(tokensPath); err != nil || !os.SameFile(tokensBefore, tokensAfter) {
				t.Errorf("the second serve rewrote %s (err %v)", tokensFileName, err)
			}
			if !waitForListen(adminAddr, 2*time.Second) {
				t.Errorf("the live bridge's console stopped answering after the second serve; stderr=%s", live.stderr.String())
			}
		})
	}
}

// TestServeFreesTheDataDirWhenItReturns stops a serve and starts another
// on the same data dir in this process, as the launcher menu does: the
// lock goes with the runServe that took it, not with the process.
func TestServeFreesTheDataDirWhenItReturns(t *testing.T) {
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
