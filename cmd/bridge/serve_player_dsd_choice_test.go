package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// PUT /v1/outputs/{id}/dsd through serve's view of its player (B728 B9; the
// design's section 7.2): the answer is the epoch the backend published, as GET
// /v1/outputs returns it afterwards; the write is durable before it is
// answered; each refusal changes nothing; the key is the one GET publishes,
// whatever the unit's strings hold; and serve wires the route to its player.

// choiceLines records the policy's lines, rendered with their attributes.
type choiceLines struct {
	mu    sync.Mutex
	lines []string
}

func (c *choiceLines) add(msg string, args []any) {
	var b strings.Builder
	b.WriteString(msg)
	for i := 0; i+1 < len(args); i += 2 {
		fmt.Fprintf(&b, " %v=%v", args[i], args[i+1])
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, b.String())
}

func (c *choiceLines) Info(msg string, args ...any) { c.add(msg, args) }
func (c *choiceLines) Warn(msg string, args ...any) { c.add(msg, args) }

func (c *choiceLines) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

// writeLines are the lines a choice write logged.
func (c *choiceLines) writeLines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, l := range c.lines {
		if strings.Contains(l, " outcome=") {
			out = append(out, l)
		}
	}
	return out
}

// choiceRig is an API server whose player is serve's view of a ready player
// running b, the store it writes and its lines.
type choiceRig struct {
	hs    *httptest.Server
	token string
	b     *playback.Backend
	lines *choiceLines
}

// newChoiceRig is a rig over a backend whose DSD is the effective policy over
// box and store, the test DAC bound at card 5, its lines recorded; wrap, when
// set, wraps the API's handler.
func newChoiceRig(t *testing.T, box *playbacktest.Box, store playback.DSDStore, device config.PlayerDevice, wrap func(http.Handler) http.Handler) *choiceRig {
	t.Helper()
	lines := &choiceLines{}
	b := readSideBackend(t, box, store, func(p *playback.DSDPolicy) { p.Log = lines })
	view := readSideOutput(t, b, device, true)
	tokens, err := auth.OpenStore(filepath.Join(t.TempDir(), tokensFileName))
	if err != nil {
		t.Fatal(err)
	}
	bearer, _, err := tokens.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(&config.Config{ListenAddress: ":7788", LibraryName: "T", LibraryRoots: []string{t.TempDir()}}, tokens, nil, "fp").
		WithLocalOutputs(api.LocalOutputsConfig{GOOS: "linux", Output: func() config.OutputConfig { return config.OutputConfig{MPD: "off"} }}).
		WithBridgeOutput(view)
	var h http.Handler = srv.Handler()
	if wrap != nil {
		h = wrap(h)
	}
	hs := httptest.NewServer(h)
	t.Cleanup(hs.Close)
	return &choiceRig{hs: hs, token: bearer, b: b, lines: lines}
}

// get is GET /v1/outputs' bridge object.
func (r *choiceRig) get(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, r.hs.URL+"/v1/outputs", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/outputs: %d %s %v", resp.StatusCode, raw, err)
	}
	return bridgeObjectOf(t, raw)
}

// choice is the bound DAC's key and revision GET publishes.
func (r *choiceRig) choice(t *testing.T) (key string, revision uint64) {
	t.Helper()
	var dac api.OutputDSDDAC
	if err := json.Unmarshal(r.get(t)["dsdDAC"], &dac); err != nil {
		t.Fatalf("no dsdDAC: %v", err)
	}
	return dac.DACKey, dac.Choice.Revision
}

// put is the PUT of body, its status and its body.
func (r *choiceRig) put(t *testing.T, body string) (int, map[string]json.RawMessage) {
	t.Helper()
	status, m, err := r.send(body)
	if err != nil {
		t.Fatal(err)
	}
	return status, m
}

// send is the PUT of body, its status and its body, or why there is none:
// put's, for a goroutine of the test's, which must not end the test.
func (r *choiceRig) send(body string) (int, map[string]json.RawMessage, error) {
	req, err := http.NewRequest(http.MethodPut, r.hs.URL+"/v1/outputs/bridge:"+thePlayerUUID+"/dsd", strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.hs.Client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return resp.StatusCode, nil, fmt.Errorf("PUT answered %d %s: %w", resp.StatusCode, raw, err)
	}
	return resp.StatusCode, m, nil
}

// body is a choice's body.
func body(key string, base uint64, mode string, extra ...string) string {
	b := fmt.Sprintf(`{"dacKey":%q,"baseRevision":%d,"mode":%q`, key, base, mode)
	for _, e := range extra {
		b += "," + e
	}
	return b + "}"
}

// code is an answer's error word.
func code(m map[string]json.RawMessage) string {
	var c string
	_ = json.Unmarshal(m["error"], &c)
	return c
}

// compactJSON is raw with nothing between its tokens.
func compactJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return b.String()
}

// TestTheAnswerIsTheEffectiveChoice (B728 B9; binding note 6): a written
// choice is answered with the bound DAC's DSD as the epoch the backend
// published says it, byte for byte what GET /v1/outputs returns afterwards:
// the choice with its new revision, and what it plays, refusals included,
// for a mode the DAC does not honour at some rate (native, offered at DSD64
// alone).
func TestTheAnswerIsTheEffectiveChoice(t *testing.T) {
	box := playbacktest.NewBox(5)
	native := playbacktest.NativeAltset()
	native.Rates = []int{playbacktest.DSD64 / 32}
	box.SetAltsets(5, 0, playbacktest.S32Altset(), playbacktest.S24Altset(), native)
	r := newChoiceRig(t, box, playbacktest.NewMemStore(dsdstate.Defaults()), config.PlayerDevice{}, nil)
	key, rev := r.choice(t)
	status, answer := r.put(t, body(key, rev, dsdstate.ModeNative, `"firstUseAcknowledged":true`))
	if status != http.StatusOK {
		t.Fatalf("PUT: %d %s", status, answer)
	}
	var dac api.OutputDSDDAC
	if err := json.Unmarshal(answer["dsdDAC"], &dac); err != nil {
		t.Fatal(err)
	}
	if dac.Choice.Mode != dsdstate.ModeNative || dac.Choice.Revision != rev+1 || dac.Choice.MaxBitRate != 0 {
		t.Errorf("the answer's choice %+v, want native, uncapped, at revision %d", dac.Choice, rev+1)
	}
	var dsd api.OutputDSDPlayback
	if err := json.Unmarshal(answer["dsdPlayback"], &dsd); err != nil {
		t.Fatal(err)
	}
	if len(dsd.Tuples) != 1 || dsd.Tuples[0].BitRate != playbacktest.DSD64 || len(dsd.Refused) != 2 || dsd.Refused[0].Reason != "native_not_offered" {
		t.Errorf("the answer's set %+v, want native at DSD64 and the two higher rates refused as native_not_offered", dsd)
	}
	after := r.get(t)
	for _, member := range []string{"dsdDAC", "dsdPlayback"} {
		if got, want := compactJSON(t, after[member]), compactJSON(t, answer[member]); got != want {
			t.Errorf("GET's %s after the write:\n%s\nthe answer's:\n%s", member, got, want)
		}
	}
}

// syncLog orders what the durable writer's barrier and the answer did.
type syncLog struct {
	mu     sync.Mutex
	events []string
}

func (s *syncLog) note(e string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *syncLog) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// answerRecorder notes the answer's status line as it is written.
type answerRecorder struct {
	http.ResponseWriter
	log  *syncLog
	once sync.Once
}

func (a *answerRecorder) WriteHeader(code int) {
	a.once.Do(func() { a.log.note("answered") })
	a.ResponseWriter.WriteHeader(code)
}

func (a *answerRecorder) Write(b []byte) (int, error) {
	a.once.Do(func() { a.log.note("answered") })
	return a.ResponseWriter.Write(b)
}

func (a *answerRecorder) Unwrap() http.ResponseWriter { return a.ResponseWriter }

// TestTheChoiceIsDurableBeforeItIsAnswered (B728 B9; the design's section
// 2.5): the write's answer comes only once the player's DSD state is durable:
// the durable writer's barrier on the state's directory, once the file is in
// place, is held until the test lets it go, and the answer is written after
// that barrier returns, never before; the file then holds the choice.
func TestTheChoiceIsDurableBeforeItIsAnswered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, dsdstate.FileName)
	log := &syncLog{}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(d string) error {
		if _, err := os.Stat(path); err != nil {
			return nil
		}
		once.Do(func() {
			log.note("sync begun")
			close(entered)
			<-release
			log.note("sync done")
		})
		return nil
	}))
	persistentMedium(t)
	store, err := dsdstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := newChoiceRig(t, playbacktest.NewBox(5), store, config.PlayerDevice{}, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodPut {
				w = &answerRecorder{ResponseWriter: w, log: log}
			}
			h.ServeHTTP(w, req)
		})
	})
	key, rev := r.choice(t)
	type answer struct {
		status int
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		status, _, err := r.send(body(key, rev, dsdstate.ModeConvert))
		done <- answer{status, err}
	}()
	select {
	case <-entered:
	case a := <-done:
		close(release)
		t.Fatalf("the write was answered %d (%v) before the state's barrier, once its file was in place, began; events %v", a.status, a.err, log.all())
	case <-time.After(time.Minute):
		close(release)
		t.Fatalf("the state's barrier never began once its file was in place; events %v", log.all())
	}
	close(release)
	if a := <-done; a.err != nil || a.status != http.StatusOK {
		t.Fatalf("PUT: %d %v", a.status, a.err)
	}
	if got := log.all(); strings.Join(got, ", ") != "sync begun, sync done, answered" {
		t.Errorf("the barrier and the answer came %v, want the answer after the barrier returned", got)
	}
	again, err := dsdstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode, _, own := again.State().ChoiceFor(playbacktest.DACKey()); !own || mode != dsdstate.ModeConvert {
		t.Errorf("the file holds %s (own %v), want convert", mode, own)
	}
}

// TestAStaleRevisionChangesNothing (B728 B9; binding notes 1 and 2): a write
// whose base is not the stored revision is 409 stale, carrying the current
// dsdDAC, whose revision a write-back takes; the state and what GET publishes
// are as they were, and one line says so.
func TestAStaleRevisionChangesNothing(t *testing.T) {
	store := playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	r := newChoiceRig(t, playbacktest.NewBox(5), store, config.PlayerDevice{}, nil)
	key, rev := r.choice(t)
	if status, m := r.put(t, body(key, rev, dsdstate.ModeConvert)); status != http.StatusOK {
		t.Fatalf("the first write: %d %s", status, m)
	}
	state, writes, published := store.State(), store.Writes(), r.get(t)
	status, m := r.put(t, body(key, rev, dsdstate.ModeOff))
	if status != http.StatusConflict || code(m) != "stale" {
		t.Fatalf("a write of a revision a write moved past: %d %s, want 409 stale", status, m)
	}
	var dac api.OutputDSDDAC
	if err := json.Unmarshal(m["dsdDAC"], &dac); err != nil || dac.Choice.Revision != rev+1 || dac.Choice.Mode != dsdstate.ModeConvert {
		t.Fatalf("the stale answer's dsdDAC %s, want the current choice, convert at revision %d", m["dsdDAC"], rev+1)
	}
	if store.Writes() != writes || !equalJSON(t, store.State(), state) {
		t.Errorf("a stale write changed the state")
	}
	now := r.get(t)
	for _, member := range []string{"dsdDAC", "dsdPlayback"} {
		if compactJSON(t, now[member]) != compactJSON(t, published[member]) {
			t.Errorf("a stale write moved GET's %s", member)
		}
	}
	if lines := r.lines.writeLines(); len(lines) != 2 || !strings.Contains(lines[1], "outcome=stale") {
		t.Errorf("the writes' lines %v, want one each, the second stale", lines)
	}
	if status, m := r.put(t, body(key, dac.Choice.Revision, dsdstate.ModeOff)); status != http.StatusOK {
		t.Errorf("a write-back of the stale answer's revision: %d %s", status, m)
	}
}

// equalJSON reports whether a and b encode alike.
func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(x, y)
}

// TestAChoiceForAnotherDACIsRefused (B728 B9; the design's section 7.2,
// check 3): a key that is not the bound DAC's, another model's or one GET
// published before another DAC took the binding's place, is 409 dac_changed,
// and nothing is written.
func TestAChoiceForAnotherDACIsRefused(t *testing.T) {
	box := playbacktest.NewBox(5)
	store := playbacktest.NewMemStore(dsdstate.Defaults())
	r := newChoiceRig(t, box, store, config.PlayerDevice{}, nil)
	key, rev := r.choice(t)
	if status, m := r.put(t, body("d1-0000000000000000", rev, dsdstate.ModeOff)); status != http.StatusConflict || code(m) != "dac_changed" {
		t.Errorf("another model's key: %d %s, want 409 dac_changed", status, m)
	}
	other := playbacktest.DACUnit()
	other.Product = "Another DAC"
	box.SetUnit(5, other)
	if status, m := r.put(t, body(key, rev, dsdstate.ModeOff)); status != http.StatusConflict || code(m) != "dac_changed" {
		t.Errorf("the key GET published before another DAC took the binding's place: %d %s, want 409 dac_changed", status, m)
	}
	if store.Writes() != 0 {
		t.Errorf("a refused write wrote the state %d times", store.Writes())
	}
}

// TestEachCheckRefusesInItsOrder (binding note 1; the design's section 7.2):
// a write that fails several checks answers the first of them in the
// design's order, through serve's view: the body, the DAC, the key, the
// revision, the mode, DoP's confirmation, first use; each writes nothing.
func TestEachCheckRefusesInItsOrder(t *testing.T) {
	box := playbacktest.NewBox(5)
	box.SetAltsets(5, 0, playbacktest.S32Altset(), playbacktest.S24Altset())
	store := playbacktest.NewMemStore(dsdstate.Defaults())
	r := newChoiceRig(t, box, store, config.PlayerDevice{}, nil)
	key, rev := r.choice(t)
	for _, tc := range []struct {
		name, body, want string
	}{
		{"a body and a key wrong", `{"dacKey":"d1-0000000000000000","mode":"off"}`, "bad_request"},
		{"a key and a revision wrong", body("d1-0000000000000000", rev+3, dsdstate.ModeNative), "dac_changed"},
		{"a revision and a mode wrong", body(key, rev+3, dsdstate.ModeNative), "stale"},
		{"a mode not offered, unacknowledged", body(key, rev, dsdstate.ModeNative), "mode_not_offered"},
		{"DoP unconfirmed and unacknowledged", body(key, rev, dsdstate.ModeDoP), "dop_confirmation_required"},
		{"DoP confirmed and unacknowledged", body(key, rev, dsdstate.ModeDoP, `"confirmDoP":true`), "first_use_unacknowledged"},
	} {
		status, m := r.put(t, tc.body)
		if code(m) != tc.want || (status != http.StatusConflict && status != http.StatusBadRequest) {
			t.Errorf("%s: %d %s, want %s", tc.name, status, m, tc.want)
		}
	}
	if store.Writes() != 0 {
		t.Errorf("refused writes wrote the state %d times", store.Writes())
	}
}

// TestAKeyRoundTripsWhateverTheUnitsStringsHold (binding note 12): the key a
// write names is compared with the one GET publishes, which the unit's own
// serial and port are taken out of first, so a product string that holds the
// serial, or the port in any of the spellings a USB string holds it in, makes
// a key GET publishes and a write takes; the write lands on the DAC's own
// entry, keyed by its product string as the unit reports it, and neither the
// answer nor the log names the serial or the port.
func TestAKeyRoundTripsWhateverTheUnitsStringsHold(t *testing.T) {
	const serial = "TESTDAC-0001"
	port := playbacktest.DACUSB(0).Port
	products := []string{"Test DAC " + serial}
	for _, form := range playback.PortForms(port) {
		products = append(products, "Test DAC "+form)
	}
	for _, product := range products {
		t.Run(product, func(t *testing.T) {
			box := playbacktest.NewBox(5)
			m := playbacktest.DACUnit()
			m.Product = product
			box.SetUnit(5, m)
			store := playbacktest.NewMemStore(dsdstate.Defaults())
			r := newChoiceRig(t, box, store, config.PlayerDevice{}, nil)
			key, rev := r.choice(t)
			if !dacKeyForm.MatchString(key) {
				t.Fatalf("GET publishes the key %q", key)
			}
			status, answer := r.put(t, body(key, rev, dsdstate.ModeConvert))
			if status != http.StatusOK {
				t.Fatalf("PUT of the key GET published: %d %s", status, answer)
			}
			var dac api.OutputDSDDAC
			if err := json.Unmarshal(answer["dsdDAC"], &dac); err != nil || dac.DACKey != key {
				t.Errorf("the answer's key %q, want %q", dac.DACKey, key)
			}
			own, err := dsdstate.KeyOf(m.USB, product)
			if err != nil {
				t.Fatal(err)
			}
			if mode, _, ok := store.State().ChoiceFor(own); !ok || mode != dsdstate.ModeConvert {
				t.Errorf("the DAC's own entry holds %s (own %v), want convert", mode, ok)
			}
			raw, err := json.Marshal(answer)
			if err != nil {
				t.Fatal(err)
			}
			lines := r.lines.writeLines()
			if len(lines) != 1 || !strings.Contains(lines[0], "dac="+key) {
				t.Errorf("the write's lines %v, want one naming the key %s", lines, key)
			}
			for _, secret := range append([]string{serial}, playback.PortForms(port)...) {
				if strings.Contains(string(raw), secret) || strings.Contains(r.lines.all(), secret) {
					t.Errorf("the answer or the log names %q", secret)
				}
			}
		})
	}
}

// TestALandedChoiceIsUncertainAndFencesTheNext (binding note 5): a write whose
// rename landed and whose barrier failed is 500 persist_uncertain, and so is
// every later write, which the store fences; GET then publishes a rate only
// where the state in memory and the file's both play it alike.
func TestALandedChoiceIsUncertainAndFencesTheNext(t *testing.T) {
	store := playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	r := newChoiceRig(t, playbacktest.NewBox(5), store, config.PlayerDevice{}, nil)
	key, rev := r.choice(t)
	store.Land(fmt.Errorf("the directory's barrier failed: %w", atomicwrite.ErrLanded))
	if status, m := r.put(t, body(key, rev, dsdstate.ModeNative, fmt.Sprintf(`"maxBitRate":%d`, playbacktest.DSD128))); status != http.StatusInternalServerError || code(m) != "persist_uncertain" {
		t.Fatalf("a landed write: %d %s, want 500 persist_uncertain", status, m)
	}
	var dsd api.OutputDSDPlayback
	if err := json.Unmarshal(r.get(t)["dsdPlayback"], &dsd); err != nil {
		t.Fatal(err)
	}
	if len(dsd.Refused) != 1 || dsd.Refused[0] != (api.OutputDSDRefused{BitRate: playbacktest.DSD256, Reason: "dsd_choice_uncertain"}) {
		t.Errorf("GET's refusals after a landed write %+v, want DSD256 refused as dsd_choice_uncertain", dsd.Refused)
	}
	if status, m := r.put(t, body(key, rev, dsdstate.ModeOff)); status != http.StatusInternalServerError || code(m) != "persist_uncertain" {
		t.Errorf("a write after a landed one: %d %s, want 500 persist_uncertain", status, m)
	}
}

// TestServeTakesAChoiceThroughItsRoute (binding note 9): serve wires the
// choice's route to its player: /v1/health advertises bridgeDSDChoice, a
// write of the key GET /v1/outputs publishes lands in the player's DSD state
// in the data directory, and GET then publishes what the write answered.
func TestServeTakesAChoiceThroughItsRoute(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newDSDBox(t)
	goBarDSD(t, box.trees)
	probes := newProbeStandIn()
	var bearer string
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, seams: noControlNodes(),
		before: func(dataDir string) {
			tokens, err := auth.OpenStore(filepath.Join(dataDir, tokensFileName))
			if err != nil {
				t.Fatal(err)
			}
			if bearer, _, err = tokens.Mint("phone"); err != nil {
				t.Fatal(err)
			}
		},
		with: func(o *serveOpts) {
			box.with(o)
			o.playerBackend = probes.backend
		}})
	defer stopped(t, ps)
	availableOutput(t, ps.runningEngine(t))
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true}}
	do := func(method, path, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, "https://"+ps.addr+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	_, raw := do(http.MethodGet, "/v1/health", "")
	if !strings.Contains(string(raw), `"bridgeDSDChoice"`) {
		t.Errorf("/v1/health advertises no bridgeDSDChoice: %s", raw)
	}
	// The policy read once more, as the supervisor reads it at the snapshot
	// that made the output available, so the set is published now.
	be, ok := ps.player.be.(*playback.Backend)
	if !ok {
		t.Fatalf("serve's player runs %T, want the backend serve builds", ps.player.be)
	}
	be.RefreshDSD(true)
	bridge := func() map[string]json.RawMessage {
		t.Helper()
		status, raw := do(http.MethodGet, "/v1/outputs", "")
		if status != http.StatusOK {
			t.Fatalf("GET /v1/outputs: %d %s", status, raw)
		}
		return bridgeObjectOf(t, raw)
	}
	var dac api.OutputDSDDAC
	if err := json.Unmarshal(bridge()["dsdDAC"], &dac); err != nil {
		t.Fatalf("serve's player publishes no dsdDAC: %v", err)
	}
	status, answer := do(http.MethodPut, "/v1/outputs/bridge:"+ps.player.id+"/dsd", body(dac.DACKey, dac.Choice.Revision, dsdstate.ModeConvert))
	if status != http.StatusOK {
		t.Fatalf("PUT: %d %s", status, answer)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(answer, &got); err != nil {
		t.Fatal(err)
	}
	after := bridge()
	if compactJSON(t, after["dsdDAC"]) != compactJSON(t, got["dsdDAC"]) {
		t.Errorf("GET's dsdDAC after the write:\n%s\nthe answer's:\n%s", after["dsdDAC"], got["dsdDAC"])
	}
	again, err := dsdstate.Open(ps.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range again.State().DACs {
		if d.Choice != nil && d.Choice.Mode == dsdstate.ModeConvert && d.Choice.SetBy == dsdstate.ByApp {
			found = true
		}
	}
	if !found {
		t.Errorf("the player's DSD state in the data directory holds no convert the app chose: %+v", again.State())
	}
}
