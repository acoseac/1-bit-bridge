package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B760's fifth residual: the count's writes that fail.

// TestACountWhoseBarrierFailedKeepsItsStartsHold: a count whose barrier
// failed stands in the marker with its token, published before the barrier:
// the start keeps the lock its token names while it runs, so a reader beside
// it reads it running, and clears the token as it returns, the count of a
// start that did not end cleanly kept. Until B760 the admission let go of the
// lock at the failed write, so the running start read as one that had not
// ended cleanly, and its token outlived it.
func TestACountWhoseBarrierFailedKeepsItsStartsHold(t *testing.T) {
	r := newRefusedInstall(t, "")
	// A start of this version opened the data dir before, so this start's
	// count is its admission's only write; the start then ends in an error,
	// at its TLS material.
	r.marker.DataDirOpenedBy = version.ServerVersion
	r.saveMarker(t, r.dataDir)
	r.withATLSPairThatWillNotLoad(t)
	var writes atomic.Int32
	var armed atomic.Bool
	t.Cleanup(updater.SetBeforeDisarmRecordForTest(func(string) {
		if writes.Add(1) == 1 {
			armed.Store(true)
		}
	}))
	t.Cleanup(updater.SetMarkerBarrierForTest(func(step string) error {
		if step == "sync" && armed.CompareAndSwap(true, false) {
			return syscall.EIO
		}
		return nil
	}))
	parked, release := newOnceCloser(), newOnceCloser()
	stderr := &safeBuffer{}
	codes, done := make(chan int, 1), make(chan struct{})
	go func() {
		defer close(done)
		codes <- runServe(context.Background(), serveOpts{configPath: r.cfg, binaryPath: r.bin,
			playerStatePreflight: func(string, bool) error {
				parked.close()
				<-release.ch
				return nil
			}}, &safeBuffer{}, stderr)
	}()
	drainLoopOnCleanup(t, release.close, done, "serve")
	select {
	case <-parked.ch:
	case <-time.After(withinTheTest):
		t.Fatalf("serve never reached its player's preflight; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "was not counted against its install") || armed.Load() {
		t.Fatalf("the count's barrier did not fail: the test shows nothing; stderr=%s", stderr.String())
	}
	st := markerOf(t, r.dataDir)
	if st.FailedStarts != 1 || st.RunningStart == "" || !updater.RunningServeCounted(r.dataDir, st, version.ServerVersion) {
		t.Errorf("the marker while the start whose count's barrier failed runs = %+v, want its count and its token, read as a start running", st)
	}
	release.close()
	select {
	case code := <-codes:
		if code != 1 {
			t.Fatalf("serve = %d, want 1 at its TLS material; stderr=%s", code, stderr.String())
		}
	case <-serveGiveUp(t):
		t.Fatalf("serve did not return; stderr=%s", stderr.String())
	}
	if st := markerOf(t, r.dataDir); st.FailedStarts != 1 || st.RunningStart != "" {
		t.Errorf("the marker once the start returned an error = %+v, want its count kept and its token cleared", st)
	}
	if left, _ := filepath.Glob(filepath.Join(r.dataDir, "serve-start-*.lock")); len(left) != 0 {
		t.Errorf("the start's lock files left behind: %v", left)
	}
}

// TestACountThatFailedBeforeItPublishedLeavesItsStartUncounted: a count
// whose write failed before it published leaves the marker as it was, and the
// start goes on, saying so (backlog B760).
func TestACountThatFailedBeforeItPublishedLeavesItsStartUncounted(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.DataDirOpenedBy, r.marker.FailedStarts = version.ServerVersion, 1
	r.saveMarker(t, r.dataDir)
	marker := watch(t, updater.StatePath(r.dataDir))
	var errs bytes.Buffer
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		runServe(t.Context(), serveOpts{configPath: r.cfg, binaryPath: r.bin, backupProbeTimeout: testBackupProbeTimeout,
			saveUpdateState:      func(string, updater.State) error { return errors.New("the disk is full") },
			playerStatePreflight: func(string, bool) error { panic("a crash of the start") }}, &bytes.Buffer{}, &errs)
	}()
	if !panicked {
		t.Fatal("serve returned where its player's preflight panics: the start was meant to crash there")
	}
	if !strings.Contains(errs.String(), "was not counted against its install") {
		t.Errorf("serve did not say its count was not written; stderr=%s", errs.String())
	}
	marker.requireUnchanged(t)
}

// TestARefusedAdmissionLeavesAtMostOneCount: the admission's one write of its
// record and its count refuses the start where it cannot be made durable, and
// every later start while the barrier fails refuses before it writes, adding
// no count of its own (backlog B760).
func TestARefusedAdmissionLeavesAtMostOneCount(t *testing.T) {
	r := newRefusedInstall(t, "")
	t.Cleanup(updater.SetMarkerBarrierForTest(func(step string) error {
		if step == "sync" {
			return syscall.EIO
		}
		return nil
	}))
	for start := 1; start <= updater.ProbationFailedStarts; start++ {
		if code, stderr := r.serve(t, serveOpts{}); code != 1 || !strings.Contains(updaterLines(stderr), "not starting") {
			t.Fatalf("start %d while the marker's barrier fails = %d, want 1 before its first write; stderr=%s", start, code, stderr)
		}
	}
	if st := markerOf(t, r.dataDir); st.FailedStarts != 1 || st.DataDirOpenedBy != version.ServerVersion {
		t.Errorf("the marker after the refused admissions = %+v, want the first one's record and count, published, and no more", st)
	}
	requireNoStoreOpened(t, r.dataDir)
}
