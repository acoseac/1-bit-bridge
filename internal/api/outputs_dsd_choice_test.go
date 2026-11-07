package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// PUT /v1/outputs/{id}/dsd on the API's side (B728 B9; the design's section
// 7.2): the route and its flag, its gates and its id, its body, and the
// answer each outcome is.

// fakeChooser is serve's player answering the choice's route with result,
// each request it was asked recorded.
type fakeChooser struct {
	*fakeBridgeOutput
	mu     sync.Mutex
	result DSDChoiceResult
	asked  []DSDChoiceRequest
}

func newFakeChooser(enabled bool) *fakeChooser {
	return &fakeChooser{fakeBridgeOutput: &fakeBridgeOutput{enabled: enabled, facts: listedPlayer()},
		result: DSDChoiceResult{Outcome: "written", DSDDAC: populatedDSD().DSDDAC, DSDPlayback: populatedDSD().DSDPlayback}}
}

func (f *fakeChooser) OwnsOutput(id string) bool { return id == bridgeOutputIDPrefix+testPlayerUUID }

func (f *fakeChooser) ChooseDSD(_ context.Context, req DSDChoiceRequest) DSDChoiceResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, req)
	return f.result
}

func (f *fakeChooser) requests() []DSDChoiceRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// choicePath is the player's choice route.
const choicePath = "/v1/outputs/bridge:" + testPlayerUUID + "/dsd"

// choiceBody is a well-formed body.
const choiceBody = `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"native","maxBitRate":5644800,"confirmDoP":true,"firstUseAcknowledged":true,"interrupt":true}`

// put sends body to path with the token, answering the status and the body.
func put(t *testing.T, hs *httptest.Server, token, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, hs.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

// errorCode is an error body's code, "" for a body that is no error.
func errorCode(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	return e.Error
}

// newChoiceHTTP is a bridge whose MPD gate resolves mpd and whose player is c,
// public as asked.
func newChoiceHTTP(t *testing.T, mpd string, public bool, c BridgeOutput) (*httptest.Server, string, *Server) {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := store.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600, LibraryName: "T", LibraryRoots: []string{t.TempDir()}}
	if public {
		cfg.Deployment = config.DeploymentConfig{Mode: "public", AdminTLSTerminatedByProxy: true}
	}
	lo := quietOutputs("linux", mpd)
	lo.Getenv = func(string) string { return "" }
	srv := New(cfg, store, nil, "fp").WithLocalOutputs(lo)
	if c != nil {
		srv.WithBridgeOutput(c)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, srv
}

// TestTheChoiceFlagArrivesWithItsRoute (B728 B9; the design's section 7.1;
// binding note 9): bridgeDSDChoice is advertised exactly while the route is
// mounted and the player's gate holds; a public bridge never mounts it, and
// a client asking it there is answered as for any path no route serves;
// any other bridge mounts it, and it answers the gates' 404s while they are
// shut. The check's flag and routes are B12's, and none of them is here.
func TestTheChoiceFlagArrivesWithItsRoute(t *testing.T) {
	for _, public := range []bool{false, true} {
		for _, player := range []bool{true, false} {
			for _, mpd := range []string{"on", "off"} {
				c := newFakeChooser(player)
				hs, token, srv := newChoiceHTTP(t, mpd, public, c)
				feats := healthFeatures(t, hs)
				if got, want := slices.Contains(feats, "bridgeDSDChoice"), player && !public; got != want {
					t.Errorf("public %v, player %v, MPD %s: bridgeDSDChoice advertised %v, want %v: %v", public, player, mpd, got, want, feats)
				}
				if slices.Contains(feats, "bridgeDSDCheck") {
					t.Errorf("public %v, player %v, MPD %s: bridgeDSDCheck advertised before its routes", public, player, mpd)
				}
				mounted := false
				for _, rt := range srv.routeRegistry() {
					switch {
					case rt.pattern == "PUT /v1/outputs/{id}/dsd":
						mounted = true
					case strings.Contains(rt.pattern, "/dsd"):
						t.Errorf("a route of the check is registered before its PR: %s", rt.pattern)
					}
				}
				if mounted == public {
					t.Errorf("public %v: the choice's route mounted %v", public, mounted)
				}
				status, raw := put(t, hs, token, choicePath, choiceBody)
				switch {
				case public:
					if status != http.StatusNotFound || errorCode(raw) != "" {
						t.Errorf("public %v, player %v, MPD %s: %d %s, want the plain 404 of a path no route serves", public, player, mpd, status, raw)
					}
				case !player && mpd == "off":
					if status != http.StatusNotFound || errorCode(raw) != "local_outputs_not_supported" {
						t.Errorf("player %v, MPD %s: %d %s, want 404 local_outputs_not_supported", player, mpd, status, raw)
					}
				case !player:
					if status != http.StatusNotFound || errorCode(raw) != "output_not_found" {
						t.Errorf("player %v, MPD %s: %d %s, want 404 output_not_found", player, mpd, status, raw)
					}
				default:
					if status != http.StatusOK {
						t.Errorf("player %v, MPD %s: %d %s, want the write answered", player, mpd, status, raw)
					}
				}
				if !player && len(c.requests()) != 0 {
					t.Errorf("player %v, MPD %s: the player was asked to write while its gate is shut", player, mpd)
				}
			}
		}
	}
}

// TestTheChoiceRouteAnswersOnlyThePlayersID (binding note 9): an id that is
// not the player's element's now, an MPD element's or a stale player's, is
// 404 output_not_found before the body is read, and nothing is asked.
func TestTheChoiceRouteAnswersOnlyThePlayersID(t *testing.T) {
	c := newFakeChooser(true)
	hs, token, _ := newChoiceHTTP(t, "on", false, c)
	for _, id := range []string{"bridge:00000000-0000-0000-0000-000000000000", "0123abcd", "bridge:"} {
		status, raw := put(t, hs, token, "/v1/outputs/"+id+"/dsd", "{not json")
		if status != http.StatusNotFound || errorCode(raw) != "output_not_found" {
			t.Errorf("%s: %d %s, want 404 output_not_found", id, status, raw)
		}
	}
	if len(c.requests()) != 0 {
		t.Errorf("an id not the player's asked the player %+v", c.requests())
	}
}

// TestTheChoiceRouteIsAnAuthenticatedBoundedWrite (binding note 9): the
// route is a bearer-token write in the registry's pattern: bounded, drawing
// from the write bucket, 401 without a token.
func TestTheChoiceRouteIsAnAuthenticatedBoundedWrite(t *testing.T) {
	c := newFakeChooser(true)
	hs, _, srv := newChoiceHTTP(t, "off", false, c)
	found := false
	for _, rt := range srv.routeRegistry() {
		if rt.pattern == "PUT /v1/outputs/{id}/dsd" {
			found = true
			if rt.kind != boundedRoute || rt.rateClass != rateWrite {
				t.Errorf("the route is kind %v, rate class %v; want bounded and rateWrite", rt.kind, rt.rateClass)
			}
		}
	}
	if !found {
		t.Fatal("the choice's route is not registered")
	}
	if status, _ := put(t, hs, "", choicePath, choiceBody); status != http.StatusUnauthorized {
		t.Errorf("without a token: %d, want 401", status)
	}
}

// TestTheChoiceBodyIsBoundedAndStrict (the design's section 7.2, check 1;
// binding note 9; B728 B9 round 2, points 6 and 7): a body past its bound is
// refused before it is parsed, a well-formed choice after white space past the
// bound among them, and one that is not exactly the body (a malformed one, an
// unknown member, a member missing, a mode or a cap the bridge does not know,
// an explicit null for any member, the optional ones included) is 400
// bad_request; neither asks the player anything. A well-formed one reaches it
// whole.
func TestTheChoiceBodyIsBoundedAndStrict(t *testing.T) {
	c := newFakeChooser(true)
	hs, token, _ := newChoiceHTTP(t, "off", false, c)
	padded := `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"native","pad":"` + strings.Repeat("x", 8<<10) + `"}`
	for name, body := range map[string]string{
		"past its bound":                 padded,
		"a choice after its bound":       strings.Repeat(" ", maxDSDChoiceBody) + `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"off"}`,
		"a null dacKey":                  `{"dacKey":null,"baseRevision":4,"mode":"off"}`,
		"a null baseRevision":            `{"dacKey":"d1-0123456789abcdef","baseRevision":null,"mode":"off"}`,
		"a null mode":                    `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":null}`,
		"a null maxBitRate":              `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"off","maxBitRate":null}`,
		"a null confirmDoP":              `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"off","confirmDoP":null}`,
		"a null firstUseAcknowledged":    `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"off","firstUseAcknowledged":null}`,
		"a null interrupt":               `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"off","interrupt":null}`,
		"a cap that is a string":         `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"auto","maxBitRate":"5644800"}`,
		"a confirmation that is a word":  `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"dop","confirmDoP":"true"}`,
		"an acknowledgement that is one": `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"native","firstUseAcknowledged":1}`,
		"not JSON":                       `{"dacKey":`,
		"data after the body":            `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"off"}{}`,
		"an unknown member":              `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"off","dsd512":true}`,
		"no dacKey":                      `{"baseRevision":4,"mode":"off"}`,
		"an empty dacKey":                `{"dacKey":"","baseRevision":4,"mode":"off"}`,
		"no baseRevision":                `{"dacKey":"d1-0123456789abcdef","mode":"off"}`,
		"a negative revision":            `{"dacKey":"d1-0123456789abcdef","baseRevision":-1,"mode":"off"}`,
		"a fractional revision":          `{"dacKey":"d1-0123456789abcdef","baseRevision":1.5,"mode":"off"}`,
		"no mode":                        `{"dacKey":"d1-0123456789abcdef","baseRevision":4}`,
		"a mode it does not know":        `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"loud"}`,
		"a cap past DSD256":              `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"auto","maxBitRate":22579200}`,
		"a cap that is no rate":          `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"auto","maxBitRate":1}`,
		"a flag that is no bool":         `{"dacKey":"d1-0123456789abcdef","baseRevision":4,"mode":"auto","interrupt":"yes"}`,
	} {
		status, raw := put(t, hs, token, choicePath, body)
		if status != http.StatusBadRequest || errorCode(raw) != "bad_request" {
			t.Errorf("%s: %d %s, want 400 bad_request", name, status, raw)
		}
	}
	if got := c.requests(); len(got) != 0 {
		t.Fatalf("a refused body asked the player %+v", got)
	}
	if status, raw := put(t, hs, token, choicePath, choiceBody); status != http.StatusOK {
		t.Fatalf("a well-formed body: %d %s", status, raw)
	}
	want := DSDChoiceRequest{DACKey: "d1-0123456789abcdef", BaseRevision: 4, Mode: "native", MaxBitRate: 5644800, ConfirmDoP: true, FirstUseAcknowledged: true, Interrupt: true}
	if got := c.requests(); len(got) != 1 || got[0] != want {
		t.Errorf("the player was asked %+v, want %+v", got, want)
	}
	if status, _ := put(t, hs, token, choicePath, `{"dacKey":"d1-0123456789abcdef","baseRevision":0,"mode":"off"}`); status != http.StatusOK {
		t.Errorf("a body with the optional members left out: %d", status)
	}
	if got := c.requests(); len(got) != 2 || got[1] != (DSDChoiceRequest{DACKey: "d1-0123456789abcdef", Mode: "off"}) {
		t.Errorf("the second request: %+v", got)
	}
}

// TestEachChoiceOutcomeIsItsStatusAndWord (the design's section 7.2; binding
// notes 1 and 5): the player's outcome is the answer: written is 200 with the
// published dsdDAC and dsdPlayback, their lists [] where empty; stale is 409
// carrying the current dsdDAC; every other refusal its status and its word.
func TestEachChoiceOutcomeIsItsStatusAndWord(t *testing.T) {
	for _, tc := range []struct {
		outcome string
		status  int
		code    string
	}{
		{"written", http.StatusOK, ""},
		{"bad_request", http.StatusBadRequest, "bad_request"},
		{"dac_unavailable", http.StatusConflict, "dac_unavailable"},
		{"dac_changed", http.StatusConflict, "dac_changed"},
		{"stale", http.StatusConflict, "stale"},
		{"mode_not_offered", http.StatusConflict, "mode_not_offered"},
		{"dop_confirmation_required", http.StatusConflict, "dop_confirmation_required"},
		{"first_use_unacknowledged", http.StatusConflict, "first_use_unacknowledged"},
		{"would_interrupt", http.StatusConflict, "would_interrupt"},
		{"state_unreadable", http.StatusConflict, "state_unreadable"},
		{"persist_failed", http.StatusInternalServerError, "persist_failed"},
		{"persist_uncertain", http.StatusInternalServerError, "persist_uncertain"},
		{"a word nobody wrote", http.StatusInternalServerError, "internal"},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			c := newFakeChooser(true)
			c.result.Outcome = tc.outcome
			// A DAC with no profile and no first use: lists the answer fills.
			c.result.DSDDAC = &OutputDSDDAC{DACKey: "d1-0123456789abcdef", Choice: OutputDSDChoice{Mode: "auto", Revision: 9}}
			c.result.DSDPlayback = &OutputDSDPlayback{BootID: "boot", PolicyGeneration: 3}
			hs, token, _ := newChoiceHTTP(t, "off", false, c)
			status, raw := put(t, hs, token, choicePath, choiceBody)
			if status != tc.status || errorCode(raw) != tc.code {
				t.Fatalf("%s: %d %s, want %d %q", tc.outcome, status, raw, tc.status, tc.code)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("%s", raw)
			}
			switch tc.outcome {
			case "written":
				dac, playback := rawKeysOf(t, body["dsdDAC"]), rawKeysOf(t, body["dsdPlayback"])
				if string(dac["options"]) != "[]" || string(dac["firstUse"]) != "[]" || string(playback["tuples"]) != "[]" || string(playback["refused"]) != "[]" {
					t.Errorf("the answer's lists %s: want each [] where empty, as GET sends them", raw)
				}
			case "stale":
				if dac := rawKeysOf(t, body["dsdDAC"]); string(dac["dacKey"]) != `"d1-0123456789abcdef"` {
					t.Errorf("a stale answer %s, want the current dsdDAC", raw)
				}
			default:
				if _, present := body["dsdDAC"]; present {
					t.Errorf("a %s answer carries dsdDAC: %s", tc.outcome, raw)
				}
			}
		})
	}
}

// rawKeysOf is a JSON object's members.
func rawKeysOf(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return m
}
