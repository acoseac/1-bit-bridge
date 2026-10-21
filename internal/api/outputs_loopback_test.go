package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
// the verdict of the gate's most recent probe, so a verdict that turns
// false refuses. The route is the real handler, wired as serve wires it.
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
