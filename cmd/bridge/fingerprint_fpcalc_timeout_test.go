package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// fpcalcStandInEnv re-execs the test binary as fpcalc. TestMain checks it
// before the suite starts. The child inherits it through acoustid.Compute's
// environment, which is the process environment plus a pinned locale.
const fpcalcStandInEnv = "BRIDGE_FPCALC_STAND_IN"

const (
	fpcalcStandInPID  = "FPCALC_STANDIN_PID"
	fpcalcStandInNext = "FPCALC_STANDIN_NEXT"
)

// runFpcalcStandIn is the fpcalc a hung-file test puts on PATH. A file named
// hang.flac writes its pid and never returns. next.flac writes a marker and
// prints a fingerprint the gate refuses, so the lookup is never reached.
func runFpcalcStandIn() int {
	path := ""
	if len(os.Args) > 0 {
		path = os.Args[len(os.Args)-1]
	}
	switch filepath.Base(path) {
	case "hang.flac":
		if pidPath := os.Getenv(fpcalcStandInPID); pidPath != "" {
			_ = os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644)
		}
		select {}
		return 0
	case "next.flac":
		if next := os.Getenv(fpcalcStandInNext); next != "" {
			_ = os.WriteFile(next, []byte("ran"), 0o644)
		}
		fmt.Println(`{"duration":1,"fingerprint":"AAAA"}`)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "fpcalc stand-in: unexpected path %q\n", path)
		return 2
	}
}

// installFpcalcStandIn puts a copy of this test binary on PATH under the
// name fpcalc, and nothing else, so LookPath cannot find a real fpcalc.
func installFpcalcStandIn(t *testing.T, pidFile, nextFile string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	name := "fpcalc"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(bin, name)
	// A hard link of the running binary cannot be removed while the test
	// binary is mapped, which fails TempDir cleanup on Windows. Copy there,
	// and wherever the link itself fails.
	if runtime.GOOS == "windows" || os.Link(exe, dst) != nil {
		copyFpcalcStandIn(t, exe, dst)
	}
	t.Setenv("PATH", bin)
	t.Setenv(fpcalcStandInEnv, "1")
	t.Setenv(fpcalcStandInPID, pidFile)
	t.Setenv(fpcalcStandInNext, nextFile)
}

func copyFpcalcStandIn(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestFpcalcBudgetDefaultsToTenMinutes pins the production bound. A test
// overrides decodeTimeout; a zero field is the ten minutes.
func TestFpcalcBudgetDefaultsToTenMinutes(t *testing.T) {
	s := &fingerprintSweeper{}
	if fingerprintDecodeTimeout != 10*time.Minute {
		t.Fatalf("fingerprintDecodeTimeout = %s, want 10m", fingerprintDecodeTimeout)
	}
	if got := s.fpcalcBudget(); got != fingerprintDecodeTimeout {
		t.Fatalf("budget = %s, want %s", got, fingerprintDecodeTimeout)
	}
	s.decodeTimeout = 200 * time.Millisecond
	if got := s.fpcalcBudget(); got != 200*time.Millisecond {
		t.Fatalf("override = %s, want 200ms", got)
	}
}

// TestAHungFpcalcDoesNotHoldTheSweep runs the real sweeper with compute left
// nil, so fingerprintOne calls acoustid.Compute. The first file's fpcalc
// never returns. The sweep must finish, the child must be gone, the next
// file must have been fingerprinted, and the hung file must not be cached
// as a miss.
func TestAHungFpcalcDoesNotHoldTheSweep(t *testing.T) {
	rec := loggingtest.Record(t)
	dir := t.TempDir()
	hangAbs := filepath.Join(dir, "hang.flac")
	nextAbs := filepath.Join(dir, "next.flac")
	if err := os.WriteFile(hangAbs, []byte("hang"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nextAbs, []byte("next"), 0o644); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "pid")
	nextFile := filepath.Join(dir, "next-ran")
	installFpcalcStandIn(t, pidFile, nextFile)

	const rel = "Album/hang.flac"
	hangKey := acoustid.Key{Path: rel, Size: 1, MTimeNS: 1}
	s := &fingerprintSweeper{
		cache:         acoustid.NewCache(8),
		workers:       1,
		length:        time.Second,
		decodeTimeout: 200 * time.Millisecond,
	}
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		s.fingerprintAll(ctx, []candidate{
			{path: rel, absPath: hangAbs, size: 1, mtimeNS: 1},
			{path: "Album/next.flac", absPath: nextAbs, size: 2, mtimeNS: 2},
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		killStandIn(pidFile)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		t.Fatal("one hung fpcalc held the sweep past 5s")
	}
	if ctx.Err() != nil {
		t.Fatalf("sweep context: %v", ctx.Err())
	}
	if _, err := os.Stat(nextFile); err != nil {
		t.Fatalf("the file after the hang was not fingerprinted: %v", err)
	}
	pid := readStandInPID(t, pidFile)
	if gone, how := standInExited(pid); !gone {
		t.Fatalf("fpcalc %d still running (%s)", pid, how)
	}
	timedOut := rec.Failures(msgFpcalcTimedOut)
	if len(timedOut) != 1 || !strings.Contains(timedOut[0], rel) {
		t.Fatalf("timeout lines = %q, want one naming %s", timedOut, rel)
	}
	if dec := rec.Failures("fingerprint: decode"); len(dec) != 0 {
		t.Fatalf("decode lines = %q, want none", dec)
	}
	for _, line := range rec.All() {
		if strings.Contains(line, hangAbs) {
			t.Fatalf("a log line names the absolute path: %s", line)
		}
	}
	if _, ok := s.cache.Get(hangKey); ok {
		t.Fatal("a timed-out file was cached as a miss")
	}
}

func readStandInPID(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		t.Fatalf("pid file %q: %v", b, err)
	}
	return pid
}

func killStandIn(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil || p == nil {
		return
	}
	_ = p.Kill()
}
