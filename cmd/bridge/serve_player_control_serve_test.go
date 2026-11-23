package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// The console's control of serve's player through serve's own console (B634,
// PR 1): the routes, the publisher and the control as serve wires them, over
// the stand-in backend.

// controlOpFor is an operation's identity for the epoch a GET now answered.
func controlOpFor(t *testing.T, cs *consoleServe, seq int) (op string, st admin.PlayerState) {
	t.Helper()
	code, body := cs.call(t, http.MethodGet, "/api/output/player/now", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/output/player/now = %d: %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{"tab": strings.Repeat("ab", 16), "seq": seq, "epoch": st.Epoch})
	if err != nil {
		t.Fatal(err)
	}
	return string(b), st
}

// consoleEvents connects to a console's event stream and answers the names of
// the events its first burst carries, within bound.
func consoleEvents(t *testing.T, console string, bound time.Duration) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+console+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
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

// TestALongListIsWindowedThroughTheConsolesRoute pins K5 through serve's own
// console: a replace whose list of paths is over the 1 MiB the console's
// other writes take reaches the control whole, and the engine's queue holds
// the window of 1,000 from the start track.
func TestALongListIsWindowedThroughTheConsolesRoute(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	long := strings.Repeat("a", 200) + "/" + strings.Repeat("b", 200) + "/" + strings.Repeat("c", 200) + "/01 Song.flac"
	cs := bootConsoleServe(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn(), files: map[string][]byte{long: []byte("a track")}})
	eng := cs.runningEngine(t)
	availableOutput(t, eng)
	op, st := controlOpFor(t, cs, 1)
	tracks := make([]string, 1800)
	for i := range tracks {
		tracks[i] = long
	}
	body, err := json.Marshal(map[string]any{"op": json.RawMessage(op), "at": st.Generation, "tracks": tracks})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= 1<<20 {
		t.Fatalf("the body is %d bytes, not over 1 MiB", len(body))
	}
	code, raw := cs.call(t, http.MethodPost, "/api/output/player/queue", string(body), nil)
	if code != http.StatusOK {
		t.Fatalf("a replace of %d bytes = %d: %.400s", len(body), code, raw)
	}
	var a struct {
		Record admin.PlayerRecord `json:"record"`
	}
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatal(err)
	}
	if a.Record.Status != admin.PlayerOpDone || a.Record.Of != engine.MaxEntries+1 {
		t.Errorf("the record: %+v, want done, a DeleteAll and %d inserts", a.Record, engine.MaxEntries)
	}
	if n := eng.Snapshot().Len(); n != engine.MaxEntries {
		t.Fatalf("the queue holds %d, want the window of %d", n, engine.MaxEntries)
	}
	if code := cs.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
	}
}

// TestAPlayerTurnedOffIsNotOfferedJustAfterTheRestart pins K1 through serve:
// a player the console offered, turned off and the bridge restarted, is a
// deployment that offers none from the new serve's first request: GET now
// answers player_not_offered at once, the switch-out (the barrier) is refused
// the same way, and the event stream carries no player frame.
func TestAPlayerTurnedOffIsNotOfferedJustAfterTheRestart(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	cs := bootConsoleServe(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn()})
	cs.runningEngine(t)
	if code, body := cs.call(t, http.MethodGet, "/api/output/player/now", "", nil); code != http.StatusOK {
		t.Fatalf("the player offered: GET now = %d: %s", code, body)
	}
	if events := consoleEvents(t, cs.console, 1500*time.Millisecond); len(events) == 0 || events[0] != "player" {
		t.Fatalf("the player offered: the stream's first burst %v, want the player frame first", events)
	}
	cs.change(t, "/api/output/player/enabled", `{"enabled":false}`, "restart")
	if code := cs.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
	}
	var console string
	next := bootPlayerServeAt(t, playerServeCase{keep: true, standIn: newPlayerStandIn(), with: func(o *serveOpts) {
		console = o.adminListener.Addr().String()
	}}, filepath.Dir(cs.configPath), cs.music, cs.dataDir)
	again := &consoleServe{playerServe: next, console: console}
	start := time.Now()
	code, body := again.call(t, http.MethodGet, "/api/output/player/now", "", nil)
	if code != http.StatusConflict || !strings.Contains(body, `"error":"player_not_offered"`) || time.Since(start) > 2*time.Second {
		t.Fatalf("just after the restart: GET now = %d in %v: %s; want 409 player_not_offered at once", code, time.Since(start), body)
	}
	op := `{"tab":"` + strings.Repeat("ab", 16) + `","seq":1,"epoch":"` + strings.Repeat("e", 32) + `"}`
	if code, body := again.call(t, http.MethodPost, "/api/output/player/switch-out", `{"op":`+op+`,"take":true}`, nil); code != http.StatusConflict ||
		!strings.Contains(body, "player_not_offered") {
		t.Fatalf("just after the restart: the switch-out = %d: %s; want 409 player_not_offered", code, body)
	}
	events := consoleEvents(t, console, 1500*time.Millisecond)
	for _, e := range events {
		if e == "player" {
			t.Fatalf("a bridge that offers no player sent a player frame: %v", events)
		}
	}
	if len(events) == 0 {
		t.Fatal("the stream sent nothing: no proof it was open")
	}
	if code := next.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, next.stderr.String())
	}
}
