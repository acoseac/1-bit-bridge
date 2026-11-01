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

	// boot and live, once playAsServes sets them, make Withholds and
	// Sounding serve's player's own answers, over the binding it started
	// with and the live config.
	boot *config.PlayerDevice
	live func() *config.Config
	// devices is the binding Chosen answers for a token it holds; a token
	// it does not hold answers device.
	devices map[string]config.PlayerDevice
	// park, while set, holds the next Chosen (parkNextChoice).
	park *choicePark
	// onPlatform and onReport, while set, are told of each Platform and
	// Report call: onPlatform under mu.
	onPlatform func()
	onReport   func(live *config.Config)
}

// choicePark is one Chosen held inside the DAC route's config turn: parked
// closes once it waits, release lets it go.
type choicePark struct {
	parked, release chan struct{}
	once            sync.Once
}

// parkNextChoice makes the next Chosen wait, inside the DAC route's config
// turn, so with the holder's lock held (and the console's, which the route
// takes first), until release is called. parked closes once it waits.
// release may be called more than once, and the test's cleanup calls it in
// any case, so a failure leaves no write held.
func (f *fakePlayerOutput) parkNextChoice(t *testing.T) (parked <-chan struct{}, release func()) {
	p := &choicePark{parked: make(chan struct{}), release: make(chan struct{})}
	f.mu.Lock()
	f.park = p
	f.mu.Unlock()
	release = func() { p.once.Do(func() { close(p.release) }) }
	t.Cleanup(release)
	return p.parked, release
}

// playAsServes makes Withholds and Sounding answer as serve's player does
// (consolePlayer.Withholds and Sounding, over its withheld) for a player
// started on boot, under a controller that sends Play whenever the player
// can take it: its output is withheld while the live config turns it off or
// names another binding than boot, and it plays exactly while nothing is
// withheld. So a play starts the moment a save lifts the withholding and
// stops the moment one withholds it, both at the save's Store, inside the
// write's update with the holder's lock held, never at a step of the test's
// own.
func (f *fakePlayerOutput) playAsServes(boot config.PlayerDevice, live func() *config.Config) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.boot, f.live = &boot, live
}

// withheld is why cfg withholds the output of a player started on boot, as
// serve's player hears it: turned off first, then another binding.
func (f *fakePlayerOutput) withheld(boot config.PlayerDevice, cfg *config.Config) string {
	switch {
	case !cfg.Output.Player.Enabled:
		return "disabled"
	case !f.SameBinding(cfg.Output.Player.Device, boot):
		return "binding_changed"
	}
	return ""
}

// model is playAsServes' player, nil boot when none is set.
func (f *fakePlayerOutput) model() (*config.PlayerDevice, func() *config.Config) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.boot, f.live
}

// platformCall closes the channel it returns at the nth Platform call from
// now on.
func (f *fakePlayerOutput) platformCall(n int) <-chan struct{} {
	ch := make(chan struct{})
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := 0
	f.onPlatform = func() {
		if calls++; calls == n {
			close(ch)
		}
	}
	return ch
}

// eachReport makes every later Report call fn with the config it was given.
func (f *fakePlayerOutput) eachReport(fn func(live *config.Config)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onReport = fn
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

func (f *fakePlayerOutput) Platform() string {
	f.mu.Lock()
	if f.onPlatform != nil {
		f.onPlatform()
	}
	f.mu.Unlock()
	return f.platform
}

func (f *fakePlayerOutput) Report(_ context.Context, live *config.Config) PlayerReport {
	f.count("report")
	f.mu.Lock()
	hook := f.onReport
	f.mu.Unlock()
	if hook != nil {
		hook(live)
	}
	return f.report
}

func (f *fakePlayerOutput) DACs(context.Context, *config.Config) PlayerDACList {
	f.count("dacs")
	return f.list
}

func (f *fakePlayerOutput) Chosen(_ context.Context, selection, kind string) (config.PlayerDevice, error) {
	f.count("chosen")
	f.mu.Lock()
	f.selection, f.kind = selection, kind
	dev, err := f.device, f.chosenErr
	if d, ok := f.devices[selection]; ok {
		dev = d
	}
	park := f.park
	f.park = nil
	f.mu.Unlock()
	if park != nil {
		close(park.parked)
		<-park.release
	}
	return dev, err
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

func (f *fakePlayerOutput) Sounding() bool {
	if boot, live := f.model(); boot != nil {
		return f.withheld(*boot, live()) == ""
	}
	return f.sounding != nil && f.sounding()
}

func (f *fakePlayerOutput) Withholds(before, after *config.Config) bool {
	if boot, _ := f.model(); boot != nil {
		return f.withheld(*boot, before) == "" && f.withheld(*boot, after) != ""
	}
	return f.withholds
}

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
// to hold the player block it held, its switch, its OpenHome opt-in and its
// binding: a refused write wrote nothing anywhere.
func unchangedConfig(t *testing.T, srv *Server, cfgPath string, want []byte, player config.PlayerConfig, what string) {
	t.Helper()
	if got := readFileOrNil(cfgPath); !bytes.Equal(got, want) {
		t.Errorf("%s: the config file changed:\n%s", what, got)
	}
	live := srv.deps.CfgHolder.Load().Output.Player
	sameSerial := live.Device.Serial.Equal(player.Device.Serial) || !live.Device.Serial.Present() && !player.Device.Serial.Present()
	if live.Enabled != player.Enabled || live.OpenHome != player.OpenHome || live.Device.Binding != player.Device.Binding || !sameSerial {
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
	for _, key := range []string{"started", "state", "reason", "detail", "transport", "lastError", "withheld", "openHome", "openHomeDetail", "binding", "dac", "track", "signal"} {
		if _, ok := running[key]; !ok {
			t.Errorf("running has no %q: %s", key, body)
		}
	}
	// The OpenHome switch's (B612): who owns it, and what the config asks of
	// it, beside what runs.
	configured, _ := got["configured"].(map[string]any)
	for _, key := range []string{"enabled", "starts", "reason", "binding", "openHome", "openHomePort", "openHomeName"} {
		if _, ok := configured[key]; !ok {
			t.Errorf("configured has no %q: %s", key, body)
		}
	}
	managed, _ := got["managed"].(map[string]any)
	for _, key := range []string{"enabled", "device", "openHome"} {
		if _, ok := managed[key]; !ok {
			t.Errorf("managed has no %q: %s", key, body)
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
	// write's turn found just before its save. The operator's go-ahead goes
	// with the write, which would be refused without it.
	fake.withholds = true
	fake.sounding = func() bool { return srv.deps.CfgHolder.Load().Output.Player.Enabled }
	code, body := playerRequest(t, h, http.MethodPost, "/api/output/player/enabled", `{"enabled":false,"interrupt":true}`, nil)
	var resp playerChangeResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil || code != http.StatusOK {
		t.Fatalf("POST enabled false with the go-ahead = %d: %s", code, body)
	}
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

// playerInterruptingWrites are the page's two writes as each stops what the
// player plays: the switch turning it off (from on), and the choice of a DAC,
// without the operator's go-ahead and with it.
var playerInterruptingWrites = []struct {
	path, body, goAhead string
}{
	{"/api/output/player/enabled", `{"enabled":false}`, `{"enabled":false,"interrupt":true}`},
	{"/api/output/player/dac", `{"selection":"tok-5","binding":"serial"}`, `{"selection":"tok-5","binding":"serial","interrupt":true}`},
}

// turnPlayerOn turns the player on through the switch, nothing sounding, so a
// later write can turn it off.
func turnPlayerOn(t *testing.T, srv *Server) {
	t.Helper()
	if code, body := playerRequest(t, srv.Handler(), http.MethodPost, "/api/output/player/enabled", `{"enabled":true}`, nil); code != http.StatusOK {
		t.Fatalf("turning the player on = %d: %s", code, body)
	}
}

// TestAWriteThatWouldStopPlaybackAsksFirst pins the server's confirmation: a
// write whose change would stop what the player plays or holds paused now (the
// player sounding in the write's turn, and the change withholding its output)
// is refused with would_interrupt, in words that say what it stops, and
// nothing is saved; sent again with the operator's go-ahead (interrupt), it is
// saved, and the answer says it interrupted.
func TestAWriteThatWouldStopPlaybackAsksFirst(t *testing.T) {
	for _, route := range playerInterruptingWrites {
		t.Run(route.path, func(t *testing.T) {
			srv, fake, cfgPath := newPlayerTestServer(t)
			turnPlayerOn(t, srv)
			fake.withholds = true
			fake.sounding = func() bool { return true }
			want := readFileOrNil(cfgPath)
			player := srv.deps.CfgHolder.Load().Output.Player
			code, body := playerRequest(t, srv.Handler(), http.MethodPost, route.path, route.body, nil)
			if code != http.StatusConflict || errorCode(body) != "would_interrupt" || !strings.Contains(body, "stops it at once") {
				t.Fatalf("a write that would stop playback, without the go-ahead = %d: %s; want 409 would_interrupt saying it stops it at once", code, body)
			}
			unchangedConfig(t, srv, cfgPath, want, player, "a write that would stop playback, without the go-ahead")
			code, body = playerRequest(t, srv.Handler(), http.MethodPost, route.path, route.goAhead, nil)
			var resp playerChangeResponse
			if err := json.Unmarshal([]byte(body), &resp); err != nil || code != http.StatusOK || resp.Status != "restart" || !resp.Interrupted {
				t.Fatalf("the same write with the go-ahead = %d: %s; want 200 restart, interrupted", code, body)
			}
			if bytes.Equal(readFileOrNil(cfgPath), want) {
				t.Errorf("the write with the go-ahead saved nothing")
			}
		})
	}
}

// TestAWriteThatStopsNothingIsNotAsked: with nothing sounding, or with a change
// that withholds nothing (the player turned on, or the DAC it started with
// chosen again), a write is saved at once, the go-ahead neither asked for nor
// needed.
func TestAWriteThatStopsNothingIsNotAsked(t *testing.T) {
	for _, tc := range []struct {
		name                string
		sounding, withholds bool
	}{
		{"nothing sounding", false, true},
		{"a change that withholds nothing", true, false},
	} {
		for _, route := range playerInterruptingWrites {
			t.Run(tc.name+route.path, func(t *testing.T) {
				srv, fake, _ := newPlayerTestServer(t)
				turnPlayerOn(t, srv)
				fake.withholds = tc.withholds
				fake.sounding = func() bool { return tc.sounding }
				code, body := playerRequest(t, srv.Handler(), http.MethodPost, route.path, route.body, nil)
				var resp playerChangeResponse
				if err := json.Unmarshal([]byte(body), &resp); err != nil || code != http.StatusOK || resp.Status != "restart" || resp.Interrupted {
					t.Fatalf("= %d: %s; want 200 restart, not interrupted", code, body)
				}
			})
		}
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
	player := srv.deps.CfgHolder.Load().Output.Player
	code, body := playerRequest(t, srv.Handler(), http.MethodPost, "/api/output/player/enabled", `{"enabled":true}`, nil)
	if code != http.StatusConflict || errorCode(body) != "player_refused" {
		t.Fatalf("turning on a player whose OpenHome port the console binds = %d: %s; want 409 player_refused", code, body)
	}
	if !strings.Contains(body, "adminAddress") || strings.Contains(body, "7789") {
		t.Errorf("the refusal should name the setting and never the port: %s", body)
	}
	unchangedConfig(t, srv, cfgPath, want, player, "the refused switch")
}

// playerWriteRoutes are the page's three writes, each with a body that would
// change the config and the managed setting that owns it.
var playerWriteRoutes = []struct {
	path, body, managed string
}{
	{"/api/output/player/enabled", `{"enabled":true}`, managedPlayerEnabled},
	{"/api/output/player/openhome", `{"openhome":true}`, managedPlayerOpenHome},
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
	// The operator's go-ahead goes with the choice, which would be refused
	// without it.
	fake.withholds = true
	fake.sounding = func() bool { return !srv.deps.CfgHolder.Load().Output.Player.Device.Configured() }
	code, body := playerRequest(t, h, http.MethodPost, "/api/output/player/dac", `{"selection":"tok-5","binding":"serial","interrupt":true}`, nil)
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

// The DACs the race below binds: the one the player started with, the one
// the config names instead as the race begins, and a third.
var (
	raceBootDAC = config.PlayerDevice{Transport: "usb", Binding: config.BindingSerial, VendorID: "20b1", ProductID: "3009",
		Serial: usbid.New("GOBAR-7Q2X-91KZ-4471")}
	raceOtherDAC = config.PlayerDevice{Transport: "usb", Binding: config.BindingSerial, VendorID: "152a", ProductID: "85dd",
		Serial: usbid.New("HUGO2-5521-0937-1186")}
	raceThirdDAC = config.PlayerDevice{Transport: "usb", Binding: config.BindingSerial, VendorID: "262a", ProductID: "1048",
		Serial: usbid.New("MOJO2-3310-7754-0021")}
)

// playerAnswer is one answer of the console: its status and its body.
type playerAnswer struct {
	code int
	body string
}

// TestAWriteBehindADACChoiceAsksBeforeStoppingThePlayItLetStart pins the
// console's lock on the page's two writes (lockPlayerWrite). The player
// started on one DAC and the config names another, so its output is withheld
// and nothing plays. A choice of the DAC it started with is in its config
// turn, parked there (parkNextChoice); its save lifts the withholding, and a
// play starts in that save (playAsServes: a controller that plays whenever
// the player can). A second write comes while the choice is parked: the
// switch turning the player off, or the choice of a third DAC, either of which
// would stop that play. With the lock, the second write reads the player
// configuration (before) only once the choice is saved, so its interruption
// check compares what it would save, and, sent without the operator's
// go-ahead, it is answered as the page's contract for a write that would stop
// playback says (playerInterruption): 409 would_interrupt with the words for
// what it stops, nothing saved over the choice's result, in the file or the
// live config, and the play still sounding. Without the lock it reads before
// ahead of the choice's save, finds the output withheld already, and stops
// the play unasked, answered as not interrupted.
//
// The order is the test's, never the scheduler's: the choice is let go only
// once the second write is known to wait for the console's lock
// (testPlayerWriteWaits) or, with its lock or the choice's taken out, to have
// read before (its Platform call right after that Load; its first is its
// check ahead of the lock).
func TestAWriteBehindADACChoiceAsksBeforeStoppingThePlayItLetStart(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, stops string
	}{
		{"the switch turning the player off", "/api/output/player/enabled", `{"enabled":false}`, playerStopsOff},
		{"the choice of another DAC", "/api/output/player/dac", `{"selection":"tok-third","binding":"serial"}`, playerStopsDAC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake, cfgPath := newPlayerTestServer(t)
			waits := make(chan struct{})
			var waited sync.Once
			srv.testPlayerWriteWaits = func() { waited.Do(func() { close(waits) }) }
			fake.devices = map[string]config.PlayerDevice{"tok-boot": raceBootDAC, "tok-other": raceOtherDAC, "tok-third": raceThirdDAC}
			fake.playAsServes(raceBootDAC, srv.deps.CfgHolder.Load)
			h := srv.Handler()

			// The config names another DAC than the one the player started
			// with: its output is withheld, and nothing plays.
			turnPlayerOn(t, srv)
			if code, body := playerRequest(t, h, http.MethodPost, "/api/output/player/dac", `{"selection":"tok-other","binding":"serial"}`, nil); code != http.StatusOK {
				t.Fatalf("choosing another DAC than the one the player started with = %d: %s", code, body)
			}
			if fake.Sounding() {
				t.Fatal("the player sounds while the config names another DAC than the one it started with")
			}

			// Each write runs on a goroutine of its own, and the cleanup lets
			// the parked choice go and waits for every one of them to end,
			// whatever ended the test.
			var writes sync.WaitGroup
			parked, release := fake.parkNextChoice(t)
			t.Cleanup(func() {
				release()
				ended := make(chan struct{})
				go func() { writes.Wait(); close(ended) }()
				select {
				case <-ended:
				case <-playerGiveUp(t):
					t.Error("a write never ended")
				}
			})
			send := func(path, body string) <-chan playerAnswer {
				answer := make(chan playerAnswer, 1)
				writes.Add(1)
				go func() {
					defer writes.Done()
					code, body := playerRequest(t, h, http.MethodPost, path, body, nil)
					answer <- playerAnswer{code, body}
				}()
				return answer
			}
			// What the choice saved, read as the choice answers with its
			// status, while it still holds the console's lock: the file and
			// the live config it stored.
			var saved struct {
				once sync.Once
				file []byte
				live *config.Config
			}
			fake.eachReport(func(live *config.Config) {
				if live.Output.Player.Enabled && fake.SameBinding(live.Output.Player.Device, raceBootDAC) {
					saved.once.Do(func() { saved.file, saved.live = readFileOrNil(cfgPath), live })
				}
			})

			first := send("/api/output/player/dac", `{"selection":"tok-boot","binding":"serial"}`)
			select {
			case <-parked:
			case a := <-first:
				t.Fatalf("the choice answered before it reached its choice: %d %s", a.code, a.body)
			case <-playerGiveUp(t):
				t.Fatal("the choice never reached its choice")
			}
			readBefore := fake.platformCall(2)
			second := send(tc.path, tc.body)
			select {
			case <-waits:
			case <-readBefore:
			case a := <-second:
				t.Fatalf("the second write answered while the choice was in its turn: %d %s", a.code, a.body)
			case <-playerGiveUp(t):
				t.Fatal("the second write neither waited for the console's lock nor read the player's configuration")
			}
			release()
			var answers [2]playerAnswer
			for i, answer := range []<-chan playerAnswer{first, second} {
				select {
				case answers[i] = <-answer:
				case <-playerGiveUp(t):
					t.Fatalf("write %d never answered", i+1)
				}
			}

			var resp playerChangeResponse
			if err := json.Unmarshal([]byte(answers[0].body), &resp); err != nil || answers[0].code != http.StatusOK ||
				resp.Status != "restart" || resp.Interrupted {
				t.Fatalf("the choice of the DAC the player started with = %d: %s; want 200 restart, nothing interrupted",
					answers[0].code, answers[0].body)
			}
			var refusal struct{ Error, Message string }
			_ = json.Unmarshal([]byte(answers[1].body), &refusal)
			if answers[1].code != http.StatusConflict || refusal.Error != playerWouldInterrupt || refusal.Message != tc.stops {
				t.Errorf("the write behind the choice = %d: %s; want 409 %s saying %q", answers[1].code, answers[1].body,
					playerWouldInterrupt, tc.stops)
			}
			if !fake.Sounding() {
				t.Error("the play the choice let start was stopped")
			}
			if saved.live == nil {
				t.Fatal("the choice answered with no status of the config it saved")
			}
			unchangedConfig(t, srv, cfgPath, saved.file, saved.live.Output.Player, "the write behind the choice")
			if srv.deps.CfgHolder.Load() != saved.live {
				t.Error("the live config was stored again after the choice's save")
			}
		})
	}
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
// or the live config, on any bridge, a public one included: not the switch,
// not the DAC, not the OpenHome opt-in (B612), by the managed-setting names
// or the config's own keys.
func TestTheSettingsSaveIsNoRouteToThePlayer(t *testing.T) {
	srv, _, cfgPath := newPlayerTestServer(t)
	code, body := playerRequest(t, srv.Handler(), http.MethodPatch, "/api/settings",
		`{"playerEnabled":true,"playerDevice":{"binding":"port"},"playerOpenHome":true,"openhome":true,"openHome":true,`+
			`"output":{"player":{"enabled":true,"openhome":true,"openhomePort":7800}}}`, nil)
	if code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", code, body)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p, live := cfg.Output.Player, srv.deps.CfgHolder.Load().Output.Player
	if p.Enabled || p.Device.Configured() || p.OpenHome || p.OpenHomePort != nil || live.Enabled || live.OpenHome || live.OpenHomePort != nil {
		t.Errorf("a settings save changed the player: the file's %+v, the live %+v", p, live)
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

// openHomeTo sends the OpenHome switch's write with the opt-in on, and
// requires it answered 200 with want (restart or unchanged).
func openHomeTo(t *testing.T, h http.Handler, on bool, want string) playerChangeResponse {
	t.Helper()
	code, body := playerRequest(t, h, http.MethodPost, "/api/output/player/openhome", fmt.Sprintf(`{"openhome":%v}`, on), nil)
	var resp playerChangeResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil || code != http.StatusOK || resp.Status != want {
		t.Fatalf("POST openhome %v = %d: %s; want 200 %s", on, code, body, want)
	}
	return resp
}

// playerOnDisk is the player block the config file holds now.
func playerOnDisk(t *testing.T, cfgPath string) config.PlayerConfig {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Output.Player
}

// TestTheOpenHomeSwitchWritesTheConfigThroughTheHolder pins the OpenHome
// switch (B612): on and off reach the file on disk and the live config, each
// answered restart, and leave the rest of the player block as it was; a value
// the config holds already writes nothing and is answered unchanged. It asks
// nothing of the player: a player that sounds, under a provider that says
// every change withholds its output, is not asked for a go-ahead, and the
// answer says nothing was interrupted, since the switch stops nothing.
func TestTheOpenHomeSwitchWritesTheConfigThroughTheHolder(t *testing.T) {
	srv, fake, cfgPath := newPlayerTestServer(t)
	h := srv.Handler()
	turnPlayerOn(t, srv)
	fake.withholds = true
	fake.sounding = func() bool { return true }
	for _, step := range []struct {
		on   bool
		want string
	}{{true, "restart"}, {true, "unchanged"}, {false, "restart"}, {false, "unchanged"}, {true, "restart"}} {
		saved := readFileOrNil(cfgPath)
		resp := openHomeTo(t, h, step.on, step.want)
		if resp.Interrupted {
			t.Errorf("turning the opt-in %v answered interrupted: the switch stops nothing", step.on)
		}
		disk, live := playerOnDisk(t, cfgPath), srv.deps.CfgHolder.Load().Output.Player
		if disk.OpenHome != step.on || live.OpenHome != step.on {
			t.Fatalf("turning the opt-in %v: the file holds %v and the live config %v", step.on, disk.OpenHome, live.OpenHome)
		}
		if !disk.Enabled || !live.Enabled {
			t.Errorf("turning the opt-in %v turned the player off", step.on)
		}
		if step.want == "unchanged" && !bytes.Equal(readFileOrNil(cfgPath), saved) {
			t.Errorf("an unchanged opt-in rewrote the config file")
		}
		if !resp.Player.Available || resp.Player.Running.State != "playing" {
			t.Errorf("the answer carries no status after the change: %+v", resp.Player)
		}
	}
	if n := fake.called("chosen") + fake.called("dacs"); n != 0 {
		t.Errorf("the OpenHome switch read the box: %v", fake.calls)
	}
}

// TestTheOpenHomeSwitchTurnsOnWithThePlayerOff: the opt-in turned on while
// the player is off is saved, the player left off, in the file and the live
// config: the surface starts once the player does (the status says so,
// cmd/bridge's console view). The port is still asked of it: a port another
// listener binds is refused with the player off too
// (TestTheOpenHomeSwitchRefusesAPortAnotherListenerBinds).
func TestTheOpenHomeSwitchTurnsOnWithThePlayerOff(t *testing.T) {
	srv, _, cfgPath := newPlayerTestServer(t)
	openHomeTo(t, srv.Handler(), true, "restart")
	disk, live := playerOnDisk(t, cfgPath), srv.deps.CfgHolder.Load().Output.Player
	if !disk.OpenHome || !live.OpenHome || disk.Enabled || live.Enabled {
		t.Fatalf("the opt-in on with the player off: the file holds %+v and the live config %+v; want the opt-in on, the player off", disk, live)
	}
}

// TestTheOpenHomeSwitchRefusesAPortAnotherListenerBinds pins the switch's
// check of the port before it saves the opt-in on (B612): a port another
// listener of serve binds (the API's, the console's, the DLNA listener's
// default, the loopback route's default) is refused with 409 player_refused
// and the setting's name, never the port, and nothing is written, whether the
// player is on or off (config.PlayerEnabled asks it only of a player that is
// on); port 0 and a port of its own are saved; and turning the opt-in off
// asks no port, so a clash another writer left is no refusal of it.
func TestTheOpenHomeSwitchRefusesAPortAnotherListenerBinds(t *testing.T) {
	for _, tc := range []struct {
		name, port, setting string
	}{
		{"the API's port", "7788", "listenAddress"},
		{"the console's port", "7789", "adminAddress"},
		{"the DLNA listener's default port", "7790", "dlna.listenAddress"},
		{"the loopback route's default port", "7791", "output.loopbackPort"},
		{"a port of its own", "7800", ""},
		{"a port the kernel picks for each address", "0", ""},
	} {
		for _, playerOn := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, the player on %v", tc.name, playerOn), func(t *testing.T) {
				srv, _, cfgPath := newPlayerTestServer(t)
				writePlayerConfig(t, srv, cfgPath, fmt.Sprintf("output:\n  player:\n    enabled: %v\n    openhomePort: %s\n", playerOn, tc.port))
				want := readFileOrNil(cfgPath)
				player := srv.deps.CfgHolder.Load().Output.Player
				code, body := playerRequest(t, srv.Handler(), http.MethodPost, "/api/output/player/openhome", `{"openhome":true}`, nil)
				if tc.setting == "" {
					if code != http.StatusOK || !playerOnDisk(t, cfgPath).OpenHome || !srv.deps.CfgHolder.Load().Output.Player.OpenHome {
						t.Fatalf("the opt-in on with %s = %d: %s; want saved", tc.name, code, body)
					}
					return
				}
				if code != http.StatusConflict || errorCode(body) != "player_refused" || !strings.Contains(body, tc.setting) || !strings.Contains(body, "openhomePort") {
					t.Fatalf("the opt-in on with %s = %d: %s; want 409 player_refused naming openhomePort and %s", tc.name, code, body, tc.setting)
				}
				if strings.Contains(body, tc.port) {
					t.Errorf("the refusal quotes the port: %s", body)
				}
				unchangedConfig(t, srv, cfgPath, want, player, "the refused opt-in")
			})
		}
	}
	t.Run("off over a clash another writer left", func(t *testing.T) {
		srv, _, cfgPath := newPlayerTestServer(t)
		writePlayerConfig(t, srv, cfgPath, "output:\n  player:\n    openhome: true\n    openhomePort: 7790\n")
		openHomeTo(t, srv.Handler(), false, "restart")
		if p := playerOnDisk(t, cfgPath); p.OpenHome || p.EffectiveOpenHomePort() != 7790 {
			t.Errorf("the opt-in off over a clash: the file holds %+v; want the opt-in off, its port kept", p)
		}
	})
}

// TestTheOpenHomeSwitchTakesOneBooleanAndNothingElse pins the switch's body:
// one object with openhome true or false and nothing after it but white
// space. Each of the bodies below is 400 bad_json, nothing written to the
// file or the live config, and the provider never asked for the box.
func TestTheOpenHomeSwitchTakesOneBooleanAndNothingElse(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no openhome", `{}`},
		{"openhome null", `{"openhome":null}`},
		{"a null body", `null`},
		{"openhome a string", `{"openhome":"true"}`},
		{"openhome a number", `{"openhome":1}`},
		{"openhome a list", `{"openhome":[true]}`},
		{"an array", `[{"openhome":true}]`},
		{"an unknown key", `{"openhome":true,"enabled":true}`},
		{"a go-ahead it does not take", `{"openhome":true,"interrupt":true}`},
		{"a second object", `{"openhome":true}{"openhome":false}`},
		{"a trailing brace", `{"openhome":true}}`},
		{"a trailing bracket", `{"openhome":true}]`},
		{"a trailing word", `{"openhome":true} true`},
		{"a cut object", `{"openhome":true`},
		{"an oversized object", `{"openhome":true,"x":"` + strings.Repeat("a", adminMaxBodyBytes) + `"}`},
		{"an object followed by more than the body takes", `{"openhome":true}` + strings.Repeat(" ", adminMaxBodyBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake, cfgPath := newPlayerTestServer(t)
			want := readFileOrNil(cfgPath)
			code, body := playerRequest(t, srv.Handler(), http.MethodPost, "/api/output/player/openhome", tc.body, nil)
			if code != http.StatusBadRequest || errorCode(body) != errCodeBadJSON {
				t.Fatalf("= %d: %.300s; want 400 %s", code, body, errCodeBadJSON)
			}
			unchangedConfig(t, srv, cfgPath, want, config.PlayerConfig{}, tc.name)
			if n := fake.called("chosen") + fake.called("dacs"); n != 0 {
				t.Errorf("a refused body read the box: %v", fake.calls)
			}
		})
	}
	t.Run("white space after the object", func(t *testing.T) {
		srv, _, cfgPath := newPlayerTestServer(t)
		if code, body := playerRequest(t, srv.Handler(), http.MethodPost, "/api/output/player/openhome", "{\"openhome\":true}\n \t\r\n", nil); code != http.StatusOK {
			t.Fatalf("an object and white space = %d: %s; want 200", code, body)
		}
		if !playerOnDisk(t, cfgPath).OpenHome {
			t.Errorf("an object and white space saved nothing")
		}
	})
}

// TestAPlayerWriteTakesNothingAfterItsObject is the regression case for the
// body check of the page's other two writes (B612): Decoder.More answers
// false for a closing brace or bracket, so `{...}}` passed until then. Each
// body here is 400 bad_json and writes nothing; the DAC route never asks the
// provider for a choice.
func TestAPlayerWriteTakesNothingAfterItsObject(t *testing.T) {
	for _, route := range []struct{ path, object string }{
		{"/api/output/player/enabled", `{"enabled":true}`},
		{"/api/output/player/dac", `{"selection":"tok","binding":"serial"}`},
	} {
		for _, tail := range []string{"}", "]", route.object, " true", `"x"`} {
			t.Run(route.path+" "+tail, func(t *testing.T) {
				srv, fake, cfgPath := newPlayerTestServer(t)
				want := readFileOrNil(cfgPath)
				code, body := playerRequest(t, srv.Handler(), http.MethodPost, route.path, route.object+tail, nil)
				if code != http.StatusBadRequest || errorCode(body) != errCodeBadJSON {
					t.Fatalf("= %d: %s; want 400 %s", code, body, errCodeBadJSON)
				}
				unchangedConfig(t, srv, cfgPath, want, config.PlayerConfig{}, route.path+tail)
				if fake.called("chosen") != 0 {
					t.Errorf("the provider was asked for a choice")
				}
			})
		}
	}
}

// TestTheManagedPlayerSettingsHideAndRefuseTheOpenHomeSwitch pins who owns
// the OpenHome switch (B612) for each managed setting, at the route, in the
// status, and on the page by the status's flags (the panel shows for an
// available bridge whose player switch is the operator's, and the OpenHome
// switch inside it when its own setting is too: renderPlayerOutput, pinned by
// TestThePlayerPageOffersTheOpenHomeSwitchBelowThePlayersOwn and run by
// TestThePlayerPageDrawsTheOpenHomeSwitchStateByState). playerEnabled managed
// owns playerOpenHome too: both routes refuse, and the whole panel is hidden.
// playerOpenHome managed alone: its route refuses and its switch is hidden,
// and the player's switch stays the operator's. Neither: both are offered
// and both routes save.
func TestTheManagedPlayerSettingsHideAndRefuseTheOpenHomeSwitch(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		managed                    string
		enabled, openHome          bool // the flags the status carries
		panelShows, openHomeShows  bool
		enabledSaves, openHomeSave bool
	}{
		{"playerEnabled", "playerEnabled", true, true, false, false, false, false},
		{"playerOpenHome alone", "playerOpenHome", false, true, true, false, true, false},
		{"neither", "libraryName", false, false, true, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, cfgPath := newPlayerTestServer(t)
			writePlayerConfig(t, srv, cfgPath, "deployment:\n  managedSettings: ["+tc.managed+"]\n")
			code, body := playerRequest(t, srv.Handler(), http.MethodGet, "/api/output/player", "", nil)
			var st PlayerStatus
			if err := json.Unmarshal([]byte(body), &st); err != nil || code != http.StatusOK {
				t.Fatalf("GET = %d, %v: %s", code, err, body)
			}
			if st.Managed.Enabled != tc.enabled || st.Managed.OpenHome != tc.openHome {
				t.Errorf("managed %+v; want enabled %v, openHome %v", st.Managed, tc.enabled, tc.openHome)
			}
			panel := st.Available && !st.Managed.Enabled
			if panel != tc.panelShows || (panel && !st.Managed.OpenHome) != tc.openHomeShows {
				t.Errorf("the page shows the panel %v and the OpenHome switch %v; want %v and %v", panel, panel && !st.Managed.OpenHome, tc.panelShows, tc.openHomeShows)
			}
			for _, w := range []struct {
				path, body string
				saves      bool
			}{
				{"/api/output/player/openhome", `{"openhome":true}`, tc.openHomeSave},
				{"/api/output/player/enabled", `{"enabled":true}`, tc.enabledSaves},
			} {
				want := readFileOrNil(cfgPath)
				player := srv.deps.CfgHolder.Load().Output.Player
				code, body := playerRequest(t, srv.Handler(), http.MethodPost, w.path, w.body, nil)
				switch {
				case w.saves && code != http.StatusOK:
					t.Errorf("POST %s = %d: %s; want 200", w.path, code, body)
				case !w.saves && (code != http.StatusForbidden || errorCode(body) != playerSettingManaged):
					t.Errorf("POST %s = %d: %s; want 403 %s", w.path, code, body, playerSettingManaged)
				case !w.saves:
					unchangedConfig(t, srv, cfgPath, want, player, "the managed "+w.path)
				}
			}
		})
	}
}

// TestTheOpenHomeSwitchWaitsItsTurnBehindAnotherPlayerWrite pins the console's
// lock on the OpenHome switch (lockPlayerWrite), as on the page's other
// writes: with the DAC choice or the player switch held inside its config
// turn, the switch waits for the console's lock (testPlayerWriteWaits) and
// reads no player configuration (before) until it has it; let go, both are
// saved, each keeping the other's field in the file and the live config,
// through the holder. Without the lock, the switch's before is read beside a
// write that changes the block (its Platform call right after that Load; its
// first is its check ahead of the lock), and a value the other write saves
// can be answered as unchanged with nothing saved; it then waits on the
// holder's own mutex, which UpdateChecked takes before bridge.yaml's lock, so
// the file's lock is no signal of it.
func TestTheOpenHomeSwitchWaitsItsTurnBehindAnotherPlayerWrite(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		park             func(t *testing.T, fake *fakePlayerOutput) (parked <-chan struct{}, release func())
		saved            func(p config.PlayerConfig) bool
	}{
		{"behind a DAC choice", "/api/output/player/dac", `{"selection":"tok-5","binding":"serial"}`,
			func(t *testing.T, fake *fakePlayerOutput) (<-chan struct{}, func()) { return fake.parkNextChoice(t) },
			func(p config.PlayerConfig) bool { return p.Device.Binding == config.BindingSerial }},
		{"behind the player switch", "/api/output/player/enabled", `{"enabled":true}`,
			func(t *testing.T, fake *fakePlayerOutput) (<-chan struct{}, func()) {
				parked, release := make(chan struct{}), make(chan struct{})
				var once, freed sync.Once
				fake.sounding = func() bool {
					once.Do(func() { close(parked) })
					<-release
					return false
				}
				free := func() { freed.Do(func() { close(release) }) }
				t.Cleanup(free)
				return parked, free
			},
			func(p config.PlayerConfig) bool { return p.Enabled }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake, cfgPath := newPlayerTestServer(t)
			waits := make(chan struct{})
			var waited sync.Once
			srv.testPlayerWriteWaits = func() { waited.Do(func() { close(waits) }) }
			h := srv.Handler()
			parked, release := tc.park(t, fake)
			var writes sync.WaitGroup
			t.Cleanup(func() {
				release()
				ended := make(chan struct{})
				go func() { writes.Wait(); close(ended) }()
				select {
				case <-ended:
				case <-playerGiveUp(t):
					t.Error("a write never ended")
				}
			})
			send := func(path, body string) <-chan playerAnswer {
				answer := make(chan playerAnswer, 1)
				writes.Add(1)
				go func() {
					defer writes.Done()
					code, body := playerRequest(t, h, http.MethodPost, path, body, nil)
					answer <- playerAnswer{code, body}
				}()
				return answer
			}
			first := send(tc.path, tc.body)
			select {
			case <-parked:
			case a := <-first:
				t.Fatalf("the first write answered before its turn held: %d %s", a.code, a.body)
			case <-playerGiveUp(t):
				t.Fatal("the first write never reached its turn")
			}
			// The parked write made every Platform call of its own before its
			// turn held: the next two are the switch's.
			readBefore := fake.platformCall(2)
			second := send("/api/output/player/openhome", `{"openhome":true}`)
			select {
			case <-waits:
			case <-readBefore:
				t.Fatal("the OpenHome switch read the player's configuration while another write held its turn, without waiting for the console's lock")
			case a := <-second:
				t.Fatalf("the OpenHome switch answered while another write held its turn: %d %s", a.code, a.body)
			case <-playerGiveUp(t):
				t.Fatal("the OpenHome switch never waited for the console's lock")
			}
			release()
			for i, answer := range []<-chan playerAnswer{first, second} {
				select {
				case a := <-answer:
					var resp playerChangeResponse
					if err := json.Unmarshal([]byte(a.body), &resp); err != nil || a.code != http.StatusOK || resp.Status != "restart" {
						t.Errorf("write %d = %d: %s; want 200 restart", i+1, a.code, a.body)
					}
				case <-playerGiveUp(t):
					t.Fatalf("write %d never answered", i+1)
				}
			}
			disk, live := playerOnDisk(t, cfgPath), srv.deps.CfgHolder.Load().Output.Player
			if !tc.saved(disk) || !tc.saved(live) || !disk.OpenHome || !live.OpenHome {
				t.Errorf("after both writes: the file holds %+v and the live config %+v; want both changes", disk, live)
			}
		})
	}
}

// TestTheOpenHomeSwitchAndThePlayerSwitchBothLand: the two switches sent
// together, round after round, each land, in the file and the live config:
// the console's lock gives them one turn at a time, and each clones the
// other's result.
func TestTheOpenHomeSwitchAndThePlayerSwitchBothLand(t *testing.T) {
	srv, _, cfgPath := newPlayerTestServer(t)
	h := srv.Handler()
	for i := 0; i < 6; i++ {
		enabled, openHome := i%2 == 0, i%3 != 1
		var wg sync.WaitGroup
		codes := make([]int, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			codes[0], _ = playerRequest(t, h, http.MethodPost, "/api/output/player/enabled", fmt.Sprintf(`{"enabled":%v}`, enabled), nil)
		}()
		go func() {
			defer wg.Done()
			codes[1], _ = playerRequest(t, h, http.MethodPost, "/api/output/player/openhome", fmt.Sprintf(`{"openhome":%v}`, openHome), nil)
		}()
		wg.Wait()
		if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
			t.Fatalf("round %d: the switch = %d, the OpenHome switch = %d", i, codes[0], codes[1])
		}
		disk, live := playerOnDisk(t, cfgPath), srv.deps.CfgHolder.Load().Output.Player
		if disk.Enabled != enabled || disk.OpenHome != openHome || live.Enabled != enabled || live.OpenHome != openHome {
			t.Fatalf("round %d: the file holds %v %v and the live config %v %v, want %v %v",
				i, disk.Enabled, disk.OpenHome, live.Enabled, live.OpenHome, enabled, openHome)
		}
	}
}
