package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// fakeDSDChoice is serve's player as PUT /v1/outputs/{id}/dsd writes through
// it: the player's element is bridge: and testPlayerUUID, and a write answers
// answer, counted, after wait when one is set (a write that waits for its
// context, say).
type fakeDSDChoice struct {
	calls  atomic.Int32
	mu     sync.Mutex
	answer BridgeDSDChoiceResult
	got    []BridgeDSDChoiceRequest
	wait   func(ctx context.Context) BridgeDSDChoiceResult
}

func (f *fakeDSDChoice) Lists(id string) bool { return id == BridgeOutputID(testPlayerUUID) }

func (f *fakeDSDChoice) ChooseDSD(ctx context.Context, req BridgeDSDChoiceRequest) BridgeDSDChoiceResult {
	f.calls.Add(1)
	f.mu.Lock()
	f.got = append(f.got, req)
	answer, wait := f.answer, f.wait
	f.mu.Unlock()
	if wait != nil {
		return wait(ctx)
	}
	return answer
}

func (f *fakeDSDChoice) set(r BridgeDSDChoiceResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = r
}

// choiceAnswer is what one PUT's handler wrote, as the server's side saw it:
// the status and the bytes, whether or not the client stayed for them.
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

// choiceHTTP is the route over a real HTTP server: the API's handler with the
// player's gate as player says, MPD's as cfg says, c the write when not nil
// and budget the route's when not 0, each PUT's answer sent on answers as the
// handler wrote it.
type choiceHTTP struct {
	hs      *httptest.Server
	token   string
	srv     *Server
	answers chan choiceAnswer
}

func newChoiceHTTP(t *testing.T, cfg LocalOutputsConfig, player bool, c BridgeDSDChoice, budget time.Duration) *choiceHTTP {
	t.Helper()
	store := outputsToken(t)
	raw, _, err := store.Mint("phone")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	conf := &config.Config{ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600, LibraryName: "T", LibraryRoots: []string{t.TempDir()}}
	if cfg.Getenv == nil {
		cfg.Getenv = func(string) string { return "" }
	}
	b := &fakeBridgeOutput{enabled: player}
	b.set(listedPlayer())
	srv := New(conf, store, nil, "fp").WithLocalOutputs(cfg).WithBridgeOutput(b)
	if c != nil {
		srv.WithBridgeDSDChoice(c)
	}
	srv.dsdChoiceTimeout = budget
	h := &choiceHTTP{token: raw, srv: srv, answers: make(chan choiceAnswer, 16)}
	handler := srv.Handler()
	h.hs = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &answerRecorder{ResponseWriter: w}
		handler.ServeHTTP(rec, r)
		if r.Method == http.MethodPut {
			h.answers <- choiceAnswer{status: rec.status, body: rec.body.Bytes()}
		}
	}))
	t.Cleanup(h.hs.Close)
	return h
}

// put sends body as a PUT of the DSD choice of the output id, with the
// bearer unless bare, and answers what the server's handler wrote.
func (h *choiceHTTP) put(t *testing.T, ctx context.Context, id, body string, bare bool) (choiceAnswer, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, h.hs.URL+"/v1/outputs/"+id+"/dsd", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if !bare {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	resp, clientErr := h.hs.Client().Do(req)
	if clientErr == nil {
		resp.Body.Close()
	}
	select {
	case a := <-h.answers:
		return a, clientErr
	case <-time.After(30 * time.Second):
		t.Fatalf("PUT %s: the handler never answered", id)
	}
	return choiceAnswer{}, nil
}

// putOK is put on a context of its own, failing on a client error.
func (h *choiceHTTP) putOK(t *testing.T, id, body string) choiceAnswer {
	t.Helper()
	a, err := h.put(t, context.Background(), id, body, false)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	return a
}

// aChoice is a body every check of the route's takes.
const aChoice = `{"dacKey":"d1-0123456789abcdef","dacToken":"t1-0123456789abcdef0123456789abcdef","baseRevision":4,"mode":"auto"}`

// bridgeID is the player's element's id.
var bridgeID = BridgeOutputID(testPlayerUUID)

// answerKeys is a body's members, sorted.
func answerKeys(t *testing.T, body []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("the answer %q is no JSON object: %v", body, err)
	}
	return keysOf(m)
}

// TestThePUTsChecksBeforeTheWrite (B728 B9 PR 3): the route's checks before
// serve's write, in PROTOCOL.md's order, none of which calls the write: the
// bearer (401) and the write bucket (429); the write's gate, the player's and
// the write wired, its 404 word by the outputs route's answer as it stands,
// output_not_found while the player's gate or MPD's last answer holds and
// local_outputs_not_supported otherwise, MPD's asked without a probe (H13);
// an id that is not the player's element (an MPD output's, another UUID,
// garbage), output_not_found; and the body, 400 bad_request for anything but
// one object of at most 4 KiB with every required member of its type and no
// member null.
func TestThePUTsChecksBeforeTheWrite(t *testing.T) {
	t.Run("the bearer and the write bucket", func(t *testing.T) {
		c := &fakeDSDChoice{answer: BridgeDSDChoiceResult{Outcome: DSDChoiceWritten}}
		h := newChoiceHTTP(t, quietOutputs("linux", "off"), true, c, 0)
		if a, err := h.put(t, context.Background(), bridgeID, aChoice, true); err != nil || a.status != http.StatusUnauthorized {
			t.Fatalf("no bearer: %d %s (%v), want 401", a.status, a.body, err)
		}
		h.srv.writeRateLimiter = newTokenRateLimiter(60, 1)
		if a := h.putOK(t, bridgeID, aChoice); a.status != http.StatusOK {
			t.Fatalf("the bucket's one: %d %s", a.status, a.body)
		}
		if a := h.putOK(t, bridgeID, aChoice); a.status != http.StatusTooManyRequests || !strings.Contains(string(a.body), `"rate_limited"`) {
			t.Fatalf("past the bucket: %d %s, want 429 rate_limited", a.status, a.body)
		}
		if n := c.calls.Load(); n != 1 {
			t.Fatalf("the write was called %d times, want once: for the PUT the bucket let through", n)
		}
	})

	t.Run("the write's gate, by the outputs route's answer as it stands", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			player  bool
			chooser bool
			mode    string
			hit     bool
			// asked says /v1/health asked MPD's gate before the PUT.
			asked bool
			want  string
		}{
			{"the player's gate shut, MPD on", false, true, "on", false, false, "output_not_found"},
			{"the player's gate shut, MPD off", false, true, "off", false, false, "local_outputs_not_supported"},
			{"the player's gate shut, MPD auto that last found MPD", false, true, "auto", true, true, "output_not_found"},
			{"the player's gate shut, MPD auto that last found none", false, true, "auto", false, true, "local_outputs_not_supported"},
			{"the player's gate shut, MPD auto never asked, MPD there", false, true, "auto", true, false, "local_outputs_not_supported"},
			{"no write wired, the player's gate holding", true, false, "off", false, false, "output_not_found"},
			{"no write wired, the player's gate holding, MPD on", true, false, "on", false, false, "output_not_found"},
			{"no write wired, both gates shut", false, false, "off", false, false, "local_outputs_not_supported"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var n gateCounts
				c := &fakeDSDChoice{answer: BridgeDSDChoiceResult{Outcome: DSDChoiceWritten}}
				var chooser BridgeDSDChoice
				if tc.chooser {
					chooser = c
				}
				h := newChoiceHTTP(t, countedOutputs("linux", tc.mode, tc.hit, &n), tc.player, chooser, 0)
				if tc.asked {
					// MPD's gate answers once, as /v1/health asks it.
					healthFeatures(t, h.hs)
				}
				probes := n.probes.Load()
				a := h.putOK(t, bridgeID, aChoice)
				if a.status != http.StatusNotFound || !strings.Contains(string(a.body), `"`+tc.want+`"`) {
					t.Fatalf("%d %s, want 404 %s", a.status, a.body, tc.want)
				}
				if got := n.probes.Load(); got != probes {
					t.Errorf("the PUT probed MPD %d times: it asks the gate's answer as it stands", got-probes)
				}
				if n.dials.Load() != 0 || c.calls.Load() != 0 {
					t.Errorf("dialled %d, wrote %d: want neither", n.dials.Load(), c.calls.Load())
				}
				if keys := answerKeys(t, a.body); !slices.Equal(keys, []string{"error", "message"}) {
					t.Errorf("the answer's members %q, want the envelope alone", keys)
				}
			})
		}
	})

	t.Run("an id that is not the player's element", func(t *testing.T) {
		c := &fakeDSDChoice{answer: BridgeDSDChoiceResult{Outcome: DSDChoiceWritten}}
		h := newChoiceHTTP(t, quietOutputs("linux", "on"), true, c, 0)
		for _, id := range []string{strings.Repeat("ab", 32), "bridge:0f8fad5b-d9cb-469f-a165-70867728950e", "garbage", "bridge:"} {
			if a := h.putOK(t, id, aChoice); a.status != http.StatusNotFound || !strings.Contains(string(a.body), `"output_not_found"`) {
				t.Errorf("id %q: %d %s, want 404 output_not_found", id, a.status, a.body)
			}
		}
		if n := c.calls.Load(); n != 0 {
			t.Fatalf("the write was called %d times", n)
		}
	})

	t.Run("the body", func(t *testing.T) {
		c := &fakeDSDChoice{answer: BridgeDSDChoiceResult{Outcome: DSDChoiceWritten}}
		h := newChoiceHTTP(t, quietOutputs("linux", "off"), true, c, 0)
		for name, body := range map[string]string{
			"empty":                  ``,
			"malformed":              `{"dacKey":`,
			"not an object":          `[1,2]`,
			"null":                   `null`,
			"trailing data":          aChoice + ` {}`,
			"trailing garbage":       aChoice + `x`,
			"over 4 KiB":             strings.TrimSuffix(aChoice, "}") + `,"padding":"` + strings.Repeat("p", 4<<10) + `"}`,
			"no dacKey":              `{"dacToken":"t1-0123456789abcdef0123456789abcdef","baseRevision":4,"mode":"auto"}`,
			"no dacToken":            `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"auto"}`,
			"no baseRevision":        `{"dacKey":"d1-0123456789abcdef","dacToken":"t1-0123456789abcdef0123456789abcdef","mode":"auto"}`,
			"no mode":                `{"dacKey":"d1-0123456789abcdef","dacToken":"t1-0123456789abcdef0123456789abcdef","baseRevision":4}`,
			"a null required member": `{"dacKey":"d1-0123456789abcdef","dacToken":null,"baseRevision":4,"mode":"auto"}`,
			"baseRevision a string":  `{"dacKey":"d1-0123456789abcdef","dacToken":"t1-0123456789abcdef0123456789abcdef","baseRevision":"4","mode":"auto"}`,
			"baseRevision negative":  `{"dacKey":"d1-0123456789abcdef","dacToken":"t1-0123456789abcdef0123456789abcdef","baseRevision":-1,"mode":"auto"}`,
			"maxBitRate a fraction":  `{"dacKey":"d1-0123456789abcdef","dacToken":"t1-0123456789abcdef0123456789abcdef","baseRevision":4,"mode":"auto","maxBitRate":2.5}`,
			"confirmDoP a string":    `{"dacKey":"d1-0123456789abcdef","dacToken":"t1-0123456789abcdef0123456789abcdef","baseRevision":4,"mode":"dop","confirmDoP":"yes"}`,
			"mode a number":          `{"dacKey":"d1-0123456789abcdef","dacToken":"t1-0123456789abcdef0123456789abcdef","baseRevision":4,"mode":1}`,
			"maxBitRate null":        `{"dacKey":"d1-0123456789abcdef","dacToken":"t1-0123456789abcdef0123456789abcdef","baseRevision":4,"mode":"off","maxBitRate":null}`,
			"confirmDoP null":        `{"dacKey":"d1-0123456789abcdef","dacToken":"t1-0123456789abcdef0123456789abcdef","baseRevision":4,"mode":"dop","confirmDoP":null}`,
			"first use null":         `{"dacKey":"d1-0123456789abcdef","dacToken":"t1-0123456789abcdef0123456789abcdef","baseRevision":4,"mode":"native","firstUseAcknowledged":null}`,
		} {
			a := h.putOK(t, bridgeID, body)
			if a.status != http.StatusBadRequest || !strings.Contains(string(a.body), `"bad_request"`) {
				t.Errorf("%s: %d %s, want 400 bad_request", name, a.status, a.body)
				continue
			}
			if keys := answerKeys(t, a.body); !slices.Equal(keys, []string{"error", "message"}) {
				t.Errorf("%s: the answer's members %q, want the envelope alone", name, keys)
			}
		}
		if n := c.calls.Load(); n != 0 {
			t.Fatalf("the write was called %d times for bodies the route refuses", n)
		}
		// What the route takes, an unknown member ignored and the optional ones
		// at their defaults, reaches the write as sent.
		if a := h.putOK(t, bridgeID, strings.TrimSuffix(aChoice, "}")+`,"confirmdop2":true,"maxBitRate":5644800}`); a.status != http.StatusOK {
			t.Fatalf("a body the route takes: %d %s", a.status, a.body)
		}
		want := BridgeDSDChoiceRequest{DACKey: "d1-0123456789abcdef", DACToken: "t1-0123456789abcdef0123456789abcdef", BaseRevision: 4, Mode: "auto", MaxBitRate: 5644800}
		if c.mu.Lock(); len(c.got) != 1 || c.got[0] != want {
			t.Errorf("the write got %+v, want %+v", c.got, want)
		}
		c.mu.Unlock()
	})
}

// choiceMembers are the members a write's answer carries with its epoch:
// populatedDSD's, which name a DAC.
func choiceMembers(dac bool) BridgeDSDChoiceResult {
	f := populatedDSD()
	r := BridgeDSDChoiceResult{Epoch: true, DSDPlayback: f.DSDPlayback, DSDMode: "auto", AllowDoP: false}
	if dac {
		r.DSDDAC = f.DSDDAC
	}
	return r
}

// TestEveryChoiceOutcomeHasItsAnswer (B728 B9 PR 3): each outcome of serve's
// write is answered with its status, code and own message, and the members
// by the presence rule: dsdPlayback, dsdMode and allowDoP together while the
// answer carries the write's epoch, dsdDAC while that epoch names a DAC, none
// without one, and none on a 400 or an internal answer whatever the write
// carried; dsdMode and allowDoP present at "" and false; each list [] for
// none, as GET builds it. An outcome this build does not know is 500
// internal, and none can arrive: playback's outcomes are the API's, read
// from the source, and the handler's switch names each.
func TestEveryChoiceOutcomeHasItsAnswer(t *testing.T) {
	envelope := []string{"error", "message"}
	all := []string{"allowDoP", "dsdDAC", "dsdMode", "dsdPlayback"}
	three := []string{"allowDoP", "dsdMode", "dsdPlayback"}
	withEnvelope := func(keys []string) []string {
		out := append(slices.Clone(keys), envelope...)
		slices.Sort(out)
		return out
	}
	for _, tc := range []struct {
		outcome string
		status  int
		code    string
		message string
		members bool // members sent with an epoch
	}{
		{DSDChoiceWritten, http.StatusOK, "", "", true},
		{DSDChoiceBadRequest, http.StatusBadRequest, "bad_request", dsdChoiceWordsMessage, false},
		{DSDChoicePlaybackNotStopped, http.StatusConflict, "playback_not_stopped", "Stop playback to change DSD", true},
		{DSDChoiceDACUnavailable, http.StatusConflict, "dac_unavailable", dsdChoiceNoDAC, true},
		{DSDChoiceDACChanged, http.StatusConflict, "dac_changed", dsdChoiceDACChanged, true},
		{DSDChoiceStale, http.StatusConflict, "stale", dsdChoiceStaleMessage, true},
		{DSDChoiceModeNotOffered, http.StatusConflict, "mode_not_offered", dsdChoiceNotOffered, true},
		{DSDChoiceConfirmationRequired, http.StatusConflict, "dop_confirmation_required", dsdChoiceConfirmDoP, true},
		{DSDChoiceFirstUseUnacknowledged, http.StatusConflict, "first_use_unacknowledged", dsdChoiceAcknowledge, true},
		{DSDChoiceStateUnreadable, http.StatusConflict, "state_unreadable", dsdChoiceStateMessage, true},
		{DSDChoicePersistFailed, http.StatusInternalServerError, "persist_failed", dsdChoicePersistFailed, true},
		{DSDChoicePersistUncertain, http.StatusInternalServerError, "persist_uncertain", dsdChoicePersistUnknown, true},
		{DSDChoiceInternal, http.StatusInternalServerError, "internal", dsdChoiceInternalError, false},
		{"an outcome this build does not know", http.StatusInternalServerError, "internal", dsdChoiceInternalError, false},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			c := &fakeDSDChoice{}
			h := newChoiceHTTP(t, quietOutputs("linux", "off"), true, c, 0)
			for _, shape := range []string{"with its epoch, a DAC named", "with its epoch, no DAC", "with no epoch"} {
				res := choiceMembers(shape == "with its epoch, a DAC named")
				if shape == "with no epoch" {
					res = BridgeDSDChoiceResult{}
				}
				res.Outcome = tc.outcome
				if tc.outcome == DSDChoicePersistFailed || tc.outcome == DSDChoicePersistUncertain {
					res.Cause = errors.New("the test's cause")
				}
				c.set(res)
				a := h.putOK(t, bridgeID, aChoice)
				if a.status != tc.status {
					t.Fatalf("%s: status %d, want %d: %s", shape, a.status, tc.status, a.body)
				}
				var body map[string]json.RawMessage
				if err := json.Unmarshal(a.body, &body); err != nil {
					t.Fatalf("%s: %s: %v", shape, a.body, err)
				}
				var want []string
				switch {
				case !tc.members || shape == "with no epoch":
				case shape == "with its epoch, no DAC":
					want = three
				default:
					want = all
				}
				if tc.status != http.StatusOK {
					want = withEnvelope(want)
					if string(body["error"]) != `"`+tc.code+`"` || string(body["message"]) != `"`+tc.message+`"` {
						t.Errorf("%s: error %s, message %s; want %s, %q", shape, body["error"], body["message"], tc.code, tc.message)
					}
				}
				if want == nil {
					want = []string{}
				}
				if got := keysOf(body); !slices.Equal(got, want) && !(len(got) == 0 && len(want) == 0) {
					t.Errorf("%s: members %q, want %q", shape, got, want)
				}
				if p, ok := body["dsdPlayback"]; ok {
					var pb map[string]json.RawMessage
					if err := json.Unmarshal(p, &pb); err != nil {
						t.Fatal(err)
					}
					if string(pb["policyGeneration"]) != "18446744073709551615" || string(pb["publication"]) != "7" {
						t.Errorf("%s: dsdPlayback %s, want the epoch the write carried", shape, p)
					}
					if string(body["dsdMode"]) != `"auto"` || string(body["allowDoP"]) != "false" {
						t.Errorf("%s: dsdMode %s, allowDoP %s; want auto, false, present", shape, body["dsdMode"], body["allowDoP"])
					}
				}
				if d, ok := body["dsdDAC"]; ok && !bytes.Contains(d, []byte(`"dacToken":"t1-0123456789abcdef0123456789abcdef"`)) {
					t.Errorf("%s: dsdDAC %s carries no token", shape, d)
				}
			}
		})
	}

	t.Run("the lists are [] for none, dsdMode and allowDoP present at their zero", func(t *testing.T) {
		c := &fakeDSDChoice{answer: BridgeDSDChoiceResult{Outcome: DSDChoiceStale, Epoch: true,
			DSDPlayback: &OutputDSDPlayback{BootID: "b"}, DSDDAC: &OutputDSDDAC{DACKey: "d1-0123456789abcdef", DACToken: "t1-0123456789abcdef0123456789abcdef", Profile: &OutputDSDProfile{}}}}
		h := newChoiceHTTP(t, quietOutputs("linux", "off"), true, c, 0)
		a := h.putOK(t, bridgeID, aChoice)
		for _, part := range []string{`"tuples":[]`, `"converted":[]`, `"refused":[]`, `"warnings":[]`, `"options":[]`, `"firstUse":[]`, `"caveats":[]`, `"dsdMode":""`, `"allowDoP":false`} {
			if !bytes.Contains(a.body, []byte(part)) {
				t.Errorf("the answer %s lacks %s", a.body, part)
			}
		}
		if bytes.Contains(a.body, []byte("null")) {
			t.Errorf("the answer %s carries a null", a.body)
		}
	})

	t.Run("playback's outcomes are the API's, and the handler names each", func(t *testing.T) {
		r := tokenResolver{"playback": parseGoPackage(t, filepath.Join("..", "player", "playback")), "api": parseGoPackage(t, ".")}
		backend := r.typed(t, "playback", "ChoiceOutcome")
		ours := r.named(t, "api", "DSDChoice")
		if !ours[DSDChoiceInternal] {
			t.Fatalf("the API's outcomes %v lack internal", ours)
		}
		delete(ours, DSDChoiceInternal)
		if !mapsEqual(backend, ours) {
			t.Fatalf("playback's outcomes %v are not the API's %v", backend, ours)
		}
		named := map[string]bool{}
		ast.Inspect(r["api"].files["outputs_dsd_choice.go"], func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "putOutputDSD" {
				return true
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if cc, ok := n.(*ast.CaseClause); ok {
					for _, e := range cc.List {
						named[r.value(t, "api", e)] = true
					}
				}
				return true
			})
			return false
		})
		ours[DSDChoiceInternal] = true
		if !mapsEqual(named, ours) {
			t.Fatalf("the handler's switch names %v, want every outcome %v: an outcome it does not name is answered internal", named, ours)
		}
	})
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// TestAServerDeadlineAndAClientGoneAreAnsweredApart (B728 B9 PR 3): through
// the real handler, its budget short and a write that waits for its context:
// the route's deadline is the server's, 500 persist_failed with the members
// the write carried, logged at Error; a request whose own context the client
// ended is recorded 499 at Info, the plain envelope; a cancellation whose
// request is still live is the server's too, 500 at Error. The handler
// answers each, never an empty 200.
func TestAServerDeadlineAndAClientGoneAreAnsweredApart(t *testing.T) {
	waitForContext := func(entered chan<- struct{}) func(ctx context.Context) BridgeDSDChoiceResult {
		return func(ctx context.Context) BridgeDSDChoiceResult {
			if entered != nil {
				close(entered)
			}
			<-ctx.Done()
			res := choiceMembers(true)
			res.Outcome, res.Cause = DSDChoicePersistFailed, ctx.Err()
			return res
		}
	}

	t.Run("the route's deadline", func(t *testing.T) {
		rec := loggingtest.Record(t)
		c := &fakeDSDChoice{wait: waitForContext(nil)}
		h := newChoiceHTTP(t, quietOutputs("linux", "off"), true, c, 50*time.Millisecond)
		a := h.putOK(t, bridgeID, aChoice)
		if a.status != http.StatusInternalServerError || !strings.Contains(string(a.body), `"persist_failed"`) {
			t.Fatalf("%d %s, want 500 persist_failed", a.status, a.body)
		}
		if keys := answerKeys(t, a.body); !slices.Equal(keys, []string{"allowDoP", "dsdDAC", "dsdMode", "dsdPlayback", "error", "message"}) {
			t.Errorf("the members %q, want the write's epoch beside the envelope", keys)
		}
		if lines := rec.Lines("request failed"); len(lines) != 1 || !strings.HasPrefix(lines[0], "ERROR") || !strings.Contains(lines[0], "status=500") ||
			!strings.Contains(lines[0], "deadline exceeded") {
			t.Errorf("the log %q, want one Error line with the deadline as its cause", lines)
		}
	})

	t.Run("a client gone", func(t *testing.T) {
		rec := loggingtest.Record(t)
		entered := make(chan struct{})
		c := &fakeDSDChoice{wait: waitForContext(entered)}
		h := newChoiceHTTP(t, quietOutputs("linux", "off"), true, c, time.Minute)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-entered
			cancel()
		}()
		a, err := h.put(t, ctx, bridgeID, aChoice, false)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the client's PUT answered %v, want its own cancellation", err)
		}
		if a.status != statusClientClosedRequest || !strings.Contains(string(a.body), `"persist_failed"`) {
			t.Fatalf("%d %s, want 499 recorded with persist_failed", a.status, a.body)
		}
		if keys := answerKeys(t, a.body); !slices.Equal(keys, []string{"error", "message"}) {
			t.Errorf("the members %q, want the plain envelope", keys)
		}
		if lines := rec.Lines("request failed"); len(lines) != 1 || !strings.HasPrefix(lines[0], "INFO") || !strings.Contains(lines[0], "status=499") {
			t.Errorf("the log %q, want one Info line at 499", lines)
		}
	})

	t.Run("a cancellation while the request is live", func(t *testing.T) {
		rec := loggingtest.Record(t)
		res := choiceMembers(true)
		res.Outcome, res.Cause = DSDChoicePersistFailed, context.Canceled
		c := &fakeDSDChoice{answer: res}
		h := newChoiceHTTP(t, quietOutputs("linux", "off"), true, c, 0)
		a := h.putOK(t, bridgeID, aChoice)
		if a.status != http.StatusInternalServerError || !bytes.Contains(a.body, []byte(`"dsdPlayback"`)) {
			t.Fatalf("%d %s, want 500 persist_failed with the members", a.status, a.body)
		}
		if lines := rec.Lines("request failed"); len(lines) != 1 || !strings.HasPrefix(lines[0], "ERROR") || !strings.Contains(lines[0], "status=500") {
			t.Errorf("the log %q, want one Error line at 500", lines)
		}
	})
}
