package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The control routes of the bridge's own player (B634, PR 1), over a fake
// PlayerControl: what each route refuses before anything is read, the guards
// every console write has, the bounds of a body, the answers' statuses, the
// event stream's player frame and the catalog rows' bridge field. The control
// itself, over the real engine, is cmd/bridge's (serve_player_control_test.go).

// fakeControl is a PlayerControl that records what reaches it and answers
// what the test sets.
type fakeControl struct {
	platform string
	lists    bool
	plays    PlayerPlays
	runs     bool

	mu      sync.Mutex
	calls   []string
	replace PlayerReplace
	answer  PlayerAnswer
	pub     PlayerPublication
	wake    chan struct{}
	// handoverPaths are the library paths a read back names its record's
	// handover's tracks by.
	handoverPaths []string
}

func newFakeControl() *fakeControl {
	pub := PlayerPublication{Epoch: strings.Repeat("e", 32), Revision: 7, QueueRev: 3,
		Bytes: []byte(`{"epoch":"` + strings.Repeat("e", 32) + `","revision":7}`)}
	return &fakeControl{platform: "linux", lists: true, runs: true, pub: pub, wake: make(chan struct{}),
		answer: PlayerAnswer{Record: &PlayerRecord{Status: PlayerOpDone, Effect: PlayerEffectSome}, Player: pub}}
}

func (f *fakeControl) count(what string) {
	f.mu.Lock()
	f.calls = append(f.calls, what)
	f.mu.Unlock()
}

func (f *fakeControl) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeControl) Platform() string { return f.platform }
func (f *fakeControl) Lists() bool      { return f.lists }
func (f *fakeControl) Now(context.Context) PlayerPublication {
	f.count("now")
	return f.pub
}
func (f *fakeControl) Latest() (PlayerPublication, bool) { return f.pub, true }
func (f *fakeControl) Wake() <-chan struct{}             { return f.wake }
func (f *fakeControl) Queue(context.Context) PlayerQueue {
	f.count("queue")
	return PlayerQueue{Epoch: f.pub.Epoch, ReadAt: f.pub.Revision, QueueRev: f.pub.QueueRev, Entries: []PlayerQueueEntry{}}
}
func (f *fakeControl) Plays() (PlayerPlays, bool) { return f.plays, f.runs }
func (f *fakeControl) Replace(_ context.Context, r PlayerReplace) PlayerAnswer {
	f.count("replace")
	f.mu.Lock()
	f.replace = r
	f.mu.Unlock()
	return f.answer
}
func (f *fakeControl) Append(context.Context, PlayerAppend) PlayerAnswer {
	f.count("append")
	return f.answer
}
func (f *fakeControl) Delete(context.Context, PlayerDelete) PlayerAnswer {
	f.count("delete")
	return f.answer
}
func (f *fakeControl) Transport(context.Context, PlayerCommand) PlayerAnswer {
	f.count("transport")
	return f.answer
}
func (f *fakeControl) SwitchOut(context.Context, PlayerSwitchOut) PlayerAnswer {
	f.count("switch-out")
	return f.answer
}
func (f *fakeControl) Operation(epoch, tab string, seq uint64) PlayerReadBack {
	f.count("operation")
	return PlayerReadBack{Epoch: f.pub.Epoch, Revision: f.pub.Revision, Record: f.answer.Record, HandoverPaths: f.handoverPaths}
}

// The identity and the generation a test's bodies carry.
var (
	testOp  = `{"tab":"` + strings.Repeat("ab", 16) + `","seq":1,"epoch":"` + strings.Repeat("e", 32) + `"}`
	testGen = "0123456789abcdef.4"
)

// controlRoutes are the eight control routes, each with a body it takes.
var controlRoutes = []struct{ method, path, body string }{
	{http.MethodGet, "/api/output/player/now", ""},
	{http.MethodGet, "/api/output/player/queue", ""},
	{http.MethodGet, "/api/output/player/operation?tab=" + strings.Repeat("ab", 16) + "&seq=1&epoch=" + strings.Repeat("e", 32), ""},
	{http.MethodPost, "/api/output/player/queue", `{"op":` + testOp + `,"at":"` + testGen + `","tracks":["A/01.flac"],"play":true}`},
	{http.MethodPost, "/api/output/player/queue/append", `{"op":` + testOp + `,"at":"` + testGen + `","tracks":["A/01.flac"]}`},
	{http.MethodPost, "/api/output/player/queue/delete", `{"op":` + testOp + `,"at":"` + testGen + `","entry":3}`},
	{http.MethodPost, "/api/output/player/transport", `{"op":` + testOp + `,"command":"stop"}`},
	{http.MethodPost, "/api/output/player/switch-out", `{"op":` + testOp + `,"take":true}`},
}

// newControlTestServer is the console with a fake control wired.
func newControlTestServer(t *testing.T) (*Server, *fakeControl, string) {
	t.Helper()
	srv, _, cfgPath := newTestServer(t)
	recordBootRead(t, srv, cfgPath)
	fake := newFakeControl()
	srv.deps.PlayerControl = fake
	return srv, fake, cfgPath
}

// TestTheControlRoutesRefuseWhereThePlayerCannotRun pins controlRefusal:
// with no control wired, on a public bridge (a signed-in session included),
// on a host that is not Linux, and where serve's boot plan lists no player,
// every control route refuses with its word before anything reaches the
// control, and each word says no player is offered here.
func TestTheControlRoutesRefuseWhereThePlayerCannotRun(t *testing.T) {
	for _, route := range controlRoutes {
		for _, tc := range []struct {
			name, code string
			status     int
			arrange    func(srv *Server, fake *fakeControl)
		}{
			{"no control wired", playerNotWired, http.StatusServiceUnavailable, func(srv *Server, _ *fakeControl) { srv.deps.PlayerControl = nil }},
			{"not Linux", playerNotSupported, http.StatusConflict, func(_ *Server, fake *fakeControl) { fake.platform = "darwin" }},
			{"no player in the boot plan", playerNotOffered, http.StatusConflict, func(_ *Server, fake *fakeControl) { fake.lists = false }},
		} {
			t.Run(route.path+"/"+tc.name, func(t *testing.T) {
				srv, fake, _ := newControlTestServer(t)
				tc.arrange(srv, fake)
				code, body := playerRequest(t, srv.Handler(), route.method, route.path, route.body, nil)
				if code != tc.status || errorCode(body) != tc.code {
					t.Fatalf("= %d: %s; want %d %s", code, body, tc.status, tc.code)
				}
				if calls := fake.called(); len(calls) != 0 {
					t.Errorf("the refusal reached the control: %v", calls)
				}
			})
		}
		t.Run(route.path+"/public with a session", func(t *testing.T) {
			srv, store, _, _ := newPublicTestServerAt(t, "test-password-123")
			fake := newFakeControl()
			srv.deps.PlayerControl = fake
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			req, err := http.NewRequest(route.method, ts.URL+route.path, strings.NewReader(route.body))
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
			if calls := fake.called(); len(calls) != 0 {
				t.Errorf("the refusal reached the control: %v", calls)
			}
		})
	}
}

// TestAnUnreadablePlayerBlockStopsNothingTheControlAsks pins R8: the control
// routes ask neither the player block's Unread nor the managed settings,
// which guard writes of output.player no control route makes, so a block
// that does not read stops no Stop and no read of a player that runs.
func TestAnUnreadablePlayerBlockStopsNothingTheControlAsks(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"an unreadable block", "output:\n  player:\n    enabled: true\n    device:\n      card: 5\n"},
		{"managed player settings", "deployment:\n  managedSettings: [playerEnabled, playerDevice]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake, cfgPath := newControlTestServer(t)
			writePlayerConfig(t, srv, cfgPath, tc.text)
			if code, body := playerRequest(t, srv.Handler(), http.MethodGet, "/api/output/player/now", "", nil); code != http.StatusOK {
				t.Fatalf("GET now = %d: %s", code, body)
			}
			if code, body := playerRequest(t, srv.Handler(), http.MethodPost, "/api/output/player/transport", `{"op":`+testOp+`,"command":"stop"}`, nil); code != http.StatusOK {
				t.Fatalf("a Stop = %d: %s", code, body)
			}
			if calls := fake.called(); strings.Join(calls, ",") != "now,transport" {
				t.Fatalf("the control was asked %v, want now and the Stop", calls)
			}
		})
	}
}

// TestTheControlRoutesAreGuardedLikeEveryConsoleWrite pins the control's
// writes behind csrfGuard and the body checks every console write has:
// another origin, a null one, a form body and no body are refused, nothing
// reaching the control.
func TestTheControlRoutesAreGuardedLikeEveryConsoleWrite(t *testing.T) {
	for _, route := range controlRoutes {
		if route.method != http.MethodPost {
			continue
		}
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
			{"two objects", route.body + route.body, nil, http.StatusBadRequest, errCodeBadJSON},
			{"a key it does not take", strings.Replace(route.body, `{"op":`, `{"shuffle":true,"op":`, 1), nil, http.StatusBadRequest, errCodeBadJSON},
			{"no identity", strings.Replace(route.body, testOp, `{"tab":"x","seq":0,"epoch":""}`, 1), nil, http.StatusBadRequest, errCodeBadOperation},
		} {
			t.Run(route.path+"/"+tc.name, func(t *testing.T) {
				srv, fake, _ := newControlTestServer(t)
				code, body := playerRequest(t, srv.Handler(), http.MethodPost, route.path, tc.body, tc.change)
				if code != tc.status || (tc.code != "" && errorCode(body) != tc.code) {
					t.Fatalf("= %d: %s; want %d %s", code, body, tc.status, tc.code)
				}
				if calls := fake.called(); len(calls) != 0 {
					t.Errorf("a refused request reached the control: %v", calls)
				}
			})
		}
	}
}

// TestAControlBodyHasBoundsOfItsOwn pins K5: the control routes' body is
// bounded at 8 MiB, past the 1 MiB the Player page's writes take, so a list
// of paths over 1 MiB reaches the control whole (which windows it to the
// queue); past 8 MiB it is 413 body_too_large, past 20,000 paths
// too_many_tracks, and an empty path or one over 4,096 bytes bad_track,
// each its own refusal and none reaching the control.
func TestAControlBodyHasBoundsOfItsOwn(t *testing.T) {
	long := strings.Repeat("a", 200) + "/" + strings.Repeat("b", 200) + "/" + strings.Repeat("c", 200) + "/01.flac"
	body := func(tracks []string) string {
		b, err := json.Marshal(map[string]any{"op": json.RawMessage(testOp), "at": testGen, "tracks": tracks})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	list := func(n int, p string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = p
		}
		return out
	}
	over1MiB := body(list(1800, long))
	if len(over1MiB) <= 1<<20 {
		t.Fatalf("the body is %d bytes, not over 1 MiB", len(over1MiB))
	}
	t.Run("a list over 1 MiB", func(t *testing.T) {
		srv, fake, _ := newControlTestServer(t)
		if code, raw := playerRequest(t, srv.Handler(), http.MethodPost, "/api/output/player/queue", over1MiB, nil); code != http.StatusOK {
			t.Fatalf("a %d-byte list = %d: %.300s", len(over1MiB), code, raw)
		}
		fake.mu.Lock()
		got := len(fake.replace.Tracks)
		fake.mu.Unlock()
		if got != 1800 {
			t.Fatalf("the control got %d tracks of 1,800", got)
		}
	})
	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"a body over 8 MiB", body(list(14500, long)), errCodeBodyTooLarge, http.StatusRequestEntityTooLarge},
		{"more than 20,000 paths", body(list(20001, "A/01.flac")), errCodeTooManyTracks, http.StatusBadRequest},
		{"an empty path", body([]string{"A/01.flac", ""}), errCodeBadTrack, http.StatusBadRequest},
		{"a path over 4,096 bytes", body([]string{strings.Repeat("x", 4097)}), errCodeBadTrack, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake, _ := newControlTestServer(t)
			code, raw := playerRequest(t, srv.Handler(), http.MethodPost, "/api/output/player/queue", tc.body, nil)
			if code != tc.status || errorCode(raw) != tc.code {
				t.Fatalf("= %d: %.300s; want %d %s", code, raw, tc.status, tc.code)
			}
			if calls := fake.called(); len(calls) != 0 {
				t.Errorf("a refused body reached the control: %v", calls)
			}
		})
	}
}

// TestEveryControlAnswerHasItsStatus pins §5.3's table: a record's status
// answers 200 done, 202 running, and 409 with its word for the rest (404 for
// not_found); a refusal before the request became an operation carries no
// record and the latest publication, operation_conflict 409 among them (K11).
func TestEveryControlAnswerHasItsStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer PlayerAnswer
		status int
		code   string
		record bool
	}{
		{"done", PlayerAnswer{Record: &PlayerRecord{Status: PlayerOpDone}}, http.StatusOK, "", true},
		{"running", PlayerAnswer{Record: &PlayerRecord{Status: PlayerOpRunning}}, http.StatusAccepted, "", true},
		{"changed", PlayerAnswer{Record: &PlayerRecord{Status: PlayerOpChanged}}, http.StatusConflict, "changed", true},
		{"cancelled", PlayerAnswer{Record: &PlayerRecord{Status: PlayerOpCancelled}}, http.StatusConflict, "cancelled", true},
		{"an outcome not known", PlayerAnswer{Record: &PlayerRecord{Status: PlayerOpUnknown}}, http.StatusConflict, "outcome_unknown", true},
		{"refused stale", PlayerAnswer{Record: &PlayerRecord{Status: PlayerOpRefused, Error: "stale"}}, http.StatusConflict, "stale", true},
		{"refused not_found", PlayerAnswer{Record: &PlayerRecord{Status: PlayerOpRefused, Error: "not_found"}}, http.StatusNotFound, "not_found", true},
		{"epoch_changed", PlayerAnswer{Refused: "epoch_changed", Status: http.StatusConflict}, http.StatusConflict, "epoch_changed", false},
		{"replayed", PlayerAnswer{Refused: "replayed", Status: http.StatusConflict}, http.StatusConflict, "replayed", false},
		{"operation_conflict", PlayerAnswer{Refused: "operation_conflict", Status: http.StatusConflict}, http.StatusConflict, "operation_conflict", false},
		{"operation_gone", PlayerAnswer{Refused: "operation_gone", Status: http.StatusNotFound}, http.StatusNotFound, "operation_gone", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake, _ := newControlTestServer(t)
			tc.answer.Player = fake.pub
			fake.answer = tc.answer
			code, raw := playerRequest(t, srv.Handler(), http.MethodPost, "/api/output/player/transport", `{"op":`+testOp+`,"command":"stop"}`, nil)
			var got struct {
				Error   string          `json:"error"`
				Message string          `json:"message"`
				Record  json.RawMessage `json:"record"`
				Player  json.RawMessage `json:"player"`
			}
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
			if code != tc.status || got.Error != tc.code {
				t.Fatalf("= %d %q, want %d %q: %s", code, got.Error, tc.status, tc.code, raw)
			}
			if (len(got.Record) > 0 && string(got.Record) != "null") != tc.record {
				t.Errorf("record %s, want one: %v", got.Record, tc.record)
			}
			if string(got.Player) != string(fake.pub.Bytes) {
				t.Errorf("the answer's publication %s, want %s", got.Player, fake.pub.Bytes)
			}
			if tc.code != "" && got.Message == "" {
				t.Errorf("%s has no sentence", tc.code)
			}
		})
	}
}

// sseEvents connects to the console's event stream and answers the events
// its first burst names within bound.
func sseEvents(t *testing.T, srv *Server, bound time.Duration) []string {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var names []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if name, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			names = append(names, name)
		}
	}
	return names
}

// TestEachExcludedDeploymentSaysNoPlayerIsOfferedHere pins K1: where no
// player is offered (no control wired, not Linux, no player in the boot plan,
// a public bridge), the event stream sends no player frame and GET now, the
// bar's first question once the stream opens, answers at once with the word
// that says so; where one is offered the stream's first burst carries it.
// Each server is new, as serve's console is just after a start.
func TestEachExcludedDeploymentSaysNoPlayerIsOfferedHere(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		arrange    func(srv *Server, fake *fakeControl)
	}{
		{"no control wired", playerNotWired, func(srv *Server, _ *fakeControl) { srv.deps.PlayerControl = nil }},
		{"not Linux", playerNotSupported, func(_ *Server, fake *fakeControl) { fake.platform = "darwin" }},
		{"no player in the boot plan", playerNotOffered, func(_ *Server, fake *fakeControl) { fake.lists = false }},
		{"offered", "", func(*Server, *fakeControl) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake, _ := newControlTestServer(t)
			tc.arrange(srv, fake)
			start := time.Now()
			code, body := playerRequest(t, srv.Handler(), http.MethodGet, "/api/output/player/now", "", nil)
			if took := time.Since(start); took > 2*time.Second {
				t.Errorf("GET now took %v", took)
			}
			events := sseEvents(t, srv, 1500*time.Millisecond)
			hasPlayer := false
			for _, e := range events {
				if e == "player" {
					hasPlayer = true
				}
			}
			if tc.code == "" {
				if code != http.StatusOK || !hasPlayer || events[0] != "player" {
					t.Fatalf("offered: GET now = %d (%s), events %v; want 200 and the player frame first", code, body, events)
				}
				return
			}
			if errorCode(body) != tc.code || hasPlayer {
				t.Fatalf("GET now = %d %s, events %v; want %s and no player frame", code, body, events, tc.code)
			}
			if len(events) == 0 {
				t.Fatal("the stream sent nothing: no proof it was open")
			}
		})
	}
	t.Run("a public bridge", func(t *testing.T) {
		srv, _, _, _ := newPublicTestServerAt(t, "test-password-123")
		srv.deps.PlayerControl = newFakeControl()
		if !strings.Contains(srv.controlRefusal().code, playerNotAvailable) || srv.playerControlOffered() {
			t.Fatalf("a public bridge offers its player: %+v", srv.controlRefusal())
		}
	})
}

// TestTheReadBackRouteHydratesItsHandoverThroughTheCatalog pins the read
// back's hydration in the route itself: GET /api/output/player/operation
// answers each of the record's handover tracks with its catalog row, found by
// the library path the control named it by, its id and its path beside it;
// a track whose path no row holds any more, and one the control could name
// by no path, answer no row and track_gone, in their places.
func TestTheReadBackRouteHydratesItsHandoverThroughTheCatalog(t *testing.T) {
	srv, fake, _ := newControlTestServer(t)
	for i, p := range []string{"Mix/Album/01.flac", "Mix/Album/02.flac"} {
		tr := &manifest.Track{Path: p, Title: []string{"One", "Two"}[i], Album: "Album", AlbumArtist: "Artist", Artist: "Artist", Codec: "FLAC",
			Size: 1000, ModTime: time.Unix(1, 0)}
		if err := srv.deps.Manifest.UpsertTrack(t.Context(), tr); err != nil {
			t.Fatal(err)
		}
	}
	fake.answer = PlayerAnswer{Record: &PlayerRecord{Tab: strings.Repeat("ab", 16), Seq: 1, Kind: "switch-out", Status: PlayerOpDone,
		Handover: &PlayerHandover{Tracks: []string{"id1", "id2", "id3", "id4"}, Selected: 1, Playing: true}}}
	fake.handoverPaths = []string{"Mix/Album/01.flac", "Mix/Album/02.flac", "Mix/Album/gone.flac", ""}
	w, body := playerGet(t, srv, "/api/output/player/operation?tab="+strings.Repeat("ab", 16)+"&seq=1&epoch="+strings.Repeat("e", 32))
	if w.Code != http.StatusOK {
		t.Fatalf("the read back = %d: %s", w.Code, w.Body.String())
	}
	rec, _ := body["record"].(map[string]any)
	h, _ := rec["handover"].(map[string]any)
	tracks, _ := h["tracks"].([]any)
	if len(tracks) != 4 || h["selected"] != float64(1) || h["playing"] != true {
		t.Fatalf("the handover %v, want its four tracks, the second selected, playing", h)
	}
	for i, want := range []struct{ id, path, title, word string }{
		{"id1", "Mix/Album/01.flac", "One", ""},
		{"id2", "Mix/Album/02.flac", "Two", ""},
		{"id3", "Mix/Album/gone.flac", "", "track_gone"},
		{"id4", "", "", "track_gone"},
	} {
		e, _ := tracks[i].(map[string]any)
		row, _ := e["track"].(map[string]any)
		path, _ := e["path"].(string)
		word, _ := e["word"].(string)
		switch {
		case e["id"] != want.id || path != want.path || word != want.word:
			t.Fatalf("track %d: %v, want id %s, path %q, word %q", i, e, want.id, want.path, want.word)
		case want.title == "" && e["track"] != nil:
			t.Fatalf("track %d: a row %v for a track with none", i, e["track"])
		case want.title != "" && (row == nil || row["title"] != want.title || row["path"] != want.path):
			t.Fatalf("track %d: row %v, want the catalog's row titled %s", i, row, want.title)
		}
	}
}

// TestTheCatalogSaysWhatTheBridgesPlayerPlays pins §6.7: each catalog row
// carries how the bridge's own player plays it (BridgeWayOf, the function a
// replace's checks run): pcm for a container it plays, native, DoP or
// converted for a DSD rate its set plays, none with its word for a rate its
// set refuses or does not name, a routed track, an SACD virtual track, a
// gated container and a lossy one; and no field at all while no player runs.
func TestTheCatalogSaysWhatTheBridgesPlayerPlays(t *testing.T) {
	srv, fake, _ := newControlTestServer(t)
	fake.plays = PlayerPlays{Codecs: []string{"flac", "wav", "aiff", "alac", "dsf", "dff"}, DSD: map[int]PlayerBridgeWay{
		2822400:  {Way: BridgeWayNative},
		5644800:  {Way: BridgeWayDoP},
		11289600: {Way: BridgeWayConverted},
		22579200: {Way: BridgeWayNone, Reason: "dsd512_unsupported"},
	}}
	st := srv.deps.Manifest
	rows := []struct {
		path, codec string
		rate        float64
		dsd         bool
		want        PlayerBridgeWay
	}{
		{"Mix/Album/01.flac", "FLAC", 96000, false, PlayerBridgeWay{Way: BridgeWayPCM}},
		{"Mix/Album/02.dsf", "DSF", 2822400, true, PlayerBridgeWay{Way: BridgeWayNative}},
		{"Mix/Album/03.dsf", "DSF", 5644800, true, PlayerBridgeWay{Way: BridgeWayDoP}},
		{"Mix/Album/04.dsf", "DSF", 11289600, true, PlayerBridgeWay{Way: BridgeWayConverted}},
		{"Mix/Album/05.dsf", "DSF", 22579200, true, PlayerBridgeWay{Way: BridgeWayNone, Reason: "dsd512_unsupported"}},
		{"Mix/Album/06.dff", "DFF", 45158400, true, PlayerBridgeWay{Way: BridgeWayNone, Reason: BridgeWordDSDNotPlayed}},
		{"Mix/Album/07.mp3", "MP3", 44100, false, PlayerBridgeWay{Way: BridgeWayNone, Reason: BridgeWordLossy}},
		{"Mix/Album/08.wv", "WAVPACK", 44100, false, PlayerBridgeWay{Way: BridgeWayNone, Reason: BridgeWordGated}},
	}
	for i, r := range rows {
		tr := &manifest.Track{Path: r.path, Title: "T" + strconv.Itoa(i), Album: "Album", AlbumArtist: "Artist", Artist: "Artist", Codec: r.codec,
			Size: 1000, ModTime: time.Unix(1, 0)}
		rate := r.rate
		tr.SampleRate = &rate
		if r.dsd {
			d := true
			tr.IsDSD = &d
		}
		if err := st.UpsertTrack(t.Context(), tr); err != nil {
			t.Fatal(err)
		}
	}
	detail := func() []map[string]any {
		_, body := playerGet(t, srv, "/api/player/albums?sort=artist")
		albums, _ := body["albums"].([]any)
		if len(albums) != 1 {
			t.Fatalf("%d albums, want 1", len(albums))
		}
		id := albums[0].(map[string]any)["id"].(string)
		w, d := playerGet(t, srv, "/api/player/albums/"+id)
		if w.Code != http.StatusOK {
			t.Fatalf("detail = %d: %s", w.Code, w.Body.String())
		}
		var out []map[string]any
		for _, tr := range d["tracks"].([]any) {
			out = append(out, tr.(map[string]any))
		}
		return out
	}
	byPath := map[string]map[string]any{}
	for _, tr := range detail() {
		byPath[tr["path"].(string)] = tr
	}
	for _, r := range rows {
		tr, ok := byPath[r.path]
		if !ok {
			t.Errorf("%s: no row", r.path)
			continue
		}
		b, _ := tr["bridge"].(map[string]any)
		got := PlayerBridgeWay{}
		if b != nil {
			got.Way, _ = b["way"].(string)
			got.Reason, _ = b["reason"].(string)
		}
		if got != r.want {
			t.Errorf("%s: bridge %+v, want %+v", r.path, got, r.want)
		}
	}
	// A routed track and an SACD virtual track, by the one function.
	if w := BridgeWayOf("Remote/01.flac", "FLAC", true, false, 44100, fake.plays); w.Reason != BridgeWordRouted {
		t.Errorf("a routed track: %+v", w)
	}
	if w := BridgeWayOf("Disc/Album.iso/st/01.dff", "DFF", false, true, 2822400, fake.plays); w.Reason != BridgeWordSACDVirtual {
		t.Errorf("an SACD virtual track: %+v", w)
	}
	// No player running: no field.
	fake.runs = false
	for _, tr := range detail() {
		if _, ok := tr["bridge"]; ok {
			t.Fatalf("with no player running a row carries a bridge field: %v", tr)
		}
	}
	srv.deps.PlayerControl = nil
	for _, tr := range detail() {
		if _, ok := tr["bridge"]; ok {
			t.Fatalf("with no control a row carries a bridge field: %v", tr)
		}
	}
}
