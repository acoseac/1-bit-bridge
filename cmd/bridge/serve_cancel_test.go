package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
	"github.com/acoseac/1-bit-bridge/internal/upnpingest"
	"tailscale.com/ipn/ipnstate"
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
	a := atlasCoverRefetcher{premium: landingFetcher{cancel: cancel}, artworkDir: t.TempDir(), store: store, coverSize: 500}

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
	a := atlasCoverRefetcher{premium: landingFetcher{}, artworkDir: t.TempDir(), store: store, coverSize: 500}

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

func (cancelTestResolver) ResolveControlURL(context.Context, config.UPnPUpstreamServerConfig) (string, error) {
	return "http://192.0.2.1:8200/ctl/ContentDir", nil
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
