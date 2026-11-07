package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B660's third round: a stop takes the start's count back as it
// arrives only once the start has answered as ready, and otherwise once its
// teardown has run; a stop that came before an error is a stop; a running
// start is the lock its own token names, read under the install marker's
// lock; only serve's own close of the tailnet listener ends a start cleanly;
// a stop's take-back waits for the marker's lock for the rest of one budget;
// and the probe of a wildcard listener tries the IPv6 loopback too.

// neverReady keeps a start from asking whether it is ready for as long as a
// test runs: a start that has not answered as ready.
var neverReady = probationTiming{window: time.Hour, ask: time.Hour, retry: time.Hour, round: 5 * time.Second}

// TestAStopBeforeReadyStaysCountedUntilItsTeardownHasRun: a stop that
// arrives before this start has answered as ready takes its count back only
// once its teardown has run, so a start that hangs before it is ready, and
// whose stop its supervisor kills mid-teardown, stays counted (the third
// round: the second took the count back as the stop arrived, and a version
// that hung after its admission never reached its rollback).
func TestAStopBeforeReadyStaysCountedUntilItsTeardownHasRun(t *testing.T) {
	r := newRefusedInstall(t, "")
	teardown := newParkedTeardown()
	_, seams := newProbationWatch(neverReady)
	sb := r.bootWith(t, serveOpts{probation: seams, upnpStopping: teardown.seam})
	teardown.releaseFirst(t)
	r.requireCount(t, 1, "while serve runs, not ready yet")
	sb.stop()
	teardown.waitParked(t)
	if r.waitForCountWithin(t, 0, time.Second) {
		t.Errorf("the marker while the teardown of a stop before ready is held = %+v, want the count to stand", markerOf(t, r.dataDir))
	}
	teardown.release.close()
	waitForExit(t, sb, 0)
	r.requireCount(t, 0, "once the teardown has run")
}

// TestAStopBeforeReadyIsNotArmedByAReadyAnswerInItsTeardown: a round that
// answered ready before the stop, and comes to count this start as ready only
// after it, arms nothing: the stop came first, and its count goes back once
// the teardown has run.
func TestAStopBeforeReadyIsNotArmedByAReadyAnswerInItsTeardown(t *testing.T) {
	r := newRefusedInstall(t, "")
	teardown := newParkedTeardown()
	atReady, ready := newOnceCloser(), newOnceCloser()
	events, seams := newProbationWatch(longProbation)
	seams.beforeReady = func() {
		atReady.close()
		<-ready.ch
	}
	sb := r.bootWith(t, serveOpts{probation: seams, upnpStopping: teardown.seam})
	t.Cleanup(ready.close)
	teardown.releaseFirst(t)
	select {
	case <-atReady.ch:
	case <-time.After(withinTheTest):
		t.Fatalf("the confirmation's first ready answer never came to count the start as ready; it did %+v", events.events())
	}
	sb.stop()
	teardown.waitParked(t)
	ready.close()
	events.waitFor(t, probationEnded)
	if r.waitForCountWithin(t, 0, time.Second) {
		t.Errorf("the marker while the teardown of a stop before ready is held = %+v, want the count to stand: "+
			"a ready answer that comes after the stop arms nothing", markerOf(t, r.dataDir))
	}
	teardown.release.close()
	waitForExit(t, sb, 0)
	r.requireCount(t, 0, "once the teardown has run")
}

// withATLSPairThatWillNotLoad points r's settings at a certificate and a key
// that are directories, so serve's start ends in an error at its TLS
// material, past its admission.
func (r *refusedInstall) withATLSPairThatWillNotLoad(t *testing.T) {
	t.Helper()
	r.writeSettings(t, "dataDir: "+r.dataDir+"\n", "tlsCertPath: "+r.dataDir+"\ntlsKeyPath: "+r.dataDir+"\n")
}

// TestAStopBeforeAnErrorIsAStop: a stop that arrives as serve starts, past its
// admission, followed at once by an error (its TLS material), ends the start
// as a stop: once the teardown has run the count goes back, whether or not
// the stop's own notice had reached the take-back by then; a teardown that
// hangs leaves it counted.
func TestAStopBeforeAnErrorIsAStop(t *testing.T) {
	t.Run("its teardown runs", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		r.withATLSPairThatWillNotLoad(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// The stop's notice to the take-back, held until serve has returned.
		arrive := newOnceCloser()
		var out, errs bytes.Buffer
		code := runServe(ctx, serveOpts{configPath: r.cfg, binaryPath: r.bin,
			playerStatePreflight: func(string, bool) error { cancel(); return nil },
			stopArriving:         func() { <-arrive.ch }}, &out, &errs)
		arrive.close()
		if code != 1 || !strings.Contains(errs.String(), "TLS material") {
			t.Fatalf("serve stopped as it started, then failing at its TLS material = %d, want 1 there; stderr=%s", code, errs.String())
		}
		r.requireCount(t, 0, "after a stop that came before an error, its teardown run")
	})
	t.Run("its teardown hangs", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		r.withATLSPairThatWillNotLoad(t)
		held, release := newOnceCloser(), newOnceCloser()
		ctx, cancel := context.WithCancel(context.Background())
		stderr := &safeBuffer{}
		codes, done := make(chan int, 1), make(chan struct{})
		go func() {
			defer close(done)
			codes <- runServe(ctx, serveOpts{configPath: r.cfg, binaryPath: r.bin,
				playerStatePreflight: func(string, bool) error { cancel(); return nil },
				teardownHold: func() {
					held.close()
					<-release.ch
				}}, &safeBuffer{}, stderr)
		}()
		drainLoopOnCleanup(t, func() { cancel(); release.close() }, done, "serve")
		select {
		case <-held.ch:
		case <-time.After(withinTheTest):
			t.Fatalf("serve's teardown never reached its hold; stderr=%s", stderr.String())
		}
		if r.waitForCountWithin(t, 0, time.Second) {
			t.Errorf("the marker while the teardown of a stop before an error hangs = %+v, want the count to stand", markerOf(t, r.dataDir))
		}
		release.close()
		select {
		case code := <-codes:
			if code != 1 {
				t.Errorf("serve = %d, want 1 at its TLS material; stderr=%s", code, stderr.String())
			}
		case <-serveGiveUp(t):
			t.Fatalf("serve did not return; stderr=%s", stderr.String())
		}
		r.requireCount(t, 0, "once the teardown has run")
	})
}

// holdTheDataDirsLockAsACommand holds the data dir's lock from an open this
// process notes as no serve's, as `bridge reset-player` or `bridge output dac
// bind` in another process holds it, until the test ends.
func holdTheDataDirsLockAsACommand(t *testing.T, dataDir string) {
	t.Helper()
	release, err := lockServeDataDir(dataDir)
	if err != nil {
		t.Fatalf("take the data dir's lock: %v", err)
	}
	t.Cleanup(release)
}

// TestTheDoctorBesideACommandHoldingTheDataDirsLockCountsEveryStart: one
// start that did not end cleanly, before the store's pending migration, and
// no serve running: a command in another process holding the data dir's lock
// is no running start, so `bridge doctor` counts the one, migrates nothing,
// and says so; and so for a start killed with its token left in the marker.
// Until the third round a held lock read as a serve's counted start.
func TestTheDoctorBesideACommandHoldingTheDataDirsLockCountsEveryStart(t *testing.T) {
	for _, tc := range []struct{ name, token string }{
		{"a start that ended in an error", ""},
		{"a start killed with its token", "00112233445566778899aabbccddeeff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "")
			r.marker.FailedStarts = 1
			r.marker.RunningStart = tc.token
			r.withAnOlderStore(t)
			behind := storeVersion(t, r.dataDir)
			holdTheDataDirsLockAsACommand(t, r.dataDir)
			line := doctorLineOf(t, r.cfg)
			if line == nil || line.Status != "warn" || !strings.Contains(line.Summary, "1 of") || strings.Contains(line.Summary, "running now") {
				t.Errorf("bridge doctor beside a command holding the data dir's lock = %+v, want a warn counting the one start, none running", line)
			}
			if v := storeVersion(t, r.dataDir); v != behind {
				t.Errorf("the store after bridge doctor is at %d, want %d, unmigrated", v, behind)
			}
			if st := markerOf(t, r.dataDir); st.StoreMigratedBy != "" {
				t.Errorf("the marker after bridge doctor = %+v, want no migration recorded", st)
			}
		})
	}
}

// TestTheDoctorAfterAStopsTakeBackCountsTheStartsBeforeIt: a stop's count is
// taken back while its serve still tears down, holding the data dir's lock:
// the one start left counted is the one before it, which `bridge doctor`
// reads as a start that did not end cleanly, with no start running. Until the
// third round the held lock took one more off.
func TestTheDoctorAfterAStopsTakeBackCountsTheStartsBeforeIt(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.FailedStarts = 1
	r.withACurrentStore(t)
	teardown := newParkedTeardown()
	events, seams := newProbationWatch(longProbation)
	sb := r.bootWith(t, serveOpts{probation: seams, upnpStopping: teardown.seam})
	teardown.releaseFirst(t)
	events.waitFor(t, probationWindow)
	r.requireCount(t, 2, "while serve runs")
	sb.stop()
	teardown.waitParked(t)
	if !r.waitForCountWithin(t, 1, withinTheTest) {
		t.Fatalf("the marker while the stopped start tears down = %+v, want its count taken back", markerOf(t, r.dataDir))
	}
	line := doctorLineOf(t, r.cfg)
	if line == nil || line.Status != "warn" || !strings.Contains(line.Summary, "1 of") || strings.Contains(line.Summary, "running now") {
		t.Errorf("bridge doctor while the stopped start tears down = %+v, want a warn counting the start before it, none running", line)
	}
	teardown.release.close()
	waitForExit(t, sb, 0)
}

// TestACountedStartCarriesItsTokenUntilItReturns: a counted start writes its
// token with its count and holds the lock the token names while it runs, and
// whatever it returns (here an error, which keeps its count) the token is
// cleared and its lock file gone: the launcher's in-process serve returns to
// its menu with the process alive.
func TestACountedStartCarriesItsTokenUntilItReturns(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.withATLSPairThatWillNotLoad(t)
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
	st := markerOf(t, r.dataDir)
	if st.FailedStarts != 1 || st.RunningStart == "" || !updater.RunningServeCounted(r.dataDir, st, version.ServerVersion) {
		t.Errorf("the marker while the counted start runs = %+v, want its count and its token, read as a start running", st)
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
	st = markerOf(t, r.dataDir)
	if st.FailedStarts != 1 || st.RunningStart != "" || updater.RunningServeCounted(r.dataDir, st, version.ServerVersion) {
		t.Errorf("the marker once the start returned an error = %+v, want its count kept and its token cleared", st)
	}
	if left, _ := filepath.Glob(filepath.Join(r.dataDir, "serve-start-*.lock")); len(left) != 0 {
		t.Errorf("the start's lock files left behind: %v", left)
	}
}

// TestATailnetListenerThatFailsIsAFailedStart: a tailnet listener that fails
// while serve runs counts as a start that did not end cleanly, whatever it
// answers: tsnet's listener answers net.ErrClosed once anything closes it,
// and an accept error is the listener's, so neither shows a failure of the
// tailnet's network the backup would meet too (the third round: the second
// took both for one).
func TestATailnetListenerThatFailsIsAFailedStart(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"closed under serve", fmt.Errorf("tsnet: %w", net.ErrClosed)},
		{"an accept that failed", &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.ENETDOWN)}},
		{"another failure", errors.New("the tailnet listener broke")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "")
			r.writeSettings(t, "dataDir: "+r.dataDir+"\n", "tailscale:\n  mode: tsnet\n")
			node := newFakeTsnetNode()
			node.listen = func() (net.Listener, error) {
				return &refusingTailnetListener{err: tc.err, closed: make(chan struct{})}, nil
			}
			// To its own end: a stop on its banner, as serveToItsEnd makes,
			// would race the listener's failure, and take the count back.
			code, stderr := r.serve(t, serveOpts{tsnetNode: node})
			if code != 1 || !strings.Contains(stderr, "tsnet listener:") {
				t.Fatalf("serve over a tailnet listener that fails = %d, want 1 at it; stderr=%s", code, stderr)
			}
			r.requireCount(t, 1, "after the tailnet listener failed")
		})
	}
}

// TestOnlyACloseServeBeganEndsItsTailnetListenerCleanly: the tailnet
// listener's end takes the start's count back only where serve began the
// close itself (its stop, marked begun before it shuts a server down or closes
// the node), or where the server says it was closed (http.ErrServerClosed);
// a closed listener alone is no proof of either.
func TestOnlyACloseServeBeganEndsItsTailnetListenerCleanly(t *testing.T) {
	closed := fmt.Errorf("tsnet: %w", net.ErrClosed)
	running := startTsnetFront(context.Background(), newFakeTsnetNode(), http.NotFoundHandler(), "127.0.0.1:0", false, io.Discard)
	t.Cleanup(func() { running.stop(context.Background()) })
	if tailnetEndIsServes(running, closed) {
		t.Error("a closed listener with no close of serve's ends the start cleanly")
	}
	if !tailnetEndIsServes(running, http.ErrServerClosed) {
		t.Error("the server's own closed answer does not end the start cleanly")
	}
	stopping := startTsnetFront(context.Background(), newFakeTsnetNode(), http.NotFoundHandler(), "127.0.0.1:0", false, io.Discard)
	stopping.stop(context.Background())
	if !tailnetEndIsServes(stopping, closed) {
		t.Error("a closed listener once serve's stop of the tailnet had begun does not end the start cleanly")
	}
}

// TestAStopsTakeBackWaitsForTheMarkersLockWithinItsBudget: a stop whose
// take-back finds the install marker's lock held to past its teardown keeps
// asking for it for the rest of the stop's budget: held two seconds and a half
// past the teardown's end, longer than the second round's fixed wait and
// shorter than the budget, the count goes back, and serve exits once it has.
func TestAStopsTakeBackWaitsForTheMarkersLockWithinItsBudget(t *testing.T) {
	r := newRefusedInstall(t, "")
	events, seams := newProbationWatch(longProbation)
	sb := r.bootWith(t, serveOpts{probation: seams})
	events.waitFor(t, probationWindow)
	r.requireCount(t, 1, "while serve runs")
	held, _ := holdTheMarkersLock(t, r.dataDir)
	pid := filepath.Join(r.dataDir, serverPIDFileName)
	sb.stop()
	// The teardown's end: serve removes its pid file among its last steps.
	deadline := time.Now().Add(withinTheTest)
	for {
		if _, err := os.Stat(pid); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve's teardown never removed its pid file; stderr=%s", sb.stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(2500 * time.Millisecond)
	held.Release()
	waitForExit(t, sb, 0)
	r.requireCount(t, 0, "once the marker's lock was free within the stop's budget")
}

// TestTheReadinessProbeTriesTheIPv6LoopbackForAWildcard: a listener bound to
// the IPv6 wildcard alone (IPv6-only), whose port's IPv4 loopback is another
// listener's, is reached all the same: the probe dials 127.0.0.1, gives that
// connect its share of the round, and dials ::1 (the third round: it dialled
// 127.0.0.1 alone, reached the other listener and waited out the round).
func TestTheReadinessProbeTriesTheIPv6LoopbackForAWildcard(t *testing.T) {
	v6, _ := ipv6AndAnotherIPv4OnOnePort(t)
	w := newAcceptWatch(wildcardListener{v6})
	go func() {
		for {
			c, err := w.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), productionProbation.round)
	defer cancel()
	if err := w.probe(ctx); err != nil {
		t.Fatalf("the probe of a wildcard listener whose port's IPv4 loopback is another listener's = %v, "+
			"want its own accept loop reached through ::1", err)
	}
}

// ipv6AndAnotherIPv4OnOnePort is an IPv6 loopback listener, the bridge's, and
// another listener on the same port's IPv4 loopback, neither of which the test
// leaves open. It skips where the host has no IPv6 loopback, or the port's
// IPv4 one is taken.
func ipv6AndAnotherIPv4OnOnePort(t *testing.T) (v6, v4 net.Listener) {
	t.Helper()
	v6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	t.Cleanup(func() { _ = v6.Close() })
	port := strconv.Itoa(v6.Addr().(*net.TCPAddr).Port)
	v4, err = net.Listen("tcp4", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Skipf("the IPv4 loopback's port %s is taken here: %v", port, err)
	}
	t.Cleanup(func() { _ = v4.Close() })
	return v6, v4
}

// TestBridgeHealthTriesTheIPv6LoopbackForAWildcard: `bridge health` over a
// listen address on the IPv6 wildcard, its listener IPv6-only and nothing on
// the port's IPv4 loopback, dials ::1 once 127.0.0.1 refuses, and says ok.
func TestBridgeHealthTriesTheIPv6LoopbackForAWildcard(t *testing.T) {
	v6, v4 := ipv6AndAnotherIPv4OnOnePort(t)
	// Nothing on the IPv4 loopback's port: the listener there let go.
	_ = v4.Close()
	r := newRefusedInstall(t, "")
	port := strconv.Itoa(v6.Addr().(*net.TCPAddr).Port)
	// Quoted: a bracket opens a YAML sequence.
	r.writeListeners(t, strconv.Quote(net.JoinHostPort("::", port)), "127.0.0.1:0")
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"health", "--config", r.cfg}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != "ok" {
		t.Errorf("bridge health over [::]:%s, its listener on ::1 alone = %d %q, want 0 ok; stderr=%s", port, code, stdout.String(), stderr.String())
	}
}
