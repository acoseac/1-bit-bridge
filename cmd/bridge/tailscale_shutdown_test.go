package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	servertailscale "github.com/acoseac/1-bit-bridge/internal/tailscale"
)

// heldMintCLI is a tailscaleCLI whose Detect answers at once and whose
// MintCert IGNORES its context: it blocks until the test releases it.
// That is a mint the shutdown's cancel does not stop, which is exactly
// the case a join exists for (a CLI process the kill never reached),
// and it lets a test hold one open across runServe's return.
//
// A fake that honoured its context would prove nothing about the join:
// the cancel would end the mint on its own, and an unjoined runServe
// would usually return after it anyway.
type heldMintCLI struct {
	write bool // whether a released MintCert writes the cert file

	started     chan struct{}
	startOnce   sync.Once
	releaseCh   chan struct{}
	releaseOnce sync.Once
	finished    chan struct{}
	finishOnce  sync.Once
}

func newHeldMintCLI(write bool) *heldMintCLI {
	return &heldMintCLI{
		write:     write,
		started:   make(chan struct{}),
		releaseCh: make(chan struct{}),
		finished:  make(chan struct{}),
	}
}

func (c *heldMintCLI) Detect(context.Context) (servertailscale.NodeInfo, error) {
	return fakeNodeInfo(), nil
}

func (c *heldMintCLI) MintCert(ctx context.Context, _, _, certPath, _ string) error {
	c.startOnce.Do(func() { close(c.started) })
	<-c.releaseCh
	defer c.finishOnce.Do(func() { close(c.finished) })
	if c.write {
		if err := os.WriteFile(certPath, []byte("written by the held mint\n"), 0o600); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// release lets the held mint return. Idempotent.
func (c *heldMintCLI) release() { c.releaseOnce.Do(func() { close(c.releaseCh) }) }

// waitStarted blocks until serve's auto-pilot is inside MintCert. A serve
// that exits first is reported with its exit code, not as a timeout.
func (c *heldMintCLI) waitStarted(t *testing.T, done <-chan int, stderr *safeBuffer) {
	t.Helper()
	select {
	case <-c.started:
	case code := <-done:
		t.Fatalf("serve exited with code %d before its Tailscale auto-pilot reached the mint; stderr=%s",
			code, stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("the Tailscale auto-pilot never reached the mint within 30s; stderr=%s", stderr.String())
	}
}

// TestServeWaitsForAnInFlightTailscaleMint pins the join: runServe must
// not return while a mint its Tailscale auto-pilot started is still
// running, and the mint's write must land BEFORE runServe returns.
//
// Start used to launch the startup mint and the renewer as bare
// goroutines, so runServe returned with the mint in flight. On a host
// running tailscaled that made
// TestServeWiresResolvedConfigPathIntoAdminAndBackups fail in 3 or 4
// runs of 12 with `TempDir RemoveAll cleanup: unlinkat …/data/tls:
// directory not empty`: the test's cleanup was removing the data dir
// while `tailscale cert` wrote into it. CI has no tailscaled and never
// saw it. The fake makes it deterministic on every host.
func TestServeWaitsForAnInFlightTailscaleMint(t *testing.T) {
	cli := newHeldMintCLI(true)
	cfgPath := writeValidConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: "127.0.0.1:0", tailscaleCLI: cli},
			&safeBuffer{}, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	// Registered after the drain, so it runs before it: a failing run
	// lets the mint go first, and the drain then finds a serve that can
	// finish instead of one waiting out the grace.
	t.Cleanup(cli.release)

	cli.waitStarted(t, done, stderr)
	cancel()
	select {
	case <-exited:
		t.Fatalf("runServe returned while the Tailscale mint it started was still running. "+
			"Nothing waited for the auto-pilot, so whatever the mint writes lands after serve "+
			"has returned: into a data dir a test is removing, or after the process exits. "+
			"stderr=%s", stderr.String())
	case <-time.After(time.Second):
	}
	cli.release()
	select {
	case <-exited:
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatalf("runServe did not return once the mint was released; stderr=%s", stderr.String())
	}
	select {
	case <-cli.finished:
	default:
		t.Fatal("runServe returned before the mint it was waiting for had finished")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfgPath), "data", "tls", "tailscale.crt")); err != nil {
		t.Errorf("the released mint's write is missing: %v", err)
	}
	if s := stderr.String(); strings.Contains(s, "did not drain within grace") {
		t.Errorf("the mint was released inside the grace, yet shutdown reported giving up on it; stderr=%s", s)
	}
}

// TestServeGivesUpOnAWedgedTailscaleMintAfterTheGrace pins the other half
// of the join: it is BOUNDED. A mint that never returns, a CLI stuck in
// a way no kill resolves, must cost shutdown the grace and a log line,
// never a hung exit.
func TestServeGivesUpOnAWedgedTailscaleMintAfterTheGrace(t *testing.T) {
	cli := newHeldMintCLI(false)
	cfgPath := writeValidConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: "127.0.0.1:0", tailscaleCLI: cli},
			&safeBuffer{}, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	// Runs before the drain and before the data dir is removed: let the
	// abandoned mint return, and wait for it, so nothing it does can
	// overlap the removal.
	t.Cleanup(func() {
		cli.release()
		select {
		case <-cli.finished:
		case <-time.After(5 * time.Second):
			t.Error("the released mint did not return")
		}
	})

	cli.waitStarted(t, done, stderr)
	cancelled := time.Now()
	cancel()
	select {
	case <-exited:
	case <-time.After(shutdownGrace + 10*time.Second):
		t.Fatalf("runServe hung on a mint that never returns; the wait for it must be "+
			"bounded by the grace. stderr=%s", stderr.String())
	}
	if took := time.Since(cancelled); took < shutdownGrace {
		t.Errorf("runServe returned %v after the cancel, inside the %v grace, with the mint "+
			"still running: it did not wait for the auto-pilot at all", took, shutdownGrace)
	}
	if s := stderr.String(); !strings.Contains(s, "did not drain within grace") {
		t.Errorf("shutdown abandoned a running writer without saying so; stderr=%s", s)
	}
}
