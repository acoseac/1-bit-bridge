package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/updater"
)

// Backlog B660's fourth round: a doctor's probe of a crashed start's lock
// lets no command migrate the store, the tailnet's closed answer ends the
// start it reaches, and a stop before ready whose teardown outlasts the
// stop's budget still has its count taken back.

// crashedStartToken is the token a start killed before its migration left in
// the marker, its lock file beside it with nobody holding it.
const crashedStartToken = "00112233445566778899aabbccddeeff"

// TestADoctorsProbeHeldBeforeItsUnlockLetsNoScanMigrate: a start killed
// before its migration leaves one start counted, its token and its lock file.
// `bridge doctor` probes that lock and is held there, after its try took the
// lock and before it lets go; a `bridge scan` beside it, which would migrate
// the store, meets that lock at its migration's gate and refuses all the
// same: the doctor's probe is no running start, so the one start counted is
// one before now that did not end cleanly. Until the fourth round the scan's
// probe read the doctor's hold as the crashed start's, took the sole failure
// off the count, migrated, and refused the crash rollback for good.
func TestADoctorsProbeHeldBeforeItsUnlockLetsNoScanMigrate(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.FailedStarts = 1
	r.marker.RunningStart = crashedStartToken
	r.withAnOlderStore(t)
	if err := os.WriteFile(filepath.Join(r.dataDir, "serve-start-"+crashedStartToken+".lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	behind := storeVersion(t, r.dataDir)
	parked, release := newOnceCloser(), newOnceCloser()
	var once sync.Once
	t.Cleanup(updater.SetStartProbeHeldForTest(func(string) {
		once.Do(func() {
			parked.close()
			<-release.ch
		})
	}))
	doctored := make(chan struct{})
	go func() {
		defer close(doctored)
		var out, errs bytes.Buffer
		run(context.Background(), []string{"doctor", "--config", r.cfg}, &out, &errs)
	}()
	t.Cleanup(func() {
		release.close()
		<-doctored
	})
	select {
	case <-parked.ch:
	case <-time.After(withinTheTest):
		t.Fatal("bridge doctor never probed the crashed start's lock")
	}
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"scan", "--config", r.cfg}, &stdout, &stderr)
	release.close()
	<-doctored
	if code == 0 || !strings.Contains(stderr.String(), "awaits its confirmation") {
		t.Errorf("bridge scan beside the doctor's held probe = %d, want a refusal naming the probation; stderr=%s", code, stderr.String())
	}
	if v := storeVersion(t, r.dataDir); v != behind {
		t.Errorf("the store after the scan is at %d, want %d, unmigrated", v, behind)
	}
	if st := markerOf(t, r.dataDir); st.StoreMigratedBy != "" {
		t.Errorf("the marker after the scan = %+v, want no migration recorded", st)
	}
}

// TestATailnetServerClosedAnswerEndsTheStart: serve's select reads the
// tailnet listener's end once, and its closed answer (http.ErrServerClosed)
// ends the start there, as the second outcome of that case does: serve
// returns 0 at once, and the count goes back, as the third round has it for
// that answer. So the flag the answer sets cannot outlive it into a later end
// of the same start: no later end comes (the fourth review's second point
// read it as serving on). In production the answer comes only once serve's
// own stop of its tailnet side has begun, which follows the select; here the
// listener gives it.
func TestATailnetServerClosedAnswerEndsTheStart(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.writeSettings(t, "dataDir: "+r.dataDir+"\n", "tailscale:\n  mode: tsnet\n")
	node := newFakeTsnetNode()
	node.listen = func() (net.Listener, error) {
		return &refusingTailnetListener{err: http.ErrServerClosed, closed: make(chan struct{})}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &safeBuffer{}
	codes, done := make(chan int, 1), make(chan struct{})
	go func() {
		defer close(done)
		codes <- runServe(ctx, serveOpts{configPath: r.cfg, binaryPath: r.bin, tsnetNode: node}, &safeBuffer{}, stderr)
	}()
	drainLoopOnCleanup(t, cancel, done, "serve")
	select {
	case code := <-codes:
		if code != 0 {
			t.Errorf("serve over a tailnet that answers that its server closed = %d, want 0; stderr=%s", code, stderr.String())
		}
	case <-time.After(withinTheTest):
		t.Fatalf("serve went on past the tailnet's closed answer; stderr=%s", stderr.String())
	}
	r.requireCount(t, 0, "once the tailnet's closed answer ended the start")
}

// TestAPreReadyStopWhoseTeardownOutlastsTheBudgetIsTakenBack: a stop before
// the start has answered as ready, whose teardown runs past the stop's budget
// with nothing killing it, still has its count taken back: the take-back
// begun once the teardown has run has two seconds of its own at least, here
// for the install marker's lock, held across the teardown's end and let go a
// second after it. Until the fourth round its deadline was the budget's from
// the stop's arrival, past by then, so it gave up at the lock's first refusal,
// and three such stops rolled a working install back.
func TestAPreReadyStopWhoseTeardownOutlastsTheBudgetIsTakenBack(t *testing.T) {
	r := newRefusedInstall(t, "")
	teardown := newParkedTeardown()
	_, seams := newProbationWatch(neverReady)
	sb := r.bootWith(t, serveOpts{probation: seams, upnpStopping: teardown.seam})
	teardown.releaseFirst(t)
	r.requireCount(t, 1, "while serve runs, not ready yet")
	stoppedAt := time.Now()
	sb.stop()
	teardown.waitParked(t)
	time.Sleep(time.Until(stoppedAt.Add(stopBudget + 500*time.Millisecond)))
	held, _ := holdTheMarkersLock(t, r.dataDir)
	teardown.release.close()
	time.Sleep(time.Second)
	held.Release()
	waitForExit(t, sb, 0)
	r.requireCount(t, 0, "once its teardown, past the stop's budget, had run")
}
