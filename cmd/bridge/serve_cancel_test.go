package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/sqlitetest"
	servertailscale "github.com/acoseac/1-bit-bridge/internal/tailscale"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
	"github.com/acoseac/1-bit-bridge/internal/upnpingest"
)

// runServe's own goroutines run on the serve ctx: the tsnet start and its
// HTTP/3 status query, the UPnP ingest, and the premium cover refetch the
// harvest sweep drives. A shutdown that cancels one of them part-way is a
// pass that STOPPED, not one that failed (ctxerr.WithoutCancellation). Each
// test cancels inside the call the shutdown would land in, and each has a
// twin in which the same call fails on a live context and is still reported.

const (
	msgTsnetStatus    = "Failed to query tsnet status for h3 bind, running HTTP/2 only on tailnet"
	msgArtworkVersion = "artwork version: record"
	msgOrphanSweep    = "UPnP upstream: orphan sweep failed (retries next tick)"
	msgPerServer      = "UPnP upstream: per-server error"
	cancelReleaseMBID = "66666666-6666-4666-8666-666666666666"
)

// TestATsnetStartStoppedByShutdownReportsNothing: the node is still coming
// up (interactive auth can hold it for minutes) when the shutdown lands.
func TestATsnetStartStoppedByShutdownReportsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	node := &blockingTsnetNode{entered: make(chan struct{})}
	var stderr bytes.Buffer
	var up atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		up.Store(bringTsnetUp(ctx, node, &stderr))
	}()
	drainLoopOnCleanup(t, cancel, done, "the tsnet start")

	<-node.entered
	cancel()
	<-done
	if up.Load() {
		t.Fatal("a start the shutdown stopped reported the node up")
	}
	if got := stderr.String(); got != "" {
		t.Errorf("a start the shutdown stopped was reported:\n%s", got)
	}
}

// TestATsnetStartThatFailsIsStillReported is the twin.
func TestATsnetStartThatFailsIsStillReported(t *testing.T) {
	node := &blockingTsnetNode{fail: errors.New("tsnet.Up: backend: NeedsLogin")}
	var stderr bytes.Buffer
	if bringTsnetUp(context.Background(), node, &stderr) {
		t.Fatal("a failed start reported the node up")
	}
	if got := stderr.String(); !strings.Contains(got, "tsnet: bring node up: tsnet.Up: backend: NeedsLogin") {
		t.Errorf("a failed start was not reported, stderr = %q", got)
	}
}

// TestATsnetStatusQueryStoppedByShutdownReportsNothing: the shutdown lands in
// the status query the tailnet HTTP/3 bind makes after the node is up.
func TestATsnetStatusQueryStoppedByShutdownReportsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	node := &blockingTsnetNode{entered: make(chan struct{})}
	rec := loggingtest.Record(t)
	var answered atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, ok := tsnetH3Status(ctx, node)
		answered.Store(ok)
	}()
	drainLoopOnCleanup(t, cancel, done, "the tsnet status query")

	<-node.entered
	cancel()
	<-done
	if answered.Load() {
		t.Fatal("a status query the shutdown stopped reported an answer")
	}
	mustNotReportServe(t, rec, msgTsnetStatus)
}

// TestATsnetStatusQueryThatFailsIsStillReported is the twin.
func TestATsnetStatusQueryThatFailsIsStillReported(t *testing.T) {
	node := &blockingTsnetNode{fail: errors.New("tsnet: local client: connection refused")}
	rec := loggingtest.Record(t)
	if _, ok := tsnetH3Status(context.Background(), node); ok {
		t.Fatal("a failed status query reported an answer")
	}
	mustReportOnceServe(t, rec, msgTsnetStatus)
}

// TestAnArtworkVersionStoppedByShutdownLeavesTheCoverPending: the premium
// bytes have landed, and the shutdown stops the record of their version.
// Settling the cover then would leave clients keyed to the old bytes, so the
// refetch answers with the cancellation instead: the harvest sweep keeps a
// cover whose refetch returned an error pending, and its next pass fetches
// the same bytes and records them.
func TestAnArtworkVersionStoppedByShutdownLeavesTheCoverPending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := openServeCancelStore(t)
	a := atlasCoverRefetcher{premium: landingFetcher{cancel: cancel}, artworkDir: t.TempDir(), store: store, coverSize: 500,
		enrichmentOn: alwaysOn}

	rec := loggingtest.Record(t)
	got, err := a.RefetchPremium(ctx, cancelReleaseMBID)

	mustNotReportServe(t, rec, msgArtworkVersion)
	if got || !errors.Is(err, context.Canceled) {
		t.Errorf("RefetchPremium = (%v, %v), want (false, the cancellation) so the cover stays pending", got, err)
	}
}

// TestAnArtworkVersionThatFailsIsStillReported is the twin, and keeps the
// existing contract for a genuine failure: reported, and the refetch still
// succeeds, because the cover itself is served correctly either way.
func TestAnArtworkVersionThatFailsIsStillReported(t *testing.T) {
	store := openServeCancelStore(t)
	_ = store.Close()
	a := atlasCoverRefetcher{premium: landingFetcher{}, artworkDir: t.TempDir(), store: store, coverSize: 500,
		enrichmentOn: alwaysOn}

	rec := loggingtest.Record(t)
	got, err := a.RefetchPremium(context.Background(), cancelReleaseMBID)

	mustReportOnceServe(t, rec, msgArtworkVersion)
	if !got || err != nil {
		t.Errorf("RefetchPremium = (%v, %v), want (true, nil)", got, err)
	}
}

// TestAnIngestStoppedBeforeItsFirstServerReportsNothing: the shutdown lands
// right after Run's own check, so the orphan sweep's listing fails with the
// cancellation, and no configured server is started on the cancelled context.
func TestAnIngestStoppedBeforeItsFirstServerReportsNothing(t *testing.T) {
	store := openServeCancelStore(t)
	doer := &cancellingSOAP{}
	ing := newCancelTestIngester(t, store, doer)
	ctx := cancelledAfterItsFirstCheck(t)

	rec := loggingtest.Record(t)
	l := &upnpUpstreamLifecycle{log: slog.Default(), adminState: newUPnPAdminState()}
	l.runOneIngest(ctx, ing)

	mustNotReportServe(t, rec, msgOrphanSweep, msgPerServer)
}

// TestAnIngestStoppedInAServerReportsNothingAndKeepsItsLastResult: the
// shutdown lands in a server's first SOAP call. The server's error is only
// the cancellation, so it is neither reported nor recorded: the console keeps
// showing the last result that server really had.
func TestAnIngestStoppedInAServerReportsNothingAndKeepsItsLastResult(t *testing.T) {
	store := openServeCancelStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doer := &cancellingSOAP{cancel: cancel}
	ing := newCancelTestIngester(t, store, doer)
	key := upnpingest.StableServerKey(cancelTestServer)

	rec := loggingtest.Record(t)
	l := &upnpUpstreamLifecycle{log: slog.Default(), adminState: newUPnPAdminState()}
	l.adminState.record(upnpingest.IngestResult{PerServer: []upnpingest.ServerIngestResult{{StableKey: key, Walked: 7}}})
	l.runOneIngest(ctx, ing)

	mustNotReportServe(t, rec, msgPerServer)
	if last := l.adminState.snapshot()[key]; last.Walked != 7 || last.Err != nil {
		t.Errorf("the stopped run replaced the server's last result: %+v", last)
	}
}

// TestAnIngestWhoseServerFailsStillReportsIt is the twin.
func TestAnIngestWhoseServerFailsStillReportsIt(t *testing.T) {
	store := openServeCancelStore(t)
	doer := &cancellingSOAP{fail: errors.New("dial tcp 192.0.2.1:8200: connect: connection refused")}
	ing := newCancelTestIngester(t, store, doer)
	key := upnpingest.StableServerKey(cancelTestServer)

	rec := loggingtest.Record(t)
	l := &upnpUpstreamLifecycle{log: slog.Default(), adminState: newUPnPAdminState()}
	l.runOneIngest(context.Background(), ing)

	mustReportOnceServe(t, rec, msgPerServer)
	if last := l.adminState.snapshot()[key]; last.Err == nil {
		t.Error("a server that failed on a live context was not recorded as failed")
	}
}

// TestAnIngestWhoseOrphanSweepFailsStillReportsIt is the orphan sweep's twin:
// its listing fails on a live context, against a store that is closed.
func TestAnIngestWhoseOrphanSweepFailsStillReportsIt(t *testing.T) {
	store := openServeCancelStore(t)
	ing := newCancelTestIngester(t, store, &cancellingSOAP{fail: errors.New("connection refused")})
	_ = store.Close()

	rec := loggingtest.Record(t)
	l := &upnpUpstreamLifecycle{log: slog.Default(), adminState: newUPnPAdminState()}
	l.runOneIngest(context.Background(), ing)

	mustReportOnceServe(t, rec, msgOrphanSweep)
}

// TestARescanStoppedByShutdownKeepsTheServersLastResult is the ingest-stopped
// test through the console's "Rescan now", which runs on the lifecycle's
// context and records its own result.
func TestARescanStoppedByShutdownKeepsTheServersLastResult(t *testing.T) {
	store := openServeCancelStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, key := rescanAdapter(t, store, ctx, &cancellingSOAP{cancel: cancel})
	a.state.record(upnpingest.IngestResult{PerServer: []upnpingest.ServerIngestResult{{StableKey: key, Walked: 7}}})

	if err := a.ForceRescan(context.Background(), ""); err != nil {
		t.Fatalf("ForceRescan: %v", err)
	}
	a.ingestWg.Wait()

	if last := a.state.snapshot()[key]; last.Walked != 7 || last.Err != nil {
		t.Errorf("the stopped rescan replaced the server's last result: %+v", last)
	}
}

// TestARescanWhoseServerFailsRecordsIt is the twin.
func TestARescanWhoseServerFailsRecordsIt(t *testing.T) {
	store := openServeCancelStore(t)
	a, key := rescanAdapter(t, store, context.Background(),
		&cancellingSOAP{fail: errors.New("dial tcp 192.0.2.1:8200: connect: connection refused")})

	if err := a.ForceRescan(context.Background(), ""); err != nil {
		t.Fatalf("ForceRescan: %v", err)
	}
	a.ingestWg.Wait()

	if last := a.state.snapshot()[key]; last.Err == nil {
		t.Error("a server that failed on a live context was not recorded as failed")
	}
}

// rescanAdapter is a console adapter whose rescans run on bgCtx through an
// ingester over doer, and the key its one server is recorded under.
func rescanAdapter(t *testing.T, store *manifest.Store, bgCtx context.Context, doer *cancellingSOAP) (*upnpAdminAdapter, string) {
	t.Helper()
	return &upnpAdminAdapter{
		cfgHolder: runtimeCfgFor(t, newUPnPTestCfg(t, cancelTestServer)),
		cache:     upnp.NewServerCache(),
		store:     store,
		ingester:  newCancelTestIngester(t, store, doer),
		state:     newUPnPAdminState(),
		bgCtx:     bgCtx,
		ingestWg:  &sync.WaitGroup{},
	}, upnpingest.StableServerKey(cancelTestServer)
}

// blockingTsnetNode is a tsnet node whose Start and Status block until
// their context ends and then fail with it, as the real ones do, or, with
// fail set, fail with it at once.
type blockingTsnetNode struct {
	entered chan struct{} // closed when a call is waiting
	fail    error
}

func (n *blockingTsnetNode) Start(ctx context.Context) error {
	if n.fail != nil {
		return n.fail
	}
	close(n.entered)
	<-ctx.Done()
	return fmt.Errorf("tsnet: bring node up: tsnet.Up: %w", ctx.Err())
}

func (n *blockingTsnetNode) Status(ctx context.Context) (*ipnstate.Status, error) {
	if n.fail != nil {
		return nil, n.fail
	}
	close(n.entered)
	<-ctx.Done()
	return nil, fmt.Errorf("tsnet: status: %w", ctx.Err())
}

// landingFetcher is a premium fetcher that lands a cover at the path it is
// handed and, when cancel is set, then cancels the pass, as a shutdown
// arriving just after the write would.
type landingFetcher struct{ cancel context.CancelFunc }

func (landingFetcher) TryCache(context.Context, string, string, int) bool { return false }

func (f landingFetcher) RefetchPremium(_ context.Context, path, _ string, _ int) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte{0xFF, 0xD8, 0xFF, 0xE0, 'p', 'r', 'e', 'm'}, 0o644); err != nil {
		return false, err
	}
	if f.cancel != nil {
		f.cancel()
	}
	return true, nil
}

// cancelTestServer is the one upstream the ingest tests configure.
var cancelTestServer = config.UPnPUpstreamServerConfig{Name: "2Go", UDN: "uuid:cancel-test", PathPrefix: "Chord 2Go"}

// cancellingSOAP answers SOAP calls as a real transport does on a cancelled
// context, with the cancellation. When cancel is set, its first call is
// where the shutdown lands; when fail is set, every call fails with it.
type cancellingSOAP struct {
	cancel context.CancelFunc
	fail   error
}

func (c *cancellingSOAP) Do(ctx context.Context, _ *http.Request) (*http.Response, error) {
	if c.fail != nil {
		return nil, c.fail
	}
	if c.cancel != nil {
		c.cancel()
	}
	return nil, fmt.Errorf("soap: %w", ctx.Err())
}

// cancelTestResolver always resolves the configured server.
type cancelTestResolver struct{}

func (cancelTestResolver) ResolveControlURL(context.Context, config.UPnPUpstreamServerConfig) (string, discovery.DialApproval, error) {
	return "http://192.0.2.1:8200/ctl/ContentDir", discovery.DialApproval{}, nil
}

// newCancelTestIngester builds an ingester over doer for cancelTestServer.
func newCancelTestIngester(t *testing.T, store *manifest.Store, doer *cancellingSOAP) *upnpingest.Ingester {
	t.Helper()
	cfg := config.UPnPUpstreamConfig{Enabled: true, Servers: []config.UPnPUpstreamServerConfig{cancelTestServer}}
	ing, err := upnpingest.NewIngester(cfg, upnp.NewContentDirectoryClient(doer), cancelTestResolver{}, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ing
}

// cancelledAfterItsFirstCheck returns a context whose first Err answers nil
// and cancels it, so a pass that asks its context once before starting then
// runs on a context shutdown has just cancelled.
func cancelledAfterItsFirstCheck(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &cancelOnFirstErr{Context: ctx, cancel: cancel}
}

// cancelOnFirstErr is the context behind cancelledAfterItsFirstCheck. It
// embeds the context it cancels, which is how every derived context is
// built.
type cancelOnFirstErr struct {
	context.Context
	cancel context.CancelFunc
	asked  atomic.Bool
}

// Err answers nil the first time, cancelling as it does, and the embedded
// context's answer after that.
func (c *cancelOnFirstErr) Err() error {
	if c.asked.CompareAndSwap(false, true) {
		c.cancel()
		return nil
	}
	return c.Context.Err()
}

// openServeCancelStore opens a manifest store closed at cleanup.
func openServeCancelStore(t *testing.T) *manifest.Store {
	t.Helper()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// mustNotReportServe fails the test for each msg logged at Warn or above.
func mustNotReportServe(t *testing.T, rec *loggingtest.Recorder, msgs ...string) {
	t.Helper()
	for _, m := range msgs {
		if got := rec.Failures(m); len(got) != 0 {
			t.Errorf("a pass that shutdown stopped reported %q:\n%s", m, strings.Join(got, "\n"))
		}
	}
}

// mustReportOnceServe fails the test for each msg not logged exactly once
// at Warn or above.
func mustReportOnceServe(t *testing.T, rec *loggingtest.Recorder, msgs ...string) {
	t.Helper()
	for _, m := range msgs {
		if got := rec.Failures(m); len(got) != 1 {
			t.Errorf("a failure on a live context logged %q %d times, want 1:\n%s", m, len(got), strings.Join(got, "\n"))
		}
	}
}

// TestATsnetListenAfterTheShutdownBeganOpensNothing: the shutdown landed
// while the goroutine was still bringing the tailnet up (in its HTTP/3
// status query, say), so no listener is opened and nothing is reported.
func TestATsnetListenAfterTheShutdownBeganOpensNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	node := &listeningTsnetNode{}
	var stderr bytes.Buffer
	if lis, ok := tsnetListen(ctx, node, "127.0.0.1:0", &stderr); ok || lis != nil {
		t.Fatal("a listen after the shutdown began opened a listener")
	}
	if n := node.calls.Load(); n != 0 {
		t.Errorf("ListenTLS was called %d time(s) after the shutdown began", n)
	}
	if got := stderr.String(); got != "" {
		t.Errorf("a listen the shutdown got to first was reported: %q", got)
	}
}

// TestATsnetListenFailingAsTheShutdownClosesTheNodeReportsNothing: the
// shutdown begins while ListenTLS runs and closes the node under it. The
// error that comes back is the node's and carries no cancellation, so it is
// the context that shows the shutdown caused it.
func TestATsnetListenFailingAsTheShutdownClosesTheNodeReportsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node := &listeningTsnetNode{err: errors.New("tsnet: server closed"), onListen: cancel}
	var stderr bytes.Buffer
	if _, ok := tsnetListen(ctx, node, "127.0.0.1:0", &stderr); ok {
		t.Fatal("a failed listen reported the listener open")
	}
	if got := stderr.String(); got != "" {
		t.Errorf("a listen the shutdown closed the node under was reported: %q", got)
	}
}

// TestATsnetListenThatSucceedsAsTheShutdownBeginsClosesTheListener: the
// shutdown lands while ListenTLS runs, and the listen succeeds anyway (the
// wrapper takes no context). The listener is closed rather than returned for
// the goroutine to serve on (CodeRabbit, #1005).
func TestATsnetListenThatSucceedsAsTheShutdownBeginsClosesTheListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node := &listeningTsnetNode{onListen: cancel}
	var stderr bytes.Buffer
	if lis, ok := tsnetListen(ctx, node, "127.0.0.1:0", &stderr); ok || lis != nil {
		t.Fatal("a listener opened as the shutdown began was returned to be served")
	}
	if node.opened == nil {
		t.Fatal("precondition: ListenTLS opened no listener")
	}
	tcp, ok := node.opened.(*net.TCPListener)
	if !ok {
		t.Fatalf("the fake opened a %T, want a *net.TCPListener", node.opened)
	}
	// A deadline, so a listener left open fails the test instead of
	// blocking it in Accept.
	_ = tcp.SetDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := tcp.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("the listener was left open: Accept = %v", err)
	}
	if got := stderr.String(); got != "" {
		t.Errorf("a listen the shutdown overtook was reported: %q", got)
	}
}

// TestATsnetListenThatFailsIsStillReported is the twin: the listen fails on
// a live context and is reported, as before.
func TestATsnetListenThatFailsIsStillReported(t *testing.T) {
	node := &listeningTsnetNode{err: errors.New("listen tcp :443: address already in use")}
	var stderr bytes.Buffer
	if _, ok := tsnetListen(context.Background(), node, "127.0.0.1:0", &stderr); ok {
		t.Fatal("a failed listen reported the listener open")
	}
	if got := stderr.String(); !strings.Contains(got, "tsnet: ListenTLS: listen tcp :443: address already in use") {
		t.Errorf("a failed listen was not reported: %q", got)
	}
}

// TestATsnetListenOnALiveContextOpensTheListener: the ordinary case.
func TestATsnetListenOnALiveContextOpensTheListener(t *testing.T) {
	node := &listeningTsnetNode{}
	var stderr bytes.Buffer
	lis, ok := tsnetListen(context.Background(), node, "127.0.0.1:0", &stderr)
	if !ok || lis == nil {
		t.Fatalf("tsnetListen = (%v, %v), want the listener", lis, ok)
	}
	_ = lis.Close()
	if got := stderr.String(); got != "" {
		t.Errorf("a listen that succeeded reported: %q", got)
	}
}

// TestAServeStoppedInItsUpscaleSeedExitsCleanly: the shutdown lands while
// serve, not yet up, seeds the upscale target on a fresh database. That is
// a requested stop, so serve exits 0 and reports no failed seed. A boot
// test's early cancel landed there on #1001's macOS leg: `serve exit code
// = 1, want 0; stderr=seed upscale target: context canceled`.
//
// The seed's INSERT is parked (sqlitetest) and the cancel lands inside it.
// A context cancelled before runServe starts cannot reach the seed at all:
// the GetUpscaleTarget read before it fails first, and the seed is skipped.
//
// The Park is armed with ArmUntil, never Arm: its first wait covers serve's
// boot up to the seed, and its last the whole teardown after the cancel,
// neither of which a starved host keeps to Arm's 10 s per wait ("no
// statement compared a key within 10s" on CI's Windows leg, gate run
// 36524079308, backlog B63).
func TestAServeStoppedInItsUpscaleSeedExitsCleanly(t *testing.T) {
	cfgPath := writeValidConfig(t)
	parkUpscaleSeed(t, filepath.Join(filepath.Dir(cfgPath), "data"))
	park := sqlitetest.ArmUntil(t, serveGiveUpTime(t))
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: "127.0.0.1:0", tailscaleCLI: noTailscaleCLI()},
			&safeBuffer{}, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	// Registered after the drain, so it runs before it: a failing run lets
	// the parked seed go, and the drain then finds a serve that can finish.
	// Arm's own Disarm runs after the drain, which would wait on a serve
	// held inside the collation until the drain gives up.
	t.Cleanup(park.Disarm)

	if !park.WaitUnless(t, exited) {
		t.Fatalf("serve exited with code %d before its upscale seed compared a key; stderr=%s",
			<-done, stderr.String())
	}
	cancel()
	park.ReleaseUntil(t, exited)
	if code := <-done; code != 0 {
		t.Errorf("a serve stopped in its upscale seed exited %d, want 0; stderr=%s", code, stderr.String())
	}
	if s := stderr.String(); strings.Contains(s, msgUpscaleSeed) {
		t.Errorf("a startup the shutdown stopped reported its upscale seed as failed; stderr=%s", s)
	}
}

// TestAServeWhoseUpscaleSeedFailsStillExitsWithAnError is the twin: the
// seed fails on a live context, through a trigger that refuses it, and
// serve still refuses to start and says why.
func TestAServeWhoseUpscaleSeedFailsStillExitsWithAnError(t *testing.T) {
	cfgPath := writeValidConfig(t)
	refuseUpscaleSeed(t, filepath.Join(filepath.Dir(cfgPath), "data"))
	stderr := &safeBuffer{}
	code := runServe(context.Background(),
		serveOpts{configPath: cfgPath, addrOverride: "127.0.0.1:0", tailscaleCLI: noTailscaleCLI()},
		&safeBuffer{}, stderr)
	if code != 1 {
		t.Errorf("a serve whose upscale seed failed exited %d, want 1; stderr=%s", code, stderr.String())
	}
	if s := stderr.String(); !strings.Contains(s, msgUpscaleSeed) {
		t.Errorf("a failed upscale seed was not reported; stderr=%s", s)
	}
}

// msgUpscaleSeed opens the line serve prints when the upscale seed fails.
const msgUpscaleSeed = "seed upscale target:"

// msgUpscaleSeedTxDone is that line for a seed whose commit found its
// transaction already ended, the line backlog B662's macOS run printed.
const msgUpscaleSeedTxDone = msgUpscaleSeed + " " + "sql: transaction has already been committed or rolled back\n"

// TestAServeStoppedAtItsUpscaleSeedsCommitExitsCleanly: the shutdown lands
// after the seed's last statement has run and before its commit. That is a
// requested stop, so serve exits 0 and reports no failed seed. On macOS,
// TestServeRunsThePlayersWatcherWhileThePlayerIsOn, which stops serve once
// its hotplug watcher has read once (the watcher starts before the seed),
// failed there (run 41480956932): `serve exit code 1; stderr=seed upscale
// target: sql: transaction has already been committed or rolled back`
// (backlog B662).
//
// Parking inside the seed's INSERT is the stop the test above makes, and
// comes back as context.Canceled. This one holds the seed between its last
// statement and its commit (serveOpts.upscaleTargetBeforeCommit), cancels,
// and waits at the driver, on the transaction's own connection, for the
// rollback database/sql's own goroutine makes of the transaction once its
// context is done (Tx.awaitDone; stopAndWaitForTheRollback). Only then does
// the seed reach tx.Commit(), which answers sql.ErrTxDone from its own state,
// never calling the driver's Commit.
func TestAServeStoppedAtItsUpscaleSeedsCommitExitsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := holdServeAtItsUpscaleSeedsCommit(t, ctx, cancel)
	h.stopAndWaitForTheRollback(t, cancel)
	h.release(nil)
	code := h.exitCode(t)
	if code != 0 {
		t.Errorf("a serve stopped at its upscale seed's commit exited %d, want 0; stderr=%q", code, h.stderr.String())
	}
	if s := h.stderr.String(); strings.Contains(s, msgUpscaleSeed) {
		t.Errorf("a startup the shutdown stopped reported its upscale seed as failed; stderr=%q", s)
	}
}

// TestAServeStoppedAtItsUpscaleSeedsCommitWaitsForTheSeedsOwnRollback proves
// stopAndWaitForTheRollback (backlog B869): with the seed's ROLLBACK held at
// the driver, once database/sql has marked the transaction done and before
// SQLite rolls it back, the wait does not answer, with nothing else rolled
// back on serve's database or once a reader's transaction has rolled back on
// another connection to it (sqlitetest's WaitProof), and answers once the
// held rollback has run; serve then exits 0, reporting no failed seed.
func TestAServeStoppedAtItsUpscaleSeedsCommitWaitsForTheSeedsOwnRollback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := holdServeAtItsUpscaleSeedsCommit(t, ctx, cancel)
	proof := h.rollbacks.HoldRollbackOf(t, h.tx).BesideAReader(t)
	h.stopAndWaitForTheRollback(t, cancel)
	proof.Returned(t)
	h.release(nil)
	if code := h.exitCode(t); code != 0 {
		t.Errorf("a serve stopped at its upscale seed's commit exited %d, want 0; stderr=%q", code, h.stderr.String())
	}
	if s := h.stderr.String(); strings.Contains(s, msgUpscaleSeed) {
		t.Errorf("a startup the shutdown stopped reported its upscale seed as failed; stderr=%q", s)
	}
}

// TestAServeWhoseUpscaleSeedFindsItsTransactionEndedStillFails: the same
// sql.ErrTxDone from the seed's commit, on the same seed path, with serve's
// stop context live, is a failed start: the transaction ended before its
// commit for a reason that is no stop (here a rollback made at the hold, as a
// transaction used twice would end it), and serve exits 1 saying so.
func TestAServeWhoseUpscaleSeedFindsItsTransactionEndedStillFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := holdServeAtItsUpscaleSeedsCommit(t, ctx, cancel)
	if err := h.tx.Rollback(); err != nil {
		t.Fatalf("rolling the held seed back: %v", err)
	}
	h.release(nil)
	if code := h.exitCode(t); code != 1 {
		t.Errorf("a serve whose seed found its transaction ended exited %d, want 1; stderr=%q", code, h.stderr.String())
	}
	if s := h.stderr.String(); !strings.Contains(s, msgUpscaleSeedTxDone) {
		t.Errorf("the failed seed was not reported as its commit's error; stderr=%q, want it to hold %q", s, msgUpscaleSeedTxDone)
	}
}

// TestAServePastItsDeadlineAtItsUpscaleSeedsCommitStillFails: serve's
// context ending at a deadline, not a cancel, is no requested stop
// (ctxerr.WithoutCancellation's rule), so the sql.ErrTxDone its rollback
// leaves at the seed's commit is a failed start, exit 1.
func TestAServePastItsDeadlineAtItsUpscaleSeedsCommitStillFails(t *testing.T) {
	ctx := newPassableDeadline()
	h := holdServeAtItsUpscaleSeedsCommit(t, ctx, ctx.pass)
	h.stopAndWaitForTheRollback(t, ctx.pass)
	h.release(nil)
	if code := h.exitCode(t); code != 1 {
		t.Errorf("a serve past its deadline at its seed's commit exited %d, want 1; stderr=%q", code, h.stderr.String())
	}
	if s := h.stderr.String(); !strings.Contains(s, msgUpscaleSeedTxDone) {
		t.Errorf("the failed seed was not reported as its commit's error; stderr=%q, want it to hold %q", s, msgUpscaleSeedTxDone)
	}
}

// TestAServeWhoseUpscaleSeedFailsAsItIsStoppedStillFails: an error that is no
// stop's, arriving after the stop and its rollback have both landed, is a
// failed start: the stop context being cancelled is not enough.
func TestAServeWhoseUpscaleSeedFailsAsItIsStoppedStillFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := holdServeAtItsUpscaleSeedsCommit(t, ctx, cancel)
	h.stopAndWaitForTheRollback(t, cancel)
	h.release(errSeedRefusedByTheTest)
	if code := h.exitCode(t); code != 1 {
		t.Errorf("a serve whose seed failed as it stopped exited %d, want 1; stderr=%q", code, h.stderr.String())
	}
	if s, want := h.stderr.String(), msgUpscaleSeed+" "+errSeedRefusedByTheTest.Error()+"\n"; !strings.Contains(s, want) {
		t.Errorf("the failed seed was not reported; stderr=%q, want it to hold %q", s, want)
	}
}

// errSeedRefusedByTheTest is the seed's error in the test above: one no
// stop leaves.
var errSeedRefusedByTheTest = errors.New("refused by the test, as a failing disk would")

// heldSeedServe is a serve held inside its upscale seed's transaction, once
// both keys are written and before its commit.
type heldSeedServe struct {
	tx        *sql.Tx
	rollbacks *sqlitetest.RollbackWatch
	answer    chan error
	once      sync.Once
	exited    <-chan struct{}
	done      <-chan int
	stderr    *safeBuffer
}

// holdServeAtItsUpscaleSeedsCommit boots serve on a fresh config under ctx,
// whose stop is stop, and returns once its upscale seed is held between its
// last statement and its commit, with a watch on the rollbacks of serve's
// database armed.
func holdServeAtItsUpscaleSeedsCommit(t *testing.T, ctx context.Context, stop context.CancelFunc) *heldSeedServe {
	t.Helper()
	cfgPath := writeValidConfig(t)
	dbPath := manifest.DefaultDBPath(filepath.Join(filepath.Dir(cfgPath), "data"))
	atCommit := make(chan *sql.Tx, 1)
	stderr := &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	h := &heldSeedServe{
		rollbacks: sqlitetest.WatchRollbacksUntil(t, dbPath, serveGiveUpTime(t)),
		answer:    make(chan error, 1),
		exited:    exited, done: done, stderr: stderr,
	}
	opts := serveOpts{
		configPath: cfgPath, addrOverride: "127.0.0.1:0", tailscaleCLI: noTailscaleCLI(),
		upscaleTargetBeforeCommit: func(tx *sql.Tx) error {
			atCommit <- tx
			return <-h.answer
		},
	}
	go func() {
		defer close(exited)
		done <- runServe(ctx, opts, &safeBuffer{}, stderr)
	}()
	drainServeOnCleanup(t, stop, exited, done, stderr)
	// Registered after the drain, so it runs before it: a test that fails
	// while the seed is held lets it go, and the drain then finds a serve
	// that can finish.
	t.Cleanup(func() { h.release(nil) })
	select {
	case h.tx = <-atCommit:
	case <-exited:
		t.Fatalf("serve exited with code %d before its upscale seed reached its commit; stderr=%s", <-done, stderr.String())
	case <-serveGiveUp(t):
		t.Fatalf("serve's upscale seed never reached its commit; stderr=%s\nserve's goroutines:\n%s", stderr.String(), serveStacks())
	}
	return h
}

// stopAndWaitForTheRollback stops serve and waits until database/sql has
// rolled the held seed's transaction back at the driver, on the seed's own
// connection: the rollback is taken before the stop (RollbackOf), so one that
// runs before the wait begins is kept, and has run once the driver's ROLLBACK
// there has rolled the seed back. The seed holding the store's writer lock
// keeps another write transaction from rolling back meanwhile, not another
// connection's read transaction, and the seed's transaction answers
// sql.ErrTxDone from the moment database/sql marks it done, before it calls
// the driver's Rollback, so neither a rollback counted on serve's database
// nor that answer says that the seed's own has run (backlog B869). A serve
// that exits first fails the test, and the wait gives up at the watch's
// instant.
func (h *heldSeedServe) stopAndWaitForTheRollback(t *testing.T, stop context.CancelFunc) {
	t.Helper()
	rollback := h.rollbacks.RollbackOf(t, h.tx)
	stop()
	if !rollback.WaitUnless(t, h.exited) {
		t.Fatalf("serve exited while its upscale seed was held; stderr=%s", h.stderr.String())
	}
}

// release lets the held seed go on to its commit, its hold answering err.
func (h *heldSeedServe) release(err error) {
	h.once.Do(func() { h.answer <- err })
}

// exitCode waits for serve to exit and returns its code.
func (h *heldSeedServe) exitCode(t *testing.T) int {
	t.Helper()
	select {
	case <-h.exited:
		return <-h.done
	case <-serveGiveUp(t):
		t.Fatalf("serve did not exit after its upscale seed was let go; stderr=%s\nserve's goroutines:\n%s", h.stderr.String(), serveStacks())
		return -1
	}
}

// passableDeadline is a context whose deadline passes when the test says so:
// pass closes Done and makes Err context.DeadlineExceeded, as a deadline
// passing does, at a moment the test chooses. Its Deadline is a long way off.
type passableDeadline struct {
	context.Context // Background, for Value
	done            chan struct{}
	passed          atomic.Bool
	once            sync.Once
	deadline        time.Time
}

func newPassableDeadline() *passableDeadline {
	return &passableDeadline{Context: context.Background(), done: make(chan struct{}), deadline: time.Now().Add(24 * time.Hour)}
}

func (c *passableDeadline) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *passableDeadline) Done() <-chan struct{}       { return c.done }

func (c *passableDeadline) Err() error {
	if c.passed.Load() {
		return context.DeadlineExceeded
	}
	return nil
}

// pass is the deadline passing.
func (c *passableDeadline) pass() {
	c.once.Do(func() {
		c.passed.Store(true)
		close(c.done)
	})
}

// noTailscaleCLI is a Tailscale CLI with no node behind it, so a boot test's
// auto-pilot finds nothing to mint and never reaches the host's CLI.
func noTailscaleCLI() tailscaleCLI {
	return scriptedCLI{
		detect: func(context.Context) (servertailscale.NodeInfo, error) { return servertailscale.NodeInfo{}, nil },
		mint: func(context.Context, string, string) error {
			return errors.New("noTailscaleCLI: nothing to mint")
		},
	}
}

// refuseUpscaleSeed migrates the manifest database serve will open under
// dataDir and adds a trigger that refuses the upscale target's first write,
// the way a failing disk would.
func refuseUpscaleSeed(t *testing.T, dataDir string) {
	t.Helper()
	db := openMigratedServeDB(t, dataDir)
	if _, err := db.Exec(`CREATE TRIGGER refuse_upscale_seed BEFORE INSERT ON scan_state
		WHEN NEW.k = '` + manifest.UpscaleTargetRateKey + `'
		BEGIN SELECT RAISE(ABORT, 'refused by the test'); END`); err != nil {
		t.Fatal(err)
	}
}

// parkUpscaleSeed migrates the manifest database serve will open under
// dataDir and indexes the upscale target's keys under sqlitetest's
// collation, beside one neighbour row, so the seed's INSERT compares a key
// and parks while a Park is armed. The partial index leaves every other
// scan_state write out of it.
func parkUpscaleSeed(t *testing.T, dataDir string) {
	t.Helper()
	db := openMigratedServeDB(t, dataDir)
	keys := "'" + manifest.UpscaleTargetRateKey + "', '" + manifest.UpscaleTargetBitsKey + "', 'sqlitetest_neighbour'"
	for _, stmt := range []string{
		"CREATE INDEX park_upscale_seed ON scan_state(k COLLATE " + sqlitetest.Collation + ") WHERE k IN (" + keys + ")",
		"INSERT INTO scan_state(k, v) VALUES('sqlitetest_neighbour', '')",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// openMigratedServeDB migrates the manifest database serve will open under
// dataDir and returns a plain handle to it, closed when the test ends.
func openMigratedServeDB(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := manifest.DefaultDBPath(dataDir)
	store, err := manifest.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// listeningTsnetNode is a tsnet node whose ListenTLS counts its calls, runs
// onListen first (a shutdown landing mid-call), then fails with err, or
// opens a loopback listener when err is nil and keeps it in opened.
type listeningTsnetNode struct {
	err      error
	onListen func()
	calls    atomic.Int32
	opened   net.Listener
}

func (n *listeningTsnetNode) ListenTLS(addr string) (net.Listener, error) {
	n.calls.Add(1)
	if n.onListen != nil {
		n.onListen()
	}
	if n.err != nil {
		return nil, n.err
	}
	lis, err := net.Listen("tcp", addr)
	n.opened = lis
	return lis, err
}
