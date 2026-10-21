package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/mpd"
)

// TestOutputsCarryTheLoopbackBaseURL: while the bridge serves its track
// route on loopback, /v1/outputs tells the app the route's base URL on the
// mpd object, so the app can hand that MPD <base>/dlna/file/<trackID>.
// The key is additive: absent when the route is not serving.
func TestOutputsCarryTheLoopbackBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		base func() string
		want string
	}{
		{"serving", func() string { return "http://127.0.0.1:7791" }, "http://127.0.0.1:7791"},
		{"not serving", func() string { return "" }, ""},
		{"not wired", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := quietOutputs("linux", "on")
			cfg.LoopbackBaseURL = tc.base
			hs, token := newOutputsHTTP(t, "fp", cfg)
			resp := outputsGet(t, hs, token, "/v1/outputs")
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				t.Fatalf("status %d", resp.StatusCode)
			}
			body, raw := decodeOutputs(t, resp)
			if got := body.Outputs[0].MPD.LoopbackBaseURL; got != tc.want {
				t.Fatalf("loopbackBaseURL %q, want %q", got, tc.want)
			}
			var top struct {
				Outputs []struct {
					MPD map[string]json.RawMessage `json:"mpd"`
				} `json:"outputs"`
			}
			if err := json.Unmarshal(raw, &top); err != nil {
				t.Fatal(err)
			}
			_, present := top.Outputs[0].MPD["loopbackBaseURL"]
			if present != (tc.want != "") {
				t.Fatalf("loopbackBaseURL present %v, want %v: %s", present, tc.want != "", raw)
			}
		})
	}
}

// TestLocalOutputsActiveStartsTheLoopbackRoute: the gate that answers
// localOutputs is the gate that starts the loopback route. OnActive runs
// whenever output.mpd resolves on, and never when it does not. On by
// default: a config that does not set output.mpd at all is on when
// detection finds an MPD on this box. An explicit setting wins: off with
// an MPD there stays off, on with none stays on.
func TestLocalOutputsActiveStartsTheLoopbackRoute(t *testing.T) {
	for _, tc := range []struct {
		name  string
		goos  string
		mode  string
		found bool
		want  bool
	}{
		{"linux on", "linux", "on", false, true},
		{"linux unset with an MPD", "linux", "", true, true},
		{"linux unset without one", "linux", "", false, false},
		{"linux auto with an MPD", "linux", "auto", true, true},
		{"linux auto without one", "linux", "auto", false, false},
		{"linux off with an MPD", "linux", "off", true, false},
		{"darwin on", "darwin", "on", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var started atomic.Int32
			cfg := quietOutputs(tc.goos, tc.mode)
			cfg.Inspect = func(context.Context, mpd.Options) mpd.Report {
				if !tc.found {
					return mpd.Report{}
				}
				return mpd.Report{ConfPath: "/etc/mpd.conf"}
			}
			cfg.Output = func() config.OutputConfig { return config.OutputConfig{MPD: tc.mode} }
			cfg.OnActive = func() { started.Add(1) }
			o := newLocalOutputs(cfg)
			if got := o.Active(context.Background()); got != tc.want {
				t.Fatalf("Active %v, want %v", got, tc.want)
			}
			if got := started.Load() > 0; got != tc.want {
				t.Fatalf("OnActive ran %d times, want it to run: %v", started.Load(), tc.want)
			}
		})
	}
}

// oneTrack is a library of one track: what the loopback route looks up.
type oneTrack dlna.TrackInfo

func (t oneTrack) GetTrackInfo(id string) (dlna.TrackInfo, bool) {
	return dlna.TrackInfo(t), id == t.TrackID
}

// TestTheLoopbackRouteAsksTheGateWithoutDialling: the loopback route asks
// on every request whether output.mpd resolves on, and gets the gate's
// answer as it stands: off refuses at once, on serves, and auto follows
// the gate's most recent answer, so an answer that turns false refuses.
// The route is the real handler, wired as serve wires it.
// No request inspects, dials or greets MPD: every inspection here is the
// gate's own, asked the way /v1/health asks it.
func TestTheLoopbackRouteAsksTheGateWithoutDialling(t *testing.T) {
	clock := &manualClock{}
	var mode atomic.Value
	mode.Store("on")
	var found atomic.Bool
	found.Store(true)
	var asked atomic.Int32 // every Inspect, Dial and Greet
	cfg := quietOutputs("linux", "on")
	cfg.Now, cfg.AfterFunc = clock.Now, clock.AfterFunc
	cfg.Output = func() config.OutputConfig { return config.OutputConfig{MPD: mode.Load().(string)} }
	cfg.Inspect = func(context.Context, mpd.Options) mpd.Report {
		asked.Add(1)
		if !found.Load() {
			return mpd.Report{}
		}
		return mpd.Report{ConfPath: "/etc/mpd.conf"}
	}
	cfg.Dial = func(context.Context, string, string) (outputSession, error) {
		asked.Add(1)
		return &fakeOutputSession{version: "0.24.0", state: "stop"}, nil
	}
	cfg.Greet = func(context.Context, string) bool { asked.Add(1); return false }
	s := &Server{localOutputs: newLocalOutputs(cfg)}

	root := t.TempDir()
	path := filepath.Join(root, "01.flac")
	if err := os.WriteFile(path, []byte("FLAC TRACK BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	route := dlna.LoopbackFileHandler(oneTrack{TrackID: "t1", AbsolutePath: path, FileExtension: ".flac"},
		func() []string { return []string{root} }, s.LocalOutputsActiveNow)
	fetch := func(step string, status int, code string) {
		t.Helper()
		requireLoopbackAnswer(t, route, &asked, step, status, code)
	}

	fetch("on", http.StatusOK, "")
	mode.Store("off")
	fetch("off", http.StatusNotFound, "not_serving")
	mode.Store("on")
	fetch("on again", http.StatusOK, "")
	mode.Store("auto")
	fetch("auto before the gate has probed", http.StatusNotFound, "not_serving")
	if !s.LocalOutputsActive(context.Background()) {
		t.Fatal("auto with an MPD on the box: the gate said off")
	}
	fetch("auto after a probe found MPD", http.StatusOK, "")
	found.Store(false)
	clock.Advance(outputProbeHit + time.Second)
	if s.LocalOutputsActive(context.Background()) {
		t.Fatal("auto with no MPD on the box: the gate said on")
	}
	fetch("auto after a probe found none", http.StatusNotFound, "not_serving")
	if n := asked.Load(); n != 2 {
		t.Fatalf("MPD was asked %d times, want the gate's two probes", n)
	}
}

// TestTheRouteFollowsAnExpiredProbeThatFoundMPD: a probe that read MPD's
// configuration and then passed its own deadline is not stored, yet the
// gate answers true and starts the loopback route, so the route serves:
// activeNow is the gate's answer, not the stored probe's.
func TestTheRouteFollowsAnExpiredProbeThatFoundMPD(t *testing.T) {
	var started atomic.Int32
	cfg := quietOutputs("linux", "auto")
	cfg.Inspect = func(ctx context.Context, _ mpd.Options) mpd.Report {
		<-ctx.Done() // the probe's own deadline passes
		return mpd.Report{ConfPath: "/etc/mpd.conf"}
	}
	cfg.OnActive = func() { started.Add(1) }
	o := newLocalOutputs(cfg)
	if !o.Active(context.Background()) || started.Load() != 1 {
		t.Fatalf("an expired probe that found MPD: the gate said off, or started nothing (%d)", started.Load())
	}
	if !o.activeNow() {
		t.Fatal("the gate answered true and started the route, and the route refuses")
	}
}

// TestTheRouteStopsOnAnExpiredMissAfterATrue: after the gate answered true
// from a probe it stored, a probe that found nothing and passed its own
// deadline is not stored, yet the gate (and /v1/health with it) answers
// false, so the route refuses too.
func TestTheRouteStopsOnAnExpiredMissAfterATrue(t *testing.T) {
	clock := &manualClock{}
	var gone atomic.Bool
	cfg := quietOutputs("linux", "auto")
	cfg.Now, cfg.AfterFunc = clock.Now, clock.AfterFunc
	cfg.Inspect = func(ctx context.Context, _ mpd.Options) mpd.Report {
		if gone.Load() {
			<-ctx.Done()
			return mpd.Report{}
		}
		return mpd.Report{ConfPath: "/etc/mpd.conf"}
	}
	o := newLocalOutputs(cfg)
	if !o.Active(context.Background()) || !o.activeNow() {
		t.Fatal("a stored probe that found MPD: the gate or the route said off")
	}
	gone.Store(true)
	clock.Advance(outputProbeHit + time.Second)
	if o.Active(context.Background()) {
		t.Fatal("an expired miss: the gate said on")
	}
	if o.activeNow() {
		t.Fatal("the gate answered false, and the route still serves")
	}
}

// TestActiveNowIsTheGatesLastAnswer: whichever way the gate reached its
// answer (a probe it stored, a probe past its deadline it did not store, a
// fresh cache hit, a new configuration), activeNow is the answer Active
// last returned.
func TestActiveNowIsTheGatesLastAnswer(t *testing.T) {
	const here, elsewhere = "127.0.0.1:6600", "127.0.0.1:6601"
	clock := &manualClock{}
	var endpoint atomic.Value
	cfg := quietOutputs("linux", "auto")
	cfg.Now, cfg.AfterFunc = clock.Now, clock.AfterFunc
	cfg.Output = func() config.OutputConfig {
		return config.OutputConfig{MPD: "auto", Endpoint: endpoint.Load().(string)}
	}
	cfg.Inspect = func(ctx context.Context, opt mpd.Options) mpd.Report {
		if opt.Endpoint == here {
			return mpd.Report{ConfPath: "/etc/mpd.conf"}
		}
		<-ctx.Done() // nothing there, found only after the probe's deadline
		return mpd.Report{}
	}
	o := newLocalOutputs(cfg)
	for _, step := range []struct {
		name, endpoint string
		advance        time.Duration
		want           bool
	}{
		{"a stored probe that found MPD", here, 0, true},
		{"a fresh cache hit", here, time.Second, true},
		{"a new configuration, past its probe's deadline, found none", elsewhere, 0, false},
		{"the first configuration again, a fresh cache hit", here, 0, true},
		{"the second again, past its deadline again", elsewhere, 0, false},
	} {
		endpoint.Store(step.endpoint)
		clock.Advance(step.advance)
		if got := o.Active(context.Background()); got != step.want {
			t.Fatalf("%s: Active %v, want %v", step.name, got, step.want)
		}
		if got := o.activeNow(); got != step.want {
			t.Fatalf("%s: activeNow %v, and the gate answered %v", step.name, got, step.want)
		}
	}
}

// TestACallerThatStopsWaitingGetsTheGatesLastAnswer: a caller whose request
// ends while it waits on another caller's probe has no verdict of its own.
// The gate answers it with its last answer and records nothing, so a
// cancelled /v1/health cannot turn the route off; the probe's own answer
// is recorded when it lands.
func TestACallerThatStopsWaitingGetsTheGatesLastAnswer(t *testing.T) {
	clock := &manualClock{}
	var second atomic.Bool
	inspecting, release := make(chan struct{}), make(chan struct{})
	cfg := quietOutputs("linux", "auto")
	cfg.Now, cfg.AfterFunc = clock.Now, clock.AfterFunc
	cfg.Inspect = func(context.Context, mpd.Options) mpd.Report {
		if !second.Load() {
			return mpd.Report{ConfPath: "/etc/mpd.conf"}
		}
		close(inspecting)
		<-release
		return mpd.Report{} // MPD has gone
	}
	o := newLocalOutputs(cfg)
	if !o.Active(context.Background()) {
		t.Fatal("a probe that found MPD: the gate said off")
	}
	second.Store(true)
	clock.Advance(outputProbeHit + time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	o.probeWait = cancel // the waiter's request ends as it starts to wait
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	owner := make(chan bool, 1)
	go func() { owner <- o.Active(context.Background()) }()
	<-inspecting
	if !o.Active(ctx) {
		t.Fatal("a caller that stopped waiting was answered off; the gate's last answer was on")
	}
	if !o.activeNow() {
		t.Fatal("a caller that stopped waiting turned the route off")
	}
	releaseOnce.Do(func() { close(release) })
	if <-owner {
		t.Fatal("the probe found no MPD, and the gate said on")
	}
	if o.activeNow() {
		t.Fatal("the gate answered false, and the route still serves")
	}
}

// TestActiveNowReadsTheConfigLikeTheProbe: with no output reader wired,
// activeNow reads the empty config, as probe does, and answers off (auto,
// no answer yet) instead of panicking on a request.
func TestActiveNowReadsTheConfigLikeTheProbe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("activeNow panicked with no output reader: %v", r)
		}
	}()
	if (&localOutputs{goos: "linux"}).activeNow() {
		t.Fatal("no output reader: activeNow said on")
	}
}

// requireLoopbackAnswer asks route for track t1 ten times, requires the
// same answer each time, and requires that no request asked MPD anything:
// asked counts every Inspect, Dial and Greet.
func requireLoopbackAnswer(t *testing.T, route http.Handler, asked *atomic.Int32, step string, status int, code string) {
	t.Helper()
	before := asked.Load()
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7791"+dlna.FilePathPrefix+"t1", nil)
		req.RemoteAddr = "127.0.0.1:50000"
		rec := httptest.NewRecorder()
		route.ServeHTTP(rec, req)
		var refusal struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
		if rec.Code != status || refusal.Error != code {
			t.Fatalf("%s: status %d error %q, want %d %q", step, rec.Code, refusal.Error, status, code)
		}
	}
	if n := asked.Load() - before; n != 0 {
		t.Fatalf("%s: the requests asked MPD %d times", step, n)
	}
}
