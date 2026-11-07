package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B660's second round: a stop takes the start's count back as it
// arrives, not after a teardown its supervisor may cut short; a command
// other than serve does not migrate the store while the install awaits its
// confirmation after a start that did not end cleanly; a permission refusal
// and a tailnet failure are the host's only where the backup would meet them
// too; and the doctor and the console grade the starts before the running
// one, promising no put-back.

// waitForCountWithin waits, for at most within, for the marker in r's data
// dir to hold count starts that did not end cleanly, and reports whether it
// came to.
func (r *refusedInstall) waitForCountWithin(t *testing.T, count int, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if st, err := updater.LoadState(r.dataDir); err == nil && st.FailedStarts == count {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// withACurrentStore puts a store at the ladder's top in r's data dir, made
// with no install armed, and arms r's marker again: a start over it
// migrates nothing.
func (r *refusedInstall) withACurrentStore(t *testing.T) {
	t.Helper()
	if err := updater.ClearState(r.dataDir); err != nil {
		t.Fatal(err)
	}
	s, err := manifest.OpenStore(manifest.DefaultDBPath(r.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r.saveMarker(t, r.dataDir)
}

// parkedTeardown is a seam that parks serve's teardown where it stops the
// UPnP upstream (serveOpts.upnpStopping), after the drain and before every
// teardown registered ahead of it, the take-back after the teardown among
// them, until the test lets it go.
type parkedTeardown struct {
	parked, release *onceCloser
}

func newParkedTeardown() *parkedTeardown {
	return &parkedTeardown{parked: newOnceCloser(), release: newOnceCloser()}
}

// releaseFirst lets the teardown go as the test ends, before serve's drain,
// which cleanups run last-registered-first put after it: registered after
// the boot.
func (p *parkedTeardown) releaseFirst(t *testing.T) {
	t.Cleanup(p.release.close)
}

func (p *parkedTeardown) seam() {
	p.parked.close()
	<-p.release.ch
}

func (p *parkedTeardown) waitParked(t *testing.T) {
	t.Helper()
	select {
	case <-p.parked.ch:
	case <-time.After(withinTheTest):
		t.Fatal("serve's teardown never reached the UPnP upstream's stop")
	}
}

// TestAStopTakesItsCountBackAsItArrives: serve's stop takes back the count
// its admission added as soon as it arrives, while the teardown still runs: a
// supervisor that kills a stop it finds too slow (launchd's ExitTimeOut,
// systemd's TimeoutStopSec, the SCM's wait) must not leave a clean stop
// counted. Until the second round the count went back only after every
// teardown had run.
func TestAStopTakesItsCountBackAsItArrives(t *testing.T) {
	r := newRefusedInstall(t, "")
	teardown := newParkedTeardown()
	_, seams := newProbationWatch(longProbation)
	sb := r.bootWith(t, serveOpts{probation: seams, upnpStopping: teardown.seam})
	teardown.releaseFirst(t)
	r.requireCount(t, 1, "while serve runs")
	sb.stop()
	teardown.waitParked(t)
	if !r.waitForCountWithin(t, 0, withinTheTest) {
		t.Errorf("the marker while the teardown runs = %+v, want the stop's count taken back already", markerOf(t, r.dataDir))
	}
	teardown.release.close()
	waitForExit(t, sb, 0)
	r.requireCount(t, 0, "after the stop")
}

// TestARestartRequestTakesItsCountBackAsItArrives: the console's restart is a
// stop of its own, which runServe's own context alone carries: its count goes
// back as the request arrives, while the teardown still runs, as a stop's
// does.
func TestARestartRequestTakesItsCountBackAsItArrives(t *testing.T) {
	r := newRefusedInstall(t, "")
	teardown := newParkedTeardown()
	_, seams := newProbationWatch(longProbation)
	sb, admin := r.bootConsole(t, serveOpts{probation: seams, upnpStopping: teardown.seam})
	teardown.releaseFirst(t)
	r.requireCount(t, 1, "while serve runs")
	requestRestart(t, admin)
	teardown.waitParked(t)
	if !r.waitForCountWithin(t, 0, withinTheTest) {
		t.Errorf("the marker while the restart's teardown runs = %+v, want the count taken back already", markerOf(t, r.dataDir))
	}
	teardown.release.close()
	waitForExit(t, sb, supervision.RestartExitCode)
}

// TestAStopWhileServeStartsTakesItsCountBackAsItArrives: a stop that lands
// while serve is still starting, past its admission, takes the count back as
// it arrives, while the start is still held where it was.
func TestAStopWhileServeStartsTakesItsCountBackAsItArrives(t *testing.T) {
	r := newRefusedInstall(t, "")
	parked, release := newOnceCloser(), newOnceCloser()
	t.Cleanup(release.close)
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &safeBuffer{}
	codes, done := make(chan int, 1), make(chan struct{})
	go func() {
		defer close(done)
		codes <- runServe(ctx, serveOpts{configPath: r.cfg, binaryPath: r.bin,
			playerStatePreflight: func(string, bool) error {
				parked.close()
				<-release.ch
				return nil
			}}, &safeBuffer{}, stderr)
	}()
	drainLoopOnCleanup(t, func() { cancel(); release.close() }, done, "serve")
	select {
	case <-parked.ch:
	case <-time.After(withinTheTest):
		t.Fatalf("serve never reached its player's preflight; stderr=%s", stderr.String())
	}
	r.requireCount(t, 1, "as the start is held past its admission")
	cancel()
	if !r.waitForCountWithin(t, 0, withinTheTest) {
		t.Errorf("the marker while the stopped start is held = %+v, want its count taken back already", markerOf(t, r.dataDir))
	}
	release.close()
	select {
	case code := <-codes:
		if code != 0 {
			t.Errorf("serve stopped while it started = %d, want 0; stderr=%s", code, stderr.String())
		}
	case <-serveGiveUp(t):
		t.Fatalf("serve did not return; stderr=%s", stderr.String())
	}
	r.requireCount(t, 0, "after a stop while it started")
}

// TestAStopThatFindsTheMarkersLockHeldTakesItsCountBackOnceItIsFree: a stop
// whose take-back finds the install marker's lock held keeps asking for it
// for the rest of the teardown, past a wait that gives up, and lands as soon
// as the lock is free, while the teardown still runs.
func TestAStopThatFindsTheMarkersLockHeldTakesItsCountBackOnceItIsFree(t *testing.T) {
	r := newRefusedInstall(t, "")
	teardown := newParkedTeardown()
	_, seams := newProbationWatch(longProbation)
	sb := r.bootWith(t, serveOpts{probation: seams, upnpStopping: teardown.seam})
	teardown.releaseFirst(t)
	r.requireCount(t, 1, "while serve runs")
	held, err := updater.LockState(t.Context(), r.dataDir)
	if err != nil || !held.Held() {
		t.Fatalf("hold the install marker's lock: %+v, %v", held, err)
	}
	t.Cleanup(held.Release)
	// Each wait for the lock gives up quickly, so the take-back's asking
	// again is what lands it.
	t.Cleanup(updater.SetStateLockWaitForTest(50 * time.Millisecond))
	var waits atomic.Int32
	second := make(chan struct{}, 1)
	t.Cleanup(updater.SetStateLockWaitingForTest(func(string) {
		if waits.Add(1) == 2 {
			signalOnce(second)
		}
	}))
	sb.stop()
	teardown.waitParked(t)
	select {
	case <-second:
	case <-time.After(withinTheTest):
		t.Fatalf("the take-back did not ask for the install marker's lock again while the teardown ran (%d waits)", waits.Load())
	}
	r.requireCount(t, 1, "while the install marker's lock is held")
	held.Release()
	if !r.waitForCountWithin(t, 0, withinTheTest) {
		t.Errorf("the marker once the lock is free, the teardown still running = %+v, want the count taken back", markerOf(t, r.dataDir))
	}
	teardown.release.close()
	waitForExit(t, sb, 0)
}

// TestAnErrorExitStaysCountedWhateverItsCallerDoesAfter: a start that ends in
// an error, or a panic, stays counted, and its caller cancelling the context
// it gave serve once serve has returned (main's signal context, a test's
// cleanup) takes nothing back.
func TestAnErrorExitStaysCountedWhateverItsCallerDoesAfter(t *testing.T) {
	t.Run("an error", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		if err := os.Symlink("elsewhere.db", manifest.DefaultDBPath(r.dataDir)); err != nil {
			t.Skipf("no symbolic link can be made here: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		var out, errs bytes.Buffer
		if code := runServe(ctx, serveOpts{configPath: r.cfg, binaryPath: r.bin}, &out, &errs); code != 1 {
			t.Fatalf("serve over a store that will not open = %d, want 1; stderr=%s", code, errs.String())
		}
		cancel()
		// Nothing comes to wait for: a take-back left registered on the
		// context would land within this.
		time.Sleep(200 * time.Millisecond)
		r.requireCount(t, 1, "after its caller's context ended")
	})
	t.Run("a panic", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		ctx, cancel := context.WithCancel(context.Background())
		func() {
			defer func() { _ = recover() }()
			var out, errs bytes.Buffer
			runServe(ctx, serveOpts{configPath: r.cfg, binaryPath: r.bin,
				playerStatePreflight: func(string, bool) error { panic("a crash of the start") }}, &out, &errs)
		}()
		cancel()
		time.Sleep(200 * time.Millisecond)
		r.requireCount(t, 1, "after its caller's context ended")
	})
}

// TestACommandDoesNotMigrateTheStoreWhileTheInstallAwaitsItsConfirmation: a
// command other than serve that would migrate the store, while the install
// of its version awaits its confirmation after a start that did not end
// cleanly, refuses and says why: the migration it would begin would refuse
// the crash rollback for good, and the next start of serve migrates the store
// itself.
func TestACommandDoesNotMigrateTheStoreWhileTheInstallAwaitsItsConfirmation(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.FailedStarts = 1
	r.withAnOlderStore(t)
	behind := storeVersion(t, r.dataDir)
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"scan", "--config", r.cfg}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "awaits its confirmation") {
		t.Errorf("bridge scan over a store behind, the install on probation after a failed start = %d, "+
			"want a refusal naming the probation; stderr=%s", code, stderr.String())
	}
	if st := markerOf(t, r.dataDir); st.StoreMigratedBy != "" {
		t.Errorf("the marker after the scan = %+v, want no migration recorded", st)
	}
	if v := storeVersion(t, r.dataDir); v != behind {
		t.Errorf("the store after the scan is at %d, want %d, unmigrated", v, behind)
	}
}

// TestTheDoctorLeavesThePendingMigrationsAndSaysSo: where a command may not
// migrate the store, `bridge doctor` reads it as it is and says how many
// migrations are pending, migrating nothing.
func TestTheDoctorLeavesThePendingMigrationsAndSaysSo(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.FailedStarts = 1
	r.withAnOlderStore(t)
	behind := storeVersion(t, r.dataDir)
	var stdout, stderr bytes.Buffer
	run(t.Context(), []string{"doctor", "--config", r.cfg}, &stdout, &stderr)
	if out := stdout.String(); !strings.Contains(out, "1 migration pending") {
		t.Errorf("bridge doctor over a store one migration behind does not say it is pending; stdout=%s stderr=%s", out, stderr.String())
	}
	if st := markerOf(t, r.dataDir); st.StoreMigratedBy != "" {
		t.Errorf("the marker after bridge doctor = %+v, want no migration recorded", st)
	}
	if v := storeVersion(t, r.dataDir); v != behind {
		t.Errorf("the store after bridge doctor is at %d, want %d, unmigrated", v, behind)
	}
}

// TestServeMigratesTheStoreAfterAFailedStart: serve's own start migrates the
// store at a pending migration whatever the count, its admission's
// permission passed through the open, and records that it began.
func TestServeMigratesTheStoreAfterAFailedStart(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.FailedStarts = 1
	r.withAnOlderStore(t)
	_, seams := newProbationWatch(longProbation)
	sb := r.bootWith(t, serveOpts{probation: seams})
	if st := markerOf(t, r.dataDir); st.StoreMigratedBy != version.ServerVersion || st.FailedStarts != 2 {
		t.Errorf("the marker as serve serves = %+v, want its migration recorded and its start counted", st)
	}
	if v := storeVersion(t, r.dataDir); v != latestStoreVersion(t) {
		t.Errorf("the store as serve serves is at %d, want the ladder's top", v)
	}
	stopServed(t, sb)
}

// TestACommandBesideAStartingServe: a command that would migrate the store
// while a serve holds the data dir's lock reads that serve's own start out
// of the count: with a start before it that did not end cleanly it refuses,
// and with none it migrates, as before the second round.
func TestACommandBesideAStartingServe(t *testing.T) {
	for _, tc := range []struct {
		name    string
		before  int
		refused bool
	}{
		{"a start before it did not end cleanly", 1, true},
		{"none did", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "")
			r.marker.FailedStarts = tc.before
			r.withAnOlderStore(t)
			behind := storeVersion(t, r.dataDir)
			parked, release := newOnceCloser(), newOnceCloser()
			t.Cleanup(release.close)
			ctx, cancel := context.WithCancel(context.Background())
			stderr := &safeBuffer{}
			codes, done := make(chan int, 1), make(chan struct{})
			go func() {
				defer close(done)
				codes <- runServe(ctx, serveOpts{configPath: r.cfg, binaryPath: r.bin,
					playerStatePreflight: func(string, bool) error {
						parked.close()
						<-release.ch
						return nil
					}}, &safeBuffer{}, stderr)
			}()
			drainLoopOnCleanup(t, func() { cancel(); release.close() }, done, "serve")
			select {
			case <-parked.ch:
			case <-time.After(withinTheTest):
				t.Fatalf("serve never reached its player's preflight; stderr=%s", stderr.String())
			}
			r.requireCount(t, tc.before+1, "with serve held past its admission")
			var out, errs bytes.Buffer
			code := run(t.Context(), []string{"scan", "--config", r.cfg}, &out, &errs)
			st := markerOf(t, r.dataDir)
			if tc.refused {
				if code == 0 || st.StoreMigratedBy != "" || storeVersion(t, r.dataDir) != behind {
					t.Errorf("bridge scan beside a starting serve, one start before it unclean = %d, the marker %+v; "+
						"want a refusal and nothing migrated; stderr=%s", code, st, errs.String())
				}
			} else if code != 0 || st.StoreMigratedBy != version.ServerVersion {
				t.Errorf("bridge scan beside a starting serve, no start before it unclean = %d, the marker %+v; "+
					"want the store migrated, as before; stderr=%s", code, st, errs.String())
			}
			cancel()
			release.close()
			select {
			case <-codes:
			case <-serveGiveUp(t):
				t.Fatalf("serve did not return; stderr=%s", stderr.String())
			}
		})
	}
}

// refusedPermissionAddr is a loopback address this user may not bind: port 1,
// below the host's unprivileged floor. It skips where the bind is let through
// (root, or a lowered floor).
func refusedPermissionAddr(t *testing.T) string {
	t.Helper()
	const addr = "127.0.0.1:1"
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		_ = ln.Close()
		t.Skipf("this process may bind %s, so no bind is refused for a permission here", addr)
	}
	if !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) {
		t.Skipf("a bind of %s here is refused with %v, not a permission", addr, err)
	}
	return addr
}

// capabilities stands in for the file capabilities of r's binary and its
// backup (serveOpts.fileCapability).
func (r *refusedInstall) capabilities(bin, bak []byte) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		switch path {
		case r.bin:
			return bin, nil
		case r.bak:
			return bak, nil
		}
		return nil, nil
	}
}

// TestAPermissionRefusalTheBackupWouldNotMeetIsAFailedStart: a bind refused
// for a permission is the host's trouble only where the backup would meet it
// too: a backup that carries a file capability the binary lacks (the
// operator's setcap for a port below 1024, which a swap that cannot set it
// does not carry) would bind, so the refusal stays counted; with the same
// capability on both, or none, it is taken back.
func TestAPermissionRefusalTheBackupWouldNotMeetIsAFailedStart(t *testing.T) {
	addr := refusedPermissionAddr(t)
	capNetBind := []byte("a file capability: cap_net_bind_service")
	for _, tc := range []struct {
		name     string
		bin, bak []byte
		counted  int
	}{
		{"the backup carries a capability the binary lacks", nil, capNetBind, 1},
		{"both carry it", capNetBind, capNetBind, 0},
		{"neither does", nil, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "")
			r.writeListeners(t, addr, "127.0.0.1:0")
			code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin,
				fileCapability: r.capabilities(tc.bin, tc.bak)})
			if served || code != 1 || !strings.Contains(stderr, "listen "+addr) {
				t.Fatalf("serve over %s = %d (served %v), want 1 at its listen; stderr=%s", addr, code, served, stderr)
			}
			r.requireCount(t, tc.counted, "after a bind refused for a permission")
		})
	}
}

// refusingTailnetListener is a tailnet listener whose Accept fails with err.
type refusingTailnetListener struct {
	err    error
	once   sync.Once
	closed chan struct{}
}

func (l *refusingTailnetListener) Accept() (net.Conn, error) { return nil, l.err }
func (l *refusingTailnetListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}
func (l *refusingTailnetListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(100, 64, 0, 1), Port: 7788}
}

// TestATailnetFailureIsTheNetworksOnlyWhenItSaysSo: a tailnet listener that
// fails takes the start's count back only for a failure of the network
// (an accept the network refused, a listener the node closed), which the
// backup would meet too; any other failure of it stays counted.
func TestATailnetFailureIsTheNetworksOnlyWhenItSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		counted int
	}{
		{"the network went down", &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.ENETDOWN)}, 0},
		{"the node closed its listener", net.ErrClosed, 0},
		{"another failure", errors.New("the tailnet listener broke"), 1},
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
			r.requireCount(t, tc.counted, "after the tailnet listener failed")
		})
	}
}

// wildcardListener is a loopback listener that reports the wildcard address
// on its port, as a listener on listenAddress ":7788" does.
type wildcardListener struct{ net.Listener }

func (l wildcardListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv6unspecified, Port: l.Listener.Addr().(*net.TCPAddr).Port}
}

// TestAnInstallListeningOnTheWildcardIsConfirmed: the default listenAddress
// is the wildcard, which the confirmation's round dials on loopback
// (probeHost) and names as the Host of its in-process GET /v1/health,
// "[::]:port": both answer, and the install is confirmed.
func TestAnInstallListeningOnTheWildcardIsConfirmed(t *testing.T) {
	r := newRefusedInstall(t, "")
	lan := holdLoopback(t)
	wild := wildcardListener{lan.ln}
	addr := wild.Addr().String()
	// Quoted: a bracket opens a YAML sequence.
	r.writeListeners(t, strconv.Quote(addr), "127.0.0.1:0")
	events, seams := newProbationWatch(shortProbation)
	sb := r.bootWith(t, serveOpts{probation: seams, addrOverride: addr, lanListener: wild})
	events.waitFor(t, probationConfirmed)
	if st := markerOf(t, r.dataDir); st.Status != "installed" {
		t.Errorf("the marker = %+v, want the install confirmed through its wildcard listener", st)
	}
	stopServed(t, sb)
}

// TestTheConsoleGradesTheStartsBeforeTheRunningOne: the Updates panel is
// read by the start that runs, which counts itself until it stops cleanly:
// two starts before it that did not end cleanly are a warning, not yet a
// failure, and the row promises no put-back.
func TestTheConsoleGradesTheStartsBeforeTheRunningOne(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.FailedStarts = 2
	// The store as the version before left it, at the ladder's top, so this
	// start migrates nothing: a migration would refuse the rollback.
	r.withACurrentStore(t)
	_, seams := newProbationWatch(longProbation)
	sb, admin := r.bootConsole(t, serveOpts{probation: seams})
	panel := probationPanel(t, admin)
	if panel.Level != "warn" {
		t.Errorf("the console's Probation row with two starts before this one unclean = %+v, want a warn", panel)
	}
	if strings.Contains(panel.Says, "the next start puts") || !strings.Contains(panel.Says, "tries to put "+previousVersion+" back") {
		t.Errorf("the console's Probation row = %q, want it to say the next start tries to put %s back, promising nothing", panel.Says, previousVersion)
	}
	stopServed(t, sb)
}

// TestTheDoctorCountsEveryStartWhereNoServeRuns: with no serve holding the
// data dir's lock, every start the marker counts is one that did not end
// cleanly: one is a warning, and the line says it, with no start running
// among them.
func TestTheDoctorCountsEveryStartWhereNoServeRuns(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.FailedStarts = 1
	r.saveMarker(t, r.dataDir)
	line := doctorLineOf(t, r.cfg)
	if line == nil {
		t.Fatal("bridge doctor has no install-probation line")
	}
	if line.Status != "warn" || strings.Contains(line.Summary, "running now") || !strings.Contains(line.Summary, "1 of") {
		t.Errorf("bridge doctor's line with one start that did not end cleanly and no serve = %+v, "+
			"want a warn counting it, no start running among them", *line)
	}
}

// TestTheDoctorSaysTheBackupIsTriedNotPromised: at the limit, with a marker
// the crash rollback takes, the doctor says the next start tries to put the
// backup back, never that it does: it reads no backup, which may be missing
// or refuse the settings.
func TestTheDoctorSaysTheBackupIsTriedNotPromised(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.FailedStarts = updater.ProbationFailedStarts
	r.saveMarker(t, r.dataDir)
	if err := os.Remove(r.bak); err != nil {
		t.Fatal(err)
	}
	line := doctorLineOf(t, r.cfg)
	if line == nil {
		t.Fatal("bridge doctor has no install-probation line")
	}
	if line.Status != "fail" || strings.Contains(line.Summary, "the next start puts") ||
		!strings.Contains(line.Summary, "tries to put "+previousVersion+" back") {
		t.Errorf("bridge doctor's line at the limit, the backup missing = %+v, want a fail that promises no put-back", *line)
	}
}

// TestTheDoctorPromisesNoPutBackTheMarkerRefuses: a marker the crash
// rollback refuses (a migration of the store begun) puts nothing back at the
// limit, and the doctor says why, before the count gets there.
func TestTheDoctorPromisesNoPutBackTheMarkerRefuses(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.FailedStarts = 2
	r.marker.StoreMigratedBy = version.ServerVersion
	r.saveMarker(t, r.dataDir)
	line := doctorLineOf(t, r.cfg)
	if line == nil {
		t.Fatal("bridge doctor has no install-probation line")
	}
	if strings.Contains(line.Summary, "puts "+previousVersion+" back") || strings.Contains(line.Summary, "tries to put") ||
		!strings.Contains(line.Summary, "began a migration") {
		t.Errorf("bridge doctor's line over a marker the rollback refuses = %+v, want no put-back, and why", *line)
	}
}

// TestTheDoctorBesideARunningServeTakesItsStartOff: with a serve holding the
// data dir's lock, the marker counts that start until it stops cleanly, and
// the doctor takes it off: one start counted is a serve on probation, ok.
func TestTheDoctorBesideARunningServeTakesItsStartOff(t *testing.T) {
	r := newRefusedInstall(t, "")
	_, seams := newProbationWatch(longProbation)
	sb := r.bootWith(t, serveOpts{probation: seams})
	r.requireCount(t, 1, "while serve runs")
	line := doctorLineOf(t, r.cfg)
	if line == nil || line.Status != "ok" {
		t.Errorf("bridge doctor beside the running start = %+v, want ok", line)
	}
	stopServed(t, sb)
}
