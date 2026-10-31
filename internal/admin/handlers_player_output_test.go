package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// fakePlayerOutput is the Player page's provider as a test drives it: a
// report and a DAC list it answers as given, a choice it answers with a
// binding or a refusal, and every call counted, so a refusal can be shown to
// have read nothing of the box.
type fakePlayerOutput struct {
	mu        sync.Mutex
	platform  string
	report    PlayerReport
	list      PlayerDACList
	device    config.PlayerDevice
	chosenErr error
	// sounding is what Sounding answers, nil for false; withholds is what
	// Withholds answers.
	sounding  func() bool
	withholds bool
	calls     map[string]int
	selection string
	kind      string
}

func newFakePlayerOutput() *fakePlayerOutput {
	return &fakePlayerOutput{platform: "linux", calls: map[string]int{},
		report: PlayerReport{
			Running:    PlayerRunning{Started: true, State: "playing", Transport: "playing"},
			Configured: PlayerConfigured{Enabled: true, Starts: true},
			Pending:    []PlayerPending{},
			Transports: []PlayerTransport{{Kind: "pcm", Plays: true, Capable: "yes", Approved: []string{}}},
		},
		list: PlayerDACList{Read: true, Cards: []PlayerDACCard{{Card: 5, Name: "iFi (by AMR) HD USB Audio", Selection: "tok", Kinds: []PlayerDACKind{}}}},
		device: config.PlayerDevice{Transport: "usb", Binding: config.BindingSerial, VendorID: "20b1", ProductID: "3009",
			Serial: usbid.New("GOBAR-7Q2X-91KZ-4471")},
	}
}

func (f *fakePlayerOutput) count(what string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[what]++
}

// called is how many times what was called.
func (f *fakePlayerOutput) called(what string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[what]
}

func (f *fakePlayerOutput) Platform() string { return f.platform }

func (f *fakePlayerOutput) Report(context.Context, *config.Config) PlayerReport {
	f.count("report")
	return f.report
}

func (f *fakePlayerOutput) DACs(context.Context, *config.Config) PlayerDACList {
	f.count("dacs")
	return f.list
}

func (f *fakePlayerOutput) Chosen(_ context.Context, selection, kind string) (config.PlayerDevice, error) {
	f.count("chosen")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selection, f.kind = selection, kind
	return f.device, f.chosenErr
}

// answerChosen makes every later choice answer err, as the box does once the
// DAC a token names is swapped.
func (f *fakePlayerOutput) answerChosen(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chosenErr = err
}

func (f *fakePlayerOutput) SameBinding(a, b config.PlayerDevice) bool {
	return a.Binding == b.Binding && a.VendorID == b.VendorID && a.ProductID == b.ProductID && a.Port == b.Port &&
		(a.Serial.Equal(b.Serial) || !a.Serial.Present() && !b.Serial.Present())
}

func (f *fakePlayerOutput) Sounding() bool { return f.sounding != nil && f.sounding() }

func (f *fakePlayerOutput) Withholds(*config.Config, *config.Config) bool { return f.withholds }

// newPlayerTestServer is newTestServer with the Player page's provider, its
// holder told the config file as serve's boot read leaves it.
func newPlayerTestServer(t *testing.T) (*Server, *fakePlayerOutput, string) {
	t.Helper()
	srv, _, cfgPath := newTestServer(t)
	recordBootRead(t, srv, cfgPath)
	fake := newFakePlayerOutput()
	srv.deps.PlayerOutput = fake
	return srv, fake, cfgPath
}

// playerRequest sends one request to the console as its page's fetch does:
// from loopback, a JSON body when body is not empty, and change's last word on
// the request. It answers the status and the body.
func playerRequest(t *testing.T, h http.Handler, method, path, body string, change func(*http.Request)) (int, string) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.RemoteAddr = "127.0.0.1:54321"
	r.Host = testConsoleHost
	if change != nil {
		change(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// errorCode is the error envelope's code in body, "" for none.
func errorCode(body string) string {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &e)
	return e.Error
}

// unchangedConfig requires the config file to hold want and the live holder
// to hold the player block it held: a refused write wrote nothing anywhere.
func unchangedConfig(t *testing.T, srv *Server, cfgPath string, want []byte, player config.PlayerConfig, what string) {
	t.Helper()
	if got := readFileOrNil(cfgPath); !bytes.Equal(got, want) {
		t.Errorf("%s: the config file changed:\n%s", what, got)
	}
	live := srv.deps.CfgHolder.Load().Output.Player
	sameSerial := live.Device.Serial.Equal(player.Device.Serial) || !live.Device.Serial.Present() && !player.Device.Serial.Present()
	if live.Enabled != player.Enabled || live.Device.Binding != player.Device.Binding || !sameSerial {
		t.Errorf("%s: the live config's player changed: %+v", what, live)
	}
}

// writePlayerConfig rewrites the config file with text appended and loads it
// into the console's holder as serve's boot read would.
func writePlayerConfig(t *testing.T, srv *Server, cfgPath, text string) {
	t.Helper()
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append(raw, []byte(text)...), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, raw, file, err := config.LoadFileAt(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	srv.deps.CfgHolder.Store(cfg)
	srv.deps.CfgHolder.RecordFile(file, raw)
}

// TestThePlayerStatusCarriesEveryKey pins GET /api/output/player's shape: the
// page's own words beside the provider's report, every key present and no list
// null, and, where the player cannot run, the word and the sentence with the
// box left unread.
func TestThePlayerStatusCarriesEveryKey(t *testing.T) {
	srv, fake, cfgPath := newPlayerTestServer(t)
	h := srv.Handler()
	fake.report.Pending, fake.report.Transports = nil, nil
	code, body := playerRequest(t, h, http.MethodGet, "/api/output/player", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/output/player = %d: %s", code, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"available", "unavailable", "why", "managed", "running", "configured", "pending", "transports"} {
		if _, ok := got[key]; !ok {
			t.Errorf("the status has no %q: %s", key, body)
		}
	}
	for _, key := range []string{"pending", "transports"} {
		if _, ok := got[key].([]any); !ok {
			t.Errorf("%q is %v, want a list, never null", key, got[key])
		}
	}
	if got["available"] != true || got["unavailable"] != "" {
		t.Errorf("a Linux loopback bridge: available %v, unavailable %v", got["available"], got["unavailable"])
	}
	running, _ := got["running"].(map[string]any)
	for _, key := range []string{"started", "state", "reason", "detail", "transport", "lastError", "withheld", "openHome", "binding", "dac", "track", "signal"} {
		if _, ok := running[key]; !ok {
			t.Errorf("running has no %q: %s", key, body)
		}
	}

	// Where the player cannot run, the status says why and reads nothing.
	for _, tc := range []struct {
		name, want string
		arrange    func()
		readsBox   bool
	}{
		{"not Linux", playerNotSupported, func() { fake.platform = "darwin" }, false},
		{"an unreadable block", playerBlockUnread, func() {
			fake.platform = "linux"
			writePlayerConfig(t, srv, cfgPath, "output:\n  player:\n    enabled: true\n    resample: yes\n")
		}, true},
		{"no player wired", playerNotWired, func() { srv.deps.PlayerOutput = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.arrange()
			before := fake.called("report")
			code, body := playerRequest(t, h, http.MethodGet, "/api/output/player", "", nil)
			var st PlayerStatus
			if err := json.Unmarshal([]byte(body), &st); err != nil || code != http.StatusOK {
				t.Fatalf("GET = %d, %v: %s", code, err, body)
			}
			if st.Available || st.Unavailable != tc.want || st.Why == "" {
				t.Errorf("available %v, unavailable %q, why %q; want %s with its sentence", st.Available, st.Unavailable, st.Why, tc.want)
			}
			if read := fake.called("report") > before; read != tc.readsBox {
				t.Errorf("the provider's report was asked: %v, want %v", read, tc.readsBox)
			}
		})
	}
}

// TestThePlayerSwitchWritesTheConfigThroughTheHolder pins the switch: on and
// off reach the file on disk and the live config, each answered restart, off
// as interrupting a player that sounded until its save; a value the config
// holds already writes nothing and is answered unchanged.
func TestThePlayerSwitchWritesTheConfigThroughTheHolder(t *testing.T) {
	srv, fake, cfgPath := newPlayerTestServer(t)
	h := srv.Handler()
	switchTo := func(on bool) playerChangeResponse {
		t.Helper()
		code, body := playerRequest(t, h, http.MethodPost, "/api/output/player/enabled", fmt.Sprintf(`{"enabled":%v}`, on), nil)
		if code != http.StatusOK {
			t.Fatalf("POST enabled %v = %d: %s", on, code, body)
		}
		var resp playerChangeResponse
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	onDisk := func() config.PlayerConfig {
		t.Helper()
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Output.Player
	}
	if resp := switchTo(true); resp.Status != "restart" {
		t.Errorf("turning the player on answered %q, want restart", resp.Status)
	}
	if !onDisk().Enabled || !srv.deps.CfgHolder.Load().Output.Player.Enabled {
		t.Fatalf("turning the player on: the file holds %v and the live config %v, want both on", onDisk().Enabled, srv.deps.CfgHolder.Load().Output.Player.Enabled)
	}
	saved := readFileOrNil(cfgPath)
	if resp := switchTo(true); resp.Status != "unchanged" {
		t.Errorf("turning on a player that is on answered %q, want unchanged", resp.Status)
	}
	if got := readFileOrNil(cfgPath); !bytes.Equal(got, saved) {
		t.Errorf("an unchanged switch rewrote the config file")
	}
	// The fake player sounds while the live config has it on, so it stops the
	// moment the save is stored, as serve's player withholds its output at
	// once: the answer still says the change interrupted it, from what the
	// write's turn found just before its save.
	fake.withholds = true
	fake.sounding = func() bool { return srv.deps.CfgHolder.Load().Output.Player.Enabled }
	resp := switchTo(false)
	if resp.Status != "restart" || !resp.Interrupted {
		t.Errorf("turning off a player that plays answered %q, interrupted %v; want restart, interrupted", resp.Status, resp.Interrupted)
	}
	if onDisk().Enabled || srv.deps.CfgHolder.Load().Output.Player.Enabled {
		t.Fatalf("turning the player off: the file or the live config still has it on")
	}
	if !resp.Player.Available || resp.Player.Running.State != "playing" {
		t.Errorf("the answer carries no status after the change: %+v", resp.Player)
	}
}

// TestThePlayerSwitchRefusesAConfigThePlayerCannotRunOn: turning the player on
// over a config it cannot run on (here an OpenHome port the console binds) is
// refused with the config's own reason, which quotes no port, and nothing is
// written.
func TestThePlayerSwitchRefusesAConfigThePlayerCannotRunOn(t *testing.T) {
	srv, _, cfgPath := newPlayerTestServer(t)
	writePlayerConfig(t, srv, cfgPath, "output:\n  player:\n    openhome: true\n    openhomePort: 7789\n")
	want := readFileOrNil(cfgPath)
	code, body := playerRequest(t, srv.Handler(), http.MethodPost, "/api/output/player/enabled", `{"enabled":true}`, nil)
	if code != http.StatusConflict || errorCode(body) != "player_refused" {
		t.Fatalf("turning on a player whose OpenHome port the console binds = %d: %s; want 409 player_refused", code, body)
	}
	if !strings.Contains(body, "adminAddress") || strings.Contains(body, "7789") {
		t.Errorf("the refusal should name the setting and never the port: %s", body)
	}
	unchangedConfig(t, srv, cfgPath, want, config.PlayerConfig{}, "the refused switch")
}

// playerWriteRoutes are the page's two writes, each with a body that would
// change the config, the managed setting that owns it, and the provider calls
// it makes on its way.
var playerWriteRoutes = []struct {
	path, body, managed string
}{
	{"/api/output/player/enabled", `{"enabled":true}`, managedPlayerEnabled},
	{"/api/output/player/dac", `{"selection":"tok","binding":"serial"}`, managedPlayerDevice},
}

// TestThePlayerRoutesRefuseWhereThePlayerCannotRun pins the server-side
// eligibility of every route of the page: not on a public bridge (a signed-in
// session included), not where deployment.managedSettings gives the setting to
// the control plane, not off Linux, not over a player block that does not read.
// Each refusal writes nothing, to the file or the live config, and reads
// nothing of the box: no DAC listing, no choice.
func TestThePlayerRoutesRefuseWhereThePlayerCannotRun(t *testing.T) {
	for _, route := range append(playerWriteRoutes, struct{ path, body, managed string }{"/api/output/player/dacs", "", ""}) {
		method := http.MethodPost
		if route.body == "" {
			method = http.MethodGet
		}
		t.Run(route.path+"/public with a session", func(t *testing.T) {
			srv, store, _, _ := newPublicTestServerAt(t, "test-password-123")
			fake := newFakePlayerOutput()
			srv.deps.PlayerOutput = fake
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			want, _ := os.ReadFile(srv.deps.CfgPath)
			req, err := http.NewRequest(method, ts.URL+route.path, strings.NewReader(route.body))
			if err != nil {
				t.Fatal(err)
			}
			if route.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("Origin", publicOrigin)
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: signIn(t, store)})
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var e struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&e)
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden || e.Error != playerNotAvailable {
				t.Fatalf("a signed-in public console = %d %q, want 403 %s", resp.StatusCode, e.Error, playerNotAvailable)
			}
			unchangedConfig(t, srv, srv.deps.CfgPath, want, config.PlayerConfig{}, "public")
			if fake.called("dacs")+fake.called("chosen") != 0 {
				t.Errorf("the refusal read the box: %v", fake.calls)
			}
		})
		for _, tc := range []struct {
			name, code string
			arrange    func(srv *Server, fake *fakePlayerOutput, cfgPath string)
		}{
			{"not Linux", playerNotSupported, func(_ *Server, fake *fakePlayerOutput, _ string) { fake.platform = "darwin" }},
			{"an unreadable block", playerBlockUnread, func(srv *Server, _ *fakePlayerOutput, cfgPath string) {
				writePlayerConfig(t, srv, cfgPath, "output:\n  player:\n    enabled: true\n    device:\n      card: 5\n")
			}},
			{"a managed setting", playerSettingManaged, func(srv *Server, _ *fakePlayerOutput, cfgPath string) {
				if route.managed == "" {
					writePlayerConfig(t, srv, cfgPath, "deployment:\n  managedControls: [roots]\n")
					return
				}
				writePlayerConfig(t, srv, cfgPath, "deployment:\n  managedSettings: ["+route.managed+"]\n")
			}},
		} {
			t.Run(route.path+"/"+tc.name, func(t *testing.T) {
				srv, fake, cfgPath := newPlayerTestServer(t)
				tc.arrange(srv, fake, cfgPath)
				want := readFileOrNil(cfgPath)
				player := srv.deps.CfgHolder.Load().Output.Player
				code, body := playerRequest(t, srv.Handler(), method, route.path, route.body, nil)
				if route.managed == "" && tc.code == playerSettingManaged {
					// A read is no managed setting's: the listing answers.
					if code != http.StatusOK {
						t.Fatalf("the DAC list on a bridge managing another control = %d: %s", code, body)
					}
					return
				}
				if errorCode(body) != tc.code || code < 400 {
					t.Fatalf("%s = %d: %s; want %s", route.path, code, body, tc.code)
				}
				unchangedConfig(t, srv, cfgPath, want, player, tc.name)
				if fake.called("dacs")+fake.called("chosen") != 0 {
					t.Errorf("the refusal read the box: %v", fake.calls)
				}
			})
		}
	}
}

// TestTheDACChoiceWritesTheBindingItNames pins the DAC route: the token and
// the kind go to the provider, whose binding is written to the file and the
// live config, the serial kept in the file alone and in no answer, and the
// answer says the choice interrupted a player that sounded until its save;
// choosing the binding the config holds already writes nothing.
func TestTheDACChoiceWritesTheBindingItNames(t *testing.T) {
	srv, fake, cfgPath := newPlayerTestServer(t)
	h := srv.Handler()
	// The fake player sounds until the live config names a DAC: it stops the
	// moment the save is stored, and the answer still says it was interrupted.
	fake.withholds = true
	fake.sounding = func() bool { return !srv.deps.CfgHolder.Load().Output.Player.Device.Configured() }
	code, body := playerRequest(t, h, http.MethodPost, "/api/output/player/dac", `{"selection":"tok-5","binding":"serial"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("POST dac = %d: %s", code, body)
	}
	if !strings.Contains(body, `"interrupted":true`) {
		t.Errorf("a choice that stopped the player answered: %s; want interrupted", body)
	}
	if fake.selection != "tok-5" || fake.kind != config.BindingSerial {
		t.Errorf("the provider was asked for %q by %q, want tok-5 by serial", fake.selection, fake.kind)
	}
	if strings.Contains(body, "GOBAR") {
		t.Errorf("the answer carries the serial: %s", body)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.Output.Player.Device; d.Binding != config.BindingSerial || !d.Serial.Equal(usbid.New("GOBAR-7Q2X-91KZ-4471")) || d.VendorID != "20b1" {
		t.Errorf("the file's binding is %+v, want the GO bar by serial", d)
	}
	if d := srv.deps.CfgHolder.Load().Output.Player.Device; !fake.SameBinding(d, fake.device) {
		t.Errorf("the live config's binding is %+v, want the chosen one", d)
	}
	saved := readFileOrNil(cfgPath)
	code, body = playerRequest(t, h, http.MethodPost, "/api/output/player/dac", `{"selection":"tok-5","binding":"serial"}`, nil)
	var resp playerChangeResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil || code != http.StatusOK || resp.Status != "unchanged" {
		t.Errorf("choosing the binding the config holds = %d %+v, want 200 unchanged", code, resp)
	}
	if !bytes.Equal(readFileOrNil(cfgPath), saved) {
		t.Errorf("an unchanged choice rewrote the config file")
	}
}

// playerGiveUp fires when a wait in these tests gives up: ten seconds before
// the test binary's deadline, and never when it has none.
func playerGiveUp(t *testing.T) <-chan time.Time {
	deadline, ok := t.Deadline()
	if !ok {
		return nil
	}
	return time.After(max(time.Until(deadline)-10*time.Second, 0))
}

// TestADACChoiceIsCheckedAgainWhenItsTurnComes pins where the DAC route checks
// a choice against the box: in the config's turn, under the console's lock
// and bridge.yaml's, just before the save. A choice that waits for the file's
// lock behind another writer while the DAC it names is swapped is refused as
// stale once its turn comes, and writes nothing.
func TestADACChoiceIsCheckedAgainWhenItsTurnComes(t *testing.T) {
	srv, fake, cfgPath := newPlayerTestServer(t)
	want := readFileOrNil(cfgPath)
	held, err := config.LockFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var release sync.Once
	defer release.Do(held.Unlock)
	waited, stop := config.LockWaitedForTest(cfgPath)
	defer stop()
	type answer struct {
		code int
		body string
	}
	done := make(chan answer, 1)
	h := srv.Handler()
	go func() {
		code, body := playerRequest(t, h, http.MethodPost, "/api/output/player/dac", `{"selection":"tok-5","binding":"port"}`, nil)
		done <- answer{code, body}
	}()
	select {
	case <-waited:
	case a := <-done:
		t.Fatalf("the choice answered without waiting for the file's lock: %d %s", a.code, a.body)
	case <-playerGiveUp(t):
		t.Fatal("the choice never waited for the file's lock")
	}
	// Another unit takes the card's place while the choice waits.
	fake.answerChosen(fmt.Errorf("%w: card 5 is another DAC now", ErrPlayerSelectionStale))
	release.Do(held.Unlock)
	var a answer
	select {
	case a = <-done:
	case <-playerGiveUp(t):
		t.Fatal("the choice never answered once the file's lock was free")
	}
	if a.code != http.StatusConflict || errorCode(a.body) != "stale_selection" {
		t.Fatalf("a choice whose DAC was swapped while it waited = %d: %s; want 409 stale_selection", a.code, a.body)
	}
	unchangedConfig(t, srv, cfgPath, want, config.PlayerConfig{}, "the choice of a swapped DAC")
}

// TestAStaleOrRefusedDACChoiceWritesNothing pins the choice's refusals: a
// token the provider no longer stands behind, a binding that would not name
// the card now, and a box that could not be read are each answered with their
// own code, and none writes the config; a request that names no token or an
// unknown kind never reaches the provider.
func TestAStaleOrRefusedDACChoiceWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		err        error
		status     int
		code       string
		asks       bool
	}{
		{"stale", `{"selection":"tok","binding":"serial"}`, fmt.Errorf("%w: card 5 is another DAC now", ErrPlayerSelectionStale), http.StatusConflict, "stale_selection", true},
		{"refused", `{"selection":"tok","binding":"uniqueModel"}`, fmt.Errorf("%w: a second unit of the model", ErrPlayerBindingRefused), http.StatusConflict, "binding_refused", true},
		{"unread", `{"selection":"tok","binding":"port"}`, fmt.Errorf("%w: a listing was cut short", ErrPlayerBoxUnread), http.StatusServiceUnavailable, "box_unread", true},
		{"no token", `{"selection":"","binding":"serial"}`, nil, http.StatusBadRequest, errCodeBadJSON, false},
		{"an unknown kind", `{"selection":"tok","binding":"card"}`, nil, http.StatusBadRequest, errCodeBadJSON, false},
		{"an unknown key", `{"selection":"tok","binding":"serial","card":5}`, nil, http.StatusBadRequest, errCodeBadJSON, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake, cfgPath := newPlayerTestServer(t)
			fake.chosenErr = tc.err
			want := readFileOrNil(cfgPath)
			code, body := playerRequest(t, srv.Handler(), http.MethodPost, "/api/output/player/dac", tc.body, nil)
			if code != tc.status || errorCode(body) != tc.code {
				t.Fatalf("= %d: %s; want %d %s", code, body, tc.status, tc.code)
			}
			unchangedConfig(t, srv, cfgPath, want, config.PlayerConfig{}, tc.name)
			if asked := fake.called("chosen") > 0; asked != tc.asks {
				t.Errorf("the provider was asked: %v, want %v", asked, tc.asks)
			}
		})
	}
}

// TestThePlayerWritesAreGuardedAgainstAnotherOrigin pins the console's CSRF
// guard on the page's writes: a cross-origin Origin and a null one are
// refused with the typed cross_origin, a body that is not JSON is 415, and a
// bodyless POST, which the guard lets through with no Content-Type to check,
// is refused by the route; none writes the config or asks the provider.
func TestThePlayerWritesAreGuardedAgainstAnotherOrigin(t *testing.T) {
	for _, route := range playerWriteRoutes {
		for _, tc := range []struct {
			name   string
			body   string
			change func(*http.Request)
			status int
			code   string
		}{
			{"another origin", route.body, func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") }, http.StatusForbidden, errCodeCrossOrigin},
			{"a null origin", route.body, func(r *http.Request) { r.Header.Set("Origin", "null") }, http.StatusForbidden, errCodeCrossOrigin},
			{"a form body", route.body, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType, ""},
			{"no body", "", nil, http.StatusBadRequest, errCodeBadJSON},
		} {
			t.Run(route.path+"/"+tc.name, func(t *testing.T) {
				srv, fake, cfgPath := newPlayerTestServer(t)
				want := readFileOrNil(cfgPath)
				code, body := playerRequest(t, srv.Handler(), http.MethodPost, route.path, tc.body, tc.change)
				if code != tc.status || (tc.code != "" && errorCode(body) != tc.code) {
					t.Fatalf("= %d: %s; want %d %s", code, body, tc.status, tc.code)
				}
				unchangedConfig(t, srv, cfgPath, want, config.PlayerConfig{}, tc.name)
				if fake.called("chosen") != 0 {
					t.Errorf("the provider was asked for a choice")
				}
			})
		}
	}
}

// TestAPlayerWriteOverAnEditedConfigIsRefused: a write over a config file
// another writer changed since serve read it is refused by the holder, as
// every console save is, and the file keeps that writer's change.
func TestAPlayerWriteOverAnEditedConfigIsRefused(t *testing.T) {
	for _, route := range playerWriteRoutes {
		t.Run(route.path, func(t *testing.T) {
			srv, _, cfgPath := newPlayerTestServer(t)
			edited := editConfigBesideTheConsole(t, srv, cfgPath)
			code, body := playerRequest(t, srv.Handler(), http.MethodPost, route.path, route.body, nil)
			if code != http.StatusInternalServerError || errorCode(body) != errCodeSaveConfig {
				t.Fatalf("a write over an edited config = %d: %s; want 500 %s", code, body, errCodeSaveConfig)
			}
			unchangedConfig(t, srv, cfgPath, edited, config.PlayerConfig{}, "the refused write")
		})
	}
}

// TestAPlayerWriteAndASettingsSaveBothLand: the switch and a settings save
// sent together each land, in the file and the live config: the holder takes
// one turn at a time and each clones the other's result.
func TestAPlayerWriteAndASettingsSaveBothLand(t *testing.T) {
	srv, _, cfgPath := newPlayerTestServer(t)
	h := srv.Handler()
	for i := 0; i < 4; i++ {
		on := i%2 == 0
		name := fmt.Sprintf("Kitchen %d", i)
		var wg sync.WaitGroup
		codes := make([]int, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			codes[0], _ = playerRequest(t, h, http.MethodPost, "/api/output/player/enabled", fmt.Sprintf(`{"enabled":%v}`, on), nil)
		}()
		go func() {
			defer wg.Done()
			codes[1], _ = playerRequest(t, h, http.MethodPatch, "/api/settings", fmt.Sprintf(`{"libraryName":%q}`, name), nil)
		}()
		wg.Wait()
		if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
			t.Fatalf("round %d: the switch = %d, the settings save = %d", i, codes[0], codes[1])
		}
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		live := srv.deps.CfgHolder.Load()
		if cfg.Output.Player.Enabled != on || cfg.LibraryName != name || live.Output.Player.Enabled != on || live.LibraryName != name {
			t.Fatalf("round %d: the file holds %v %q and the live config %v %q, want %v %q",
				i, cfg.Output.Player.Enabled, cfg.LibraryName, live.Output.Player.Enabled, live.LibraryName, on, name)
		}
	}
}

// TestTheSettingsSaveIsNoRouteToThePlayer: the settings PATCH takes no player
// setting, so a body naming one changes nothing of output.player, in the file
// or the live config, on any bridge, a public one included.
func TestTheSettingsSaveIsNoRouteToThePlayer(t *testing.T) {
	srv, _, cfgPath := newPlayerTestServer(t)
	code, body := playerRequest(t, srv.Handler(), http.MethodPatch, "/api/settings",
		`{"playerEnabled":true,"playerDevice":{"binding":"port"},"output":{"player":{"enabled":true}}}`, nil)
	if code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", code, body)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Output.Player.Enabled || cfg.Output.Player.Device.Configured() || srv.deps.CfgHolder.Load().Output.Player.Enabled {
		t.Errorf("a settings save changed the player: %+v", cfg.Output.Player)
	}
}

// TestTheDACListAnswersAListNeverNull: the listing passes the provider's
// reading on, every list present.
func TestTheDACListAnswersAListNeverNull(t *testing.T) {
	srv, fake, _ := newPlayerTestServer(t)
	fake.list = PlayerDACList{Problem: "a listing was cut short"}
	code, body := playerRequest(t, srv.Handler(), http.MethodGet, "/api/output/player/dacs", "", nil)
	if code != http.StatusOK || !strings.Contains(body, `"cards":[]`) || !strings.Contains(body, "cut short") {
		t.Fatalf("GET dacs = %d: %s; want 200, cards [] and the problem", code, body)
	}
}
