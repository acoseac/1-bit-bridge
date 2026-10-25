package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/mpd"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// testPlayerUUID is the bridge's player's identity in these tests.
const testPlayerUUID = "7d444840-9dc0-11d1-b245-5ffdce74fad2"

// fakeBridgeOutput is serve's player as the outputs route asks it: Enabled
// fixed, Facts as the test sets them, each ask counted.
type fakeBridgeOutput struct {
	enabled bool
	asked   atomic.Int32
	mu      sync.Mutex
	facts   BridgeOutputFacts
}

func (f *fakeBridgeOutput) Enabled() bool { return f.enabled }

func (f *fakeBridgeOutput) Facts(context.Context) BridgeOutputFacts {
	f.asked.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.facts
}

func (f *fakeBridgeOutput) set(facts BridgeOutputFacts) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.facts = facts
}

// listedPlayer is the facts of a player that has an identity and plays.
func listedPlayer() BridgeOutputFacts {
	return BridgeOutputFacts{Listed: true, UUID: testPlayerUUID, State: "stopped",
		Codecs: []string{"flac", "wav", "aiff", "alac"}, FixedVolume: true, DSDMode: "off"}
}

// newBridgeOutputsHTTP is newOutputsHTTP with serve's player wired as b,
// returning the server too.
func newBridgeOutputsHTTP(t *testing.T, cfg LocalOutputsConfig, b BridgeOutput) (*httptest.Server, string, *Server) {
	t.Helper()
	store := outputsToken(t)
	raw, _, err := store.Mint("phone")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	c := &config.Config{ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600, LibraryName: "T", LibraryRoots: []string{t.TempDir()}}
	if cfg.Getenv == nil {
		cfg.Getenv = func(string) string { return "" }
	}
	srv := New(c, store, nil, "fp").WithLocalOutputs(cfg)
	if b != nil {
		srv.WithBridgeOutput(b)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, srv
}

// healthFeatures is /v1/health's features.
func healthFeatures(t *testing.T, hs *httptest.Server) []string {
	t.Helper()
	resp := outputsGet(t, hs, "", "/v1/health")
	defer resp.Body.Close()
	var h HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	return h.Features
}

// rawElements is the outputs body's elements, each as its keys.
func rawElements(t *testing.T, raw []byte) []map[string]json.RawMessage {
	t.Helper()
	var top struct {
		Outputs []map[string]json.RawMessage `json:"outputs"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	return top.Outputs
}

func keysOf(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestAnMPDElementsBytesAreMains: the bridge object is an omitempty pointer
// beside mpd, so an MPD element marshals to the bytes it did before kind
// bridge existed (captured from main at 9e7b2675).
func TestAnMPDElementsBytesAreMains(t *testing.T) {
	yes := true
	body := OutputsResponse{Outputs: []OutputInfo{{
		ID:     "0f3c",
		Kind:   "mpd",
		Player: OutputPlayer{Software: "volumio", Version: "4.119"},
		MPD:    &OutputMPD{Port: 6600, Addresses: []string{"192.168.1.42"}, Version: "0.24.0", State: "play", ReachableFromLAN: true, LoopbackBaseURL: "http://127.0.0.1:7791"},
		DAC: &OutputDAC{Card: 5, ID: "Audio", Name: "iFi (by AMR) HD USB Audio", USBID: "20b1:3009", IsMPDsOutput: &yes,
			Capabilities: OutputCapabilities{MaxPCMRate: 384000, PCMBitDepths: []int{16, 24, 32}, NativeDSD: true, NativeDSDRates: []int{2822400, 5644800}, DSDFormats: []string{"DSD_U32_BE"}}},
		PathRules: []OutputPathRule{{Root: "Music", StripBridgePrefix: "Music/", PrependMpdPrefix: "", State: "proven", Checked: 20, Found: 20}},
	}, {ID: "9a1b", Kind: "mpd", Player: OutputPlayer{}, MPD: &OutputMPD{Addresses: []string{}, State: "unknown"}, PathRules: []OutputPathRule{}}}}
	const mains = `{"outputs":[{"id":"0f3c","kind":"mpd","player":{"software":"volumio","version":"4.119"},"mpd":{"port":6600,"addresses":["192.168.1.42"],"version":"0.24.0","state":"play","reachableFromLAN":true,"loopbackBaseURL":"http://127.0.0.1:7791"},"dac":{"card":5,"id":"Audio","name":"iFi (by AMR) HD USB Audio","usbId":"20b1:3009","isMPDsOutput":true,"capabilities":{"maxPCMRate":384000,"pcmBitDepths":[16,24,32],"nativeDSD":true,"nativeDSDRates":[2822400,5644800],"dsdFormats":["DSD_U32_BE"]}},"pathRules":[{"root":"Music","stripBridgePrefix":"Music/","prependMpdPrefix":"","state":"proven","checked":20,"found":20}]},{"id":"9a1b","kind":"mpd","player":{"software":"","version":""},"mpd":{"port":0,"addresses":[],"version":"","state":"unknown","reachableFromLAN":false},"dac":null,"pathRules":[]}]}`
	got, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != mains {
		t.Fatalf("an MPD element's bytes changed:\n got %s\nwant %s", got, mains)
	}
}

// TestTheBridgesOwnElement pins kind bridge's element as the route builds it
// from serve's facts (B470 PR 5b): its id, kind and player, mpd absent, dac
// null, pathRules [], and the bridge object with every key; a reason only
// while unavailable, lastError null or its word, openHome null with no
// Source or no endpoint, codecs never null. Beside MPD's elements it comes
// after them, and theirs carry no bridge key.
func TestTheBridgesOwnElement(t *testing.T) {
	b := &fakeBridgeOutput{enabled: true}
	facts := listedPlayer()
	facts.State, facts.Reason, facts.LastError = "unavailable", "device_missing", "device_busy"
	facts.OpenHome = &BridgeOpenHomeFacts{UDN: "uuid:" + testPlayerUUID, Endpoints: []string{"10.0.0.2:7792", "192.168.1.20:7792"}}
	b.set(facts)
	hs, token, _ := newBridgeOutputsHTTP(t, quietOutputs("linux", "off"), b)
	resp := outputsGet(t, hs, token, "/v1/outputs")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, raw := decodeOutputs(t, resp)
	if len(body.Outputs) != 1 {
		t.Fatalf("outputs %+v", body.Outputs)
	}
	el := body.Outputs[0]
	if el.ID != "bridge:"+testPlayerUUID || el.Kind != "bridge" || el.Player.Software != "bridge" || el.Player.Version != version.ServerVersion || el.MPD != nil || el.DAC != nil {
		t.Fatalf("element %+v", el)
	}
	elems := rawElements(t, raw)
	if got := keysOf(elems[0]); !slices.Equal(got, []string{"bridge", "dac", "id", "kind", "pathRules", "player"}) {
		t.Fatalf("the element's keys %q", got)
	}
	if string(elems[0]["dac"]) != "null" || string(elems[0]["pathRules"]) != "[]" {
		t.Fatalf("dac %s, pathRules %s", elems[0]["dac"], elems[0]["pathRules"])
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(elems[0]["bridge"], &obj); err != nil {
		t.Fatal(err)
	}
	if got := keysOf(obj); !slices.Equal(got, []string{"allowDoP", "codecs", "dsdMode", "fixedVolume", "lastError", "libraryScope", "openHome", "reason", "state"}) {
		t.Fatalf("the bridge object's keys %q", got)
	}
	want := map[string]string{
		"state": `"unavailable"`, "reason": `"device_missing"`, "lastError": `"device_busy"`, "libraryScope": `"self"`,
		"fixedVolume": "true", "codecs": `["flac","wav","aiff","alac"]`, "dsdMode": `"off"`, "allowDoP": "false",
		"openHome": `{"udn":"uuid:` + testPlayerUUID + `","endpoints":["10.0.0.2:7792","192.168.1.20:7792"]}`,
	}
	for k, v := range want {
		if string(obj[k]) != v {
			t.Errorf("bridge.%s = %s, want %s", k, obj[k], v)
		}
	}

	// A reason outside unavailable, no last error, no Source, no codecs.
	facts = listedPlayer()
	facts.State, facts.Reason, facts.Codecs = "playing", "a reason that comes with no unavailable", nil
	b.set(facts)
	_, raw = decodeOutputs(t, outputsGet(t, hs, token, "/v1/outputs"))
	if err := json.Unmarshal(rawElements(t, raw)[0]["bridge"], &obj); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"state": `"playing"`, "reason": "null", "lastError": "null", "openHome": "null", "codecs": "[]"} {
		if string(obj[k]) != v {
			t.Errorf("bridge.%s = %s, want %s", k, obj[k], v)
		}
	}
	// A Source that listens on no address is no Source.
	facts.OpenHome = &BridgeOpenHomeFacts{UDN: "uuid:" + testPlayerUUID}
	b.set(facts)
	_, raw = decodeOutputs(t, outputsGet(t, hs, token, "/v1/outputs"))
	if err := json.Unmarshal(rawElements(t, raw)[0]["bridge"], &obj); err != nil {
		t.Fatal(err)
	}
	if string(obj["openHome"]) != "null" {
		t.Errorf("a Source with no endpoint: openHome %s, want null", obj["openHome"])
	}

	// Beside MPD's element: after it, and MPD's has no bridge key.
	b.set(listedPlayer())
	hs2, token2, _ := newBridgeOutputsHTTP(t, quietOutputs("linux", "on"), b)
	body2, raw2 := decodeOutputs(t, outputsGet(t, hs2, token2, "/v1/outputs"))
	if len(body2.Outputs) != 2 || body2.Outputs[0].Kind != "mpd" || body2.Outputs[1].Kind != "bridge" {
		t.Fatalf("outputs %+v", body2.Outputs)
	}
	if _, has := rawElements(t, raw2)[0]["bridge"]; has {
		t.Fatalf("the MPD element carries a bridge key: %s", raw2)
	}
	if body2.Outputs[0].Bridge != nil || body2.Outputs[1].MPD != nil {
		t.Fatalf("outputs %+v", body2.Outputs)
	}

	// No identity: no element, and 200 with outputs [] while the MPD gate
	// is off.
	b.set(BridgeOutputFacts{})
	resp3 := outputsGet(t, hs, token, "/v1/outputs")
	if resp3.StatusCode != http.StatusOK {
		resp3.Body.Close()
		t.Fatalf("a player with no identity: status %d", resp3.StatusCode)
	}
	if _, raw3 := decodeOutputs(t, resp3); string(raw3) != `{"outputs":[]}` {
		t.Fatalf("a player with no identity: %s", raw3)
	}
}

// gateCounts is what the outputs builder and the MPD gate did.
type gateCounts struct {
	probes, dials, greets, tracks, cards, onActive atomic.Int32
}

// countedOutputs is quietOutputs for mode with every reach toward MPD and
// the box counted: auto finds MPD when hit.
func countedOutputs(goos, mode string, hit bool, n *gateCounts) LocalOutputsConfig {
	cfg := quietOutputs(goos, mode)
	cfg.Inspect = func(context.Context, mpd.Options) mpd.Report {
		n.probes.Add(1)
		if !hit {
			return mpd.Report{}
		}
		return mpd.Report{MPD: mpd.Daemon{Version: "0.24.0", Endpoint: "127.0.0.1:6600"}}
	}
	cfg.Dial = func(context.Context, string, string) (outputSession, error) {
		n.dials.Add(1)
		return &fakeOutputSession{version: "0.24.0", state: "stop"}, nil
	}
	cfg.Greet = func(context.Context, string) bool { n.greets.Add(1); return false }
	cfg.Advertised = func() []string { return []string{"https://192.168.1.42:7788"} }
	cfg.Tracks = func(context.Context) ([]string, error) { n.tracks.Add(1); return nil, nil }
	cfg.Cards = func() (alsa.Snapshot, error) { n.cards.Add(1); return alsa.Snapshot{}, nil }
	cfg.OnActive = func() { n.onActive.Add(1) }
	return cfg
}

// TestTheOutputsRouteHasTwoGates is the gate matrix at the route (B470 PR
// 5b): MPD off, on, auto with a hit and auto with a miss, and a host that is
// not Linux, each with serve's player on and off. localOutputs is either
// gate, bridgeOutput the player's, outputReading MPD's; GET /v1/outputs is
// 404 only while both are off. With MPD off, or off Linux, nothing probes or
// dials; with auto's miss only the gate's own probe runs, and the builder is
// never entered (no session, greeting, track list or card read); the player
// alone starts nothing the MPD gate starts (OnActive).
func TestTheOutputsRouteHasTwoGates(t *testing.T) {
	type mpdCase struct {
		name, goos, mode string
		hit, on          bool
	}
	for _, m := range []mpdCase{
		{"mpd off", "linux", "off", true, false},
		{"mpd on", "linux", "on", true, true},
		{"mpd auto with a hit", "linux", "auto", true, true},
		{"mpd auto with a miss", "linux", "auto", false, false},
		{"mpd on off Linux", "darwin", "on", true, false},
	} {
		for _, player := range []bool{true, false} {
			name := m.name + ", player off"
			if player {
				name = m.name + ", player on"
			}
			t.Run(name, func(t *testing.T) {
				var n gateCounts
				b := &fakeBridgeOutput{enabled: player}
				b.set(listedPlayer())
				hs, token, _ := newBridgeOutputsHTTP(t, countedOutputs(m.goos, m.mode, m.hit, &n), b)
				feats := healthFeatures(t, hs)
				for key, want := range map[string]bool{"localOutputs": m.on || player, "bridgeOutput": player, "outputReading": m.on} {
					if slices.Contains(feats, key) != want {
						t.Errorf("%s advertised %v, want %v: %v", key, !want, want, feats)
					}
				}
				resp := outputsGet(t, hs, token, "/v1/outputs")
				wantCode := http.StatusNotFound
				if m.on || player {
					wantCode = http.StatusOK
				}
				if resp.StatusCode != wantCode {
					resp.Body.Close()
					t.Fatalf("GET /v1/outputs: %d, want %d", resp.StatusCode, wantCode)
				}
				if wantCode == http.StatusOK {
					body, _ := decodeOutputs(t, resp)
					var kinds []string
					for _, o := range body.Outputs {
						kinds = append(kinds, o.Kind)
					}
					var want []string
					if m.on {
						want = append(want, "mpd")
					}
					if player {
						want = append(want, "bridge")
					}
					if !slices.Equal(kinds, want) {
						t.Errorf("the kinds listed %q, want %q", kinds, want)
					}
				} else {
					resp.Body.Close()
				}
				switch {
				case m.mode == "off" || m.goos != "linux":
					if n.probes.Load()+n.dials.Load()+n.greets.Load()+n.tracks.Load()+n.cards.Load() != 0 {
						t.Errorf("MPD probed %d, dialled %d, greeted %d, tracks %d, cards %d: want none", n.probes.Load(), n.dials.Load(), n.greets.Load(), n.tracks.Load(), n.cards.Load())
					}
				case !m.on:
					if n.probes.Load() == 0 {
						t.Error("auto's own probe never ran")
					}
					if n.dials.Load()+n.greets.Load()+n.tracks.Load()+n.cards.Load() != 0 {
						t.Errorf("after auto's miss the builder was entered: dials %d, greets %d, tracks %d, cards %d", n.dials.Load(), n.greets.Load(), n.tracks.Load(), n.cards.Load())
					}
				default:
					if n.dials.Load() != 1 {
						t.Errorf("the MPD body dialled %d sessions, want 1", n.dials.Load())
					}
				}
				if want := int32(0); !m.on && n.onActive.Load() != want {
					t.Errorf("OnActive ran %d times with MPD's gate off", n.onActive.Load())
				}
				if m.on && n.onActive.Load() != 2 {
					t.Errorf("OnActive ran %d times for a health answer and a body, want 2", n.onActive.Load())
				}
			})
		}
	}
}

// TestTheMPDGateIsAskedOnEveryRequestWhateverThePlayer pins the operand
// order (B470 PR 5b): with serve's player on, health and GET /v1/outputs
// still ask the MPD gate on every request, so its true answers start the
// loopback track route (OnActive, counted) and auto's answer is the one the
// route reads (activeNow): a probe that turns from a hit to a miss turns
// the route off, through health and through the body alike.
func TestTheMPDGateIsAskedOnEveryRequestWhateverThePlayer(t *testing.T) {
	var n gateCounts
	b := &fakeBridgeOutput{enabled: true}
	b.set(listedPlayer())
	hs, token, srv := newBridgeOutputsHTTP(t, countedOutputs("linux", "on", true, &n), b)
	for i := 0; i < 3; i++ {
		healthFeatures(t, hs)
		resp := outputsGet(t, hs, token, "/v1/outputs")
		resp.Body.Close()
	}
	if got := n.onActive.Load(); got != 6 {
		t.Errorf("OnActive ran %d times for three health answers and three bodies with the player on, want 6", got)
	}

	for _, through := range []string{"/v1/health", "/v1/outputs"} {
		t.Run("auto through "+through, func(t *testing.T) {
			clock := &manualClock{now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
			var hit atomic.Bool
			hit.Store(true)
			var counts gateCounts
			cfg := countedOutputs("linux", "auto", true, &counts)
			cfg.Now, cfg.AfterFunc = clock.Now, clock.AfterFunc
			cfg.Inspect = func(context.Context, mpd.Options) mpd.Report {
				if !hit.Load() {
					return mpd.Report{}
				}
				return mpd.Report{MPD: mpd.Daemon{Version: "0.24.0", Endpoint: "127.0.0.1:6600"}}
			}
			hs2, token2, srv2 := newBridgeOutputsHTTP(t, cfg, b)
			ask := func() {
				resp := outputsGet(t, hs2, token2, through)
				resp.Body.Close()
			}
			ask()
			if !srv2.LocalOutputsActiveNow() {
				t.Fatalf("after a hit through %s the route's answer is off", through)
			}
			hit.Store(false)
			clock.Advance(outputProbeHit + time.Second)
			ask()
			if srv2.LocalOutputsActiveNow() {
				t.Fatalf("after a miss through %s, with the player on, the route's answer is still the hit's", through)
			}
		})
	}
	_ = srv
}

// TestABridgeIDNamesNoReading: a kind bridge element's id is an id from GET
// /v1/outputs, and the reading route, on the MPD gate alone, answers it 404
// output_not_found, with no probe and no dial (B470 PR 5b: the reading of
// kind bridge is a later change).
func TestABridgeIDNamesNoReading(t *testing.T) {
	var n gateCounts
	b := &fakeBridgeOutput{enabled: true}
	b.set(listedPlayer())
	hs, token, _ := newBridgeOutputsHTTP(t, countedOutputs("linux", "on", true, &n), b)
	body, _ := decodeOutputs(t, outputsGet(t, hs, token, "/v1/outputs"))
	id := body.Outputs[len(body.Outputs)-1].ID
	dials := n.dials.Load()
	probes := n.probes.Load()
	code, _, raw := getReading(t, hs, token, id)
	if code != http.StatusNotFound || !jsonContains(raw, `"output_not_found"`) {
		t.Fatalf("the reading of %s: %d %s", id, code, raw)
	}
	if n.dials.Load() != dials || n.probes.Load() != probes {
		t.Fatalf("the reading of a bridge id dialled %d and probed %d", n.dials.Load()-dials, n.probes.Load()-probes)
	}
	// A bridge-only box has no reading route at all.
	hs2, token2, _ := newBridgeOutputsHTTP(t, countedOutputs("linux", "off", true, &n), b)
	code, _, raw = getReading(t, hs2, token2, id)
	if code != http.StatusNotFound || !jsonContains(raw, `"local_outputs_not_supported"`) {
		t.Fatalf("a bridge-only box's reading: %d %s", code, raw)
	}
}

// TestTwoRequestsInTheCacheWindowLeaveTheCachedBodyAlone: the composer
// builds a slice of its own for every answer (B470 PR 5b), so two requests
// served from one cached MPD body, at once, each get MPD's element and the
// bridge's, and the cached body afterwards holds MPD's elements alone and
// nothing past them, even one with room to grow (run under -race).
func TestTwoRequestsInTheCacheWindowLeaveTheCachedBodyAlone(t *testing.T) {
	fixed := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for _, room := range []bool{false, true} {
		name := "the builder's body"
		if room {
			name = "a cached body with room to grow"
		}
		t.Run(name, func(t *testing.T) {
			var n gateCounts
			cfg := countedOutputs("linux", "on", true, &n)
			cfg.Now = func() time.Time { return fixed }
			cfg.AfterFunc = func(time.Duration, func()) func() { return func() {} }
			b := &fakeBridgeOutput{enabled: true}
			b.set(listedPlayer())
			hs, token, srv := newBridgeOutputsHTTP(t, cfg, b)
			if _, raw := decodeOutputs(t, outputsGet(t, hs, token, "/v1/outputs")); len(rawElements(t, raw)) != 2 {
				t.Fatalf("the first body: %s", raw)
			}
			o := srv.localOutputs
			if room {
				o.mu.Lock()
				grown := make([]OutputInfo, len(o.cached.body.Outputs), len(o.cached.body.Outputs)+4)
				copy(grown, o.cached.body.Outputs)
				o.cached.body.Outputs = grown
				o.mu.Unlock()
			}
			var wg sync.WaitGroup
			kinds := make([][]string, 2)
			for i := range kinds {
				wg.Add(1)
				go func() {
					defer wg.Done()
					body, _ := decodeOutputs(t, outputsGet(t, hs, token, "/v1/outputs"))
					for _, el := range body.Outputs {
						kinds[i] = append(kinds[i], el.Kind)
					}
				}()
			}
			wg.Wait()
			for i, k := range kinds {
				if !slices.Equal(k, []string{"mpd", "bridge"}) {
					t.Errorf("answer %d listed %q, want mpd then bridge", i, k)
				}
			}
			if n.dials.Load() != 1 {
				t.Fatalf("the cache window's answers dialled %d sessions: they were not served from the cache", n.dials.Load())
			}
			o.mu.Lock()
			cached := o.cached.body.Outputs
			full := cached[:cap(cached)]
			o.mu.Unlock()
			if len(cached) != 1 || cached[0].Kind != "mpd" || cached[0].Bridge != nil {
				t.Fatalf("the cached body afterwards: %+v, want MPD's element alone", cached)
			}
			for i := len(cached); i < len(full); i++ {
				if full[i].Kind != "" {
					t.Fatalf("an answer was written into the cached body's backing array at %d: %+v", i, full[i])
				}
			}
		})
	}
}

// TestAnMPDFailureWithBothGatesOnIsStill500: an MPD body that fails (here a
// build whose dial panics) keeps the 500 it answered before the bridge's
// element (B470 PR 5b changes no MPD error), and the answer carries no
// element; the next body is whole, MPD's and the bridge's.
func TestAnMPDFailureWithBothGatesOnIsStill500(t *testing.T) {
	var n gateCounts
	cfg := countedOutputs("linux", "on", true, &n)
	var panics atomic.Bool
	panics.Store(true)
	cfg.Dial = func(context.Context, string, string) (outputSession, error) {
		if panics.Load() {
			panic("a dial that panics")
		}
		return &fakeOutputSession{version: "0.24.0", state: "stop"}, nil
	}
	b := &fakeBridgeOutput{enabled: true}
	b.set(listedPlayer())
	hs, token, _ := newBridgeOutputsHTTP(t, cfg, b)
	resp := outputsGet(t, hs, token, "/v1/outputs")
	var errBody struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&errBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || errBody.Error != "internal" {
		t.Fatalf("an MPD body that failed with the player on: %d %q, want 500 internal", resp.StatusCode, errBody.Error)
	}
	panics.Store(false)
	body, _ := decodeOutputs(t, outputsGet(t, hs, token, "/v1/outputs"))
	if len(body.Outputs) != 2 {
		t.Fatalf("the next body: %+v", body.Outputs)
	}
}

// TestTheServersOutputsBodyIsTheRoutesWithThePlayersElement: the console's
// checklist reads the body GET /v1/outputs serves, through one composer, so
// with serve's player wired the server's body is the route's, the player's
// element included (B470 PR 5b).
func TestTheServersOutputsBodyIsTheRoutesWithThePlayersElement(t *testing.T) {
	clock := &manualClock{now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	cfg := quietOutputs("linux", "on")
	cfg.Now, cfg.AfterFunc = clock.Now, clock.AfterFunc
	b := &fakeBridgeOutput{enabled: true}
	facts := listedPlayer()
	facts.OpenHome = &BridgeOpenHomeFacts{UDN: "uuid:" + testPlayerUUID, Endpoints: []string{"192.168.1.20:7792"}}
	b.set(facts)
	hs, token, srv := newBridgeOutputsHTTP(t, cfg, b)
	direct, err := srv.LocalOutputsResponse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	routed, _ := decodeOutputs(t, outputsGet(t, hs, token, "/v1/outputs"))
	a, _ := json.Marshal(direct)
	c, _ := json.Marshal(routed)
	if string(a) != string(c) || len(direct.Outputs) != 2 || direct.Outputs[1].Kind != "bridge" {
		t.Fatalf("direct body\n%s\nroute body\n%s", a, c)
	}
}
