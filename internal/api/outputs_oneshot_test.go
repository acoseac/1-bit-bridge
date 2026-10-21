package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/mpd"
)

// TestAOneShotBuilderClosesItsSession: bridge doctor builds the outputs
// body once, outside a server, and closes the builder. The MPD session
// ends then, not after the idle minute, and no idle close stays armed.
func TestAOneShotBuilderClosesItsSession(t *testing.T) {
	clock := &manualClock{now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	var dials, closed atomic.Int32
	cfg := quietOutputs("linux", "on")
	cfg.Now, cfg.AfterFunc = clock.Now, clock.AfterFunc
	cfg.Dial = func(context.Context, string, string) (outputSession, error) {
		dials.Add(1)
		return &fakeOutputSession{version: "0.24.0", state: "play", closed: &closed}, nil
	}
	lo := NewLocalOutputs(cfg)
	ctx := context.Background()
	if !lo.Active(ctx) {
		t.Fatal("not active")
	}
	body, err := lo.Response(ctx)
	if err != nil || len(body.Outputs) != 1 || body.Outputs[0].MPD.State != "play" {
		t.Fatalf("body %+v err %v", body, err)
	}
	if dials.Load() != 1 || closed.Load() != 0 {
		t.Fatalf("before Close: dials %d closed %d", dials.Load(), closed.Load())
	}
	lo.Close()
	if closed.Load() != 1 {
		t.Fatalf("Close left the session open (closed %d)", closed.Load())
	}
	for _, task := range clock.tasks {
		if task.fn != nil {
			t.Fatalf("an idle close armed for %v is still armed after Close", task.deadline)
		}
	}
	clock.Advance(2 * outputIdle)
	if closed.Load() != 1 {
		t.Fatalf("an idle close ran after Close (closed %d)", closed.Load())
	}
}

// TestTheServersOutputsBodyIsTheRoutes: the console's checklist reads the
// body GET /v1/outputs serves, from the same builder, so the two surfaces
// cannot describe two boxes.
func TestTheServersOutputsBodyIsTheRoutes(t *testing.T) {
	clock := &manualClock{now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	cfg := quietOutputs("linux", "on")
	cfg.Now, cfg.AfterFunc = clock.Now, clock.AfterFunc
	cfg.LoopbackBaseURL = func() string { return "http://127.0.0.1:7791" }
	cfg.Getenv = func(string) string { return "" }
	store := outputsToken(t)
	raw, _, err := store.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}
	c := &config.Config{ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600, LibraryName: "T", LibraryRoots: []string{t.TempDir()}}
	srv := New(c, store, nil, "fp").WithLocalOutputs(cfg)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	ctx := context.Background()
	if !srv.LocalOutputsActive(ctx) {
		t.Fatal("not active")
	}
	direct, err := srv.LocalOutputsResponse(ctx)
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	resp := outputsGet(t, hs, raw, "/v1/outputs")
	routed, _ := decodeOutputs(t, resp)
	a, _ := json.Marshal(direct)
	b, _ := json.Marshal(routed)
	if string(a) != string(b) {
		t.Fatalf("direct body\n%s\nroute body\n%s", a, b)
	}

	off := New(c, store, nil, "fp")
	if _, err := off.LocalOutputsResponse(ctx); err == nil {
		t.Fatal("a server with no outputs wired answered a body")
	}
}

// TestTheGateNeverOpensTheMPDSession: the connection stays lazy (decision
// 4). Asking the localOutputs gate, which /v1/health and the loopback
// route's start do, dials no MPD session; the first body opens it.
func TestTheGateNeverOpensTheMPDSession(t *testing.T) {
	for _, mode := range []string{"on", "auto"} {
		t.Run(mode, func(t *testing.T) {
			var dials, started atomic.Int32
			cfg := quietOutputs("linux", mode)
			cfg.Output = func() config.OutputConfig { return config.OutputConfig{MPD: mode} }
			cfg.Inspect = func(context.Context, mpd.Options) mpd.Report {
				return mpd.Report{ConfPath: "/etc/mpd.conf", MPD: mpd.Daemon{Version: "0.24.0", Endpoint: "127.0.0.1:6600"}}
			}
			cfg.Dial = func(context.Context, string, string) (outputSession, error) {
				dials.Add(1)
				return &fakeOutputSession{version: "0.24.0", state: "stop"}, nil
			}
			cfg.OnActive = func() { started.Add(1) }
			lo := NewLocalOutputs(cfg)
			t.Cleanup(lo.Close)
			for i := 0; i < 3; i++ {
				if !lo.Active(context.Background()) {
					t.Fatal("not active")
				}
			}
			if started.Load() != 3 || dials.Load() != 0 {
				t.Fatalf("after the gate: OnActive %d, session dials %d, want 3 and 0", started.Load(), dials.Load())
			}
			if _, err := lo.Response(context.Background()); err != nil {
				t.Fatal(err)
			}
			if dials.Load() != 1 {
				t.Fatalf("the first body dialled %d sessions, want 1", dials.Load())
			}
		})
	}
}
