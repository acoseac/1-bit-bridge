package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// PUT /v1/outputs/{id}/dsd through serve's adapter (B728 B9 PR 3): the
// API's route over serve's view of a player (bridgeOutput), the real engine
// running, over the real backend or serve's stand-in, as main.go wires them.

// choiceTestHeader names a test's PUT, so the rig hands each its handler's
// answer, whether or not its client stayed for it; choiceBudgetHeader gives a
// PUT a route's time shorter than the route's own, a deadline the server sets.
const (
	choiceTestHeader   = "X-Choice-Test"
	choiceBudgetHeader = "X-Choice-Budget"
)

// choiceAnswer is what one PUT's handler wrote: its status and its bytes.
type choiceAnswer struct {
	status int
	body   []byte
}

// answerRecorder keeps what a handler writes.
type answerRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (a *answerRecorder) WriteHeader(code int) {
	if a.status == 0 {
		a.status = code
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *answerRecorder) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	a.body.Write(b)
	return a.ResponseWriter.Write(b)
}

// choiceRig is the API's routes over serve's view of a player whose real
// engine runs over a backend until the test ends, the output available and
// the transport stopped, behind a real HTTP server.
type choiceRig struct {
	eng    *engine.Engine
	view   *bridgeOutput
	base   string
	bearer string
	client *http.Client

	// budget, when set, is each PUT's route's time (choiceBudgetHeader).
	budget time.Duration

	mu      sync.Mutex
	n       int
	answers map[string]chan choiceAnswer
}

// newChoiceRig runs an engine over be and serves its view.
func newChoiceRig(t *testing.T, be playBackend) *choiceRig {
	t.Helper()
	needsDirectorySync(t)
	persistentMedium(t)
	dir := t.TempDir()
	eng, err := engine.New(engine.Config{Backend: be, PlayerUUID: thePlayerUUID, DataDir: dir, QueuePath: filepath.Join(dir, "queue.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(ctx) }()
	joinBackendOnCleanup(t, be)
	drainLoopOnCleanup(t, cancel, done, "the DSD choice rig's engine")
	if err := eng.ReportOutput(ctx, engine.OutputReport{Generation: 1, Available: true}); err != nil {
		t.Fatal(err)
	}
	availableOutput(t, eng)
	ready := make(chan struct{})
	close(ready)
	cfg := playerOutputConfig(t.TempDir(), nil)
	p := &bridgePlayer{ready: ready, eng: eng, be: be, id: thePlayerUUID, device: cfg.Output.Player.Device}
	return serveChoiceRoute(t, eng, newBridgeOutput(servePlayerPlan{lists: true, start: true}, p, cfg))
}

// joinBackendOnCleanup joins be's goroutines in a cleanup, until a drain's
// give-up (serveDrainReserve before the test binary's deadline, with no bound
// where it has none), so a wait that gave up before it still leaves the join
// its time.
func joinBackendOnCleanup(t *testing.T, be interface{ Wait(context.Context) error }) {
	t.Cleanup(func() {
		ctx := context.Background()
		if at := serveDeadlineLess(t, serveDrainReserve); !at.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, at)
			defer cancel()
		}
		if err := be.Wait(ctx); err != nil {
			t.Error(err)
		}
	})
}

// serveChoiceRoute serves the API's routes over view, as main.go wires them.
func serveChoiceRoute(t *testing.T, eng *engine.Engine, view *bridgeOutput) *choiceRig {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), tokensFileName))
	if err != nil {
		t.Fatal(err)
	}
	bearer, _, err := store.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(&config.Config{ListenAddress: ":7788", LibraryName: "T", LibraryRoots: []string{t.TempDir()}}, store, nil, "fp").
		WithLocalOutputs(api.LocalOutputsConfig{GOOS: "linux", Output: func() config.OutputConfig { return config.OutputConfig{MPD: "off"} }}).
		WithBridgeOutput(view).
		WithBridgeDSDChoice(view)
	r := &choiceRig{eng: eng, view: view, bearer: bearer, answers: map[string]chan choiceAnswer{}}
	handler := srv.Handler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if d, err := time.ParseDuration(req.Header.Get(choiceBudgetHeader)); err == nil {
			ctx, cancel := context.WithTimeout(req.Context(), d)
			defer cancel()
			req = req.WithContext(ctx)
		}
		rec := &answerRecorder{ResponseWriter: w}
		handler.ServeHTTP(rec, req)
		r.mu.Lock()
		ch := r.answers[req.Header.Get(choiceTestHeader)]
		r.mu.Unlock()
		if ch != nil {
			ch <- choiceAnswer{status: rec.status, body: rec.body.Bytes()}
		}
	}))
	t.Cleanup(hs.Close)
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	r.base, r.client = hs.URL, &http.Client{Transport: transport}
	return r
}

// request is a PUT of body for the player's element on ctx, and the channel
// its handler's answer comes on.
func (r *choiceRig) request(t *testing.T, ctx context.Context, body string) (*http.Request, <-chan choiceAnswer) {
	t.Helper()
	r.mu.Lock()
	r.n++
	id := strconv.Itoa(r.n)
	ch := make(chan choiceAnswer, 1)
	r.answers[id] = ch
	r.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, r.base+"/v1/outputs/"+api.BridgeOutputID(thePlayerUUID)+"/dsd", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(choiceTestHeader, id)
	if r.budget > 0 {
		req.Header.Set(choiceBudgetHeader, r.budget.String())
	}
	return req, ch
}

// put sends body and answers what the handler wrote.
func (r *choiceRig) put(t *testing.T, body string) choiceAnswer {
	t.Helper()
	ch, errc := r.putAsync(t, context.Background(), body)
	a := r.wait(t, ch, "the PUT's answer")
	if err := <-errc; err != nil {
		t.Fatalf("the PUT's client: %v", err)
	}
	return a
}

// putAsync sends body on a goroutine of its own, on ctx: the handler's answer
// comes on the first channel, and the client's error on the second.
func (r *choiceRig) putAsync(t *testing.T, ctx context.Context, body string) (<-chan choiceAnswer, <-chan error) {
	t.Helper()
	req, ch := r.request(t, ctx, body)
	errc := make(chan error, 1)
	go func() {
		resp, err := r.client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		errc <- err
	}()
	return ch, errc
}

// wait waits for an answer on ch until the test's deadline.
func (r *choiceRig) wait(t *testing.T, ch <-chan choiceAnswer, what string) choiceAnswer {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-serveGiveUp(t):
		t.Fatalf("waiting for %s", what)
	}
	return choiceAnswer{}
}

// bridge is GET /v1/outputs' bridge object.
func (r *choiceRig) bridge(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, r.base+"/v1/outputs", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.bearer)
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/outputs: %d %s %v", resp.StatusCode, body, err)
	}
	return bridgeObjectOf(t, body)
}

// dac is GET /v1/outputs' dsdDAC, as a client holds it.
func (r *choiceRig) dac(t *testing.T) api.OutputDSDDAC {
	t.Helper()
	raw, ok := r.bridge(t)["dsdDAC"]
	if !ok {
		t.Fatal("GET /v1/outputs carries no dsdDAC")
	}
	var d api.OutputDSDDAC
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// choiceBody is a write of mode made from d, the dsdDAC a client holds, with
// extra members (`"confirmDoP":true`) after.
func choiceBody(t *testing.T, d api.OutputDSDDAC, mode string, extra ...string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"dacKey": d.DACKey, "dacToken": d.DACToken, "baseRevision": d.Choice.Revision, "mode": mode})
	if err != nil {
		t.Fatal(err)
	}
	s := strings.TrimSuffix(string(b), "}")
	for _, e := range extra {
		s += "," + e
	}
	return s + "}"
}

// dacOf is the dsdDAC a client holding p's would send a write from.
func dacOf(t *testing.T, p playback.PlayerPolicy) api.OutputDSDDAC {
	t.Helper()
	if p.DSD == nil || p.DSD.DAC == nil {
		t.Fatalf("the policy %+v names no DAC", p)
	}
	d := p.DSD.DAC
	return api.OutputDSDDAC{DACKey: d.Key, DACToken: string(d.Token), Choice: api.OutputDSDChoice{Mode: d.Choice.Mode, Revision: d.Choice.Revision}}
}

// answerObject is an answer's body, by member.
func answerObject(t *testing.T, a choiceAnswer) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(a.body, &m); err != nil {
		t.Fatalf("the answer %d %q is no JSON object: %v", a.status, a.body, err)
	}
	return m
}

// dsdMembers are the four DSD members an object carries, raw.
func dsdMembers(obj map[string]json.RawMessage) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"dsdPlayback", "dsdDAC", "dsdMode", "allowDoP"} {
		if v, ok := obj[k]; ok {
			out[k] = string(v)
		}
	}
	return out
}

// wantAnswer requires a's status and code, and its members: the four of an
// epoch naming a DAC, the three of one naming none, or none.
func wantAnswer(t *testing.T, what string, a choiceAnswer, status int, code string, members int) map[string]json.RawMessage {
	t.Helper()
	obj := answerObject(t, a)
	if a.status != status {
		t.Fatalf("%s: %d %s, want %d %s", what, a.status, a.body, status, code)
	}
	if code != "" && string(obj["error"]) != `"`+code+`"` {
		t.Fatalf("%s: %s, want the code %s", what, a.body, code)
	}
	want := map[int][]string{0: nil, 3: {"allowDoP", "dsdMode", "dsdPlayback"}, 4: {"allowDoP", "dsdDAC", "dsdMode", "dsdPlayback"}}[members]
	var got []string
	for k := range dsdMembers(obj) {
		got = append(got, k)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("%s: the members %q, want %q: %s", what, got, want, a.body)
	}
	return obj
}

// dsdDACOf is an answer's dsdDAC.
func dsdDACOf(t *testing.T, obj map[string]json.RawMessage) api.OutputDSDDAC {
	t.Helper()
	var d api.OutputDSDDAC
	if err := json.Unmarshal(obj["dsdDAC"], &d); err != nil {
		t.Fatalf("dsdDAC %s: %v", obj["dsdDAC"], err)
	}
	return d
}

// dsdPlaybackOf is an answer's or a GET's dsdPlayback.
func dsdPlaybackOf(t *testing.T, obj map[string]json.RawMessage) api.OutputDSDPlayback {
	t.Helper()
	var p api.OutputDSDPlayback
	if err := json.Unmarshal(obj["dsdPlayback"], &p); err != nil {
		t.Fatalf("dsdPlayback %s: %v", obj["dsdPlayback"], err)
	}
	return p
}

// withKnownDAC is the profile database that knows the test DAC.
func withKnownDAC(p *playback.DSDPolicy) { p.Profiles = playbacktest.KnownProfiles(nil) }

// tokenForm is a dacToken's form.
var tokenForm = regexp.MustCompile(`^t1-[0-9a-f]{32}$`)

// TestTheDACTokenRoundTrips (B728 B9 PR 3): a client's write made from the
// dsdDAC GET published is written, its answer's four members GET's next;
// the same body again is stale, applied once; a unit with no manufacturer
// string has a token of its form that round-trips; and a backend of another
// boot over the same state, its token key another, refuses a write made
// from the first's read side dac_changed, its answer carrying its own
// token, from which a write is written.
func TestTheDACTokenRoundTrips(t *testing.T) {
	store := playbacktest.NewMemStore(dsdstate.Defaults())
	box := playbacktest.NewBox(5)
	r := newChoiceRig(t, readSideBackend(t, box, store, withKnownDAC))
	d := r.dac(t)
	if !tokenForm.MatchString(d.DACToken) {
		t.Fatalf("the published token %q is not of its form", d.DACToken)
	}
	body := choiceBody(t, d, dsdstate.ModeConvert)
	obj := wantAnswer(t, "the write", r.put(t, body), http.StatusOK, "", 4)
	if got, want := dsdMembers(obj), dsdMembers(r.bridge(t)); !maps.Equal(got, want) {
		t.Errorf("the answer's members %v are not GET's next %v", got, want)
	}
	if c := dsdDACOf(t, obj).Choice; c.Mode != dsdstate.ModeConvert || c.Revision != 1 {
		t.Errorf("the answer's choice %+v, want convert at revision 1", c)
	}
	wantAnswer(t, "the same write again", r.put(t, body), http.StatusConflict, "stale", 4)

	t.Run("a unit with no manufacturer string", func(t *testing.T) {
		box := playbacktest.NewBox(5)
		m := playbacktest.DACUnit()
		m.Manufacturer = ""
		box.SetUnit(5, m)
		r := newChoiceRig(t, readSideBackend(t, box, playbacktest.NewMemStore(dsdstate.Defaults()), withKnownDAC))
		d := r.dac(t)
		if !tokenForm.MatchString(d.DACToken) {
			t.Fatalf("the token %q of a unit with no maker is not of its form", d.DACToken)
		}
		wantAnswer(t, "a write from its read side", r.put(t, choiceBody(t, d, dsdstate.ModeOff)), http.StatusOK, "", 4)
	})

	t.Run("a backend of another boot over the same state", func(t *testing.T) {
		held := r.dac(t)
		other := newChoiceRig(t, readSideBackendKeyed(t, []byte("another boot's token key, 32 b.."), nil, box, store, withKnownDAC))
		obj := wantAnswer(t, "a write from the first boot's read side", other.put(t, choiceBody(t, held, dsdstate.ModeOff)), http.StatusConflict, "dac_changed", 4)
		now := dsdDACOf(t, obj)
		if now.DACToken == held.DACToken || now.DACToken != other.dac(t).DACToken || now.DACKey != held.DACKey {
			t.Fatalf("the answer's token %q (key %s): want this boot's %q, the key %s, not the first boot's %q",
				now.DACToken, now.DACKey, other.dac(t).DACToken, held.DACKey, held.DACToken)
		}
		wantAnswer(t, "a write from the answer's dsdDAC", other.put(t, choiceBody(t, now, dsdstate.ModeOff)), http.StatusOK, "", 4)
	})
}

// TestAWriteFromAnotherUnitsReadSideIsRefusedThroughTheRoute (B728 B9 PR 3):
// a write made from the read side of a unit of the bound unit's key but of
// another maker, the bound unit swapped in after the read, is dac_changed,
// whether both makers' entries hold no choice (revisions 0 and 0) or one at
// one revision (N and N): neither entry changed, and the answer names the
// unit bound now, its token the one the next GET publishes.
func TestAWriteFromAnotherUnitsReadSideIsRefusedThroughTheRoute(t *testing.T) {
	other := playbacktest.DACUnit()
	other.Manufacturer = "Another Maker"
	for _, tc := range []struct {
		name string
		st   func() dsdstate.State
	}{
		{"revisions 0 and 0", dsdstate.Defaults},
		{"revisions N and N", func() dsdstate.State {
			st := playbacktest.ChoiceState(dsdstate.ModeNative)
			theirs := st.DACs[0]
			theirs.Stamp = dsdstate.StampOf(other.Manufacturer)
			theirs.Choice = &dsdstate.Choice{Mode: dsdstate.ModeConvert, Revision: 1, SetAt: playbacktest.T0, SetBy: dsdstate.ByApp}
			st.DACs = append(st.DACs, theirs)
			return st
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := playbacktest.NewMemStore(tc.st())
			box := playbacktest.NewBox(5)
			r := newChoiceRig(t, readSideBackend(t, box, store, withKnownDAC))
			d := r.dac(t)
			before := store.State()
			box.SetUnit(5, other)
			obj := wantAnswer(t, "the write", r.put(t, choiceBody(t, d, dsdstate.ModeOff)), http.StatusConflict, "dac_changed", 4)
			if now := dsdDACOf(t, obj); now.DACToken == d.DACToken || now.DACToken != r.dac(t).DACToken {
				t.Errorf("the answer names %q, want the bound unit's token as GET publishes it next, not the read side's %q", now.DACToken, d.DACToken)
			}
			if after := store.State(); !reflect.DeepEqual(after, before) || store.Writes() != 0 {
				t.Errorf("an entry changed: %d writes", store.Writes())
			}
		})
	}
}

// TestAFirmwareChangeBetweenTheReadAndTheWriteIsDACChanged (B728 B9 PR 3):
// the bound unit's firmware changing between a client's read and its write
// makes the write dac_changed, the answer carrying the token and firmware of
// the unit as the write read it.
func TestAFirmwareChangeBetweenTheReadAndTheWriteIsDACChanged(t *testing.T) {
	store := playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	box := playbacktest.NewBox(5)
	r := newChoiceRig(t, readSideBackend(t, box, store, withKnownDAC))
	d := r.dac(t)
	m := playbacktest.DACUnit()
	m.BCDDevice = "0200"
	box.SetUnit(5, m)
	obj := wantAnswer(t, "the write", r.put(t, choiceBody(t, d, dsdstate.ModeOff)), http.StatusConflict, "dac_changed", 4)
	if now := dsdDACOf(t, obj); now.DACToken == d.DACToken || now.DACKey != d.DACKey {
		t.Errorf("the answer's DAC %s/%s, want the key %s under another token than %s", now.DACKey, now.DACToken, d.DACKey, d.DACToken)
	}
	if p := dsdPlaybackOf(t, obj); p.BCDDevice != "0200" {
		t.Errorf("the answer's firmware %q, want the reading's 0200", p.BCDDevice)
	}
	if store.Writes() != 0 {
		t.Errorf("the state was written %d times", store.Writes())
	}
}

// TestAMakerChangeBetweenPublicationAndGETIsOneObservation (B728 B9 PR 3):
// GET copies the pair the backend published and reads nothing of the box, so
// a maker changed after the publication and before a GET leaves the GET one
// observation, its token the publication's own beside that publication's key
// and choice; a write made from it is dac_changed, read against the box as
// it is now, and its answer and the next GET name the new maker's unit.
func TestAMakerChangeBetweenPublicationAndGETIsOneObservation(t *testing.T) {
	box := playbacktest.NewBox(5)
	r := newChoiceRig(t, readSideBackend(t, box, playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative)), withKnownDAC))
	first := r.bridge(t)
	m := playbacktest.DACUnit()
	m.Manufacturer = "Another Maker"
	box.SetUnit(5, m)
	reads := box.Reads()
	second := r.bridge(t)
	if got, want := dsdMembers(second), dsdMembers(first); !maps.Equal(got, want) {
		t.Fatalf("a GET after the maker changed says %v, want the publication's %v", got, want)
	}
	if box.Reads() != reads {
		t.Errorf("GET read the box %d times", box.Reads()-reads)
	}
	held := dsdDACOf(t, second)
	obj := wantAnswer(t, "a write from that GET", r.put(t, choiceBody(t, held, dsdstate.ModeOff)), http.StatusConflict, "dac_changed", 4)
	now := dsdDACOf(t, obj)
	if now.DACToken == held.DACToken {
		t.Fatalf("the answer names the token the write was made from, %s", now.DACToken)
	}
	if next := dsdDACOf(t, r.bridge(t)); next.DACToken != now.DACToken {
		t.Errorf("the next GET's token %s, want the answer's %s", next.DACToken, now.DACToken)
	}
}

// stampForm is a maker's stamp as the state keeps it: never on a wire.
var stampForm = regexp.MustCompile(`s1-[0-9a-f]{64}`)

// TestNoStampReachesTheWire (B728 B9 PR 3): no body of GET /v1/outputs or of
// the PUT holds a maker's stamp, of the form or the bound unit's, nor a
// manufacturer string's serial; the token is keyed, so two token keys give
// one unit two tokens.
func TestNoStampReachesTheWire(t *testing.T) {
	m := playbacktest.DACUnit()
	m.Manufacturer = "Test Maker TESTDAC-0001"
	box := playbacktest.NewBox(5)
	box.SetUnit(5, m)
	store := playbacktest.NewMemStore(dsdstate.Defaults())
	r := newChoiceRig(t, readSideBackend(t, box, store, withKnownDAC))
	stamp := string(dsdstate.StampOf(m.Manufacturer))
	var bodies [][]byte
	get := func() {
		raw, err := json.Marshal(r.bridge(t))
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, raw)
	}
	get()
	d := r.dac(t)
	for _, body := range []string{choiceBody(t, d, dsdstate.ModeConvert), choiceBody(t, d, dsdstate.ModeOff)} {
		bodies = append(bodies, r.put(t, body).body)
		get()
	}
	for _, b := range bodies {
		for _, bad := range []string{stamp, strings.TrimPrefix(stamp, "s1-"), "TESTDAC-0001", m.Manufacturer} {
			if bytes.Contains(b, []byte(bad)) {
				t.Errorf("a body holds %q: %s", bad, b)
			}
		}
		if stampForm.Match(b) {
			t.Errorf("a body holds a stamp: %s", b)
		}
	}
	other := readSideBackendKeyed(t, []byte("another boot's token key, 32 b.."), nil, box, store, withKnownDAC)
	if a, b := d.DACToken, string(other.Policy().DSD.DAC.Token); a == b || !tokenForm.MatchString(b) {
		t.Errorf("two token keys give one unit the tokens %s and %s", a, b)
	}
}

// TestTheAnswerIsTheEffectiveChoice (plan §11; B728 B9 PR 3): with nothing
// but the write publishing, the PUT's four members are byte for byte the next
// GET's, for a choice written and for a refusal.
func TestTheAnswerIsTheEffectiveChoice(t *testing.T) {
	r := newChoiceRig(t, readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative)), withKnownDAC))
	for _, w := range []struct {
		what, mode string
		status     int
		code       string
	}{
		{"a choice written", dsdstate.ModeConvert, http.StatusOK, ""},
		{"a refusal", dsdstate.ModeDoP, http.StatusConflict, "stale"},
	} {
		d := r.dac(t)
		if w.code == "stale" {
			d.Choice.Revision--
		}
		obj := wantAnswer(t, w.what, r.put(t, choiceBody(t, d, w.mode)), w.status, w.code, 4)
		if got, want := dsdMembers(obj), dsdMembers(r.bridge(t)); !maps.Equal(got, want) {
			t.Errorf("%s: the answer's members %v are not GET's next %v", w.what, got, want)
		}
	}
}

// TestAnExplicitChoiceWithNoAcknowledgementIsWrittenThroughTheRoute (B990):
// real PUTs that carry no firstUseAcknowledged. An explicit native is
// written, 200, its answer's set native at every rate and first use still
// pending; an explicit dop carrying confirmDoP alone is written and plays DoP
// at every rate; and a native chosen over a convert whose state holds a
// listener's noise at DSD64 is the retry that clears it, native playing at
// every rate.
func TestAnExplicitChoiceWithNoAcknowledgementIsWrittenThroughTheRoute(t *testing.T) {
	r := newChoiceRig(t, readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Defaults()), withKnownDAC))
	body := choiceBody(t, r.dac(t), dsdstate.ModeNative)
	if strings.Contains(body, "firstUseAcknowledged") {
		t.Fatalf("the body %s carries the acknowledgement", body)
	}
	obj := wantAnswer(t, "an explicit native", r.put(t, body), http.StatusOK, "", 4)
	wantEveryRateTuples(t, "the native write's set", dsdPlaybackOf(t, obj), "")
	for _, f := range dsdDACOf(t, obj).FirstUse {
		if f.State != playback.FirstUsePending {
			t.Errorf("%s's first use is %s after a write that acknowledged nothing", f.Transport, f.State)
		}
	}
	obj = wantAnswer(t, "an explicit dop with confirmDoP", r.put(t, choiceBody(t, dsdDACOf(t, obj), dsdstate.ModeDoP, `"confirmDoP":true`)), http.StatusOK, "", 4)
	wantEveryRateTuples(t, "the dop write's set", dsdPlaybackOf(t, obj), "dop")

	st := dsdstate.Defaults()
	st.DACs = []dsdstate.DAC{{Key: playbacktest.DACKey(), Stamp: playbacktest.DACStamp(),
		Choice:   &dsdstate.Choice{Mode: dsdstate.ModeConvert, Revision: 1, SetAt: playbacktest.T0, SetBy: dsdstate.ByApp},
		Listener: []dsdstate.Answer{{Transport: dsdstate.TransportNative, BitRate: playbacktest.DSD64, BCDDevice: playbacktest.DACBCD, Answer: dsdstate.AnswerNoise, On: "2026-11-21"}}}}
	retry := newChoiceRig(t, readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(st), withKnownDAC))
	obj = wantAnswer(t, "native chosen over convert", retry.put(t, choiceBody(t, retry.dac(t), dsdstate.ModeNative)), http.StatusOK, "", 4)
	wantEveryRateTuples(t, "the retry's set", dsdPlaybackOf(t, obj), "")
}

// wantEveryRateTuples requires p to publish a tuple of transport ("" native)
// at DSD64, DSD128 and DSD256, and nothing converted or refused.
func wantEveryRateTuples(t *testing.T, what string, p api.OutputDSDPlayback, transport string) {
	t.Helper()
	var rates []int
	for _, tu := range p.Tuples {
		if tu.Transport != transport {
			t.Errorf("%s: a tuple at %d is %q, want %q", what, tu.BitRate, tu.Transport, transport)
		}
		rates = append(rates, tu.BitRate)
	}
	if !slices.Equal(rates, []int{playbacktest.DSD64, playbacktest.DSD128, playbacktest.DSD256}) || len(p.Converted) != 0 || len(p.Refused) != 0 {
		t.Errorf("%s: tuples %+v, converted %+v, refused %+v; want %q at DSD64, DSD128 and DSD256 alone", what, p.Tuples, p.Converted, p.Refused, transport)
	}
}
