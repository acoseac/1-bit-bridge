package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
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
