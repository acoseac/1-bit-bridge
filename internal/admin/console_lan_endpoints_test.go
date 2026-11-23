package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The console's endpoint list is the API's (backlog B989, its third round):
// the console waits for it on the request's context, and the API's shared
// Tailscale status read behind it, which /v1/health takes too, runs to its end
// on no request's context.

// tailnetHeldTestName is the embedded node's name the held status answers.
const tailnetHeldTestName = "box.example.ts.net"

// heldTailnet is the embedded node's status read, held until the case lets it
// go, counting the reads asked and those that returned; back is closed once
// the first read has returned.
type heldTailnet struct {
	asked   chan struct{}
	release chan struct{}
	back    chan struct{}
	once    sync.Once
	backed  sync.Once

	mu       sync.Mutex
	calls    int
	returned int
}

// newHeldTailnet is a status read held until release is closed.
func newHeldTailnet() *heldTailnet {
	return &heldTailnet{asked: make(chan struct{}), release: make(chan struct{}), back: make(chan struct{})}
}

func (h *heldTailnet) Status() admin.TailscaleStatus {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	h.once.Do(func() { close(h.asked) })
	<-h.release
	h.mu.Lock()
	h.returned++
	h.mu.Unlock()
	h.backed.Do(func() { close(h.back) })
	return admin.TailscaleStatus{BackendState: "Running", MagicDNSName: tailnetHeldTestName + "."}
}

func (h *heldTailnet) RefreshNow(context.Context) admin.TailscaleStatus {
	panic("a status read asked the provider to refresh")
}

func (h *heldTailnet) counts() (calls, returned int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls, h.returned
}

// consoleBesideTheAPI is a loopback console whose endpoint list is the API
// server's, over one config: the API's listen port is 7788 and its Tailscale
// mode tsnet, so the list asks the API's shared status read.
func consoleBesideTheAPI(t *testing.T, status admin.TailscaleProvider) (*admin.Server, *api.Server) {
	t.Helper()
	srv, apiSrv, _ := consoleBesideTheAPIIssuing(t, status, nil)
	return srv, apiSrv
}

// consoleBesideTheAPIIssuing is consoleBesideTheAPI with codes issuing the
// pairing links' one-time codes, and the device tokens' store it writes.
func consoleBesideTheAPIIssuing(t *testing.T, status admin.TailscaleProvider, codes admin.PairingCodeIssuer) (*admin.Server, *api.Server, *auth.Store) {
	t.Helper()
	tmp := t.TempDir()
	lib := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		LibraryRoots: []string{lib}, ListenAddress: "127.0.0.1:7788", AdminAddress: "127.0.0.1:7789",
		DataDir: filepath.Join(tmp, "data"), ScanIntervalSec: 3600, LibraryName: "Endpoints",
		Tailscale: config.TailscaleConfig{Mode: "tsnet"},
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(tmp, "bridge.yaml")
	lock, err := config.LockFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Save(lock)
	lock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	store, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	tokens, err := auth.OpenStore(filepath.Join(cfg.DataDir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	apiSrv := api.New(cfg, tokens, nil, "")
	apiSrv.WithTailscaleStatus(status)
	srv, err := admin.New(admin.Deps{
		CfgHolder: config.NewRuntimeConfig(cfg), CfgPath: cfgPath, Auth: tokens, Manifest: store,
		Scanner: manifest.NewScanner(cfg.LibraryRoots, store, ""), Resolver: bridgefs.New(cfg.LibraryRoots),
		StartedAt: time.Now(), Restart: func() {}, Endpoints: apiSrv.ReachableEndpoints, PairingCodes: codes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, apiSrv, tokens
}

// askEndpoints serves GET /api/endpoints on ctx, on a goroutine of its own, and
// answers on the channel it returns once the handler has returned.
func askEndpoints(srv *admin.Server, ctx context.Context) <-chan int {
	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/api/endpoints", nil).WithContext(ctx)
		req.RemoteAddr, req.Host = "127.0.0.1:51000", "127.0.0.1:7789"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		done <- rec.Code
	}()
	return done
}

// TestACancelledEndpointsWaitLeavesTheHealthReadWhole: the console's wait for
// its endpoint list ends with its request's context, the first request's in
// front of a held Tailscale status read and the second's behind it, while the
// read itself, the API's shared one, runs to its end on no request's context:
// once let go it answers once for every caller, and /v1/health's list carries
// the tailnet's name it found, never the empty status of a read cut short.
// Neither request ends before the second's read has found the API's status
// lock held, which the API tells the case just before that read calls Lock
// (OnTailscaleStatusLockFoundHeldForTest), the first's read holding the lock
// in the held status until the case lets it go: the one place that read can
// wait (backlog B997), so the case cannot pass without the contention it is
// about. The hook says the lock was found held, not that the second read has
// begun to wait for it.
func TestACancelledEndpointsWaitLeavesTheHealthReadWhole(t *testing.T) {
	held := newHeldTailnet()
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(held.release) }) }
	t.Cleanup(letGo)
	srv, apiSrv := consoleBesideTheAPI(t, held)
	var found atomic.Int32
	foundHeld := make(chan struct{})
	var foundOnce sync.Once
	apiSrv.OnTailscaleStatusLockFoundHeldForTest(func() {
		found.Add(1)
		foundOnce.Do(func() { close(foundHeld) })
	})
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	first := askEndpoints(srv, ctx1)
	select {
	case <-held.asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the console's endpoint list never asked the Tailscale status")
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	second := askEndpoints(srv, ctx2)
	select {
	case <-foundHeld:
	case <-time.After(10 * time.Second):
	}
	if n := found.Load(); n != 1 {
		t.Fatalf("before the cancellations %d reads found the API's status lock held, want the second request's", n)
	}
	for i, w := range []struct {
		cancel context.CancelFunc
		done   <-chan int
	}{{cancel2, second}, {cancel1, first}} {
		w.cancel()
		select {
		case <-w.done:
		case <-time.After(time.Second):
			t.Fatalf("request %d: the console's wait for its endpoint list did not end with its request's context", 2-i)
		}
	}
	if calls, returned := held.counts(); calls != 1 || returned != 0 {
		t.Fatalf("with both waits ended the status was asked %d times and returned %d, want one read still held", calls, returned)
	}
	letGo()
	select {
	case <-held.back:
	case <-time.After(10 * time.Second):
		t.Fatal("the shared status read never returned once let go")
	}
	want := "https://" + tailnetHeldTestName + ":7788"
	if eps := apiSrv.HealthEndpoints(); !slices.Contains(eps, want) {
		t.Fatalf("/v1/health's endpoints %v do not carry %s: the read the cancelled waits left was not cached whole", eps, want)
	}
	if calls, _ := held.counts(); calls != 1 {
		t.Fatalf("the status was asked %d times, want the one shared read", calls)
	}
}
