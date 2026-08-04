package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The name and location must match what doctor derives independently
// (cmd/bridge/doctor.go: filepath.Join(cfg.DataDir, "server.pid")). If
// the two drift, doctor reads a file nothing writes — which is exactly
// the state this fixes.
func TestServerPIDFilePathMatchesWhatDoctorReads(t *testing.T) {
	dir := t.TempDir()
	path, err := writeServerPIDFile(dir)
	if err != nil {
		t.Fatalf("writeServerPIDFile: %v", err)
	}
	if want := filepath.Join(dir, "server.pid"); path != want {
		t.Errorf("pidfile at %q, want %q — doctor derives that path "+
			"independently and the two must agree", path, want)
	}
}

// The written value must be this process's PID, parseable by doctor's
// readPID (a bare integer, no trailing newline required either way).
func TestServerPIDFileHoldsOurPID(t *testing.T) {
	dir := t.TempDir()
	path, err := writeServerPIDFile(dir)
	if err != nil {
		t.Fatalf("writeServerPIDFile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("pidfile %q does not parse as an integer: %v", raw, err)
	}
	if got != os.Getpid() {
		t.Errorf("pidfile holds %d, want %d", got, os.Getpid())
	}
}

// Removal is what keeps the common case clean. A leftover file is not
// dangerous — checkPort asks the OS whether that PID actually holds the
// port — but a graceful stop should not leave one.
func TestRemoveServerPIDFile(t *testing.T) {
	dir := t.TempDir()
	path, err := writeServerPIDFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	removeServerPIDFile(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("pidfile survived removal: %v", err)
	}
	// Idempotent, and safe on the empty path (the write-failed branch
	// never registers the defer, but a future caller might).
	removeServerPIDFile(path)
	removeServerPIDFile("")
}

// Overwriting must work: a bridge restarting into the same data dir
// finds the previous run's file there.
func TestServerPIDFileOverwritesAStaleOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.pid")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("999999"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeServerPIDFile(dir); err != nil {
		t.Fatalf("writeServerPIDFile over a stale file: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getpid()) {
		t.Errorf("stale pidfile not replaced: got %q", raw)
	}
}
