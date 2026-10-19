package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/mpd"
)

type fakeOutputSession struct {
	version   string
	state     string
	found     map[string]bool
	statuses  *atomic.Int32
	closed    *atomic.Int32
	block     chan struct{}
	started   chan struct{}
	once      sync.Once
	statusErr error
	findErr   error
	waitCtx   bool
	finds     *atomic.Int32
}

func (f *fakeOutputSession) Status(ctx context.Context) (mpd.Reply, error) {
	if f.statuses != nil {
		f.statuses.Add(1)
	}
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.waitCtx {
		<-ctx.Done()
		return mpd.Reply{}, ctx.Err()
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return mpd.Reply{}, ctx.Err()
		}
	}
	if f.statusErr != nil {
		return mpd.Reply{}, f.statusErr
	}
	return mpd.Reply{Pairs: []mpd.Pair{{Key: "state", Value: f.state}}}, nil
}

func (f *fakeOutputSession) Find(_ context.Context, path string) (mpd.Reply, error) {
	if f.finds != nil {
		f.finds.Add(1)
	}
	if f.findErr != nil {
		return mpd.Reply{}, f.findErr
	}
	if f.found[path] {
		return mpd.Reply{Pairs: []mpd.Pair{{Key: "file", Value: path}}}, nil
	}
	return mpd.Reply{}, nil
}

func (f *fakeOutputSession) Version() string { return f.version }

func (f *fakeOutputSession) Close() error {
	if f.closed != nil {
		f.closed.Add(1)
	}
	return nil
}

func outputsToken(t *testing.T) *auth.Store {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatalf("open token store: %v", err)
	}
	return store
}

func newOutputsHTTP(t *testing.T, identity string, cfg LocalOutputsConfig) (*httptest.Server, string) {
	t.Helper()
	store := outputsToken(t)
	raw, _, err := store.Mint("phone")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	dir := t.TempDir()
	c := &config.Config{
		ListenAddress:   ":7788",
		AdminAddress:    "127.0.0.1:7789",
		ScanIntervalSec: 3600,
		LibraryName:     "T",
		LibraryRoots:    []string{dir},
	}
	if cfg.Getenv == nil {
		cfg.Getenv = func(string) string { return "" }
	}
	srv := New(c, store, nil, identity).WithLocalOutputs(cfg)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw
}

func outputsGet(t *testing.T, hs *httptest.Server, token, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, hs.URL+path, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	return resp
}

func decodeOutputs(t *testing.T, resp *http.Response) (OutputsResponse, []byte) {
	t.Helper()
	defer resp.Body.Close()
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var body OutputsResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return body, raw
}

func TestLocalOutputsGate(t *testing.T) {
	cases := []struct {
		name     string
		goos     string
		mode     string
		found    bool
		wantCode int
		wantFlag bool
	}{
		{"linux on even when nothing answers", "linux", "on", false, http.StatusOK, true},
		{"linux auto when a daemon greeted", "linux", "auto", true, http.StatusOK, true},
		{"linux auto when nothing is there", "linux", "auto", false, http.StatusNotFound, false},
		{"linux off", "linux", "off", true, http.StatusNotFound, false},
		{"darwin on", "darwin", "on", true, http.StatusNotFound, false},
		{"windows auto", "windows", "auto", true, http.StatusNotFound, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var inspected atomic.Int32
			cfg := LocalOutputsConfig{
				GOOS: tc.goos,
				Output: func() config.OutputConfig {
					return config.OutputConfig{MPD: tc.mode}
				},
				Inspect: func(context.Context, mpd.Options) mpd.Report {
					inspected.Add(1)
					if !tc.found {
						return mpd.Report{}
					}
					return mpd.Report{MPD: mpd.Daemon{Version: "0.24.0", Endpoint: "127.0.0.1:6600"}}
				},
				Dial: func(context.Context, string, string) (outputSession, error) {
					return &fakeOutputSession{version: "0.24.0", state: "stop"}, nil
				},
				Greet:      func(context.Context, string) bool { return false },
				Advertised: func() []string { return nil },
				Cards:      func() (alsa.Snapshot, error) { return alsa.Snapshot{}, nil },
			}
			hs, token := newOutputsHTTP(t, "fp", cfg)
			bare := outputsGet(t, hs, "", "/v1/outputs")
			bare.Body.Close()
			if bare.StatusCode != http.StatusUnauthorized {
				t.Fatalf("no bearer: status %d, want 401", bare.StatusCode)
			}
			resp := outputsGet(t, hs, token, "/v1/outputs")
			if resp.StatusCode != tc.wantCode {
				resp.Body.Close()
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.wantCode)
			}
			if tc.wantCode == http.StatusNotFound {
				var errBody struct {
					Error string `json:"error"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
					t.Fatalf("error body: %v", err)
				}
				resp.Body.Close()
				if errBody.Error != "local_outputs_not_supported" {
					t.Fatalf("error %q", errBody.Error)
				}
			} else {
				body, _ := decodeOutputs(t, resp)
				if len(body.Outputs) != 1 || body.Outputs[0].Kind != "mpd" {
					t.Fatalf("body %+v", body)
				}
			}
			health := outputsGet(t, hs, "", "/v1/health")
			defer health.Body.Close()
			var h HealthResponse
			if err := json.NewDecoder(health.Body).Decode(&h); err != nil {
				t.Fatalf("health: %v", err)
			}
			has := false
			for _, f := range h.Features {
				if f == "localOutputs" {
					has = true
				}
			}
			if has != tc.wantFlag {
				t.Fatalf("localOutputs in features = %v, want %v (%v)", has, tc.wantFlag, h.Features)
			}
			if tc.goos != "linux" && inspected.Load() != 0 {
				t.Fatalf("probed the host off Linux (%d)", inspected.Load())
			}
		})
	}
}

func TestOutputsJSONKeepsEveryKey(t *testing.T) {
	snap, err := (alsa.Reader{ProcRoot: filepath.Join("..", "alsa", "testdata", "dsd256-native", "proc")}).Read()
	if err != nil {
		t.Fatalf("ifi fixture: %v", err)
	}
	music := t.TempDir()
	var dialed atomic.Value
	cfg := LocalOutputsConfig{
		GOOS: "linux",
		Output: func() config.OutputConfig {
			return config.OutputConfig{MPD: "on", Password: "s3cret-value"}
		},
		Roots: func() []string { return []string{music} },
		Tracks: func(context.Context) ([]string, error) {
			return []string{"Album/t.flac"}, nil
		},
		Inspect: func(context.Context, mpd.Options) mpd.Report {
			return mpd.Report{
				Software:       mpd.Software{Name: "Volumio", Version: "4.119"},
				MPD:            mpd.Daemon{Version: "0.24.0", Endpoint: "127.0.0.1:6600"},
				MusicDirectory: music,
				Outputs:        []mpd.Output{{Type: "alsa", Device: "hw:5,0", Enabled: "yes"}},
			}
		},
		Dial: func(_ context.Context, endpoint, password string) (outputSession, error) {
			dialed.Store(endpoint + " " + password)
			return &fakeOutputSession{
				version: "0.24.4",
				state:   "play",
				found:   map[string]bool{"Album/t.flac": true},
			}, nil
		},
		Greet: func(_ context.Context, addr string) bool { return addr == "192.168.1.42:6600" },
		Advertised: func() []string {
			return []string{
				"https://192.168.1.42:7788",
				"https://10.0.0.8:7788",
				"https://127.0.0.1:7788",
				"https://[::1]:7788",
				"https://box.local:7788",
				"https://192.168.1.42:7788",
			}
		},
		Cards: func() (alsa.Snapshot, error) { return snap, nil },
	}
	hs, token := newOutputsHTTP(t, "fingerprint-a", cfg)
	resp := outputsGet(t, hs, token, "/v1/outputs")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, raw := decodeOutputs(t, resp)
	if string(dialed.Load().(string)) != "127.0.0.1:6600 s3cret-value" {
		t.Fatalf("dialed %v", dialed.Load())
	}
	if jsonContains(raw, "s3cret-value") {
		t.Fatalf("password leaked into the body")
	}
	one := body.Outputs[0]
	if one.Kind != "mpd" || one.Player.Software != "volumio" || one.Player.Version != "4.119" {
		t.Fatalf("player %+v kind %s", one.Player, one.Kind)
	}
	if one.MPD.Port != 6600 || one.MPD.Version != "0.24.4" || one.MPD.State != "play" || !one.MPD.ReachableFromLAN {
		t.Fatalf("mpd %+v", one.MPD)
	}
	if len(one.MPD.Addresses) != 1 || one.MPD.Addresses[0] != "192.168.1.42" {
		t.Fatalf("addresses %v", one.MPD.Addresses)
	}
	if one.DAC == nil || one.DAC.Card != 5 || one.DAC.ID != "Audio" || one.DAC.USBID != "20b1:3009" ||
		one.DAC.IsMPDsOutput == nil || !*one.DAC.IsMPDsOutput || one.DAC.Capabilities.MaxPCMRate != 384000 {
		t.Fatalf("dac %+v", one.DAC)
	}
	if len(one.PathRules) != 1 || one.PathRules[0].State != "proven" || one.PathRules[0].Found != 1 {
		t.Fatalf("rules %+v", one.PathRules)
	}
	assertKeys(t, raw, "outputs")
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	var elems []map[string]json.RawMessage
	if err := json.Unmarshal(top["outputs"], &elems); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "kind", "player", "mpd", "dac", "pathRules"} {
		if _, ok := elems[0][key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
	var mpdObj map[string]json.RawMessage
	if err := json.Unmarshal(elems[0]["mpd"], &mpdObj); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"port", "addresses", "version", "state", "reachableFromLAN"} {
		if _, ok := mpdObj[key]; !ok {
			t.Errorf("mpd missing %s", key)
		}
	}
	if string(mpdObj["addresses"]) == "null" {
		t.Error("addresses is null")
	}

	// An unresolved device lists the USB card with isMPDsOutput null.
	cfg.Inspect = func(context.Context, mpd.Options) mpd.Report {
		return mpd.Report{
			MPD:     mpd.Daemon{Version: "0.24.0", Endpoint: "127.0.0.1:6600"},
			Outputs: []mpd.Output{{Type: "alsa", Device: "volumio", Enabled: "yes"}},
		}
	}
	hs2, token2 := newOutputsHTTP(t, "fingerprint-a", cfg)
	resp2 := outputsGet(t, hs2, token2, "/v1/outputs")
	body2, raw2 := decodeOutputs(t, resp2)
	if body2.Outputs[0].DAC == nil || body2.Outputs[0].DAC.IsMPDsOutput != nil {
		t.Fatalf("unresolved dac %+v", body2.Outputs[0].DAC)
	}
	if !jsonContains(raw2, `"isMPDsOutput":null`) {
		t.Fatalf("isMPDsOutput was not null: %s", raw2)
	}

	// No card at all still returns one output, with dac null and empty lists.
	cfg.Inspect = func(context.Context, mpd.Options) mpd.Report {
		return mpd.Report{MPD: mpd.Daemon{Endpoint: "127.0.0.1:6600"}}
	}
	cfg.Roots = func() []string { return nil }
	cfg.Cards = func() (alsa.Snapshot, error) { return alsa.Snapshot{}, nil }
	cfg.Greet = func(context.Context, string) bool { return false }
	hs3, token3 := newOutputsHTTP(t, "fingerprint-a", cfg)
	resp3 := outputsGet(t, hs3, token3, "/v1/outputs")
	_, raw3 := decodeOutputs(t, resp3)
	for _, want := range []string{`"dac":null`, `"addresses":[]`, `"pathRules":[]`, `"reachableFromLAN":false`} {
		if !jsonContains(raw3, want) {
			t.Errorf("body missing %s: %s", want, raw3)
		}
	}
}

func TestOutputKindIsOpen(t *testing.T) {
	extra := OutputsResponse{Outputs: []OutputInfo{{
		Kind:      "bridge",
		MPD:       OutputMPD{Addresses: []string{}, State: "unknown"},
		PathRules: []OutputPathRule{},
	}}}
	raw, err := json.Marshal(extra)
	if err != nil {
		t.Fatal(err)
	}
	var back OutputsResponse
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Outputs[0].Kind != "bridge" {
		t.Fatalf("kind %q was dropped", back.Outputs[0].Kind)
	}
}

func TestOutputsLANAddresses(t *testing.T) {
	var greeted []string
	var greetMu sync.Mutex
	cfg := LocalOutputsConfig{
		GOOS: "linux",
		Output: func() config.OutputConfig {
			return config.OutputConfig{MPD: "on"}
		},
		Inspect: func(context.Context, mpd.Options) mpd.Report {
			return mpd.Report{MPD: mpd.Daemon{Version: "0.24.0", Endpoint: "/run/mpd/socket"}}
		},
		Dial: func(context.Context, string, string) (outputSession, error) {
			return &fakeOutputSession{state: "stop", version: "0.24.0"}, nil
		},
		Greet: func(_ context.Context, addr string) bool {
			greetMu.Lock()
			greeted = append(greeted, addr)
			greetMu.Unlock()
			return addr == "192.168.1.42:6600"
		},
		Advertised: func() []string {
			return []string{
				"https://192.168.1.42:7788",
				"https://127.0.0.1:7788",
				"https://[::1]:7788",
				"https://box.local:7788",
			}
		},
		Cards: func() (alsa.Snapshot, error) { return alsa.Snapshot{}, nil },
	}
	hs, token := newOutputsHTTP(t, "fp", cfg)
	resp := outputsGet(t, hs, token, "/v1/outputs")
	body, _ := decodeOutputs(t, resp)
	if len(body.Outputs[0].MPD.Addresses) != 1 || body.Outputs[0].MPD.Addresses[0] != "192.168.1.42" {
		t.Fatalf("addresses %v", body.Outputs[0].MPD.Addresses)
	}
	for _, addr := range greeted {
		if addr == "127.0.0.1:6600" || addr == "[::1]:6600" || addr == "box.local:6600" {
			t.Fatalf("greeted a non-LAN address %s (%v)", addr, greeted)
		}
	}
	if body.Outputs[0].MPD.Port != 6600 {
		t.Fatalf("socket endpoint used port %d", body.Outputs[0].MPD.Port)
	}
}

func TestLANProbesRunTogetherAndSkipLinkLocal(t *testing.T) {
	entered := make(chan string, 8)
	release := make(chan struct{})
	cfg := quietOutputs("linux", "on")
	cfg.Greet = func(ctx context.Context, addr string) bool {
		entered <- addr
		select {
		case <-release:
		case <-ctx.Done():
		}
		return true
	}
	cfg.Advertised = func() []string {
		return []string{
			"https://192.168.1.42:7788",
			"https://192.168.1.43:7788",
			"https://[fe80::1]:7788",
			"https://169.254.1.1:7788",
		}
	}
	hs, token := newOutputsHTTP(t, "fp", cfg)
	done := make(chan struct{})
	go func() {
		req, err := http.NewRequest(http.MethodGet, hs.URL+"/v1/outputs", nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := hs.Client().Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}
		close(done)
	}()
	got := map[string]struct{}{}
	deadline := time.After(300 * time.Millisecond)
	for len(got) < 2 {
		select {
		case addr := <-entered:
			got[addr] = struct{}{}
		case <-deadline:
			t.Fatalf("in flight %v", got)
		}
	}
	select {
	case addr := <-entered:
		t.Fatalf("greeted an extra address %s", addr)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-done
	if _, ok := got["192.168.1.42:6600"]; !ok {
		t.Fatalf("missing 192.168.1.42 in %v", got)
	}
	if _, ok := got["192.168.1.43:6600"]; !ok {
		t.Fatalf("missing 192.168.1.43 in %v", got)
	}
}

func TestGreetDeadlineCoversDialAndRead(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			time.Sleep(time.Second)
			c.Close()
		}
	}()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if greetMPD(ctx, ln.Addr().String()) {
		t.Fatal("greeting succeeded")
	}
	if elapsed := time.Since(start); elapsed > 280*time.Millisecond {
		t.Fatalf("read kept its own deadline: %s", elapsed)
	}
}

func TestOutputsCacheAndIdle(t *testing.T) {
	clock := &manualClock{}
	var dials, statuses, closed atomic.Int32
	cfg := quietOutputs("linux", "on")
	cfg.Now = clock.Now
	cfg.AfterFunc = clock.AfterFunc
	cfg.Dial = func(context.Context, string, string) (outputSession, error) {
		dials.Add(1)
		return &fakeOutputSession{version: "0.24.0", state: "stop", statuses: &statuses, closed: &closed}, nil
	}
	hs, token := newOutputsHTTP(t, "fp", cfg)
	get := func() {
		t.Helper()
		resp := outputsGet(t, hs, token, "/v1/outputs")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	get()
	if dials.Load() != 1 || statuses.Load() != 1 || closed.Load() != 0 {
		t.Fatalf("first call dials %d status %d closed %d", dials.Load(), statuses.Load(), closed.Load())
	}
	get()
	if dials.Load() != 1 || statuses.Load() != 1 {
		t.Fatalf("cache hit rebuilt: dials %d status %d", dials.Load(), statuses.Load())
	}
	clock.Advance(outputCacheTTL - time.Millisecond)
	get()
	if statuses.Load() != 1 {
		t.Fatalf("a call inside 500ms rebuilt (status %d)", statuses.Load())
	}
	clock.Advance(time.Millisecond)
	get()
	if dials.Load() != 1 || statuses.Load() != 2 {
		t.Fatalf("at 500ms want a rebuild on the same session, dials %d status %d", dials.Load(), statuses.Load())
	}
	clock.Advance(outputIdle)
	if closed.Load() != 1 {
		t.Fatalf("idle close did not run, closed %d", closed.Load())
	}
	get()
	if dials.Load() != 2 {
		t.Fatalf("after idle, dials %d, want a new session", dials.Load())
	}
}

func TestCancelledCallerDoesNotSpoilTheBuild(t *testing.T) {
	hold := make(chan struct{})
	started := make(chan struct{})
	joined := make(chan struct{})
	cfg := quietOutputs("linux", "on")
	cfg.Dial = func(context.Context, string, string) (outputSession, error) {
		return &fakeOutputSession{state: "play", version: "0.24.4", block: hold, started: started}, nil
	}
	store := outputsToken(t)
	raw, _, err := store.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	srv := New(&config.Config{
		ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789",
		ScanIntervalSec: 3600, LibraryName: "T", LibraryRoots: []string{dir},
	}, store, nil, "fp").WithLocalOutputs(cfg)
	srv.localOutputs.joined = func() { close(joined) }
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan struct{})
	go func() {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/outputs", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
		close(firstDone)
	}()
	<-started
	secondDone := make(chan *httptest.ResponseRecorder)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/v1/outputs", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		secondDone <- rr
	}()
	<-joined
	cancel()
	close(hold)
	<-firstDone
	rr := <-secondDone
	var body OutputsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || body.Outputs[0].MPD.State != "play" {
		t.Fatalf("waiter status %d state %s", rr.Code, body.Outputs[0].MPD.State)
	}
}

func TestBuildDeadlineIsNotCached(t *testing.T) {
	var statuses atomic.Int32
	cfg := quietOutputs("linux", "on")
	cfg.BuildLimit = time.Millisecond
	cfg.Dial = func(context.Context, string, string) (outputSession, error) {
		return &fakeOutputSession{waitCtx: true, version: "0.24.0", statuses: &statuses}, nil
	}
	hs, token := newOutputsHTTP(t, "fp", cfg)
	for i := 0; i < 2; i++ {
		resp := outputsGet(t, hs, token, "/v1/outputs")
		resp.Body.Close()
	}
	if statuses.Load() != 2 {
		t.Fatalf("status calls %d, a deadline was cached", statuses.Load())
	}
}

func TestPathRulesUseTheStoredRootPrefix(t *testing.T) {
	dir := t.TempDir()
	music := filepath.Join(dir, "Music")
	other := filepath.Join(dir, "Archive")
	if err := os.Mkdir(music, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	o := &localOutputs{
		roots: func() []string { return []string{"/", music, other} },
		tracks: func(context.Context) ([]string, error) {
			return []string{"Music/a.flac", "music/b.flac", "Archive/c.flac"}, nil
		},
	}
	rules := o.pathRules(context.Background(), music, func(context.Context, string) (bool, error) {
		return true, nil
	})
	if rules[0].Root != filepath.Base("/") {
		t.Fatalf("filesystem root prefix %q", rules[0].Root)
	}
	var musicRule OutputPathRule
	for _, rule := range rules {
		if rule.Root == "Music" {
			musicRule = rule
		}
	}
	if musicRule.StripBridgePrefix != "Music/" || musicRule.Checked != 2 {
		t.Fatalf("strip %q checked %d", musicRule.StripBridgePrefix, musicRule.Checked)
	}
}

func TestIdleCloseSkipsABuildInFlight(t *testing.T) {
	clock := &manualClock{}
	hold := make(chan struct{})
	started := make(chan struct{})
	var closed atomic.Int32
	cfg := quietOutputs("linux", "on")
	cfg.Now = clock.Now
	cfg.AfterFunc = clock.AfterFunc
	cfg.Dial = func(context.Context, string, string) (outputSession, error) {
		return &fakeOutputSession{state: "stop", version: "0.24.0", block: hold, started: started, closed: &closed}, nil
	}
	store := outputsToken(t)
	raw, _, err := store.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	srv := New(&config.Config{
		ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789",
		ScanIntervalSec: 3600, LibraryName: "T", LibraryRoots: []string{dir},
	}, store, nil, "fp").WithLocalOutputs(cfg)
	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/v1/outputs", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	<-started
	clock.Advance(outputIdle)
	if closed.Load() != 0 {
		t.Fatalf("closed the session mid-build: %d", closed.Load())
	}
	close(hold)
	<-done
	clock.Advance(outputIdle)
	if closed.Load() != 1 {
		t.Fatalf("idle close after the build: closed %d", closed.Load())
	}
}

func TestOutputsOneBuildInFlight(t *testing.T) {
	hold := make(chan struct{})
	started := make(chan struct{})
	joined := make(chan struct{})
	var dials, statuses atomic.Int32
	cfg := quietOutputs("linux", "on")
	cfg.Dial = func(context.Context, string, string) (outputSession, error) {
		n := dials.Add(1)
		sess := &fakeOutputSession{version: "0.24.0", state: "stop", statuses: &statuses}
		if n == 1 {
			sess.block = hold
			sess.started = started
		}
		return sess, nil
	}
	store := outputsToken(t)
	raw, _, err := store.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	srv := New(&config.Config{
		ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789",
		ScanIntervalSec: 3600, LibraryName: "T", LibraryRoots: []string{dir},
	}, store, nil, "fp").WithLocalOutputs(cfg)
	srv.localOutputs.joined = func() { close(joined) }

	var wg sync.WaitGroup
	codes := make([]int, 2)
	call := func(i int) {
		defer wg.Done()
		req := httptest.NewRequest(http.MethodGet, "/v1/outputs", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		codes[i] = rr.Code
	}
	wg.Add(1)
	go call(0)
	<-started
	wg.Add(1)
	go call(1)
	// joined fires when the second caller waits on the build in flight.
	// A second build does not: it enters Status, and waiting here forever
	// would hide that. Stop when that happens, then the counts below fail.
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case <-joined:
			deadline = time.Time{}
		default:
		}
		if deadline.IsZero() || statuses.Load() >= 2 || time.Now().After(deadline) {
			break
		}
		runtime.Gosched()
	}
	close(hold)
	wg.Wait()
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("statuses %v", codes)
	}
	if dials.Load() != 1 || statuses.Load() != 1 {
		t.Fatalf("in-flight build was not shared: dials %d status %d", dials.Load(), statuses.Load())
	}
}

func quietOutputs(goos, mode string) LocalOutputsConfig {
	return LocalOutputsConfig{
		GOOS: goos,
		Output: func() config.OutputConfig {
			return config.OutputConfig{MPD: mode}
		},
		Inspect: func(context.Context, mpd.Options) mpd.Report {
			return mpd.Report{MPD: mpd.Daemon{Version: "0.24.0", Endpoint: "127.0.0.1:6600"}}
		},
		Dial: func(context.Context, string, string) (outputSession, error) {
			return &fakeOutputSession{version: "0.24.0", state: "stop"}, nil
		},
		Greet:      func(context.Context, string) bool { return false },
		Advertised: func() []string { return nil },
		Cards:      func() (alsa.Snapshot, error) { return alsa.Snapshot{}, nil },
	}
}

func TestPathRulesOutliveTheResponseCache(t *testing.T) {
	clock := &manualClock{}
	music := t.TempDir()
	endpoint := ""
	var finds atomic.Int32
	cfg := quietOutputs("linux", "on")
	cfg.Now = clock.Now
	cfg.AfterFunc = clock.AfterFunc
	cfg.Output = func() config.OutputConfig {
		return config.OutputConfig{MPD: "on", Endpoint: endpoint}
	}
	cfg.Inspect = func(context.Context, mpd.Options) mpd.Report {
		return mpd.Report{MPD: mpd.Daemon{Version: "0.24.0", Endpoint: "127.0.0.1:6600"}, MusicDirectory: music}
	}
	cfg.Dial = func(context.Context, string, string) (outputSession, error) {
		return &fakeOutputSession{version: "0.24.0", state: "stop", found: map[string]bool{"a.flac": true}, finds: &finds}, nil
	}
	cfg.Roots = func() []string { return []string{music} }
	cfg.Tracks = func(context.Context) ([]string, error) { return []string{"a.flac"}, nil }
	hs, token := newOutputsHTTP(t, "fp", cfg)
	get := func() {
		t.Helper()
		resp := outputsGet(t, hs, token, "/v1/outputs")
		resp.Body.Close()
	}
	get()
	if finds.Load() != 1 {
		t.Fatalf("first find count %d", finds.Load())
	}
	clock.Advance(outputCacheTTL)
	get()
	if finds.Load() != 1 {
		t.Fatalf("finds %d after the response cache expired", finds.Load())
	}
	clock.Advance(outputIdle)
	get()
	if finds.Load() != 2 {
		t.Fatalf("finds %d after the path-rule cache", finds.Load())
	}
	endpoint = "192.0.2.10:6600"
	clock.Advance(outputCacheTTL)
	get()
	if finds.Load() != 3 {
		t.Fatalf("finds %d after a config change", finds.Load())
	}
}

func TestDeadMPDSessionIsRedialed(t *testing.T) {
	var dials atomic.Int32
	var closed atomic.Int32
	cfg := quietOutputs("linux", "on")
	cfg.Dial = func(context.Context, string, string) (outputSession, error) {
		n := dials.Add(1)
		if n == 1 {
			return &fakeOutputSession{statusErr: errors.New("eof"), closed: &closed}, nil
		}
		return &fakeOutputSession{state: "play", version: "0.24.4", closed: &closed}, nil
	}
	hs, token := newOutputsHTTP(t, "fp", cfg)
	resp := outputsGet(t, hs, token, "/v1/outputs")
	body, _ := decodeOutputs(t, resp)
	if body.Outputs[0].MPD.State != "play" || dials.Load() != 2 || closed.Load() != 1 {
		t.Fatalf("state %s dials %d closed %d", body.Outputs[0].MPD.State, dials.Load(), closed.Load())
	}
}

func TestSessionFollowsTheEndpoint(t *testing.T) {
	clock := &manualClock{}
	endpoint := "127.0.0.1:6600"
	password := ""
	var dialed []string
	cfg := quietOutputs("linux", "on")
	cfg.Now = clock.Now
	cfg.AfterFunc = clock.AfterFunc
	cfg.Output = func() config.OutputConfig {
		return config.OutputConfig{MPD: "on", Password: password}
	}
	cfg.Inspect = func(context.Context, mpd.Options) mpd.Report {
		return mpd.Report{MPD: mpd.Daemon{Version: "0.24.0", Endpoint: endpoint}}
	}
	cfg.Dial = func(_ context.Context, ep, pw string) (outputSession, error) {
		dialed = append(dialed, ep+" "+pw)
		return &fakeOutputSession{state: "stop", version: "0.24.0"}, nil
	}
	hs, token := newOutputsHTTP(t, "fp", cfg)
	one := func() {
		t.Helper()
		resp := outputsGet(t, hs, token, "/v1/outputs")
		resp.Body.Close()
	}
	one()
	password = "next"
	clock.Advance(outputCacheTTL)
	one()
	if len(dialed) != 2 || dialed[1] != "127.0.0.1:6600 next" {
		t.Fatalf("password change dialed %v", dialed)
	}
	endpoint = "127.0.0.1:6601"
	clock.Advance(outputProbeHit)
	one()
	if len(dialed) != 3 || dialed[2] != "127.0.0.1:6601 next" {
		t.Fatalf("endpoint change dialed %v", dialed)
	}
}

func TestDeadFindRedialsOnce(t *testing.T) {
	music := t.TempDir()
	var dials atomic.Int32
	cfg := quietOutputs("linux", "on")
	cfg.Roots = func() []string { return []string{music} }
	cfg.Tracks = func(context.Context) ([]string, error) { return []string{"a.flac"}, nil }
	cfg.Inspect = func(context.Context, mpd.Options) mpd.Report {
		return mpd.Report{MPD: mpd.Daemon{Version: "0.24.0", Endpoint: "127.0.0.1:6600"}, MusicDirectory: music}
	}
	cfg.Dial = func(context.Context, string, string) (outputSession, error) {
		n := dials.Add(1)
		if n == 1 {
			return &fakeOutputSession{state: "stop", version: "0.24.0", findErr: errors.New("eof")}, nil
		}
		return &fakeOutputSession{state: "stop", version: "0.24.0", found: map[string]bool{"a.flac": true}}, nil
	}
	hs, token := newOutputsHTTP(t, "fp", cfg)
	resp := outputsGet(t, hs, token, "/v1/outputs")
	body, _ := decodeOutputs(t, resp)
	if dials.Load() != 2 || len(body.Outputs[0].PathRules) != 1 || body.Outputs[0].PathRules[0].State != "proven" {
		t.Fatalf("dials %d rules %+v", dials.Load(), body.Outputs[0].PathRules)
	}
}

func TestOutputsIDStableAcrossRestarts(t *testing.T) {
	snap, err := (alsa.Reader{ProcRoot: filepath.Join("..", "alsa", "testdata", "dsd256-native", "proc")}).Read()
	if err != nil {
		t.Fatal(err)
	}
	report := mpd.Report{
		MPD:     mpd.Daemon{Version: "0.24.0", Endpoint: "127.0.0.1:6600"},
		Outputs: []mpd.Output{{Type: "alsa", Device: "hw:5,0", Enabled: "yes"}},
	}
	cfgFor := func(identity string) LocalOutputsConfig {
		return LocalOutputsConfig{
			GOOS:     "linux",
			Identity: identity,
			Output: func() config.OutputConfig {
				return config.OutputConfig{MPD: "on"}
			},
			Inspect: func(context.Context, mpd.Options) mpd.Report { return report },
			Dial: func(context.Context, string, string) (outputSession, error) {
				return &fakeOutputSession{version: "0.24.0", state: "stop"}, nil
			},
			Greet:      func(context.Context, string) bool { return false },
			Advertised: func() []string { return nil },
			Cards:      func() (alsa.Snapshot, error) { return snap, nil },
		}
	}
	one := outputIDOf(t, cfgFor("same-fingerprint"))
	two := outputIDOf(t, cfgFor("same-fingerprint"))
	if one != two || one == "" {
		t.Fatalf("ids %q and %q", one, two)
	}
	other := outputIDOf(t, cfgFor("other-fingerprint"))
	if other == one {
		t.Fatal("identity did not change the id")
	}
}

func TestOutputIDIgnoresTheCardIndex(t *testing.T) {
	snap, err := (alsa.Reader{ProcRoot: filepath.Join("..", "alsa", "testdata", "dsd256-native", "proc")}).Read()
	if err != nil {
		t.Fatal(err)
	}
	at5, _ := outputAt(t, snap, "hw:5,0")
	moved := moveUSBCard(snap, 5, 6)
	at6, _ := outputAt(t, moved, "hw:6,0")
	if at5.ID == "" || at5.ID != at6.ID {
		t.Fatalf("index changed the id: %q vs %q", at5.ID, at6.ID)
	}
	if at5.DAC == nil || at6.DAC == nil || at5.DAC.Card != 5 || at6.DAC.Card != 6 {
		t.Fatalf("cards %+v %+v", at5.DAC, at6.DAC)
	}
	if at5.DAC.ID != at6.DAC.ID || at5.DAC.USBID != at6.DAC.USBID {
		t.Fatalf("stable names differ: %+v %+v", at5.DAC, at6.DAC)
	}
	renamed := moveUSBCard(snap, 5, 5)
	for i := range renamed.Cards {
		if renamed.Cards[i].Index == 5 && renamed.Cards[i].USBID != nil {
			renamed.Cards[i].ID = "Other"
		}
	}
	renamedOut, _ := outputAt(t, renamed, "hw:5,0")
	if renamedOut.ID == at5.ID {
		t.Fatal("a different ALSA id kept the same output id")
	}
	rewired := moveUSBCard(snap, 5, 5)
	for i := range rewired.Cards {
		if rewired.Cards[i].USBID != nil && rewired.Cards[i].Index == 5 {
			id := *rewired.Cards[i].USBID
			id.Product++
			rewired.Cards[i].USBID = &id
		}
	}
	rewiredOut, _ := outputAt(t, rewired, "hw:5,0")
	if rewiredOut.ID == at5.ID {
		t.Fatal("a different USB id kept the same output id")
	}
}

func TestIsMPDsOutputIsTrueOrNull(t *testing.T) {
	snap, err := (alsa.Reader{ProcRoot: filepath.Join("..", "alsa", "testdata", "dsd256-native", "proc")}).Read()
	if err != nil {
		t.Fatal(err)
	}
	resolved, rawResolved := outputAt(t, snap, "hw:5,0")
	if resolved.DAC == nil || resolved.DAC.IsMPDsOutput == nil || !*resolved.DAC.IsMPDsOutput {
		t.Fatalf("resolved card %+v", resolved.DAC)
	}
	if jsonContains(rawResolved, `"isMPDsOutput":false`) {
		t.Fatalf("resolved card sent false: %s", rawResolved)
	}
	unresolved, rawUnresolved := outputAt(t, snap, "volumio")
	if unresolved.DAC == nil || unresolved.DAC.IsMPDsOutput != nil {
		t.Fatalf("unresolved card %+v", unresolved.DAC)
	}
	if !jsonContains(rawUnresolved, `"isMPDsOutput":null`) || jsonContains(rawUnresolved, `"isMPDsOutput":false`) {
		t.Fatalf("unresolved card was not null: %s", rawUnresolved)
	}
}

func outputAt(t *testing.T, snap alsa.Snapshot, device string) (OutputInfo, []byte) {
	t.Helper()
	cfg := LocalOutputsConfig{
		GOOS:     "linux",
		Identity: "same-fingerprint",
		Output: func() config.OutputConfig {
			return config.OutputConfig{MPD: "on"}
		},
		Inspect: func(context.Context, mpd.Options) mpd.Report {
			return mpd.Report{
				MPD:     mpd.Daemon{Version: "0.24.0", Endpoint: "127.0.0.1:6600"},
				Outputs: []mpd.Output{{Type: "alsa", Device: device, Enabled: "yes"}},
			}
		},
		Dial: func(context.Context, string, string) (outputSession, error) {
			return &fakeOutputSession{version: "0.24.0", state: "stop"}, nil
		},
		Greet:      func(context.Context, string) bool { return false },
		Advertised: func() []string { return nil },
		Cards:      func() (alsa.Snapshot, error) { return snap, nil },
	}
	hs, token := newOutputsHTTP(t, cfg.Identity, cfg)
	resp := outputsGet(t, hs, token, "/v1/outputs")
	body, raw := decodeOutputs(t, resp)
	if len(body.Outputs) != 1 {
		t.Fatalf("outputs %+v", body.Outputs)
	}
	return body.Outputs[0], raw
}

func moveUSBCard(snap alsa.Snapshot, from, to int) alsa.Snapshot {
	cards := append([]alsa.CardReading(nil), snap.Cards...)
	for i := range cards {
		if cards[i].USBID != nil {
			id := *cards[i].USBID
			cards[i].USBID = &id
		}
		if cards[i].Index == from && cards[i].USBID != nil {
			cards[i].Index = to
		}
	}
	snap.Cards = cards
	return snap
}

func outputIDOf(t *testing.T, cfg LocalOutputsConfig) string {
	t.Helper()
	hs, token := newOutputsHTTP(t, cfg.Identity, cfg)
	resp := outputsGet(t, hs, token, "/v1/outputs")
	body, _ := decodeOutputs(t, resp)
	if len(body.Outputs) != 1 {
		t.Fatalf("outputs %+v", body.Outputs)
	}
	return body.Outputs[0].ID
}

func jsonContains(raw []byte, needle string) bool {
	return strings.Contains(string(raw), needle)
}

func assertKeys(t *testing.T, raw []byte, key string) {
	t.Helper()
	if !jsonContains(raw, `"`+key+`"`) {
		t.Errorf("missing key %s", key)
	}
}
